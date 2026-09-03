package sql

import (
	"strings"
	"testing"

	"minidb/internal/keys"
)

// sql_test.go kiểm tầng cú pháp MỘT MÌNH: không catalog, không dữ liệu.
//
// Tách được như thế là bằng chứng cho chính lằn ranh mà package này vẽ ra: nếu
// parser cần biết catalog thì mọi bài test ở đây phải mở một database, và một
// bài test cú pháp mở database là dấu hiệu tầng đã bị trộn.

func TestParseRoundTrip(t *testing.T) {
	// In lại câu rồi phân tích lại phải ra cùng một câu. Bất biến này bắt
	// được cả lỗi phân tích lẫn lỗi in, mà không cần viết ra AST mong đợi.
	cases := []string{
		"SELECT a, b FROM t",
		"SELECT * FROM t WHERE (a >= 1)",
		"SELECT t.a FROM t WHERE ((a >= 1) AND (b < 'x'))",
		"SELECT a.x, b.y FROM p a JOIN q b ON (a.id = b.id) WHERE (a.x > 3)",
		"SELECT a FROM t ORDER BY a DESC LIMIT 10",
		"SELECT a FROM t WHERE ((a = 1) OR ((b = 2) AND (c = 3)))",
	}
	for _, src := range cases {
		s, err := Parse(src)
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		sel, ok := s.(*Select)
		if !ok {
			t.Fatalf("%s: ra %T", src, s)
		}
		got := sel.String()
		s2, err := Parse(got)
		if err != nil {
			t.Fatalf("in lại rồi phân tích lại %q: %v", got, err)
		}
		if again := s2.(*Select).String(); again != got {
			t.Fatalf("không bền:\n  lần 1: %s\n  lần 2: %s", got, again)
		}
	}
}

// TestPrecedence: AND phải chặt hơn OR. Nếu sai thì `a OR b AND c` thành
// `(a OR b) AND c` — cùng số token, cùng không có lỗi cú pháp, KHÁC ngữ nghĩa.
// Đây là loại lỗi mà round-trip ở trên không bắt được, vì cả hai cây đều in
// lại và phân tích lại được.
func TestPrecedence(t *testing.T) {
	s, err := Parse("SELECT a FROM t WHERE x = 1 OR y = 2 AND z = 3")
	if err != nil {
		t.Fatal(err)
	}
	want := "((x = 1) OR ((y = 2) AND (z = 3)))"
	if got := s.(*Select).Where.String(); got != want {
		t.Fatalf("độ ưu tiên sai:\n got %s\nwant %s", got, want)
	}
}

// TestConjuncts là tiền đề của predicate pushdown: AND tách được, OR không.
func TestConjuncts(t *testing.T) {
	s, _ := Parse("SELECT a FROM t WHERE x = 1 AND y = 2 AND z = 3")
	if n := len(Conjuncts(s.(*Select).Where)); n != 3 {
		t.Fatalf("AND tách ra %d vế, mong 3", n)
	}
	s, _ = Parse("SELECT a FROM t WHERE x = 1 OR y = 2")
	if n := len(Conjuncts(s.(*Select).Where)); n != 1 {
		t.Fatalf("OR tách ra %d vế, mong 1 — OR KHÔNG được tách,"+
			" nếu tách thì pushdown sẽ đẩy một nửa điều kiện xuống và trả về"+
			" hàng không thoả", n)
	}
}

// TestFlipPutsColumnLeft: `10 < x` phải thành `x > 10`.
//
// Không có phép xoay này thì câu vẫn ĐÚNG — chỉ là không bao giờ dùng được
// index. Một lỗi thuần về hiệu năng, nên không bài test nào về kết quả bắt
// được nó; phải khẳng định trực tiếp hình dạng cây.
func TestFlipPutsColumnLeft(t *testing.T) {
	for _, tc := range []struct{ op, want BinOp }{
		{OpLt, OpGt}, {OpLe, OpGe}, {OpGt, OpLt}, {OpGe, OpLe},
		{OpEq, OpEq}, {OpNe, OpNe},
	} {
		if got := tc.op.Flip(); got != tc.want {
			t.Fatalf("%s.Flip() = %s, mong %s", tc.op, got, tc.want)
		}
	}
}

