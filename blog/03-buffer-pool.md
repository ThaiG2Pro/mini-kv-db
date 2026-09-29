# Bài 3 — RAM của database

> Series [Mở nắp database](README.md) · bài 3/11 · cần đọc trước: [bài 0](00-ban-do.md)

Trong mọi file cấu hình Postgres đều có dòng `shared_buffers`, trong mọi file cấu hình MySQL đều có
`innodb_buffer_pool_size`, và lời khuyên quen thuộc là *"đặt nó bằng 25% RAM"* hoặc *"70-80% RAM"*.
Nhưng cái vùng nhớ đó giữ cái gì? Và vì sao người ta hay kể chuyện *"một câu báo cáo chạy
`SELECT *` trên bảng lớn làm cả hệ thống chậm đi"*?

## Thí nghiệm 1: đọc từ RAM và đọc từ đĩa

Ở bài 0, ta đã thấy mọi thứ DB làm đều là đọc và ghi page. **Buffer pool** là vùng RAM mà DB dùng
để giữ các page đó. Muốn đọc một page, DB tìm nó trong buffer pool trước; không có thì mới đọc
từ đĩa lên, và phải đuổi một page khác ra để lấy chỗ.

Postgres cho bạn thấy trực tiếp một câu truy vấn đã lấy page từ đâu:

```console
$ reallab/q.sh pg <<< "EXPLAIN (ANALYZE, BUFFERS) SELECT count(*) FROM big;"
 Aggregate (actual rows=1 loops=1)
   Buffers: shared hit=1551 read=50225 written=32
   I/O Timings: shared read=62.711 write=0.182
```

- `hit=1551`: 1551 page đã có sẵn trong buffer pool.
- `read=50225`: 50225 page phải đọc lên, mất 62.7ms chờ I/O.

Đọc lại một bảng nhỏ đã có sẵn trong RAM thì toàn bộ là `hit`:

```text
   Buffers: shared hit=3456
 Execution Time: 7.709 ms
```

Đây là dòng đáng nhìn đầu tiên mỗi khi một câu truy vấn chậm: **nó chạm bao nhiêu page, và bao
nhiêu page trong số đó phải lên đĩa lấy.**

## Thí nghiệm 2: một câu `SELECT` có đuổi dữ liệu nóng ra khỏi RAM không?

Kịch bản: bảng `hot` (25MB) được ứng dụng đọc liên tục nên nằm trọn trong buffer pool. Rồi có
người chạy một báo cáo quét cả bảng `big`, lớn hơn cả buffer pool (256MB). Sau lần quét đó, bảng
`hot` còn nằm trong RAM không?

Nếu buffer pool dùng thuật toán đơn giản nhất, **LRU** (đuổi page lâu nhất chưa được dùng), thì câu
trả lời là **không**. Mỗi page của `big` vừa được đọc đều là "mới dùng nhất", nên lần lượt đẩy
mọi page của `hot` ra ngoài. Hiện tượng này tên là **sequential flooding**. Phase 3 của minidb đo
đúng nó, với một vùng nóng và một luồng quét trộn vào:

```text
hit ratio của RIÊNG vùng nóng
scan/200 op         lru    clock    lru-2
0                 0.755    0.744    0.817
50                0.648    0.638    0.816
1000              0.638    0.637    0.813
```

LRU tụt từ 0.755 xuống 0.638 ngay khi có quét. LRU-2 gần như không nhúc nhích. Vậy DB thật dùng gì?

**Postgres** ([`blog/lab/03-buffer-pg.sql`](lab/03-buffer-pg.sql)):

