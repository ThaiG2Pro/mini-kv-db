// Package btree hiện thực B+Tree trên buffer pool của phase 3: mỗi node là
// đúng một page 4KB, cell nằm trong slotted page của phase 2 với mảng slot
// giữ đúng thứ tự khóa.
package btree

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	"minidb/internal/page"
	"minidb/internal/pager"
)

var (
	ErrKeyNotFound   = errors.New("btree: không có khóa")
	ErrEntryTooLarge = errors.New("btree: entry lớn quá, một page không chứa nổi hai cái")
	ErrCorruptNode   = errors.New("btree: node hỏng")
	ErrEmptyKey      = errors.New("btree: khóa rỗng")
)

// MaxEntrySize là giới hạn cho (khóa + giá trị) của một entry.
//
// Không phải MaxRecordSize (vừa một page) mà là **nửa page**: một node phải
// chứa được ít nhất 2 cell, nếu không thì split một node đầy sẽ sinh ra node
// vẫn đầy, và insert lặp vô hạn. Đây là lý do mọi B+Tree thật đều có overflow
// page cho value lớn (SQLite), hoặc TOAST (Postgres).
const MaxEntrySize = (page.PageSize-page.HeaderSize)/2 - 8 /*slot + keyLen + dự phòng*/

// ---------- cell ----------
//
// leaf cell:   keyLen uint16 | key | value
// branch cell: child uint32  | keyLen uint16 | key
//
// Branch không chứa value: đó chính là chữ "+" trong B+Tree. Nhờ vậy fanout
// của branch chỉ phụ thuộc kích thước khóa, không phụ thuộc kích thước dữ
// liệu — cây thấp đi, và mọi lookup tốn đúng `height` lần I/O.

func encodeLeaf(buf []byte, key, val []byte) []byte {
	buf = buf[:0]
	buf = binary.LittleEndian.AppendUint16(buf, uint16(len(key)))
	buf = append(buf, key...)
	buf = append(buf, val...)
	return buf
}

func leafKey(cell []byte) []byte {
	n := int(binary.LittleEndian.Uint16(cell))
	return cell[2 : 2+n]
}

func leafVal(cell []byte) []byte {
	n := int(binary.LittleEndian.Uint16(cell))
	return cell[2+n:]
}

func encodeBranch(buf []byte, child pager.PageID, key []byte) []byte {
	buf = buf[:0]
	buf = binary.LittleEndian.AppendUint32(buf, uint32(child))
	buf = binary.LittleEndian.AppendUint16(buf, uint16(len(key)))
	buf = append(buf, key...)
	return buf
}

func branchChild(cell []byte) pager.PageID { return pager.PageID(binary.LittleEndian.Uint32(cell)) }
func branchKey(cell []byte) []byte         { return cell[6:] }

// ---------- node ----------

// node bọc một page đang được pin. Không giữ trạng thái nào ngoài page: mọi
// thứ nó biết đều đọc thẳng từ 4KB đó.
type node struct{ p page.Page }

func (n node) isLeaf() bool  { return n.p.Type() == page.TypeLeaf }
func (n node) numCells() int { return n.p.NumSlots() }

func (n node) cell(i int) []byte {
	c, err := n.p.Get(page.SlotID(i))
	if err != nil {
		panic(fmt.Sprintf("btree: cell %d: %v", i, err)) // slot chết không tồn tại trong B+Tree
	}
	return c
}

// keyAt trả khóa của cell i, dùng chung cho leaf và branch.
func (n node) keyAt(i int) []byte {
	if n.isLeaf() {
		return leafKey(n.cell(i))
	}
	return branchKey(n.cell(i))
}

// rightmost là con cực phải của một branch: cây con chứa mọi khóa >= khóa
// phân tách cuối cùng. Nó không có cell riêng vì không có cận trên.
func (n node) rightmost() pager.PageID      { return pager.PageID(n.p.Link()) }
func (n node) setRightmost(id pager.PageID) { n.p.SetLink(uint32(id)) }

// next/setNext là con trỏ sibling của leaf — thứ biến B+Tree thành cấu trúc
// quét dải được: đọc hết một leaf rồi nhảy ngang, không phải quay lên root.
func (n node) next() pager.PageID      { return pager.PageID(n.p.Link()) }
func (n node) setNext(id pager.PageID) { n.p.SetLink(uint32(id)) }

// search là binary search trong page — chỗ tiêu tốn CPU chính của mọi lookup.
// Trả về (i, exact) với i = số cell có khóa < target.
func (n node) search(target []byte) (int, bool) {
	lo, hi := 0, n.numCells()
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		switch bytes.Compare(n.keyAt(mid), target) {
		case 0:
			return mid, true
		case -1:
			lo = mid + 1
		default:
			hi = mid
		}
	}
	return lo, false
}

// childIndex trả vị trí con phải đi xuống khi tìm target.
//
// Quy ước: cell i = (khóa Kᵢ, con Cᵢ) với cây con Cᵢ chứa mọi khóa **< Kᵢ**;
// con cực phải chứa phần còn lại. Nên đích đến là Cᵢ đầu tiên có Kᵢ > target,
// tức là số cell có Kᵢ <= target. Chọn quy ước này (thay vì "Kᵢ là khóa nhỏ
// nhất của Cᵢ") vì nó khiến split chỉ phải sửa đúng một cell của cha.
func (n node) childIndex(target []byte) int {
	i, exact := n.search(target)
	if exact {
		i++
	}
	return i
}

// childAt trả con thứ i, với i == numCells nghĩa là con cực phải.
func (n node) childAt(i int) pager.PageID {
	if i == n.numCells() {
		return n.rightmost()
	}
	return branchChild(n.cell(i))
}

// setChildAt đổi con thứ i (i == numCells là con cực phải), giữ nguyên khóa.
func (n node) setChildAt(i int, id pager.PageID) error {
	if i == n.numCells() {
		n.setRightmost(id)
		return nil
	}
	cell := n.cell(i)
	binary.LittleEndian.PutUint32(cell, uint32(id)) // sửa tại chỗ, độ dài không đổi
	return nil
}

// insertBranchCell chèn (khóa, con) vào vị trí i của một branch.
func (n node) insertBranchCell(i int, child pager.PageID, key []byte) error {
	var buf [page.PageSize]byte
	return n.p.InsertAt(page.SlotID(i), encodeBranch(buf[:0], child, key))
}

// fillRatio là tỉ lệ đầy dùng cho bất biến "node không phải root >= 50%".
// Đo bằng BYTE chứ không phải số cell: với khóa biến độ dài, "một nửa số cell"
// không nói lên điều gì về chỗ trống thật.
func (n node) fillRatio() float64 {
	return float64(n.p.Used()) / float64(page.PageSize)
}
