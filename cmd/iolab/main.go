// iolab — Phase 0: đo 4 sự thật vật lý mà mọi thiết kế DB dựa lên.
//
//  1. write() != durable          -> giá thật của fsync
//  2. group commit                -> vì sao gộp fsync là đòn bẩy lớn nhất
//  3. random vs sequential I/O    -> vì sao B+Tree tồn tại, fanout phải lớn
//  4. page cache của OS           -> vì sao buffer pool hit/miss chênh nhau ~100x
//
// Cờ quan trọng:
//
//	-repeat N   lặp cả bộ N lần, gộp mẫu lại để tính p50/p99 (đuôi phân phối mới
//	            là thứ quyết định commit latency thực tế, không phải trung bình)
//	-direct     mở file với O_DIRECT: bỏ qua page cache của kernel, đo I/O thật.
//	            Đây là cách DB thật vận hành (InnoDB, Oracle).
//	-json PATH  ghi kết quả ra JSON để so giữa các máy
//	-verify-cache  in % file còn nằm trong page cache (mincore) trước/sau khi
//	            drop cache — chứng minh posix_fadvise(DONTNEED) thật sự có tác dụng
//
// Chạy: go run ./cmd/iolab -filemb 512 -repeat 5
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"
	"unsafe"

	"runtime"
)

const (
	pageSize          = 4096
	posixFadvDontNeed = 4
)

var (
	flagDir    = flag.String("dir", "./data/iolab", "thư mục chạy thí nghiệm")
	flagFileMB = flag.Int("filemb", 512, "kích thước file cho phép đo random/sequential (MB)")
	flagRepeat = flag.Int("repeat", 1, "lặp cả bộ bao nhiêu lần (gộp mẫu để tính p50/p99)")
	flagDirect = flag.Bool("direct", false, "mở file với O_DIRECT (bỏ qua page cache của kernel)")
	flagJSON   = flag.String("json", "", "ghi kết quả ra file JSON (để so giữa các máy)")
	flagVerify = flag.Bool("verify-cache", false, "in % residency page cache (mincore) trước/sau drop cache")
	flagOps    = flag.Int("ops", 4000, "số thao tác mỗi phép đo random/sequential")
)

func must(err error) {
	if err != nil {
		panic(err)
	}
}

// ---------------------------------------------------------------------------
// Mở file, buffer aligned cho O_DIRECT
// ---------------------------------------------------------------------------

func create(path string) *os.File {
	flags := os.O_RDWR | os.O_CREATE | os.O_TRUNC
	if *flagDirect {
		flags |= syscall.O_DIRECT
	}
	f, err := os.OpenFile(path, flags, 0o644)
	if err != nil && *flagDirect {
		panic(fmt.Sprintf("mở %s với O_DIRECT thất bại: %v\n"+
			"(tmpfs, overlayfs và một số filesystem không hỗ trợ O_DIRECT — thử thư mục khác qua -dir)", path, err))
	}
	must(err)
	return f
}

// O_DIRECT đòi buffer, offset và độ dài đều aligned theo block của thiết bị.
// Go không cấp phát aligned sẵn nên phải tự cắt từ một slice lớn hơn.
func alignedBuf(n int) []byte {
	b := make([]byte, n+pageSize)
	off := int(uintptr(unsafe.Pointer(&b[0])) % pageSize)
	if off != 0 {
		off = pageSize - off
	}
	return b[off : off+n]
}

// ---------------------------------------------------------------------------
// Page cache: drop + đo residency bằng mincore(2)
// ---------------------------------------------------------------------------

// residency trả về % số page của file đang nằm trong page cache của kernel.
// Đây là cách KIỂM CHỨNG trực tiếp rằng fadvise(DONTNEED) có tác dụng — thay vì
// suy luận gián tiếp từ việc "đọc chậm hơn nên chắc là cache đã trống".
func residency(f *os.File, size int64) (float64, error) {
	if size == 0 {
		return 0, nil
	}
	data, err := syscall.Mmap(int(f.Fd()), 0, int(size), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return 0, fmt.Errorf("mmap: %w", err)
	}
	defer syscall.Munmap(data)

	// syscall.Mincore không có trong stdlib -> gọi thẳng syscall.
	// mincore(2) điền vào vec: bit 0 = page đó có đang nằm trong page cache không.
	vec := make([]byte, (size+pageSize-1)/pageSize)
	_, _, errno := syscall.Syscall(syscall.SYS_MINCORE,
		uintptr(unsafe.Pointer(&data[0])), uintptr(size), uintptr(unsafe.Pointer(&vec[0])))
	if errno != 0 {
		return 0, fmt.Errorf("mincore: %w", errno)
	}
	in := 0
	for _, v := range vec {
		if v&1 == 1 {
			in++
		}
	}
	return float64(in) / float64(len(vec)) * 100, nil
}

