package txn

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"sync/atomic"

	"minidb/internal/btree"
	"minidb/internal/db"
	"minidb/internal/lock"
)

// Txn là một transaction logic.
//
// Nó KHÔNG phải db.Txn (transaction vật lý của phase 5). Một Txn ở đây sống
// bao lâu cũng được, không giữ latch nào, không chặn ai; nó chỉ biến thành một
// db.Txn trong khoảnh khắc commit. Phân biệt được hai thứ này là phân biệt
// được "transaction" với "một lần ghi xuống đĩa" — và đó là lằn ranh mà
// phase 5 chưa có.
type Txn struct {
	s   *Store
	vid uint64 // id ảo: danh tính, tuổi, khóa của bảng active
	iso Level

	// xid là id THẬT, cấp LƯỜI ở lần ghi đầu tiên và 0 khi chưa ghi gì. Nó là
	// con số đi vào xmin của mọi version transaction này tạo ra.
	//
	// Hệ quả quan trọng của việc cấp lười: xid của ta luôn LỚN HƠN Xmax của
	// snapshot của chính ta (snapshot chụp ở Begin, xid cấp sau đó). Nên ta
	// không bao giờ "nhìn thấy" version của chính mình qua luật visibility —
	// và đó là lý do read-your-own-writes BẮT BUỘC phải đi qua write set.
	xid atomic.Uint64

	// gcXmin là ràng buộc của transaction này với bộ dọn version: version nào
	// có xmin < gcXmin thì nó không cần nữa. Để atomic vì horizon() đọc nó
	// trong khi transaction đang chạy — và luôn đọc được giá trị CŨ (nhỏ hơn)
	// là an toàn, vì bộ dọn khi đó chỉ dọn ít hơn mức có thể.
	gcXmin atomic.Uint64

	mu   sync.Mutex // giữ snap + ws; Store.peekDirty của txn KHÁC cũng vào đây
	snap Snapshot
	ws   map[string]Version

	done bool
}

// ID là id ảo — dùng để nhận diện transaction trong log của test và lab.
func (t *Txn) ID() uint64 { return t.vid }

// XID là id thật, 0 nếu transaction chưa ghi gì. Một transaction chỉ đọc
// không bao giờ có xid, và không bao giờ chạm đĩa vì chuyện đó.
func (t *Txn) XID() uint64 { return t.xid.Load() }

func (t *Txn) Level() Level { return t.iso }

// Snapshot trả về snapshot hiện hành (ReadCommitted thì nó đổi theo câu lệnh).
func (t *Txn) Snapshot() Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.snap
}

// setGCXmin cập nhật ràng buộc của transaction này với bộ dọn version.
//
// Hai vế, và thiếu vế nào cũng sai:
//
//   - snapshot của ta (nếu mức isolation dùng snapshot): bản cũ mà ta còn
//     phải đọc không được dọn.
//   - **xid của chính ta**: version ta vừa ghi mà chưa commit không được coi
//     là rác. Bỏ vế này thì một transaction Serializable ghi tombstone rồi
//     thấy chính tombstone ấy bị bộ dọn xử là "không ai cần" — và lần chạy
//     đầu của TestReadYourOwnWrites đã đỏ vì đúng lý do đó.
func (t *Txn) setGCXmin() {
	g := uint64(math.MaxUint64)
	if t.iso.usesSnapshot() {
		g = t.snap.Xmin
	}
	if x := t.xid.Load(); x != 0 && x < g {
		g = x
	}
	t.gcXmin.Store(g)
}

// ensureXID cấp xid ở lần ghi đầu tiên. Đặt gcXmin lại ngay sau đó: từ giây
// phút này, bộ dọn không được phép chạm tới version của ta.
func (t *Txn) ensureXID() (uint64, error) {
	if x := t.xid.Load(); x != 0 {
		return x, nil
	}
	x, err := t.s.allocXID()
	if err != nil {
		return 0, err
	}
	t.xid.Store(x)
	t.setGCXmin()
	return x, nil
}

// own tra write set của chính transaction — read-your-own-writes.
//
// Đây là thứ mà mọi thiết kế deferred-write buộc phải có, và là chỗ dễ quên
// nhất: nếu Get không hỏi write set trước, một transaction ghi rồi đọc lại sẽ
// thấy giá trị CŨ của chính nó. Không mức isolation nào cho phép điều đó.
func (t *Txn) own(key []byte) (Version, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	v, ok := t.ws[string(key)]
	return v, ok
}

