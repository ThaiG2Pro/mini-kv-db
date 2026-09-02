// Package keys là bộ mã hoá khóa GIỮ THỨ TỰ: nó biến một dãy giá trị có kiểu
// thành một dãy byte sao cho
//
//	bytes.Compare(Encode(a), Encode(b)) == CompareTuple(a, b)
//
// Một dòng đó là toàn bộ lý do package này tồn tại, và nó là điều kiện để
// phase 7 có gì để làm cả.
//
// Vì sao nó là điều kiện: B+Tree của phase 4 chỉ biết một phép so — memcmp
// trên khóa. Nó không biết "cột", không biết "kiểu", không biết ASC/DESC. Muốn
// có composite key và secondary index thì phải NHÉT toàn bộ ngữ nghĩa ấy vào
// hình dạng byte, chứ không thể nhét vào phép so. Đây chính là chỗ ranh giới
// "index chỉ dùng được cho tiền tố bên trái của composite key" sinh ra: một
// khóa đã ép phẳng thành byte thì lọc theo cột thứ hai không còn là một khoảng
// liên tục nữa. Đọc `EXPLAIN` của Postgres/MySQL bằng trực giác của người đã
// tự viết ra nó nghĩa là nhìn ra điều đó từ hình dạng byte.
//
// Ba luật của bộ mã hoá, và tất cả đều bị fuzz:
//
//  1. GIỮ THỨ TỰ. Xem FuzzKeyOrder.
//  2. CANONICAL: một giá trị có đúng MỘT cách viết, và Decode từ chối mọi byte
//     mà Encode không sinh ra được. Phase 6 đã trả giá cho việc thiếu luật này
//     (fuzzer bắt trong 3 giây: bit cờ lạ bị nuốt, tombstone mang thân), nên ở
//     đây nó được viết ra TRƯỚC khi có bug.
//  3. TỰ PHÂN GIỚI: mỗi trường tự biết mình hết ở đâu, không cần độ dài đứng
//     trước. Bắt buộc, vì một khóa composite là các trường NỐI ĐUÔI nhau và
//     phép so là memcmp: nhét độ dài lên trước là để độ dài tham gia vào phép
//     so, và "aa" sẽ đứng sau "b".
package keys

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
)

var (
	ErrBadKey      = errors.New("keys: byte khóa không hợp lệ")
	ErrShortKey    = errors.New("keys: byte khóa bị cắt giữa trường")
	ErrTypeUnknown = errors.New("keys: tag kiểu lạ")
)

// Type là kiểu của một giá trị. Giá trị của hằng CHÍNH LÀ tag trên đĩa, nên
// thứ tự khai báo ở đây là thứ tự so giữa hai kiểu khác nhau: NULL nhỏ nhất,
// rồi bool, rồi số, rồi byte.
//
// 0x00 KHÔNG được dùng làm tag: nhờ vậy byte đầu của một trường không bao giờ
// là 0x00, nên khóa của tầng trên không bao giờ đi lạc vào không gian metadata
// 0x00... mà internal/txn dành riêng (checkKey chặn ở mọi cửa vào).
type Type uint8

const (
	TypeNull  Type = 0x01
	TypeFalse Type = 0x02
	TypeTrue  Type = 0x03
	TypeInt   Type = 0x04 // int64, 8 byte, đã đảo bit dấu
	TypeUint  Type = 0x05 // uint64, 8 byte big-endian
	TypeBytes Type = 0x06 // byte/chuỗi, 0x00 được escape, kết bằng 0x00 0x00
)

func (t Type) String() string {
	switch t {
	case TypeNull:
		return "null"
	case TypeFalse, TypeTrue:
		return "bool"
	case TypeInt:
		return "int"
	case TypeUint:
		return "uint"
	case TypeBytes:
		return "bytes"
	}
	return fmt.Sprintf("kiểu?%#02x", uint8(t))
}

// Value là một giá trị có kiểu. Không dùng interface{}: một cột chứa hàng
// triệu giá trị thì mỗi lần boxing là một lần cấp phát, và đường đọc của phase
// 7 là chỗ duy nhất trong repo này có vòng lặp chạy hàng triệu lần.
type Value struct {
	T Type
	I int64
	U uint64
	B []byte
}

func Null() Value          { return Value{T: TypeNull} }
func Int(i int64) Value    { return Value{T: TypeInt, I: i} }
func Uint(u uint64) Value  { return Value{T: TypeUint, U: u} }
func Bytes(b []byte) Value { return Value{T: TypeBytes, B: b} }
func Str(s string) Value   { return Value{T: TypeBytes, B: []byte(s)} }
func Bool(b bool) Value {
	if b {
		return Value{T: TypeTrue}
	}
	return Value{T: TypeFalse}
}

// IsNull: NULL là một giá trị, không phải sự vắng mặt — và nó sắp TRƯỚC mọi
// giá trị khác (chọn giống Postgres mặc định NULLS FIRST cho ASC).
func (v Value) IsNull() bool { return v.T == TypeNull }

