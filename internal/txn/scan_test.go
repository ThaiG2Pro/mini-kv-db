package txn

import (
	"fmt"
	"testing"
)

func sk(i int) []byte { return []byte(fmt.Sprintf("k%05d", i)) }

// TestScanKeepsSnapshotDespiteConcurrentCommit là bài test đáng giá nhất của
// phần trả nợ P6-4, vì nó khẳng định đúng cái lý lẽ cho phép nhả latch.
//
// Hình: một Scan ở RepeatableRead đang đi giữa khoảng thì một transaction khác
// commit một giá trị mới cho một khóa NẰM PHÍA TRƯỚC nó. Scan nhả latch giữa
// hai bước nên nó CHẮC CHẮN thấy cái cây đã đổi. Nhưng nó phải trả về giá trị
// CŨ, vì snapshot của nó chụp trước đó.
//
// Nói cách khác: cái latch mà phase 4 phải giữ suốt lần duyệt được tháo ra nhờ
// một cơ chế của phase 6 (chuỗi version + luật visibility), không nhờ một cơ
// chế của phase 4. Nếu bài test này đỏ thì việc nhả latch là sai, không phải
// việc scan là sai.
func TestScanKeepsSnapshotDespiteConcurrentCommit(t *testing.T) {
	s, _ := openStore(t)
	const n = 200
	for i := 0; i < n; i++ {
		mustPut(t, s, RepeatableRead, string(sk(i)), "old")
	}

	tx, err := s.Begin(RepeatableRead)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Abort()

	changed := false
	got := 0
	err = tx.Scan(nil, nil, func(k, v []byte) bool {
		got++
		if got == 5 && !changed {
			changed = true
			// Commit thật, ở transaction khác, vào một khóa phía TRƯỚC.
			mustPut(t, s, RepeatableRead, string(sk(150)), "new")
		}
		if string(v) != "old" {
			t.Errorf("khóa %q trả về %q — snapshot bị rò", k, v)
			return false
		}
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != n {
		t.Fatalf("scan thấy %d khóa, mong %d", got, n)
	}
	if !changed {
		t.Fatal("không có commit nào chen vào: bài test chưa kiểm được gì")
	}
}

// TestScanCallbackCanRead: callback của Scan gọi lại được Get. Đây là điều
// kiện cần của index scan (duyệt index -> tra bảng theo primary key), và là
// thứ bản materialize cũ vô tình cho được còn bản stream phải cố ý giữ.
func TestScanCallbackCanRead(t *testing.T) {
	s, _ := openStore(t)
	for i := 0; i < 50; i++ {
		mustPut(t, s, RepeatableRead, string(sk(i)), fmt.Sprintf("v%d", i))
	}
	n := 0
	err := s.View(RepeatableRead, func(tx *Txn) error {
		return tx.Scan(nil, nil, func(k, _ []byte) bool {
			v, ok, err := tx.Get(sk(0))
			if err != nil || !ok || string(v) != "v0" {
				t.Errorf("đọc lồng nhau: v=%q ok=%v err=%v", v, ok, err)
				return false
			}
			n++
			return true
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 50 {
		t.Fatalf("duyệt %d khóa, mong 50", n)
	}
}

// TestScanMergesWriteSetInOrder: bản stream là một phép trộn hai dòng đã sắp,
// nên thứ tự là chỗ dễ vỡ nhất. Ba ca trong một: khóa chỉ có trong cây, khóa
// chỉ có trong write set (chèn giữa), khóa có ở cả hai (write set thắng), và
// khóa bị xóa trong write set (phải biến mất).
func TestScanMergesWriteSetInOrder(t *testing.T) {
	s, _ := openStore(t)
	for _, i := range []int{10, 20, 30, 40} {
		mustPut(t, s, RepeatableRead, string(sk(i)), "tree")
	}
	err := s.Update(RepeatableRead, func(tx *Txn) error {
		if err := tx.Put(sk(25), []byte("ws")); err != nil { // chèn giữa
			return err
		}
		if err := tx.Put(sk(30), []byte("ws")); err != nil { // ghi đè
			return err
		}
		if err := tx.Delete(sk(40)); err != nil { // xóa
			return err
		}
		if err := tx.Put(sk(50), []byte("ws")); err != nil { // sau cùng
			return err
		}
		var order []string
		if err := tx.Scan(nil, nil, func(k, v []byte) bool {
			order = append(order, fmt.Sprintf("%s=%s", k, v))
			return true
		}); err != nil {
			return err
		}
		want := []string{
			"k00010=tree", "k00020=tree", "k00025=ws", "k00030=ws", "k00050=ws",
		}
		if len(order) != len(want) {
			t.Fatalf("scan ra %v, mong %v", order, want)
		}
		for i := range want {
			if order[i] != want[i] {
				t.Fatalf("vị trí %d: %q, mong %q (cả dãy %v)", i, order[i], want[i], order)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestScanStopsEarly: fn trả false thì Scan dừng NGAY, không đọc hết khoảng.
// Với bản materialize thì "dừng" chỉ là dừng việc gọi fn — cả khoảng đã nằm
// trong RAM từ trước rồi. Giá của khác biệt ấy đo ở BenchmarkScanLimit.
func TestScanStopsEarly(t *testing.T) {
	s, _ := openStore(t)
	for i := 0; i < 500; i++ {
		mustPut(t, s, RepeatableRead, string(sk(i)), "v")
	}
	n := 0
	err := s.View(RepeatableRead, func(tx *Txn) error {
		return tx.Scan(nil, nil, func(_, _ []byte) bool { n++; return n < 3 })
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("fn được gọi %d lần, mong 3", n)
	}
}
