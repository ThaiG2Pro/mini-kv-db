# Bài 8 — Khi nào index không được dùng

> Series [Mở nắp database](README.md) · bài 8/11 · cần đọc trước: [bài 4](04-uuid.md)

Bạn tạo index, chạy `EXPLAIN`, và thấy `Seq Scan` (Postgres) hoặc `type: ALL` (MySQL). Có index
rồi mà DB vẫn quét cả bảng. Có hai lý do hoàn toàn khác nhau cho chuyện này:

1. **DB không thể dùng index**, vì cách bạn viết `WHERE` không khớp với cách index được sắp xếp.
2. **DB có thể dùng, nhưng chọn không dùng**, vì với câu truy vấn này, quét cả bảng thật sự nhanh
   hơn.

Bài này đo cả hai.

## Phần 1: khi index không dùng được

### Index là dữ liệu đã được sắp xếp

Một index B+Tree (bài 4) là các khoá **đã sắp theo thứ tự**, và thứ duy nhất nó làm nhanh là: *tìm
một khoảng liên tục trong thứ tự đó*. Mọi quy tắc "viết thế này thì index không chạy" đều suy ra
từ một câu hỏi: **điều kiện của bạn có phải là một khoảng liên tục trong thứ tự của index không?**

minidb cho thấy thứ tự đó ở mức từng byte. Đây là cách nó mã hoá khoá của một index `(city, age)`
(`go run ./cmd/idxlab -work bytes`):

```text
index (city, age). Điều kiện city='HN' là một khoảng liên tục:
  [06484e0000, 06484e0001)
Điều kiện age=30 KHÔNG là khoảng nào cả — byte của age nằm SAU
byte của city, nên các hàng age=30 rải khắp cây:
  (DN, 30) -> 06444e0000fb7fffffffffffffe1
  (HN, 30) -> 06484e0000fb7fffffffffffffe1
  (SG, 30) -> 0653470000fb7fffffffffffffe1
```

Khoá composite là **các cột nối đuôi nhau** rồi so từng byte từ trái sang phải. Mọi hàng có
`city = 'HN'` bắt đầu bằng cùng các byte `06484e00`, nên chúng nằm cạnh nhau trong cây. Còn các
hàng `age = 30` thì nằm rải rác giữa DN, HN, SG, vì byte của `age` đứng **sau** byte của `city`.
Người ta gọi đây là *leftmost prefix rule*. Nó không phải một quy ước tuỳ tiện. Nó là hệ quả trực
tiếp của việc cây chỉ biết so byte từ trái sang phải.

### Năm cách viết `WHERE` làm index "biến mất"

Bảng `u8` có 200000 hàng, với index trên `email`, `(city, age)` và `code`:

```bash
reallab/q.sh pg    blog/lab/08-index-pg.sql
reallab/q.sh mysql blog/lab/08-index-mysql.sql
```

**1. Bọc cột trong một hàm**

```text
Postgres   WHERE email = 'user42@mail.com'         → Index Scan using u8_email
Postgres   WHERE lower(email) = 'user42@mail.com'  → Seq Scan, Filter: (lower(email) = ...)
MySQL      WHERE lower(email) = 'user42@mail.com'  → type=ALL, rows=199526
```

Index sắp theo `email`, không sắp theo `lower(email)`. Hai thứ tự đó khác nhau (`'B' < 'a'` nhưng
`'b' > 'a'`), nên DB không có cách nào dùng index này để tìm `lower(email)`. Cách chữa là đánh
index đúng cho biểu thức đó:

```text
CREATE INDEX u8_lower_email ON u8 (lower(email));
Postgres   WHERE lower(email) = 'user42@mail.com'  → Index Scan using u8_lower_email
```

(MySQL 8 có *functional index* với cú pháp `INDEX ((lower(email)))`.)

**2. Chỉ lọc theo cột thứ hai của index composite**

```text
Postgres   WHERE city = 'c7' AND age = 30   → Bitmap Index Scan on u8_city_age
             Buffers: shared read=2                          ← đọc 2 page index
Postgres   WHERE age = 30                   → Bitmap Index Scan on u8_city_age
             Buffers: shared hit=2 read=173                  ← đọc 175 page: CẢ index
MySQL      WHERE age = 30                   → type=ALL, rows=199526
```

