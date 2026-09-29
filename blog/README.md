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
git clone <repo> && cd db
docker compose -f reallab/docker-compose.yml up -d   # Postgres 17, MySQL 8.4, MariaDB 11.8
reallab/q.sh pg <<< "select version();"
```

## Mục lục

| # | Bài | Câu hỏi của bạn | Thí nghiệm | Phase |
|---|---|---|---|---|
| 0 | [Database trên máy bạn là gì?](00-ban-do.md) | "Postgres" thật ra là mấy file và mấy tiến trình? | Mở thư mục dữ liệu, tìm đúng file của một bảng | — |
| 1 | Vì sao `COMMIT` chậm? | Sao insert từng dòng chậm hơn insert theo lô cả trăm lần? | `fsync` và group commit | 0 |
| 2 | Một hàng nằm ở đâu? | `ctid` là gì, sao `UPDATE` lại làm nó đổi? | Soi page bằng `pageinspect` | 1-2 |
| 3 | RAM của database | `shared_buffers` / buffer pool để làm gì, vì sao một câu `SELECT *` làm chậm cả hệ thống? | Hit ratio, sequential flooding | 3 |
| 4 | [Vì sao UUID làm chậm insert](04-uuid.md) | Đổi PK sang UUIDv4 thì InnoDB chậm 8-11x, Postgres gần như không sao. Vì sao? | Bảng 3 của phase 9 | 4, 9 |
| 5 | Vì sao mất điện không mất dữ liệu? | WAL / redo log là gì, `kill -9` thì sao? | 200 lần `kill -9` | 5 |
| 6 | Isolation level không phải là định nghĩa | Sao code chạy đúng trên Postgres lại mất tiền trên MySQL? | Bảng 5 anomaly × 4 mức × 3 DB | 6, 9 |
| 7 | Transaction quên `COMMIT` | Một cửa sổ `psql` bỏ quên làm hỏng cả DB thế nào? | Bloat vs history list | 6, 9 |
| 8 | Khi nào index không được dùng | Có index rồi mà sao planner vẫn quét cả bảng? | Điểm hoà vốn 4.7% / 4.9% / 7.0% | 7, 9 |
| 9 | Planner đoán mò | Sao cùng một câu, hôm qua 10ms hôm nay 10 giây? | Thống kê lệch, cũ, tương quan | 7, 9 |
| 10 | Đọc `EXPLAIN` như người viết ra nó | Các dòng `Hash Join`, `Sort`, `Bitmap Heap Scan` nghĩa là gì? | `EXPLAIN` của minidb vs Postgres | 8 |
| 11 | Bản đồ mang theo | Tổng kết: chuyện gì xảy ra từ lúc gõ `SELECT` tới lúc có kết quả | — | — |

Số đo trong các bài lấy từ [`diary/phase9.md`](../diary/phase9.md), nơi có lệnh và output gốc.
