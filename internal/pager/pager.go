// Package pager biến một file thành mảng page có kích thước cố định, và cung cấp
// atomicity ở mức "chuyển trạng thái file" mà CHƯA cần WAL.
//
// Ý tưởng cốt lõi (giống BoltDB / LMDB):
//
//   - Page 0 và page 1 là hai META PAGE, ghi LUÂN PHIÊN theo txnID%2.
//     Do đó tại mọi thời điểm luôn tồn tại ít nhất một meta page nguyên vẹn —
//     cái vừa được ghi ở commit trước.
//   - Mỗi meta page có crc32c của chính nó. Meta bị ghi dở (torn write) sẽ sai
//     checksum, nên khi mở file ta chọn meta có txnID LỚN NHẤT trong số những
//     meta CÓ CHECKSUM HỢP LỆ. Đó chính là hành vi rollback.
//   - Không bao giờ ghi đè một page mà meta hợp lệ hiện tại đang trỏ tới.
//     Page được Free trong txn N chỉ trở thành cấp phát được từ txn N+1
//     (danh sách `pending`), vì trước khi commit N hoàn tất, meta cũ vẫn là
//     trạng thái mà ta sẽ rollback về.
package pager

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"sort"
)

const (
	// PageSize cố định 4096 = kích thước page của kernel và (thường là) đơn vị
	// ghi nguyên tử của ổ đĩa. Xem diary/phase0.md.
	PageSize = 4096

	magic   uint32 = 0xD1DBDB01
	version uint32 = 1

	metaPageA PageID = 0
	metaPageB PageID = 1

	// Layout meta page (little-endian), phần còn lại của page là 0.
	offMagic     = 0  // uint32
	offVersion   = 4  // uint32
	offPageSize  = 8  // uint32
	offRoot      = 12 // uint32  PageID gốc của B+Tree (phase 4), 0 = chưa có
	offFreelist  = 16 // uint32  PageID đầu chuỗi freelist, 0 = rỗng
	offTxnID     = 20 // uint64
	offPageCount = 28 // uint32  tổng số page của file
	offChecksum  = 32 // uint32  crc32c của byte [0:32]
	metaBodyLen  = 32

	// Layout freelist page: count uint32 | next PageID uint32 | ids...
	offFLCount = 0
	offFLNext  = 4
	offFLIDs   = 8

	// Số PageID chứa được trong một freelist page.
	freelistPerPage = (PageSize - offFLIDs) / 4
)

// crc32c (Castagnoli) — có lệnh CPU riêng, nhanh hơn crc32 IEEE.
var crcTable = crc32.MakeTable(crc32.Castagnoli)

// PageID là chỉ số page trong file: offset = int64(id) * PageSize.
type PageID uint32

var (
	ErrCorrupt      = errors.New("pager: cả hai meta page đều không hợp lệ")
	ErrBadPageID    = errors.New("pager: page id ngoài phạm vi file")
	ErrBadPageSize  = errors.New("pager: buffer không đúng PageSize")
	ErrMetaPageBusy = errors.New("pager: không được ghi trực tiếp vào meta page")
)

// File là phần tối thiểu của *os.File mà pager cần. Tách ra interface để test
// chèn được lỗi (ghi dở, mất lời ghi) mà không cần crash tiến trình thật.
type File interface {
	ReadAt(p []byte, off int64) (int, error)
	WriteAt(p []byte, off int64) (int, error)
	Truncate(size int64) error
	Sync() error
	Close() error
}

type meta struct {
	root      PageID
	freelist  PageID
	txnID     uint64
	pageCount uint32
}

// AllocPolicy quyết định lấy page nào ra khỏi freelist.
//
// Vì sao đây là lựa chọn có thật chứ không phải chi tiết: phase 0 đo được
// fsync sau khi ghi NGẪU NHIÊN đắt ~4x fsync sau khi ghi TUẦN TỰ. Chính sách
// cấp phát quyết định các lời ghi của một commit nằm gần hay xa nhau trên đĩa.
type AllocPolicy int

const (
	// AllocLIFO lấy page vừa được free gần nhất. Rẻ nhất về CPU, thân thiện
	// với cache, nhưng id nhảy lung tung -> ghi ngẫu nhiên.
	AllocLIFO AllocPolicy = iota
	// AllocLowest luôn lấy id nhỏ nhất. File có xu hướng đặc lại ở đầu và các
	// lời ghi gần nhau hơn, đổi lại phải giữ danh sách có thứ tự.
	AllocLowest
)

