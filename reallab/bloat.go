package main

// Bảng 4 — một transaction mở lâu làm phình cái gì.
//
// Kịch bản ai vận hành DB cũng từng gặp: một phiên mở transaction (một báo
// cáo chạy lâu, một cửa sổ psql quên COMMIT), trong khi ứng dụng vẫn cập nhật
// bình thường. Mỗi lần UPDATE sinh một version mới, và version cũ KHÔNG dọn
// được chừng nào phiên kia còn có thể cần nó.
//
// Dự đoán viết trước khi chạy, suy từ chỗ mỗi DB để version cũ:
//   - Postgres để version cũ NGAY TRONG HEAP: bảng phình, và MỌI người đọc
//     (kể cả người đọc mới) phải lội qua xác hàng chết.
//   - InnoDB để version cũ trong UNDO: bảng không phình, người đọc mới không
//     sao; chỉ người đọc CŨ phải đi ngược chuỗi undo, và history list dài ra.
//   - minidb (phase 6) để cả chuỗi trong record: ai đọc cũng phải giải mã
//     chuỗi (nợ P6-2) — tức là giống Postgres ở chỗ "mọi người cùng chịu".

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strconv"
	"time"
)

const (
	blRows  = 100_000
	blBatch = 1000 // mỗi vòng = blRows/blBatch transaction nhỏ, như ứng dụng thật
)

var blRounds = []int{0, 1, 2, 5, 10}

func workBloat(es []*Engine) error {
	fmt.Printf("\n== 4. một transaction mở lâu; %d hàng, mỗi vòng UPDATE toàn bảng bằng %d transaction × %d hàng ==\n",
		blRows, blRows/blBatch, blBatch)
	for _, e := range es {
		if err := bloatOne(e); err != nil {
			return fmt.Errorf("%s: %w", e.Name, err)
		}
	}
	return nil
}

