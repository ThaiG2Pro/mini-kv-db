// Package page cài slotted page: chứa record biến độ dài trong một page cố định.
//
// Layout (PageSize = 4096):
//
//	0                                                              4096
//	+--------+------------------+--------------+--------------------+
//	| header | slot0 slot1 ...  |  ...trống... | ... cell1  cell0   |
//	+--------+------------------+--------------+--------------------+
//	         ^ mọc sang phải                    ^ cellStart, mọc sang trái
//
// Hai điều quan trọng của thiết kế này, và cũng là lý do nó tồn tại trong mọi
// DB dùng page (Postgres, SQLite, InnoDB):
//
//  1. Record được trỏ tới **gián tiếp** qua slot. Compact dồn cell lại làm đổi
//     offset, nhưng chỉ số slot thì không đổi -> con trỏ từ bên ngoài
//     (tuple id = (PageID, SlotID), ví dụ từ secondary index) vẫn đúng.
//  2. Xóa không rút mảng slot lại, chỉ đánh dấu slot chết. Rút lại sẽ làm mọi
//     SlotID phía sau tụt đi một -> hỏng toàn bộ con trỏ ngoài.
package page

import (
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
)

// PageSize trùng pager.PageSize. Không import pager để package này không phụ
// thuộc tầng dưới (và để test page chạy độc lập).
const PageSize = 4096

// Layout header (little-endian).
const (
	offNumSlots  = 0  // uint16 tổng số slot, kể cả slot chết
	offNumDead   = 2  // uint16 số slot chết
	offCellStart = 4  // uint16 mép trái của vùng cell; vùng [cellStart,PageSize) là cell
	offFrag      = 6  // uint16 số byte chết nằm lẫn trong vùng cell
	offType      = 8  // uint8
	offFlags     = 9  // uint8  (dự trữ)
	offReserved  = 10 // uint16 (dự trữ)
	offPageLSN   = 16 // uint64 móc sẵn cho WAL ở phase 5 (flushedLSN >= pageLSN)

	// HeaderSize 24 byte, chừa chỗ cho pageLSN của phase 5.
	HeaderSize = 24

	// slotSize: mỗi slot = offset uint16 + length uint16.
	slotSize = 4

	// deadOffset: offset 0 không thể là cell hợp lệ (header chiếm chỗ đó),
	// nên dùng làm dấu "slot đã chết". Nhờ vậy record rỗng vẫn hợp lệ.
	deadOffset = 0
)

// Loại page — mới dùng ở phase 2, sẽ có thêm leaf/internal ở phase 4.
const (
	TypeHeap   uint8 = 1
	TypeLeaf   uint8 = 2
	TypeBranch uint8 = 3
)

// MaxRecordSize: record lớn nhất nhét vừa một page trống (đã trừ slot của nó).
const MaxRecordSize = PageSize - HeaderSize - slotSize

var (
	ErrPageFull       = errors.New("page: hết chỗ")
	ErrNeedCompact    = errors.New("page: đủ chỗ nhưng bị phân mảnh, cần compact")
	ErrBadSlot        = errors.New("page: slot id không tồn tại")
	ErrSlotDead       = errors.New("page: slot đã bị xóa")
	ErrRecordTooLarge = errors.New("page: record lớn hơn một page")
	ErrCorrupt        = errors.New("page: page hỏng")
)

// SlotID là chỉ số trong mảng slot. Cùng PageID nó tạo thành tuple id.
type SlotID uint16

// Page là một page thô. Kiểu này cố ý là []byte chứ không phải struct: nó bọc
// thẳng buffer của pager, không copy, không giữ trạng thái nào ngoài đĩa.
type Page []byte

// ---------- header ----------

func (p Page) u16(off int) uint16       { return binary.LittleEndian.Uint16(p[off:]) }
func (p Page) setU16(off int, v uint16) { binary.LittleEndian.PutUint16(p[off:], v) }

func (p Page) NumSlots() int  { return int(p.u16(offNumSlots)) }
func (p Page) NumDead() int   { return int(p.u16(offNumDead)) }
func (p Page) NumLive() int   { return p.NumSlots() - p.NumDead() }
func (p Page) cellStart() int { return int(p.u16(offCellStart)) }
func (p Page) Frag() int      { return int(p.u16(offFrag)) }
func (p Page) Type() uint8    { return p[offType] }

func (p Page) LSN() uint64     { return binary.LittleEndian.Uint64(p[offPageLSN:]) }
func (p Page) SetLSN(v uint64) { binary.LittleEndian.PutUint64(p[offPageLSN:], v) }

// slotEnd là mép phải của mảng slot = chỗ đặt slot tiếp theo.
func (p Page) slotEnd() int { return HeaderSize + p.NumSlots()*slotSize }

// FreeContiguous là khoảng trống liền mạch ở giữa — chỗ duy nhất ghi được ngay.
func (p Page) FreeContiguous() int { return p.cellStart() - p.slotEnd() }

