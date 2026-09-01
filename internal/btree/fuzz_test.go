package btree

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"testing"

	"minidb/internal/bufpool"
)

// runTreeProgram diễn giải prog thành một chuỗi Put/Delete rồi đối chiếu cây
// với một map tham chiếu, đồng thời gọi Verify() để bắt lỗi cấu trúc.
//
// Tách khỏi Fuzz vì bài học phase 2: khi fuzz tìm ra corpus, phải chạy lại
// được nó bằng một test thường thì mới gỡ được.
func runTreeProgram(t *testing.T, prog []byte, frames int, rightmost bool) {
	t.Helper()
	db := NewMemDB()
	pool := bufpool.New(db, frames, bufpool.NewLRU(frames))
	pool.Alloc = db
	tr, err := Create(pool)
	if err != nil {
		t.Fatal(err)
	}
	tr.RightmostSplit = rightmost
	model := map[string]string{}

	// Khóa lấy từ chính prog nhưng ép về không gian hẹp: fuzz mà mỗi thao tác
	// một khóa mới thì chỉ chèn, không bao giờ chạm tới merge.
	for i := 0; i+2 < len(prog); i += 3 {
		op, kb, vlen := prog[i], prog[i+1], int(prog[i+2])
		key := []byte{kb, kb ^ 0x5a}
		if op&0x20 != 0 { // thỉnh thoảng dùng khóa dài để đổi fanout
			key = bytes.Repeat(key, 1+int(op%40))
		}
		switch op % 3 {
		case 0, 1:
			val := bytes.Repeat([]byte{kb}, vlen%300)
			if err := tr.Put(key, val); err != nil {
				t.Fatalf("Put(%x, %d byte): %v", key, len(val), err)
			}
			model[string(key)] = string(val)
		case 2:
			err := tr.Delete(key)
			if _, had := model[string(key)]; had {
				if err != nil {
					t.Fatalf("Delete(%x): %v", key, err)
				}
				delete(model, string(key))
			} else if !errors.Is(err, ErrKeyNotFound) {
				t.Fatalf("Delete(%x) khóa không có = %v", key, err)
			}
		}
		if n := pool.PinnedCount(); n != 0 {
			t.Fatalf("sau thao tác %d: còn %d frame bị pin", i/3, n)
		}
	}

	r, err := tr.Verify()
	if err != nil {
		t.Fatal(err)
	}
	if !r.OK() {
		d, _ := tr.Dump(3)
		st := tr.Stats()
		t.Fatalf("cây hỏng: %v\nstats: %+v\n%s", r.Errors, st, d)
	}
	if r.Keys != len(model) {
		t.Fatalf("cây %d khóa, mô hình %d khóa", r.Keys, len(model))
	}
	// KHÔNG đòi ">= 50%" ở đây: fuzz sinh value tới 300 byte, mà một cell 300
	// byte là 7% page, nên nửa nhẹ sau split hụt được tới 7% — 50% là bất khả
	// thi về nguyên tắc, không phải do bug. Hai thứ thay thế nó đều mạnh hơn
	// và đều do Verify() gác ở trên: B7 (sàn (PageSize-maxCell)/2) và B8 (hai
	// leaf kề nhau không được cùng dưới nửa). Bất biến 50% chặt vẫn được kiểm
	// ở TestPropertyRandomOps/value_ngắn, nơi cell đủ nhỏ để nó đúng.
	for ks, want := range model {
		got, err := tr.Get([]byte(ks))
		if err != nil {
			t.Fatalf("mất khóa %x: %v", ks, err)
		}
		if string(got) != want {
			t.Fatalf("khóa %x sai giá trị", ks)
		}
	}

	// Cursor phải đi qua đúng tập khóa đó, đúng thứ tự.
	want := make([]string, 0, len(model))
	for ks := range model {
		want = append(want, ks)
	}
	sort.Strings(want)
	i := 0
	for c := tr.First(); c.Valid(); c.Next() {
		if i >= len(want) {
			t.Fatalf("cursor ra thừa khóa %x", c.Key())
		}
		if string(c.Key()) != want[i] {
			t.Fatalf("cursor vị trí %d = %x, muốn %x", i, c.Key(), want[i])
		}
		i++
	}
	if i != len(want) {
		t.Fatalf("cursor ra %d khóa, muốn %d", i, len(want))
	}
}

func FuzzTreeOps(f *testing.F) {
	f.Add([]byte{0, 1, 10, 0, 2, 10, 2, 1, 0})
	f.Add(bytes.Repeat([]byte{0, 7, 200}, 64))
	// Chèn tăng dần rồi xóa ngược — chuỗi hay làm vỡ merge với anh em trái.
	seed := make([]byte, 0, 3*128)
	for i := 0; i < 128; i++ {
		seed = append(seed, 0, byte(i), 100)
	}
	for i := 127; i >= 0; i-- {
		seed = append(seed, 2, byte(i), 0)
	}
	f.Add(seed)

	f.Fuzz(func(t *testing.T, prog []byte) {
		if len(prog) > 4096 {
			prog = prog[:4096]
		}
		for _, rightmost := range []bool{false, true} {
			t.Run(fmt.Sprintf("rightmost=%v", rightmost), func(t *testing.T) {
				runTreeProgram(t, prog, 8, rightmost)
			})
		}
	})
}

// TestTreeProgramSeeds chạy đúng thân của fuzz trên vài chuỗi cố định — để
// `go test` thường cũng đi qua đường này, không chỉ khi chạy -fuzz.
func TestTreeProgramSeeds(t *testing.T) {
	prog := make([]byte, 0, 3*512)
	for i := 0; i < 256; i++ {
		prog = append(prog, byte(i%3), byte(i*7), byte(i))
	}
	for _, frames := range []int{6, 8, 32} {
		runTreeProgram(t, prog, frames, false)
		runTreeProgram(t, prog, frames, true)
	}
}
