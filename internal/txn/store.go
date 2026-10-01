// Package txn là tầng transaction & concurrency control (roadmap phase 6).
//
// # Quyết định kiến trúc, và vì sao
//
// Phase 5 để lại một database ĐÚNG MỘT WRITER: db.Begin trả ErrWriterBusy nếu
// đã có transaction ghi. Phase 6 phải cho nhiều transaction chạy đồng thời mà
// KHÔNG được phá recovery của phase 5. Có hai đường:
//
//	(a) cho nhiều writer cùng sửa cây -> cần latch-coupling (nợ P4-5, roadmap
//	    xếp vào phase 7) VÀ phải bỏ undo physical: nếu txn A và txn B cùng sửa
//	    một page rồi A abort, dán ảnh-trước của A đè lên là xoá luôn việc của
//	    B. Đây chính là lý do Postgres KHÔNG có pha undo.
//	(b) giữ tầng vật lý đúng một writer, và cho transaction ĐỢI tới lúc commit
//	    mới ghi: mỗi transaction gom thay đổi vào một write set trong RAM, lúc
//	    commit thì xin quyền ghi, kiểm tra xung đột, rồi ghi TẤT CẢ trong đúng
//	    MỘT transaction vật lý của phase 5.
//
// Bản này chọn (b) — **deferred write / optimistic concurrency control**, đúng
// cách FoundationDB và read-write transaction của Spanner làm. Ba hệ quả:
//
//  1. WAL, checkpoint, recovery, undo của phase 5 KHÔNG đổi một dòng nào, và
//     vẫn đúng: một transaction vật lý không bao giờ bị đan xen với cái khác.
//     Bằng chứng: `make crashlab` vẫn 200/200 sau phase 6.
//  2. **Abort là miễn phí** — không I/O, không undo, không log. Ném write set
//     đi là xong. Đây là món quà của MVCC mà tầng vật lý không cho được.
//  3. Cái giá, và phải nói thẳng: write set nằm trong RAM nên transaction ghi
//     không được to; và hai writer không hề chặn nhau *ở tầng logic* nhưng
//     vẫn tuần tự hoá *ở lúc commit*. Throughput ghi không tăng — phase 6 mua
//     **tính đúng đắn khi có đồng thời**, không mua tốc độ. Số đo ở diary.
//
// # Ai chặn ai
//
//	reader vs reader   : không bao giờ chặn (trừ latch từng-lệnh của db.mu)
//	reader vs writer   : không bao giờ chặn — reader đọc version cũ  ← trả nợ P1-2b
//	writer vs writer   : chỉ gặp nhau ở lúc commit (RepeatableRead), hoặc
//	                     chặn nhau bằng lock từ lúc ghi (Serializable)
package txn

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"minidb/internal/btree"
	"minidb/internal/db"
	"minidb/internal/lock"
)

var (
	ErrTxnDone     = errors.New("txn: transaction đã kết thúc")
	ErrConflict    = errors.New("txn: xung đột ghi-ghi, kẻ commit trước thắng")
	ErrReservedKey = errors.New("txn: khóa bắt đầu bằng 0x00 dành cho metadata")
	ErrDeadlock    = lock.ErrDeadlock
	ErrTimeout     = lock.ErrTimeout
)

// metaNext giữ mốc trên của bộ cấp id transaction.
//
// Không gian khóa 0x00... dành riêng cho metadata của tầng này và bị chặn ở
// mọi cửa vào (xem checkKey). Vì sao phải giữ mốc này trên đĩa: xmin của
// version nằm trong cây là con số VĨNH VIỄN, nên sau khi mở lại file, id cấp
// tiếp theo bắt buộc phải lớn hơn mọi xmin đang có — nếu không, một version
// cũ đột nhiên trở thành "bắt đầu sau ta" và biến mất khỏi mọi snapshot.
var metaNext = []byte{0x00, 't', 'x', 'n'}

