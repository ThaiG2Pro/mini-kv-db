// sqllab là bài lab của phase 8. Sáu bảng, sáu câu hỏi — và mỗi bảng đọc bằng
// TỈ SỐ, không bằng nano giây (bài học phase 0).
//
//	1  nested loop vs hash join, quét theo kích thước vế ngoài -> điểm đổi vai
//	2  hạn mức bộ nhớ của hash join -> chỗ nó buộc phải tràn ra đĩa, và cái giá
//	3  ORDER BY: sắp trong RAM vs sắp ngoài, quét theo hạn mức -> số run
//	4  predicate pushdown bật/tắt -> nó ăn tiền ở đâu trong CHÍNH engine này
//	5  bỏ bước ORDER BY nhờ index -> tỉ số, và chỗ LIMIT đổi bậc
//	6  chi phí front-end: lex/parse/bind/optimize/plan so với thi hành
//
// Cả sáu chạy trên cùng một bộ dữ liệu, dựng lại từ đầu mỗi lần.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"minidb/internal/db"
	"minidb/internal/exec"
	"minidb/internal/keys"
	"minidb/internal/plan"
	"minidb/internal/query"
	"minidb/internal/sql"
	"minidb/internal/table"
	"minidb/internal/txn"
)

var (
	dir      = flag.String("dir", "data/sql", "thư mục làm việc")
	rowsFlag = flag.Int("rows", 20000, "số hàng bảng ev")
	dimFlag  = flag.Int("dim", 200, "số hàng bảng dim (cũng là số giá trị của ev.kind)")
	frames   = flag.Int("frames", 1024, "số frame của buffer pool")
	repeat   = flag.Int("repeat", 3, "số lần chạy mỗi phép đo, lấy nhanh nhất")
	work     = flag.String("work", "all", "join | budget | sort | pushdown | order | pipeline | all")
	nlCap    = flag.Int("nl-cap", 1000000, "trần số hàng vòng trong cho nested loop (bỏ đo nếu vượt)")
)

// lab giữ mọi thứ một phép đo cần.
type lab struct {
	s   *txn.Store
	cat *table.Catalog
	ev  *table.Schema
	dim *table.Schema
	pl  *plan.Planner
}

func main() {
	flag.Parse()
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(*dir, "sqllab.db")
	// Dựng lại từ đầu: một bảng số đo trên dữ liệu còn sót của lần trước là
	// một bảng số không tái lập được.
	for _, suf := range []string{"", "-wal"} {
		os.Remove(path + suf)
	}
	os.Remove(path + ".wal")

	s, err := txn.Open(path, db.Options{Frames: *frames})
	if err != nil {
		return err
	}
	defer s.Close()

	l := &lab{s: s}
	if err := l.build(); err != nil {
		return err
	}

	sec := func(name string, fn func() error) error {
		if *work != "all" && *work != name {
			return nil
		}
		return fn()
	}
	if err := sec("join", l.secJoin); err != nil {
		return err
	}
	if err := sec("budget", l.secBudget); err != nil {
		return err
	}
	if err := sec("sort", l.secSort); err != nil {
		return err
	}
	if err := sec("pushdown", l.secPushdown); err != nil {
		return err
	}
	if err := sec("order", l.secOrder); err != nil {
		return err
	}
	return sec("pipeline", l.secPipeline)
}

// ---------- dữ liệu ----------

