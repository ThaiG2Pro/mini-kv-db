package pager

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// ---------- helper chèn lỗi ----------

// flakey bọc *os.File để mô phỏng crash mà không cần giết tiến trình thật.
// Nhắc lại kết luận phase 0: kill -9 KHÔNG tạo được torn write (kernel vẫn
// writeback đầy đủ), nên muốn test đường rollback thì phải chèn lỗi ở tầng này
// hoặc ở tầng device (dm-flakey).
type flakey struct {
	*os.File
	// dropMetaWrite: bỏ hẳn lời ghi vào meta page (mô phỏng chết TRƯỚC bước 3).
	dropMetaWrite bool
	// tornPrefix > 0: chỉ ghi tornPrefix byte đầu của meta page rồi báo lỗi
	// (mô phỏng chết GIỮA bước 3).
	tornPrefix int
}

func (fl *flakey) WriteAt(p []byte, off int64) (int, error) {
	isMeta := off == 0 || off == PageSize
	switch {
	case isMeta && fl.dropMetaWrite:
		return len(p), nil // báo thành công nhưng không ghi gì — đúng như dm-flakey drop_writes
	case isMeta && fl.tornPrefix > 0:
		n, _ := fl.File.WriteAt(p[:fl.tornPrefix], off)
		return n, fmt.Errorf("mô phỏng crash sau %d byte", n)
	}
	return fl.File.WriteAt(p, off)
}

func tmpDB(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "test.db")
}

func fill(b byte) []byte {
	buf := make([]byte, PageSize)
	for i := range buf {
		buf[i] = b
	}
	return buf
}

// rawMeta đọc thẳng meta page từ đĩa, không qua pager.
func rawMeta(t *testing.T, path string, id PageID) (meta, error) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	buf := make([]byte, PageSize)
	if _, err := f.ReadAt(buf, int64(id)*PageSize); err != nil {
		t.Fatal(err)
	}
	return decodeMeta(buf)
}

// ---------- meta page ----------

func TestOpenCreatesTwoValidMetas(t *testing.T) {
	path := tmpDB(t)
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	if p.TxnID() != 1 {
		t.Errorf("txnID mới tạo = %d, muốn 1", p.TxnID())
	}
	if p.PageCount() != 2 {
		t.Errorf("pageCount = %d, muốn 2 (hai meta page)", p.PageCount())
	}
	// Cả HAI meta page phải hợp lệ ngay từ lúc tạo file, nếu không thì lần
	// commit đầu tiên đã không có đích rollback.
	for _, id := range []PageID{metaPageA, metaPageB} {
		if _, err := rawMeta(t, path, id); err != nil {
			t.Errorf("meta page %d không hợp lệ ngay sau khi tạo: %v", id, err)
		}
	}
}

func TestCommitAlternatesMetaPage(t *testing.T) {
	path := tmpDB(t)
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	for i := 0; i < 6; i++ {
		if err := p.Commit(PageID(100 + i)); err != nil {
			t.Fatal(err)
		}
		want := MetaPageOf(p.TxnID())
		m, err := rawMeta(t, path, want)
		if err != nil {
			t.Fatalf("txn %d: meta page %d không đọc được: %v", p.TxnID(), want, err)
		}
		if m.txnID != p.TxnID() {
			t.Errorf("txn %d ghi vào page %d nhưng page đó có txnID=%d", p.TxnID(), want, m.txnID)
		}
		// Page còn lại phải giữ txnID = txn trước — đây là đích rollback.
		other, err := rawMeta(t, path, 1-want)
		if err != nil {
			t.Fatalf("meta page đối diện hỏng: %v", err)
		}
		if p.TxnID() >= 2 && other.txnID != p.TxnID()-1 {
			t.Errorf("meta page %d có txnID=%d, muốn %d", 1-want, other.txnID, p.TxnID()-1)
		}
	}
}

