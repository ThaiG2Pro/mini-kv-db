package query

import (
	"fmt"
	"time"

	"minidb/internal/keys"
	"minidb/internal/table"
	"minidb/internal/txn"
)

// Dataset là bộ dữ liệu chung của phase 7.
//
// Nó KHÔNG nằm trong một file _test.go, cùng lý do như internal/txn/workload.go
// của phase 6: bảng mà cmd/idxlab in ra và bảng mà `go test` khẳng định phải
// sinh ra từ đúng một đoạn code. Hai bản sao của cùng một workload là hai chỗ
// để lệch nhau, và chỗ lệch ấy sẽ được phát hiện bằng cách đọc diary rồi không
// tái lập được số.
//
// Lược đồ:
//
//	events(id uint PK, kind int, city bytes, payload bytes)
//	  ev_kind (kind)            non-unique
//	  ev_city (city)            non-unique
//
// kind phân bố ĐỀU trên [0, 1000): nhờ vậy độ chọn lọc của `kind < w` đúng
// bằng w/1000, tức là trục hoành của bài đo điểm hoà vốn được điều khiển chính
// xác. city thì LỆCH nặng (99% một giá trị) — để đo cái giá của việc ước lượng
// bằng mô hình phân bố đều.
type Dataset struct {
	Cat    *table.Catalog
	Schema *table.Schema
	Stats  Stats
	Rows   int
}

// KindMod là số giá trị khác nhau của cột kind. Độ chọn lọc của điều kiện
// `kind < w` là w/KindMod.
const KindMod = 1000

