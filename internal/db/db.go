// Package db nối bốn tầng dưới lại thành một database có transaction:
// pager (file) + bufpool (đệm) + wal (log) + btree (access method).
//
// Đây là chỗ phase 5 trả lời câu hỏi mà bốn phase trước cố tình để ngỏ: một
// lệnh ghi "xong" nghĩa là gì. Câu trả lời của WAL — và của Postgres, InnoDB,
// SQLite ở chế độ WAL — là:
//
//	commit = log record của transaction đã fsync. Hết.
//
// Page dữ liệu có thể còn nằm nguyên trong RAM hàng phút sau đó. Đổi lại, mở
// file mà không chạy recovery là đọc phải một cái cây dở dang: từ phase 5,
// Open() và Recover() là một, không tách ra được.
package db

import (
	"bytes"
	"errors"
	"fmt"
	"sync"

	"minidb/internal/btree"
	"minidb/internal/bufpool"
	"minidb/internal/pager"
	"minidb/internal/wal"
)

var (
	ErrWriterBusy = errors.New("db: đã có transaction ghi đang mở")
	ErrTxnDone    = errors.New("db: transaction đã kết thúc")
	ErrNoTxn      = errors.New("db: chưa mở transaction")
)

// Options là các nút vặn được. Mặc định (giá trị 0) là cấu hình AN TOÀN, trừ
// khi ghi rõ ngược lại — một tùy chọn nguy hiểm mà bật sẵn là một cái bẫy.
type Options struct {
	// Frames là số frame của buffer pool. 0 -> 256.
	Frames int

	// NoSync tắt fsync ở CẢ log lẫn pager. Chỉ để đo cái giá của durability.
	NoSync bool

	// NoWrite giữ byte log trong buffer của tiến trình, không write(2) xuống
	// kernel. Chỉ dùng để TỰ KIỂM TRA bộ kiểm tra crash: xem wal.Log.NoWrite.
	NoWrite bool

	// NoFullPageWrites tắt việc ghi trọn ảnh page ở lần chạm đầu sau mỗi
	// checkpoint. Mặc định là BẬT (tức field này false) vì nếu không, một
	// torn write làm pageLSN thành rác và redo lặng lẽ bỏ qua đúng page hỏng.
	// Xem wal.FlagFullPage và nợ P2-3.
	NoFullPageWrites bool

	// CheckpointBytes: cứ ngần này byte log thì tự checkpoint. 0 -> 4 MiB.
	// Đặt âm để tắt hẳn checkpoint tự động.
	CheckpointBytes int64
}

func (o Options) frames() int {
	if o.Frames <= 0 {
		return 256
	}
	return o.Frames
}

func (o Options) ckptBytes() int64 {
	if o.CheckpointBytes == 0 {
		return 4 << 20
	}
	return o.CheckpointBytes
}

// DB là một database mở.
type DB struct {
	mu sync.Mutex

	pg   *pager.Pager
	pool *bufpool.Pool
	log  *wal.Log
	tree *btree.Tree
	j    *journal
	opt  Options

	root    pager.PageID
	nextTxn uint64
	active  *Txn

	// att: transaction đang chạy -> LSN record cuối của nó.
	// dpt: page bẩn -> recLSN, tức LSN của thay đổi ĐẦU TIÊN chưa nằm trên đĩa.
	//
	// Hai bảng này là toàn bộ nội dung của một checkpoint, và recLSN là con số
	// quyết định recovery chạy bao lâu: redo bắt đầu từ recLSN NHỎ NHẤT.
	att map[uint64]uint64
	dpt map[pager.PageID]uint64

	// fpw: page đã ghi trọn ảnh kể từ checkpoint gần nhất. Reset mỗi lần
	// checkpoint, vì mốc chống torn write là checkpoint chứ không phải phiên.
	fpw map[pager.PageID]bool

	lastCkptEnd uint64

	// Thống kê để bench/diary nhìn.
	Checkpoints, Recoveries               int64
	RedoApplied, RedoSkipped, UndoApplied int64
	LoserTxns, OrphanPages                int64
}