// Chết TRƯỚC khi meta được ghi: dữ liệu đã fsync nhưng không ai trỏ tới.
func TestRollbackWhenMetaWriteLost(t *testing.T) {
	path := tmpDB(t)
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// commit 1: root = 7, có thật trên đĩa
	id, err := p.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.WritePage(id, fill(0xAA)); err != nil {
		t.Fatal(err)
	}
	if err := p.Commit(id); err != nil {
		t.Fatal(err)
	}
	goodTxn, goodRoot := p.TxnID(), p.Root()
	p.Close()

	// commit 2 trên file đã bị "drop_writes" ở meta page
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := NewWithFile(&flakey{File: f, dropMetaWrite: true}, path)
	if err != nil {
		t.Fatal(err)
	}
	id2, err := p2.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	if err := p2.WritePage(id2, fill(0xBB)); err != nil {
		t.Fatal(err)
	}
	if err := p2.Commit(id2); err != nil {
		t.Fatal(err)
	}
	p2.Close()

	// mở lại bằng file thật: phải thấy trạng thái của commit 1
	p3, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p3.Close()
	if p3.TxnID() != goodTxn || p3.Root() != goodRoot {
		t.Fatalf("sau crash: txnID=%d root=%d; muốn txnID=%d root=%d",
			p3.TxnID(), p3.Root(), goodTxn, goodRoot)
	}
	buf := make([]byte, PageSize)
	if err := p3.ReadPage(goodRoot, buf); err != nil {
		t.Fatal(err)
	}
	if buf[0] != 0xAA {
		t.Errorf("dữ liệu đã commit bị mất: buf[0]=%#x, muốn 0xAA", buf[0])
	}
}

// Chết GIỮA lúc ghi meta: checksum sai -> bị loại -> rollback.
func TestRollbackWhenMetaTorn(t *testing.T) {
	path := tmpDB(t)
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Commit(5); err != nil {
		t.Fatal(err)
	}
	goodTxn, goodRoot := p.TxnID(), p.Root()
	p.Close()

	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	// 20 byte: root mới đã xuống đĩa nhưng txnID/pageCount/checksum còn của
	// meta cũ -> checksum không khớp. (Xem ghi chú trong diary về 512B sector:
	// đây là lỗi MẠNH HƠN thực tế, cố tình, để test đúng nhánh checksum.)
	p2, err := NewWithFile(&flakey{File: f, tornPrefix: 20}, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := p2.Commit(9); err == nil {
		t.Fatal("Commit phải trả lỗi khi ghi meta thất bại")
	}
	p2.Close()

	tornPage := MetaPageOf(goodTxn + 1)
	if _, err := rawMeta(t, path, tornPage); err == nil {
		t.Errorf("meta page %d bị ghi dở nhưng vẫn decode được — checksum không bắt được", tornPage)
	}

	p3, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p3.Close()
	if p3.TxnID() != goodTxn || p3.Root() != goodRoot {
		t.Fatalf("sau torn write: txnID=%d root=%d; muốn %d/%d",
			p3.TxnID(), p3.Root(), goodTxn, goodRoot)
	}
}

func TestBothMetaCorruptIsError(t *testing.T) {
	path := tmpDB(t)
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	p.Close()

	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	for _, off := range []int64{0, PageSize} {
		if _, err := f.WriteAt([]byte{0xFF}, off+offRoot); err != nil { // đổi root, không sửa checksum
			t.Fatal(err)
		}
	}
	f.Close()

	if _, err := Open(path); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Open trả %v, muốn ErrCorrupt", err)
	}
}

// ---------- freelist ----------

