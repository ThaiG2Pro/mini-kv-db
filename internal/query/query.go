// Package query là tầng mỏng nhất mà vẫn trả lời được câu hỏi của phase 7:
//
//	cùng một truy vấn, hai đường lấy dữ liệu, đường nào rẻ hơn — và ở ĐÂU
//	chúng đổi vai?
//
// Không có parser, không có join, không có planner tổng quát (đó là phase 8).
// Chỉ có một dạng truy vấn duy nhất, đúng dạng mà mọi sách DB dùng để dạy về
// selectivity:
//
//	SELECT <cột cần>  FROM <bảng>  WHERE <cột> >= lo AND <cột> < hi
//
// và ba cách chạy nó:
//
//	SeqScan       quét cả bảng theo thứ tự primary key, lọc từng hàng
//	IndexScan     quét khoảng của index, rồi TRA BẢNG từng hàng theo pk
//	IndexOnlyScan quét khoảng của index và KHÔNG tra bảng — chỉ dùng được khi
//	              mọi cột cần đều nằm trong index (index "phủ" truy vấn)
//
// Ba dòng ấy là toàn bộ nội dung của mọi `EXPLAIN` mà người ta đọc hàng ngày.
package query

import (
	"fmt"
	"strings"

	"minidb/internal/keys"
	"minidb/internal/table"
)

// Kind là loại kế hoạch.
type Kind int

const (
	SeqScan Kind = iota
	IndexScan
	IndexOnlyScan
)

func (k Kind) String() string {
	switch k {
	case SeqScan:
		return "SeqScan"
	case IndexScan:
		return "IndexScan"
	case IndexOnlyScan:
		return "IndexOnlyScan"
	}
	return "?"
}

// Query là một truy vấn: một khoảng trên một cột, và danh sách cột cần trả về.
//
// Need không phải để tiết kiệm bộ nhớ — nó là thứ quyết định index có PHỦ được
// truy vấn hay không, tức là quyết định giữa IndexScan và IndexOnlyScan. Trong
// SQL thật, đây đúng là chỗ `SELECT *` trả giá: nó làm mọi index mất khả năng
// phủ.
type Query struct {
	Table  *table.Schema
	Col    int        // cột bị ràng buộc
	Lo, Hi keys.Value // khoảng [Lo, Hi); Lo/Hi kiểu Null = không chặn đầu ấy
	Need   []int      // cột cần trả về
}

func (q Query) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "SELECT ")
	for i, c := range q.Need {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(q.Table.Cols[c].Name)
	}
	fmt.Fprintf(&b, " FROM %s WHERE %s", q.Table.Name, q.Table.Cols[q.Col].Name)
	switch {
	case q.Lo.IsNull() && q.Hi.IsNull():
		b.WriteString(" IS ANY")
	case q.Hi.IsNull():
		fmt.Fprintf(&b, " >= %v", q.Lo)
	case q.Lo.IsNull():
		fmt.Fprintf(&b, " < %v", q.Hi)
	default:
		fmt.Fprintf(&b, " >= %v AND < %v", q.Lo, q.Hi)
	}
	return b.String()
}

// Plan là quyết định, kèm LÝ DO. Lý do được mang theo vì một planner không nói
// được vì sao nó chọn thế là một planner không debug được — và "đọc EXPLAIN
// bằng trực giác" nghĩa là đọc được đúng cái lý do này.
type Plan struct {
	Kind    Kind
	Index   *table.Index
	Sel     float64 // chọn lọc ước lượng: phần hàng thoả điều kiện
	Rows    float64 // số hàng ước lượng
	Cost    float64 // chi phí ước lượng, đơn vị "một lần chạm entry"
	SeqCost float64 // chi phí của đường kia, để so
	Why     string
}

func (p Plan) Explain() string {
	s := p.Kind.String()
	if p.Index != nil {
		s += " dùng " + p.Index.Name
	}
	return fmt.Sprintf("%-28s sel=%.4f rows=%.0f cost=%.0f (seq=%.0f) — %s",
		s, p.Sel, p.Rows, p.Cost, p.SeqCost, p.Why)
}

// Stats là thống kê của một bảng — thứ mà `ANALYZE` sinh ra.
//
// Mô hình phân bố ĐỀU trên [Min, Max]: nghèo nhất trong các mô hình còn dùng
// được, và cố ý chọn nghèo, vì phase 7 muốn ĐO cái giá của việc ước lượng sai
// chứ không muốn tránh nó. Histogram là chỗ Postgres bỏ tiền vào, và lý do nó
// phải bỏ tiền vào chính là con số mà lab in ra.
type Stats struct {
	Rows int
	Cols map[int]ColStats
}

