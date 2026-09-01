package bufpool

import (
	"errors"
	"fmt"
	"strings"

	"minidb/internal/pager"
)

var ErrInvariant = errors.New("bufpool: vỡ bất biến")

// Verify kiểm tra toàn bộ bất biến của pool. Gọi được sau MỌI thao tác — test
// ngẫu nhiên gọi nó sau từng bước, nên nó phải rẻ và phải bắt được thật.
//
// I1 bảng tra và frame khớp nhau hai chiều
// I2 một PageID không nằm ở hai frame (nếu vỡ: hai bản sao của cùng một page,
//
//	ghi vào bản này thì mất ở bản kia — hỏng dữ liệu im lặng)
//
// I3 pin count không âm
// I4 frame đang pin thì replacer phải coi là KHÔNG đuổi được
// I5 frame không hợp lệ thì không được bẩn và không được pin
func (p *Pool) Verify() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	seen := make(map[pager.PageID]int, len(p.frames))
	for i := range p.frames {
		f := &p.frames[i]
		if f.pin < 0 {
			return fmt.Errorf("%w (I3): frame %d pin=%d", ErrInvariant, i, f.pin)
		}
		if !f.valid {
			if f.dirty || f.pin > 0 {
				return fmt.Errorf("%w (I5): frame %d không hợp lệ nhưng dirty=%v pin=%d",
					ErrInvariant, i, f.dirty, f.pin)
			}
			continue
		}
		if j, dup := seen[f.id]; dup {
			return fmt.Errorf("%w (I2): page %d nằm ở cả frame %d lẫn frame %d",
				ErrInvariant, f.id, j, i)
		}
		seen[f.id] = i
		if got, ok := p.table[f.id]; !ok || got != i {
			return fmt.Errorf("%w (I1): frame %d giữ page %d nhưng bảng tra nói frame %d (có=%v)",
				ErrInvariant, i, f.id, got, ok)
		}
		if f.pin > 0 && p.repl.Evictable(i) {
			return fmt.Errorf("%w (I4): frame %d giữ page %d đang pin=%d nhưng replacer %s vẫn coi là đuổi được",
				ErrInvariant, i, f.id, f.pin, p.repl.Name())
		}
	}
	for id, i := range p.table {
		if i < 0 || i >= len(p.frames) {
			return fmt.Errorf("%w (I1): bảng tra page %d -> frame %d ngoài phạm vi", ErrInvariant, id, i)
		}
		if f := &p.frames[i]; !f.valid || f.id != id {
			return fmt.Errorf("%w (I1): bảng tra page %d -> frame %d nhưng frame giữ page %d (valid=%v)",
				ErrInvariant, id, i, f.id, f.valid)
		}
	}
	return nil
}

// Dump vẽ trạng thái pool cho cmd/bufferlab. Mỗi frame một ký tự:
//
//	.  trống      c  sạch      D  bẩn      p  đang pin (sạch)      P  đang pin + bẩn
func (p *Pool) Dump() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var b strings.Builder
	for i := range p.frames {
		f := &p.frames[i]
		switch {
		case !f.valid:
			b.WriteByte('.')
		case f.pin > 0 && f.dirty:
			b.WriteByte('P')
		case f.pin > 0:
			b.WriteByte('p')
		case f.dirty:
			b.WriteByte('D')
		default:
			b.WriteByte('c')
		}
	}
	return b.String()
}

// Resident liệt kê PageID đang nằm trong pool, theo thứ tự frame.
func (p *Pool) Resident() []pager.PageID {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]pager.PageID, 0, len(p.frames))
	for i := range p.frames {
		if p.frames[i].valid {
			out = append(out, p.frames[i].id)
		}
	}
	return out
}
