package main

// Bảng 8 — nợ P9-7: vì sao hash join 16 batch (tràn ra file tạm) lại nhanh hơn
// 1 batch (cả bảng băm 21.6MB nằm trong RAM) trên Postgres?
//
// Hai giả thuyết, viết ra TRƯỚC khi đo, mỗi cái một dự báo phân biệt được:
//
//	H1 (L3 cache, 12MB trên máy đo): mỗi lần probe vào bảng băm lớn là một
//	   lần trượt cache. ⇒ khoảng chênh (1 batch − nhiều batch) TĂNG theo số
//	   hàng probe; tắt page fault (H2) cũng không xoá được nó.
//	H2 (cấp phát bộ nhớ mới mỗi câu): bảng băm được malloc mới rồi trả lại
//	   cho OS sau mỗi câu, nên lần chạm đầu vào mỗi page 4KB là một page
//	   fault. ⇒ khoảng chênh đi cùng số minor page fault, KHÔNG phụ thuộc số
//	   hàng probe, và biến mất khi malloc giữ bộ nhớ lại (container pgm).
//
// perf không dùng được trên máy đo đầu (WSL2 không đưa PMU vào VM: không có
// /sys/bus/event_source/devices/cpu), nên không đếm được cache miss trực
// tiếp. Page fault thì đếm được: trường minflt trong /proc/<pid>/stat.
// Phép E chỉ chạy khi có PMU: trên Linux thuần, scripts/p97-hashjoin.sh.
//
// Ba phép đo, mỗi phép vặn đúng một núm:
//
//	A. work_mem 1MB → 256MB, cùng một câu: số batch đổi, dữ liệu không đổi.
//	B. số hàng probe 1M → 4M (bảng hjp nhân bản hj 4 lần, lọc a.r <= k),
//	   bảng băm cố định 500k hàng. Phía probe luôn LỚN hơn phía build, để
//	   planner không đổi phía băm (lượt đầu lọc a.id <= P thì planner đổi).
//	C. cả A và B, chạy trên pg (malloc mặc định) và pgm (malloc giữ bộ nhớ).

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type hjConn struct {
	c       *sql.Conn
	name    string
	hostPID int
}

