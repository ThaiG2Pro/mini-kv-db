package main

// Bảng 7 — nợ P9-6: ở chế độ "không chờ log" (flush_log_at_trx_commit=0),
// redo được write() xuống OS theo nhịp nào?
//
// kill -9 chỉ làm mất phần redo còn nằm trong RAM của tiến trình, tức phần
// chưa được write(). Nên số commit mất sau một lần kill xấp xỉ
//
//	tốc độ commit × khoảng thời gian từ lần write() gần nhất
//
// Bài này đo khoảng thời gian đó: một goroutine chèn liên tục (mỗi câu một
// commit), một kết nối khác đọc bộ đếm "redo đã ra khỏi tiến trình" mỗi 5ms,
// và ghi lại những lúc bộ đếm nhảy. Khoảng cách giữa hai lần nhảy là khoảng
// "cửa sổ mất".
//
// Bộ đếm KHÔNG giống nhau giữa hai DB, và đây là cái bẫy đã sập một lần:
//   - MySQL: Innodb_os_log_written cộng dồn số byte thật sự được write().
//   - MariaDB: Innodb_os_log_written tăng đúng bằng LSN hiện tại (đo được:
//     cùng tăng 850223 byte trong cùng 0.4s), tức là đếm redo được SINH RA,
//     không phải được ghi. Lượt đo đầu dùng nó và thấy MariaDB "ghi mỗi
//     5ms" — trái với 9123 commit mất. Phải đọc Innodb_lsn_flushed. MariaDB
//     mở redo với O_DIRECT (innodb_log_file_buffering=OFF) nên write() và
//     xuống đĩa là cùng một lần.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"sync/atomic"
	"time"
)

type logMode struct {
	name         string
	setup, reset []string
}

func logModes(e *Engine) []logMode {
	ms := []logMode{{"flush_log_at_trx_commit=0",
		[]string{`SET GLOBAL innodb_flush_log_at_trx_commit = 0`},
		[]string{`SET GLOBAL innodb_flush_log_at_trx_commit = 1`}}}
	if e.Name == "mysql" {
		ms = []logMode{
			{"=0 + sync_binlog=0",
				[]string{`SET GLOBAL innodb_flush_log_at_trx_commit = 0`, `SET GLOBAL sync_binlog = 0`},
				[]string{`SET GLOBAL innodb_flush_log_at_trx_commit = 1`, `SET GLOBAL sync_binlog = 1`}},
			{"=0 + sync_binlog=0 + log_writer_threads=OFF",
				[]string{`SET GLOBAL innodb_flush_log_at_trx_commit = 0`, `SET GLOBAL sync_binlog = 0`, `SET GLOBAL innodb_log_writer_threads = OFF`},
				[]string{`SET GLOBAL innodb_flush_log_at_trx_commit = 1`, `SET GLOBAL sync_binlog = 1`, `SET GLOBAL innodb_log_writer_threads = ON`}},
		}
	}
	return ms
}

func workLogRate(es []*Engine) error {
	fmt.Printf("\n== 7. nhịp write() của redo ở chế độ không chờ log (3s, lấy mẫu mỗi 5ms) ==\n")
	fmt.Printf("%-8s%-46s%10s%10s%12s%12s%14s\n", "db", "chế độ", "commit/s", "số write", "khoảng p50", "khoảng max", " dự báo mất/kill")
	for _, e := range es {
		if e.Kind != "my" {
			continue
		}
		for _, m := range logModes(e) {
			if err := logRateOne(e, m); err != nil {
				return fmt.Errorf("%s/%s: %w", e.Name, m.name, err)
			}
		}
	}
	return nil
}

func logWritten(ctx context.Context, e *Engine, c *sql.Conn) (int64, error) {
	counter := "Innodb_os_log_written"
	if e.Name == "maria" {
		counter = "Innodb_lsn_flushed"
	}
	var name string
	var v int64
	err := c.QueryRowContext(ctx, `SHOW GLOBAL STATUS LIKE '`+counter+`'`).Scan(&name, &v)
	return v, err
}

func logRateOne(e *Engine, m logMode) error {
	ctx := context.Background()
	if err := exec_(e, append([]string{`DROP TABLE IF EXISTS lr`, `CREATE TABLE lr (id int PRIMARY KEY)`}, m.setup...)); err != nil {
		return err
	}
	defer exec_(e, m.reset)

	w, err := e.DB.Conn(ctx)
	if err != nil {
		return err
	}
	defer w.Close()
	s, err := e.DB.Conn(ctx)
	if err != nil {
		return err
	}
	defer s.Close()

	var n atomic.Int64
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		for i := int64(1); ; i++ {
			select {
			case <-stop:
				done <- nil
				return
			default:
			}
			if _, err := w.ExecContext(ctx, fmt.Sprintf(`INSERT INTO lr VALUES (%d)`, i)); err != nil {
				done <- err
				return
			}
			n.Store(i)
		}
	}()

	time.Sleep(300 * time.Millisecond) // cho tốc độ ổn định
	prev, err := logWritten(ctx, e, s)
	if err != nil {
		return err
	}
	n0, t0 := n.Load(), time.Now()
	var bumps []time.Time
	for time.Since(t0) < 3*time.Second {
		time.Sleep(5 * time.Millisecond)
		v, err := logWritten(ctx, e, s)
		if err != nil {
			return err
		}
		if v != prev {
			bumps = append(bumps, time.Now())
			prev = v
		}
	}
	rate := float64(n.Load()-n0) / time.Since(t0).Seconds()
	close(stop)
	if err := <-done; err != nil {
		return err
	}

	// Khoảng cách giữa hai lần nhảy. Tính cả hai đầu mút của cửa sổ đo, để
	// một chế độ không write() lần nào trong 3s vẫn hiện ra là "≥ 3000ms".
	pts := append([]time.Time{t0}, bumps...)
	pts = append(pts, time.Now())
	var gaps []time.Duration
	for i := 1; i < len(pts); i++ {
		gaps = append(gaps, pts[i].Sub(pts[i-1]))
	}
	sort.Slice(gaps, func(i, j int) bool { return gaps[i] < gaps[j] })
	p50, max := gaps[len(gaps)/2], gaps[len(gaps)-1]
	// kill -9 rơi vào một thời điểm ngẫu nhiên. Xác suất rơi vào một khoảng
	// tỉ lệ với độ dài của nó, và trong khoảng dài g thì trung bình mất g/2.
	// Nên kỳ vọng số commit mất = rate × Σg² / (2 Σg).
	var sum, sq float64
	for _, g := range gaps {
		sum += g.Seconds()
		sq += g.Seconds() * g.Seconds()
	}
	fmt.Printf("%-8s%-46s%10.0f%10d%10.1fms%10.1fms%14.0f\n", e.Name, m.name, rate, len(bumps),
		float64(p50.Microseconds())/1000, float64(max.Microseconds())/1000, rate*sq/(2*sum))
	return nil
}
