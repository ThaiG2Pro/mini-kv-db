# Bài 2 — Một hàng nằm ở đâu?

> Series [Mở nắp database](README.md) · bài 2/11 · cần đọc trước: [bài 0](00-ban-do.md)

Ở bài 0 ta đã thấy một bảng là một file, file chia thành các page 8KB, và Postgres gắn cho mỗi
hàng một địa chỉ `ctid = (page, ô)`. Bài này mở hẳn một page ra, xem bên trong nó trông thế nào,
và trả lời ba câu hỏi mà ai dùng Postgres lâu cũng từng thắc mắc:

- Sao `UPDATE` một hàng lại làm `ctid` của nó đổi?
- Sao `DELETE` rồi mà file không nhỏ đi?
- `VACUUM` thật ra làm gì bên trong page?

## Thí nghiệm

Postgres có sẵn extension `pageinspect` để đọc thẳng các byte của một page. Tạo một bảng 3 hàng
(tắt autovacuum để nó không dọn hộ ta giữa chừng) rồi soi:

```bash
reallab/q.sh pg blog/lab/02-page-pg.sql
```

**1. Header của page:**

```text
 lower | upper | special | pagesize
-------+-------+---------+----------
    36 |  8088 |    8192 |     8192
```

**2. Mảng con trỏ:**

```text
 lp | lp_off | lp_len | t_xmin | t_xmax | t_ctid
----+--------+--------+--------+--------+--------
  1 |   8160 |     31 | 263651 |      0 | (0,1)
  2 |   8120 |     33 | 263651 |      0 | (0,2)
  3 |   8088 |     32 | 263651 |      0 | (0,3)
```

Đầu page là một header 24 byte, tiếp theo là một **mảng con trỏ** (line pointer, `lp`). Mỗi con
trỏ 4 byte cho biết hàng tương ứng nằm ở byte nào (`lp_off`) và dài bao nhiêu (`lp_len`). Ba
hàng được đặt từ **cuối** page ngược lên: 8160, 8120, 8088. `lower = 36` là chỗ mảng con trỏ kết
thúc (24 + 3×4), `upper = 8088` là chỗ vùng dữ liệu bắt đầu. Khoảng ở giữa là chỗ trống.

```text
  byte 0                 36                          8088                    8192
  +--------+-------------+---------------------------+------+------+------+
  | header | lp1 lp2 lp3 |          trống            | hàng3| hàng2| hàng1|
  +--------+-------------+---------------------------+------+------+------+
             mọc sang phải →                  ← mọc sang trái
```

Hai vùng mọc ngược chiều nhau, gặp nhau ở giữa thì page đầy. Thiết kế này tên là **slotted page**,
và gần như mọi database lưu theo page đều dùng nó: Postgres, SQLite, InnoDB, SQL Server.

**3. `UPDATE` một hàng:**

```text
 ctid  | id | name                     ctid  | id |   name
-------+----+------        UPDATE →   -------+----+----------
 (0,2) |  2 | binh                     (0,4) |  2 | binh moi

 lp | lp_off | lp_len | t_xmin | t_xmax | t_ctid
----+--------+--------+--------+--------+--------
  1 |   8160 |     31 | 263651 |      0 | (0,1)
  2 |   8120 |     33 | 263651 | 263652 | (0,4)     ← bản cũ: bị đánh dấu "chết từ transaction 263652", trỏ sang bản mới
  3 |   8088 |     32 | 263651 |      0 | (0,3)
  4 |   8048 |     37 | 263652 |      0 | (0,4)     ← bản mới: một hàng HOÀN TOÀN MỚI ở ô số 4
```

Postgres **không sửa hàng tại chỗ**. Nó ghi một bản mới (ô 4), đánh dấu bản cũ bằng `t_xmax` (số
của transaction đã "xoá" nó), và cho bản cũ trỏ sang bản mới. Vì vậy `ctid` đổi từ `(0,2)` sang
`(0,4)`. Lý do phải giữ bản cũ: một transaction khác đang chạy có thể vẫn cần nhìn thấy
`'binh'`. Đó là MVCC, chủ đề của bài 6 và bài 7.

**4. `DELETE` một hàng:**

