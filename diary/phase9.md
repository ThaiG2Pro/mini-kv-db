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
một câu hỏi ở trên, chạy trên cả ba DB. Bảng thứ sáu (`-work crash`, `kill -9`) và các script
trong `blog/lab/` được thêm lúc viết series blog.

## Reproduce toàn bộ phase

```bash
docker compose -f reallab/docker-compose.yml up -d
cd reallab
go run . -work breakeven -repeat 9     # bảng 1, ~3 phút
go run . -work anomaly                 # bảng 2
go run . -work pkorder                 # bảng 3
go run . -work bloat                   # bảng 4
go run . -work stats                   # bảng 5
go run . -work crash -rounds 5         # bảng 6, ~4 phút, giết và khởi động lại container
go run . -work lograte -db mysql,maria # bảng 7, ~15 giây
go run . -work purge -db mysql,maria   # bảng 11, ~6 phút
go run . -work hashjoin -db pg,pgm -repeat 11   # bảng 8, ~6 phút, cần: docker compose --profile p97 up -d pgm
../scripts/p97-hashjoin.sh             # bảng 8 phép E, chỉ trên Linux thuần (cần PMU + perf + sudo)
../scripts/p91-seqscan.sh              # bảng 9, A/B 348f120 ↔ HEAD, ~15 phút, cần máy rảnh
for f in ../blog/lab/0[1235]*-pg.sql ../blog/lab/08-index-pg.sql ../blog/lab/10-explain-pg.sql; do ./q.sh pg $f; done
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
   một chỗ MariaDB đã sửa lại InnoDB từ 10.6 trở đi, chưa đào sâu (nợ P9-4). **Bảng 11: phần lớn
   khoảng chênh là bộ đếm.** History list của MySQL chỉ rơi khi history được cắt (mỗi 128 lô purge),
   còn việc dọn thật xong sau ~2s.

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

### 2026-09-29 — bảng 6: `kill -9` giữa lúc ghi (thêm khi viết blog bài 5)

Cùng câu hỏi với `cmd/crashlab` của phase 5, trên DB thật (`reallab/crash.go`). Một goroutine chèn
`id = 1, 2, …` mỗi câu một commit, và ghi lại id lớn nhất đã được báo OK. Sau 1–3 giây,
`docker kill -s KILL`. Khởi động lại rồi đếm: "mất" = id ≤ lastAck mà không còn trong bảng (phải
bằng 0), "thừa" = id > lastAck mà có trong bảng (câu đang bay, hợp lệ). Mỗi DB có một chế độ
"không chờ log", đóng vai `crashlab -nowrite`: nó **phải** làm mất dữ liệu.

```console
$ go run . -work crash -rounds 5          # (lọc bỏ log "unexpected EOF" của driver mysql)
== 6. kill -9 giữa lúc ghi, 5 vòng mỗi chế độ ==

-- pg
chế độ                                    vòng   đã báo OK       mất      thừa   khởi động
mặc định (synchronous_commit=on)           5/5       12246         0         2       800ms
synchronous_commit=off                     0/5       34302      2400         0       600ms

-- mysql
chế độ                                    vòng   đã báo OK       mất      thừa   khởi động
mặc định (flush_log_at_trx_commit=1)       5/5        2515         0         3        4.1s
flush_log_at_trx_commit=2                  5/5        4397         0         4        2.8s
flush_log_at_trx_commit=0                  5/5        2847         0         5        2.9s

-- maria
chế độ                                    vòng   đã báo OK       mất      thừa   khởi động
mặc định (flush_log_at_trx_commit=1)       5/5        8879         0         4          4s
flush_log_at_trx_commit=2                  5/5       23878         0         1          2s
flush_log_at_trx_commit=0                  0/5       43427      9123         0        2.7s
```

**Đọc kết quả:** chế độ mặc định giữ đúng lời hứa ở cả ba DB (15/15). Phản chứng đỏ đúng chỗ ở
Postgres (`synchronous_commit=off` mất 2400) và MariaDB (`flush_log_at_trx_commit=0` mất 9123).
**MySQL ở `flush_log_at_trx_commit=0` thì không mất gì**, và tốc độ ghi cũng thấp ngang chế độ mặc
định. Theo quy tắc 5, đây là chỗ phải hỏi: phép phản chứng không đỏ thì hoặc bộ đo sai, hoặc có
một cơ chế khác đang che.

```console
$ echo "SELECT @@log_bin, @@sync_binlog, @@innodb_flush_log_at_trx_commit;" | ./q.sh mysql   # rồi maria
mysql: | 1 | 1 | 1 |
maria: | 0 | 0 | 1 |
```

MySQL 8 bật binlog với `sync_binlog=1`, MariaDB không bật binlog. Giả thuyết: có binlog thì commit
là 2PC, và bước flush của binlog group commit ghi redo xuống OS bất kể `flush_log_at_trx_commit`.
Thêm một chế độ tắt luôn cả `sync_binlog`:

```console
$ go run . -work crash -rounds 5 -db mysql
== 6. kill -9 giữa lúc ghi, 5 vòng mỗi chế độ ==

