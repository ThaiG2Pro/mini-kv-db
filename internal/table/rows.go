package table

import (
	"bytes"
	"fmt"

	"minidb/internal/keys"
	"minidb/internal/txn"
)

// Tx là một transaction ở tầng bảng: nó bọc txn.Txn và thêm việc duy trì
// index. Không có state nào ngoài bộ đếm, nên nó rẻ như một con trỏ.
type Tx struct {
	c  *Catalog
	tx *txn.Txn
	St Stat
}

// Stat đếm việc THẬT SỰ đã làm. Nó là thước đo của cả phase 7: một index scan
// và một seq scan trả về cùng kết quả, cái khác nhau nằm ở đây.
type Stat struct {
	IndexEntries int // số mục index đã đọc
	RowFetches   int // số lần đi tra bảng theo primary key
	RowsScanned  int // số hàng đã đọc từ bảng khi quét tuần tự
	RowsMatched  int // số hàng thoả điều kiện
	IndexWrites  int // số lần ghi/xoá một mục index (thuế của việc có index)
	RowWrites    int // số lần ghi/xoá một hàng
}

// Txn là transaction logic bên dưới — cần khi muốn tự đọc khóa thô.
func (t *Tx) Txn() *txn.Txn { return t.tx }

// Update chạy fn trong một transaction ghi.
func (c *Catalog) Update(l txn.Level, fn func(*Tx) error) error {
	return c.s.Update(l, func(tx *txn.Txn) error { return fn(&Tx{c: c, tx: tx}) })
}

// View chạy fn trong một transaction chỉ đọc.
func (c *Catalog) View(l txn.Level, fn func(*Tx) error) error {
	return c.s.View(l, func(tx *txn.Txn) error { return fn(&Tx{c: c, tx: tx}) })
}

// ---------- đọc ----------

// Get đọc một hàng theo primary key.
func (t *Tx) Get(sc *Schema, pk []keys.Value) ([]keys.Value, bool, error) {
	t.St.RowFetches++
	raw, ok, err := t.tx.Get(sc.RowKey(nil, pk))
	if err != nil || !ok {
		return nil, false, err
	}
	row, _, err := keys.Decode(raw, len(sc.Cols), nil)
	if err != nil {
		return nil, false, fmt.Errorf("table %s: hàng %v: %w", sc.Name, pk, err)
	}
	return row, true, nil
}

// ---------- ghi ----------

// Insert chèn một hàng mới. Trả ErrDuplicateKey nếu primary key đã có.
//
// Chú ý cái giá: một lần chèn là một lần ĐỌC (kiểm pk) cộng một lần ghi hàng
// cộng hai lần ghi cho mỗi index unique (kiểm rồi ghi) — chưa tính gì cả mà
// đã ba lần xuống cây. Đó là lý do mọi công cụ nạp dữ liệu hàng loạt đều
// khuyên tạo index SAU khi nạp.
func (t *Tx) Insert(sc *Schema, row []keys.Value) error {
	if err := sc.Check(row); err != nil {
		return err
	}
	pk := sc.PKOf(row)
	if _, ok, err := t.Get(sc, pk); err != nil {
		return err
	} else if ok {
		return fmt.Errorf("%w: primary key %v của bảng %s", ErrDuplicateKey, pk, sc.Name)
	}
	return t.write(sc, pk, nil, row)
}

// Upsert ghi một hàng, thay thế hàng cũ nếu có. Nó phải ĐỌC hàng cũ để biết
// mục index nào cần xoá — không có cách nào tránh, và đây là lý do sâu xa vì
// sao "UPDATE một cột không được index" vẫn tốn tiền khi bảng có nhiều index:
// hệ thống không biết cột nào đổi cho tới khi nó đã đọc bản cũ.
func (t *Tx) Upsert(sc *Schema, row []keys.Value) error {
	if err := sc.Check(row); err != nil {
		return err
	}
	pk := sc.PKOf(row)
	old, ok, err := t.Get(sc, pk)
	if err != nil {
		return err
	}
	if !ok {
		old = nil
	}
	return t.write(sc, pk, old, row)
}

// Delete xóa một hàng theo primary key.
func (t *Tx) Delete(sc *Schema, pk []keys.Value) error {
	old, ok, err := t.Get(sc, pk)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: %v", ErrNoRow, pk)
	}
	return t.write(sc, pk, old, nil)
}

// write là chỗ DUY NHẤT sửa hàng và index. Một điểm thực thi cho bất biến
// "mọi hàng có mặt đều có đúng một mục trong mỗi index, và không mục nào trỏ
// tới hàng không còn" — bài học của phase 5: một bất biến có hai chỗ thực thi
// là một bất biến có hai chỗ để lệch.
//
// old == nil: chèn mới. row == nil: xóa.
func (t *Tx) write(sc *Schema, pk []keys.Value, old, row []keys.Value) error {
	idxs := t.c.Indexes(sc.OID)
	pkOrd := sc.PKOrder()

	for _, ix := range idxs {
		var oldKey, newKey []byte
		if old != nil {
			oldKey = ix.EntryKey(nil, ix.IndexValsOf(old), pk, pkOrd)
		}
		if row != nil {
			newKey = ix.EntryKey(nil, ix.IndexValsOf(row), pk, pkOrd)
		}
		if bytes.Equal(oldKey, newKey) && old != nil && row != nil {
			// Giá trị được index không đổi -> mục index không đổi. Bỏ qua
			// chứ không ghi lại: ghi lại là thêm một version vào chuỗi của
			// khóa ấy, và với trần ~2KB của phase 6 (nợ P6-1) thì một cột
			// không đổi cũng đủ làm chuỗi đầy.
			continue
		}
		if oldKey != nil {
			if err := t.tx.Delete(oldKey); err != nil {
				return err
			}
			t.St.IndexWrites++
		}
		if newKey != nil {
			if ix.Unique {
				// Ràng buộc duy nhất TRONG transaction này: hai hàng khác pk
				// cùng giá trị index. Cùng lúc đó, xung đột GIỮA các
				// transaction được phase 6 lo — hai bên ghi cùng một khóa
				// cây nên first-committer-wins bắt được. Đây là chỗ hai cơ
				// chế gặp nhau, và là lý do khóa index unique không được chứa
				// pk.
				if cur, ok, err := t.tx.Get(newKey); err != nil {
					return err
				} else if ok {
					other, _, derr := keys.Decode(cur, len(sc.PK), pkOrd)
					if derr != nil {
						return fmt.Errorf("index %s: %w", ix.Name, derr)
					}
					if keys.CompareTuple(other, pk, pkOrd) != 0 {
						return fmt.Errorf("%w: index %s, giá trị %v đã thuộc hàng %v",
							ErrDuplicateKey, ix.Name, ix.IndexValsOf(row), other)
					}
				}
			}
			val := []byte(nil)
			if ix.Unique {
				val = keys.Encode(nil, pk, pkOrd)
			}
			if err := t.tx.Put(newKey, val); err != nil {
				return err
			}
			t.St.IndexWrites++
		}
	}

	rk := sc.RowKey(nil, pk)
	t.St.RowWrites++
	if row == nil {
		return t.tx.Delete(rk)
	}
	return t.tx.Put(rk, keys.Encode(nil, row, nil))
}
