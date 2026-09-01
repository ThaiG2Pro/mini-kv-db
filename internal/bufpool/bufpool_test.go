package bufpool

import (
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"minidb/internal/page"
	"minidb/internal/pager"
)

func policies(n int) []Replacer {
	return []Replacer{NewLRU(n), NewClock(n), NewLRUK(n, 2)}
}

// ---------- bất biến 1: không đuổi page đang pin ----------

func TestPinnedPageNeverEvicted(t *testing.T) {
	for _, r := range policies(4) {
		t.Run(r.Name(), func(t *testing.T) {
			st := NewMemStore(64)
			p := New(st, 4, r)
			for i := 0; i < 4; i++ {
				if _, err := p.Pin(pager.PageID(i)); err != nil {
					t.Fatalf("Pin(%d): %v", i, err)
				}
			}
			_, err := p.Pin(4)
			if !errors.Is(err, ErrNoFrame) {
				t.Fatalf("pool đầy toàn page bị pin mà Pin(4) trả %v — lẽ ra phải là ErrNoFrame", err)
			}
			for i := 0; i < 4; i++ {
				if !p.Contains(pager.PageID(i)) {
					t.Fatalf("page %d bị đuổi dù đang pin", i)
				}
			}
			if err := p.Verify(); err != nil {
				t.Fatal(err)
			}
			// Thả một cái thì phải nạp được ngay.
			if err := p.Unpin(2, false); err != nil {
				t.Fatal(err)
			}
			if _, err := p.Pin(4); err != nil {
				t.Fatalf("thả 1 frame rồi mà vẫn không Pin được: %v", err)
			}
		})
	}
}