-- mysql
chế độ                                    vòng   đã báo OK       mất      thừa   khởi động
mặc định (flush_log_at_trx_commit=1)       5/5        3862         0         1        2.2s
flush_log_at_trx_commit=2                  5/5        5944         0         4        2.3s
flush_log_at_trx_commit=0                  5/5        4268         0         4        2.9s
flush_log_at_trx_commit=0 + sync_binlog=0     0/5       19304         6         0        2.9s
```

**Đọc kết quả:** giả thuyết chỉ đúng một nửa. Tắt fsync của binlog thì tốc độ tăng gấp 4.5 lần
(19304 so với 4268 commit đã báo OK) và **có mất dữ liệu**, nhưng chỉ 6 commit qua 5 vòng, so với
9123 của MariaDB. Vậy fsync của binlog đúng là đã che cho redo ở chế độ `=0`, nhưng khi bỏ lớp
che ấy, MySQL vẫn mất ít hơn MariaDB rất nhiều. Chưa biết vì sao (nợ P9-6). Nghi phạm là luồng
`log_writer` riêng của MySQL 8, liên tục ghi redo xuống OS thay vì mỗi giây một lần. **Đã xác
nhận ở bảng 7.**

Và lưu ý đã có từ phase 5: `kill -9` giết tiến trình chứ không giết page cache, nên chế độ
`flush_log_at_trx_commit=2` (write mỗi commit, fsync mỗi giây) xanh ở đây nhưng vẫn mất dữ liệu
khi mất điện thật.

### 2026-09-29 — bảng 7: trả nợ P9-6, redo được write() theo nhịp nào

Câu hỏi còn treo từ bảng 6: ở chế độ không chờ log, sau khi đã tắt cả `sync_binlog`, vì sao MySQL
chỉ mất 6 commit trong khi MariaDB mất 9123? `kill -9` chỉ xoá phần redo còn nằm trong RAM của
tiến trình, tức phần **chưa được write()**. Nên số commit mất mỗi lần kill phải xấp xỉ bằng
*tốc độ commit × độ dài khoảng thời gian từ lần write() gần nhất*. Bài `reallab/lograte.go` đo
độ dài khoảng đó: một kết nối chèn liên tục, kết nối khác đọc bộ đếm redo mỗi 5ms và ghi lại
những lúc bộ đếm nhảy.

**Kỳ vọng viết trước:** MySQL nhảy liên tục vì có luồng `log_writer` riêng; MariaDB nhảy mỗi
giây một lần (`innodb_flush_log_at_timeout=1`).

Lượt đầu đọc `Innodb_os_log_written` trên cả hai DB:

```console
$ go run . -work lograte -db mysql,maria
db      chế độ                                          commit/s  số write  khoảng p50  khoảng max
mysql   =0 + sync_binlog=0                                  4077       514       5.8ms       7.2ms
mysql   =0 + sync_binlog=0 + log_writer_threads=OFF         4194         8     172.8ms     826.9ms
maria   flush_log_at_trx_commit=0                           4366       526       5.6ms       7.4ms
```

MariaDB cũng "ghi mỗi 5.6ms", nhưng nếu vậy thì nó không thể mất 9123 commit. Theo quy tắc 5, mình
nghi bộ đo trước. Lấy mẫu tay trong lúc chèn:

```console
Innodb_lsn_current 3602546893  Innodb_lsn_flushed 3601694282  Innodb_os_log_written 3789950  38.077
Innodb_lsn_current 3603397116  Innodb_lsn_flushed 3601694282  Innodb_os_log_written 4640173  38.495
Innodb_lsn_current 3604430049  Innodb_lsn_flushed 3603725140  Innodb_os_log_written 5673106  38.897
```

Giữa hai mẫu đầu, `Innodb_os_log_written` tăng 850223 byte, **đúng bằng** mức tăng của
`Innodb_lsn_current` (850223 byte). Trên MariaDB bộ đếm này đếm redo được *sinh ra*, không đếm
redo được *ghi*. Bộ đếm đúng là `Innodb_lsn_flushed`, nhảy khoảng mỗi giây một lần. MariaDB mở
redo với `O_DIRECT` (`innodb_log_file_buffering=OFF`), nên write() và xuống đĩa là cùng một lần.
Sửa bộ đo để đọc `Innodb_lsn_flushed` trên MariaDB, rồi chạy lại hai lượt:

```console
$ go run . -work lograte -db mysql,maria        # ×2
db      chế độ                                          commit/s  số write  khoảng p50  khoảng max dự báo mất/kill
mysql   =0 + sync_binlog=0                                  3819       508       5.9ms       7.6ms            11
mysql   =0 + sync_binlog=0 + log_writer_threads=OFF         2516         8     184.2ms     818.6ms           844
maria   flush_log_at_trx_commit=0                           2897         3    1001.7ms    1003.2ms          1363

mysql   =0 + sync_binlog=0                                  2425       473       6.2ms      19.5ms             8
mysql   =0 + sync_binlog=0 + log_writer_threads=OFF         2295         9     189.7ms     799.4ms           767
maria   flush_log_at_trx_commit=0                           2001         3    1002.6ms    1007.1ms           857
```

"Dự báo mất/kill" = `rate × Σg² / (2·Σg)`. Lần kill rơi vào khoảng dài g với xác suất tỉ lệ với g,
và trong khoảng đó trung bình mất g/2. Khoảng 5.9ms của MySQL là giới hạn của nhịp lấy mẫu: MySQL
ghi *ít nhất* dày chừng đó.

Kiểm tra nhân quả: tắt luồng ghi log (`innodb_log_writer_threads=OFF`) thì dự báo nhảy từ ~10 lên
~800 commit mỗi lần kill. Thêm chế độ đó vào `-work crash` rồi giết thật:

```console
$ go run . -work crash -db mysql 2>&1 | grep -v packets.go     # lượt 2; lượt 1: OFF mất 2986 / 22559
chế độ                                    vòng   đã báo OK       mất      thừa   khởi động
mặc định (flush_log_at_trx_commit=1)       5/5        3464         0         4        2.5s
flush_log_at_trx_commit=2                  5/5        4715         0         4        2.4s
flush_log_at_trx_commit=0                  5/5        5070         0         5        2.4s
flush_log_at_trx_commit=0 + sync_binlog=0     0/5       24339         6         0        2.6s
=0 + sync_binlog=0 + writer_threads=OFF     0/5       21242      3447         0        2.6s
```

**Đọc kết quả:**

| | dự báo mất / kill | đo được mất / kill |
|---|---|---|
| MySQL, có `log_writer` | 8–11 | 6 / 5 = **1.2** (hai lượt đều ra 6) |
| MySQL, `log_writer_threads=OFF` | 767–844 | 2986 / 5 = 597, 3447 / 5 = **689** |
| MariaDB | 857–1363 ở 2000–2900 commit/s | 9123 / 5 = **1825** ở ~4300 commit/s (bảng 6) |

Đúng bậc ở cả ba dòng. Chỉ bằng một biến, MySQL chuyển từ "gần như không mất" sang "mất như
MariaDB": 3447 / 6 = **575x**. Vậy P9-6 đã trả. Khác biệt nằm ở **ai** gọi write(). MySQL 8 có luồng
`log_writer` riêng, ghi redo từ log buffer xuống OS ngay khi có dữ liệu mới, bất kể
`flush_log_at_trx_commit`. Biến này chỉ quyết định commit có **chờ** hay không. MariaDB không có
luồng đó: ở `=0`, redo nằm trong log buffer cho tới lần ghi mỗi giây của master thread.

Hai điều giữ lại cho blog:
1. `kill -9` đo được **write()**, không đo được **fsync**. MySQL `=0` mất rất ít khi tiến trình
   chết, nhưng mất điện thật vẫn mất tới 1 giây dữ liệu, vì luồng `log_flusher` chỉ fsync mỗi giây.
   Tài liệu của MySQL nói đúng về mất điện; thí nghiệm này chỉ cho thấy tiến trình chết là một
   trường hợp nhẹ hơn.
2. Cùng tên biến, cùng tên bộ đếm, hai DB cùng họ InnoDB mà nghĩa khác nhau:
   `Innodb_os_log_written` của MariaDB đếm LSN, còn của MySQL đếm byte đã write().

### 2026-09-29 — bảng 8: nợ P9-7, trả một nửa trên WSL2 (phần còn lại chờ Linux thuần)

Câu hỏi từ blog bài 10: vì sao hash join 16 batch (tràn ra file tạm) lại nhanh hơn 1 batch (bảng
băm 21.6MB nằm trọn trong RAM)? Bài `reallab/hashjoin.go`. Hai giả thuyết, mỗi cái một dự báo
khác nhau, viết **trước** khi đo:

- **H1, cache/TLB:** mỗi lần probe vào bảng băm lớn là một lần trượt cache. Dự báo: khoảng chênh
  (1 batch − 16 batch) **tăng tuyến tính theo số hàng probe**, và tắt page fault không xoá được nó.
- **H2, page fault:** glibc cấp khối lớn bằng `mmap` rồi trả lại cho OS sau mỗi câu, nên câu sau
  chạm page nào cũng dính fault. Dự báo: khoảng chênh đi cùng số minor fault, **không** phụ thuộc
  số hàng probe, và biến mất trên `rl-pgm` (cùng Postgres 17, `GLIBC_TUNABLES` giữ bộ nhớ lại).

`perf stat -e cache-misses` (cách trả nợ ghi trong debts) **không chạy được**: WSL2 không đưa
PMU vào VM, không có `/sys/bus/event_source/devices/cpu`. Nên chỉ đo được gián tiếp: page fault
đọc từ `/proc/<pid>/stat` của backend, và độ trễ bộ nhớ của máy bằng một microbenchmark (phép D).

Ba cái bẫy của bộ đo, sửa lần lượt trước khi có số dùng được:

1. `mmap_threshold=1GB` bị glibc **lặng lẽ bỏ qua**: trần của nó là 32MB (HEAP_MAX/2). `rl-pgm`
   vẫn dính 4000 fault mỗi câu cho tới khi đặt `33554432`.
2. Lọc `a.id <= P` để giảm số hàng probe thì planner **đổi phía băm** khi P nhỏ. Thay bằng bảng
   `hjp` (hj × 4, lọc `a.r <= k`) và in cột "phía băm" để chắc nó luôn là `b`.
3. Đọc page fault qua `docker exec … cat /proc/…` trước và sau mỗi câu: mỗi lần là một lần khởi
   động runc ngay trước câu đang đo, một dòng ra khoảng chênh **âm** (−27.8ms). Chuyển sang đọc
   `/proc` của host (đổi pid bằng dòng `NSpid`).

Sau đó số vẫn nhảy ±30% giữa hai lượt (cùng câu 337ms rồi 540ms). i5-1235U có 2 nhân P + 8 nhân
E, WSL2 không cho ghim nhân, máy đang dùng 1.4GB swap. Không giảm được nhiễu thì **trừ** nó đi: B
và C đo theo cặp, hai cấu hình chạy xen kẽ sát nhau, lấy trung vị của hiệu từng cặp.

```console
$ go run . -work hashjoin -db pg,pgm -repeat 11
== 8. hash join: vì sao tràn đĩa lại nhanh hơn (Postgres) ==

