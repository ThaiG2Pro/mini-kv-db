# Phase 9 — Đối chiếu với DB thật: Postgres, MySQL, MariaDB

- **Thời lượng dự kiến:** 1-2 ngày (5 thí nghiệm × 1-2 giờ) · **thực tế:** 1 buổi
- **Bắt đầu:** 2026-09-29 · **Kết thúc:** 2026-09-29
- **Trạng thái:** ✅ xong
- **Commit:** `e2fe5ed`

> **Quy tắc ghi nhật ký:** mọi con số, mọi kết luận đều phải kèm **lệnh shell sinh ra nó**
> và **output thật** (dán nguyên, không tóm tắt). Sáu tháng sau đọc lại phải chạy lại được.
> Ghi trong lúc làm, không phải sau khi xong.

## Môi trường

```console
$ uname -srmo && go version && df -hT . | tail -1
Linux 6.6.87.2-microsoft-standard-WSL2 x86_64 GNU/Linux
go version go1.26.2 linux/amd64
/dev/sdd       ext4 1007G   66G  891G   7% /

$ nproc && grep -m1 'model name' /proc/cpuinfo && free -g | sed -n 2p
6
model name	: 12th Gen Intel(R) Core(TM) i5-1235U
Mem:              11           8           0           0           3           3

$ docker --version && docker compose -f reallab/docker-compose.yml images
Docker version 29.6.0, build fb59821
CONTAINER           REPOSITORY          TAG                 PLATFORM            IMAGE ID            SIZE                CREATED
rl-maria            mariadb             11.8                linux/amd64         cc7f5fdd7c8f        329MB               13 days ago
rl-mysql            mysql               8.4                 linux/amd64         ee241324a55f        813MB               7 days ago
rl-pg               postgres            17                  linux/amd64         212aeeeb8faa        454MB               10 days ago
```

Phiên bản chạy thật: **PostgreSQL 17.11**, **MySQL 8.4.11**, **MariaDB 11.8.9**. Mỗi DB được cấp
buffer pool **256MB** (`shared_buffers` / `innodb_buffer_pool_size`), các tham số khác để mặc định.
Cả ba chạy trong Docker trên cùng máy WSL2, nên số tuyệt đối **không** so được với máy chủ thật.
Chỉ các tỉ số là có giá trị.

## Mục tiêu phase

Tám phase trước trả lời câu *"DB hoạt động thế nào"* bằng cách tự viết ra nó. Phase này hỏi câu
còn lại: **những gì minidb dạy có đúng với DB người ta dùng thật không?** Mỗi thí nghiệm lấy một
con số đã đo trên minidb, đặt câu hỏi y hệt cho Postgres, MySQL và MariaDB, rồi so. Chỗ khớp là
hiểu biết dùng được ngay; chỗ lệch thì phải giải thích được vì sao.

Phase này cũng là nguyên liệu cho series blog (xem `blog/`).

## Câu hỏi phải trả lời được khi xong

1. Điểm hoà vốn index vs seq scan của DB thật nằm ở đâu, khi dữ liệu nằm trọn trong RAM như minidb?
   Lời giải thích của phase 7 ("36.8% vì không có I/O") có đứng vững không?
2. Bảng 5 anomaly × 4 mức isolation của ba DB khác nhau ở ô nào, và vì sao?
3. Chèn PK ngẫu nhiên (UUID) đắt hơn chèn tăng dần bao nhiêu trên InnoDB, và trên Postgres (heap)?
4. Một transaction mở lâu làm phình cái gì ở Postgres, và cái gì ở InnoDB?
5. Planner chọn sai khi nào vì thống kê, và mỗi DB tự chữa bằng cách nào?

## Deliverable

`go run ./reallab` (module riêng, để minidb vẫn không có dependency nào) in năm bảng, mỗi bảng
một câu hỏi ở trên, chạy trên cả ba DB.

## Reproduce toàn bộ phase

```bash
docker compose -f reallab/docker-compose.yml up -d
cd reallab
go run . -work breakeven -repeat 9     # bảng 1, ~3 phút
go run . -work anomaly                 # bảng 2
go run . -work pkorder                 # bảng 3
go run . -work bloat                   # bảng 4
go run . -work stats                   # bảng 5
docker compose down -v                 # dọn
```

`reallab/q.sh <pg|mysql|maria>` chạy SQL từ stdin trên một DB, dùng để soi tay.

---

## Nhật ký

### 2026-09-29 — dựng lab

```console
$ docker compose -f reallab/docker-compose.yml up -d
 Container rl-pg Started
 Container rl-mysql Started
 Container rl-maria Started
$ echo 'select version();' | reallab/q.sh pg        # (rút gọn)
 PostgreSQL 17.11 (Debian 17.11-1.pgdg13+2) on x86_64-pc-linux-gnu, ...
$ echo 'select version();' | reallab/q.sh mysql
| 8.4.11    |
$ echo 'select version();' | reallab/q.sh maria
| 11.8.9-MariaDB-ubu2404 |
```

Bộ đo viết bằng Go (`reallab/`, driver `pgx` + `go-sql-driver/mysql`). Nó là **module riêng**
để `go.mod` của minidb vẫn không có dependency nào.

### 2026-09-29 — bảng 1, lượt đầu: Postgres cho ba plan cùng một con số

Thiết lập giống phase 7: bảng `be(id PK, k, pad)` với 1 triệu hàng, `k` ngẫu nhiên và **không**
tương quan với thứ tự vật lý, index trên `k`. Truy vấn là `SELECT count(*), sum(length(pad)) … WHERE k < v`,
phải đọc `pad` nên không dùng được index-only scan. Mỗi plan được ép bằng cơ chế của chính DB đó
(`enable_*` ở Postgres, `FORCE/IGNORE INDEX` ở MySQL). Postgres tắt truy vấn song song cho công bằng
với ba bên còn lại, vốn chạy mỗi truy vấn trên một luồng.

```console
$ go run . -work breakeven
-- pg (nạp 1000000 hàng mất 5.942s)
sel            seq     index    bitmap   index/seq   planner tự chọn
0.1%          52.8      55.3      57.5       1.05x   Bitmap Heap Scan + Index Scan
1.0%          53.6      52.8      49.6       0.99x   Bitmap Heap Scan + Index Scan
...
30.0%        135.5     133.6     127.9       0.99x   Bitmap Heap Scan + Index Scan
50.0%        182.4     191.4     183.5       1.05x   Seq Scan
100.0%       309.6     310.6     314.7       1.00x   Seq Scan
  -> hoà vốn index vs seq ĐO ĐƯỢC: 2.0%
```

**Đọc kết quả:** ba plan mà cùng một thời gian ở mọi hàng là điều không thể: index scan lấy 1000
hàng không thể tốn 55ms như quét cả 1 triệu hàng. Theo quy tắc 5 của diary, nghi **bộ đo** trước.

Soi tay bằng `psql` thì thấy index scan thật chỉ tốn 9ms:

```console
$ reallab/q.sh pg <<< "SET max_parallel_workers_per_gather=0; SET enable_seqscan=off; SET enable_bitmapscan=off;
EXPLAIN (ANALYZE, BUFFERS) SELECT count(*), sum(length(pad)) FROM be WHERE k < 1000;"
 Aggregate  (cost=4013.99..4014.00 rows=1 width=16) (actual time=9.165..9.167 rows=1 loops=1)
   ->  Index Scan using be_k on be  (cost=0.42..4006.32 rows=1023 width=101) (actual time=0.050..8.612 rows=947 loops=1)
 Execution Time: 9.339 ms
```

**Nguyên nhân:** driver `pgx` mặc định **prepare** câu lệnh rồi giữ trong cache theo chuỗi SQL.
Ở Postgres, chuỗi SQL của ba plan giống hệt nhau (chỉ khác `SET enable_*` chạy trước), nên câu
lệnh được lập plan **một lần** ở lượt `seq` rồi dùng lại mãi. Một prepared statement không có tham
số thì Postgres dùng luôn generic plan, và **đổi `enable_*` không làm plan đã cache mất hiệu lực**.
Kết quả là cả ba cột đều đo seq scan.

**Đã sửa:** `default_query_exec_mode=simple_protocol` trong DSN, giống cách `psql` gửi câu lệnh.

**Đang nghĩ gì:** đây đúng là thứ minidb chưa có (nợ P8-6, plan cache), và giờ thấy luôn mặt
trái của nó: một plan đã cache sẽ **không biết** môi trường đã đổi. Postgres chỉ vô hiệu hoá plan
khi *schema* đổi, không khi *cấu hình* đổi.

Cùng lượt đó, MySQL/MariaDB cho seq scan dao động tới 2x (130 → 304ms) chỉ giữa hai hàng liền
nhau. Đã kiểm tra: bảng (132MB + 16MB index) nằm trọn trong buffer pool 256MB
(`Innodb_buffer_pool_reads` = 1861 so với 73 triệu `read_requests`), và các container khác đều
rảnh (`docker stats`: < 1% CPU, load average 0.92). Nên đây là nhiễu CPU của laptop, không phải I/O.
Tăng lên **9 lần lặp** để lấy trung vị.