```text
 lp | lp_off | lp_len | t_xmin | t_xmax | t_ctid
----+--------+--------+--------+--------+--------
  3 |   8088 |     32 | 263651 | 263653 | (0,3)     ← vẫn nằm nguyên đó, chỉ có thêm t_xmax
```

`DELETE` cũng không xoá gì cả, nó chỉ đánh dấu. Byte của hàng vẫn nằm nguyên trong page. Đó là lý
do `DELETE` một nửa bảng không làm file nhỏ đi.

**5. `VACUUM`:**

```text
 lp | lp_off | lp_len | lp_flags | t_xmin | t_xmax | t_ctid
----+--------+--------+----------+--------+--------+--------
  1 |   8160 |     31 |        1 | 263651 |      0 | (0,1)
  2 |      4 |      0 |        2 |        |        |          ← REDIRECT: "sang ô 4 mà tìm"
  3 |      0 |      0 |        0 |        |        |          ← UNUSED: ô trống, dùng lại được
  4 |   8120 |     37 |        1 | 263652 |      0 | (0,4)    ← đã DỜI từ byte 8048 sang 8120
```

Đây là chỗ đáng xem nhất. `VACUUM` làm ba việc trong page:

- **Dọn hai xác** (bản cũ của hàng 2 và hàng 3 đã xoá).
- **Dồn các hàng còn sống lại** cho liền nhau (*compact*): hàng `binh moi` được dời từ byte 8048
  sang 8120.
- Nhưng **không đổi số ô của hàng nào**. `binh moi` vẫn là `(0,4)`.

Tại sao phải giữ số ô? Vì có những thứ bên ngoài page đang trỏ vào nó. Index khoá chính vẫn ghi
hàng `id = 2` ở `(0,2)`:

```text
 itemoffset | ctid  |          data
------------+-------+-------------------------
          1 | (0,1) | 01 00 00 00 00 00 00 00
          2 | (0,2) | 02 00 00 00 00 00 00 00
```

Index chưa hề được cập nhật khi ta `UPDATE`. Postgres gọi đây là **HOT update** (heap-only
tuple): bản mới nằm cùng page với bản cũ và cột bị đổi không nằm trong index nào, nên không cần
sửa index. Muốn vậy thì ô số 2 phải sống tiếp dưới dạng một biển chỉ đường (`REDIRECT → 4`) để
index tìm được đường sang bản mới.

## Bên trong: vì sao phải gián tiếp?

Cái mảng con trỏ ở đầu page tồn tại vì đúng một lý do: **để dời được hàng mà không làm hỏng địa
chỉ của nó**. Ai ở bên ngoài (một index, một con trỏ) chỉ cần nhớ `(page, số ô)`. Hàng thật nằm
ở byte nào trong page là chuyện riêng của page, và page được tự do sắp xếp lại.

minidb cài đúng cấu trúc này ở `internal/page/page.go`. Comment đầu file nói hết:

```go
//	+--------+------------------+--------------+--------------------+
//	| header | slot0 slot1 ...  |  ...trống... | ... cell1  cell0   |
//	+--------+------------------+--------------+--------------------+
//	         ^ mọc sang phải                    ^ cellStart, mọc sang trái
//
//  1. Record được trỏ tới **gián tiếp** qua slot. Compact dồn cell lại làm đổi
//     offset, nhưng chỉ số slot thì không đổi -> con trỏ từ bên ngoài
//     (tuple id = (PageID, SlotID), ví dụ từ secondary index) vẫn đúng.
//  2. Xóa không rút mảng slot lại, chỉ đánh dấu slot chết. Rút lại sẽ làm mọi
//     SlotID phía sau tụt đi một -> hỏng toàn bộ con trỏ ngoài.
```

Và đây là phép compact, cùng việc mà `VACUUM` vừa làm với hàng `binh moi` (bỏ bớt phần phụ):

```go
func (p Page) Compact() {
	// gom các ô còn sống, sắp theo offset giảm dần
	for i := 0; i < p.NumSlots(); i++ {
		if off, _ := p.slot(i); off != deadOffset {
			order = append(order, uint32(off)<<16|uint32(i))
		}
	}
	slices.SortFunc(order, func(a, b uint32) int { return int(b>>16) - int(a>>16) })

	dst := PageSize
	for _, v := range order {
		i := int(v & 0xffff)
		off, ln := p.slot(i)
		dst -= ln
		if dst != off {
			copy(p[dst:dst+ln], p[off:off+ln]) // dời byte của hàng
			p.setSlot(i, dst, ln)              // sửa con trỏ; SỐ Ô i giữ nguyên
		}
	}
	p.setU16(offCellStart, uint16(dst))
}
```

