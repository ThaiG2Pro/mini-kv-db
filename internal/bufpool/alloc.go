package bufpool

import (
	"errors"
	"fmt"

	"minidb/internal/page"
	"minidb/internal/pager"
)

// Allocator là phần cấp phát của pager mà tầng trên cần khi nó tự sinh page
// mới (B+Tree split, phase 4). Tách riêng khỏi Store vì bench hit-ratio của
// phase 3 chỉ cần đọc/ghi, không cần cấp phát.
type Allocator interface {
	Allocate() (pager.PageID, error)
	Free(id pager.PageID) error
}

var (
	ErrNoAllocator = errors.New("bufpool: pool không có Allocator")
	ErrReallocLive = errors.New("bufpool: pager cấp lại một page đang bị pin")
)

// Alloc bật khả năng NewPage/FreePage. Nil = pool chỉ đọc/ghi page có sẵn.
// Đặt như một field công khai giống FlushLog: hai cái đều là móc nối lên tầng
// khác, và pool chạy được khi thiếu cả hai.

// NewPage cấp một page mới, khởi tạo rỗng với kiểu typ, và trả về đã pin sẵn.
//
// Nó KHÔNG đọc page đó từ đĩa — đây là khác biệt duy nhất so với Pin, và là
// khác biệt đáng giá: nội dung cũ của một page vừa cấp là rác, đọc nó lên là
// một lần I/O 4KB hoàn toàn vô ích. Cây có 1 triệu key sinh vài nghìn page
// mới; mỗi page tiết kiệm một lần pread.
func (p *Pool) NewPage(typ uint8) (*Frame, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Alloc == nil {
		return nil, ErrNoAllocator
	}
	id, err := p.Alloc.Allocate()
	if err != nil {
		return nil, err
	}

	// Bẫy: pager tái dùng id từ freelist, nên id "mới" này có thể vẫn còn một
	// bản CŨ nằm trong pool (page bị Free rồi cấp lại). Nếu bỏ qua nhánh này
	// thì bảng tra sẽ có hai frame cùng một PageID -> vỡ bất biến I2, và bản
	// cũ bẩn kia còn có thể bị ghi đè lên nội dung mới sau đó.
	if i, ok := p.table[id]; ok {
		f := &p.frames[i]
		if f.pin > 0 {
			return nil, fmt.Errorf("%w: page %d pin=%d", ErrReallocLive, id, f.pin)
		}
		// Nội dung cũ là rác: KHÔNG ghi nó xuống đĩa dù đang bẩn. Đây là chỗ
		// duy nhất trong pool được phép vứt một dirty page đi.
		f.dirty = false
		p.repl.Pin(i)
		page.Init(f.Data, typ)
		f.pin, f.dirty, f.valid = 1, true, true
		p.repl.Access(i)
		return f, nil
	}

	i, err := p.victim()
	if err != nil {
		return nil, err
	}
	f := &p.frames[i]
	page.Init(f.Data, typ)
	// dirty=true ngay từ đầu: page mới chưa từng có mặt trên đĩa, nếu bị evict
	// mà không ghi thì lần Pin sau sẽ đọc lên toàn số 0 — không phải page rỗng
	// hợp lệ (cellStart=0 chứ không phải 4096).
	f.id, f.pin, f.dirty, f.valid = id, 1, true, true
	p.table[id] = i
	p.repl.Access(i)
	p.repl.Pin(i)
	return f, nil
}

// FreePage trả page về freelist của pager và bỏ nó khỏi pool.
//
// Cờ dirty bị vứt luôn: page đã chết, ghi nội dung cuối cùng của nó xuống đĩa
// là ghi rác. (Ở phase 5 việc "trang này đã chết" sẽ phải vào log trước, chứ
// không im lặng như đây.)
func (p *Pool) FreePage(id pager.PageID) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Alloc == nil {
		return ErrNoAllocator
	}
	if i, ok := p.table[id]; ok {
		f := &p.frames[i]
		if f.pin > 0 {
			return fmt.Errorf("%w: Free page %d đang pin=%d", ErrNotPinned, id, f.pin)
		}
		delete(p.table, id)
		f.valid, f.dirty = false, false
		p.repl.Pin(i) // rút khỏi hàng đợi nạn nhân; victim() sẽ nhặt nó vì !valid
	}
	return p.Alloc.Free(id)
}
