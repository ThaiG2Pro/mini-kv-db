# Bài 5 — Vì sao mất điện không mất dữ liệu?

> Series [Mở nắp database](README.md) · bài 5/11 · cần đọc trước: [bài 1](01-commit.md), [bài 3](03-buffer-pool.md)

Bài 1 để lại một câu hỏi. Mỗi `COMMIT` chỉ tốn **một** lần `fsync`, dù transaction đó sửa hàng
chục page nằm rải rác khắp file. Vậy DB fsync cái gì? Và nếu các page đã sửa vẫn chỉ nằm trong
buffer pool (bài 3), chưa hề được ghi xuống file dữ liệu, thì mất điện đúng lúc đó sẽ ra sao?

Câu trả lời là một file mà bạn có lẽ đã thấy trong thư mục dữ liệu nhưng chưa bao giờ mở ra:
`pg_wal/` của Postgres, `#innodb_redo/` của MySQL.

## Thí nghiệm 1: giết database giữa lúc đang ghi

Một vòng lặp chèn `id = 1, 2, 3, …`, mỗi câu là một commit, và ghi lại **id lớn nhất mà DB đã báo
commit thành công**. Sau 1–3 giây, giết container bằng `docker kill -s KILL` (tương đương
`kill -9`: tiến trình chết ngay, không kịp dọn dẹp gì). Khởi động lại, rồi kiểm tra: mọi id đã
được báo thành công có còn trong bảng không?

```bash
cd reallab && go run . -work crash -rounds 5
```

```text
-- pg
chế độ                                    vòng   đã báo OK       mất      thừa   khởi động
mặc định (synchronous_commit=on)           5/5       12246         0         2       800ms
synchronous_commit=off                     0/5       34302      2400         0       600ms

-- mysql
mặc định (flush_log_at_trx_commit=1)       5/5        3862         0         1        2.2s
flush_log_at_trx_commit=2                  5/5        5944         0         4        2.3s
flush_log_at_trx_commit=0                  5/5        4268         0         4        2.9s
flush_log_at_trx_commit=0 + sync_binlog=0  0/5       19304         6         0        2.9s

-- maria
mặc định (flush_log_at_trx_commit=1)       5/5        8879         0         4          4s
flush_log_at_trx_commit=2                  5/5       23878         0         1          2s
flush_log_at_trx_commit=0                  0/5       43427      9123         0        2.7s
```

- **Với cấu hình mặc định, cả ba DB giữ đúng lời hứa.** Mọi commit đã báo thành công đều còn
  nguyên sau khi bị giết giữa chừng, qua 15 lần giết. Cột "thừa" là những câu đang bay đúng lúc
  bị giết: DB đã kịp ghi xong nhưng chưa kịp báo cho client. Chuyện đó hợp lệ, vì client chưa
  bao giờ được hứa gì về chúng.
- **Và khi bảo DB "đừng chờ log", nó thật sự mất dữ liệu.** Postgres với `synchronous_commit=off`
  mất 2400 commit đã báo thành công. MariaDB với `innodb_flush_log_at_trx_commit=0` mất 9123.
  Một bài kiểm tra chưa bao giờ báo sai thì chưa chứng minh được gì. Dòng này cho thấy bài kiểm
  tra **biết** báo sai.

Hai dòng MySQL đáng để ý. Ở `flush_log_at_trx_commit=0`, MySQL không mất gì, trong khi MariaDB (cùng
engine InnoDB) mất 9123. Khác biệt nằm ở **binlog**: MySQL 8 bật binlog mặc định với
`sync_binlog=1` (fsync binlog ở mỗi commit), còn MariaDB tắt binlog. Tắt nốt fsync của binlog thì
MySQL cũng bắt đầu mất dữ liệu (6 commit), và nhanh hơn 4.5 lần. Vì sao MySQL vẫn mất ít hơn
MariaDB tới 1500 lần? Vì MySQL 8 có một luồng riêng ghi redo xuống OS mỗi vài mili giây. Tắt luồng
đó thì MySQL mất 3447 commit. [Bài 12](12-ai-goi-write.md) đo chuyện này.

**Một lưu ý quan trọng:** `kill -9` giết **tiến trình**, không giết **page cache của hệ điều hành**.
Nên thí nghiệm này chỉ kiểm được *"log đã được giao cho hệ điều hành trước khi DB báo OK"*. Nó
không kiểm được `fsync`. Đó là lý do `flush_log_at_trx_commit=2` (ghi log vào OS mỗi commit,
fsync mỗi giây) vẫn xanh ở đây: nó chỉ mất dữ liệu khi **cả máy** mất điện. Muốn kiểm fsync thì
phải rút điện thật.

## Thí nghiệm 2: một commit ghi những gì vào log?