// idBatch: mỗi lần chạm đĩa thì đặt trước ngần này id. Ghi mốc mỗi commit sẽ
// biến một khóa duy nhất thành điểm nóng (cùng một page bẩn ở mọi commit, và
// một record log thêm cho mỗi transaction). Đổi lại: sau crash, id bị nhảy
// một khoảng — vô hại, vì id chỉ cần TĂNG, không cần liền.
const idBatch = 64

// Stats là ảnh chụp số đếm.
type Stats struct {
	Begins, Commits, Aborts       int64
	Conflicts, Deadlocks, Timeout int64
	Retries                       int64
	DirtyReads                    int64
	VersionsWritten               int64
	VersionsPruned                int64
	KeysReclaimed                 int64
	IDBumps                       int64
}

type stats struct {
	begins, commits, aborts       atomic.Int64
	conflicts, deadlocks, timeout atomic.Int64
	retries                       atomic.Int64
	dirtyReads                    atomic.Int64
	verWritten, verPruned         atomic.Int64
	keysReclaimed                 atomic.Int64
	idBumps                       atomic.Int64
}

// Store là một database có transaction đồng thời.
type Store struct {
	d   *db.DB
	lk  *lock.Manager
	own bool // Store có sở hữu d không (Open thì có, Wrap thì không)

	// dbMu tuần tự hoá MỌI lần chạm tầng vật lý: apply write set, ghi mốc id,
	// vacuum. Nó tồn tại vì db.Begin cho đúng một writer — dbMu biến
	// ErrWriterBusy từ một lỗi phải xử lý thành một điều không thể xảy ra.
	//
	// Thứ tự lấy latch, một chiều, không có ngoại lệ:  dbMu -> mu.
	// Không bao giờ giữ mu mà xin dbMu.
	dbMu sync.Mutex

	mu sync.Mutex

	// Hai bộ đếm, và đó là điểm tinh tế nhất của cả file.
	//
	//   vnext : id ẢO, chỉ sống trong RAM. Dùng làm danh tính: khóa của bảng
	//           active, danh tính trước lock manager, và thứ tự tuổi khi chọn
	//           nạn nhân deadlock.
	//   next  : id THẬT (xid), là con số đi vào xmin của version nên phải bền
	//           qua khởi động lại — và vì thế mỗi lần nới nó là một lần chạm đĩa.
	//
	// Vì sao phải tách: một transaction CHỈ ĐỌC không tạo version nào, nên nó
	// không cần xid. Gộp hai bộ đếm lại thì cứ 64 lượt đọc là một lần ghi log
	// — và bài test TestReadOnlyCommitTouchesNothing đã bắt đúng lỗi đó ở lần
	// chạy đầu. Postgres gọi id ảo này là *virtual transaction id*, và đây
	// chính là lý do nó tồn tại.
	vnext     uint64
	next      uint64 // xid sẽ cấp tiếp theo
	persisted uint64 // mốc trên đã nằm trên đĩa; luôn giữ next <= persisted
	active    map[uint64]*Txn

	st stats
}

// Open mở store trên file path. Recovery của phase 5 chạy trong db.Open.
func Open(path string, opt db.Options) (*Store, error) {
	d, err := db.Open(path, opt)
	if err != nil {
		return nil, err
	}
	s, err := Wrap(d)
	if err != nil {
		d.Close()
		return nil, err
	}
	s.own = true
	return s, nil
}

// Wrap dựng store trên một db đang mở. Dùng cho test muốn soi cả hai tầng.
func Wrap(d *db.DB) (*Store, error) {
	s := &Store{d: d, lk: lock.New(), active: map[uint64]*Txn{}, vnext: 1}
	hi, err := s.readNext()
	if err != nil {
		return nil, err
	}
	s.persisted = hi
	s.next = hi
	if s.next == 0 {
		s.next = 1 // id 0 dành làm "không có transaction"
	}
	return s, nil
}

func (s *Store) DB() *db.DB           { return s.d }
func (s *Store) Locks() *lock.Manager { return s.lk }

func (s *Store) Close() error {
	if !s.own {
		return nil
	}
	return s.d.Close()
}

