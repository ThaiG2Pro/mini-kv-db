// btreelab — soi một B+Tree: hình dạng cây, số lần split, độ đầy của lá, và
// giá phải trả cho việc chèn khóa ngẫu nhiên thay vì khóa tăng dần.
//
// Chạy trên store trong RAM: phase này đo CẤU TRÚC cây (page chạm tới, split,
// độ đầy), không đo ổ đĩa. Muốn số của ổ đĩa thì xem lại phase 0.
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"math/rand"
	"time"

	"minidb/internal/btree"
	"minidb/internal/bufpool"
	"minidb/internal/page"
)

func keys(n int, kind string, klen int, seed int64) [][]byte {
	out := make([][]byte, n)
	rng := rand.New(rand.NewSource(seed))
	for i := range out {
		b := make([]byte, klen)
		switch kind {
		case "seq":
			binary.BigEndian.PutUint64(b[klen-8:], uint64(i))
		case "desc":
			binary.BigEndian.PutUint64(b[klen-8:], uint64(n-i))
		default: // rand — UUIDv4 giả
			rng.Read(b)
		}
		out[i] = b
	}
	return out
}

type result struct {
	name     string
	dur      time.Duration
	splits   int64
	height   int
	leaves   int
	branches int
	pages    int
	fill     float64
	writes   int64
	reads    int64
}

func run(name string, ks [][]byte, vlen, frames int, rightmost bool) result {
	val := make([]byte, vlen)
	db := btree.NewMemDB()
	pool := bufpool.New(db, frames, bufpool.NewLRU(frames))
	pool.Alloc = db
	tr, err := btree.Create(pool)
	if err != nil {
		panic(err)
	}
	tr.RightmostSplit = rightmost

	start := time.Now()
	for _, k := range ks {
		if err := tr.Put(k, val); err != nil {
			panic(err)
		}
	}
	dur := time.Since(start)

	r, err := tr.Verify()
	if err != nil {
		panic(err)
	}
	if !r.OK() {
		fmt.Println("CÂY HỎNG:", r.Errors)
	}
	st, ps := tr.Stats(), pool.Stats()
	return result{name, dur, st.Splits, r.Height, r.Leaves, r.Branches,
		db.LivePages(), r.LeafFill(), ps.Writes, ps.Reads}
}

