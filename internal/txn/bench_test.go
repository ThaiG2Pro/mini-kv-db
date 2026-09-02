package txn

import (
	"fmt"
	"path/filepath"
	"testing"

	"minidb/internal/db"
)

// bench_test.go đo bốn cái giá của phase 6. Mỗi bench đều có một mốc để so —
// một con số ns/op đứng một mình không nói được gì.

func benchStore(b *testing.B) *Store {
	b.Helper()
	path := filepath.Join(b.TempDir(), "data.db")
	s, err := Open(path, db.Options{Frames: 256})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { s.Close() })
	return s
}

// BenchmarkGetPerLevel: giá của mỗi mức isolation trên đường ĐỌC.
//
// Dự đoán: repeatable-read rẻ nhất (snapshot chụp một lần ở Begin),
// read-committed đắt hơn (chụp lại mỗi câu lệnh, tức một lần lấy s.mu và một
// lần cấp map), serializable đắt nhất (một lần vào lock manager cho mỗi khóa).
func BenchmarkGetPerLevel(b *testing.B) {
	for _, l := range AllLevels {
		b.Run(l.String(), func(b *testing.B) {
			s := benchStore(b)
			for i := 0; i < 100; i++ {
				if err := seedKey(s, string(key(i)), "giá trị"); err != nil {
					b.Fatal(err)
				}
			}
			tx, err := s.Begin(l)
			if err != nil {
				b.Fatal(err)
			}
			defer tx.Abort()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, _, err := tx.Get(key(i % 100)); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkCommitPerLevel: giá của một transaction ghi một khóa, trọn vẹn từ
// Begin tới fsync. Mốc để so là BenchmarkInsertBatch1 của internal/db (phase
// 5): phần chênh chính là toàn bộ chi phí của tầng phase 6.
func BenchmarkCommitPerLevel(b *testing.B) {
	for _, l := range AllLevels {
		b.Run(l.String(), func(b *testing.B) {
			s := benchStore(b)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := s.Update(l, func(tx *Txn) error {
					return tx.Put(key(i%1000), []byte("v"))
				}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkAbort đo cái mà phase 6 mua được: abort không có I/O.
//
// So với BenchmarkAbort của internal/db (phase 5) — ở đó abort phải đọc ngược
// chuỗi undo, dán từng ảnh-trước và ghi một CLR cho mỗi page đã sửa. Ở đây nó
// là một map bị ném đi.
//
// keys=0 phải có, và nó là hàng đáng đọc nhất. Lần đo đầu cho keys=1 ra ~30µs
// — không hề "miễn phí" — và nguyên nhân KHÔNG phải abort: một transaction có
// GHI thì phải nhận một xid, và cứ 64 xid là một lần fsync để nới mốc trên
// (idBatch). 1/64 của một fsync trên máy này đúng cỡ 30µs. keys=0 là
// transaction chỉ đọc, không nhận xid, và đó mới là con số của riêng abort.
func BenchmarkAbort(b *testing.B) {
	for _, n := range []int{0, 1, 100} {
		b.Run(fmt.Sprintf("keys=%d", n), func(b *testing.B) {
			s := benchStore(b)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				tx, err := s.Begin(RepeatableRead)
				if err != nil {
					b.Fatal(err)
				}
				for j := 0; j < n; j++ {
					if err := tx.Put(key(j), []byte("v")); err != nil {
						b.Fatal(err)
					}
				}
				if err := tx.Abort(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkGetChainDepth là cái giá THẬT của phình version: đường đọc phải đi
// dọc chuỗi để tìm bản đầu tiên nhìn thấy được, nên mỗi version rác là thêm
// một bước.
//
// Đây là con số biện hộ cho vacuum. Nếu nó phẳng theo độ sâu thì vacuum chỉ
// tiết kiệm chỗ; nếu nó dốc thì vacuum còn là chuyện tốc độ.
//
// Và hai nhánh newest/oldest là bài đo phụ để tìm chi phí nằm ở ĐÂU. Nếu
// oldest đắt hơn newest thì chi phí nằm ở vòng lặp visibility. Nếu hai cái
// bằng nhau thì chi phí nằm ở DecodeChain — nó giải mã trọn chuỗi trước khi
// ai kịp hỏi version nào nhìn thấy được, nên không nhánh nào rẻ hơn. Lần đo
// đầu cho thấy đúng vế thứ hai, và đó là một món nợ có tên: giải mã lười.
func BenchmarkGetChainDepth(b *testing.B) {
	for _, depth := range []int{1, 8, 32, 60} {
		b.Run(fmt.Sprintf("depth=%d", depth), func(b *testing.B) {
			s := benchStore(b)
			if err := seedKey(s, "k", "v0"); err != nil {
				b.Fatal(err)
			}
			// Reader cũ ghim horizon để chuỗi mọc đúng độ sâu muốn đo.
			pin, err := s.Begin(RepeatableRead)
			if err != nil {
				b.Fatal(err)
			}
			defer pin.Abort()
			if _, _, err := pin.Get([]byte("k")); err != nil {
				b.Fatal(err)
			}
			for i := 1; i < depth; i++ {
				if err := seedKey(s, "k", fmt.Sprintf("v%d", i)); err != nil {
					b.Fatalf("độ sâu %d, lần %d: %v", depth, i, err)
				}
			}
			if cs, err := s.ChainStats(); err != nil {
				b.Fatal(err)
			} else if cs.MaxChain != depth {
				b.Fatalf("dựng được chuỗi dài %d, muốn %d", cs.MaxChain, depth)
			}

			// Đọc bằng snapshot MỚI: nó thấy ngay bản đầu chuỗi.
			b.Run("newest", func(b *testing.B) {
				tx, err := s.Begin(RepeatableRead)
				if err != nil {
					b.Fatal(err)
				}
				defer tx.Abort()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, _, err := tx.Get([]byte("k")); err != nil {
						b.Fatal(err)
					}
				}
			})
			// Đọc bằng snapshot CŨ: nó phải đi tới cuối chuỗi. Đây là ca xấu
			// nhất, và là ca mà một reader chạy lâu tự gây ra cho chính nó.
			b.Run("oldest", func(b *testing.B) {
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, _, err := pin.Get([]byte("k")); err != nil {
						b.Fatal(err)
					}
				}
			})
		})
	}
}

// BenchmarkScan: giá của range scan, gồm cả chi phí vật chất hoá mà Txn.Scan
// đang phải trả (nợ: chưa stream được).
func BenchmarkScan(b *testing.B) {
	s := benchStore(b)
	const n = 2000
	for i := 0; i < n; i++ {
		if err := seedKey(s, string(key(i)), "v"); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	b.ReportMetric(float64(n), "keys/scan")
	for i := 0; i < b.N; i++ {
		if err := s.View(RepeatableRead, func(tx *Txn) error {
			cnt, err := tx.Count(nil, nil)
			if err != nil {
				return err
			}
			if cnt != n {
				return fmt.Errorf("đếm ra %d, muốn %d", cnt, n)
			}
			return nil
		}); err != nil {
			b.Fatal(err)
		}
	}
}

func key(i int) []byte { return []byte(fmt.Sprintf("k%06d", i)) }
