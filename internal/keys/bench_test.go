package keys

import (
	"fmt"
	"testing"
)

// BenchmarkEncode đo giá của việc mã hoá một khóa. Con số này phải được đặt
// cạnh giá của một bước quét (~350 ns, BenchmarkSeqStep của internal/query):
// nếu mã hoá khóa chiếm phần đáng kể ở đó thì mọi kết luận về "index nhanh
// hơn seq" đang đo bộ mã hoá chứ không đo cấu trúc dữ liệu.
func BenchmarkEncode(b *testing.B) {
	cases := []struct {
		name string
		vals []Value
		ord  Order
	}{
		{"uint", []Value{Uint(1 << 40)}, nil},
		{"str16", []Value{Str("abcdefghijklmnop")}, nil},
		{"str16-desc", []Value{Str("abcdefghijklmnop")}, Order{true}},
		{"composite3", []Value{Str("HN"), Int(-30), Uint(7)}, Order{false, true, false}},
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			buf := make([]byte, 0, 64)
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				buf = Encode(buf[:0], c.vals, c.ord)
			}
			if len(buf) == 0 {
				b.Fatal("rỗng")
			}
		})
	}
}

// BenchmarkDecode: giải mã đắt hơn mã hoá vì phần thân của bytes phải được
// CHÉP ra (buffer khóa là của cursor, sẽ bị ghi lại ở bước Next kế). Đó là một
// lần cấp phát cho mỗi cột kiểu bytes trên mỗi hàng — và là món nợ hiển nhiên
// nhất của tầng này.
func BenchmarkDecode(b *testing.B) {
	ord := Order{false, true, false}
	enc := Encode(nil, []Value{Str("HN"), Int(-30), Uint(7)}, ord)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, _, err := Decode(enc, 3, ord); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkCompareVsBytes là câu hỏi nền của cả package: so trên byte đã mã
// hoá (cái mà B+Tree làm) so với so trên giá trị có kiểu (cái mà một engine
// giữ tuple sẽ phải làm). Tỉ số này là lý do mọi DB đều ép khóa thành byte.
func BenchmarkCompareVsBytes(b *testing.B) {
	// ASC cả hai cột: bản đầu của benchmark này dùng cột 2 DESC rồi khẳng
	// định 30 < 31 và đỏ ngay — chiều sắp là một phần của phép so, không
	// phải một thuộc tính của giá trị. Lỗi của bộ đo, giữ lại chú thích.
	var ord Order
	a := []Value{Str("HN"), Int(30)}
	c := []Value{Str("HN"), Int(31)}
	ea, ec := Encode(nil, a, ord), Encode(nil, c, ord)
	b.Run("tuple", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if CompareTuple(a, c, ord) >= 0 {
				b.Fatal("thứ tự")
			}
		}
	})
	b.Run("memcmp", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if string(ea) >= string(ec) {
				b.Fatal("thứ tự")
			}
		}
	})
	_ = fmt.Sprint(len(ea))
}
