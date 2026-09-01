package bufpool

import (
	"math/rand"

	"minidb/internal/page"
	"minidb/internal/pager"
)

// MemStore là "đĩa" trong RAM: đủ để đếm số lần đọc/ghi mà không đo tốc độ ổ
// đĩa. Bench hit-ratio PHẢI dùng cái này — nếu để store thật thì con số đo được
// là đặc tính của ổ SSD, không phải của chính sách thay thế.
type MemStore struct {
	pages         [][]byte
	Reads, Writes int
}

func NewMemStore(n int) *MemStore {
	s := &MemStore{pages: make([][]byte, n)}
	for i := range s.pages {
		s.pages[i] = make([]byte, page.PageSize)
		page.Init(page.Page(s.pages[i]), page.TypeHeap)
	}
	return s
}

func (s *MemStore) ReadPage(id pager.PageID, buf []byte) error {
	s.Reads++
	copy(buf, s.pages[id])
	return nil
}

func (s *MemStore) WritePage(id pager.PageID, buf []byte) error {
	s.Writes++
	copy(s.pages[id], buf)
	return nil
}

// ---------- workload ----------

// Uniform: mọi page có xác suất bằng nhau. Đây là workload TỆ NHẤT cho mọi
// buffer pool — không có locality thì không có gì để giữ lại.
func Uniform(pages, ops int, seed int64) []pager.PageID {
	rng := rand.New(rand.NewSource(seed))
	tr := make([]pager.PageID, ops)
	for i := range tr {
		tr[i] = pager.PageID(rng.Intn(pages))
	}
	return tr
}

// Zipf: một ít page rất nóng, phần đuôi rất dài — hình dạng thật của workload
// OLTP (và của mọi thứ có người dùng thật đứng sau).
// Bẫy: rand.NewZipf trả về nil (KHÔNG phải lỗi) khi s <= 1, và chỗ panic sẽ
// nằm mãi tận lần gọi Uint64 sau đó. Chặn ngay tại đây cho khỏi mất công tìm.
func Zipf(pages, ops int, s float64, seed int64) []pager.PageID {
	if s <= 1 {
		panic("bufpool: rand.NewZipf cần s > 1")
	}
	rng := rand.New(rand.NewSource(seed))
	z := rand.NewZipf(rng, s, 1, uint64(pages-1))
	tr := make([]pager.PageID, ops)
	for i := range tr {
		tr[i] = pager.PageID(z.Uint64())
	}
	return tr
}

// ZipfWithScan trộn sequential scan vào workload zipfian: cứ `period` thao tác
// thì quét tuần tự `scanLen` page ở vùng LẠNH (không giao với vùng nóng).
//
// Đây là "sequential flooding" — thứ mà mọi tài liệu DB nhắc tới khi nói vì sao
// đừng dùng LRU thuần. Mỗi page trong lần quét chỉ được chạm ĐÚNG MỘT LẦN,
// nhưng với LRU thì "vừa chạm" = "quý nhất", nên nó đẩy sạch vùng nóng ra.
func ZipfWithScan(hotPages, coldPages, ops int, s float64, seed int64, scanLen, period int) []pager.PageID {
	if s <= 1 {
		panic("bufpool: rand.NewZipf cần s > 1")
	}
	rng := rand.New(rand.NewSource(seed))
	z := rand.NewZipf(rng, s, 1, uint64(hotPages-1))
	tr := make([]pager.PageID, 0, ops)
	scanAt := pager.PageID(hotPages)
	for len(tr) < ops {
		for i := 0; i < period && len(tr) < ops; i++ {
			tr = append(tr, pager.PageID(z.Uint64()))
		}
		for i := 0; i < scanLen && len(tr) < ops; i++ {
			tr = append(tr, scanAt)
			scanAt++
			if int(scanAt) >= hotPages+coldPages {
				scanAt = pager.PageID(hotPages)
			}
		}
	}
	return tr
}

// Replay chạy hết trace qua pool: Pin rồi Unpin ngay (mô hình một thao tác đọc
// một page). writeEvery > 0 thì cứ n thao tác lại đánh dấu bẩn một lần.
func Replay(p *Pool, trace []pager.PageID, writeEvery int) error {
	for i, id := range trace {
		f, err := p.Pin(id)
		if err != nil {
			return err
		}
		dirty := writeEvery > 0 && i%writeEvery == 0
		if dirty {
			f.Data.SetLSN(uint64(i))
		}
		if err := p.Unpin(id, dirty); err != nil {
			return err
		}
	}
	return nil
}

// ---------- giới hạn trên: Belady ----------

// Belady mô phỏng chính sách tối ưu OFFLINE: đuổi page mà lần dùng KẾ TIẾP còn
// xa nhất. Không cài được trong DB thật (cần biết tương lai), nhưng là thước đo
// duy nhất trả lời được "còn bao nhiêu phần trăm nữa để mất công tối ưu".
//
// Trả về số hit.
func Belady(trace []pager.PageID, frames int) int {
	// nextUse[i] = vị trí lần dùng kế tiếp của trace[i], len(trace) nếu không còn.
	nextUse := make([]int, len(trace))
	last := make(map[pager.PageID]int, frames*4)
	for i := len(trace) - 1; i >= 0; i-- {
		if j, ok := last[trace[i]]; ok {
			nextUse[i] = j
		} else {
			nextUse[i] = len(trace)
		}
		last[trace[i]] = i
	}

	resident := make(map[pager.PageID]int, frames) // page -> nextUse của nó
	hits := 0
	for i, id := range trace {
		if _, ok := resident[id]; ok {
			hits++
			resident[id] = nextUse[i]
			continue
		}
		if len(resident) == frames {
			var victim pager.PageID
			far := -1
			for pid, nu := range resident {
				if nu > far {
					victim, far = pid, nu
				}
			}
			delete(resident, victim)
		}
		resident[id] = nextUse[i]
	}
	return hits
}

// ReplayHot chạy trace và đếm hit RIÊNG cho nhóm page nóng (id < hotBelow).
//
// Vì sao phải tách: trong workload có sequential scan, các page bị quét chỉ
// được chạm đúng một lần nên chúng luôn là miss — trộn chung vào một tỉ lệ
// tổng sẽ làm loãng đúng cái ta muốn nhìn, tức là "vùng nóng có sống sót qua
// lần quét không".
func ReplayHot(p *Pool, trace []pager.PageID, hotBelow pager.PageID) (hotHits, hotOps int, err error) {
	for _, id := range trace {
		before := p.Stats().Hits
		f, err := p.Pin(id)
		if err != nil {
			return hotHits, hotOps, err
		}
		_ = f
		hit := p.Stats().Hits > before
		if err := p.Unpin(id, false); err != nil {
			return hotHits, hotOps, err
		}
		if id < hotBelow {
			hotOps++
			if hit {
				hotHits++
			}
		}
	}
	return hotHits, hotOps, nil
}
