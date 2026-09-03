// Package sql là tầng CÚ PHÁP: chuỗi ký tự -> token -> AST. Nó không biết gì
// về catalog, về kiểu cột, về index, về chi phí.
//
// Biên giới ấy là có chủ ý và nó chính là câu trả lời cho câu hỏi mở đầu của
// phase 8 — "logical plan khác physical plan ở đâu?". Ba tầng, ba loại lỗi:
//
//	internal/sql   cú pháp     "SELECT FORM t"        -> lỗi phân tích
//	internal/plan  ngữ nghĩa   "SELECT z FROM t"      -> lỗi tên/kiểu, rồi TỐI ƯU
//	internal/exec  thi hành    hết bộ nhớ, đĩa lỗi    -> lỗi lúc chạy
//
// Nếu ba loại ấy trộn vào một package thì không thể trả lời được câu hỏi "một
// truy vấn sai chính tả có tốn một lần xuống cây nào không" — và câu trả lời
// (không) là lý do mọi database đều tách hẳn parser ra.
package sql

import "fmt"

// Kind là loại token.
type Kind uint8

const (
	EOF Kind = iota
	Ident
	Num   // số nguyên (chưa có số thực: xem nợ P8)
	Str   // 'chuỗi trong nháy đơn'
	Punct // , ( ) ; . *
	Op    // = <> < <= > >=
	Keyword
)

func (k Kind) String() string {
	switch k {
	case EOF:
		return "hết câu"
	case Ident:
		return "tên"
	case Num:
		return "số"
	case Str:
		return "chuỗi"
	case Punct:
		return "dấu"
	case Op:
		return "toán tử"
	case Keyword:
		return "từ khoá"
	}
	return "?"
}

// Token mang cả VỊ TRÍ. Không phải để đẹp: một câu lệnh SQL do người gõ tay
// thì thông báo lỗi không có vị trí là thông báo lỗi vô dụng, và cột lỗi là
// thứ duy nhất trong cả repo này mà người dùng cuối nhìn thấy.
type Token struct {
	Kind Kind
	Text string // chữ nguyên văn; với Keyword đã nâng thành CHỮ HOA
	Val  int64  // với Num
	Pos  int    // chỉ số byte trong câu, đếm từ 0
}

func (t Token) String() string {
	if t.Kind == EOF {
		return "<hết câu>"
	}
	return fmt.Sprintf("%q", t.Text)
}

// keywords là tập từ khoá được nhận. Danh sách NGẮN có chủ ý: mỗi từ khoá là
// một chữ mà người dùng không được dùng làm tên cột, nên thêm từ khoá là một
// thay đổi phá tương thích. Postgres có ~450 từ khoá và một bảng phân loại
// (reserved / unreserved / col-name-keyword) chỉ để giảm cái giá đó.
var keywords = map[string]bool{
	"SELECT": true, "FROM": true, "WHERE": true, "ORDER": true, "BY": true,
	"ASC": true, "DESC": true, "LIMIT": true, "JOIN": true, "INNER": true,
	"ON": true, "AS": true, "AND": true, "OR": true, "NOT": true,
	"CREATE": true, "TABLE": true, "INDEX": true, "UNIQUE": true,
	"PRIMARY": true, "KEY": true, "INSERT": true, "INTO": true, "VALUES": true,
	"EXPLAIN": true, "ANALYZE": true, "NULL": true, "TRUE": true, "FALSE": true,
	"INT": true, "UINT": true, "TEXT": true, "BOOL": true,
}
