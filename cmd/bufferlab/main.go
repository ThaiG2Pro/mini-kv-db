// bufferlab — soi buffer pool: hit ratio của từng chính sách thay thế trên các
// workload khác nhau, so với giới hạn trên Belady.
//
// Mọi thứ chạy trên store trong RAM: cần đo CHÍNH SÁCH, không đo ổ đĩa.
package main

import (
	"flag"
	"fmt"

	"minidb/internal/bufpool"
	"minidb/internal/pager"
)

func main() {
	var (
		hot    = flag.Int("hot", 200, "số page thuộc vùng nóng")
		cold   = flag.Int("cold", 1800, "số page thuộc vùng lạnh (chỗ để scan)")
		frames = flag.Int("frames", 64, "số frame của pool")
		ops    = flag.Int("ops", 200000, "số thao tác của trace")
		s      = flag.Float64("s", 1.05, "độ nghiêng zipf (phải > 1)")
		seed   = flag.Int64("seed", 11, "seed")
	)
	flag.Parse()

	total := *hot + *cold
	fmt.Printf("pool %d frame / %d page (%.1f%% dữ liệu), zipf s=%.2f, %d thao tác\n\n",
		*frames, total, 100*float64(*frames)/float64(total), *s, *ops)

	fmt.Println("== 1. hit ratio TỔNG theo workload ==")
	fmt.Printf("%-22s %8s %8s %8s %8s %8s\n", "workload", "lru", "clock", "lru-2", "OPT", "lru/OPT")
	workloads := []struct {
		name  string
		trace []pager.PageID
	}{
		{"uniform", bufpool.Uniform(total, *ops, *seed)},
		{"zipf (không scan)", bufpool.Zipf(*hot, *ops, *s, *seed)},
		{"zipf + scan 500/200", bufpool.ZipfWithScan(*hot, *cold, *ops, *s, *seed, 500, 200)},
	}
	for _, w := range workloads {
		opt := float64(bufpool.Belady(w.trace, *frames)) / float64(len(w.trace))
		r := make([]float64, 3)
		for i, repl := range []bufpool.Replacer{
			bufpool.NewLRU(*frames), bufpool.NewClock(*frames), bufpool.NewLRUK(*frames, 2),
		} {
			p := bufpool.New(bufpool.NewMemStore(total), *frames, repl)
			must(bufpool.Replay(p, w.trace, 0))
			r[i] = p.Stats().HitRatio()
		}
		fmt.Printf("%-22s %8.3f %8.3f %8.3f %8.3f %8.2f\n", w.name, r[0], r[1], r[2], opt, r[0]/opt)
	}

	fmt.Println("\n== 2. sequential flooding: hit ratio của RIÊNG vùng nóng ==")
	fmt.Println("(page bị quét chỉ được chạm 1 lần nên luôn miss — trộn vào tỉ lệ tổng sẽ làm loãng)")
	fmt.Printf("%-14s %8s %8s %8s %10s\n", "scan/200 op", "lru", "clock", "lru-2", "lru-2/lru")
	for _, scanLen := range []int{0, 50, 100, 200, 500, 1000} {
		var trace []pager.PageID
		if scanLen == 0 {
			trace = bufpool.Zipf(*hot, *ops, *s, *seed)
		} else {
			trace = bufpool.ZipfWithScan(*hot, *cold, *ops, *s, *seed, scanLen, 200)
		}
		r := make([]float64, 3)
		for i, repl := range []bufpool.Replacer{
			bufpool.NewLRU(*frames), bufpool.NewClock(*frames), bufpool.NewLRUK(*frames, 2),
		} {
			p := bufpool.New(bufpool.NewMemStore(total), *frames, repl)
			h, n, err := bufpool.ReplayHot(p, trace, pager.PageID(*hot))
			must(err)
			r[i] = float64(h) / float64(n)
		}
		fmt.Printf("%-14d %8.3f %8.3f %8.3f %10.2f\n", scanLen, r[0], r[1], r[2], r[2]/r[0])
	}

	fmt.Println("\n== 3. pool to bao nhiêu thì đủ? (zipf, không scan) ==")
	fmt.Printf("%-10s %-10s %8s %8s %8s %8s\n", "frames", "%dữ liệu", "lru", "clock", "lru-2", "OPT")
	trace := bufpool.Zipf(*hot, *ops, *s, *seed)
	for _, nf := range []int{8, 16, 32, 64, 128, 256} {
		if nf > *hot {
			break
		}
		opt := float64(bufpool.Belady(trace, nf)) / float64(len(trace))
		r := make([]float64, 3)
		for i, repl := range []bufpool.Replacer{
			bufpool.NewLRU(nf), bufpool.NewClock(nf), bufpool.NewLRUK(nf, 2),
		} {
			p := bufpool.New(bufpool.NewMemStore(total), nf, repl)
			must(bufpool.Replay(p, trace, 0))
			r[i] = p.Stats().HitRatio()
		}
		fmt.Printf("%-10d %-10.1f %8.3f %8.3f %8.3f %8.3f\n",
			nf, 100*float64(nf)/float64(*hot), r[0], r[1], r[2], opt)
	}

	fmt.Println("\n== 4. bản đồ pool (16 frame, . trống  c sạch  D bẩn  p pin  P pin+bẩn) ==")
	st := bufpool.NewMemStore(64)
	p := bufpool.New(st, 16, bufpool.NewClock(16))
	for i := 0; i < 10; i++ {
		f, err := p.Pin(pager.PageID(i))
		must(err)
		dirty := i%3 == 0
		if dirty {
			f.Data.SetLSN(uint64(i))
		}
		must(p.Unpin(pager.PageID(i), dirty))
	}
	fmt.Printf("nạp 10 page, cứ 3 page bẩn 1:      %s\n", p.Dump())
	for i := 10; i < 14; i++ {
		_, err := p.Pin(pager.PageID(i))
		must(err)
	}
	fmt.Printf("pin thêm 4 page (chưa thả):        %s\n", p.Dump())
	must(p.FlushAll())
	fmt.Printf("sau FlushAll (bẩn -> sạch):        %s\n", p.Dump())
	fmt.Printf("store: %d lần đọc, %d lần ghi | %+v\n", st.Reads, st.Writes, p.Stats())
	if err := p.Verify(); err != nil {
		fmt.Println("VỠ BẤT BIẾN:", err)
	} else {
		fmt.Println("Verify(): mọi bất biến còn nguyên")
	}
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
