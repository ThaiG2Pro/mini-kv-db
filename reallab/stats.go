package main

// Bảng 5 — planner đoán sai số hàng khi nào, và mỗi DB tự chữa bằng gì.
//
// minidb ước lượng bằng phân bố ĐỀU từ min/max (nợ P7-6). Phase 7 đã thấy
// planner chọn sai chỉ vì một hằng số chi phí; ở đây hỏi nguồn sai thứ hai,
// và là nguồn số một ở DB thật: ước lượng SỐ HÀNG sai.
//
// Ba ca, mỗi ca đánh vào một giả định của bộ ước lượng:
//
//	lệch     : 98% 'done', 1% 'pending', 1% 'failed'  — giả định phân bố đều
//	tương quan: city quyết định country                — giả định độc lập
//	cũ       : ANALYZE lúc bảng có 1000 hàng 'done', rồi nạp thêm 1 triệu hàng,
//	           một nửa 'pending', không ANALYZE lại     — giả định thống kê còn mới
//
// Mỗi dòng in: ước lượng của planner, số hàng thật, lệch bao nhiêu lần, plan
// đã chọn và thời gian chạy thật (đo phía server).

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

func workStats(es []*Engine, n int) error {
	if n > 1_000_000 {
		n = 1_000_000
	}
	fmt.Printf("\n== 5. planner ước lượng sai số hàng (%d hàng) ==\n", n)
	for _, e := range es {
		if err := statsOne(e, n); err != nil {
			return fmt.Errorf("%s: %w", e.Name, err)
		}
	}
	return nil
}

func loadST(ctx context.Context, e *Engine, c *sql.Conn, n int) error {
	if e.Kind == "pg" {
		return exec(ctx, c,
			`DROP TABLE IF EXISTS st`,
			`CREATE TABLE st (id int PRIMARY KEY, status text NOT NULL, city text NOT NULL, country text NOT NULL, pad text NOT NULL)`,
			`SELECT setseed(0.7)`,
			fmt.Sprintf(`INSERT INTO st SELECT g,
				CASE WHEN r1 < 0.98 THEN 'done' WHEN r1 < 0.99 THEN 'pending' ELSE 'failed' END,
				'c' || c, 'k' || (c %% 10), repeat('x', 100)
			 FROM (SELECT g, random() r1, floor(random()*100)::int c FROM generate_series(1,%d) g) s`, n),
			`CREATE INDEX st_status ON st (status)`,
			`CREATE INDEX st_city ON st (city)`,
			`VACUUM ANALYZE st`,
			`SET max_parallel_workers_per_gather = 0`,
		)
	}
	return exec(ctx, c,
		`DROP TABLE IF EXISTS st, d10, d1000`,
		`CREATE TABLE d10 (n int)`,
		`INSERT INTO d10 VALUES (0),(1),(2),(3),(4),(5),(6),(7),(8),(9)`,
		`CREATE TABLE d1000 AS SELECT a.n*100+b.n*10+c.n AS n FROM d10 a, d10 b, d10 c`,
		`CREATE TABLE st (id int PRIMARY KEY, status varchar(10) NOT NULL, city varchar(10) NOT NULL,
			country varchar(10) NOT NULL, pad varchar(100) NOT NULL) ENGINE=InnoDB`,
		fmt.Sprintf(`INSERT INTO st SELECT id,
				CASE WHEN r1 < 0.98 THEN 'done' WHEN r1 < 0.99 THEN 'pending' ELSE 'failed' END,
				CONCAT('c', c), CONCAT('k', c %% 10), REPEAT('x', 100)
			 FROM (SELECT a.n*1000+b.n+1 id, RAND(7) r1, FLOOR(RAND(11)*100) c
			       FROM d1000 a, d1000 b WHERE a.n*1000+b.n < %d
			       LIMIT 18446744073709551615) s`, n),
		`CREATE INDEX st_status ON st (status)`,
		`CREATE INDEX st_city ON st (city)`,
		`ANALYZE TABLE st`,
		`DROP TABLE d10, d1000`,
	)
}

