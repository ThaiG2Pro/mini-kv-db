package db

import (
	"container/heap"
	"fmt"

	"minidb/internal/page"
	"minidb/internal/pager"
	"minidb/internal/wal"
)

// Recovery ba pha của ARIES, rút gọn.
//
//	1. ANALYSIS  — đọc từ checkpoint tới hết log, dựng lại ATT (ai còn dở dang)
//	               và DPT (page nào có thay đổi chưa chắc đã xuống đĩa).
//	2. REDO      — *repeat history*: áp lại MỌI thay đổi từ recLSN nhỏ nhất,
//	               kể cả của transaction sẽ bị hủy ở pha 3.
//	3. UNDO      — quay ngược transaction chưa commit, mỗi bước ghi một CLR.
//
// Chỗ khiến ARIES khác các sơ đồ trước nó nằm ở pha 2: redo cả những thay đổi
// sắp bị undo. Nghe như phí, nhưng nó đưa database về đúng trạng thái lúc
// crash, và nhờ vậy pha undo chỉ cần một loại thao tác duy nhất — quay ngược
// từ trạng thái-lúc-crash — thay vì phải suy luận xem thay đổi nào đã kịp
// xuống đĩa. Nó cũng là điều kiện để recovery tự nó chịu được crash: chạy lại
// nửa chừng vẫn ra cùng kết quả.

