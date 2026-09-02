package table

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"minidb/internal/db"
	"minidb/internal/keys"
	"minidb/internal/txn"
)

// FuzzTableIndex nối phase 7 vào phase 5 và 6: mọi hàng và mọi mục index đều
// chỉ là khóa/giá trị đi qua chuỗi version của phase 6, rồi qua WAL, checkpoint,
// pha redo và pha undo của phase 5 — không tầng nào biết bên trên có "index".
//
// Câu hỏi phải trả lời bằng thí nghiệm chứ không bằng suy luận: sau crash, có
// mục index nào trỏ tới hàng không còn, hay hàng nào thiếu mục index?
//
// Bất biến kiểm sau mỗi lần crash và ở cuối:
//
//  1. cây hợp lệ (bảy bất biến của phase 4);
//  2. mọi chuỗi version giải mã được (BadChains == 0 — bất biến của phase 6);
//  3. số mục index BẰNG số hàng, và mỗi mục trỏ tới một hàng CÓ đúng giá trị
//     đã được index. Đây là bất biến riêng của phase 7, và là chiều dễ vỡ:
//     nó vỡ khi một lần sửa quên xoá mục cũ, và khi đó truy vấn qua index trả
//     về NHIỀU hàng hơn sự thật.
func FuzzTableIndex(f *testing.F) {
	f.Add([]byte{1, 2, 3, 4, 5, 6, 7, 8})
	f.Add([]byte{9, 9, 4, 4, 7, 7, 1, 1, 5})
	f.Add(make([]byte, 40))

	f.Fuzz(func(t *testing.T, script []byte) {
		if len(script) > 400 {
			script = script[:400]
		}
		path := filepath.Join(t.TempDir(), "f.db")
		// Pool nhỏ: page bẩn bị ép ra đĩa liên tục nên pha undo thật sự phải
		// chạy, không chỉ pha redo. Cùng lý do như FuzzTxnCrash của phase 6.
		opt := db.Options{Frames: 8, CheckpointBytes: 16 << 10}
		s, err := txn.Open(path, opt)
		if err != nil {
			t.Fatal(err)
		}
		c, err := Load(s)
		if err != nil {
			t.Fatal(err)
		}
		sc, err := c.CreateTable("t", []Column{
			{Name: "id", T: keys.TypeUint},
			{Name: "a", T: keys.TypeInt},
			{Name: "b", T: keys.TypeBytes},
		}, []string{"id"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.CreateIndex("t", "t_a", []string{"a"}, false); err != nil {
			t.Fatal(err)
		}
		if _, err := c.CreateIndex("t", "t_b", []string{"b"}, true); err != nil {
			t.Fatal(err)
		}

		check := func(c *Catalog, sc *Schema) {
			t.Helper()
			rep, err := c.Store().DB().Tree().Verify()
			if err != nil {
				t.Fatal(err)
			}
			if !rep.OK() {
				t.Fatalf("cây hỏng: %v", rep.Errors)
			}
			cs, err := c.Store().ChainStats()
			if err != nil {
				t.Fatal(err)
			}
			if cs.BadChains != 0 {
				t.Fatalf("%d/%d chuỗi version giải mã ra rác", cs.BadChains, cs.Keys)
			}
			if err := c.View(txn.RepeatableRead, func(tx *Tx) error {
				rows := map[uint64][]keys.Value{}
				if err := tx.ScanRows(sc, nil, nil, func(pk, row []keys.Value) bool {
					rows[pk[0].U] = row
					return true
				}); err != nil {
					return err
				}
				for _, name := range []string{"t_a", "t_b"} {
					ix, err := c.Index(name)
					if err != nil {
						return err
					}
					col := ix.Cols[0]
					n := 0
					var inner error
					if err := tx.ScanIndex(ix, nil, nil, func(vals, pk []keys.Value) bool {
						n++
						row, ok := rows[pk[0].U]
						if !ok {
							inner = fmt.Errorf("index %s: mục %v trỏ tới hàng %v không còn",
								name, vals[0], pk[0].U)
							return false
						}
						if keys.Compare(row[col], vals[0]) != 0 {
							inner = fmt.Errorf("index %s: hàng %v có %v nhưng mục nói %v",
								name, pk[0].U, row[col], vals[0])
							return false
						}
						return true
					}); err != nil {
						return err
					}
					if inner != nil {
						return inner
					}
					if n != len(rows) {
						return fmt.Errorf("index %s có %d mục cho %d hàng", name, n, len(rows))
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}

		reopen := func() {
			if err := s.DB().SimulateCrash(); err != nil {
				t.Fatal(err)
			}
			if s, err = txn.Open(path, opt); err != nil {
				t.Fatalf("mở lại: %v", err)
			}
			if c, err = Load(s); err != nil {
				t.Fatalf("đọc lại catalog: %v", err)
			}
			if sc, err = c.Table("t"); err != nil {
				t.Fatalf("bảng t biến mất sau recovery: %v", err)
			}
			check(c, sc)
		}

		for i := 0; i < len(script); i++ {
			op := script[i]
			id := uint64(op) % 12
			// b là cột UNIQUE: giá trị phải phụ thuộc id, nếu không thì mọi
			// lần chèn thứ hai đều vi phạm ràng buộc và fuzz không đi đâu cả.
			row := []keys.Value{keys.Uint(id), keys.Int(int64(op) % 5),
				keys.Str(fmt.Sprintf("b%02d-%d", id, int(op)%3))}
			switch op % 6 {
			case 0, 1, 2:
				err := c.Update(txn.RepeatableRead, func(tx *Tx) error {
					return tx.Upsert(sc, row)
				})
				if err != nil && !isDup(err) {
					t.Fatalf("upsert: %v", err)
				}
			case 3:
				err := c.Update(txn.RepeatableRead, func(tx *Tx) error {
					return tx.Delete(sc, []keys.Value{keys.Uint(id)})
				})
				if err != nil && !isNoRow(err) {
					t.Fatalf("delete: %v", err)
				}
			case 4:
				if _, err := s.Vacuum(); err != nil {
					t.Fatalf("vacuum: %v", err)
				}
			case 5:
				reopen()
			}
		}
		reopen()
		if err := s.Close(); err != nil {
			t.Fatalf("đóng: %v", err)
		}
	})
}

func isDup(err error) bool   { return errors.Is(err, ErrDuplicateKey) }
func isNoRow(err error) bool { return errors.Is(err, ErrNoRow) }
