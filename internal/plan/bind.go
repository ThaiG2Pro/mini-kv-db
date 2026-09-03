// Package plan là tầng NGỮ NGHĨA và TỐI ƯU: AST -> logical plan -> physical
// plan. Nó biết catalog, biết kiểu, biết thống kê, biết chi phí — và không
// biết cách chạy (đó là internal/exec).
//
// Ba việc, tách rời có chủ ý:
//
//	bind.go      phân giải tên và kiểu; câu sai tên chết ở đây, không tốn
//	             một lần xuống cây nào
//	opt.go       viết lại LOGICAL: đẩy điều kiện xuống. Đúng bất kể chạy thế nào
//	physical.go  chọn đường đi VẬT LÝ: index nào, join thuật toán nào, có bỏ
//	             được bước sắp xếp không. Chỉ đúng cho một cách chạy cụ thể
//
// Lằn ranh giữa opt.go và physical.go là câu trả lời cho câu hỏi mở đầu phase
// 8: **predicate pushdown là phép viết lại LOGICAL** (đẩy `a.x > 10` xuống sát
// bảng a đúng dù sau đó chạy bằng seq scan, index scan hay hash join), còn
// **bỏ bước ORDER BY là quyết định VẬT LÝ** (chỉ bỏ được nếu đường đi đã chọn
// tình cờ trả về đúng thứ tự ấy). Một cái là định lý, một cái là tình huống.
package plan

import (
	"fmt"
	"math"
	"strings"

	"minidb/internal/keys"
	"minidb/internal/sql"
	"minidb/internal/table"
)

// Rel là một quan hệ đã phân giải: một bảng cùng bí danh của nó trong câu.
//
// Base là chỗ cột của quan hệ này bắt đầu trong TUPLE PHẲNG. Cả cây toán tử
// truyền đi đúng một []keys.Value là nối đuôi mọi quan hệ, và mỗi cột là một
// chỉ số cố định trong đó. Không dùng map tên->giá trị: một hash mỗi cột mỗi
// hàng là cái giá không đáng, và Postgres cũng dùng đúng cách này (TupleSlot
// cộng attnum).
type Rel struct {
	Ref  string // tên dùng để tham chiếu: bí danh nếu có, không thì tên bảng
	Sc   *table.Schema
	Base int
}

// Bound là một câu SELECT đã phân giải xong: cây logical, danh sách quan hệ,
// và bề rộng của tuple phẳng.
type Bound struct {
	Rels  []Rel
	Width int
	Root  Node
	Out   []OutCol
	Src   *sql.Select
}

// OutCol là một cột kết quả đã phân giải.
type OutCol struct {
	E    Expr
	Name string
}

// ---------- biểu thức đã phân giải ----------

// Expr là biểu thức đã gắn kiểu và gắn CHỖ (slot), tính được trên một tuple
// phẳng.
type Expr interface {
	Eval(row []keys.Value) (keys.Value, error)
	Type() keys.Type
	Refs() uint64 // bitmask quan hệ mà biểu thức này dùng
	String() string
}

// ColExpr đọc một cột. Slot là chỉ số trong tuple phẳng — đã cộng Base.
type ColExpr struct {
	Slot int
	T    keys.Type
	Rel  int
	Idx  int
	Name string
}

func (c *ColExpr) Eval(row []keys.Value) (keys.Value, error) { return row[c.Slot], nil }
func (c *ColExpr) Type() keys.Type                           { return c.T }
func (c *ColExpr) Refs() uint64                              { return 1 << uint(c.Rel) }
func (c *ColExpr) String() string                            { return c.Name }

// LitExpr là hằng, đã ép về kiểu của cột nó được so với.
type LitExpr struct{ V keys.Value }

func (l *LitExpr) Eval([]keys.Value) (keys.Value, error) { return l.V, nil }
func (l *LitExpr) Type() keys.Type                       { return l.V.T }
func (l *LitExpr) Refs() uint64                          { return 0 }
func (l *LitExpr) String() string {
	if l.V.T == keys.TypeBytes {
		// Cùng phép thoát như sql.LitExpr: EXPLAIN in ra câu mà người ta chép
		// lại để chạy, nên nó phải chép lại được.
		return "'" + strings.ReplaceAll(string(l.V.B), "'", "''") + "'"
	}
	return l.V.String()
}

