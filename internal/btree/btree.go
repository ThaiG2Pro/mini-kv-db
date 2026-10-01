package btree

import (
	"errors"
	"fmt"

	"minidb/internal/bufpool"
	"minidb/internal/page"
	"minidb/internal/pager"
)

// Stats đếm các sự kiện cấu trúc. Splits là con số phase này muốn đo: nó là
// write amplification nhìn từ tầng access method.
type Stats struct {
	Inserts       int64
	Overwrites    int64
	Deletes       int64
	Splits        int64 // tổng số lần một node bị tách
	LeafSplits    int64
	RootSplits    int64 // = số lần cây cao thêm một tầng
	Merges        int64
	Redistributes int64
	Shrinks       int64 // số lần root tụt một tầng

	// SkippedRebalance: node thiếu nhưng không cân bằng được vì khóa phân
	// tách mới dài hơn khóa cũ và cha không còn chỗ. Cây vẫn đúng, chỉ tốn
	// chỗ — nhưng phải đếm, vì nó là ngoại lệ duy nhất của bất biến >= 50%.
	SkippedRebalance int64
}

// Tree là một B+Tree đặt trên buffer pool.
//
// Cây KHÔNG giữ page nào pin lâu dài: giữa hai lệnh, mọi page đều tự do để
// pool đuổi. Đó là điều kiện để một cây 1 triệu key chạy trong pool 64 frame.
type Tree struct {
	pool *bufpool.Pool
	root pager.PageID
	st   Stats

	// gen là SỐ ĐỜI CẤU TRÚC: tăng một lần cho mỗi thao tác có thể làm dịch
	// chuyển một entry khỏi chỗ nó đang nằm. Cursor chụp lấy nó để biết vị
	// trí (page, slot) mà mình đang giữ còn nghĩa hay không.
	//
	// Vì sao tăng ở MỌI Put/Delete chứ không chỉ ở split/merge: Put ghi đè
	// một khóa cũng có thể compact cả page (phase 2), và compact dồn lại mọi
	// offset — slot index không đổi nhưng khóa ở slot ấy thì đổi khi có ai
	// khác bị xóa. Đếm hẹp hơn là đúng ở nhiều ca và sai lặng lẽ ở vài ca,
	// tức là đúng cái loại bug mà phase 5 gọi là "hai nguồn sự thật".
	//
	// KHÔNG dùng atomic: mọi lối vào cây đều đã ở dưới d.mu của internal/db
	// (một writer, do code bắt buộc — P1-2). Ngày nào bỏ được ràng buộc ấy
	// thì đây là dòng đầu tiên phải đổi, nên nó được ghi ra thành chữ.
	gen uint64

	// RightmostSplit bật tối ưu chèn cực phải: khi khóa mới lớn hơn mọi khóa
	// đang có, tách 100/0 thay vì 50/50. Khóa auto-increment nhờ nó mà lấp
	// đầy page thay vì để lại một dãy page nửa rỗng. Tắt được để bench đo
	// đúng cái giá của nó.
	RightmostSplit bool

	// J nhận báo cáo mọi thay đổi page để tầng trên ghi WAL (phase 5).
	// Nil = không log gì — đúng hành vi phase 4, và là cách bench tách được
	// giá của WAL khỏi giá của cây.
	J Journal

	sc scratch

	// cellBuf là chỗ dựng cell trước khi chép vào page. Một buffer dùng lại
	// cho cả đường lan split: mỗi bậc chỉ giữ cell của mình tới khi InsertAt
	// (hoặc collect) đã chép nó đi.
	cellBuf [page.PageSize]byte
}

// Create dựng một cây rỗng: một leaf duy nhất, vừa là root vừa là leaf.
func Create(pool *bufpool.Pool) (*Tree, error) {
	f, err := pool.NewPage(page.TypeLeaf)
	if err != nil {
		return nil, err
	}
	id := f.PageID()
	if err := pool.Unpin(id, true); err != nil {
		return nil, err
	}
	return Open(pool, id), nil
}