// Open mở (hoặc tạo) database tại path. File log là path + ".wal".
//
// Recovery chạy NGAY trong Open, không phải một lệnh riêng người dùng nhớ gọi:
// một database mở được mà chưa recovery là một database đang nói dối.
func Open(path string, opt Options) (*DB, error) {
	pg, err := pager.Open(path)
	if err != nil {
		return nil, err
	}
	pg.NoSync = opt.NoSync

	lg, err := wal.Open(path + ".wal")
	if err != nil {
		pg.Close()
		return nil, err
	}
	lg.NoSync = opt.NoSync
	lg.NoWrite = opt.NoWrite
	lg.FullPageWrites = !opt.NoFullPageWrites

	pool := bufpool.New(pg, opt.frames(), bufpool.NewLRU(opt.frames()))
	pool.Alloc = pg

	d := &DB{
		pg: pg, pool: pool, log: lg, opt: opt,
		root: pg.Root(),
		att:  map[uint64]uint64{},
		dpt:  map[pager.PageID]uint64{},
		fpw:  map[pager.PageID]bool{},
	}

	// WAL rule cắm vào đúng một chỗ: hàm pool gọi trước khi ghi page bẩn.
	pool.FlushLog = func(pageLSN uint64) error { return lg.Flush(pageLSN) }
	// Page đã rời RAM thì không còn bẩn -> ra khỏi DPT.
	pool.OnFlush = func(id pager.PageID) { delete(d.dpt, id) }

	d.j = newJournal(d)

	if err := d.recover(); err != nil {
		lg.Close()
		pg.Close()
		return nil, fmt.Errorf("db: recovery thất bại: %w", err)
	}

	if d.root == 0 {
		// Database trống: dựng cây rỗng trong một transaction thật, để cả
		// việc "database này tồn tại" cũng nằm trong log.
		tx, err := d.beginLocked()
		if err != nil {
			lg.Close()
			pg.Close()
			return nil, err
		}
		d.j.enter(tx)
		t, err := btree.CreateLogged(pool, d.j)
		d.j.leave()
		if err != nil {
			lg.Close()
			pg.Close()
			return nil, err
		}
		d.tree = t
		if err := tx.Commit(); err != nil {
			lg.Close()
			pg.Close()
			return nil, err
		}
	} else {
		d.tree = btree.Open(pool, d.root)
		d.tree.J = d.j
	}
	return d, nil
}

// Close chốt một checkpoint rồi đóng file. Không bắt buộc phải gọi — đó là
// điểm khác biệt giữa "có WAL" và "không có": crash và Close chỉ khác nhau ở
// tốc độ mở lần sau, không khác nhau ở dữ liệu.
func (d *DB) Close() error {
	d.mu.Lock()
	if d.active != nil {
		d.mu.Unlock()
		return ErrWriterBusy
	}
	err := d.checkpointLocked(true)
	d.mu.Unlock()
	if cerr := d.log.Close(); err == nil {
		err = cerr
	}
	if cerr := d.pg.Close(); err == nil {
		err = cerr
	}
	return err
}

func (d *DB) Root() pager.PageID  { return d.root }
func (d *DB) Log() *wal.Log       { return d.log }
func (d *DB) Pool() *bufpool.Pool { return d.pool }
func (d *DB) Pager() *pager.Pager { return d.pg }
func (d *DB) Tree() *btree.Tree   { return d.tree }

// DirtyPages là kích thước dirty page table — thứ quyết định recovery dài bao
// nhiêu, và là số duy nhất mà checkpoint cố gắng giảm.
func (d *DB) DirtyPages() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.dpt)
}

// ---------- transaction ----------

