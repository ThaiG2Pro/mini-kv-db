// crashlab — bài kiểm tra quyết định của phase 5: giết tiến trình bằng kill -9
// ở một thời điểm ngẫu nhiên, mở lại, và đòi hỏi chữ D trong ACID.
//
// Ba mệnh đề phải đúng ở MỌI lần, không phải "hầu hết":
//
//	D1  mọi transaction đã báo commit thì còn nguyên, đúng từng byte;
//	D2  mọi transaction chưa commit biến mất SẠCH — không để lại nửa vời;
//	D3  cây vẫn hợp lệ (bảy bất biến của phase 4).
//
// Vì sao phải là tiến trình con và kill -9, chứ không phải một hàm
// SimulateCrash trong test: kill -9 không chạy defer, không chạy destructor,
// không flush buffer của thư viện, và không cho code cơ hội "dọn dẹp cho đẹp".
// Nó cũng là cách duy nhất chứng minh mình không vô tình dựa vào Close().
//
// Cái mà kill -9 KHÔNG mô phỏng được: torn write. Kernel vẫn writeback đầy đủ
// những gì đã write(). Muốn torn write thật phải bơm lỗi ở tầng thiết bị —
// xem nợ P0-1 và scripts/dm-flakey.sh.
package main

import (
	"bufio"
	"encoding/binary"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"minidb/internal/db"
)

// ---------- workload sinh ra từ số thứ tự transaction ----------
//
// Mọi thứ suy ra được từ (seed, số thứ tự txn). Nhờ vậy bên kiểm tra không cần
// tiến trình con kể lại nó đã làm gì — nó chỉ cần biết txn cuối cùng nào đã
// commit. Đây là điều kiện để bài test còn đúng khi tiến trình bị giết giữa
// chừng: một tiến trình chết không kịp kể gì cả.

func hash(a ...uint64) uint64 {
	h := uint64(1469598103934665603)
	for _, v := range a {
		for i := 0; i < 8; i++ {
			h ^= (v >> (8 * i)) & 0xff
			h *= 1099511628211
		}
	}
	return h
}

// nKeys: phần lớn transaction nhỏ, nhưng cứ 13 cái lại có một cái BÉO.
//
// Không có mấy cái béo thì bài test gần như không bao giờ chạm tới pha undo:
// một transaction nhỏ chết đi cùng toàn bộ log của nó trong RAM (chưa fsync),
// nên sau khi mở lại chẳng có gì để quay ngược — "chưa commit thì biến mất"
// đúng một cách tầm thường. Transaction béo ép buffer pool phải đuổi page bẩn,
// WAL rule kéo log của nó xuống đĩa theo, và LÚC ĐÓ recovery mới thật sự phải
// undo. Đo được: pool 16 frame + txn béo cho loser>0 ở phần lớn các vòng, còn
// pool 64 frame + txn nhỏ cho loser=0 ở TẤT CẢ các vòng.
func nKeys(seed uint64, txn int) int {
	if txn%13 == 5 {
		return 400
	}
	return 3 + int(hash(seed, uint64(txn), 1)%6)
}

func mkKey(seed uint64, txn, j int, poison bool) []byte {
	b := make([]byte, 20)
	if poison {
		copy(b, "P")
	} else {
		copy(b, "K")
	}
	binary.BigEndian.PutUint64(b[4:], hash(seed, uint64(txn), uint64(j), 7))
	binary.BigEndian.PutUint32(b[12:], uint32(txn))
	binary.BigEndian.PutUint32(b[16:], uint32(j))
	return b
}

func mkVal(seed uint64, txn, j int) []byte {
	n := 30 + int(hash(seed, uint64(txn), uint64(j), 3)%420)
	v := make([]byte, n)
	h := hash(seed, uint64(txn), uint64(j), 5)
	for i := range v {
		v[i] = byte(h >> uint(8*(i%8)))
		if i%8 == 7 {
			h = hash(h)
		}
	}
	return v
}

// isAbortTxn: cứ 7 transaction thì một cái cố tình bị hủy. Khóa của nó mang
// tiền tố P và không bao giờ được phép xuất hiện — đó là vế D2.
func isAbortTxn(txn int) bool { return txn%7 == 3 }

// deleteTarget: txn i xóa khóa của txn i-4, để workload có cả merge/redistribute
// chứ không chỉ có chèn.
func deleteTarget(txn int) int {
	if txn > 4 {
		return txn - 4
	}
	return 0
}

// expected dựng tập khóa phải có sau khi các txn 1..committed đã commit.
func expected(seed uint64, committed int) map[string]string {
	m := map[string]string{}
	for i := 1; i <= committed; i++ {
		if isAbortTxn(i) {
			continue
		}
		if d := deleteTarget(i); d > 0 && !isAbortTxn(d) {
			for j := 0; j < nKeys(seed, d); j++ {
				delete(m, string(mkKey(seed, d, j, false)))
			}
		}
		for j := 0; j < nKeys(seed, i); j++ {
			m[string(mkKey(seed, i, j, false))] = string(mkVal(seed, i, j))
		}
	}
	return m
}

// ---------- tiến trình con ----------

