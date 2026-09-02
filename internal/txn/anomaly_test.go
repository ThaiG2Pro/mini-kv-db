package txn

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"minidb/internal/db"
)

// anomaly_test.go là deliverable của phase 6: **tái tạo được từng anomaly, và
// chứng minh mức isolation cao chặn nó.**
//
// Một bảng chỉ có cột "chặn" thì không chứng minh gì cả — nếu bài test dựng
// anomaly sai thì mọi mức đều "chặn" và bảng vẫn xanh. Vì thế mỗi ô đều được
// khẳng định theo CẢ HAI chiều: mức thấp phải để lọt, mức cao phải chặn. Đây
// đúng là bài học của `make crashlab-nowrite` ở phase 5: một bài test chưa bao
// giờ đỏ thì chưa phải bằng chứng.

func anomalyStore(t *testing.T) *Store {
	t.Helper()
	s, _ := openStore(t)
	// Lock timeout ngắn: ở Serializable, "bị chặn" là kết cục ĐÚNG của nhiều
	// ô trong bảng, và bài test phải kết luận được điều đó trong vài trăm ms
	// thay vì ngồi đợi 5 giây mặc định.
	s.Locks().Timeout = ProbeWait
	return s
}

// TestAnomalyMatrix chạy cả bảng và khẳng định theo CẢ HAI chiều: mức thấp
// phải để lọt, mức cao phải chặn.
//
// Một bảng chỉ có cột "chặn" thì không chứng minh gì cả — nếu bài dựng anomaly
// sai thì mọi mức đều "chặn" và bảng vẫn xanh. Đây đúng là bài học của
// `make crashlab-nowrite` ở phase 5: một bài test chưa bao giờ đỏ thì chưa
// phải bằng chứng.
func TestAnomalyMatrix(t *testing.T) {
	for _, a := range Anomalies {
		for i, l := range AllLevels {
			t.Run(fmt.Sprintf("%s/%s", a.Name, l), func(t *testing.T) {
				s := anomalyStore(t)
				got, err := a.Run(s, l)
				if err != nil {
					t.Fatal(err)
				}
				if got != a.Want[i] {
					verb := map[bool]string{true: "XẢY RA", false: "bị chặn"}
					t.Fatalf("%s ở %s: %s, nhưng phải %s (%s)",
						a.Name, l, verb[got], verb[a.Want[i]], a.Note)
				}
			})
		}
	}
}

// TestSerializableBlocksWriteSkewByDeadlock không chỉ kiểm "write skew bị
// chặn" mà kiểm CÁCH nó bị chặn: bằng một deadlock thật, do đồ thị wait-for
// bắt được, chứ không phải bằng timeout. Phân biệt được hai thứ đó là phân
// biệt được "đúng" với "may".
func TestSerializableBlocksWriteSkewByDeadlock(t *testing.T) {
	s := anomalyStore(t)
	got, err := probeWriteSkew(s, Serializable)
	if err != nil {
		t.Fatal(err)
	}
	if got {
		t.Fatal("Serializable để lọt write skew")
	}
	st := s.Locks().Stats()
	if st.Deadlocks == 0 {
		t.Fatalf("write skew bị chặn nhưng Deadlocks = 0, Timeouts = %d — "+
			"nếu chặn được chỉ nhờ timeout thì trên máy chậm hơn nó sẽ lọt", st.Timeouts)
	}
}

// TestReadUncommittedNeedsExtraCode ghi lại phát hiện ngược đời của phase 6:
// trong kiến trúc deferred-write, dirty read phải được CÀI THÊM. Bằng chứng
// là bộ đếm DirtyReads — nó chỉ tăng ở đúng một mức.
func TestReadUncommittedNeedsExtraCode(t *testing.T) {
	for _, l := range AllLevels {
		s := anomalyStore(t)
		if _, err := probeDirtyRead(s, l); err != nil {
			t.Fatal(err)
		}
		got := s.Stats().DirtyReads
		if (l == ReadUncommitted) != (got > 0) {
			t.Errorf("%s: DirtyReads = %d", l, got)
		}
	}
}

