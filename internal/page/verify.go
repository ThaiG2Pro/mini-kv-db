package page

import (
	"fmt"
	"strings"
)

// Verify kiểm tra mọi bất biến của một slotted page. Đây là hàm mà fuzz test
// gọi sau *mỗi* thao tác — nếu nó im lặng thì page không thể tự mâu thuẫn.
//
// Bất biến:
//
//	I1  cellStart nằm trong [slotEnd, PageSize]  -> slot array và cell không chồng nhau
//	I2  numDead <= numSlots
//	I3  mọi cell sống nằm trong [cellStart, PageSize]
//	I4  không hai cell sống nào chồng lấn
//	I5  frag == (PageSize - cellStart) - tổng độ dài cell sống
//	I6  số slot có offset == deadOffset đúng bằng numDead
func (p Page) Verify() error {
	if len(p) != PageSize {
		return fmt.Errorf("%w: page dài %d byte, cần %d", ErrCorrupt, len(p), PageSize)
	}
	n := p.NumSlots()
	if n < 0 || HeaderSize+n*slotSize > PageSize {
		return fmt.Errorf("%w: numSlots=%d vô lý", ErrCorrupt, n)
	}
	cs, se := p.cellStart(), p.slotEnd()
	if cs < se || cs > PageSize {
		return fmt.Errorf("%w (I1): cellStart=%d ngoài [slotEnd=%d, %d]", ErrCorrupt, cs, se, PageSize)
	}
	if p.NumDead() > n {
		return fmt.Errorf("%w (I2): numDead=%d > numSlots=%d", ErrCorrupt, p.NumDead(), n)
	}

	// Phát hiện chồng lấn bằng bitmap 512 byte trên stack thay vì sort danh
	// sách cell: O(số byte sống) <= 4096, không cấp phát. Bản đầu dùng
	// sort.Slice tốn 130µs + 20KB mỗi lần gọi, và vì fuzz gọi Verify sau MỖI
	// thao tác, chính cái checker đó đã làm fuzzer đứng hình.
	var seen [PageSize / 8]byte
	dead, sum := 0, 0
	for i := 0; i < n; i++ {
		off, ln := p.slot(i)
		if off == deadOffset {
			dead++
			if ln != 0 {
				return fmt.Errorf("%w: slot %d chết nhưng len=%d", ErrCorrupt, i, ln)
			}
			continue
		}
		if off < cs || off+ln > PageSize {
			return fmt.Errorf("%w (I3): slot %d [%d,%d) ngoài vùng cell [%d,%d)", ErrCorrupt, i, off, off+ln, cs, PageSize)
		}
		for j := off; j < off+ln; j++ {
			if seen[j/8]&(1<<(j%8)) != 0 {
				return fmt.Errorf("%w (I4): byte %d thuộc hai cell, một trong đó là slot %d [%d,%d)",
					ErrCorrupt, j, i, off, off+ln)
			}
			seen[j/8] |= 1 << (j % 8)
		}
		sum += ln
	}
	if dead != p.NumDead() {
		return fmt.Errorf("%w (I6): đếm được %d slot chết, header ghi %d", ErrCorrupt, dead, p.NumDead())
	}
	if want := (PageSize - cs) - sum; want != p.Frag() {
		return fmt.Errorf("%w (I5): frag header=%d, tính lại=%d (vùng cell %d byte, sống %d byte)",
			ErrCorrupt, p.Frag(), want, PageSize-cs, sum)
	}
	return nil
}

// Dump vẽ page ra chữ để nhìn bằng mắt (cmd/slotlab dùng).
func (p Page) Dump(maxSlots int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "type=%d numSlots=%d (sống %d, chết %d)  cellStart=%d\n",
		p.Type(), p.NumSlots(), p.NumLive(), p.NumDead(), p.cellStart())
	fmt.Fprintf(&b, "header %d | slot array %d | trống liền mạch %d | frag %d | cell sống %d  (tổng %d)\n",
		HeaderSize, p.NumSlots()*slotSize, p.FreeContiguous(), p.Frag(),
		PageSize-p.cellStart()-p.Frag(), PageSize)
	fmt.Fprintf(&b, "trống: liền mạch %d, tổng %d  -> phân mảnh %.1f%%\n",
		p.FreeContiguous(), p.FreeTotal(), 100*float64(p.Frag())/float64(max(1, p.FreeTotal())))
	b.WriteString(p.Map(64))
	shown := p.NumSlots()
	if maxSlots > 0 && shown > maxSlots {
		shown = maxSlots
	}
	for i := 0; i < shown; i++ {
		off, ln := p.slot(i)
		if off == deadOffset {
			fmt.Fprintf(&b, "  slot %3d: CHẾT\n", i)
			continue
		}
		rec := p[off : off+ln]
		if len(rec) > 24 {
			rec = rec[:24]
		}
		fmt.Fprintf(&b, "  slot %3d: off=%4d len=%4d  %q\n", i, off, ln, rec)
	}
	if shown < p.NumSlots() {
		fmt.Fprintf(&b, "  ... còn %d slot\n", p.NumSlots()-shown)
	}
	return b.String()
}

// Map vẽ bản đồ page trên `width` ký tự: H header, s slot array, . trống,
// # cell sống, x byte chết (frag).
func (p Page) Map(width int) string {
	kind := make([]byte, PageSize)
	for i := range kind {
		switch {
		case i < HeaderSize:
			kind[i] = 'H'
		case i < p.slotEnd():
			kind[i] = 's'
		case i < p.cellStart():
			kind[i] = '.'
		default:
			kind[i] = 'x' // mặc định là rác, cell sống sẽ ghi đè
		}
	}
	for i := 0; i < p.NumSlots(); i++ {
		off, ln := p.slot(i)
		if off == deadOffset {
			continue
		}
		for j := off; j < off+ln && j < PageSize; j++ {
			kind[j] = '#'
		}
	}
	// Mỗi ký tự đại diện PageSize/width byte; lấy loại "nặng" nhất trong khối.
	step := PageSize / width
	rank := map[byte]int{'.': 0, 'x': 1, '#': 2, 's': 3, 'H': 4}
	out := make([]byte, 0, width+32)
	out = append(out, "[0"...)
	for i := 0; i < width; i++ {
		best := byte('.')
		for j := i * step; j < (i+1)*step; j++ {
			if rank[kind[j]] > rank[best] {
				best = kind[j]
			}
		}
		out = append(out, best)
	}
	out = append(out, "4096]\n"...)
	return string(out)
}
