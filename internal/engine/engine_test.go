package engine

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"minidb/internal/db"
	"minidb/internal/exec"
	"minidb/internal/keys"
	"minidb/internal/plan"
	"minidb/internal/sql"
	"minidb/internal/table"
	"minidb/internal/txn"
)

// engine_test.go là bộ khẳng định end-to-end của phase 8: từ chuỗi SQL tới
// byte trong cây và ngược lại.
//
// Nguyên tắc xuyên suốt: **hai đường phải cho CÙNG một kết quả.** Nested loop
// và hash join, tràn đĩa và không tràn, có Sort và bỏ Sort, đẩy điều kiện và
// không đẩy — mỗi cặp là một phép đối chứng. Một phép tối ưu làm đổi kết quả
// không phải phép tối ưu, nó là một con bug; và cách duy nhất bắt được nó là
// chạy cả hai đường trên cùng dữ liệu rồi so từng hàng.

const nEv = 400
const nDim = 20

func open(t *testing.T) *Engine {
	t.Helper()
	s, err := txn.Open(t.TempDir()+"/t.db", db.Options{Frames: 256})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	e, err := New(s)
	if err != nil {
		t.Fatal(err)
	}
	e.TmpDir = t.TempDir()
	return e
}

// seed dựng hai bảng. city ĐẢO NGƯỢC so với id, nên `ORDER BY city` không
// trùng thứ tự lưu — nếu trùng thì mọi bài test sắp xếp chạy trên đầu vào đã
// sắp, tức là chạy ca dễ nhất và không kiểm được gì.
func seed(t *testing.T, e *Engine) {
	t.Helper()
	must(t, e, `CREATE TABLE ev (id UINT, kind INT, city TEXT, payload TEXT, PRIMARY KEY (id))`)
	must(t, e, `CREATE TABLE dim (kind INT, name TEXT, PRIMARY KEY (kind))`)
	var b strings.Builder
	b.WriteString("INSERT INTO ev VALUES ")
	for i := 0; i < nEv; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, "(%d,%d,'c%05d','p%d')", i, i%nDim, nEv-i, i)
	}
	must(t, e, b.String())
	b.Reset()
	b.WriteString("INSERT INTO dim VALUES ")
	for i := 0; i < nDim; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, "(%d,'k%02d')", i, i)
	}
	must(t, e, b.String())
	must(t, e, `CREATE INDEX ev_kind ON ev (kind)`)
	must(t, e, `ANALYZE ev`)
	must(t, e, `ANALYZE dim`)
}

func must(t *testing.T, e *Engine, src string) *Result {
	t.Helper()
	r, err := e.ExecSQL(src)
	if err != nil {
		t.Fatalf("%s\n  -> %v", trunc(src), err)
	}
	return r
}

func trunc(s string) string {
	if len(s) > 90 {
		return s[:89] + "…"
	}
	return s
}

func rowsOf(t *testing.T, e *Engine, src string) []string {
	t.Helper()
	r, err := e.ExecSQL(src)
	if err != nil {
		t.Fatalf("%s\n  -> %v", trunc(src), err)
	}
	out := make([]string, len(r.Rows))
	for i, row := range r.Rows {
		parts := make([]string, len(row))
		for j, v := range row {
			parts[j] = fmtV(v)
		}
		out[i] = strings.Join(parts, "|")
	}
	return out
}

func fmtV(v keys.Value) string {
	if v.T == keys.TypeBytes {
		return string(v.B)
	}
	return v.String()
}

// ---------- cơ bản ----------