// Txn là một transaction ghi. Phase 5 cho phép ĐÚNG MỘT writer tại một thời
// điểm; nhiều writer song song là phase 6 (2PL/MVCC). Ràng buộc đó giờ được
// CODE bắt buộc chứ không còn là quy ước trong đầu — trả một phần nợ P1-2.
type Txn struct {
	db    *DB
	id    uint64
	first uint64
	prev  uint64 // LSN record cuối của txn này; đầu chuỗi undo
	done  bool
}

func (d *DB) Begin() (*Txn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.beginLocked()
}

func (d *DB) beginLocked() (*Txn, error) {
	if d.active != nil {
		return nil, ErrWriterBusy
	}
	d.nextTxn++
	tx := &Txn{db: d, id: d.nextTxn}
	lsn, err := d.log.Append(&wal.Record{Type: wal.TypeBegin, TxnID: tx.id})
	if err != nil {
		return nil, err
	}
	tx.first, tx.prev = lsn, lsn
	d.att[tx.id] = lsn
	d.active = tx
	return tx, nil
}

func (tx *Txn) ID() uint64 { return tx.id }

func (tx *Txn) Put(key, val []byte) error {
	return tx.do(func() error { return tx.db.tree.Put(key, val) })
}

func (tx *Txn) Delete(key []byte) error {
	return tx.do(func() error { return tx.db.tree.Delete(key) })
}

// Get đọc trong transaction. Không có isolation nào ở đây — một writer duy
// nhất thì không có gì để cách ly. Phase 6.
func (tx *Txn) Get(key []byte) ([]byte, error) {
	if tx.done {
		return nil, ErrTxnDone
	}
	return tx.db.tree.Get(key)
}

func (tx *Txn) do(fn func() error) error {
	if tx.done {
		return ErrTxnDone
	}
	d := tx.db
	d.mu.Lock()
	defer d.mu.Unlock()
	d.j.enter(tx)
	err := fn()
	d.j.leave()
	if jerr := d.j.Err(); jerr != nil {
		// Ghi được vào cây mà không ghi được vào log là mất recoverability.
		// Không có đường lùi an toàn ở đây: transaction phải chết.
		return fmt.Errorf("db: log hỏng giữa lệnh: %w", jerr)
	}
	return err
}

// Commit chốt transaction: ghi commit record rồi fsync log tới đúng nó.
//
// Đây là toàn bộ định nghĩa của durability trong bản này. Không FlushAll, không
// ghi meta, không đụng gì tới page dữ liệu — chúng có thể còn bẩn nguyên trong
// pool. Phase 1 phải fsync cả file rồi ghi meta rồi fsync lần nữa cho MỖI
// commit; ở đây chỉ còn một lần fsync một vùng log ghi tuần tự.
func (tx *Txn) Commit() error {
	if tx.done {
		return ErrTxnDone
	}
	d := tx.db
	d.mu.Lock()
	defer d.mu.Unlock()

	lsn, err := d.log.Append(&wal.Record{Type: wal.TypeCommit, TxnID: tx.id, PrevLSN: tx.prev})
	if err != nil {
		return err
	}
	if err := d.log.Flush(lsn + 1); err != nil {
		return err
	}
	// Chỉ sau khi commit record đã durable, page mà txn này giải phóng mới
	// được phép cấp lại — xem pager.ReleasePending.
	d.pg.ReleasePending()
	delete(d.att, tx.id)
	tx.done = true
	d.active = nil
	return d.maybeCheckpoint()
}