// CmpExpr là một phép so sánh. Nó trả về TypeTrue / TypeFalse / TypeNull —
// logic BA GIÁ TRỊ, không phải bool.
//
// Đây là chỗ ngữ nghĩa SQL và bộ mã hoá của phase 7 KHÁC NHAU, và trộn chúng
// là một con bug im lặng:
//
//	keys.Compare(Null, Null) == 0     — vì để SẮP THỨ TỰ thì NULL phải có chỗ
//	SQL: NULL = NULL  ->  NULL        — vì để SO SÁNH thì NULL là "không biết"
//
// Cùng một cặp giá trị, hai câu trả lời, và cả hai đều đúng trong địa hạt của
// mình. Bộ mã hoá cần NULL sắp được (nếu không thì không có index nào chứa
// NULL); SQL cần NULL lan truyền (nếu không thì `WHERE x = NULL` trả về hàng,
// và đó là lý do SQL phải có `IS NULL` như một toán tử riêng).
type CmpExpr struct {
	Op   sql.BinOp
	L, R Expr
}

func (c *CmpExpr) Type() keys.Type { return keys.TypeTrue }
func (c *CmpExpr) Refs() uint64    { return c.L.Refs() | c.R.Refs() }
func (c *CmpExpr) String() string  { return fmt.Sprintf("(%s %s %s)", c.L, c.Op, c.R) }

func (c *CmpExpr) Eval(row []keys.Value) (keys.Value, error) {
	l, err := c.L.Eval(row)
	if err != nil {
		return keys.Null(), err
	}
	r, err := c.R.Eval(row)
	if err != nil {
		return keys.Null(), err
	}
	if l.IsNull() || r.IsNull() {
		return keys.Null(), nil // "không biết", không phải false
	}
	n := keys.Compare(l, r)
	switch c.Op {
	case sql.OpEq:
		return keys.Bool(n == 0), nil
	case sql.OpNe:
		return keys.Bool(n != 0), nil
	case sql.OpLt:
		return keys.Bool(n < 0), nil
	case sql.OpLe:
		return keys.Bool(n <= 0), nil
	case sql.OpGt:
		return keys.Bool(n > 0), nil
	case sql.OpGe:
		return keys.Bool(n >= 0), nil
	}
	return keys.Null(), fmt.Errorf("plan: toán tử so sánh lạ %v", c.Op)
}

// LogicExpr là AND/OR trên logic ba giá trị.
type LogicExpr struct {
	Op   sql.BinOp
	L, R Expr
}

func (e *LogicExpr) Type() keys.Type { return keys.TypeTrue }
func (e *LogicExpr) Refs() uint64    { return e.L.Refs() | e.R.Refs() }
func (e *LogicExpr) String() string  { return fmt.Sprintf("(%s %s %s)", e.L, e.Op, e.R) }

// Eval theo bảng chân lý ba giá trị. Chú ý AND có ĐOẢN MẠCH ở FALSE và OR ở
// TRUE, nhưng KHÔNG đoản mạch ở NULL: `NULL AND FALSE` là FALSE, nên gặp NULL
// vẫn phải xét tiếp vế kia. Đó là chỗ một bản cài "coi NULL như false" sẽ ra
// đúng kết quả và vẫn sai ngữ nghĩa — cho tới khi có NOT ở trên.
func (e *LogicExpr) Eval(row []keys.Value) (keys.Value, error) {
	l, err := e.L.Eval(row)
	if err != nil {
		return keys.Null(), err
	}
	if e.Op == sql.OpAnd && l.T == keys.TypeFalse {
		return keys.Bool(false), nil
	}
	if e.Op == sql.OpOr && l.T == keys.TypeTrue {
		return keys.Bool(true), nil
	}
	r, err := e.R.Eval(row)
	if err != nil {
		return keys.Null(), err
	}
	if e.Op == sql.OpAnd {
		switch {
		case r.T == keys.TypeFalse:
			return keys.Bool(false), nil
		case l.IsNull() || r.IsNull():
			return keys.Null(), nil
		default:
			return keys.Bool(true), nil
		}
	}
	switch {
	case r.T == keys.TypeTrue:
		return keys.Bool(true), nil
	case l.IsNull() || r.IsNull():
		return keys.Null(), nil
	default:
		return keys.Bool(false), nil
	}
}