### 2026-09-29 — bảng 1, lượt chạy thật

```console
$ go run . -work breakeven -repeat 9
== 1. điểm hoà vốn selectivity (1000000 hàng, k ngẫu nhiên, trung vị 9 lần, ms) ==

-- pg (nạp 1000000 hàng mất 2.634s)
sel            seq     index    bitmap   index/seq   planner tự chọn
0.1%          30.9       0.7       0.9       0.02x   Bitmap Heap Scan
1.0%          34.1       7.7       8.5       0.23x   Bitmap Heap Scan
2.0%          34.5      14.5      13.7       0.42x   Bitmap Heap Scan
5.0%          43.9      46.5      40.9       1.06x   Bitmap Heap Scan
10.0%        134.1     153.7      58.1       1.15x   Bitmap Heap Scan
20.0%        146.8     212.4      87.3       1.45x   Bitmap Heap Scan
30.0%        117.3     269.7     115.2       2.30x   Bitmap Heap Scan
50.0%        165.3     421.5     177.2       2.55x   Seq Scan
70.0%        238.4     621.2     264.4       2.61x   Seq Scan
100.0%       309.0     850.4     313.4       2.75x   Seq Scan
  -> hoà vốn index vs seq ĐO ĐƯỢC: 4.7%

-- mysql (nạp 1000000 hàng mất 9.509s)
sel            seq     index   index/seq   planner tự chọn
0.1%         112.6       2.1       0.02x   type=range key=be_k (Using index condition)
1.0%         110.9      21.3       0.19x   type=range key=be_k (Using index condition)
2.0%         109.5      41.2       0.38x   type=range key=be_k (Using index condition)
5.0%         112.4     114.5       1.02x   type=range key=be_k (Using index condition)
10.0%        113.0     194.2       1.72x   type=range key=be_k (Using index condition)
20.0%        123.0     369.9       3.01x   type=ALL (Using where)
30.0%        125.2     595.4       4.76x   type=ALL (Using where)
50.0%        137.2    1140.1       8.31x   type=ALL (Using where)
70.0%        143.7    1328.1       9.24x   type=ALL (Using where)
100.0%       149.6    1837.5      12.28x   type=ALL (Using where)
  -> hoà vốn index vs seq ĐO ĐƯỢC: 4.9%

-- maria (nạp 1000000 hàng mất 3.851s)
sel            seq     index   index/seq   planner tự chọn
0.1%         115.5       1.8       0.02x   type=range key=be_k (Using index condition)
1.0%         119.1      17.2       0.14x   type=range key=be_k (Using index condition)
2.0%         123.1      36.6       0.30x   type=range key=be_k (Using index condition)
5.0%         117.4      93.6       0.80x   type=range key=be_k (Using index condition)
10.0%        141.9     186.2       1.31x   type=ALL (Using where)
20.0%        130.7     366.7       2.81x   type=ALL (Using where)
30.0%        135.5     613.8       4.53x   type=ALL (Using where)
50.0%        167.5     826.9       4.94x   type=ALL (Using where)
70.0%        153.5    1193.0       7.77x   type=ALL (Using where)
100.0%       167.6    1981.0      11.82x   type=ALL (Using where)
  -> hoà vốn index vs seq ĐO ĐƯỢC: 7.0%
```

**Đọc kết quả:** ba DB thật hoà vốn ở **4.7% / 4.9% / 7.0%**, trong khi minidb hoà vốn ở **36.8%**.
Cả ba cũng chạy trọn trong RAM: dữ liệu nằm hết trong buffer pool, `I/O Timings` của Postgres
gần như bằng 0. Vậy lời giải thích của phase 7, *"36.8% chứ không phải 5-20% như sách vì ở quy mô
này không có I/O thật"*, **sai**. Không có I/O mà DB thật vẫn hoà vốn đúng trong khoảng của sách.

Tìm nguyên nhân thật bằng cách tách chi phí của **một hàng**. Seq ở 0.1% gần như chỉ còn phí quét
(lọc 1 triệu hàng, cộng dồn gần như không có hàng nào), còn index ở 2% chia cho 20000 hàng là
phí tra một hàng:

```console
$ python3 - <<'EOF'
rows = {
 "minidb (phase 7)": (480, 2142),
 "postgres 17":      (30.9e6/1e6, 14.5e6/20000),
 "mysql 8.4":        (112.6e6/1e6, 41.2e6/20000),
 "mariadb 11.8":     (115.5e6/1e6, 36.6e6/20000),
}
...
EOF
                    quét/hàng ns  tra/hàng ns  tra/quét    hoà vốn ≈ quét/tra
minidb (phase 7)             480         2142      4.5x                22.4%
postgres 17                   31          725     23.5x                 4.3%
mysql 8.4                    113         2060     18.3x                 5.5%
mariadb 11.8                 116         1830     15.8x                 6.3%
```

Hai cột kể hai chuyện khác nhau:

- **Tra một hàng: minidb (2142ns) ≈ InnoDB (1830–2060ns).** Đây là cùng một việc: đi từ mục
  secondary index, cầm PK, **xuống clustered B+Tree thêm một lần nữa**. Kiến trúc giống nhau thì
  giá gần như bằng nhau. Postgres rẻ hơn gần 3 lần (725ns), vì `ctid` trỏ thẳng tới page và slot
  trong heap, không phải đi xuống cây. Đây chính là dòng *"secondary index trỏ tới hàng bằng gì"*
  trong `docs/vs-innodb.md`, giờ có số đo đi kèm.
- **Quét một hàng: minidb (480ns) đắt gấp 4 lần InnoDB và 15 lần Postgres.** Đây mới là nguyên
  nhân. Điểm hoà vốn ≈ *phí quét / phí tra*, nên seq scan đắt thì điểm hoà vốn bị đẩy lên cao. Cột
  "tra" của minidb không có gì sai, cột "quét" mới chậm. Nghi phạm đã có tên trong sổ nợ: `keys.Decode`
  cấp phát trên đường đọc nóng nhất (P7-1), và `DecodeChain` giải mã cả chuỗi version cho mỗi hàng
  (P6-2). Ngược lại, Postgres chỉ giải mã (*deform*) đúng những cột mà điều kiện cần.

Thêm ba điều mà minidb không có để so:

1. **Bitmap heap scan của Postgres san phẳng đường cong.** Ở 30%, index scan chậm hơn seq 2.3x,
   còn bitmap thì **bằng** seq (115 vs 117ms). Bitmap gom các `ctid` lại, sắp theo số page rồi mới
   đọc heap, biến truy cập ngẫu nhiên thành truy cập tuần tự. Vì vậy planner Postgres chọn bitmap
   tới 30% mà **không sai**: nó không so index với seq, nó so bitmap với seq.
2. **Planner MySQL chọn sai ở 10%**: nó vẫn chọn index (`type=range`) trong khi index chậm hơn
   seq **1.72x**. MariaDB đổi plan giữa 5% và 10%, gần đúng điểm hoà vốn 7.0% của nó. Cùng một
   engine InnoDB, cùng dữ liệu, chỉ khác optimizer, mà đổi plan ở chỗ khác nhau.
3. **Ở 100%, index/seq của InnoDB là 12x, của Postgres chỉ 2.75x.** Mỗi lần tra trong InnoDB là một
   lần đi xuống cây (log n bước), còn trong Postgres là một phép nhảy thẳng tới page.

**Đang nghĩ gì:** đây là lần đầu trong 9 phase một thí nghiệm **bác bỏ lời giải thích** của một
phase trước, chứ không chỉ bác bỏ một giả thuyết đặt ra trong phase. Số 36.8% vẫn đúng, cách giải
thích nó thì sai. Phải sửa lại dòng phase 7 trong `ROADMAP.md`, và thêm một món nợ: đo thẳng phí
quét một hàng của minidb, tách phần `Decode` khỏi phần `DecodeChain`.

### 2026-09-29 — bảng 2, hai lượt đầu: bộ lập lịch tự gây deadlock

Năm bài dựng giống `internal/txn/workload.go`, chạy trên **hai kết nối thật**. Mỗi bước được gửi
cho transaction của nó rồi chờ tối đa 400ms; quá thời gian thì ghi nhận là "đang bị chặn" và đi
tiếp, đúng như khi mở hai cửa sổ `psql` và gõ xen kẽ. Ô trong bảng nói thêm DB chặn **bằng
cách nào**: `.w` = một bên phải chờ khoá (bi quan), `.a` = một bên bị huỷ (lạc quan, hoặc deadlock).