func workHashJoin(es []*Engine, repeat int) error {
	defer memLatency()
	var hs []*hjConn
	for _, e := range es {
		if e.Kind != "pg" {
			continue
		}
		h, err := hashJoinSetup(e)
		if err != nil {
			return fmt.Errorf("%s: %w", e.Name, err)
		}
		defer h.c.Close()
		hs = append(hs, h)
	}
	ctx := context.Background()
	fmt.Printf("\n== 8. hash join: vì sao tràn đĩa lại nhanh hơn (Postgres) ==\n")

	for _, h := range hs {
		fmt.Printf("\nA. %s: build 500k hàng, probe 1M hàng, chỉ đổi work_mem (trung vị %d lần)\n", h.name, repeat)
		fmt.Printf("%10s%8s%10s%11s%10s%12s\n", "work_mem", "batch", "bucket", "bộ nhớ", "ms", "page fault")
		for _, wm := range []string{"1MB", "2MB", "4MB", "8MB", "16MB", "32MB", "256MB"} {
			p, err := h.plan(ctx, wm, 1)
			if err != nil {
				return err
			}
			var ts []time.Duration
			var fs []int64
			for i := 0; i <= repeat; i++ {
				d, f, err := h.once(ctx, wm, 1)
				if err != nil {
					return err
				}
				if i > 0 {
					ts, fs = append(ts, d), append(fs, f)
				}
			}
			fmt.Printf("%10s%8d%10d%9dkB%10s%12d\n", wm, p.batches, p.buckets, p.memKB, ms(medianOf(ts)), medianOf(fs))
		}
	}

	// Số đo đơn lẻ trên máy này lệch tới ±30% giữa hai lượt (i5-1235U có 2
	// nhân P và 8 nhân E, WSL2 không cho ghim nhân). Nên B và C đo THEO CẶP:
	// hai cấu hình chạy xen kẽ, sát nhau, và lấy trung vị của hiệu từng cặp.
	// Hai số trong một cặp chịu cùng một điều kiện máy; nhiễu chung bị trừ đi.
	for _, h := range hs {
		fmt.Printf("\nB. %s: build 500k hàng cố định, đổi số hàng probe; cặp (1MB, 256MB) × %d\n", h.name, repeat)
		fmt.Printf("%8s%10s%11s%12s%12s%13s%13s\n", "probe", "phía băm", "1MB ms", "256MB ms", "chênh ms", "ns/probe", "fault 256MB")
		for k := 1; k <= 4; k++ {
			p, err := h.plan(ctx, "256MB", k)
			if err != nil {
				return err
			}
			a, b, diff, fb, err := pair(ctx, repeat, func() (time.Duration, int64, error) { return h.once(ctx, "1MB", k) },
				func() (time.Duration, int64, error) { return h.once(ctx, "256MB", k) })
			if err != nil {
				return err
			}
			fmt.Printf("%7dM%10s%11s%12s%12s%13.1f%13d\n", k, p.hashed, ms(a), ms(b), ms(diff),
				float64(diff.Nanoseconds())/float64(k*1_000_000), fb)
		}
	}

	if len(hs) == 2 {
		fmt.Printf("\nC. cùng câu, cùng work_mem, chỉ khác malloc: cặp (%s, %s) × %d, probe 1M\n", hs[0].name, hs[1].name, repeat)
		fmt.Printf("%10s%11s%11s%12s%11s%11s\n", "work_mem", hs[0].name+" ms", hs[1].name+" ms", "chênh ms", "fault "+hs[0].name, "fault "+hs[1].name)
		for _, wm := range []string{"1MB", "256MB"} {
			var fa int64
			a, b, diff, fb, err := pair(ctx, repeat,
				func() (time.Duration, int64, error) {
					d, f, err := hs[0].once(ctx, wm, 1)
					fa = f
					return d, f, err
				},
				func() (time.Duration, int64, error) { return hs[1].once(ctx, wm, 1) })
			if err != nil {
				return err
			}
			fmt.Printf("%10s%11s%11s%12s%11d%11d\n", wm, ms(a), ms(b), ms(diff), fa, fb)
		}
	}

	if !hasPMU() {
		fmt.Printf("\nE. bỏ qua: máy này không có PMU (WSL2, VM không bật vPMU) — không đếm được cache miss.\n")
		fmt.Printf("   Chạy trên Linux thuần: scripts/p97-hashjoin.sh\n")
		return nil
	}
	// H1 đúng thì bản 1 batch phải trượt cache / TLB NHIỀU HƠN hẳn trên mỗi
	// hàng probe; H1 sai thì hai cột gần bằng nhau và khoảng chênh ở đâu đó
	// khác (số lệnh? nhánh?). cycles và instructions đi kèm để thấy IPC.
	for _, h := range hs {
		fmt.Printf("\nE. %s: perf stat trên backend, %d câu mỗi cấu hình, probe 1M; đơn vị: sự kiện / hàng probe\n", h.name, repeat)
		var cols []map[string]float64
		var names []string
		for _, wm := range []string{"1MB", "256MB"} {
			if _, _, err := h.once(ctx, wm, 1); err != nil { // làm nóng
				return err
			}
			c, n, err := h.perfAround(func() error {
				for i := 0; i < repeat; i++ {
					if _, _, err := h.once(ctx, wm, 1); err != nil {
						return err
					}
				}
				return nil
			})
			if err != nil {
				return err
			}
			cols, names = append(cols, c), n
		}
		fmt.Printf("%-36s%14s%14s%10s\n", "sự kiện", "1MB", "256MB", "256/1")
		probes := float64(repeat) * 1_000_000
		for _, n := range names {
			a, b := cols[0][n], cols[1][n]
			ratio := "-"
			if a > 0 {
				ratio = fmt.Sprintf("%.2fx", b/a)
			}
			fmt.Printf("%-36s%14.3f%14.3f%10s\n", n, a/probes, b/probes, ratio)
		}
	}
	return nil
}

// hasPMU: có bộ đếm phần cứng cho perf không. CPU lai của Intel (nhân P +
// nhân E) có cpu_core và cpu_atom thay cho cpu.
func hasPMU() bool {
	for _, d := range []string{"cpu", "cpu_core", "cpu_atom"} {
		if _, err := os.Stat("/sys/bus/event_source/devices/" + d); err == nil {
			return true
		}
	}
	return false
}

// perfEvents: hai cái đầu để tính IPC, còn lại là thứ H1 dự báo.
// dTLB-load-misses vì mảng bucket 4MB vượt tầm phủ của L1 dTLB (64 mục × 4KB
// = 256KB) và cả L2 TLB (~2048 mục × 4KB = 8MB) khi tính cả phần tuple 21MB.
const perfEvents = "cycles,instructions,cache-references,cache-misses,LLC-load-misses,dTLB-load-misses,page-faults"

