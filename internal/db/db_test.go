package db

import (
	"encoding/binary"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"minidb/internal/pager"
	"minidb/internal/wal"
)

func key(i int) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(i))
	return b[:]
}

func val(i, n int) []byte {
	v := make([]byte, n)
	for j := range v {
		v[j] = byte(i + j)
	}
	return v
}

func openTmp(t *testing.T, dir string, opt Options) *DB {
	t.Helper()
	d, err := Open(filepath.Join(dir, "test.db"), opt)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return d
}

// TestCommitSurvivesCrash là bất biến số 1 của phase 5 ở dạng nhỏ nhất: cái gì
// đã báo commit thì phải còn sau khi mất điện, dù KHÔNG page dữ liệu nào kịp
// xuống đĩa.
func TestCommitSurvivesCrash(t *testing.T) {
	dir := t.TempDir()
	d := openTmp(t, dir, Options{CheckpointBytes: -1})
	for i := 0; i < 500; i++ {
		if err := d.Update(func(tx *Txn) error { return tx.Put(key(i), val(i, 60)) }); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	dirty := d.DirtyPages()
	if dirty == 0 {
		t.Fatal("không còn page bẩn nào: test này chỉ có nghĩa khi dữ liệu CHƯA xuống đĩa")
	}
	if err := d.SimulateCrash(); err != nil {
		t.Fatalf("crash: %v", err)
	}

	d2 := openTmp(t, dir, Options{CheckpointBytes: -1})
	defer d2.Close()
	for i := 0; i < 500; i++ {
		got, err := d2.Get(key(i))
		if err != nil {
			t.Fatalf("sau recovery, mất khóa %d: %v", i, err)
		}
		if string(got) != string(val(i, 60)) {
			t.Fatalf("khóa %d: value sai", i)
		}
	}
	t.Logf("crash với %d page bẩn; redo áp %d, bỏ qua %d; loser=%d",
		dirty, d2.RedoApplied, d2.RedoSkipped, d2.LoserTxns)
}

// TestUncommittedVanishes là nửa còn lại của durability: cái chưa commit phải
// biến mất SẠCH, không để lại nửa vời.
func TestUncommittedVanishes(t *testing.T) {
	dir := t.TempDir()
	// Pool nhỏ là điều kiện để test này có nghĩa: chỉ khi page bẩn bị ĐUỔI ra
	// đĩa thì log của transaction dở dang mới bị kéo xuống đĩa theo (WAL rule),
	// và mới có gì để undo. Pool rộng thì crash làm bay hết trong RAM và
	// "transaction chưa commit biến mất" đúng một cách tầm thường.
	d := openTmp(t, dir, Options{Frames: 8, CheckpointBytes: -1})
	for i := 0; i < 200; i++ {
		if err := d.Update(func(tx *Txn) error { return tx.Put(key(i), val(i, 60)) }); err != nil {
			t.Fatal(err)
		}
	}
	// Một transaction to, không commit.
	tx, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 1000; i < 1400; i++ {
		if err := tx.Put(key(i), val(i, 200)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 100; i++ {
		if err := tx.Delete(key(i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.SimulateCrash(); err != nil {
		t.Fatal(err)
	}

	d2 := openTmp(t, dir, Options{Frames: 8, CheckpointBytes: -1})
	defer d2.Close()
	if d2.LoserTxns != 1 {
		t.Fatalf("mong đúng 1 transaction thua cuộc, có %d", d2.LoserTxns)
	}
	if d2.UndoApplied == 0 {
		t.Fatal("không có bước undo nào chạy")
	}
	for i := 0; i < 200; i++ {
		if _, err := d2.Get(key(i)); err != nil {
			t.Fatalf("khóa đã commit %d biến mất: %v", i, err)
		}
	}
	for i := 1000; i < 1400; i++ {
		if ok, _ := d2.Has(key(i)); ok {
			t.Fatalf("khóa %d của transaction chưa commit vẫn còn", i)
		}
	}
	mustVerify(t, d2)
}

// TestAbortIsUndoTooKhớp: Abort lúc chạy và undo lúc recovery phải cho ra cùng
// một cây. Nếu hai đường khác nhau thì cái sai chỉ lộ ra sau khi mất điện.
func TestAbortMatchesRecoveryUndo(t *testing.T) {
	build := func(crash bool) string {
		dir := t.TempDir()
		d := openTmp(t, dir, Options{CheckpointBytes: -1})
		for i := 0; i < 300; i++ {
			if err := d.Update(func(tx *Txn) error { return tx.Put(key(i), val(i, 80)) }); err != nil {
				t.Fatal(err)
			}
		}
		tx, err := d.Begin()
		if err != nil {
			t.Fatal(err)
		}
		for i := 300; i < 700; i++ {
			if err := tx.Put(key(i), val(i, 300)); err != nil {
				t.Fatal(err)
			}
		}
		for i := 0; i < 150; i++ {
			if err := tx.Delete(key(i)); err != nil {
				t.Fatal(err)
			}
		}
		if crash {
			if err := d.SimulateCrash(); err != nil {
				t.Fatal(err)
			}
			d2 := openTmp(t, dir, Options{CheckpointBytes: -1})
			defer d2.Close()
			return digest(t, d2)
		}
		if err := tx.Abort(); err != nil {
			t.Fatalf("abort: %v", err)
		}
		defer d.Close()
		return digest(t, d)
	}
	a, b := build(false), build(true)
	if a != b {
		t.Fatalf("Abort và recovery-undo cho hai cây khác nhau:\n abort:    %s\n recovery: %s", a, b)
	}
}

// digest tóm tắt nội dung cây thành một chuỗi so sánh được.
func digest(t *testing.T, d *DB) string {
	t.Helper()
	n, sum := 0, uint64(0)
	err := d.tree.Range(nil, nil, func(k, v []byte) bool {
		n++
		for _, c := range k {
			sum = sum*1315423911 + uint64(c)
		}
		sum = sum*31 + uint64(len(v))
		return true
	})
	if err != nil {
		t.Fatalf("range: %v", err)
	}
	return fmt.Sprintf("n=%d sum=%016x", n, sum)
}

func mustVerify(t *testing.T, d *DB) {
	t.Helper()
	r, err := d.tree.Verify()
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !r.OK() {
		t.Fatalf("cây hỏng sau recovery: %v", r.Errors)
	}
}

// TestRecoveryIsIdempotent: chạy recovery hai lần liên tiếp phải ra cùng một
// kết quả. Đây là điều kiện để recovery tự nó chịu được crash.
func TestRecoveryIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	d := openTmp(t, dir, Options{CheckpointBytes: -1})
	for i := 0; i < 400; i++ {
		if err := d.Update(func(tx *Txn) error { return tx.Put(key(i), val(i, 100)) }); err != nil {
			t.Fatal(err)
		}
	}
	tx, _ := d.Begin()
	for i := 400; i < 600; i++ {
		tx.Put(key(i), val(i, 100))
	}
	d.SimulateCrash()

	var prev string
	for round := 0; round < 3; round++ {
		d2 := openTmp(t, dir, Options{CheckpointBytes: -1})
		got := digest(t, d2)
		mustVerify(t, d2)
		if round > 0 && got != prev {
			t.Fatalf("recovery lần %d khác lần trước: %s vs %s", round+1, got, prev)
		}
		prev = got
		if round < 2 {
			if err := d2.SimulateCrash(); err != nil {
				t.Fatal(err)
			}
		} else {
			d2.Close()
		}
	}
}

// TestCheckpointShortensRecovery đo đúng cái checkpoint sinh ra để làm.
func TestCheckpointShortensRecovery(t *testing.T) {
	run := func(ckpt bool) (redo int64, from uint64) {
		dir := t.TempDir()
		d := openTmp(t, dir, Options{CheckpointBytes: -1})
		for i := 0; i < 800; i++ {
			d.Update(func(tx *Txn) error { return tx.Put(key(i), val(i, 60)) })
			if ckpt && i == 700 {
				if err := d.CheckpointFlush(); err != nil {
					t.Fatal(err)
				}
			}
		}
		d.SimulateCrash()
		d2 := openTmp(t, dir, Options{CheckpointBytes: -1})
		defer d2.Close()
		return d2.RedoApplied + d2.RedoSkipped, d2.log.Checkpoint()
	}
	without, _ := run(false)
	with, ck := run(true)
	if with >= without {
		t.Fatalf("checkpoint không rút ngắn redo: có=%d, không=%d (ckpt lsn=%d)", with, without, ck)
	}
	t.Logf("record phải xét khi redo: không checkpoint=%d, có checkpoint=%d (%.1fx)",
		without, with, float64(without)/float64(with))
}

// TestWALRuleHolds: không page bẩn nào được rời RAM khi log của nó chưa fsync.
// Kiểm bằng cách ép pool nhỏ xíu (liên tục phải đuổi page) rồi đối chiếu
// pageLSN của MỌI page trên đĩa với flushedLSN.
func TestWALRuleHolds(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal.db")
	d, err := Open(path, Options{Frames: 8, CheckpointBytes: -1})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3000; i++ {
		if err := d.Update(func(tx *Txn) error { return tx.Put(key(i), val(i, 120)) }); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	flushed := d.log.Flushed()
	st := d.pool.Stats()
	if st.DirtyEvictions == 0 {
		t.Fatal("không có lần đuổi page bẩn nào: test này chưa kiểm được gì")
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fi, _ := f.Stat()
	buf := make([]byte, 4096)
	bad := 0
	for off := int64(2 * 4096); off+4096 <= fi.Size(); off += 4096 {
		if _, err := f.ReadAt(buf, off); err != nil {
			t.Fatal(err)
		}
		lsn := binary.LittleEndian.Uint64(buf[16:])
		if lsn > flushed {
			bad++
		}
	}
	if bad != 0 {
		t.Fatalf("%d page trên đĩa mang pageLSN > flushedLSN=%d — WAL rule bị vi phạm", bad, flushed)
	}
	t.Logf("%d lần đuổi page bẩn, %d lần gọi FlushLog, flushedLSN=%d",
		st.DirtyEvictions, st.FlushLogCalls, flushed)
}

// TestRandomOpsThenCrash: fuzz nhẹ — N thao tác ngẫu nhiên, crash ở một điểm
// ngẫu nhiên, mở lại và đòi hỏi (a) cây hợp lệ, (b) đúng tập khóa đã commit.
func TestRandomOpsThenCrash(t *testing.T) {
	for seed := int64(1); seed <= 12; seed++ {
		t.Run(fmt.Sprintf("seed%d", seed), func(t *testing.T) {
			dir := t.TempDir()
			rng := rand.New(rand.NewSource(seed))
			d := openTmp(t, dir, Options{Frames: 16, CheckpointBytes: 64 << 10})
			want := map[string]string{}

			ops := 300 + rng.Intn(400)
			for i := 0; i < ops; i++ {
				tx, err := d.Begin()
				if err != nil {
					t.Fatal(err)
				}
				local := map[string]string{}
				del := map[string]bool{}
				for k := 0; k < 1+rng.Intn(8); k++ {
					kk := key(rng.Intn(600))
					if rng.Intn(4) == 0 {
						if err := tx.Delete(kk); err == nil {
							del[string(kk)] = true
							delete(local, string(kk))
						}
						continue
					}
					vv := val(rng.Intn(1000), 20+rng.Intn(400))
					if err := tx.Put(kk, vv); err != nil {
						t.Fatalf("put: %v", err)
					}
					local[string(kk)] = string(vv)
					delete(del, string(kk))
				}
				if rng.Intn(6) == 0 {
					if err := tx.Abort(); err != nil {
						t.Fatalf("abort: %v", err)
					}
					continue
				}
				if err := tx.Commit(); err != nil {
					t.Fatalf("commit: %v", err)
				}
				for k := range del {
					delete(want, k)
				}
				for k, v := range local {
					want[k] = v
				}
			}
			if err := d.SimulateCrash(); err != nil {
				t.Fatal(err)
			}

			d2 := openTmp(t, dir, Options{Frames: 16, CheckpointBytes: -1})
			defer d2.Close()
			mustVerify(t, d2)
			got := map[string]string{}
			if err := d2.tree.Range(nil, nil, func(k, v []byte) bool {
				got[string(k)] = string(v)
				return true
			}); err != nil {
				t.Fatal(err)
			}
			if len(got) != len(want) {
				t.Fatalf("số khóa: có %d, mong %d", len(got), len(want))
			}
			for k, v := range want {
				if got[k] != v {
					t.Fatalf("khóa %x: value sai sau recovery", k)
				}
			}
		})
	}
}

// TestFullPageWriteAppearsOncePerCheckpoint kiểm đúng cái nợ P2-3 định trả.
func TestFullPageWriteAppearsOncePerCheckpoint(t *testing.T) {
	dir := t.TempDir()
	d := openTmp(t, dir, Options{CheckpointBytes: -1})
	defer d.Close()
	// Checkpoint trước khi đo: page gốc vừa được ALLOC lúc Open, mà ALLOC đã
	// đóng vai ảnh-trọn-page rồi (redo của nó dựng lại page từ số 0). Không
	// checkpoint thì 40 lần chèn đầu tiên hợp lệ khi KHÔNG ghi trọn page nào.
	if err := d.CheckpointFlush(); err != nil {
		t.Fatal(err)
	}
	d.log.ResetStats()
	for i := 0; i < 40; i++ {
		if err := d.Update(func(tx *Txn) error { return tx.Put(key(i), val(i, 40)) }); err != nil {
			t.Fatal(err)
		}
	}
	first := d.log.Stats().FullPages
	if first == 0 {
		t.Fatal("không có full-page write nào — mặc định phải BẬT")
	}
	// 40 lần chèn vào cùng một leaf: chỉ lần đầu ghi trọn page.
	if first > 4 {
		t.Fatalf("full-page write %d lần cho 40 insert vào ít page — mốc checkpoint sai", first)
	}
	if err := d.CheckpointFlush(); err != nil {
		t.Fatal(err)
	}
	for i := 40; i < 60; i++ {
		d.Update(func(tx *Txn) error { return tx.Put(key(i), val(i, 40)) })
	}
	if d.log.Stats().FullPages <= first {
		t.Fatal("sau checkpoint, lần chạm đầu tiên phải ghi trọn page lần nữa")
	}
	t.Logf("full-page write: %d trước checkpoint, %d sau", first, d.log.Stats().FullPages-first)
}

// TestSingleWriter: ràng buộc một writer giờ do code bắt buộc (nợ P1-2).
func TestSingleWriter(t *testing.T) {
	dir := t.TempDir()
	d := openTmp(t, dir, Options{})
	defer d.Close()
	tx, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Begin(); err == nil {
		t.Fatal("mở được hai writer cùng lúc")
	}
	tx.Abort()
	tx2, err := d.Begin()
	if err != nil {
		t.Fatalf("sau abort phải mở được: %v", err)
	}
	tx2.Abort()
}

// TestLogTailTornIsIgnored: đuôi log ghi dở phải bị coi là hết log, không được
// diễn giải bừa và cũng không được làm hỏng phần trước nó.
func TestLogTailTornIsIgnored(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "torn.db")
	d, err := Open(path, Options{CheckpointBytes: -1})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		d.Update(func(tx *Txn) error { return tx.Put(key(i), val(i, 50)) })
	}
	if err := d.SimulateCrash(); err != nil {
		t.Fatal(err)
	}
	// Nối 300 byte rác vào cuối log — đúng hình dạng của một record ghi dở.
	f, err := os.OpenFile(path+".wal", os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	junk := make([]byte, 300)
	rand.New(rand.NewSource(9)).Read(junk)
	if _, err := f.Write(junk); err != nil {
		t.Fatal(err)
	}
	f.Close()

	d2, err := Open(path, Options{CheckpointBytes: -1})
	if err != nil {
		t.Fatalf("mở lại với đuôi rác: %v", err)
	}
	defer d2.Close()
	for i := 0; i < 100; i++ {
		if _, err := d2.Get(key(i)); err != nil {
			t.Fatalf("khóa %d mất vì đuôi log rác: %v", i, err)
		}
	}
	// Và log phải nối tiếp được từ chỗ record hợp lệ cuối cùng, chứ không phải
	// từ sau đống rác: Open cắt file về đúng đó.
	if d2.log.End() < wal.FirstLSN {
		t.Fatal("log rỗng sau khi cắt đuôi")
	}
}

// Đường đọc không được chụp một tấm ảnh page nào. Benchmark chỉ nói "chênh
// khoảng 3%", con số ấy đổi theo máy; còn cái này thì không: 0 là 0.
func TestReadPathTakesNoSnapshot(t *testing.T) {
	d := openTmp(t, t.TempDir(), Options{Frames: 64})
	defer d.Close()
	if err := d.Update(func(tx *Txn) error {
		for i := 0; i < 500; i++ {
			if err := tx.Put(key(i), val(i, 60)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := d.j.SnapTaken
	if before == 0 {
		t.Fatal("đường GHI phải chụp ảnh, mà lại không chụp cái nào")
	}
	for i := 0; i < 500; i++ {
		if _, err := d.Get(key(i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.tree.Range(nil, nil, func(k, v []byte) bool { return true }); err != nil {
		t.Fatal(err)
	}
	if got := d.j.SnapTaken - before; got != 0 {
		t.Fatalf("đường đọc chụp %d ảnh page, phải là 0", got)
	}
}

// Nợ P1-1: transaction bị abort đã nới file, những page ấy phải quay về
// freelist chứ không được thành page mồ côi — không thuộc meta nào, cũng
// không ai cấp lại được. Trước phase 5 chỉ phát hiện được bằng cmd/dbcheck
// sau khi sự đã rồi; giờ nó là một test.
//
// Phép thử: đếm page trước và sau một transaction abort đủ lớn để nới file.
// Số page cấp mới phải bằng số page thêm vào freelist. Lệch nghĩa là rò.
func TestAbortLeavesNoOrphanPage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "orphan.db")
	d, err := Open(path, Options{Frames: 16})
	if err != nil {
		t.Fatal(err)
	}
	// Một ít dữ liệu nền để cây có hình dạng thật, rồi checkpoint để mọi
	// thứ trước đó đã yên vị trong meta.
	if err := d.Update(func(tx *Txn) error {
		for i := 0; i < 200; i++ {
			if err := tx.Put(key(i), val(i, 80)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.CheckpointFlush(); err != nil {
		t.Fatal(err)
	}
	pagesBefore := d.pg.PageCount()
	freeBefore := len(d.pg.FreeList()) + d.pg.MetaPendingCount()

	tx, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 1000; i < 1600; i++ {
		if err := tx.Put(key(i), val(i, 300)); err != nil {
			t.Fatal(err)
		}
	}
	grown := d.pg.PageCount() - pagesBefore
	if grown == 0 {
		t.Fatal("transaction không nới file, phép thử này không kiểm được gì")
	}
	if err := tx.Abort(); err != nil {
		t.Fatal(err)
	}
	if err := d.CheckpointFlush(); err != nil {
		t.Fatal(err)
	}
	// Freelist phải tự nó nằm ở đâu đó: CommitMeta lấy page từ chính freelist
	// ra làm page chứa freelist, nên page ấy rời `free` mà vẫn thuộc quyền sở
	// hữu của meta. Đếm thiếu nó là kết luận sai có một page mồ côi.
	freeAfter := len(d.pg.FreeList()) + d.pg.MetaPendingCount()
	if got := freeAfter - freeBefore; got != int(grown) {
		t.Fatalf("abort nới %d page nhưng chỉ trả %d page (freelist + page chứa freelist): %d page mồ côi",
			grown, got, int(grown)-got)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	// Và bên ngoài nhìn vào cũng phải thấy sạch: đúng cái mà dbcheck kiểm.
	rep, err := pager.Verify(path)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK() {
		t.Fatalf("pager.Verify thấy lỗi sau abort: %v", rep.Errors)
	}
}