// loadStale: ANALYZE lúc bảng còn bé, rồi nạp thêm mà không cho DB tự
// ANALYZE lại (tắt autovacuum / STATS_AUTO_RECALC cho riêng bảng này).
// n ở đây là số hàng nạp THÊM; giữ nhỏ để st + st2 vẫn vừa buffer pool.
func loadStale(ctx context.Context, e *Engine, c *sql.Conn, n int) error {
	if e.Kind == "pg" {
		return exec(ctx, c,
			`DROP TABLE IF EXISTS st2`,
			`CREATE TABLE st2 (id int PRIMARY KEY, status text NOT NULL, pad text NOT NULL) WITH (autovacuum_enabled = off)`,
			`INSERT INTO st2 SELECT g, 'done', repeat('x',100) FROM generate_series(1,1000) g`,
			`CREATE INDEX st2_status ON st2 (status)`,
			`ANALYZE st2`,
			fmt.Sprintf(`INSERT INTO st2 SELECT g, CASE WHEN g %% 2 = 0 THEN 'pending' ELSE 'done' END, repeat('x',100)
			 FROM generate_series(1001, %d) g`, n+1000),
		)
	}
	return exec(ctx, c,
		`DROP TABLE IF EXISTS st2, d10, d1000`,
		`CREATE TABLE d10 (n int)`,
		`INSERT INTO d10 VALUES (0),(1),(2),(3),(4),(5),(6),(7),(8),(9)`,
		`CREATE TABLE d1000 AS SELECT a.n*100+b.n*10+c.n AS n FROM d10 a, d10 b, d10 c`,
		`CREATE TABLE st2 (id int PRIMARY KEY, status varchar(10) NOT NULL, pad varchar(100) NOT NULL)
			ENGINE=InnoDB STATS_AUTO_RECALC=0`,
		`INSERT INTO st2 SELECT n+1, 'done', REPEAT('x',100) FROM d1000`,
		`CREATE INDEX st2_status ON st2 (status)`,
		`ANALYZE TABLE st2`,
		fmt.Sprintf(`INSERT INTO st2 SELECT id, CASE WHEN id %% 2 = 0 THEN 'pending' ELSE 'done' END, REPEAT('x',100)
			FROM (SELECT 1001 + a.n*1000+b.n id FROM d1000 a, d1000 b WHERE a.n*1000+b.n < %d) s`, n),
		`DROP TABLE d10, d1000`,
	)
}

func statsOne(e *Engine, n int) error {
	ctx := context.Background()
	c, err := e.DB.Conn(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := loadST(ctx, e, c, n); err != nil {
		return err
	}
	// Dọn bảng của các thí nghiệm trước để st + st2 vừa buffer pool 256MB.
	if err := exec(ctx, c, `DROP TABLE IF EXISTS be, pk, bl, acc`); err != nil {
		return err
	}
	if err := loadStale(ctx, e, c, n/5); err != nil {
		return err
	}
	fmt.Printf("\n-- %s\n", e.Name)
	fmt.Printf("%-40s%10s%10s%10s  %-30s%9s\n", "ca", "ước lượng", "thật", "lệch", "plan", "ms")
	row := func(label, q string) error {
		est, act, plan, t, err := estimate(ctx, e, c, q)
		if err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		fmt.Printf("%-40s%10.0f%10.0f%10s  %-30s%9.1f\n", label, est, act, skew(est, act), plan, t)
		return nil
	}
	sel := `SELECT id, pad FROM `
	cases := [][2]string{
		{"lệch: status='pending' (1%)", sel + `st WHERE status = 'pending'`},
		{"lệch: status='done' (98%)", sel + `st WHERE status = 'done'`},
		{"tương quan: city='c7' AND country='k7'", sel + `st WHERE city = 'c7' AND country = 'k7'`},
		{"tương quan: city='c7' AND country='k8'", sel + `st WHERE city = 'c7' AND country = 'k8'`},
		{"cũ: status='pending' (50%)", sel + `st2 WHERE status = 'pending'`},
		{"cũ + JOIN st", `SELECT a.id, b.pad FROM st2 a JOIN st b ON b.id = a.id WHERE a.status = 'pending'`},
	}
	for _, cs := range cases {
		if err := row(cs[0], cs[1]); err != nil {
			return err
		}
	}

	// Mỗi DB chữa bằng công cụ của nó, rồi hỏi lại đúng những câu đã sai.
	var fix []string
	var fixName string
	switch e.Name {
	case "pg":
		fixName = "CREATE STATISTICS (city, country) + ANALYZE st2"
		fix = []string{`CREATE STATISTICS IF NOT EXISTS st_cc (dependencies, mcv) ON city, country FROM st`, `ANALYZE st`, `ANALYZE st2`}
	case "mysql":
		fixName = "histogram trên country + ANALYZE st2"
		fix = []string{`ANALYZE TABLE st UPDATE HISTOGRAM ON country WITH 64 BUCKETS`, `ANALYZE TABLE st2`}
	default:
		fixName = "ANALYZE ... PERSISTENT FOR ALL (histogram) + ANALYZE st2"
		fix = []string{`ANALYZE TABLE st PERSISTENT FOR ALL`, `ANALYZE TABLE st2`}
	}
	if err := exec(ctx, c, fix...); err != nil {
		return err
	}
	fmt.Printf("sau khi chữa: %s\n", fixName)
	for _, cs := range cases[2:] {
		if err := row(cs[0], cs[1]); err != nil {
			return err
		}
	}
	return nil
}

func skew(est, act float64) string {
	if est < 1 {
		est = 1
	}
	if act < 1 {
		act = 1
	}
	r := act / est
	if r < 1 {
		return fmt.Sprintf("÷%.1f", 1/r)
	}
	return fmt.Sprintf("×%.1f", r)
}

// estimate chạy câu truy vấn dưới EXPLAIN ANALYZE của từng DB và trả về
// (hàng ước lượng, hàng thật, tên plan, ms phía server).
func estimate(ctx context.Context, e *Engine, c *sql.Conn, q string) (float64, float64, string, float64, error) {
	switch e.Name {
	case "pg":
		var js string
		if err := c.QueryRowContext(ctx, `EXPLAIN (ANALYZE, FORMAT JSON) `+q).Scan(&js); err != nil {
			return 0, 0, "", 0, err
		}
		var out []struct {
			Plan struct {
				NodeType   string  `json:"Node Type"`
				IndexName  string  `json:"Index Name"`
				PlanRows   float64 `json:"Plan Rows"`
				ActualRows float64 `json:"Actual Rows"`
				Plans      []struct {
					IndexName string `json:"Index Name"`
				} `json:"Plans"`
			} `json:"Plan"`
			ExecutionTime float64 `json:"Execution Time"`
		}
		if err := json.Unmarshal([]byte(js), &out); err != nil {
			return 0, 0, "", 0, err
		}
		p := out[0].Plan
		name := p.NodeType
		idx := p.IndexName
		if idx == "" && len(p.Plans) > 0 {
			idx = p.Plans[0].IndexName
		}
		if idx != "" {
			name += " " + idx
		}
		return p.PlanRows, p.ActualRows, name, out[0].ExecutionTime, nil
	case "mysql":
		var txt string
		if err := c.QueryRowContext(ctx, `EXPLAIN ANALYZE `+q).Scan(&txt); err != nil {
			return 0, 0, "", 0, err
		}
		return parseMyAnalyze(txt)
	default:
		var js string
		if err := c.QueryRowContext(ctx, `ANALYZE FORMAT=JSON `+q).Scan(&js); err != nil {
			return 0, 0, "", 0, err
		}
		return parseMariaAnalyze(js)
	}
}

var reMyTop = regexp.MustCompile(`rows=([0-9.e+]+)\) \(actual time=[0-9.]+\.\.([0-9.]+) rows=([0-9.e+]+)`)

// parseMyAnalyze đọc dòng ĐẦU của cây EXPLAIN ANALYZE (nút gốc: ước lượng và
// thật của cả câu), và lấy tên phép truy cập ở dòng cuối (lá).
func parseMyAnalyze(txt string) (float64, float64, string, float64, error) {
	lines := strings.Split(strings.TrimSpace(txt), "\n")
	m := reMyTop.FindStringSubmatch(lines[0])
	if m == nil {
		return 0, 0, "", 0, fmt.Errorf("không đọc được: %s", lines[0])
	}
	est, _ := strconv.ParseFloat(m[1], 64)
	t, _ := strconv.ParseFloat(m[2], 64)
	act, _ := strconv.ParseFloat(m[3], 64)
	leaf := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lines[len(lines)-1]), "->"))
	if top := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lines[0]), "->")); strings.Contains(strings.ToLower(top), "join") {
		leaf = strings.TrimSpace(top[:strings.Index(top, "  (")])
	}
	switch {
	case strings.HasPrefix(leaf, "Table scan"):
		leaf = "Table scan"
	case strings.HasPrefix(leaf, "Index lookup"):
		leaf = "Index lookup " + between(leaf, " using ", " ")
	case strings.HasPrefix(leaf, "Index range scan"):
		leaf = "Index range " + between(leaf, " using ", " ")
	default:
		if len(leaf) > 28 {
			leaf = leaf[:28]
		}
	}
	return est, act, leaf, t, nil
}

