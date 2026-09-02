package db

import (
	"encoding/binary"

	"minidb/internal/page"
	"minidb/internal/pager"
	"minidb/internal/wal"
)

// journal cài btree.Journal: biến "cây vừa sửa page X" thành log record.
//
// Nguyên tắc gói gọn trong một câu: MỘT page bị chạm trong MỘT lệnh sinh ĐÚNG
// MỘT record. Không phải một record cho mỗi lần InsertAt — giữa hai lần
// InsertAt của cùng một cú split, page ở trạng thái không hợp lệ, và một
// recovery dừng đúng giữa đó sẽ dựng lại một page vô nghĩa. Chốt ở ranh giới
// pin/unpin của cây là chốt ở ranh giới mà cây tự đảm bảo page hợp lệ.
type journal struct {
	db   *DB
	tx   *Txn
	on   bool
	snap map[pager.PageID]*snapEntry
	pool [][]byte // ảnh trước dùng lại, tránh cấp phát 4KB mỗi lần pin
	buf  []byte   // payload dựng tạm
	err  error

	Records, DiffBytes, FullPages int64
	SnapTaken                     int64
}

type snapEntry struct {
	live  page.Page
	img   []byte
	depth int
}

func newJournal(d *DB) *journal {
	return &journal{db: d, snap: make(map[pager.PageID]*snapEntry, 16)}
}

// enter/leave đóng mở cửa sổ ghi log.
//
// Vì sao cần cờ này thay vì cứ để journal luôn bật: t.pin() của cây dùng chung
// cho cả đường ĐỌC (Get, cursor). Nếu PageIn luôn chụp ảnh thì mỗi Get tốn một
// memcpy 4KB — biến một tra cứu ~1µs thành ~1.4µs cho một bản sao không ai
// dùng. Đường đọc không sinh log, nên nó không cần ảnh trước.
func (j *journal) enter(tx *Txn) {
	j.tx = tx
	j.on = true
	j.err = nil
}

func (j *journal) leave() {
	j.on = false
	// Mọi page phải đã được PageOut. Còn sót nghĩa là cây rò rỉ pin — để lại
	// ảnh trước ở đây sẽ làm lệnh SAU diff nhầm với một mốc quá cũ.
	for id, e := range j.snap {
		j.release(e)
		delete(j.snap, id)
	}
	j.tx = nil
}

func (j *journal) Err() error { return j.err }

func (j *journal) fail(err error) {
	if j.err == nil && err != nil {
		j.err = err
	}
}

func (j *journal) take() []byte {
	if n := len(j.pool); n > 0 {
		b := j.pool[n-1]
		j.pool = j.pool[:n-1]
		return b
	}
	return make([]byte, page.PageSize)
}

func (j *journal) release(e *snapEntry) {
	if e.img != nil {
		j.pool = append(j.pool, e.img)
		e.img = nil
	}
}

// ---------- btree.Journal ----------

func (j *journal) PageIn(id pager.PageID, p page.Page) {
	if !j.on {
		return
	}
	if e, ok := j.snap[id]; ok {
		e.depth++
		return
	}
	img := j.take()
	copy(img, p)
	j.snap[id] = &snapEntry{live: p, img: img, depth: 1}
	j.SnapTaken++
}

func (j *journal) PageOut(id pager.PageID, dirty bool) bool {
	if !j.on {
		return false
	}
	e, ok := j.snap[id]
	if !ok {
		return false
	}
	e.depth--
	if e.depth > 0 {
		return false
	}
	delete(j.snap, id)
	changed := j.emit(id, e)
	if dirty && !changed {
		// Cây báo bẩn nhưng byte không đổi (SetAt ghi đè đúng giá trị cũ).
		// Không có gì để redo, nhưng pool vẫn coi page là bẩn nên nó phải nằm
		// trong DPT — thiếu nó thì redoLSN tính ra quá muộn.
		j.noteDirty(id, j.db.log.End())
	}
	j.release(e)
	return changed
}