func (l *lab) build() error {
	c, err := table.Load(l.s)
	if err != nil {
		return err
	}
	l.cat = c
	l.ev, err = c.CreateTable("ev", []table.Column{
		{Name: "id", T: keys.TypeUint},
		{Name: "kind", T: keys.TypeInt},
		{Name: "city", T: keys.TypeBytes},
		{Name: "payload", T: keys.TypeBytes},
	}, []string{"id"})
	if err != nil {
		return err
	}
	l.dim, err = c.CreateTable("dim", []table.Column{
		{Name: "kind", T: keys.TypeInt},
		{Name: "name", T: keys.TypeBytes},
	}, []string{"kind"})
	if err != nil {
		return err
	}

	const batch = 2000
	for start := 0; start < *rowsFlag; start += batch {
		end := start + batch
		if end > *rowsFlag {
			end = *rowsFlag
		}
		err := c.Update(txn.RepeatableRead, func(tx *table.Tx) error {
			for i := start; i < end; i++ {
				if err := tx.Insert(l.ev, []keys.Value{
					keys.Uint(uint64(i)),
					keys.Int(int64(i % *dimFlag)),
					// city đảo ngược so với id, nên ORDER BY city KHÔNG trùng
					// thứ tự lưu — nếu trùng thì phép sắp xếp gặp đầu vào đã
					// sắp và mọi con số của mục 3 là con số của ca tốt nhất.
					keys.Str(fmt.Sprintf("c%08d", *rowsFlag-i)),
					keys.Str(fmt.Sprintf("payload-%d-xxxxxxxxxxxxxxxx", i)),
				}); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	err = c.Update(txn.RepeatableRead, func(tx *table.Tx) error {
		for i := 0; i < *dimFlag; i++ {
			if err := tx.Insert(l.dim, []keys.Value{
				keys.Int(int64(i)), keys.Str(fmt.Sprintf("kind-%03d", i)),
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if _, err := c.CreateIndex("ev", "ev_kind", []string{"kind"}, false); err != nil {
		return err
	}

	l.pl = plan.NewPlanner(c)

	for _, sc := range []*table.Schema{l.ev, l.dim} {
		cols := make([]int, len(sc.Cols))
		for i := range cols {
			cols[i] = i
		}
		var st query.Stats
		if err := c.View(txn.RepeatableRead, func(tx *table.Tx) error {
			st, err = query.Analyze(tx, sc, cols)
			return err
		}); err != nil {
			return err
		}
		l.pl.Stats[sc.OID] = st
	}
	fmt.Printf("dữ liệu: ev %d hàng, dim %d hàng, index ev_kind(kind), buffer pool %d frame\n\n",
		*rowsFlag, *dimFlag, *frames)
	return nil
}

// ---------- máy móc đo ----------

// prep phân tích + bind + (tuỳ chọn) tối ưu + chọn kế hoạch.
func (l *lab) prep(src string, optimize bool) (*plan.Bound, plan.PNode, error) {
	st, err := sql.Parse(src)
	if err != nil {
		return nil, nil, err
	}
	sel, ok := st.(*sql.Select)
	if !ok {
		return nil, nil, fmt.Errorf("cần SELECT: %s", src)
	}
	b, err := plan.BindSelect(l.cat, sel)
	if err != nil {
		return nil, nil, err
	}
	root := b.Root
	if optimize {
		root = plan.Optimize(root)
	}
	p, err := l.pl.Plan(b, root)
	if err != nil {
		return nil, nil, err
	}
	return b, p, nil
}

// measure chạy một kế hoạch repeat lần và lấy lần NHANH NHẤT.
//
// Nhanh nhất, không phải trung bình: nhiễu ở đây luôn là nhiễu CỘNG THÊM (một
// lần lập lịch, một lần GC), nên min gần với chi phí thật hơn mean. Đây là
// cách của Go benchmark và của mọi bộ đo micro nghiêm túc.
func (l *lab) measure(b *plan.Bound, p plan.PNode, budget int, limit int) (time.Duration, exec.Stat, table.Stat, int, error) {
	best := time.Duration(1 << 62)
	var st exec.Stat
	var ts table.Stat
	rows := 0
	for i := 0; i < *repeat; i++ {
		var d time.Duration
		err := l.cat.View(txn.RepeatableRead, func(tx *table.Tx) error {
			r := exec.New(tx, b, budget, os.TempDir())
			top, err := r.Build(p)
			if err != nil {
				return err
			}
			defer top.Close()
			n := 0
			t0 := time.Now()
			err = r.Rows(top, func([]keys.Value) bool {
				n++
				return limit <= 0 || n < limit
			})
			d = time.Since(t0)
			st, ts, rows = r.St, tx.St, n
			return err
		})
		if err != nil {
			return 0, st, ts, 0, err
		}
		if d < best {
			best = d
		}
	}
	return best, st, ts, rows, nil
}

// ---------- 1. nested loop vs hash join ----------

func (l *lab) secJoin() error {
	fmt.Println("=== 1. nested loop vs hash join, theo kích thước vế ngoài")
	fmt.Println("    câu: SELECT ev.city, dim.name FROM ev JOIN dim ON ev.kind=dim.kind WHERE ev.id < W")
	fmt.Println()
	fmt.Printf("%8s %9s %12s %12s %9s %11s %12s %12s\n",
		"W", "hàng ra", "nested(ms)", "hash(ms)", "nl/hash", "vòng ngoài",
		"đọc(nl)", "planner chọn")
	widths := []int{1, 2, 5, 20, 100, 500, 2000, 10000, *rowsFlag}
	for _, w := range widths {
		if w > *rowsFlag {
			continue
		}
		src := fmt.Sprintf(
			"SELECT ev.city, dim.name FROM ev JOIN dim ON ev.kind=dim.kind WHERE ev.id < %d", w)
		b, p, err := l.prep(src, true)
		if err != nil {
			return err
		}
		chosen := joinKindOf(p)

		nlStr, hjStr, ratio := "—", "—", "—"
		var inner, nlRead int
		if w*(*dimFlag) <= *nlCap {
			nl, st, ts, rows, err := l.measure(b, forceJoin(p, false), plan.DefaultBudget, 0)
			if err != nil {
				return err
			}
			inner = st.InnerScans
			// Số entry nested loop THẬT SỰ đọc. In nó cạnh vòng ngoài vì hai
			// con số ấy cùng nhau giải thích cả cột thời gian: đọc ≈ ngoài ×
			// trong, và đó là cả câu chuyện của thuật toán này. Bản đầu chỉ có
			// vòng ngoài, nên ba dòng giữa bảng (vòng ngoài đứng yên ở 200 mà
			// thời gian tăng 13x) trông như số sai.
			nlRead = ts.RowsScanned + ts.IndexEntries + ts.RowFetches
			nlStr = fmt.Sprintf("%.3f", ms(nl))
			hj, _, _, _, err := l.measure(b, forceJoin(p, true), plan.DefaultBudget, 0)
			if err != nil {
				return err
			}
			hjStr = fmt.Sprintf("%.3f", ms(hj))
			ratio = fmt.Sprintf("%.2fx", float64(nl)/float64(hj))
			// inner-scan = số hàng của vế NGOÀI, và vế ngoài không nhất thiết
			// là ev: planner đổi vai build/probe theo ước lượng, nên với W nhỏ
			// thì dim (100 hàng) thành vòng ngoài. Cột này in con số ĐO ĐƯỢC
			// thay vì W, vì bản đầu để nhầm hai thứ ấy vào cùng một cột và
			// bảng trông như bị sai.
			fmt.Printf("%8d %9d %12s %12s %9s %11d %12d %12s\n",
				w, rows, nlStr, hjStr, ratio, inner, nlRead, chosen)
			continue
		}
		hj, _, _, rows, err := l.measure(b, forceJoin(p, true), plan.DefaultBudget, 0)
		if err != nil {
			return err
		}
		hjStr = fmt.Sprintf("%.3f", ms(hj))
		fmt.Printf("%8d %9d %12s %12s %9s %11s %12s %12s\n", w, rows,
			"quá đắt", hjStr, ratio, "-", "-", chosen)
	}
	fmt.Println()
	fmt.Println("    Đọc: nested loop chỉ thắng khi vế ngoài rất ít hàng, vì chi phí của nó là")
	fmt.Println("    |L|·|R| còn hash join là |L|+|R|. Cột inner-scan là số lần vế TRONG bị chạy")
	fmt.Println("    lại. Chú ý nó KHÔNG luôn bằng W: planner chọn vế ít hàng làm build, nên với")
	fmt.Println("    W nhỏ thì dim (200 hàng) thành vòng ngoài. Ba dòng giữa bảng có vòng ngoài")
	fmt.Println("    ĐỨNG YÊN ở 200 mà thời gian tăng 13x — cột đọc(nl) giải thích: vòng TRONG")
	fmt.Println("    lớn dần theo W, và nested loop trả giá bằng phép nhân.")
	fmt.Println()
	return nil
}

func joinKindOf(p plan.PNode) string {
	switch x := p.(type) {
	case *plan.PNestLoop:
		return "NestedLoop"
	case *plan.PHashJoin:
		return "HashJoin"
	case *plan.PFilter:
		return joinKindOf(x.In)
	case *plan.PProject:
		return joinKindOf(x.In)
	case *plan.PLimit:
		return joinKindOf(x.In)
	case *plan.PSort:
		return joinKindOf(x.In)
	}
	return "-"
}

// forceJoin đổi thuật toán join trong một cây đã chọn xong, giữ nguyên hai
// cây con. Nhờ vậy hai con số đo được là hai thuật toán trên CÙNG một access
// path, không lẫn khác biệt nào khác.
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

// ---------- 2. hạn mức bộ nhớ của hash join ----------

func (l *lab) secBudget() error {
	fmt.Println("=== 2. hash join: hạn mức bộ nhớ và cái giá của việc tràn ra đĩa")
	fmt.Printf("    vế build là dim (%d hàng); hạn mức tính bằng HÀNG\n\n", *dimFlag)
	fmt.Printf("%10s %10s %10s %12s %12s %12s %8s\n",
		"hạn mức", "thời gian", "vs tốt nhất", "file tạm", "byte ghi", "hàng đọc lại", "tràn?")
	src := fmt.Sprintf(
		"SELECT ev.city, dim.name FROM ev JOIN dim ON ev.kind=dim.kind WHERE ev.id < %d",
		*rowsFlag)
	b, p, err := l.prep(src, true)
	if err != nil {
		return err
	}
	p = forceJoin(p, true)
	budgets := uniqSorted([]int{1, 4, 16, 64, *dimFlag / 2, *dimFlag, *dimFlag * 2,
		plan.DefaultBudget})
	// Đo HẾT trước, in sau. Bản đầu chia cho "min tính tới lúc này", nên dòng
	// đầu bảng luôn là 1.00x và cả cột tỉ số vô nghĩa — một cột so sánh với
	// một mốc chưa biết là một cột không so sánh gì.
	type row struct {
		bud int
		d   time.Duration
		st  exec.Stat
	}
	var rows []row
	base := time.Duration(1 << 62)
	for _, bud := range budgets {
		d, st, _, _, err := l.measure(b, p, bud, 0)
		if err != nil {
			return err
		}
		rows = append(rows, row{bud, d, st})
		if d < base {
			base = d
		}
	}
	for _, r := range rows {
		spill := "không"
		if r.st.SpillFiles > 0 {
			spill = "CÓ"
		}
		fmt.Printf("%10d %9.3fms %10.2fx %12d %12d %12d %8s\n",
			r.bud, ms(r.d), float64(r.d)/float64(base), r.st.SpillFiles, r.st.SpillBytes,
			r.st.SpillRereads, spill)
	}
	fmt.Println()
	fmt.Println("    Đọc: hạn mức là tham số ĐỔI THUẬT TOÁN, không phải tham số tinh chỉnh.")
	fmt.Println("    Dưới ngưỡng thì hash join chia phần, ghi ra đĩa, đọc lại — mỗi vế đi qua")
	fmt.Println("    ba lần thay vì một. Trên ngưỡng thì thêm bộ nhớ KHÔNG mua được gì nữa.")
	fmt.Println()
	return nil
}

// ---------- 3. ORDER BY: trong RAM vs ngoài ----------

func (l *lab) secSort() error {
	fmt.Println("=== 3. ORDER BY: sắp trong RAM vs sắp ngoài (external merge sort)")
	fmt.Println("    câu: SELECT city FROM ev WHERE id < W ORDER BY city   (city KHÔNG có index)")
	fmt.Println()
	fmt.Printf("%8s %8s %10s %10s %8s %12s %12s\n",
		"W", "hạn mức", "thời gian", "vs RAM", "số run", "file tạm", "hàng đọc lại")
	for _, w := range []int{*rowsFlag / 4, *rowsFlag} {
		src := fmt.Sprintf("SELECT city FROM ev WHERE id < %d ORDER BY city", w)
		b, p, err := l.prep(src, true)
		if err != nil {
			return err
		}
		var inRAM time.Duration
		buds := uniqSorted([]int{w + 1, w / 2, w / 8, w / 32, 512, 64})
		for i := len(buds) - 1; i >= 0; i-- { // từ hạn mức lớn xuống nhỏ
			bud := buds[i]
			if bud < 2 {
				continue
			}
			d, st, _, _, err := l.measure(b, p, bud, 0)
			if err != nil {
				return err
			}
			if inRAM == 0 {
				inRAM = d
			}
			fmt.Printf("%8d %8d %9.3fms %9.2fx %8d %12d %12d\n",
				w, bud, ms(d), float64(d)/float64(inRAM), st.SortRuns,
				st.SpillFiles, st.SpillRereads)
		}
		fmt.Println()
	}
	fmt.Println("    Đọc: cái giá của việc tràn ra đĩa là một BẬC THANG, không phải một đường")
	fmt.Println("    dốc. Bước qua ngưỡng \"không vừa RAM\" tốn ~1.3-1.7x; sau đó giảm hạn mức")
	fmt.Println("    thêm 20 lần (10000 -> 512) gần như KHÔNG tốn thêm gì, dù số run tăng 20 lần.")
	fmt.Println("    Chỉ khi số run lên tới hàng trăm thì bề rộng phép trộn mới hiện ra.")
	fmt.Println()
	fmt.Println("    Suy ra được một điều từ chính chỗ phẳng ấy: phép SO SÁNH không phải chỗ")
	fmt.Println("    tốn. Đi từ 2 run lên 40 run là 5.3 lần nhiều so sánh hơn trong bước trộn")
	fmt.Println("    (log2 40 / log2 2), mà thời gian không đổi — nên cái tốn là ghi và đọc đĩa.")
	fmt.Println("    Trộn rẻ vì khoá được ghi ra ở dạng mã hoá GIỮ THỨ TỰ của phase 7, nên so")
	fmt.Println("    sánh là memcmp trên byte thô (đo riêng ở phase 7: rẻ hơn so theo kiểu 5.7x).")
	fmt.Println()
	fmt.Println("    Đây là lý do tinh chỉnh work_mem của Postgres có một VÁCH chứ không có độ")
	fmt.Println("    dốc: điều đáng biết là ở TRÊN hay DƯỚI ngưỡng, không phải cách ngưỡng bao xa.")
	fmt.Println()
	return nil
}

// ---------- 4. predicate pushdown ----------

func (l *lab) secPushdown() error {
	fmt.Println("=== 4. predicate pushdown: bật vs tắt, trong CHÍNH engine này")
	fmt.Println()
	fmt.Printf("%-58s %10s %10s %8s %11s %11s\n",
		"câu", "tắt(ms)", "bật(ms)", "tỉ số", "hàng(tắt)", "hàng(bật)")
	queries := []string{
		fmt.Sprintf("SELECT city FROM ev WHERE kind = 3 AND id < %d", *rowsFlag),
		"SELECT ev.city, dim.name FROM ev JOIN dim ON ev.kind=dim.kind WHERE ev.kind = 3",
		"SELECT ev.city, dim.name FROM ev JOIN dim ON ev.kind=dim.kind AND dim.kind < 5",
	}
	for _, src := range queries {
		bOff, pOff, err := l.prep(src, false)
		if err != nil {
			return err
		}
		bOn, pOn, err := l.prep(src, true)
		if err != nil {
			return err
		}
		dOff, _, tsOff, _, err := l.measure(bOff, pOff, plan.DefaultBudget, 0)
		if err != nil {
			return err
		}
		dOn, _, tsOn, _, err := l.measure(bOn, pOn, plan.DefaultBudget, 0)
		if err != nil {
			return err
		}
		touchedOff := tsOff.RowsScanned + tsOff.IndexEntries + tsOff.RowFetches
		touchedOn := tsOn.RowsScanned + tsOn.IndexEntries + tsOn.RowFetches
		fmt.Printf("%-58s %10.3f %10.3f %7.2fx %11d %11d\n",
			trunc(src, 58), ms(dOff), ms(dOn), float64(dOff)/float64(dOn), touchedOff, touchedOn)
	}
	fmt.Println()
	fmt.Println("    Đọc: ba cơ chế khác nhau, và ba dòng trên là ba cơ chế ấy.")
	fmt.Println("      1. một bảng: điều kiện thành KHOẢNG QUÉT trên index — hàng không khớp")
	fmt.Println("         không được ĐỌC, chứ không phải đọc rồi bỏ. Đây là chỗ ăn nhiều nhất.")
	fmt.Println("      2. join, điều kiện trên vế build: build teo lại nên bảng băm nhỏ hơn và")
	fmt.Println("         ít khả năng tràn đĩa hơn.")
	fmt.Println("      3. join, điều kiện SUY RA qua phép bằng: `ON ev.kind=dim.kind AND")
	fmt.Println("         dim.kind<5` cho suy ra `ev.kind<5`, và ĐÓ mới là điều kiện đáng đẩy —")
	fmt.Println("         nó thu vế PROBE (20000 hàng), còn đẩy dim.kind chỉ thu vế build (200).")
	fmt.Println("         Trước khi có luật suy ra, dòng thứ ba chỉ đo được 1.18x; chính con số")
	fmt.Println("         ấy chỉ ra luật còn thiếu. Xem plan.propagate.")
	fmt.Println()
	return nil
}

// ---------- 5. bỏ bước ORDER BY nhờ index ----------

func (l *lab) secOrder() error {
	fmt.Println("=== 5. bỏ bước ORDER BY nhờ thứ tự của index (nợ P7-9)")
	fmt.Println()
	fmt.Printf("%-46s %8s %11s %11s %8s\n", "câu", "LIMIT", "có Sort", "bỏ Sort", "tỉ số")
	cases := []struct {
		src   string
		limit int
	}{
		{"SELECT id, kind FROM ev ORDER BY kind", 0},
		{"SELECT id, kind FROM ev ORDER BY kind LIMIT 10", 10},
		{"SELECT id, city FROM ev ORDER BY id", 0},
		{"SELECT id, city FROM ev ORDER BY id LIMIT 10", 10},
	}
	for _, c := range cases {
		b, p, err := l.prep(c.src, true)
		if err != nil {
			return err
		}
		elided := !hasSort(p)
		withSort := forceSort(b, p)
		dSort, _, _, _, err := l.measure(b, withSort, plan.DefaultBudget, 0)
		if err != nil {
			return err
		}
		dNo, _, _, _, err := l.measure(b, p, plan.DefaultBudget, 0)
		if err != nil {
			return err
		}
		lim := "-"
		if c.limit > 0 {
			lim = fmt.Sprintf("%d", c.limit)
		}
		note := ""
		if !elided {
			note = "  (planner KHÔNG bỏ được)"
		}
		fmt.Printf("%-46s %8s %10.3fms %10.3fms %7.2fx%s\n",
			trunc(c.src, 46), lim, ms(dSort), ms(dNo), float64(dSort)/float64(dNo), note)
	}
	fmt.Println()
	fmt.Println("    Đọc: với LIMIT thì tỉ số không còn là một hệ số mà là một BẬC — có Sort thì")
	fmt.Println("    phải đọc và sắp cả bảng để biết 10 hàng đầu; bỏ được Sort thì đọc đúng 10")
	fmt.Println("    hàng rồi dừng. Đó là chỗ toán tử CHẶN đắt hơn nhiều so với chi phí sắp xếp.")
	fmt.Println()
	return nil
}

func hasSort(p plan.PNode) bool {
	switch x := p.(type) {
	case *plan.PSort:
		return true
	case *plan.PFilter:
		return hasSort(x.In)
	case *plan.PProject:
		return hasSort(x.In)
	case *plan.PLimit:
		return hasSort(x.In)
	}
	return false
}

// forceSort cắm lại một PSort ngay dưới Project, dùng khoá sắp của câu gốc.
// Cần để đo được ca "không bỏ Sort" ngay cả khi planner đã bỏ nó.
func forceSort(b *plan.Bound, p plan.PNode) plan.PNode {
	by := sortKeysOf(b.Root)
	if len(by) == 0 {
		return p
	}
	if hasSort(p) {
		return p
	}
	switch x := p.(type) {
	case *plan.PLimit:
		return &plan.PLimit{In: forceSort(b, x.In), N: x.N, E: x.E}
	case *plan.PProject:
		return &plan.PProject{
			In:  &plan.PSort{In: x.In, By: by, Budget: plan.DefaultBudget, E: x.In.Est()},
			Out: x.Out, E: x.E}
	}
	return &plan.PSort{In: p, By: by, Budget: plan.DefaultBudget, E: p.Est()}
}

func sortKeysOf(n plan.Node) []plan.SortKey {
	switch x := n.(type) {
	case *plan.Sort:
		return x.By
	case *plan.Project:
		return sortKeysOf(x.In)
	case *plan.Limit:
		return sortKeysOf(x.In)
	case *plan.Filter:
		return sortKeysOf(x.In)
	}
	return nil
}

// ---------- 6. chi phí front-end ----------

func (l *lab) secPipeline() error {
	fmt.Println("=== 6. chi phí front-end: một câu SQL tốn bao nhiêu TRƯỚC khi chạm dữ liệu")
	fmt.Println()
	fmt.Printf("%-46s %9s %9s %9s %9s %9s %11s\n",
		"câu", "lex(µs)", "parse*", "bind", "optimize", "plan", "thi hành")
	cases := []string{
		"SELECT id, city FROM ev WHERE id = 12345",
		fmt.Sprintf("SELECT city FROM ev WHERE kind = 3 AND id < %d", *rowsFlag),
		"SELECT ev.city, dim.name FROM ev JOIN dim ON ev.kind=dim.kind WHERE ev.id < 100",
	}
	const n = 2000
	for _, src := range cases {
		// KHÔNG trừ lex khỏi parse. Bản đầu in ra `parse-lex` và cột ấy ra
		// SỐ ÂM, vì hai phép đo không đo cùng một thứ: sql.Tokens cấp phát
		// một slice chứa mọi token, còn Parse thì lex theo yêu cầu và không
		// cấp phát slice nào. Nên "parse trừ lex" là trừ hai đại lượng khác
		// nhau, và một số âm là cách bảng số tự tố giác điều đó.
		lex := timeIt(n, func() { sql.Tokens(src) })
		parse := timeIt(n, func() { sql.Parse(src) })
		st, err := sql.Parse(src)
		if err != nil {
			return err
		}
		sel := st.(*sql.Select)
		bind := timeIt(n, func() { plan.BindSelect(l.cat, sel) })
		b, err := plan.BindSelect(l.cat, sel)
		if err != nil {
			return err
		}
		opt := timeIt(n, func() { plan.Optimize(b.Root) })
		root := plan.Optimize(b.Root)
		pl := timeIt(n, func() { l.pl.Plan(b, root) })
		p, err := l.pl.Plan(b, root)
		if err != nil {
			return err
		}
		d, _, _, _, err := l.measure(b, p, plan.DefaultBudget, 0)
		if err != nil {
			return err
		}
		fmt.Printf("%-46s %9.2f %9.2f %9.2f %9.2f %9.2f %10.2fµs\n",
			trunc(src, 46), us(lex), us(parse), us(bind), us(opt), us(pl), us(d))
	}
	fmt.Println()
	fmt.Println("    Đọc: parse* ĐÃ BAO GỒM lex (parser lex theo yêu cầu, không cắt sẵn cả câu),")
	fmt.Println("    nên hai cột đầu không trừ được cho nhau. Với một truy vấn ĐIỂM, front-end là phần")
	fmt.Println("    đáng kể của cả câu — và nó lặp lại y nguyên mỗi lần chạy cùng câu ấy. Đó")
	fmt.Println("    chính là lý do prepared statement tồn tại, và lý do mọi database thật đều")
	fmt.Println("    có plan cache.")
	fmt.Println()
	return nil
}

func timeIt(n int, fn func()) time.Duration {
	best := time.Duration(1 << 62)
	for r := 0; r < *repeat; r++ {
		t0 := time.Now()
		for i := 0; i < n; i++ {
			fn()
		}
		if d := time.Since(t0) / time.Duration(n); d < best {
			best = d
		}
	}
	return best
}

// uniqSorted sắp tăng và bỏ trùng. Một bảng quét mà cột đầu không sắp thì
// người đọc phải tự sắp bằng mắt, và đó là chỗ người ta đọc sai bảng.
func uniqSorted(a []int) []int {
	sort.Ints(a)
	out := a[:0]
	for i, v := range a {
		if v < 1 || (i > 0 && v == a[i-1]) {
			continue
		}
		out = append(out, v)
	}
	return out
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
func us(d time.Duration) float64 { return float64(d) / float64(time.Microsecond) }

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