Lượt đầu, ở mức serializable của MySQL/MariaDB, hai hàng `non-repeatable-read` và `phantom` ra
`.a`. Lý thuyết nói InnoDB serializable là S2PL, tức phải ra `.w`: A bị khoá S của B chặn, chờ B
commit rồi chạy tiếp. Không có lý do gì để bị huỷ.

```console
$ go run . -work anomaly          # lượt 1 và lượt 2 cho cùng kết quả ở hai ô này
-- mysql (không có biến innodb_snapshot_isolation)
non-repeatable-read   X               X               .               .a
phantom               X               X               .               .a
```

**Nguyên nhân (hai lỗi chồng nhau, cùng một hình dạng):**

1. `do()` chờ bước trước của A xong rồi mới gửi bước kế tiếp của A. Nhưng bước trước của A đang
   chờ B, và B đang chờ `do()` gửi lệnh cho nó. Sửa: nếu A đang bị chặn thì chỉ **xếp hàng** bước
   mới cho A rồi đi tiếp. Lượt 2 vẫn ra `.a`, vì còn lỗi thứ hai.
2. `finish()` commit A, chờ A xong, rồi mới commit B. A đang chờ khoá của B nên chờ tới
   `innodb_lock_wait_timeout` (3s) thì bị DB huỷ. Sửa: gửi lệnh commit cho **cả hai** trước rồi mới chờ.

**Đang nghĩ gì:** cả hai lỗi là một bài học của phase 6 tự lặp lại ở bộ đo: **chờ theo thứ tự
cố định trong khi kẻ bị chờ lại cần mình đi trước là deadlock**. Chỉ khác là lần này nó nằm trong
bộ điều phối viết bằng Go, nên không có wait-for graph nào phát hiện, chỉ có timeout.

### 2026-09-29 — bảng 2, lượt chạy thật

```console
$ go run . -work anomaly

== 2. anomaly nào lọt ở mức nào ==
  X = XẢY RA   . = không xảy ra   .w = chặn bằng CHỜ khoá   .a = chặn bằng HUỶ transaction

-- pg
anomaly               read-uncomm     read-comm       repeat-read     serializable    
dirty-read            .               .               .               .               
non-repeatable-read   X               X               .               .               
phantom               X               X               .               .               
lost-update           X               X               .a              .a              
write-skew            X               X               X               .a              
lỗi DB trả về khi huỷ (.a):
  lost-update × repeat-read: ERROR: could not serialize access due to concurrent update (SQLSTATE 40001)
  write-skew × serializable: ERROR: could not serialize access due to read/write dependencies among transactions (SQLSTATE 40001)

-- mysql (không có biến innodb_snapshot_isolation)
anomaly               read-uncomm     read-comm       repeat-read     serializable    
dirty-read            X               .               .               .w              
non-repeatable-read   X               X               .               .w              
phantom               X               X               .               .w              
lost-update           X               X               X               .a              
write-skew            X               X               X               .a              
lỗi DB trả về khi huỷ (.a):
  lost-update × serializable: Error 1213 (40001): Deadlock found when trying to get lock; try restarting transaction

-- maria (innodb_snapshot_isolation = 1)
anomaly               read-uncomm     read-comm       repeat-read     serializable    
dirty-read            X               .               .               .w              
non-repeatable-read   X               X               .               .w              
phantom               X               X               .               .w              
lost-update           X               X               .a              .a              
write-skew            X               X               X               .a              
lỗi DB trả về khi huỷ (.a):
  lost-update × repeat-read: Error 1020 (HY000): Record has changed since last read in table 'acc'; try restarting transaction
  lost-update × serializable: Error 1213 (40001): Deadlock found when trying to get lock; try restarting transaction
```

So với bảng của minidb (phase 6):

```text
minidb                read-uncomm       read-comm         repeat-read       serializable
dirty-read            X                 .                 .                 .
non-repeatable-read   X                 X                 .                 .
phantom               X                 X                 .                 .
lost-update           X                 X                 .                 .
write-skew            X                 X                 X                 .
```

**Đọc kết quả:** hàng read-committed giống nhau ở cả bốn DB. Mọi khác biệt nằm ở ba chỗ, và mỗi
chỗ là một lựa chọn kiến trúc:

1. **Postgres không có dirty read, kể cả ở read-uncommitted.** Postgres nâng RU lên thành RC,
   còn InnoDB đọc được dữ liệu chưa commit thật. minidb đứng về phía InnoDB, nhưng phase 6 đã
   ghi rằng muốn có dirty read thì phải **viết thêm mã** (`peekDirty`). Postgres đơn giản là
   không viết đoạn mã đó.
2. **Lost update ở repeatable-read, ba kết cục khác nhau cho cùng một cái tên:**
   - Postgres và minidb **chặn**, theo luật first-committer-wins. Postgres báo
     `could not serialize access due to concurrent update (SQLSTATE 40001)`.
   - MySQL 8.4 **để lọt** (`X`). `UPDATE` ở RR của InnoDB đọc bản **mới nhất** ("current read")
     chứ không đọc snapshot, nên B ghi đè kết quả của A mà không ai báo lỗi.
   - MariaDB 11.8 **chặn**, nhờ biến `innodb_snapshot_isolation = 1` (mặc định từ 11.6). Tắt
     biến đó đi thì ô này quay về `X`, giống hệt MySQL (xem lượt phản chứng bên dưới).

   Lỗi của MariaDB là `Error 1020 (HY000)`, **không phải SQLSTATE 40001**. Code retry chỉ bắt
   40001 (serialization failure / deadlock) sẽ **không** retry lỗi này. Đây là một cái bẫy thực tế
   khi chuyển từ MySQL sang MariaDB.
3. **Serializable: hai trường phái hiện ra ngay trong ký hiệu.**
   - Postgres ra toàn `.` và `.a`, **không bao giờ `.w`**. Đó là SSI: không ai phải chờ ai, DB
     theo dõi các phụ thuộc đọc-ghi rồi huỷ một bên khi thấy nguy hiểm
     (`read/write dependencies among transactions`).
   - InnoDB ra `.w` ở ba hàng đầu và `.a` (deadlock) ở hai hàng cuối. Đó là S2PL: mọi `SELECT`
     lấy khoá S, người ghi phải chờ; hai bên cùng đọc rồi cùng ghi thì chờ vòng tròn, gây deadlock
     (`Error 1213 (40001)`).
   - minidb ở mức này là S2PL, tức **cùng phe với InnoDB**, đúng như `docs/vs-innodb.md` viết.
     Bảng của phase 6 chỉ in `.` nên không phân biệt được hai cách chặn. Bảng này phân biệt được.

**Đang nghĩ gì:** câu *"RepeatableRead chặn lost update"* đúng ở Postgres, đúng ở minidb, sai ở
MySQL và tuỳ phiên bản ở MariaDB. Phải có ô này thì phase 6 mới đủ: bảng của phase 6 là **một**
cách hiện thực các mức isolation, không phải định nghĩa của chúng.

### 2026-09-29 — bảng 2, phản chứng: tắt `innodb_snapshot_isolation` trên MariaDB

```console
$ echo "SET GLOBAL innodb_snapshot_isolation=0;" | ./q.sh maria && go run . -work anomaly -db maria
-- maria (innodb_snapshot_isolation = 0)
anomaly               read-uncomm     read-comm       repeat-read     serializable
dirty-read            X               .               .               .w
non-repeatable-read   X               X               .               .w
phantom               X               X               .               .w
lost-update           X               X               X               .a
write-skew            X               X               X               .a
$ echo "SET GLOBAL innodb_snapshot_isolation=1;" | ./q.sh maria     # trả lại mặc định
```

**Đọc kết quả:** đúng một ô đổi (lost-update × repeat-read: `.a` → `X`). Như vậy khác biệt giữa
MySQL và MariaDB ở ô đó **là do biến này**, không phải do một thứ gì khác trong fork.

### 2026-09-29 — bảng 3: PK tăng dần vs ngẫu nhiên (UUIDv4 vs khoá kiểu UUIDv7)

Câu hỏi mà dev thật sự gặp: chọn PK là UUIDv4 (ngẫu nhiên) hay một khoá 16 byte tăng dần theo
thời gian (hình dạng của UUIDv7, ở đây dùng bộ đếm thay đồng hồ để hai lần chạy cùng dãy khoá).
Cùng độ dài, chỉ khác **thứ tự**. Nạp 2 triệu hàng × ~150B, mỗi lô 1000 hàng là một commit. Tổng
dữ liệu (~300MB) **cố ý lớn hơn** buffer pool 256MB. Cột `¼ … 4/4` là tốc độ của từng phần tư,
để thấy lúc bảng còn vừa RAM và lúc đã tràn.

**Dự đoán (viết trong comment của `pkorder.go` trước khi chạy):** InnoDB đau hơn Postgres nhiều,
vì ở InnoDB cả bảng **là** cây PK, còn ở Postgres heap không theo thứ tự PK, chỉ riêng index PK
bị chèn lung tung.

