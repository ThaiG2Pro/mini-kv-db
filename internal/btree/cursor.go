package btree

import "minidb/internal/pager"

// Cursor duyệt cây theo thứ tự khóa tăng dần. Nó là nền của mọi `WHERE x > ?`
// và của mọi iterator ở tầng trên.
//
// Cursor KHÔNG giữ pin giữa hai lần Next(): mỗi bước nó pin lại leaf, chép
// entry ra rồi thả. Đắt hơn (một lần tra bảng của pool cho mỗi khóa thay vì
// cho mỗi leaf) nhưng đổi lại không ai có thể quên đóng cursor và làm rò rỉ
// một frame vĩnh viễn — với pool 64 frame thì vài cursor bỏ quên là chết cả
// hệ thống. Giá của lựa chọn này đo được ở BenchmarkScan.
type Cursor struct {
	t    *Tree
	leaf pager.PageID
	idx  int
	key  []byte
	val  []byte
	ok   bool
	err  error
}

// Seek đặt cursor ở entry đầu tiên có khóa >= key. Đây là *lower bound*, đúng
// ngữ nghĩa của một index range scan.
func (t *Tree) Seek(key []byte) *Cursor {
	c := &Cursor{t: t}
	id := t.root
	for {
		n, err := t.pin(id)
		if err != nil {
			c.err = err
			return c
		}
		if n.isLeaf() {
			i, _ := n.search(key)
			c.leaf, c.idx = id, i
			t.unpin(id, false)
			c.load()
			return c
		}
		child := n.childAt(n.childIndex(key))
		t.unpin(id, false)
		id = child
	}
}

// First đặt cursor ở khóa nhỏ nhất — đi dọc mép trái của cây.
func (t *Tree) First() *Cursor {
	c := &Cursor{t: t}
	id := t.root
	for {
		n, err := t.pin(id)
		if err != nil {
			c.err = err
			return c
		}
		if n.isLeaf() {
			c.leaf, c.idx = id, 0
			t.unpin(id, false)
			c.load()
			return c
		}
		child := n.childAt(0)
		t.unpin(id, false)
		id = child
	}
}

// load nạp entry hiện tại, tự nhảy sang leaf kế nếu đã hết leaf này.
//
// Vòng lặp (chứ không phải một lần nhảy) vì một leaf rỗng vẫn hợp lệ ngay sau
// khi xóa hết khóa của nó mà chưa kịp merge.
func (c *Cursor) load() {
	for {
		// PageID 0 là meta page A của pager, không bao giờ là node -> dùng
		// làm dấu "không còn leaf nào nữa".
		if c.leaf == 0 {
			c.ok = false
			return
		}
		n, err := c.t.pin(c.leaf)
		if err != nil {
			c.err, c.ok = err, false
			return
		}
		if c.idx < n.numCells() {
			cell := n.cell(c.idx)
			k, v := leafKey(cell), leafVal(cell)
			// Chép: hết pin là byte này có thể thành page khác.
			c.key = append(c.key[:0], k...)
			c.val = append(c.val[:0], v...)
			c.ok = true
			c.t.unpin(c.leaf, false)
			return
		}
		next := n.next()
		c.t.unpin(c.leaf, false)
		c.leaf, c.idx = next, 0
	}
}

// Next bước sang entry kế. Trả false khi hết.
func (c *Cursor) Next() bool {
	if !c.ok || c.err != nil {
		return false
	}
	c.idx++
	c.load()
	return c.ok
}

// Valid cho biết cursor đang đứng trên một entry.
func (c *Cursor) Valid() bool   { return c.ok && c.err == nil }
func (c *Cursor) Key() []byte   { return c.key }
func (c *Cursor) Value() []byte { return c.val }
func (c *Cursor) Err() error    { return c.err }

// Range gọi fn cho mọi khóa trong [lo, hi). hi == nil nghĩa là tới hết.
func (t *Tree) Range(lo, hi []byte, fn func(key, val []byte) bool) error {
	c := t.Seek(lo)
	for c.Valid() {
		if hi != nil && string(c.Key()) >= string(hi) {
			break
		}
		if !fn(c.Key(), c.Value()) {
			break
		}
		c.Next()
	}
	return c.Err()
}

// Count đếm toàn bộ khóa bằng cách đi ngang tầng lá — không đụng branch lần
// nào. Chính là thứ mà B-Tree (data ở mọi tầng) không làm được.
func (t *Tree) Count() (int, error) {
	n := 0
	c := t.First()
	for c.Valid() {
		n++
		c.Next()
	}
	return n, c.Err()
}