// readSnap là snapshot dùng cho lần đọc này, và cũng là chỗ mức isolation
// biến thành hành vi:
//
//	ReadUncommitted / Serializable : Latest — không cách ly bằng version
//	ReadCommitted                  : snapshot MỚI cho mỗi câu lệnh
//	RepeatableRead                 : snapshot chụp ở Begin, không đổi
func (t *Txn) readSnap() Snapshot {
	switch t.iso {
	case ReadCommitted:
		snap := t.s.snapshot()
		t.mu.Lock()
		t.snap = snap
		t.mu.Unlock()
		t.setGCXmin()
		return snap
	case RepeatableRead:
		return t.Snapshot()
	default:
		return Latest
	}
}

// ---------- đọc ----------

// Get đọc một khóa. ok == false nghĩa là khóa không tồn tại TRONG THẾ GIỚI CỦA
// TRANSACTION NÀY — có thể nó tồn tại với người khác, và đó không phải lỗi.
func (t *Txn) Get(key []byte) (val []byte, ok bool, err error) {
	if t.done {
		return nil, false, ErrTxnDone
	}
	if err := checkKey(key); err != nil {
		return nil, false, err
	}
	if v, has := t.own(key); has {
		return v.Val, !v.Deleted, nil
	}
	if t.iso == ReadUncommitted {
		if v, has := t.s.peekDirty(t.vid, key); has {
			return v.Val, !v.Deleted, nil
		}
	}
	if t.iso == Serializable {
		if err := t.s.lk.Acquire(t.vid, lock.Key(key), lock.S); err != nil {
			return nil, false, err
		}
	}
	snap := t.readSnap()
	c, err := t.s.chain(key)
	if err != nil {
		return nil, false, err
	}
	v, has := c.Visible(snap)
	if !has || v.Deleted {
		return nil, false, nil
	}
	return v.Val, true, nil
}

// GetForUpdate đọc một khóa và lấy luôn lock X trên nó — chính là
// `SELECT ... FOR UPDATE`.
//
// Ở các mức dùng MVCC nó giống Get y hệt (không có lock nào). Nó chỉ có nghĩa
// ở Serializable, và ở đó nó là thứ BẮT BUỘC cho mẫu đọc-rồi-ghi. Lý do là
// **conversion deadlock**: nếu đọc lấy S rồi ghi nâng lên X thì hai
// transaction cùng đọc một khóa sẽ khoá chết nhau CHẮC CHẮN, không phải
// thỉnh thoảng. Đo được: bài chuyển tiền ở Serializable bỏ 68/160 lượt sau 50
// lần thử — và vòng thử lại không cứu được, vì lần thử nào cũng vấp đúng cái
// bẫy ấy.
//
// Đây là chỗ 2PL để lộ cái giá thật của nó: nó đòi ỨNG DỤNG phải nói trước ý
// định ghi. MVCC không đòi gì cả. Đó là một nửa lý do các DB hiện đại mặc
// định dùng MVCC.
func (t *Txn) GetForUpdate(key []byte) (val []byte, ok bool, err error) {
	if t.done {
		return nil, false, ErrTxnDone
	}
	if err := checkKey(key); err != nil {
		return nil, false, err
	}
	if t.iso == Serializable {
		if err := t.s.lk.Acquire(t.vid, lock.Key(key), lock.X); err != nil {
			return nil, false, err
		}
	}
	return t.Get(key)
}