A. pg: build 500k hàng, probe 1M hàng, chỉ đổi work_mem (trung vị 11 lần)
  work_mem   batch    bucket     bộ nhớ        ms  page fault
       1MB      16     65536     1615kB     742.6         296
       2MB       8    131072     3225kB     505.2         535
       4MB       4    262144     6444kB     554.8        3969
       8MB       2    524288    12879kB     605.0        4064
      16MB       1    524288    21657kB     561.4        4091
      32MB       1    524288    21657kB     530.8        4095
     256MB       1    524288    21657kB     577.9        4103

A. pgm: build 500k hàng, probe 1M hàng, chỉ đổi work_mem (trung vị 11 lần)
  work_mem   batch    bucket     bộ nhớ        ms  page fault
       1MB      16     65536     1615kB     427.7          11
       2MB       8    131072     3225kB     827.8           3
       4MB       4    262144     6444kB     517.2           3
       8MB       2    524288    12879kB     519.3           3
      16MB       1    524288    21657kB     510.2           3
      32MB       1    524288    21657kB     757.6           3
     256MB       1    524288    21657kB     544.4           3

B. pg: build 500k hàng cố định, đổi số hàng probe; cặp (1MB, 256MB) × 11
   probe  phía băm     1MB ms    256MB ms    chênh ms     ns/probe  fault 256MB
      1M         b      452.3       562.5        78.4         78.4         4091
      2M         b      671.9       860.7       191.1         95.6         4079
      3M         b      920.4      1250.3       275.4         91.8         4095
      4M         b     1106.3      1485.8       390.7         97.7         4079

B. pgm: build 500k hàng cố định, đổi số hàng probe; cặp (1MB, 256MB) × 11
   probe  phía băm     1MB ms    256MB ms    chênh ms     ns/probe  fault 256MB
      1M         b      522.9       611.4        78.0         78.0            3
      2M         b     1004.4      1032.7       136.0         68.0            3
      3M         b      862.2      1092.1       201.3         67.1            3
      4M         b     1125.7      1497.9       332.4         83.1            3

C. cùng câu, cùng work_mem, chỉ khác malloc: cặp (pg, pgm) × 11, probe 1M
  work_mem      pg ms     pgm ms    chênh ms   fault pg  fault pgm
       1MB      497.9      485.5         6.7         67          3
     256MB      555.9      535.2       -22.9       4079          3

E. bỏ qua: máy này không có PMU (WSL2, VM không bật vPMU) — không đếm được cache miss.
   Chạy trên Linux thuần: scripts/p97-hashjoin.sh

D. đọc ngẫu nhiên, độc lập, vào mảng uint64 (như probe vào mảng bucket)
      mảng    độc lập ns dây chuyền ns
      64KB          0.71          4.11
     256KB          0.78          6.32
     512KB          0.92          8.36
    1024KB          0.99          8.41
    2048KB          1.72         18.09
    4096KB          2.19         32.77
    8192KB          3.21         79.69
   16384KB          5.63        108.38
   32768KB          9.39        141.97
   65536KB         21.57        138.88