// dropCache bảo kernel quên nội dung file trong page cache.
// Không có bước này thì mọi phép đo "đọc đĩa" chỉ là đo memcpy từ RAM.
// Bắt buộc Sync() TRƯỚC: kernel bỏ qua DONTNEED trên page còn dirty (im lặng, không báo lỗi).
func dropCache(f *os.File, size int64) {
	must(f.Sync())

	var before float64
	if *flagVerify {
		before, _ = residency(f, size)
	}

	_, _, errno := syscall.Syscall6(syscall.SYS_FADVISE64,
		f.Fd(), 0, uintptr(size), posixFadvDontNeed, 0, 0)
	if errno != 0 {
		fmt.Printf("  (cảnh báo: fadvise DONTNEED lỗi: %v — số đo đọc có thể là cache hit)\n", errno)
	}

	if *flagVerify {
		after, err := residency(f, size)
		if err != nil {
			fmt.Printf("  [cache] không đo được residency: %v\n", err)
			return
		}
		verdict := "OK — cache đã bị đẩy ra"
		if after > 5 {
			verdict = "!! CẢNH BÁO: cache còn nhiều, số đo 'lạnh' bên dưới KHÔNG đáng tin"
		}
		fmt.Printf("  [cache] residency %.1f%% -> %.1f%%  (%s)\n", before, after, verdict)
	}
}

// ---------------------------------------------------------------------------
// Thu mẫu và thống kê
// ---------------------------------------------------------------------------

type result struct {
	section string
	name    string
	note    string
	bytes   int64 // số byte mỗi op, để tính MB/s
	samples []time.Duration
	total   time.Duration
}

// sample đo TỪNG op riêng lẻ thay vì chỉ đo tổng, để có phân phối.
// Chi phí: 2 lần time.Now() (~50ns) mỗi op. Với op ~1µs là ~5% overhead —
// chấp nhận được, và nó ảnh hưởng đều lên mọi phép đo nên tỉ số vẫn đúng.
func sample(n int, bytesPerOp int64, fn func(i int)) ([]time.Duration, time.Duration) {
	out := make([]time.Duration, n)
	start := time.Now()
	for i := 0; i < n; i++ {
		t0 := time.Now()
		fn(i)
		out[i] = time.Since(t0)
	}
	_ = bytesPerOp
	return out, time.Since(start)
}

func (r *result) merge(o result) {
	r.samples = append(r.samples, o.samples...)
	r.total += o.total
}

func (r result) opsPerSec() float64 { return float64(len(r.samples)) / r.total.Seconds() }
func (r result) mbPerSec() float64 {
	return float64(len(r.samples)) * float64(r.bytes) / 1e6 / r.total.Seconds()
}
func (r result) mean() time.Duration {
	var s time.Duration
	for _, d := range r.samples {
		s += d
	}
	return s / time.Duration(len(r.samples))
}
func (r result) pct(p float64) time.Duration {
	c := append([]time.Duration(nil), r.samples...)
	sort.Slice(c, func(i, j int) bool { return c[i] < c[j] })
	i := int(p / 100 * float64(len(c)-1))
	return c[i]
}

func rnd(d time.Duration) time.Duration {
	if d < time.Millisecond {
		return d.Round(100 * time.Nanosecond)
	}
	return d.Round(time.Microsecond)
}

func report(section string, rs []*result) {
	fmt.Printf("\n=== %s ===\n", section)
	fmt.Printf("%-38s %9s %9s %9s %9s %8s  %s\n",
		"phép đo", "ops/s", "trung bình", "p50", "p99", "MB/s", "ghi chú")
	for _, r := range rs {
		fmt.Printf("%-38s %9.0f %9s %9s %9s %8.1f  %s\n",
			r.name, r.opsPerSec(), rnd(r.mean()), rnd(r.pct(50)), rnd(r.pct(99)),
			r.mbPerSec(), r.note)
	}
}