func (s *Store) Stats() Stats {
	lst := s.lk.Stats()
	return Stats{
		Begins:          s.st.begins.Load(),
		Commits:         s.st.commits.Load(),
		Aborts:          s.st.aborts.Load(),
		Conflicts:       s.st.conflicts.Load(),
		Deadlocks:       lst.Deadlocks,
		Timeout:         lst.Timeouts,
		Retries:         s.st.retries.Load(),
		DirtyReads:      s.st.dirtyReads.Load(),
		VersionsWritten: s.st.verWritten.Load(),
		VersionsPruned:  s.st.verPruned.Load(),
		KeysReclaimed:   s.st.keysReclaimed.Load(),
		IDBumps:         s.st.idBumps.Load(),
	}
}

// ---------- id transaction ----------

func (s *Store) readNext() (uint64, error) {
	v, err := s.d.Get(metaNext)
	if errors.Is(err, btree.ErrKeyNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if len(v) != 8 {
		return 0, fmt.Errorf("txn: mốc id dài %d byte, phải là 8", len(v))
	}
	return binary.LittleEndian.Uint64(v), nil
}

// persistNext nâng mốc trên id trên đĩa lên ít nhất `want`, trong một
// transaction vật lý riêng. Ghi max chứ không ghi thẳng: hai goroutine có thể
// cùng phát hiện hết id và cùng gọi vào đây.
func (s *Store) persistNext(want uint64) error {
	s.dbMu.Lock()
	defer s.dbMu.Unlock()
	cur, err := s.readNext()
	if err != nil {
		return err
	}
	if cur >= want {
		return nil
	}
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], want)
	if err := s.d.Update(func(ptx *db.Txn) error { return ptx.Put(metaNext, b[:]) }); err != nil {
		return err
	}
	s.st.idBumps.Add(1)
	return nil
}

// ---------- snapshot & horizon ----------

// snapshotLocked chụp snapshot. Gọi khi đang giữ s.mu — snapshot và việc ghi
// tên mình vào bảng active PHẢI nằm trong cùng một vùng găng, nếu không sẽ có
// khe hở mà horizon() không thấy transaction vừa sinh ra.
func (s *Store) snapshotLocked() Snapshot {
	snap := Snapshot{Xmax: s.next, Xmin: s.next}
	for _, t := range s.active {
		// Chỉ những transaction ĐÃ nhận xid mới có thể để lại version, nên
		// chỉ chúng cần nằm trong tập active. Kẻ chưa ghi gì sẽ nhận một xid
		// >= Xmax của ta, tức tự khắc vô hình — không cần ghi tên nó vào đây.
		x := t.xid.Load()
		if x == 0 {
			continue
		}
		if snap.Active == nil {
			snap.Active = make(map[uint64]bool, len(s.active))
		}
		snap.Active[x] = true
		if x < snap.Xmin {
			snap.Xmin = x
		}
	}
	return snap
}

func (s *Store) snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshotLocked()
}

// horizon là OldestXmin: id nhỏ nhất mà một transaction còn sống vẫn có thể
// coi là vô hình. Mọi version có xmin < horizon thì với MỌI snapshot đang
// sống, hoặc nhìn thấy được, hoặc đã bị một bản nhìn thấy được che đi.
//
// Transaction Serializable và ReadUncommitted đọc bản mới nhất nên không giữ
// version cũ nào — chúng chỉ đóng góp CHÍNH XID CỦA MÌNH (để version chúng
// vừa ghi không bị coi là rác), còn khi chưa ghi gì thì không ràng buộc gì cả.
func (s *Store) horizon() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.next
	for _, t := range s.active {
		if g := t.gcXmin.Load(); g < h {
			h = g
		}
	}
	return h
}

// ---------- đọc chuỗi version ----------

