package sql

import (
	"strings"
	"testing"
	"time"
)

// FuzzParse khẳng định BA điều, và điều thứ hai là điều đắt nhất.
//
//  1. Parser không panic với đầu vào bất kỳ. Đây là điều hiển nhiên phải có:
//     câu SQL đến từ người dùng, nên mọi chuỗi byte là đầu vào hợp lệ của
//     hàm này.
//  2. Parser luôn KẾT THÚC. Lượt code của phase 8 có đúng một con bug thuộc
//     loại này và nó không phải lỗi sai kết quả — nó là một cái treo:
//     `WHERE x = = 1` làm vòng leo tầng quay vô hạn, vì đường lỗi không đẩy
//     con trỏ token đi. Một bài test thường chỉ đỏ khi kết quả sai; cái treo
//     thì làm cả bộ test đứng, và fuzz là chỗ duy nhất chắc chắn tìm ra nó.
//  3. In lại rồi phân tích lại phải BỀN: nếu Parse thành công thì
//     Parse(String(Parse(x))) phải cho cùng chuỗi. Bất biến này bắt lỗi ở cả
//     parser lẫn hàm in mà không cần viết ra AST mong đợi.
func FuzzParse(f *testing.F) {
	seeds := []string{
		"SELECT a FROM t",
		"SELECT * FROM t WHERE a = 1 AND b < 'x'",
		"SELECT a.x FROM p a JOIN q b ON a.id = b.id ORDER BY a.x DESC LIMIT 3",
		"CREATE TABLE t (a INT, b TEXT, PRIMARY KEY (a))",
		"CREATE UNIQUE INDEX ix ON t (a, b DESC)",
		"INSERT INTO t (a, b) VALUES (1, 'x'), (2, NULL)",
		"EXPLAIN ANALYZE SELECT a FROM t WHERE a >= 1",
		"ANALYZE t",
		// Những chuỗi từng làm treo hoặc từng sinh lỗi khó:
		"SELECT a FROM t WHERE x = = 1",
		"SELECT a FROM t WHERE ((((",
		"SELECT a FROM t WHERE a AND AND b",
		"SELECT a FROM t ORDER BY , ,",
		"SELECT a FORM t",
		"'",
		"--",
		";;;;",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, src string) {
		// Chốt thời gian cho MỖI đầu vào. Không có nó thì một cái treo biểu
		// hiện thành "fuzz chạy mãi", và khi ấy rất khó phân biệt với "fuzz
		// đang tìm được nhiều input mới" — đúng cái lẫn lộn mà phase 5 mất
		// nửa buổi mới gỡ ra (bộ rút gọn của Go, không phải code chậm).
		done := make(chan struct{})
		var out Stmt
		var err error
		go func() {
			defer close(done)
			out, err = Parse(src)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("Parse KHÔNG kết thúc sau 2s với %q — đường lỗi không đẩy"+
				" con trỏ token đi được", src)
		}
		if err != nil {
			// Lỗi cú pháp phải mang vị trí NẰM TRONG câu. Một thông báo lỗi
			// trỏ ra ngoài chuỗi là thông báo lỗi sai.
			if se, ok := err.(*SyntaxError); ok {
				if se.Pos < 0 || se.Pos > len(src) {
					t.Fatalf("vị trí lỗi %d ngoài chuỗi dài %d: %q", se.Pos, len(src), src)
				}
				if !strings.Contains(err.Error(), "cú pháp") {
					t.Fatalf("thông báo lỗi lạ: %v", err)
				}
			}
			return
		}
		sel, ok := out.(*Select)
		if !ok {
			return // chỉ SELECT có hàm in lại
		}
		printed := sel.String()
		again, err := Parse(printed)
		if err != nil {
			t.Fatalf("in lại ra câu KHÔNG phân tích được:\n  gốc:    %q\n  in lại: %q\n  lỗi: %v",
				src, printed, err)
		}
		if got := again.(*Select).String(); got != printed {
			t.Fatalf("không bền:\n  lần 1: %s\n  lần 2: %s", printed, got)
		}
	})
}

// FuzzLexer: bộ cắt token phải luôn kết thúc và luôn tiến lên.
//
// Bất biến "luôn tiến lên" (mỗi Next hoặc trả EOF hoặc đẩy pos lên) là thứ duy
// nhất bảo đảm Tokens hữu hạn. Khẳng định trực tiếp nó, thay vì tin rằng mọi
// nhánh của switch đều có p.pos++.
func FuzzLexer(f *testing.F) {
	for _, s := range []string{"a 1 'x' <= <> ! . * , ( ) ;", "0x", "'''", "--x\ny"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, src string) {
		l := NewLexer(src)
		prev := -1
		for i := 0; ; i++ {
			if i > len(src)+8 {
				t.Fatalf("cắt %d token từ chuỗi dài %d — có nhánh không tiến lên",
					i, len(src))
			}
			at := l.pos
			tok, err := l.Next()
			if err != nil {
				return
			}
			if tok.Kind == EOF {
				return
			}
			if l.pos <= at {
				t.Fatalf("token %v không đẩy pos (%d -> %d)", tok, at, l.pos)
			}
			if tok.Pos < prev {
				t.Fatalf("vị trí token lùi: %d sau %d", tok.Pos, prev)
			}
			prev = tok.Pos
		}
	})
}