// emit so ảnh trước với nội dung hiện tại và ghi một record UPDATE. Trả về
// true nếu thật sự có thay đổi (và đã ghi record).
//
// Cờ dirty của cây KHÔNG được dùng ở đây. Đây là chỗ đã dính một bug tốn thời
// gian: khi một leaf bị xóa khóa rồi bị gộp vào anh em bên trái, cây thả pin
// nó với dirty=false (nó sắp chết, ghi xuống đĩa làm gì) — nên không record
// nào ghi lại lần xóa ấy, và Abort không có gì để quay ngược. Khóa bị xóa
// không bao giờ quay lại. Từ đây, "page có đổi không" chỉ có một nguồn sự
// thật: so byte.
func (j *journal) emit(id pager.PageID, e *snapEntry) bool {
	segs := wal.Diff(e.img, e.live, wal.DiffGran)
	if len(segs) == 0 {
		return false
	}
	// Ảnh TRƯỚC luôn là diff (undo chạy sau redo, lúc page đã lành). Chỉ ảnh
	// SAU mới cần trọn page, và chỉ ở lần chạm đầu tiên sau mỗi checkpoint.
	after := segs
	var flags uint8
	full := j.db.log.FullPageWrites && !j.db.fpw[id]
	if full {
		after = []wal.Seg{{Off: 0, Len: page.PageSize}}
		flags = wal.FlagFullPage
	}
	j.buf = wal.EncodePayload(j.buf[:0], segs, after, e.img, e.live)
	lsn, err := j.db.log.Append(&wal.Record{
		Type:    wal.TypeUpdate,
		PageID:  id,
		TxnID:   j.tx.id,
		PrevLSN: j.tx.prev,
		Flags:   flags,
		Payload: j.buf,
	})
	if err != nil {
		j.fail(err)
		return false
	}
	j.tx.prev = lsn
	j.db.att[j.tx.id] = lsn
	// pageLSN đặt SAU khi có LSN và TRƯỚC khi thả pin: từ lúc thả pin, pool
	// được phép ghi page này xuống đĩa, và FlushLog sẽ so đúng con số vừa đặt.
	e.live.SetLSN(lsn)
	j.noteDirty(id, lsn)
	if full {
		j.db.fpw[id] = true
		j.FullPages++
	}
	j.Records++
	j.DiffBytes += int64(wal.SegBytes(segs))
	return true
}

// noteDirty ghi page vào DPT với recLSN = thay đổi ĐẦU TIÊN chưa nằm trên đĩa.
// Không ghi đè entry cũ: recLSN phải là cái sớm nhất, vì redo bắt đầu từ đó.
func (j *journal) noteDirty(id pager.PageID, lsn uint64) {
	if _, ok := j.db.dpt[id]; !ok {
		j.db.dpt[id] = lsn
	}
}

func (j *journal) PageAlloc(id pager.PageID, typ uint8, p page.Page) {
	if !j.on {
		return
	}
	lsn, err := j.db.log.Append(&wal.Record{
		Type:    wal.TypeAlloc,
		PageID:  id,
		TxnID:   j.tx.id,
		PrevLSN: j.tx.prev,
		Payload: []byte{typ},
	})
	if err != nil {
		j.fail(err)
		return
	}
	j.tx.prev = lsn
	j.db.att[j.tx.id] = lsn
	p.SetLSN(lsn)
	j.noteDirty(id, lsn)
	// Page vừa Init: redo của ALLOC dựng lại nguyên vẹn nội dung đó vô điều
	// kiện, nên nó đã là một "full page write" rồi. Đánh dấu để lần sửa đầu
	// tiên sau đây chỉ cần ghi diff.
	j.db.fpw[id] = true
	j.Records++
}

func (j *journal) PageFree(id pager.PageID, p page.Page) {
	if !j.on {
		return
	}
	// Payload = trọn ảnh page tại thời điểm chết. Undo của FREE dán ảnh này
	// trở lại, đưa page về đúng trạng thái-lúc-crash để chuỗi ảnh-trước phía
	// trước nó áp lên được. Xem btree.freePage.
	segs := []wal.Seg{{Off: 0, Len: page.PageSize}}
	j.buf = wal.EncodePayload(j.buf[:0], nil, segs, nil, p)
	lsn, err := j.db.log.Append(&wal.Record{
		Type:    wal.TypeFree,
		PageID:  id,
		TxnID:   j.tx.id,
		PrevLSN: j.tx.prev,
		Flags:   wal.FlagFullPage,
		Payload: j.buf,
	})
	if err != nil {
		j.fail(err)
		return
	}
	j.tx.prev = lsn
	j.db.att[j.tx.id] = lsn
	// Page đã chết: bỏ khỏi DPT (pool cũng vứt cờ dirty của nó) và khỏi bảng
	// full-page — nếu nó được cấp lại, ALLOC sẽ đặt lại từ đầu.
	delete(j.db.dpt, id)
	delete(j.db.fpw, id)
	j.Records++
}

func (j *journal) RootChanged(old, nw pager.PageID) {
	if !j.on {
		return
	}
	var pl [8]byte
	binary.LittleEndian.PutUint32(pl[0:], uint32(old))
	binary.LittleEndian.PutUint32(pl[4:], uint32(nw))
	lsn, err := j.db.log.Append(&wal.Record{
		Type:    wal.TypeRoot,
		TxnID:   j.tx.id,
		PrevLSN: j.tx.prev,
		Payload: pl[:],
	})
	if err != nil {
		j.fail(err)
		return
	}
	j.tx.prev = lsn
	j.db.att[j.tx.id] = lsn
	j.db.root = nw
	j.Records++
}

// rootOf tách payload của record ROOT.
func rootOf(p []byte) (old, nw pager.PageID, ok bool) {
	if len(p) != 8 {
		return 0, 0, false
	}
	return pager.PageID(binary.LittleEndian.Uint32(p[0:])),
		pager.PageID(binary.LittleEndian.Uint32(p[4:])), true
}
