package db

import (
	"path/filepath"
	"testing"
)

// FuzzCrashRecover: một chuỗi thao tác do fuzzer sinh, một điểm crash, rồi mở
// lại. Bất biến kiểm sau mỗi lần:
//
//  1. cây hợp lệ (bảy bất biến của phase 4);
//  2. đúng bằng tập khóa của các transaction ĐÃ commit — không thiếu (mất
//     durability) và không thừa (transaction dở dang sống sót).
//
// Fuzzer mạnh ở chỗ nó tự tìm ra những dãy mà mình không nghĩ ra: xóa hết một
// leaf ngay trước khi crash, ghi đè bằng value ngắn hơn làm node co lại rồi
// abort, split rồi merge lại trong cùng một transaction.
func FuzzCrashRecover(f *testing.F) {
	f.Add([]byte{1, 2, 3, 0, 4, 5, 9, 9, 7, 1, 2})
	f.Add([]byte{9, 9, 9, 1, 1, 1, 2, 2, 2, 3, 3, 3, 0, 0, 0})
	f.Add(make([]byte, 64))

	f.Fuzz(func(t *testing.T, script []byte) {
		if len(script) > 4000 {
			script = script[:4000]
		}
		dir := t.TempDir()
		path := filepath.Join(dir, "f.db")
		// Pool 6 frame: ép page bẩn ra đĩa liên tục, nên log của transaction
		// dở dang cũng bị kéo xuống đĩa và pha undo thật sự phải chạy.
		d, err := Open(path, Options{Frames: 6, CheckpointBytes: 8 << 10})
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]string{}
		var tx *Txn
		local := map[string]string{}
		del := map[string]bool{}

		commit := func() {
			if tx == nil {
				return
			}
			if err := tx.Commit(); err != nil {
				t.Fatalf("commit: %v", err)
			}
			for k := range del {
				delete(want, k)
			}
			for k, v := range local {
				want[k] = v
			}
			tx, local, del = nil, map[string]string{}, map[string]bool{}
		}
		abort := func() {
			if tx == nil {
				return
			}
			if err := tx.Abort(); err != nil {
				t.Fatalf("abort: %v", err)
			}
			tx, local, del = nil, map[string]string{}, map[string]bool{}
		}

		for i := 0; i < len(script); i++ {
			op := script[i]
			if tx == nil {
				if tx, err = d.Begin(); err != nil {
					t.Fatal(err)
				}
			}
			k := key(int(op) % 40)
			switch op % 5 {
			case 0:
				if err := tx.Delete(k); err == nil {
					del[string(k)] = true
					delete(local, string(k))
				}
			case 1, 2, 3:
				v := val(int(op), 10+int(op)%500)
				if err := tx.Put(k, v); err != nil {
					t.Fatalf("put: %v", err)
				}
				local[string(k)] = string(v)
				delete(del, string(k))
			case 4:
				if op%2 == 0 {
					commit()
				} else {
					abort()
				}
			}
		}
		// Transaction cuối cùng cố tình để dở: crash phải nuốt trọn nó.
		if err := d.SimulateCrash(); err != nil {
			t.Fatal(err)
		}

		d2, err := Open(path, Options{Frames: 6, CheckpointBytes: -1})
		if err != nil {
			t.Fatalf("mở lại: %v", err)
		}
		defer d2.Close()
		rep, err := d2.tree.Verify()
		if err != nil {
			t.Fatal(err)
		}
		if !rep.OK() {
			t.Fatalf("cây hỏng sau recovery: %v", rep.Errors)
		}
		got := map[string]string{}
		if err := d2.tree.Range(nil, nil, func(k, v []byte) bool {
			got[string(k)] = string(v)
			return true
		}); err != nil {
			t.Fatal(err)
		}
		if len(got) != len(want) {
			t.Fatalf("sau recovery có %d khóa, mong %d", len(got), len(want))
		}
		for k, v := range want {
			if got[k] != v {
				t.Fatalf("khóa %x sai value sau recovery", k)
			}
		}
	})
}
