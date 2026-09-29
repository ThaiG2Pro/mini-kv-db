# Bài 1 — Vì sao `COMMIT` chậm?

> Series [Mở nắp database](README.md) · bài 1/11 · cần đọc trước: [bài 0](00-ban-do.md)

Bạn viết một script import 5000 dòng từ file CSV. Vòng lặp `for` quen thuộc, mỗi dòng một câu
`INSERT`. Chạy mất vài giây. Đồng nghiệp sửa lại để gom cả 5000 dòng vào một transaction, và nó
xong trong chớp mắt. Cùng một lượng dữ liệu, cùng những câu `INSERT` ấy. Chỗ khác nhau duy nhất
là **số lần `COMMIT`**.

Bài này đo xem một `COMMIT` đắt bao nhiêu, và đắt vì cái gì.

## Thí nghiệm

Cùng 5000 câu `INSERT`, chỉ khác số lần commit. Vòng lặp chạy **ngay trong server** (một stored
procedure), nên không có chi phí mạng hay driver nào xen vào:

```bash
reallab/q.sh pg    blog/lab/01-commit-pg.sql
reallab/q.sh mysql blog/lab/01-commit-mysql.sql
```

| 5000 INSERT | Postgres 17 | MySQL 8.4 |
|---|---|---|
| commit sau **mỗi** dòng (5000 commit) | **2687–2723 ms** | **10146–13106 ms** |
| commit mỗi 100 dòng (50 commit) | 38–47 ms | 171–225 ms |
| commit **một** lần | **8.6–10.9 ms** | **59–61 ms** |
| mỗi dòng một commit, nhưng **không chờ đĩa** | 16.5–17.9 ms (`synchronous_commit = off`) | 399–544 ms (`innodb_flush_log_at_trx_commit = 2`, `sync_binlog = 0`) |

(Hai lượt chạy, in cả hai số. Máy là laptop chạy WSL2, nên số tuyệt đối không giống máy chủ thật.
Chỉ tỉ số là đáng tin.)

Commit từng dòng chậm hơn commit một lần **250–310 lần** trên Postgres và **170–215 lần** trên
MySQL. Nhìn dòng cuối: vẫn 5000 commit, nhưng bảo DB **đừng chờ đĩa** thì Postgres nhanh hơn 160
lần. Vậy gần như toàn bộ thời gian của một `COMMIT` là **chờ đĩa**.

Chờ cái gì trên đĩa? Đếm thử:

```console
$ # Postgres: 1000 commit
wal_fsyncs cho 1000 commit: 1000
$ # MySQL: 1000 commit
| redo_fsyncs |  1158 |
| binlog_misc_ops_incl_fsync | 1000 |
```

Postgres gọi **đúng một lần `fsync`** cho mỗi commit. MySQL gọi **hai lần**: một cho redo log của
InnoDB, một cho binlog (nhật ký dùng cho replication). Đó là một phần lý do mỗi commit của MySQL
ở đây đắt hơn Postgres khoảng 4 lần.

## Bên trong: `write()` chưa phải là "đã lưu"

Khi chương trình gọi `write()` để ghi xuống file, dữ liệu **chưa** tới đĩa. Nó mới tới **page
cache** của hệ điều hành, một vùng RAM. Hệ điều hành sẽ ghi xuống đĩa sau, khi nào nó thấy tiện.
Nếu mất điện trước lúc đó, dữ liệu mất.

Muốn chắc chắn dữ liệu đã nằm trên đĩa thì phải gọi `fsync()`: *"đừng trả về cho tới khi mọi thứ
tôi đã ghi vào file này thật sự nằm trên thiết bị lưu trữ"*. Phase 0 của minidb đo đúng hai việc
đó trên cùng máy:

```text
write 4KB (không fsync)                    533471 ops/s      2µs    chỉ tới page cache — CHƯA durable
write 4KB + fsync mỗi lần                     690 ops/s   1.45ms    = trần commit/s của 1 luồng
```

Chênh nhau **700 lần**. Khi DB báo `COMMIT` thành công, nó đang hứa: *"dữ liệu này không mất, kể
cả khi mất điện ngay sau đây"*. Muốn giữ lời hứa đó thì nó phải `fsync` trước khi trả lời bạn.
Một `COMMIT` vì vậy tốn **ít nhất một lần fsync**, dù bạn chỉ chèn một hàng 10 byte.

Còn khi commit một lần cho 5000 dòng, DB vẫn chỉ `fsync` **một lần** ở cuối. 5000 dòng chia nhau
một lần chờ đĩa.

### Group commit: nhiều người cùng chờ chung một lần

Nếu có 64 client cùng commit một lúc, liệu có cần 64 lần fsync? Không cần. Đo bằng `pgbench`,
mỗi client liên tục chèn một hàng rồi commit:

```console
$ pgbench -n -f ins.sql -c $clients -T 5 lab
clients=1   tps = 1616
clients=4   tps = 3155
clients=16  tps = 9759
clients=64  tps = 14010

clients=1  commits=8194  wal_fsyncs=8194
clients=64 commits=72578 wal_fsyncs=2643     ← 27.5 commit cho mỗi fsync
```

(Chạy lại: `blog/lab/01-group-commit.sh`. Lượt chạy lại cho 85423 commit / 3089 fsync ở 64
client, tức 27.7, gần như y hệt.)

Một client thì mỗi commit một fsync. 64 client thì **27 commit dùng chung một fsync**. Trong lúc
một lần fsync đang chạy, các commit khác xếp hàng chờ; khi nó xong, lần fsync kế tiếp đẩy xuống
đĩa phần log của cả hàng chờ cùng lúc. Người ta gọi đó là **group commit**.

Đây là toàn bộ cơ chế đó trong minidb (`internal/wal/log.go`, bỏ bớt phần phụ):

```go
// Group commit nằm ngay trong vòng lặp dưới đây: ai thấy đã có người đang
// fsync thì ĐỢI thay vì fsync thêm một lần nữa.
func (l *Log) Flush(upto uint64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for {
		if l.flushed >= upto {
			return nil // có người đã fsync hộ phần của mình rồi
		}
		if l.syncing {
			l.cond.Wait() // đang có người fsync: ngồi chờ, đừng fsync thêm
			continue
		}
		target := l.end // fsync tới CUỐI log, không chỉ tới phần của mình
		l.writeLocked(target)
		l.syncing = true
		l.mu.Unlock()
		err := l.f.Sync()
		l.mu.Lock()
		l.syncing = false
		l.flushed = target
		l.cond.Broadcast() // đánh thức mọi người đang chờ
		if err != nil {
			return err
		}
	}
}
```

Mẹo nằm ở dòng `target := l.end`: ai được quyền fsync sẽ fsync **tới cuối log**, tức là cuốn luôn
phần của những người vào sau. Họ thức dậy, thấy `flushed >= upto`, và về luôn mà không tốn thêm
lần fsync nào. Không cần tham số "gom trong bao lâu": thời gian của chính lần fsync đang chạy đã
là cửa sổ để gom.

### Tắt fsync thì sao?

`synchronous_commit = off` (Postgres) và `innodb_flush_log_at_trx_commit = 2` (MySQL) nói với DB:
*"trả lời tôi ngay, fsync sau cũng được"*. Nhanh hơn 25–160 lần như bảng trên, và đổi lại, **mất
điện thì mất những commit trong khoảng tối đa cỡ một giây gần nhất**. DB không bị hỏng, chỉ quên mất vài
transaction mà nó đã báo là thành công.

Với log hay số liệu thống kê, đó có thể là cái giá chấp nhận được. Với tiền thì không.

## Mang về dùng

1. **Gom việc ghi vào ít transaction hơn.** Import, migration, batch job: commit mỗi vài trăm
   hoặc vài nghìn dòng, không commit từng dòng. Bảng trên cho thấy commit mỗi 100 dòng đã nhanh
   hơn khoảng 60 lần so với commit từng dòng.
2. **Chèn nhiều hàng trong một câu** (`INSERT … VALUES (…), (…), …`) hoặc dùng `COPY` / `LOAD DATA`.
   Ít câu hơn, ít lượt đi về hơn, và thường cũng ít commit hơn.
3. **Nhiều kết nối song song thì DB tự gom fsync** (group commit). Một luồng ghi tuần tự là cách
   dùng DB chậm nhất có thể.
4. **Chỉ tắt `synchronous_commit` khi đã biết rõ mình được phép mất gì.** Postgres cho phép tắt
   theo từng transaction (`SET LOCAL synchronous_commit = off`), nên bạn có thể tắt cho dữ liệu
   log và giữ nguyên cho dữ liệu thanh toán.
5. **Số fsync trên giây là trần của hệ thống.** Muốn biết một DB ghi được tối đa bao nhiêu
   transaction mỗi giây trên một luồng, hãy đo độ trễ fsync của ổ đĩa. SSD cho máy chủ có tụ
   điện dự phòng fsync nhanh hơn laptop hàng chục lần, vì nó được phép báo xong ngay khi dữ liệu
   vào bộ nhớ đệm có điện dự phòng.

---

Số đo gốc: [`blog/lab/01-commit-*.sql`](lab/) và [`diary/phase0.md`](../diary/phase0.md) (fsync,
group commit trên minidb).

**Bài tiếp theo:** [Bài 2 — Một hàng nằm ở đâu?](02-page.md)