Lượt 1 (cột tỉ số "đọc MB" chia cho gần 0 nên ra số vô nghĩa, sau đó đã sửa để in `0→N`):

```console
$ go run . -work pkorder

== 3. PK tăng dần vs ngẫu nhiên (2000000 hàng, khoá 16 byte, lô 1000 hàng/commit) ==

-- pg
khoá           ¼ h/s     ½ h/s     ¾ h/s   4/4 h/s   tổng s  bảng MB   idx MB   log MB   đọc MB   ghi MB
tăng dần      138118    130689    158352    143865     14.1      298       63      505        0       90
ngẫu nhiên    149937    147312    147507    119138     14.3      298       80      533        3      108
ngẫu/tăng      0.92x     0.89x     1.07x     1.21x    1.02x    1.00x    1.26x    1.06x   24.44x    1.20x
  (cột h/s: tỉ số là tăng/ngẫu — bao nhiêu lần NHANH hơn; các cột còn lại: ngẫu/tăng)

-- mysql
khoá           ¼ h/s     ½ h/s     ¾ h/s   4/4 h/s   tổng s  bảng MB   idx MB   log MB   đọc MB   ghi MB
tăng dần       70253     76054     65305     57156     30.1      294        0      366        0      326
ngẫu nhiên     34394     24402      9624      3077    249.5      476        0      576     2417    10544
ngẫu/tăng      2.04x     3.12x     6.79x    18.58x    8.29x    1.62x        —    1.58x49182.00x   32.35x
  (cột h/s: tỉ số là tăng/ngẫu — bao nhiêu lần NHANH hơn; các cột còn lại: ngẫu/tăng)

-- maria
khoá           ¼ h/s     ½ h/s     ¾ h/s   4/4 h/s   tổng s  bảng MB   idx MB   log MB   đọc MB   ghi MB
tăng dần      120447    129994    113308    132624     16.2      294        0      346        0      260
ngẫu nhiên    100455     46062     12306      4127    177.6      420        0      560     2923     6857
ngẫu/tăng      1.20x     2.82x     9.21x    32.14x   10.98x    1.43x        —    1.62x178391.00x   26.33x
  (cột h/s: tỉ số là tăng/ngẫu — bao nhiêu lần NHANH hơn; các cột còn lại: ngẫu/tăng)
```

Lượt 2 (đã sửa cách in, còn lại giữ nguyên):

```console
$ go run . -work pkorder
== 3. PK tăng dần vs ngẫu nhiên (2000000 hàng, khoá 16 byte, lô 1000 hàng/commit) ==

-- pg
khoá           ¼ h/s     ½ h/s     ¾ h/s   4/4 h/s   tổng s  bảng MB   idx MB   log MB   đọc MB   ghi MB
tăng dần      155087    130936    131642    110392     15.4      298       63      504        1       87
ngẫu nhiên     90798     84641     74071     52035     27.8      298       80      533        3      119
ngẫu/tăng      1.71x     1.55x     1.78x     2.12x    1.81x    1.00x    1.27x    1.06x    3.03x    1.36x
  (cột h/s: tỉ số là tăng/ngẫu — bao nhiêu lần NHANH hơn; các cột còn lại: ngẫu/tăng)

-- mysql
khoá           ¼ h/s     ½ h/s     ¾ h/s   4/4 h/s   tổng s  bảng MB   idx MB   log MB   đọc MB   ghi MB
tăng dần       47207     46655     47404     43829     43.3      294        0      366        0      336
ngẫu nhiên     33352     20459      8045      2757    282.9      476        0      576     2460    10680
ngẫu/tăng      1.42x     2.28x     5.89x    15.90x    6.54x    1.62x      0→0    1.58x   0→2460   31.79x
  (cột h/s: tỉ số là tăng/ngẫu — bao nhiêu lần NHANH hơn; các cột còn lại: ngẫu/tăng)

-- maria
khoá           ¼ h/s     ½ h/s     ¾ h/s   4/4 h/s   tổng s  bảng MB   idx MB   log MB   đọc MB   ghi MB
tăng dần       96105     89449    102794    135700     19.3      294        0      346        0      261
ngẫu nhiên     95517     53257     10802      3976    186.7      418        0      559     2945     6916
ngẫu/tăng      1.01x     1.68x     9.52x    34.13x    9.65x    1.43x      0→0    1.61x   0→2945   26.50x
  (cột h/s: tỉ số là tăng/ngẫu — bao nhiêu lần NHANH hơn; các cột còn lại: ngẫu/tăng)
```

Postgres ra 1.02x ở lượt 1 và 1.81x ở lượt 2, dao động quá lớn nên chạy riêng thêm ba lượt:

```console
$ for i in 1 2 3; do go run . -work pkorder -db pg | grep -E "tăng dần|ngẫu nhiên|ngẫu/tăng"; done
== 3. PK tăng dần vs ngẫu nhiên (2000000 hàng, khoá 16 byte, lô 1000 hàng/commit) ==
tăng dần       99438    101149    106993     57969     23.3      298       63      504        1       89
ngẫu nhiên     77492     96635     60225     80098     26.2      298       81      534        2      104
ngẫu/tăng      1.28x     1.05x     1.78x     0.72x    1.12x    1.00x    1.29x    1.06x      0→2    1.17x
  (cột h/s: tỉ số là tăng/ngẫu — bao nhiêu lần NHANH hơn; các cột còn lại: ngẫu/tăng)

== 3. PK tăng dần vs ngẫu nhiên (2000000 hàng, khoá 16 byte, lô 1000 hàng/commit) ==
tăng dần      144688    151140    150099    126088     14.1      298       63      504        1       91
ngẫu nhiên    120350    114849    101371     58308     22.0      298       81      534        3      112
ngẫu/tăng      1.20x     1.32x     1.48x     2.16x    1.57x    1.00x    1.29x    1.06x    2.27x    1.23x
  (cột h/s: tỉ số là tăng/ngẫu — bao nhiêu lần NHANH hơn; các cột còn lại: ngẫu/tăng)

== 3. PK tăng dần vs ngẫu nhiên (2000000 hàng, khoá 16 byte, lô 1000 hàng/commit) ==
tăng dần      151486    129671    141921     98119     15.8      298       63      504        1       80
ngẫu nhiên    111479    113487    104094     70026     20.8      298       81      533        3      125
ngẫu/tăng      1.36x     1.14x     1.36x     1.40x    1.32x    1.00x    1.28x    1.06x      0→3    1.57x
  (cột h/s: tỉ số là tăng/ngẫu — bao nhiêu lần NHANH hơn; các cột còn lại: ngẫu/tăng)
```

**Đọc kết quả:** dự đoán đúng, và InnoDB khớp với minidb tới mức đáng ngạc nhiên:

| | Postgres (5 lượt) | MySQL (2 lượt) | MariaDB (2 lượt) | minidb (phase 4) |
|---|---|---|---|---|
| tổng thời gian, ngẫu/tăng | **1.02–1.81x** (trung vị 1.32x) | **8.29x / 6.54x** | **10.98x / 9.65x** | — |
| tốc độ phần tư cuối, tăng/ngẫu | 0.72–2.16x | 18.58x / 15.90x | 32.14x / 34.13x | — |
| page ghi xuống đĩa, ngẫu/tăng | 1.17–1.57x | **32.35x / 31.79x** | **26.33x / 26.50x** | **33x** (writes/op) |
| kích thước, ngẫu/tăng | heap 1.00x, index 1.26–1.29x | **1.62x** | **1.43x** | lá đặc ~70% vs ~100% |
| WAL / redo, ngẫu/tăng | 1.06x | 1.58x | 1.61x | — |
| MB đọc từ đĩa khi nạp | 1–3 | 0 → **2460** | 0 → **2945** | — |

1. **InnoDB ghi page nhiều hơn 26–32x khi khoá ngẫu nhiên, minidb là 33x.** Cùng kiến trúc
   (clustered B+Tree, buffer pool nhỏ hơn cây) cho cùng tỉ số. Khoá tăng dần chỉ làm bẩn leaf cực
   phải; khoá ngẫu nhiên làm bẩn một leaf khác ở mỗi lần chèn, và khi cây lớn hơn buffer pool, mỗi
   leaf bẩn bị đẩy ra đĩa rồi lại phải đọc vào: 2.4–2.9GB đọc từ đĩa để nạp 300MB dữ liệu.
2. **Tốc độ không tụt đều mà tụt theo bậc, đúng lúc bảng tràn buffer pool.** Phần tư đầu chênh
   1.0–1.4x (còn vừa RAM), phần tư cuối chênh 16–34x. Trên máy dev với bảng nhỏ bạn sẽ không bao
   giờ thấy vấn đề này. Nó chỉ hiện ra trên production, khi bảng lớn hơn RAM.
