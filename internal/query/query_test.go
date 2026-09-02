package query

import (
	"fmt"
	"path/filepath"
	"testing"

	"minidb/internal/db"
	"minidb/internal/keys"
	"minidb/internal/table"
	"minidb/internal/txn"
)

// events(id uint PK, kind int, city bytes, payload bytes)
// index ev_kind (kind), index ev_city (city)
// fixtureBare dựng bảng rỗng, không index, không thống kê — cho benchmark
// đường ghi.
func fixtureBare(t testing.TB) (*table.Catalog, *table.Schema, Stats) {
	t.Helper()
	s, err := txn.Open(filepath.Join(t.TempDir(), "data.db"), db.Options{Frames: 256})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	c, err := table.Load(s)
	if err != nil {
		t.Fatal(err)
	}
	sc, err := c.CreateTable("events", []table.Column{
		{Name: "id", T: keys.TypeUint},
		{Name: "kind", T: keys.TypeInt},
		{Name: "city", T: keys.TypeBytes},
		{Name: "payload", T: keys.TypeBytes},
	}, []string{"id"})
	if err != nil {
		t.Fatal(err)
	}
	return c, sc, Stats{}
}

func fixture(t testing.TB, n int) (*table.Catalog, *table.Schema, Stats) {
	t.Helper()
	s, err := txn.Open(filepath.Join(t.TempDir(), "data.db"), db.Options{Frames: 256})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	d, err := BuildDataset(s, n)
	if err != nil {
		t.Fatal(err)
	}
	return d.Cat, d.Schema, d.Stats
}

// TestThreePlansSameRows là bất biến số một của bất kỳ planner nào: đổi kế
// hoạch KHÔNG ĐƯỢC đổi kết quả. Một planner chậm là một vấn đề về hiệu năng;
// một planner đổi kết quả là một database sai.
//
// Bài test chạy cả ba đường cho cùng một truy vấn ở nhiều độ chọn lọc, và so
// từng hàng. Nó cũng in ra số entry mỗi đường đã chạm — đó là con số mà phần
// benchmark sẽ dùng.
func TestThreePlansSameRows(t *testing.T) {
	c, sc, _ := fixture(t, 2000)
	for _, w := range []int{1, 10, 100, 500, 1000} {
		q := Query{Table: sc, Col: 1, Lo: keys.Int(200), Hi: keys.Int(int64(200 + w)),
			Need: []int{0, 1}} // id + kind: nằm hết trong mục index -> phủ được
		ix, err := c.Index("ev_kind")
		if err != nil {
			t.Fatal(err)
		}
		var out [3][]string
		var res [3]Result
		plans := []Plan{{Kind: SeqScan}, {Kind: IndexScan, Index: ix}, {Kind: IndexOnlyScan, Index: ix}}
		for i, p := range plans {
			if err := c.View(txn.RepeatableRead, func(tx *table.Tx) error {
				r, err := Run(tx, p, q, func(row []keys.Value) bool {
					out[i] = append(out[i], fmt.Sprintf("%v/%v", row[0], row[1]))
					return true
				})
				res[i] = r
				return err
			}); err != nil {
				t.Fatalf("w=%d %v: %v", w, p.Kind, err)
			}
		}
		for i := 1; i < 3; i++ {
			if len(out[i]) != len(out[0]) {
				t.Fatalf("w=%d: %v ra %d hàng, SeqScan ra %d",
					w, plans[i].Kind, len(out[i]), len(out[0]))
			}
		}
		// SeqScan đi theo pk, IndexScan đi theo (kind, pk) -> thứ tự KHÁC
		// nhau là đúng, nên so theo tập hợp. So TỪNG CẶP với SeqScan, không
		// dồn cả ba vào một map: bản đầu của bài test này trừ hai lần trên
		// cùng một map và báo lệch -1 ở mọi hàng — lỗi của bộ đo, không của
		// code. Đúng cái bẫy mà phase 0 gọi là "nghi bài test trước".
		for i := 1; i < 3; i++ {
			set := map[string]int{}
			for _, s := range out[0] {
				set[s]++
			}
			for _, s := range out[i] {
				set[s]--
			}
			for s, n := range set {
				if n != 0 {
					t.Fatalf("w=%d: hàng %q lệch giữa SeqScan và %v (%d)",
						w, s, plans[i].Kind, n)
				}
			}
		}
		t.Logf("w=%4d rows=%4d | seq touched=%5d | idx touched=%5d | only touched=%5d",
			w, len(out[0]), res[0].Touched, res[1].Touched, res[2].Touched)
	}
}

