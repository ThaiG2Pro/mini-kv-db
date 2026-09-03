package plan

import (
	"fmt"
	"strings"

	"minidb/internal/table"
)

// logical.go là cây LOGICAL: nó nói LÀM GÌ, không nói làm THẾ NÀO.
//
// Không có node nào ở đây biết tới index, tới thuật toán join, tới bộ nhớ.
// Đó là toàn bộ định nghĩa của "logical": hai cây logical bằng nhau thì trả về
// cùng một tập hàng, bất kể chạy ra sao. Nhờ vậy mọi phép viết lại ở opt.go
// chứng minh được đúng bằng lập luận về tập hợp, không cần biết máy.

// Node là một node logical.
type Node interface {
	node()
	// Write in cây ra theo lối thụt dòng. Có mặt trong cả logical và physical
	// vì EXPLAIN in CẢ HAI: xem hai cây cạnh nhau là cách nhanh nhất để thấy
	// optimizer đã làm gì.
	Write(w *strings.Builder, depth int)
}

func indent(w *strings.Builder, depth int) {
	for i := 0; i < depth; i++ {
		w.WriteString("  ")
	}
	if depth > 0 {
		w.WriteString("-> ")
	}
}

// Scan là "đọc một quan hệ". Preds là những điều kiện đã được ĐẨY XUỐNG tới
// sát quan hệ này — vẫn còn là logical: chưa nói chúng sẽ thành khoảng quét
// trên index hay thành một phép lọc từng hàng.
type Scan struct {
	Rel    int
	RelRef string
	Sc     *table.Schema
	Preds  []Expr
}

func (*Scan) node() {}

func (s *Scan) Write(w *strings.Builder, depth int) {
	indent(w, depth)
	fmt.Fprintf(w, "Scan %s", s.RelRef)
	if s.RelRef != s.Sc.Name {
		fmt.Fprintf(w, " (%s)", s.Sc.Name)
	}
	if len(s.Preds) > 0 {
		fmt.Fprintf(w, "  preds=%s", exprList(s.Preds))
	}
	w.WriteString("\n")
}

// Filter là những điều kiện KHÔNG đẩy xuống được: chúng dùng cột của nhiều
// quan hệ, nên chỉ tính được sau khi đã join.
type Filter struct {
	In    Node
	Preds []Expr
}

func (*Filter) node() {}

func (f *Filter) Write(w *strings.Builder, depth int) {
	indent(w, depth)
	fmt.Fprintf(w, "Filter %s\n", exprList(f.Preds))
	f.In.Write(w, depth+1)
}

// Join là inner join. On là các vế nối bằng AND.
type Join struct {
	L, R Node
	On   []Expr
}

func (*Join) node() {}

func (j *Join) Write(w *strings.Builder, depth int) {
	indent(w, depth)
	fmt.Fprintf(w, "Join on=%s\n", exprList(j.On))
	j.L.Write(w, depth+1)
	j.R.Write(w, depth+1)
}

// SortKey là một khoá sắp. Slot >= 0 nghĩa là nó là một CỘT THUẦN, không phải
// biểu thức — và chỉ khi ấy mới có hy vọng bỏ được bước sắp xếp, vì index chỉ
// sắp theo cột chứ không sắp theo biểu thức. (Postgres có index trên biểu
// thức, và đó đúng là cách nó lấy lại được cơ hội này.)
type SortKey struct {
	E    Expr
	Desc bool
	Slot int
	Rel  int
	Idx  int
}

type Sort struct {
	In Node
	By []SortKey
}

func (*Sort) node() {}

func (s *Sort) Write(w *strings.Builder, depth int) {
	indent(w, depth)
	fmt.Fprintf(w, "Sort by=%s\n", sortKeys(s.By))
	s.In.Write(w, depth+1)
}

type Project struct {
	In  Node
	Out []OutCol
}

func (*Project) node() {}

func (p *Project) Write(w *strings.Builder, depth int) {
	indent(w, depth)
	names := make([]string, len(p.Out))
	for i, o := range p.Out {
		names[i] = o.E.String()
	}
	fmt.Fprintf(w, "Project %s\n", strings.Join(names, ", "))
	p.In.Write(w, depth+1)
}

type Limit struct {
	In Node
	N  int64
}

func (*Limit) node() {}

func (l *Limit) Write(w *strings.Builder, depth int) {
	indent(w, depth)
	fmt.Fprintf(w, "Limit %d\n", l.N)
	l.In.Write(w, depth+1)
}

func exprList(es []Expr) string {
	if len(es) == 0 {
		return "-"
	}
	parts := make([]string, len(es))
	for i, e := range es {
		parts[i] = e.String()
	}
	return strings.Join(parts, " AND ")
}

func sortKeys(ks []SortKey) string {
	parts := make([]string, len(ks))
	for i, k := range ks {
		parts[i] = k.E.String()
		if k.Desc {
			parts[i] += " DESC"
		}
	}
	return strings.Join(parts, ", ")
}

// Explain in một cây logical.
func Explain(n Node) string {
	var w strings.Builder
	n.Write(&w, 0)
	return w.String()
}
