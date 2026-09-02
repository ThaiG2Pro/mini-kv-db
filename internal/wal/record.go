// Package wal cài write-ahead log và các bản ghi của nó.
//
// Bất biến số 1 của cả database (roadmap phase 5): log record của một thay đổi
// phải nằm trên đĩa TRƯỚC khi page dữ liệu tương ứng được ghi. Package này chỉ
// lo phần "log"; chỗ ép buộc luật đó nằm ở bufpool.Pool.FlushLog, và chỗ dùng
// nó nằm ở internal/db.
//
// Ba quyết định định hình toàn bộ file này:
//
//  1. **LSN = offset byte trong file log.** Không có bảng tra LSN -> vị trí,
//     Read(lsn) là một pread. Postgres làm y hệt. Cái giá: log không nén được
//     và không đánh số lại được sau khi cắt bớt.
//  2. **Physiological logging**: redo theo page (byte range trong đúng một
//     page), undo theo page. Không log "chèn khóa K vào cây" (logical) vì undo
//     logical đòi cây phải nhất quán ở mọi thời điểm — thứ không đúng giữa
//     chừng một cú split.
//  3. **Mỗi record tự kiểm tra được bằng crc32c.** Đuôi log sau khi crash gần
//     như luôn là một record ghi dở; nó phải bị phát hiện là rác chứ không
//     được diễn giải bừa. Đây là chỗ duy nhất phân biệt "log đọc được" với
//     "log an toàn".
package wal

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"

	"minidb/internal/pager"
)

var crcTable = crc32.MakeTable(crc32.Castagnoli)

// Type là loại log record.
type Type uint8

const (
	TypeBegin     Type = 1 // mở transaction
	TypeUpdate    Type = 2 // sửa byte trong một page (có cả before và after)
	TypeCLR       Type = 3 // compensation: kết quả của một bước undo, chỉ có after
	TypeCommit    Type = 4
	TypeAbort     Type = 5
	TypeAlloc     Type = 6 // cấp một page mới (payload: 1 byte kiểu page)
	TypeFree      Type = 7 // trả page về freelist
	TypeRoot      Type = 8 // root của B+Tree đổi (payload: old u32, new u32)
	TypeCkptBegin Type = 9
	TypeCkptEnd   Type = 10 // payload: bảng ATT + DPT tại lúc bắt đầu checkpoint
)

func (t Type) String() string {
	switch t {
	case TypeBegin:
		return "BEGIN"
	case TypeUpdate:
		return "UPDATE"
	case TypeCLR:
		return "CLR"
	case TypeCommit:
		return "COMMIT"
	case TypeAbort:
		return "ABORT"
	case TypeAlloc:
		return "ALLOC"
	case TypeFree:
		return "FREE"
	case TypeRoot:
		return "ROOT"
	case TypeCkptBegin:
		return "CKPT-BEGIN"
	case TypeCkptEnd:
		return "CKPT-END"
	}
	return fmt.Sprintf("TYPE(%d)", uint8(t))
}

// Cờ trong header.
const (
	// FlagFullPage: payload chứa TRỌN page (before + after 4096 byte) chứ
	// không phải mấy đoạn diff. Redo của nó được áp dụng VÔ ĐIỀU KIỆN, không
	// so pageLSN.
	//
	// Vì sao cần: nếu page trên đĩa bị torn write, pageLSN đọc lên là rác —
	// có thể lớn hơn LSN của record cần redo, và redo sẽ bỏ qua đúng cái page
	// đang hỏng. Ghi trọn page ở lần chạm đầu tiên sau mỗi checkpoint phá vỡ
	// vòng luẩn quẩn đó: bản đầy đủ đè lên, pageLSN trở lại tin được. Đây
	// chính là full_page_writes của Postgres, và là câu trả lời cho nợ P2-3.
	FlagFullPage uint8 = 1 << 0
)

// Không có cờ "có ảnh trước / có ảnh sau": số đoạn trong payload đã nói điều
// đó rồi (xem diff.go). Hai nguồn sự thật cho cùng một sự việc là hai chỗ để
// lệch nhau.

