package sql

import (
	"fmt"
	"strings"

	"minidb/internal/keys"
)

// ast.go là cây cú pháp. Nó mô tả CÂU NGƯỜI GÕ, không mô tả việc sẽ làm.
//
// Chỗ dễ nhầm nhất khi lần đầu viết một front-end SQL là gộp AST với logical
// plan. Chúng khác nhau ở một điểm không thể thoả hiệp: AST giữ nguyên hình
// người gõ (kể cả những chỗ dư thừa, kể cả thứ tự JOIN người ta viết ra), còn
// logical plan là hình đã được phép VIẾT LẠI. Trộn hai thứ ấy thì mọi phép
// biến đổi của optimizer đều biến thành "sửa câu của người dùng", và lúc cần
// in lại câu gốc trong một thông báo lỗi thì không còn câu gốc nữa.

// ---------- câu lệnh ----------

type Stmt interface{ stmt() }

// CreateTable: CREATE TABLE t (a INT, b TEXT, PRIMARY KEY (a))
type CreateTable struct {
	Name string
	Cols []ColDef
	PK   []string
}

// ColDef là một cột trong CREATE TABLE. Chưa có NOT NULL / DEFAULT / kiểu độ
// dài giới hạn — xem nợ cuối phase.
type ColDef struct {
	Name string
	Type keys.Type
	Pos  int
}

// CreateIndex: CREATE [UNIQUE] INDEX ix ON t (a, b DESC)
type CreateIndex struct {
	Name   string
	Table  string
	Cols   []IndexCol
	Unique bool
}

type IndexCol struct {
	Name string
	Desc bool
}

// Insert: INSERT INTO t (a, b) VALUES (1, 'x'), (2, 'y')
//
// Nhiều hàng trong một câu là có chủ ý, không phải tiện lợi: phase 7 đo được
// rằng một transaction mỗi hàng thì 99.7% thời gian là fsync, nên nếu câu
// INSERT chỉ nhận được một hàng thì cái giá của một lần nạp dữ liệu bằng SQL
// hoàn toàn không đo được — chỉ đo được fsync.
type Insert struct {
	Table string
	Cols  []string // rỗng = theo đúng thứ tự cột của bảng
	Rows  [][]Expr
	Pos   int
}

// Select: SELECT ... FROM t [alias] [JOIN u ON ...] [WHERE] [ORDER BY] [LIMIT]
type Select struct {
	Cols    []ResultCol
	From    TableRef
	Joins   []Join
	Where   Expr
	OrderBy []OrderItem
	Limit   int64 // -1 = không giới hạn
}

// ResultCol: một cột kết quả. Star = `*` hoặc `t.*`.
//
// Star được giữ NGUYÊN HÌNH trong AST thay vì mở ra ngay lúc phân tích, vì mở
// ra cần catalog — mà parser không được biết catalog. Nó cũng là chỗ phase 7
// đã đo: `SELECT *` phá hết khả năng phủ của mọi index.
type ResultCol struct {
	Star      bool
	StarTable string // "t" trong `t.*`
	E         Expr
	Alias     string
}

type TableRef struct {
	Name  string
	Alias string
	Pos   int
}

// Name của quan hệ khi được tham chiếu: alias thắng tên thật, đúng như SQL.
func (r TableRef) Ref() string {
	if r.Alias != "" {
		return r.Alias
	}
	return r.Name
}

// Join hiện chỉ có INNER JOIN ... ON. Không LEFT/RIGHT/FULL, không USING,
// không NATURAL, không dấu phẩy kiểu cũ — xem nợ cuối phase. Lý do dừng ở
// INNER: câu hỏi của phase là "nested loop vs hash join", và outer join không
// thêm gì cho câu hỏi ấy ngoài một đống ca biên.
type Join struct {
	Right TableRef
	On    Expr
	Pos   int
}

type OrderItem struct {
	E    Expr
	Desc bool
}

// Explain in ra kế hoạch. Analyze = chạy thật rồi in kèm số ĐO ĐƯỢC cạnh số
// ƯỚC LƯỢNG — đúng nghĩa EXPLAIN ANALYZE của Postgres, và khoảng cách giữa
// hai cột ấy là thứ phase 7 đã chứng minh là nguồn của mọi kế hoạch tồi.
type Explain struct {
	Stmt    Stmt
	Analyze bool
}

// AnalyzeStmt: ANALYZE t — sinh thống kê cho planner.
type AnalyzeStmt struct{ Table string }

func (*CreateTable) stmt() {}
func (*CreateIndex) stmt() {}
func (*Insert) stmt()      {}
func (*Select) stmt()      {}
func (*Explain) stmt()     {}
func (*AnalyzeStmt) stmt() {}

// ---------- biểu thức ----------

type Expr interface {
	expr()
	String() string
}

// Op là toán tử hai toán hạng.
type BinOp uint8

const (
	OpEq BinOp = iota
	OpNe
	OpLt
	OpLe
	OpGt
	OpGe
	OpAnd
	OpOr
)

var opText = [...]string{"=", "<>", "<", "<=", ">", ">=", "AND", "OR"}

func (o BinOp) String() string { return opText[o] }