// FreeTotal gồm cả byte chết kẹt trong vùng cell; chỉ dùng được sau compact.
func (p Page) FreeTotal() int { return p.FreeContiguous() + p.Frag() }

func (p Page) slot(i int) (off, ln int) {
	base := HeaderSize + i*slotSize
	return int(p.u16(base)), int(p.u16(base + 2))
}

func (p Page) setSlot(i, off, ln int) {
	base := HeaderSize + i*slotSize
	p.setU16(base, uint16(off))
	p.setU16(base+2, uint16(ln))
}

// ---------- vòng đời ----------

// Init dọn page về trạng thái rỗng.
func Init(p Page, typ uint8) {
	if len(p) != PageSize {
		panic("page: buffer phải đúng PageSize")
	}
	for i := range p {
		p[i] = 0
	}
	p.setU16(offCellStart, PageSize)
	p[offType] = typ
}

// New cấp một page rỗng mới (tiện cho test; đường chính là bọc buffer pager).
func New(typ uint8) Page {
	p := make(Page, PageSize)
	Init(p, typ)
	return p
}

// ---------- đọc ----------

// Get trả về view **trỏ thẳng vào buffer** của record. Không copy: caller phải
// tự copy nếu muốn giữ sau khi page bị sửa hay bị evict (phase 3).
func (p Page) Get(id SlotID) ([]byte, error) {
	if int(id) >= p.NumSlots() {
		return nil, fmt.Errorf("%w: id=%d numSlots=%d", ErrBadSlot, id, p.NumSlots())
	}
	off, ln := p.slot(int(id))
	if off == deadOffset {
		return nil, fmt.Errorf("%w: id=%d", ErrSlotDead, id)
	}
	if off < p.slotEnd() || off+ln > PageSize {
		return nil, fmt.Errorf("%w: slot %d trỏ ra ngoài (off=%d len=%d)", ErrCorrupt, id, off, ln)
	}
	return p[off : off+ln], nil
}

// Alive cho biết slot có record sống không.
func (p Page) Alive(id SlotID) bool {
	if int(id) >= p.NumSlots() {
		return false
	}
	off, _ := p.slot(int(id))
	return off != deadOffset
}

// ---------- ghi ----------

// Insert thêm record, tự compact nếu chỗ trống bị phân mảnh.
func (p Page) Insert(rec []byte) (SlotID, error) {
	id, err := p.InsertNoCompact(rec)
	if errors.Is(err, ErrNeedCompact) {
		p.Compact()
		return p.InsertNoCompact(rec)
	}
	return id, err
}

// InsertNoCompact thêm record mà không dồn page: trả ErrNeedCompact nếu tổng
// chỗ trống đủ nhưng phần liền mạch thì không. Tách ra để đo được giá compact.
//
// Slot mới luôn nối vào **cuối** mảng: không tái dùng slot chết. Tái dùng sẽ
// làm một tuple id cũ (có thể còn nằm trong secondary index) bỗng trỏ sang
// record khác — đúng cái mà Postgres phải chạy VACUUM mới dám làm.
func (p Page) InsertNoCompact(rec []byte) (SlotID, error) {
	if len(rec) > MaxRecordSize {
		return 0, fmt.Errorf("%w: %d > %d", ErrRecordTooLarge, len(rec), MaxRecordSize)
	}
	need := len(rec) + slotSize
	if need > p.FreeContiguous() {
		if need <= p.FreeTotal() {
			return 0, ErrNeedCompact
		}
		return 0, fmt.Errorf("%w: cần %d, liền mạch %d, tổng %d", ErrPageFull, need, p.FreeContiguous(), p.FreeTotal())
	}
	off := p.cellStart() - len(rec)
	copy(p[off:], rec)
	id := SlotID(p.NumSlots())
	p.setU16(offNumSlots, uint16(p.NumSlots()+1))
	p.setSlot(int(id), off, len(rec))
	p.setU16(offCellStart, uint16(off))
	return id, nil
}

// Delete đánh dấu slot chết. Byte của cell thành rác (frag) cho tới lần compact.
func (p Page) Delete(id SlotID) error {
	if int(id) >= p.NumSlots() {
		return fmt.Errorf("%w: id=%d", ErrBadSlot, id)
	}
	off, ln := p.slot(int(id))
	if off == deadOffset {
		return fmt.Errorf("%w: id=%d", ErrSlotDead, id)
	}
	p.setSlot(int(id), deadOffset, 0)
	p.setU16(offNumDead, uint16(p.NumDead()+1))
	p.setU16(offFrag, uint16(p.Frag()+ln))
	return nil
}