// Layout header (little-endian). Cố định 44 byte.
const (
	offTotalLen = 0  // u32 tổng độ dài record, kể cả header và crc
	offLSN      = 4  // u64 — cũng là offset của chính record này trong file
	offPrevLSN  = 12 // u64 record trước của CÙNG transaction (0 = không có)
	offTxnID    = 20 // u64
	offUndoNext = 28 // u64 chỉ dùng cho CLR: undo tiếp tục từ LSN nào
	offPageID   = 36 // u32
	offType     = 40 // u8
	offFlags    = 41 // u8
	offPad      = 42 // u16

	// HeaderSize/TrailerSize: mọi record = header + payload + crc32c.
	HeaderSize  = 44
	TrailerSize = 4
)

// MaxRecordSize chặn trên để một totalLen rác (đọc từ đuôi log hỏng) không
// làm ta cấp phát một slice khổng lồ. 2 page + header là đủ cho record to
// nhất thật sự tồn tại: full-page write mang cả before lẫn after.
const MaxRecordSize = 2*4096 + 512

var (
	ErrShortRecord = errors.New("wal: record ngắn hơn header")
	ErrBadLength   = errors.New("wal: totalLen vô lý")
	ErrBadCRC      = errors.New("wal: crc sai")
)

// Record là một bản ghi log đã giải mã. Payload trỏ THẲNG vào buffer nguồn khi
// giải mã từ buffer trong RAM — người gọi phải tự chép nếu muốn giữ lâu.
type Record struct {
	LSN      uint64
	PrevLSN  uint64
	TxnID    uint64
	UndoNext uint64
	PageID   pager.PageID
	Type     Type
	Flags    uint8
	Payload  []byte
}

// Size là số byte record chiếm trên đĩa.
func (r *Record) Size() int { return HeaderSize + len(r.Payload) + TrailerSize }

// Encode nối record vào dst và trả về slice mới. lsn được ghi vào header ở đây
// chứ không phải ở người gọi: LSN là offset, nên chỉ Log mới biết nó.
func Encode(dst []byte, r *Record, lsn uint64) []byte {
	total := r.Size()
	base := len(dst)
	dst = append(dst, make([]byte, total)...)
	b := dst[base:]
	binary.LittleEndian.PutUint32(b[offTotalLen:], uint32(total))
	binary.LittleEndian.PutUint64(b[offLSN:], lsn)
	binary.LittleEndian.PutUint64(b[offPrevLSN:], r.PrevLSN)
	binary.LittleEndian.PutUint64(b[offTxnID:], r.TxnID)
	binary.LittleEndian.PutUint64(b[offUndoNext:], r.UndoNext)
	binary.LittleEndian.PutUint32(b[offPageID:], uint32(r.PageID))
	b[offType] = uint8(r.Type)
	b[offFlags] = r.Flags
	copy(b[HeaderSize:], r.Payload)
	binary.LittleEndian.PutUint32(b[total-TrailerSize:], crc32.Checksum(b[:total-TrailerSize], crcTable))
	return dst
}

// Decode đọc một record từ đầu buf. Trả về record và số byte đã tiêu thụ.
//
// Mọi lỗi ở đây đều có nghĩa "từ đây trở đi log không đọc được nữa", không
// phải "bỏ qua record này rồi đi tiếp": LSN là offset, mất đồng bộ một byte là
// mất cả phần đuôi.
func Decode(buf []byte) (Record, int, error) {
	if len(buf) < HeaderSize+TrailerSize {
		return Record{}, 0, ErrShortRecord
	}
	total := int(binary.LittleEndian.Uint32(buf[offTotalLen:]))
	if total < HeaderSize+TrailerSize || total > MaxRecordSize {
		return Record{}, 0, fmt.Errorf("%w: %d", ErrBadLength, total)
	}
	if len(buf) < total {
		return Record{}, 0, ErrShortRecord
	}
	b := buf[:total]
	want := binary.LittleEndian.Uint32(b[total-TrailerSize:])
	if got := crc32.Checksum(b[:total-TrailerSize], crcTable); got != want {
		return Record{}, 0, fmt.Errorf("%w: file=%#x tính lại=%#x", ErrBadCRC, want, got)
	}
	return Record{
		LSN:      binary.LittleEndian.Uint64(b[offLSN:]),
		PrevLSN:  binary.LittleEndian.Uint64(b[offPrevLSN:]),
		TxnID:    binary.LittleEndian.Uint64(b[offTxnID:]),
		UndoNext: binary.LittleEndian.Uint64(b[offUndoNext:]),
		PageID:   pager.PageID(binary.LittleEndian.Uint32(b[offPageID:])),
		Type:     Type(b[offType]),
		Flags:    b[offFlags],
		Payload:  b[HeaderSize : total-TrailerSize],
	}, total, nil
}
