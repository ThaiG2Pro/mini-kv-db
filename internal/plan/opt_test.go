package plan

import (
	"strings"
	"testing"

	"minidb/internal/keys"
	"minidb/internal/sql"
	"minidb/internal/table"
)

// opt_test.go kiểm phép đẩy điều kiện xuống trên cây dựng BẰNG TAY — không
// catalog, không dữ liệu, không transaction.
//
// Tách được như thế là bằng chứng cho chính lằn ranh mà opt.go tuyên bố:
// pushdown là một phép biến đổi trên TẬP HỢP, đúng bất kể chạy thế nào. Nếu
// bài test của nó cần mở một database thì lời tuyên bố ấy là sai.

func col(rel, idx int, name string) *ColExpr {
	return &ColExpr{Slot: rel*10 + idx, T: keys.TypeInt, Rel: rel, Idx: idx, Name: name}
}

func lit(n int64) *LitExpr { return &LitExpr{V: keys.Int(n)} }

func cmp(op sql.BinOp, l, r Expr) *CmpExpr { return &CmpExpr{Op: op, L: l, R: r} }

func scan(rel int, ref string) *Scan {
	return &Scan{Rel: rel, RelRef: ref, Sc: &table.Schema{Name: ref,
		Cols: []table.Column{{Name: "a"}, {Name: "b"}}, PK: []int{0}}}
}

func TestPushdownSingleRelation(t *testing.T) {
	in := &Filter{In: scan(0, "t"), Preds: []Expr{cmp(sql.OpLt, col(0, 1, "t.b"), lit(10))}}
	out := Optimize(in)
	s, ok := out.(*Scan)
	if !ok {
		t.Fatalf("Filter trên một bảng phải TAN vào Scan, còn lại %T:\n%s", out, Explain(out))
	}
	if len(s.Preds) != 1 {
		t.Fatalf("Scan giữ %d điều kiện", len(s.Preds))
	}
}

// TestPushdownSplitsAcrossJoin: mỗi vế của AND về đúng bảng của nó, và vế nào
// dùng cả hai bảng thì ở lại làm điều kiện join.
func TestPushdownSplitsAcrossJoin(t *testing.T) {
	j := &Join{L: scan(0, "a"), R: scan(1, "b"),
		On: []Expr{cmp(sql.OpEq, col(0, 0, "a.id"), col(1, 0, "b.id"))}}
	in := &Filter{In: j, Preds: []Expr{
		cmp(sql.OpLt, col(0, 1, "a.x"), lit(10)),
		cmp(sql.OpGt, col(1, 1, "b.y"), lit(5)),
	}}
	out := Optimize(in)
	jo, ok := out.(*Join)
	if !ok {
		t.Fatalf("còn lại %T, mong Join trần (mọi điều kiện đã xuống):\n%s", out, Explain(out))
	}
	l := jo.L.(*Scan)
	r := jo.R.(*Scan)
	if len(l.Preds) != 1 || len(r.Preds) != 1 {
		t.Fatalf("a giữ %d, b giữ %d — mong 1 và 1:\n%s", len(l.Preds), len(r.Preds), Explain(out))
	}
	if len(jo.On) != 1 {
		t.Fatalf("điều kiện join còn %d vế", len(jo.On))
	}
}

// TestPushdownFromOnClause: `ON a.id=b.id AND b.kind=3` — vế thứ hai KHÔNG
// phải điều kiện join, nó là một phép lọc bị viết lẫn vào ON, và với inner
// join thì optimizer được quyền đẩy nó xuống.
//
// (Với LEFT JOIN thì không được — đó là một trong những lý do phase 8 dừng ở
// inner join.)
func TestPushdownFromOnClause(t *testing.T) {
	j := &Join{L: scan(0, "a"), R: scan(1, "b"), On: []Expr{
		cmp(sql.OpEq, col(0, 0, "a.id"), col(1, 0, "b.id")),
		cmp(sql.OpEq, col(1, 1, "b.kind"), lit(3)),
	}}
	out := Optimize(j)
	jo := out.(*Join)
	if n := len(jo.R.(*Scan).Preds); n != 1 {
		t.Fatalf("b giữ %d điều kiện, mong 1:\n%s", n, Explain(out))
	}
	if len(jo.On) != 1 {
		t.Fatalf("ON còn %d vế, mong 1:\n%s", len(jo.On), Explain(out))
	}
}

