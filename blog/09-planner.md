# Bài 9 — Planner đoán mò

> Series [Mở nắp database](README.md) · bài 9/11 · cần đọc trước: [bài 8](08-index.md)

Một câu truy vấn chạy 10ms suốt nhiều tháng. Sáng nay nó mất 10 giây. Không ai sửa code, không ai
đổi index. Chỉ có đêm qua một job đã nạp thêm một triệu hàng.

Bài 8 kết thúc với một câu hỏi: vì sao planner của MySQL lại chọn index ở chỗ mà index chậm hơn?
Câu trả lời của bài này: **planner không biết câu truy vấn sẽ lấy ra bao nhiêu hàng. Nó đoán.**
Và mọi quyết định sau đó (index hay quét, nested loop hay hash join, join bảng nào trước) đều xây
trên con số đoán đó.

## Planner đoán bằng gì?

Để chọn plan, planner cần biết `WHERE status = 'pending'` sẽ khớp bao nhiêu hàng. Nó không thể
chạy thử để biết, vì như thế thì chạy luôn cho xong. Nên nó ước lượng dựa trên **thống kê** đã
thu thập trước đó (bằng `ANALYZE`): tổng số hàng, số giá trị khác nhau, giá trị nào hay xuất
hiện, phân bố ra sao.

Cách đơn giản nhất là giả định **phân bố đều**. minidb làm đúng như vậy
(`internal/query/query.go`):

```go
func (s Stats) Selectivity(col int, lo, hi keys.Value) float64 {
	cs, ok := s.Cols[col]
	if !ok || s.Rows == 0 {
		return 0.5 // không biết gì thì đoán một nửa — và đoán sai
	}
	...
	// Không phải kiểu số, hoặc cột chỉ có một giá trị: mô hình đều không
	// nói được gì. Trả về 1/Distinct — ước lượng của một phép bằng
	if cs.Distinct > 0 {
		return 1 / float64(cs.Distinct)
	}
	...
	f := spanOf(lo1, hi1) / whole // khoảng hỏi chiếm bao nhiêu phần của [min, max]
```

Một cột có 3 giá trị khác nhau thì mỗi giá trị chiếm 1/3 bảng. Một khoảng chiếm 10% của
`[min, max]` thì chứa 10% số hàng. Giả định này đúng với dữ liệu đẹp, và sai với dữ liệu thật.

## Thí nghiệm: ba cách làm planner đoán sai

Bảng 1 triệu hàng, dựng sẵn ba cái bẫy, mỗi bẫy đánh vào một giả định:

- **Lệch:** `status` gồm 98% `'done'`, 1% `'pending'`, 1% `'failed'`. Đánh vào giả định *phân bố đều*.
- **Tương quan:** `city` quyết định luôn `country` (ai ở `'c7'` thì chắc chắn ở `'k7'`). Đánh vào
  giả định *các cột độc lập với nhau*.
- **Cũ:** `ANALYZE` lúc bảng chỉ có 1000 hàng toàn `'done'`, rồi nạp thêm 200000 hàng, một nửa là
  `'pending'`, và không `ANALYZE` lại. Đánh vào giả định *thống kê còn mới*.

```bash
cd reallab && go run . -work stats
```

```text
-- pg
ca                                       ước lượng      thật      lệch  plan
lệch: status='pending' (1%)                   9600      9995      ×1.0  Index Scan st_status
lệch: status='done' (98%)                   980133    979972      ÷1.0  Seq Scan
tương quan: city='c7' AND country='k7'         915      9982     ×10.9  Bitmap Heap Scan st_city
tương quan: city='c7' AND country='k8'         899         0    ÷899.0  Bitmap Heap Scan st_city
cũ: status='pending' (50%)                       1    100000 ×100000.0  Index Scan st2_status
cũ + JOIN st                                     1    100000 ×100000.0  Nested Loop st2_status

-- mysql
lệch: status='pending' (1%)                  17934      9965      ÷1.8  Index lookup st_status
lệch: status='done' (98%)                   502702    979777      ×1.9  Index lookup st_status
tương quan: city='c7' AND country='k7'        1003     10030     ×10.0  Index lookup st_city
tương quan: city='c7' AND country='k8'        1003         0   ÷1003.0  Index lookup st_city
cũ: status='pending' (50%)                  100500    100000      ÷1.0  Index lookup st2_status

-- maria
lệch: status='pending' (1%)                  19928      9965      ÷2.0  ref st_status
lệch: status='done' (98%)                   966940    979777      ×1.0  ALL
tương quan: city='c7' AND country='k7'       10030     10030      ×1.0  ref st_city
tương quan: city='c7' AND country='k8'       10030         0  ÷10030.0  ref st_city
cũ: status='pending' (50%)                  191358    100000      ÷1.9  ALL
```

