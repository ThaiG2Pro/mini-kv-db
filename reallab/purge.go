package main

// Bảng 11 — nợ P9-4: vì sao purge của MariaDB nhanh hơn MySQL 40 lần?
//
// Bảng 4 thấy: sau khi phiên cũ commit, history list của MySQL mất 8s mới về 0,
// MariaDB 200ms, cùng 1000 transaction × 1000 hàng. Cấu hình mặc định khác nhau
// ngay từ đầu (SHOW VARIABLES, 2026-10-01):
//
//	                         MySQL 8.4.11   MariaDB 11.8.9
//	innodb_purge_threads          1              4
//	innodb_purge_batch_size     300            127
//
// Ba giả thuyết, mỗi cái một dự báo phân biệt được, viết TRƯỚC khi đo:
//
//	H1 số luồng: MariaDB hạ về 1 luồng thì phải chậm đi cỡ 4 lần.
//	   Nhưng 4 lần không giải thích nổi 40 lần, nên H1 cùng lắm là một phần.
//	H2 cỡ lô / nhịp ngủ: MySQL dọn theo lô rồi nghỉ. Tăng batch_size thì
//	   MySQL phải nhanh lên rõ, và đường history list đi xuống theo bậc.
//	H3 bộ đếm nói dối (bài học bảng 7): "history list" của hai DB đếm khác
//	   nhau. Đối chiếu với bộ đếm thứ hai, số undo page đã purge
//	   (purge_undo_log_pages): nếu MariaDB "xong" mà số page gần 0 thì nó
//	   chưa dọn gì cả, chỉ là con số khác nghĩa.
//
// Mỗi chế độ vặn đúng một núm. Lấy mẫu mỗi 20ms từ INNODB_METRICS (rẻ hơn
// SHOW ENGINE INNODB STATUS, có ở cả hai DB, chỉ khác tên cột trạng thái).

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

const (
	pgRounds = 5 // 5 vòng × 100 transaction × 1000 hàng = 500 transaction trong history list
	pgSample = 20 * time.Millisecond
	pgLimit  = 120 * time.Second
)

type purgeMode struct {
	name         string
	setup, reset []string
}

func purgeModes(e *Engine) []purgeMode {
	if e.Name == "mysql" {
		// innodb_purge_threads của MySQL không đổi được lúc chạy (cần khởi
		// động lại), nên ở MySQL chỉ vặn được cỡ lô.
		return []purgeMode{
			{"mặc định (1 luồng, lô 300)", nil, nil},
			{"lô 5000", []string{`SET GLOBAL innodb_purge_batch_size = 5000`},
				[]string{`SET GLOBAL innodb_purge_batch_size = 300`}},
			// H4, thêm SAU lượt đo đầu: history list của MySQL đứng yên 48s rồi
			// rơi một lần. Nghi: nó chỉ giảm khi history được CẮT, mà việc cắt
			// chạy mỗi rseg_truncate_frequency (128) lô purge.
			{"cắt history mỗi lô (truncate_frequency=1)", []string{`SET GLOBAL innodb_purge_rseg_truncate_frequency = 1`},
				[]string{`SET GLOBAL innodb_purge_rseg_truncate_frequency = 128`}},
		}
	}
	return []purgeMode{
		{"mặc định (4 luồng, lô 127)", nil, nil},
		{"1 luồng", []string{`SET GLOBAL innodb_purge_threads = 1`},
			[]string{`SET GLOBAL innodb_purge_threads = 4`}},
		{"1 luồng, lô 300 (như MySQL)", []string{`SET GLOBAL innodb_purge_threads = 1`, `SET GLOBAL innodb_purge_batch_size = 300`},
			[]string{`SET GLOBAL innodb_purge_threads = 4`, `SET GLOBAL innodb_purge_batch_size = 127`}},
		{"truncate_frequency=1", []string{`SET GLOBAL innodb_purge_rseg_truncate_frequency = 1`},
			[]string{`SET GLOBAL innodb_purge_rseg_truncate_frequency = 128`}},
	}
}

func workPurge(es []*Engine) error {
	fmt.Printf("\n== 11. purge sau khi phiên cũ đóng: %d transaction × %d hàng trong history list ==\n",
		pgRounds*blRows/blBatch, blBatch)
	fmt.Printf("%-8s%-44s%6s%10s%10s%10s%12s%11s%9s\n", "db", "chế độ", "đầu", "t 50%", "t 90%", "t xong", "page ngừng", "undo page", "lần gọi")
	for _, e := range es {
		if e.Kind != "my" {
			continue
		}
		for _, m := range purgeModes(e) {
			if err := purgeOne(e, m); err != nil {
				return fmt.Errorf("%s/%s: %w", e.Name, m.name, err)
			}
		}
	}
	return nil
}