```bash
reallab/q.sh pg blog/lab/05-wal-pg.sql
```

```text
                              step                               | wal_bytes
-----------------------------------------------------------------+-----------
 1. UPDATE hàng 1 (lần chạm ĐẦU TIÊN vào page đó sau checkpoint) |     18016
 2. UPDATE hàng 2 (cùng page, lần chạm thứ hai)                  |       208
 3. UPDATE hàng 3 (cùng page)                                    |       216
 4. UPDATE hàng 900 (page KHÁC, lần chạm đầu tiên)               |     13976
 5. UPDATE hàng 900 lần nữa                                      |       208
```

Cùng một câu `UPDATE` sửa một số `int`. Có lúc nó sinh ra 208 byte log, có lúc 14–18KB, **gấp
67–87 lần**. `pg_waldump` đọc thẳng file WAL cho biết vì sao:

```text
rmgr: Heap2  len (rec/tot): 57/ 8073  desc: PRUNE_ON_ACCESS ... blkref #0: rel 1663/16384/98305 blk 0 FPW
rmgr: Heap   len (rec/tot): 71/   71  desc: HOT_UPDATE old_off: 5 ... new_off: 61
rmgr: Transaction len (rec/tot): 34/ 34  desc: COMMIT 2026-09-29 08:52:55.229603 UTC

rmgr: Heap2  len (rec/tot): 58/   58  desc: PRUNE_ON_ACCESS ... redirected: [5->61]
rmgr: Heap   len (rec/tot): 71/   71  desc: HOT_UPDATE old_off: 6 ... new_off: 62
rmgr: Transaction len (rec/tot): 34/ 34  desc: COMMIT 2026-09-29 08:52:55.230617 UTC
```

Lần `UPDATE` thứ hai sinh ra đúng những gì bạn đoán: *"ở page này, ô 6, sửa thành bản mới ở ô
62"* (71 byte) và *"commit"* (34 byte). Lần đầu tiên thì kèm thêm một chữ **`FPW`**: *full page
write*, tức **nguyên cả page 8KB** được chép vào log.

## Bên trong: ghi nhật ký trước, sửa sau

WAL là viết tắt của **write-ahead log**, và cái tên nói hết quy tắc: *trước khi một page đã sửa
được phép ghi xuống file dữ liệu, bản ghi log mô tả thay đổi đó phải nằm trên đĩa trước*.

```text
  UPDATE ... ; COMMIT
      │
      ├─ 1. sửa page trong buffer pool (RAM)          ← chưa ghi file dữ liệu
      ├─ 2. nối một bản ghi vào cuối WAL: "page 0, ô 6 → ô 62"
      ├─ 3. COMMIT: fsync WAL                          ← MỘT lần fsync, ghi TUẦN TỰ vào cuối file
      └─ 4. báo "OK" cho client

  ... rất lâu sau, lúc tiện (checkpoint, hoặc khi buffer pool cần chỗ):
      └─ 5. ghi page đã sửa xuống file dữ liệu         ← ghi NGẪU NHIÊN, không ai phải chờ
```

Đây là lý do mỗi commit chỉ cần một lần fsync: DB **không** ghi các page đã sửa xuống file dữ
liệu lúc commit. Nó chỉ ghi vào cuối **một** file log, tuần tự, rồi fsync file đó. Page dữ liệu
được ghi xuống sau, lúc nào tiện. Nếu mất điện trước lúc đó, khi khởi động lại DB đọc log và
**làm lại** (redo) những thay đổi đã commit. Cột "khởi động" ở thí nghiệm 1 (0.6–4 giây) chính là
thời gian làm việc đó.

Quy tắc "log trước, page sau" phải được giữ ở **một** chỗ duy nhất: chỗ buffer pool ghi một page
bẩn xuống đĩa. Trong minidb, đó là mấy dòng này (`internal/bufpool/bufpool.go`):

```go
func (p *Pool) writeFrame(f *Frame) error {
	if p.FlushLog != nil {
		// WAL rule: log phải đã fsync tới pageLSN của page này.
		if err := p.FlushLog(f.Data.LSN()); err != nil {
			return fmt.Errorf("%w: page %d pageLSN=%d: %w", ErrWALRule, f.id, f.Data.LSN(), err)
		}
	}
	return p.store.WritePage(f.id, f.Data)
}
```

Mỗi page mang một con số `pageLSN`: vị trí trong log của bản ghi cuối cùng đã sửa nó. Trước khi
ghi page xuống file, buffer pool hỏi log: *"anh đã fsync tới vị trí này chưa?"* Chưa thì phải
fsync log trước, hoặc chọn page khác để đuổi.

### Vì sao lại chép nguyên cả page?

