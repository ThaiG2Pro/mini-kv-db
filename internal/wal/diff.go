package wal

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
)

// Diff và payload của record UPDATE/CLR.
//
// Vì sao phải diff thay vì log trọn page: một lần chèn khóa vào slotted page
// đụng vào ba chỗ rời nhau — header (numSlots, cellStart), một slot ở giữa
// mảng slot, và cell ở sát mép phải. Ba chỗ đó cộng lại chưa tới 200 byte
// trong khi page là 4096. Log trọn page là 20x write amplification cho đúng
// một lần insert. Postgres/InnoDB đều log ở mức "byte range trong page" vì lẽ
// đó — chỉ riêng lần chạm đầu tiên sau checkpoint mới ghi trọn page
// (FlagFullPage), để chống torn write.
//
// Diff theo KHỐI chứ không theo byte: so từng byte rồi gộp lại sẽ đẻ ra hàng
// chục đoạn 1-2 byte, mà mỗi đoạn tốn 4 byte mô tả. Khối 32 byte là chỗ cân
// bằng: đủ nhỏ để không kéo theo nhiều byte thừa, đủ lớn để số đoạn nhỏ.
const DiffGran = 32

// Seg là một đoạn byte thay đổi trong page.
type Seg struct {
	Off uint16
	Len uint16
}

var ErrBadPayload = errors.New("wal: payload của record không hợp lệ")

// Diff trả về danh sách đoạn khác nhau giữa before và after, gộp các khối liền
// kề lại. before và after phải cùng độ dài.
func Diff(before, after []byte, gran int) []Seg {
	if len(before) != len(after) {
		panic("wal: Diff hai buffer khác độ dài")
	}
	var segs []Seg
	n := len(before)
	i := 0
	for i < n {
		hi := min(i+gran, n)
		if equal(before[i:hi], after[i:hi]) {
			i = hi
			continue
		}
		// Nuốt luôn các khối khác nhau nối tiếp: một cú Compact() đổi cả vùng
		// cell, để nguyên sẽ thành 100 đoạn thay vì 1.
		start := i
		for i < n {
			h := min(i+gran, n)
			if equal(before[i:h], after[i:h]) {
				break
			}
			i = h
		}
		segs = append(segs, Seg{Off: uint16(start), Len: uint16(i - start)})
	}
	return segs
}

// So khối bằng bytes.Equal chứ không phải vòng lặp từng byte: bytes.Equal là
// assembly SIMD, so 16-32 byte mỗi nhịp. Vòng lặp tay ở đây từng ngốn 3.2µs
// cho một cặp page 4KB — nhiều hơn cả phần còn lại của một lần Put cộng lại,
// vì mỗi lần Put phải diff cả các page trong đã pin mà không đổi gì.
func equal(a, b []byte) bool { return bytes.Equal(a, b) }

// SegBytes là tổng số byte các đoạn phủ.
func SegBytes(segs []Seg) int {
	n := 0
	for _, s := range segs {
		n += int(s.Len)
	}
	return n
}

// Payload của UPDATE/CLR/FREE mang HAI danh sách đoạn, không phải một:
//
//	u16 nB | nB*(u16 off,u16 len)   -- các đoạn của ảnh TRƯỚC (undo)
//	u16 nA | nA*(u16 off,u16 len)   -- các đoạn của ảnh SAU   (redo)
//	before[] | after[]
//
// Vì sao hai: ảnh-trọn-page (FlagFullPage) chỉ cần TRỌN ở phía redo. Nó tồn
// tại để đè lên một page có thể đã bị torn write, và pha redo bao giờ cũng
// chạy trước pha undo — nên khi undo cần ảnh trước, page đã lành lại rồi và
// một bản diff là đủ. Dùng chung một danh sách đoạn cho cả hai thì mỗi lần
// ghi trọn page tốn 8192 byte thay vì 4096 + diff. Đo được trên workload của
// crashlab: 1928 record trọn page = 15.8 MB trong tổng 51.7 MB log.
//
// Ghi chú cho phase 6: chỗ nhân đôi CÒN LẠI — ảnh trước của mọi UPDATE thường
// — là cái giá của undo VẬT LÝ. Postgres không trả giá này vì nó không undo
// tại chỗ (MVCC: bản cũ vẫn nằm trong heap); InnoDB trả nó ở một undo log
// riêng chứ không trong WAL. Xem nợ P5-2.

