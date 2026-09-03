// Package engine nối bốn tầng của phase 8 lại thành một thứ nhận được một
// chuỗi SQL: sql (cú pháp) -> plan (ngữ nghĩa + tối ưu) -> exec (thi hành),
// trên nền table/txn của phase 6-7.
//
// Nó cũng là chỗ quyết định RANH GIỚI TRANSACTION của một câu lệnh, và quyết
// định ấy đáng nói: mỗi câu chạy trong MỘT transaction riêng (autocommit).
// Chưa có BEGIN/COMMIT tường minh — nên chưa thể viết một transaction nhiều
// câu bằng SQL, và mọi bất biến mà phase 6 chứng minh được vẫn chỉ kiểm được
// bằng Go API. Đó là món nợ đáng nhất của phase 8, không phải một thiếu sót
// nhỏ về cú pháp.
package engine

import (
	"fmt"
	"os"
	"strings"
	"time"

	"minidb/internal/exec"
	"minidb/internal/keys"
	"minidb/internal/plan"
	"minidb/internal/query"
	"minidb/internal/sql"
	"minidb/internal/table"
	"minidb/internal/txn"
)

// Engine là một phiên làm việc.
type Engine struct {
	Cat    *table.Catalog
	Pl     *plan.Planner
	Level  txn.Level
	TmpDir string
}

func New(s *txn.Store) (*Engine, error) {
	c, err := table.Load(s)
	if err != nil {
		return nil, err
	}
	return &Engine{Cat: c, Pl: plan.NewPlanner(c), Level: txn.RepeatableRead,
		TmpDir: os.TempDir()}, nil
}

// Result là kết quả của một câu lệnh.
type Result struct {
	Cols    []string
	Rows    [][]keys.Value
	Msg     string        // với DDL/DML: câu thông báo
	Explain string        // với EXPLAIN: cây kế hoạch
	Stat    exec.Stat     // với SELECT: việc thật sự đã làm ở tầng toán tử
	TStat   table.Stat    // và ở tầng bảng: hàng đọc, mục index, lần tra bảng
	Elapsed time.Duration // thời gian của riêng phần thi hành
}

// ExecSQL phân tích rồi chạy một chuỗi có thể nhiều câu, trả kết quả của câu
// CUỐI. Dùng cho test và cho các script.
func (e *Engine) ExecSQL(src string) (*Result, error) {
	stmts, err := sql.ParseMany(src)
	if err != nil {
		return nil, err
	}
	var last *Result
	for _, s := range stmts {
		last, err = e.Exec(s)
		if err != nil {
			return nil, err
		}
	}
	if last == nil {
		return &Result{Msg: "không có câu nào"}, nil
	}
	return last, nil
}

// Exec chạy một câu đã phân tích.
func (e *Engine) Exec(s sql.Stmt) (*Result, error) {
	switch x := s.(type) {
	case *sql.CreateTable:
		return e.createTable(x)
	case *sql.CreateIndex:
		return e.createIndex(x)
	case *sql.Insert:
		return e.insert(x)
	case *sql.AnalyzeStmt:
		return e.analyze(x)
	case *sql.Select:
		return e.selectStmt(x, nil)
	case *sql.Explain:
		return e.explain(x)
	}
	return nil, fmt.Errorf("engine: câu lệnh lạ %T", s)
}

// ---------- DDL ----------

func (e *Engine) createTable(x *sql.CreateTable) (*Result, error) {
	cols := make([]table.Column, len(x.Cols))
	for i, c := range x.Cols {
		cols[i] = table.Column{Name: c.Name, T: c.Type}
	}
	sc, err := e.Cat.CreateTable(x.Name, cols, x.PK)
	if err != nil {
		return nil, err
	}
	return &Result{Msg: fmt.Sprintf("bảng %s (oid %d), %d cột, pk %v",
		sc.Name, sc.OID, len(sc.Cols), pkNames(sc))}, nil
}

func pkNames(sc *table.Schema) []string {
	out := make([]string, len(sc.PK))
	for i, c := range sc.PK {
		out[i] = sc.Cols[c].Name
	}
	return out
}

func (e *Engine) createIndex(x *sql.CreateIndex) (*Result, error) {
	names := make([]string, len(x.Cols))
	desc := false
	for i, c := range x.Cols {
		names[i] = c.Name
		if c.Desc {
			desc = true
		}
	}
	if desc {
		// table.CreateIndex của phase 7 chưa nhận chiều giảm dần cho từng cột,
		// dù bộ mã hoá thì nhận (DESC = lấy bù byte). Từ chối tường minh thay
		// vì im lặng tạo một index ASC: một index sai chiều vẫn cho kết quả
		// ĐÚNG, chỉ là mọi ORDER BY DESC mất cơ hội bỏ bước sắp xếp — đúng
		// loại lỗi im lặng mà cả repo này cố tránh.
		return nil, fmt.Errorf("chưa hỗ trợ cột DESC trong CREATE INDEX (xem nợ P8-6)")
	}
	ix, err := e.Cat.CreateIndex(x.Table, x.Name, names, x.Unique)
	if err != nil {
		return nil, err
	}
	kind := "non-unique"
	if ix.Unique {
		kind = "unique"
	}
	return &Result{Msg: fmt.Sprintf("index %s (oid %d) trên %s%v, %s",
		ix.Name, ix.OID, x.Table, names, kind)}, nil
}