// ---------------------------------------------------------------------------
// 1. write() có phải durable không? Giá của fsync.
// ---------------------------------------------------------------------------

func expWriteVsFsync(dir string) []result {
	const sec = "1. write() KHÔNG phải durability"
	buf := alignedBuf(pageSize)
	var out []result

	// (a) write thuần: dữ liệu mới chỉ nằm trong page cache của OS.
	//     Mất điện lúc này = mất sạch. Đây KHÔNG phải durability.
	f := create(filepath.Join(dir, "w.dat"))
	n := 20000
	noFsyncNote := "chỉ tới page cache — CHƯA durable"
	if *flagDirect {
		// O_DIRECT bỏ qua page cache nên write chậm hơn ~100x -> giảm số mẫu.
		// Lưu ý: O_DIRECT vẫn KHÔNG phải durable — dữ liệu có thể còn nằm trong
		// bộ nhớ đệm (write cache) của chính ổ đĩa. Chỉ fsync mới ép flush cache đó.
		n = 2000
		noFsyncNote = "O_DIRECT: qua mặt page cache, VẪN chưa durable (write cache của ổ)"
	}
	s, tot := sample(n, pageSize, func(int) { _, err := f.Write(buf); must(err) })
	out = append(out, result{sec, "write 4KB (không fsync)", noFsyncNote, pageSize, s, tot})
	must(f.Close())

	// (b) write + fsync mỗi lần = một transaction commit thật.
	f = create(filepath.Join(dir, "wf.dat"))
	s, tot = sample(300, pageSize, func(int) {
		_, err := f.Write(buf)
		must(err)
		must(f.Sync())
	})
	out = append(out, result{sec, "write 4KB + fsync mỗi lần", "= trần commit/s của 1 luồng", pageSize, s, tot})
	must(f.Close())

	// (c) fsync trên file không đổi: overhead cố định của lệnh fsync.
	f = create(filepath.Join(dir, "nop.dat"))
	_, err := f.Write(buf)
	must(err)
	must(f.Sync())
	s, tot = sample(300, pageSize, func(int) { must(f.Sync()) })
	out = append(out, result{sec, "fsync khi không có gì bẩn", "overhead nền của syscall", pageSize, s, tot})
	must(f.Close())

	// (d) tạo file mới + fsync file + fsync thư mục.
	//     Không fsync directory -> file có thể biến mất khỏi thư mục sau crash dù
	//     nội dung đã trên đĩa. Bẫy kinh điển khi tạo WAL segment.
	s, tot = sample(100, pageSize, func(i int) {
		p := filepath.Join(dir, fmt.Sprintf("seg%03d.tmp", i))
		nf := create(p)
		_, err := nf.Write(buf)
		must(err)
		must(nf.Sync())
		must(nf.Close())
		df, err := os.Open(dir)
		must(err)
		must(df.Sync())
		must(df.Close())
		must(os.Remove(p))
	})
	out = append(out, result{sec, "tạo file + fsync file + fsync dir", "giá tạo 1 WAL segment mới", pageSize, s, tot})

	return out
}

// ---------------------------------------------------------------------------
// 2. Group commit: N transaction chia nhau MỘT lần fsync.
// ---------------------------------------------------------------------------

func expGroupCommit(dir string) []result {
	const sec = "2. Group commit: chia nhau một lần fsync"
	buf := alignedBuf(pageSize)
	var out []result

	for _, group := range []int{1, 4, 16, 64, 256} {
		f := create(filepath.Join(dir, fmt.Sprintf("g%d.dat", group)))
		batches := 200 / group
		if batches < 3 {
			batches = 3
		}
		// Mẫu ở đây là ĐỘ TRỄ MỖI TXN: mỗi txn ghi xong phải đợi fsync của cả nhóm.
		// Nhờ vậy p99 cho thấy group commit có làm đuôi trễ phình ra hay không.
		samples := make([]time.Duration, 0, batches*group)
		start := time.Now()
		for b := 0; b < batches; b++ {
			t0 := time.Now()
			for i := 0; i < group; i++ {
				_, err := f.Write(buf)
				must(err)
			}
			must(f.Sync()) // một fsync cho cả nhóm
			d := time.Since(t0)
			for i := 0; i < group; i++ {
				samples = append(samples, d) // mỗi txn trong nhóm chịu cùng độ trễ
			}
		}
		out = append(out, result{sec,
			fmt.Sprintf("group commit, %d txn / 1 fsync", group),
			"txn/s hiệu dụng; p99 = độ trễ txn", pageSize, samples, time.Since(start)})
		must(f.Close())
	}
	return out
}

