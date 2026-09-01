package bufpool

import (
	"errors"
	"testing"

	"minidb/internal/page"
	"minidb/internal/pager"
)

// runPoolProgram diễn giải chuỗi byte prog thành thao tác trên pool và kiểm
// tra bất biến sau MỖI thao tác.
//
// Tách khỏi Fuzz để test có seed cũng chạy được đúng thân này — bài học phase 2:
// mọi nghi vấn về fuzz phải tái hiện được bằng một test thường.
func runPoolProgram(t *testing.T, prog []byte, frames int, repl Replacer) {
	t.Helper()
	const nPages = 24
	st := NewMemStore(nPages)
	p := New(st, frames, repl)
	// flushedLSN giả lập WAL của phase 5: có lúc chặn, có lúc không.
	flushed := uint64(0)
	p.FlushLog = func(lsn uint64) error {
		if lsn > flushed {
			return errors.New("log chưa fsync tới đó")
		}
		return nil
	}
	pins := map[pager.PageID]int{}

	for i := 0; i+1 < len(prog); i += 2 {
		op, arg := prog[i]%5, pager.PageID(prog[i+1]%nPages)
		switch op {
		case 0, 1: // Pin
			f, err := p.Pin(arg)
			if err != nil {
				if !errors.Is(err, ErrNoFrame) {
					t.Fatalf("Pin(%d): %v", arg, err)
				}
			} else {
				pins[arg]++
				f.Data.SetLSN(uint64(prog[i]))
			}
		case 2: // Unpin
			if pins[arg] > 0 {
				if err := p.Unpin(arg, prog[i]&0x80 != 0); err != nil {
					t.Fatalf("Unpin(%d): %v", arg, err)
				}
				pins[arg]--
			}
		case 3: // Flush một page. ErrWALRule là câu trả lời HỢP LỆ: page có
			// pageLSN vượt quá phần log đã fsync thì không được ghi.
			if err := p.Flush(arg); err != nil && !errors.Is(err, ErrWALRule) {
				t.Fatalf("Flush(%d): %v", arg, err)
			}
		case 4: // đẩy flushedLSN lên, có lúc FlushAll
			flushed = uint64(prog[i+1])
			if prog[i]&0x40 != 0 {
				if err := p.FlushAll(); err != nil && !errors.Is(err, ErrWALRule) {
					t.Fatalf("FlushAll: %v", err)
				}
			}
		}
		if err := p.Verify(); err != nil {
			t.Fatalf("sau thao tác %d (op=%d arg=%d): %v", i/2, op, arg, err)
		}
	}

	// Dọn: thả hết pin rồi FlushAll với log đã fsync hết -> pool phải sạch.
	for id, n := range pins {
		for k := 0; k < n; k++ {
			if err := p.Unpin(id, false); err != nil {
				t.Fatal(err)
			}
		}
	}
	flushed = ^uint64(0)
	if err := p.FlushAll(); err != nil {
		t.Fatal(err)
	}
	if n := p.DirtyCount(); n != 0 {
		t.Fatalf("còn %d page bẩn sau FlushAll", n)
	}
	if n := p.PinnedCount(); n != 0 {
		t.Fatalf("còn %d frame bị pin sau khi thả hết", n)
	}
	if err := p.Verify(); err != nil {
		t.Fatal(err)
	}
	_ = page.PageSize
}

func FuzzPoolOps(f *testing.F) {
	f.Add([]byte{0, 1, 0, 2, 2, 1, 3, 1, 4, 200})
	f.Add([]byte{0, 1, 0, 1, 0, 2, 2, 2, 4, 0, 0, 3})
	f.Fuzz(func(t *testing.T, prog []byte) {
		if len(prog) > 2048 { // bài học phase 2: input dài làm worker đứng hình
			prog = prog[:2048]
		}
		frames := 2 + int(len(prog))%6
		for _, r := range []Replacer{NewLRU(frames), NewClock(frames), NewLRUK(frames, 2)} {
			runPoolProgram(t, prog, frames, r)
		}
	})
}

func TestPoolProgramStress(t *testing.T) {
	rng := newXorshift(12345)
	for iter := 0; iter < 2000; iter++ {
		n := 2 * (1 + int(rng.next()%200))
		prog := make([]byte, n)
		for i := range prog {
			prog[i] = byte(rng.next())
		}
		frames := 2 + iter%6
		for _, r := range []Replacer{NewLRU(frames), NewClock(frames), NewLRUK(frames, 2)} {
			runPoolProgram(t, prog, frames, r)
		}
	}
}

type xorshift struct{ s uint64 }

func newXorshift(seed uint64) *xorshift { return &xorshift{s: seed} }
func (x *xorshift) next() uint64 {
	x.s ^= x.s << 13
	x.s ^= x.s >> 7
	x.s ^= x.s << 17
	return x.s
}