```

**Đọc kết quả:**

| | H1 dự báo | H2 dự báo | đo được |
|---|---|---|---|
| chênh theo số hàng probe (B) | tăng tuyến tính | phẳng | 78 → 191 → 275 → 391ms: **tuyến tính**, 78–98 ns/probe |
| tắt page fault (B trên pgm) | vẫn còn | mất | fault 4091 → 3, chênh vẫn 78 → 332ms, 67–83 ns/probe |
| cùng câu 256MB, pg − pgm (C) | — | = toàn bộ khoảng chênh | **23ms**, trong khi khoảng chênh ở 1M probe là 78ms |

1. **H2 đúng nhưng nhỏ.** Page fault có thật (4000 lần mỗi câu khi bảng băm ≥ 4MB, vì glibc trả
   khối `mmap` lại cho OS), nhưng chỉ chiếm khoảng 20ms, và không đổi theo số hàng probe.
2. **Phần lớn là một phí cố định trên mỗi hàng probe: 67–98ns.** Con số này khớp với độ trễ của
   một lần trượt cache thật trên máy này (phép D, dây chuyền: 80ns ở mảng 8MB, 108–142ns ở
   16–32MB). Nó **không** khớp với đọc độc lập (2–9ns): giữa hai lần probe có hàng nghìn lệnh
   của executor, nên CPU không chồng được hai lần trượt lên nhau.
3. Nghĩa là mỗi hàng probe trượt khoảng **một lần** xuống RAM. Mảng bucket 4MB cộng 21.6MB tuple
   rải khắp heap vượt L3 12MB. Còn 16 batch thì mỗi batch chỉ có 512KB bucket và 1.6MB tuple,
   nằm gọn trong L2 1.25MB/L3.

**Chưa trả hẳn:** mọi bằng chứng cho H1 ở đây đều gián tiếp (độ dốc theo số hàng probe, và con số
khớp với microbenchmark). Chưa đếm được cache miss trực tiếp, và chưa tách được L3 miss với TLB
miss. Cửa sau đã dựng sẵn cho máy Linux thuần, cùng tinh thần `scripts/linux-baseline.sh` của
phase 0: `./scripts/p97-hashjoin.sh`, hướng dẫn ở `docs/linux-phase9.md`. Script kiểm có PMU trước, ghi `lscpu`/governor/THP vào
`env.txt`, dựng `rl-pg` + `rl-pgm`, rồi chạy thêm phép E: `perf stat -p <backend>` với
`cache-misses`, `LLC-load-misses`, `dTLB-load-misses`, `cycles`, `instructions` cho 1MB và 256MB,
chia theo hàng probe. Phần parse đã được thử bằng một `perf` giả; `perf` thật thì chưa chạy lần
nào. **Dự báo cho máy đó:** nếu H1 đúng, cột 256MB phải có ≥ 1 lần LLC miss hoặc dTLB miss trên
mỗi hàng probe, còn cột 1MB thì thấp hơn hẳn.

### 2026-10-01 — bảng 9: nợ P9-1, lượt 1 — cấp phát giảm thật, thời gian chưa đo được

Câu hỏi từ bảng 1: quét một hàng ở minidb tốn 480ns, Postgres 31ns. Nợ ghi hai nghi phạm:
`keys.Decode` (P7-1) và `DecodeChain` (P6-2). **Kỳ vọng viết trước:** phần lớn nằm ở `keys.Decode`,
vì nó cấp phát cho mỗi cột bytes của mỗi hàng.

Profile CPU, chỉ lấy phần dưới `RowIter.Next` (không tính bước dựng dữ liệu):

```console
$ go test ./internal/query/ -run '^$' -bench SeqStep -benchtime=300x -benchmem -cpuprofile seq.prof
BenchmarkSeqStep-6   	     300	  16400680 ns/op	     20000 rows	 7041248 B/op	  140021 allocs/op
$ go tool pprof -top -cum -focus 'ScanRows' query.test seq.prof
     4.91s  table.(*RowIter).Next
     2.67s  keys.Decode               ← 54%
     2.20s  txn.(*Iter).Next
     1.47s    db.(*Iter).Next         (con trỏ B+Tree, pin/unpin)
     0.36s    txn.DecodeChain         ← 7%
     1.44s  runtime.mallocgc
```

Kỳ vọng đúng về thứ tự: `keys.Decode` 54%, `DecodeChain` 7%. Mỗi hàng 7 lần cấp phát, 352 B.
Thân của cột bytes được dựng bằng `append` từng byte từ `[]byte{}`, nên một cột 40 byte tốn 4 lần
cấp phát (8, 16, 32, 48).

**Sửa (commit e48552f):**
1. `keys`: đi một lượt trước để biết độ dài thật rồi mới chép, một lần, đúng cỡ. `DecodeAppend`
   chép mọi cột bytes của một hàng vào chung một arena.
2. `table.RowIter`: mỗi hàng một mảng `Value` (cho cả pk lẫn hàng) và một arena. Hợp đồng "lần
   `Next` sau không ghi đè hàng trước" giữ nguyên, vì hash join và sort dựa vào nó.
   `TestRowsSurviveNext` giữ hợp đồng đó; đã thấy nó đỏ khi cố ý dùng lại arena.
3. `txn`: `VisibleRaw` đi thẳng trên byte của chuỗi version, không dựng `Chain`, nhưng vẫn kiểm
   hết phần đuôi, để chuỗi hỏng không bị nuốt lặng lẽ. `FuzzChainCodec` đối chiếu nó với
   `DecodeChain + Visible`, và bắt được bản cố ý dừng sớm sau 0.08s.

Không làm: cho `ScanRows` dùng lại buffer (0 cấp phát). `query/run.go:181` giữ `mins[i] = v`, và
`v.B` trỏ vào buffer của hàng: dùng lại buffer là min/max bị ghi đè mà không có gì báo. Muốn làm
thì phải đổi hợp đồng và rà khoảng 30 chỗ gọi.

**Số đo: cấp phát chắc chắn, thời gian thì thô.** Máy đang có `chroma-mcp` ăn 269% CPU và
codegraph 100%, load 5–8 trên 6 nhân:

| | trước (348f120) | sau (e48552f) | độ tin |
|---|---|---|---|
| allocs / 20000 hàng | 140021 | **40021** (7 → 2 mỗi hàng) | chắc chắn: đếm, không đo giờ |
| B / 20000 hàng | 7.04 MB | 6.24 MB | chắc chắn |
| `BenchmarkDecode` (khóa 3 cột) | 147–160 ns, 152 B | 127–138 ns, 146 B | vừa |
| SeqStep, 16 cặp xen kẽ | trung vị 871 ns/hàng | 846 ns/hàng; tỉ số theo cặp **1.07** | **không kết luận được** |
| `idxlab breakeven`, lượt 1 | CFetch/CSeq 5.2x, hoà vốn 17.5% / 29.3% | 7.0x, **13.2%** / 24.9% | thô |
| `idxlab breakeven`, lượt 2 | 4.3x, 21.0% / 34.8% | 4.3x, 20.6% / 25.6% | thô |

Hai lượt `breakeven` lệch nhau tới 8 điểm phần trăm, nên chưa nói được tiêu chí "< 15%" đã đạt.
Số byte chỉ giảm 11%, mà công việc của GC tỉ lệ với số byte chứ không với số lần cấp phát: đó là
một lý do để nghi rằng thời gian giảm ít hơn số cấp phát gợi ý.

**P6-2 có thêm một dấu hiệu:** sau khi bỏ hẳn việc dựng `Chain`, `GetChainDepth` ở depth=60 vẫn
cho `newest` ≈ `oldest` (2125–2211 so với 2355–2640ns, cùng một lượt chạy). Nếu chi phí nằm ở
việc dựng `Chain` thì `newest` phải tụt về gần depth=1. Nó không tụt, vậy chi phí theo độ sâu
nằm ở chỗ khác. Nghi phạm: `db.Get` chép cả chuỗi (60 version) ra một bản riêng trước khi
`VisibleRaw` kịp đọc. Chưa đo.

Đo lại trên Linux thuần: `./scripts/p91-seqscan.sh`, hướng dẫn ở `docs/linux-phase9.md`.

### 2026-10-01 — bảng 10: trả nợ P6-2 — chi phí theo độ sâu là CHÉP và DỰNG, không phải giải mã

Bảng 9 để lại một dấu hiệu: bỏ hẳn việc dựng `Chain` mà `GetChainDepth` ở depth=60 vẫn cho
`newest` ≈ `oldest`. Nghi phạm ghi ở đó là `db.Get` chép cả chuỗi. Profile `depth=60/newest`
(e48552f + bảng 9):

```console
$ go test ./internal/txn/ -run '^$' -bench 'GetChainDepth/depth=60$/newest' -benchtime=300000x -benchmem -cpuprofile gc60.prof
BenchmarkGetChainDepth/depth=60/newest-6  	  300000	      5520 ns/op	     897 B/op	       2 allocs/op
$ go tool pprof -top -cum -focus 'Txn..Get$' txn.test gc60.prof
     1.50s  txn.(*Txn).Get
     0.84s    txn.VisibleRaw            ← 56%: đi hết 60 header để kiểm đuôi
     0.43s      txn.(*chainReader).next
     0.64s    db.(*DB).Get              ← 43%
     0.51s      runtime.memmove         (dòng `copy(out, v)` của btree.Get, 897 byte)