// Update ghi đè record, **giữ nguyên tuple id**.
//   - ngắn hơn hoặc bằng: ghi tại chỗ, phần dư thành frag.
//   - dài hơn: cấp cell mới ở vùng trống, cell cũ thành frag, slot trỏ chỗ mới.
//
// Không đủ chỗ -> ErrPageFull, và lúc đó tầng trên phải quyết định: dời record
// sang page khác (forwarding pointer của Oracle) hay tạo phiên bản mới ở page
// khác (Postgres). Phase 2 chưa làm, chỉ trả lỗi.
func (p Page) Update(id SlotID, rec []byte) error {
	if int(id) >= p.NumSlots() {
		return fmt.Errorf("%w: id=%d", ErrBadSlot, id)
	}
	off, ln := p.slot(int(id))
	if off == deadOffset {
		return fmt.Errorf("%w: id=%d", ErrSlotDead, id)
	}
	if len(rec) > MaxRecordSize {
		return fmt.Errorf("%w: %d > %d", ErrRecordTooLarge, len(rec), MaxRecordSize)
	}
	if len(rec) <= ln {
		copy(p[off:], rec)
		p.setSlot(int(id), off, len(rec))
		p.setU16(offFrag, uint16(p.Frag()+ln-len(rec)))
		return nil
	}
	// Cần cell mới. Cell cũ trở thành rác ngay, nên tính nó vào chỗ trống.
	if len(rec) > p.FreeContiguous() {
		if len(rec) <= p.FreeTotal() {
			// Đánh dấu cell cũ chết trước rồi compact, tránh copy vô ích.
			p.setSlot(int(id), deadOffset, 0)
			p.setU16(offFrag, uint16(p.Frag()+ln))
			p.Compact()
			if len(rec) > p.FreeContiguous() {
				return fmt.Errorf("%w: update %d byte", ErrPageFull, len(rec))
			}
			noff := p.cellStart() - len(rec)
			copy(p[noff:], rec)
			p.setSlot(int(id), noff, len(rec))
			p.setU16(offCellStart, uint16(noff))
			return nil
		}
		return fmt.Errorf("%w: update cần %d, tổng trống %d", ErrPageFull, len(rec), p.FreeTotal()+ln)
	}
	noff := p.cellStart() - len(rec)
	copy(p[noff:], rec)
	p.setSlot(int(id), noff, len(rec))
	p.setU16(offCellStart, uint16(noff))
	p.setU16(offFrag, uint16(p.Frag()+ln))
	return nil
}

// Compact dồn mọi cell sống về sát mép phải, xóa sạch frag.
// **Không** đụng tới mảng slot: SlotID giữ nguyên, chỉ offset bên trong đổi.
// Đây chính là câu trả lời cho "vì sao cần slot indirection".
func (p Page) Compact() {
	n := p.NumSlots()
	// Duyệt cell từ phải sang trái rồi chép dồn về phải: vùng đích luôn nằm
	// bên phải vùng nguồn, nên copy đè lên nhau vẫn đúng.
	//
	// Cần thứ tự offset giảm dần. Thứ tự slot KHÔNG cho sẵn điều đó: Update
	// cấp cell mới ở mép trái mà giữ nguyên slot, nên chỉ vài lần update là
	// hai thứ tự lệch hẳn nhau (xem TestCompactOrderIsScrambled). Vì thế phải
	// sort thật, O(n log n) — insertion sort ở đây từng làm fuzzer đứng hình.
	//
	// numSlots tối đa (PageSize-HeaderSize)/slotSize = 1018 nên mảng nằm trên
	// stack: Compact không cấp phát heap lần nào.
	var buf [(PageSize - HeaderSize) / slotSize]uint32
	order := buf[:0]
	for i := 0; i < n; i++ {
		if off, _ := p.slot(i); off != deadOffset {
			// gói (offset, slot) vào một uint32 để sort chỉ so số nguyên
			order = append(order, uint32(off)<<16|uint32(i))
		}
	}
	slices.SortFunc(order, func(a, b uint32) int { return int(b>>16) - int(a>>16) })

	dst := PageSize
	for _, v := range order {
		i := int(v & 0xffff)
		off, ln := p.slot(i)
		dst -= ln
		if dst != off {
			copy(p[dst:dst+ln], p[off:off+ln])
			p.setSlot(i, dst, ln)
		}
	}
	p.setU16(offCellStart, uint16(dst))
	p.setU16(offFrag, 0)
}

// TrimDeadSlots cắt các slot chết ở **đuôi** mảng slot, thu lại 4 byte mỗi cái.
//
// Chỉ đuôi mới cắt được, và nó an toàn: một con trỏ ngoài trỏ vào slot đã cắt
// sẽ rơi vào nhánh "id >= numSlots" -> vẫn báo là không còn record, đúng bằng
// câu trả lời trước khi cắt. Cắt slot chết ở **giữa** thì không: nó sẽ làm mọi
// SlotID phía sau tụt đi. Postgres làm y hệt khi prune page.
func (p Page) TrimDeadSlots() int {
	n := p.NumSlots()
	trimmed := 0
	for n > 0 {
		off, _ := p.slot(n - 1)
		if off != deadOffset {
			break
		}
		p.setSlot(n-1, 0, 0)
		n--
		trimmed++
	}
	p.setU16(offNumSlots, uint16(n))
	p.setU16(offNumDead, uint16(p.NumDead()-trimmed))
	return trimmed
}