func (a AllocPolicy) String() string {
	if a == AllocLowest {
		return "lowest"
	}
	return "lifo"
}

// Pager là handle của một file database.
type Pager struct {
	f    File
	path string

	meta meta

	// free: page có thể cấp phát ngay.
	// pending: page vừa được Free trong txn hiện tại — CHƯA được cấp phát lại,
	// vì meta cũ (đích rollback) vẫn còn trỏ tới chúng.
	free    []PageID
	pending []PageID

	// metaPending: các page CHỨA freelist mà meta hợp lệ HIỆN TẠI đang trỏ
	// tới. Chúng chỉ được cấp lại sau khi một meta MỚI đã durable.
	//
	// Trước phase 5 chúng nằm chung trong `pending` và điều đó đúng, vì
	// `pending` chỉ được rót vào `free` ở đầu Commit — tức là ở đúng lúc meta
	// sắp bị thay. WAL tách hai mốc đó ra: `pending` giờ được rót ở lúc
	// TRANSACTION commit, còn meta chỉ đổi ở lúc CHECKPOINT. Để chung là cấp
	// lại một freelist page mà meta trên đĩa vẫn đang trỏ vào; ghi đè nó bằng
	// một node B+Tree; rồi lần mở file sau, loadFreelist đọc node đó như một
	// freelist page. Đã dính đúng vậy: crashlab báo "đọc freelist page 363:
	// EOF" và "page 5 kiểu 4 không phải node B+Tree" ở 4/20 vòng.
	metaPending []PageID

	// Policy: cách chọn page khi tái dùng. Mặc định AllocLIFO.
	Policy AllocPolicy

	// NoSync bỏ hẳn fsync. CHỈ dùng để ĐO cái giá của durability (và để test
	// chạy nhanh). Bật cái này lên là mất sạch đảm bảo crash-safety: dữ liệu
	// chỉ nằm trong page cache. Xem diary/phase0.md — write() != durable.
	NoSync bool

	// Đếm để quan sát trong test/bench, không ảnh hưởng logic.
	Reads, Writes, Syncs int64
}

// ---------- mở / tạo ----------