```

Dòng `copy(out, v)` bị tính 0.51s, tức 1.7µs cho 897 byte, nghe vô lý. Mình nghi bộ đo trước, nên
đo thẳng trên máy này: riêng `copy` 897 byte mất **18–25ns**, còn `make` rồi `copy` mất
**890–1865ns**. Vậy cái đắt là lần cấp phát; nó bị tính vào `memmove` vì đó là lần đầu chạm vào
vùng nhớ mới. Page fault không phải nguyên nhân chính: đặt `GODEBUG=madvdontneed=0` thì fault về 0
mà vẫn mất ~550ns (máy hôm đó đang bận, xem bảng 9).

Hai khoản, cả hai đều tỉ lệ với độ sâu, và cả hai đều **chung cho `newest` lẫn `oldest`**. Đó
chính là lý do hai nhánh bằng nhau:

1. `btree.Get` cấp phát rồi chép cả chuỗi ra, trong khi người gọi chỉ cần một bản. **Sửa:**
   `btree.Tree.GetFunc` / `db.DB.GetFunc` gọi `fn` với value trỏ thẳng vào page lúc page còn pin,
   và `Txn.Get` chỉ chép bản nhìn thấy được. `Tree.Get` giờ là `GetFunc` cộng một lần chép.
2. `chainReader.next` dựng một `Version` 48 byte cho từng bản trong 60 bản, chỉ để vứt đi.
   Profile từng dòng: `return v, nil` chiếm 490 / 640ms. **Sửa:** `head()` chỉ trả cờ, xmin và
   thân; `VisibleRaw` chỉ dựng `Version` cho đúng bản trả về. Mọi phép kiểm canonical vẫn nằm một
   chỗ (`head`), và vẫn đi hết đuôi chuỗi.

Đo theo cặp, 10 cặp xen kẽ, so với 348f120 (trước cả bảng 9):

```console
depth=1 newest:  cũ 348,  mới 299 ns (trung vị); tỉ số mới/cũ 0.85 [0.80, 0.88]
depth=60 newest: cũ 5213, mới 708 ns (trung vị); tỉ số mới/cũ 0.14 [0.13, 0.16]
depth=60 oldest: cũ 3878, mới 714 ns (trung vị); tỉ số mới/cũ 0.18 [0.17, 0.19]
B/op ở depth=60: 897 → 4
```

Khoảng tứ phân vị hẹp và nằm xa 1.0, nên kết quả đứng được dù máy nhiễu.

**So với kỳ vọng ghi trong nợ P6-2 (từ phase 6):** "`newest` ở depth=60 tiến gần `newest` ở
depth=1; `oldest` giữ nguyên. Nếu cả hai đều giảm thì bench đang đo cái khác." **Cả hai đều giảm**,
5–7 lần. Bench không sai. Sai là mô hình: phase 6 tưởng chi phí nằm ở *giải mã* (`DecodeChain`
đọc cả chuỗi), và giải mã lười sẽ chỉ cứu được nhánh `newest`. Thực ra chi phí nằm ở hai việc
làm cho MỌI bản, bất kể snapshot: chép cả chuỗi ra khỏi page, và dựng struct cho từng bản. Bỏ hai
việc đó thì cả hai nhánh cùng nhanh lên.

**Còn lại:** depth=60 vẫn đắt hơn depth=1 khoảng 2.4 lần (708 so với 299ns). Đó là bước đi hết 60
header để kiểm đuôi, và mình **cố ý giữ** nó: một chuỗi hỏng ở đuôi phải báo lỗi ở lần đọc đầu
tiên chạm vào nó. Phần còn lại của phí phình version là việc của vacuum, không phải của đường đọc.

Test: `go test ./internal/{btree,db,txn,table,query}` xanh. `FuzzChainCodec` 45s sạch; nó đối chiếu
`VisibleRaw` với `DecodeChain + Visible` và đã từng bắt được bản dừng sớm (bảng 9).

### 2026-10-01 — bảng 11: trả nợ P9-4 — "purge nhanh hơn 40 lần" là bộ đếm, không phải purge

Bảng 4 đo thời gian từ lúc phiên cũ commit tới lúc `History list length` về dưới 50: MySQL 8s,
MariaDB 200ms. Nợ P9-4 hỏi vì sao. Cấu hình mặc định khác nhau ngay từ đầu:

```console
$ reallab/q.sh mysql <<< "SHOW GLOBAL VARIABLES LIKE 'innodb_purge%'"     # và tương tự cho maria
                                       MySQL 8.4.11   MariaDB 11.8.9
