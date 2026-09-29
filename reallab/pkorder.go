package main

// Bảng 3 — PK tăng dần vs ngẫu nhiên.
//
// Phase 4 đo trên minidb: chèn ngẫu nhiên làm bẩn 33x số page so với chèn
// tăng dần. Ở đây hỏi câu mà dev thật sự gặp: PK là UUIDv4 (ngẫu nhiên) hay
// một khoá 16 byte tăng dần theo thời gian (hình dạng của UUIDv7). Cùng độ
// dài khoá, chỉ khác THỨ TỰ.
//
// Dự đoán viết trước khi chạy: InnoDB đau hơn Postgres nhiều, vì ở InnoDB
// cả bảng LÀ cây PK (clustered), còn ở Postgres heap không theo thứ tự PK —
// chỉ riêng index PK bị chèn lung tung.
//
// Tổng dữ liệu (~2 triệu × ~150B) cố ý lớn hơn buffer pool 256MB, để thấy
// cả giai đoạn "còn vừa RAM" lẫn giai đoạn "tràn RAM".

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

const (
	pkRows  = 2_000_000
	pkBatch = 1000
	pkParts = 4
)

type pkStat struct {
	partRate          [pkParts]float64 // hàng/giây của từng phần tư
	total             time.Duration
	tableMB, indexMB  float64
	logMB             float64 // WAL (pg) / redo (InnoDB) sinh ra
	readsMB, writesMB float64 // page đọc từ đĩa / ghi xuống đĩa
}

func workPKOrder(es []*Engine) error {
	fmt.Printf("\n== 3. PK tăng dần vs ngẫu nhiên (%d hàng, khoá 16 byte, lô %d hàng/commit) ==\n", pkRows, pkBatch)
	for _, e := range es {
		fmt.Printf("\n-- %s\n", e.Name)
		fmt.Printf("%-10s%10s%10s%10s%10s%9s%9s%9s%9s%9s%9s\n", "khoá", "¼ h/s", "½ h/s", "¾ h/s", "4/4 h/s",
			"tổng s", "bảng MB", "idx MB", "log MB", "đọc MB", "ghi MB")
		var st [2]pkStat
		for i, mode := range []string{"tăng dần", "ngẫu nhiên"} {
			s, err := pkOne(e, i == 1)
			if err != nil {
				return fmt.Errorf("%s/%s: %w", e.Name, mode, err)
			}
			st[i] = s
			fmt.Printf("%-10s", mode)
			for _, r := range s.partRate {
				fmt.Printf("%10.0f", r)
			}
			fmt.Printf("%9.1f%9.0f%9.0f%9.0f%9.0f%9.0f\n", s.total.Seconds(), s.tableMB, s.indexMB, s.logMB, s.readsMB, s.writesMB)
		}
		r := func(a, b float64) string {
			if a < 1 { // mẫu số ~0: tỉ số vô nghĩa, in "0→N" cho thật
				return fmt.Sprintf("0→%.0f", b)
			}
			return fmt.Sprintf("%.2fx", b/a)
		}
		fmt.Printf("%-10s%10s%10s%10s%10s%9s%9s%9s%9s%9s%9s\n", "ngẫu/tăng",
			r(st[1].partRate[0], st[0].partRate[0]), r(st[1].partRate[1], st[0].partRate[1]),
			r(st[1].partRate[2], st[0].partRate[2]), r(st[1].partRate[3], st[0].partRate[3]),
			r(st[0].total.Seconds(), st[1].total.Seconds()), r(st[0].tableMB, st[1].tableMB),
			r(st[0].indexMB, st[1].indexMB), r(st[0].logMB, st[1].logMB),
			r(st[0].readsMB, st[1].readsMB), r(st[0].writesMB, st[1].writesMB))
		fmt.Println("  (cột h/s: tỉ số là tăng/ngẫu — bao nhiêu lần NHANH hơn; các cột còn lại: ngẫu/tăng)")
	}
	return nil
}

func pkKey(i int, random bool) string {
	var b [16]byte
	if random {
		rand.Read(b[:])
		b[6] = b[6]&0x0f | 0x40 // version 4
		b[8] = b[8]&0x3f | 0x80
	} else {
		// Hình dạng UUIDv7: 48 bit thời gian ở đầu, tăng dần. Dùng bộ đếm
		// thay cho đồng hồ để hai lần chạy cho cùng một dãy khoá.
		binary.BigEndian.PutUint64(b[:8], uint64(i)<<16)
		rand.Read(b[8:])
		b[6] = b[6]&0x0f | 0x70
	}
	return hex.EncodeToString(b[:])
}