// ---------------------------------------------------------------------------
// 3 & 4. Random vs sequential, và page cache hit vs miss.
// ---------------------------------------------------------------------------

func expRandomVsSequential(dir string, fileMB int) []result {
	const sec = "3+4. Random vs tuần tự, cache lạnh vs nóng"
	path := filepath.Join(dir, "big.dat")
	f := create(path)
	size := int64(fileMB) << 20
	pages := size / pageSize
	buf := alignedBuf(pageSize)

	for i := int64(0); i < pages; i++ {
		_, err := f.Write(buf)
		must(err)
	}
	must(f.Sync())

	rng := rand.New(rand.NewSource(42))
	n := *flagOps
	var out []result

	// --- GHI ---
	// Không O_DIRECT thì cả hai kiểu ghi chỉ làm bẩn page cache -> per-op gần như
	// bằng nhau. Khác biệt thật nằm ở lần fsync CUỐI: writeback dirty page rải rác
	// đắt hơn nhiều dirty page liền kề. Nên phải đo fsync bằng đồng hồ riêng.
	s, tot := sample(n, pageSize, func(i int) {
		_, err := f.WriteAt(buf, int64(i%int(pages))*pageSize)
		must(err)
	})
	out = append(out, result{sec, "pwrite 4KB tuần tự", "", pageSize, s, tot})
	t0 := time.Now()
	must(f.Sync())
	out = append(out, result{sec, "  -> fsync sau ghi tuần tự",
		"writeback page liền kề", pageSize * int64(n),
		[]time.Duration{time.Since(t0)}, time.Since(t0)})

	s, tot = sample(n, pageSize, func(int) {
		_, err := f.WriteAt(buf, rng.Int63n(pages)*pageSize)
		must(err)
	})
	out = append(out, result{sec, "pwrite 4KB ngẫu nhiên", "cùng số byte, vị trí rải rác", pageSize, s, tot})
	t0 = time.Now()
	must(f.Sync())
	out = append(out, result{sec, "  -> fsync sau ghi ngẫu nhiên",
		"<- GIÁ THẬT CỦA RANDOM WRITE", pageSize * int64(n),
		[]time.Duration{time.Since(t0)}, time.Since(t0)})

	// --- ĐỌC, cache lạnh (I/O thật) ---
	dropCache(f, size)
	s, tot = sample(n, pageSize, func(i int) {
		_, err := f.ReadAt(buf, int64(i)*pageSize)
		must(err)
	})
	out = append(out, result{sec, "pread 4KB tuần tự, CACHE LẠNH", "kernel readahead giúp sức", pageSize, s, tot})

	// Ghi lại đúng dãy offset để lần đọc "nóng" chạm ĐÚNG những page vừa nạp.
	// Bốc offset ngẫu nhiên mới trên file lớn -> working set quá thưa -> "nóng"
	// hoá ra vẫn là miss. (Đã dính bẫy này ở lần đo đầu tiên, xem diary/phase0.md.)
	offsets := make([]int64, n)
	for i := range offsets {
		offsets[i] = rng.Int63n(pages) * pageSize
	}

	dropCache(f, size)
	s, tot = sample(n, pageSize, func(i int) {
		_, err := f.ReadAt(buf, offsets[i])
		must(err)
	})
	out = append(out, result{sec, "pread 4KB ngẫu nhiên, CACHE LẠNH", "<- CHI PHÍ 1 LẦN CHẠM NODE B+TREE", pageSize, s, tot})

	// --- ĐỌC, cache nóng: cùng dãy offset, không drop cache = buffer-pool hit ---
	s, tot = sample(n, pageSize, func(i int) {
		_, err := f.ReadAt(buf, offsets[i])
		must(err)
	})
	note := "<- cùng offset, giá trị của buffer pool"
	if *flagDirect {
		note = "(O_DIRECT: không có cache nên KHÔNG nóng)"
	}
	out = append(out, result{sec, "pread 4KB ngẫu nhiên, CACHE NÓNG", note, pageSize, s, tot})

	must(f.Close())
	must(os.Remove(path))
	return out
}

