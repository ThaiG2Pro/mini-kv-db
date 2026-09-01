package btree

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"minidb/internal/bufpool"
	"minidb/internal/page"
	"minidb/internal/pager"
)

// newTree dựng cây trên MemDB với pool đúng nFrames frame.
//
// nFrames nhỏ là CÓ CHỦ Ý: cây phải chạy được khi pool nhỏ hơn cây rất nhiều.
// Ngưỡng dưới là height+2 (đường root->leaf được ghim hết trong lúc ghi, cộng
// page anh em lúc split/merge). Test nào cũng phải trả lời được câu "nếu
// quên Unpin ở đâu đó thì có nổ không" — pool 8 frame làm nó nổ ngay.
func newTree(t testing.TB, nFrames int) (*Tree, *bufpool.Pool, *MemDB) {
	t.Helper()
	db := NewMemDB()
	pool := bufpool.New(db, nFrames, bufpool.NewLRU(nFrames))
	pool.Alloc = db
	tr, err := Create(pool)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return tr, pool, db
}

func k(i int) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(i))
	return b[:]
}

func v(i int) []byte { return []byte(fmt.Sprintf("v%d", i)) }

// mustVerify chạy Verify và in cả cây ra khi hỏng. Không in cây thì một lỗi
// "B1: leaf lệch tầng" không cho biết gì để sửa.
func mustVerify(t *testing.T, tr *Tree, when string) *Report {
	t.Helper()
	r, err := tr.Verify()
	if err != nil {
		t.Fatalf("%s: Verify lỗi: %v", when, err)
	}
	if !r.OK() {
		d, _ := tr.Dump(4)
		t.Fatalf("%s: cây hỏng:\n  %v\n%s", when, r.Errors, d)
	}
	return r
}

// noPins là điều kiện sau MỌI thao tác công khai: cây không giữ pin nào giữa
// hai lệnh. Rò một pin không làm test sai ngay, nó chỉ làm pool hết frame sau
// vài nghìn thao tác — rất khó lần ngược. Nên kiểm ngay tại chỗ.
func noPins(t *testing.T, pool *bufpool.Pool, when string) {
	t.Helper()
	if n := pool.PinnedCount(); n != 0 {
		t.Fatalf("%s: còn %d frame bị pin", when, n)
	}
}

func TestEmptyTree(t *testing.T) {
	tr, pool, _ := newTree(t, 8)
	if _, err := tr.Get(k(1)); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("Get trên cây rỗng = %v, muốn ErrKeyNotFound", err)
	}
	r := mustVerify(t, tr, "cây rỗng")
	if r.Height != 1 || r.Leaves != 1 || r.Keys != 0 {
		t.Fatalf("cây rỗng: height=%d leaves=%d keys=%d, muốn 1/1/0", r.Height, r.Leaves, r.Keys)
	}
	noPins(t, pool, "cây rỗng")
}

func TestPutGetOneNode(t *testing.T) {
	tr, pool, _ := newTree(t, 8)
	for i := 0; i < 10; i++ {
		if err := tr.Put(k(i), v(i)); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
	}
	for i := 0; i < 10; i++ {
		got, err := tr.Get(k(i))
		if err != nil {
			t.Fatalf("Get %d: %v", i, err)
		}
		if !bytes.Equal(got, v(i)) {
			t.Fatalf("Get %d = %q, muốn %q", i, got, v(i))
		}
	}
	r := mustVerify(t, tr, "10 khóa")
	if r.Height != 1 {
		t.Fatalf("10 khóa mà cây cao %d tầng", r.Height)
	}
	noPins(t, pool, "10 khóa")
}

func TestOverwriteKeepsKeyCount(t *testing.T) {
	tr, _, _ := newTree(t, 8)
	for i := 0; i < 200; i++ {
		if err := tr.Put(k(i), v(i)); err != nil {
			t.Fatal(err)
		}
	}
	before := mustVerify(t, tr, "trước ghi đè")
	for i := 0; i < 200; i++ {
		if err := tr.Put(k(i), []byte("khac")); err != nil {
			t.Fatal(err)
		}
	}
	after := mustVerify(t, tr, "sau ghi đè")
	if after.Keys != before.Keys {
		t.Fatalf("ghi đè làm số khóa đổi: %d -> %d", before.Keys, after.Keys)
	}
	if tr.Stats().Overwrites != 200 {
		t.Fatalf("Overwrites = %d, muốn 200", tr.Stats().Overwrites)
	}
	got, _ := tr.Get(k(7))
	if !bytes.Equal(got, []byte("khac")) {
		t.Fatalf("giá trị sau ghi đè = %q", got)
	}
}

