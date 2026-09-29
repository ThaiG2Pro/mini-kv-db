package main

// Bảng 2 — anomaly nào lọt ở mức isolation nào.
//
// Năm bài dựng y hệt internal/txn/workload.go của phase 6, chạy trên hai kết
// nối thật. Mỗi bước gửi cho transaction của nó rồi chờ tối đa stepWait; quá
// thời gian thì coi là "đang bị chặn" và đi tiếp bước sau — đúng cái người
// dùng thấy khi mở hai cửa sổ psql. Cuối cùng commit cả hai và chờ cho xong.
//
// Ô của bảng không chỉ nói CÓ hay KHÔNG, mà nói DB chặn bằng CÁCH NÀO:
//
//	X   anomaly xảy ra
//	.   không xảy ra, không ai phải chờ, không ai bị huỷ (snapshot tự che)
//	.w  không xảy ra vì một bên phải CHỜ khoá
//	.a  không xảy ra vì một bên bị DB HUỶ (deadlock / serialization failure)
//
// Hai chữ .w và .a chính là hai trường phái của phase 6: bi quan (khoá) và
// lạc quan (phát hiện rồi huỷ).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const stepWait = 400 * time.Millisecond

var isoLevels = []struct {
	name string
	lvl  sql.IsolationLevel
}{
	{"read-uncomm", sql.LevelReadUncommitted},
	{"read-comm", sql.LevelReadCommitted},
	{"repeat-read", sql.LevelRepeatableRead},
	{"serializable", sql.LevelSerializable},
}

// actor là một transaction chạy trên goroutine riêng, nhận từng bước qua kênh.
// Bước có f == nil nghĩa là COMMIT ngay tại đó.
type actor struct {
	steps   chan func(context.Context, *sql.Tx) error
	done    chan error
	pending int // số bước đã gửi mà chưa xong (>0: actor đang bị chặn)
	err     error
	aborted bool
}

type scene struct {
	e       *Engine
	lvl     sql.IsolationLevel
	a, b    *actor
	blocked bool // có bước nào phải chờ quá stepWait
}

func (s *scene) start(ctx context.Context) error {
	for _, x := range []**actor{&s.a, &s.b} {
		tx, err := s.e.DB.BeginTx(ctx, &sql.TxOptions{Isolation: s.lvl})
		if err != nil {
			return err
		}
		lt := `SET LOCAL lock_timeout = '3s'`
		if s.e.Kind == "my" {
			lt = `SET SESSION innodb_lock_wait_timeout = 3`
		}
		if _, err := tx.ExecContext(ctx, lt); err != nil {
			tx.Rollback()
			return err
		}
		ac := &actor{steps: make(chan func(context.Context, *sql.Tx) error, 8), done: make(chan error, 8)}
		*x = ac
		go ac.loop(ctx, tx)
	}
	return nil
}

func (ac *actor) loop(ctx context.Context, tx *sql.Tx) {
	closed := false // đã commit hoặc rollback
	for f := range ac.steps {
		switch {
		case closed:
		case f == nil:
			ac.err, closed = tx.Commit(), true
		default:
			if err := f(ctx, tx); err != nil {
				ac.err, closed = err, true
				tx.Rollback()
			}
		}
		ac.done <- ac.err
	}
	if !closed {
		ac.err = tx.Commit()
	}
	ac.done <- ac.err
}

// do gửi một bước cho actor và chờ tối đa stepWait. Nếu actor đang bị chặn
// ở một bước trước thì chỉ XẾP HÀNG bước mới rồi đi tiếp: y như người gõ sẵn
// lệnh vào cửa sổ psql đang treo rồi quay sang cửa sổ kia. Chờ ở đây là sai,
// vì cái chặn actor này thường chính là actor kia, và actor kia cần được đi.
func (s *scene) do(x *actor, f func(context.Context, *sql.Tx) error) {
	x.steps <- f
	x.pending++
	if x.pending > 1 {
		return
	}
	select {
	case <-x.done:
		x.pending--
	case <-time.After(stepWait):
		s.blocked = true
	}
}

// finish cho cả hai commit rồi chờ tới khi xong hẳn. Phải đóng kênh của CẢ
// HAI trước rồi mới chờ: nếu chờ A xong mới cho B commit, mà A lại đang chờ
// khoá của B, thì chính bộ đo tạo ra deadlock và DB huỷ A sau lock timeout.
func (s *scene) finish() {
	for _, x := range []*actor{s.a, s.b} {
		close(x.steps)
	}
	for _, x := range []*actor{s.a, s.b} {
		for ; x.pending >= 0; x.pending-- {
			<-x.done
		}
		x.aborted = x.err != nil
	}
}