3. **Bảng to hơn 1.43–1.62x chỉ vì thứ tự khoá.** Khoá tăng dần được InnoDB tách page ở điểm chèn
   (giống `RightmostSplit` của minidb); khoá ngẫu nhiên thì tách 50/50, và page chỉ đầy dần trở lại
   tới khoảng 70%.
4. **Postgres gần như miễn nhiễm, nhưng chỉ ở quy mô này.** Heap của Postgres là đống không thứ
   tự: hàng mới luôn được thêm vào cuối, bất kể PK. Chỉ index PK bị chèn lung tung (to hơn ~28%),
   và index đó (80MB) vẫn vừa `shared_buffers`. Khi riêng index PK lớn hơn RAM, Postgres sẽ gặp
   lại đúng vấn đề này, chỉ muộn hơn và ở cấu trúc nhỏ hơn. Chưa đo được (nợ P9-3): cần giới hạn
   cả page cache của OS, vì Postgres đọc qua page cache chứ không đọc thẳng từ đĩa.

**Đang nghĩ gì:** đây là thí nghiệm có giá trị thực tế nhất cho tới giờ, vì nó trả lời một câu
tranh luận có thật ("dùng UUID làm PK có sao không?") bằng một câu trả lời phụ thuộc vào kiến
trúc: **trên InnoDB thì có, và rất nặng; trên Postgres thì nhẹ hơn nhiều.** Cách chữa ở cả hai DB
đều là giữ thứ tự: UUIDv7, hoặc PK tự tăng kèm một cột UUID riêng cho bên ngoài dùng.

### 2026-09-29 — bảng 4, lượt đầu: history list chỉ lên tới 21

Kịch bản: một phiên mở transaction `REPEATABLE READ` rồi để đó (một báo cáo chạy lâu, hoặc một
cửa sổ `psql` quên `COMMIT`), trong khi ứng dụng vẫn `UPDATE` cả bảng 100000 hàng, 10 vòng. Đo
sau mỗi vòng: kích thước bảng, số version chết (`pgstattuple`), thời gian `SELECT sum(v)` của một
người đọc **mới** và của chính phiên **cũ**, và "History list length" của InnoDB.

**Dự đoán (comment trong `bloat.go`):** Postgres để version cũ ngay trong heap nên bảng phình và
**mọi** người đọc cùng chậm; InnoDB để version cũ trong undo nên bảng không đổi, người đọc mới
không sao, chỉ người đọc cũ phải đi ngược chuỗi undo.

```console
$ go run . -work bloat
== 4. một transaction mở lâu, 100000 hàng, mỗi vòng UPDATE toàn bảng ==

-- pg
vòng      bảng MBversion chết    đọc MỚI ms     đọc CŨ ms    history list
0            14.1           0           4.2           4.1               —
1            28.3      100000           9.0           9.1               —
2            42.4      200000           9.2          10.0               —
5            84.7      500000          12.9          13.2               —
10          155.4     1000000          24.1          26.3               —
sau COMMIT phiên cũ + VACUUM (101ms):
sau         155.4           0          11.1             —               —

-- mysql
vòng      bảng MBversion chết    đọc MỚI ms     đọc CŨ ms    history list
0            14.2           —          13.2          13.6               3
1            14.2           —          13.8          71.9               6
2            14.2           —          12.5         105.6               9
5            14.2           —          13.0         227.0              14
10           14.2           —          13.1         628.1              21
sau COMMIT phiên cũ, purge nền chạy xong trong 0s:
sau          14.2           —          11.0             —              23

-- maria
vòng      bảng MBversion chết    đọc MỚI ms     đọc CŨ ms    history list
0            14.2           —          13.4          13.7               5
1            14.2           —          13.3          37.1               3
2            14.2           —          13.9          55.7               5
5            14.2           —          13.4         108.6               9
10           14.2           —          17.5         245.0              15
sau COMMIT phiên cũ, purge nền chạy xong trong 0s:
sau          14.2           —          17.2             —               5
```

**Đọc kết quả:** ba cột đầu khớp dự đoán. Cột history list thì không: 10 vòng cập nhật tổng cộng
1 triệu hàng, mà history list chỉ lên tới **21**, và điều kiện "purge xong" (`< 1000`) đã đúng
ngay từ đầu, nên con số "0s" vô nghĩa.

**Nguyên nhân:** history list length đếm **số transaction** đã commit mà undo chưa được dọn,
không đếm số hàng. Mỗi vòng ở đây là **một** câu `UPDATE` cả bảng, tức một transaction. 10 vòng
thì được 10 mục, cộng thêm vài mục nền.

**Đã sửa:** mỗi vòng chia thành 100 transaction × 1000 hàng, giống cách ứng dụng thật cập nhật
dữ liệu. Thêm cột dung lượng file undo, và thêm một dòng `VACUUM FULL` cho Postgres.

### 2026-09-29 — bảng 4, lượt chạy thật

```console
$ go run . -work bloat
== 4. một transaction mở lâu; 100000 hàng, mỗi vòng UPDATE toàn bảng bằng 100 transaction × 1000 hàng ==

-- pg
vòng      bảng MB  version chết  đọc MỚI ms   đọc CŨ ms  history list   undo MB
0            14.1             0         3.9         3.9             —         —
1            28.3        100000         9.6         8.1             —         —
2            42.4        200000         8.1         8.9             —         —
5            84.7        500000        19.7        15.4             —         —
10          155.4       1000000        19.9        23.1             —         —
sau COMMIT phiên cũ + VACUUM (93ms):
sau         155.4             0        10.8           —             —         —
sau VACUUM FULL (229ms, khoá ACCESS EXCLUSIVE cả bảng trong lúc chạy):
sau          14.1             0         7.0           —             —         —

-- mysql
vòng      bảng MB  version chết  đọc MỚI ms   đọc CŨ ms  history list   undo MB
0            14.2             —        12.5        11.8            33      83.9
1            14.2             —        11.4        59.5           103      83.9
2            14.2             —        11.6       102.8           205      83.9
5            14.2             —        11.9       216.6           507      83.9
10           14.2             —        11.4       410.7          1009      83.9
sau COMMIT phiên cũ, purge nền chạy xong trong 8s:
sau          14.2             —         8.0           —             2      83.9

-- maria
vòng      bảng MB  version chết  đọc MỚI ms   đọc CŨ ms  history list   undo MB
0            14.2             —         9.6         9.4             3      54.5
1            14.2             —         9.6        25.6           102      54.5
2            14.2             —         9.3        40.6           203      54.5
5            14.2             —         9.6        77.5           504      55.6
10           14.2             —         9.6       173.8          1005      68.2
sau COMMIT phiên cũ, purge nền chạy xong trong 200ms:
sau          14.2             —        13.6           —             0      68.2
```

**Đọc kết quả:** dự đoán đúng từng vế, và mỗi DB bắt **một nhóm người khác nhau** trả giá:

| sau 10 vòng (1 triệu version cũ) | Postgres | MySQL | MariaDB | minidb (phase 6) |
|---|---|---|---|---|
| bảng phình | **11.0x** (14 → 155MB) | 1.0x | 1.0x | 18.07x (chuỗi trong record) |
| người đọc MỚI chậm đi | **5.1x** (3.9 → 19.9ms) | 0.9x (không đổi) | 1.0x (không đổi) | cũng phải giải mã chuỗi (P6-2) |
| người đọc CŨ chậm đi | 5.9x | **34.8x** (11.8 → 410.7ms) | **18.5x** (9.4 → 173.8ms) | — |
| sau khi phiên cũ đóng | `VACUUM` 93ms dọn hết version chết nhưng **bảng vẫn 155MB**, đọc vẫn chậm 2.8x | purge nền dọn trong **8s** | purge nền dọn trong **200ms** | `Vacuum()` thủ công |
| muốn lấy lại dung lượng | `VACUUM FULL`: viết lại cả bảng, **khoá ACCESS EXCLUSIVE** | không cần | không cần | — |

1. **Postgres: mọi người cùng trả giá.** Version cũ là tuple đầy đủ nằm ngay trong heap, nên mọi
   lần quét, kể cả của người vừa kết nối, đều phải lội qua 1 triệu xác hàng. Mỗi vòng thêm ~14MB,
   đúng bằng kích thước cả bảng: `UPDATE` ở Postgres **chép cả hàng** dù chỉ đổi một cột `int`.
2. **InnoDB: chỉ phiên cũ trả giá.** Bản mới nhất vẫn nằm tại chỗ trong clustered index, nên người
   đọc mới không hề biết có chuyện gì. Phiên cũ thì phải dựng lại bản cũ bằng cách áp ngược từng
   bản ghi undo, và chuỗi đó dài thêm sau mỗi vòng.
3. **Undo của InnoDB chỉ ghi phần thay đổi.** 1 triệu version cũ mà file undo của MariaDB chỉ tăng
   13.7MB (54.5 → 68.2), còn của MySQL không tăng vì còn dư chỗ từ các thí nghiệm trước. Heap của
   Postgres tăng 141MB cho cùng số version, gấp khoảng 10 lần.
