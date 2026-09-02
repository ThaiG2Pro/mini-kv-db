package btree

import (
	"fmt"
	"testing"
)

func ck(i int) []byte { return []byte(fmt.Sprintf("k%06d", i)) }

// TestCursorRestoresAfterSplit là bài test của nợ P4-5 ở phía NGƯỜI ĐỌC.
//
// Hình dựng ra: một cursor đứng giữa cây, rồi cây bị đổi cấu trúc dưới chân
// nó (chèn thêm cho tới khi split lan tới root), rồi cursor đi tiếp. Trước
// phase 7, (leaf, idx) mà cursor giữ là một cặp số vô nghĩa sau split — nó
// không báo lỗi, nó trả về khóa SAI, và đó là loại bug tệ nhất.
//
// Bài test khẳng định theo cả hai chiều: kết quả đúng, VÀ cơ chế đã thật sự
// chạy (Restores > 0). Không có nửa sau thì một cài đặt không làm gì cả vẫn
// xanh — bài học crashlab-nowrite của phase 5.
func TestCursorRestoresAfterSplit(t *testing.T) {
	tr, _, _ := newTree(t, 256)
	// Khóa chẵn trước, để chỗ cho khóa lẻ chèn vào giữa sau.
	for i := 0; i < 400; i += 2 {
		if err := tr.Put(ck(i), []byte("v")); err != nil {
			t.Fatal(err)
		}
	}
	c := tr.Seek(ck(100))
	if !c.Valid() || string(c.Key()) != string(ck(100)) {
		t.Fatalf("seek sai chỗ: %q", c.Key())
	}
	gen := tr.Gen()

	// Đổi cấu trúc: chèn 200 khóa lẻ, thừa sức bắt leaf đang đứng phải split.
	for i := 1; i < 400; i += 2 {
		if err := tr.Put(ck(i), []byte("v")); err != nil {
			t.Fatal(err)
		}
	}
	if tr.Gen() == gen {
		t.Fatal("cây đã đổi mà số đời không tăng — cursor sẽ không bao giờ biết")
	}

	// Đi tiếp: khóa kế phải là 101 (khóa lẻ vừa chèn), không phải 102.
	if !c.Next() {
		t.Fatalf("hết cursor sau khi cây đổi: err=%v", c.Err())
	}
	if got := string(c.Key()); got != string(ck(101)) {
		t.Fatalf("sau khi cây đổi, khóa kế = %q, mong %q", got, ck(101))
	}
	if c.Restores() != 1 {
		t.Fatalf("Restores = %d, mong đúng 1 — cơ chế tìm lại chỗ chưa chạy",
			c.Restores())
	}

	// Và đi hết phải ra đúng thứ tự tăng dần, không thiếu không lặp.
	n, last := 1, string(c.Key())
	for c.Next() {
		cur := string(c.Key())
		if cur <= last {
			t.Fatalf("thứ tự vỡ: %q sau %q", cur, last)
		}
		last = cur
		n++
	}
	if err := c.Err(); err != nil {
		t.Fatal(err)
	}
	if want := 400 - 101; n != want {
		t.Fatalf("đi được %d khóa từ 101 tới hết, mong %d", n, want)
	}
}

// TestCursorRestoresAfterDeleteOfOwnKey: khóa mà cursor đang đứng bị XÓA.
// Đây là ca mà "tìm lại chỗ theo khóa" phải xử lý khác: khóa cũ không còn
// tồn tại, nên seek(last) rơi vào khóa lớn hơn và KHÔNG được bỏ qua nó.
func TestCursorRestoresAfterDeleteOfOwnKey(t *testing.T) {
	tr, _, _ := newTree(t, 256)
	for i := 0; i < 100; i++ {
		if err := tr.Put(ck(i), []byte("v")); err != nil {
			t.Fatal(err)
		}
	}
	c := tr.Seek(ck(50))
	if err := tr.Delete(ck(50)); err != nil {
		t.Fatal(err)
	}
	if err := tr.Delete(ck(51)); err != nil {
		t.Fatal(err)
	}
	if !c.Next() {
		t.Fatalf("hết cursor: %v", c.Err())
	}
	if got := string(c.Key()); got != string(ck(52)) {
		t.Fatalf("khóa kế sau khi 50 và 51 bị xóa = %q, mong %q", got, ck(52))
	}
	if c.Restores() != 1 {
		t.Fatalf("Restores = %d, mong 1", c.Restores())
	}
}