// CreateLogged giống Create nhưng báo cáo qua journal: page gốc là một ALLOC
// và một lần đổi root, cả hai đều phải nằm trong log trước khi ai đó tin rằng
// database này tồn tại.
func CreateLogged(pool *bufpool.Pool, j Journal) (*Tree, error) {
	t := Open(pool, 0)
	t.J = j
	id, _, err := t.newPage(page.TypeLeaf)
	if err != nil {
		return nil, err
	}
	t.unpin(id, true)
	t.setRoot(id)
	return t, j.Err()
}

// Open mở cây đã có, root lấy từ meta page của pager.
func Open(pool *bufpool.Pool, root pager.PageID) *Tree {
	return &Tree{pool: pool, root: root, RightmostSplit: true, sc: newScratch()}
}

func (t *Tree) Root() pager.PageID { return t.root }

// Gen là số đời cấu trúc hiện tại — xem trường gen.
func (t *Tree) Gen() uint64  { return t.gen }
func (t *Tree) Stats() Stats { return t.st }
func (t *Tree) ResetStats()  { t.st = Stats{} }

// ---------- pin/unpin ----------

func (t *Tree) pin(id pager.PageID) (node, error) {
	f, err := t.pool.Pin(id)
	if err != nil {
		return node{}, fmt.Errorf("btree: pin page %d: %w", id, err)
	}
	n := node{p: f.Data}
	if typ := n.p.Type(); typ != page.TypeLeaf && typ != page.TypeBranch {
		t.pool.Unpin(id, false)
		return node{}, fmt.Errorf("%w: page %d kiểu %d không phải node B+Tree", ErrCorruptNode, id, typ)
	}
	if t.J != nil {
		t.J.PageIn(id, n.p)
	}
	return n, nil
}

func (t *Tree) unpin(id pager.PageID, dirty bool) {
	// Sinh log record TRƯỚC khi thả pin: sau khi thả, pool được phép đuổi page
	// đi bất cứ lúc nào, và nếu record chưa có thì đó đúng là vi phạm WAL rule
	// mà không ai bắt được (pageLSN vẫn là giá trị cũ nên FlushLog cho qua).
	if t.J != nil && t.J.PageOut(id, dirty) {
		// Journal so byte và thấy page thật sự đổi dù cây bảo không. Nó vừa
		// ghi log record cho thay đổi đó, nên pool BẮT BUỘC phải coi page là
		// bẩn — một page có log record mà pool tưởng sạch sẽ không bao giờ
		// được ghi xuống, và bản trên đĩa sẽ cũ hơn cả log lẫn RAM.
		dirty = true
	}
	if err := t.pool.Unpin(id, dirty); err != nil {
		// Unpin chỉ hỏng khi bookkeeping của chính cây sai; im lặng ở đây sẽ
		// biến một bug thành rò rỉ pin, và rò rỉ pin biểu hiện muộn hơn nhiều
		// dưới dạng "hết frame" ở một lệnh chẳng liên quan.
		panic(fmt.Sprintf("btree: unpin %d: %v", id, err))
	}
}

// crumb là một bậc trên đường đi từ root xuống leaf.
type crumb struct {
	id    pager.PageID
	n     node
	idx   int // vị trí con đã đi xuống (== numCells nghĩa là con cực phải)
	dirty bool
	gone  bool // node đã bị gộp/thu gọn và trả về freelist -> đừng Unpin nữa
}