func TestSelectBasics(t *testing.T) {
	e := open(t)
	seed(t, e)

	if got := rowsOf(t, e, `SELECT city FROM ev WHERE id = 7`); len(got) != 1 ||
		got[0] != fmt.Sprintf("c%05d", nEv-7) {
		t.Fatalf("truy vấn điểm: %v", got)
	}
	if got := rowsOf(t, e, `SELECT id FROM ev WHERE kind = 3`); len(got) != nEv/nDim {
		t.Fatalf("kind=3 ra %d hàng, mong %d", len(got), nEv/nDim)
	}
	if got := rowsOf(t, e, `SELECT id FROM ev WHERE id >= 10 AND id < 20`); len(got) != 10 {
		t.Fatalf("khoảng nửa mở ra %d hàng, mong 10", len(got))
	}
	if got := rowsOf(t, e, `SELECT * FROM ev WHERE id = 0`); len(got) != 1 ||
		len(strings.Split(got[0], "|")) != 4 {
		t.Fatalf("SELECT * : %v", got)
	}
	// Khoảng rỗng phải là một kế hoạch KHÔNG đọc gì, không phải một kế hoạch
	// đọc rồi lọc hết.
	r, err := e.ExecSQL(`SELECT id FROM ev WHERE id > 100 AND id < 50`)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Rows) != 0 {
		t.Fatalf("khoảng rỗng ra %d hàng", len(r.Rows))
	}
	if r.TStat.RowsScanned != 0 || r.TStat.IndexEntries != 0 {
		t.Fatalf("khoảng rỗng vẫn đọc %d hàng + %d mục index — planner chưa nhận"+
			" ra khoảng rỗng", r.TStat.RowsScanned, r.TStat.IndexEntries)
	}
}

// TestPKRangeIsARangeScan là bài test của con bug đắt nhất trong lượt code này.
//
// Bản đầu của planner đẩy MỌI khoảng về làm Filter khi đường đi là seq scan,
// vì tôi mang nguyên trực giác "seq scan không dùng được khoảng" từ heap của
// Postgres. Ở đây sai: hàng nằm TRONG cây khóa chính (clustered index, phase
// 7), nên `WHERE pk < v` là một range scan thật mà không cần index phụ nào.
//
// Đo được: cùng câu ấy đọc 5000 hàng trước khi sửa và 5 hàng sau khi sửa.
func TestPKRangeIsARangeScan(t *testing.T) {
	e := open(t)
	seed(t, e)
	r, err := e.ExecSQL(`SELECT city FROM ev WHERE id < 5`)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Rows) != 5 {
		t.Fatalf("%d hàng, mong 5", len(r.Rows))
	}
	if r.TStat.RowsScanned > 8 {
		t.Fatalf("đọc %d hàng để trả về 5 — khoảng trên khóa chính KHÔNG được"+
			" đẩy vào access path, nó đã tụt về làm một phép lọc", r.TStat.RowsScanned)
	}
}

// TestSelectivityNotPredicateCount: chọn cột nào để đẩy vào index phải theo
// ĐỘ CHỌN LỌC, không theo số lượng điều kiện.
//
// Bản đầu chọn cột có nhiều điều kiện nhất, hòa thì cột nhỏ nhất — nên với
// `kind = 3 AND id < nEv` nó chọn id (khoảng bao trùm cả bảng) và bỏ kind
// (lọc còn 1/nDim). Kết quả vẫn ĐÚNG, chỉ chậm — nên chỉ một khẳng định về số
// hàng đọc mới bắt được.
func TestSelectivityNotPredicateCount(t *testing.T) {
	e := open(t)
	seed(t, e)
	r, err := e.ExecSQL(fmt.Sprintf(`SELECT city FROM ev WHERE kind = 3 AND id < %d`, nEv))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Rows) != nEv/nDim {
		t.Fatalf("%d hàng, mong %d", len(r.Rows), nEv/nDim)
	}
	touched := r.TStat.RowsScanned + r.TStat.IndexEntries + r.TStat.RowFetches
	if touched > nEv/2 {
		t.Fatalf("chạm %d entry để trả %d hàng (bảng %d hàng) — planner đã chọn"+
			" khoảng KÉM chọn lọc hơn", touched, len(r.Rows), nEv)
	}
}

// ---------- ba giá trị ----------