innodb_purge_threads                        1              4
innodb_purge_batch_size                   300            127
innodb_purge_rseg_truncate_frequency      128            128
```

**Kỳ vọng viết trước** (comment đầu `reallab/purge.go`):

- H1, số luồng: MariaDB hạ về 1 luồng thì phải chậm đi khoảng 4 lần. Ngay cả khi đúng, 4 lần cũng
  không đủ giải thích 40 lần.
- H2, cỡ lô và nhịp ngủ: tăng `batch_size` thì MySQL phải nhanh lên rõ.
- H3, bộ đếm nói dối (bài học bảng 7): đối chiếu với bộ đếm thứ hai, số undo page đã purge.

Bài `reallab/purge.go` (`-work purge`) dùng cùng bảng với bảng 4, 5 vòng × 100 transaction × 1000
hàng. Sau khi phiên cũ commit, nó lấy mẫu `INNODB_METRICS` mỗi 20ms, và mỗi chế độ chỉ vặn đúng một
núm. Lượt đầu:

```console
$ go run . -work purge -db mysql,maria
db      chế độ                              đầu    t 50%    t 90%    t xong     txn/s   undo page   lần gọi
mysql   mặc định (1 luồng, lô 300)          502  48032.5  48886.8   48886.8        10         503        58
mysql   lô 5000                             504 104675.5 104675.5  104675.5         5         505       114
maria   mặc định (4 luồng, lô 127)          500    697.1    740.6     740.6       675        1892        16
maria   1 luồng                             501    601.3    669.4     669.4       748        1890        16
maria   1 luồng, lô 300 (như MySQL)         501   1008.8   1074.3    1074.3       466        1879         8
```

H1 sai: MariaDB chạy 1 luồng không chậm đi. H2 sai theo chiều ngược: lô 5000 làm MySQL **chậm hơn**.
Điều lạ nằm ở hình dạng: ở MySQL, `t 50%` ≈ `t 90%` ≈ `t xong`. History list không đi xuống dần
mà **đứng yên 48 giây rồi rơi một lần**.

**H4, thêm sau lượt đầu:** history list của MySQL chỉ giảm khi history được **cắt** khỏi rollback
segment, và việc cắt chỉ chạy mỗi `innodb_purge_rseg_truncate_frequency` = 128 lô purge. Nếu đúng
thì (a) việc dọn thật (số undo page) phải xong sớm hơn nhiều so với lúc bộ đếm rơi, và (b) đặt
biến đó về 1 thì MySQL phải xong gần như ngay. Thêm chế độ đó và cột "page ngừng" (lần cuối
`purge_undo_log_pages` còn tăng), rồi chạy hai lượt:

```console
db      chế độ                                         đầu     t 50%     t 90%    t xong  page ngừng  undo page  lần gọi
mysql   mặc định (1 luồng, lô 300)                     503   84955.8   84955.8   84955.8      2260.6        504       94
mysql   lô 5000                                        502  112578.1  112578.1  112578.1      9205.4        503      121
mysql   cắt history mỗi lô (truncate_frequency=1)      500      44.1      44.1      44.1        44.1        500        2
maria   mặc định (4 luồng, lô 127)                     500     194.3     237.9     237.9       237.9       1890       16
maria   1 luồng                                        500     811.0     832.4     832.4       832.4       1890       16
maria   1 luồng, lô 300 (như MySQL)                    500    1049.4    1092.4    1092.4      1092.4       1876        8
maria   truncate_frequency=1                           500     853.5     875.0     875.0       875.0       1891       16

mysql   mặc định (1 luồng, lô 300)                     500   11247.8   11247.8   11247.8      1849.7        501       21
mysql   lô 5000                                        500   67683.4   67683.4   67683.4      1740.8        501       76
mysql   cắt history mỗi lô (truncate_frequency=1)      502      98.0      98.0      98.0        98.0        502        2
maria   mặc định (4 luồng, lô 127)                     500     187.6     241.3     241.3       241.3       1891       16
maria   1 luồng                                        500      66.3      87.8      87.8        87.8       1891       16
maria   1 luồng, lô 300 (như MySQL)                    500     732.7     797.5     797.5       797.5       1876        8
maria   truncate_frequency=1                           500     255.9     277.3     277.3       277.3       1890       16
```

Và MariaDB tự mô tả biến này:

```console
$ reallab/q.sh maria <<< "SELECT VARIABLE_COMMENT FROM information_schema.SYSTEM_VARIABLES
                          WHERE VARIABLE_NAME = 'INNODB_PURGE_RSEG_TRUNCATE_FREQUENCY'"