func TestUnpinCleanDoesNotClearDirty(t *testing.T) {
	st := NewMemStore(8)
	p := New(st, 2, NewLRU(2))
	f, err := p.Pin(1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Pin(1); err != nil { // pin lần hai
		t.Fatal(err)
	}
	copy(f.Data[page.HeaderSize:], []byte("xin chao"))
	if err := p.Unpin(1, true); err != nil {
		t.Fatal(err)
	}
	if err := p.Unpin(1, false); err != nil { // người thứ hai chỉ đọc
		t.Fatal(err)
	}
	if p.DirtyCount() != 1 {
		t.Fatalf("Unpin(dirty=false) đã xoá mất cờ bẩn — thay đổi sẽ biến mất khi evict")
	}
}

func TestDirtyEvictionPreservesContent(t *testing.T) {
	for _, r := range policies(2) {
		t.Run(r.Name(), func(t *testing.T) {
			st := NewMemStore(16)
			p := New(st, 2, r)
			f, err := p.Pin(3)
			if err != nil {
				t.Fatal(err)
			}
			copy(f.Data[page.HeaderSize:], []byte("du lieu quan trong"))
			if err := p.Unpin(3, true); err != nil {
				t.Fatal(err)
			}
			// Đẩy nó ra bằng cách nạp đủ page khác.
			for i := 10; i < 14; i++ {
				if _, err := p.Pin(pager.PageID(i)); err != nil {
					t.Fatal(err)
				}
				if err := p.Unpin(pager.PageID(i), false); err != nil {
					t.Fatal(err)
				}
			}
			if p.Contains(3) {
				t.Fatal("page 3 chưa bị đuổi — test không chứng minh được gì")
			}
			f2, err := p.Pin(3)
			if err != nil {
				t.Fatal(err)
			}
			if got := string(f2.Data[page.HeaderSize : page.HeaderSize+18]); got != "du lieu quan trong" {
				t.Fatalf("nội dung sau khi evict rồi nạp lại = %q", got)
			}
			if st.Writes != 1 {
				t.Fatalf("store.Writes = %d, chờ đúng 1 (chỉ page bẩn mới được ghi)", st.Writes)
			}
		})
	}
}

func TestCleanPageIsNeverWritten(t *testing.T) {
	st := NewMemStore(64)
	p := New(st, 4, NewClock(4))
	for i := 0; i < 40; i++ {
		id := pager.PageID(i % 20)
		if _, err := p.Pin(id); err != nil {
			t.Fatal(err)
		}
		if err := p.Unpin(id, false); err != nil {
			t.Fatal(err)
		}
	}
	if st.Writes != 0 {
		t.Fatalf("store.Writes = %d dù không ai sửa page nào", st.Writes)
	}
	if st.Reads == 0 {
		t.Fatal("store.Reads = 0 — trace không hề gây miss, test vô nghĩa")
	}
}

// ---------- bất biến 2: WAL rule (điểm móc cho phase 5) ----------

func TestWALRuleBlocksDirtyEviction(t *testing.T) {
	st := NewMemStore(32)
	p := New(st, 2, NewLRU(2))
	flushedLSN := uint64(10)
	p.FlushLog = func(pageLSN uint64) error {
		if pageLSN > flushedLSN {
			return fmt.Errorf("pageLSN=%d > flushedLSN=%d", pageLSN, flushedLSN)
		}
		return nil
	}

	// page 1: bẩn với LSN 99 — log CHƯA fsync tới đó.
	f, err := p.Pin(1)
	if err != nil {
		t.Fatal(err)
	}
	f.Data.SetLSN(99)
	if err := p.Unpin(1, true); err != nil {
		t.Fatal(err)
	}
	// page 2: sạch — đây là nạn nhân hợp lệ duy nhất.
	if _, err := p.Pin(2); err != nil {
		t.Fatal(err)
	}
	if err := p.Unpin(2, false); err != nil {
		t.Fatal(err)
	}

	if _, err := p.Pin(3); err != nil {
		t.Fatalf("Pin(3): %v", err)
	}
	if !p.Contains(1) {
		t.Fatal("page bẩn có pageLSN chưa fsync đã bị ghi xuống đĩa — vỡ WAL rule")
	}
	if p.Contains(2) {
		t.Fatal("nạn nhân đáng ra phải là page 2 (sạch)")
	}
	if st.Writes != 0 {
		t.Fatalf("store.Writes = %d — không được phép ghi gì cả", st.Writes)
	}
	if err := p.Unpin(3, false); err != nil {
		t.Fatal(err)
	}

	// Khi mọi ứng viên đều bị WAL rule chặn thì pool phải chịu thua, không
	// được lén ghi.
	flushedLSN = 0
	f2, err := p.Pin(2)
	if err != nil {
		t.Fatal(err)
	}
	f2.Data.SetLSN(50)
	if err := p.Unpin(2, true); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Pin(4); !errors.Is(err, ErrNoFrame) {
		t.Fatalf("mọi ứng viên đều bị WAL chặn mà Pin trả %v", err)
	}
	if st.Writes != 0 {
		t.Fatalf("store.Writes = %d — pool đã ghi vòng qua WAL rule", st.Writes)
	}

	// fsync log tới nơi thì mọi thứ thông trở lại.
	flushedLSN = 1000
	if _, err := p.Pin(4); err != nil {
		t.Fatalf("log đã fsync mà vẫn không evict được: %v", err)
	}
	if st.Writes == 0 {
		t.Fatal("log đã fsync rồi mà page bẩn vẫn không được ghi")
	}
}

// ---------- checker có răng không? ----------

func TestVerifyCatchesBrokenTable(t *testing.T) {
	st := NewMemStore(8)
	p := New(st, 2, NewLRU(2))
	if _, err := p.Pin(1); err != nil {
		t.Fatal(err)
	}
	if err := p.Verify(); err != nil {
		t.Fatalf("pool lành lặn mà Verify đã kêu: %v", err)
	}
	// Bẻ bảng tra: trỏ page 1 sang frame sai.
	p.table[1] = 1
	if err := p.Verify(); !errors.Is(err, ErrInvariant) {
		t.Fatalf("bảng tra sai mà Verify() = %v", err)
	}
	p.table[1] = 0
	// Bẻ I4: nói với replacer rằng frame đang pin vẫn đuổi được.
	p.repl.Unpin(0)
	if err := p.Verify(); !errors.Is(err, ErrInvariant) {
		t.Fatalf("frame đang pin mà replacer coi là đuổi được — Verify() = %v", err)
	}
}

// ---------- đối chiếu với model trong RAM ----------

func TestRandomOpsKeepInvariants(t *testing.T) {
	const nPages, nFrames, nOps = 40, 8, 50000
	for _, r := range policies(nFrames) {
		t.Run(r.Name(), func(t *testing.T) {
			st := NewMemStore(nPages)
			p := New(st, nFrames, r)
			rng := rand.New(rand.NewSource(7))
			model := make([][]byte, nPages)
			for i := range model {
				model[i] = make([]byte, page.PageSize)
				copy(model[i], st.pages[i])
			}
			pinned := map[pager.PageID]int{}

			for op := 0; op < nOps; op++ {
				switch {
				case len(pinned) >= nFrames || (len(pinned) > 0 && rng.Intn(2) == 0):
					// thả một cái đang pin
					for id := range pinned {
						dirty := rng.Intn(3) == 0
						if dirty {
							// sửa cùng lúc ở cả pool lẫn model
							f, _ := p.Pin(id)
							v := byte(rng.Intn(256))
							f.Data[page.HeaderSize] = v
							model[id][page.HeaderSize] = v
							if err := p.Unpin(id, true); err != nil {
								t.Fatal(err)
							}
						}
						if err := p.Unpin(id, dirty); err != nil {
							t.Fatalf("op %d Unpin(%d): %v", op, id, err)
						}
						pinned[id]--
						if pinned[id] == 0 {
							delete(pinned, id)
						}
						break
					}
				default:
					id := pager.PageID(rng.Intn(nPages))
					if _, err := p.Pin(id); err != nil {
						t.Fatalf("op %d Pin(%d): %v", op, id, err)
					}
					pinned[id]++
				}
				if err := p.Verify(); err != nil {
					t.Fatalf("op %d: %v", op, err)
				}
			}
			for id, n := range pinned {
				for i := 0; i < n; i++ {
					if err := p.Unpin(id, false); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := p.FlushAll(); err != nil {
				t.Fatal(err)
			}
			for i := range model {
				if string(model[i]) != string(st.pages[i]) {
					t.Fatalf("page %d khác model sau FlushAll", i)
				}
			}
			s := p.Stats()
			if s.Hits == 0 || s.Evictions == 0 || s.DirtyEvictions == 0 {
				t.Fatalf("trace không chạm tới đường nào đáng kể: %+v", s)
			}
			t.Logf("%s: %+v hit=%.3f", r.Name(), s, s.HitRatio())
		})
	}
}

// ---------- LRU của pool có đúng là LRU không? ----------

// refLRUHits mô phỏng LRU một cách ngây thơ và độc lập: một slice, dùng tới
// đâu đẩy lên cuối tới đó. Chậm nhưng không thể sai.
func refLRUHits(trace []pager.PageID, frames int) int {
	var order []pager.PageID
	hits := 0
	for _, id := range trace {
		found := -1
		for i, x := range order {
			if x == id {
				found = i
				break
			}
		}
		if found >= 0 {
			hits++
			order = append(append(order[:found], order[found+1:]...), id)
			continue
		}
		if len(order) == frames {
			order = order[1:]
		}
		order = append(order, id)
	}
	return hits
}

func TestPoolLRUAgreesWithReferenceLRU(t *testing.T) {
	trace := Zipf(200, 20000, 1.1, 42)
	const frames = 32
	st := NewMemStore(200)
	p := New(st, frames, NewLRU(frames))
	if err := Replay(p, trace, 0); err != nil {
		t.Fatal(err)
	}
	got := p.Stats().Hits
	want := int64(refLRUHits(trace, frames))
	if got != want {
		t.Fatalf("hit của pool = %d, model LRU độc lập = %d", got, want)
	}
	t.Logf("hai cài đặt LRU độc lập cùng ra %d hit / %d thao tác", got, len(trace))
}

// ---------- Belady là trần ----------

func TestBeladyIsUpperBound(t *testing.T) {
	trace := Zipf(200, 20000, 1.05, 5)
	const frames = 32
	opt := Belady(trace, frames)
	for _, r := range policies(frames) {
		st := NewMemStore(200)
		p := New(st, frames, r)
		if err := Replay(p, trace, 0); err != nil {
			t.Fatal(err)
		}
		h := int(p.Stats().Hits)
		if h > opt {
			t.Fatalf("%s đạt %d hit > Belady %d — hoặc Belady sai, hoặc policy gian lận", r.Name(), h, opt)
		}
		t.Logf("%-6s %d hit / OPT %d = %.3f", r.Name(), h, opt, float64(h)/float64(opt))
	}
}

// ---------- tích hợp với pager thật ----------

func TestPoolRoundTripsThroughPager(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	pg, err := pager.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var ids []pager.PageID
	for i := 0; i < 12; i++ {
		id, err := pg.Allocate()
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}

	p := New(pg, 4, NewClock(4))
	for i, id := range ids {
		f, err := p.Pin(id)
		if err != nil {
			t.Fatal(err)
		}
		page.Init(f.Data, page.TypeHeap)
		if _, err := f.Data.Insert([]byte(fmt.Sprintf("ban ghi cua page %d", i))); err != nil {
			t.Fatal(err)
		}
		if err := p.Unpin(id, true); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.Verify(); err != nil {
		t.Fatal(err)
	}
	if p.Stats().Evictions == 0 {
		t.Fatal("12 page qua pool 4 frame mà không evict lần nào")
	}
	// FlushAll TRƯỚC Commit: pager chỉ đảm bảo atomicity cho cái đã trên đĩa.
	if err := p.FlushAll(); err != nil {
		t.Fatal(err)
	}
	if err := pg.Commit(ids[0]); err != nil {
		t.Fatal(err)
	}
	if err := pg.Close(); err != nil {
		t.Fatal(err)
	}

	pg2, err := pager.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer pg2.Close()
	buf := make([]byte, page.PageSize)
	for i, id := range ids {
		if err := pg2.ReadPage(id, buf); err != nil {
			t.Fatal(err)
		}
		rec, err := page.Page(buf).Get(0)
		if err != nil {
			t.Fatalf("page %d: %v", id, err)
		}
		want := fmt.Sprintf("ban ghi cua page %d", i)
		if string(rec) != want {
			t.Fatalf("page %d đọc lại được %q, chờ %q", id, rec, want)
		}
	}
}

func TestFlushAllThenCommitIsTheOnlySafeOrder(t *testing.T) {
	dir := t.TempDir()
	pg, err := pager.Open(filepath.Join(dir, "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	id, err := pg.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	p := New(pg, 2, NewLRU(2))
	f, err := p.Pin(id)
	if err != nil {
		t.Fatal(err)
	}
	page.Init(f.Data, page.TypeHeap)
	if _, err := f.Data.Insert([]byte("chua flush")); err != nil {
		t.Fatal(err)
	}
	if err := p.Unpin(id, true); err != nil {
		t.Fatal(err)
	}
	// Commit mà QUÊN FlushAll: pager ghi meta page trỏ tới một page mà nội
	// dung vẫn còn nằm trong RAM.
	if err := pg.Commit(id); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, page.PageSize)
	if err := pg.ReadPage(id, buf); err != nil {
		t.Fatal(err)
	}
	if page.Page(buf).NumSlots() != 0 {
		t.Fatal("page trên đĩa đã có dữ liệu — test này không còn chứng minh được gì")
	}
	if _, err := os.Stat(pg.Path()); err != nil {
		t.Fatal(err)
	}
	t.Log("đúng như dự đoán: commit mà quên FlushAll -> trên đĩa page vẫn rỗng (0 slot)")
}

// ---------- thí nghiệm chính của phase: sequential flooding ----------

func hotHitRatio(t *testing.T, r Replacer, frames int, trace []pager.PageID, hot int) float64 {
	t.Helper()
	st := NewMemStore(4096)
	p := New(st, frames, r)
	h, n, err := ReplayHot(p, trace, pager.PageID(hot))
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("trace không có thao tác nào vào vùng nóng")
	}
	return float64(h) / float64(n)
}

func TestScanFloodsLRUButNotLRUK(t *testing.T) {
	const (
		hot    = 200
		cold   = 1800
		frames = 64
		ops    = 200000
	)
	clean := Zipf(hot, ops, 1.05, 11)
	dirtyTrace := ZipfWithScan(hot, cold, ops, 1.05, 11, 500, 200)

	lruClean := hotHitRatio(t, NewLRU(frames), frames, clean, hot)
	lruScan := hotHitRatio(t, NewLRU(frames), frames, dirtyTrace, hot)
	clockScan := hotHitRatio(t, NewClock(frames), frames, dirtyTrace, hot)
	lrukScan := hotHitRatio(t, NewLRUK(frames, 2), frames, dirtyTrace, hot)

	t.Logf("hit ratio vùng nóng: lru không scan %.3f | lru có scan %.3f | clock có scan %.3f | lru-2 có scan %.3f",
		lruClean, lruScan, clockScan, lrukScan)

	// Test tự phủ quyết: nếu lần quét không thật sự phá được LRU thì mọi kết
	// luận bên dưới đều vô nghĩa, thà FAIL còn hơn báo PASS rỗng.
	if lruScan > lruClean*0.9 {
		t.Fatalf("scan chỉ làm LRU tụt từ %.3f xuống %.3f — chưa dựng được sequential flooding, bench vô nghĩa",
			lruClean, lruScan)
	}
	if lrukScan <= lruScan {
		t.Fatalf("LRU-2 (%.3f) không hơn LRU (%.3f) dưới scan — hoặc cài sai, hoặc workload chưa đúng",
			lrukScan, lruScan)
	}
}

// ---------- đa luồng ----------

// TestConcurrentPinUnpin chạy dưới -race. Nó kiểm tra hai tầng khoá KHÁC NHAU
// (phase 0, mục latch vs lock):
//
//	p.mu       — bảo vệ BẢNG TRA và metadata frame, giữ vài chục ns
//	Frame.Latch — bảo vệ NỘI DUNG một page, giữ trong lúc đọc/sửa page
//
// Nếu gộp làm một thì mọi thao tác đọc page đều nối đuôi nhau qua một khoá.
func TestConcurrentPinUnpin(t *testing.T) {
	const (
		pages   = 64
		frames  = 8
		workers = 8
		ops     = 4000
	)
	for _, r := range policies(frames) {
		t.Run(r.Name(), func(t *testing.T) {
			st := NewMemStore(pages)
			p := New(st, frames, r)
			var wg sync.WaitGroup
			for w := 0; w < workers; w++ {
				wg.Add(1)
				go func(w int) {
					defer wg.Done()
					rng := rand.New(rand.NewSource(int64(w)))
					for i := 0; i < ops; i++ {
						id := pager.PageID(rng.Intn(pages))
						f, err := p.Pin(id)
						if err != nil {
							if errors.Is(err, ErrNoFrame) {
								continue // pool đang chật, thử lại sau
							}
							t.Error(err)
							return
						}
						if rng.Intn(4) == 0 {
							f.Latch.Lock()
							f.Data[page.HeaderSize] = byte(w)
							f.Latch.Unlock()
							if err := p.Unpin(id, true); err != nil {
								t.Error(err)
								return
							}
						} else {
							f.Latch.RLock()
							_ = f.Data[page.HeaderSize]
							f.Latch.RUnlock()
							if err := p.Unpin(id, false); err != nil {
								t.Error(err)
								return
							}
						}
					}
				}(w)
			}
			wg.Wait()
			if err := p.Verify(); err != nil {
				t.Fatal(err)
			}
			if p.PinnedCount() != 0 {
				t.Fatalf("còn %d frame bị pin sau khi mọi worker xong — rò rỉ pin", p.PinnedCount())
			}
			s := p.Stats()
			if s.Evictions == 0 {
				t.Fatal("không evict lần nào — test không chạm tới đường đua đáng ngại")
			}
			t.Logf("%s: %+v", r.Name(), s)
		})
	}
}