// BuildDataset nạp n hàng rồi tạo index SAU KHI nạp — đúng thứ tự mà mọi công
// cụ nạp dữ liệu hàng loạt khuyên, và BenchmarkIndexMaintenance đo cái giá của
// việc làm ngược lại.
func BuildDataset(s *txn.Store, n int) (*Dataset, error) {
	c, err := table.Load(s)
	if err != nil {
		return nil, err
	}
	sc, err := c.Table("events")
	if err != nil {
		sc, err = c.CreateTable("events", []table.Column{
			{Name: "id", T: keys.TypeUint},
			{Name: "kind", T: keys.TypeInt},
			{Name: "city", T: keys.TypeBytes},
			{Name: "payload", T: keys.TypeBytes},
		}, []string{"id"})
		if err != nil {
			return nil, err
		}
	}
	// Chèn theo lô: một transaction logic giữ cả write set trong RAM, nên
	// 200k hàng trong một transaction là 200k value trong một map. Lô 2000 là
	// chỗ dung hoà, và nó cũng làm cho lab có tiến độ nhìn được.
	const batch = 2000
	for start := 0; start < n; start += batch {
		end := start + batch
		if end > n {
			end = n
		}
		err := c.Update(txn.RepeatableRead, func(tx *table.Tx) error {
			for i := start; i < end; i++ {
				if err := tx.Insert(sc, Row(i)); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	if _, err := c.CreateIndex("events", "ev_kind", []string{"kind"}, false); err != nil {
		return nil, err
	}
	if _, err := c.CreateIndex("events", "ev_city", []string{"city"}, false); err != nil {
		return nil, err
	}
	d := &Dataset{Cat: c, Schema: sc, Rows: n}
	if err := c.View(txn.RepeatableRead, func(tx *table.Tx) error {
		st, err := Analyze(tx, sc, []int{1, 2})
		d.Stats = st
		return err
	}); err != nil {
		return nil, err
	}
	return d, nil
}

// Row là hàng thứ i của bộ dữ liệu. Một hàm, dùng ở cả lab, test và benchmark.
func Row(i int) []keys.Value {
	city := "HN"
	if i%100 == 0 {
		city = fmt.Sprintf("c%03d", i%97)
	}
	return []keys.Value{
		keys.Uint(uint64(i)),
		keys.Int(int64(i % KindMod)),
		keys.Str(city),
		keys.Str(fmt.Sprintf("payload-%d-xxxxxxxxxxxxxxxx", i)),
	}
}

// Sweep là một dòng của bảng điểm hoà vốn.
type Sweep struct {
	Width       int
	Sel         float64
	Rows        int
	SeqNs       int64
	IdxNs       int64
	OnlyNs      int64
	SeqTouched  int
	IdxTouched  int
	OnlyTouched int
	Chosen      Kind // planner với mô hình chi phí ĐO ĐƯỢC
	ChosenGuess Kind // planner với mô hình ĐOÁN trong code (DefaultCost)
	Best        Kind // đường thật sự nhanh hơn, theo đồng hồ
}

// Ratio là tỉ số index/seq. < 1 là index thắng. Đây là con số phải đọc, không
// phải ns — tuyệt đối thì đổi theo máy, tỉ số thì không (bài học phase 0).
func (s Sweep) Ratio() float64 { return float64(s.IdxNs) / float64(s.SeqNs) }

// RatioOnly là tỉ số index-only/seq.
func (s Sweep) RatioOnly() float64 { return float64(s.OnlyNs) / float64(s.SeqNs) }

// SweepBreakEven chạy cùng một truy vấn theo ba đường ở nhiều độ chọn lọc.
//
// repeat > 1 vì một lần chạy của một truy vấn hẹp chỉ tốn vài micro giây, và
// đo một thứ vài micro giây một lần là đo nhiễu của bộ đếm thời gian. Đây là
// cái bẫy mà phase 6 dính một lần (200 vòng lặp "chứng minh" depth=60 nhanh
// hơn depth=32) và phase 0 dính lần đầu.
func (d *Dataset) SweepBreakEven(widths []int, repeat int, cm CostModel) ([]Sweep, error) {
	ix, err := d.Cat.Index("ev_kind")
	if err != nil {
		return nil, err
	}
	var out []Sweep
	for _, w := range widths {
		s := Sweep{Width: w, Sel: float64(w) / KindMod}
		// Hai truy vấn khác nhau, không phải hai cách chạy một truy vấn:
		// seq/index cần cột payload (index không phủ), only chỉ cần id+kind.
		qFull := Query{Table: d.Schema, Col: 1, Lo: keys.Int(0), Hi: keys.Int(int64(w)),
			Need: []int{0, 3}}
		qCov := qFull
		qCov.Need = []int{0, 1}

		runs := []struct {
			p  Plan
			q  Query
			ns *int64
			tc *int
		}{
			{Plan{Kind: SeqScan}, qFull, &s.SeqNs, &s.SeqTouched},
			{Plan{Kind: IndexScan, Index: ix}, qFull, &s.IdxNs, &s.IdxTouched},
			{Plan{Kind: IndexOnlyScan, Index: ix}, qCov, &s.OnlyNs, &s.OnlyTouched},
		}
		for _, r := range runs {
			var res Result
			t0 := time.Now()
			for i := 0; i < repeat; i++ {
				err := d.Cat.View(txn.RepeatableRead, func(tx *table.Tx) error {
					var err error
					res, err = Run(tx, r.p, r.q, func(row []keys.Value) bool { return true })
					return err
				})
				if err != nil {
					return nil, err
				}
			}
			*r.ns = time.Since(t0).Nanoseconds() / int64(repeat)
			*r.tc = res.Touched
			s.Rows = res.Rows
		}
		s.Chosen = Choose(d.Cat, d.Stats, qFull, cm).Kind
		s.ChosenGuess = Choose(d.Cat, d.Stats, qFull, DefaultCost).Kind
		s.Best = SeqScan
		if s.IdxNs < s.SeqNs {
			s.Best = IndexScan
		}
		out = append(out, s)
	}
	return out, nil
}

// CostFrom dựng mô hình chi phí từ SỐ ĐO thật (ns), quy về đơn vị "một bước
// quét tuần tự". Có hàm này để mô hình không phải là ba con số đoán trong code
// mãi mãi: đo bằng BenchmarkSeqStep / BenchmarkPointLookup rồi truyền vào.
func CostFrom(nsSeqStep, nsIdxStep, nsFetch float64) CostModel {
	if nsSeqStep <= 0 {
		return DefaultCost
	}
	return CostModel{CSeq: 1, CIndex: nsIdxStep / nsSeqStep, CFetch: nsFetch / nsSeqStep}
}

// CrossOver là điểm hoà vốn ĐO ĐƯỢC — độ chọn lọc mà tại đó idx/seq = 1 —
// nội suy tuyến tính giữa hai dòng kề nhau của bảng sweep. Tỉ số ấy gần như
// tuyến tính theo độ chọn lọc vì chi phí của seq scan gần như phẳng (nó quét
// cả bảng dù điều kiện hẹp cỡ nào) trong khi chi phí của index scan tỉ lệ với
// số hàng khớp. Trả về 0 nếu index thắng ở mọi dòng đo.
//
// Có hàm này vì bản đầu của cmd/idxlab in ra điểm hoà vốn của MÔ HÌNH rồi gọi
// nó là "thời gian đổi vai", trong khi chính cái bảng ngay bên trên cho thấy
// index còn thắng 1.6x ở dải mà mô hình đã bảo chuyển sang seq. Một dòng tổng
// kết phản bác bảng số của chính nó là một lỗi của bộ đo, và nó lọt qua được
// vì con số ấy không hề được tính từ số đo — nó chỉ được chép lại từ mô hình.
func CrossOver(sw []Sweep) float64 {
	for i, s := range sw {
		if s.Ratio() < 1 {
			continue
		}
		if i == 0 {
			return s.Sel
		}
		p := sw[i-1]
		r0, r1 := p.Ratio(), s.Ratio()
		if r1 == r0 {
			return s.Sel
		}
		return p.Sel + (s.Sel-p.Sel)*(1-r0)/(r1-r0)
	}
	return 0
}