func (e *Engine) analyze(x *sql.AnalyzeStmt) (*Result, error) {
	sc, err := e.Cat.Table(x.Table)
	if err != nil {
		return nil, err
	}
	cols := make([]int, len(sc.Cols))
	for i := range sc.Cols {
		cols[i] = i
	}
	var st query.Stats
	err = e.Cat.View(e.Level, func(tx *table.Tx) error {
		st, err = query.Analyze(tx, sc, cols)
		return err
	})
	if err != nil {
		return nil, err
	}
	e.Pl.Stats[sc.OID] = st
	return &Result{Msg: fmt.Sprintf("%s: %d hàng, thống kê %d cột",
		sc.Name, st.Rows, len(st.Cols))}, nil
}

// ---------- DML ----------

func (e *Engine) insert(x *sql.Insert) (*Result, error) {
	sc, err := e.Cat.Table(x.Table)
	if err != nil {
		return nil, err
	}
	// Ánh xạ cột: danh sách rỗng nghĩa là theo đúng thứ tự cột của bảng.
	idx := make([]int, 0, len(x.Cols))
	for _, name := range x.Cols {
		i, ok := sc.ColIndex(name)
		if !ok {
			return nil, fmt.Errorf("bảng %s không có cột %q", sc.Name, name)
		}
		idx = append(idx, i)
	}
	rows := make([][]keys.Value, 0, len(x.Rows))
	for _, r := range x.Rows {
		row := make([]keys.Value, len(sc.Cols))
		for i := range row {
			row[i] = keys.Null()
		}
		if len(idx) == 0 {
			if len(r) != len(sc.Cols) {
				return nil, fmt.Errorf("bảng %s có %d cột, câu lệnh cho %d giá trị",
					sc.Name, len(sc.Cols), len(r))
			}
			for i, ex := range r {
				v, err := litOf(ex)
				if err != nil {
					return nil, err
				}
				row[i], err = coerceTo(v, sc.Cols[i].T, sc.Cols[i].Name)
				if err != nil {
					return nil, err
				}
			}
		} else {
			if len(r) != len(idx) {
				return nil, fmt.Errorf("danh sách %d cột nhưng cho %d giá trị", len(idx), len(r))
			}
			for i, ex := range r {
				v, err := litOf(ex)
				if err != nil {
					return nil, err
				}
				c := idx[i]
				row[c], err = coerceTo(v, sc.Cols[c].T, sc.Cols[c].Name)
				if err != nil {
					return nil, err
				}
			}
		}
		rows = append(rows, row)
	}

	// MỘT transaction cho CẢ câu lệnh, không phải một transaction mỗi hàng.
	// Phase 7 đo được rằng một transaction mỗi hàng làm 99.7% thời gian là
	// fsync, nên cách chia này quyết định con số mà mọi phép đo nạp dữ liệu
	// của phase 8 sẽ ra.
	t0 := time.Now()
	err = e.Cat.Update(e.Level, func(tx *table.Tx) error {
		for _, row := range rows {
			if err := tx.Insert(sc, row); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &Result{Msg: fmt.Sprintf("chèn %d hàng vào %s", len(rows), sc.Name),
		Elapsed: time.Since(t0)}, nil
}

func litOf(e sql.Expr) (keys.Value, error) {
	l, ok := e.(*sql.LitExpr)
	if !ok {
		return keys.Null(), fmt.Errorf("VALUES chỉ nhận hằng, gặp %s", e)
	}
	return l.V, nil
}

func coerceTo(v keys.Value, t keys.Type, name string) (keys.Value, error) {
	out, err := plan.CoerceValue(v, t)
	if err != nil {
		return v, fmt.Errorf("cột %s: %w", name, err)
	}
	return out, nil
}

// ---------- SELECT ----------

// prepare làm hết phần KHÔNG chạm dữ liệu: bind, tối ưu, chọn kế hoạch.
//
// Tách ra để EXPLAIN không phải chạy truy vấn, và để cmd/sqllab đo được riêng
// từng giai đoạn. Cái đáng nhất của việc tách: một câu sai tên cột chết ở đây,
// và nó không tốn một lần xuống cây nào.
func (e *Engine) prepare(x *sql.Select) (*plan.Bound, plan.Node, plan.PNode, error) {
	b, err := plan.BindSelect(e.Cat, x)
	if err != nil {
		return nil, nil, nil, err
	}
	opt := plan.Optimize(b.Root)
	p, err := e.Pl.Plan(b, opt)
	if err != nil {
		return nil, nil, nil, err
	}
	return b, opt, p, nil
}

func (e *Engine) selectStmt(x *sql.Select, sink func(row []keys.Value) bool) (*Result, error) {
	b, _, p, err := e.prepare(x)
	if err != nil {
		return nil, err
	}
	res := &Result{}
	for _, o := range b.Out {
		res.Cols = append(res.Cols, o.Name)
	}
	t0 := time.Now()
	err = e.Cat.View(e.Level, func(tx *table.Tx) error {
		r := exec.New(tx, b, e.Pl.Budget, e.TmpDir)
		top, err := r.Build(p)
		if err != nil {
			return err
		}
		defer top.Close()
		err = r.Rows(top, func(row []keys.Value) bool {
			if sink != nil {
				return sink(row)
			}
			res.Rows = append(res.Rows, append([]keys.Value(nil), row...))
			return true
		})
		res.Stat, res.TStat = r.St, tx.St
		return err
	})
	res.Elapsed = time.Since(t0)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// Query chạy một câu SELECT theo lối STREAM: fn được gọi cho mỗi hàng và trả
// false để dừng. Đây là API mà LIMIT có nghĩa — xem cmd/sqllab.
func (e *Engine) Query(src string, fn func(row []keys.Value) bool) (*Result, error) {
	s, err := sql.Parse(src)
	if err != nil {
		return nil, err
	}
	sel, ok := s.(*sql.Select)
	if !ok {
		return nil, fmt.Errorf("Query chỉ nhận SELECT, gặp %T", s)
	}
	return e.selectStmt(sel, fn)
}

// ---------- EXPLAIN ----------

// explain in CẢ HAI cây: logical sau khi tối ưu, và physical.
//
// In cả hai vì đó là cách duy nhất thấy được optimizer đã làm gì: cây logical
// cho thấy điều kiện đã bị đẩy xuống đâu (một phép biến đổi đúng bất kể cách
// chạy), cây physical cho thấy đường đi và thuật toán đã chọn (chỉ đúng cho
// một cách chạy). Postgres chỉ in cây physical, nên phép pushdown của nó chỉ
// nhìn thấy được GIÁN TIẾP qua dòng "Index Cond" và "Filter" — và đó chính là
// hai chỗ mà người đọc EXPLAIN hay lẫn.
func (e *Engine) explain(x *sql.Explain) (*Result, error) {
	sel, ok := x.Stmt.(*sql.Select)
	if !ok {
		return nil, fmt.Errorf("EXPLAIN hiện chỉ nhận SELECT, gặp %T", x.Stmt)
	}
	b, opt, p, err := e.prepare(sel)
	if err != nil {
		return nil, err
	}
	var w strings.Builder
	w.WriteString("câu:      " + sel.String() + "\n\n")
	w.WriteString("logical (sau khi đẩy điều kiện xuống):\n")
	w.WriteString(indentBlock(plan.Explain(b.Root), "  gốc:  "))
	w.WriteString(indentBlock(plan.Explain(opt), "  tối ưu: "))
	w.WriteString("\nphysical:\n")
	w.WriteString(indentBlock(plan.ExplainP(p), "  "))
	res := &Result{Explain: w.String()}

	if x.Analyze {
		run, err := e.selectStmt(sel, func([]keys.Value) bool { return true })
		if err != nil {
			return nil, err
		}
		res.Stat, res.TStat = run.Stat, run.TStat
		res.Elapsed = run.Elapsed
		res.Explain += fmt.Sprintf("\nĐO ĐƯỢC: %d hàng ra trong %v\n"+
			"  scan: %d hàng bảng · %d mục index · %d lần tra bảng\n"+
			"  join: %d lần chạy lại vế trong · %d hàng build · %d hàng probe\n"+
			"  sort: %d hàng · %d run ra đĩa\n"+
			"  đĩa tạm: %d file · %d byte ghi · %d hàng đọc lại\n",
			run.Stat.RowsOut, run.Elapsed.Round(time.Microsecond),
			run.TStat.RowsScanned, run.TStat.IndexEntries, run.TStat.RowFetches,
			run.Stat.InnerScans, run.Stat.HashBuild, run.Stat.HashProbe,
			run.Stat.SortRows, run.Stat.SortRuns,
			run.Stat.SpillFiles, run.Stat.SpillBytes, run.Stat.SpillRereads)
	}
	return res, nil
}

func indentBlock(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	var w strings.Builder
	pad := strings.Repeat(" ", len(prefix))
	for i, l := range lines {
		if i == 0 {
			w.WriteString(prefix + l + "\n")
		} else {
			w.WriteString(pad + l + "\n")
		}
	}
	return w.String()
}
