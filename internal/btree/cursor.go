package btree

import (
	"bytes"

	"minidb/internal/pager"
)

// Cursor duyệt cây theo thứ tự khóa tăng dần. Nó là nền của mọi `WHERE x > ?`
// và của mọi iterator ở tầng trên.
//
// Cursor KHÔNG giữ pin giữa hai lần Next(): mỗi bước nó pin lại leaf, chép
// entry ra rồi thả. Đắt hơn (một lần tra bảng của pool cho mỗi khóa thay vì
// cho mỗi leaf) nhưng đổi lại không ai có thể quên đóng cursor và làm rò rỉ
// một frame vĩnh viễn — với pool 64 frame thì vài cursor bỏ quên là chết cả
// hệ thống. Giá của lựa chọn này đo được ở BenchmarkScan.
//
// Phase 7 thêm một thứ: cursor mang theo `gen`, số đời cấu trúc của cây lúc
// nó đứng vào chỗ hiện tại. Nhờ nó, người gọi được phép NHẢ latch giữa hai
// bước Next() — cái mà phase 4 không cho phép. Xem restore().
type Cursor struct {
	t    *Tree
	leaf pager.PageID
	idx  int
	key  []byte
	val  []byte
	ok   bool
	err  error

	// gen là Tree.gen lúc (leaf, idx) được xác lập. Cây đổi cấu trúc thì cặp
	// ấy hết nghĩa: page có thể đã bị split, bị merge, hoặc bị trả về
	// freelist rồi cấp lại cho một node KHÁC HẲN. Không có con số này thì
	// cursor sẽ đọc rác một cách hoàn toàn im lặng.
	gen uint64

	// restores đếm số lần phải tìm lại chỗ. Nó là số đo của phase 7: nếu nó
	// bằng 0 trong một lab có writer chạy song song thì cơ chế này chưa bao
	// giờ được thử, và bài test không chứng minh gì cả.
	restores int
}

// Seek đặt cursor ở entry đầu tiên có khóa >= key. Đây là *lower bound*, đúng
// ngữ nghĩa của một index range scan.
func (t *Tree) Seek(key []byte) *Cursor {
	c := &Cursor{t: t}
	c.seek(key)
	return c
}

// First đặt cursor ở khóa nhỏ nhất — đi dọc mép trái của cây.
func (t *Tree) First() *Cursor {
	c := &Cursor{t: t}
	c.seek(nil)
	return c
}

// seek đi từ root xuống leaf chứa key (key == nil: mép trái).
//
// Nó ghi lại t.gen TRƯỚC khi đi xuống, không phải sau. Ghi sau là một cửa sổ
// đua: cây đổi trong lúc ta đang xuống, ta lưu số đời MỚI cho một vị trí tìm
// được theo hình CŨ, và lần Next() kế sẽ tin vào một vị trí đã hết nghĩa.
func (c *Cursor) seek(key []byte) {
	c.gen = c.t.gen
	id := c.t.root
	for {
		n, err := c.t.pin(id)
		if err != nil {
			c.err, c.ok = err, false
			return
		}
		if n.isLeaf() {
			i := 0
			if key != nil {
				i, _ = n.search(key)
			}
			c.leaf, c.idx = id, i
			c.t.unpin(id, false)
			c.load()
			return
		}
		child := n.childAt(0)
		if key != nil {
			child = n.childAt(n.childIndex(key))
		}
		c.t.unpin(id, false)
		id = child
	}
}

// restore tìm lại chỗ sau khi cây đã đổi cấu trúc: đi lại từ root tới khóa
// đầu tiên LỚN HƠN khóa vừa trả về.
//
// Đây là cách InnoDB/SQLite khôi phục vị trí cursor sau split, và nó rẻ hơn
// latch-coupling một bậc về độ phức tạp: không cần latch trên page, chỉ cần
// biết "cây đã đổi" và biết mình đang ở đâu theo KHÓA (một giá trị logic,
// không phụ thuộc hình cây) thay vì theo (page, slot).
//
// Cái giá phải nói thẳng: giữa hai bước Next() có thể có writer chen vào, nên
// một lần duyệt KHÔNG còn là ảnh chụp một thời điểm của cây. Ở tầng txn điều
// đó vô hại vì thứ quyết định nhìn thấy gì là snapshot MVCC của phase 6, chứ
// không phải cái latch của phase 4. Ở tầng btree thuần thì nó là hành vi
// "read committed" — ghi vào doc của Tree.Range.
func (c *Cursor) restore() {
	last := append([]byte(nil), c.key...)
	c.restores++
	c.seek(last)
	// seek cho lower bound; khóa cũ có thể còn đó (thì phải bỏ qua) hoặc đã
	// bị xóa (thì ta đã ở sau nó). Vòng lặp xử lý cả trùng khóa lặp lại.
	for c.ok && c.err == nil && bytes.Compare(c.key, last) <= 0 {
		c.idx++
		c.load()
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
	if c.gen != c.t.gen {
		c.restore()
		return c.ok && c.err == nil
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

// Restores là số lần cursor phải tìm lại chỗ vì cây đổi cấu trúc dưới chân nó.
func (c *Cursor) Restores() int { return c.restores }

// Range gọi fn cho mọi khóa trong [lo, hi). hi == nil nghĩa là tới hết.
//
// Ngữ nghĩa: KHÔNG phải ảnh chụp. Nếu có writer chạy song song (và người gọi
// nhả latch giữa hai bước — xem db.Iter) thì lần duyệt có thể thấy cả cái ghi
// sau khi nó bắt đầu. Muốn ảnh chụp thì phải hỏi tầng có version: txn.Txn.Scan.
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