// descend đi từ root xuống leaf chứa key, GIỮ PIN toàn bộ đường đi.
//
// Giữ pin cả đường vì split lan ngược từ dưới lên: lúc leaf tách, cha của nó
// phải còn đúng nội dung vừa đọc. Cái giá là pool phải có ít nhất height+2
// frame rảnh — với fanout ~200 thì height <= 4 cho 1 tỉ khóa, nên đây là ràng
// buộc rẻ. (Đường đi ngắn hơn — Get — thả pin ngay khi bước xuống.)
func (t *Tree) descend(key []byte) ([]crumb, error) {
	path := make([]crumb, 0, 8)
	id := t.root
	for {
		n, err := t.pin(id)
		if err != nil {
			t.release(path)
			return nil, err
		}
		if n.isLeaf() {
			path = append(path, crumb{id: id, n: n, idx: -1})
			return path, nil
		}
		i := n.childIndex(key)
		path = append(path, crumb{id: id, n: n, idx: i})
		id = n.childAt(i)
	}
}

func (t *Tree) release(path []crumb) {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i].gone {
			continue
		}
		t.unpin(path[i].id, path[i].dirty)
	}
}

// ---------- đọc ----------

// Get tìm key. Thả pin của mỗi tầng ngay khi đã lấy được con trỏ xuống tầng
// dưới: đường đọc không cần nhớ đường về.
func (t *Tree) Get(key []byte) ([]byte, error) {
	var out []byte
	err := t.GetFunc(key, func(v []byte) error {
		// Copy: cell trỏ thẳng vào arena của pool, hết pin là page có thể
		// bị đuổi và byte đó thành page khác (bất biến số 1 của phase 3).
		out = make([]byte, len(v))
		copy(out, v)
		return nil
	})
	return out, err
}

// GetFunc là Get không chép: fn nhận value trỏ THẲNG vào page, trong lúc page
// còn bị pin. v chỉ sống trong fn; giữ nó lâu hơn là đọc phải page khác sau
// khi pool đuổi page này. fn không được gọi lại vào cây (đang giữ pin, và ở
// tầng db là đang giữ latch).
//
// Có mặt vì nợ P6-2: một chuỗi version 60 bản là ~900 byte, và txn.Get chỉ
// cần đúng MỘT bản trong đó. Get chép cả 900 byte ra rồi mới đọc; trên máy đo,
// riêng lần cấp phát + chép ấy là 1/3 thời gian của Get ở depth=60.
func (t *Tree) GetFunc(key []byte, fn func(v []byte) error) error {
	if len(key) == 0 {
		return ErrEmptyKey
	}
	id := t.root
	for {
		n, err := t.pin(id)
		if err != nil {
			return err
		}
		if n.isLeaf() {
			i, exact := n.search(key)
			if !exact {
				t.unpin(id, false)
				return fmt.Errorf("%w: %q", ErrKeyNotFound, key)
			}
			err := fn(leafVal(n.cell(i)))
			t.unpin(id, false)
			return err
		}
		child := n.childAt(n.childIndex(key))
		t.unpin(id, false)
		id = child
	}
}

// Has kiểm tra tồn tại mà không copy value.
func (t *Tree) Has(key []byte) (bool, error) {
	_, err := t.Get(key)
	if errors.Is(err, ErrKeyNotFound) {
		return false, nil
	}
	return err == nil, err
}

// ---------- ghi ----------

// Put chèn hoặc ghi đè key.
func (t *Tree) Put(key, val []byte) error {
	if len(key) == 0 {
		return ErrEmptyKey
	}
	if n := 2 + len(key) + len(val); n > MaxEntrySize {
		return fmt.Errorf("%w: %d > %d", ErrEntryTooLarge, n, MaxEntrySize)
	}
	t.gen++
	path, err := t.descend(key)
	if err != nil {
		return err
	}
	defer func() { t.release(path) }()

	leaf := &path[len(path)-1]
	i, exact := leaf.n.search(key)

	cell := encodeLeaf(t.cellBuf[:0], key, val)

	if exact {
		// Ghi đè: thử sửa tại chỗ trước (rẻ nhất, không đụng mảng slot).
		if err := leaf.n.p.SetAt(page.SlotID(i), cell); err == nil {
			leaf.dirty = true
			t.st.Overwrites++
			// Ghi đè bằng value NGẮN HƠN cũng làm node co lại, y hệt xóa. Bỏ
			// qua vế này thì một chuỗi update thu nhỏ để lại các leaf gần rỗng
			// mà không lần Delete nào chạm tới — cây vẫn đúng nhưng file phình
			// mãi không co. Fuzz bắt được đúng ca này: 1 split + 2 overwrite +
			// 1 delete cho ra leaf 45% mà merges=0.
			if underfull(leaf.n) {
				return t.rebalance(path)
			}
			return nil
		}
		// Không vừa -> xóa rồi chèn lại như một entry mới. Có thể dẫn tới
		// split, dù số khóa không tăng: độ dài value mới là thứ làm page đầy.
		if err := leaf.n.p.RemoveAt(page.SlotID(i)); err != nil {
			return err
		}
		leaf.dirty = true
		t.st.Overwrites++
	} else {
		t.st.Inserts++
	}
	return t.insertAndSplit(path, len(path)-1, i, cell)
}