```text
== trước khi quét big: bao nhiêu page của mỗi bảng đang nằm trong shared_buffers
 relname | buffers | pages_on_disk
---------+---------+---------------
 big     |    1600 |         51776
 hot     |    3460 |          3456

== quét cả bảng big (400MB) một lần
   Buffers: shared hit=1551 read=50225 written=32

== sau khi quét big
 relname | buffers | pages_on_disk
---------+---------+---------------
 big     |    1600 |         51776       ← đọc 51776 page mà vẫn chỉ chiếm 1600 buffer
 hot     |    3460 |          3456       ← không mất page nào

== đọc lại bảng hot: hit hay read?
   Buffers: shared hit=3456                ← 100% từ RAM
```

Postgres đọc 51776 page của `big`, vậy mà số buffer `big` chiếm **không tăng lên một chút nào**,
và `hot` còn nguyên. Lý do: khi quét tuần tự một bảng lớn hơn 1/4 `shared_buffers`, Postgres không
cho lần quét đó dùng cả buffer pool. Nó cấp cho lần quét một **vòng đệm (ring buffer)** nhỏ,
256KB, và cứ dùng đi dùng lại vòng đó. Lần quét chỉ có quyền đuổi chính các page của nó.

**MySQL** thì bảo vệ bằng cách khác, và có một biến để tắt cơ chế đó đi, nên ta làm được phản
chứng ([`blog/lab/03-buffer-mysql-run.sql`](lab/03-buffer-mysql-run.sql)):

```text
== innodb_old_blocks_time = 1000        (mặc định)
| trước khi quét big       |      2050 |       1 |       ← hot_young, hot_old
| sau khi quét big (590MB) |      2050 |       0 |       ← còn nguyên

== innodb_old_blocks_time = 0           (tắt bảo vệ)
| trước khi quét big       |      2050 |       0 |
| sau khi quét big (590MB) |      NULL |    NULL |       ← mất sạch
```

Cùng một câu quét 590MB. Bật bảo vệ thì 2050 page nóng còn nguyên, tắt đi thì **không còn page
nào**. Đó chính là sequential flooding, dựng lại được trên MySQL thật chỉ bằng một biến cấu hình.

## Bên trong: "dùng một lần" khác "dùng thường xuyên"

Cả ba cách chữa (LRU-K của minidb, vùng old/young của InnoDB, ring buffer của Postgres) đều trả lời
cùng một câu hỏi: **làm sao phân biệt một page được chạm một lần rồi thôi với một page được dùng
đi dùng lại?** LRU thuần không phân biệt được: với nó, cả hai loại đều là "vừa mới dùng".

**minidb (LRU-K)** nhớ K lần truy cập gần nhất của mỗi page, và đuổi page mà lần truy cập thứ K
tính ngược lại đã xa nhất (`internal/bufpool/replacer.go`, bỏ bớt phần phụ):

```go
// Đây chính là cơ chế chống sequential scan: một page bị quét qua đúng MỘT lần
// thì chưa có mốc thứ K, nên nó bị coi là "khoảng cách vô hạn" và bị đuổi
// TRƯỚC mọi page đã được dùng >= K lần.
func (r *lrukReplacer) Victim() (int, bool) {
	for i := range r.hist {
		h := r.hist[i]
		if len(h) < r.k {
			// chưa đủ K lần truy cập: ứng viên hàng đầu để bị đuổi
			...
			continue
		}
		// đủ K lần: so mốc thứ K tính ngược, cũ hơn thì bị đuổi trước
		...
	}
}
```

Page của lần quét chỉ được chạm một lần, nên luôn bị đuổi trước mọi page của `hot`.

**InnoDB (midpoint insertion)** chia danh sách LRU làm hai: vùng *young* (5/8) và vùng *old* (3/8).
Page mới đọc lên được đặt vào **đầu vùng old**, chứ không phải đầu danh sách. Nó chỉ được lên
vùng young nếu bị chạm lại **sau hơn `innodb_old_blocks_time` mili giây** (mặc định 1000). Một
lần quét chạm mỗi page trong vài micro giây rồi đi tiếp, nên các page đó không bao giờ lên được
vùng young, và bị đuổi khỏi vùng old mà không làm phiền ai. Đặt `innodb_old_blocks_time = 0` là
xoá cái điều kiện "sau hơn 1 giây" đó, và bạn thấy ngay hậu quả ở trên.