func (v Value) String() string {
	switch v.T {
	case TypeNull:
		return "NULL"
	case TypeFalse:
		return "false"
	case TypeTrue:
		return "true"
	case TypeInt:
		return strconv.FormatInt(v.I, 10)
	case TypeUint:
		return strconv.FormatUint(v.U, 10)
	case TypeBytes:
		return strconv.Quote(string(v.B))
	}
	return "?"
}

// Compare so hai giá trị theo thứ tự LOGIC. Kiểu khác nhau thì so theo tag —
// không phải vì so số với chuỗi có nghĩa gì, mà vì một phép so không có câu
// trả lời là một phép so sẽ vỡ ở chỗ khó tìm nhất. Schema mới là chỗ ngăn hai
// kiểu gặp nhau trong một cột.
func Compare(a, b Value) int {
	if a.T != b.T {
		if a.T < b.T {
			return -1
		}
		return 1
	}
	switch a.T {
	case TypeInt:
		switch {
		case a.I < b.I:
			return -1
		case a.I > b.I:
			return 1
		}
		return 0
	case TypeUint:
		switch {
		case a.U < b.U:
			return -1
		case a.U > b.U:
			return 1
		}
		return 0
	case TypeBytes:
		return bytes.Compare(a.B, b.B)
	}
	return 0 // null, false, true: tag đã quyết định
}

// CompareTuple so hai dãy giá trị theo thứ tự từ điển, có tính chiều sắp.
// Dãy ngắn hơn mà là tiền tố thì đứng trước — đúng như "abc" > "ab".
func CompareTuple(a, b []Value, ord Order) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		c := Compare(a[i], b[i])
		if ord.desc(i) {
			c = -c
		}
		if c != 0 {
			return c
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	}
	return 0
}

// Order là chiều sắp của từng cột; true = DESC. nil = ASC hết.
type Order []bool

func (o Order) desc(i int) bool { return i < len(o) && o[i] }

// xorOf: DESC cài bằng cách BÙ TỪNG BYTE của cả trường (tag lẫn thân).
//
// Phép bù đảo thứ tự byte-wise, nên nó đảo đúng thứ tự của một mã hoá tự phân
// giới. Hệ quả đáng chú ý: nó đảo cả vị trí của NULL, tức DESC ở đây là
// "NULLS LAST" — giống hệt mặc định của Postgres, và không phải do bắt chước
// mà do hình dạng byte không cho phép khác.
func xorOf(desc bool) byte {
	if desc {
		return 0xff
	}
	return 0
}

// AppendField mã hoá một trường vào dst.
func AppendField(dst []byte, v Value, desc bool) []byte {
	x := xorOf(desc)
	dst = append(dst, byte(v.T)^x)
	switch v.T {
	case TypeNull, TypeFalse, TypeTrue:
		// Tag đã là toàn bộ nội dung.
	case TypeInt:
		// Đảo bit dấu: -1 -> 0x7fff...ff, 0 -> 0x8000...00. Nhờ đó thứ tự
		// big-endian không dấu TRÙNG với thứ tự có dấu. Không có bước này thì
		// mọi số âm đứng SAU mọi số dương, và một index trên cột int là một
		// cái bẫy im lặng.
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(v.I)^(1<<63))
		for _, c := range b {
			dst = append(dst, c^x)
		}
	case TypeUint:
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], v.U)
		for _, c := range b {
			dst = append(dst, c^x)
		}
	case TypeBytes:
		// 0x00 -> 0x00 0xff, kết thúc bằng 0x00 0x00. Đây là cách của
		// CockroachDB/Postgres cho khóa văn bản, và nó là cách duy nhất vừa
		// tự phân giới vừa giữ thứ tự: một dấu kết thúc NHỎ HƠN mọi byte nội
		// dung thì tiền tố tự động đứng trước.
		for _, c := range v.B {
			if c == 0x00 {
				dst = append(dst, 0x00^x, 0xff^x)
			} else {
				dst = append(dst, c^x)
			}
		}
		dst = append(dst, 0x00^x, 0x00^x)
	default:
		panic(fmt.Sprintf("keys: mã hoá kiểu lạ %#02x", uint8(v.T)))
	}
	return dst
}