type ColStats struct {
	Min, Max keys.Value
	Distinct int
}

// CostModel là ba con số quyết định điểm hoà vốn. Chúng là chi phí TƯƠNG ĐỐI,
// đơn vị "một lần chạm entry của cây", và giá trị mặc định lấy từ số đo của
// phase 4/6 trên máy này (xem diary/phase7.md).
type CostModel struct {
	CSeq   float64 // đọc một entry kế tiếp trong một lần quét tuần tự
	CIndex float64 // đọc một entry kế tiếp của index (cũng tuần tự, nhưng entry nhỏ hơn)
	CFetch float64 // MỘT lần tra bảng theo pk: đi từ root xuống lá
}

// DefaultCost là mô hình ĐOÁN, giữ nguyên sau khi đã bị số đo bác bỏ.
//
// Giả thuyết viết ra trước khi đo: CFetch/CSeq = 20, tức một lần xuống cây đắt
// bằng khoảng hai chục bước đi ngang tầng lá, và điểm hoà vốn ở 4.85%.
//
// Số đo (make idxlab): CFetch/CSeq = 2.0-2.8x, điểm hoà vốn 30-40% — con số
// chốt kèm lệnh sinh ra nó nằm ở diary/phase7.md.
// SAI khoảng 7-10 lần, và lý do đáng nhớ hơn con số: ở quy mô này KHÔNG CÓ I/O nào cả —
// cả cây nằm trong buffer pool, nên một lần "tra ngẫu nhiên" chỉ là ba lần
// tra bảng băm của pool, không phải một cú seek 69µs (phase 0). Con số 20 là
// con số của một cây KHÔNG nằm trong RAM.
//
// Cố ý KHÔNG sửa hằng số này thành 2.8, và cột `planner đoán` của bảng idxlab
// là lý do: một mô hình chi phí lệch 7 lần làm planner chọn SAI đúng ở dải
// 5-25%, và chỗ ấy index nhanh hơn seq nhiều lần. Đó chính là
// `random_page_cost` của Postgres — tham số mà mọi hướng dẫn tuning đều bảo
// hạ xuống khi chạy trên SSD, và đây là bảng số giải thích vì sao.
var DefaultCost = CostModel{CSeq: 1, CIndex: 0.6, CFetch: 20}

// MeasuredCost là mô hình ĐÃ ĐO, và nó tồn tại vì phase 8 cần một planner
// dùng được trong khi DefaultCost phải giữ nguyên cái sai của nó.
//
// Số lấy từ diary/phase7.md (make idxlab, máy i5-1235U, WSL2):
//
//	một bước seq scan       413-544 ns   -> CSeq   = 1
//	một bước quét index     241-295 ns   -> CIndex = 0.6
//	một lần tra bảng theo pk 988-1308 ns -> CFetch = 2.4
//
// CFetch ở đây là ca TƯƠNG QUAN: index scan tra bảng theo thứ tự index, và khi
// thứ tự ấy gần thứ tự pk thì mỗi lần tra rơi vào trang vừa đọc. Đo cùng phép
// ấy theo khóa nhảy lung tung thì ra 2142-2327 ns, tức 1.6-2.2x đắt hơn — và
// CẢ HAI đều đúng, cho hai index tương quan khác nhau. Postgres lưu chuyện này
// thành thống kê `correlation` của từng cột rồi nội suy chi phí giữa hai đầu;
// ở đây chọn đầu tương quan, và đó là một GIẢ THUYẾT nhìn thấy được chứ không
// phải một hằng số trốn trong code.
//
// Điểm hoà vốn của mô hình này: 1/(0.6+2.4) = 33.3%, so với 36.8% đo được.
var MeasuredCost = CostModel{CSeq: 1, CIndex: 0.6, CFetch: 2.4}

// Selectivity ước lượng phần hàng thoả điều kiện, theo phân bố đều.
func (s Stats) Selectivity(col int, lo, hi keys.Value) float64 {
	cs, ok := s.Cols[col]
	if !ok || s.Rows == 0 {
		return 0.5 // không biết gì thì đoán một nửa — và đoán sai
	}
	lo1, hi1 := lo, hi
	if lo1.IsNull() {
		lo1 = cs.Min
	}
	if hi1.IsNull() {
		hi1 = cs.Max
	}
	whole := spanOf(cs.Min, cs.Max)
	if whole <= 0 {
		// Không phải kiểu số, hoặc cột chỉ có một giá trị: mô hình đều không
		// nói được gì. Trả về 1/Distinct — ước lượng của một phép bằng — và
		// ghi thẳng vào Why ở Choose rằng đây là chỗ ước lượng mù.
		if cs.Distinct > 0 {
			return 1 / float64(cs.Distinct)
		}
		return 1
	}
	f := spanOf(lo1, hi1) / whole
	if f < 0 {
		f = 0
	}
	if f > 1 {
		f = 1
	}
	// Chặn dưới 1/Rows: một khoảng rỗng theo ước lượng vẫn có thể có một
	// hàng, và một kế hoạch tính "0 hàng" là một kế hoạch chia cho 0.
	if min := 1 / float64(s.Rows); f < min {
		f = min
	}
	return f
}

