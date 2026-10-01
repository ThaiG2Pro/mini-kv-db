# Mở nắp database — series blog

> Cho dev đã viết SQL hằng ngày, nhưng chưa bao giờ biết cái database đang chạy trên máy mình
> thật ra là gì. Không cần đọc hàng triệu dòng code của Postgres hay MySQL: mỗi bài lấy **một
> câu hỏi** bạn từng gặp, trả lời bằng **một thí nghiệm chạy được trong 5 phút** trên DB thật,
> rồi mở ra **vài chục dòng code** của một database đồ chơi ([minidb](../README.md)) làm đúng
> việc đó.

## Cách đọc

Mỗi bài có bốn phần, luôn theo thứ tự này:

1. **Câu hỏi:** một thứ bạn đã thấy nhưng chưa hiểu, kiểu *"sao đổi sang UUID thì insert chậm hẳn?"*.
2. **Thí nghiệm:** lệnh copy-paste chạy trên Postgres/MySQL thật (Docker), kèm số đo thật.
3. **Bên trong:** cơ chế, vẽ bằng một hình và kể bằng code minidb (ngắn, bỏ bớt phần phụ).
4. **Mang về dùng:** một hai quy tắc làm khác đi được ngay từ ngày mai.

Muốn chạy thí nghiệm thì chỉ cần Docker:

```bash
git clone https://github.com/ThaiG2Pro/mini-kv-db.git && cd mini-kv-db
docker compose -f reallab/docker-compose.yml up -d   # Postgres 17, MySQL 8.4, MariaDB 11.8
reallab/q.sh pg <<< "select version();"
```

## Mục lục

| # | Bài | Câu hỏi của bạn | Thí nghiệm | Phase |
|---|---|---|---|---|
| 0 | [Database trên máy bạn là gì?](00-ban-do.md) | "Postgres" thật ra là mấy file và mấy tiến trình? | Mở thư mục dữ liệu, tìm đúng file và đúng byte của một hàng | 1 |
| 1 | [Vì sao `COMMIT` chậm?](01-commit.md) | Sao insert từng dòng chậm hơn insert theo lô cả trăm lần? | 5000 INSERT: commit từng dòng chậm hơn 170–310x; 64 client chia nhau 1 fsync cho 27 commit | 0 |
| 2 | [Một hàng nằm ở đâu?](02-page.md) | `ctid` là gì, sao `UPDATE` làm nó đổi, `VACUUM` làm gì trong page? | Soi page bằng `pageinspect`: HOT update, redirect, compact | 2 |
| 3 | [RAM của database](03-buffer-pool.md) | Một câu `SELECT *` có đuổi dữ liệu nóng ra khỏi RAM không? | Ring buffer của Postgres; tắt `innodb_old_blocks_time` thì mất sạch | 3 |
| 4 | [Vì sao UUID làm chậm insert](04-uuid.md) | Sao UUIDv4 làm InnoDB chậm 6.5–11x mà Postgres gần như không sao? | 2 triệu hàng, khoá ngẫu nhiên vs tăng dần | 4, 9 |
| 5 | [Vì sao mất điện không mất dữ liệu?](05-wal.md) | WAL / redo log là gì, `kill -9` thì sao? | `kill -9` giữa lúc ghi ×15; full page write 8KB vs 176 byte | 5, 9 |
| 6 | [Isolation level không phải là định nghĩa](06-isolation.md) | Sao code chạy đúng trên Postgres lại mất tiền trên MySQL? | 5 anomaly × 4 mức × 3 DB | 6, 9 |
| 7 | [Transaction quên `COMMIT`](07-long-txn.md) | Một cửa sổ `psql` bỏ quên làm hỏng cả DB thế nào? | Heap phình 11x vs undo chỉ phạt phiên cũ | 6, 9 |
| 8 | [Khi nào index không được dùng](08-index.md) | Có index rồi mà sao vẫn quét cả bảng? | Năm cách viết `WHERE` làm mất index; hoà vốn 4.7 / 4.9 / 7.0% | 7, 9 |
| 9 | [Planner đoán mò](09-planner.md) | Sao cùng một câu, hôm qua 10ms hôm nay 10 giây? | Thống kê lệch, tương quan, cũ | 7, 9 |
| 10 | [Đọc `EXPLAIN` như người viết ra nó](10-explain.md) | `Hash Join`, `Sort`, `Batches`, `external merge` nghĩa là gì? | `EXPLAIN` của minidb vs Postgres; `LIMIT 10` chênh 221x | 8 |
| 11 | [Bản đồ mang theo](11-ban-do-mang-theo.md) | Tổng kết: một câu `UPDATE` đi qua những đâu | — | — |
| 12 | [Cùng một biến, hai database, chênh nhau 575 lần](12-ai-goi-write.md) | `flush_log_at_trx_commit=0` thật ra mất bao nhiêu? | Đếm nhịp `write()` của redo mỗi 5ms; tắt `log_writer` của MySQL: mất 6 → 3447 | 5, 9 |
| 13 | [Chỗ chậm không nằm ở chỗ bạn đoán](13-cho-cham-khong-o-cho-doan.md) | Tối ưu theo trực giác thì sai ở đâu? Đo thế nào trên một máy ồn? | Chuỗi 60 phiên bản: profile, chép vs cấp phát, A/B theo cặp: 5213 → 708ns | 6, 9 |

Số đo trong các bài lấy từ [`diary/phase9.md`](../diary/phase9.md) và các script ở [`lab/`](lab/), nơi có lệnh và output gốc.
Máy đo là một laptop chạy WSL2 (i5-1235U), nên số tuyệt đối không giống máy chủ thật. Chỉ tỉ số là đáng tin.
