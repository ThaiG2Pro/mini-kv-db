package wal

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"sync"
)

// Header của file log. 32 byte, nằm gọn trong một sector nên ghi nó là nguyên
// tử ở mức phần cứng — điều kiện để nó làm được việc "master record" của ARIES:
// nói cho recovery biết bắt đầu đọc từ đâu.
const (
	offMagic   = 0  // u32
	offVersion = 4  // u32
	offCkpt    = 8  // u64 LSN của record CKPT-BEGIN gần nhất đã hoàn tất
	offHdrCRC  = 16 // u32 crc32c của [0:16]

	HeaderLen = 32

	logMagic   uint32 = 0xD1DB1A05
	logVersion uint32 = 1
)

// FirstLSN là LSN của record đầu tiên. Bằng HeaderLen, nên LSN 0 luôn vô nghĩa
// và dùng được làm "không có" — page mới toanh có pageLSN = 0.
const FirstLSN = HeaderLen

var (
	ErrClosed     = errors.New("wal: log đã đóng")
	ErrBadLogFile = errors.New("wal: file log không phải log của minidb")
	ErrLSNRange   = errors.New("wal: LSN nằm ngoài log")
)

// Log là write-ahead log: một file chỉ nối thêm, cộng một buffer trong RAM.
//
// Ba mốc LSN phải phân biệt rõ, lẫn một cái là mất durability mà test đơn
// luồng không bao giờ bắt được:
//
//	end      — đã cấp LSN, mới nằm trong RAM
//	written  — đã gọi write(2), nằm trong page cache của OS (CHƯA durable)
//	flushed  — đã fsync, thật sự nằm trên đĩa
//
// WAL rule so với `flushed`, không phải `written`. Phase 0 đo được: bỏ fsync
// nhanh hơn ~100x, nên đây đúng là chỗ dễ ăn gian nhất và cũng là chỗ mất
// durability êm nhất.
type Log struct {
	mu   sync.Mutex
	cond *sync.Cond
	f    *os.File
	path string

	buf     []byte // phần chưa write(2), bắt đầu tại LSN bufBase
	bufBase uint64
	end     uint64
	written uint64
	flushed uint64
	ckpt    uint64
	lastLSN uint64

	syncing bool // đang có goroutine fsync -> người khác xếp hàng thay vì fsync thêm

	// NoSync bỏ fsync. CHỈ dùng để ĐO cái giá của durability. Bật lên là mất
	// sạch crash-safety, y hệt pager.NoSync.
	NoSync bool

	// NoWrite bỏ luôn cả write(2): byte log chỉ nằm trong buffer của tiến
	// trình. Đây là cách DUY NHẤT để mô phỏng mất điện bằng kill -9, vì
	// page cache của kernel sống lâu hơn tiến trình — với NoSync thì dữ liệu
	// đã commit vẫn còn sau kill -9, nên bộ kiểm tra crash không bao giờ có
	// cơ hội chứng minh rằng nó biết báo SAI. Bật cái này lên thì những
	// commit cuối phải mất, và crashlab phải bắt được. Chỉ dùng để tự kiểm
	// tra bộ đo, không bao giờ dùng thật.
	NoWrite bool

	// FullPageWrites: ghi trọn page ở lần chạm đầu tiên sau mỗi checkpoint.
	// Xem FlagFullPage. Tắt được để bench đo đúng cái giá của nó.
	FullPageWrites bool

	// Thống kê. Đọc bằng Stats().
	appends, syncs, writes int64
	bytes, fullPages       int64
	groupedSyncs           int64 // số lần một commit được người khác fsync hộ
	flushCalls             int64
	flushNoops             int64
}

type Stats struct {
	Appends, Syncs, Writes int64
	Bytes, FullPages       int64
	GroupedSyncs           int64
	FlushCalls, FlushNoops int64
	End, Flushed, Ckpt     uint64
}