// Open mở file database, tạo mới nếu chưa có.
func Open(path string) (*Pager, error) {
	created := false
	if _, err := os.Stat(path); os.IsNotExist(err) {
		created = true
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	if created {
		if err := initFile(f); err != nil {
			f.Close()
			return nil, err
		}
		// fsync THƯ MỤC: nếu không, entry của file mới có thể chưa durable
		// dù nội dung file đã fsync. Xem diary/phase0.md.
		if err := syncDir(filepath.Dir(path)); err != nil {
			f.Close()
			return nil, err
		}
	}
	return NewWithFile(f, path)
}

// NewWithFile dùng cho test: nhận một File đã được khởi tạo sẵn.
func NewWithFile(f File, path string) (*Pager, error) {
	p := &Pager{f: f, path: path}
	m, err := p.pickMeta()
	if err != nil {
		f.Close()
		return nil, err
	}
	p.meta = m
	if err := p.loadFreelist(); err != nil {
		f.Close()
		return nil, err
	}
	return p, nil
}

// initFile ghi hai meta page ban đầu. Cả hai đều hợp lệ ngay từ đầu, khác nhau
// duy nhất ở txnID (0 và 1) — nên Open sẽ chọn page 1.
func initFile(f File) error {
	for i, txn := range []uint64{0, 1} {
		m := meta{root: 0, freelist: 0, txnID: txn, pageCount: 2}
		if err := writeFull(f, encodeMeta(m), int64(i)*PageSize); err != nil {
			return err
		}
	}
	return f.Sync()
}

// writeFull coi "ghi thiếu byte mà không báo lỗi" là LỖI. POSIX cho phép
// write() trả về n < len(p) với err == nil; nếu bỏ qua, pager sẽ commit một
// page ghi dở và tưởng là thành công. Xem TestShortWriteIsAnError.
func writeFull(f File, buf []byte, off int64) error {
	n, err := f.WriteAt(buf, off)
	if err != nil {
		return err
	}
	if n != len(buf) {
		return fmt.Errorf("pager: ghi thiếu tại offset %d: %d/%d byte", off, n, len(buf))
	}
	return nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// pickMeta đọc cả hai meta page và chọn cái có txnID lớn nhất trong số các meta
// CÓ CHECKSUM HỢP LỆ. Đây là toàn bộ cơ chế rollback của pager.
func (p *Pager) pickMeta() (meta, error) {
	var best meta
	found := false
	for _, id := range []PageID{metaPageA, metaPageB} {
		buf := make([]byte, PageSize)
		if _, err := p.f.ReadAt(buf, int64(id)*PageSize); err != nil {
			continue // page thiếu/ngắn cũng coi như không hợp lệ
		}
		m, err := decodeMeta(buf)
		if err != nil {
			continue
		}
		if !found || m.txnID > best.txnID {
			best, found = m, true
		}
	}
	if !found {
		return meta{}, ErrCorrupt
	}
	return best, nil
}

func encodeMeta(m meta) []byte {
	buf := make([]byte, PageSize)
	binary.LittleEndian.PutUint32(buf[offMagic:], magic)
	binary.LittleEndian.PutUint32(buf[offVersion:], version)
	binary.LittleEndian.PutUint32(buf[offPageSize:], PageSize)
	binary.LittleEndian.PutUint32(buf[offRoot:], uint32(m.root))
	binary.LittleEndian.PutUint32(buf[offFreelist:], uint32(m.freelist))
	binary.LittleEndian.PutUint64(buf[offTxnID:], m.txnID)
	binary.LittleEndian.PutUint32(buf[offPageCount:], m.pageCount)
	binary.LittleEndian.PutUint32(buf[offChecksum:], crc32.Checksum(buf[:metaBodyLen], crcTable))
	return buf
}

func decodeMeta(buf []byte) (meta, error) {
	if len(buf) < PageSize {
		return meta{}, ErrBadPageSize
	}
	if got := binary.LittleEndian.Uint32(buf[offMagic:]); got != magic {
		return meta{}, fmt.Errorf("magic sai: %#x", got)
	}
	if got := binary.LittleEndian.Uint32(buf[offVersion:]); got != version {
		return meta{}, fmt.Errorf("version lạ: %d", got)
	}
	if got := binary.LittleEndian.Uint32(buf[offPageSize:]); got != PageSize {
		return meta{}, fmt.Errorf("pageSize file=%d, binary=%d", got, PageSize)
	}
	want := binary.LittleEndian.Uint32(buf[offChecksum:])
	if got := crc32.Checksum(buf[:metaBodyLen], crcTable); got != want {
		return meta{}, fmt.Errorf("checksum sai: file=%#x tính lại=%#x", want, got)
	}
	return meta{
		root:      PageID(binary.LittleEndian.Uint32(buf[offRoot:])),
		freelist:  PageID(binary.LittleEndian.Uint32(buf[offFreelist:])),
		txnID:     binary.LittleEndian.Uint64(buf[offTxnID:]),
		pageCount: binary.LittleEndian.Uint32(buf[offPageCount:]),
	}, nil
}

// ---------- đọc / ghi page ----------

// ReadPage đọc page id vào buf (len(buf) phải == PageSize).
// Dùng ReadAt = pread(2): offset đi kèm từng lời gọi, nên nhiều goroutine đọc
// cùng lúc vẫn đúng. Seek+Read thì KHÔNG, vì file offset là trạng thái dùng
// chung của file description.
func (p *Pager) ReadPage(id PageID, buf []byte) error {
	if len(buf) != PageSize {
		return ErrBadPageSize
	}
	if uint32(id) >= p.meta.pageCount {
		return fmt.Errorf("%w: id=%d pageCount=%d", ErrBadPageID, id, p.meta.pageCount)
	}
	p.Reads++
	_, err := p.f.ReadAt(buf, int64(id)*PageSize)
	return err
}

// WritePage ghi page id. Không cho ghi vào hai meta page — chỉ Commit được làm.
func (p *Pager) WritePage(id PageID, buf []byte) error {
	if len(buf) != PageSize {
		return ErrBadPageSize
	}
	if id == metaPageA || id == metaPageB {
		return ErrMetaPageBusy
	}
	if uint32(id) >= p.meta.pageCount {
		return fmt.Errorf("%w: id=%d pageCount=%d", ErrBadPageID, id, p.meta.pageCount)
	}
	// Chốt chặn thường trực, không phải chỉ khi debug: tầng trên KHÔNG ĐƯỢC
	// ghi đè một page mà meta hợp lệ hiện tại đang dùng làm chuỗi freelist.
	//
	// Giữ lại vì nó là cái đã bắt được bug khó nhất của phase 5. Ba lần sửa
	// đầu đều chỉ chữa triệu chứng ("đọc freelist page 4096: EOF") ở cách chỗ
	// hỏng hàng trăm mili giây; đặt kiểm tra ngay tại lời ghi thì stack trace
	// chỉ thẳng vào bufpool.victim -> writeFrame và mọi thứ sáng ra trong một
	// lần chạy. Giá: một vòng lặp trên danh sách thường dài 0-2 phần tử.
	if p.inMetaPending(id) {
		return fmt.Errorf("%w: page %d đang là page chứa freelist của meta hiện tại", ErrMetaPageBusy, id)
	}
	p.Writes++
	return writeFull(p.f, buf, int64(id)*PageSize)
}

// ---------- cấp phát ----------

// Allocate cấp một page: ưu tiên tái dùng từ freelist, hết thì nới file.
func (p *Pager) Allocate() (PageID, error) {
	// Lọc trước: không bao giờ cấp một page mà meta HỢP LỆ HIỆN TẠI đang dùng
	// làm page chứa freelist. Đáng lẽ metaPending và free đã rời nhau, nhưng
	// "đáng lẽ" là thứ đã sai một lần rồi (xem FreeNow trong wal.go), và cái
	// giá của một lần lọt lưới là ghi đè lên chính chuỗi freelist mà lần mở
	// file sau phải đọc. Chốt chặn cuối đặt ở đây, chỗ DUY NHẤT page được cấp.
	if len(p.metaPending) > 0 {
		kept := p.free[:0]
		for _, id := range p.free {
			if !p.inMetaPending(id) {
				kept = append(kept, id)
			}
		}
		p.free = kept
	}
	if n := len(p.free); n > 0 {
		// p.free giữ thứ tự tăng dần (loadFreelist đọc từ danh sách đã sort,
		// và Commit sort lại trước khi ghi). Nên LIFO = lấy cuối, lowest = đầu.
		if p.Policy == AllocLowest {
			id := p.free[0]
			p.free = p.free[1:]
			return id, nil
		}
		id := p.free[n-1]
		p.free = p.free[:n-1]
		return id, nil
	}
	id := PageID(p.meta.pageCount)
	p.meta.pageCount++
	// Nới file ngay để ReadPage của page vừa cấp không đọc quá EOF.
	if err := p.f.Truncate(int64(p.meta.pageCount) * PageSize); err != nil {
		return 0, err
	}
	return id, nil
}

func (p *Pager) inMetaPending(id PageID) bool {
	for _, h := range p.metaPending {
		if h == id {
			return true
		}
	}
	return false
}

// Free đánh dấu page bỏ đi. Page vào `pending`: chưa được cấp phát lại trong
// txn này, vì meta cũ — đích rollback nếu commit này chết giữa đường — vẫn
// đang trỏ tới nó.
func (p *Pager) Free(id PageID) error {
	if id == metaPageA || id == metaPageB {
		return ErrMetaPageBusy
	}
	if uint32(id) >= p.meta.pageCount {
		return fmt.Errorf("%w: id=%d", ErrBadPageID, id)
	}
	p.pending = append(p.pending, id)
	return nil
}

// ---------- freelist trên đĩa ----------

func (p *Pager) loadFreelist() error {
	p.free, p.pending = nil, nil
	id := p.meta.freelist
	buf := make([]byte, PageSize)
	for id != 0 {
		if _, err := p.f.ReadAt(buf, int64(id)*PageSize); err != nil {
			return fmt.Errorf("đọc freelist page %d: %w", id, err)
		}
		n := binary.LittleEndian.Uint32(buf[offFLCount:])
		next := PageID(binary.LittleEndian.Uint32(buf[offFLNext:]))
		if n > freelistPerPage {
			return fmt.Errorf("freelist page %d: count=%d > %d", id, n, freelistPerPage)
		}
		for i := uint32(0); i < n; i++ {
			p.free = append(p.free, PageID(binary.LittleEndian.Uint32(buf[offFLIDs+4*int(i):])))
		}
		// Bản thân page freelist này sẽ bị thay ở lần ghi meta tới. Cho tới
		// lúc đó meta hợp lệ vẫn trỏ vào nó -> metaPending, không phải pending.
		p.metaPending = append(p.metaPending, id)
		id = next
	}
	return nil
}

// writeFreelist ghi p.free thành chuỗi page.
//
// Bẫy đã dính (xem diary/phase1.md): phải CẤP page chứa freelist TRƯỚC khi
// serialize. Nếu serialize trước rồi mới cấp, page chứa freelist lại nằm trong
// chính danh sách free mà nó ghi ra -> commit sau cấp lại nó và ghi đè lên
// freelist page mà meta hợp lệ hiện tại đang trỏ tới.
func (p *Pager) writeFreelist() (head PageID, hosts []PageID, err error) {
	// Cấp host lặp: mỗi lần Allocate có thể pop từ chính p.free, làm p.free
	// ngắn lại, nên số host cần thiết phải tính lại sau mỗi lần cấp.
	for {
		need := (len(p.free) + freelistPerPage - 1) / freelistPerPage
		if need <= len(hosts) {
			break
		}
		h, err := p.Allocate()
		if err != nil {
			return 0, nil, err
		}
		hosts = append(hosts, h)
	}
	if len(p.free) == 0 {
		// Không còn gì để ghi. Host đã cấp thì trả vào pending — không ai trỏ
		// tới chúng, nên commit sau tái dùng được.
		p.pending = append(p.pending, hosts...)
		return 0, nil, nil
	}

	ids := make([]PageID, len(p.free))
	copy(ids, p.free)
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	// Ghi từ chunk cuối về đầu để biết `next`.
	var next PageID
	for i := len(hosts) - 1; i >= 0; i-- {
		lo := i * freelistPerPage
		hi := min(lo+freelistPerPage, len(ids))
		chunk := ids[lo:hi]

		buf := make([]byte, PageSize)
		binary.LittleEndian.PutUint32(buf[offFLCount:], uint32(len(chunk)))
		binary.LittleEndian.PutUint32(buf[offFLNext:], uint32(next))
		for j, id := range chunk {
			binary.LittleEndian.PutUint32(buf[offFLIDs+4*j:], uint32(id))
		}
		p.Writes++
		if err := writeFull(p.f, buf, int64(hosts[i])*PageSize); err != nil {
			return 0, nil, err
		}
		next = hosts[i]
	}
	return next, hosts, nil
}

// ---------- commit ----------

// Commit chốt trạng thái mới của file. Thứ tự dưới đây LÀ toàn bộ lý do
// atomicity hoạt động; đổi thứ tự là mất đúng tính chất đó:
//
//  1. ghi các data page + freelist page (chỗ nào cũng được, trừ page mà meta
//     hợp lệ hiện tại đang trỏ tới)
//  2. fsync — đảm bảo (1) đã nằm trên đĩa TRƯỚC khi có ai trỏ tới nó
//  3. ghi meta page vào page txnID%2 (cái không chứa meta hợp lệ hiện tại)
//  4. fsync — từ giây phút này, trạng thái mới trở thành trạng thái chính thức
//
// Chết ở bất kỳ điểm nào trước (4) → meta cũ vẫn nguyên → rollback.
func (p *Pager) Commit(root PageID) error {
	// pending của txn trước giờ đã an toàn: meta ta sắp ghi đè không còn là
	// đích rollback nữa (đích rollback là meta của commit gần nhất).
	//
	// Từ phase 5, hai nửa này tách ra: dưới WAL, "pending an toàn rồi" xảy ra
	// ở lúc transaction commit chứ không ở lúc ghi meta. Xem pager/wal.go.
	p.ReleasePending()
	return p.CommitMeta(root)
}

func (p *Pager) sync() error {
	if p.NoSync {
		return nil
	}
	p.Syncs++
	return p.f.Sync()
}

// ---------- quan sát ----------

func (p *Pager) Root() PageID         { return p.meta.root }
func (p *Pager) TxnID() uint64        { return p.meta.txnID }
func (p *Pager) PageCount() uint32    { return p.meta.pageCount }
func (p *Pager) FreeCount() int       { return len(p.free) }
func (p *Pager) PendingCount() int    { return len(p.pending) }
func (p *Pager) FreelistHead() PageID { return p.meta.freelist }
func (p *Pager) Path() string         { return p.path }

// MetaPageOf cho biết commit với txnID này ghi vào meta page nào.
func MetaPageOf(txnID uint64) PageID { return PageID(txnID % 2) }

func (p *Pager) Close() error { return p.f.Close() }
