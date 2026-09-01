package btree

import (
	"fmt"

	"minidb/internal/page"
	"minidb/internal/pager"
)

// MemDB là một Store + Allocator sống trong RAM.
//
// Vì sao cần nó khi đã có pager thật: phase này đo CẤU TRÚC cây (số split, độ
// đầy leaf, số page chạm tới), không đo ổ đĩa. Đo trên file thật thì mọi con
// số đều bị page cache và fsync của phase 0 trộn vào, không tách được phần
// nào là do B+Tree. Bù lại nó đếm Reads/Writes nên vẫn nói được "một Get chạm
// mấy page" — đúng thứ phase 4 muốn chứng minh.
//
// Page 0 và 1 chiếm chỗ sẵn để bắt chước hai meta page của pager: nhờ vậy
// Allocate không bao giờ trả về 0, và cursor được phép dùng PageID 0 làm dấu
// hết chuỗi leaf.
type MemDB struct {
	pages  [][]byte
	free   []pager.PageID
	Reads  int64
	Writes int64
	Allocs int64
	Frees  int64
}

func NewMemDB() *MemDB {
	return &MemDB{pages: [][]byte{make([]byte, page.PageSize), make([]byte, page.PageSize)}}
}

func (m *MemDB) ReadPage(id pager.PageID, buf []byte) error {
	if int(id) >= len(m.pages) {
		return fmt.Errorf("memdb: đọc page %d quá EOF (%d page)", id, len(m.pages))
	}
	copy(buf, m.pages[id])
	m.Reads++
	return nil
}

func (m *MemDB) WritePage(id pager.PageID, buf []byte) error {
	if int(id) >= len(m.pages) {
		return fmt.Errorf("memdb: ghi page %d quá EOF (%d page)", id, len(m.pages))
	}
	copy(m.pages[id], buf)
	m.Writes++
	return nil
}

func (m *MemDB) Allocate() (pager.PageID, error) {
	m.Allocs++
	if n := len(m.free); n > 0 {
		id := m.free[n-1]
		m.free = m.free[:n-1]
		return id, nil
	}
	m.pages = append(m.pages, make([]byte, page.PageSize))
	return pager.PageID(len(m.pages) - 1), nil
}

func (m *MemDB) Free(id pager.PageID) error {
	for _, f := range m.free {
		if f == id {
			return fmt.Errorf("memdb: double free page %d", id)
		}
	}
	m.Frees++
	m.free = append(m.free, id)
	return nil
}

// NumPages là số page đã cấp phát (kể cả đang nằm trong freelist).
func (m *MemDB) NumPages() int { return len(m.pages) }

// LivePages là số page đang thật sự được cây dùng.
func (m *MemDB) LivePages() int { return len(m.pages) - 2 - len(m.free) }