var purgeMetrics = []string{"trx_rseg_history_len", "purge_undo_log_pages", "purge_invoked"}

func readPurge(ctx context.Context, c *sql.Conn) (map[string]int64, error) {
	rows, err := c.QueryContext(ctx, `SELECT NAME, COUNT FROM information_schema.INNODB_METRICS WHERE NAME IN ('`+
		strings.Join(purgeMetrics, "','")+`')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]int64{}
	for rows.Next() {
		var n string
		var v int64
		if err := rows.Scan(&n, &v); err != nil {
			return nil, err
		}
		m[n] = v
	}
	return m, rows.Err()
}

func purgeOne(e *Engine, m purgeMode) error {
	ctx := context.Background()
	c, err := e.DB.Conn(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	// module_purge mặc định tắt ở cả hai DB; bật lên thì các bộ đếm mới chạy.
	if err := exec(ctx, c, append([]string{`SET GLOBAL innodb_monitor_enable = 'module_purge'`}, m.setup...)...); err != nil {
		return err
	}
	defer exec(ctx, c, m.reset...)

	// Cùng bảng với bảng 4.
	recur := `SET SESSION max_recursive_iterations = 1000000`
	if e.Name == "mysql" {
		recur = `SET SESSION cte_max_recursion_depth = 1000000`
	}
	if err := exec(ctx, c, `DROP TABLE IF EXISTS bl`,
		`CREATE TABLE bl (id int PRIMARY KEY, v int NOT NULL, pad varchar(100) NOT NULL) ENGINE=InnoDB`, recur,
		fmt.Sprintf(`INSERT INTO bl WITH RECURSIVE g(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM g WHERE n < %d)
			SELECT n, 0, REPEAT('x',100) FROM g`, blRows)); err != nil {
		return err
	}
	// Chờ purge của lần nạp (và của chế độ trước) dọn xong, để mọi chế độ bắt
	// đầu từ cùng một chỗ.
	if err := waitHistory(ctx, c, 50, pgLimit); err != nil {
		return err
	}

	old, err := e.DB.Conn(ctx)
	if err != nil {
		return err
	}
	defer old.Close()
	var x int64
	if err := exec(ctx, old, `START TRANSACTION WITH CONSISTENT SNAPSHOT`); err != nil {
		return err
	}
	if err := old.QueryRowContext(ctx, `SELECT sum(v) FROM bl`).Scan(&x); err != nil {
		return err
	}
	for r := 0; r < pgRounds; r++ {
		for lo := 1; lo <= blRows; lo += blBatch {
			if err := exec(ctx, c, fmt.Sprintf(`UPDATE bl SET v = v + 1 WHERE id BETWEEN %d AND %d`, lo, lo+blBatch-1)); err != nil {
				return err
			}
		}
	}
	m0, err := readPurge(ctx, c)
	if err != nil {
		return err
	}
	h0 := m0["trx_rseg_history_len"]

	// Đóng phiên cũ, rồi lấy mẫu cho tới khi history list về mức nền.
	if err := exec(ctx, old, `COMMIT`); err != nil {
		return err
	}
	t0 := time.Now()
	var t50, t90, tDone, tPages time.Duration
	var last map[string]int64
	pages := m0["purge_undo_log_pages"]
	for time.Since(t0) < pgLimit {
		time.Sleep(pgSample)
		cur, err := readPurge(ctx, c)
		if err != nil {
			return err
		}
		last = cur
		h, d := cur["trx_rseg_history_len"], time.Since(t0)
		// Lúc việc dọn THẬT dừng: lần cuối số undo page đã purge còn tăng.
		if p := cur["purge_undo_log_pages"]; p != pages {
			pages, tPages = p, d
		}
		if t50 == 0 && h <= h0/2 {
			t50 = d
		}
		if t90 == 0 && h <= h0/10 {
			t90 = d
		}
		if h < 50 {
			tDone = d
			break
		}
	}
	if tDone == 0 {
		return fmt.Errorf("history list chưa về dưới 50 sau %s (còn %d)", pgLimit, last["trx_rseg_history_len"])
	}
	fmt.Printf("%-8s%-44s%6d%10s%10s%10s%12s%11d%9d\n", e.Name, m.name, h0,
		ms(t50), ms(t90), ms(tDone), ms(tPages),
		last["purge_undo_log_pages"]-m0["purge_undo_log_pages"], last["purge_invoked"]-m0["purge_invoked"])
	return nil
}

func waitHistory(ctx context.Context, c *sql.Conn, below int64, limit time.Duration) error {
	t := time.Now()
	for time.Since(t) < limit {
		m, err := readPurge(ctx, c)
		if err != nil {
			return err
		}
		if m["trx_rseg_history_len"] < below {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("history list không về dưới %d trước khi đo", below)
}
