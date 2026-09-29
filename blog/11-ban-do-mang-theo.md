# Bài 11 — Bản đồ mang theo

> Series [Mở nắp database](README.md) · bài 11/11 · bài tổng kết

Mười bài trước mở từng tầng của database ra một lần. Bài cuối này ghép chúng lại: đi theo **một**
câu lệnh từ lúc bạn bấm Enter tới lúc byte nằm yên trên đĩa, và ở mỗi bước ghi lại điều đáng nhớ
nhất, kèm con số đã đo được.

## Hành trình của một câu `UPDATE`

```sql
UPDATE accounts SET balance = balance - 10 WHERE email = 'an@mail.com';
COMMIT;
```

```text
 ① Parser → Optimizer → Planner                                      bài 8, 9, 10
    "email = 'an@mail.com'" là một khoảng trên index email? → có
    planner đoán: 1 hàng → chọn Index Scan
      ⚠ đoán bằng thống kê; thống kê cũ thì đoán 1 cho 100000 hàng thật
      ⚠ bọc cột trong hàm, LIKE '%…', sai kiểu tham số → index không dùng được

 ② Executor: cây toán tử, mỗi toán tử một vòng Next()                 bài 10
      ⚠ Sort và Hash là toán tử CHẶN: đọc hết đầu vào trước khi trả hàng đầu tiên

 ③ Transaction / MVCC / Lock                                          bài 6, 7
    ai khác đang sửa hàng này? → chờ khoá, hoặc bị huỷ (40001 / 1020 / 1213)
    phiên bản cũ của hàng phải giữ lại cho ai?
      ⚠ REPEATABLE READ của MySQL để lọt lost update; của Postgres thì không
      ⚠ một transaction quên COMMIT ghim mọi phiên bản cũ lại

 ④ B+Tree: đi từ gốc xuống lá, 3-4 bước                               bài 4
      ⚠ InnoDB: bảng CHÍNH LÀ cây khoá chính. UUIDv4 làm nó chậm 6-11x khi bảng lớn hơn RAM

 ⑤ Buffer pool: page có sẵn trong RAM chưa?                           bài 3
    có → sửa ngay trong RAM. Chưa → đọc từ đĩa, đuổi một page khác ra
      ⚠ một lần quét lớn có thể đuổi dữ liệu nóng ra; DB thật có cơ chế chống

 ⑥ Page: sửa hàng trong page                                           bài 2
    Postgres: ghi một bản MỚI, đánh dấu bản cũ. InnoDB: sửa tại chỗ, bản cũ ra undo

 ⑦ WAL: nối một bản ghi vào cuối log                                  bài 5
    lần sửa đầu tiên của page sau checkpoint: chép NGUYÊN page (8KB thay vì ~200 byte)

 ⑧ COMMIT: fsync log                                                   bài 1
    MỘT lần fsync, dù transaction sửa bao nhiêu page. Nhiều client thì gom chung
      ⚠ commit từng dòng chậm hơn commit một lần 170-310x

 ⑨ ... rất lâu sau: checkpoint ghi page đã sửa xuống file dữ liệu     bài 5
    mất điện trước lúc đó? → khởi động lại, đọc log, làm lại
```

Toàn bộ series là chín bước này. Không có bước nào là phép màu. Mỗi bước là một bài toán cụ thể,
có lời giải viết được trong vài nghìn dòng code, và minidb là bằng chứng: khoảng 30000 dòng Go,
đủ chín bước, và ở những chỗ cùng kiến trúc với DB thật, nó đo ra **cùng tỉ số**. Chèn khoá ngẫu
nhiên ghi page nhiều hơn 33 lần trên minidb, 26–32 lần trên InnoDB. Tra một hàng qua index phụ mất
2.1µs trên minidb, 1.8–2.1µs trên InnoDB.

## Hai câu hỏi dùng được cho mọi database

Nếu chỉ nhớ một điều từ series này, hãy nhớ hai câu hỏi. Chúng giải thích được gần như mọi khác
biệt giữa Postgres và MySQL mà ta đã đo:

**1. Nó để hàng ở đâu?**

| | Postgres | MySQL / MariaDB (InnoDB) |
|---|---|---|
| hàng nằm ở | một đống (heap) không thứ tự | lá của cây khoá chính |
| index phụ trỏ tới hàng bằng | địa chỉ vật lý `(page, ô)` | giá trị khoá chính |
| hệ quả đo được | UUIDv4 gần như không sao (1.0–1.8x) | UUIDv4 chậm 6.5–11x, ghi page gấp 26–32x |
| | tra một hàng qua index: 725ns | tra một hàng qua index: 1830–2060ns (đi xuống thêm một cây) |

**2. Nó để phiên bản cũ ở đâu?**

| | Postgres | MySQL / MariaDB (InnoDB) |
|---|---|---|
| phiên bản cũ nằm ở | ngay trong bảng, cạnh bản mới | undo log, chỉ lưu phần thay đổi |
| transaction quên commit | bảng phình 11x, **mọi** người đọc chậm 5x | bảng không đổi, chỉ **phiên đó** chậm 18–35x |
| dọn dẹp | `VACUUM`, và bảng không tự co lại | purge chạy nền |