func bloatOne(e *Engine) error {
	ctx := context.Background()
	pad := `repeat('x',100)`
	create := `CREATE TABLE bl (id int PRIMARY KEY, v int NOT NULL, pad text NOT NULL)`
	fill := fmt.Sprintf(`INSERT INTO bl SELECT g, 0, %s FROM generate_series(1,%d) g`, pad, blRows)
	if e.Kind == "my" {
		create = `CREATE TABLE bl (id int PRIMARY KEY, v int NOT NULL, pad varchar(100) NOT NULL) ENGINE=InnoDB`
		fill = fmt.Sprintf(`INSERT INTO bl WITH RECURSIVE g(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM g WHERE n < %d)
			SELECT n, 0, REPEAT('x',100) FROM g`, blRows)
	}
	setup := []string{`DROP TABLE IF EXISTS bl`, create}
	if e.Kind == "pg" {
		setup = append(setup, `CREATE EXTENSION IF NOT EXISTS pgstattuple`)
	} else if e.Name == "mysql" {
		setup = append(setup, `SET SESSION cte_max_recursion_depth = 1000000`)
	} else {
		setup = append(setup, `SET SESSION max_recursive_iterations = 1000000`)
	}
	// SET SESSION phải cùng kết nối với INSERT.
	c, err := e.DB.Conn(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := exec(ctx, c, append(setup, fill)...); err != nil {
		return err
	}
	if e.Kind == "pg" {
		if err := exec(ctx, c, `VACUUM ANALYZE bl`); err != nil {
			return err
		}
	}

	// Phiên "quên COMMIT": mở transaction, đọc một lần để chốt snapshot.
	old, err := e.DB.Conn(ctx)
	if err != nil {
		return err
	}
	defer old.Close()
	begin := `BEGIN ISOLATION LEVEL REPEATABLE READ`
	if e.Kind == "my" {
		begin = `START TRANSACTION WITH CONSISTENT SNAPSHOT`
	}
	if err := exec(ctx, old, begin); err != nil {
		return err
	}
	var x int64
	if err := old.QueryRowContext(ctx, `SELECT sum(v) FROM bl`).Scan(&x); err != nil {
		return err
	}

	fmt.Printf("\n-- %s\n", e.Name)
	fmt.Printf("%-7s%10s%14s%12s%12s%14s%10s\n", "vòng", "bảng MB", "version chết", "đọc MỚI ms", "đọc CŨ ms", "history list", "undo MB")
	done := 0
	for _, r := range blRounds {
		for ; done < r; done++ {
			for lo := 1; lo <= blRows; lo += blBatch {
				q := fmt.Sprintf(`UPDATE bl SET v = v + 1 WHERE id BETWEEN %d AND %d`, lo, lo+blBatch-1)
				if err := exec(ctx, c, q); err != nil {
					return err
				}
			}
		}
		if err := bloatRow(ctx, e, c, old, r); err != nil {
			return err
		}
	}

	// Đóng phiên cũ rồi xem DB dọn được gì, và mất bao lâu.
	if err := exec(ctx, old, `COMMIT`); err != nil {
		return err
	}
	t := time.Now()
	if e.Kind == "pg" {
		if err := exec(ctx, c, `VACUUM bl`); err != nil {
			return err
		}
		fmt.Printf("sau COMMIT phiên cũ + VACUUM (%s):\n", time.Since(t).Round(time.Millisecond))
	} else {
		// Purge chạy nền; chờ tới khi history list về gần 0 (tối đa 120s).
		// History list đếm TRANSACTION đã commit mà undo chưa dọn, không
		// đếm hàng — nên mới phải cập nhật bằng nhiều transaction nhỏ.
		for time.Since(t) < 120*time.Second {
			h, err := historyLen(ctx, e, c)
			if err != nil {
				return err
			}
			if h < 50 {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		fmt.Printf("sau COMMIT phiên cũ, purge nền chạy xong trong %s:\n", time.Since(t).Round(100*time.Millisecond))
	}
	if err := bloatRow(ctx, e, c, nil, -1); err != nil {
		return err
	}
	if e.Kind == "pg" {
		// VACUUM chỉ đánh dấu chỗ trống để tái dùng, KHÔNG trả file về cho OS.
		// Muốn bảng co lại phải viết lại cả bảng — và giữ khoá độc quyền.
		t = time.Now()
		if err := exec(ctx, c, `VACUUM FULL bl`); err != nil {
			return err
		}
		fmt.Printf("sau VACUUM FULL (%s, khoá ACCESS EXCLUSIVE cả bảng trong lúc chạy):\n", time.Since(t).Round(time.Millisecond))
		return bloatRow(ctx, e, c, nil, -2)
	}
	return nil
}

func bloatRow(ctx context.Context, e *Engine, c *sql.Conn, old *sql.Conn, r int) error {
	var mb float64
	var dead int64 = -1
	if e.Kind == "pg" {
		if err := c.QueryRowContext(ctx,
			`SELECT table_len/1e6, dead_tuple_count FROM pgstattuple('bl')`).Scan(&mb, &dead); err != nil {
			return err
		}
	} else {
		if err := exec(ctx, c, `ANALYZE TABLE bl`); err != nil {
			return err
		}
		if err := c.QueryRowContext(ctx, `SELECT data_length/1e6 FROM information_schema.tables
			WHERE table_schema = 'lab' AND table_name = 'bl'`).Scan(&mb); err != nil {
			return err
		}
	}
	var x int64
	newMs, err := median(5, func() error { return c.QueryRowContext(ctx, `SELECT sum(v) FROM bl`).Scan(&x) })
	if err != nil {
		return err
	}
	oldCol := "—"
	if old != nil {
		d, err := median(5, func() error { return old.QueryRowContext(ctx, `SELECT sum(v) FROM bl`).Scan(&x) })
		if err != nil {
			return err
		}
		oldCol = ms(d)
	}
	hl := "—"
	if e.Kind == "my" {
		h, err := historyLen(ctx, e, c)
		if err != nil {
			return err
		}
		hl = strconv.FormatInt(h, 10)
	}
	deadCol := "—"
	if dead >= 0 {
		deadCol = strconv.FormatInt(dead, 10)
	}
	label := strconv.Itoa(r)
	if r < 0 {
		label = "sau"
	}
	undo := "—"
	if e.Kind == "my" {
		u, err := undoMB(ctx, e, c)
		if err != nil {
			return err
		}
		undo = fmt.Sprintf("%.1f", u)
	}
	fmt.Printf("%-7s%10.1f%14s%12s%12s%14s%10s\n", label, mb, deadCol, ms(newMs), oldCol, hl, undo)
	return nil
}

var reHistory = regexp.MustCompile(`History list length (\d+)`)

// historyLen đọc "History list length" từ SHOW ENGINE INNODB STATUS — có ở cả
// MySQL lẫn MariaDB, không phụ thuộc bộ đếm nào có bật hay không.
func historyLen(ctx context.Context, e *Engine, c *sql.Conn) (int64, error) {
	var typ, name, status string
	if err := c.QueryRowContext(ctx, `SHOW ENGINE INNODB STATUS`).Scan(&typ, &name, &status); err != nil {
		return 0, err
	}
	m := reHistory.FindStringSubmatch(status)
	if m == nil {
		return 0, fmt.Errorf("không thấy History list length trong INNODB STATUS")
	}
	return strconv.ParseInt(m[1], 10, 64)
}

// undoMB là tổng kích thước các file undo tablespace. File undo lớn lên khi
// phải giữ version cũ, và chỉ co lại khi DB cắt (truncate) nó.
func undoMB(ctx context.Context, e *Engine, c *sql.Conn) (float64, error) {
	q := `SELECT COALESCE(SUM(TOTAL_EXTENTS*EXTENT_SIZE), 0)/1e6 FROM information_schema.FILES WHERE FILE_TYPE = 'UNDO LOG'`
	if e.Name == "maria" {
		q = `SELECT COALESCE(SUM(file_size), 0)/1e6 FROM information_schema.INNODB_SYS_TABLESPACES WHERE name LIKE 'innodb_undo%'`
	}
	var v float64
	err := c.QueryRowContext(ctx, q).Scan(&v)
	return v, err
}