4. **`VACUUM` không làm bảng nhỏ lại.** Nó chỉ đánh dấu chỗ trống để tái sử dụng. Bảng 155MB vẫn
   là 155MB, và seq scan vẫn chậm 2.8x vì phải đọc qua các page trống. Muốn co lại phải dùng
   `VACUUM FULL`, tức viết lại cả bảng dưới khoá độc quyền. Trên production, đó là downtime.
5. **Purge của MariaDB nhanh hơn MySQL 40 lần** (200ms so với 8s) trên cùng lượng việc. Đây là
   một chỗ MariaDB đã sửa lại InnoDB từ 10.6 trở đi, chưa đào sâu (nợ P9-4).

**Đang nghĩ gì:** minidb đứng ở phía Postgres (mọi người đọc cùng trả giá) nhưng còn tệ hơn: nó
không chỉ lưu version cũ cạnh bản mới, mà lưu cả chuỗi **trong cùng một record**. Lời giải của
InnoDB (bản mới tại chỗ, bản cũ ra undo, chỉ ghi phần thay đổi) chính là cách trả nợ P6-1 +
P6-2 đã ghi trong `docs/vs-innodb.md`. Giờ nó có số đo đi kèm.

### 2026-09-29 — bảng 5, lượt đầu: MySQL sinh sai dữ liệu, và buffer pool tràn

Ba ca, mỗi ca đánh vào một giả định của bộ ước lượng số hàng:

- **lệch:** `status` gồm 98% `'done'`, 1% `'pending'`, 1% `'failed'`. Đánh vào giả định *phân bố đều*.
- **tương quan:** `city` quyết định `country` (`country = 'k' || city % 10`). Đánh vào giả định *độc lập*.
- **cũ:** `ANALYZE` lúc bảng có 1000 hàng toàn `'done'`, rồi nạp thêm mà không cho DB tự
  `ANALYZE` lại (tắt autovacuum / `STATS_AUTO_RECALC=0` cho riêng bảng đó). Đánh vào giả định
  *thống kê còn mới*.

```console
$ go run . -work stats
== 5. planner ước lượng sai số hàng (1000000 hàng) ==

-- pg
ca                                   ước lượng      thật     lệch  plan                               ms
lệch: status='pending' (1%)              10367      9995     ÷1.0  Index Scan st_status             22.1
lệch: status='done' (98%)               978833    979972     ×1.0  Seq Scan                        170.2
tương quan: city='c7' AND country='k7'       964      9982    ×10.4  Bitmap Heap Scan st_city         35.2
tương quan: city='c7' AND country='k8'       987         0   ÷987.0  Bitmap Heap Scan st_city          9.2
cũ: status='pending' (50%)                   1    500000×500000.0  Index Scan st2_status           142.3
sau khi chữa: CREATE STATISTICS (city, country) + ANALYZE st2
tương quan: city='c7' AND country='k7'     10267      9982     ÷1.0  Bitmap Heap Scan st_city         14.1
tương quan: city='c7' AND country='k8'         1         0     ×1.0  Bitmap Heap Scan st_city         10.3
cũ: status='pending' (50%)              496463    500000     ×1.0  Bitmap Heap Scan st2_status     108.0

-- mysql
ca                                   ước lượng      thật     lệch  plan                               ms
lệch: status='pending' (1%)              37458     20056     ÷1.9  Index lookup st_status         5307.0
lệch: status='done' (98%)               483334    979750     ×2.0  Index lookup st_status         3419.0
tương quan: city='c7' AND country='k7'      1005      1016     ×1.0  Index lookup st_city           1230.0
tương quan: city='c7' AND country='k8'      1005       991     ÷1.0  Index lookup st_city           1253.0
cũ: status='pending' (50%)              500500    500000     ÷1.0  Index lookup st2_status        1052.0
sau khi chữa: histogram trên country + ANALYZE st2
tương quan: city='c7' AND country='k7'      1005      1016     ×1.0  Index lookup st_city           1122.0
tương quan: city='c7' AND country='k8'      1005       991     ÷1.0  Index lookup st_city            675.0
cũ: status='pending' (50%)              476866    500000     ×1.0  Index lookup st2_status         991.0

-- maria
ca                                   ước lượng      thật     lệch  plan                               ms
lệch: status='pending' (1%)              19928      9965     ÷2.0  ref st_status                  3745.1
lệch: status='done' (98%)              1022894    979777     ÷1.0  ALL                            2478.6
tương quan: city='c7' AND country='k7'     10030     10030     ×1.0  ref st_city                    1236.1
tương quan: city='c7' AND country='k8'     10030         0 ÷10030.0  ref st_city                    1240.9
cũ: status='pending' (50%)             1001000    500000     ÷2.0  ALL                             896.1
sau khi chữa: ANALYZE ... PERSISTENT FOR ALL (histogram) + ANALYZE st2
tương quan: city='c7' AND country='k7'      1001     10030    ×10.0  ref st_city                    1271.2
tương quan: city='c7' AND country='k8'      1003         0  ÷1003.0  ref st_city                    1192.1
cũ: status='pending' (50%)             1038430    500000     ÷2.1  ALL                             851.6
```

**Đọc kết quả:** trước khi đọc tới planner, số hàng **thật** của MySQL đã sai. `'pending'` có
20056 hàng (phải khoảng 1%, tức ~10000), và `city='c7' AND country='k8'` có 991 hàng, trong khi
theo cách sinh dữ liệu thì phải là 0. MariaDB chạy cùng câu `INSERT` lại cho 9965 và 0.

**Nguyên nhân:** MySQL *merge* derived table vào câu ngoài (`derived_merge`), nên mỗi lần câu ngoài
nhắc tới `r1` hay `c` là một lần gọi **mới** `RAND()`. `CASE WHEN r1 < 0.98 … WHEN r1 < 0.99`
gọi hai lần, nên P('pending') = 0.02 × 0.99 ≈ 2%. `city` và `country` cũng được sinh bằng hai lần
gọi khác nhau, nên thành độc lập. MariaDB không merge một derived table có hàm không tất định.

**Đã sửa:** thêm `LIMIT 18446744073709551615` vào derived table để chặn việc merge (chạy được trên
cả hai DB).

Chỗ lạ thứ hai: MySQL tra 20000 hàng qua index mất 5.3 giây. Nguyên nhân: `st` + `st2` (~300MB)
cộng các bảng còn sót từ thí nghiệm trước đã vượt buffer pool 256MB, nên truy vấn đọc từ đĩa.
Postgres không bị vì nó còn có page cache của OS phía sau. **Đã sửa:** dọn các bảng cũ, `st2`
chỉ nạp thêm 200000 hàng, và thêm một câu join để thấy hậu quả của ước lượng sai.

### 2026-09-29 — bảng 5, lượt chạy thật

(Cột `ms` là **một** lần `EXPLAIN ANALYZE`, không có lượt làm nóng. Nó chỉ dùng để thấy thứ tự
độ lớn, không dùng để so tỉ số.)

```console
$ go run . -work stats
== 5. planner ước lượng sai số hàng (1000000 hàng) ==

-- pg
ca                                       ước lượng      thật      lệch  plan                                 ms
lệch: status='pending' (1%)                   9600      9995      ×1.0  Index Scan st_status                6.2
lệch: status='done' (98%)                   980133    979972      ÷1.0  Seq Scan                          103.6
tương quan: city='c7' AND country='k7'         915      9982     ×10.9  Bitmap Heap Scan st_city            7.4
tương quan: city='c7' AND country='k8'         899         0    ÷899.0  Bitmap Heap Scan st_city            6.1
cũ: status='pending' (50%)                       1    100000 ×100000.0  Index Scan st2_status              14.3
cũ + JOIN st                                     1    100000 ×100000.0  Nested Loop st2_status             95.7
sau khi chữa: CREATE STATISTICS (city, country) + ANALYZE st2
tương quan: city='c7' AND country='k7'        9733      9982      ×1.0  Bitmap Heap Scan st_city            8.3
tương quan: city='c7' AND country='k8'           1         0      ×1.0  Bitmap Heap Scan st_city            6.9
cũ: status='pending' (50%)                  100091    100000      ÷1.0  Bitmap Heap Scan st2_status        14.0
cũ + JOIN st                                100091    100000      ÷1.0  Merge Join st2_pkey                58.7

-- mysql
ca                                       ước lượng      thật      lệch  plan                                 ms
lệch: status='pending' (1%)                  17934      9965      ÷1.8  Index lookup st_status            143.0
lệch: status='done' (98%)                   502702    979777      ×1.9  Index lookup st_status           1627.0
tương quan: city='c7' AND country='k7'        1003     10030     ×10.0  Index lookup st_city               76.2
tương quan: city='c7' AND country='k8'        1003         0   ÷1003.0  Index lookup st_city               17.0
cũ: status='pending' (50%)                  100500    100000      ÷1.0  Index lookup st2_status           128.0
cũ + JOIN st                                100500    100000      ÷1.0  Nested loop inner join            153.0
sau khi chữa: histogram trên country + ANALYZE st2
tương quan: city='c7' AND country='k7'        1003     10030     ×10.0  Index lookup st_city               18.5
tương quan: city='c7' AND country='k8'        1003         0   ÷1003.0  Index lookup st_city               16.5
cũ: status='pending' (50%)                   98865    100000      ×1.0  Index lookup st2_status            77.4
cũ + JOIN st                                 98865    100000      ×1.0  Nested loop inner join            107.0

-- maria
ca                                       ước lượng      thật      lệch  plan                                 ms
lệch: status='pending' (1%)                  19928      9965      ÷2.0  ref st_status                     475.5
lệch: status='done' (98%)                   966940    979777      ×1.0  ALL                               738.6
tương quan: city='c7' AND country='k7'       10030     10030      ×1.0  ref st_city                       242.0
tương quan: city='c7' AND country='k8'       10030         0  ÷10030.0  ref st_city                        13.7
cũ: status='pending' (50%)                  191358    100000      ÷1.9  ALL                                26.8
cũ + JOIN st                                191358    100000      ÷1.9  ref st2_status                    138.2
sau khi chữa: ANALYZE ... PERSISTENT FOR ALL (histogram) + ANALYZE st2
tương quan: city='c7' AND country='k7'        1001     10030     ×10.0  ref st_city                       308.5
tương quan: city='c7' AND country='k8'        1003         0   ÷1003.0  ref st_city                        14.0
cũ: status='pending' (50%)                  191358    100000      ÷1.9  ALL                                27.6
cũ + JOIN st                                191358    100000      ÷1.9  ref st2_status                    143.8
```