// True là phép thử của WHERE và của ON: chỉ TRUE mới lọt.
//
// NULL không lọt, và điều đó không đối xứng với NOT: `WHERE x > 1` và
// `WHERE NOT (x > 1)` cùng bỏ hàng có x IS NULL, nên hợp của hai truy vấn
// không phải cả bảng. Đây là chỗ SQL làm người ta ngạc nhiên nhiều nhất, và
// nó là hệ quả trực tiếp của dòng này.
func True(v keys.Value) bool { return v.T == keys.TypeTrue }

// ---------- phân giải ----------

// scope là bảng tên trong lúc bind.
type scope struct {
	rels []Rel
	// byName: tên tham chiếu -> chỉ số quan hệ. Trùng tên là lỗi, không phải
	// "cái sau thắng": `FROM t JOIN t` mà không có bí danh thì mọi `t.x` đều
	// nhập nhằng, và im lặng chọn một bên là con bug tệ nhất có thể.
	byName map[string]int
}

func (s *scope) resolve(tbl, col string, pos int) (*ColExpr, error) {
	if tbl != "" {
		i, ok := s.byName[tbl]
		if !ok {
			return nil, fmt.Errorf("không có quan hệ %q trong FROM", tbl)
		}
		return s.colOf(i, col, tbl)
	}
	// Không có tiền tố: tìm trong mọi quan hệ, và ĐÒI đúng một chỗ khớp.
	var found *ColExpr
	for i := range s.rels {
		c, err := s.colOf(i, col, s.rels[i].Ref)
		if err != nil {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("cột %q nhập nhằng: có trong cả %s và %s",
				col, found.Name[:strings.IndexByte(found.Name, '.')], s.rels[i].Ref)
		}
		found = c
	}
	if found == nil {
		return nil, fmt.Errorf("không có cột %q trong các bảng của FROM", col)
	}
	return found, nil
}

func (s *scope) colOf(i int, col, ref string) (*ColExpr, error) {
	r := s.rels[i]
	idx, ok := r.Sc.ColIndex(col)
	if !ok {
		return nil, fmt.Errorf("bảng %s không có cột %q", r.Sc.Name, col)
	}
	return &ColExpr{Slot: r.Base + idx, T: r.Sc.Cols[idx].T, Rel: i, Idx: idx,
		Name: ref + "." + col}, nil
}

// CoerceValue ép một hằng về kiểu của cột nó được so với.
//
// Làm ở đây, KHÔNG làm ở parser: parser không biết kiểu cột. Nếu để nguyên thì
// `WHERE u = 5` với u kiểu UINT sẽ so TypeUint với TypeInt, mà keys.Compare so
// TAG trước — nên phép so luôn ra cùng một chiều bất kể giá trị, và truy vấn
// im lặng trả về sai. Đúng cái loại lỗi mà một bài test với đúng một kiểu cột
// sẽ không bao giờ bắt được.
func CoerceValue(v keys.Value, t keys.Type) (keys.Value, error) {
	if v.IsNull() {
		return v, nil
	}
	switch t {
	case keys.TypeInt:
		switch v.T {
		case keys.TypeInt:
			return v, nil
		case keys.TypeUint:
			if v.U > math.MaxInt64 {
				return v, fmt.Errorf("%v không vừa kiểu INT", v)
			}
			return keys.Int(int64(v.U)), nil
		}
	case keys.TypeUint:
		switch v.T {
		case keys.TypeUint:
			return v, nil
		case keys.TypeInt:
			if v.I < 0 {
				return v, fmt.Errorf("%v âm, không vào được cột UINT", v)
			}
			return keys.Uint(uint64(v.I)), nil
		}
	case keys.TypeBytes:
		if v.T == keys.TypeBytes {
			return v, nil
		}
	case keys.TypeTrue, keys.TypeFalse:
		if v.T == keys.TypeTrue || v.T == keys.TypeFalse {
			return v, nil
		}
	}
	return v, fmt.Errorf("không so được %s với cột kiểu %s", v.T, t)
}

