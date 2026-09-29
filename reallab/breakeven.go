package main

// Bảng 1 — điểm hoà vốn selectivity.
//
// Câu hỏi y hệt phase 7: `WHERE k < v` trên cột có index, k phân bố đều và
// KHÔNG tương quan với thứ tự vật lý của hàng (ca xấu nhất cho index, cùng ca
// "khóa nhảy lung tung" đã cho 36.8% trên minidb). Truy vấn phải đọc `pad` để
// index-only scan không có đường tắt.
//
// Ba điều phải giữ để phép so công bằng:
//  1. Dữ liệu nằm trọn trong buffer pool (256MB, bảng ~150MB), giống minidb.
//     Nên đây vẫn là tỉ số CPU + số lần chạm cấu trúc, không phải tỉ số I/O.
//  2. Postgres tắt truy vấn song song: minidb, MySQL và MariaDB đều chạy một
//     truy vấn trên một luồng.
//  3. Ép từng plan bằng cơ chế của chính DB đó (enable_* / FORCE INDEX), rồi
//     hỏi riêng planner nó TỰ chọn gì — hai câu hỏi khác nhau.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

var beSels = []float64{0.001, 0.01, 0.02, 0.05, 0.10, 0.20, 0.30, 0.50, 0.70, 1.00}

func workBreakEven(es []*Engine, n, rep int) error {
	if n > 1_000_000 {
		return fmt.Errorf("breakeven: -rows tối đa 1000000 (bộ sinh dữ liệu MySQL dùng 1000×1000)")
	}
	fmt.Printf("== 1. điểm hoà vốn selectivity (%d hàng, k ngẫu nhiên, trung vị %d lần, ms) ==\n", n, rep)
	for _, e := range es {
		if err := breakEvenOne(e, n, rep); err != nil {
			return fmt.Errorf("%s: %w", e.Name, err)
		}
	}
	return nil
}

func loadBE(ctx context.Context, e *Engine, c *sql.Conn, n int) error {
	if e.Kind == "pg" {
		return exec(ctx, c,
			`DROP TABLE IF EXISTS be`,
			`CREATE TABLE be (id int PRIMARY KEY, k int NOT NULL, pad text NOT NULL)`,
			`SELECT setseed(0.42)`,
			fmt.Sprintf(`INSERT INTO be SELECT g, floor(random()*%d)::int, repeat('x',100)
			 FROM generate_series(1,%d) g`, n, n),
			`CREATE INDEX be_k ON be (k)`,
			`VACUUM ANALYZE be`,
		)
	}
	return exec(ctx, c,
		`DROP TABLE IF EXISTS be, d10, d1000`,
		`CREATE TABLE d10 (n int)`,
		`INSERT INTO d10 VALUES (0),(1),(2),(3),(4),(5),(6),(7),(8),(9)`,
		`CREATE TABLE d1000 AS SELECT a.n*100+b.n*10+c.n AS n FROM d10 a, d10 b, d10 c`,
		`CREATE TABLE be (id int PRIMARY KEY, k int NOT NULL, pad varchar(100) NOT NULL) ENGINE=InnoDB`,
		fmt.Sprintf(`INSERT INTO be SELECT a.n*1000+b.n+1, FLOOR(RAND(42)*%d), REPEAT('x',100)
		 FROM d1000 a, d1000 b WHERE a.n*1000+b.n < %d ORDER BY 1`, n, n),
		`CREATE INDEX be_k ON be (k)`,
		`ANALYZE TABLE be`,
		`DROP TABLE d10, d1000`,
	)
}

type bePlan struct{ name, pgSet, myHint string }

func bePlans(e *Engine) []bePlan {
	if e.Kind == "pg" {
		return []bePlan{
			{"seq", "SET enable_seqscan=on; SET enable_indexscan=off; SET enable_bitmapscan=off", ""},
			{"index", "SET enable_seqscan=off; SET enable_indexscan=on; SET enable_bitmapscan=off", ""},
			{"bitmap", "SET enable_seqscan=off; SET enable_indexscan=off; SET enable_bitmapscan=on", ""},
		}
	}
	return []bePlan{
		{"seq", "", "IGNORE INDEX (be_k)"},
		{"index", "", "FORCE INDEX (be_k)"},
	}
}

