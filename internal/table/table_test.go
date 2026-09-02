package table

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"minidb/internal/db"
	"minidb/internal/keys"
	"minidb/internal/txn"
)

func openCat(t *testing.T) *Catalog {
	t.Helper()
	s, err := txn.Open(filepath.Join(t.TempDir(), "data.db"), db.Options{Frames: 64})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	c, err := Load(s)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// users(id uint PK, city bytes, age int, name bytes)
func usersTable(t *testing.T, c *Catalog) *Schema {
	t.Helper()
	sc, err := c.CreateTable("users", []Column{
		{Name: "id", T: keys.TypeUint},
		{Name: "city", T: keys.TypeBytes},
		{Name: "age", T: keys.TypeInt},
		{Name: "name", T: keys.TypeBytes},
	}, []string{"id"})
	if err != nil {
		t.Fatal(err)
	}
	return sc
}

func row(id uint64, city string, age int64, name string) []keys.Value {
	return []keys.Value{keys.Uint(id), keys.Str(city), keys.Int(age), keys.Str(name)}
}

// TestIndexStaysConsistentWithRows là bất biến trung tâm của tầng bảng:
//
//	mọi hàng có mặt có ĐÚNG MỘT mục trong mỗi index, và không mục nào trỏ tới
//	một hàng không còn.
//
// Bài test bắt bất biến ấy sau một dãy chèn/sửa/xóa, và kiểm theo cả hai
// chiều (đếm hai bên, rồi đối chiếu từng mục). Chiều "không mục mồ côi" là
// chiều dễ vỡ: nó vỡ khi Upsert quên xoá mục cũ, và khi đó truy vấn qua index
// trả về NHIỀU hàng hơn sự thật — hoặc trả lỗi "mục trỏ tới hàng không tồn
// tại" nếu may.
func TestIndexStaysConsistentWithRows(t *testing.T) {
	c := openCat(t)
	sc := usersTable(t, c)
	if _, err := c.CreateIndex("users", "users_city", []string{"city"}, false); err != nil {
		t.Fatal(err)
	}
	ix, err := c.Index("users_city")
	if err != nil {
		t.Fatal(err)
	}

	cities := []string{"HN", "SG", "DN"}
	if err := c.Update(txn.RepeatableRead, func(tx *Tx) error {
		for i := 0; i < 60; i++ {
			if err := tx.Insert(sc, row(uint64(i), cities[i%3], int64(20+i%40), "n")); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Sửa: đổi city của 20 hàng -> mục index phải DI CHUYỂN.
	if err := c.Update(txn.RepeatableRead, func(tx *Tx) error {
		for i := 0; i < 20; i++ {
			if err := tx.Upsert(sc, row(uint64(i), "HCM", int64(20+i), "n2")); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Xóa 10 hàng.
	if err := c.Update(txn.RepeatableRead, func(tx *Tx) error {
		for i := 50; i < 60; i++ {
			if err := tx.Delete(sc, []keys.Value{keys.Uint(uint64(i))}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := c.View(txn.RepeatableRead, func(tx *Tx) error {
		rows := map[string][]keys.Value{}
		if err := tx.ScanRows(sc, nil, nil, func(pk, r []keys.Value) bool {
			rows[fmt.Sprint(pk[0])] = r
			return true
		}); err != nil {
			return err
		}
		if len(rows) != 50 {
			t.Fatalf("còn %d hàng, mong 50", len(rows))
		}
		nEntry := 0
		if err := tx.ScanIndex(ix, nil, nil, func(vals, pk []keys.Value) bool {
			nEntry++
			r, ok := rows[fmt.Sprint(pk[0])]
			if !ok {
				t.Errorf("mục index city=%v trỏ tới hàng %v không còn", vals[0], pk[0])
				return false
			}
			if keys.Compare(r[1], vals[0]) != 0 {
				t.Errorf("hàng %v có city=%v nhưng mục index nói %v", pk[0], r[1], vals[0])
				return false
			}
			return true
		}); err != nil {
			return err
		}
		if nEntry != len(rows) {
			t.Fatalf("%d mục index cho %d hàng", nEntry, len(rows))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestIndexScanOrderFollowsIndexNotPK: index scan trả về hàng theo thứ tự của
// CỘT ĐƯỢC INDEX, không theo primary key. Đó là lý do `ORDER BY city` miễn phí
// khi có index trên city, và là nửa còn lại của giá trị một index (nửa kia là
// lọc).
func TestIndexScanOrderFollowsIndexNotPK(t *testing.T) {
	c := openCat(t)
	sc := usersTable(t, c)
	if err := c.Update(txn.RepeatableRead, func(tx *Tx) error {
		// id tăng, city GIẢM -> hai thứ tự ngược nhau hoàn toàn.
		for i := 0; i < 10; i++ {
			if err := tx.Insert(sc, row(uint64(i), fmt.Sprintf("c%02d", 9-i), 30, "n")); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateIndex("users", "users_city", []string{"city"}, false); err != nil {
		t.Fatal(err)
	}
	ix, _ := c.Index("users_city")
	var got []string
	if err := c.View(txn.RepeatableRead, func(tx *Tx) error {
		return tx.ScanIndex(ix, nil, nil, func(vals, pk []keys.Value) bool {
			got = append(got, fmt.Sprintf("%s/%v", vals[0].B, pk[0]))
			return true
		})
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 10 {
		t.Fatalf("%d mục, mong 10", len(got))
	}
	if got[0] != "c00/9" || got[9] != "c09/0" {
		t.Fatalf("thứ tự index sai: %v", got)
	}
}

// TestUniqueIndexRejectsDuplicateInSameTxn: ràng buộc duy nhất bên trong một
// transaction. Đây là nửa dễ.
func TestUniqueIndexRejectsDuplicateInSameTxn(t *testing.T) {
	c := openCat(t)
	sc := usersTable(t, c)
	if _, err := c.CreateIndex("users", "users_name", []string{"name"}, true); err != nil {
		t.Fatal(err)
	}
	err := c.Update(txn.RepeatableRead, func(tx *Tx) error {
		if err := tx.Insert(sc, row(1, "HN", 30, "an")); err != nil {
			return err
		}
		return tx.Insert(sc, row(2, "SG", 31, "an"))
	})
	if !errors.Is(err, ErrDuplicateKey) {
		t.Fatalf("mong ErrDuplicateKey, được %v", err)
	}
}

// TestUniqueIndexConflictsAcrossTxns là bài test đáng nhất của package này.
//
// Hai transaction song song, mỗi bên chèn một hàng KHÁC pk nhưng cùng giá trị
// của một index unique. Không bên nào thấy bên kia (snapshot isolation), nên
// phép kiểm "đọc rồi ghi" của cả hai đều nói OK. Thứ chặn được là bộ phát
// hiện xung đột ghi-ghi của phase 6: khóa index unique KHÔNG chứa pk, nên cả
// hai ghi vào ĐÚNG MỘT khóa của cây, và first-committer-wins bắt được.
//
// Nói cách khác: ràng buộc duy nhất ở đây không cần cơ chế nào mới. Nó là hệ
// quả của việc chọn hình dạng khóa cho đúng. Và chiều ngược lại cũng đúng và
// đáng nhớ hơn: với index NON-unique thì hai bên ghi hai khóa khác nhau, nên
// không bao giờ có xung đột — đó là lý do index non-unique không bao giờ chặn
// được gì cả.
func TestUniqueIndexConflictsAcrossTxns(t *testing.T) {
	c := openCat(t)
	sc := usersTable(t, c)
	if _, err := c.CreateIndex("users", "users_name", []string{"name"}, true); err != nil {
		t.Fatal(err)
	}

	// Không dùng c.Update: nó tự thử lại khi gặp xung đột, mà ở đây xung đột
	// CHÍNH LÀ thứ cần quan sát.
	tx1, err := c.Store().Begin(txn.RepeatableRead)
	if err != nil {
		t.Fatal(err)
	}
	tx2, err := c.Store().Begin(txn.RepeatableRead)
	if err != nil {
		t.Fatal(err)
	}
	t1, t2 := &Tx{c: c, tx: tx1}, &Tx{c: c, tx: tx2}
	if err := t1.Insert(sc, row(1, "HN", 30, "an")); err != nil {
		t.Fatalf("tx1 chèn: %v", err)
	}
	if err := t2.Insert(sc, row(2, "SG", 31, "an")); err != nil {
		t.Fatalf("tx2 chèn: %v — hai bên KHÔNG được thấy nhau ở snapshot"+
			" isolation, nên phép kiểm trong transaction phải im lặng", err)
	}
	if err := tx1.Commit(); err != nil {
		t.Fatalf("tx1 commit: %v", err)
	}
	err = tx2.Commit()
	if !errors.Is(err, txn.ErrConflict) {
		t.Fatalf("tx2 commit = %v, mong ErrConflict — ràng buộc duy nhất phải"+
			" được bộ phát hiện xung đột của phase 6 thực thi", err)
	}
}

// TestNonUniqueIndexNeverConflicts là chiều phủ định của bài trên, và nó là
// chỗ để bảng anomaly không bị đọc sai: chèn hai hàng cùng giá trị index
// non-unique thì KHÔNG có xung đột, vì hai khóa cây khác nhau.
func TestNonUniqueIndexNeverConflicts(t *testing.T) {
	c := openCat(t)
	sc := usersTable(t, c)
	if _, err := c.CreateIndex("users", "users_city", []string{"city"}, false); err != nil {
		t.Fatal(err)
	}
	tx1, _ := c.Store().Begin(txn.RepeatableRead)
	tx2, _ := c.Store().Begin(txn.RepeatableRead)
	t1, t2 := &Tx{c: c, tx: tx1}, &Tx{c: c, tx: tx2}
	if err := t1.Insert(sc, row(1, "HN", 30, "a")); err != nil {
		t.Fatal(err)
	}
	if err := t2.Insert(sc, row(2, "HN", 31, "b")); err != nil {
		t.Fatal(err)
	}
	if err := tx1.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatalf("tx2 commit = %v, mong thành công: index non-unique thì hai"+
			" hàng khác pk là hai khóa cây khác nhau", err)
	}
}

// TestCatalogSurvivesReopen: lược đồ phải sống qua một lần đóng/mở, và index
// phải còn dùng được ngay — nếu không thì mọi index là một bộ nhớ đệm chứ
// không phải một cấu trúc dữ liệu.
func TestCatalogSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.db")
	opt := db.Options{Frames: 64}
	s, err := txn.Open(path, opt)
	if err != nil {
		t.Fatal(err)
	}
	c, err := Load(s)
	if err != nil {
		t.Fatal(err)
	}
	sc, err := c.CreateTable("users", []Column{
		{Name: "id", T: keys.TypeUint},
		{Name: "city", T: keys.TypeBytes},
		{Name: "age", T: keys.TypeInt},
		{Name: "name", T: keys.TypeBytes},
	}, []string{"id"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateIndex("users", "users_city", []string{"city"}, false); err != nil {
		t.Fatal(err)
	}
	if err := c.Update(txn.RepeatableRead, func(tx *Tx) error {
		return tx.Insert(sc, row(7, "HN", 30, "an"))
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := txn.Open(path, opt)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	c2, err := Load(s2)
	if err != nil {
		t.Fatal(err)
	}
	sc2, err := c2.Table("users")
	if err != nil {
		t.Fatal(err)
	}
	if sc2.OID != sc.OID || len(sc2.Cols) != 4 || len(sc2.PK) != 1 {
		t.Fatalf("lược đồ sau khi mở lại: %+v", sc2)
	}
	ix2, err := c2.Index("users_city")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	if err := c2.View(txn.RepeatableRead, func(tx *Tx) error {
		return tx.ScanIndex(ix2, []keys.Value{keys.Str("HN")}, []keys.Value{keys.Str("HO")},
			func(vals, pk []keys.Value) bool {
				n++
				if pk[0].U != 7 {
					t.Errorf("pk = %v, mong 7", pk[0])
				}
				return true
			})
	}); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("index trả %d mục sau khi mở lại, mong 1", n)
	}
}

// TestCheckRejectsBadRows: kiểu và NULL trong pk bị chặn ở MỘT chỗ.
func TestCheckRejectsBadRows(t *testing.T) {
	c := openCat(t)
	sc := usersTable(t, c)
	bad := map[string][]keys.Value{
		"thiếu cột":     {keys.Uint(1), keys.Str("HN")},
		"sai kiểu":      {keys.Str("1"), keys.Str("HN"), keys.Int(3), keys.Str("n")},
		"NULL trong pk": {keys.Null(), keys.Str("HN"), keys.Int(3), keys.Str("n")},
	}
	for name, r := range bad {
		if err := sc.Check(r); err == nil {
			t.Fatalf("%s: Check im lặng cho %v", name, r)
		}
	}
	// NULL ở cột thường thì HỢP LỆ, và phải index được.
	if err := sc.Check([]keys.Value{keys.Uint(1), keys.Null(), keys.Int(3), keys.Str("n")}); err != nil {
		t.Fatalf("NULL ở cột thường phải hợp lệ: %v", err)
	}
}

// TestConcurrentWritersKeepIndexConsistent: nhiều goroutine cùng ghi vào bảng
// có index, rồi kiểm lại bất biến hàng<->index. Đây là chỗ hai thứ gặp nhau:
// duy trì index cần ĐỌC bản cũ, nên nó là mẫu read-modify-write — chính cái
// mà phase 6 đã chứng minh là bị lost update ở ReadCommitted.
func TestConcurrentWritersKeepIndexConsistent(t *testing.T) {
	c := openCat(t)
	sc := usersTable(t, c)
	if _, err := c.CreateIndex("users", "users_city", []string{"city"}, false); err != nil {
		t.Fatal(err)
	}
	ix, _ := c.Index("users_city")

	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				id := uint64(w*25 + i)
				err := c.Update(txn.RepeatableRead, func(tx *Tx) error {
					return tx.Upsert(sc, row(id, fmt.Sprintf("c%d", int(id)%5), 30, "n"))
				})
				if err != nil {
					t.Errorf("upsert %d: %v", id, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	if err := c.View(txn.RepeatableRead, func(tx *Tx) error {
		nRow, nIdx := 0, 0
		if err := tx.ScanRows(sc, nil, nil, func(pk, r []keys.Value) bool { nRow++; return true }); err != nil {
			return err
		}
		if err := tx.ScanIndex(ix, nil, nil, func(vals, pk []keys.Value) bool { nIdx++; return true }); err != nil {
			return err
		}
		if nRow != 100 || nIdx != 100 {
			t.Fatalf("%d hàng, %d mục index — mong 100/100", nRow, nIdx)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestIndexScanSurvivesWriterMidScan lấp một LỖ PHỦ mà bài phản chứng của
// phase 7 phát hiện ra, chứ không phải một bug của code.
//
// Cách phát hiện: tắt cơ chế số đời cấu trúc (đổi `if c.gen != c.t.gen` thành
// `if false` trong internal/btree/cursor.go) rồi chạy lại cả bộ test. Hai tầng
// dưới đỏ ngay — cursor nhảy từ khóa 51 lên 101, mất đúng nửa leaf sau một
// split. Nhưng internal/txn, internal/table và internal/query thì VẪN XANH cả
// ba. Tức là toàn bộ bài test của phase 7 sẽ xanh với một cursor hỏng.
//
// Lý do rất đơn giản và cũng rất dễ mắc lại: mọi lần quét trong các bài test
// kia đều chạy một mình. Không có ai ghi vào cây giữa hai bước, nên số đời
// không bao giờ đổi, nên nhánh code cần kiểm không bao giờ chạy. Một bài test
// chỉ kiểm được cái mà nó làm cho XẢY RA.
//
// Bài test này ghi từ chính goroutine đang quét, giữa mọi bước. Nó cũng là
// bằng chứng của điều ngược lại với bài test đã treo 60s ở internal/db: ở đây
// mỗi bước chèn một hàng MỚI nằm PHÍA TRƯỚC cursor, mà lần quét vẫn kết thúc —
// vì ảnh chụp MVCC của phase 6 chốt chặn trên ngay lúc bắt đầu, nên mục vừa
// chèn không nhìn thấy được. Đó chính là cái mà tầng btree thuần không có.
func TestIndexScanSurvivesWriterMidScan(t *testing.T) {
	c := openCat(t)
	sc := usersTable(t, c)
	if _, err := c.CreateIndex("users", "users_city", []string{"city"}, false); err != nil {
		t.Fatal(err)
	}
	ix, _ := c.Index("users_city")

	const n = 200
	for i := 0; i < n; i++ {
		if err := c.Update(txn.RepeatableRead, func(tx *Tx) error {
			// city cách nhau 2 để writer còn chỗ chèn vào GIỮA.
			return tx.Upsert(sc, row(uint64(i), fmt.Sprintf("c%05d", 2*i), 30, "n"))
		}); err != nil {
			t.Fatal(err)
		}
	}

	seen := map[uint64]bool{}
	var last []keys.Value
	step := 0
	err := c.View(txn.RepeatableRead, func(tx *Tx) error {
		var inner error
		err := tx.ScanIndex(ix, nil, nil, func(vals, pk []keys.Value) bool {
			if last != nil && keys.CompareTuple(last, vals, ix.Desc) >= 0 {
				inner = fmt.Errorf("khóa index không tăng ngặt: %v sau %v", vals, last)
				return false
			}
			last = append(last[:0], vals...)
			seen[pk[0].U] = true
			step++
			// Chèn một hàng MỚI với city lẻ, tức nằm giữa hai mục đang được
			// quét: nó làm split đúng cái leaf mà cursor đang đứng.
			if inner = c.Update(txn.RepeatableRead, func(w *Tx) error {
				return w.Upsert(sc, row(uint64(n+step),
					fmt.Sprintf("c%05d", 2*step+1), 30, "n"))
			}); inner != nil {
				return false
			}
			return true
		})
		if err == nil {
			err = inner
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	// Mọi hàng có TRƯỚC lần quét phải xuất hiện. Với cơ chế gen bị tắt, con số
	// này tụt xuống vì cursor nhảy qua cả nửa leaf sau mỗi split.
	if len(seen) != n {
		t.Fatalf("thấy %d hàng có từ trước lần quét, mong đúng %d", len(seen), n)
	}
	// Và không được thấy hàng nào chèn SAU khi ảnh chụp mở: đây là vế chứng
	// minh lần quét hữu hạn nhờ snapshot, không nhờ may.
	for id := range seen {
		if id >= n {
			t.Fatalf("thấy hàng %d được chèn sau khi ảnh chụp mở", id)
		}
	}
	t.Logf("quét %d mục, chèn %d mục mới trong lúc quét", len(seen), step)
}
