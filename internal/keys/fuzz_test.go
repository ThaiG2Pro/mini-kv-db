package keys

import (
	"bytes"
	"testing"
)

// valuesFrom biến byte rác thành một dãy Value hợp lệ. Mỗi giá trị lấy 1 byte
// tag + phần thân, nên fuzzer điều khiển được cả kiểu lẫn nội dung.
func valuesFrom(script []byte) ([]Value, Order) {
	var vals []Value
	var ord Order
	for i := 0; i < len(script); {
		b := script[i]
		i++
		desc := b&0x80 != 0
		switch b % 6 {
		case 0:
			vals = append(vals, Null())
		case 1:
			vals = append(vals, Bool(b&0x40 != 0))
		case 2:
			var n int64
			for j := 0; j < 8 && i < len(script); j++ {
				n = n<<8 | int64(script[i])
				i++
			}
			vals = append(vals, Int(n-(1<<40)))
		case 3:
			var n uint64
			for j := 0; j < 8 && i < len(script); j++ {
				n = n<<8 | uint64(script[i])
				i++
			}
			vals = append(vals, Uint(n))
		default:
			ln := int(b) % 9
			if ln > len(script)-i {
				ln = len(script) - i
			}
			vals = append(vals, Bytes(append([]byte(nil), script[i:i+ln]...)))
			i += ln
		}
		ord = append(ord, desc)
	}
	return vals, ord
}

// FuzzKeyOrder đánh vào bất biến ĐỊNH NGHĨA của package: thứ tự byte phải
// bằng thứ tự logic. Đây là chỗ mà một bài test viết tay không bao giờ đủ —
// những ca vỡ là những ca tiền tố, byte 0x00, và biên của số có dấu, tức là
// đúng những hình mà người viết test hay tưởng mình đã nghĩ ra hết.
func FuzzKeyOrder(f *testing.F) {
	f.Add([]byte{2, 0, 0, 0, 0, 0, 0, 0, 1}, []byte{2, 0, 0, 0, 0, 0, 0, 0, 2})
	f.Add([]byte{5, 'a'}, []byte{5, 'a', 'b'})
	f.Add([]byte{5, 0x00}, []byte{5, 0x01})
	f.Add([]byte{0}, []byte{1})

	f.Fuzz(func(t *testing.T, sa, sb []byte) {
		if len(sa) > 64 {
			sa = sa[:64]
		}
		if len(sb) > 64 {
			sb = sb[:64]
		}
		va, orda := valuesFrom(sa)
		vb, _ := valuesFrom(sb)
		// Chiều sắp phải là MỘT cho cả hai bên: so hai khóa mã hoá bằng hai
		// schema khác nhau là một câu hỏi vô nghĩa.
		n := len(va)
		if len(vb) < n {
			n = len(vb)
		}
		ord := orda
		if len(ord) > n {
			ord = ord[:n]
		}

		ea := Encode(nil, va, ord)
		eb := Encode(nil, vb, ord)
		want := CompareTuple(va, vb, ord)
		got := bytes.Compare(ea, eb)
		if sign(want) != sign(got) {
			t.Fatalf("thứ tự vỡ:\n a=%v -> %x\n b=%v -> %x\n logic=%d byte=%d ord=%v",
				va, ea, vb, eb, want, got, ord)
		}

		// Và giải mã lại phải ra đúng dãy cũ, dùng đúng hết byte.
		back, used, err := Decode(ea, len(va), ord)
		if err != nil {
			t.Fatalf("giải mã khóa tự mã hoá: %v (a=%v %x)", err, va, ea)
		}
		if used != len(ea) {
			t.Fatalf("dùng %d/%d byte", used, len(ea))
		}
		if CompareTuple(va, back, ord) != 0 {
			t.Fatalf("vòng tròn lệch: %v -> %v", va, back)
		}
	})
}

func sign(x int) int {
	switch {
	case x < 0:
		return -1
	case x > 0:
		return 1
	}
	return 0
}

// FuzzKeyCodec đánh vào tính CANONICAL từ phía byte rác: giải mã được thì mã
// hoá lại phải ra đúng byte cũ. Đây là bài mà phase 6 phải học bằng một con
// bug thật, nên ở đây nó có mặt từ dòng code đầu tiên.
func FuzzKeyCodec(f *testing.F) {
	f.Add([]byte{0x01})
	f.Add([]byte{0x06, 'a', 0x00, 0x00})
	f.Add([]byte{0x04, 0x80, 0, 0, 0, 0, 0, 0, 0})
	f.Add(make([]byte, 24))

	f.Fuzz(func(t *testing.T, script []byte) {
		if len(script) > 200 {
			script = script[:200]
		}
		for _, desc := range []bool{false, true} {
			v, used, err := DecodeField(script, desc)
			if err != nil {
				continue
			}
			if used > len(script) {
				t.Fatalf("báo dùng %d byte trên %d", used, len(script))
			}
			again := AppendField(nil, v, desc)
			if !bytes.Equal(again, script[:used]) {
				t.Fatalf("mã hoá lại khác byte gốc (desc=%v):\n gốc %x\n lại %x\n giá trị %v",
					desc, script[:used], again, v)
			}
		}
		// DecodeAppend chép thân của nhiều trường vào CÙNG một arena: giải
		// hai lần liền nhau vào một arena có sẵn rác ở đầu, rồi so từng trường
		// với DecodeField. Trường sau giẫm lên trường trước thì đỏ ở đây.
		ord := Order{false, true}
		vs, _, used, err := DecodeAppend(nil, []byte("rác"), script, 2, ord)
		if err != nil {
			return
		}
		off := 0
		for i, v := range vs {
			w, u, err := DecodeField(script[off:], ord.desc(i))
			if err != nil || !bytes.Equal(AppendField(nil, v, ord.desc(i)), AppendField(nil, w, ord.desc(i))) {
				t.Fatalf("trường %d: DecodeAppend %v, DecodeField %v (%v)", i, v, w, err)
			}
			off += u
		}
		if off != used {
			t.Fatalf("DecodeAppend báo dùng %d byte, từng trường cộng lại %d", used, off)
		}
	})
}