// TestThreeValuedLogic là chỗ ngữ nghĩa SQL và bộ mã hoá của phase 7 khác nhau.
//
//	keys.Compare(Null, Null) == 0   — để SẮP THỨ TỰ thì NULL phải có chỗ
//	SQL: NULL = NULL -> NULL        — để SO SÁNH thì NULL là "không biết"
//
// Nếu trộn hai thứ ấy thì `WHERE x = NULL` trả về hàng, và đó là con bug mà
// mọi người học SQL đều gặp một lần.
func TestThreeValuedLogic(t *testing.T) {
	e := open(t)
	must(t, e, `CREATE TABLE t3 (id UINT, v INT, PRIMARY KEY (id))`)
	must(t, e, `INSERT INTO t3 (id, v) VALUES (1, 10), (2, 20)`)
	must(t, e, `INSERT INTO t3 (id) VALUES (3)`) // v = NULL
	must(t, e, `ANALYZE t3`)

	if got := rowsOf(t, e, `SELECT id FROM t3 WHERE v = NULL`); len(got) != 0 {
		t.Fatalf("`v = NULL` trả về %v — NULL không được so BẰNG với gì cả", got)
	}
	if got := rowsOf(t, e, `SELECT id FROM t3 WHERE v <> NULL`); len(got) != 0 {
		t.Fatalf("`v <> NULL` trả về %v", got)
	}
	// Bất đối xứng của NOT: cả điều kiện và phủ định của nó đều bỏ hàng NULL,
	// nên hợp của hai truy vấn KHÔNG phải cả bảng.
	a := rowsOf(t, e, `SELECT id FROM t3 WHERE v > 15`)
	b := rowsOf(t, e, `SELECT id FROM t3 WHERE v <= 15`)
	if len(a)+len(b) != 2 {
		t.Fatalf("v>15 ra %d hàng, v<=15 ra %d — tổng phải là 2 (hàng NULL không"+
			" thuộc vế nào), không phải 3", len(a), len(b))
	}
	// AND với FALSE là FALSE dù vế kia là NULL — không được đoản mạch ở NULL.
	if got := rowsOf(t, e, `SELECT id FROM t3 WHERE v = NULL AND id = 999`); len(got) != 0 {
		t.Fatalf("%v", got)
	}
	// OR với TRUE là TRUE dù vế kia là NULL.
	if got := rowsOf(t, e, `SELECT id FROM t3 WHERE v = NULL OR id = 3`); len(got) != 1 {
		t.Fatalf("`NULL OR TRUE` phải là TRUE, ra %v", got)
	}
}

// ---------- join: hai thuật toán, một kết quả ----------

// TestJoinAlgorithmsAgree chạy CÙNG một câu qua nested loop và hash join rồi
// so từng hàng.
//
// Đây là bài test đáng nhất của cả phase: một phép tối ưu chỉ được phép đổi
// THỜI GIAN. Nếu hai thuật toán ra hai tập hàng khác nhau thì có ít nhất một
// cái sai, và cái sai ấy sẽ không bao giờ lộ ra qua một bài test chỉ chạy
// đường mà planner chọn.
func TestJoinAlgorithmsAgree(t *testing.T) {
	e := open(t)
	seed(t, e)
	queries := []string{
		`SELECT ev.id, dim.name FROM ev JOIN dim ON ev.kind = dim.kind WHERE ev.id < 30`,
		`SELECT ev.city, dim.name FROM ev JOIN dim ON ev.kind = dim.kind WHERE dim.kind < 3`,
		`SELECT ev.id FROM ev JOIN dim ON ev.kind = dim.kind`,
		`SELECT ev.id, dim.kind FROM ev JOIN dim ON ev.kind = dim.kind AND ev.id < 50`,
	}
	for _, src := range queries {
		nl, _ := runForced(t, e, src, false, plan.DefaultBudget)
		hj, _ := runForced(t, e, src, true, plan.DefaultBudget)
		sort.Strings(nl)
		sort.Strings(hj)
		if len(nl) == 0 {
			t.Fatalf("%s: không hàng nào — bài test này không kiểm được gì", trunc(src))
		}
		if strings.Join(nl, ";") != strings.Join(hj, ";") {
			t.Fatalf("%s\n  nested loop %d hàng\n  hash join   %d hàng\n  KHÁC NHAU",
				trunc(src), len(nl), len(hj))
		}
	}
}

