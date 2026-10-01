package table

import (
	"fmt"
	"testing"

	"minidb/internal/keys"
	"minidb/internal/txn"
)

// TestRowsSurviveNext giữ hợp đồng của RowIter.PK/Row: hàng đã trả ra KHÔNG
// bị lần Next sau ghi đè. Hash join giữ build side, sort giữ cả bảng; cả hai
// đều sai lặng lẽ nếu hợp đồng này vỡ. Nợ P9-1 gom thân của mọi cột bytes vào
// một arena mỗi hàng; bài này đỏ nếu arena (hoặc mảng Value) bị dùng lại sang
// hàng sau. Tên có byte 0x00 để đi qua cả nhánh bỏ escape.
func TestRowsSurviveNext(t *testing.T) {
	c := openCat(t)
	sc := usersTable(t, c)
	const n = 300
	want := func(i int) []keys.Value {
		return row(uint64(i), fmt.Sprintf("city-%d", i%7), int64(i)-100, fmt.Sprintf("name\x00%d-%s", i, string(make([]byte, i%5))))
	}
	if err := c.Update(txn.RepeatableRead, func(tx *Tx) error {
		for i := 0; i < n; i++ {
			if err := tx.Insert(sc, want(i)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var pks, rows [][]keys.Value
	if err := c.View(txn.RepeatableRead, func(tx *Tx) error {
		it := tx.IterRows(sc, nil, nil)
		for it.Next() {
			pks, rows = append(pks, it.PK()), append(rows, it.Row())
		}
		return it.Close()
	}); err != nil {
		t.Fatal(err)
	}
	if len(rows) != n {
		t.Fatalf("quét được %d hàng, mong %d", len(rows), n)
	}
	// Kiểm SAU khi quét xong: mọi hàng giữ lại vẫn đúng.
	for i := range rows {
		if got, w := fmt.Sprint(rows[i]), fmt.Sprint(want(i)); got != w {
			t.Fatalf("hàng %d bị ghi đè sau khi quét:\n có  %s\n mong %s", i, got, w)
		}
		if pks[i][0].U != uint64(i) {
			t.Fatalf("pk của hàng %d bị ghi đè: %v", i, pks[i])
		}
	}
	// pk là slice cắt cap: append vào nó không được giẫm lên cột đầu của hàng.
	_ = append(pks[0], keys.Uint(999))
	if rows[0][0].U != 0 {
		t.Fatalf("append vào PK() ghi đè Row(): %v", rows[0])
	}
}