func TestStringEscape(t *testing.T) {
	s, err := Parse("SELECT a FROM t WHERE x = 'it''s'")
	if err != nil {
		t.Fatal(err)
	}
	lit := s.(*Select).Where.(*BinExpr).R.(*LitExpr)
	if got := string(lit.V.B); got != "it's" {
		t.Fatalf("thoát nháy đơn: %q", got)
	}
}

// TestSyntaxErrorHasPosition: thông báo lỗi phải chỉ được vào cột sai.
//
// Kiểm cả VỊ TRÍ, không chỉ kiểm "có lỗi". Một parser báo lỗi mà không chỉ
// chỗ thì với người gõ tay nó tương đương không báo gì.
func TestSyntaxErrorHasPosition(t *testing.T) {
	const src = "SELECT a FROM t WHERE x = = 1"
	_, err := Parse(src)
	if err == nil {
		t.Fatal("câu sai mà không báo lỗi")
	}
	se, ok := err.(*SyntaxError)
	if !ok {
		t.Fatalf("lỗi kiểu %T, mong *SyntaxError", err)
	}
	if want := strings.LastIndex(src, "="); se.Pos != want {
		t.Fatalf("vị trí lỗi = %d, mong %d\n%v", se.Pos, want, err)
	}
	if !strings.Contains(err.Error(), "^") {
		t.Fatalf("thông báo lỗi không có khung chỉ chỗ:\n%v", err)
	}
}

// TestMisspelledKeywordPointsElsewhere ghi lại một hành vi mà tôi đã ĐOÁN SAI.
//
// Bài test đầu của tôi khẳng định `SELECT a FORM t` báo lỗi tại `FORM` (cột 10).
// Nó đỏ, và parser đúng: `FORM` không phải từ khoá, nên nó là một BÍ DANH của
// cột a (`SELECT a AS FORM`) — câu vẫn hợp cú pháp tới đó, và chỗ đầu tiên
// không đọc được là `t` ở cột 15.
//
// Nguyên nhân sâu hơn nằm ở chính SQL: bí danh KHÔNG CẦN chữ AS. Cái tiện lợi
// ấy làm mọi từ khoá gõ sai bị nuốt thành một bí danh, và lỗi hiện ra muộn một
// token. Postgres cho đúng cùng một thông báo (`syntax error at or near "t"`),
// nên đây không phải khuyết điểm của bản cài này mà là cái giá của một luật
// ngữ pháp — và là lý do thông báo lỗi SQL nổi tiếng khó đọc.
func TestMisspelledKeywordPointsElsewhere(t *testing.T) {
	const src = "SELECT a FORM t"
	_, err := Parse(src)
	if err == nil {
		t.Fatal("câu sai mà không báo lỗi")
	}
	se := err.(*SyntaxError)
	if want := strings.Index(src, "t"); se.Pos == want-6 {
		t.Fatalf("lỗi lại chỉ vào FORM — nếu bí danh giờ CẦN chữ AS thì" +
			" sửa chú thích của bài test này, đừng sửa con số")
	}
	if se.Pos != strings.LastIndex(src, " ")+1 {
		t.Fatalf("vị trí lỗi = %d, mong trỏ vào %q\n%v", se.Pos, "t", err)
	}
}

func TestErrors(t *testing.T) {
	bad := []string{
		"SELECT",
		"SELECT a FROM",
		"SELECT a FROM t WHERE",
		"SELECT a FROM t WHERE x =",
		"CREATE TABLE t (a INT)",                    // thiếu PRIMARY KEY
		"CREATE TABLE t (a FLOAT, PRIMARY KEY (a))", // kiểu chưa hỗ trợ
		"SELECT a FROM t WHERE x = 1.5",             // số thực
		"SELECT a FROM t WHERE x = 'chưa đóng",
		"INSERT INTO t VALUES",
		"SELECT a FROM t ORDER BY",
		"SELECT a FROM t LIMIT x",
		"SELECT a FROM t; SELECT",
	}
	for _, src := range bad {
		if _, err := Parse(src); err == nil {
			t.Errorf("%q: không báo lỗi", src)
		}
	}
}

