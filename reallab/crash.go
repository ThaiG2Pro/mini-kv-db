package main

// Bảng 6 — kill -9 giữa lúc đang ghi: DB có giữ lời hứa COMMIT không?
//
// Cùng câu hỏi với cmd/crashlab của phase 5, trên DB thật. Một goroutine chèn
// id = 1, 2, 3, … mỗi câu một commit, và ghi lại id LỚN NHẤT mà DB đã báo
// commit thành công. Sau 1-3 giây, `docker kill -s KILL` container. Khởi động
// lại, đợi DB mở cửa, rồi kiểm:
//
//	mất   = số id <= lastAck mà KHÔNG có trong bảng  (phải là 0)
//	thừa  = số id > lastAck mà CÓ trong bảng         (0 hoặc 1: câu đang bay)
//
// Và bài phản chứng, cùng tinh thần `crashlab -nowrite`: chế độ "không chờ log
// xuống đĩa" để log nằm lại trong RAM của tiến trình — kill -9 phải làm mất
// dữ liệu đã báo commit. Một bộ kiểm tra chưa từng báo SAI thì chưa phải bằng
// chứng.
//
// Lưu ý: kill -9 giết TIẾN TRÌNH, không giết page cache của OS. Nó kiểm được
// "log đã được write() trước khi trả lời" chứ không kiểm được fsync — muốn kiểm
// fsync phải rút điện thật (xem phase 5, mục "kill -9 không phải mất điện").

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand/v2"
	osexec "os/exec"
	"sync/atomic"
	"time"
)

var containers = map[string]string{"pg": "rl-pg", "mysql": "rl-mysql", "maria": "rl-maria"}

type crashMode struct {
	name  string
	setup []string // chạy (GLOBAL) trước mỗi vòng
	reset []string // trả lại mặc định
}

func crashModes(e *Engine) []crashMode {
	if e.Kind == "pg" {
		return []crashMode{
			{"mặc định (synchronous_commit=on)", []string{`ALTER SYSTEM RESET synchronous_commit`, `SELECT pg_reload_conf()`}, nil},
			{"synchronous_commit=off", []string{`ALTER SYSTEM SET synchronous_commit = off`, `SELECT pg_reload_conf()`},
				[]string{`ALTER SYSTEM RESET synchronous_commit`, `SELECT pg_reload_conf()`}},
		}
	}
	ms := []crashMode{
		{"mặc định (flush_log_at_trx_commit=1)", []string{`SET GLOBAL innodb_flush_log_at_trx_commit = 1`}, nil},
		{"flush_log_at_trx_commit=2", []string{`SET GLOBAL innodb_flush_log_at_trx_commit = 2`},
			[]string{`SET GLOBAL innodb_flush_log_at_trx_commit = 1`}},
		{"flush_log_at_trx_commit=0", []string{`SET GLOBAL innodb_flush_log_at_trx_commit = 0`},
			[]string{`SET GLOBAL innodb_flush_log_at_trx_commit = 1`}},
	}
	if e.Name == "mysql" {
		// MySQL 8 bật binlog mặc định (MariaDB thì không). Có binlog thì commit
		// là 2PC giữa binlog và redo, và bước flush của binlog group commit
		// write() redo xuống OS bất kể flush_log_at_trx_commit. Tắt cả fsync
		// của binlog để xem tốc độ có nhảy lên mà vẫn không mất gì không.
		ms = append(ms, crashMode{"flush_log_at_trx_commit=0 + sync_binlog=0",
			[]string{`SET GLOBAL innodb_flush_log_at_trx_commit = 0`, `SET GLOBAL sync_binlog = 0`},
			[]string{`SET GLOBAL innodb_flush_log_at_trx_commit = 1`, `SET GLOBAL sync_binlog = 1`}})
		// Nợ P9-6: MySQL 8 có luồng log_writer riêng write() redo liên tục
		// (-work lograte: mỗi ~6ms). Tắt luồng đó thì write() chỉ còn theo
		// nhịp giây, và -work lograte dự báo mất ~1500 commit mỗi lần kill.
		ms = append(ms, crashMode{"=0 + sync_binlog=0 + writer_threads=OFF",
			[]string{`SET GLOBAL innodb_flush_log_at_trx_commit = 0`, `SET GLOBAL sync_binlog = 0`, `SET GLOBAL innodb_log_writer_threads = OFF`},
			[]string{`SET GLOBAL innodb_flush_log_at_trx_commit = 1`, `SET GLOBAL sync_binlog = 1`, `SET GLOBAL innodb_log_writer_threads = ON`}})
	}
	return ms
}