// perfAround gắn `perf stat -p` vào backend, chạy fn, rồi dừng perf bằng
// SIGINT. Biến PERF đổi lệnh gọi (scripts/p97-hashjoin.sh đặt "sudo -n perf":
// backend chạy dưới uid postgres của container, perf của user khác không gắn
// vào được nếu thiếu CAP_PERFMON / CAP_SYS_PTRACE).
func (h *hjConn) perfAround(fn func() error) (map[string]float64, []string, error) {
	out := filepath.Join(os.TempDir(), fmt.Sprintf("p97-perf-%d.csv", h.hostPID))
	defer os.Remove(out)
	argv := strings.Fields(os.Getenv("PERF"))
	if len(argv) == 0 {
		argv = []string{"perf"}
	}
	argv = append(argv, "stat", "-x,", "-e", perfEvents, "-p", strconv.Itoa(h.hostPID), "-o", out)
	cmd := osexec.Command(argv[0], argv[1:]...)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("perf: %w", err)
	}
	time.Sleep(300 * time.Millisecond) // cho perf kịp gắn vào trước câu đầu
	ferr := fn()
	// Dừng perf. Với "sudo perf", tín hiệu phải tới perf chứ không chỉ sudo:
	// sudo chuyển tiếp SIGINT cho tiến trình con.
	cmd.Process.Signal(os.Interrupt)
	cmd.Wait()
	if ferr != nil {
		return nil, nil, ferr
	}
	b, err := os.ReadFile(out)
	if err != nil {
		return nil, nil, fmt.Errorf("perf không ghi ra %s: %w", out, err)
	}
	// Mỗi dòng CSV: giá trị,đơn vị,tên sự kiện,… — giá trị "<not supported>"
	// hoặc "<not counted>" thì bỏ. CPU lai in mỗi sự kiện hai dòng
	// (cpu_core/…/ và cpu_atom/…/): giữ nguyên cả hai tên.
	m := map[string]float64{}
	var names []string
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Split(l, ",")
		if len(f) < 3 || strings.HasPrefix(l, "#") {
			continue
		}
		v, err := strconv.ParseFloat(f[0], 64)
		if err != nil {
			continue
		}
		if _, ok := m[f[2]]; !ok {
			names = append(names, f[2])
		}
		m[f[2]] += v
	}
	if len(names) == 0 {
		return nil, nil, fmt.Errorf("perf không đếm được sự kiện nào; nội dung:\n%s", b)
	}
	return m, names, nil
}

// pair chạy x, y xen kẽ repeat lần (cộng một cặp làm nóng), trả trung vị của
// x, của y, của (x − y) theo từng cặp, và trung vị page fault của y.
func pair(ctx context.Context, repeat int, x, y func() (time.Duration, int64, error)) (time.Duration, time.Duration, time.Duration, int64, error) {
	var xs, ys, ds []time.Duration
	var fs []int64
	for i := 0; i <= repeat; i++ {
		a, _, err := x()
		if err != nil {
			return 0, 0, 0, 0, err
		}
		b, f, err := y()
		if err != nil {
			return 0, 0, 0, 0, err
		}
		if i > 0 {
			xs, ys, ds, fs = append(xs, a), append(ys, b), append(ds, b-a), append(fs, f)
		}
	}
	return medianOf(xs), medianOf(ys), medianOf(ds), medianOf(fs), nil
}

func medianOf[T int64 | time.Duration](v []T) T {
	sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
	return v[len(v)/2]
}