// TestSplitPreservesEverything là test then chốt của nửa insert: sau khi cây
// đã tách nhiều lần, KHÔNG khóa nào được biến mất.
func TestSplitPreservesEverything(t *testing.T) {
	const n = 5000
	for _, order := range []string{"tăng", "giảm", "ngẫu nhiên"} {
		t.Run(order, func(t *testing.T) {
			tr, pool, _ := newTree(t, 16)
			keys := make([]int, n)
			for i := range keys {
				keys[i] = i
			}
			switch order {
			case "giảm":
				sort.Sort(sort.Reverse(sort.IntSlice(keys)))
			case "ngẫu nhiên":
				rng := rand.New(rand.NewSource(42))
				rng.Shuffle(n, func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })
			}
			for _, key := range keys {
				if err := tr.Put(k(key), v(key)); err != nil {
					t.Fatalf("Put %d: %v", key, err)
				}
			}
			r := mustVerify(t, tr, order)
			if r.Keys != n {
				t.Fatalf("%s: cây có %d khóa, muốn %d", order, r.Keys, n)
			}
			if r.Height < 2 {
				t.Fatalf("%s: %d khóa mà cây vẫn 1 tầng — split không chạy?", order, n)
			}
			for i := 0; i < n; i++ {
				got, err := tr.Get(k(i))
				if err != nil {
					t.Fatalf("%s: mất khóa %d: %v", order, i, err)
				}
				if !bytes.Equal(got, v(i)) {
					t.Fatalf("%s: khóa %d = %q, muốn %q", order, i, got, v(i))
				}
			}
			noPins(t, pool, order)
			t.Logf("%s: height=%d leaves=%d branches=%d fill=%.1f%% splits=%d",
				order, r.Height, r.Leaves, r.Branches, r.LeafFill()*100, tr.Stats().Splits)
		})
	}
}

// TestDeleteEverythingShrinksBack: xóa hết thì cây phải TỤT về đúng trạng
// thái ban đầu (1 leaf, 0 khóa) và trả lại gần hết page cho freelist. Nếu
// merge/shrink sai thì cây vẫn "đúng" khi tra cứu nhưng để lại một cái xác
// nhiều tầng — chỉ test này bắt được.
func TestDeleteEverythingShrinksBack(t *testing.T) {
	const n = 5000
	tr, pool, db := newTree(t, 16)
	for i := 0; i < n; i++ {
		if err := tr.Put(k(i), v(i)); err != nil {
			t.Fatal(err)
		}
	}
	peak := db.LivePages()
	full := mustVerify(t, tr, "trước khi xóa")

	rng := rand.New(rand.NewSource(7))
	order := rng.Perm(n)
	for step, key := range order {
		if err := tr.Delete(k(key)); err != nil {
			t.Fatalf("Delete %d: %v", key, err)
		}
		if step%500 == 0 {
			mustVerify(t, tr, fmt.Sprintf("sau %d lần xóa", step))
		}
	}
	r := mustVerify(t, tr, "sau khi xóa hết")
	if r.Keys != 0 || r.Height != 1 || r.Leaves != 1 {
		d, _ := tr.Dump(4)
		t.Fatalf("xóa hết: keys=%d height=%d leaves=%d, muốn 0/1/1\n%s", r.Keys, r.Height, r.Leaves, d)
	}
	if got := db.LivePages(); got != 1 {
		t.Fatalf("xóa hết mà còn %d page sống (đỉnh %d) — merge làm rơi page", got, peak)
	}
	noPins(t, pool, "sau khi xóa hết")
	t.Logf("đỉnh %d page (height %d), xóa hết còn %d page; merges=%d redistributes=%d shrinks=%d",
		peak, full.Height, db.LivePages(), tr.Stats().Merges, tr.Stats().Redistributes, tr.Stats().Shrinks)
}