// TestOrIsNotPushedDown là vế NGƯỢC, và nó quan trọng hơn: đẩy một vế của OR
// xuống là trả về hàng KHÔNG thoả điều kiện. Nếu bài test này xanh sai chiều
// thì mọi truy vấn có OR đều cho kết quả sai.
//
// Bất biến khẳng định là "không vế nào của OR xuống tới Scan", KHÔNG phải "nó
// nằm ở Filter phía trên join". Bản đầu của bài test khẳng định điều thứ hai
// và đỏ — vì optimizer để vế ấy lại trong On của Join, mà với INNER join thì
// `ON p` và `WHERE p` tương đương hoàn toàn về tập hàng. Nên chỗ nó đậu là
// chuyện cài đặt; chỗ nó KHÔNG được đậu mới là bất biến.
func TestOrIsNotPushedDown(t *testing.T) {
	j := &Join{L: scan(0, "a"), R: scan(1, "b"),
		On: []Expr{cmp(sql.OpEq, col(0, 0, "a.id"), col(1, 0, "b.id"))}}
	or := &LogicExpr{Op: sql.OpOr,
		L: cmp(sql.OpEq, col(0, 1, "a.x"), lit(1)),
		R: cmp(sql.OpEq, col(1, 1, "b.y"), lit(2))}
	out := Optimize(&Filter{In: j, Preds: []Expr{or}})

	var jo *Join
	switch x := out.(type) {
	case *Filter:
		if len(x.Preds) != 1 {
			t.Fatalf("%d điều kiện ở lại trên join", len(x.Preds))
		}
		jo = x.In.(*Join)
	case *Join:
		if len(x.On) != 2 {
			t.Fatalf("ON có %d vế, mong 2 (điều kiện join + vế OR):\n%s", len(x.On), Explain(out))
		}
		jo = x
	default:
		t.Fatalf("cây thành %T:\n%s", out, Explain(out))
	}
	if n := len(jo.L.(*Scan).Preds); n != 0 {
		t.Fatalf("%d vế bị đẩy xuống bảng a — kết quả sẽ SAI:\n%s", n, Explain(out))
	}
	if n := len(jo.R.(*Scan).Preds); n != 0 {
		t.Fatalf("%d vế bị đẩy xuống bảng b — kết quả sẽ SAI:\n%s", n, Explain(out))
	}
}

// TestOrOnOneRelationIsPushed là mặt còn lại của luật: một vế OR chỉ dùng cột
// của MỘT bảng thì đẩy được, vì lúc ấy nó là một vị từ của bảng ấy.
func TestOrOnOneRelationIsPushed(t *testing.T) {
	j := &Join{L: scan(0, "a"), R: scan(1, "b"),
		On: []Expr{cmp(sql.OpEq, col(0, 0, "a.id"), col(1, 0, "b.id"))}}
	or := &LogicExpr{Op: sql.OpOr,
		L: cmp(sql.OpEq, col(0, 1, "a.x"), lit(1)),
		R: cmp(sql.OpEq, col(0, 1, "a.x"), lit(2))}
	out := Optimize(&Filter{In: j, Preds: []Expr{or}})
	jo, ok := out.(*Join)
	if !ok {
		t.Fatalf("còn lại %T:\n%s", out, Explain(out))
	}
	if n := len(jo.L.(*Scan).Preds); n != 1 {
		t.Fatalf("bảng a giữ %d vế, mong 1 — một OR trong PHẠM VI một bảng vẫn"+
			" phải đẩy được:\n%s", n, Explain(out))
	}
}

// TestPushdownKeepsOriginalTree: Optimize không được sửa cây gốc tại chỗ, vì
// EXPLAIN in cả cây trước và cây sau.
func TestPushdownKeepsOriginalTree(t *testing.T) {
	in := &Filter{In: scan(0, "t"), Preds: []Expr{cmp(sql.OpLt, col(0, 1, "t.b"), lit(10))}}
	before := Explain(in)
	Optimize(in)
	if after := Explain(in); after != before {
		t.Fatalf("cây gốc bị sửa:\ntrước:\n%s\nsau:\n%s", before, after)
	}
	if !strings.Contains(before, "Filter") {
		t.Fatalf("cây gốc lẽ ra có Filter:\n%s", before)
	}
}

// ---------- khoảng ----------

