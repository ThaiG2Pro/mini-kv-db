# Bài 12 — Cùng một biến, hai database, chênh nhau 575 lần

> Series [Mở nắp database](README.md) · bài 12 · bài thêm · cần đọc trước: [bài 1](01-commit.md), [bài 5](05-wal.md)

Bài 5 để lại một câu hỏi chưa trả lời. Cùng đặt `innodb_flush_log_at_trx_commit = 0` (bảo DB
"commit xong thì báo OK luôn, đừng chờ log"), cùng bị `kill -9` 5 lần giữa lúc đang ghi:

- **MariaDB** mất **9123** commit đã báo thành công.
- **MySQL**, sau khi tắt luôn fsync của binlog, chỉ mất **6**.

Cả hai đều dùng InnoDB, và tài liệu của cả hai đều nói gần như cùng một câu: *"có thể mất tới
khoảng 1 giây transaction"*. Vậy khoảng cách 1500 lần kia từ đâu ra?

Bài này trả lời câu đó, và trên đường đi, **bộ đo của mình nói dối mình một lần**.

## Ba tầng giữa `COMMIT` và mặt đĩa

Trước khi đo, cần tách rõ một chuyện mà bài 5 mới nói lướt qua. Byte log đi qua **ba** chỗ trước khi
thật sự an toàn:

```text
  ① log buffer            ② page cache của OS          ③ đĩa
  (RAM của tiến trình DB)  (RAM của kernel)
          │                        │
          └──── write() ──────────►└──── fsync() ──────►
```

Mỗi sự cố xoá một phần khác nhau:

| Sự cố | Mất ① | Mất ② |
|---|---|---|
| tiến trình DB chết (`kill -9`, OOM killer, crash do bug) | có | **không**: kernel vẫn sống và sẽ ghi ② xuống đĩa |
| cả máy mất điện, kernel panic | có | có |

Vậy khi `kill -9`, số commit bị mất chỉ phụ thuộc vào **một** câu hỏi: *từ lần `write()` gần nhất
tới giờ, đã có bao nhiêu commit?* fsync không liên quan gì. Nếu DB gọi `write()` mỗi giây một
lần và nhận 4000 commit mỗi giây, thì trung bình mỗi lần kill mất khoảng nửa giây commit, tức
khoảng 2000. Nếu nó gọi `write()` mỗi 6ms thì mỗi lần mất khoảng 12.

Đó là một giả thuyết đo được. Viết kỳ vọng ra trước: **MySQL 8 có một luồng riêng tên
`log_writer`, liên tục đẩy log buffer xuống OS. MariaDB thì không có, nên chờ tới nhịp mỗi giây.**

## Thí nghiệm 1: đếm số lần redo ra khỏi tiến trình

Một kết nối chèn liên tục, mỗi câu một commit. Một kết nối khác cứ 5ms đọc bộ đếm của InnoDB một
lần và ghi lại những lúc nó nhảy. Khoảng cách giữa hai lần nhảy chính là độ rộng của "cửa sổ mất".

```bash
cd reallab && go run . -work lograte -db mysql,maria
```

Cả hai DB đều có một bộ đếm tên `Innodb_os_log_written`, *"số byte đã ghi vào redo log"*. Dùng nó
cho cả hai:

```text
db      chế độ                                          commit/s  số write  khoảng p50  khoảng max
mysql   =0 + sync_binlog=0                                  4077       514       5.8ms       7.2ms
mysql   =0 + sync_binlog=0 + log_writer_threads=OFF         4194         8     172.8ms     826.9ms
maria   flush_log_at_trx_commit=0                           4366       526       5.6ms       7.4ms
```

MySQL đúng như kỳ vọng: 514 lần trong 3 giây. Tắt luồng `log_writer` thì chỉ còn 8.

Nhưng **MariaDB cũng "ghi mỗi 5.6ms"**. Nếu vậy thật thì nó không thể mất 9123 commit. Một con số
mâu thuẫn với một con số khác đã đo được, nên thứ đáng nghi đầu tiên là **bộ đo**, không phải
database.

## Bộ đếm nói dối

Lấy mẫu tay trong lúc MariaDB đang nhận ghi, đặt bộ đếm kia cạnh hai bộ đếm LSN:

```text
Innodb_lsn_current 3602546893   Innodb_lsn_flushed 3601694282   Innodb_os_log_written 3789950   giây 38.077
Innodb_lsn_current 3603397116   Innodb_lsn_flushed 3601694282   Innodb_os_log_written 4640173   giây 38.495
Innodb_lsn_current 3604430049   Innodb_lsn_flushed 3603725140   Innodb_os_log_written 5673106   giây 38.897
```