Hàng bị dời (`copy`), con trỏ được sửa (`setSlot`), còn số ô `i` thì không đổi. Postgres làm
đúng như vậy trong hàm `PageRepairFragmentation`.

Còn một chi tiết nữa trong minidb đáng để ý: khi chèn hàng mới, nó **không** dùng lại ô đã chết.

```go
// Slot mới luôn nối vào **cuối** mảng: không tái dùng slot chết. Tái dùng sẽ
// làm một tuple id cũ (có thể còn nằm trong secondary index) bỗng trỏ sang
// record khác — đúng cái mà Postgres phải chạy VACUUM mới dám làm.
```

Nếu một index vẫn còn giữ địa chỉ `(0,3)` của hàng đã xoá, rồi ô 3 lại được giao cho một hàng mới,
thì index đó sẽ trỏ sang một hàng hoàn toàn khác. Postgres chỉ đánh dấu ô là `UNUSED` (dùng lại
được) **sau khi** `VACUUM` đã dọn xong mọi mục index trỏ vào nó. Đó là lý do `VACUUM` phải đi qua
cả index chứ không chỉ riêng bảng.

### InnoDB thì sao?

InnoDB cũng dùng page có header, vùng dữ liệu và một "thư mục" ở cuối page. Có hai khác biệt lớn:

- Hàng nằm trong **lá của cây khoá chính** (bài 4), nên địa chỉ của hàng là **giá trị khoá chính**,
  không phải `(page, ô)`. Hàng có thể dời sang page khác (khi page bị tách) mà index phụ không cần
  biết.
- Các hàng trong page được nối thành **danh sách liên kết theo thứ tự khoá**, và thư mục chỉ giữ
  một con trỏ cho mỗi 4–8 hàng. Tìm trong page là: tìm nhị phân trên thư mục, rồi đi tuyến tính
  vài bước.

`UPDATE` ở InnoDB sửa hàng **tại chỗ** khi được, và đẩy bản cũ sang undo log thay vì để trong
page. Bài 7 sẽ cho thấy khác biệt nhỏ này dẫn tới hậu quả rất khác nhau.

## Mang về dùng

1. **Trong Postgres, `UPDATE` là "chèn bản mới + đánh dấu bản cũ".** Bảng hay bị `UPDATE` sẽ có
   nhiều xác hàng, và cần `VACUUM` chạy đều đặn. Đừng tắt autovacuum.
2. **Tận dụng HOT update:** đừng đánh index cho những cột bị cập nhật liên tục (`updated_at`,
   `view_count`, …) nếu không thật sự cần. Có index trên cột bị đổi thì `UPDATE` nào cũng phải sửa
   cả index. Để page còn chỗ cho bản mới, có thể đặt `fillfactor` thấp hơn 100 cho bảng hay bị
   cập nhật: `ALTER TABLE t SET (fillfactor = 90)`. Kiểm tra tỉ lệ HOT bằng `n_tup_hot_upd` trong
   `pg_stat_user_tables`.
3. **`DELETE` không làm file nhỏ đi, `VACUUM` cũng không** (nó chỉ dọn chỗ trống bên trong page để
   dùng lại). Muốn trả dung lượng cho hệ điều hành phải viết lại bảng, bài 7 có số đo.
4. **Đừng dùng `ctid` làm định danh lâu dài** trong ứng dụng. Nó đổi sau mỗi `UPDATE` và sau
   `VACUUM FULL`. Nó chỉ là địa chỉ vật lý của **một phiên bản** của hàng.

---

Script: [`blog/lab/02-page-pg.sql`](lab/02-page-pg.sql) · slotted page trong minidb:
[`internal/page/page.go`](../internal/page/page.go) · nhật ký: [`diary/phase2.md`](../diary/phase2.md).

**Bài tiếp theo:** [Bài 3 — RAM của database](03-buffer-pool.md)