// Scan gọi fn cho mọi khóa trong [lo, hi) theo thứ tự tăng dần. hi == nil = hết.
//
// Ở Serializable, Scan lấy lock S trên cả KHOẢNG, không phải trên từng khóa nó
// thấy. Đó là toàn bộ cơ chế chặn phantom: khóa chưa tồn tại thì không lock
// được, nên phải lock chỗ nó SẼ nằm. Đây là predicate lock ở dạng nghèo nhất
// còn dùng được.
//
// Phase 7 đổi cài đặt: STREAM bằng một phép trộn (merge join) giữa hai dòng đã
// sắp — cây và write set — thay vì gom hết vào RAM rồi sort. Đây là chỗ trả nợ
// P6-4, và ba thứ đổi theo:
//
//   - bộ nhớ: O(số khóa BẨN của chính transaction) thay vì O(số khóa trong
//     khoảng). Với một scan cả bảng và một write set rỗng thì từ O(n) về O(1).
//   - fn được gọi trong lúc đang duyệt, nên nó được phép GỌI LẠI vào Get/Scan.
//     Không có tính chất này thì không có index scan (xem internal/query).
//   - fn thấy khóa đầu tiên sau ~một lần xuống cây, không phải sau khi đã đọc
//     hết khoảng. Đó là khác biệt giữa "trả về một mảng" và "trả về một
//     cursor", và là lý do LIMIT trong SQL có nghĩa.
//
// Write set VẪN phải được chép ra và sắp trước khi trộn: fn có quyền Put trong
// lúc scan, và duyệt trực tiếp trên t.ws thì vừa sai ngữ nghĩa (ngữ nghĩa đúng
// là một câu lệnh nhìn thấy trạng thái lúc nó BẮT ĐẦU) vừa panic ngay.
func (t *Txn) Scan(lo, hi []byte, fn func(key, val []byte) bool) error {
	if t.done {
		return ErrTxnDone
	}
	if lo == nil {
		lo = []byte{0x01} // vượt qua không gian metadata 0x00
	}
	if err := checkKey(lo); err != nil {
		return err
	}
	if t.iso == Serializable {
		if err := t.s.lk.Acquire(t.vid, lock.Span(lo, hi), lock.S); err != nil {
			return err
		}
	}
	snap := t.readSnap()
	own := t.ownSorted(lo, hi)

	it := t.s.d.Iter(lo, hi)

	// tk/tv giữ bản NHÌN THẤY ĐƯỢC kế tiếp từ cây; tvalid = còn không.
	var tk, tv []byte
	tvalid := false
	// advance đi tới entry nhìn thấy được kế tiếp. Lỗi giải mã phải mang ra
	// NGOÀI vòng lặp: nuốt nó là biến một chuỗi version hỏng thành một scan
	// lặng lẽ thiếu khóa.
	advance := func() error {
		for it.Next() {
			k := it.Key()
			if reserved(k) {
				continue
			}
			c, err := DecodeChain(it.Value())
			if err != nil {
				return fmt.Errorf("txn: khóa %q: %w", k, err)
			}
			v, has := c.Visible(snap)
			if !has || v.Deleted {
				continue
			}
			tk = append(tk[:0], k...)
			tv = append(tv[:0], v.Val...)
			tvalid = true
			return nil
		}
		tvalid = false
		return it.Err()
	}
	if err := advance(); err != nil {
		return err
	}

	for {
		switch {
		case !tvalid && len(own) == 0:
			return nil

		case len(own) == 0 || (tvalid && bytes.Compare(tk, own[0].k) < 0):
			// Chỉ có trong cây. fn gọi TRƯỚC advance: advance ghi lại chính
			// hai buffer tk/tv mà fn đang cầm.
			if !fn(tk, tv) {
				return nil
			}
			if err := advance(); err != nil {
				return err
			}

		case !tvalid || bytes.Compare(own[0].k, tk) < 0:
			// Chỉ có trong write set: một khóa ta vừa tạo ra.
			w := own[0]
			own = own[1:]
			if !w.v.Deleted && !fn(w.k, w.v.Val) {
				return nil
			}

		default:
			// Cùng khóa: write set thắng — read-your-own-writes. Nếu ta vừa
			// xóa nó thì nó phải BIẾN MẤT khỏi scan của ta, nên nhánh này
			// cũng là chỗ cài phép xóa.
			w := own[0]
			own = own[1:]
			if err := advance(); err != nil {
				return err
			}
			if !w.v.Deleted && !fn(w.k, w.v.Val) {
				return nil
			}
		}
	}
}

// ownKV là một mục của write set đã chép ra và sắp theo khóa.
type ownKV struct {
	k []byte
	v Version
}

// ownSorted chép những khóa của write set nằm trong [lo, hi) ra và sắp lại.
// O(w log w) với w = số khóa bẩn, không phụ thuộc độ dài khoảng.
func (t *Txn) ownSorted(lo, hi []byte) []ownKV {
	t.mu.Lock()
	out := make([]ownKV, 0, len(t.ws))
	for ks, v := range t.ws {
		k := []byte(ks)
		if bytes.Compare(k, lo) < 0 || (hi != nil && bytes.Compare(k, hi) >= 0) {
			continue
		}
		out = append(out, ownKV{k: k, v: v})
	}
	t.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return bytes.Compare(out[i].k, out[j].k) < 0 })
	return out
}

// Count đếm khóa trong khoảng — dạng gọn của Scan, dùng cho test phantom.
func (t *Txn) Count(lo, hi []byte) (int, error) {
	n := 0
	err := t.Scan(lo, hi, func(_, _ []byte) bool { n++; return true })
	return n, err
}

