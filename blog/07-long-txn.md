# Bài 7 — Transaction quên `COMMIT`

> Series [Mở nắp database](README.md) · bài 7/11 · cần đọc trước: [bài 2](02-page.md), [bài 6](06-isolation.md)

Một đồng nghiệp mở `psql` trên production, gõ `BEGIN;`, chạy vài câu `SELECT` để điều tra một lỗi,
rồi đi ăn trưa. Không ai sửa gì cả, chỉ đọc. Ba tiếng sau, dashboard báo các câu truy vấn chậm dần
đều, ổ đĩa đầy thêm vài chục GB, và không ai hiểu vì sao.

Bài này đo xem một transaction **chỉ đọc**, bị bỏ quên, làm gì với database, và vì sao hậu quả ở
Postgres khác hẳn ở MySQL.

## Thí nghiệm

Một phiên mở transaction `REPEATABLE READ`, đọc bảng một lần rồi để đó. Trong lúc ấy, ứng dụng vẫn
chạy bình thường: cập nhật cả bảng 100000 hàng, 10 vòng, mỗi vòng là 100 transaction nhỏ. Sau mỗi
vòng đo kích thước bảng, số phiên bản cũ, và thời gian `SELECT sum(v) FROM bl` của **một người đọc
mới** và của **chính phiên bị bỏ quên**.

```bash
cd reallab && go run . -work bloat
```

```text
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
...
5            14.2             —         9.6        77.5           504      55.6
10           14.2             —         9.6       173.8          1005      68.2
sau COMMIT phiên cũ, purge nền chạy xong trong 200ms:
sau          14.2             —        13.6           —             0      68.2
```

Gom lại sau 10 vòng, tức 1 triệu phiên bản cũ:

| | Postgres | MySQL | MariaDB |
|---|---|---|---|
| bảng phình | **11x** (14 → 155MB) | không đổi | không đổi |
| người đọc **mới** chậm đi | **5.1x** | không đổi | không đổi |
| phiên **bị bỏ quên** chậm đi | 5.9x | **35x** | **18.5x** |
| sau khi phiên cũ đóng | `VACUUM` dọn xác nhưng **bảng vẫn 155MB** | tự dọn trong 8s | tự dọn trong 200ms |

Cùng một phiên bị bỏ quên. Ở Postgres, **mọi người** cùng trả giá. Ở InnoDB, chỉ **chính phiên
đó** trả giá.

## Bên trong: một phiên cũ giữ lại cái gì?

Bài 6 đã nói: mỗi transaction đọc một **bản chụp** của dữ liệu. Phiên bị bỏ quên chụp lúc 12 giờ
trưa. Sau đó mỗi câu `UPDATE` đều tạo ra phiên bản mới, và phiên bản cũ **không được phép dọn**,
vì phiên lúc 12 giờ, về lý thuyết, vẫn có thể đọc lại nó. DB không biết phiên đó đã bị bỏ quên;
nó chỉ biết phiên đó còn mở.

Mọi DB có MVCC đều tính một con số gọi là **horizon**: phiên bản nào cũ hơn *snapshot cũ nhất còn
sống* thì không ai còn cần, và dọn được. Trong minidb (`internal/txn/store.go`):

```go
// horizon là OldestXmin: id nhỏ nhất mà một transaction còn sống vẫn có thể
// coi là vô hình. Mọi version có xmin < horizon thì với MỌI snapshot đang
// sống, hoặc nhìn thấy được, hoặc đã bị một bản nhìn thấy được che đi.
func (s *Store) horizon() uint64 {
	h := s.next
	for _, t := range s.active {
		if g := t.gcXmin.Load(); g < h {
			h = g
		}
	}
	return h
}
```

Đúng một vòng `for` lấy giá trị nhỏ nhất. Một transaction duy nhất còn mở là đủ ghim horizon lại
mãi mãi. Postgres cho bạn thấy đúng điều đó trong `VACUUM VERBOSE`, khi có một phiên đang mở:

```text
INFO:  vacuuming "lab.public.w5"
tuples: 0 removed, 2000 remain, 1000 are dead but not yet removable
removable cutoff: 307849, which was 1 XIDs old when operation ended
```

*"1000 are dead but not yet removable"*: 1000 hàng đã chết nhưng chưa được phép dọn.

### Vì sao hậu quả khác nhau? Vì phiên bản cũ nằm ở chỗ khác nhau

Đây là câu hỏi đã gặp ở bài 2 và bài 4: *nó để cái đó ở đâu?*

```text
POSTGRES: phiên bản cũ nằm NGAY TRONG BẢNG (heap)

  page: [ hàng1 v11 ][ hàng1 v10 ✝ ][ hàng1 v9 ✝ ] ... [ hàng2 v11 ][ hàng2 v10 ✝ ] ...
         ▲ mọi lần quét, của BẤT KỲ AI, cũng phải đi qua các xác ✝ này


INNODB: bản mới nhất nằm tại chỗ, bản cũ nằm trong UNDO LOG

  bảng: [ hàng1 v11 ][ hàng2 v11 ] ...        ← người đọc mới: đọc thẳng, không biết gì
            │
            └── undo: v10 ← v9 ← v8 ← ...      ← chỉ phiên cũ phải đi ngược chuỗi này
```

- **Postgres** giữ phiên bản cũ ngay trong heap (bài 2: `UPDATE` ghi một hàng mới, hàng cũ được
  đánh dấu `t_xmax`). Mỗi vòng `UPDATE` thêm đúng một bản sao của cả bảng: 14MB mỗi vòng. Người
  đọc mới cũng phải lội qua 1 triệu xác hàng để tìm bản mình được thấy.
