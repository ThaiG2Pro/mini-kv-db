package bufpool

import (
	"fmt"
	"testing"

	"minidb/internal/pager"
)

// BenchmarkPinHit: chi phí của một lần trúng pool. Đây là con số phải đem so
// với 69µs (pread 4KB cache lạnh, phase 0) để biết buffer pool đáng giá bao
// nhiêu — và so với 1µs (cache nóng) để biết ta có tự làm chậm mình không.
func BenchmarkPinHit(b *testing.B) {
	for _, r := range []Replacer{NewLRU(64), NewClock(64), NewLRUK(64, 2)} {
		b.Run(r.Name(), func(b *testing.B) {
			p := New(NewMemStore(128), 64, r)
			if _, err := p.Pin(7); err != nil {
				b.Fatal(err)
			}
			if err := p.Unpin(7, false); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				f, err := p.Pin(7)
				if err != nil {
					b.Fatal(err)
				}
				_ = f
				if err := p.Unpin(7, false); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkPinMiss: miss + đuổi một nạn nhân SẠCH, store trong RAM. Đây là chi
// phí thuần của cơ chế thay thế, chưa tính I/O.
func BenchmarkPinMiss(b *testing.B) {
	const frames = 64
	for _, mk := range []func() Replacer{
		func() Replacer { return NewLRU(frames) },
		func() Replacer { return NewClock(frames) },
		func() Replacer { return NewLRUK(frames, 2) },
	} {
		r := mk()
		b.Run(r.Name(), func(b *testing.B) {
			p := New(NewMemStore(1<<16), frames, r)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				id := pager.PageID(i % (1 << 16))
				if _, err := p.Pin(id); err != nil {
					b.Fatal(err)
				}
				if err := p.Unpin(id, false); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			if p.Stats().Hits > p.Stats().Misses/10 {
				b.Fatalf("quá nhiều hit (%+v) — bench này phải toàn miss", p.Stats())
			}
		})
	}
}

// BenchmarkReplayZipf: chi phí trung bình một thao tác trên workload thật.
// ns/op ở đây trộn cả hit lẫn miss theo đúng tỉ lệ của workload.
func BenchmarkReplayZipf(b *testing.B) {
	const (
		pages  = 2000
		frames = 64
	)
	trace := Zipf(pages, 100000, 1.05, 3)
	for _, mk := range []func() Replacer{
		func() Replacer { return NewLRU(frames) },
		func() Replacer { return NewClock(frames) },
		func() Replacer { return NewLRUK(frames, 2) },
	} {
		name := mk().Name()
		b.Run(name, func(b *testing.B) {
			var ratio float64
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				p := New(NewMemStore(pages), frames, mk())
				if err := Replay(p, trace, 0); err != nil {
					b.Fatal(err)
				}
				ratio = p.Stats().HitRatio()
			}
			b.ReportMetric(float64(len(trace)), "ops/iter")
			b.ReportMetric(ratio, "hit-ratio")
		})
	}
}

// BenchmarkVictim: riêng chi phí CHỌN nạn nhân, không có gì khác. LRU-K quét
// toàn bộ mảng frame nên nó phải xấu đi theo số frame — bench này để xem xấu
// tới mức nào so với một lần miss thật.
func BenchmarkVictim(b *testing.B) {
	for _, frames := range []int{64, 1024} {
		for _, mk := range []func(int) Replacer{
			NewLRU, NewClock, func(n int) Replacer { return NewLRUK(n, 2) },
		} {
			r := mk(frames)
			b.Run(fmt.Sprintf("%s/%d", r.Name(), frames), func(b *testing.B) {
				for i := 0; i < frames; i++ {
					r.Access(i)
					r.Unpin(i)
				}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					v, ok := r.Victim()
					if !ok {
						b.Fatal("hết ứng viên")
					}
					r.Access(v)
					r.Unpin(v)
				}
			})
		}
	}
}

// BenchmarkPinHitParallel: cùng một page nóng, nhiều goroutine. Toàn bộ chi phí
// ở đây là tranh nhau p.mu — latch của BẢNG TRA. Con số này là lý do DB thật
// chia bảng tra thành nhiều mảnh (sharded/partitioned page table).
func BenchmarkPinHitParallel(b *testing.B) {
	for _, mk := range []func() Replacer{
		func() Replacer { return NewLRU(64) },
		func() Replacer { return NewClock(64) },
	} {
		r := mk()
		b.Run(r.Name(), func(b *testing.B) {
			p := New(NewMemStore(128), 64, r)
			if _, err := p.Pin(7); err != nil {
				b.Fatal(err)
			}
			if err := p.Unpin(7, false); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					f, err := p.Pin(7)
					if err != nil {
						b.Error(err)
						return
					}
					f.Latch.RLock()
					_ = f.Data[0]
					f.Latch.RUnlock()
					if err := p.Unpin(7, false); err != nil {
						b.Error(err)
						return
					}
				}
			})
		})
	}
}
