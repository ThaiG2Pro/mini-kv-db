package db

import (
	"encoding/binary"
	"fmt"

	"minidb/internal/pager"
	"minidb/internal/wal"
)

// Checkpoint là thứ duy nhất giới hạn thời gian recovery.
//
// Không có checkpoint, recovery phải đọc log từ byte đầu tiên của đời database.
// Checkpoint ghi vào log hai bảng — transaction đang chạy (ATT) và page bẩn
// (DPT) — rồi nói với header của log: "lần sau bắt đầu đọc từ đây".
//
// Bản này là checkpoint MỜ (fuzzy): nó KHÔNG bắt mọi page bẩn phải xuống đĩa
// trước. Đó là khác biệt đáng giá nhất giữa ARIES và các sơ đồ trước nó — một
// checkpoint "sắc" phải dừng mọi ghi và flush cả buffer pool, tức là một cú
// khựng tỉ lệ với kích thước pool. Cái giá của fuzzy: recovery phải bắt đầu
// redo từ recLSN NHỎ NHẤT trong DPT, có thể nằm xa trước checkpoint.
func (d *DB) Checkpoint() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.checkpointLocked(false)
}

// CheckpointFlush là checkpoint có flush toàn bộ page bẩn. Recovery sau đó chỉ
// còn phải đọc phần log sinh ra sau nó. Dùng lúc đóng file và sau recovery.
func (d *DB) CheckpointFlush() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.checkpointLocked(true)
}

func (d *DB) maybeCheckpoint() error {
	limit := d.opt.ckptBytes()
	if limit < 0 {
		return nil
	}
	if int64(d.log.End()-d.lastCkptEnd) < limit {
		return nil
	}
	return d.checkpointLocked(false)
}

func (d *DB) checkpointLocked(flush bool) error {
	begin, err := d.log.Append(&wal.Record{Type: wal.TypeCkptBegin})
	if err != nil {
		return err
	}
	// Flush TRƯỚC khi chụp bảng, không phải sau.
	//
	// Đã dính đúng bug này: chụp trước rồi mới flush thì CKPT-END mang một DPT
	// đã lỗi thời, đầy recLSN cũ. Recovery lấy recLSN nhỏ nhất trong bảng đó
	// làm điểm bắt đầu redo, nên nó quét lại y nguyên đoạn log mà checkpoint
	// vừa làm cho thành thừa — checkpoint có flush mà không rút ngắn redo được
	// một record nào. Đo được: 844 record cả khi có lẫn khi không checkpoint.
	if flush {
		if err := d.pool.FlushAll(); err != nil {
			return err
		}
		d.dpt = map[pager.PageID]uint64{}
	}

	// Chụp hai bảng SAU CKPT-BEGIN: mọi thay đổi sau mốc này đều nằm trong
	// phần log mà recovery sẽ đọc lại, nên bỏ sót chúng khỏi bảng cũng không
	// sao. Bỏ sót một thay đổi TRƯỚC mốc thì mới chết.
	payload := encodeCkpt(d.att, d.dpt)
	if _, err := d.log.Append(&wal.Record{Type: wal.TypeCkptEnd, Payload: payload}); err != nil {
		return err
	}
	if err := d.log.Sync(); err != nil {
		return err
	}

	// Meta page giữ trạng thái CẤP PHÁT + root. Từ phase 5 nó không còn là
	// ảnh chụp nhất quán của dữ liệu; nó chỉ nói cho recovery biết file dài
	// bao nhiêu page, page nào rảnh, và cây bắt đầu từ đâu.
	if err := d.pg.CommitMeta(d.root); err != nil {
		return err
	}

	// Master record: SAU khi CKPT-END đã durable. Ngược lại thì một lần crash
	// xen giữa để lại header trỏ tới checkpoint không tồn tại.
	if err := d.log.SetCheckpoint(begin); err != nil {
		return err
	}
	d.lastCkptEnd = d.log.End()
	// Mốc chống torn write là checkpoint: page nào bị chạm sau đây phải ghi
	// trọn ảnh một lần nữa.
	d.fpw = map[pager.PageID]bool{}
	d.Checkpoints++
	return nil
}

// ---------- mã hóa hai bảng ----------
//
//	u32 nATT | nATT * (u64 txnID, u64 lastLSN)
//	u32 nDPT | nDPT * (u32 pageID, u64 recLSN)

func encodeCkpt(att map[uint64]uint64, dpt map[pager.PageID]uint64) []byte {
	buf := make([]byte, 0, 8+16*len(att)+12*len(dpt))
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(att)))
	for id, lsn := range att {
		buf = binary.LittleEndian.AppendUint64(buf, id)
		buf = binary.LittleEndian.AppendUint64(buf, lsn)
	}
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(dpt)))
	for id, lsn := range dpt {
		buf = binary.LittleEndian.AppendUint32(buf, uint32(id))
		buf = binary.LittleEndian.AppendUint64(buf, lsn)
	}
	return buf
}

func decodeCkpt(p []byte) (map[uint64]uint64, map[pager.PageID]uint64, error) {
	if len(p) < 4 {
		return nil, nil, fmt.Errorf("db: CKPT-END payload ngắn (%d byte)", len(p))
	}
	n := int(binary.LittleEndian.Uint32(p))
	off := 4
	if len(p) < off+16*n+4 {
		return nil, nil, fmt.Errorf("db: CKPT-END thiếu bảng ATT")
	}
	att := make(map[uint64]uint64, n)
	for i := 0; i < n; i++ {
		att[binary.LittleEndian.Uint64(p[off:])] = binary.LittleEndian.Uint64(p[off+8:])
		off += 16
	}
	m := int(binary.LittleEndian.Uint32(p[off:]))
	off += 4
	if len(p) < off+12*m {
		return nil, nil, fmt.Errorf("db: CKPT-END thiếu bảng DPT")
	}
	dpt := make(map[pager.PageID]uint64, m)
	for i := 0; i < m; i++ {
		dpt[pager.PageID(binary.LittleEndian.Uint32(p[off:]))] = binary.LittleEndian.Uint64(p[off+4:])
		off += 12
	}
	return att, dpt, nil
}