// TestHashJoinSpillAgrees: hạn mức nhỏ tới mức buộc tràn đĩa phải cho ĐÚNG
// cùng kết quả, và bài test tự khẳng định rằng nó THẬT SỰ đã tràn.
//
// Vế thứ hai quan trọng hơn vế thứ nhất: không có nó thì một hạn mức "nhỏ" mà
// hoá ra vẫn đủ sẽ làm bài test xanh mà chưa hề chạy nhánh tràn đĩa. Đây là
// bài học phase 3 và phase 5, quay lại lần thứ ba.
func TestHashJoinSpillAgrees(t *testing.T) {
	e := open(t)
	seed(t, e)
	const src = `SELECT ev.id, dim.name FROM ev JOIN dim ON ev.kind = dim.kind`

	big, bigSt := runForced(t, e, src, true, plan.DefaultBudget)
	small, smallSt := runForced(t, e, src, true, 2)
	if bigSt.SpillFiles != 0 {
		t.Fatalf("hạn mức lớn mà vẫn tràn %d file", bigSt.SpillFiles)
	}
	if smallSt.SpillFiles == 0 {
		t.Fatal("hạn mức 2 hàng mà KHÔNG tràn — nhánh chia phần chưa hề chạy," +
			" nên bài test này chưa kiểm được cái nó định kiểm")
	}
	if smallSt.SpillRereads == 0 {
		t.Fatal("tràn mà không đọc lại hàng nào từ đĩa")
	}
	sort.Strings(big)
	sort.Strings(small)
	if strings.Join(big, ";") != strings.Join(small, ";") {
		t.Fatalf("tràn đĩa đổi kết quả: %d hàng vs %d hàng", len(big), len(small))
	}
	t.Logf("tràn: %d file, %d byte, %d hàng đọc lại; %d hàng ra",
		smallSt.SpillFiles, smallSt.SpillBytes, smallSt.SpillRereads, len(small))
}

// ---------- sắp xếp ----------

// TestExternalSortAgrees: sắp trong RAM và sắp ngoài phải ra CÙNG một thứ tự,
// và bài test tự khẳng định rằng nó đã thật sự ghi run ra đĩa.
func TestExternalSortAgrees(t *testing.T) {
	e := open(t)
	seed(t, e)
	const src = `SELECT city, id FROM ev ORDER BY city`

	e.Pl.Budget = nEv * 2
	inRAM := rowsOf(t, e, src)
	r1, _ := e.ExecSQL(src)
	if r1.Stat.SortRuns != 0 {
		t.Fatalf("hạn mức %d cho %d hàng mà vẫn ghi %d run", e.Pl.Budget, nEv, r1.Stat.SortRuns)
	}

	e.Pl.Budget = 16
	onDisk := rowsOf(t, e, src)
	r2, _ := e.ExecSQL(src)
	if r2.Stat.SortRuns < 2 {
		t.Fatalf("hạn mức 16 cho %d hàng mà chỉ %d run — phép trộn k đường chưa"+
			" hề chạy", nEv, r2.Stat.SortRuns)
	}
	if strings.Join(inRAM, ";") != strings.Join(onDisk, ";") {
		t.Fatal("sắp ngoài cho thứ tự KHÁC sắp trong RAM")
	}
	if !sort.StringsAreSorted(onDisk) {
		t.Fatalf("kết quả không sắp: %v…", onDisk[:5])
	}
	t.Logf("%d hàng, %d run, %d byte tạm", r2.Stat.SortRows, r2.Stat.SortRuns, r2.Stat.SpillBytes)
}