// ---------------------------------------------------------------------------

type jsonResult struct {
	Section string  `json:"section"`
	Name    string  `json:"name"`
	N       int     `json:"n"`
	OpsPerS float64 `json:"ops_per_sec"`
	MeanNs  int64   `json:"mean_ns"`
	P50Ns   int64   `json:"p50_ns"`
	P99Ns   int64   `json:"p99_ns"`
	MBPerS  float64 `json:"mb_per_sec"`
}

type jsonReport struct {
	Host    string       `json:"host"`
	Kernel  string       `json:"kernel"`
	Go      string       `json:"go"`
	Date    string       `json:"date"`
	FileMB  int          `json:"file_mb"`
	Repeat  int          `json:"repeat"`
	Direct  bool         `json:"o_direct"`
	Results []jsonResult `json:"results"`
}

func kernelRelease() string {
	b, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return "unknown"
	}
	return string(b[:len(b)-1])
}

func main() {
	flag.Parse()
	must(os.MkdirAll(*flagDir, 0o755))
	defer os.RemoveAll(*flagDir)

	host, _ := os.Hostname()
	fmt.Printf("iolab — Phase 0\nmáy: %s | kernel: %s | %s\nthư mục: %s | page %dB | file %dMB | repeat %d | O_DIRECT %v\n",
		host, kernelRelease(), runtimeVersion(), *flagDir, pageSize, *flagFileMB, *flagRepeat, *flagDirect)
	if *flagDirect {
		fmt.Println("LƯU Ý: O_DIRECT bỏ qua page cache -> phép đo 'CACHE NÓNG' sẽ không nóng. Đó là chủ ý.")
	}

	order := []string{}
	agg := map[string]*result{}
	for r := 0; r < *flagRepeat; r++ {
		var all []result
		all = append(all, expWriteVsFsync(*flagDir)...)
		all = append(all, expGroupCommit(*flagDir)...)
		all = append(all, expRandomVsSequential(*flagDir, *flagFileMB)...)
		for _, res := range all {
			k := res.section + "\x00" + res.name
			if cur, ok := agg[k]; ok {
				cur.merge(res)
			} else {
				cp := res
				agg[k] = &cp
				order = append(order, k)
			}
		}
	}

	var jr jsonReport
	jr.Host, jr.Kernel, jr.Go = host, kernelRelease(), runtimeVersion()
	jr.Date = time.Now().Format(time.RFC3339)
	jr.FileMB, jr.Repeat, jr.Direct = *flagFileMB, *flagRepeat, *flagDirect

	curSection := ""
	var batch []*result
	flush := func() {
		if len(batch) > 0 {
			report(curSection, batch)
			batch = nil
		}
	}
	for _, k := range order {
		r := agg[k]
		if r.section != curSection {
			flush()
			curSection = r.section
		}
		batch = append(batch, r)
		jr.Results = append(jr.Results, jsonResult{r.section, r.name, len(r.samples),
			r.opsPerSec(), r.mean().Nanoseconds(), r.pct(50).Nanoseconds(),
			r.pct(99).Nanoseconds(), r.mbPerSec()})
	}
	flush()

	if *flagJSON != "" {
		must(os.MkdirAll(filepath.Dir(*flagJSON), 0o755))
		b, err := json.MarshalIndent(jr, "", "  ")
		must(err)
		must(os.WriteFile(*flagJSON, b, 0o644))
		fmt.Printf("\n[json] đã ghi %s\n", *flagJSON)
	}

	fmt.Println(`
Đọc kết quả:
  - (write không fsync) / (write + fsync) = cái giá bạn trả cho chữ D trong ACID.
  - Group commit: ops/s tăng gần tuyến tính theo cỡ nhóm cho tới khi chạm băng thông đĩa.
    Xem cột p99: nếu p99 không phình theo cỡ nhóm thì gộp commit gần như miễn phí.
  - (pread ngẫu nhiên cache lạnh) = chi phí MỘT lần chạm node B+Tree.
    Nhân với chiều cao cây = độ trễ một lookup. Fanout lớn -> cây thấp -> ít lần nhân.
  - (cache nóng)/(cache lạnh) là lý do buffer pool tồn tại -> phase 3.
  - p99/p50 lớn = đĩa hoặc filesystem nhiễu; số trung bình lúc đó không đáng tin.`)
}

func runtimeVersion() string { return runtime.Version() }