// Open mở (hoặc tạo) file log.
//
// Lúc mở, log được QUÉT LẠI từ đầu để tìm record hợp lệ cuối cùng, rồi file bị
// cắt xuống đúng đó. Đây không phải dọn dẹp cho gọn: một record ghi dở ở đuôi
// (crash giữa lúc write) sẽ được người ghi tiếp theo nối thêm vào SAU nó, và
// khi ấy trong log có một vùng rác nằm giữa hai record thật — recovery lần sau
// dừng ở đó và làm mất mọi thứ phía sau. Cắt ngay lúc mở là cách rẻ nhất để
// bất biến "log là chuỗi record liên tục" không bao giờ vỡ.
func Open(path string) (*Log, error) {
	created := false
	if _, err := os.Stat(path); os.IsNotExist(err) {
		created = true
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	l := &Log{f: f, path: path}
	l.cond = sync.NewCond(&l.mu)

	if created {
		if err := l.writeHeader(0); err != nil {
			f.Close()
			return nil, err
		}
		if err := f.Sync(); err != nil {
			f.Close()
			return nil, err
		}
		if d, err := os.Open(filepath.Dir(path)); err == nil {
			d.Sync()
			d.Close()
		}
		l.end, l.written, l.flushed, l.bufBase = FirstLSN, FirstLSN, FirstLSN, FirstLSN
		return l, nil
	}

	hdr := make([]byte, HeaderLen)
	if _, err := f.ReadAt(hdr, 0); err != nil {
		f.Close()
		return nil, fmt.Errorf("%w: đọc header: %w", ErrBadLogFile, err)
	}
	if binary.LittleEndian.Uint32(hdr[offMagic:]) != logMagic {
		f.Close()
		return nil, fmt.Errorf("%w: magic sai", ErrBadLogFile)
	}
	if binary.LittleEndian.Uint32(hdr[offVersion:]) != logVersion {
		f.Close()
		return nil, fmt.Errorf("%w: version lạ", ErrBadLogFile)
	}
	if got, want := crc32.Checksum(hdr[:offHdrCRC], crcTable), binary.LittleEndian.Uint32(hdr[offHdrCRC:]); got != want {
		f.Close()
		return nil, fmt.Errorf("%w: crc header sai", ErrBadLogFile)
	}
	l.ckpt = binary.LittleEndian.Uint64(hdr[offCkpt:])

	endLSN, lastLSN, err := scanTail(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Truncate(int64(endLSN)); err != nil {
		f.Close()
		return nil, err
	}
	l.end, l.written, l.flushed, l.bufBase = endLSN, endLSN, endLSN, endLSN
	l.lastLSN = lastLSN
	return l, nil
}

// scanTail đọc tuần tự từ FirstLSN và trả về vị trí ngay sau record hợp lệ
// cuối cùng. Record hỏng/ngắn KHÔNG phải lỗi — nó là dấu hiệu bình thường của
// một lần crash, và là điểm kết thúc của log.
func scanTail(f *os.File) (end, last uint64, err error) {
	st, err := f.Stat()
	if err != nil {
		return 0, 0, err
	}
	size := uint64(st.Size())
	if size < FirstLSN {
		return FirstLSN, 0, nil
	}
	pos := uint64(FirstLSN)
	buf := make([]byte, MaxRecordSize)
	for pos < size {
		n := min(uint64(len(buf)), size-pos)
		chunk := buf[:n]
		if _, err := f.ReadAt(chunk, int64(pos)); err != nil {
			return pos, last, nil // đọc hụt = đuôi hỏng
		}
		r, sz, err := Decode(chunk)
		if err != nil {
			return pos, last, nil
		}
		if r.LSN != pos {
			// LSN trong header không khớp offset: log bị nối sai chỗ. Coi như
			// hết log, đừng tin phần sau.
			return pos, last, nil
		}
		last = pos
		pos += uint64(sz)
	}
	return pos, last, nil
}

func (l *Log) writeHeader(ckpt uint64) error {
	hdr := make([]byte, HeaderLen)
	binary.LittleEndian.PutUint32(hdr[offMagic:], logMagic)
	binary.LittleEndian.PutUint32(hdr[offVersion:], logVersion)
	binary.LittleEndian.PutUint64(hdr[offCkpt:], ckpt)
	binary.LittleEndian.PutUint32(hdr[offHdrCRC:], crc32.Checksum(hdr[:offHdrCRC], crcTable))
	_, err := l.f.WriteAt(hdr, 0)
	return err
}

// ---------- nối record ----------

// Append cấp LSN cho record và đưa nó vào buffer. KHÔNG chạm đĩa.
//
// Tách hẳn khỏi Flush là điều kiện để có group commit: N transaction append
// xong rồi cùng chờ MỘT lần fsync.
func (l *Log) Append(r *Record) (uint64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return 0, ErrClosed
	}
	if r.Size() > MaxRecordSize {
		return 0, fmt.Errorf("%w: record %d byte", ErrBadLength, r.Size())
	}
	lsn := l.end
	l.buf = Encode(l.buf, r, lsn)
	l.end += uint64(r.Size())
	l.lastLSN = lsn
	l.appends++
	l.bytes += int64(r.Size())
	if r.Flags&FlagFullPage != 0 {
		l.fullPages++
	}
	return lsn, nil
}

// writeLocked đẩy buffer ra file tới `upto` (không fsync). Giữ l.mu.
func (l *Log) writeLocked(upto uint64) error {
	if upto <= l.written {
		return nil
	}
	from := l.written - l.bufBase
	to := upto - l.bufBase
	if _, err := l.f.WriteAt(l.buf[from:to], int64(l.written)); err != nil {
		return err
	}
	l.writes++
	l.written = upto
	// Cắt phần đã ghi khỏi buffer. Chép về đầu thay vì reslice để mảng nền
	// không phình vô hạn trong một tiến trình chạy lâu.
	if l.written == l.end {
		l.buf = l.buf[:0]
		l.bufBase = l.end
	} else {
		rest := int(l.end - l.written)
		copy(l.buf, l.buf[int(to):int(to)+rest])
		l.buf = l.buf[:rest]
		l.bufBase = l.written
	}
	return nil
}

// Flush đảm bảo mọi record có LSN < upto đã nằm trên đĩa (đã fsync).
//
// Đây vừa là hàm commit gọi, vừa là hàm bufpool gọi qua FlushLog trước khi
// ghi một page bẩn. Một hàm cho cả hai vì cả hai hỏi đúng một câu: "log tới
// điểm này đã durable chưa?".
//
// Group commit nằm ngay trong vòng lặp dưới đây: ai thấy đã có người đang
// fsync thì ĐỢI thay vì fsync thêm một lần nữa. Vì writeLocked luôn đẩy tới
// l.end (chứ không chỉ tới upto), lần fsync đang chạy rất có thể đã cuốn luôn
// phần của người đợi -> họ tỉnh dậy và về ngay, không tốn fsync nào. Không có
// tham số "đợi bao lâu": độ trễ của chính fsync đã là cửa sổ gom.
func (l *Log) Flush(upto uint64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.flushCalls++
	if l.f == nil {
		return ErrClosed
	}
	if l.flushed >= upto {
		l.flushNoops++
		return nil
	}
	for {
		if l.flushed >= upto {
			return nil
		}
		if l.syncing {
			l.groupedSyncs++
			l.cond.Wait()
			continue
		}
		target := l.end
		if l.NoWrite {
			l.flushed = target
			return nil
		}
		if err := l.writeLocked(target); err != nil {
			return err
		}
		if l.NoSync {
			l.flushed = target
			return nil
		}
		l.syncing = true
		l.mu.Unlock()
		err := l.f.Sync()
		l.mu.Lock()
		l.syncing = false
		if err == nil {
			l.syncs++
			if l.flushed < target {
				l.flushed = target
			}
		}
		l.cond.Broadcast()
		if err != nil {
			return err
		}
	}
}

// Sync đẩy toàn bộ log xuống đĩa.
func (l *Log) Sync() error {
	l.mu.Lock()
	end := l.end
	l.mu.Unlock()
	return l.Flush(end)
}

// ---------- đọc lại ----------

// Read lấy record tại lsn. Đọc được cả record còn nằm trong buffer — undo của
// một transaction đang chạy phải đi ngược chuỗi prevLSN của chính nó, mà chuỗi
// đó gần như chắc chắn chưa fsync (và cũng không cần fsync: abort không hứa gì
// với ai cả).
func (l *Log) Read(lsn uint64) (Record, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.readLocked(lsn)
}

func (l *Log) readLocked(lsn uint64) (Record, error) {
	if lsn < FirstLSN || lsn >= l.end {
		return Record{}, fmt.Errorf("%w: lsn=%d, log=[%d,%d)", ErrLSNRange, lsn, FirstLSN, l.end)
	}
	if lsn >= l.bufBase {
		r, _, err := Decode(l.buf[lsn-l.bufBase:])
		if err != nil {
			return Record{}, err
		}
		// Payload trỏ vào buf, mà buf bị cắt/ghi đè khi flush. Chép ra.
		return copyRec(r), nil
	}
	// Chặn theo bufBase chứ không theo end: end tính cả phần còn nằm trong
	// buffer RAM, đọc quá mốc đó là đọc quá EOF của file và os.ReadAt trả EOF.
	// Đã dính: undo lúc recovery vừa nối CLR vào buffer (end tăng) vừa đọc
	// ngược một record cũ trên đĩa -> "đọc lsn=...: EOF".
	n := min(uint64(MaxRecordSize), l.bufBase-lsn)
	buf := make([]byte, n)
	if _, err := l.f.ReadAt(buf, int64(lsn)); err != nil {
		return Record{}, err
	}
	r, _, err := Decode(buf)
	if err != nil {
		return Record{}, err
	}
	return r, nil
}

func copyRec(r Record) Record {
	p := make([]byte, len(r.Payload))
	copy(p, r.Payload)
	r.Payload = p
	return r
}

// Scan duyệt tuần tự từ `from` tới hết log, gọi fn cho mỗi record. Dừng êm
// (không lỗi) khi gặp record hỏng — đó là đuôi log sau crash.
func (l *Log) Scan(from uint64, fn func(*Record) error) error {
	if from < FirstLSN {
		from = FirstLSN
	}
	l.mu.Lock()
	if err := l.writeLocked(l.end); err != nil {
		l.mu.Unlock()
		return err
	}
	end := l.end
	l.mu.Unlock()

	buf := make([]byte, MaxRecordSize)
	for pos := from; pos < end; {
		n := min(uint64(len(buf)), end-pos)
		chunk := buf[:n]
		if _, err := l.f.ReadAt(chunk, int64(pos)); err != nil {
			return nil
		}
		r, sz, err := Decode(chunk)
		if err != nil || r.LSN != pos {
			return nil
		}
		if err := fn(&r); err != nil {
			return err
		}
		pos += uint64(sz)
	}
	return nil
}

// ---------- checkpoint ----------

// SetCheckpoint ghi LSN của checkpoint vừa hoàn tất vào header rồi fsync.
//
// Đây là "master record" của ARIES. Nó phải được ghi SAU khi record CKPT-END
// đã durable: nếu ngược lại, một lần crash xen giữa để lại header trỏ tới một
// checkpoint không tồn tại, và recovery bắt đầu từ chỗ không có gì.
func (l *Log) SetCheckpoint(lsn uint64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.NoWrite {
		if err := l.writeHeader(lsn); err != nil {
			return err
		}
	}
	if !l.NoSync && !l.NoWrite {
		if err := l.f.Sync(); err != nil {
			return err
		}
		l.syncs++
	}
	l.ckpt = lsn
	return nil
}

func (l *Log) Checkpoint() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.ckpt
}

