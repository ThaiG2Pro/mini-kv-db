package plan

import (
	"fmt"
	"strings"

	"minidb/internal/keys"
	"minidb/internal/query"
	"minidb/internal/table"
)

// physical.go chọn CÁCH CHẠY. Đây là chỗ index, thuật toán join và bộ nhớ
// hiện ra — và là chỗ những kết luận chỉ đúng cho một cách chạy cụ thể được
// phép sống.
//
// Bộ chọn access path ở LÁ không viết lại: nó gọi query.Choose của phase 7.
// Đó là một quyết định có ý: phase 7 đã đo điểm hoà vốn của đúng ba đường ấy
// (seq / index / index-only) trên máy này, và mô hình chi phí kèm số đo đáng
// hơn một mô hình mới chưa đo. Cái phase 8 thêm vào là những thứ NẰM TRÊN lá:
// join, sắp xếp, giới hạn — và ba thứ đó có một tính chất mà lá không có: chi
// phí của chúng phụ thuộc BỘ NHỚ được cấp.

// Span là khoảng giá trị trên cột dẫn đầu, ở mức GIÁ TRỊ chứ không phải byte.
// Việc mã hoá thành khóa là của internal/exec — plan không dựng byte.
type Span struct {
	Col    int
	Lo, Hi keys.Value
	HiIncl bool // Hi đóng: cần cho phép BẰNG, xem table.IterIndexKeys
	Eq     bool
}

// Empty: khoảng chắc chắn rỗng (ví dụ `x > 5 AND x < 3`). Nhận ra ở đây thì
// không phải quét gì cả — phép tối ưu rẻ nhất trong mọi optimizer.
func (s Span) Empty() bool {
	if s.Lo.IsNull() || s.Hi.IsNull() {
		return false
	}
	c := keys.Compare(s.Lo, s.Hi)
	if s.HiIncl {
		return c > 0
	}
	return c >= 0
}

// PNode là một node vật lý.
type PNode interface {
	pnode()
	Write(w *strings.Builder, depth int)
	Est() Est
}

// Est là ước lượng. Rows là số hàng RA; Cost theo đơn vị "một lần chạm entry
// của cây" — cùng đơn vị với query.CostModel của phase 7 để hai tầng cộng
// được vào nhau.
type Est struct {
	Rows float64
	Cost float64
}

// ---------- lá ----------

// PScan là một lần đọc quan hệ theo một đường cụ thể.
type PScan struct {
	Rel      int
	RelRef   string
	Sc       *table.Schema
	Kind     query.Kind
	Index    *table.Index
	Span     Span   // khoảng đẩy được vào access path; Col = -1 nếu không có
	Residual []Expr // điều kiện còn phải kiểm từng hàng
	Need     []int  // cột của quan hệ này mà tầng trên cần
	Why      string
	Order    []SortKey // thứ tự mà đường đi này TRẢ VỀ (dùng để bỏ Sort)
	// Empty: điều kiện tự mâu thuẫn, không có hàng nào thoả.
	//
	// Phải là một CỜ RIÊNG, không phải "chi phí 0". Bản đầu chỉ đặt
	// Est{Rows:0, Cost:0} rồi thêm một điều kiện FALSE vào residual — nên
	// planner nói 0 hàng mà toán tử vẫn quét cả bảng và lọc hết. "Chi phí 0"
	// là một câu về ƯỚC LƯỢNG; "đọc 0 hàng" là một câu về THI HÀNH, và hai
	// tầng ấy không tự đồng bộ với nhau.
	Empty bool
	E     Est
}

func (*PScan) pnode()     {}
func (p *PScan) Est() Est { return p.E }

func (p *PScan) Write(w *strings.Builder, depth int) {
	indent(w, depth)
	if p.Empty {
		fmt.Fprintf(w, "NoScan %s  (rows≈0 cost≈0) — %s\n", p.RelRef, p.Why)
		return
	}
	fmt.Fprintf(w, "%s %s", p.Kind, p.RelRef)
	if p.Index != nil {
		fmt.Fprintf(w, " dùng %s", p.Index.Name)
	}
	if p.Span.Col >= 0 {
		fmt.Fprintf(w, " [%s]", spanText(p.Sc, p.Span))
	}
	if len(p.Residual) > 0 {
		fmt.Fprintf(w, " filter=%s", exprList(p.Residual))
	}
	fmt.Fprintf(w, "  (rows≈%.0f cost≈%.0f) — %s\n", p.E.Rows, p.E.Cost, p.Why)
}

func spanText(sc *table.Schema, s Span) string {
	name := sc.Cols[s.Col].Name
	switch {
	case s.Eq:
		return fmt.Sprintf("%s = %v", name, s.Lo)
	case s.Lo.IsNull():
		return fmt.Sprintf("%s < %v", name, s.Hi)
	case s.Hi.IsNull():
		return fmt.Sprintf("%s >= %v", name, s.Lo)
	}
	return fmt.Sprintf("%v <= %s < %v", s.Lo, name, s.Hi)
}

// ---------- join ----------

// JoinKey là một cặp cột bằng nhau của điều kiện join, đã quy về slot.
type JoinKey struct {
	L, R int // slot trong tuple phẳng
	T    keys.Type
}