**Postgres (ring buffer)** không cố đoán page nào sẽ được dùng lại. Nó nhìn vào **kiểu truy vấn**:
quét tuần tự một bảng lớn, `VACUUM`, `COPY` hàng loạt thì chỉ được dùng một vòng đệm nhỏ. Còn lại,
Postgres dùng thuật toán **clock sweep**: mỗi buffer có một bộ đếm `usage_count` (tối đa 5), mỗi
lần được dùng thì tăng lên, và kim đồng hồ quét qua sẽ giảm nó xuống. Buffer nào về 0 thì bị đuổi.

minidb chưa có ring buffer (nợ P3-4 trong sổ nợ). Nó chống quét hoàn toàn nhờ LRU-K.

### Một bẫy nhỏ khi đo: `count(*)`

Lần đo MySQL đầu tiên dùng `SELECT count(*) FROM hot` để làm nóng bảng, và bảng nóng bị đuổi sạch
ở **cả hai** cấu hình. Soi vào thì thấy chỉ 605/2051 page lên được vùng young. Đổi câu làm nóng
thành `SELECT sum(length(pad)) FROM hot` (đọc từng hàng thật) thì 2050/2051 page lên young, và kết
quả như bảng trên. Ở MySQL 8, `count(*)` không có `WHERE` đi qua một đường đọc song song riêng
(`innodb_parallel_read_threads`), và trong thí nghiệm này, đường đọc đó không đẩy page lên vùng young.
Bài học chung hơn: **câu lệnh bạn dùng để đo có thể đi một đường khác với câu lệnh thật của ứng
dụng.**

## Mang về dùng

1. **Nhìn vào `Buffers:` trong `EXPLAIN (ANALYZE, BUFFERS)`** (Postgres) mỗi khi một truy vấn
   chậm. `read` lớn nghĩa là dữ liệu không nằm trong RAM; lúc đó thêm RAM hoặc giảm số page phải
   đọc (index tốt hơn, chọn ít cột hơn) có tác dụng hơn là sửa câu SQL.
2. **Hit ratio của cả DB** xem được bằng:
   ```sql
   -- Postgres
   SELECT sum(blks_hit)::float / nullif(sum(blks_hit) + sum(blks_read), 0) FROM pg_stat_database;
   -- MySQL
   SHOW GLOBAL STATUS LIKE 'Innodb_buffer_pool_read%';   -- reads / read_requests
   ```
   Một hệ thống OLTP khoẻ thường ở trên 99%. Tụt đột ngột là dấu hiệu tập dữ liệu nóng đã lớn
   hơn buffer pool.
3. **Đừng tắt các cơ chế chống quét.** Đừng đặt `innodb_old_blocks_time = 0` "cho nhanh". Chạy báo
   cáo lớn trên một replica riêng vẫn là cách an toàn nhất.
4. **Kích thước buffer pool phụ thuộc kiến trúc.** InnoDB đọc thẳng xuống đĩa (bỏ qua page cache
   của hệ điều hành), nên buffer pool là cache duy nhất, và người ta đặt nó 70-80% RAM. Postgres
   đọc qua page cache của hệ điều hành, tức mỗi page có thể nằm hai lần trong RAM, nên
   `shared_buffers` thường chỉ khoảng 25% RAM, phần còn lại để cho page cache.

---

Script: [`blog/lab/03-buffer-*.sql`](lab/) · buffer pool của minidb:
[`internal/bufpool/`](../internal/bufpool) · nhật ký: [`diary/phase3.md`](../diary/phase3.md).

**Bài tiếp theo:** [Bài 4 — Vì sao UUID làm chậm insert](04-uuid.md)