- **InnoDB** sửa hàng **tại chỗ**, và chỉ ghi phần thay đổi vào **undo log**. Người đọc mới thấy
  ngay bản mới nhất, như thể không có chuyện gì. Chỉ phiên cũ phải lấy bản mới nhất rồi áp ngược
  từng bản ghi undo cho tới khi về đúng thời điểm của nó. Chuỗi đó dài ra sau mỗi vòng, nên phiên
  cũ chậm dần tới 35 lần.

Hai chi tiết nữa từ bảng số:

- **Undo chỉ ghi phần thay đổi.** 1 triệu phiên bản cũ mà undo của MariaDB chỉ tăng 13.7MB. Heap
  của Postgres tăng 141MB cho cùng số phiên bản cũ, vì mỗi phiên bản là **cả một hàng**.
- **`history list length` đếm transaction, không đếm hàng.** Lần đo đầu tiên cập nhật cả bảng
  bằng một câu `UPDATE` duy nhất mỗi vòng, và history list chỉ lên tới 21. Đổi sang 100 transaction
  nhỏ mỗi vòng thì nó lên 1009. Khi theo dõi con số này trên production, hãy nhớ nó là số
  **transaction** chưa được dọn.

### `VACUUM` không làm bảng nhỏ lại

Sau khi phiên cũ đóng, `VACUUM` dọn hết xác trong 93ms. Nhưng bảng **vẫn là 155MB**, và seq scan
vẫn chậm 2.8 lần so với ban đầu. `VACUUM` chỉ đánh dấu chỗ trống bên trong page để tái sử dụng
(bài 2), chứ không trả file lại cho hệ điều hành. Muốn bảng co lại phải `VACUUM FULL`, tức viết
lại cả bảng, dưới một khoá **chặn mọi đọc và ghi** trong suốt thời gian chạy. Trên một bảng vài
trăm GB, đó là downtime. Công cụ `pg_repack` làm việc tương tự mà không giữ khoá lâu như vậy.

minidb thì đứng về phía Postgres, và còn tệ hơn: nó giữ cả chuỗi phiên bản **trong cùng một
record**, nên ai đọc cũng phải giải mã cả chuỗi. Phase 6 đo được bảng phình 18 lần vì một reader
còn mở. Cách InnoDB làm (bản mới tại chỗ, bản cũ ra undo, chỉ ghi phần thay đổi) chính là cách
minidb cần học để trả món nợ đó.

## Thí nghiệm nhỏ: tìm phiên bị bỏ quên

Mở một phiên, `BEGIN`, đọc một câu, rồi để đó. Từ một cửa sổ khác:

```console
$ reallab/q.sh pg <<< "SELECT pid, state, now() - xact_start AS mo_tu_bao_gio, backend_xmin, left(query, 40) AS cau_cuoi
                       FROM pg_stat_activity WHERE state = 'idle in transaction';"
 pid |        state        | mo_tu_bao_gio  | backend_xmin |         cau_cuoi
-----+---------------------+----------------+--------------+--------------------------
  72 | idle in transaction | 00:00:03.93575 |       307849 | SELECT count(*) FROM w5;

$ reallab/q.sh mysql <<< "SELECT trx_id, trx_state, TIMESTAMPDIFF(SECOND, trx_started, NOW()) AS giay,
                          trx_mysql_thread_id, trx_isolation_level FROM information_schema.innodb_trx;"
| trx_id          | trx_state | giay | trx_mysql_thread_id | trx_isolation_level |
| 408783450115288 | RUNNING   |    5 |                   9 | REPEATABLE READ     |
```

`backend_xmin = 307849` của Postgres chính là con số `removable cutoff: 307849` mà `VACUUM` đã
in ra ở trên: phiên này đang ghim horizon.

## Mang về dùng

1. **Đặt giới hạn thời gian cho transaction bị bỏ quên:**
   - Postgres: `idle_in_transaction_session_timeout = '5min'` (tự ngắt phiên "idle in transaction"
     quá 5 phút), và `transaction_timeout` (Postgres 17+) cho cả transaction đang chạy.
   - MySQL/MariaDB: không có biến tương đương trực tiếp; theo dõi `information_schema.innodb_trx`
     và kill những transaction mở quá lâu.
2. **Theo dõi đúng con số:**
   - Postgres: `pg_stat_activity` với `state = 'idle in transaction'` và tuổi của `xact_start`;
     `n_dead_tup` trong `pg_stat_user_tables`.
   - InnoDB: `History list length` trong `SHOW ENGINE INNODB STATUS`. Nhớ là nó đếm transaction.
3. **Báo cáo chạy lâu thì chạy trên replica.** Nhưng ở Postgres, nếu bật
   `hot_standby_feedback`, transaction lâu trên replica **vẫn ghim horizon trên máy chính**.
4. **Ở Postgres, bảng đã phình thì không tự co lại.** Phòng bệnh (timeout, autovacuum đủ mạnh)
   rẻ hơn chữa bệnh (`VACUUM FULL` / `pg_repack`) rất nhiều.
5. **Transaction ngắn là quy tắc chung cho mọi DB có MVCC.** Mở transaction, làm việc, commit.
   Đừng giữ transaction mở trong lúc chờ người dùng bấm nút hay chờ một API bên ngoài trả lời.

---

Bộ đo: [`reallab/bloat.go`](../reallab/bloat.go) · số đo gốc và lỗi đo history list:
[`diary/phase9.md`](../diary/phase9.md), bảng 4 · MVCC của minidb: [`internal/txn/`](../internal/txn) ·
[`diary/phase6.md`](../diary/phase6.md).

**Bài tiếp theo:** [Bài 8 — Khi nào index không được dùng](08-index.md)