// bindExpr phân giải một biểu thức của AST.
func (s *scope) bindExpr(e sql.Expr) (Expr, error) {
	switch x := e.(type) {
	case *sql.ColRefExpr:
		return s.resolve(x.Table, x.Name, x.Pos)

	case *sql.LitExpr:
		return &LitExpr{V: x.V}, nil

	case *sql.BinExpr:
		l, err := s.bindExpr(x.L)
		if err != nil {
			return nil, err
		}
		r, err := s.bindExpr(x.R)
		if err != nil {
			return nil, err
		}
		if !x.Op.IsCompare() {
			return &LogicExpr{Op: x.Op, L: l, R: r}, nil
		}
		// Ép hằng theo cột. Nếu hằng nằm bên TRÁI thì xoay cả phép so lại cho
		// cột về bên trái — nếu không thì `WHERE 10 < x` sẽ không bao giờ
		// dùng được index, một lỗi chỉ về hiệu năng nên không test nào đỏ.
		op := x.Op
		lc, lIsCol := l.(*ColExpr)
		rc, rIsCol := r.(*ColExpr)
		switch {
		case lIsCol && !rIsCol:
			lit, ok := r.(*LitExpr)
			if !ok {
				break
			}
			v, err := CoerceValue(lit.V, lc.T)
			if err != nil {
				return nil, err
			}
			r = &LitExpr{V: v}
		case rIsCol && !lIsCol:
			lit, ok := l.(*LitExpr)
			if !ok {
				break
			}
			v, err := CoerceValue(lit.V, rc.T)
			if err != nil {
				return nil, err
			}
			l, r = r, &LitExpr{V: v}
			op = op.Flip()
		case lIsCol && rIsCol:
			if lc.T != rc.T {
				return nil, fmt.Errorf("không so được %s (%s) với %s (%s)",
					lc.Name, lc.T, rc.Name, rc.T)
			}
		}
		return &CmpExpr{Op: op, L: l, R: r}, nil
	}
	return nil, fmt.Errorf("plan: biểu thức lạ %T", e)
}

// ---------- dựng logical plan ----------