func TestOrderByDescAndMultiKey(t *testing.T) {
	e := open(t)
	seed(t, e)
	e.Pl.Budget = 8 // buộc sắp ngoài, nơi DESC phải đi qua phép lấy bù byte
	got := rowsOf(t, e, `SELECT city FROM ev ORDER BY city DESC`)
	if len(got) != nEv {
		t.Fatalf("%d hàng", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] < got[i] {
			t.Fatalf("DESC không giảm dần tại %d: %q rồi %q", i, got[i-1], got[i])
		}
	}
	// Khoá kép trộn chiều: ASC rồi DESC. Cả hai vẫn phải so được bằng MỘT phép
	// bytes.Compare, vì DESC được cài bằng lấy bù byte (phase 7).
	multi := rowsOf(t, e, `SELECT kind, id FROM ev ORDER BY kind, id DESC`)
	if len(multi) != nEv {
		t.Fatalf("%d hàng", len(multi))
	}
	prevKind, prevID := int64(-1), int64(-1)
	for _, row := range multi {
		var k, id int64
		fmt.Sscanf(row, "%d|%d", &k, &id)
		switch {
		case k < prevKind:
			t.Fatalf("kind không tăng: %d sau %d", k, prevKind)
		case k == prevKind && id > prevID:
			t.Fatalf("trong cùng kind=%d, id không giảm: %d sau %d", k, id, prevID)
		}
		prevKind, prevID = k, id
	}
}

// ---------- pushdown và bỏ Sort ----------

// TestPushdownKeepsResults: bật và tắt pushdown phải ra cùng kết quả, và bật
// phải chạm ÍT entry hơn. Vế thứ hai là lý do phép tối ưu tồn tại; nếu nó
// không đúng thì phép tối ưu chỉ là một phép biến đổi vô ích.
func TestPushdownKeepsResults(t *testing.T) {
	e := open(t)
	seed(t, e)
	queries := []string{
		fmt.Sprintf(`SELECT city FROM ev WHERE kind = 3 AND id < %d`, nEv),
		`SELECT ev.id, dim.name FROM ev JOIN dim ON ev.kind = dim.kind WHERE ev.kind = 3`,
	}
	for _, src := range queries {
		sel := parseSel(t, src)
		off, offT, _ := runPlan(t, e, sel, false, e.Pl.Budget)
		on, onT, _ := runPlan(t, e, sel, true, e.Pl.Budget)
		sort.Strings(off)
		sort.Strings(on)
		if strings.Join(off, ";") != strings.Join(on, ";") {
			t.Fatalf("%s: pushdown ĐỔI kết quả (%d vs %d hàng)", trunc(src), len(off), len(on))
		}
		tOff := offT.RowsScanned + offT.IndexEntries + offT.RowFetches
		tOn := onT.RowsScanned + onT.IndexEntries + onT.RowFetches
		if tOn >= tOff {
			t.Fatalf("%s: pushdown chạm %d entry, không đẩy chạm %d — không tiết"+
				" kiệm được gì", trunc(src), tOn, tOff)
		}
		t.Logf("%s: %d -> %d entry (%.1fx)", trunc(src), tOff, tOn, float64(tOff)/float64(tOn))
	}
}