// Abort quay ngược mọi thay đổi của transaction bằng đúng cỗ máy mà recovery
// dùng (undoChain). Một đường code cho cả hai: nếu undo lúc chạy khác undo lúc
// recovery thì một trong hai sẽ sai, và cái sai đó chỉ lộ ra sau khi mất điện.
func (tx *Txn) Abort() error {
	if tx.done {
		return ErrTxnDone
	}
	d := tx.db
	d.mu.Lock()
	defer d.mu.Unlock()

	orphans, err := d.undoChain(map[uint64]uint64{tx.id: tx.prev})
	if err != nil {
		return err
	}
	// Page txn này cấp phát trở thành mồ côi (ALLOC không undo được) -> thu
	// hồi. Danh sách lấy từ LOG chứ không từ tx.alloc: undo lúc chạy và undo
	// lúc recovery phải nhìn thấy đúng một sự thật.
	for _, id := range orphans {
		// Dọn khỏi pool TRƯỚC khi trả về allocator: xem bufpool.Discard.
		if err := d.pool.Discard(id); err != nil {
			return err
		}
		if err := d.pg.FreeNow(id); err != nil {
			return err
		}
		d.OrphanPages++
	}
	d.pg.DropPending()
	// undoChain khôi phục d.root; cây giữ một bản sao riêng trong RAM và phải
	// được kéo về theo, nếu không nó sẽ đọc tiếp từ root mà txn vừa hủy tạo ra.
	d.tree.SetRoot(d.root)
	delete(d.att, tx.id)
	tx.done = true
	d.active = nil
	return nil
}

// ---------- đọc ngoài transaction ----------

func (d *DB) Get(key []byte) ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.tree.Get(key)
}

// GetFunc: xem btree.Tree.GetFunc. fn chạy trong lúc giữ d.mu, nên nó phải
// ngắn và không được gọi lại vào DB.
func (d *DB) GetFunc(key []byte, fn func(v []byte) error) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.tree.GetFunc(key, fn)
}

func (d *DB) Has(key []byte) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.tree.Has(key)
}

// Iter là một lần duyệt khoảng khóa [lo, hi) mà latch chỉ bị giữ TRONG MỘT
// BƯỚC, không phải suốt lần duyệt.
//
// Đây là chỗ trả nợ P6-4, và nó phải trả trước phase 7 chứ không phải để cho
// đẹp: một index scan là "duyệt index rồi với mỗi entry đi tra bảng theo
// primary key" — tức là một lần đọc cây TỪ TRONG callback của một lần duyệt
// cây. Với bản cũ (giữ d.mu suốt lần duyệt) việc đó tự khoá chết chính nó.
// Nghĩa là nợ P4-5/P6-4 không phải món xa xỉ; nó là cửa vào của cả phase.
//
// Vì sao nhả latch giữa hai bước lại AN TOÀN, trong khi chú thích cũ ở đây
// nói ngược lại:
//
//  1. cursor mang số đời cấu trúc của cây (btree.Tree.gen). Writer chen vào
//     giữa hai bước thì số đời lệch, và cursor đi lại từ root tới khóa kế
//     tiếp SAU khóa vừa trả — vị trí theo khóa, không theo (page, slot).
//  2. thứ quyết định người đọc THẤY GÌ không còn là latch mà là snapshot MVCC
//     của phase 6. Một lần duyệt thấy cả cái vừa được commit giữa đường vẫn
//     ra kết quả đúng, vì chuỗi version giữ cả bản cũ và luật visibility
//     chọn bản đúng cho snapshot ấy.
//
// Điểm (2) là điều đáng nhớ nhất: cái latch mà phase 4 phải giữ thật lâu được
// tháo ra nhờ một cơ chế của phase 6, không nhờ một cơ chế của phase 4.
type Iter struct {
	d      *DB
	lo, hi []byte
	c      *btree.Cursor
	key    []byte
	val    []byte
	ok     bool
	err    error
}

// Iter mở một lần duyệt. Chưa đứng trên entry nào: Next() đầu tiên đặt vị trí.
func (d *DB) Iter(lo, hi []byte) *Iter {
	return &Iter{d: d, lo: lo, hi: hi}
}

