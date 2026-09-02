// txnlab là bài lab của phase 6: in ra bảng anomaly × mức isolation, bảng
// chuyển tiền đồng thời, và số đo phình version của MVCC.
//
// Ba bảng, ba câu hỏi:
//
//	-work anomaly  : mức isolation nào chặn anomaly nào — và mức nào KHÔNG
//	-work transfer : N goroutine chuyển tiền, tổng số dư có đổi không
//	-work bloat    : MVCC phình bao nhiêu, và vacuum thu lại được bao nhiêu
//
// Mọi con số ở đây sinh ra từ đúng cùng đoạn code mà bài test dùng
// (internal/txn/workload.go), nên bảng in ra và bảng xanh là một.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"minidb/internal/db"
	"minidb/internal/txn"
)

func main() {
	var (
		dir      = flag.String("dir", "data/txn", "thư mục làm việc")
		work     = flag.String("work", "all", "anomaly | transfer | bloat | all")
		levelStr = flag.String("level", "", "chỉ chạy một mức isolation (mặc định: tất cả)")
		workers  = flag.Int("workers", 6, "số goroutine chuyển tiền")
		ops      = flag.Int("ops", 200, "số lượt chuyển mỗi goroutine")
		accounts = flag.Int("accounts", 8, "số tài khoản")
		initial  = flag.Int("initial", 10000, "số dư ban đầu mỗi tài khoản")
		amount   = flag.Int("amount", 7, "số tiền mỗi lượt chuyển")
		frames   = flag.Int("frames", 256, "số frame của buffer pool")
		seed     = flag.Int64("seed", 1, "seed")
		bloatN   = flag.Int("bloat-n", 200, "số khóa cho bài đo phình")
		bloatRep = flag.Int("bloat-rep", 20, "số lần ghi lại mỗi khóa")
	)
	flag.Parse()

	levels := txn.AllLevels
	if *levelStr != "" {
		l, err := txn.ParseLevel(*levelStr)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		levels = []txn.Level{l}
	}

	if err := os.MkdirAll(*dir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	run := func(name string, fn func() error) {
		if *work != "all" && *work != name {
			return
		}
		if err := fn(); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", name, err)
			os.Exit(1)
		}
	}

	open := func(tag string) (*txn.Store, error) {
		path := filepath.Join(*dir, tag+".db")
		os.Remove(path)
		os.Remove(path + ".wal")
		s, err := txn.Open(path, db.Options{Frames: *frames})
		if err != nil {
			return nil, err
		}
		s.Locks().Timeout = 2 * txn.ProbeWait
		return s, nil
	}

	run("anomaly", func() error { return anomalyTable(open, levels) })
	run("transfer", func() error {
		return transferTable(open, levels, *workers, *ops, *accounts, *initial, *amount, *seed)
	})
	run("bloat", func() error { return bloatTable(open, *bloatN, *bloatRep) })
}

// ---------- bảng 1: anomaly × mức isolation ----------

func anomalyTable(open func(string) (*txn.Store, error), levels []txn.Level) error {
	fmt.Println("== anomaly nào xảy ra ở mức nào ==")
	fmt.Println("  X = anomaly XẢY RA (mức này không chặn)   . = bị chặn")
	fmt.Println()

	hdr := fmt.Sprintf("%-22s", "anomaly")
	for _, l := range levels {
		hdr += fmt.Sprintf("%-18s", short(l))
	}
	fmt.Println(hdr)
	fmt.Println(strings.Repeat("-", len(hdr)))

	bad := 0
	for _, a := range txn.Anomalies {
		row := fmt.Sprintf("%-22s", a.Name)
		for i, l := range levels {
			s, err := open("anomaly")
			if err != nil {
				return err
			}
			got, err := a.Run(s, l)
			s.Close()
			if err != nil {
				return fmt.Errorf("%s / %s: %w", a.Name, l, err)
			}
			mark := "."
			if got {
				mark = "X"
			}
			// Chỉ số của l trong AllLevels, không phải trong `levels` (có thể
			// đã bị -level lọc bớt).
			want := a.Want[indexOf(l)]
			if got != want {
				mark += " ← KHÁC DỰ ĐOÁN"
				bad++
			}
			_ = i
			row += fmt.Sprintf("%-18s", mark)
		}
		fmt.Println(row)
	}
	fmt.Println()
	for _, a := range txn.Anomalies {
		fmt.Printf("  %-22s %s\n", a.Name, a.Note)
	}
	fmt.Println()
	fmt.Println("Đọc bảng: hàng write-skew là hàng đáng chú ý nhất — snapshot isolation")
	fmt.Println("(repeatable-read) KHÔNG chặn nó, vì hai transaction ghi hai khóa khác")
	fmt.Println("nhau nên chẳng có xung đột ghi-ghi nào để phát hiện. Chặn nó cần biết về")
	fmt.Println("chỗ ĐỌC, tức lock (S2PL) hoặc SSI.")
	if bad > 0 {
		return fmt.Errorf("%d ô khác dự đoán", bad)
	}
	fmt.Println()
	return nil
}

func indexOf(l txn.Level) int {
	for i, x := range txn.AllLevels {
		if x == l {
			return i
		}
	}
	return 0
}

