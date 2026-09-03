// minidb là REPL SQL: nó nhận SQL trên stdin (hoặc -e / -f) rồi chạy trên một
// file database thật.
//
// Đây là deliverable "chạy được end-to-end" của phase 8, và nó cũng là công cụ
// duy nhất trong repo mà người dùng cuối nhìn thấy — nên nó phải làm được hai
// việc mà một REPL đồ chơi thường bỏ: in lỗi cú pháp có VỊ TRÍ, và in EXPLAIN
// của chính nó.
//
//	$ echo "EXPLAIN SELECT a.city FROM ev a JOIN u b ON a.id=b.id WHERE a.kind<5;" \
//	    | go run ./cmd/minidb -db data/sql/x.db
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"minidb/internal/db"
	"minidb/internal/engine"
	"minidb/internal/keys"
	"minidb/internal/sql"
	"minidb/internal/txn"
)

func main() {
	var (
		path   = flag.String("db", "data/sql/minidb.db", "file database")
		one    = flag.String("e", "", "chạy một chuỗi SQL rồi thoát")
		file   = flag.String("f", "", "chạy một file .sql rồi thoát")
		frames = flag.Int("frames", 256, "số frame của buffer pool")
		budget = flag.Int("budget", 0, "hạn mức bộ nhớ cho sort/hash join, tính bằng HÀNG (0 = mặc định)")
		level  = flag.String("level", "repeatable-read", "mức isolation")
		quiet  = flag.Bool("q", false, "không in số hàng và thời gian")
	)
	flag.Parse()

	if err := run(*path, *one, *file, *frames, *budget, *level, *quiet); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(path, one, file string, frames, budget int, levelStr string, quiet bool) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	s, err := txn.Open(path, db.Options{Frames: frames})
	if err != nil {
		return err
	}
	defer s.Close()

	e, err := engine.New(s)
	if err != nil {
		return err
	}
	if l, err := txn.ParseLevel(levelStr); err == nil {
		e.Level = l
	} else {
		return err
	}
	if budget > 0 {
		e.Pl.Budget = budget
	}

	switch {
	case one != "":
		return runScript(e, one, quiet)
	case file != "":
		b, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		return runScript(e, string(b), quiet)
	}
	return repl(e, quiet)
}

// runScript chạy nhiều câu và in kết quả TỪNG câu — khác ExecSQL của engine
// (chỉ trả câu cuối), vì một script thì mỗi câu đều có tiếng nói.
func runScript(e *engine.Engine, src string, quiet bool) error {
	stmts, err := sql.ParseMany(src)
	if err != nil {
		return err
	}
	for _, st := range stmts {
		if err := one(e, st, quiet); err != nil {
			return err
		}
	}
	return nil
}

func repl(e *engine.Engine, quiet bool) error {
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 1<<20)
	var buf strings.Builder
	interactive := isTTY()
	if interactive {
		fmt.Println("minidb — kết thúc câu bằng ';', Ctrl-D để thoát")
	}
	prompt := func() {
		if !interactive {
			return
		}
		if buf.Len() == 0 {
			fmt.Print("minidb> ")
		} else {
			fmt.Print("     ...> ")
		}
	}
	prompt()
	for in.Scan() {
		line := in.Text()
		buf.WriteString(line)
		buf.WriteString("\n")
		// Chỉ chạy khi thấy ';': nhờ vậy câu nhiều dòng gõ được, và một câu
		// chưa xong không bị chạy nửa vời.
		if !strings.Contains(line, ";") {
			prompt()
			continue
		}
		src := buf.String()
		buf.Reset()
		stmts, err := sql.ParseMany(src)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			prompt()
			continue
		}
		for _, st := range stmts {
			if err := one(e, st, quiet); err != nil {
				fmt.Fprintln(os.Stderr, "lỗi:", err)
				break
			}
		}
		prompt()
	}
	return in.Err()
}

func isTTY() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func one(e *engine.Engine, st sql.Stmt, quiet bool) error {
	res, err := e.Exec(st)
	if err != nil {
		return err
	}
	switch {
	case res.Explain != "":
		fmt.Print(res.Explain)
	case res.Msg != "":
		fmt.Println(res.Msg)
	default:
		printRows(res.Cols, res.Rows)
		if !quiet {
			fmt.Printf("(%d hàng, %v · đọc %d hàng bảng, %d mục index, %d lần tra bảng)\n",
				len(res.Rows), res.Elapsed.Round(time.Microsecond),
				res.TStat.RowsScanned, res.TStat.IndexEntries, res.TStat.RowFetches)
		}
	}
	return nil
}

// printRows in bảng có cột thẳng. Bề rộng tính từ dữ liệu THẬT, nên phải gom
// hết hàng trước khi in — đó là lý do REPL không stream, dù engine thì có.
func printRows(cols []string, rows [][]keys.Value) {
	if len(cols) == 0 {
		return
	}
	w := make([]int, len(cols))
	for i, c := range cols {
		w[i] = len(c)
	}
	cells := make([][]string, len(rows))
	for r, row := range rows {
		cells[r] = make([]string, len(row))
		for i, v := range row {
			s := fmtVal(v)
			cells[r][i] = s
			if len(s) > w[i] {
				w[i] = len(s)
			}
		}
	}
	var b strings.Builder
	for i, c := range cols {
		fmt.Fprintf(&b, " %-*s ", w[i], c)
		if i < len(cols)-1 {
			b.WriteString("|")
		}
	}
	fmt.Println(b.String())
	b.Reset()
	for i := range cols {
		b.WriteString(strings.Repeat("-", w[i]+2))
		if i < len(cols)-1 {
			b.WriteString("+")
		}
	}
	fmt.Println(b.String())
	for _, row := range cells {
		b.Reset()
		for i, s := range row {
			fmt.Fprintf(&b, " %-*s ", w[i], s)
			if i < len(row)-1 {
				b.WriteString("|")
			}
		}
		fmt.Println(b.String())
	}
}

func fmtVal(v keys.Value) string {
	switch v.T {
	case keys.TypeNull:
		return "NULL"
	case keys.TypeBytes:
		return string(v.B)
	}
	return v.String()
}
