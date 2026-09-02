package keys

import (
	"bytes"
	"math"
	"sort"
	"testing"
)

// TestOrderPreservedByCase là bảng thứ tự viết bằng tay: từng ca mà một bộ mã
// hoá tự nhiên (fmt.Sprint, hoặc binary.Write thẳng) sẽ SAI.
func TestOrderPreservedByCase(t *testing.T) {
	cases := []struct {
		name string
		vals []Value
	}{
		{"int âm và dương", []Value{Int(math.MinInt64), Int(-2), Int(-1), Int(0), Int(1), Int(math.MaxInt64)}},
		{"uint quanh biên", []Value{Uint(0), Uint(1), Uint(255), Uint(256), Uint(math.MaxUint64)}},
		{"chuỗi và tiền tố", []Value{Str(""), Str("a"), Str("ab"), Str("abc"), Str("b")}},
		{"chuỗi có byte 0x00", []Value{Str("a"), Str("a\x00"), Str("a\x00\x00"), Str("a\x00b"), Str("a\x01"), Str("ab")}},
		{"số 10 không đứng trước số 9", []Value{Int(9), Int(10), Int(100)}},
		{"null trước tất cả", []Value{Null(), Bool(false), Bool(true), Int(0), Uint(0), Str("")}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for i := 1; i < len(c.vals); i++ {
				a := AppendField(nil, c.vals[i-1], false)
				b := AppendField(nil, c.vals[i], false)
				if bytes.Compare(a, b) >= 0 {
					t.Fatalf("%v (%x) phải đứng TRƯỚC %v (%x)",
						c.vals[i-1], a, c.vals[i], b)
				}
				// Và chiều DESC phải đảo lại đúng thứ tự ấy.
				da := AppendField(nil, c.vals[i-1], true)
				db := AppendField(nil, c.vals[i], true)
				if bytes.Compare(da, db) <= 0 {
					t.Fatalf("DESC: %v (%x) phải đứng SAU %v (%x)",
						c.vals[i-1], da, c.vals[i], db)
				}
			}
		})
	}
}

// TestCompositeOrderMatchesSort là bất biến chính, phát biểu ở dạng dùng được:
// sắp các tuple bằng CompareTuple và sắp bằng bytes.Compare trên khóa đã mã
// hoá phải cho CÙNG MỘT thứ tự.
func TestCompositeOrderMatchesSort(t *testing.T) {
	ord := Order{false, true, false} // cột 2 DESC
	rows := [][]Value{
		{Str("b"), Int(1), Uint(1)},
		{Str("a"), Int(1), Uint(2)},
		{Str("a"), Int(2), Uint(0)},
		{Str("a"), Int(1), Uint(1)},
		{Str("a"), Null(), Uint(9)},
		{Str(""), Int(0), Uint(0)},
	}
	byLogic := append([][]Value(nil), rows...)
	sort.SliceStable(byLogic, func(i, j int) bool {
		return CompareTuple(byLogic[i], byLogic[j], ord) < 0
	})
	byBytes := append([][]Value(nil), rows...)
	sort.SliceStable(byBytes, func(i, j int) bool {
		return bytes.Compare(Encode(nil, byBytes[i], ord), Encode(nil, byBytes[j], ord)) < 0
	})
	for i := range byLogic {
		if CompareTuple(byLogic[i], byBytes[i], ord) != 0 {
			t.Fatalf("vị trí %d: logic=%v byte=%v", i, byLogic[i], byBytes[i])
		}
	}
}

// TestRoundTrip: giải mã lại phải ra đúng giá trị, và số byte đã dùng phải
// khớp — đó là cách duy nhất để biết chỗ nối giữa index key và primary key.
func TestRoundTrip(t *testing.T) {
	ord := Order{false, true, false, true}
	in := []Value{Str("khóa\x00lạ"), Int(-42), Null(), Uint(1 << 40)}
	enc := Encode(nil, in, ord)
	out, used, err := Decode(enc, len(in), ord)
	if err != nil {
		t.Fatal(err)
	}
	if used != len(enc) {
		t.Fatalf("dùng %d/%d byte", used, len(enc))
	}
	if CompareTuple(in, out, ord) != 0 {
		t.Fatalf("giải mã ra %v, mong %v", out, in)
	}
}

// TestNoLeadingZeroByte: byte đầu của một trường không bao giờ là 0x00. Nhờ nó
// mà khóa của tầng bảng không đi lạc vào không gian metadata 0x00... của
// internal/txn — một bất biến giữa hai package, nên phải có test ở đây.
func TestNoLeadingZeroByte(t *testing.T) {
	vals := []Value{Null(), Bool(false), Bool(true), Int(math.MinInt64), Int(0),
		Uint(0), Uint(math.MaxUint64), Str(""), Str("\x00"), Bytes([]byte{0xff, 0x00})}
	for _, v := range vals {
		for _, desc := range []bool{false, true} {
			b := AppendField(nil, v, desc)
			if b[0] == 0x00 {
				t.Fatalf("%v (desc=%v) mã hoá thành %x — byte đầu là 0x00", v, desc, b)
			}
		}
	}
}

// TestDecodeRejectsNonCanonical: mọi hình mà Encode không sinh ra được đều
// phải bị TỪ CHỐI, không phải được "hiểu đại khái". Đây là luật mà phase 6
// phải học bằng một con bug (bit cờ lạ bị nuốt im lặng).
func TestDecodeRejectsNonCanonical(t *testing.T) {
	bad := map[string][]byte{
		"tag lạ":               {0x7f},
		"tag 0x00":             {0x00},
		"int bị cắt":           {byte(TypeInt), 1, 2, 3},
		"bytes không kết thúc": {byte(TypeBytes), 'a', 'b'},
		"0x00 lẻ ở cuối":       {byte(TypeBytes), 'a', 0x00},
		"escape sai":           {byte(TypeBytes), 'a', 0x00, 0x01, 0x00, 0x00},
	}
	for name, b := range bad {
		if v, _, err := DecodeField(b, false); err == nil {
			t.Fatalf("%s: %x giải mã êm thành %v — phải là lỗi", name, b, v)
		}
	}
}

func TestPrefixEnd(t *testing.T) {
	cases := []struct{ in, want []byte }{
		{[]byte{0x01}, []byte{0x02}},
		{[]byte{0x01, 0xff}, []byte{0x02}},
		{[]byte{0xff, 0xff}, nil},
		{[]byte{0x0a, 0x00, 0x01}, []byte{0x0a, 0x00, 0x02}},
	}
	for _, c := range cases {
		if got := PrefixEnd(c.in); !bytes.Equal(got, c.want) {
			t.Fatalf("PrefixEnd(%x) = %x, mong %x", c.in, got, c.want)
		}
	}
}