// DecodeField giải mã một trường, trả về giá trị và số byte đã dùng.
//
// Phần thân của TypeBytes được CHÉP ra: người gọi thường giữ giá trị lâu hơn
// buffer khóa (buffer ấy là của cursor và bị ghi lại ở bước Next kế).
func DecodeField(b []byte, desc bool) (Value, int, error) {
	if len(b) == 0 {
		return Value{}, 0, fmt.Errorf("%w: hết byte khi chờ tag", ErrShortKey)
	}
	x := xorOf(desc)
	t := Type(b[0] ^ x)
	switch t {
	case TypeNull, TypeFalse, TypeTrue:
		return Value{T: t}, 1, nil
	case TypeInt:
		if len(b) < 9 {
			return Value{}, 0, fmt.Errorf("%w: int cần 8 byte, có %d", ErrShortKey, len(b)-1)
		}
		var raw [8]byte
		for i := 0; i < 8; i++ {
			raw[i] = b[1+i] ^ x
		}
		return Value{T: TypeInt, I: int64(binary.BigEndian.Uint64(raw[:]) ^ (1 << 63))}, 9, nil
	case TypeUint:
		if len(b) < 9 {
			return Value{}, 0, fmt.Errorf("%w: uint cần 8 byte, có %d", ErrShortKey, len(b)-1)
		}
		var raw [8]byte
		for i := 0; i < 8; i++ {
			raw[i] = b[1+i] ^ x
		}
		return Value{T: TypeUint, U: binary.BigEndian.Uint64(raw[:])}, 9, nil
	case TypeBytes:
		out := []byte{}
		i := 1
		for {
			if i >= len(b) {
				return Value{}, 0, fmt.Errorf("%w: bytes không có dấu kết thúc", ErrShortKey)
			}
			c := b[i] ^ x
			if c != 0x00 {
				out = append(out, c)
				i++
				continue
			}
			if i+1 >= len(b) {
				return Value{}, 0, fmt.Errorf("%w: 0x00 lẻ ở cuối", ErrShortKey)
			}
			switch b[i+1] ^ x {
			case 0x00:
				return Value{T: TypeBytes, B: out}, i + 2, nil
			case 0xff:
				out = append(out, 0x00)
				i += 2
			default:
				// Không canonical: Encode không bao giờ sinh ra hình này.
				// Chấp nhận nó là mở cửa cho hai dãy byte khác nhau cùng giải
				// ra một giá trị — tức là cùng một hàng có hai khóa index.
				return Value{}, 0, fmt.Errorf("%w: sau 0x00 phải là 0x00 hoặc 0xff, gặp %#02x",
					ErrBadKey, b[i+1]^x)
			}
		}
	}
	return Value{}, 0, fmt.Errorf("%w: %#02x", ErrTypeUnknown, uint8(t))
}

// Encode mã hoá cả dãy giá trị.
func Encode(dst []byte, vals []Value, ord Order) []byte {
	for i, v := range vals {
		dst = AppendField(dst, v, ord.desc(i))
	}
	return dst
}

// EncodeTo là dạng tiện cho lối gọi nóng: cấp một lần rồi dùng lại buffer.
func EncodeTo(dst []byte, ord Order, vals ...Value) []byte {
	return Encode(dst, vals, ord)
}

// Decode giải mã đúng n trường và trả về số byte đã dùng. n phải là số cột mà
// schema nói, chứ không phải "đọc tới hết": một khóa index là <cột index> nối
// <primary key>, và chỗ nối nằm ở đâu chỉ schema biết.
func Decode(b []byte, n int, ord Order) ([]Value, int, error) {
	out := make([]Value, 0, n)
	off := 0
	for i := 0; i < n; i++ {
		v, used, err := DecodeField(b[off:], ord.desc(i))
		if err != nil {
			return nil, 0, fmt.Errorf("trường %d: %w", i, err)
		}
		out = append(out, v)
		off += used
	}
	return out, off, nil
}

// DecodeAll đọc tới hết byte. Trả lỗi nếu còn byte lẻ không thành trường.
func DecodeAll(b []byte, ord Order) ([]Value, error) {
	var out []Value
	off := 0
	for off < len(b) {
		v, used, err := DecodeField(b[off:], ord.desc(len(out)))
		if err != nil {
			return nil, fmt.Errorf("trường %d: %w", len(out), err)
		}
		out = append(out, v)
		off += used
	}
	return out, nil
}

// PrefixEnd trả khóa nhỏ nhất lớn hơn mọi khóa có tiền tố p — chặn trên của
// một range scan theo tiền tố. nil nghĩa là "tới hết cây" (p toàn 0xff).
//
// Hàm ba dòng này là thứ biến "mọi hàng của bảng 7" thành một KHOẢNG LIÊN TỤC,
// tức là biến seq scan của một bảng thành một range scan của cây. Không có nó
// thì mọi bảng phải là một cây riêng.
func PrefixEnd(p []byte) []byte {
	out := append([]byte(nil), p...)
	for i := len(out) - 1; i >= 0; i-- {
		if out[i] != 0xff {
			out[i]++
			return out[:i+1]
		}
	}
	return nil
}

// MaxUint/MinInt... là các giá trị biên dùng làm chặn khi người gọi chỉ ràng
// buộc một đầu. Có sẵn ở đây để không ai phải tự viết 1<<63 nhầm dấu.
var (
	MinInt  = Int(math.MinInt64)
	MaxInt  = Int(math.MaxInt64)
	MaxUint = Uint(math.MaxUint64)
)
