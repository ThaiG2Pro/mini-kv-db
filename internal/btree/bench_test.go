package btree

import (
	"encoding/binary"
	"math/rand"
	"testing"

	"minidb/internal/bufpool"
	"minidb/internal/page"
)

// benchKeys sinh dãy khóa 16 byte theo hai kiểu:
//
//	seq  = auto-increment (8 byte 0 + 8 byte đếm tăng, big-endian)
//	rand = UUIDv4 giả (16 byte ngẫu nhiên)
//
// Cùng độ dài khóa, cùng độ dài giá trị, cùng số thao tác — khác đúng MỘT
// thứ: thứ tự. Nếu không giữ độ dài khóa bằng nhau thì chênh lệch fanout sẽ
// trộn vào và không kết luận được gì về thứ tự chèn.
func benchKeys(n int, sequential bool, seed int64) [][]byte {
	keys := make([][]byte, n)
	rng := rand.New(rand.NewSource(seed))
	for i := range keys {
		b := make([]byte, 16)
		if sequential {
			binary.BigEndian.PutUint64(b[8:], uint64(i))
		} else {
			rng.Read(b)
		}
		keys[i] = b
	}
	return keys
}

// benchInsert đo chèn n khóa và báo cáo các chỉ số cấu trúc theo từng khóa.
// Đơn vị là "trên mỗi khóa" chứ không phải tổng: tổng thì không so được giữa
// hai lần chạy có b.N khác nhau.
func benchInsert(b *testing.B, sequential, rightmost bool, frames int) {
	val := make([]byte, 100)
	keys := benchKeys(b.N, sequential, 1)
	db := NewMemDB()
	pool := bufpool.New(db, frames, bufpool.NewLRU(frames))
	pool.Alloc = db
	tr, err := Create(pool)
	if err != nil {
		b.Fatal(err)
	}
	tr.RightmostSplit = rightmost

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := tr.Put(keys[i], val); err != nil {
			b.Fatalf("Put %d: %v", i, err)
		}
	}
	b.StopTimer()

	st := tr.Stats()
	r, err := tr.Verify()
	if err != nil {
		b.Fatal(err)
	}
	if !r.OK() {
		b.Fatalf("cây hỏng sau bench: %v", r.Errors)
	}
	ps := pool.Stats()
	b.ReportMetric(float64(st.Splits)/float64(b.N)*1000, "splits/1k")
	b.ReportMetric(r.LeafFill()*100, "%fill")
	b.ReportMetric(float64(db.LivePages())/float64(b.N)*1000, "pages/1k")
	b.ReportMetric(float64(ps.Writes)/float64(b.N), "writes/op")
	b.ReportMetric(float64(r.Height), "height")
}

func BenchmarkInsertSequential(b *testing.B) { benchInsert(b, true, true, 512) }

// Cùng cây, cùng số khóa, tắt tối ưu cực phải: phần chênh với bench trên là
// giá trị đúng của một dòng if trong split().
func BenchmarkInsertSequentialNoOpt(b *testing.B) { benchInsert(b, true, false, 512) }

// Khóa ngẫu nhiên: mỗi lần chèn rơi vào một leaf bất kỳ. Đây là kịch bản
// UUIDv4 làm khóa chính.
func BenchmarkInsertRandom(b *testing.B) { benchInsert(b, false, true, 512) }

// Pool 64 frame (nhỏ hơn cây rất nhiều) — chỗ khác biệt giữa hai thứ tự chèn
// chuyển từ "tốn chỗ" sang "tốn I/O".
func BenchmarkInsertSequentialSmallPool(b *testing.B) { benchInsert(b, true, true, 64) }
func BenchmarkInsertRandomSmallPool(b *testing.B)     { benchInsert(b, false, true, 64) }

func benchGet(b *testing.B, sequential bool, frames int) {
	const n = 200000
	val := make([]byte, 100)
	keys := benchKeys(n, sequential, 3)
	db := NewMemDB()
	pool := bufpool.New(db, frames, bufpool.NewLRU(frames))
	pool.Alloc = db
	tr, _ := Create(pool)
	tr.RightmostSplit = true
	for _, key := range keys {
		if err := tr.Put(key, val); err != nil {
			b.Fatal(err)
		}
	}
	pool.ResetStats()
	rng := rand.New(rand.NewSource(4))

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := tr.Get(keys[rng.Intn(n)]); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	ps := pool.Stats()
	b.ReportMetric(float64(ps.Reads)/float64(b.N), "reads/op")
	b.ReportMetric(ps.HitRatio()*100, "%hit")
}