func (s *scene) abortedAny() bool { return s.a.aborted || s.b.aborted }

func readInt(ctx context.Context, tx *sql.Tx, q string, dst *int) error {
	return tx.QueryRowContext(ctx, q).Scan(dst)
}

func execTx(q string) func(context.Context, *sql.Tx) error {
	return func(ctx context.Context, tx *sql.Tx) error { _, err := tx.ExecContext(ctx, q); return err }
}

func reset(ctx context.Context, e *Engine, rows ...[2]int) error {
	var vals []string
	for _, r := range rows {
		vals = append(vals, fmt.Sprintf("(%d,%d)", r[0], r[1]))
	}
	return exec(ctx, e.DB,
		`DROP TABLE IF EXISTS acc`,
		`CREATE TABLE acc (id int PRIMARY KEY, bal int NOT NULL)`,
		`INSERT INTO acc VALUES `+strings.Join(vals, ","),
	)
}

type probe struct {
	name string
	run  func(ctx context.Context, e *Engine, lvl sql.IsolationLevel) (bool, *scene, error)
}

var probes = []probe{
	{"dirty-read", func(ctx context.Context, e *Engine, lvl sql.IsolationLevel) (bool, *scene, error) {
		if err := reset(ctx, e, [2]int{1, 100}); err != nil {
			return false, nil, err
		}
		s := &scene{e: e, lvl: lvl}
		if err := s.start(ctx); err != nil {
			return false, nil, err
		}
		var got int
		s.do(s.a, execTx(`UPDATE acc SET bal = 999 WHERE id = 1`))
		s.do(s.b, func(ctx context.Context, tx *sql.Tx) error {
			return readInt(ctx, tx, `SELECT bal FROM acc WHERE id = 1`, &got)
		})
		s.do(s.a, func(context.Context, *sql.Tx) error { return errRollback })
		s.finish()
		return got == 999, s, nil
	}},
	{"non-repeatable-read", func(ctx context.Context, e *Engine, lvl sql.IsolationLevel) (bool, *scene, error) {
		if err := reset(ctx, e, [2]int{1, 100}); err != nil {
			return false, nil, err
		}
		s := &scene{e: e, lvl: lvl}
		if err := s.start(ctx); err != nil {
			return false, nil, err
		}
		var r1, r2 int
		s.do(s.b, func(ctx context.Context, tx *sql.Tx) error {
			return readInt(ctx, tx, `SELECT bal FROM acc WHERE id = 1`, &r1)
		})
		s.do(s.a, execTx(`UPDATE acc SET bal = 200 WHERE id = 1`))
		s.do(s.a, commitStep)
		s.do(s.b, func(ctx context.Context, tx *sql.Tx) error {
			return readInt(ctx, tx, `SELECT bal FROM acc WHERE id = 1`, &r2)
		})
		s.finish()
		return r2 != 0 && r1 != r2, s, nil
	}},
	{"phantom", func(ctx context.Context, e *Engine, lvl sql.IsolationLevel) (bool, *scene, error) {
		if err := reset(ctx, e, [2]int{1, 100}, [2]int{2, 100}); err != nil {
			return false, nil, err
		}
		s := &scene{e: e, lvl: lvl}
		if err := s.start(ctx); err != nil {
			return false, nil, err
		}
		var c1, c2 int
		q := `SELECT count(*) FROM acc WHERE id BETWEEN 1 AND 100`
		s.do(s.b, func(ctx context.Context, tx *sql.Tx) error { return readInt(ctx, tx, q, &c1) })
		s.do(s.a, execTx(`INSERT INTO acc VALUES (50, 100)`))
		s.do(s.a, commitStep)
		s.do(s.b, func(ctx context.Context, tx *sql.Tx) error { return readInt(ctx, tx, q, &c2) })
		s.finish()
		return c2 != 0 && c1 != c2, s, nil
	}},
	// Tiêu chí giống phase 6: kẻ nào BÁO commit thành công thì phải được tính.
	// Số dư cuối phải bằng 100 - 10 × (số transaction commit được).
	{"lost-update", func(ctx context.Context, e *Engine, lvl sql.IsolationLevel) (bool, *scene, error) {
		if err := reset(ctx, e, [2]int{1, 100}); err != nil {
			return false, nil, err
		}
		s := &scene{e: e, lvl: lvl}
		if err := s.start(ctx); err != nil {
			return false, nil, err
		}
		var ra, rb int
		s.do(s.a, func(ctx context.Context, tx *sql.Tx) error {
			return readInt(ctx, tx, `SELECT bal FROM acc WHERE id = 1`, &ra)
		})
		s.do(s.b, func(ctx context.Context, tx *sql.Tx) error {
			return readInt(ctx, tx, `SELECT bal FROM acc WHERE id = 1`, &rb)
		})
		s.do(s.a, func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE acc SET bal = %d WHERE id = 1`, ra-10))
			return err
		})
		s.do(s.b, func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE acc SET bal = %d WHERE id = 1`, rb-10))
			return err
		})
		s.finish()
		var final int
		if err := e.DB.QueryRowContext(ctx, `SELECT bal FROM acc WHERE id = 1`).Scan(&final); err != nil {
			return false, nil, err
		}
		commits := 0
		for _, x := range []*actor{s.a, s.b} {
			if !x.aborted {
				commits++
			}
		}
		return final != 100-10*commits, s, nil
	}},
	// Bất biến wa + wb >= 0. Mỗi bên đọc cả hai, thấy tổng 100, rút 100 từ
	// tài khoản CỦA RIÊNG NÓ. Hai bên ghi hai khoá khác nhau.
	{"write-skew", func(ctx context.Context, e *Engine, lvl sql.IsolationLevel) (bool, *scene, error) {
		if err := reset(ctx, e, [2]int{1, 50}, [2]int{2, 50}); err != nil {
			return false, nil, err
		}
		s := &scene{e: e, lvl: lvl}
		if err := s.start(ctx); err != nil {
			return false, nil, err
		}
		var sa, sb int
		q := `SELECT sum(bal) FROM acc WHERE id IN (1, 2)`
		s.do(s.a, func(ctx context.Context, tx *sql.Tx) error { return readInt(ctx, tx, q, &sa) })
		s.do(s.b, func(ctx context.Context, tx *sql.Tx) error { return readInt(ctx, tx, q, &sb) })
		s.do(s.a, execTx(`UPDATE acc SET bal = bal - 100 WHERE id = 1`))
		s.do(s.b, execTx(`UPDATE acc SET bal = bal - 100 WHERE id = 2`))
		s.finish()
		var total int
		if err := e.DB.QueryRowContext(ctx, `SELECT sum(bal) FROM acc`).Scan(&total); err != nil {
			return false, nil, err
		}
		return total < 0, s, nil
	}},
}