// TestKeywordNotIdent: từ khoá không được làm tên, và thông báo phải nói rõ
// điều đó — đây là lỗi người dùng gặp nhiều nhất khi có cột tên `key`/`order`.
func TestKeywordNotIdent(t *testing.T) {
	_, err := Parse("SELECT a FROM order")
	if err == nil {
		t.Fatal("dùng từ khoá làm tên bảng mà không báo lỗi")
	}
	if !strings.Contains(err.Error(), "từ khoá") {
		t.Fatalf("thông báo không nói vì sao:\n%v", err)
	}
}

func TestParseDDL(t *testing.T) {
	s, err := Parse("CREATE TABLE ev (id UINT, kind INT, city TEXT, PRIMARY KEY (id, kind))")
	if err != nil {
		t.Fatal(err)
	}
	ct := s.(*CreateTable)
	if len(ct.Cols) != 3 || len(ct.PK) != 2 {
		t.Fatalf("%d cột, pk %d cột", len(ct.Cols), len(ct.PK))
	}
	if ct.Cols[0].Type != keys.TypeUint || ct.Cols[2].Type != keys.TypeBytes {
		t.Fatalf("kiểu cột sai: %v", ct.Cols)
	}

	s, err = Parse("CREATE UNIQUE INDEX ix ON ev (kind, city DESC)")
	if err != nil {
		t.Fatal(err)
	}
	ci := s.(*CreateIndex)
	if !ci.Unique || len(ci.Cols) != 2 || !ci.Cols[1].Desc {
		t.Fatalf("%+v", ci)
	}
}

func TestParseInsertManyRows(t *testing.T) {
	s, err := Parse("INSERT INTO t (a, b) VALUES (1, 'x'), (2, 'y'), (3, NULL)")
	if err != nil {
		t.Fatal(err)
	}
	ins := s.(*Insert)
	if len(ins.Cols) != 2 || len(ins.Rows) != 3 {
		t.Fatalf("%d cột, %d hàng", len(ins.Cols), len(ins.Rows))
	}
	if !ins.Rows[2][1].(*LitExpr).V.IsNull() {
		t.Fatal("NULL không thành TypeNull")
	}
}

func TestParseMany(t *testing.T) {
	stmts, err := ParseMany(`
		-- chú thích bị bỏ qua
		CREATE TABLE t (a INT, PRIMARY KEY (a));
		INSERT INTO t VALUES (1);
		SELECT * FROM t;
	`)
	if err != nil {
		t.Fatal(err)
	}
	if len(stmts) != 3 {
		t.Fatalf("%d câu, mong 3", len(stmts))
	}
}

func TestStarForms(t *testing.T) {
	s, err := Parse("SELECT *, t.*, t.a AS x, b y FROM t JOIN u b ON (t.a = b.a)")
	if err != nil {
		t.Fatal(err)
	}
	sel := s.(*Select)
	if len(sel.Cols) != 4 {
		t.Fatalf("%d cột kết quả", len(sel.Cols))
	}
	if !sel.Cols[0].Star || sel.Cols[0].StarTable != "" {
		t.Fatalf("cột 0: %+v", sel.Cols[0])
	}
	if !sel.Cols[1].Star || sel.Cols[1].StarTable != "t" {
		t.Fatalf("cột 1: %+v", sel.Cols[1])
	}
	if sel.Cols[2].Alias != "x" || sel.Cols[3].Alias != "y" {
		t.Fatalf("bí danh: %+v %+v", sel.Cols[2], sel.Cols[3])
	}
}