// Tra cứu điểm trên cây đã dựng sẵn: reads/op chính là "một lần Get chạm mấy
// page trên đĩa" — con số mà cả kiến trúc B+Tree tồn tại để giữ cho nhỏ.
func BenchmarkGetHotPool(b *testing.B)  { benchGet(b, true, 4096) }
func BenchmarkGetColdPool(b *testing.B) { benchGet(b, true, 32) }

func BenchmarkScan(b *testing.B) {
	const n = 200000
	val := make([]byte, 100)
	db := NewMemDB()
	pool := bufpool.New(db, 256, bufpool.NewLRU(256))
	pool.Alloc = db
	tr, _ := Create(pool)
	tr.RightmostSplit = true
	for _, key := range benchKeys(n, true, 5) {
		if err := tr.Put(key, val); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	got := 0
	for i := 0; i < b.N; i++ {
		for c := tr.First(); c.Valid(); c.Next() {
			got++
		}
	}
	b.StopTimer()
	// Đơn vị hữu ích của quét là "mỗi khóa bao nhiêu ns", không phải mỗi lần
	// quét — b.N ở đây là số lần quét cả cây.
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(got), "ns/key")
}

// BenchmarkSearchInPage tách riêng phần binary search trong một page khỏi mọi
// chi phí I/O và pin: nó cho biết trần tốc độ của tầng node.
func BenchmarkSearchInPage(b *testing.B) {
	db := NewMemDB()
	pool := bufpool.New(db, 4, bufpool.NewLRU(4))
	pool.Alloc = db
	f, err := pool.NewPage(page.TypeLeaf)
	if err != nil {
		b.Fatal(err)
	}
	n := node{f.Data}
	val := make([]byte, 8)
	keys := benchKeys(4096, true, 6)
	cnt := 0
	var buf [4096]byte
	for _, key := range keys {
		cell := encodeLeaf(buf[:0], key, val)
		if err := n.p.InsertAt(page.SlotID(cnt), cell); err != nil {
			break
		}
		cnt++
	}
	b.Logf("page chứa %d khóa 16 byte", cnt)
	rng := rand.New(rand.NewSource(7))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := n.search(keys[rng.Intn(cnt)]); !ok {
			b.Fatal("không tìm thấy khóa vừa chèn")
		}
	}
}

// BenchmarkGetPool* quét kích thước pool để tách hai thứ hay bị lẫn: reads/op
// (số lần chạm tầng lưu trữ — thứ B+Tree tồn tại để giữ nhỏ) và ns/op. Với
// MemDB, một "read" chỉ là memcpy 4KB nên ns/op ở đây KHÔNG phải số đo I/O; nó
// đo footprint cache CPU của mảng frame (32 frame = 128KB lọt L2, 4096 frame =
// 16MB vượt L3 12MB của máy này). Trên đĩa thật một read ~100µs sẽ nuốt trọn
// mọi hiệu ứng cache, và chỉ còn reads/op có ý nghĩa. Xem nợ P4-6.
func BenchmarkGetPool32(b *testing.B)   { benchGet(b, true, 32) }
func BenchmarkGetPool128(b *testing.B)  { benchGet(b, true, 128) }
func BenchmarkGetPool512(b *testing.B)  { benchGet(b, true, 512) }
func BenchmarkGetPool2048(b *testing.B) { benchGet(b, true, 2048) }
func BenchmarkGetPool4096(b *testing.B) { benchGet(b, true, 4096) }
func BenchmarkGetPool8192(b *testing.B) { benchGet(b, true, 8192) }

// BenchmarkCopyVsAlloc tách hai thứ mà profiler gộp làm một (nợ P6-2, blog bài
// 13): Get cũ cấp phát rồi chép cả chuỗi version (~900 byte), và pprof tính
// hết vào runtime.memmove vì memmove là người đầu tiên chạm vào vùng nhớ mới.
// Đo trên máy đo: alloc+copy 890–1865 ns, chỉ copy 18–27 ns. Cái đắt là lần
// cấp phát, không phải phép chép — lý do GetFunc tồn tại.
var copySink []byte

func BenchmarkCopyVsAlloc(b *testing.B) {
	src := make([]byte, 4096) // một page
	b.Run("alloc+copy", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			out := make([]byte, 897)
			copy(out, src[1000:1897])
			copySink = out
		}
	})
	b.Run("copy", func(b *testing.B) {
		out := make([]byte, 897)
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			copy(out, src[1000:1897])
		}
		copySink = out
	})
}
