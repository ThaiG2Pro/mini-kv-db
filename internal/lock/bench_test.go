package lock

import (
	"fmt"
	"testing"
)

// Cái giá của lock manager, và chỗ nó sẽ gãy khi hệ thống lớn lên.
//
// conflicts() quét TOÀN BỘ bảng lock đang giữ cho mỗi lần xin — O(số
// transaction × số lock mỗi transaction). Đó là một lựa chọn có ý thức (không
// có bảng băm theo đối tượng, không có hàng đợi theo đối tượng) và bench này
// tồn tại để đo xem lựa chọn ấy chịu được tới đâu.

func BenchmarkAcquireUncontended(b *testing.B) {
	m := New()
	k := Key([]byte("k"))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		txn := uint64(i + 1)
		if err := m.Acquire(txn, k, X); err != nil {
			b.Fatal(err)
		}
		m.ReleaseAll(txn)
	}
}

// BenchmarkAcquireShared: N transaction cùng giữ S trên cùng một khóa. Mỗi
// lần xin phải quét qua N holder để kết luận "không xung đột".
func BenchmarkAcquireShared(b *testing.B) {
	for _, n := range []int{1, 16, 256} {
		b.Run(fmt.Sprintf("holders=%d", n), func(b *testing.B) {
			m := New()
			k := Key([]byte("k"))
			for i := 0; i < n; i++ {
				if err := m.Acquire(uint64(i+1), k, S); err != nil {
					b.Fatal(err)
				}
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				txn := uint64(n + i + 1)
				if err := m.Acquire(txn, k, S); err != nil {
					b.Fatal(err)
				}
				m.ReleaseAll(txn)
			}
		})
	}
}

// BenchmarkAcquireDisjoint: N transaction giữ lock trên N khóa KHÁC nhau. Lý
// tưởng thì chi phí phải phẳng theo N (chúng không liên quan gì tới nhau);
// thực tế thì nó dốc, vì không có chỉ mục theo đối tượng. Tỉ số giữa
// holders=256 và holders=1 chính là cái giá của việc thiếu chỉ mục ấy.
func BenchmarkAcquireDisjoint(b *testing.B) {
	for _, n := range []int{1, 16, 256} {
		b.Run(fmt.Sprintf("holders=%d", n), func(b *testing.B) {
			m := New()
			for i := 0; i < n; i++ {
				if err := m.Acquire(uint64(i+1), Key([]byte(fmt.Sprintf("k%06d", i))), X); err != nil {
					b.Fatal(err)
				}
			}
			mine := Key([]byte("zzz"))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				txn := uint64(n + i + 1)
				if err := m.Acquire(txn, mine, X); err != nil {
					b.Fatal(err)
				}
				m.ReleaseAll(txn)
			}
		})
	}
}

// BenchmarkOverlaps là phép so hình học thuần, không có latch: mốc dưới cho
// mọi con số ở trên.
func BenchmarkOverlaps(b *testing.B) {
	a := Span([]byte("aaaaaa"), []byte("mmmmmm"))
	c := Key([]byte("ffffff"))
	b.ResetTimer()
	n := 0
	for i := 0; i < b.N; i++ {
		if a.Overlaps(c) {
			n++
		}
	}
	if n != b.N {
		b.Fatal("phép so sai")
	}
}