// ---------- ghi ----------

func (t *Txn) Put(key, val []byte) error {
	return t.write(key, Version{Val: append([]byte(nil), val...)})
}

func (t *Txn) Delete(key []byte) error {
	return t.write(key, Version{Deleted: true})
}

// write đặt một version vào write set. Xmin điền ở đây, không ở người gọi:
// nó là xid, và xid chỉ tồn tại từ lần ghi đầu tiên.
func (t *Txn) write(key []byte, v Version) error {
	if t.done {
		return ErrTxnDone
	}
	if err := checkKey(key); err != nil {
		return err
	}
	if n := 2 + len(key) + 1 + (1 + 8 + 2 + len(v.Val)); n > btree.MaxEntrySize {
		// Chặn sớm: một version đơn lẻ đã không vừa thì chuỗi không bao giờ
		// vừa, và báo lỗi ở Put dễ hiểu hơn nhiều so với báo ở Commit.
		return fmt.Errorf("%w: value %d byte là quá lớn cho một chuỗi version",
			ErrChainFull, len(v.Val))
	}
	if t.iso == Serializable {
		if err := t.s.lk.Acquire(t.vid, lock.Key(key), lock.X); err != nil {
			return err
		}
	}
	xid, err := t.ensureXID()
	if err != nil {
		return err
	}
	v.Xmin = xid
	t.mu.Lock()
	t.ws[string(key)] = v
	t.mu.Unlock()
	return nil
}

// Dirty là số khóa trong write set — kích thước của cái đang giữ trong RAM.
func (t *Txn) Dirty() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.ws)
}

// ---------- kết thúc ----------

// Abort huỷ transaction. Không I/O, không log, không undo — chỉ ném write set
// đi và thả lock.
//
// So với db.Txn.Abort của phase 5 (đọc ngược chuỗi undo, dán từng ảnh-trước,
// ghi CLR, thu hồi page mồ côi) thì đây là toàn bộ chỗ khác nhau giữa
// "quay ngược thay đổi đã ghi" và "chưa từng ghi". Đó là lý do sâu xa vì sao
// MVCC thắng: abort miễn phí, và rollback một transaction dài không tốn gì.
func (t *Txn) Abort() error {
	if t.done {
		return ErrTxnDone
	}
	t.finish()
	t.s.st.aborts.Add(1)
	return nil
}

func (t *Txn) finish() {
	t.done = true
	t.mu.Lock()
	t.ws = map[string]Version{}
	t.mu.Unlock()
	t.s.lk.ReleaseAll(t.vid)
	t.s.unregister(t.vid)
}

// Commit áp write set xuống cây trong ĐÚNG MỘT transaction vật lý.
//
// Trình tự, và mỗi bước ở đúng chỗ của nó:
//
//  1. Nếu đã bị chọn làm nạn nhân deadlock -> chết, không thương lượng.
//  2. Write set rỗng -> transaction chỉ đọc, không có gì để ghi, không có gì
//     để xung đột. Đây là đường mà 100% reader đi qua, nên nó phải KHÔNG chạm
//     đĩa lần nào (kiểm bằng TestReadOnlyCommitTouchesNothing).
//  3. Giữ dbMu, rồi kiểm tra xung đột và ghi TRONG CÙNG vùng găng ấy. Tách hai
//     việc này ra là mở đúng cái khe mà first-committer-wins phải bịt: hai
//     transaction đều thấy "chưa ai ghi" rồi cả hai đều ghi.
//  4. Thất bại ở bất kỳ đâu trong db.Update -> phase 5 undo lại phần đã ghi.
//     Xung đột phát hiện ở giữa cũng đi qua đường đó, nên nó cũng là một bài
//     test cho pha undo của phase 5.
func (t *Txn) Commit() error {
	if t.done {
		return ErrTxnDone
	}
	if t.s.lk.Killed(t.vid) {
		t.finish()
		t.s.st.aborts.Add(1)
		return fmt.Errorf("%w: bị giết trước khi commit", lock.ErrDeadlock)
	}
	if t.Dirty() == 0 {
		t.finish()
		t.s.st.commits.Add(1)
		return nil
	}

	t.s.dbMu.Lock()
	err := t.apply()
	t.s.dbMu.Unlock()

	if err != nil {
		t.finish()
		if errors.Is(err, ErrConflict) {
			t.s.st.conflicts.Add(1)
		}
		t.s.st.aborts.Add(1)
		return err
	}
	t.finish()
	t.s.st.commits.Add(1)
	return nil
}