Vì page 8KB (hoặc 16KB) **không được ghi nguyên tử**. Ổ đĩa chỉ đảm bảo ghi trọn từng sector
512B hoặc 4KB. Mất điện đúng lúc đang ghi một page thì có thể nửa đầu là bản mới, nửa sau là bản
cũ: một page **rách** (*torn page*). Lúc đó bản ghi log kiểu *"ở ô 6 sửa thành…"* là vô dụng, vì
nó cần một page lành để áp lên.

Postgres giải bằng cách: lần **đầu tiên** một page bị sửa sau mỗi checkpoint, chép nguyên page
vào log. Page trên đĩa có rách thì recovery vẫn còn một bản nguyên vẹn trong log để bắt đầu lại.
minidb làm y hệt (`internal/db/journal.go`):

```go
// Ảnh TRƯỚC luôn là diff (undo chạy sau redo, lúc page đã lành). Chỉ ảnh
// SAU mới cần trọn page, và chỉ ở lần chạm đầu tiên sau mỗi checkpoint.
after := segs
full := j.db.log.FullPageWrites && !j.db.fpw[id]
if full {
	after = []wal.Seg{{Off: 0, Len: page.PageSize}}
	flags = wal.FlagFullPage
}
```

InnoDB giải cùng bài toán theo cách khác: **doublewrite buffer**. Trước khi ghi một page vào chỗ
của nó, InnoDB ghi page đó vào một vùng riêng (file `#ib_16384_0.dblwr` bạn thấy ở bài 0), fsync,
rồi mới ghi vào chỗ thật. Page thật có rách thì vẫn còn bản trong vùng doublewrite.

### Checkpoint

Nếu log cứ dài mãi thì recovery sẽ phải đọc lại từ đầu. **Checkpoint** là lúc DB ghi mọi page bẩn
xuống file dữ liệu và đánh dấu trong log: *"mọi thứ trước điểm này đã nằm an toàn trong file dữ
liệu"*. Recovery chỉ cần bắt đầu từ checkpoint gần nhất, và log trước đó có thể được dọn đi.
Tiến trình `checkpointer` ở bài 0 làm đúng việc này.

Checkpoint cũng là lý do lần chạm đầu tiên vào mỗi page đắt hơn: sau mỗi checkpoint, lần sửa đầu
tiên của mỗi page lại phải chép nguyên page. Checkpoint càng dày thì WAL càng to.

minidb làm lại toàn bộ quá trình này theo thuật toán ARIES (analysis → redo → undo), và bị giết
bằng `kill -9` 200 lần ở những thời điểm ngẫu nhiên. Cả 200 lần, mọi transaction đã commit đều
còn nguyên ([`diary/phase5.md`](../diary/phase5.md)).

## Mang về dùng

1. **Đừng tắt `fsync` hay `full_page_writes`** của Postgres, trừ khi bạn chấp nhận được việc **mất
   cả database** chứ không chỉ mất vài commit. Tắt `fsync` thì mất điện có thể để lại file dữ
   liệu hỏng mà log không cứu được.
2. **`synchronous_commit = off` và `innodb_flush_log_at_trx_commit = 0/2` là đánh đổi có chủ ý:**
   nhanh hơn nhiều, và mất những commit gần nhất khi sự cố. Thí nghiệm trên cho thấy "mất" ở đây
   là thật: 2400 và 9123 commit đã báo thành công.
3. **Đừng giả định MySQL và MariaDB giống nhau.** Cùng một biến `innodb_flush_log_at_trx_commit=0`,
   MySQL (có binlog) không mất gì còn MariaDB (không có binlog) mất 9123 commit. Mặc định khác
   nhau đổi luôn cả hành vi khi có sự cố Chi tiết ở [bài 12](12-ai-goi-write.md).
4. **WAL tăng vọt sau mỗi checkpoint** vì full page write. Nếu dung lượng WAL hay độ trễ
   replication là vấn đề, xem lại `checkpoint_timeout` / `max_wal_size`: checkpoint thưa hơn thì
   ít full page write hơn (đổi lại, recovery lâu hơn).
5. **Muốn biết một transaction sinh bao nhiêu log**, lấy hiệu hai lần `pg_current_wal_lsn()` như
   thí nghiệm 2. Đó cũng là số byte sẽ phải gửi sang replica.

---

Script: [`blog/lab/05-wal-pg.sql`](lab/05-wal-pg.sql), [`reallab/crash.go`](../reallab/crash.go) ·
WAL của minidb: [`internal/wal/`](../internal/wal), recovery: [`internal/db/recover.go`](../internal/db/recover.go) ·
nhật ký: [`diary/phase5.md`](../diary/phase5.md).

**Bài tiếp theo:** [Bài 6 — Isolation level không phải là định nghĩa](06-isolation.md)