// Next bước một bước. Toàn bộ thân hàm nằm dưới d.mu; lúc nó trả về thì không
// còn latch nào bị giữ, nên người gọi được phép đọc cây tiếp, ghi cây, hoặc
// bỏ cursor đó luôn.
func (it *Iter) Next() bool {
	it.d.mu.Lock()
	defer it.d.mu.Unlock()
	if it.err != nil {
		it.ok = false
		return false
	}
	if it.c == nil {
		it.c = it.d.tree.Seek(it.lo)
	} else if it.ok {
		it.c.Next()
	} else {
		return false
	}
	if err := it.c.Err(); err != nil {
		it.err, it.ok = err, false
		return false
	}
	if !it.c.Valid() {
		it.ok = false
		return false
	}
	if it.hi != nil && bytes.Compare(it.c.Key(), it.hi) >= 0 {
		it.ok = false
		return false
	}
	// Chép ra: hết latch là byte của cursor có thể bị bước kế ghi lại.
	it.key = append(it.key[:0], it.c.Key()...)
	it.val = append(it.val[:0], it.c.Value()...)
	it.ok = true
	return true
}

func (it *Iter) Key() []byte   { return it.key }
func (it *Iter) Value() []byte { return it.val }
func (it *Iter) Err() error    { return it.err }

// Restores là số lần cursor phải đi lại từ root vì có writer chen vào. Bằng 0
// trong một bài test có writer song song = cơ chế chưa từng được thử.
func (it *Iter) Restores() int {
	if it.c == nil {
		return 0
	}
	return it.c.Restores()
}

// Range gọi fn cho mọi khóa trong [lo, hi) theo thứ tự tăng dần. hi == nil
// nghĩa là tới hết.
//
// fn chạy KHÔNG giữ latch nào (xem Iter), nên nó được phép gọi lại vào DB.
// Đổi lại, lần duyệt không phải ảnh chụp một thời điểm: muốn ảnh chụp thì
// dùng txn.Txn.Scan, hoặc RangeAtomic nếu chỉ cần chặn writer bằng sức mạnh.
func (d *DB) Range(lo, hi []byte, fn func(key, val []byte) bool) error {
	it := d.Iter(lo, hi)
	for it.Next() {
		if !fn(it.Key(), it.Value()) {
			break
		}
	}
	return it.Err()
}

// RangeAtomic là hành vi cũ của Range: giữ d.mu SUỐT lần duyệt, nên nó thật
// sự là một ảnh chụp của cây — và nó chặn mọi writer trong khoảng thời gian
// ấy. Chỉ dùng cho kiểm tra tính đúng đắn (verify, fsck, test), không dùng
// trên đường đọc thường: một scan 200k khóa giữ latch ~40ms, và đó chính là
// con số mà nợ P4-5 nói tới.
//
// fn ở đây KHÔNG được gọi lại vào DB — sẽ khoá chết. Chữ "Atomic" trong tên
// là để người gọi phải nghĩ tới điều đó.
func (d *DB) RangeAtomic(lo, hi []byte, fn func(key, val []byte) bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.tree.Range(lo, hi, fn)
}

// Update chạy fn trong một transaction, tự commit hoặc tự abort.
func (d *DB) Update(fn func(tx *Txn) error) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Abort()
		return err
	}
	return tx.Commit()
}

// SimulateCrash bỏ tiến trình lại đúng chỗ kill -9 sẽ bỏ: mọi thứ chưa fsync
// biến mất, không checkpoint, không FlushAll, page bẩn trong pool bốc hơi.
//
// Nó KHÔNG thay thế được crashlab: kill -9 thật còn xóa cả những gì nằm trong
// buffer của thư viện lẫn của tiến trình, và kiểm tra luôn rằng ta không vô
// tình dựa vào một destructor nào. Nhưng nó chạy trong một mili giây nên fuzz
// và property test dùng được.
func (d *DB) SimulateCrash() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	err := d.log.CloseNoFlush()
	if cerr := d.pg.Close(); err == nil {
		err = cerr
	}
	d.active = nil
	return err
}
