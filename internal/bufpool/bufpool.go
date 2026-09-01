// Package bufpool là tầng đệm giữa tầng trên (B+Tree, phase 4) và pager.
//
// Lý do tồn tại — đo được, không phải cảm tính. Phase 0:
//
//	pread 4KB ngẫu nhiên, CACHE LẠNH     14521 ops/s   69µs
//	pread 4KB ngẫu nhiên, CACHE NÓNG    891515 ops/s    1µs
//
// 61x. Mỗi lần tránh được một lần đọc đĩa là tiết kiệm ~68µs; mọi thứ trong file
// này chỉ tốn cỡ trăm nanosecond. Nên chính sách chọn nạn nhân được phép "đắt"
// một cách vô lý so với chi phí trong RAM — miễn là nó giảm được số lần miss.
package bufpool

import (
	"errors"
	"fmt"
	"sync"

	"minidb/internal/page"
	"minidb/internal/pager"
)

// Store là phần của pager mà buffer pool cần. Tách interface để bench có thể
// thay bằng một store đếm I/O trong RAM — nếu không thì bench hit-ratio sẽ đo
// tốc độ ổ đĩa chứ không đo chính sách thay thế.
type Store interface {
	ReadPage(id pager.PageID, buf []byte) error
	WritePage(id pager.PageID, buf []byte) error
}

var (
	ErrNoFrame     = errors.New("bufpool: hết frame — tất cả đều đang bị pin")
	ErrNotPinned   = errors.New("bufpool: Unpin một page không hề được pin")
	ErrPinnedFlush = errors.New("bufpool: FlushAll khi vẫn còn page bị pin")
	ErrWALRule     = errors.New("bufpool: vi phạm WAL rule — pageLSN chưa nằm trên đĩa")
)

// Frame là một ô chứa page trong pool.
//
// Data trỏ thẳng vào arena, KHÔNG copy: tầng trên sửa tại chỗ rồi Unpin(dirty=true).
// Đó là điểm khác biệt then chốt so với Pager.ReadPage (chép vào buffer của caller).
type Frame struct {
	id    pager.PageID
	Data  page.Page
	pin   int
	dirty bool
	valid bool

	// Latch của page — bảo vệ NỘI DUNG page, sống vài trăm ns (phase 0, mục
	// latch vs lock). Khác hẳn lock của transaction, thứ giữ tới hết txn.
	Latch sync.RWMutex
}

func (f *Frame) PageID() pager.PageID { return f.id }
func (f *Frame) Dirty() bool          { return f.dirty }
func (f *Frame) PinCount() int        { return f.pin }

// Pool là mảng frame cố định + bảng tra pageID -> frame.
type Pool struct {
	mu     sync.Mutex // latch của BẢNG TRA + metadata frame, không phải của nội dung page
	store  Store
	arena  []byte
	frames []Frame
	table  map[pager.PageID]int
	repl   Replacer

	// FlushLog là điểm móc cho WAL rule (phase 5): trước khi ghi một page bẩn
	// xuống đĩa, log record của nó phải đã fsync. Nil = chưa có WAL (phase 3).
	// Trả lỗi = pool sẽ KHÔNG ghi page đó và KHÔNG evict nó.
	FlushLog func(pageLSN uint64) error

	// Alloc là móc nối xuống phần cấp phát của pager (xem alloc.go). Nil =
	// pool không tự sinh page mới được.
	Alloc Allocator

	// Thống kê. Đọc bằng Stats(), đừng đọc trực tiếp (không có latch).
	hits, misses   int64
	evictions      int64
	dirtyEvictions int64
	reads, writes  int64
	flushLogCalls  int64
}

type Stats struct {
	Hits, Misses              int64
	Evictions, DirtyEvictions int64
	Reads, Writes             int64
	FlushLogCalls             int64
}

func (s Stats) HitRatio() float64 {
	n := s.Hits + s.Misses
	if n == 0 {
		return 0
	}
	return float64(s.Hits) / float64(n)
}

// New tạo pool với n frame, dùng chính sách thay thế repl.
func New(store Store, n int, repl Replacer) *Pool {
	if n < 1 {
		panic("bufpool: cần ít nhất 1 frame")
	}
	p := &Pool{
		store:  store,
		arena:  make([]byte, n*page.PageSize),
		frames: make([]Frame, n),
		table:  make(map[pager.PageID]int, n),
		repl:   repl,
	}
	for i := range p.frames {
		p.frames[i].Data = page.Page(p.arena[i*page.PageSize : (i+1)*page.PageSize])
	}
	return p
}

func (p *Pool) NumFrames() int { return len(p.frames) }
func (p *Pool) Policy() string { return p.repl.Name() }

func (p *Pool) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return Stats{
		Hits: p.hits, Misses: p.misses,
		Evictions: p.evictions, DirtyEvictions: p.dirtyEvictions,
		Reads: p.reads, Writes: p.writes,
		FlushLogCalls: p.flushLogCalls,
	}
}

func (p *Pool) ResetStats() {
	p.mu.Lock()
	p.hits, p.misses, p.evictions, p.dirtyEvictions = 0, 0, 0, 0
	p.reads, p.writes, p.flushLogCalls = 0, 0, 0
	p.mu.Unlock()
}