// peekDirty tìm giá trị CHƯA COMMIT của khóa trong write set của một
// transaction khác — tức là dựng lại dirty read bằng tay.
//
// Hàm này chỉ tồn tại vì ReadUncommitted, và nó là bằng chứng cho một điều
// đáng ghi vào diary: trong kiến trúc deferred-write, dirty read KHÔNG phải
// một luật bị nới ra, nó là một tính năng phải thêm vào. Đó cũng là lý do
// không DB nào hiện đại còn coi ReadUncommitted là mức đáng cài cho tử tế.
//
// Chọn ai nếu nhiều transaction cùng ghi dở khóa đó: kẻ có id lớn nhất, tức
// "bẩn nhất trong những cái bẩn".
func (s *Store) peekDirty(self uint64, key []byte) (Version, bool) {
	s.mu.Lock()
	cands := make([]*Txn, 0, len(s.active))
	for vid, t := range s.active {
		if vid != self {
			cands = append(cands, t)
		}
	}
	s.mu.Unlock()

	var best Version
	var bestVID uint64
	var found bool
	for _, t := range cands {
		// So theo id ẢO, không theo xmin: kẻ ghi dở có thể còn chưa nhận xid
		// (xmin = 0), và khi đó so theo xmin sẽ chọn bừa.
		if v, ok := t.own(key); ok && (!found || t.vid > bestVID) {
			best, bestVID, found = v, t.vid, true
		}
	}
	if found {
		s.st.dirtyReads.Add(1)
	}
	return best, found
}

// ---------- begin ----------

// Begin mở một transaction ở mức isolation cho trước.
//
// Không chạm đĩa lần nào, và đó là điều bắt buộc: phần lớn transaction của
// một hệ thống thật là chỉ đọc, và một Begin có I/O sẽ làm mọi lần đọc phải
// trả giá cho một thứ nó không dùng.
func (s *Store) Begin(l Level) (*Txn, error) {
	s.mu.Lock()
	vid := s.vnext
	s.vnext++
	t := &Txn{s: s, vid: vid, iso: l, ws: map[string]Version{}}
	t.snap = s.snapshotLocked()
	t.setGCXmin()
	s.active[vid] = t
	s.mu.Unlock()
	s.st.begins.Add(1)
	return t, nil
}

// allocXID cấp một xid THẬT. Chỉ gọi ở lần ghi đầu tiên của transaction.
func (s *Store) allocXID() (uint64, error) {
	for {
		s.mu.Lock()
		if s.next < s.persisted {
			id := s.next
			s.next++
			s.mu.Unlock()
			return id, nil
		}
		want := s.next + idBatch
		s.mu.Unlock()

		// Chạm đĩa NGOÀI s.mu: thứ tự latch là dbMu -> mu, và persistNext lấy
		// dbMu. Giữ mu ở đây là tự khoá chết với một Commit đang chạy.
		if err := s.persistNext(want); err != nil {
			return 0, err
		}
		s.mu.Lock()
		if want > s.persisted {
			s.persisted = want
		}
		s.mu.Unlock()
	}
}

func (s *Store) unregister(vid uint64) {
	s.mu.Lock()
	delete(s.active, vid)
	s.mu.Unlock()
}

// NextID là xid sẽ cấp tiếp theo. Test và cmd/txnlab dùng nó để kiểm chứng
// bất biến quan trọng nhất của bộ cấp id: sau khi mở lại file, NextID phải
// LỚN HƠN mọi xmin đang nằm trong cây.
func (s *Store) NextID() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.next
}

// Active là số transaction đang chạy — test dùng để chờ đúng trạng thái thay
// vì ngủ một khoảng đoán bừa.
func (s *Store) Active() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.active)
}

// ---------- vòng thử lại ----------

// MaxRetries chặn số lần Update thử lại. Chặn hữu hạn chứ không phải vô hạn:
// một workload mà xung đột không bao giờ hết thì phải BÁO LỖI, không được
// quay mãi và giả vờ là đang tiến triển.
const MaxRetries = 50