Ở đây hai DB xử lý khác nhau, và cả hai đều không nhanh. MySQL bỏ hẳn index và quét cả bảng.
Postgres vẫn "dùng" index, nhưng bằng cách **đọc hết cả index** (175 page so với 2 page khi có
`city`), vì index nhỏ hơn bảng. Nếu chỉ nhìn chữ `Index Scan` trong `EXPLAIN` thì bạn sẽ tưởng
mọi thứ ổn. Phải nhìn `Buffers` mới thấy nó đang đọc gấp gần 90 lần.

**3. `LIKE` bắt đầu bằng `%`**

```text
MySQL      WHERE email LIKE 'user42@%'      → type=range, key=u8_email
MySQL      WHERE email LIKE '%42@mail.com'  → type=ALL
Postgres   WHERE email LIKE '%42@mail.com'  → Seq Scan
```

`'user42@%'` là một khoảng: mọi chuỗi bắt đầu bằng `user42@`. `'%42@mail.com'` thì không, vì các
chuỗi kết thúc bằng `42@mail.com` nằm rải rác khắp index.

**3b. Bẫy riêng của Postgres: `LIKE` có tiền tố mà vẫn không dùng index**

```text
Postgres   WHERE email LIKE 'user42@%'      → Seq Scan              ← ?!
 datcollate
------------
 en_US.utf8
```

Database được tạo với collation `en_US.utf8`, và index thường được sắp theo luật so chuỗi của ngôn
ngữ đó, không phải theo byte. Với luật đó, "mọi chuỗi bắt đầu bằng `user42@`" không đảm bảo nằm
liền nhau. Cần một index sắp theo byte:

```text
CREATE INDEX u8_email_pat ON u8 (email text_pattern_ops);
Postgres   WHERE email LIKE 'user42@%'
  → Index Scan using u8_email_pat
      Index Cond: ((email ~>=~ 'user42@'::text) AND (email ~<~ 'user42A'::text))
```

Nhìn dòng `Index Cond`: Postgres đổi `LIKE 'user42@%'` thành đúng một **khoảng byte**,
`[user42@, user42A)`, vì `A` là ký tự ngay sau `@` trong bảng mã. Đó chính là kiểu khoảng mà
minidb in ra ở trên.

**4. So cột chuỗi với một con số (MySQL)**

```text
MySQL      WHERE code = '00000077'   → type=ref, key=u8_code, rows=1
MySQL      WHERE code = 77           → type=ALL, rows=199526
             kết quả: | 77 | 00000077 |
```

`code` là `varchar`. So nó với **số** 77 thì MySQL phải đổi **từng giá trị** của cột sang số rồi
mới so, tức là cũng như bọc cột trong một hàm (ca 1). Index không dùng được, và kết quả còn khớp
cả `'00000077'`, vì `'00000077'` đổi sang số là 77. Lỗi này hay xuất hiện khi ORM hoặc driver
truyền tham số sai kiểu.

**5. `OR` trên hai cột: cả hai DB đều xử lý được**

```text
Postgres   WHERE email = ... OR code = ...  → BitmapOr (Bitmap Index Scan on u8_email_pat, on u8_code)
MySQL      WHERE email = ... OR code = ...  → type=index_merge, Using union(u8_email,u8_code)
```

Cả hai tra hai index riêng rồi gộp kết quả. Chuyện "`OR` làm mất index" là chuyện của các phiên
bản rất cũ.

## Phần 2: khi DB chọn không dùng index, và nó đúng

Giờ trường hợp còn lại: index dùng được, nhưng DB vẫn quét cả bảng. Thí nghiệm: `WHERE k < v`
trên 1 triệu hàng, `k` có index, và các hàng có `k` gần nhau lại nằm rải rác khắp bảng. Ép từng
plan và đo, với độ chọn lọc (tỉ lệ số hàng lấy ra) từ 0.1% tới 100%:

```bash
cd reallab && go run . -work breakeven -repeat 9
```

```text
-- pg                          (ms)
sel            seq     index    bitmap   index/seq   planner tự chọn
0.1%          30.9       0.7       0.9       0.02x   Bitmap Heap Scan
2.0%          34.5      14.5      13.7       0.42x   Bitmap Heap Scan
5.0%          43.9      46.5      40.9       1.06x   Bitmap Heap Scan
10.0%        134.1     153.7      58.1       1.15x   Bitmap Heap Scan
30.0%        117.3     269.7     115.2       2.30x   Bitmap Heap Scan
50.0%        165.3     421.5     177.2       2.55x   Seq Scan
  -> hoà vốn index vs seq ĐO ĐƯỢC: 4.7%

-- mysql
sel            seq     index   index/seq   planner tự chọn
0.1%         112.6       2.1       0.02x   type=range key=be_k
2.0%         109.5      41.2       0.38x   type=range key=be_k
5.0%         112.4     114.5       1.02x   type=range key=be_k
10.0%        113.0     194.2       1.72x   type=range key=be_k       ← chọn sai
20.0%        123.0     369.9       3.01x   type=ALL
50.0%        137.2    1140.1       8.31x   type=ALL
  -> hoà vốn index vs seq ĐO ĐƯỢC: 4.9%
```