func between(s, a, b string) string {
	i := strings.Index(s, a)
	if i < 0 {
		return ""
	}
	s = s[i+len(a):]
	if j := strings.Index(s, b); j >= 0 {
		s = s[:j]
	}
	return s
}

// parseMariaAnalyze: ANALYZE FORMAT=JSON cho mỗi bảng "rows"/"filtered" (ước
// lượng) và "r_rows"/"r_filtered" (thật). Hàng ra = rows × filtered%.
func parseMariaAnalyze(js string) (float64, float64, string, float64, error) {
	var root map[string]any
	if err := json.Unmarshal([]byte(js), &root); err != nil {
		return 0, 0, "", 0, err
	}
	qb, _ := root["query_block"].(map[string]any)
	t := num(qb["r_total_time_ms"])
	tbl := findKey(qb, "table")
	if tbl == nil {
		return 0, 0, "", 0, fmt.Errorf("không thấy khoá table trong %s", js)
	}
	est := num(tbl["rows"]) * num(tbl["filtered"]) / 100
	act := num(tbl["r_rows"]) * num(tbl["r_filtered"]) / 100
	plan := fmt.Sprint(tbl["access_type"])
	if k, ok := tbl["key"]; ok {
		plan += " " + fmt.Sprint(k)
	}
	return math.Round(est), math.Round(act), plan, t, nil
}

func num(v any) float64 {
	f, _ := v.(float64)
	return f
}

func findKey(v any, key string) map[string]any {
	switch x := v.(type) {
	case map[string]any:
		if t, ok := x[key].(map[string]any); ok {
			return t
		}
		for _, c := range x {
			if r := findKey(c, key); r != nil {
				return r
			}
		}
	case []any:
		for _, c := range x {
			if r := findKey(c, key); r != nil {
				return r
			}
		}
	}
	return nil
}
