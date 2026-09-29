// reallab là bài lab của phase 9: lấy từng con số đã đo trên minidb, đặt câu
// hỏi y hệt cho Postgres, MySQL (InnoDB) và MariaDB (InnoDB fork), rồi so.
//
// Tám bảng, tám câu hỏi:
//
//	-work breakeven : index scan thắng seq scan tới độ chọn lọc nào (phase 7: 36.8%)
//	-work anomaly   : anomaly nào lọt ở mức isolation nào (phase 6: bảng 5×4)
//	-work pkorder   : PK tăng dần vs ngẫu nhiên đắt khác nhau bao nhiêu (phase 4: 33x)
//	-work bloat     : một transaction chạy lâu làm phình cái gì (phase 6: ChainStats)
//	-work stats     : planner chọn sai khi thống kê lệch (phase 7: nợ P7-6)
//	-work crash     : kill -9 giữa lúc ghi, có mất commit nào không (phase 5: crashlab)
//	-work lograte   : ở chế độ không chờ log, redo được write() theo nhịp nào (nợ P9-6)
//	-work hashjoin  : vì sao hash join tràn đĩa lại nhanh hơn trong RAM (nợ P9-7)
//
// Cần lab đang chạy: `docker compose -f reallab/docker-compose.yml up -d`.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// Engine là một DB trong lab. Kind quyết định cú pháp: "pg" hoặc "my" (MySQL
// và MariaDB nói cùng một phương ngữ ở mọi chỗ bài lab dùng tới, trừ vài chỗ
// ghi rõ tại chỗ).
type Engine struct {
	Name, Kind string
	DB         *sql.DB
}

var dsns = map[string][2]string{
	"pg":    {"pgx", "postgres://postgres:lab@127.0.0.1:55432/lab?sslmode=disable&default_query_exec_mode=simple_protocol"},
	"mysql": {"mysql", "root:lab@tcp(127.0.0.1:53306)/lab?multiStatements=true&interpolateParams=true"},
	"maria": {"mysql", "root:lab@tcp(127.0.0.1:53307)/lab?multiStatements=true&interpolateParams=true"},
	// Postgres với glibc malloc không trả bộ nhớ lại cho OS (nợ P9-7, bảng 8).
	"pgm": {"pgx", "postgres://postgres:lab@127.0.0.1:55433/lab?sslmode=disable&default_query_exec_mode=simple_protocol"},
}

func open(name string) (*Engine, error) {
	d, ok := dsns[name]
	if !ok {
		return nil, fmt.Errorf("không biết db %q (pg|mysql|maria|pgm)", name)
	}
	db, err := sql.Open(d[0], d[1])
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(16)
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("%s: %w (lab đã chạy chưa?)", name, err)
	}
	kind := "my"
	if strings.HasPrefix(name, "pg") {
		kind = "pg"
	}
	return &Engine{Name: name, Kind: kind, DB: db}, nil
}

func main() {
	var (
		work   = flag.String("work", "all", "breakeven | anomaly | pkorder | bloat | stats | crash | lograte | hashjoin | all")
		dbs    = flag.String("db", "pg,mysql,maria", "danh sách DB, cách nhau bằng dấu phẩy")
		rows   = flag.Int("rows", 1_000_000, "số hàng cho breakeven / stats")
		repeat = flag.Int("repeat", 5, "số lần chạy mỗi truy vấn, lấy trung vị")
		rounds = flag.Int("rounds", 5, "số vòng kill -9 mỗi chế độ (crash)")
	)
	flag.Parse()

	var es []*Engine
	for _, n := range strings.Split(*dbs, ",") {
		e, err := open(strings.TrimSpace(n))
		if err != nil {
			die(err)
		}
		es = append(es, e)
	}
	bad := false
	run := func(name string, fn func() error) {
		if *work != "all" && *work != name {
			return
		}
		if err := fn(); err != nil {
			fmt.Fprintf(os.Stderr, "\n%s: %v\n", name, err)
			bad = true
		}
	}
	run("breakeven", func() error { return workBreakEven(es, *rows, *repeat) })
	run("anomaly", func() error { return workAnomaly(es) })
	run("pkorder", func() error { return workPKOrder(es) })
	run("bloat", func() error { return workBloat(es) })
	run("stats", func() error { return workStats(es, *rows) })
	run("crash", func() error { return workCrash(es, *rounds) })
	run("lograte", func() error { return workLogRate(es) })
	run("hashjoin", func() error { return workHashJoin(es, *repeat) })
	if bad {
		os.Exit(1)
	}
}

// exec chạy nhiều câu, câu nào lỗi thì dừng và nói rõ câu nào.
func exec(ctx context.Context, q interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, stmts ...string) error {
	for _, s := range stmts {
		if _, err := q.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("%s: %w", firstLine(s), err)
		}
	}
	return nil
}

// median chạy fn n lần (cộng một lần làm nóng không tính) và trả trung vị.
// Trung vị chứ không phải trung bình: một lần GC hay một lần checkpoint
// không được phép kéo lệch cả hàng.
func median(n int, fn func() error) (time.Duration, error) {
	if err := fn(); err != nil {
		return 0, err
	}
	ds := make([]time.Duration, n)
	for i := range ds {
		t := time.Now()
		if err := fn(); err != nil {
			return 0, err
		}
		ds[i] = time.Since(t)
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	return ds[n/2], nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " …"
	}
	if len(s) > 80 {
		s = s[:80] + " …"
	}
	return s
}

func ms(d time.Duration) string { return fmt.Sprintf("%.1f", float64(d.Microseconds())/1000) }

func die(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