// apply chạy khi đang giữ dbMu.
func (t *Txn) apply() error {
	t.mu.Lock()
	keys := make([]string, 0, len(t.ws))
	for k := range t.ws {
		keys = append(keys, k)
	}
	ws := make(map[string]Version, len(t.ws))
	for k, v := range t.ws {
		ws[k] = v
	}
	snap := t.snap
	t.mu.Unlock()
	// Thứ tự ghi xác định: cùng một write set phải sinh cùng một chuỗi log,
	// nếu không thì hai lần chạy cùng seed cho ra hai file WAL khác nhau và
	// crashlab mất khả năng tái lập.
	sort.Strings(keys)

	horizon := t.s.horizon()
	var written, pruned, reclaimed int64

	err := t.s.d.Update(func(ptx *db.Txn) error {
		for _, ks := range keys {
			key := []byte(ks)
			var c Chain
			inTree := true
			raw, err := ptx.Get(key)
			switch {
			case err == nil:
				c, err = DecodeChain(raw)
				if err != nil {
					return fmt.Errorf("txn: khóa %q: %w", key, err)
				}
			case errors.Is(err, btree.ErrKeyNotFound):
				c, inTree = nil, false
			default:
				return err
			}

			// first-committer-wins. Chỉ từ RepeatableRead trở lên:
			//
			//   - ReadCommitted CỐ Ý không kiểm, vì đó chính là chỗ sinh ra
			//     lost update — và anomaly ấy là thứ phase 6 phải tái tạo
			//     được, không phải thứ phải tránh. Postgres ở RC cũng để lọt
			//     đúng ca này với mẫu SELECT-rồi-UPDATE.
			//   - Serializable không cần kiểm: lock X đã giữ từ lúc Put nên
			//     không ai chen vào được.
			if t.iso == RepeatableRead {
				if nv, ok := c.Newest(); ok && !snap.Visible(nv.Xmin) {
					return fmt.Errorf("%w: khóa %q đã bị txn %d ghi sau snapshot %d",
						ErrConflict, key, nv.Xmin, snap.Xmax)
				}
			}

			nc := c.Prepend(ws[ks])
			nc, np := nc.Prune(horizon)
			pruned += int64(np)
			written++

			if nc.Dead(horizon) {
				// Chỉ còn một tombstone mà không ai cần thấy -> thu hồi khóa.
				// Đây là VACUUM ở dạng cơ hội: nó xảy ra đúng lúc ta đã có
				// page trong tay, nên gần như miễn phí.
				//
				// inTree phải kiểm: xóa một khóa CHƯA TỪNG tồn tại là hợp lệ
				// ở tầng logic (nó chỉ có nghĩa "vẫn không có") nhưng
				// btree.Delete trả ErrKeyNotFound. Không có nhánh này thì mọi
				// lần xóa một khóa vắng mặt là một transaction chết oan.
				if inTree {
					if err := ptx.Delete(key); err != nil {
						return fmt.Errorf("txn: thu hồi khóa %q: %w", key, err)
					}
					reclaimed++
				}
				continue
			}
			buf := nc.Encode(make([]byte, 0, nc.EncodedSize()))
			// Hai cái trần, và cái BYTE mới là cái chạm trước.
			//
			// MaxVersions = 64 nghe như giới hạn thật, nhưng với value 40 byte
			// thì entry đã vượt btree.MaxEntrySize ở version thứ 41. Nếu chỉ
			// kiểm số version thì lỗi hiện ra dưới dạng "entry lớn quá, một
			// page không chứa nổi hai cái" — đúng triệu chứng, sai nguyên
			// nhân, và người đọc log sẽ đi tìm bug ở B+Tree.
			if n := 2 + len(key) + len(buf); len(nc) > MaxVersions || n > btree.MaxEntrySize {
				return fmt.Errorf("%w: khóa %q có %d version / %d byte (trần %d version, %d byte), horizon=%d",
					ErrChainFull, key, len(nc), n, MaxVersions, btree.MaxEntrySize, horizon)
			}
			if err := ptx.Put(key, buf); err != nil {
				return fmt.Errorf("txn: ghi khóa %q (%d version, %d byte): %w",
					key, len(nc), len(buf), err)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	t.s.st.verWritten.Add(written)
	t.s.st.verPruned.Add(pruned)
	t.s.st.keysReclaimed.Add(reclaimed)
	return nil
}