// PNestLoop là nested loop join: với MỖI hàng vế ngoài, chạy lại cả vế trong.
//
// Chi phí ≈ |L| + |L|·|R|. Nó thắng ở đúng một chỗ, và chỗ ấy không phải là
// "bảng nhỏ": nó thắng khi vế ngoài RẤT ít hàng, vì lúc đó |L|·|R| nhỏ dù |R|
// lớn — và nó là thuật toán DUY NHẤT cần O(1) bộ nhớ, nên nó là đường lùi khi
// không còn bộ nhớ để băm.
//
// Nó cũng là thuật toán duy nhất chạy được với điều kiện join KHÔNG PHẢI phép
// bằng (`ON a.x < b.y`), vì băm chỉ băm được phép bằng. Đó là lý do không thể
// bỏ nested loop đi.
type PNestLoop struct {
	L, R PNode
	On   []Expr
	E    Est
}

func (*PNestLoop) pnode()     {}
func (p *PNestLoop) Est() Est { return p.E }

func (p *PNestLoop) Write(w *strings.Builder, depth int) {
	indent(w, depth)
	fmt.Fprintf(w, "NestedLoopJoin on=%s  (rows≈%.0f cost≈%.0f)\n", exprList(p.On), p.E.Rows, p.E.Cost)
	p.L.Write(w, depth+1)
	p.R.Write(w, depth+1)
}

// PHashJoin dựng bảng băm từ vế BUILD rồi dò bằng vế PROBE.
//
// Chi phí ≈ |B| + |P|: mỗi hàng đọc đúng một lần. Đó là toàn bộ lý do nó
// thắng, và cũng là lý do nó cần bộ nhớ — nó phải giữ được cả vế build.
//
// Budget là số hàng build được giữ trong RAM. Vượt qua thì phải TRÀN RA ĐĨA,
// và lúc đó chi phí không còn là |B|+|P| nữa mà thành 3(|B|+|P|): ghi ra, đọc
// lại, cộng lần đọc đầu. Con số 3 ấy là lý do một memory budget quá nhỏ làm
// hash join chậm hơn nested loop dù lý thuyết bảo ngược lại — cmd/sqllab đo
// đúng chỗ đó.
type PHashJoin struct {
	Build, Probe PNode
	Keys         []JoinKey
	Extra        []Expr // vế của ON không phải phép bằng: kiểm sau khi băm khớp
	Budget       int
	E            Est
}

func (*PHashJoin) pnode()     {}
func (p *PHashJoin) Est() Est { return p.E }

func (p *PHashJoin) Write(w *strings.Builder, depth int) {
	indent(w, depth)
	fmt.Fprintf(w, "HashJoin keys=%d budget=%d hàng", len(p.Keys), p.Budget)
	if len(p.Extra) > 0 {
		fmt.Fprintf(w, " extra=%s", exprList(p.Extra))
	}
	fmt.Fprintf(w, "  (rows≈%.0f cost≈%.0f)\n", p.E.Rows, p.E.Cost)
	indent(w, depth+1)
	w.WriteString("[build]\n")
	p.Build.Write(w, depth+2)
	indent(w, depth+1)
	w.WriteString("[probe]\n")
	p.Probe.Write(w, depth+2)
}

// ---------- một toán hạng ----------

type PFilter struct {
	In    PNode
	Preds []Expr
	E     Est
}

func (*PFilter) pnode()     {}
func (p *PFilter) Est() Est { return p.E }
func (p *PFilter) Write(w *strings.Builder, depth int) {
	indent(w, depth)
	fmt.Fprintf(w, "Filter %s  (rows≈%.0f)\n", exprList(p.Preds), p.E.Rows)
	p.In.Write(w, depth+1)
}

// PSort sắp xếp. Budget là số hàng giữ được trong RAM; vượt thì sắp NGOÀI —
// chia thành run đã sắp, ghi ra đĩa, rồi trộn k đường.
type PSort struct {
	In     PNode
	By     []SortKey
	Budget int
	E      Est
}

func (*PSort) pnode()     {}
func (p *PSort) Est() Est { return p.E }
func (p *PSort) Write(w *strings.Builder, depth int) {
	indent(w, depth)
	fmt.Fprintf(w, "Sort by=%s budget=%d hàng  (rows≈%.0f cost≈%.0f)\n",
		sortKeys(p.By), p.Budget, p.E.Rows, p.E.Cost)
	p.In.Write(w, depth+1)
}

type PProject struct {
	In  PNode
	Out []OutCol
	E   Est
}

func (*PProject) pnode()     {}
func (p *PProject) Est() Est { return p.E }
func (p *PProject) Write(w *strings.Builder, depth int) {
	indent(w, depth)
	names := make([]string, len(p.Out))
	for i, o := range p.Out {
		names[i] = o.Name
	}
	fmt.Fprintf(w, "Project %s\n", strings.Join(names, ", "))
	p.In.Write(w, depth+1)
}

type PLimit struct {
	In PNode
	N  int64
	E  Est
}

func (*PLimit) pnode()     {}
func (p *PLimit) Est() Est { return p.E }
func (p *PLimit) Write(w *strings.Builder, depth int) {
	indent(w, depth)
	fmt.Fprintf(w, "Limit %d  (rows≈%.0f)\n", p.N, p.E.Rows)
	p.In.Write(w, depth+1)
}

// ExplainP in một cây vật lý.
func ExplainP(n PNode) string {
	var w strings.Builder
	n.Write(&w, 0)
	return w.String()
}