// ---------- quan sát ----------

func (l *Log) End() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.end
}

func (l *Log) Flushed() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.flushed
}

func (l *Log) LastLSN() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lastLSN
}

func (l *Log) Path() string { return l.path }

func (l *Log) Stats() Stats {
	l.mu.Lock()
	defer l.mu.Unlock()
	return Stats{
		Appends: l.appends, Syncs: l.syncs, Writes: l.writes,
		Bytes: l.bytes, FullPages: l.fullPages,
		GroupedSyncs: l.groupedSyncs,
		FlushCalls:   l.flushCalls, FlushNoops: l.flushNoops,
		End: l.end, Flushed: l.flushed, Ckpt: l.ckpt,
	}
}

func (l *Log) ResetStats() {
	l.mu.Lock()
	l.appends, l.syncs, l.writes = 0, 0, 0
	l.bytes, l.fullPages, l.groupedSyncs = 0, 0, 0
	l.flushCalls, l.flushNoops = 0, 0
	l.mu.Unlock()
}

func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	if err := l.writeLocked(l.end); err != nil {
		return err
	}
	f := l.f
	l.f = nil
	return f.Close()
}

// CloseNoFlush đóng file mà KHÔNG đẩy buffer ra: mô phỏng tiến trình chết đột
// ngột. Mọi record đã Append nhưng chưa Flush biến mất, đúng như kill -9.
//
// Có mặt trong code sản phẩm chứ không trong file _test vì cmd/crashlab dùng
// nó để dựng kịch bản crash "mềm" cạnh kịch bản kill -9 thật.
func (l *Log) CloseNoFlush() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	f := l.f
	l.f = nil
	l.buf = l.buf[:0]
	return f.Close()
}