func hashJoinSetup(e *Engine) (*hjConn, error) {
	ctx := context.Background()
	var n int
	if err := e.DB.QueryRowContext(ctx, `SELECT count(*) FROM pg_class WHERE relname = 'hjp'`).Scan(&n); err != nil {
		return nil, err
	}
	if n == 0 {
		fmt.Printf("%s: nạp hj (1 triệu hàng, cùng dữ liệu với blog/lab/10-explain-pg.sql) và hjp (hj × 4) …\n", e.Name)
		if err := exec(ctx, e.DB,
			`DROP TABLE IF EXISTS hj, hjp`,
			`CREATE TABLE hj (id int PRIMARY KEY, kind int, city text, amount int)`,
			`SELECT setseed(0.5)`,
			`INSERT INTO hj SELECT g, (random()*999)::int, 'city ' || (random()*9999)::int, (random()*1000)::int FROM generate_series(1, 1000000) g`,
			`CREATE TABLE hjp AS SELECT r, hj.* FROM hj, generate_series(1, 4) r ORDER BY r, id`,
			`VACUUM ANALYZE hj`, `VACUUM ANALYZE hjp`); err != nil {
			return nil, err
		}
	}
	c, err := e.DB.Conn(ctx)
	if err != nil {
		return nil, err
	}
	var pid int
	if err := c.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		return nil, err
	}
	h := &hjConn{c: c, name: e.Name}
	if h.hostPID, err = hostPIDOf(containers[e.Name], pid); err != nil {
		return nil, err
	}
	if err := exec(ctx, c, `SET enable_mergejoin = off`, `SET enable_nestloop = off`,
		`SET max_parallel_workers_per_gather = 0`); err != nil {
		return nil, err
	}
	// Đọc cả hai bảng một lượt cho nóng shared_buffers.
	if _, err := c.ExecContext(ctx, `SELECT sum(amount) FROM hj UNION ALL SELECT sum(amount) FROM hjp`); err != nil {
		return nil, err
	}
	return h, nil
}

type hjResult struct {
	batches, buckets, memKB int
	hashed                  string // bí danh của bảng nằm dưới nút Hash: phải luôn là "b"
}

func hjQuery(k int) string {
	return fmt.Sprintf(`SELECT count(*) FROM hjp a JOIN hj b ON a.id = b.id WHERE b.amount < 500 AND a.r <= %d`, k)
}

// plan chạy EXPLAIN ANALYZE một lần để lấy số batch / bucket / bộ nhớ và
// phía băm. Không dùng nó để đo giờ: đo giờ từng nút tự nó làm chậm câu.
func (h *hjConn) plan(ctx context.Context, workMem string, k int) (hjResult, error) {
	var r hjResult
	if _, err := h.c.ExecContext(ctx, `SET work_mem = '`+workMem+`'`); err != nil {
		return r, err
	}
	rows, err := h.c.QueryContext(ctx, `EXPLAIN (ANALYZE, TIMING OFF, COSTS OFF) `+hjQuery(k))
	if err != nil {
		return r, err
	}
	defer rows.Close()
	afterHash := false
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return r, err
		}
		if i := strings.Index(line, "Buckets:"); i >= 0 {
			// "Buckets: 65536 (originally 16384)  Batches: 32 (originally 16)  Memory Usage: …"
			f := strings.Fields(line[i:])
			for j := 0; j+1 < len(f); j++ {
				switch f[j] {
				case "Buckets:":
					r.buckets, _ = strconv.Atoi(f[j+1])
				case "Batches:":
					r.batches, _ = strconv.Atoi(f[j+1])
				case "Usage:":
					r.memKB, _ = strconv.Atoi(strings.TrimSuffix(f[j+1], "kB"))
				}
			}
			afterHash = true
		} else if afterHash && strings.Contains(line, "Seq Scan on") {
			f := strings.Fields(line[strings.Index(line, "Seq Scan on"):])
			r.hashed = f[4] // "Seq Scan on hj b" → "b"
			afterHash = false
		}
	}
	return r, rows.Err()
}

// once chạy câu một lần: thời gian (chỉ bấm giờ quanh câu truy vấn) và số
// minor page fault của backend trong lúc chạy.
func (h *hjConn) once(ctx context.Context, workMem string, k int) (time.Duration, int64, error) {
	if _, err := h.c.ExecContext(ctx, `SET work_mem = '`+workMem+`'`); err != nil {
		return 0, 0, err
	}
	f0, err := h.minflt()
	if err != nil {
		return 0, 0, err
	}
	var n int64
	t0 := time.Now()
	if err := h.c.QueryRowContext(ctx, hjQuery(k)).Scan(&n); err != nil {
		return 0, 0, err
	}
	d := time.Since(t0)
	f1, err := h.minflt()
	return d, f1 - f0, err
}