// Pin lấy page id vào pool và tăng pin count. Page đang bị pin KHÔNG BAO GIỜ
// bị evict — đó là bất biến số 1 của tầng này: tầng trên đang giữ con trỏ
// thẳng vào arena, evict nó ra là con trỏ đó trỏ vào nội dung của page khác.
func (p *Pool) Pin(id pager.PageID) (*Frame, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if i, ok := p.table[id]; ok {
		f := &p.frames[i]
		f.pin++
		p.hits++
		p.repl.Access(i)
		p.repl.Pin(i)
		return f, nil
	}

	p.misses++
	i, err := p.victim()
	if err != nil {
		return nil, err
	}
	f := &p.frames[i]
	if err := p.store.ReadPage(id, f.Data); err != nil {
		// Frame đã bị dọn sạch trong victim(); trả nó về trạng thái trống.
		f.valid = false
		return nil, err
	}
	p.reads++
	f.id, f.pin, f.dirty, f.valid = id, 1, false, true
	p.table[id] = i
	p.repl.Access(i)
	p.repl.Pin(i)
	return f, nil
}

// Unpin giảm pin count. dirty=true nghĩa là "tôi đã sửa nội dung" — cờ này
// dính lại cho tới khi page được ghi xuống, một lần Unpin(false) sau đó KHÔNG
// xoá được nó.
func (p *Pool) Unpin(id pager.PageID, dirty bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	i, ok := p.table[id]
	if !ok {
		return fmt.Errorf("%w: page %d không có trong pool", ErrNotPinned, id)
	}
	f := &p.frames[i]
	if f.pin == 0 {
		return fmt.Errorf("%w: page %d pin=0", ErrNotPinned, id)
	}
	f.pin--
	if dirty {
		f.dirty = true
	}
	if f.pin == 0 {
		p.repl.Unpin(i)
	}
	return nil
}

// victim chọn một frame trống hoặc đuổi một frame ra. Gọi khi đang giữ p.mu.
func (p *Pool) victim() (int, error) {
	// Frame chưa dùng bao giờ thì lấy luôn — rẻ hơn mọi chính sách.
	for i := range p.frames {
		if !p.frames[i].valid {
			return i, nil
		}
	}
	// blocked đếm số ứng viên bị WAL rule trả lại. Không có bộ đếm này thì khi
	// MỌI ứng viên đều bị chặn, vòng lặp quay mãi: replacer trả frame ra, ta
	// trả nó về, nó lại được trả ra. Đã dính đúng lỗi đó — xem diary phase 3.
	for blocked := 0; ; {
		i, ok := p.repl.Victim()
		if !ok {
			return 0, ErrNoFrame
		}
		f := &p.frames[i]
		if f.pin > 0 {
			// Replacer không được phép trả về frame đang pin. Nếu xảy ra thì
			// đó là bug của replacer, không phải trạng thái hợp lệ.
			return 0, fmt.Errorf("bufpool: replacer %s trả về frame %d đang pin=%d", p.repl.Name(), i, f.pin)
		}
		if f.dirty {
			if err := p.writeFrame(f); err != nil {
				if errors.Is(err, ErrWALRule) {
					// WAL chưa fsync tới pageLSN này -> KHÔNG đuổi page này.
					// Trả nó lại hàng đợi và tìm nạn nhân khác.
					p.repl.Unpin(i)
					blocked++
					if blocked >= len(p.frames) {
						// Đã đi hết một vòng mà ai cũng bị chặn: chịu thua,
						// tuyệt đối không được ghi vòng qua WAL rule.
						return 0, fmt.Errorf("%w: mọi ứng viên đều bị WAL rule chặn", ErrNoFrame)
					}
					continue
				}
				return 0, err
			}
			p.dirtyEvictions++
		}
		delete(p.table, f.id)
		p.evictions++
		f.valid, f.dirty, f.pin = false, false, 0
		return i, nil
	}
}

// writeFrame ghi một frame bẩn xuống store. Đây là CHỖ DUY NHẤT page rời RAM,
// nên WAL rule chỉ cần cài đúng ở đây.
func (p *Pool) writeFrame(f *Frame) error {
	if p.FlushLog != nil {
		p.flushLogCalls++
		if err := p.FlushLog(f.Data.LSN()); err != nil {
			return fmt.Errorf("%w: page %d pageLSN=%d: %w", ErrWALRule, f.id, f.Data.LSN(), err)
		}
	}
	if err := p.store.WritePage(f.id, f.Data); err != nil {
		return err
	}
	p.writes++
	f.dirty = false
	return nil
}

// Flush ghi một page cụ thể xuống đĩa nhưng GIỮ nó trong pool.
func (p *Pool) Flush(id pager.PageID) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	i, ok := p.table[id]
	if !ok {
		return nil
	}
	if !p.frames[i].dirty {
		return nil
	}
	return p.writeFrame(&p.frames[i])
}

// FlushAll ghi mọi page bẩn xuống store. Gọi trước Commit của pager: pager
// chỉ đảm bảo atomicity cho những gì ĐÃ nằm trên đĩa lúc nó ghi meta page.
func (p *Pool) FlushAll() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.frames {
		f := &p.frames[i]
		if f.valid && f.dirty {
			if err := p.writeFrame(f); err != nil {
				return err
			}
		}
	}
	return nil
}

// DirtyCount đếm page bẩn đang nằm trong pool (dùng cho test và cho checkpoint
// ở phase 5).
func (p *Pool) DirtyCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for i := range p.frames {
		if p.frames[i].valid && p.frames[i].dirty {
			n++
		}
	}
	return n
}

// PinnedCount đếm frame đang bị pin.
func (p *Pool) PinnedCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for i := range p.frames {
		if p.frames[i].pin > 0 {
			n++
		}
	}
	return n
}

// Contains cho biết page có đang nằm trong pool không (chỉ dùng để test/quan
// sát — dùng nó để quyết định luồng chạy là race).
func (p *Pool) Contains(id pager.PageID) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.table[id]
	return ok
}