**Không database nào đúng ở cả ba ca.** Mỗi DB sai ở một chỗ khác nhau, và chỗ sai cho thấy công
cụ ước lượng của nó.

### Bẫy 1: phân bố lệch. MySQL mắc đúng lỗi của minidb

Postgres ước lượng đúng cả 1% lẫn 98%, vì `ANALYZE` của nó lưu danh sách **các giá trị hay gặp
nhất** (*most common values*) cùng tần suất của từng giá trị.

MySQL ước lượng `'done'` là 502702 hàng, trong khi thật là 979777. Nhìn vào thống kê của index:

```text
mysql> SHOW INDEX FROM st WHERE Key_name = 'st_status';
| st    |          1 | st_status |            1 | status      | A         |           2 | ...
                                                                            ▲ Cardinality = 2
```

MySQL nghĩ cột có 2 giá trị (thật là 3; cardinality của InnoDB là một con số ước lượng từ vài
page lấy mẫu, không phải đếm thật). Với phép so bằng trên index, nó coi mỗi giá trị chiếm **một
nửa bảng**. Đó chính là giả định phân bố
đều của minidb. Hậu quả:

```text
-> Index lookup on st using st_status (status='done')  (cost=61044 rows=480758) (actual time=0.195..1337 rows=979777)
-> Table scan on st  (cost=99957 rows=961517) (actual time=0.0501..128 rows=1e+6)       -- ép IGNORE INDEX
```

Với 98% bảng, MySQL chọn đi qua index và mất **1337ms**. Quét cả bảng chỉ mất **198ms**. Chậm hơn 7
lần, chỉ vì một con số đoán.

### Bẫy 2: cột tương quan. Cả ba DB đều sai

`city = 'c7'` chiếm 1% bảng, `country = 'k7'` chiếm 10%. Nếu hai cột độc lập thì cả hai điều kiện
cùng đúng ở 1% × 10% = **0.1%** số hàng. Cả ba DB đều nhân như vậy và ra khoảng 1000 hàng, trong
khi thật là 10000, vì ai ở `c7` thì đều ở `k7`. Sai 10 lần.

Cách chữa của từng DB:

```text
-- pg: CREATE STATISTICS st_cc (dependencies, mcv) ON city, country FROM st; ANALYZE st;
tương quan: city='c7' AND country='k7'        9733      9982      ×1.0     ← đúng
tương quan: city='c7' AND country='k8'           1         0      ×1.0     ← đúng

-- mysql: ANALYZE TABLE st UPDATE HISTOGRAM ON country;
tương quan: city='c7' AND country='k7'        1003     10030     ×10.0     ← vẫn sai

-- maria: ANALYZE TABLE st PERSISTENT FOR ALL;
tương quan: city='c7' AND country='k7'        1001     10030     ×10.0     ← TỆ HƠN trước khi chữa
```

- **Postgres chữa được** bằng *thống kê đa cột* (`CREATE STATISTICS`), vì nó lưu thẳng mối quan
  hệ giữa hai cột.
- **MySQL không chữa được.** Histogram là thống kê của **từng cột**. Cái sai không nằm ở từng thừa
  số mà ở phép nhân, nên histogram tốt đến đâu cũng không giúp gì.
- **MariaDB tệ hơn sau khi "chữa".** Trước khi có histogram, nó không ước lượng điều kiện
  `country` (coi như 100%), nên tình cờ đúng ở ca `k7`. Có histogram rồi, nó biết `country='k7'`
  chiếm 10%, nhân vào, và sai 10 lần. **Thêm thống kê có thể làm ước lượng xấu đi**, khi thống kê
  mới bị đưa vào đúng chỗ giả định sai.