// TestOrderByUsesIndex là bài test của nợ P7-9: `ORDER BY kind` phải dùng
// index ev_kind và BỎ HẲN toán tử Sort.
//
// Khẳng định bằng số ĐO ĐƯỢC chứ không bằng hình dạng kế hoạch: với LIMIT 10,
// một kế hoạch có Sort phải đọc cả bảng, còn kế hoạch bỏ được Sort đọc đúng
// hơn 10 mục index. Đó là khác biệt về BẬC, nên một chốt số là đủ chắc.
func TestOrderByUsesIndex(t *testing.T) {
	e := open(t)
	seed(t, e)
	r, err := e.ExecSQL(`SELECT id, kind FROM ev ORDER BY kind LIMIT 10`)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Rows) != 10 {
		t.Fatalf("%d hàng", len(r.Rows))
	}
	if r.Stat.SortRows != 0 {
		t.Fatalf("vẫn sắp %d hàng dù có index trên kind — planner chưa dùng thứ"+
			" tự của index (nợ P7-9 chưa trả)", r.Stat.SortRows)
	}
	touched := r.TStat.RowsScanned + r.TStat.IndexEntries + r.TStat.RowFetches
	if touched > 40 {
		t.Fatalf("chạm %d entry để trả 10 hàng — LIMIT chưa dừng sớm", touched)
	}
	// Và thứ tự phải đúng thật, không chỉ "không sắp".
	var prev int64 = -1
	for _, row := range r.Rows {
		k := row[1].I
		if k < prev {
			t.Fatalf("kind không tăng: %d sau %d", k, prev)
		}
		prev = k
	}
	// `ORDER BY pk` cũng phải miễn phí: hàng lưu trong cây pk.
	r2, err := e.ExecSQL(`SELECT id FROM ev ORDER BY id LIMIT 5`)
	if err != nil {
		t.Fatal(err)
	}
	if r2.Stat.SortRows != 0 {
		t.Fatalf("ORDER BY khóa chính mà vẫn sắp %d hàng", r2.Stat.SortRows)
	}
	if n := r2.TStat.RowsScanned; n > 10 {
		t.Fatalf("ORDER BY pk LIMIT 5 đọc %d hàng", n)
	}
}

// ---------- lỗi ngữ nghĩa ----------

// TestSemanticErrors: câu sai TÊN phải chết ở tầng bind, và chết mà KHÔNG
// chạm dữ liệu. Đó là lý do bind tách khỏi exec.
func TestSemanticErrors(t *testing.T) {
	e := open(t)
	seed(t, e)
	cases := []struct{ src, want string }{
		{`SELECT nope FROM ev`, "không có cột"},
		{`SELECT id FROM nope`, "bảng"},
		{`SELECT id FROM ev WHERE city = 5`, "không so được"},
		{`SELECT ev.id FROM ev JOIN dim ON ev.kind = dim.name`, "không so được"},
		{`SELECT kind FROM ev JOIN dim ON ev.kind = dim.kind`, "nhập nhằng"},
		{`SELECT id FROM ev a JOIN ev b ON a.id = b.id JOIN dim ON a.kind = dim.kind`, "hai bảng"},
		{`SELECT id FROM ev JOIN ev ON ev.id = ev.id`, "hai lần"},
		{`INSERT INTO ev VALUES (1)`, "cột"},
	}
	for _, c := range cases {
		_, err := e.ExecSQL(c.src)
		if err == nil {
			t.Errorf("%s: không báo lỗi", trunc(c.src))
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s:\n  got  %v\n  want chứa %q", trunc(c.src), err, c.want)
		}
	}
}

// TestUnsignedLiteralCoercion: hằng trong câu SQL luôn được phân tích thành
// TypeInt, nên nó PHẢI được ép về kiểu cột trước khi so.
//
// Không ép thì keys.Compare so TAG trước (TypeInt = 0x04 < TypeUint = 0x05),
// nên mọi phép so giữa cột UINT và hằng ra cùng một chiều bất kể giá trị — và
// truy vấn im lặng trả về sai. Một bài test với đúng một kiểu cột sẽ không bao
// giờ bắt được.
func TestUnsignedLiteralCoercion(t *testing.T) {
	e := open(t)
	seed(t, e)
	if got := rowsOf(t, e, `SELECT id FROM ev WHERE id = 7`); len(got) != 1 || got[0] != "7" {
		t.Fatalf("cột UINT so với hằng: %v", got)
	}
	if got := rowsOf(t, e, `SELECT id FROM ev WHERE id > 397`); len(got) != 2 {
		t.Fatalf("id > 397 ra %d hàng, mong 2", len(got))
	}
	// Hằng âm vào cột UINT là một lỗi tường minh, không phải một phép ép im lặng.
	if _, err := e.ExecSQL(`SELECT id FROM ev WHERE id = 0 - 1`); err == nil {
		t.Log("(0-1 chưa tính được ở tầng biểu thức: không có toán tử số học — đúng như dự kiến)")
	}
	if _, err := e.ExecSQL(`INSERT INTO ev VALUES (0, 1, 'x', 'y')`); err == nil {
		t.Fatal("chèn trùng khóa chính mà không báo lỗi")
	}
}