func TestDeleteMissingKey(t *testing.T) {
	tr, pool, _ := newTree(t, 8)
	for i := 0; i < 100; i++ {
		if err := tr.Put(k(i*2), v(i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tr.Delete(k(3)); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("Delete khóa không có = %v, muốn ErrKeyNotFound", err)
	}
	if tr.Stats().Deletes != 0 {
		t.Fatalf("Delete hụt vẫn đếm là một lần xóa")
	}
	mustVerify(t, tr, "sau Delete hụt")
	noPins(t, pool, "sau Delete hụt")
}

// TestPropertyRandomOps là deliverable của phase: N thao tác ngẫu nhiên
// (put/xóa/ghi đè trộn lẫn), sau mỗi loạt kiểm mọi bất biến, và cuối cùng so
// từng khóa với một map tham chiếu.
func TestPropertyRandomOps(t *testing.T) {
	// Hai hồ sơ, và sự khác nhau giữa chúng LÀ bài học:
	//
	//   ngắn: value 8-12 byte -> cell nhỏ so với page -> hạt chia mịn -> bất
	//         biến ">= 50%" đúng TUYỆT ĐỐI, đúng như sách giáo khoa nói.
	//   dài:  value 150-350 byte -> cell là 4-8% page -> nửa nhẹ sau split
	//         hụt tối đa một cell -> 50% BẤT KHẢ THI, sàn thật là
	//         (PageSize-maxCell)/2. Hồ sơ này cũng là hồ sơ duy nhất đẩy cây
	//         lên 3 tầng, tức là chỗ duy nhất merge/redistribute ở tầng
	//         branch được chạy.
	for _, prof := range []struct {
		name   string
		pad    int // độ dài đệm tối đa của value
		strict bool
		minH   int
	}{
		{"value ngắn", 0, true, 2},
		{"value dài", 200, false, 3},
	} {
		t.Run(prof.name, func(t *testing.T) { propertyRandomOps(t, prof.pad, prof.strict, prof.minH) })
	}
}

func propertyRandomOps(t *testing.T, pad int, strict bool, minHeight int) {
	const ops = 60000
	tr, pool, _ := newTree(t, 24)
	tr.RightmostSplit = false // tắt để bất biến độ đầy là TUYỆT ĐỐI
	model := map[string]string{}
	rng := rand.New(rand.NewSource(2026))

	// keyspace hẹp hơn số thao tác nhiều lần: cố tình để put và delete đụng
	// nhau liên tục, đó mới là chỗ merge/redistribute chạy.
	const keyspace = 8000
	underfullSeen := 0

	for i := 0; i < ops; i++ {
		key := rng.Intn(keyspace)
		switch {
		case rng.Intn(100) < 55: // 55% put
			val := fmt.Sprintf("v%d-%d", key, i)
			if pad > 0 {
				val += strings.Repeat("p", 150+key%pad)
			}
			if err := tr.Put(k(key), []byte(val)); err != nil {
				t.Fatalf("op %d: Put %d: %v", i, key, err)
			}
			model[string(k(key))] = val
		default: // 45% delete
			err := tr.Delete(k(key))
			_, had := model[string(k(key))]
			if had {
				if err != nil {
					t.Fatalf("op %d: Delete %d: %v", i, key, err)
				}
				delete(model, string(k(key)))
			} else if !errors.Is(err, ErrKeyNotFound) {
				t.Fatalf("op %d: Delete %d (không có) = %v", i, key, err)
			}
		}

		if i%2000 == 0 {
			r := mustVerify(t, tr, fmt.Sprintf("op %d", i))
			if r.Keys != len(model) {
				t.Fatalf("op %d: cây %d khóa, mô hình %d khóa", i, r.Keys, len(model))
			}
			// Sàn (PageSize-maxCell)/2 do B7 trong Verify() gác, và mustVerify
			// đã fail nếu vi phạm. Riêng hồ sơ value ngắn thì đòi thêm bất
			// biến chặt của sách giáo khoa: >= 50%, ngoại lệ duy nhất được
			// phép là những lần SkippedRebalance đã đếm.
			if len(r.Underfull) > 0 {
				underfullSeen++
				if strict && int64(len(r.Underfull)) > tr.Stats().SkippedRebalance {
					d, _ := tr.Dump(3)
					t.Fatalf("op %d: %d node dưới 50%% nhưng chỉ có %d lần SkippedRebalance: %v\n%s",
						i, len(r.Underfull), tr.Stats().SkippedRebalance, r.Underfull, d)
				}
			}
			noPins(t, pool, fmt.Sprintf("op %d", i))
		}
	}

	r := mustVerify(t, tr, "kết thúc")
	if r.Keys != len(model) {
		t.Fatalf("kết thúc: cây %d khóa, mô hình %d khóa", r.Keys, len(model))
	}
	for ks, want := range model {
		got, err := tr.Get([]byte(ks))
		if err != nil {
			t.Fatalf("mất khóa %x: %v", ks, err)
		}
		if string(got) != want {
			t.Fatalf("khóa %x = %q, muốn %q", ks, got, want)
		}
	}
	st := tr.Stats()
	// Chốt độ sâu: nếu một thay đổi nào đó làm cây tụt về 2 tầng thì merge và
	// redistribute ở tầng branch không còn chạy, và test này mất hết giá trị.
	if r.Height < minHeight {
		t.Fatalf("cây chỉ %d tầng, cần >= %d — merge/redistribute ở tầng branch không được chạm tới",
			r.Height, minHeight)
	}
	if st.Merges == 0 || st.Redistributes == 0 {
		t.Fatalf("merges=%d redistributes=%d — nửa khó của phase không hề chạy", st.Merges, st.Redistributes)
	}
	t.Logf("%d thao tác: %d khóa còn lại, height=%d fill=%.1f%%", ops, r.Keys, r.Height, r.LeafFill()*100)
	t.Logf("splits=%d merges=%d redistributes=%d shrinks=%d skipped=%d (số loạt thấy node <50%%: %d)",
		st.Splits, st.Merges, st.Redistributes, st.Shrinks, st.SkippedRebalance, underfullSeen)
	t.Logf("maxCell=%d byte -> sàn B7 = %.1f%%; node đặc ít nhất = %.1f%%; số node <50%% = %d",
		r.MaxCell, float64(page.PageSize-r.MaxCell)/2/page.PageSize*100, r.MinFill*100, len(r.Underfull))
	t.Logf("cặp leaf kề nhau cùng dưới nửa: %d cùng cha (gộp được mà chưa gộp) + %d khác cha (không gộp được)",
		r.MergeMissed, r.CrossParentPairs)
}

// TestVariableKeySize: khóa dài ngắn khác nhau là chỗ mọi giả định "đếm cell"
// vỡ. Fanout đổi theo từng page, midpoint phải tính theo BYTE.
func TestVariableKeySize(t *testing.T) {
	tr, pool, _ := newTree(t, 24)
	rng := rand.New(rand.NewSource(99))
	model := map[string]string{}
	for i := 0; i < 4000; i++ {
		n := 1 + rng.Intn(120)
		key := make([]byte, n)
		rng.Read(key)
		val := make([]byte, rng.Intn(200))
		rng.Read(val)
		if err := tr.Put(key, val); err != nil {
			t.Fatalf("Put khóa %d byte: %v", n, err)
		}
		model[string(key)] = string(val)
	}
	r := mustVerify(t, tr, "khóa biến độ dài")
	if r.Keys != len(model) {
		t.Fatalf("cây %d khóa, mô hình %d", r.Keys, len(model))
	}
	for ks, want := range model {
		got, err := tr.Get([]byte(ks))
		if err != nil {
			t.Fatalf("mất khóa %x: %v", ks, err)
		}
		if string(got) != want {
			t.Fatalf("khóa %x sai giá trị", ks)
		}
	}
	noPins(t, pool, "khóa biến độ dài")
	t.Logf("height=%d leaves=%d fill=%.1f%%", r.Height, r.Leaves, r.LeafFill()*100)
}

func TestEntryTooLarge(t *testing.T) {
	tr, pool, _ := newTree(t, 8)
	// Sát ngưỡng theo đúng công thức Put dùng: 2 (keyLen) + len(key) + len(val).
	// Đo lệch 2 byte thì test vẫn "xanh" mà không hề chạm vào biên thật.
	big := make([]byte, MaxEntrySize-2-1+1)
	if err := tr.Put([]byte("k"), big); !errors.Is(err, ErrEntryTooLarge) {
		t.Fatalf("Put entry quá lớn = %v, muốn ErrEntryTooLarge", err)
	}
	if err := tr.Put(nil, []byte("x")); !errors.Is(err, ErrEmptyKey) {
		t.Fatalf("Put khóa rỗng = %v, muốn ErrEmptyKey", err)
	}
	// Sát ngưỡng thì phải chèn được, và chèn được hai cái (nếu không thì
	// MaxEntrySize sai và split sẽ lặp vô hạn ở đâu đó).
	fit := make([]byte, MaxEntrySize-2-len(k(1)))
	if err := tr.Put(k(1), fit); err != nil {
		t.Fatalf("Put entry sát ngưỡng: %v", err)
	}
	if err := tr.Put(k(2), fit); err != nil {
		t.Fatalf("Put entry sát ngưỡng lần hai: %v", err)
	}
	mustVerify(t, tr, "entry sát ngưỡng")
	noPins(t, pool, "entry sát ngưỡng")
}

func TestCursorScanMatchesSortedOrder(t *testing.T) {
	const n = 3000
	tr, pool, _ := newTree(t, 16)
	rng := rand.New(rand.NewSource(5))
	for _, i := range rng.Perm(n) {
		if err := tr.Put(k(i), v(i)); err != nil {
			t.Fatal(err)
		}
	}
	// Quét toàn bộ: phải ra đúng thứ tự tăng dần và đủ n khóa.
	got := 0
	prev := []byte(nil)
	for c := tr.First(); c.Valid(); c.Next() {
		if prev != nil && bytes.Compare(prev, c.Key()) >= 0 {
			t.Fatalf("cursor ra khóa không tăng: %x rồi %x", prev, c.Key())
		}
		prev = append(prev[:0], c.Key()...)
		if !bytes.Equal(c.Key(), k(got)) {
			t.Fatalf("vị trí %d: khóa %x, muốn %x", got, c.Key(), k(got))
		}
		got++
	}
	if got != n {
		t.Fatalf("quét được %d khóa, muốn %d", got, n)
	}
	// Cursor không được giữ pin giữa hai lần Next — nếu giữ thì pool 16 frame
	// đã hết frame từ lâu.
	noPins(t, pool, "sau khi quét")

	// Seek là lower bound: khóa không tồn tại thì dừng ở khóa nhỏ nhất lớn hơn.
	c := tr.Seek(k(1500))
	if !c.Valid() || !bytes.Equal(c.Key(), k(1500)) {
		t.Fatalf("Seek khóa có thật hỏng")
	}
	if err := tr.Delete(k(1500)); err != nil {
		t.Fatal(err)
	}
	c = tr.Seek(k(1500))
	if !c.Valid() || !bytes.Equal(c.Key(), k(1501)) {
		t.Fatalf("Seek lower bound = %x, muốn %x", c.Key(), k(1501))
	}
	// Seek quá khóa lớn nhất thì hết.
	if c := tr.Seek(k(n + 10)); c.Valid() {
		t.Fatalf("Seek quá cuối vẫn Valid: %x", c.Key())
	}
}

func TestRange(t *testing.T) {
	tr, _, _ := newTree(t, 16)
	for i := 0; i < 2000; i++ {
		if err := tr.Put(k(i), v(i)); err != nil {
			t.Fatal(err)
		}
	}
	n := 0
	if err := tr.Range(k(500), k(700), func(key, val []byte) bool {
		if !bytes.Equal(key, k(500+n)) {
			t.Fatalf("Range vị trí %d: %x", n, key)
		}
		n++
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if n != 200 {
		t.Fatalf("Range [500,700) ra %d khóa, muốn 200", n)
	}
	// Trả false thì dừng ngay.
	n = 0
	if err := tr.Range(k(0), nil, func(key, val []byte) bool { n++; return n < 5 }); err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Fatalf("Range dừng sớm ra %d khóa, muốn 5", n)
	}
	c, err := tr.Count()
	if err != nil {
		t.Fatal(err)
	}
	if c != 2000 {
		t.Fatalf("Count = %d, muốn 2000", c)
	}
}

// TestTinyPool: pool chỉ vừa đủ đường root->leaf. Đây là bài kiểm tra kỷ luật
// pin — mọi chỗ quên Unpin đều biến thành ErrNoFrame ở đây.
func TestTinyPool(t *testing.T) {
	for _, frames := range []int{6, 8, 10} {
		t.Run(fmt.Sprintf("%dframe", frames), func(t *testing.T) {
			tr, pool, _ := newTree(t, frames)
			for i := 0; i < 20000; i++ {
				if err := tr.Put(k(i), v(i)); err != nil {
					t.Fatalf("pool %d frame, Put %d: %v", frames, i, err)
				}
			}
			r := mustVerify(t, tr, "pool nhỏ")
			if r.Keys != 20000 {
				t.Fatalf("cây %d khóa", r.Keys)
			}
			if r.Height+2 > frames {
				t.Logf("chú ý: cây cao %d tầng mà pool chỉ %d frame — vẫn chạy", r.Height, frames)
			}
			noPins(t, pool, "pool nhỏ")
			s := pool.Stats()
			t.Logf("pool %d frame: height=%d hit=%.1f%% evictions=%d",
				frames, r.Height, s.HitRatio()*100, s.Evictions)
		})
	}
}

// TestRightmostSplitFillsPages đo đúng cái mà phase này muốn chứng minh:
// cùng một dãy khóa tăng dần, bật tối ưu cực phải thì leaf đầy hơn hẳn.
func TestRightmostSplitFillsPages(t *testing.T) {
	const n = 50000
	fill := map[bool]float64{}
	pages := map[bool]int{}
	for _, on := range []bool{false, true} {
		tr, _, db := newTree(t, 32)
		tr.RightmostSplit = on
		for i := 0; i < n; i++ {
			if err := tr.Put(k(i), v(i)); err != nil {
				t.Fatal(err)
			}
		}
		r := mustVerify(t, tr, fmt.Sprintf("RightmostSplit=%v", on))
		fill[on], pages[on] = r.LeafFill(), db.LivePages()
		t.Logf("RightmostSplit=%-5v fill=%.1f%% leaves=%d pages=%d splits=%d",
			on, r.LeafFill()*100, r.Leaves, db.LivePages(), tr.Stats().Splits)
	}
	if fill[true] <= fill[false] {
		t.Fatalf("tối ưu cực phải không làm leaf đầy hơn: %.3f -> %.3f", fill[false], fill[true])
	}
	if pages[true] >= pages[false] {
		t.Fatalf("tối ưu cực phải không giảm số page: %d -> %d", pages[false], pages[true])
	}
}

// TestRoundTripThroughPager là bài kiểm tra "cây sống sót qua file thật":
// ghi -> FlushAll -> Commit(root) -> đóng -> mở lại -> đọc đủ.
func TestRoundTripThroughPager(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "btree.db")

	const n = 20000
	var root pager.PageID
	{
		pg, err := pager.Open(path)
		if err != nil {
			t.Fatalf("pager.New: %v", err)
		}
		pool := bufpool.New(pg, 64, bufpool.NewLRU(64))
		pool.Alloc = pg
		tr, err := Create(pool)
		if err != nil {
			t.Fatal(err)
		}
		tr.RightmostSplit = true
		for i := 0; i < n; i++ {
			if err := tr.Put(k(i), v(i)); err != nil {
				t.Fatalf("Put %d: %v", i, err)
			}
		}
		mustVerify(t, tr, "trước commit")
		if err := pool.FlushAll(); err != nil {
			t.Fatalf("FlushAll: %v", err)
		}
		root = tr.Root()
		if err := pg.Commit(root); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		if err := pg.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	pg, err := pager.Open(path)
	if err != nil {
		t.Fatalf("mở lại: %v", err)
	}
	defer pg.Close()
	pool := bufpool.New(pg, 64, bufpool.NewLRU(64))
	pool.Alloc = pg
	tr := Open(pool, root)
	r := mustVerify(t, tr, "sau khi mở lại")
	if r.Keys != n {
		t.Fatalf("mở lại: %d khóa, muốn %d", r.Keys, n)
	}
	for i := 0; i < n; i += 97 {
		got, err := tr.Get(k(i))
		if err != nil || !bytes.Equal(got, v(i)) {
			t.Fatalf("mở lại: khóa %d = %q, %v", i, got, err)
		}
	}
	t.Logf("%d khóa -> file %d byte (%d page, %.1f byte/khóa), height=%d fill=%.1f%%",
		n, fi.Size(), fi.Size()/page.PageSize, float64(fi.Size())/n, r.Height, r.LeafFill()*100)
}

// TestForgottenUnpinIsLoud không kiểm cây mà kiểm giao kèo với pool: FreePage
// một page đang pin phải là lỗi, không phải im lặng.
func TestFreePinnedIsError(t *testing.T) {
	db := NewMemDB()
	pool := bufpool.New(db, 4, bufpool.NewLRU(4))
	pool.Alloc = db
	f, err := pool.NewPage(page.TypeLeaf)
	if err != nil {
		t.Fatal(err)
	}
	id := f.PageID()
	if err := pool.FreePage(id); err == nil {
		t.Fatalf("FreePage một page đang pin=%d mà không báo lỗi", f.PinCount())
	}
	if err := pool.Unpin(id, true); err != nil {
		t.Fatal(err)
	}
	if err := pool.FreePage(id); err != nil {
		t.Fatalf("FreePage sau Unpin: %v", err)
	}
	if pool.Contains(id) {
		t.Fatalf("page %d vẫn nằm trong pool sau FreePage", id)
	}
}
