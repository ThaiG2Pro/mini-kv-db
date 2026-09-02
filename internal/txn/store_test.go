package txn

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"minidb/internal/db"
)

func openAt(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(path, db.Options{Frames: 64})
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	return s
}

func openStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "data.db")
	s := openAt(t, path)
	t.Cleanup(func() { s.Close() })
	return s, path
}

func mustPut(t *testing.T, s *Store, l Level, k, v string) {
	t.Helper()
	if err := s.Update(l, func(tx *Txn) error { return tx.Put([]byte(k), []byte(v)) }); err != nil {
		t.Fatalf("Put(%s): %v", k, err)
	}
}

func mustGet(t *testing.T, s *Store, l Level, k string) (string, bool) {
	t.Helper()
	var out string
	var ok bool
	if err := s.View(l, func(tx *Txn) error {
		v, has, err := tx.Get([]byte(k))
		out, ok = string(v), has
		return err
	}); err != nil {
		t.Fatalf("Get(%s): %v", k, err)
	}
	return out, ok
}

// maxXmin đi tìm xmin lớn nhất đang nằm trong cây — nguồn sự thật O(n) để đối
// chiếu với bộ cấp id O(1).
func maxXmin(t *testing.T, s *Store) uint64 {
	t.Helper()
	var mx uint64
	err := s.DB().Range(nil, nil, func(k, raw []byte) bool {
		if reserved(k) {
			return true
		}
		c, err := DecodeChain(raw)
		if err != nil {
			t.Errorf("khóa %q: %v", k, err)
			return false
		}
		for _, v := range c {
			if v.Xmin > mx {
				mx = v.Xmin
			}
		}
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	return mx
}

func TestPutGetDelete(t *testing.T) {
	s, _ := openStore(t)
	mustPut(t, s, RepeatableRead, "a", "1")
	if v, ok := mustGet(t, s, RepeatableRead, "a"); !ok || v != "1" {
		t.Fatalf("a = %q,%v", v, ok)
	}
	mustPut(t, s, RepeatableRead, "a", "2")
	if v, ok := mustGet(t, s, RepeatableRead, "a"); !ok || v != "2" {
		t.Fatalf("sau ghi đè a = %q,%v", v, ok)
	}
	if err := s.Update(RepeatableRead, func(tx *Txn) error { return tx.Delete([]byte("a")) }); err != nil {
		t.Fatal(err)
	}
	if v, ok := mustGet(t, s, RepeatableRead, "a"); ok {
		t.Fatalf("xóa rồi mà a vẫn = %q", v)
	}
	if _, ok := mustGet(t, s, RepeatableRead, "chưa-bao-giờ-có"); ok {
		t.Fatal("khóa không tồn tại lại báo có")
	}
}

// TestReadYourOwnWrites: write set phải được hỏi TRƯỚC cây, ở cả ba đường đọc.
func TestReadYourOwnWrites(t *testing.T) {
	s, _ := openStore(t)
	mustPut(t, s, RepeatableRead, "k2", "cũ")

	err := s.Update(RepeatableRead, func(tx *Txn) error {
		if err := tx.Put([]byte("k1"), []byte("mới")); err != nil {
			return err
		}
		if err := tx.Put([]byte("k2"), []byte("ghi đè")); err != nil {
			return err
		}
		if err := tx.Delete([]byte("k3")); err != nil {
			return err
		}
		if v, ok, _ := tx.Get([]byte("k1")); !ok || string(v) != "mới" {
			return fmt.Errorf("k1 = %q,%v", v, ok)
		}
		if v, ok, _ := tx.Get([]byte("k2")); !ok || string(v) != "ghi đè" {
			return fmt.Errorf("k2 = %q,%v — đọc phải thấy bản của chính mình", v, ok)
		}
		if _, ok, _ := tx.Get([]byte("k3")); ok {
			return errors.New("k3 vừa bị chính ta xóa mà vẫn thấy")
		}
		// Và cả trong scan.
		seen := map[string]string{}
		if err := tx.Scan(nil, nil, func(k, v []byte) bool {
			seen[string(k)] = string(v)
			return true
		}); err != nil {
			return err
		}
		if seen["k1"] != "mới" || seen["k2"] != "ghi đè" {
			return fmt.Errorf("scan không thấy write set của chính mình: %v", seen)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestAbortWritesNothing là con số quan trọng nhất của cả phase 6: abort của
// tầng MVCC KHÔNG chạm log lần nào. So với phase 5 (abort đọc ngược chuỗi
// undo, ghi một CLR cho mỗi page đã sửa) thì đây là 0 byte so với hàng chục
// KB. Số byte tuyệt đối đổi theo máy; con số 0 thì không.
func TestAbortWritesNothing(t *testing.T) {
	s, _ := openStore(t)
	tx, err := s.Begin(RepeatableRead)
	if err != nil {
		t.Fatal(err)
	}
	// Đo TỪ SAU lần ghi đầu tiên: chính lần ghi đầu là chỗ transaction nhận
	// xid, và cứ 64 xid thì phải chạm đĩa một lần để nới mốc trên. Đó là một
	// byte log có thật và có lý do — đo lẫn vào đây sẽ làm con số 0 ở dưới
	// thành một con số phập phù theo thời điểm.
	if err := tx.Put([]byte("k0000"), []byte("v")); err != nil {
		t.Fatal(err)
	}
	before := s.DB().Log().End()
	for i := 1; i < 500; i++ {
		if err := tx.Put([]byte(fmt.Sprintf("k%04d", i)), []byte("v")); err != nil {
			t.Fatal(err)
		}
	}
	mid := s.DB().Log().End()
	if mid != before {
		t.Fatalf("499 lần Put đã ghi %d byte log — write set phải nằm trong RAM tới lúc commit", mid-before)
	}
	if err := tx.Abort(); err != nil {
		t.Fatal(err)
	}
	if after := s.DB().Log().End(); after != before {
		t.Fatalf("abort ghi %d byte log, phải là 0", after-before)
	}
	if _, ok := mustGet(t, s, RepeatableRead, "k0000"); ok {
		t.Fatal("abort rồi mà khóa vẫn còn")
	}
}

// TestReadOnlyCommitTouchesNothing: 100% reader không được sinh một byte log.
func TestReadOnlyCommitTouchesNothing(t *testing.T) {
	s, _ := openStore(t)
	for i := 0; i < 20; i++ {
		mustPut(t, s, RepeatableRead, fmt.Sprintf("k%d", i), "v")
	}
	before := s.DB().Log().End()
	for i := 0; i < 50; i++ {
		tx, err := s.Begin(RepeatableRead)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := tx.Get([]byte("k3")); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Count(nil, nil); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	if after := s.DB().Log().End(); after != before {
		t.Fatalf("50 transaction chỉ đọc sinh %d byte log, phải là 0", after-before)
	}
}

func TestReservedKeyRejected(t *testing.T) {
	s, _ := openStore(t)
	err := s.Update(RepeatableRead, func(tx *Txn) error { return tx.Put([]byte{0x00, 'x'}, []byte("v")) })
	if !errors.Is(err, ErrReservedKey) {
		t.Fatalf("muốn ErrReservedKey, được %v", err)
	}
	err = s.View(RepeatableRead, func(tx *Txn) error {
		_, _, err := tx.Get(metaNext)
		return err
	})
	if !errors.Is(err, ErrReservedKey) {
		t.Fatalf("đọc khóa metadata cũng phải bị chặn, được %v", err)
	}
}

// TestIDCounterSurvivesReopen kiểm bất biến sống-chết của MVCC bền: sau khi mở
// lại file, id cấp tiếp theo phải LỚN HƠN mọi xmin đang nằm trong cây. Nếu
// không, một version cũ đột nhiên "bắt đầu sau ta" và biến khỏi mọi snapshot.
//
// Đây cũng là chỗ đối chiếu hai nguồn: cơ chế O(1) (một khóa metadata, đặt
// trước 64 id mỗi lần chạm đĩa) với sự thật O(n) (quét cả cây tìm max xmin).
func TestIDCounterSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.db")
	s := openAt(t, path)
	for i := 0; i < 200; i++ {
		mustPut(t, s, RepeatableRead, fmt.Sprintf("k%03d", i), "v")
	}
	mx1 := maxXmin(t, s)
	if mx1 == 0 {
		t.Fatal("không tìm thấy xmin nào — bài test không kiểm gì cả")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2 := openAt(t, path)
	defer s2.Close()
	if next := s2.NextID(); next <= mx1 {
		t.Fatalf("mở lại: NextID = %d nhưng cây đã có xmin = %d", next, mx1)
	}
	// Dữ liệu còn nguyên và vẫn nhìn thấy được bằng snapshot mới.
	if v, ok := mustGet(t, s2, RepeatableRead, "k100"); !ok || v != "v" {
		t.Fatalf("k100 = %q,%v sau khi mở lại", v, ok)
	}
	mustPut(t, s2, RepeatableRead, "k100", "sau")
	if mx2 := maxXmin(t, s2); mx2 <= mx1 {
		t.Fatalf("ghi sau khi mở lại cho xmin = %d, không lớn hơn %d", mx2, mx1)
	}
}

// TestOpportunisticPruneKeepsChainShort: ghi lại một khóa 1000 lần mà không có
// reader cũ nào -> chuỗi không được dài ra. Đây là HOT prune, và nếu nó không
// chạy thì chuỗi vượt trần (~2KB một entry) và khóa đó chết hẳn.
func TestOpportunisticPruneKeepsChainShort(t *testing.T) {
	s, _ := openStore(t)
	for i := 0; i < 1000; i++ {
		mustPut(t, s, RepeatableRead, "hot", fmt.Sprintf("v%d", i))
	}
	cs, err := s.ChainStats()
	if err != nil {
		t.Fatal(err)
	}
	if cs.MaxChain > 2 {
		t.Fatalf("1000 lần ghi để lại chuỗi dài %d — bộ dọn cơ hội không chạy", cs.MaxChain)
	}
	if v, ok := mustGet(t, s, RepeatableRead, "hot"); !ok || v != "v999" {
		t.Fatalf("hot = %q,%v", v, ok)
	}
}

// TestVacuumBlockedByOldReader là `idle_in_transaction` của Postgres, thu nhỏ:
// một transaction chỉ đọc mở lâu ghim horizon lại và làm mọi bộ dọn vô ích.
func TestVacuumBlockedByOldReader(t *testing.T) {
	s, _ := openStore(t)
	mustPut(t, s, RepeatableRead, "k", "v0")

	old, err := s.Begin(RepeatableRead) // reader mở suốt bài test
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := old.Get([]byte("k")); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 20; i++ {
		mustPut(t, s, RepeatableRead, "k", fmt.Sprintf("v%d", i))
	}
	cs, err := s.ChainStats()
	if err != nil {
		t.Fatal(err)
	}
	if cs.MaxChain < 2 {
		t.Fatalf("reader cũ đang mở mà chuỗi chỉ dài %d — bản cũ đã bị dọn mất", cs.MaxChain)
	}
	if v, _, _ := old.Get([]byte("k")); string(v) != "v0" {
		t.Fatalf("reader cũ thấy %q, phải vẫn thấy v0", v)
	}
	vs, err := s.Vacuum()
	if err != nil {
		t.Fatal(err)
	}
	if vs.VersionsPruned() != 0 {
		t.Fatalf("vacuum dọn %d version dù reader cũ còn mở", vs.VersionsPruned())
	}

	// Đóng reader -> horizon nhảy lên -> lần vacuum sau dọn được.
	old.Abort()
	vs, err = s.Vacuum()
	if err != nil {
		t.Fatal(err)
	}
	if vs.VersionsPruned() == 0 {
		t.Fatal("đóng reader rồi mà vacuum vẫn không dọn được gì")
	}
	cs, _ = s.ChainStats()
	if cs.MaxChain != 1 {
		t.Fatalf("sau vacuum chuỗi vẫn dài %d", cs.MaxChain)
	}
}

// TestVacuumReclaimsDeletedKey: tombstone phải rời khỏi cây, không chỉ rời
// khỏi tầm nhìn. Đây là chỗ chỗ trống thật sự được trả về cho B+Tree.
func TestVacuumReclaimsDeletedKey(t *testing.T) {
	s, _ := openStore(t)
	for i := 0; i < 50; i++ {
		mustPut(t, s, RepeatableRead, fmt.Sprintf("k%02d", i), "v")
	}
	for i := 0; i < 50; i++ {
		k := fmt.Sprintf("k%02d", i)
		if err := s.Update(RepeatableRead, func(tx *Txn) error { return tx.Delete([]byte(k)) }); err != nil {
			t.Fatal(err)
		}
	}
	vs, err := s.Vacuum()
	if err != nil {
		t.Fatal(err)
	}
	cs, err := s.ChainStats()
	if err != nil {
		t.Fatal(err)
	}
	if cs.Keys != 0 {
		t.Fatalf("còn %d khóa trong cây sau khi xóa hết và vacuum (thu hồi %d)", cs.Keys, vs.KeysReclaimed)
	}
}

// TestChainFullIsReported: trần cứng của MVCC-tại-chỗ phải báo bằng ErrChainFull
// chứ không phải bằng "entry lớn hơn một page" — thông báo nói đúng nguyên nhân.
func TestChainFullIsReported(t *testing.T) {
	s, _ := openStore(t)
	mustPut(t, s, RepeatableRead, "k", "v")
	old, err := s.Begin(RepeatableRead) // ghim horizon để không dọn được gì
	if err != nil {
		t.Fatal(err)
	}
	defer old.Abort()
	if _, _, err := old.Get([]byte("k")); err != nil {
		t.Fatal(err)
	}

	var lastErr error
	for i := 0; i < MaxVersions+10; i++ {
		lastErr = s.Update(RepeatableRead, func(tx *Txn) error {
			return tx.Put([]byte("k"), []byte("0123456789012345678901234567890123456789"))
		})
		if lastErr != nil {
			break
		}
	}
	if !errors.Is(lastErr, ErrChainFull) {
		t.Fatalf("muốn ErrChainFull, được %v", lastErr)
	}
	// Và khóa vẫn đọc được: thất bại phải không phá gì.
	if v, ok := mustGet(t, s, RepeatableRead, "k"); !ok || v == "" {
		t.Fatalf("sau khi chuỗi đầy, k = %q,%v", v, ok)
	}
}

func TestScanOrderAndBounds(t *testing.T) {
	s, _ := openStore(t)
	for _, k := range []string{"a", "b", "c", "d", "e"} {
		mustPut(t, s, RepeatableRead, k, k)
	}
	if err := s.Update(RepeatableRead, func(tx *Txn) error { return tx.Delete([]byte("c")) }); err != nil {
		t.Fatal(err)
	}
	var got []string
	if err := s.View(RepeatableRead, func(tx *Txn) error {
		return tx.Scan([]byte("b"), []byte("e"), func(k, v []byte) bool {
			if string(k) != string(v) {
				t.Errorf("khóa %q mang value %q", k, v)
			}
			got = append(got, string(k))
			return true
		})
	}); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != "[b d]" {
		t.Fatalf("scan [b,e) = %v, muốn [b d] (c đã xóa, e ngoài khoảng)", got)
	}
}
