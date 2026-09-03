package sql

import (
	"fmt"
	"strconv"
	"strings"
)

// Lexer cắt câu thành token THEO YÊU CẦU (một token mỗi lần gọi Next), không
// cắt sẵn cả câu thành mảng.
//
// Vì sao theo yêu cầu: parser chỉ cần nhìn trước ĐÚNG MỘT token ở mọi chỗ
// trong ngữ pháp này (LL(1)), nên một mảng token là một mảng phải cấp phát và
// giữ nguyên trong suốt lần phân tích mà không ai đọc lại. Với một REPL thì
// chẳng đáng gì; với một hệ thống chạy hàng nghìn câu mỗi giây thì đó là rác.
// cmd/sqllab đo đúng chỗ này: bao nhiêu phần của một truy vấn điểm là chi phí
// front-end.
type Lexer struct {
	src string
	pos int
}

func NewLexer(src string) *Lexer { return &Lexer{src: src} }

// SyntaxError mang vị trí và một khung chỉ vào đúng chỗ sai.
type SyntaxError struct {
	Msg string
	Pos int
	Src string
}

func (e *SyntaxError) Error() string {
	// Khung chỉ chỗ sai: một dòng câu gốc, một dòng dấu ^. Đây là toàn bộ khác
	// biệt giữa "syntax error" và một thông báo dùng được.
	line := strings.ReplaceAll(e.Src, "\n", " ")
	p := e.Pos
	if p > len(line) {
		p = len(line)
	}
	return fmt.Sprintf("cú pháp: %s (cột %d)\n  %s\n  %s^", e.Msg, p+1, line, strings.Repeat(" ", p))
}

func (l *Lexer) errf(pos int, f string, a ...any) error {
	return &SyntaxError{Msg: fmt.Sprintf(f, a...), Pos: pos, Src: l.src}
}

func isSpace(c byte) bool  { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }
func isDigit(c byte) bool  { return c >= '0' && c <= '9' }
func isLetter(c byte) bool { return c == '_' || (c|0x20) >= 'a' && (c|0x20) <= 'z' }

// Next trả token kế tiếp. Hết câu thì trả Token{Kind: EOF} mãi mãi — không trả
// lỗi, vì "hết câu" là trạng thái bình thường mà parser phải xử lý, không phải
// một sự cố.
func (l *Lexer) Next() (Token, error) {
	// Bỏ trắng và chú thích -- tới hết dòng.
	for l.pos < len(l.src) {
		if isSpace(l.src[l.pos]) {
			l.pos++
			continue
		}
		if l.src[l.pos] == '-' && l.pos+1 < len(l.src) && l.src[l.pos+1] == '-' {
			for l.pos < len(l.src) && l.src[l.pos] != '\n' {
				l.pos++
			}
			continue
		}
		break
	}
	if l.pos >= len(l.src) {
		return Token{Kind: EOF, Pos: l.pos}, nil
	}

	start := l.pos
	c := l.src[l.pos]

	switch {
	case isLetter(c):
		for l.pos < len(l.src) && (isLetter(l.src[l.pos]) || isDigit(l.src[l.pos])) {
			l.pos++
		}
		word := l.src[start:l.pos]
		up := strings.ToUpper(word)
		if keywords[up] {
			return Token{Kind: Keyword, Text: up, Pos: start}, nil
		}
		// Tên KHÔNG được hạ chữ: catalog của phase 7 phân biệt chữ hoa chữ
		// thường, nên hạ ở đây là im lặng đổi ngữ nghĩa. (SQL chuẩn thì nâng
		// tên không nháy thành CHỮ HOA; chọn khác đi vì tên bảng trong repo
		// này được tạo bằng Go API và toàn chữ thường.)
		return Token{Kind: Ident, Text: word, Pos: start}, nil

	case isDigit(c):
		for l.pos < len(l.src) && isDigit(l.src[l.pos]) {
			l.pos++
		}
		if l.pos < len(l.src) && (l.src[l.pos] == '.' || l.src[l.pos]|0x20 == 'e') {
			return Token{}, l.errf(start, "chưa hỗ trợ số thực")
		}
		n, err := strconv.ParseInt(l.src[start:l.pos], 10, 64)
		if err != nil {
			return Token{}, l.errf(start, "số %q không vừa int64", l.src[start:l.pos])
		}
		return Token{Kind: Num, Text: l.src[start:l.pos], Val: n, Pos: start}, nil

	case c == '\'':
		l.pos++
		var sb strings.Builder
		for {
			if l.pos >= len(l.src) {
				return Token{}, l.errf(start, "chuỗi chưa đóng nháy")
			}
			if l.src[l.pos] == '\'' {
				// '' trong chuỗi là một dấu nháy — cách thoát của SQL chuẩn,
				// không phải backslash.
				if l.pos+1 < len(l.src) && l.src[l.pos+1] == '\'' {
					sb.WriteByte('\'')
					l.pos += 2
					continue
				}
				l.pos++
				break
			}
			sb.WriteByte(l.src[l.pos])
			l.pos++
		}
		return Token{Kind: Str, Text: sb.String(), Pos: start}, nil

	case c == '<':
		l.pos++
		if l.pos < len(l.src) && (l.src[l.pos] == '=' || l.src[l.pos] == '>') {
			l.pos++
			return Token{Kind: Op, Text: l.src[start:l.pos], Pos: start}, nil
		}
		return Token{Kind: Op, Text: "<", Pos: start}, nil

	case c == '>':
		l.pos++
		if l.pos < len(l.src) && l.src[l.pos] == '=' {
			l.pos++
			return Token{Kind: Op, Text: ">=", Pos: start}, nil
		}
		return Token{Kind: Op, Text: ">", Pos: start}, nil

	case c == '=':
		l.pos++
		return Token{Kind: Op, Text: "=", Pos: start}, nil

	case c == '!':
		l.pos++
		if l.pos < len(l.src) && l.src[l.pos] == '=' {
			l.pos++
			return Token{Kind: Op, Text: "<>", Pos: start}, nil
		}
		return Token{}, l.errf(start, "'!' đứng một mình")

	case strings.IndexByte(",();.*", c) >= 0:
		l.pos++
		return Token{Kind: Punct, Text: string(c), Pos: start}, nil
	}
	return Token{}, l.errf(start, "ký tự lạ %q", string(c))
}

// Tokens cắt cả câu — chỉ dùng cho test và cho cmd/sqllab khi cần đo riêng
// giai đoạn lex. Parser không gọi nó.
func Tokens(src string) ([]Token, error) {
	l := NewLexer(src)
	var out []Token
	for {
		t, err := l.Next()
		if err != nil {
			return nil, err
		}
		out = append(out, t)
		if t.Kind == EOF {
			return out, nil
		}
	}
}