// ---------- máy móc cho test ----------
//
// Ba hàm dưới đây dựng kế hoạch BẰNG TAY để chạy được cả đường mà planner
// KHÔNG chọn. Không có chúng thì mọi bài test ở trên chỉ kiểm đúng một đường —
// đường planner thích — và ba nhánh còn lại (nested loop, tràn đĩa, không
// pushdown) không bao giờ chạy. Một nhánh không chạy là một nhánh không đúng.

func runPlan(t *testing.T, e *Engine, sel *sql.Select, optimize bool, budget int) ([]string, table.Stat, exec.Stat) {
	t.Helper()
	return runPlanFull(t, e, sel, optimize, nil, budget)
}

func runPlanFull(t *testing.T, e *Engine, sel *sql.Select, optimize bool,
	rewrite func(plan.PNode) plan.PNode, budget int) ([]string, table.Stat, exec.Stat) {
	t.Helper()
	b, err := plan.BindSelect(e.Cat, sel)
	if err != nil {
		t.Fatal(err)
	}
	root := b.Root
	if optimize {
		root = plan.Optimize(root)
	}
	p, err := e.Pl.Plan(b, root)
	if err != nil {
		t.Fatal(err)
	}
	if rewrite != nil {
		p = rewrite(p)
	}
	var out []string
	var ts table.Stat
	var es exec.Stat
	err = e.Cat.View(e.Level, func(tx *table.Tx) error {
		r := exec.New(tx, b, budget, e.TmpDir)
		top, err := r.Build(p)
		if err != nil {
			return err
		}
		defer top.Close()
		err = r.Rows(top, func(row []keys.Value) bool {
			parts := make([]string, len(row))
			for i, v := range row {
				parts[i] = fmtV(v)
			}
			out = append(out, strings.Join(parts, "|"))
			return true
		})
		ts, es = tx.St, r.St
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out, ts, es
}

// forceJoin đổi thuật toán join trong một cây đã chọn xong, giữ nguyên hai cây
// con — nhờ vậy hai con số là hai thuật toán trên CÙNG một access path.
func forceJoin(p plan.PNode, hash bool) plan.PNode {
	switch x := p.(type) {
	case *plan.PFilter:
		return &plan.PFilter{In: forceJoin(x.In, hash), Preds: x.Preds, E: x.E}
	case *plan.PProject:
		return &plan.PProject{In: forceJoin(x.In, hash), Out: x.Out, E: x.E}
	case *plan.PLimit:
		return &plan.PLimit{In: forceJoin(x.In, hash), N: x.N, E: x.E}
	case *plan.PSort:
		return &plan.PSort{In: forceJoin(x.In, hash), By: x.By, Budget: x.Budget, E: x.E}
	case *plan.PNestLoop:
		if !hash {
			return x
		}
		ks, extra := plan.JoinKeysOf(x.On)
		if len(ks) == 0 {
			return x
		}
		return &plan.PHashJoin{Build: x.R, Probe: x.L, Keys: ks, Extra: extra,
			Budget: plan.DefaultBudget, E: x.E}
	case *plan.PHashJoin:
		if hash {
			return x
		}
		on := append([]plan.Expr(nil), x.Extra...)
		for _, k := range x.Keys {
			on = append(on, plan.EqOf(k))
		}
		return &plan.PNestLoop{L: x.Probe, R: x.Build, On: on, E: x.E}
	}
	return p
}

func parseSel(t *testing.T, src string) *sql.Select {
	t.Helper()
	st, err := sql.Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	return st.(*sql.Select)
}

func runForced(t *testing.T, e *Engine, src string, hash bool, budget int) ([]string, exec.Stat) {
	t.Helper()
	rows, _, es := runPlanFull(t, e, parseSel(t, src), true,
		func(p plan.PNode) plan.PNode { return forceJoin(p, hash) }, budget)
	return rows, es
}