func TestSpanEmpty(t *testing.T) {
	cases := []struct {
		lo, hi keys.Value
		incl   bool
		want   bool
	}{
		{keys.Int(5), keys.Int(3), false, true},
		{keys.Int(5), keys.Int(5), false, true}, // [5,5) rỗng
		{keys.Int(5), keys.Int(5), true, false}, // [5,5] có một giá trị
		{keys.Int(3), keys.Int(5), false, false},
		{keys.Null(), keys.Int(5), false, false}, // không chặn dưới
		{keys.Int(5), keys.Null(), false, false}, // không chặn trên
	}
	for _, c := range cases {
		s := Span{Col: 0, Lo: c.lo, Hi: c.hi, HiIncl: c.incl}
		if got := s.Empty(); got != c.want {
			t.Errorf("[%v,%v) incl=%v: Empty()=%v, mong %v", c.lo, c.hi, c.incl, got, c.want)
		}
	}
}

// TestExtractSpansAllColumns: hàm này phải trả về MỌI cột ứng viên, không tự
// chọn. Bản đầu chọn cột có nhiều điều kiện nhất và vì thế chọn sai — việc
// chọn phải để cho chỗ có thống kê làm.
func TestExtractSpansAllColumns(t *testing.T) {
	s := scan(0, "t")
	s.Preds = []Expr{
		cmp(sql.OpEq, col(0, 1, "t.b"), lit(3)),
		cmp(sql.OpLt, col(0, 0, "t.a"), lit(1000)),
	}
	cands := extractSpans(s)
	if len(cands) != 2 {
		t.Fatalf("%d ứng viên, mong 2 (một cho mỗi cột)", len(cands))
	}
	if cands[0].span.Col != 0 || cands[1].span.Col != 1 {
		t.Fatalf("thứ tự không tiền định: %d rồi %d", cands[0].span.Col, cands[1].span.Col)
	}
	if !cands[1].span.Eq {
		t.Fatal("phép bằng không được đánh dấu Eq — chặn trên sẽ thành NGẶT và" +
			" khoảng [v,v) rỗng, tức truy vấn trả về 0 hàng")
	}
	// Điều kiện đã thành khoảng thì không còn trong phần dư của ứng viên ấy.
	if len(cands[1].rest) != 1 {
		t.Fatalf("ứng viên cột b còn %d điều kiện dư, mong 1", len(cands[1].rest))
	}
}

func TestExtractSpansRejectsNotEqual(t *testing.T) {
	s := scan(0, "t")
	s.Preds = []Expr{cmp(sql.OpNe, col(0, 0, "t.a"), lit(3))}
	if cands := extractSpans(s); len(cands) != 0 {
		t.Fatalf("`<>` cho ra %d khoảng — nó KHÔNG là một khoảng liên tục", len(cands))
	}
}

// ---------- logic ba giá trị ----------