// IsCompare: toán tử so sánh (sinh ra bool từ hai giá trị) khác toán tử logic
// (nhận bool). Phân biệt ở đây để planner biết cái nào có thể thành một
// khoảng quét trên index và cái nào chỉ có thể thành một Filter.
func (o BinOp) IsCompare() bool { return o <= OpGe }

// Flip đảo chiều so sánh: dùng khi phải xoay `10 < x` thành `x > 10` để cột
// nằm bên trái. Không có phép xoay này thì `WHERE 10 < x` không bao giờ dùng
// được index — một lỗi im lặng về HIỆU NĂNG, loại lỗi khó thấy nhất.
func (o BinOp) Flip() BinOp {
	switch o {
	case OpLt:
		return OpGt
	case OpLe:
		return OpGe
	case OpGt:
		return OpLt
	case OpGe:
		return OpLe
	}
	return o // = và <> đối xứng
}

// ColRefExpr là một tên cột như người gõ: có thể có tiền tố bảng, chưa phân
// giải. Việc phân giải thuộc internal/plan.
type ColRefExpr struct {
	Table string
	Name  string
	Pos   int
}

// LitExpr là hằng. Giá trị đã là keys.Value — cùng một biểu diễn với dữ liệu
// trong cây, nên so sánh hằng với cột không cần chuyển kiểu ở giữa.
type LitExpr struct {
	V   keys.Value
	Pos int
}

type BinExpr struct {
	Op   BinOp
	L, R Expr
	Pos  int
}

func (*ColRefExpr) expr() {}
func (*LitExpr) expr()    {}
func (*BinExpr) expr()    {}

func (e *ColRefExpr) String() string {
	if e.Table != "" {
		return e.Table + "." + e.Name
	}
	return e.Name
}

func (e *LitExpr) String() string {
	if e.V.T == keys.TypeBytes {
		return quoteStr(string(e.V.B))
	}
	return e.V.String()
}

// quoteStr in một hằng chuỗi ở dạng PHÂN TÍCH LẠI ĐƯỢC: dấu nháy đơn bên trong
// phải nhân đôi, đúng phép thoát mà lexer nhận vào.
//
// Thiếu nó là con bug FuzzParse tìm ra trong 13 giây đầu của lần chạy đầu tiên:
// bốn dấu nháy liền nhau (tức một hằng chuỗi chứa MỘT dấu nháy) in ra thành ba
// dấu nháy, và câu in ra không phân tích lại được. Bất biến bị vỡ là bất biến
// in-lại-rồi-đọc-lại, và không một bài test viết tay nào trong bộ này chạm tới.
//
// Cùng một hình dạng với phép thoát 0x00 -> 0x00 0xff của internal/keys ở phase
// 7: ai viết bộ mã hoá phải viết phép thoát, và nửa dễ quên luôn là nửa GHI RA,
// vì nửa đọc vào sai một cái là lỗi ngay, còn nửa ghi ra thì sai lặng lẽ cho
// tới khi có ai đọc lại.
func quoteStr(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func (e *BinExpr) String() string {
	return fmt.Sprintf("(%s %s %s)", e.L, e.Op, e.R)
}

// Conjuncts tách một biểu thức thành các vế nối bằng AND.
//
// Đây là bước đầu của predicate pushdown, và nó là lý do AND phải được xử lý
// khác OR: `a AND b` cho phép đẩy a xuống một bảng và b xuống bảng khác, còn
// `a OR b` thì KHÔNG đẩy được vế nào — muốn đẩy phải chứng minh cả hai vế chỉ
// dùng cột của cùng một bảng. Bất đối xứng ấy có mặt trong mọi optimizer.
func Conjuncts(e Expr) []Expr {
	if b, ok := e.(*BinExpr); ok && b.Op == OpAnd {
		return append(Conjuncts(b.L), Conjuncts(b.R)...)
	}
	if e == nil {
		return nil
	}
	return []Expr{e}
}

// String in lại câu SELECT gần với hình người gõ. Dùng trong EXPLAIN và trong
// thông báo lỗi.
func (s *Select) String() string {
	var b strings.Builder
	b.WriteString("SELECT ")
	for i, c := range s.Cols {
		if i > 0 {
			b.WriteString(", ")
		}
		switch {
		case c.Star && c.StarTable != "":
			b.WriteString(c.StarTable + ".*")
		case c.Star:
			b.WriteString("*")
		default:
			b.WriteString(c.E.String())
			if c.Alias != "" {
				b.WriteString(" AS " + c.Alias)
			}
		}
	}
	b.WriteString(" FROM " + s.From.Name)
	if s.From.Alias != "" {
		b.WriteString(" " + s.From.Alias)
	}
	for _, j := range s.Joins {
		b.WriteString(" JOIN " + j.Right.Name)
		if j.Right.Alias != "" {
			b.WriteString(" " + j.Right.Alias)
		}
		b.WriteString(" ON " + j.On.String())
	}
	if s.Where != nil {
		b.WriteString(" WHERE " + s.Where.String())
	}
	if len(s.OrderBy) > 0 {
		b.WriteString(" ORDER BY ")
		for i, o := range s.OrderBy {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(o.E.String())
			if o.Desc {
				b.WriteString(" DESC")
			}
		}
	}
	if s.Limit >= 0 {
		fmt.Fprintf(&b, " LIMIT %d", s.Limit)
	}
	return b.String()
}