Khi gặp một database mới (SQL Server, Oracle, CockroachDB, SQLite, …), hãy hỏi hai câu này trước
tiên. Câu trả lời cho biết thao tác nào sẽ rẻ và thao tác nào sẽ đắt, trước khi bạn chạy một
benchmark nào.

## Checklist cho dev

**Viết truy vấn**
- [ ] Điều kiện `WHERE` là một khoảng trên cột có index: không bọc cột trong hàm, không có `LIKE '%…'`, tham số đúng kiểu (bài 8)
- [ ] Index composite: cột dùng với `=` đứng trước, cột dùng cho khoảng hoặc `ORDER BY` đứng sau (bài 8)
- [ ] `ORDER BY … LIMIT` trên bảng lớn thì có index trên cột `ORDER BY` (bài 10)
- [ ] Truy vấn chậm: `EXPLAIN (ANALYZE, BUFFERS)`, so `rows` với `actual rows` trước khi sửa gì (bài 9, 10)

**Ghi dữ liệu**
- [ ] Import và batch job: commit mỗi vài trăm đến vài nghìn dòng, không commit từng dòng (bài 1)
- [ ] PK của bảng lớn trên InnoDB: tự tăng hoặc UUIDv7, không dùng UUIDv4 (bài 4)
- [ ] Chạy `ANALYZE` sau mỗi lần nạp hoặc xoá dữ liệu hàng loạt (bài 9)
- [ ] Đừng đánh index cho cột bị cập nhật liên tục nếu không thật sự cần (bài 2)

**Transaction**
- [ ] Mẫu *đọc → tính → ghi* dùng `UPDATE … SET x = x - ?`, `SELECT … FOR UPDATE`, hoặc cột `version` (bài 6)
- [ ] Code biết retry, và bắt đúng mã lỗi: `40001`, `40P01`, `1213`, **và `1020` của MariaDB** (bài 6)
- [ ] Transaction ngắn; không giữ transaction mở trong lúc chờ người dùng hay chờ API bên ngoài (bài 7)
- [ ] Đặt `idle_in_transaction_session_timeout` (Postgres); theo dõi `innodb_trx` và history list (InnoDB) (bài 7)

**Cấu hình**
- [ ] Không tắt `fsync` / `full_page_writes`. Nới `synchronous_commit` hay `innodb_flush_log_at_trx_commit` chỉ khi đã biết mình được phép mất gì (bài 1, 5)
- [ ] Buffer pool: InnoDB khoảng 70–80% RAM; Postgres `shared_buffers` khoảng 25%, phần còn lại để cho page cache (bài 3)
- [ ] Test hiệu năng với dữ liệu **lớn hơn buffer pool**, không chỉ với dữ liệu mẫu trên máy dev (bài 4)

## Đi tiếp từ đây

- **Tự chạy lại mọi thí nghiệm.** Mọi con số trong series đều có lệnh sinh ra nó:
  `docker compose -f reallab/docker-compose.yml up -d`, rồi `blog/lab/*.sql` và `reallab/`. Số
  tuyệt đối trên máy bạn sẽ khác, nhưng tỉ số thì nên giống. Nếu không giống, đó là một câu hỏi đáng
  đào tiếp.
- **Đọc code minidb theo thứ tự các tầng:** `internal/pager` → `page` → `bufpool` → `btree` → `wal`
  → `db` → `txn` → `keys` → `table` → `query` → `sql` → `plan` → `exec`. Mỗi thư mục là một bài toán,
  và nhật ký của từng phase ở `diary/` ghi lại cả những lần đoán sai trên đường đi.
- **Đọc thêm**, theo thứ tự:
  - *Database Internals* của Alex Petrov: sát với tầng storage và B+Tree nhất.
  - Bài giảng CMU 15-445 của Andy Pavlo (miễn phí trên YouTube): mỗi bài giảng ứng với một tầng.
  - *Architecture of a Database System* (Hellerstein, Stonebraker, Hamilton): khoảng 100 trang, tổng quan cả hệ thống.
- **Một hướng khác hẳn:** mọi thứ trong series là họ **B+Tree + WAL** (Postgres, MySQL, SQLite).
  Họ còn lại là **LSM-Tree** (RocksDB, Cassandra, và MyRocks bên trong MariaDB): ghi tuần tự vào
  bộ nhớ, xả ra file đã sắp xếp, rồi gộp nền. Nó đánh đổi ngược lại: ghi rẻ hơn nhiều, đọc đắt
  hơn. Đó là câu hỏi đầu tiên khi thiết kế storage cho một hệ thống lớn: *mình ưu tiên ghi, đọc,
  hay dung lượng?*

---

Cảm ơn bạn đã đọc tới đây. Nếu một con số trong series không khớp với máy của bạn, hoặc có chỗ
giải thích sai, hãy mở issue: series này được viết theo cùng quy tắc với nhật ký của minidb, nghĩa
là **cái sai được ghi lại, không bị xoá đi**.

**Về mục lục:** [README](README.md)