func pkOne(e *Engine, random bool) (pkStat, error) {
	ctx := context.Background()
	var st pkStat
	create := `CREATE TABLE pk (id uuid PRIMARY KEY, pad text NOT NULL)`
	if e.Kind == "my" {
		create = `CREATE TABLE pk (id binary(16) PRIMARY KEY, pad varchar(100) NOT NULL) ENGINE=InnoDB`
	}
	if err := exec(ctx, e.DB, `DROP TABLE IF EXISTS pk`, create); err != nil {
		return st, err
	}
	if e.Kind == "pg" {
		// Bắt đầu từ một checkpoint mới để hai chế độ cùng xuất phát điểm
		// với full page writes.
		if err := exec(ctx, e.DB, `CHECKPOINT`); err != nil {
			return st, err
		}
	}
	log0, rd0, wr0, err := ioCounters(ctx, e)
	if err != nil {
		return st, err
	}
	pad := strings.Repeat("x", 100)
	t0 := time.Now()
	part := pkRows / pkParts
	tp := t0
	var sb strings.Builder
	for i := 0; i < pkRows; i += pkBatch {
		sb.Reset()
		sb.WriteString("INSERT INTO pk VALUES ")
		for j := 0; j < pkBatch; j++ {
			if j > 0 {
				sb.WriteByte(',')
			}
			k := pkKey(i+j, random)
			if e.Kind == "pg" {
				fmt.Fprintf(&sb, "('%s','%s')", k, pad)
			} else {
				fmt.Fprintf(&sb, "(X'%s','%s')", k, pad)
			}
		}
		if _, err := e.DB.ExecContext(ctx, sb.String()); err != nil {
			return st, err
		}
		if n := i + pkBatch; n%part == 0 {
			st.partRate[n/part-1] = float64(part) / time.Since(tp).Seconds()
			tp = time.Now()
		}
	}
	st.total = time.Since(t0)
	log1, rd1, wr1, err := ioCounters(ctx, e)
	if err != nil {
		return st, err
	}
	st.logMB, st.readsMB, st.writesMB = (log1-log0)/1e6, (rd1-rd0)/1e6, (wr1-wr0)/1e6
	st.tableMB, st.indexMB, err = pkSizes(ctx, e)
	return st, err
}

// ioCounters trả (byte log đã sinh, byte đọc từ đĩa, byte ghi xuống đĩa) tính
// từ lúc DB khởi động. Chỉ hiệu hai lần gọi là có nghĩa.
func ioCounters(ctx context.Context, e *Engine) (logB, readB, writeB float64, err error) {
	if e.Kind == "pg" {
		err = e.DB.QueryRowContext(ctx, `
			SELECT pg_wal_lsn_diff(pg_current_wal_lsn(), '0/0'),
			       (SELECT sum(reads) * 8192 FROM pg_stat_io WHERE object = 'relation'),
			       (SELECT sum(writes) * 8192 FROM pg_stat_io WHERE object = 'relation')`).Scan(&logB, &readB, &writeB)
		return
	}
	get := func(name string) (float64, error) {
		var n string
		var v float64
		err := e.DB.QueryRowContext(ctx, `SHOW GLOBAL STATUS LIKE '`+name+`'`).Scan(&n, &v)
		return v, err
	}
	lsn := "Innodb_redo_log_current_lsn"
	if e.Name == "maria" {
		lsn = "Innodb_lsn_current"
	}
	if logB, err = get(lsn); err != nil {
		return
	}
	var pages float64
	if pages, err = get("Innodb_buffer_pool_reads"); err != nil {
		return
	}
	readB = pages * 16384
	// Chỉ page dữ liệu, không tính redo/doublewrite — cho cân với cột
	// "writes" của pg_stat_io (cũng chỉ tính relation, WAL nằm ở cột log).
	if pages, err = get("Innodb_pages_written"); err != nil {
		return
	}
	writeB = pages * 16384
	return
}

func pkSizes(ctx context.Context, e *Engine) (table, index float64, err error) {
	if e.Kind == "pg" {
		err = e.DB.QueryRowContext(ctx,
			`SELECT pg_table_size('pk')/1e6, pg_indexes_size('pk')/1e6`).Scan(&table, &index)
		return
	}
	// InnoDB: bảng CHÍNH LÀ cây PK, nên data_length là cả hai. Số của
	// information_schema chỉ cập nhật sau ANALYZE.
	if err = exec(ctx, e.DB, `ANALYZE TABLE pk`); err != nil {
		return
	}
	err = e.DB.QueryRowContext(ctx, `SELECT data_length/1e6, index_length/1e6
		FROM information_schema.tables WHERE table_schema = 'lab' AND table_name = 'pk'`).Scan(&table, &index)
	return
}