// spanOf là "độ dài" của khoảng [a, b) theo kiểu số. Chỉ có nghĩa cho số —
// với chuỗi thì mọi DB đều phải đoán, và ở đây ta trả về 0.5 phần cho xong,
// kèm ghi chú rằng đó là chỗ ước lượng bắt đầu vô nghĩa.
func spanOf(a, b keys.Value) float64 {
	switch a.T {
	case keys.TypeInt:
		return float64(b.I) - float64(a.I)
	case keys.TypeUint:
		return float64(b.U) - float64(a.U)
	}
	return -1
}

// Choose là planner: nó chọn đường đi, và nó chỉ có ba luật.
//
// Luật 1: index chỉ dùng được nếu cột bị ràng buộc là CỘT ĐẦU của index.
//
//	Đây là "leftmost prefix rule" của MySQL, và ở đây nó không phải một quy
//	ước mà là hệ quả trực tiếp của hình dạng byte (xem internal/keys): khóa
//	là các cột nối đuôi nhau, nên chỉ tiền tố bên trái mới là một khoảng
//	liên tục của cây.
//
// Luật 2: nếu mọi cột cần đều nằm trong index thì bỏ hẳn bước tra bảng.
// Luật 3: so chi phí ước lượng, chọn cái nhỏ hơn.
func Choose(c *table.Catalog, st Stats, q Query, cm CostModel) Plan {
	n := float64(st.Rows)
	sel := st.Selectivity(q.Col, q.Lo, q.Hi)
	seqCost := n * cm.CSeq
	best := Plan{
		Kind: SeqScan, Sel: sel, Rows: sel * n, Cost: seqCost, SeqCost: seqCost,
		Why: "không có index dùng được",
	}

	for _, ix := range c.Indexes(q.Table.OID) {
		if len(ix.Cols) == 0 || ix.Cols[0] != q.Col {
			continue // luật 1
		}
		covering := true
		for _, need := range q.Need {
			in := false
			for _, ic := range ix.Cols {
				if ic == need {
					in = true
				}
			}
			for _, pc := range q.Table.PK {
				if pc == need {
					in = true // pk luôn có trong mục index
				}
			}
			if !in {
				covering = false
				break
			}
		}
		rows := sel * n
		kind, cost, why := IndexScan, rows*(cm.CIndex+cm.CFetch), "quét index rồi tra bảng"
		if covering {
			kind, cost = IndexOnlyScan, rows*cm.CIndex
			why = "index phủ đủ cột cần, không phải tra bảng"
		}
		if cost < best.Cost {
			best = Plan{Kind: kind, Index: ix, Sel: sel, Rows: rows,
				Cost: cost, SeqCost: seqCost, Why: why}
		} else if best.Kind == SeqScan {
			best.Why = fmt.Sprintf("có index %s nhưng chọn lọc %.1f%% quá rộng"+
				" (cost %.0f > seq %.0f)", ix.Name, sel*100, cost, seqCost)
		}
	}
	return best
}

// BreakEven là điểm hoà vốn TÍNH RA ĐƯỢC từ mô hình chi phí: phần hàng mà tại
// đó index scan và seq scan bằng giá.
//
//	sel* = CSeq / (CIndex + CFetch)
//
// Với DefaultCost thì sel* = 1/20.6 = 4.85%. Con số này được VIẾT RA TRƯỚC khi
// đo (nợ 📏 không được đoán sau), và cmd/idxlab đo xem thực tế nằm ở đâu.
func BreakEven(cm CostModel) float64 { return cm.CSeq / (cm.CIndex + cm.CFetch) }

// BreakEvenOnly là điểm hoà vốn của index-only scan: không có bước tra bảng
// nên nó gần như KHÔNG BAO GIỜ tới — index-only thắng ở mọi độ chọn lọc, và
// đó là lý do covering index là món ăn không mất tiền của tuning SQL.
func BreakEvenOnly(cm CostModel) float64 { return cm.CSeq / cm.CIndex }