func (d *DB) recover() error {
	ckpt := d.log.Checkpoint()
	start := ckpt
	if start < wal.FirstLSN {
		start = wal.FirstLSN
	}

	// ---------- pha 1: ANALYSIS ----------
	att := map[uint64]uint64{}
	dpt := map[pager.PageID]uint64{}
	// lastEv giữ SỰ KIỆN CUỐI CÙNG về vòng đời của mỗi page: nó được cấp hay
	// được trả, và bởi transaction nào.
	//
	// Phải là "cuối cùng", không phải hai tập rời nhau. Đã dính đúng bug đó:
	// bản đầu giữ một tập allocSeen (đem Reserve) và một danh sách frees (đem
	// FreeNow), rồi chạy hai vòng nối nhau. Một page bị free ở LSN 100 rồi cấp
	// lại ở LSN 200 có mặt trong CẢ HAI, và vòng FreeNow chạy sau nên nó rơi
	// vào freelist trong khi đang là một node của cây. Ngay sau đó,
	// checkpoint cuối pha recovery cấp nó làm page chứa freelist và ghi đè lên
	// — cây hỏng SAU KHI recovery báo thành công. Triệu chứng: crashlab sai
	// 12/30 vòng với "slot 0 trỏ ra ngoài (off=7 len=0)".
	type pageEvent struct {
		freed bool
		txn   uint64
	}
	lastEv := map[pager.PageID]pageEvent{}
	root := d.pg.Root()
	maxPage := uint32(0)
	maxTxn := uint64(0)
	sawAny := false

	err := d.log.Scan(start, func(r *wal.Record) error {
		sawAny = true
		if r.TxnID > maxTxn {
			maxTxn = r.TxnID
		}
		if uint32(r.PageID) > maxPage {
			maxPage = uint32(r.PageID)
		}
		switch r.Type {
		case wal.TypeCkptEnd:
			a, p, err := decodeCkpt(r.Payload)
			if err != nil {
				return err
			}
			for k, v := range a {
				att[k] = v
				if k > maxTxn {
					maxTxn = k
				}
			}
			for k, v := range p {
				if _, ok := dpt[k]; !ok {
					dpt[k] = v
				}
				if uint32(k) > maxPage {
					maxPage = uint32(k)
				}
			}
		case wal.TypeBegin:
			att[r.TxnID] = r.LSN
		case wal.TypeCommit, wal.TypeAbort:
			delete(att, r.TxnID)
		case wal.TypeUpdate, wal.TypeCLR:
			att[r.TxnID] = r.LSN
			if _, ok := dpt[r.PageID]; !ok {
				dpt[r.PageID] = r.LSN
			}
		case wal.TypeAlloc:
			att[r.TxnID] = r.LSN
			lastEv[r.PageID] = pageEvent{freed: false, txn: r.TxnID}
			if _, ok := dpt[r.PageID]; !ok {
				dpt[r.PageID] = r.LSN
			}
		case wal.TypeFree:
			att[r.TxnID] = r.LSN
			delete(dpt, r.PageID)
			lastEv[r.PageID] = pageEvent{freed: true, txn: r.TxnID}
		case wal.TypeRoot:
			att[r.TxnID] = r.LSN
			if _, nw, ok := rootOf(r.Payload); ok {
				root = nw
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("analysis: %w", err)
	}
	if !sawAny && d.pg.Root() == 0 {
		// Log rỗng và file rỗng: database mới toanh, không có gì để phục hồi.
		d.nextTxn = 0
		return nil
	}
	d.nextTxn = maxTxn

	// File có thể ngắn hơn những gì log nói tới: page cấp sau lần checkpoint
	// cuối không nằm trong pageCount của meta. Không nới ra trước thì
	// ReadPage/WritePage chặn đúng những page cần redo.
	if maxPage >= 2 {
		if err := d.pg.Extend(maxPage + 1); err != nil {
			return fmt.Errorf("analysis: nới file tới %d page: %w", maxPage+1, err)
		}
	}

	// skipRedo: những page mà redo TUYỆT ĐỐI không được chạm.
	//
	// Đây là chỗ hai hệ thống quản lý page giẫm lên nhau, và là bug tốn nhiều
	// thời gian nhất của phase 5. WAL bảo vệ nội dung page dữ liệu; freelist
	// của pager thì KHÔNG được log (nó đi theo cơ chế meta page của phase 1).
	// Hai bên dùng chung một không gian pageID. Kịch bản:
	//
	//	1. page 45 là một leaf, bị xóa hết khóa -> FREE (đã commit);
	//	2. checkpoint: pager cấp chính 45 làm page CHỨA freelist, ghi meta;
	//	3. crash;
	//	4. redo bắt đầu từ recLSN nhỏ nhất, tức TRƯỚC checkpoint, và gặp lại
	//	   các record UPDATE cũ của 45 hồi nó còn là leaf;
	//	5. phép so pageLSN không cứu được: 45 giờ chứa freelist, "pageLSN" đọc
	//	   ra là một id page rảnh nào đó, thường nhỏ hơn LSN cần redo;
	//	6. redo ghi nội dung leaf cũ lên chuỗi freelist. Lần mở file sau:
	//	   "đọc freelist page 4096: EOF" — 4096 chính là cellStart của một
	//	   page B+Tree rỗng bị đọc nhầm thành trường `next`.
	//
	// Hai nhóm phải loại: page đang là host của freelist, và page mà log nói
	// đã được free bởi một transaction ĐÃ COMMIT (nội dung của nó vô nghĩa, và
	// pager có thể đã tái dùng nó cho việc khác).
	skipRedo := map[pager.PageID]bool{}
	for _, id := range d.pg.MetaPending() {
		skipRedo[id] = true
	}
	for id, ev := range lastEv {
		if _, loser := att[ev.txn]; ev.freed && !loser {
			skipRedo[id] = true
		}
	}

	// recLSN nhỏ nhất là điểm bắt đầu redo. Đây chính là chỗ checkpoint mờ
	// phải trả giá: một page bẩn từ lâu mà chưa ai flush kéo điểm này lùi lại.
	redoLSN := start
	for _, l := range dpt {
		if l < redoLSN {
			redoLSN = l
		}
	}

	// ---------- pha 2: REDO ----------
	if err := d.redo(redoLSN, skipRedo); err != nil {
		return fmt.Errorf("redo: %w", err)
	}

	// ---------- pha 3: UNDO ----------
	d.LoserTxns = int64(len(att))
	d.root = root
	orphans, err := d.undoChain(att)
	if err != nil {
		return fmt.Errorf("undo: %w", err)
	}
	root = d.root

	// Chốt lại trạng thái cấp phát bằng MỘT vòng duy nhất, theo sự kiện cuối
	// cùng của từng page. Freelist trong meta là ảnh cũ từ lần checkpoint
	// trước, nên nó vừa có thể liệt kê page đang sống, vừa có thể thiếu page
	// đã chết; log mới là nguồn sự thật.
	for id, ev := range lastEv {
		_, loser := att[ev.txn]
		if ev.freed && !loser {
			// Free đã commit -> có hiệu lực thật.
			if err := d.pool.Discard(id); err != nil {
				return err
			}
			if err := d.pg.FreeNow(id); err != nil {
				return fmt.Errorf("trả page %d về freelist: %w", id, err)
			}
			continue
		}
		// Đang sống: hoặc vừa được cấp, hoặc lệnh free của nó thuộc một
		// transaction thua cuộc và đã bị undo. Rút nó khỏi freelist cũ.
		d.pg.Reserve(id)
	}
	// Page mà một transaction thua cuộc đã cấp: ALLOC là redo-only nên không
	// có bước undo nào trả nó lại. Sau khi undo xong thì không còn con trỏ nào
	// tới nó -> thu hồi ngay. Đây là chỗ nợ P1-1 (page mồ côi sau rollback)
	// được trả cho đường đi có WAL. Phải chạy SAU vòng trên, vì vòng trên vừa
	// Reserve chính những page này.
	for _, id := range orphans {
		if err := d.pool.Discard(id); err != nil {
			return fmt.Errorf("undo: dọn page mồ côi %d khỏi pool: %w", id, err)
		}
		if err := d.pg.FreeNow(id); err != nil {
			return fmt.Errorf("undo: thu hồi page mồ côi %d: %w", id, err)
		}
		d.OrphanPages++
	}

	d.att = map[uint64]uint64{}
	d.root = root
	d.Recoveries++

	// Chốt ngay một checkpoint có flush: recovery vừa xong là lúc rẻ nhất để
	// làm việc đó (mọi thứ đang trong pool), và nó đảm bảo lần crash tiếp theo
	// không phải đọc lại đúng đoạn log này lần nữa.
	return d.checkpointLocked(true)
}

// redo áp lại mọi thay đổi từ redoLSN. Idempotent nhờ pageLSN: page nào đã
// mang LSN >= LSN của record thì đã có thay đổi đó rồi.
func (d *DB) redo(redoLSN uint64, skip map[pager.PageID]bool) error {
	return d.log.Scan(redoLSN, func(r *wal.Record) error {
		if r.PageID != 0 && skip[r.PageID] {
			d.RedoSkipped++
			return nil
		}
		switch r.Type {
		case wal.TypeAlloc:
			if len(r.Payload) != 1 {
				return fmt.Errorf("ALLOC lsn=%d payload %d byte", r.LSN, len(r.Payload))
			}
			f, err := d.pool.Pin(r.PageID)
			if err != nil {
				return err
			}
			// Vô điều kiện, không so pageLSN: nội dung cũ của một page vừa
			// cấp là rác, và pageLSN đọc từ rác cũng là rác. Redo tuần tự từ
			// redoLSN sẽ dựng lại toàn bộ lịch sử sau đó nên không mất gì.
			page.Init(f.Data, r.Payload[0])
			f.Data.SetLSN(r.LSN)
			d.RedoApplied++
			return d.pool.Unpin(r.PageID, true)

		case wal.TypeUpdate, wal.TypeCLR:
			_, segs, _, after, err := wal.DecodePayload(r.Payload)
			if err != nil {
				return fmt.Errorf("lsn=%d: %w", r.LSN, err)
			}
			f, err := d.pool.Pin(r.PageID)
			if err != nil {
				return err
			}
			// FlagFullPage: áp vô điều kiện. Đây là lý do tồn tại của cả cơ
			// chế — một page bị torn write có pageLSN là rác, có thể lớn hơn
			// LSN cần redo, và phép so sánh dưới đây sẽ bỏ qua đúng cái page
			// đang hỏng. Ảnh trọn page ghi đè lên nó và đặt lại pageLSN tin
			// được.
			if r.Flags&wal.FlagFullPage == 0 && f.Data.LSN() >= r.LSN {
				d.RedoSkipped++
				return d.pool.Unpin(r.PageID, false)
			}
			if err := wal.Apply(f.Data, segs, after); err != nil {
				d.pool.Unpin(r.PageID, false)
				return fmt.Errorf("lsn=%d page=%d: %w", r.LSN, r.PageID, err)
			}
			f.Data.SetLSN(r.LSN)
			d.RedoApplied++
			return d.pool.Unpin(r.PageID, true)

		case wal.TypeRoot:
			if _, nw, ok := rootOf(r.Payload); ok {
				d.root = nw
			}
		}
		return nil
	})
}

// ---------- undo dùng chung cho Abort và recovery ----------

type undoItem struct {
	lsn uint64
	txn uint64
}

type undoHeap []undoItem

func (h undoHeap) Len() int           { return len(h) }
func (h undoHeap) Less(i, j int) bool { return h[i].lsn > h[j].lsn } // LSN lớn nhất trước
func (h undoHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *undoHeap) Push(x any)        { *h = append(*h, x.(undoItem)) }
func (h *undoHeap) Pop() any          { o := *h; n := len(o); x := o[n-1]; *h = o[:n-1]; return x }

// undoChain quay ngược một tập transaction, theo thứ tự LSN GIẢM DẦN trên toàn
// bộ các transaction cùng lúc (không phải hết đứa này rồi tới đứa kia).
//
// Vì sao phải trộn: hai transaction có thể đã sửa cùng một page xen kẽ nhau.
// Undo theo thứ tự thời gian ngược là cách duy nhất để mỗi lần khôi phục
// ảnh-trước rơi đúng vào trạng thái mà ảnh đó được chụp.
//
// Mỗi bước undo ghi một CLR trước khi sửa page. CLR có UndoNext trỏ tới record
// còn phải undo tiếp, và CLR KHÔNG BAO GIỜ bị undo. Nhờ đó recovery chết giữa
// pha undo rồi chạy lại sẽ không undo hai lần — vấn đề đã giết những sơ đồ
// recovery trước ARIES.
func (d *DB) undoChain(losers map[uint64]uint64) ([]pager.PageID, error) {
	h := &undoHeap{}
	heap.Init(h)
	last := make(map[uint64]uint64, len(losers))
	for txn, lsn := range losers {
		last[txn] = lsn
		if lsn != 0 {
			heap.Push(h, undoItem{lsn, txn})
		}
	}
	var orphans []pager.PageID

	for h.Len() > 0 {
		it := heap.Pop(h).(undoItem)
		r, err := d.log.Read(it.lsn)
		if err != nil {
			return nil, fmt.Errorf("đọc lsn=%d: %w", it.lsn, err)
		}
		next := r.PrevLSN

		switch {
		case r.UndoNext != 0:
			// Record đền bù: phần việc của nó đã xong, nhảy thẳng tới chỗ còn
			// dở. Đây là chỗ CLR khiến undo trở nên idempotent.
			next = r.UndoNext

		case r.Type == wal.TypeUpdate:
			segs, _, before, _, err := wal.DecodePayload(r.Payload)
			if err != nil {
				return nil, fmt.Errorf("lsn=%d: %w", r.LSN, err)
			}
			if len(segs) == 0 {
				return nil, fmt.Errorf("lsn=%d: UPDATE không có ảnh trước", r.LSN)
			}
			// CLR chỉ có phía redo: nó không bao giờ bị undo.
			clr, err := d.log.Append(&wal.Record{
				Type:     wal.TypeCLR,
				PageID:   r.PageID,
				TxnID:    r.TxnID,
				PrevLSN:  last[r.TxnID],
				UndoNext: r.PrevLSN,
				Payload:  wal.EncodePayloadRaw(nil, nil, segs, before),
			})
			if err != nil {
				return nil, err
			}
			f, perr := d.pool.Pin(r.PageID)
			if perr != nil {
				return nil, perr
			}
			if err := wal.Apply(f.Data, segs, before); err != nil {
				d.pool.Unpin(r.PageID, false)
				return nil, err
			}
			f.Data.SetLSN(clr)
			if _, ok := d.dpt[r.PageID]; !ok {
				d.dpt[r.PageID] = clr
			}
			if err := d.pool.Unpin(r.PageID, true); err != nil {
				return nil, err
			}
			last[r.TxnID] = clr
			d.UndoApplied++

		case r.Type == wal.TypeAlloc:
			orphans = append(orphans, r.PageID)

		case r.Type == wal.TypeFree:
			// Page chưa bao giờ vào freelist trên đĩa (ReleasePending chỉ chạy
			// khi commit), nên chỉ cần dựng lại NỘI DUNG của nó: dán trọn ảnh
			// đã ghi kèm record FREE. Không có bước này thì mọi ảnh-trước của
			// page đó ở phía trước sẽ được dán lên một bản đĩa cũ hơn.
			_, segs, _, img, err := wal.DecodePayload(r.Payload)
			if err != nil {
				return nil, fmt.Errorf("lsn=%d: %w", r.LSN, err)
			}
			clr, err := d.log.Append(&wal.Record{
				Type:     wal.TypeCLR,
				PageID:   r.PageID,
				TxnID:    r.TxnID,
				PrevLSN:  last[r.TxnID],
				UndoNext: r.PrevLSN,
				Flags:    wal.FlagFullPage,
				Payload:  wal.EncodePayloadRaw(nil, nil, segs, img),
			})
			if err != nil {
				return nil, err
			}
			f, perr := d.pool.Pin(r.PageID)
			if perr != nil {
				return nil, perr
			}
			if err := wal.Apply(f.Data, segs, img); err != nil {
				d.pool.Unpin(r.PageID, false)
				return nil, err
			}
			f.Data.SetLSN(clr)
			if _, ok := d.dpt[r.PageID]; !ok {
				d.dpt[r.PageID] = clr
			}
			if err := d.pool.Unpin(r.PageID, true); err != nil {
				return nil, err
			}
			last[r.TxnID] = clr
			d.UndoApplied++

		case r.Type == wal.TypeRoot:
			old, _, ok := rootOf(r.Payload)
			if !ok {
				return nil, fmt.Errorf("lsn=%d: ROOT payload hỏng", r.LSN)
			}
			// Ghi một ROOT đền bù (UndoNext != 0 nên lần undo sau bỏ qua nó).
			var pl [8]byte
			putRoot(pl[:], d.root, old)
			clr, err := d.log.Append(&wal.Record{
				Type:     wal.TypeRoot,
				TxnID:    r.TxnID,
				PrevLSN:  last[r.TxnID],
				UndoNext: r.PrevLSN,
				Payload:  pl[:],
			})
			if err != nil {
				return nil, err
			}
			d.root = old
			last[r.TxnID] = clr
			d.UndoApplied++

		case r.Type == wal.TypeBegin:
			ab, err := d.log.Append(&wal.Record{
				Type: wal.TypeAbort, TxnID: r.TxnID, PrevLSN: last[r.TxnID],
			})
			if err != nil {
				return nil, err
			}
			last[r.TxnID] = ab
			next = 0
		}

		if next != 0 {
			heap.Push(h, undoItem{next, it.txn})
		} else if r.Type != wal.TypeBegin {
			// Hết chuỗi mà chưa gặp BEGIN (transaction bắt đầu trước
			// checkpoint và BEGIN của nó đã bị cắt): vẫn phải đóng sổ bằng
			// một ABORT, nếu không lần recovery sau lại coi nó là dở dang.
			ab, err := d.log.Append(&wal.Record{
				Type: wal.TypeAbort, TxnID: r.TxnID, PrevLSN: last[r.TxnID],
			})
			if err != nil {
				return nil, err
			}
			last[r.TxnID] = ab
		}
	}
	return orphans, nil
}

func putRoot(dst []byte, old, nw pager.PageID) {
	dst[0], dst[1], dst[2], dst[3] = byte(old), byte(old>>8), byte(old>>16), byte(old>>24)
	dst[4], dst[5], dst[6], dst[7] = byte(nw), byte(nw>>8), byte(nw>>16), byte(nw>>24)
}
