package txn

import (
	"bytes"
	"math"
	"testing"
)

func TestChainRoundTrip(t *testing.T) {
	in := Chain{
		{Xmin: 300, Val: []byte("mới nhất")},
		{Xmin: 200, Deleted: true},
		{Xmin: 100, Val: bytes.Repeat([]byte("x"), 500)},
		{Xmin: 7, Val: nil},
	}
	buf := in.Encode(nil)
	if len(buf) != in.EncodedSize() {
		t.Fatalf("EncodedSize() = %d nhưng Encode ra %d byte", in.EncodedSize(), len(buf))
	}
	out, err := DecodeChain(buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != len(in) {
		t.Fatalf("giải mã ra %d version, muốn %d", len(out), len(in))
	}
	for i := range in {
		if out[i].Xmin != in[i].Xmin || out[i].Deleted != in[i].Deleted {
			t.Fatalf("version %d lệch: %+v vs %+v", i, out[i], in[i])
		}
		if in[i].Deleted {
			if out[i].Val != nil {
				t.Fatalf("version %d là tombstone mà vẫn mang %d byte", i, len(out[i].Val))
			}
			continue
		}
		if !bytes.Equal(out[i].Val, in[i].Val) {
			t.Fatalf("version %d lệch value", i)
		}
	}
}

func TestDecodeChainRejectsGarbage(t *testing.T) {
	good := Chain{{Xmin: 1, Val: []byte("abc")}}.Encode(nil)
	cases := map[string][]byte{
		"rỗng":                      {},
		"cụt giữa header":           good[:5],
		"cụt giữa value":            good[:len(good)-2],
		"nói 2 version mà chỉ có 1": append([]byte{2}, good[1:]...),
	}
	for name, b := range cases {
		if _, err := DecodeChain(b); err == nil {
			t.Errorf("%s: phải báo lỗi mà lại nhận", name)
		}
	}
	// Byte thừa ở đuôi cũng là hỏng: nó nghĩa là ai đó đã ghi vào value này
	// bằng một cách khác, và im lặng bỏ qua sẽ che mất bug đó.
	if _, err := DecodeChain(append(good, 0xff)); err == nil {
		t.Error("byte thừa ở đuôi phải báo lỗi")
	}
}

func TestSnapshotVisibility(t *testing.T) {
	// Snapshot chụp lúc next=10, txn 5 và 7 đang chạy.
	s := Snapshot{Xmax: 10, Xmin: 5, Active: map[uint64]bool{5: true, 7: true}}
	cases := []struct {
		xmin uint64
		want bool
		why  string
	}{
		{1, true, "commit từ lâu"},
		{4, true, "commit trước khi ta chụp"},
		{5, false, "đang chạy lúc ta chụp -> commit sau ta"},
		{6, true, "commit trước khi ta chụp, dù id nằm giữa hai kẻ đang chạy"},
		{7, false, "đang chạy lúc ta chụp"},
		{9, true, "commit trước khi ta chụp"},
		{10, false, "bắt đầu sau ta"},
		{99, false, "bắt đầu sau ta"},
	}
	for _, c := range cases {
		if got := s.Visible(c.xmin); got != c.want {
			t.Errorf("Visible(%d) = %v, muốn %v — %s", c.xmin, got, c.want, c.why)
		}
	}
	// Latest thấy mọi thứ.
	for _, x := range []uint64{1, 5, 10, math.MaxUint64 - 1} {
		if !Latest.Visible(x) {
			t.Errorf("Latest.Visible(%d) = false", x)
		}
	}
}

func TestChainVisiblePicksNewestVisible(t *testing.T) {
	c := Chain{
		{Xmin: 9, Val: []byte("v9")},
		{Xmin: 7, Val: []byte("v7")},
		{Xmin: 4, Val: []byte("v4")},
	}
	s := Snapshot{Xmax: 10, Xmin: 7, Active: map[uint64]bool{7: true}}
	v, ok := c.Visible(s)
	if !ok || string(v.Val) != "v9" {
		t.Fatalf("muốn v9, được %q ok=%v", v.Val, ok)
	}
	// Snapshot cũ hơn: 9 chưa tồn tại, 7 đang chạy -> phải rơi xuống v4.
	s2 := Snapshot{Xmax: 9, Xmin: 7, Active: map[uint64]bool{7: true}}
	v, ok = c.Visible(s2)
	if !ok || string(v.Val) != "v4" {
		t.Fatalf("muốn v4, được %q ok=%v", v.Val, ok)
	}
	// Tombstone mới nhất -> vẫn "thấy", nhưng là thấy sự vắng mặt.
	c2 := c.Prepend(Version{Xmin: 11, Deleted: true})
	v, ok = c2.Visible(Latest)
	if !ok || !v.Deleted {
		t.Fatalf("muốn thấy tombstone, được %+v ok=%v", v, ok)
	}
}

// TestPruneKeepsOneBelowHorizon là bài test cho cái bẫy đã ghi trong Prune:
// phải giữ ĐÚNG MỘT version dưới ngưỡng, không phải không giữ gì.
func TestPruneKeepsOneBelowHorizon(t *testing.T) {
	c := Chain{{Xmin: 20}, {Xmin: 15}, {Xmin: 8}, {Xmin: 5}, {Xmin: 3}}
	got, n := c.Prune(12)
	if len(got) != 3 || n != 2 {
		t.Fatalf("Prune(12) ra %d version (bỏ %d), muốn 3 (bỏ 2): %+v", len(got), n, got)
	}
	if got[2].Xmin != 8 {
		t.Fatalf("version dưới ngưỡng được giữ phải là 8, là %d", got[2].Xmin)
	}
	// Ngưỡng lớn hơn mọi thứ -> chỉ còn bản mới nhất.
	got, n = c.Prune(math.MaxUint64)
	if len(got) != 1 || got[0].Xmin != 20 || n != 4 {
		t.Fatalf("Prune(∞) = %+v (bỏ %d), muốn [20]", got, n)
	}
	// Ngưỡng 0 -> không bỏ gì, vì không có version nào dưới ngưỡng.
	if got, n := c.Prune(0); n != 0 || len(got) != len(c) {
		t.Fatalf("Prune(0) không được bỏ gì, bỏ %d", n)
	}
}

// TestPruneHorizonIsXminNotXmax dựng lại đúng ca ba transaction gối nhau mà
// comment trong Prune cảnh báo: nếu horizon lấy min(Xmax) = 10 thay vì
// min(Xmin) = 5 thì version 3 bị bỏ, và cả hai reader mất bản duy nhất chúng
// nhìn thấy được.
func TestPruneHorizonIsXminNotXmax(t *testing.T) {
	c := Chain{{Xmin: 5, Val: []byte("v5")}, {Xmin: 3, Val: []byte("v3")}}
	readerA := Snapshot{Xmax: 10, Xmin: 5, Active: map[uint64]bool{5: true}}
	readerB := Snapshot{Xmax: 12, Xmin: 5, Active: map[uint64]bool{5: true}}

	if got, _ := c.Prune(10); len(got) != 1 {
		t.Fatal("tiền đề của bài test sai") // horizon=Xmax bỏ mất v3
	}
	pruned, _ := c.Prune(5) // horizon = min(Xmin)
	for name, s := range map[string]Snapshot{"A": readerA, "B": readerB} {
		v, ok := pruned.Visible(s)
		if !ok || string(v.Val) != "v3" {
			t.Fatalf("reader %s sau khi dọn thấy %q (ok=%v), muốn v3", name, v.Val, ok)
		}
	}
}

func TestChainDead(t *testing.T) {
	if !(Chain{{Xmin: 3, Deleted: true}}).Dead(5) {
		t.Error("một tombstone dưới ngưỡng là rác, phải thu hồi được")
	}
	if (Chain{{Xmin: 7, Deleted: true}}).Dead(5) {
		t.Error("tombstone TRÊN ngưỡng vẫn có thể có reader cần thấy nó")
	}
	if (Chain{{Xmin: 3, Deleted: true}, {Xmin: 1}}).Dead(5) {
		t.Error("còn version cũ bên dưới thì chưa chết")
	}
	if (Chain{{Xmin: 3}}).Dead(5) {
		t.Error("version còn giá trị thì không phải rác")
	}
}
