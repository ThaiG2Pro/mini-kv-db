package page

import "fmt"

// Slot array có thứ tự — dành cho B+Tree (phase 4).
//
// Phase 2 dựng slotted page cho **heap**: SlotID phải ổn định vĩnh viễn vì nó
// là một nửa của tuple id nằm trong index, nên xóa chỉ đánh dấu chết
// (tombstone) và chèn luôn nối vào cuối. Thứ tự slot vô nghĩa.
//
// B+Tree cần quy ước ngược lại: mảng slot **chính là thứ tự khóa**, binary
// search chạy trên nó. Không ai trỏ vào cell của B+Tree bằng SlotID (đường vào
// duy nhất là đi từ root xuống và so khóa), nên chỉ số dịch chuyển được — và
// bắt buộc phải dịch, vì một slot chết nằm giữa sẽ làm binary search sai.
//
// Cùng một layout page, hai access method, hai quy ước slot. Đó là lý do hai
// bộ hàm này nằm cạnh nhau chứ không thay thế nhau.

// InsertAt chèn record vào đúng vị trí i trong mảng slot, đẩy slot i..n-1 sang
// phải một ô. Tự compact nếu chỗ trống bị phân mảnh.
func (p Page) InsertAt(i SlotID, rec []byte) error {
	n := p.NumSlots()
	if int(i) > n {
		return fmt.Errorf("%w: chèn ở %d nhưng chỉ có %d slot", ErrBadSlot, i, n)
	}
	if len(rec) > MaxRecordSize {
		return fmt.Errorf("%w: %d > %d", ErrRecordTooLarge, len(rec), MaxRecordSize)
	}
	need := len(rec) + slotSize
	if need > p.FreeContiguous() {
		if need > p.FreeTotal() {
			return fmt.Errorf("%w: cần %d, liền mạch %d, tổng %d", ErrPageFull, need, p.FreeContiguous(), p.FreeTotal())
		}
		// Compact KHÔNG đụng mảng slot (chỉ đổi offset bên trong), nên vị trí
		// chèn i vừa tính bằng binary search vẫn còn đúng sau khi dồn.
		p.Compact()
		if need > p.FreeContiguous() {
			return fmt.Errorf("%w: compact xong vẫn thiếu, cần %d có %d", ErrPageFull, need, p.FreeContiguous())
		}
	}
	off := p.cellStart() - len(rec)
	copy(p[off:], rec)
	p.setU16(offNumSlots, uint16(n+1))
	for j := n; j > int(i); j-- {
		o, l := p.slot(j - 1)
		p.setSlot(j, o, l)
	}
	p.setSlot(int(i), off, len(rec))
	p.setU16(offCellStart, uint16(off))
	return nil
}

// RemoveAt xóa hẳn slot i khỏi mảng, kéo slot i+1..n-1 lùi một ô. Byte của
// cell thành frag cho tới lần compact — giống Delete, khác ở chỗ mảng slot co
// lại chứ không để lại tombstone.
func (p Page) RemoveAt(i SlotID) error {
	n := p.NumSlots()
	if int(i) >= n {
		return fmt.Errorf("%w: xóa %d nhưng chỉ có %d slot", ErrBadSlot, i, n)
	}
	off, ln := p.slot(int(i))
	if off == deadOffset {
		return fmt.Errorf("%w: id=%d", ErrSlotDead, i)
	}
	for j := int(i); j < n-1; j++ {
		o, l := p.slot(j + 1)
		p.setSlot(j, o, l)
	}
	p.setSlot(n-1, 0, 0)
	p.setU16(offNumSlots, uint16(n-1))
	p.setU16(offFrag, uint16(p.Frag()+ln))
	return nil
}

// SetAt thay nội dung slot i, giữ nguyên vị trí trong mảng. Khác Update ở chỗ
// nó không hứa gì về SlotID (B+Tree không cần) — chỉ là Update đổi tên cho
// đúng ngữ cảnh có thứ tự.
func (p Page) SetAt(i SlotID, rec []byte) error { return p.Update(i, rec) }

// Used là số byte page đang thực sự dùng: header + mảng slot + cell sống.
// Đây là thước đo cho bất biến "node không phải root phải đầy >= 50%".
func (p Page) Used() int {
	return HeaderSize + p.NumSlots()*slotSize + (PageSize - p.cellStart() - p.Frag())
}

// Link là 4 byte dự trữ trong header, ý nghĩa do access method quyết định:
//   - leaf B+Tree: PageID của leaf kế tiếp (sibling pointer cho range scan)
//   - branch B+Tree: PageID của con cực phải (con không có khóa phân tách)
//
// Nằm ở byte 12..16, khoảng trống có sẵn từ phase 2 giữa offReserved và
// offPageLSN — thêm một field vào header nghĩa là đổi format file, mà file
// phase 1-3 vẫn phải đọc được.
func (p Page) Link() uint32     { return p.u32(offLink) }
func (p Page) SetLink(v uint32) { p.setU32(offLink, v) }