func breakEvenOne(e *Engine, n, rep int) error {
	ctx := context.Background()
	c, err := e.DB.Conn(ctx)
	if err != nil {
		return err
	}
	defer c.Close()

	t0 := time.Now()
	if err := loadBE(ctx, e, c, n); err != nil {
		return err
	}
	if e.Kind == "pg" {
		if err := exec(ctx, c, `SET max_parallel_workers_per_gather = 0`); err != nil {
			return err
		}
	}
	fmt.Printf("\n-- %s (nạp %d hàng mất %s)\n", e.Name, n, time.Since(t0).Round(time.Millisecond))

	plans := bePlans(e)
	fmt.Printf("%-8s", "sel")
	for _, p := range plans {
		fmt.Printf("%10s", p.name)
	}
	fmt.Printf("%12s   %s\n", "index/seq", "planner tự chọn")

	var sels, ratios []float64
	for _, s := range beSels {
		v := int(s * float64(n))
		var ts []time.Duration
		for _, p := range plans {
			if p.pgSet != "" {
				if err := exec(ctx, c, strings.Split(p.pgSet, "; ")...); err != nil {
					return err
				}
			}
			q := fmt.Sprintf(`SELECT count(*), sum(length(pad)) FROM be %s WHERE k < %d`, p.myHint, v)
			d, err := median(rep, func() error {
				var cnt, sum sql.NullInt64
				return c.QueryRowContext(ctx, q).Scan(&cnt, &sum)
			})
			if err != nil {
				return err
			}
			ts = append(ts, d)
		}
		if e.Kind == "pg" {
			if err := exec(ctx, c, `RESET enable_seqscan`, `RESET enable_indexscan`, `RESET enable_bitmapscan`); err != nil {
				return err
			}
		}
		choice, err := planChoice(ctx, e, c, fmt.Sprintf(`SELECT count(*), sum(length(pad)) FROM be WHERE k < %d`, v))
		if err != nil {
			return err
		}
		r := float64(ts[1]) / float64(ts[0])
		sels, ratios = append(sels, s), append(ratios, r)
		fmt.Printf("%-8s", fmt.Sprintf("%.1f%%", s*100))
		for _, d := range ts {
			fmt.Printf("%10s", ms(d))
		}
		fmt.Printf("%11.2fx   %s\n", r, choice)
	}
	if x, ok := crossOver(sels, ratios); ok {
		fmt.Printf("  -> hoà vốn index vs seq ĐO ĐƯỢC: %.1f%%\n", x*100)
	} else {
		fmt.Printf("  -> index không hoà vốn trong khoảng đo\n")
	}
	return nil
}

// crossOver nội suy tuyến tính điểm đầu tiên tỉ số index/seq vượt 1.
func crossOver(sels, ratios []float64) (float64, bool) {
	for i := 1; i < len(sels); i++ {
		if ratios[i-1] < 1 && ratios[i] >= 1 {
			f := (1 - ratios[i-1]) / (ratios[i] - ratios[i-1])
			return sels[i-1] + f*(sels[i]-sels[i-1]), true
		}
	}
	return 0, false
}

// planChoice hỏi planner nó tự chọn gì, trả một chữ gọn.
func planChoice(ctx context.Context, e *Engine, c *sql.Conn, q string) (string, error) {
	rows, err := c.QueryContext(ctx, "EXPLAIN "+q)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var out []string
	for rows.Next() {
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return "", err
		}
		if e.Kind == "pg" {
			l := vals[0].String
			if strings.Contains(l, "Bitmap Index Scan") {
				continue // con của Bitmap Heap Scan, không phải plan riêng
			}
			for _, n := range []string{"Bitmap Heap Scan", "Index Only Scan", "Index Scan", "Seq Scan"} {
				if strings.Contains(l, n) {
					out = append(out, n)
					break
				}
			}
			continue
		}
		m := map[string]string{}
		for i, cn := range cols {
			m[cn] = vals[i].String
		}
		s := "type=" + m["type"]
		if m["key"] != "" {
			s += " key=" + m["key"]
		}
		if m["Extra"] != "" {
			s += " (" + m["Extra"] + ")"
		}
		out = append(out, s)
	}
	return strings.Join(out, " + "), rows.Err()
}