// TestSnapshotReadDoesNotBlockWriter là món nợ P1-2b được trả, ở dạng khẳng
// định được: một reader mở suốt thời gian đó không làm writer nào chậm lại
// hay thất bại. Ở Serializable thì ngược lại — và bài test khẳng định luôn cả
// vế đó, vì "reader không chặn writer" là tính chất của MVCC, không phải của
// mọi cơ chế đồng thời.
func TestSnapshotReadDoesNotBlockWriter(t *testing.T) {
	s, _ := openStore(t)
	s.Locks().Timeout = ProbeWait
	mustPut(t, s, RepeatableRead, "k", "v0")

	r, err := s.Begin(RepeatableRead)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Get([]byte("k")); err != nil {
		t.Fatal(err)
	}
	// Ghi vào KHÓA KHÁC: không có trần nào, và reader không hề biết.
	for i := 0; i < 500; i++ {
		if err := s.Update(RepeatableRead, func(tx *Txn) error {
			return tx.Put([]byte(fmt.Sprintf("other%03d", i)), []byte("v"))
		}); err != nil {
			t.Fatalf("writer thứ %d (khóa khác) thất bại dù reader chỉ đọc: %v", i, err)
		}
	}
	// Ghi lại CÙNG một khóa: vẫn không bị chặn, nhưng chỉ tới trần của chuỗi
	// version — xem TestOldReaderStarvesWriterOnSameKey ngay dưới.
	for i := 0; i < MaxVersions/2; i++ {
		if err := s.Update(RepeatableRead, func(tx *Txn) error {
			return tx.Put([]byte("k"), []byte(fmt.Sprintf("v%d", i+1)))
		}); err != nil {
			t.Fatalf("writer thứ %d thất bại dù reader chỉ đọc: %v", i, err)
		}
	}
	if v, _, _ := r.Get([]byte("k")); string(v) != "v0" {
		t.Fatalf("reader thấy %q sau %d lần ghi, phải vẫn thấy v0", v, MaxVersions/2)
	}
	r.Abort()

	// Vế đối chứng: cùng kịch bản ở Serializable thì writer PHẢI vấp.
	r2, err := s.Begin(Serializable)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r2.Get([]byte("k")); err != nil {
		t.Fatal(err)
	}
	err = s.Update(Serializable, func(tx *Txn) error { return tx.Put([]byte("k"), []byte("x")) })
	if err == nil {
		t.Fatal("ở Serializable, reader đang giữ lock S mà writer vẫn ghi được — lock không có tác dụng")
	}
	if !Retryable(err) {
		t.Fatalf("writer phải chết vì đồng thời (thử lại được), chết vì: %v", err)
	}
	r2.Abort()
}

// TestPhysicalLayerStaysSingleWriter chốt lại giả định nền của cả thiết kế:
// dù có bao nhiêu transaction logic, tầng vật lý của phase 5 vẫn chỉ thấy một
// writer tại một thời điểm. Nếu bất biến này vỡ thì WAL/undo của phase 5 hết
// đúng, và crashlab sẽ đỏ theo — nhưng đỏ muộn hơn nhiều so với ở đây.
func TestPhysicalLayerStaysSingleWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.db")
	d, err := db.Open(path, db.Options{Frames: 64})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	s, err := Wrap(d)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 40; i++ {
				k := fmt.Sprintf("w%d-%03d", w, i)
				if err := s.Update(RepeatableRead, func(tx *Txn) error {
					return tx.Put([]byte(k), []byte("v"))
				}); err != nil {
					errs <- fmt.Errorf("%s: %w", k, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("8 writer song song mà có lỗi: %v", err)
	}
	cs, err := s.ChainStats()
	if err != nil {
		t.Fatal(err)
	}
	if cs.Keys != 8*40 {
		t.Fatalf("có %d khóa, muốn %d", cs.Keys, 8*40)
	}
}

// TestOldReaderStarvesWriterOnSameKey nói vế còn lại của sự thật, và là chỗ
// bài test trước đã đỏ ở lần chạy đầu (writer thứ 63 chết vì ErrChainFull).
//
// "Reader không chặn writer" là tính chất của MVCC, nhưng nó chỉ đúng TRONG
// TRẦN của chỗ chứa bản cũ. Ở bản này bản cũ nằm ngay trong entry B+Tree nên
// trần là ~2KB / MaxVersions; một reader mở lâu ghim horizon lại, chuỗi không
// dọn được, và writer chết vì hết chỗ chứ không phải vì bị chặn.
//
// Postgres không có trần cứng này (bản mới đi sang page khác) nên hậu quả ở
// đó nhẹ hơn nhưng âm thầm hơn: bảng phình mãi. Cùng một nguyên nhân, hai
// triệu chứng — và đó chính là câu trả lời cho câu hỏi "vì sao Postgres cần
// VACUUM" mà phase 2 đặt ra.
func TestOldReaderStarvesWriterOnSameKey(t *testing.T) {
	s, _ := openStore(t)
	mustPut(t, s, RepeatableRead, "k", "v0")

	r, err := s.Begin(RepeatableRead)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Get([]byte("k")); err != nil {
		t.Fatal(err)
	}

	n := 0
	var last error
	for i := 0; i < MaxVersions*4; i++ {
		last = s.Update(RepeatableRead, func(tx *Txn) error {
			return tx.Put([]byte("k"), []byte(fmt.Sprintf("v%d", i+1)))
		})
		if last != nil {
			break
		}
		n++
	}
	if !errors.Is(last, ErrChainFull) {
		t.Fatalf("sau %d lần ghi, lỗi là %v — muốn ErrChainFull", n, last)
	}
	t.Logf("reader cũ mở suốt: writer sống được %d lần ghi cùng một khóa rồi hết chỗ", n)

	// Và đây là cách chữa duy nhất: ĐÓNG reader cũ, rồi dọn.
	r.Abort()
	if _, err := s.Vacuum(); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(RepeatableRead, func(tx *Txn) error {
		return tx.Put([]byte("k"), []byte("sau-vacuum"))
	}); err != nil {
		t.Fatalf("đóng reader và vacuum rồi mà vẫn không ghi được: %v", err)
	}
	if v, ok := mustGet(t, s, RepeatableRead, "k"); !ok || v != "sau-vacuum" {
		t.Fatalf("k = %q,%v", v, ok)
	}
}