Ca `status='done'` của MySQL cần kiểm tay: 98% bảng mà vẫn chọn index.

```console
$ reallab/q.sh mysql <<< "SHOW INDEX FROM st WHERE Key_name='st_status'; EXPLAIN ANALYZE ...×2; ... IGNORE INDEX ...×2"
| st    |          1 | st_status |            1 | status      | A         |           2 | ...
-> Index lookup on st using st_status (status='done')  (cost=61044 rows=480758) (actual time=0.195..1337 rows=979777 loops=1)
-> Index lookup on st using st_status (status='done')  (cost=59514 rows=480758) (actual time=0.191..1377 rows=979777 loops=1)
-> Filter: (st.`status` = 'done')  (cost=99957 rows=480758) (actual time=0.0542..198 rows=979777 loops=1)
    -> Table scan on st  (cost=99957 rows=961517) (actual time=0.0501..128 rows=1e+6 loops=1)
-> Filter: (st.`status` = 'done')  (cost=99956 rows=480758) (actual time=0.0376..190 rows=979777 loops=1)
    -> Table scan on st  (cost=99956 rows=961517) (actual time=0.0352..122 rows=1e+6 loops=1)
```

**Đọc kết quả:** mỗi DB sai ở một chỗ khác nhau, và chỗ sai lộ ra đúng công cụ ước lượng của nó:

1. **Phân bố lệch: Postgres đúng, MySQL mắc đúng lỗi của minidb.** Postgres có danh sách
   *most common values* nên ước lượng cả 1% lẫn 98% chính xác. MySQL với truy cập `ref` (so bằng
   trên một index) không đi xuống cây để đếm, mà dùng **cardinality** của index: 2 giá trị khác
   nhau, nên mỗi giá trị được coi là chiếm N/2. Đó **chính là giả định phân bố đều** của minidb
   (nợ P7-6). Hậu quả: với 98% bảng, MySQL chọn index lookup và chạy **1337–1377ms**, trong khi quét
   cả bảng chỉ mất **190–198ms**. Chậm hơn 7x chỉ vì một ước lượng.
2. **Tương quan: cả ba DB đều sai ~10x theo mặc định**, vì đều nhân selectivity của hai điều kiện
   như thể chúng độc lập (1% × 10% = 0.1%, thật là 1%). Cách chữa của từng DB:
   - **Postgres chữa được** bằng `CREATE STATISTICS … (dependencies, mcv)`, sau đó ước lượng đúng
     cả ca có hàng (9733 so với thật 9982) lẫn ca 0 hàng (1).
   - **MySQL không chữa được**: histogram là cho từng cột, và thêm histogram cho `country` không
     đổi được gì, vì cái sai nằm ở phép nhân chứ không nằm ở từng thừa số.
   - **MariaDB tệ hơn sau khi chữa.** Trước khi có histogram, nó không ước lượng điều kiện
     `country` (coi như 100%), nên tình cờ đúng ở ca `k7` (10030 = 10030). Sau
     `ANALYZE … PERSISTENT FOR ALL`, nó biết `country='k7'` chiếm 10%, nhân vào, và **sai 10x**.
     Thêm thống kê làm ước lượng xấu đi, vì thống kê mới bị dùng đúng vào chỗ giả định sai.
3. **Thống kê cũ: chỉ Postgres bị lừa, và hậu quả hiện ra ở join.** Postgres tin
   `pg_statistic`: `'pending'` không có trong danh sách giá trị lúc `ANALYZE`, nên ước lượng
   **1 hàng**, thật là 100000. Với 1 hàng thì nested loop là lựa chọn hợp lý; sau `ANALYZE` thì nó
   đổi sang merge join (95.7 → 58.7ms). MySQL và MariaDB **không bị lừa** dù `STATS_AUTO_RECALC=0`:
   với điều kiện `range`/`ref` trên cột có index, chúng **đi xuống index để đếm** (*index dive*),
   nên số liệu luôn mới. Đây là cách trả nợ P7-6 rẻ nhất cho minidb, như `docs/vs-innodb.md` đã đoán.

**Đang nghĩ gì:** không có DB nào đúng ở cả ba ca. Postgres mạnh về thống kê (MCV, histogram,
thống kê đa cột) nhưng tin chúng mù quáng khi chúng đã cũ. InnoDB đếm thật khi có thể (index
dive) nhưng quay về giả định đều khi không thể. Bài học cho người dùng: khi một câu truy vấn đột
nhiên chậm, việc đầu tiên là so **ước lượng với thực tế** trong `EXPLAIN ANALYZE`. Lệch 10x trở
lên thì planner đang đoán mò, và chỗ cần sửa là thống kê, không phải câu SQL.

---

## Giả thuyết sai

| Tôi tưởng là | Thực tế là | Lệnh + output đã lật tẩy | Đã sửa thế nào |
|---|---|---|---|
| *(phase 7)* Hoà vốn 36.8% thay vì 5-20% **vì ở quy mô này không có I/O thật** | DB thật cũng chạy trọn trong RAM mà vẫn hoà vốn ở **4.7 / 4.9 / 7.0%**. Phí *tra* của minidb ≈ InnoDB; phí *quét* của minidb đắt gấp 4x InnoDB và 15x Postgres. Chính phí quét đẩy điểm hoà vốn lên | `go run . -work breakeven -repeat 9` + bảng tách phí mỗi hàng: `minidb 480 / 2142`, `postgres 31 / 725`, `mysql 113 / 2060` | Sửa dòng phase 7 trong ROADMAP; thêm nợ đo phí quét |
| *(bug của bộ đo)* `SET enable_*` rồi chạy câu lệnh ⇒ đo được plan đã ép | `pgx` prepare và cache câu lệnh theo chuỗi SQL; generic plan lập ở lượt đầu được dùng lại cho cả ba cột | Ba cột Postgres gần như bằng nhau (52.8 / 55.3 / 57.5ms ở 0.1%), trong khi `psql` đo index scan chỉ 9.3ms | `default_query_exec_mode=simple_protocol` |
| *(bug của bộ đo)* Chờ bước trước của A xong rồi mới gửi bước kế | Bước trước của A đang chờ khoá của B, còn B đang chờ bộ đo gửi lệnh: bộ đo tự gây deadlock, DB huỷ A sau lock timeout | Ô `non-repeatable-read` và `phantom` × serializable của InnoDB ra `.a` thay vì `.w` | Xếp hàng bước cho actor đang bị chặn; `finish()` gửi lệnh commit cho cả hai rồi mới chờ |
| "RepeatableRead chặn lost update" là một sự thật về mức isolation | Đó là sự thật về **một cách cài**. Postgres/minidb chặn (first-committer-wins), MySQL 8.4 để lọt (current read), MariaDB chặn hay không tuỳ `innodb_snapshot_isolation` | `go run . -work anomaly`: ô lost-update × repeat-read = `.a` / `X` / `.a`; tắt biến trên MariaDB thì ra `X` | Thêm ô "RR kiểu MySQL" vào hiểu biết của phase 6; nợ P9-2 |
| *(bug của bộ đo)* History list length đếm số version cũ còn giữ | Nó đếm số **transaction** đã commit mà undo chưa dọn. 10 câu `UPDATE` cả bảng thì ra ~10, dù có 1 triệu version cũ | Lượt đầu bảng 4: history list lên tới **21** sau 1 triệu hàng bị cập nhật | Cập nhật bằng 100 transaction × 1000 hàng mỗi vòng: history list ra 1009 |
| *(bug của bộ đo)* `INSERT … SELECT … FROM (SELECT RAND() r1 …) s` gọi `RAND()` một lần mỗi hàng | MySQL merge derived table vào câu ngoài, nên mỗi lần nhắc `r1` là một lần gọi `RAND()` mới | MySQL: `'pending'` = 20056 (2%), `city='c7' AND country='k8'` = 991 hàng; MariaDB cùng câu cho 9965 và 0 | `LIMIT 18446744073709551615` trong derived table |
| Thêm thống kê thì ước lượng chỉ có thể tốt lên | MariaDB: trước histogram ước lượng `city AND country` đúng (10030/10030) vì bỏ qua `country`; sau histogram sai **10x** (1001/10030) vì nhân như thể độc lập | `go run . -work stats`, khối "sau khi chữa" của maria | Không sửa; đây là hành vi của DB, ghi vào blog bài 9 |