func child(dir string, seed uint64, nosync, nowrite bool, frames, maxTxn int) error {
	// Vòng chạy thật thì cha đã tạo thư mục rồi, nhưng child phải gọi được
	// một mình (make wallab gọi thẳng nó) nên tự lo lấy chỗ làm việc.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	d, err := db.Open(filepath.Join(dir, "data.db"), db.Options{
		Frames:          frames,
		NoSync:          nosync || nowrite,
		NoWrite:         nowrite,
		CheckpointBytes: 4 << 20,
	})
	if err != nil {
		return err
	}
	prog, err := os.OpenFile(filepath.Join(dir, "committed"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}

	for txn := 1; maxTxn == 0 || txn <= maxTxn; txn++ {
		tx, err := d.Begin()
		if err != nil {
			return err
		}
		if isAbortTxn(txn) {
			for j := 0; j < nKeys(seed, txn); j++ {
				if err := tx.Put(mkKey(seed, txn, j, true), mkVal(seed, txn, j)); err != nil {
					return err
				}
			}
			if err := tx.Abort(); err != nil {
				return err
			}
			// Transaction bị hủy vẫn được ghi vào sổ: bên kiểm tra cần biết số
			// thứ tự đã đi tới đâu, và một txn hủy cũng là một mốc.
			if err := note(prog, txn, nosync); err != nil {
				return err
			}
			continue
		}
		if t := deleteTarget(txn); t > 0 && !isAbortTxn(t) {
			for j := 0; j < nKeys(seed, t); j++ {
				if err := tx.Delete(mkKey(seed, t, j, false)); err != nil {
					// Khóa có thể đã không còn (txn xóa nó chạy trước); bỏ qua.
					_ = err
				}
			}
		}
		for j := 0; j < nKeys(seed, txn); j++ {
			if err := tx.Put(mkKey(seed, txn, j, false), mkVal(seed, txn, j)); err != nil {
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		// Ghi sổ SAU khi Commit trả về. Từ giây phút Commit trả về, database
		// đã hứa; nếu bị giết trước khi ghi được sổ thì txn này thành "mập
		// mờ" và bên kiểm tra chấp nhận cả hai kết cục — nhưng phải là MỘT
		// trong hai, không được nửa vời.
		if err := note(prog, txn, nosync); err != nil {
			return err
		}
	}
	return d.Close()
}

func note(f *os.File, txn int, nosync bool) error {
	if _, err := f.WriteString(strconv.Itoa(txn) + "\n"); err != nil {
		return err
	}
	if nosync {
		return nil
	}
	return f.Sync()
}

// lastNoted đọc số cuối cùng ghi trọn một dòng. Dòng ghi dở (bị giết giữa
// chừng) bị bỏ qua — chính là lý do phải có ký tự xuống dòng.
func lastNoted(path string) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	last := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		n, err := strconv.Atoi(strings.TrimSpace(sc.Text()))
		if err != nil {
			break
		}
		last = n
	}
	return last
}

// ---------- kiểm tra ----------

type verdict struct {
	ok        bool
	reason    string
	committed int
	keys      int
	recovered string
}

// verify chạy trong một hàm có recover: Verify() của phase 4 PANIC khi gặp
// page hỏng nặng (nó được viết với giả định "page đọc lên luôn là page hợp
// lệ", điều chỉ đúng khi chưa có ai crash giữa chừng). Một panic ở đây là một
// vòng SAI, không phải lý do để bỏ dở 200 vòng.
func verify(dir string, seed uint64, frames int) (v verdict) {
	defer func() {
		if r := recover(); r != nil {
			v = verdict{reason: fmt.Sprintf("panic khi kiểm tra: %v", r),
				committed: lastNoted(filepath.Join(dir, "committed"))}
		}
	}()
	return verifyInner(dir, seed, frames)
}

func verifyInner(dir string, seed uint64, frames int) verdict {
	noted := lastNoted(filepath.Join(dir, "committed"))
	d, err := db.Open(filepath.Join(dir, "data.db"), db.Options{Frames: frames, CheckpointBytes: -1})
	if err != nil {
		return verdict{reason: "mở lại thất bại: " + err.Error(), committed: noted}
	}
	defer d.Close()

	rep, err := d.Tree().Verify()
	if err != nil {
		return verdict{reason: "Verify lỗi: " + err.Error(), committed: noted}
	}
	if !rep.OK() {
		return verdict{reason: fmt.Sprintf("cây hỏng: %v", rep.Errors), committed: noted}
	}

	got := map[string]string{}
	poison := 0
	if err := d.Tree().Range(nil, nil, func(k, v []byte) bool {
		if k[0] == 'P' {
			poison++
		}
		got[string(k)] = string(v)
		return true
	}); err != nil {
		return verdict{reason: "quét cây lỗi: " + err.Error(), committed: noted}
	}
	if poison > 0 {
		return verdict{reason: fmt.Sprintf("%d khóa của transaction ĐÃ HỦY vẫn còn", poison), committed: noted}
	}

	stat := fmt.Sprintf("redo %d/%d, undo %d, loser %d, mồ côi %d",
		d.RedoApplied, d.RedoApplied+d.RedoSkipped, d.UndoApplied, d.LoserTxns, d.OrphanPages)

	// Txn noted+1 là cái mập mờ: Commit có thể đã trả về ngay trước khi bị
	// giết, và lúc đó nó ĐƯỢC PHÉP còn. Chấp nhận đúng hai kết cục, không hơn.
	for _, c := range []int{noted, noted + 1} {
		want := expected(seed, c)
		if sameMap(want, got) {
			return verdict{ok: true, committed: c, keys: len(got), recovered: stat}
		}
	}
	want := expected(seed, noted)
	miss, extra := diffMaps(want, got)
	return verdict{
		reason: fmt.Sprintf("khớp cả với txn<=%d lẫn <=%d đều không: thiếu %d, thừa %d, có %d khóa (mong %d)",
			noted, noted+1, miss, extra, len(got), len(want)),
		committed: noted, keys: len(got), recovered: stat,
	}
}

func sameMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func diffMaps(want, got map[string]string) (miss, extra int) {
	for k, v := range want {
		if g, ok := got[k]; !ok || g != v {
			miss++
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			extra++
		}
	}
	return
}

// ---------- tiến trình cha ----------

func main() {
	var (
		isChild = flag.Bool("child", false, "chế độ tiến trình con (nội bộ)")
		dir     = flag.String("dir", "", "thư mục làm việc")
		seed    = flag.Uint64("seed", 1, "seed của workload")
		rounds  = flag.Int("n", 200, "số lần kill -9")
		minMs   = flag.Int("min", 60, "thời gian chạy tối thiểu trước khi giết (ms)")
		maxMs   = flag.Int("max", 700, "thời gian chạy tối đa trước khi giết (ms)")
		frames  = flag.Int("frames", 16, "số frame của pool (nhỏ = ép page bẩn ra đĩa sớm, và ép recovery phải undo)")
		nosync  = flag.Bool("nosync", false, "TẮT fsync — kill -9 vẫn không mất gì, xem diary")
		nowrite = flag.Bool("nowrite", false, "GIỮ log trong RAM tiến trình — mô phỏng mất điện thật, PHẢI thấy SAI")
		keep    = flag.Bool("keep", false, "giữ lại thư mục của lần đầu tiên thất bại")
		txns    = flag.Int("txns", 0, "chế độ con: dừng sau N transaction (0 = chạy tới khi bị giết)")
	)
	flag.Parse()

	if *isChild {
		if err := child(*dir, *seed, *nosync, *nowrite, *frames, *txns); err != nil {
			fmt.Fprintln(os.Stderr, "child:", err)
			os.Exit(1)
		}
		return
	}

	self, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	root := *dir
	if root == "" {
		root, err = os.MkdirTemp("", "crashlab")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		defer os.RemoveAll(root)
	}

	mode := "bình thường (fsync bật)"
	switch {
	case *nowrite:
		mode = "log GIỮ TRONG RAM — mong đợi THẤT BẠI"
	case *nosync:
		mode = "fsync TẮT — kill -9 vẫn không mất gì, xem diary"
	}
	fmt.Printf("crashlab: %d vòng kill -9, pool %d frame, chế độ: %s\n", *rounds, *frames, mode)
	fmt.Printf("%-6s %8s %8s %9s  %s\n", "vòng", "sống(ms)", "txn", "khóa", "kết quả")

	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	fails, totalTxn := 0, 0
	start := time.Now()
	for r := 1; r <= *rounds; r++ {
		wd := filepath.Join(root, fmt.Sprintf("r%03d", r))
		if err := os.MkdirAll(wd, 0o755); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		s := uint64(rng.Int63())
		args := []string{"-child", "-dir", wd, "-seed", strconv.FormatUint(s, 10),
			"-frames", strconv.Itoa(*frames)}
		if *nowrite {
			args = append(args, "-nowrite")
		}
		if *nosync {
			args = append(args, "-nosync")
		}
		cmd := exec.Command(self, args...)
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		live := *minMs + rng.Intn(*maxMs-*minMs+1)
		time.Sleep(time.Duration(live) * time.Millisecond)
		// SIGKILL: không handler nào chặn được, không defer nào chạy.
		_ = cmd.Process.Kill()
		_ = cmd.Wait()

		v := verify(wd, s, *frames)
		totalTxn += v.committed
		if v.ok {
			fmt.Printf("%-6d %8d %8d %9d  ok (%s)\n", r, live, v.committed, v.keys, v.recovered)
			os.RemoveAll(wd)
			continue
		}
		fails++
		fmt.Printf("%-6d %8d %8d %9d  SAI: %s\n", r, live, v.committed, v.keys, v.reason)
		if *keep && fails == 1 {
			fmt.Printf("       giữ lại %s để soi\n", wd)
			continue
		}
		os.RemoveAll(wd)
	}

	dur := time.Since(start)
	fmt.Printf("\n%d/%d vòng đúng, %d sai. %d transaction đã commit được kiểm, %s.\n",
		*rounds-fails, *rounds, fails, totalTxn, dur.Round(time.Millisecond))
	if fails > 0 {
		os.Exit(1)
	}
}
