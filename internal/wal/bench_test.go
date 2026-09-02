package wal

import (
	"math/rand"
	"testing"

	"minidb/internal/page"
)

// Toàn bộ thiết kế physiological logging đứng trên một giả định: một lần chèn
// vào slotted page chỉ động vào VÀI vùng byte rời nhau (header, một slot, một
// cell), nên ghi từng vùng rẻ hơn hẳn ghi trọn 4KB. Benchmark này bắt giả
// định ấy phải tự chứng minh — và đo luôn cái giá phải trả để có nó.
//
// gran là chỗ đánh đổi: gran nhỏ thì đoạn khít hơn (ít byte log) nhưng phải
// so nhiều khối hơn (chậm hơn), gran lớn thì ngược lại.

// mkPair dựng cặp ảnh trước/sau của đúng một lần chèn vào một leaf đã có nhiều
// bản ghi — hình dạng thật mà journal gặp.
func mkPair(rng *rand.Rand, fill int) (before, after []byte) {
	p := page.New(1)
	rec := make([]byte, 48)
	for i := 0; i < fill; i++ {
		rng.Read(rec)
		if _, err := p.Insert(rec); err != nil {
			break
		}
	}
	before = append([]byte(nil), p...)
	rng.Read(rec)
	if _, err := p.Insert(rec); err != nil {
		panic(err)
	}
	return before, append([]byte(nil), p...)
}

func benchDiff(b *testing.B, gran int) {
	rng := rand.New(rand.NewSource(1))
	before, after := mkPair(rng, 40)
	var segs []Seg
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		segs = Diff(before, after, gran)
	}
	b.StopTimer()
	var n int
	for _, s := range segs {
		n += int(s.Len)
	}
	b.ReportMetric(float64(len(segs)), "seg")
	b.ReportMetric(float64(n), "dataB")
	// Tỉ số là con số đáng nhớ, không phải số byte tuyệt đối.
	b.ReportMetric(float64(page.PageSize)/float64(n), "x-vs-fullpage")
}

func BenchmarkDiffGran16(b *testing.B)  { benchDiff(b, 16) }
func BenchmarkDiffGran32(b *testing.B)  { benchDiff(b, 32) }
func BenchmarkDiffGran64(b *testing.B)  { benchDiff(b, 64) }
func BenchmarkDiffGran128(b *testing.B) { benchDiff(b, 128) }

// Diff trên hai page GIỐNG HỆT nhau: trường hợp journal gặp mỗi lần một page
// được pin để đọc rồi thả ra mà không đổi gì. Phải là đường nhanh nhất.
func BenchmarkDiffIdentical(b *testing.B) {
	rng := rand.New(rand.NewSource(1))
	before, _ := mkPair(rng, 40)
	same := append([]byte(nil), before...)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if len(Diff(before, same, DiffGran)) != 0 {
			b.Fatal("phải không có đoạn nào")
		}
	}
}

func BenchmarkEncodePayload(b *testing.B) {
	rng := rand.New(rand.NewSource(1))
	before, after := mkPair(rng, 40)
	segs := Diff(before, after, DiffGran)
	buf := make([]byte, 0, MaxRecordSize)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf = EncodePayload(buf[:0], segs, segs, before, after)
	}
	b.StopTimer()
	b.ReportMetric(float64(len(buf)), "payloadB")
}

// Ảnh-trọn-page tốn thêm bao nhiêu: cùng một thay đổi, phía redo ghi cả 4KB.
func BenchmarkEncodeFullPage(b *testing.B) {
	rng := rand.New(rand.NewSource(1))
	before, after := mkPair(rng, 40)
	segs := Diff(before, after, DiffGran)
	full := []Seg{{Off: 0, Len: uint16(page.PageSize)}}
	buf := make([]byte, 0, MaxRecordSize)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf = EncodePayload(buf[:0], segs, full, before, after)
	}
	b.StopTimer()
	b.ReportMetric(float64(len(buf)), "payloadB")
}

func BenchmarkApply(b *testing.B) {
	rng := rand.New(rand.NewSource(1))
	before, after := mkPair(rng, 40)
	segs := Diff(before, after, DiffGran)
	blob := EncodePayload(nil, segs, segs, before, after)
	_, sa, _, ab, err := DecodePayload(blob)
	if err != nil {
		b.Fatal(err)
	}
	dst := append([]byte(nil), before...)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := Apply(dst, sa, ab); err != nil {
			b.Fatal(err)
		}
	}
}
