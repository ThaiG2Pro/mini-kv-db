package wal

import (
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func openTmp(t *testing.T) *Log {
	t.Helper()
	l, err := Open(filepath.Join(t.TempDir(), "x.wal"))
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestRecordRoundTrip(t *testing.T) {
	r := Record{PrevLSN: 7, TxnID: 9, UndoNext: 11, PageID: 42, Type: TypeUpdate,
		Flags: FlagFullPage, Payload: []byte("xin chào")}
	buf := Encode(nil, &r, 100)
	got, n, err := Decode(buf)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(buf) {
		t.Fatalf("tiêu thụ %d/%d byte", n, len(buf))
	}
	if got.LSN != 100 || got.TxnID != 9 || got.PageID != 42 || string(got.Payload) != "xin chào" {
		t.Fatalf("giải mã sai: %+v", got)
	}
}

// TestOneBitFlipIsCaught: crc phải bắt được hỏng ở BẤT KỲ byte nào, kể cả
// trong header. Một record hỏng mà đọc được là một recovery dựng lại dữ liệu
// sai — tệ hơn hẳn recovery báo hết log.
func TestOneBitFlipIsCaught(t *testing.T) {
	r := Record{TxnID: 3, PageID: 5, Type: TypeUpdate, Payload: make([]byte, 64)}
	buf := Encode(nil, &r, 32)
	for i := range buf {
		if i == offTotalLen || i == offTotalLen+1 { // đổi độ dài -> bắt bằng đường khác
			continue
		}
		bad := append([]byte(nil), buf...)
		bad[i] ^= 0x01
		if _, _, err := Decode(bad); err == nil {
			t.Fatalf("lật một bit ở byte %d mà vẫn giải mã được", i)
		}
	}
}

func TestDiffRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	before := make([]byte, 4096)
	rng.Read(before)
	after := append([]byte(nil), before...)
	// Ba vùng rời nhau, đúng hình dạng một lần chèn vào slotted page.
	copy(after[0:8], []byte("hdr!hdr!"))
	copy(after[600:640], make([]byte, 40))
	copy(after[4000:4090], []byte("cell"))

	segs := Diff(before, after, DiffGran)
	if len(segs) != 3 {
		t.Fatalf("mong 3 đoạn, có %d: %v", len(segs), segs)
	}
	if SegBytes(segs) > 200 {
		t.Fatalf("diff phủ %d byte cho 3 thay đổi nhỏ", SegBytes(segs))
	}
	pl := EncodePayload(nil, segs, segs, before, after)
	gsb, gsa, gb, ga, err := DecodePayload(pl)
	if err != nil {
		t.Fatal(err)
	}
	redone := append([]byte(nil), before...)
	if err := Apply(redone, gsa, ga); err != nil {
		t.Fatal(err)
	}
	if string(redone) != string(after) {
		t.Fatal("redo không dựng lại được ảnh sau")
	}
	if err := Apply(redone, gsb, gb); err != nil {
		t.Fatal(err)
	}
	if string(redone) != string(before) {
		t.Fatal("undo không dựng lại được ảnh trước")
	}

	// Ảnh-trọn-page: phía redo trọn 4096, phía undo vẫn chỉ là diff. Đó là
	// toàn bộ điểm của việc tách hai danh sách đoạn.
	fullA := []Seg{{Off: 0, Len: 4096}}
	plFull := EncodePayload(nil, segs, fullA, before, after)
	if len(plFull) >= 2*4096 {
		t.Fatalf("record trọn page tốn %d byte — phải nhỏ hơn 8192 (chỉ redo mới trọn)", len(plFull))
	}
	sb2, sa2, b2, a2, err := DecodePayload(plFull)
	if err != nil {
		t.Fatal(err)
	}
	torn := make([]byte, 4096) // page hỏng hoàn toàn
	if err := Apply(torn, sa2, a2); err != nil {
		t.Fatal(err)
	}
	if string(torn) != string(after) {
		t.Fatal("ảnh trọn page không dựng lại được page bị torn write")
	}
	if err := Apply(torn, sb2, b2); err != nil {
		t.Fatal(err)
	}
	if string(torn) != string(before) {
		t.Fatal("undo sau khi redo trọn page không ra ảnh trước")
	}
}

// TestTornTailStopsScan: đuôi ghi dở phải là "hết log", và mọi record trước nó
// vẫn phải đọc được.
func TestTornTailStopsScan(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.wal")
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		if _, err := l.Append(&Record{Type: TypeUpdate, TxnID: 1, PageID: 2, Payload: make([]byte, 100)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Sync(); err != nil {
		t.Fatal(err)
	}
	end := l.End()
	l.Close()

	// Cắt file đúng giữa một record cuối rồi nối rác.
	f, _ := os.OpenFile(path, os.O_RDWR, 0o644)
	f.Truncate(int64(end) - 40)
	f.WriteAt([]byte("rác rác rác rác"), int64(end)-40)
	f.Close()

	l2, err := Open(path)
	if err != nil {
		t.Fatalf("mở lại: %v", err)
	}
	defer l2.Close()
	n := 0
	if err := l2.Scan(FirstLSN, func(r *Record) error { n++; return nil }); err != nil {
		t.Fatal(err)
	}
	if n != 49 {
		t.Fatalf("đọc được %d record, mong 49 (record cuối bị cắt)", n)
	}
	// Và log phải nối tiếp được từ đúng chỗ đó.
	lsn, err := l2.Append(&Record{Type: TypeCommit, TxnID: 1})
	if err != nil {
		t.Fatal(err)
	}
	if lsn >= end {
		t.Fatalf("record mới ở lsn=%d, phải nằm TRƯỚC đuôi hỏng (%d)", lsn, end)
	}
}

// TestGroupCommit: N goroutine cùng commit thì số lần fsync phải NHỎ HƠN HẲN
// N. Đây là cả lý do tồn tại của group commit — fsync là thao tác đắt nhất
// trong toàn bộ database (phase 0: ~1ms), nên gộp được bao nhiêu là lãi bấy nhiêu.
func TestGroupCommit(t *testing.T) {
	l := openTmp(t)
	defer l.Close()
	const n = 200
	lsns := make([]uint64, n)
	for i := range lsns {
		lsn, err := l.Append(&Record{Type: TypeCommit, TxnID: uint64(i + 1)})
		if err != nil {
			t.Fatal(err)
		}
		lsns[i] = lsn
	}
	var wg sync.WaitGroup
	for i := range lsns {
		wg.Add(1)
		go func(u uint64) {
			defer wg.Done()
			if err := l.Flush(u + 1); err != nil {
				t.Error(err)
			}
		}(lsns[i])
	}
	wg.Wait()
	st := l.Stats()
	if st.Flushed < lsns[n-1] {
		t.Fatalf("flushedLSN=%d chưa tới record cuối %d", st.Flushed, lsns[n-1])
	}
	if st.Syncs >= n {
		t.Fatalf("%d lần fsync cho %d commit — không gộp được gì", st.Syncs, n)
	}
	t.Logf("%d commit -> %d fsync (%d lần được người khác fsync hộ, %d lần thấy đã xong sẵn)",
		n, st.Syncs, st.GroupedSyncs, st.FlushNoops)
}

// TestFlushIsMonotonic: Flush(x) trả về nghĩa là MỌI record < x đã trên đĩa.
func TestFlushIsMonotonic(t *testing.T) {
	l := openTmp(t)
	defer l.Close()
	var last uint64
	for i := 0; i < 100; i++ {
		lsn, _ := l.Append(&Record{Type: TypeUpdate, TxnID: 1, PageID: 1, Payload: make([]byte, 50)})
		if i%7 == 0 {
			if err := l.Flush(lsn); err != nil {
				t.Fatal(err)
			}
			if l.Flushed() < lsn {
				t.Fatalf("Flush(%d) xong mà flushedLSN=%d", lsn, l.Flushed())
			}
		}
		last = lsn
	}
	_ = last
}

func TestReadFromBufferAndFile(t *testing.T) {
	l := openTmp(t)
	defer l.Close()
	a, _ := l.Append(&Record{Type: TypeUpdate, TxnID: 1, PageID: 1, Payload: []byte("một")})
	if err := l.Sync(); err != nil {
		t.Fatal(err)
	}
	b, _ := l.Append(&Record{Type: TypeUpdate, TxnID: 1, PageID: 2, Payload: []byte("hai")})
	ra, err := l.Read(a)
	if err != nil || string(ra.Payload) != "một" {
		t.Fatalf("đọc record đã fsync: %v %q", err, ra.Payload)
	}
	rb, err := l.Read(b)
	if err != nil || string(rb.Payload) != "hai" {
		t.Fatalf("đọc record còn trong buffer: %v %q", err, rb.Payload)
	}
	if _, err := l.Read(l.End()); err == nil {
		t.Fatal("đọc quá cuối log phải lỗi")
	}
}