VARIABLE_COMMENT: Unused
```

**Đọc kết quả:**

| | MySQL mặc định | MySQL, cắt mỗi lô | MariaDB (mọi chế độ) |
|---|---|---|---|
| việc dọn thật xong ("page ngừng") | **1.7–2.3s** | 44–98ms | 88ms–1.1s |
| history list về 0 ("t xong") | **11–85s**, rơi một lần | 44–98ms | 88ms–1.1s, đi xuống dần |

1. **H4 đúng, và nó gần như là toàn bộ câu trả lời.** Ở MySQL, history list đứng yên sau khi việc
   dọn thật đã xong, và chỉ rơi khi tới lượt cắt (mỗi 128 lô). Đủ 128 lô mất 11 tới 85 giây, và
   con số đó nhảy lung tung giữa các lượt. Giải thích khớp với số (chưa đọc mã nguồn để kiểm):
   lúc hết việc, coordinator của purge ngủ giữa các lần gọi, nên lô đến chậm.
   Đặt `truncate_frequency=1` thì MySQL xong trong 44–98ms, **nhanh hơn cả MariaDB**.
2. **Vì sao lô 5000 làm MySQL chậm hơn** (cùng giải thích, cũng chưa kiểm bằng mã nguồn): lô to
   thì hết việc sớm hơn, các lô sau rỗng, coordinator ngủ lâu hơn giữa các lần gọi, nên đếm đủ
   128 lô lại càng lâu (68–113s).
3. **MariaDB đã bỏ hẳn cơ chế này.** Biến vẫn còn nhưng ghi là `Unused`, và vặn nó không đổi được
   gì. History list của MariaDB đi xuống dần, theo đúng việc đã dọn.
4. **Việc dọn thật vẫn có chênh**, nhưng nhỏ hơn nhiều: MySQL mặc định 1.7–2.3s, MariaDB
   0.09–1.1s (dao động lớn, và không đi theo số luồng). Khoảng 2–20 lần chứ không phải 40.
5. Hai DB đếm undo page khác nhau cho cùng một việc (khoảng 500 so với 1890). Đừng so bộ đếm đó
   giữa hai DB, chỉ so trong cùng một DB.

**Hệ quả cho người vận hành MySQL:** `History list length` (thứ mà mọi dashboard InnoDB vẽ) có thể
đứng ở mức cao **hàng chục giây sau khi purge đã dọn xong**. Thấy nó cao ngay sau khi một
transaction dài vừa kết thúc chưa phải là dấu hiệu purge đang tụt lại. Chỉ là dấu hiệu khi nó
không rơi trong vài phút. (Theo thiết kế của InnoDB, undo log chưa cắt thì chưa được tái dùng,
nên trong lúc đó vẫn tốn chỗ. Phần này chưa đo.)

### 2026-09-29 — các thí nghiệm nhỏ cho blog (bài 1, 2, 3, 5, 8, 10)

Mỗi bài blog có một script trong `blog/lab/`. Output đầy đủ nằm ngay trong bài; ở đây chỉ ghi các
con số chốt và những chỗ đo sai.

| Bài | Lệnh | Con số chốt |
|---|---|---|
| 1 | `reallab/q.sh pg blog/lab/01-commit-pg.sql` ×2 | 5000 INSERT: commit từng dòng 2687–2723ms, một commit 8.6–10.9ms (**250–310x**); `synchronous_commit=off` 16.5–17.9ms |
| 1 | `reallab/q.sh mysql blog/lab/01-commit-mysql.sql` ×2 | 10146–13106ms vs 59–61ms (**170–215x**); 1000 commit = 1158 fsync redo + 1000 thao tác sync binlog |
| 1 | `pgbench -f ins.sql -c {1,64}` + `pg_stat_wal.wal_sync` | 1 client: 8194 commit / 8194 fsync; 64 client: 72578 commit / 2643 fsync = **27.5 commit mỗi fsync** |
| 2 | `reallab/q.sh pg blog/lab/02-page-pg.sql` | `UPDATE` đổi ctid (0,2)→(0,4); `VACUUM`: ô 2 thành REDIRECT, ô 3 UNUSED, ô 4 dời 8048→8120 mà giữ số ô; index vẫn trỏ (0,2); `n_tup_hot_upd = 1` |
| 3 | `reallab/q.sh pg blog/lab/03-buffer-pg.sql` | quét 51776 page của `big`, `big` vẫn chỉ chiếm 1600 buffer; `hot` còn 3460/3456 (ring buffer) |
| 3 | `blog/lab/03-buffer-mysql-run.sql` với `innodb_old_blocks_time` = 1000 / 0 | page nóng sau khi quét 590MB: **2050 → 2050** / **2050 → 0** |
| 5 | `reallab/q.sh pg blog/lab/05-wal-pg.sql` + `pg_waldump` | lần chạm đầu sau checkpoint 8216 byte WAL (FPW 8073) vs lần sau 176 byte = **46.7x** |
| 8 | `blog/lab/08-index-{pg,mysql}.sql` | `age = 30` trên index `(city, age)`: Postgres đọc cả index (175 page vs 2), MySQL quét cả bảng; `LIKE 'x%'` trên Postgres `en_US.utf8` cần `text_pattern_ops`; MySQL `varchar = 77` quét 199526 hàng |
| 10 | `reallab/q.sh pg blog/lab/10-explain-pg.sql` | `LIMIT 10` có index 0.337ms vs top-N heapsort 74.6ms (**221x**); sort tràn đĩa 8.5MB chỉ chậm 2% |
| 10 | hash join tự nối `ev`, `work_mem` 1MB vs 256MB, ×3 | 16 batch 268–291ms vs 1 batch 344–366ms: **tràn đĩa nhanh hơn 1.2–1.3x** |

Ba chỗ đo sai hoặc bất ngờ:

1. **`count(*)` không làm nóng bảng trong InnoDB.** Lần đầu làm nóng bảng `hot` bằng hai lần
   `SELECT count(*)`, rồi quét `big`: `hot` bị đuổi sạch ở **cả hai** cấu hình
   `innodb_old_blocks_time`, tức phép phản chứng không phân biệt được gì. Soi
   `INNODB_BUFFER_PAGE_LRU` thì chỉ 605/2051 page của `hot` nằm ở vùng young. Đổi câu làm nóng
   thành `SELECT sum(length(pad))` thì 2050/2051 page lên young, và phép phản chứng tách được hai
   cấu hình. `count(*)` không `WHERE` ở MySQL 8 đi qua bộ đọc song song
   (`innodb_parallel_read_threads = 4`), và trong thí nghiệm này, đường đọc đó không đẩy page lên
   vùng young.
2. **Postgres không suy ra `ev.kind < 5` từ `ev.kind = dim.kind AND dim.kind < 5`.** Equivalence
   class của Postgres chỉ suy ra qua phép bằng. minidb (`plan.propagate`) suy ra được cả bất đẳng
   thức. Đây là một chỗ minidb làm **nhiều hơn** Postgres.
3. **Hash join tràn đĩa nhanh hơn hash join trong RAM.** Lặp lại ổn định qua 3 lượt. Giả thuyết:
   bảng băm 21.6MB lớn hơn L3 (12MB) của i5-1235U, còn mỗi batch chỉ 1.6MB; file tạm nằm trong page
   cache nên gần như không tốn I/O. Bảng 8 đo được gián tiếp: khoảng 70–95ns mỗi hàng probe, đúng cỡ một lần trượt xuống RAM; đếm trực tiếp thì chờ Linux thuần (nợ P9-7). Nó ngược với minidb, nơi tràn đĩa
   đắt 2.1x (phase 8).

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
| MySQL `innodb_flush_log_at_trx_commit=0` + `kill -9` ⇒ mất dữ liệu, như MariaDB | MySQL 8 bật binlog với `sync_binlog=1`, và fsync của binlog che cho redo; tắt cả hai thì mới mất, và chỉ 6 commit (MariaDB mất 9123) | `go run . -work crash`: MySQL `=0` 5/5 xanh; thêm `sync_binlog=0` thì 0/5, mất 6 | Thêm chế độ `sync_binlog=0` cho MySQL; nợ P9-6 cho phần còn chưa giải thích |
| *(bug của bộ đo)* `SELECT count(*)` hai lần cách nhau 1.5s là làm nóng được bảng | Ở MySQL 8, `count(*)` không `WHERE` đi đường đọc song song; chỉ 605/2051 page lên young | `INNODB_BUFFER_PAGE_LRU`: `IS_OLD` YES 1446 / NO 605; phản chứng không tách được hai cấu hình | Làm nóng bằng `SELECT sum(length(pad))`: 2050/2051 young |
| *(bug của bộ đo)* `Innodb_os_log_written` đếm byte redo đã write(), trên cả hai DB | Đúng trên MySQL. Trên MariaDB 11.8 nó tăng đúng bằng `Innodb_lsn_current`, tức đếm redo được sinh ra | Lượt đầu bảng 7: MariaDB "ghi mỗi 5.6ms" dù mất 9123 commit; mẫu tay: cả hai bộ đếm cùng tăng 850223 | Đọc `Innodb_lsn_flushed` trên MariaDB: 3 lần mỗi 3s |
| MySQL mất ít hơn MariaDB ở `=0` vì một cơ chế lạ nào đó của MySQL 8 | Chính là luồng `log_writer`: tắt nó (`innodb_log_writer_threads=OFF`) thì mất 689 mỗi lần kill thay vì 1.2 | `go run . -work crash -db mysql`: 6 so với 3447 commit mất / 5 vòng | Trả nợ P9-6; thêm chế độ vào `crash.go` |
| Hash join tràn đĩa thì chậm hơn chạy trong RAM | Postgres: 16 batch **nhanh hơn** 1 batch 1.2–1.3x (268–291 vs 344–366ms) | `blog/lab/10-explain-pg.sql` mục 6, 3 lượt | Không sửa; ghi vào blog bài 10, nợ P9-7 |
| Postgres nhanh lên khi tràn đĩa là do page fault: glibc trả bộ nhớ bảng băm cho OS sau mỗi câu | Page fault có thật (4000 lần mỗi câu) nhưng chỉ chiếm ~20ms. Khoảng chênh tăng tuyến tính theo số hàng probe, và vẫn còn khi malloc giữ bộ nhớ | `go run . -work hashjoin -db pg,pgm`: B trên pgm, fault 3, chênh vẫn 78 → 332ms; C: pg − pgm = 23ms | Giữ H1 (cache); đếm trực tiếp chờ `scripts/p97-hashjoin.sh` |
| *(P6-2)* `newest` ≈ `oldest` ở depth=60 nghĩa là chi phí nằm ở việc `DecodeChain` dựng cả chuỗi | Bỏ hẳn việc dựng `Chain` (`VisibleRaw`) mà depth=60 vẫn `newest` ≈ `oldest`: chi phí theo độ sâu nằm ở chỗ khác, nghi `db.Get` chép cả chuỗi. Bảng 10: đúng một phần. `btree.Get` cấp phát + chép (43%) **và** dựng `Version` cho từng bản (56%), cả hai chung cho hai nhánh | `go test ./internal/txn -bench GetChainDepth`: 2125–2211 so với 2355–2640ns; profile ở bảng 10 | `GetFunc` + `chainReader.head`; depth=60 5213 → 708ns |
| *(phase 6)* Giải mã lười sẽ cứu nhánh `newest`, còn `oldest` giữ nguyên | Cả hai cùng nhanh lên 5–7 lần: chi phí chưa bao giờ nằm ở giải mã hay vòng visibility, mà ở việc chép cả chuỗi và dựng struct cho từng bản | 10 cặp A/B: `newest` 0.14x, `oldest` 0.18x so với 348f120 | Trả P6-2 (bảng 10) |
| Purge của MariaDB nhanh hơn MySQL 40 lần | Việc dọn thật chỉ chênh 2–20 lần. Phần lớn khoảng chênh là **bộ đếm**: history list của MySQL đứng yên cho tới lượt cắt, mỗi 128 lô purge | `go run . -work purge`: MySQL "page ngừng" 1.7–2.3s nhưng "t xong" 11–85s; `truncate_frequency=1` → 44–98ms | Trả P9-4 (bảng 11); sửa bài 7 |
| Tăng `innodb_purge_batch_size` thì purge của MySQL nhanh hơn | Chậm hơn (68–113s): lô to hết việc sớm, các lô rỗng sau đó ngủ lâu, đếm đủ 128 lô càng lâu | cùng lệnh, chế độ "lô 5000" | — |
| `glibc.malloc.mmap_threshold=1GB` tắt được `mmap` cho khối lớn | Trần là 32MB; giá trị lớn hơn bị bỏ qua mà không báo gì | `rl-pgm` vẫn 4000 fault / câu cho tới khi đặt `33554432` | Ghi trần vào comment của `docker-compose.yml` |

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
| `kill -9` giữa lúc ghi: commit đã báo OK bị mất (mặc định / không chờ log) | 0 / — (phase 5: 200/200) | **0** / 2400 | **0** / 0 (binlog che), 6 khi tắt cả `sync_binlog` | **0** / 9123 |
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

- [x] 🔧 P6-2 (của phase 6) · chuỗi version: **trả ở bảng 10**, depth=60 5213 → 708ns, 897 → 4 B/op
- [ ] 📏 P9-1 · Đo thẳng phí quét một hàng của minidb, tách phần `keys.Decode` (P7-1) khỏi `DecodeChain` (P6-2). **Bảng 9, lượt 1:** đã tách (54% so với 7%), đã sửa (7 → 2 lần cấp phát mỗi hàng). Thời gian và điểm hoà vốn < 15% chờ đo lại: `./scripts/p91-seqscan.sh` trên Linux thuần
- [ ] ⏳ P9-2 · `txnlab` chưa có ô "RR kiểu MySQL" (đọc snapshot, ghi trên bản mới nhất)
- [ ] 📏 P9-3 · Chưa đo UUID trên Postgres khi riêng index PK lớn hơn RAM (cần giới hạn cả page cache: `docker --memory`)
- [x] 📏 P9-4 · Purge của MariaDB nhanh hơn MySQL 40x: **trả ở bảng 11**. History list của MySQL chỉ rơi khi tới lượt cắt (`innodb_purge_rseg_truncate_frequency=128`); đặt về 1 thì MySQL xong trong 44–98ms. MariaDB ghi biến đó là `Unused`
- [ ] 🔧 P9-5 · Cột `ms` của bảng 5 là một lần `EXPLAIN ANALYZE`, không làm nóng, không lấy trung vị
- [x] 📏 P9-6 · MySQL `flush_log_at_trx_commit=0` + `sync_binlog=0` chỉ mất 6 commit khi `kill -9`, MariaDB mất 9123: **trả ở bảng 7**, do luồng `log_writer` (tắt nó thì mất 3447)
- [ ] 📏 P9-7 · Hash join 16 batch nhanh hơn 1 batch 1.2–1.3x trên Postgres. **Bảng 8, trả một nửa:** page fault chỉ ~20ms; phần chính là 67–98ns mỗi hàng probe, khớp độ trễ một lần trượt xuống RAM (đo gián tiếp). Còn đếm cache/TLB miss trực tiếp: `./scripts/p97-hashjoin.sh` trên Linux thuần