func workCrash(es []*Engine, rounds int) error {
	fmt.Printf("\n== 6. kill -9 giữa lúc ghi, %d vòng mỗi chế độ ==\n", rounds)
	for _, e := range es {
		fmt.Printf("\n-- %s\n", e.Name)
		fmt.Printf("%-38s%8s%12s%10s%10s%12s\n", "chế độ", "vòng", "đã báo OK", "mất", "thừa", "khởi động")
		for _, m := range crashModes(e) {
			var ack, lost, extra int64
			var boot time.Duration
			bad := 0
			for r := 0; r < rounds; r++ {
				a, l, x, b, err := crashRound(e, m)
				if err != nil {
					return fmt.Errorf("%s/%s vòng %d: %w", e.Name, m.name, r, err)
				}
				ack, lost, extra, boot = ack+a, lost+l, extra+x, boot+b
				if l > 0 {
					bad++
				}
			}
			fmt.Printf("%-38s%8s%12d%10d%10d%12s\n", m.name, fmt.Sprintf("%d/%d", rounds-bad, rounds),
				ack, lost, extra, (boot / time.Duration(rounds)).Round(100*time.Millisecond))
			if len(m.reset) > 0 {
				if err := exec_(e, m.reset); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func exec_(e *Engine, stmts []string) error {
	ctx := context.Background()
	for _, s := range stmts {
		if _, err := e.DB.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("%s: %w", firstLine(s), err)
		}
	}
	return nil
}

// crashRound trả (số commit đã báo OK, số mất, số thừa, thời gian khởi động lại).
func crashRound(e *Engine, m crashMode) (int64, int64, int64, time.Duration, error) {
	ctx := context.Background()
	if err := exec_(e, append([]string{`DROP TABLE IF EXISTS cr`, `CREATE TABLE cr (id int PRIMARY KEY)`}, m.setup...)); err != nil {
		return 0, 0, 0, 0, err
	}
	// Một kết nối riêng, không qua pool: sau khi DB chết, pool cũ vô dụng.
	d := dsns[e.Name]
	w, err := sql.Open(d[0], d[1])
	if err != nil {
		return 0, 0, 0, 0, err
	}
	w.SetMaxOpenConns(1)
	var lastAck atomic.Int64
	stop := make(chan struct{})
	go func() {
		for i := int64(1); ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := w.ExecContext(ctx, fmt.Sprintf(`INSERT INTO cr VALUES (%d)`, i)); err != nil {
				return // DB đã chết
			}
			lastAck.Store(i)
		}
	}()
	time.Sleep(time.Duration(1000+rand.IntN(2000)) * time.Millisecond)
	if out, err := osexec.Command("docker", "kill", "-s", "KILL", containers[e.Name]).CombinedOutput(); err != nil {
		return 0, 0, 0, 0, fmt.Errorf("docker kill: %v %s", err, out)
	}
	ack := lastAck.Load() // chốt NGAY sau khi giết: không commit nào được báo sau đó
	close(stop)
	w.Close()

	t := time.Now()
	if out, err := osexec.Command("docker", "start", containers[e.Name]).CombinedOutput(); err != nil {
		return 0, 0, 0, 0, fmt.Errorf("docker start: %v %s", err, out)
	}
	for {
		if time.Since(t) > 120*time.Second {
			return 0, 0, 0, 0, fmt.Errorf("DB không lên lại sau 120s")
		}
		var n int64
		if err := e.DB.QueryRowContext(ctx, `SELECT count(*) FROM cr`).Scan(&n); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	boot := time.Since(t)
	var lost, extra int64
	if err := e.DB.QueryRowContext(ctx, fmt.Sprintf(`SELECT %d - count(*) FROM cr WHERE id <= %d`, ack, ack)).Scan(&lost); err != nil {
		return 0, 0, 0, 0, err
	}
	if err := e.DB.QueryRowContext(ctx, fmt.Sprintf(`SELECT count(*) FROM cr WHERE id > %d`, ack)).Scan(&extra); err != nil {
		return 0, 0, 0, 0, err
	}
	return ack, lost, extra, boot, nil
}