// BindSelect phân giải một câu SELECT thành cây logical.
//
// Thứ tự dựng theo đúng thứ tự NGỮ NGHĨA của SQL, không theo thứ tự người gõ:
//
//	FROM/JOIN -> WHERE -> ORDER BY -> SELECT -> LIMIT
//
// Chú ý ORDER BY nằm TRƯỚC SELECT trong cây (Sort ở dưới Project): `ORDER BY`
// được phép dùng cột KHÔNG có trong danh sách chọn, nên nếu Project chạy trước
// thì cột để sắp đã bị bỏ đi rồi. Đây là lý do một engine không thể coi
// SELECT là "bước đầu tiên" dù nó là chữ đầu tiên của câu.
func BindSelect(c *table.Catalog, s *sql.Select) (*Bound, error) {
	sc := &scope{byName: map[string]int{}}
	add := func(r sql.TableRef) error {
		t, err := c.Table(r.Name)
		if err != nil {
			return err
		}
		ref := r.Ref()
		if _, dup := sc.byName[ref]; dup {
			return fmt.Errorf("tên %q xuất hiện hai lần trong FROM — cần bí danh (AS)", ref)
		}
		base := 0
		if n := len(sc.rels); n > 0 {
			last := sc.rels[n-1]
			base = last.Base + len(last.Sc.Cols)
		}
		sc.byName[ref] = len(sc.rels)
		sc.rels = append(sc.rels, Rel{Ref: ref, Sc: t, Base: base})
		return nil
	}
	if err := add(s.From); err != nil {
		return nil, err
	}
	for _, j := range s.Joins {
		if err := add(j.Right); err != nil {
			return nil, err
		}
	}
	if len(sc.rels) > 2 {
		return nil, fmt.Errorf("phase 8 chỉ join hai bảng (thấy %d) — thứ tự join"+
			" nhiều bảng là bài toán khác, xem nợ P8-3", len(sc.rels))
	}

	b := &Bound{Rels: sc.rels, Src: s}
	last := sc.rels[len(sc.rels)-1]
	b.Width = last.Base + len(last.Sc.Cols)

	// FROM/JOIN
	var root Node = &Scan{Rel: 0, Sc: sc.rels[0].Sc, RelRef: sc.rels[0].Ref}
	for i, j := range s.Joins {
		right := &Scan{Rel: i + 1, Sc: sc.rels[i+1].Sc, RelRef: sc.rels[i+1].Ref}
		// Điều kiện ON được tách theo AND ngay tại đây, cùng lý do như WHERE:
		// một vế của ON có thể chỉ dùng cột của MỘT bảng (`ON a.id=b.id AND
		// b.kind=3`), và vế ấy không phải điều kiện join — nó là một điều kiện
		// lọc bị viết lẫn vào ON, và optimizer phải được quyền đẩy nó xuống.
		var on []Expr
		for _, cj := range sql.Conjuncts(j.On) {
			e, err := sc.bindExpr(cj)
			if err != nil {
				return nil, err
			}
			on = append(on, e)
		}
		root = &Join{L: root, R: right, On: on}
	}

	// WHERE
	if s.Where != nil {
		var preds []Expr
		for _, cj := range sql.Conjuncts(s.Where) {
			p, err := sc.bindExpr(cj)
			if err != nil {
				return nil, err
			}
			preds = append(preds, p)
		}
		root = &Filter{In: root, Preds: preds}
	}

	// ORDER BY
	if len(s.OrderBy) > 0 {
		var by []SortKey
		for _, o := range s.OrderBy {
			e, err := sc.bindExpr(o.E)
			if err != nil {
				return nil, err
			}
			k := SortKey{E: e, Desc: o.Desc, Slot: -1}
			if ce, ok := e.(*ColExpr); ok {
				k.Slot, k.Rel, k.Idx = ce.Slot, ce.Rel, ce.Idx
			}
			by = append(by, k)
		}
		root = &Sort{In: root, By: by}
	}

	// SELECT
	out, err := sc.bindOut(s.Cols)
	if err != nil {
		return nil, err
	}
	b.Out = out
	root = &Project{In: root, Out: out}

	// LIMIT
	if s.Limit >= 0 {
		root = &Limit{In: root, N: s.Limit}
	}
	b.Root = root
	return b, nil
}

// bindOut mở `*` và `t.*` ra thành danh sách cột thật.
//
// Mở ở ĐÂY, không ở parser, vì cần catalog. Và mở ra ngay lúc bind (chứ không
// để tới lúc chạy) là điều kiện để planner biết cột nào CẦN — mà đó chính là
// thứ quyết định index có phủ được truy vấn không. Nên `SELECT *` không chỉ
// trả về nhiều cột hơn: nó xoá luôn khả năng index-only scan, và phase 7 đã
// đo cái giá đó.
func (s *scope) bindOut(cols []sql.ResultCol) ([]OutCol, error) {
	var out []OutCol
	for _, rc := range cols {
		if !rc.Star {
			e, err := s.bindExpr(rc.E)
			if err != nil {
				return nil, err
			}
			name := rc.Alias
			if name == "" {
				name = rc.E.String()
			}
			out = append(out, OutCol{E: e, Name: name})
			continue
		}
		for i := range s.rels {
			if rc.StarTable != "" && s.rels[i].Ref != rc.StarTable {
				continue
			}
			r := s.rels[i]
			for j, c := range r.Sc.Cols {
				out = append(out, OutCol{
					E:    &ColExpr{Slot: r.Base + j, T: c.T, Rel: i, Idx: j, Name: r.Ref + "." + c.Name},
					Name: c.Name,
				})
			}
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("%s.* không khớp quan hệ nào", rc.StarTable)
		}
	}
	return out, nil
}
