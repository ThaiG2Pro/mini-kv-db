# Bài 0 — Database trên máy bạn thật ra là gì?

> Series [Mở nắp database](README.md) · bài 0/11

Bạn gõ `docker run postgres`, hoặc cài MySQL qua Homebrew, rồi viết `SELECT * FROM users` suốt
mấy năm. Nó chạy. Nhưng nếu ai đó hỏi *"cái bảng `users` đó đang nằm ở đâu trên máy?"* thì phần
lớn chúng ta sẽ ngập ngừng.

Bài này trả lời đúng câu đó, trong 5 phút, trên một Postgres và một MySQL thật. Sau đó nó vẽ ra
tấm bản đồ mà cả series sẽ đi qua.

## Thí nghiệm: đi tìm cái bảng

Dựng lab (chỉ cần Docker):

```bash
docker compose -f reallab/docker-compose.yml up -d
```

Tạo một bảng 100000 hàng trên Postgres, rồi hỏi Postgres xem nó để bảng đó ở đâu:

```console
$ reallab/q.sh pg <<'EOF'
CREATE TABLE users (id int PRIMARY KEY, name text);
INSERT INTO users SELECT g, 'user ' || g FROM generate_series(1, 100000) g;
CHECKPOINT;
SELECT pg_relation_filepath('users') AS file, pg_relation_size('users') AS bytes,
       pg_relation_size('users')/8192 AS pages;
EOF
       file       |  bytes  | pages
------------------+---------+-------
 base/16384/16947 | 4431872 |   541
```

Bảng `users` là **một file** có tên là một con số: `base/16384/16947`. Nó nặng 4431872 byte,
tức đúng **541 lần 8192**. Mở thư mục dữ liệu ra xem:

```console
$ docker exec rl-pg ls -la /var/lib/postgresql/data/base/16384/16947*
-rw------- 1 postgres postgres 4431872 Sep 29 08:06 base/16384/16947
-rw------- 1 postgres postgres   24576 Sep 29 08:06 base/16384/16947_fsm
```

Và đây là 48 byte cuối cùng của **8192 byte đầu tiên** trong file đó:

```console
$ docker exec rl-pg sh -c 'cd /var/lib/postgresql/data && head -c 8192 base/16384/16947 | tail -c 48 | od -A d -c'
0000000   r       2  \0  \0  \0  \0  \0 274   V  \0  \0  \0  \0  \0  \0
0000016  \0  \0  \0  \0  \0  \0  \0  \0 001  \0 002  \0 002  \b 030  \0
0000032 001  \0  \0  \0 017   u   s   e   r       1  \0  \0  \0  \0  \0
```

`u s e r   1` là hàng đầu tiên bạn vừa `INSERT`, nằm ở **cuối** khối 8192 byte đầu tiên. Hàng
thứ hai (`user 2`) nằm ngay phía trước nó. Còn cái `001 \0 \0 \0` trước chữ `user` là số `1`,
giá trị của cột `id`.

Ba điều rút ra từ ba lệnh trên:

1. **Một bảng là một file.** Không có phép thuật nào ở đây.
2. **File được chia thành các khối bằng nhau** gọi là **page**: 8192 byte ở Postgres, 16384 byte
   ở MySQL. Kích thước file luôn là bội số của kích thước page.
3. **Hàng được xếp từ cuối page ngược lên.** Vì sao lại làm vậy, bài 2 sẽ giải thích.

Postgres còn cho bạn thấy "địa chỉ" của từng hàng:

```console
$ reallab/q.sh pg <<< "SELECT ctid, * FROM users WHERE id IN (1, 2, 227, 228);"
  ctid  | id  |   name
--------+-----+----------
 (0,1)  |   1 | user 1
 (0,2)  |   2 | user 2
 (1,42) | 227 | user 227
 (1,43) | 228 | user 228
```

`ctid = (1,42)` nghĩa là *page số 1, ô số 42*. Page 0 chứa được 185 hàng, nên hàng 227 rơi vào
page 1. **Mọi thứ database làm, rốt cuộc là đọc và ghi những page này.**

Thử với MySQL (cùng bảng `users`, lần này 1000 hàng):

```console
$ docker exec rl-mysql ls /var/lib/mysql
#ib_16384_0.dblwr   #innodb_redo   binlog.000001   ibdata1   lab   mysql.ibd   undo_001   undo_002   ...
$ docker exec rl-mysql ls -la /var/lib/mysql/lab/users.ibd
-rw-r----- 1 mysql mysql 163840 Sep 29 08:06 /var/lib/mysql/lab/users.ibd
```

Cũng là một file cho một bảng (`users.ibd`), kích thước 163840 = 10 × 16384. Nhưng bên cạnh nó
còn có những file mà cái tên đã gợi ý chúng làm gì: `#innodb_redo` (nhật ký để không mất dữ liệu
khi mất điện, bài 5), `undo_001` (chỗ để các phiên bản cũ của hàng, bài 7), `dblwr` (chống ghi
dở một page, bài 5).

## Còn "database server" thì sao?

File chỉ là một nửa. Nửa kia là **tiến trình** đang chạy và giữ các file đó:

```console
$ docker exec rl-pg sh -c 'for p in /proc/[0-9]*; do tr "\0" " " < $p/cmdline; echo; done | grep ^postgres'
postgres -c shared_buffers=256MB -c track_io_timing=on
postgres: checkpointer
postgres: background writer
postgres: walwriter
postgres: autovacuum launcher
postgres: logical replication launcher
```

Postgres là **nhiều tiến trình**: một tiến trình chính, vài tiến trình nền, và mỗi kết nối của
bạn được cấp thêm một tiến trình riêng. Mỗi cái tên là một việc mà series này sẽ mở ra:
`checkpointer` và `walwriter` (bài 5), `background writer` (bài 3), `autovacuum` (bài 7).

MySQL thì ngược lại: **một tiến trình, nhiều luồng**.

```console
$ docker exec rl-mysql sh -c 'cat /proc/1/comm; ls /proc/1/task | wc -l'
mysqld
36
```

Hai kiến trúc, cùng một công việc: nhận câu SQL của bạn, rồi biến nó thành việc đọc và ghi page.

## Bản đồ

Đây là toàn bộ đường đi từ lúc bạn gõ `SELECT` tới lúc byte được đọc lên từ đĩa. Mỗi tầng là
một bài trong series:

```text
  bạn gõ:  SELECT name FROM users WHERE id = 42
                    │
  ┌─────────────────▼──────────────────┐
  │ Parser → Planner → Executor        │  bài 8, 9, 10: câu SQL thành một "kế hoạch",
  │   "nên dùng index hay quét bảng?"  │  planner đoán số hàng để chọn kế hoạch
  ├────────────────────────────────────┤
  │ Transaction / MVCC / Lock          │  bài 6, 7: ai thấy phiên bản nào của hàng,
  │   "hai người cùng sửa thì sao?"    │  ai phải chờ ai
  ├────────────────────────────────────┤
  │ B+Tree (index)                     │  bài 4: tìm hàng 42 trong 3-4 bước
  │   "tìm hàng 42 ở page nào?"        │  thay vì đọc cả triệu hàng
  ├────────────────────────────────────┤
  │ Buffer pool (RAM của database)     │  bài 3: giữ các page hay dùng trong RAM,
  │   "page đó có sẵn trong RAM chưa?" │  vì đọc đĩa chậm hơn cả nghìn lần
  ├────────────────────────────────────┤
  │ WAL / redo log                     │  bài 5: ghi nhật ký TRƯỚC khi sửa page,
  │   "mất điện giữa chừng thì sao?"   │  để luôn khôi phục được
  ├────────────────────────────────────┤
  │ Page + file                        │  bài 1, 2: file là mảng page 8KB/16KB,
  │   "hàng nằm ở byte nào?"           │  hàng nằm trong page, fsync để chắc chắn
  └─────────────────▼──────────────────┘
               đĩa (SSD)
```

Postgres có khoảng một triệu dòng C, MySQL còn nhiều hơn. Không ai trong chúng ta có thời gian đọc
hết. Nhưng **mỗi tầng trên chỉ giải một bài toán**, và bài toán đó có thể viết ra trong vài
nghìn dòng. Đó là lý do của [minidb](../README.md): một database viết bằng Go, khoảng 30000 dòng,
có đủ các tầng trên, và mỗi tầng đã được đo đạc để so với Postgres và MySQL thật.

Ví dụ, đây là toàn bộ cách minidb đọc một page từ file (`internal/pager/pager.go`):

```go
const PageSize = 4096

func (p *Pager) ReadPage(id PageID, buf []byte) error {
	if len(buf) != PageSize {
		return ErrBadPageSize
	}
	if uint32(id) >= p.meta.pageCount {
		return fmt.Errorf("%w: id=%d pageCount=%d", ErrBadPageID, id, p.meta.pageCount)
	}
	p.Reads++
	_, err := p.f.ReadAt(buf, int64(id)*PageSize)
	return err
}
```

Page số `id` nằm ở byte thứ `id × PageSize` của file. Chỉ có vậy. Cái `ctid = (1,42)` của
Postgres ở trên cũng là cùng ý tưởng: page số 1 bắt đầu ở byte 8192.

## Mang về dùng

1. **Database là file được chia thành page, cộng với một tiến trình cache các page đó trong RAM.**
   Mọi chuyện nhanh hay chậm, rốt cuộc là chuyện có bao nhiêu page phải đọc, và page đó đã nằm
   trong RAM hay chưa.
2. **Khi một câu truy vấn chậm, hỏi "nó chạm bao nhiêu page?" trước khi hỏi "câu SQL sai chỗ nào?".**
   `EXPLAIN (ANALYZE, BUFFERS)` của Postgres in ra đúng con số này (`Buffers: shared hit=950 read=2`).
3. **Postgres và MySQL khác nhau ngay từ tầng dưới cùng**: nhiều tiến trình hay một tiến trình,
   page 8KB hay 16KB, hàng nằm trong đống không thứ tự hay nằm trong cây. Những khác biệt đó
   quyết định DB nào đau ở đâu. Bài 4 sẽ cho thấy một khác biệt như vậy làm cùng một thao tác
   `INSERT` chậm hơn 10 lần trên MySQL mà gần như không sao trên Postgres.

**Bài tiếp theo:** [Bài 1 — Vì sao `COMMIT` chậm?](01-commit.md)