// TestChoosePicksIndexOnlyWhenNarrow / SeqWhenWide: hai đầu của cùng một trục.
// Cùng một truy vấn, chỉ đổi độ rộng khoảng, và kế hoạch phải đổi. Nếu planner
// chọn cùng một đường ở cả hai đầu thì mô hình chi phí không có tác dụng gì.
func TestChooseFlipsWithSelectivity(t *testing.T) {
	c, sc, st := fixture(t, 2000)
	cm := DefaultCost
	narrow := Query{Table: sc, Col: 1, Lo: keys.Int(0), Hi: keys.Int(5), Need: []int{0, 3}}
	wide := Query{Table: sc, Col: 1, Lo: keys.Int(0), Hi: keys.Int(900), Need: []int{0, 3}}

	pn := Choose(c, st, narrow, cm)
	if pn.Kind != IndexScan {
		t.Fatalf("khoảng hẹp: chọn %v (%s), mong IndexScan", pn.Kind, pn.Why)
	}
	pw := Choose(c, st, wide, cm)
	if pw.Kind != SeqScan {
		t.Fatalf("khoảng rộng: chọn %v (%s), mong SeqScan", pw.Kind, pw.Why)
	}
	// Và khi truy vấn chỉ cần cột được phủ thì index thắng cả ở khoảng rộng.
	wideCovered := wide
	wideCovered.Need = []int{0, 1}
	pc := Choose(c, st, wideCovered, cm)
	if pc.Kind != IndexOnlyScan {
		t.Fatalf("khoảng rộng + index phủ: chọn %v (%s), mong IndexOnlyScan",
			pc.Kind, pc.Why)
	}
	t.Logf("hẹp:  %s", pn.Explain())
	t.Logf("rộng: %s", pw.Explain())
	t.Logf("phủ:  %s", pc.Explain())
}

// TestChooseRefusesIndexOnNonLeadingColumn: luật tiền tố bên trái. Một index
// trên (city) không giúp gì cho điều kiện trên kind, và planner phải nhận ra
// điều đó — không phải bằng một danh sách ngoại lệ mà vì không có khoảng nào
// để quét.
func TestChooseRefusesIndexOnNonLeadingColumn(t *testing.T) {
	c, sc, st := fixture(t, 500)
	q := Query{Table: sc, Col: 3, Lo: keys.Str("payload-1"), Hi: keys.Str("payload-2"),
		Need: []int{0}}
	p := Choose(c, st, q, DefaultCost)
	if p.Kind != SeqScan {
		t.Fatalf("cột không có index: chọn %v", p.Kind)
	}
}

// TestEstimateIsWrongOnSkew là bài test của một GIỚI HẠN, không của một tính
// năng — và nó phải tồn tại, vì cái giới hạn ấy là bài học chính của phần
// planner. Cột city lệch nặng (99% là "HN"), mô hình phân bố đều không biết
// điều đó, nên ước lượng lệch nhiều lần. Bài test khẳng định nó LỆCH, để nếu
// sau này ai thêm histogram thì bài test đỏ và người đó biết mình vừa sửa cái
// gì.
func TestEstimateIsWrongOnSkew(t *testing.T) {
	c, sc, st := fixture(t, 2000)
	q := Query{Table: sc, Col: 2, Lo: keys.Str("HN"), Hi: keys.Str("HO"), Need: []int{0}}
	p := Choose(c, st, q, DefaultCost)

	actual := 0
	if err := c.View(txn.RepeatableRead, func(tx *table.Tx) error {
		return tx.ScanRows(sc, nil, nil, func(pk, row []keys.Value) bool {
			if string(row[2].B) == "HN" {
				actual++
			}
			return true
		})
	}); err != nil {
		t.Fatal(err)
	}
	ratio := float64(actual) / p.Rows
	if ratio < 2 && ratio > 0.5 {
		t.Fatalf("ước lượng %0.f hàng, thật %d — mô hình đều đang ĐÚNG trên"+
			" dữ liệu lệch, nghĩa là fixture không còn lệch nữa", p.Rows, actual)
	}
	t.Logf("cột lệch: ước lượng %.0f hàng, thật %d, lệch %.1fx — %s",
		p.Rows, actual, ratio, p.Why)
}

// TestBreakEvenFormula: điểm hoà vốn tính từ mô hình. Số này được ghi ra
// TRƯỚC khi đo (cmd/idxlab đo), đúng quy tắc 2 của sổ nợ.
func TestBreakEvenFormula(t *testing.T) {
	got := BreakEven(DefaultCost)
	if got < 0.045 || got > 0.05 {
		t.Fatalf("BreakEven = %.4f, mong ~0.0485 với DefaultCost", got)
	}
	if BreakEvenOnly(DefaultCost) < 1 {
		t.Fatalf("index-only phải thắng ở MỌI độ chọn lọc với mô hình này," +
			" tức điểm hoà vốn của nó phải >= 1")
	}
}