var errRollback = errors.New("rollback có chủ ý")

// commitStep: commit giữa chừng (actor hiểu f == nil là COMMIT).
var commitStep func(context.Context, *sql.Tx) error

func workAnomaly(es []*Engine) error {
	ctx := context.Background()
	fmt.Println("\n== 2. anomaly nào lọt ở mức nào ==")
	fmt.Println("  X = XẢY RA   . = không xảy ra   .w = chặn bằng CHỜ khoá   .a = chặn bằng HUỶ transaction")
	for _, e := range es {
		if e.Kind == "my" {
			var v sql.NullString
			err := e.DB.QueryRowContext(ctx, `SELECT @@innodb_snapshot_isolation`).Scan(&v)
			if err != nil {
				fmt.Printf("\n-- %s (không có biến innodb_snapshot_isolation)\n", e.Name)
			} else {
				fmt.Printf("\n-- %s (innodb_snapshot_isolation = %s)\n", e.Name, v.String)
			}
		} else {
			fmt.Printf("\n-- %s\n", e.Name)
		}
		fmt.Printf("%-22s", "anomaly")
		for _, l := range isoLevels {
			fmt.Printf("%-16s", l.name)
		}
		fmt.Println()
		var notes []string
		seen := map[string]bool{}
		for _, p := range probes {
			fmt.Printf("%-22s", p.name)
			for _, l := range isoLevels {
				hit, s, err := p.run(ctx, e, l.lvl)
				if err != nil {
					return fmt.Errorf("%s/%s/%s: %w", e.Name, p.name, l.name, err)
				}
				cell := "."
				switch {
				case hit:
					cell = "X"
				case s.abortedAny() && !isDeliberate(s):
					cell = ".a"
					for _, x := range []*actor{s.a, s.b} {
						if x.err != nil && !errors.Is(x.err, errRollback) {
							m := x.err.Error()
							if !seen[m] {
								seen[m] = true
								notes = append(notes, fmt.Sprintf("  %s × %s: %s", p.name, l.name, m))
							}
						}
					}
				case s.blocked:
					cell = ".w"
				}
				fmt.Printf("%-16s", cell)
			}
			fmt.Println()
		}
		if len(notes) > 0 {
			fmt.Println("lỗi DB trả về khi huỷ (.a):")
			fmt.Println(strings.Join(notes, "\n"))
		}
	}
	return nil
}

// isDeliberate: ở bài dirty read, A tự rollback — đó không phải DB huỷ.
func isDeliberate(s *scene) bool {
	return errors.Is(s.a.err, errRollback) && s.b.err == nil
}