func short(l txn.Level) string {
	switch l {
	case txn.ReadUncommitted:
		return "read-uncomm"
	case txn.ReadCommitted:
		return "read-comm"
	case txn.RepeatableRead:
		return "repeat-read"
	}
	return "serializable"
}

// ---------- bảng 2: chuyển tiền đồng thời ----------

func transferTable(open func(string) (*txn.Store, error), levels []txn.Level,
	workers, ops, accounts, initial, amount int, seed int64) error {
	fmt.Printf("== chuyển tiền: %d goroutine × %d lượt trên %d tài khoản, mỗi lượt %d ==\n",
		workers, ops, accounts, amount)
	fmt.Printf("   bất biến: tổng số dư luôn = %d\n\n", accounts*initial)

	for _, l := range levels {
		s, err := open("transfer")
		if err != nil {
			return err
		}
		res, err := txn.RunTransfers(s, l, workers, ops, accounts, initial, amount, seed)
		if err != nil {
			s.Close()
			return err
		}
		st := s.Stats()
		fmt.Println(res.String())
		fmt.Printf("%-18s   %6.0f txn/s   xung đột %d   deadlock %d   thử lại %d   version ghi %d, dọn %d\n\n",
			"", float64(res.Committed)/res.Elapsed.Seconds(),
			st.Conflicts, st.Deadlocks, st.Retries, st.VersionsWritten, st.VersionsPruned)
		s.Close()
	}
	fmt.Println("Đọc bảng: hai mức thấp làm LỆCH tổng số dư — tiền bốc hơi hoặc sinh ra từ")
	fmt.Println("không khí. Đó không phải bug của bản này: mẫu đọc-rồi-ghi với số tiền do")
	fmt.Println("ứng dụng tự tính cũng mất update trên Postgres ở read-committed.")
	fmt.Println()
	return nil
}

// ---------- bảng 3: phình version và vacuum ----------

func bloatTable(open func(string) (*txn.Store, error), n, rep int) error {
	fmt.Printf("== phình version: %d khóa, mỗi khóa ghi lại %d lần ==\n\n", n, rep)

	s, err := open("bloat")
	if err != nil {
		return err
	}
	defer s.Close()

	// Một reader mở suốt: chính là `idle_in_transaction` của Postgres, và là
	// thứ duy nhất làm bộ dọn cơ hội thành vô ích.
	reader, err := s.Begin(txn.RepeatableRead)
	if err != nil {
		return err
	}
	for i := 0; i < n; i++ {
		if err := s.Update(txn.RepeatableRead, func(tx *txn.Txn) error {
			return tx.Put(key(i), []byte("v0"))
		}); err != nil {
			return err
		}
	}
	if _, _, err := reader.Get(key(0)); err != nil {
		return err
	}

	start := time.Now()
	for r := 1; r < rep; r++ {
		for i := 0; i < n; i++ {
			if err := s.Update(txn.RepeatableRead, func(tx *txn.Txn) error {
				return tx.Put(key(i), []byte("v"+strconv.Itoa(r)))
			}); err != nil {
				return fmt.Errorf("ghi lần %d khóa %d: %w", r, i, err)
			}
		}
	}
	writeTime := time.Since(start)

	before, err := s.ChainStats()
	if err != nil {
		return err
	}
	fmt.Printf("reader cũ CÒN mở : %d khóa, %d version, chuỗi dài nhất %d, %d byte value\n",
		before.Keys, before.Versions, before.MaxChain, before.Bytes)

	vs, err := s.Vacuum()
	if err != nil {
		return err
	}
	fmt.Printf("vacuum khi reader còn mở: dọn %d version (horizon %d) — %s\n",
		vs.VersionsPruned(), vs.Horizon, verdict(vs.VersionsPruned() == 0))

	reader.Abort()
	vs2, err := s.Vacuum()
	if err != nil {
		return err
	}
	after, err := s.ChainStats()
	if err != nil {
		return err
	}
	fmt.Printf("đóng reader rồi vacuum : dọn %d version, %d khóa được thu hồi\n",
		vs2.VersionsPruned(), vs2.KeysReclaimed)
	fmt.Printf("sau vacuum       : %d khóa, %d version, chuỗi dài nhất %d, %d byte value\n",
		after.Keys, after.Versions, after.MaxChain, after.Bytes)
	if after.Bytes > 0 {
		fmt.Printf("\ntỉ số phình = %.2fx  (byte value trước / sau)\n",
			float64(before.Bytes)/float64(after.Bytes))
	}
	fmt.Printf("thời gian ghi %d lượt: %v\n", n*(rep-1), writeTime)
	fmt.Println()
	fmt.Println("Đọc bảng: version cũ chỉ dọn được khi KHÔNG còn ai có thể nhìn thấy chúng.")
	fmt.Println("Một transaction chỉ đọc mở lâu ghim horizon lại và làm mọi bộ dọn vô ích —")
	fmt.Println("đó là toàn bộ câu trả lời cho 'vì sao Postgres cần VACUUM', và vì sao")
	fmt.Println("idle_in_transaction là thứ phải theo dõi trên production.")
	return nil
}

func key(i int) []byte { return []byte(fmt.Sprintf("k%06d", i)) }

func verdict(ok bool) string {
	if ok {
		return "đúng như dự đoán: không dọn được gì"
	}
	return "KHÁC DỰ ĐOÁN: dọn được, tức horizon không bị ghim"
}