### Bẫy 3: thống kê cũ. Chỉ Postgres bị lừa

```text
-- pg, trước ANALYZE
cũ: status='pending' (50%)                       1    100000 ×100000.0  Index Scan st2_status
cũ + JOIN st                                     1    100000 ×100000.0  Nested Loop            95.7 ms
-- pg, sau ANALYZE st2
cũ: status='pending' (50%)                  100091    100000      ÷1.0  Bitmap Heap Scan
cũ + JOIN st                                100091    100000      ÷1.0  Merge Join             58.7 ms
```

Lúc `ANALYZE` chạy, bảng chưa hề có `'pending'`. Postgres tin thống kê đó và đoán **1 hàng** cho
một điều kiện thật ra khớp 100000 hàng. Với 1 hàng thì nested loop join là lựa chọn hợp lý: tra
bảng kia một lần. Với 100000 hàng thì nó là tra bảng kia 100000 lần. Đây đúng là câu chuyện mở
đầu bài: một job nạp dữ liệu, thống kê chưa kịp cập nhật, và plan tốt hôm qua thành plan tệ hôm
nay. (Ở thí nghiệm này mọi thứ nằm trong RAM nên chỉ chênh 1.6 lần. Trên bảng lớn hơn RAM, nested
loop với 100000 lần tra ngẫu nhiên có thể chậm hơn hàng trăm lần.)

MySQL và MariaDB **không bị lừa**, dù thí nghiệm đã tắt tự động cập nhật thống kê
(`STATS_AUTO_RECALC=0`). Lý do: với điều kiện khoảng hoặc so bằng trên một cột có index, chúng
**đi xuống index và đếm thật** (*index dive*) thay vì tin thống kê. Đếm thì luôn đúng với dữ liệu
hiện tại. Đây cũng là cách rẻ nhất để minidb bớt đoán mò: nó đã có sẵn cursor để đi trong index.

## Mang về dùng

1. **Khi một truy vấn đột nhiên chậm, việc đầu tiên là so ước lượng với thực tế:**
   - Postgres: `EXPLAIN ANALYZE`, so `rows=` (ước lượng) với `actual ... rows=` ở từng node.
   - MySQL 8: `EXPLAIN ANALYZE`, so `rows=` trong `(cost=... rows=...)` với `(actual ... rows=...)`.
   - MariaDB: `ANALYZE FORMAT=JSON`, so `rows × filtered` với `r_rows × r_filtered`.

   Lệch từ 10 lần trở lên ở một node là dấu hiệu planner đang đoán mò. Chỗ cần sửa là thống kê,
   không phải câu SQL.
2. **Chạy `ANALYZE` sau khi nạp hoặc xoá dữ liệu hàng loạt.** Đừng đợi autovacuum hay auto-recalc
   tự làm. Chúng có ngưỡng (khoảng 10% bảng thay đổi) và có độ trễ.
3. **Postgres:** cột tương quan thì dùng `CREATE STATISTICS … (dependencies, mcv)`. Cột lệch nặng
   mà ước lượng vẫn sai thì tăng số mẫu bằng `ALTER TABLE … ALTER COLUMN … SET STATISTICS 1000`.
4. **MySQL:** cột có index mà cardinality sai thì tăng `innodb_stats_persistent_sample_pages` rồi
   `ANALYZE TABLE`. Cột **không** có index thì mới dùng histogram. Và nhớ rằng histogram không chữa
   được tương quan giữa các cột.
5. **Thêm thống kê chưa chắc đã tốt hơn.** Sau mỗi thay đổi thống kê, chạy lại `EXPLAIN ANALYZE`
   để kiểm tra, như ca MariaDB ở trên.

---

Bộ đo: [`reallab/stats.go`](../reallab/stats.go) · số đo gốc, và lỗi `RAND()` của bộ sinh dữ liệu
MySQL: [`diary/phase9.md`](../diary/phase9.md), bảng 5 · bộ ước lượng của minidb:
[`internal/query/query.go`](../internal/query/query.go) · [`diary/phase7.md`](../diary/phase7.md).

**Bài tiếp theo:** [Bài 10 — Đọc `EXPLAIN` như người viết ra nó](10-explain.md)