// minflt đọc số minor page fault của backend: trường thứ 10 của
// /proc/<pid>/stat, đọc thẳng từ host. Lượt đầu đọc qua `docker exec … cat`
// trước và sau MỖI câu: mỗi lần là một lần khởi động runc, chạy ngay trước
// câu đang đo, và số đo nhiễu tới mức một dòng ra khoảng chênh âm.
func (h *hjConn) minflt() (int64, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", h.hostPID))
	if err != nil {
		return 0, err
	}
	// Trường 2 là "(tên)", có thể chứa dấu cách: cắt sau dấu ')' cuối.
	s := string(b)
	f := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
	return strconv.ParseInt(f[7], 10, 64) // trường 10 = f[7] sau khi bỏ pid và (tên)
}

// hostPIDOf đổi pid trong container (pg_backend_pid()) thành pid trên host:
// dòng NSpid trong /proc/<pid>/status liệt kê pid ở mọi namespace, pid của
// namespace trong cùng nằm cuối. Chỉ xét các tiến trình của đúng container
// đó (docker top), vì hai container Postgres có thể trùng pid bên trong.
func hostPIDOf(container string, pid int) (int, error) {
	out, err := osexec.Command("docker", "top", container, "-o", "pid").Output()
	if err != nil {
		return 0, fmt.Errorf("docker top %s: %w", container, err)
	}
	for _, l := range strings.Split(string(out), "\n")[1:] {
		hp, err := strconv.Atoi(strings.TrimSpace(l))
		if err != nil {
			continue
		}
		st, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", hp))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(st), "\n") {
			if f := strings.Fields(line); len(f) > 1 && f[0] == "NSpid:" && f[len(f)-1] == strconv.Itoa(pid) {
				return hp, nil
			}
		}
	}
	return 0, fmt.Errorf("không tìm thấy pid %d của %s trên host", pid, container)
}

// memLatency đo trên chính máy này: một lần đọc ngẫu nhiên vào mảng kích
// thước n tốn bao nhiêu ns, như một lần probe vào mảng bucket của bảng băm.
// Các lần đọc ĐỘC LẬP nhau (chỉ số đã tính sẵn), giống các lần probe: CPU
// được phép chồng nhiều lần trượt cache lên nhau, nên con số là thông lượng,
// không phải độ trễ của một lần trượt đơn lẻ.
var memSink uint64

func memLatency() {
	fmt.Printf("\nD. đọc ngẫu nhiên, độc lập, vào mảng uint64 (như probe vào mảng bucket)\n")
	fmt.Printf("%10s%14s%14s\n", "mảng", "độc lập ns", "dây chuyền ns")
	const reads = 1 << 24
	idx := make([]uint32, reads)
	for _, kb := range []int{64, 256, 512, 1024, 2048, 4096, 8192, 16384, 32768, 65536} {
		n := kb * 1024 / 8
		a := make([]uint64, n)
		for i := range a {
			a[i] = uint64(i)
		}
		x := uint32(2463534242)
		for i := range idx {
			x ^= x << 13
			x ^= x >> 17
			x ^= x << 5
			idx[i] = x % uint32(n)
		}
		var best time.Duration
		for r := 0; r < 5; r++ {
			t := time.Now()
			var s uint64
			for _, j := range idx {
				s += a[j]
			}
			memSink += s // không dùng kết quả thì compiler bỏ luôn vòng lặp: lượt đầu ra 0.47ns ở mọi cỡ
			if d := time.Since(t); r == 0 || d < best {
				best = d
			}
		}
		// Dây chuyền: a[j] là chỉ số của lần đọc kế tiếp, nên lần đọc sau PHẢI
		// chờ lần trước xong. Đó là độ trễ thật của một lần trượt cache, và
		// gần với probe của Postgres hơn: giữa hai lần probe là hàng nghìn
		// lệnh của executor, CPU không kịp chồng hai lần trượt lên nhau.
		// Dựng một vòng Sattolo (một chu trình duy nhất đi qua mọi ô).
		for i := range a {
			a[i] = uint64(i)
		}
		for i := n - 1; i > 0; i-- {
			x ^= x << 13
			x ^= x >> 17
			x ^= x << 5
			j := int(x % uint32(i))
			a[i], a[j] = a[j], a[i]
		}
		var chase time.Duration
		for r := 0; r < 3; r++ {
			t := time.Now()
			p := uint64(0)
			for i := 0; i < reads/4; i++ {
				p = a[p]
			}
			memSink += p
			if d := time.Since(t); r == 0 || d < chase {
				chase = d
			}
		}
		fmt.Printf("%8dKB%14.2f%14.2f\n", kb, float64(best.Nanoseconds())/reads, float64(chase.Nanoseconds())/(reads/4))
	}
}