func TestFreelistReusesPages(t *testing.T) {
	path := tmpDB(t)
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	var ids []PageID
	for i := 0; i < 5; i++ {
		id, err := p.Allocate()
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := p.Commit(ids[0]); err != nil {
		t.Fatal(err)
	}
	before := p.PageCount()

	for _, id := range ids[2:] {
		if err := p.Free(id); err != nil {
			t.Fatal(err)
		}
	}
	// Page vừa free KHÔNG được cấp lại trong cùng txn: meta cũ còn trỏ tới nó.
	got, err := p.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids[2:] {
		if got == id {
			t.Fatalf("Allocate trả về page %d vừa được Free trong cùng txn", id)
		}
	}
	if err := p.Commit(ids[0]); err != nil {
		t.Fatal(err)
	}

	// Sau commit, chúng mới được tái dùng -> file không cần phình thêm.
	n := p.FreeCount()
	if n == 0 {
		t.Fatal("sau commit freelist vẫn rỗng")
	}
	for i := 0; i < n; i++ {
		if _, err := p.Allocate(); err != nil {
			t.Fatal(err)
		}
	}
	if p.PageCount() > before+4 { // +1 page freelist mỗi commit, cho dư chút
		t.Errorf("pageCount phình từ %d lên %d dù có freelist", before, p.PageCount())
	}
}

func TestFreelistSurvivesReopen(t *testing.T) {
	path := tmpDB(t)
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var ids []PageID
	for i := 0; i < 40; i++ {
		id, _ := p.Allocate()
		ids = append(ids, id)
	}
	if err := p.Commit(ids[0]); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids[10:] {
		if err := p.Free(id); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.Commit(ids[0]); err != nil {
		t.Fatal(err)
	}
	want := p.FreeCount()
	head := p.FreelistHead()
	if head == 0 {
		t.Fatal("freelist head = 0 dù đã free 30 page")
	}
	p.Close()

	p2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Close()
	if p2.FreeCount() != want {
		t.Errorf("sau reopen FreeCount=%d, trước khi đóng=%d", p2.FreeCount(), want)
	}
}

// Nhiều hơn một page freelist -> phải nối chuỗi qua trường `next`.
func TestFreelistOverflowChain(t *testing.T) {
	path := tmpDB(t)
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	n := freelistPerPage + 50
	ids := make([]PageID, 0, n)
	for i := 0; i < n; i++ {
		id, err := p.Allocate()
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := p.Commit(ids[0]); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if err := p.Free(id); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.Commit(0); err != nil {
		t.Fatal(err)
	}
	want := p.FreeCount()
	if want <= freelistPerPage {
		t.Fatalf("FreeCount=%d, cần > %d để test được chuỗi", want, freelistPerPage)
	}
	// Đếm số page trong chuỗi bằng cách đọc thẳng đĩa.
	f, _ := os.Open(path)
	defer f.Close()
	buf := make([]byte, PageSize)
	links := 0
	for id := p.FreelistHead(); id != 0; links++ {
		if _, err := f.ReadAt(buf, int64(id)*PageSize); err != nil {
			t.Fatal(err)
		}
		id = PageID(binary.LittleEndian.Uint32(buf[offFLNext:]))
	}
	if links < 2 {
		t.Errorf("chuỗi freelist chỉ có %d page, muốn >= 2", links)
	}
	p.Close()

	p2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Close()
	if p2.FreeCount() != want {
		t.Errorf("sau reopen FreeCount=%d, muốn %d", p2.FreeCount(), want)
	}
}

// ---------- pread ----------

func TestMetaPagesAreWriteProtected(t *testing.T) {
	p, err := Open(tmpDB(t))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for _, id := range []PageID{metaPageA, metaPageB} {
		if err := p.WritePage(id, fill(1)); !errors.Is(err, ErrMetaPageBusy) {
			t.Errorf("WritePage(%d) trả %v, muốn ErrMetaPageBusy", id, err)
		}
		if err := p.Free(id); !errors.Is(err, ErrMetaPageBusy) {
			t.Errorf("Free(%d) trả %v, muốn ErrMetaPageBusy", id, err)
		}
	}
}

// pread có offset riêng cho từng lời gọi -> nhiều goroutine đọc song song vẫn
// đúng. Seek+Read dùng chung file offset và sẽ hỏng ở đây.
func TestConcurrentReadAtIsSafe(t *testing.T) {
	p, err := Open(tmpDB(t))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	const n = 64
	ids := make([]PageID, n)
	for i := 0; i < n; i++ {
		id, err := p.Allocate()
		if err != nil {
			t.Fatal(err)
		}
		if err := p.WritePage(id, fill(byte(i))); err != nil {
			t.Fatal(err)
		}
		ids[i] = id
	}
	if err := p.Commit(ids[0]); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			buf := make([]byte, PageSize)
			for r := 0; r < 200; r++ {
				if _, err := p.f.ReadAt(buf, int64(ids[i])*PageSize); err != nil {
					errs[i] = err
					return
				}
				if buf[0] != byte(i) || buf[PageSize-1] != byte(i) {
					errs[i] = fmt.Errorf("page %d đọc ra %#x/%#x", ids[i], buf[0], buf[PageSize-1])
					return
				}
			}
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
}

func TestDataSurvivesReopen(t *testing.T) {
	path := tmpDB(t)
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := p.Allocate()
	if err := p.WritePage(id, fill(0x5A)); err != nil {
		t.Fatal(err)
	}
	if err := p.Commit(id); err != nil {
		t.Fatal(err)
	}
	p.Close()

	p2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Close()
	buf := make([]byte, PageSize)
	if err := p2.ReadPage(p2.Root(), buf); err != nil {
		t.Fatal(err)
	}
	if buf[0] != 0x5A || buf[PageSize-1] != 0x5A {
		t.Errorf("page sau reopen: %#x..%#x", buf[0], buf[PageSize-1])
	}
}

// ---------- bench: cái giá của fsync trong Commit ----------

func BenchmarkCommit(b *testing.B) {
	dir := b.TempDir()
	p, err := Open(filepath.Join(dir, "bench.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer p.Close()
	id, _ := p.Allocate()
	buf := fill(1)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := p.WritePage(id, buf); err != nil {
			b.Fatal(err)
		}
		if err := p.Commit(id); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkReadPage(b *testing.B) {
	dir := b.TempDir()
	p, err := Open(filepath.Join(dir, "bench.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer p.Close()
	id, _ := p.Allocate()
	if err := p.WritePage(id, fill(1)); err != nil {
		b.Fatal(err)
	}
	if err := p.Commit(id); err != nil {
		b.Fatal(err)
	}
	buf := make([]byte, PageSize)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := p.ReadPage(id, buf); err != nil {
			b.Fatal(err)
		}
	}
}

// ---------- crash tại mọi điểm ghi ----------

// countingFlakey cho n-1 lời ghi đầu đi qua, lời ghi thứ n chỉ ghi được một
// phần rồi lỗi, các lời ghi sau đó bị bỏ. Đây là mô hình xấu nhất của một lần
// mất điện: một lời ghi dở dang + mọi thứ sau đó không xảy ra.
type countingFlakey struct {
	*os.File
	failAt  int
	n       int
	partial int
}

func (fl *countingFlakey) WriteAt(p []byte, off int64) (int, error) {
	fl.n++
	switch {
	case fl.n < fl.failAt:
		return fl.File.WriteAt(p, off)
	case fl.n == fl.failAt:
		k := min(fl.partial, len(p))
		n, _ := fl.File.WriteAt(p[:k], off)
		return n, fmt.Errorf("crash mô phỏng: lời ghi #%d chỉ đi được %d byte", fl.n, n)
	default:
		return 0, fmt.Errorf("crash mô phỏng: lời ghi #%d bị bỏ", fl.n)
	}
}

func (fl *countingFlakey) Sync() error {
	if fl.n >= fl.failAt {
		return fmt.Errorf("crash mô phỏng: fsync sau khi chết")
	}
	return fl.File.Sync()
}

// Bất kể crash ở lời ghi thứ mấy, mở lại file phải LUÔN thành công và trạng
// thái phải là MỘT TRONG HAI: commit cũ nguyên vẹn, hoặc commit mới trọn vẹn.
// Không có trạng thái thứ ba.
func TestCrashAtEveryWriteIsRecoverable(t *testing.T) {
	for _, partial := range []int{0, 20, 1000, PageSize - 1} {
		for failAt := 1; failAt <= 10; failAt++ {
			name := fmt.Sprintf("partial=%d/failAt=%d", partial, failAt)
			t.Run(name, func(t *testing.T) {
				path := tmpDB(t)

				// commit 1: trạng thái tốt đã durable
				p, err := Open(path)
				if err != nil {
					t.Fatal(err)
				}
				var keep []PageID
				for i := 0; i < 6; i++ {
					id, err := p.Allocate()
					if err != nil {
						t.Fatal(err)
					}
					if err := p.WritePage(id, fill(0xAA)); err != nil {
						t.Fatal(err)
					}
					keep = append(keep, id)
				}
				if err := p.Commit(keep[0]); err != nil {
					t.Fatal(err)
				}
				oldTxn, oldRoot := p.TxnID(), p.Root()
				p.Close()

				// commit 2 trên file sẽ "mất điện" ở lời ghi thứ failAt
				f, err := os.OpenFile(path, os.O_RDWR, 0o644)
				if err != nil {
					t.Fatal(err)
				}
				p2, err := NewWithFile(&countingFlakey{File: f, failAt: failAt, partial: partial}, path)
				if err != nil {
					t.Fatal(err)
				}
				newRoot, err := p2.Allocate()
				if err == nil {
					err = p2.WritePage(newRoot, fill(0xBB))
				}
				if err == nil {
					for _, id := range keep[3:] { // sinh freelist page ở commit này
						if err = p2.Free(id); err != nil {
							break
						}
					}
				}
				if err == nil {
					err = p2.Commit(newRoot)
				}
				_ = err // crash mô phỏng: lỗi là điều mong đợi
				p2.Close()

				// mở lại bằng file thật — đây là phần phải luôn đúng
				p3, err := Open(path)
				if err != nil {
					t.Fatalf("mở lại sau crash thất bại: %v", err)
				}
				defer p3.Close()

				buf := make([]byte, PageSize)
				switch p3.TxnID() {
				case oldTxn: // rollback
					if p3.Root() != oldRoot {
						t.Fatalf("rollback nhưng root=%d, muốn %d", p3.Root(), oldRoot)
					}
					if err := p3.ReadPage(p3.Root(), buf); err != nil {
						t.Fatal(err)
					}
					if buf[0] != 0xAA {
						t.Fatalf("rollback nhưng nội dung root = %#x", buf[0])
					}
				case oldTxn + 1: // commit mới đã kịp durable
					if err := p3.ReadPage(p3.Root(), buf); err != nil {
						t.Fatal(err)
					}
					if buf[0] != 0xBB {
						t.Fatalf("txn mới nhưng nội dung root = %#x", buf[0])
					}
				default:
					t.Fatalf("txnID=%d, chỉ được phép %d hoặc %d", p3.TxnID(), oldTxn, oldTxn+1)
				}

				// và freelist phải đọc được, không vòng lặp
				if _, _, err := InspectFreelist(path, p3.FreelistHead()); err != nil {
					t.Fatalf("freelist hỏng sau crash: %v", err)
				}
			})
		}
	}
}

// Tách cái giá của durability: cùng một Commit, chỉ khác có fsync hay không.
func BenchmarkCommitNoSync(b *testing.B) {
	p, err := Open(filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer p.Close()
	p.NoSync = true
	id, _ := p.Allocate()
	buf := fill(1)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := p.WritePage(id, buf); err != nil {
			b.Fatal(err)
		}
		if err := p.Commit(id); err != nil {
			b.Fatal(err)
		}
	}
}

// Ghi 64 page rồi commit MỘT lần: cùng lượng dữ liệu, chia đôi số fsync cho
// nhiều page hơn. Đây là lý do tồn tại của group commit (phase 5).
func BenchmarkCommitBatch64(b *testing.B) {
	p, err := Open(filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer p.Close()
	ids := make([]PageID, 64)
	for i := range ids {
		ids[i], _ = p.Allocate()
	}
	if err := p.Commit(ids[0]); err != nil {
		b.Fatal(err)
	}
	buf := fill(1)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, id := range ids {
			if err := p.WritePage(id, buf); err != nil {
				b.Fatal(err)
			}
		}
		if err := p.Commit(ids[0]); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/64, "ns/page")
}

// ---------- nợ #4 phase 1: WriteAt trả về n < len(p) mà err == nil ----------

type shortWriter struct {
	*os.File
	keep int // số byte thực sự ghi được cho lời ghi đầu tiên
	done bool
}

func (sw *shortWriter) WriteAt(p []byte, off int64) (int, error) {
	if !sw.done {
		sw.done = true
		n, _ := sw.File.WriteAt(p[:min(sw.keep, len(p))], off)
		return n, nil // ĐÚNG như POSIX cho phép: ghi thiếu, không báo lỗi
	}
	return sw.File.WriteAt(p, off)
}

// Ghi thiếu byte mà KHÔNG báo lỗi là hợp lệ theo POSIX. Pager phải tự coi đó
// là lỗi, nếu không nó commit một page ghi dở rồi tưởng là thành công.
func TestShortWriteIsAnError(t *testing.T) {
	// keep=20: meta page ghi dở tới mức checksum không khớp -> rollback thật.
	// keep=PageSize-1: meta vẫn hợp lệ (36 byte đầu đã đủ, phần sau là padding
	// vốn đã bằng 0), nên KHÔNG rollback — nhưng Commit vẫn phải báo lỗi.
	for _, tc := range []struct {
		keep         int
		wantRollback bool
	}{
		{20, true},
		{PageSize - 1, false},
	} {
		t.Run(fmt.Sprintf("keep=%d", tc.keep), func(t *testing.T) {
			path := tmpDB(t)
			p, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := p.Commit(0); err != nil {
				t.Fatal(err)
			}
			goodTxn := p.TxnID()
			p.Close()

			f, err := os.OpenFile(path, os.O_RDWR, 0o644)
			if err != nil {
				t.Fatal(err)
			}
			p2, err := NewWithFile(&shortWriter{File: f, keep: tc.keep}, path)
			if err != nil {
				t.Fatal(err)
			}
			err = p2.Commit(3)
			p2.Close()
			if err == nil {
				t.Fatalf("Commit báo THÀNH CÔNG dù lời ghi chỉ đi được %d/%d byte", tc.keep, PageSize)
			}

			p3, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer p3.Close()
			if tc.wantRollback && p3.TxnID() != goodTxn {
				t.Errorf("txnID=%d, muốn rollback về %d", p3.TxnID(), goodTxn)
			}
			// Dù rollback hay không, file phải luôn ở trạng thái đọc được.
			r, err := Verify(path)
			if err != nil {
				t.Fatal(err)
			}
			if !r.OK() {
				t.Errorf("Verify báo lỗi sau ghi thiếu: %v", r.Errors)
			}
		})
	}
}

// Verify phải bắt được đúng cái bug freelist tự trỏ vào chính nó mà tôi đã
// dính, và bắt được page mồ côi ở đuôi file.
func TestVerifyDetectsTailLeak(t *testing.T) {
	path := tmpDB(t)
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Allocate(); err != nil { // cấp rồi KHÔNG commit -> file dài ra, meta không biết
		t.Fatal(err)
	}
	p.Close()

	r, err := Verify(path)
	if err != nil {
		t.Fatal(err)
	}
	if r.TailLeak != 1 {
		t.Errorf("TailLeak=%d, muốn 1 (page cấp rồi bỏ)", r.TailLeak)
	}
	if !r.OK() {
		t.Errorf("rò rỉ đuôi file chỉ là cảnh báo, không phải lỗi: %v", r.Errors)
	}
}

func TestVerifyOnHealthyFile(t *testing.T) {
	path := tmpDB(t)
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var ids []PageID
	for i := 0; i < 30; i++ {
		id, _ := p.Allocate()
		if err := p.WritePage(id, fill(byte(i))); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := p.Commit(ids[0]); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids[5:] {
		if err := p.Free(id); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.Commit(ids[0]); err != nil {
		t.Fatal(err)
	}
	p.Close()

	r, err := Verify(path)
	if err != nil {
		t.Fatal(err)
	}
	if !r.OK() {
		t.Fatalf("file lành mà Verify báo lỗi: %v", r.Errors)
	}
	if len(r.SelfInFree) != 0 {
		t.Errorf("freelist page tự nằm trong danh sách của nó: %v", r.SelfInFree)
	}
	if len(r.Duplicates) != 0 {
		t.Errorf("double free: %v", r.Duplicates)
	}
}

// So hai chính sách cấp phát trên cùng một workload churn (cấp N, free N-1,
// commit). Chỉ số quan trọng không phải ns/op mà là "khoảng cách trung bình
// giữa hai page được ghi liên tiếp" — nó quyết định lời ghi là tuần tự hay
// ngẫu nhiên, và phase 0 đã đo được chênh lệch ~4x ở fsync.
func benchAllocPolicy(b *testing.B, policy AllocPolicy) {
	p, err := Open(filepath.Join(b.TempDir(), "alloc.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer p.Close()
	p.Policy = policy
	p.NoSync = true // tách layout ra khỏi fsync; fsync đo riêng trên máy thật

	buf := fill(7)

	// Warm-up: tạo một file đã có sẵn NHIỀU page rỗng rải rác. Không có bước
	// này thì mọi Allocate đều đi nới file, freelist không được dùng, và hai
	// chính sách cho ra kết quả giống hệt nhau (đã dính đúng bẫy đó).
	const pool = 4096
	var all []PageID
	for i := 0; i < pool; i++ {
		id, err := p.Allocate()
		if err != nil {
			b.Fatal(err)
		}
		all = append(all, id)
	}
	if err := p.Commit(all[0]); err != nil {
		b.Fatal(err)
	}
	for _, id := range all[1:] { // free gần hết -> freelist lớn
		if err := p.Free(id); err != nil {
			b.Fatal(err)
		}
	}
	if err := p.Commit(all[0]); err != nil {
		b.Fatal(err)
	}
	startCount := p.PageCount()

	var gapSum, gapN float64
	var prev PageID
	var live []PageID

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for j := 0; j < 8; j++ { // cấp 8, free 8 -> tổng số page không đổi
			id, err := p.Allocate()
			if err != nil {
				b.Fatal(err)
			}
			if err := p.WritePage(id, buf); err != nil {
				b.Fatal(err)
			}
			if prev != 0 {
				d := float64(id) - float64(prev)
				if d < 0 {
					d = -d
				}
				gapSum += d
				gapN++
			}
			prev = id
			live = append(live, id)
		}
		for j := 0; j < 8 && len(live) > 1; j++ {
			if err := p.Free(live[0]); err != nil {
				b.Fatal(err)
			}
			live = live[1:]
		}
		if err := p.Commit(prev); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	if gapN > 0 {
		b.ReportMetric(gapSum/gapN, "page-gap")
	}
	b.ReportMetric(float64(p.PageCount()-startCount), "page-phình")
}

func BenchmarkAllocLIFO(b *testing.B)   { benchAllocPolicy(b, AllocLIFO) }
func BenchmarkAllocLowest(b *testing.B) { benchAllocPolicy(b, AllocLowest) }