(MariaDB hoà vốn ở 7.0%.)

**Index chỉ thắng khi lấy dưới khoảng 5% bảng.** Quá mức đó thì quét cả bảng nhanh hơn, kể cả khi
toàn bộ dữ liệu đã nằm trong RAM. Lý do nằm ở cái giá của **một hàng**:

```text
                    quét/hàng ns  tra/hàng ns  tra/quét
postgres 17                   31          725     23.5x
mysql 8.4                    113         2060     18.3x
```

Quét tuần tự một hàng rất rẻ (31ns ở Postgres): page được đọc lần lượt, CPU đoán trước được, mọi
hàng trong page đều được dùng. Tra một hàng qua index đắt hơn 18–23 lần: đi xuống cây index, cầm
địa chỉ hàng, rồi nhảy tới một page bất kỳ trong bảng (ở MySQL là đi xuống **thêm một cây nữa**,
cây khoá chính, bài 4). Lấy 5% bảng qua index nghĩa là trả giá tra đó cho 50000 hàng, và lúc ấy nó
bằng giá quét cả 1 triệu hàng.

Hai chi tiết đáng chú ý từ bảng:

- **Bitmap scan của Postgres san phẳng đường cong.** Ở 30%, index scan chậm hơn seq 2.3 lần, còn
  bitmap scan **ngang bằng** seq. Bitmap scan gom địa chỉ các hàng lại, sắp theo số page, rồi mới
  đọc bảng. Nhảy lung tung được đổi thành đọc tuần tự. Vì vậy planner Postgres chọn bitmap tới
  tận 30% mà không sai.
- **MySQL chọn sai ở 10%:** nó vẫn chọn index, trong khi index chậm hơn quét 1.72 lần. Planner
  không phải lúc nào cũng đúng. Bài 9 sẽ nói vì sao.

(Còn một chuyện nữa: minidb hoà vốn ở 36.8%, cao gấp 7 lần DB thật. Lý do không phải index của nó
nhanh, mà là seq scan của nó chậm, 480ns mỗi hàng, gấp 15 lần Postgres. Một DB đồ chơi dạy đúng
cơ chế, nhưng các hằng số thì phải đo trên DB thật.)

## Mang về dùng

1. **Viết `WHERE` sao cho điều kiện là một khoảng trên cột được đánh index:** đừng bọc cột trong
   hàm, đừng để `LIKE` bắt đầu bằng `%`, truyền tham số **đúng kiểu** của cột.
2. **Thứ tự cột trong index composite là quyết định quan trọng nhất.** Cột nào luôn có mặt trong
   `WHERE` với phép `=` thì để trước; cột dùng cho khoảng (`<`, `BETWEEN`) hoặc `ORDER BY` để sau.
3. **Postgres với collation khác `C`:** muốn `LIKE 'abc%'` dùng index thì tạo index với
   `text_pattern_ops` (hoặc `varchar_pattern_ops`).
4. **Đọc `EXPLAIN (ANALYZE, BUFFERS)`, đừng chỉ đọc tên plan.** `Index Scan` mà đọc 175 page thay
   vì 2 page thì vẫn là một vấn đề.
5. **Index không phải lúc nào cũng nhanh hơn.** Truy vấn lấy quá khoảng 5–10% bảng thì quét cả bảng
   thường thắng. Đừng ép DB dùng index (`FORCE INDEX`, tắt `enable_seqscan`) nếu chưa đo cả hai.

---

Script: [`blog/lab/08-index-*.sql`](lab/), [`reallab/breakeven.go`](../reallab/breakeven.go) · bộ mã
hoá khoá giữ thứ tự của minidb: [`internal/keys/`](../internal/keys) · nhật ký:
[`diary/phase7.md`](../diary/phase7.md), [`diary/phase9.md`](../diary/phase9.md) (bảng 1).

**Bài tiếp theo:** [Bài 9 — Planner đoán mò](09-planner.md)