// EncodePayload dựng payload từ hai ảnh TRỌN page. segsBefore/segsAfter nil
// nghĩa là phía đó không có mặt.
func EncodePayload(dst []byte, segsBefore, segsAfter []Seg, before, after []byte) []byte {
	dst = appendSegs(dst, segsBefore)
	dst = appendSegs(dst, segsAfter)
	for _, s := range segsBefore {
		dst = append(dst, before[s.Off:s.Off+s.Len]...)
	}
	for _, s := range segsAfter {
		dst = append(dst, after[s.Off:s.Off+s.Len]...)
	}
	return dst
}

// EncodePayloadRaw nhận các blob ĐÃ trích sẵn theo đoạn. Undo dùng nó: lúc
// sinh CLR, thứ trong tay là ảnh trước đã tách ra từ record cũ.
func EncodePayloadRaw(dst []byte, segsBefore, segsAfter []Seg, blobs ...[]byte) []byte {
	dst = appendSegs(dst, segsBefore)
	dst = appendSegs(dst, segsAfter)
	for _, b := range blobs {
		dst = append(dst, b...)
	}
	return dst
}

func appendSegs(dst []byte, segs []Seg) []byte {
	dst = binary.LittleEndian.AppendUint16(dst, uint16(len(segs)))
	for _, s := range segs {
		dst = binary.LittleEndian.AppendUint16(dst, s.Off)
		dst = binary.LittleEndian.AppendUint16(dst, s.Len)
	}
	return dst
}

func readSegs(p []byte, off int) ([]Seg, int, int, error) {
	if len(p) < off+2 {
		return nil, 0, 0, fmt.Errorf("%w: thiếu số đoạn", ErrBadPayload)
	}
	n := int(binary.LittleEndian.Uint16(p[off:]))
	off += 2
	if len(p) < off+4*n {
		return nil, 0, 0, fmt.Errorf("%w: thiếu bảng đoạn", ErrBadPayload)
	}
	segs := make([]Seg, n)
	total := 0
	for i := 0; i < n; i++ {
		segs[i] = Seg{
			Off: binary.LittleEndian.Uint16(p[off+4*i:]),
			Len: binary.LittleEndian.Uint16(p[off+4*i+2:]),
		}
		total += int(segs[i].Len)
	}
	return segs, off + 4*n, total, nil
}

// DecodePayload tách payload: hai danh sách đoạn + hai blob tương ứng.
func DecodePayload(payload []byte) (segsBefore, segsAfter []Seg, before, after []byte, err error) {
	sb, off, nb, err := readSegs(payload, 0)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	sa, off, na, err := readSegs(payload, off)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if len(payload) != off+nb+na {
		return nil, nil, nil, nil, fmt.Errorf("%w: cần %d byte dữ liệu, có %d", ErrBadPayload, nb+na, len(payload)-off)
	}
	before = payload[off : off+nb]
	after = payload[off+nb:]
	return sb, sa, before, after, nil
}

// Apply ghi blob (đã tách theo segs) vào page dst. Đây là hàm dùng chung cho
// redo (blob = after) và undo (blob = before) — cùng một phép toán, chỉ khác
// dữ liệu. Nhờ vậy undo không có đường code riêng để sai riêng.
func Apply(dst []byte, segs []Seg, blob []byte) error {
	pos := 0
	for _, s := range segs {
		if int(s.Off)+int(s.Len) > len(dst) {
			return fmt.Errorf("%w: đoạn [%d,%d) vượt page %d byte", ErrBadPayload, s.Off, int(s.Off)+int(s.Len), len(dst))
		}
		if pos+int(s.Len) > len(blob) {
			return fmt.Errorf("%w: blob thiếu byte cho đoạn tại %d", ErrBadPayload, s.Off)
		}
		copy(dst[s.Off:], blob[pos:pos+int(s.Len)])
		pos += int(s.Len)
	}
	return nil
}