Giữa hai dòng đầu, `Innodb_os_log_written` tăng **850223** byte, và `Innodb_lsn_current` (vị trí
cuối của log, kể cả phần còn trong RAM) cũng tăng **850223** byte. Không lệch một byte nào. Còn
`Innodb_lsn_flushed` đứng yên suốt 0.4 giây đó, rồi nhảy một lần.

Vậy trên MariaDB 11.8, bộ đếm mang tên "đã ghi" thật ra đếm lượng log được **sinh ra**. Cùng tên
biến, cùng họ InnoDB, nhưng ở MySQL nó đếm byte đã `write()`, còn ở MariaDB nó đếm LSN. Bộ đếm
đúng cho MariaDB là `Innodb_lsn_flushed`. MariaDB mở file redo với `O_DIRECT`
(`innodb_log_file_buffering=OFF`), bỏ qua page cache của OS, nên với nó `write()` và "xuống đĩa"
là cùng một lần.

Sửa bộ đo rồi chạy lại hai lượt:

```text
db      chế độ                                          commit/s  số write  khoảng p50  khoảng max  dự báo mất/kill
mysql   =0 + sync_binlog=0                                  3819       508       5.9ms       7.6ms              11
mysql   =0 + sync_binlog=0 + log_writer_threads=OFF         2516         8     184.2ms     818.6ms             844
maria   flush_log_at_trx_commit=0                           2897         3    1001.7ms    1003.2ms            1363

mysql   =0 + sync_binlog=0                                  2425       473       6.2ms      19.5ms               8
mysql   =0 + sync_binlog=0 + log_writer_threads=OFF         2295         9     189.7ms     799.4ms             767
maria   flush_log_at_trx_commit=0                           2001         3    1002.6ms    1007.1ms             857
```

MariaDB ghi **3 lần trong 3 giây**, cách nhau 1002ms như một chiếc đồng hồ. Cột cuối là dự báo số
commit mất mỗi lần kill: lần kill rơi vào một khoảng dài g với xác suất tỉ lệ với g, và trong
khoảng đó trung bình mất g/2.

## Thí nghiệm 2: tắt một biến, giết thật

Một con số khớp chưa phải là bằng chứng nhân quả. Muốn chắc thì phải **vặn đúng một núm** và xem
kết quả có đổi theo dự báo không. MySQL cho tắt luồng `log_writer` bằng một biến. Nếu giả thuyết
đúng, tắt nó đi thì MySQL phải mất khoảng 800 commit mỗi lần kill, tức là mất như MariaDB.

```bash
cd reallab && go run . -work crash -db mysql
```

```text
chế độ                                    vòng   đã báo OK       mất      thừa   khởi động
mặc định (flush_log_at_trx_commit=1)       5/5        3464         0         4        2.5s
flush_log_at_trx_commit=2                  5/5        4715         0         4        2.4s
flush_log_at_trx_commit=0                  5/5        5070         0         5        2.4s
flush_log_at_trx_commit=0 + sync_binlog=0     0/5       24339         6         0        2.6s
=0 + sync_binlog=0 + writer_threads=OFF     0/5       21242      3447         0        2.6s
```

| | dự báo mất mỗi lần kill | đo được |
|---|---|---|
| MySQL, có `log_writer` | 8–11 | 6 / 5 lần = **1.2** |
| MySQL, tắt `log_writer` | 767–844 | 3447 / 5 lần = **689** (lượt trước: 597) |
| MariaDB | 857–1363 ở 2000–2900 commit/s | 9123 / 5 lần = **1825** ở ~4300 commit/s |

Cả ba dòng đều đúng bậc. Chỉ đổi một biến mà MySQL đi từ 6 lên 3447 commit mất: **575 lần**.

## Bên trong: ai gọi `write()`

`innodb_flush_log_at_trx_commit` **không** quyết định khi nào log được ghi. Nó chỉ quyết định
**commit có chờ hay không**:

| giá trị | commit chờ tới đâu rồi mới báo OK |
|---|---|
| `1` | ③ đã fsync |
| `2` | ② đã `write()` |
| `0` | không chờ gì cả |

Còn **ai** đẩy log đi, và **khi nào**, là chuyện của kiến trúc bên dưới:

- **MySQL 8** tách việc này ra hai luồng nền. `log_writer` gọi `write()` ngay khi log buffer có
  dữ liệu mới (đo được: ít nhất mỗi 6ms). `log_flusher` gọi fsync. Ở `=0`, commit không chờ ai,
  nhưng `log_writer` vẫn chạy phía sau nó, nên cửa sổ mất khi tiến trình chết chỉ rộng vài ms.
- **MariaDB** không có luồng `log_writer`. Ở `=0`, log nằm trong buffer cho tới lần ghi mỗi giây
  của master thread (`innodb_flush_log_at_timeout=1`). Cửa sổ mất rộng cả giây.

minidb có đúng ba tầng này, mỗi tầng một hàm trong `internal/wal/log.go`:

```go
// Append cấp LSN cho record và đưa nó vào buffer. KHÔNG chạm đĩa.       → tầng ①
func (l *Log) Append(r *Record) (uint64, error) {
	lsn := l.end
	l.buf = Encode(l.buf, r, lsn)
	l.end += uint64(r.Size())
	return lsn, nil
}

// writeLocked đẩy buffer ra file tới `upto` (không fsync).              → ① sang ②
func (l *Log) writeLocked(upto uint64) error {
	l.f.WriteAt(l.buf[from:to], int64(l.written))
	l.written = upto
	// ...
}

// Flush đảm bảo mọi record có LSN < upto đã nằm trên đĩa (đã fsync).   → ② sang ③
func (l *Log) Flush(upto uint64) error {
	// ...
	if l.NoWrite { l.flushed = target; return nil }   // như MariaDB =0: dừng ở ①
	l.writeLocked(target)
	if l.NoSync { l.flushed = target; return nil }    // như MySQL =0/=2: dừng ở ②
	l.f.Sync()
	// ...
}
```

Hai cờ kiểm thử của minidb ứng đúng với hai hành vi đã đo:

- **`NoSync`** (write mà không fsync) giống MySQL có `log_writer`. Phase 5 đã gặp đúng chuyện này:
  `crashlab -nosync` bị `kill -9` 20 lần mà **vẫn không mất gì**, vì page cache sống lâu hơn tiến
  trình.
- **`NoWrite`** (byte ở lại trong RAM của tiến trình) giống MariaDB ở `=0`. Phải thêm cờ này thì
  `crashlab` mới lần đầu bắt được dữ liệu mất: 10/10 vòng sai.

Tức là minidb đã vấp đúng cái bẫy này từ phase 5, bốn phase trước khi đo MySQL.

## Mang về dùng

1. **Đọc `flush_log_at_trx_commit` là "commit chờ tới đâu", không phải "log ghi lúc nào".** Mức mất
   dữ liệu thật phụ thuộc vào kiến trúc bên dưới. Hai DB cùng tên engine, cùng giá trị biến, chênh
   nhau tới ~1500 lần khi tiến trình chết.
2. **Hỏi riêng hai loại sự cố.** Tiến trình chết (OOM killer, crash, container bị giết) thì MySQL
   `=0` mất vài commit. Mất điện thì cả hai đều có thể mất tới ~1 giây, vì `log_flusher` của MySQL
   cũng chỉ fsync mỗi giây. Nếu bạn chạy trên cloud, nơi máy ảo biến mất thường gặp hơn mất điện
   trong datacenter, thì loại sự cố thứ hai mới là loại cần tính.
3. **Bộ đếm cùng tên không có nghĩa là cùng nghĩa.** Trước khi vẽ dashboard từ
   `Innodb_os_log_written` (hay bất kỳ bộ đếm nào) trên một DB mới, đặt nó cạnh một bộ đếm khác mà
   bạn đã hiểu rõ, và xem chúng có tăng như bạn nghĩ không. Ở đây, chỉ ba dòng lấy mẫu tay là đủ
   bắt được lỗi.
4. **Một con số mâu thuẫn với một con số khác thì nghi bộ đo trước.** "Ghi mỗi 5.6ms" và "mất 9123
   commit" không thể cùng đúng. Lần này, người sai là mình, không phải database.

---

Code: [`reallab/lograte.go`](../reallab/lograte.go), [`reallab/crash.go`](../reallab/crash.go) ·
WAL của minidb: [`internal/wal/log.go`](../internal/wal/log.go) ·
nhật ký: [`diary/phase9.md`](../diary/phase9.md) (bảng 7), [`diary/phase5.md`](../diary/phase5.md) (mục "kill -9 không phải mất điện").

**Về mục lục:** [README](README.md)