func main() {
	var (
		n      = flag.Int("n", 200000, "số khóa chèn")
		klen   = flag.Int("klen", 16, "độ dài khóa (16 = cỡ UUID)")
		vlen   = flag.Int("vlen", 100, "độ dài giá trị")
		frames = flag.Int("frames", 256, "số frame của pool")
		seed   = flag.Int64("seed", 1, "seed")
		dump   = flag.Bool("dump", false, "in hình cây")
	)
	flag.Parse()

	fmt.Printf("page %dB, khóa %dB, giá trị %dB, pool %d frame, %d khóa\n",
		page.PageSize, *klen, *vlen, *frames, *n)
	fmt.Printf("entry tối đa lọt một node: %d byte\n\n", btree.MaxEntrySize)

	fmt.Println("== 1. thứ tự chèn quyết định hình dạng file ==")
	fmt.Printf("%-28s %9s %9s %7s %8s %8s %9s\n",
		"kịch bản", "thời gian", "splits", "height", "pages", "đầy lá", "ghi/khóa")
	var rows []result
	for _, c := range []struct {
		name      string
		kind      string
		rightmost bool
	}{
		{"tăng dần (auto-increment)", "seq", true},
		{"tăng dần, tắt tối ưu phải", "seq", false},
		{"giảm dần", "desc", true},
		{"ngẫu nhiên (UUIDv4)", "rand", true},
	} {
		r := run(c.name, keys(*n, c.kind, *klen, *seed), *vlen, *frames, c.rightmost)
		rows = append(rows, r)
		fmt.Printf("%-28s %9s %9d %7d %8d %7.1f%% %9.2f\n",
			r.name, r.dur.Round(time.Millisecond), r.splits, r.height, r.pages,
			r.fill*100, float64(r.writes)/float64(*n))
	}

	// Tỉ số mới là kết luận; con số tuyệt đối đổi theo máy.
	seq, rnd := rows[0], rows[3]
	fmt.Printf("\ntỉ số ngẫu nhiên / tăng dần: split %.2fx, page %.2fx, thời gian %.2fx, độ đầy %.2fx\n",
		float64(rnd.splits)/float64(seq.splits),
		float64(rnd.pages)/float64(seq.pages),
		float64(rnd.dur)/float64(seq.dur),
		rnd.fill/seq.fill)
	fmt.Printf("cùng %d khóa: tăng dần tốn %d page, ngẫu nhiên tốn %d page (+%.0f%% dung lượng file)\n",
		*n, seq.pages, rnd.pages, 100*(float64(rnd.pages)/float64(seq.pages)-1))

	fmt.Println("\n== 2. pool nhỏ dần: chèn ngẫu nhiên biến thành I/O ==")
	fmt.Printf("%-10s %14s %14s %10s\n", "frames", "ghi/khóa tăng", "ghi/khóa ngẫu", "tỉ số")
	for _, f := range []int{16, 64, 256, 1024} {
		a := run("seq", keys(*n, "seq", *klen, *seed), *vlen, f, true)
		b := run("rand", keys(*n, "rand", *klen, *seed), *vlen, f, true)
		wa := float64(a.writes) / float64(*n)
		wb := float64(b.writes) / float64(*n)
		ratio := 0.0
		if wa > 0 {
			ratio = wb / wa
		}
		fmt.Printf("%-10d %14.3f %14.3f %9.1fx\n", f, wa, wb, ratio)
	}

	fmt.Println("\n== 3. fanout: một page chứa được bao nhiêu khóa ==")
	fmt.Printf("%-12s %10s %12s %14s\n", "khóa (byte)", "leaf/page", "branch/page", "khóa ở 3 tầng")
	for _, kl := range []int{8, 16, 32, 64, 128} {
		r := run("f", keys(20000, "seq", kl, *seed), *vlen, 256, true)
		leafPer := float64(20000) / float64(r.leaves)
		// branch fanout = số con trên mỗi branch, suy từ số leaf và số branch
		branchPer := 0.0
		if r.branches > 0 {
			branchPer = float64(r.leaves+r.branches-1) / float64(r.branches)
		}
		fmt.Printf("%-12d %10.0f %12.0f %14.0f\n", kl, leafPer, branchPer, leafPer*branchPer*branchPer)
	}
	fmt.Println("(cột cuối: sức chứa của một cây 3 tầng — mọi tra cứu <= 3 lần chạm page)")

	fmt.Println("\n== 4. xóa: cây có trả lại page không ==")
	db := btree.NewMemDB()
	pool := bufpool.New(db, *frames, bufpool.NewLRU(*frames))
	pool.Alloc = db
	tr, _ := btree.Create(pool)
	tr.RightmostSplit = true
	ks := keys(*n, "rand", *klen, *seed)
	val := make([]byte, *vlen)
	for _, k := range ks {
		if err := tr.Put(k, val); err != nil {
			panic(err)
		}
	}
	peak := db.LivePages()
	r0, _ := tr.Verify()
	fmt.Printf("%-22s %8s %8s %8s %8s\n", "trạng thái", "khóa", "pages", "height", "đầy lá")
	fmt.Printf("%-22s %8d %8d %8d %7.1f%%\n", "sau khi chèn", r0.Keys, peak, r0.Height, r0.LeafFill()*100)
	// Một hoán vị DUY NHẤT cho cả ba vòng, và đếm khóa sống bằng biến cục bộ.
	// Bản đầu gọi tr.Count() trong thân vòng lặp: Count() quét toàn bộ leaf nên
	// mỗi lần xóa tốn O(n) — 200k khóa thành O(n^2) và lab treo quá 10 phút mà
	// chưa in xong mục 4. Xóa xong 90% khóa giờ mất chưa tới một giây.
	rng := rand.New(rand.NewSource(*seed))
	order := rng.Perm(len(ks))
	pos, live := 0, len(ks)
	for _, frac := range []int{2, 4, 10} { // xóa dần tới 50%, 75%, 90%
		want := len(ks) / frac
		for live > want && pos < len(order) {
			if err := tr.Delete(ks[order[pos]]); err == nil {
				live--
			}
			pos++
		}
		r, _ := tr.Verify()
		if !r.OK() {
			fmt.Println("CÂY HỎNG:", r.Errors)
		}
		fmt.Printf("%-22s %8d %8d %8d %7.1f%%\n",
			fmt.Sprintf("còn %d%% khóa", 100/frac), r.Keys, db.LivePages(), r.Height, r.LeafFill()*100)
	}
	st := tr.Stats()
	fmt.Printf("merges=%d redistributes=%d shrinks=%d bỏ cân bằng=%d\n",
		st.Merges, st.Redistributes, st.Shrinks, st.SkippedRebalance)
	fmt.Printf("đỉnh %d page -> %d page: trả lại %.0f%% chỗ đã cấp\n",
		peak, db.LivePages(), 100*(1-float64(db.LivePages())/float64(peak)))

	if *dump {
		fmt.Println("\n== 5. hình cây ==")
		d, err := tr.Dump(6)
		if err != nil {
			panic(err)
		}
		fmt.Println(d)
	}
}