---

## Số đo

Tất cả đo ngày 2026-09-29, trên máy ở mục Môi trường, tại commit của phase này. Lệnh ở từng mục
nhật ký bên trên.

| Câu hỏi | minidb | Postgres 17 | MySQL 8.4 | MariaDB 11.8 |
|---|---|---|---|---|
| hoà vốn index vs seq (dữ liệu trong RAM) | 36.8% | **4.7%** | **4.9%** | **7.0%** |
| phí tra / phí quét một hàng | 4.5x | 23.5x | 18.3x | 15.8x |
| lost update ở repeatable-read | chặn | chặn (`40001`) | **lọt** | chặn (`1020 HY000`) |
| serializable chặn bằng | khoá (S2PL) | huỷ (SSI) | khoá + deadlock | khoá + deadlock |
| UUIDv4 / tăng dần: tổng thời gian nạp | — | 1.02–1.81x | **6.54–8.29x** | **9.65–10.98x** |
| UUIDv4 / tăng dần: page ghi xuống đĩa | 33x | 1.17–1.57x | **31.79–32.35x** | **26.33–26.50x** |
| 1 triệu version cũ: bảng phình | 18.07x | **11.0x** | 1.0x | 1.0x |
| 1 triệu version cũ: người đọc MỚI chậm | — | **5.1x** | 1.0x | 1.0x |
| 1 triệu version cũ: người đọc CŨ chậm | — | 5.9x | **34.8x** | **18.5x** |
| ước lượng sai tối đa theo mặc định (lệch / tương quan / cũ) | phân bố đều | 1.0 / 10.9 / **100000x** | 1.9 / 10.0 / 1.0 | 2.0 / 1.0–10030 / 1.9 |

## Khẳng định + lệnh kiểm chứng

| Khẳng định | Lệnh | Kết quả |
|---|---|---|
| Bộ đo ép được đúng plan mà nó nói | `go run . -work breakeven` | Postgres ba cột khác nhau (0.7 / 0.9 / 30.9ms ở 0.1%) sau khi dùng simple protocol |
| Ô `.w` là chờ thật, không phải bộ đo tự chặn | `go run . -work anomaly` | InnoDB serializable: `.w` ở 3 hàng đầu, `.a` chỉ khi deadlock thật (`Error 1213`) |
| Khác biệt lost-update MySQL/MariaDB là do `innodb_snapshot_isolation` | tắt biến rồi chạy lại `-db maria` | Đúng một ô đổi: `.a` → `X` |
| Tỉ số UUID của InnoDB bền qua các lượt | `go run . -work pkorder` ×2 | ghi page 32.35x / 31.79x (MySQL), 26.33x / 26.50x (MariaDB) |
| History list tăng theo số transaction, không theo số hàng | `go run . -work bloat` hai phiên bản | 10 transaction lớn → 21; 1000 transaction nhỏ → 1009 |
| MySQL dùng cardinality (giả định đều) cho truy cập `ref` | `SHOW INDEX` + `EXPLAIN ANALYZE` | `Cardinality 2`, ước lượng 480758 cho 979777 hàng thật; index 1337ms vs quét 190ms |

## Đọc gì

- PostgreSQL docs: *Row Estimation Examples* (MCV, histogram, `CREATE STATISTICS`), *Routine Vacuuming*.
- MySQL 8.4 Reference Manual: *InnoDB Multi-Versioning*, *Purge Configuration*, *Estimating Query Performance* (index dive, `eq_range_index_dive_limit`).
- MariaDB KB: `innodb_snapshot_isolation`, *Engine-independent Table Statistics*.

## Rút ra

**Kiến trúc quyết định ai trả giá.** Cùng một hành động (khoá ngẫu nhiên, một transaction quên
commit) mà hậu quả ở Postgres và ở InnoDB khác nhau về **bản chất**, không phải về mức độ.
UUIDv4 phạt InnoDB 6-11x vì cả bảng là cây PK, nhưng gần như không phạt Postgres, vì heap không
có thứ tự. Transaction mở lâu làm Postgres phình **cho mọi người** (version cũ nằm trong heap),
nhưng ở InnoDB chỉ phạt **chính phiên đó** (version cũ nằm trong undo). Muốn đoán một DB sẽ đau ở
đâu, hỏi: *nó để hàng ở đâu, và để version cũ ở đâu?* minidb trả lời được cả hai câu vì đã tự
đặt hai quyết định đó ở phase 4 và phase 6. Và số đo cho thấy nó trả lời đúng: InnoDB ghi page
nhiều hơn 26-32x khi khoá ngẫu nhiên, minidb là 33x.

**Một cái tên không phải là một định nghĩa.** "Repeatable read" chặn lost update ở Postgres và
minidb, để lọt ở MySQL, và ở MariaDB thì tuỳ một biến cấu hình đổi mặc định ở bản 11.6. Code
viết và test trên Postgres có thể mất tiền khi chạy trên MySQL mà không có lỗi nào được báo. Mã
lỗi cũng không thống nhất: MariaDB báo `1020 (HY000)` chứ không phải `40001`, nên code retry chỉ
bắt 40001 sẽ bỏ sót.

**Planner sai vì số hàng nhiều hơn vì chi phí.** Phase 7 đã thấy planner chọn sai vì một hằng số
chi phí. Ở đây cả ba DB chọn sai vì ước lượng số hàng: MySQL tin cardinality (giả định đều, y hệt
minidb) và chọn index cho 98% bảng, chậm 7x; Postgres tin thống kê đã cũ và ước lượng 1 hàng cho
100000 hàng thật; MariaDB được thêm histogram lại ước lượng tệ hơn, vì nó nhân như thể hai cột
độc lập. Không DB nào đúng ở cả ba ca. Cách tự vệ duy nhất có hiệu lực ở mọi DB là **so ước
lượng với thực tế** trong `EXPLAIN ANALYZE`.

**Một lời giải thích đúng số mà sai lý do vẫn là sai.** Phase 7 đo đúng 36.8% và giải thích bằng
"không có I/O". DB thật cũng không có I/O mà vẫn hoà vốn ở 5-7%. Nguyên nhân thật là seq scan
của minidb đắt gấp 15 lần Postgres cho mỗi hàng. Không có phép so với DB thật thì lời giải thích
kia sẽ nằm trong diary mãi, và nghe rất hợp lý.

**Bộ đo là chỗ sai nhiều nhất.** Năm lỗi của phase này đều nằm ở bộ đo, không có lỗi nào ở DB:
plan cache của driver, hai lần bộ lập lịch tự gây deadlock, hiểu sai đơn vị của history list,
`RAND()` bị gọi lại do merge derived table. Lỗi nào cũng cho ra một con số **trông hợp lý**. Cái
duy nhất bắt được chúng là quy tắc 5 của diary: viết tỉ số kỳ vọng trước, và nghi bộ đo khi số
lệch.

## Nợ kỹ thuật

- [ ] 📏 P9-1 · Đo thẳng phí quét một hàng của minidb, tách phần `keys.Decode` (P7-1) khỏi `DecodeChain` (P6-2)
- [ ] ⏳ P9-2 · `txnlab` chưa có ô "RR kiểu MySQL" (đọc snapshot, ghi trên bản mới nhất)
- [ ] 📏 P9-3 · Chưa đo UUID trên Postgres khi riêng index PK lớn hơn RAM (cần giới hạn cả page cache: `docker --memory`)
- [ ] 📏 P9-4 · Purge của MariaDB nhanh hơn MySQL 40x trên cùng lượng việc: chưa biết vì sao
- [ ] 🔧 P9-5 · Cột `ms` của bảng 5 là một lần `EXPLAIN ANALYZE`, không làm nóng, không lấy trung vị