func TestThreeValuedTruthTable(t *testing.T) {
	T, F, N := keys.Bool(true), keys.Bool(false), keys.Null()
	row := []keys.Value{}
	ev := func(op sql.BinOp, a, b keys.Value) keys.Value {
		v, err := (&LogicExpr{Op: op, L: &LitExpr{V: a}, R: &LitExpr{V: b}}).Eval(row)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	name := func(v keys.Value) string {
		switch v.T {
		case keys.TypeTrue:
			return "T"
		case keys.TypeFalse:
			return "F"
		}
		return "N"
	}
	// Bảng chân lý SQL. Hai ô đáng nhớ: N AND F = F (không đoản mạch ở NULL),
	// và N OR T = T.
	cases := []struct {
		op   sql.BinOp
		a, b keys.Value
		want keys.Value
	}{
		{sql.OpAnd, T, T, T}, {sql.OpAnd, T, F, F}, {sql.OpAnd, F, N, F},
		{sql.OpAnd, N, F, F}, {sql.OpAnd, N, T, N}, {sql.OpAnd, N, N, N},
		{sql.OpOr, F, F, F}, {sql.OpOr, T, N, T}, {sql.OpOr, N, T, T},
		{sql.OpOr, N, F, N}, {sql.OpOr, N, N, N},
	}
	for _, c := range cases {
		if got := ev(c.op, c.a, c.b); got.T != c.want.T {
			t.Errorf("%s %s %s = %s, mong %s",
				name(c.a), c.op, name(c.b), name(got), name(c.want))
		}
	}
}

// TestCompareWithNullIsNull là chỗ ngữ nghĩa SQL rẽ khỏi bộ mã hoá phase 7.
func TestCompareWithNullIsNull(t *testing.T) {
	// keys.Compare coi NULL bằng NULL — đúng, vì nó là phép SẮP THỨ TỰ.
	if keys.Compare(keys.Null(), keys.Null()) != 0 {
		t.Fatal("keys.Compare(NULL,NULL) != 0 — bộ mã hoá đã đổi ngữ nghĩa sắp thứ tự")
	}
	// Còn phép SO SÁNH của SQL thì không.
	v, err := cmp(sql.OpEq, &LitExpr{V: keys.Null()}, &LitExpr{V: keys.Null()}).Eval(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !v.IsNull() {
		t.Fatalf("`NULL = NULL` cho %v, phải cho NULL", v)
	}
	if True(v) {
		t.Fatal("NULL lọt qua phép thử của WHERE")
	}
}

// ---------- ép kiểu ----------

func TestCoerceValue(t *testing.T) {
	if v, err := CoerceValue(keys.Int(5), keys.TypeUint); err != nil || v.T != keys.TypeUint || v.U != 5 {
		t.Fatalf("Int(5) -> UINT: %v %v", v, err)
	}
	if _, err := CoerceValue(keys.Int(-1), keys.TypeUint); err == nil {
		t.Fatal("hằng âm vào cột UINT phải là lỗi tường minh, không phải phép ép im lặng")
	}
	if _, err := CoerceValue(keys.Str("x"), keys.TypeInt); err == nil {
		t.Fatal("chuỗi vào cột INT phải là lỗi")
	}
	if v, err := CoerceValue(keys.Null(), keys.TypeInt); err != nil || !v.IsNull() {
		t.Fatalf("NULL vào cột nào cũng được: %v %v", v, err)
	}
}

// TestTransitivePropagation là luật được thêm ở LƯỢT CHẠY của phase 8, và
// chính bảng số của cmd/sqllab chỉ ra nó.
//
//	ON ev.kind = dim.kind AND dim.kind < 5
//
// Đẩy `dim.kind<5` xuống làm vế build teo từ 200 xuống 5 hàng — mà vế probe
// vẫn 20000 hàng, nên đo được chỉ 1.18x. Điều kiện thật sự đáng đẩy là
// `ev.kind < 5`, và nó KHÔNG có trong câu người gõ.
func TestTransitivePropagation(t *testing.T) {
	j := &Join{L: scan(0, "ev"), R: scan(1, "dim"), On: []Expr{
		cmp(sql.OpEq, col(0, 1, "ev.kind"), col(1, 0, "dim.kind")),
		cmp(sql.OpLt, col(1, 0, "dim.kind"), lit(5)),
	}}
	out := Optimize(j)
	jo, ok := out.(*Join)
	if !ok {
		t.Fatalf("còn lại %T:\n%s", out, Explain(out))
	}
	if n := len(jo.L.(*Scan).Preds); n != 1 {
		t.Fatalf("bảng ev giữ %d điều kiện, mong 1 (`ev.kind < 5` suy ra được"+
			" từ phép bằng của join):\n%s", n, Explain(out))
	}
	if n := len(jo.R.(*Scan).Preds); n != 1 {
		t.Fatalf("bảng dim giữ %d điều kiện, mong 1:\n%s", n, Explain(out))
	}
	if got := jo.L.(*Scan).Preds[0].String(); got != "(ev.kind < 5)" {
		t.Fatalf("điều kiện suy ra là %s, mong (ev.kind < 5)", got)
	}
}

// TestPropagationIsIdempotent: chạy Optimize hai lần phải ra cùng một cây.
//
// Không có phép chống trùng thì mỗi lần suy lại sinh thêm một bản sao, và cùng
// một câu chạy hai lần sẽ có kế hoạch khác nhau — một planner không tiền định
// là một planner không debug được.
func TestPropagationIsIdempotent(t *testing.T) {
	mk := func() Node {
		return &Join{L: scan(0, "ev"), R: scan(1, "dim"), On: []Expr{
			cmp(sql.OpEq, col(0, 1, "ev.kind"), col(1, 0, "dim.kind")),
			cmp(sql.OpLt, col(1, 0, "dim.kind"), lit(5)),
		}}
	}
	once := Explain(Optimize(mk()))
	twice := Explain(Optimize(Optimize(mk())))
	if once != twice {
		t.Fatalf("không idempotent:\nmột lần:\n%s\nhai lần:\n%s", once, twice)
	}
}

// TestPropagationOnlyThroughEquality: chỉ phép BẰNG mới cho suy ra.
// `a.x < b.y AND b.y < 5` KHÔNG cho `a.x < 5`... thực ra nó có cho, nhưng bằng
// một lập luận về thứ tự chứ không phải về đồng nhất, và optimizer này không
// làm lập luận ấy. Khẳng định để giới hạn được ghi lại thành code.
func TestPropagationOnlyThroughEquality(t *testing.T) {
	j := &Join{L: scan(0, "a"), R: scan(1, "b"), On: []Expr{
		cmp(sql.OpLt, col(0, 1, "a.x"), col(1, 1, "b.y")),
		cmp(sql.OpLt, col(1, 1, "b.y"), lit(5)),
	}}
	out := Optimize(j)
	jo := out.(*Join)
	if n := len(jo.L.(*Scan).Preds); n != 0 {
		t.Fatalf("suy ra %d điều kiện qua một phép KHÔNG bằng:\n%s", n, Explain(out))
	}
}

// TestContradictoryEqualitiesGiveEmptySpan: `x = 3 AND x = 9` phải cho một
// khoảng RỖNG, không phải khoảng [9,9].
//
// Bản đầu của extractSpans viết nhánh OpEq thành một phép GÁN
// (`a.lo, a.hi = v, v`) trong khi hai nhánh `>` và `<` bên cạnh là phép GIAO.
// Hệ quả: điều kiện thứ hai xoá điều kiện thứ nhất, khoảng ra [9,9] không rỗng,
// `x = 3` tụt xuống làm residual — kết quả vẫn ĐÚNG (0 hàng) nhưng engine quét
// cả bảng để ra 0 hàng ấy.
//
// Vì sao không bài test nào bắt được: cả bộ test phase 8 khẳng định về KẾT QUẢ,
// và kết quả đúng. Chỉ một lời khẳng định về SỐ HÀNG ĐỌC mới bắt được — cùng
// một bài học với `TestPushdownKeepsResults` và với `TestSelectivityNotPredicateCount`.
// Tìm ra bằng cách đọc EXPLAIN của một câu vô nghiệm khi dựng ví dụ cho nhật ký.
func TestContradictoryEqualitiesGiveEmptySpan(t *testing.T) {
	cases := []struct {
		name  string
		preds []Expr
	}{
		{"= và =", []Expr{
			cmp(sql.OpEq, col(0, 0, "t.a"), lit(3)),
			cmp(sql.OpEq, col(0, 0, "t.a"), lit(9)),
		}},
		{"< rồi = ở đúng chặn trên", []Expr{
			cmp(sql.OpLt, col(0, 0, "t.a"), lit(9)),
			cmp(sql.OpEq, col(0, 0, "t.a"), lit(9)),
		}},
		{"= rồi > cao hơn", []Expr{
			cmp(sql.OpEq, col(0, 0, "t.a"), lit(3)),
			cmp(sql.OpGt, col(0, 0, "t.a"), lit(100)),
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scan(0, "t")
			s.Preds = c.preds
			cands := extractSpans(s)
			if len(cands) != 1 {
				t.Fatalf("%d ứng viên, mong 1 (cùng một cột)", len(cands))
			}
			if !cands[0].span.Empty() {
				t.Fatalf("khoảng [%v,%v] incl=%v KHÔNG rỗng — planner sẽ quét cả"+
					" bảng cho một câu vô nghiệm",
					cands[0].span.Lo, cands[0].span.Hi, cands[0].span.HiIncl)
			}
		})
	}
}

// TestSingleEqualityStillGivesPointSpan: phép giao ở trên không được làm hỏng
// trường hợp thường — một phép bằng vẫn phải ra khoảng ĐIỂM [v,v].
func TestSingleEqualityStillGivesPointSpan(t *testing.T) {
	s := scan(0, "t")
	s.Preds = []Expr{
		cmp(sql.OpEq, col(0, 0, "t.a"), lit(7)),
		cmp(sql.OpLt, col(0, 0, "t.a"), lit(1000)),
	}
	cands := extractSpans(s)
	if len(cands) != 1 {
		t.Fatalf("%d ứng viên, mong 1", len(cands))
	}
	sp := cands[0].span
	if sp.Empty() || !sp.Eq || keys.Compare(sp.Lo, keys.Int(7)) != 0 ||
		keys.Compare(sp.Hi, keys.Int(7)) != 0 || !sp.HiIncl {
		t.Fatalf("mong khoảng điểm [7,7], được [%v,%v] incl=%v eq=%v rỗng=%v",
			sp.Lo, sp.Hi, sp.HiIncl, sp.Eq, sp.Empty())
	}
}