// Update chạy fn trong một transaction, tự commit, và **tự thử lại** khi gặp
// xung đột hoặc deadlock.
//
// Vòng thử lại này không phải tiện nghi mà là phần bắt buộc của hợp đồng: từ
// RepeatableRead trở lên, một transaction có thể bị bắt phải chết vì lý do
// hoàn toàn không phải lỗi của nó. Ứng dụng nào dùng snapshot isolation mà
// không có vòng này thì sẽ hỏng khi có tải — và đó là bug hay gặp nhất khi
// người ta chuyển từ ReadCommitted lên RepeatableRead trên Postgres.
func (s *Store) Update(l Level, fn func(t *Txn) error) error {
	var last error
	var age uint64
	for i := 0; i < MaxRetries; i++ {
		t, err := s.Begin(l)
		if err != nil {
			return err
		}
		// Giữ TUỔI của lần thử đầu qua mọi lần thử lại. Không có dòng này thì
		// mỗi lần thử là một transaction trẻ măng, và ở Serializable nó sẽ bị
		// chọn làm nạn nhân deadlock mãi mãi — xem lock.Manager.SetAge.
		if age == 0 {
			age = t.vid
		}
		s.lk.SetAge(t.vid, age)
		err = fn(t)
		if err == nil {
			err = t.Commit()
		} else {
			t.Abort()
		}
		if err == nil {
			return nil
		}
		if !Retryable(err) {
			return err
		}
		last = err
		s.st.retries.Add(1)
		backoff(i)
	}
	return fmt.Errorf("txn: bỏ cuộc sau %d lần thử: %w", MaxRetries, last)
}

// View chạy fn trong một transaction chỉ đọc và luôn abort — không có write
// set thì abort và commit là một, nhưng gọi Abort nói rõ ý hơn.
func (s *Store) View(l Level, fn func(t *Txn) error) error {
	t, err := s.Begin(l)
	if err != nil {
		return err
	}
	defer t.Abort()
	return fn(t)
}

// Retryable: lỗi này là do đồng thời (thử lại có ý nghĩa) hay do logic
// (thử lại chỉ lặp lại cùng một cái sai)?
func Retryable(err error) bool {
	return errors.Is(err, ErrConflict) || errors.Is(err, lock.ErrDeadlock) ||
		errors.Is(err, lock.ErrTimeout)
}

// backoff ngủ một khoảng NGẪU NHIÊN tăng dần trước lần thử sau.
//
// Không có nó thì 50 lần thử cháy hết trong 0.22 giây và mọi kẻ tranh chấp
// đều thử lại cùng một nhịp — đúng nghĩa thrash: số lần thử tăng mà cơ hội
// thắng không tăng. Đo được ở TestTransferSerializableCommitsEverything:
// 10/160 lượt bỏ cuộc khi không có backoff.
//
// Phần NGẪU NHIÊN mới là phần quan trọng. Lùi một khoảng cố định thì hai kẻ
// tranh chấp vẫn thức cùng lúc và lại vấp vào nhau; jitter là thứ phá vỡ sự
// đồng pha đó. Đây cũng chính là lý do mọi thư viện retry của mạng đều có
// jitter chứ không chỉ có exponential.
func backoff(attempt int) {
	d := time.Duration(attempt+1) * 100 * time.Microsecond
	if d > 5*time.Millisecond {
		d = 5 * time.Millisecond
	}
	time.Sleep(time.Duration(rand.Int63n(int64(d) + 1)))
}

func checkKey(key []byte) error {
	if len(key) == 0 {
		return btree.ErrEmptyKey
	}
	if key[0] == 0x00 {
		return fmt.Errorf("%w: %q", ErrReservedKey, key)
	}
	return nil
}

// reserved cho biết khóa thuộc không gian metadata (bị bỏ qua khi scan/vacuum).
func reserved(key []byte) bool { return len(key) > 0 && key[0] == 0x00 }

// upperKey trả khóa nhỏ nhất lớn hơn mọi khóa có tiền tố key — dùng để chặn
// trên khi scan. Chỉ dùng nội bộ cho vacuum.
func upperKey(key []byte) []byte {
	out := append([]byte(nil), key...)
	for i := len(out) - 1; i >= 0; i-- {
		if out[i] != 0xff {
			out[i]++
			return out[:i+1]
		}
	}
	return nil
}