// insertAndSplit chèn cell vào node ở bậc `lv`, và nếu đầy thì tách node rồi
// lan lên trên. Đây là toàn bộ cơ chế giữ cây cân bằng: chiều cao chỉ đổi ở
// một chỗ duy nhất — khi root tách.
func (t *Tree) insertAndSplit(path []crumb, lv int, i int, cell []byte) error {
	for {
		c := &path[lv]
		err := c.n.p.InsertAt(page.SlotID(i), cell)
		if err == nil {
			c.dirty = true
			return nil
		}
		if !errors.Is(err, page.ErrPageFull) {
			return err
		}

		// Node đầy -> tách. sep là khóa phân tách đẩy lên cha, right là nửa
		// phải vừa sinh.
		sep, right, err := t.split(c, i, cell)
		if err != nil {
			return err
		}
		c.dirty = true
		t.st.Splits++

		if lv == 0 {
			return t.growRoot(c.id, sep, right)
		}
		// Cha đang trỏ tới c.id ở vị trí j với nghĩa "cây con này < K_j".
		// Sau khi tách, nửa PHẢI mới là cái giữ khoảng tới K_j, còn nửa trái
		// (c.id) lấy cận trên mới là sep. Nên: con ở j đổi thành right, rồi
		// chèn (sep, c.id) vào đúng vị trí j. Cách này đúng cho cả trường hợp
		// j == numCells (con cực phải) mà không cần nhánh riêng.
		parent := &path[lv-1]
		j := parent.idx
		leftID := c.id
		if err := parent.n.setChildAt(j, right); err != nil {
			return err
		}
		parent.dirty = true

		// sep nằm trong scratch, chỉ sống tới lần split kế tiếp — chép nó vào
		// cell của cha ngay tại đây là đủ, vì vòng sau chỉ dùng `cell`.
		cell = encodeBranch(t.cellBuf[:0], leftID, sep)
		i, lv = j, lv-1
	}
}

// growRoot dựng root mới khi root cũ tách. Root cũ (leftID) thành con trái,
// right thành con cực phải. Đây là **chỗ duy nhất** cây cao thêm, và vì cả hai
// nửa đều đã tồn tại đầy đủ nên mọi leaf vẫn cách root đúng bằng nhau.
//
// Root đổi PageID (thay vì copy nội dung xuống một page con để giữ nguyên id
// như SQLite làm). Đơn giản hơn, đổi lại root phải được ghi vào meta page ở
// Commit — nên Tree.Root() là thứ tầng trên bắt buộc phải lưu.
func (t *Tree) growRoot(leftID pager.PageID, sep []byte, right pager.PageID) error {
	id, n, err := t.newPage(page.TypeBranch)
	if err != nil {
		return err
	}
	n.setRightmost(right)
	if err := n.p.InsertAt(0, encodeBranch(t.cellBuf[:0], leftID, sep)); err != nil {
		t.unpin(id, true)
		return err
	}
	t.unpin(id, true)
	t.setRoot(id)
	t.st.RootSplits++
	return nil
}
