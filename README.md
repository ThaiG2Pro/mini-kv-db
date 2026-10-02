# minidb — tự viết một database để hiểu database thật

[![ci](https://github.com/ThaiG2Pro/mini-kv-db/actions/workflows/ci.yml/badge.svg)](https://github.com/ThaiG2Pro/mini-kv-db/actions/workflows/ci.yml) ![Go](https://img.shields.io/badge/Go-1.26-00ADD8) ![license](https://img.shields.io/badge/license-MIT-green)

> Một relational database mini bằng Go, viết từ `pread`/`fsync` lên tới `EXPLAIN`,
> để học **DB internals** theo cách duy nhất tôi tin: mỗi phase phải kết thúc bằng
> **một crash-test hoặc một benchmark** chứng minh mình hiểu, không phải bằng "code chạy được".

```sql
minidb> EXPLAIN SELECT ev.city, dim.name FROM ev JOIN dim ON ev.kind = dim.kind
        WHERE dim.kind < 5 ORDER BY ev.city;

logical (tối ưu):  Project -> Sort -> Join -> Scan ev preds=(ev.kind < 5)   <- điều kiện này
                                           -> Scan dim preds=(dim.kind < 5)    không có trong câu SQL,
physical:          Sort budget=4096 hàng                                       optimizer tự suy ra
                     -> NestedLoopJoin                     (rows≈1  cost≈10)
                       -> SeqScan ev  filter=(ev.kind < 5) (rows≈5  cost≈5)
                       -> SeqScan dim [kind < 5]           (rows≈1  cost≈1)   <- quét khoảng trên pk
```

Không dùng thư viện DB nào. Chỉ Go stdlib + syscall. **~32k dòng Go, 191 test, 11 fuzz target,
10 phase, 13 bài blog, 39 commit trong một tháng** (09/2026).

---

## Dành cho người có 3 phút

| Tôi tự xây | Rồi chứng minh bằng |
|---|---|
| **Pager** · file = mảng page 4 KB, 2 meta page luân phiên + crc32c, freelist | 40 điểm crash mô phỏng, mở lại luôn hợp lệ |
| **Slotted page** · record biến độ dài, compact | 1.42 triệu vòng fuzz, bất biến không vỡ lần nào |
| **Buffer pool** · pin/unpin, LRU / CLOCK / LRU-K | hit ratio trên zipfian + sequential flooding, so với Belady |
| **B+Tree** · split, merge, redistribute, cursor | property test 7 bất biến; 1M khóa: chèn ngẫu nhiên tốn **33x** page write so với tăng dần |
| **WAL + ARIES-lite** · analysis/redo/undo, checkpoint mờ, group commit | **200/200 lần `kill -9`** ngẫu nhiên, 9194 txn đã commit được kiểm. Và bài **phản chứng**: cố tình làm mất log, test **phải đỏ** 10/10 |
| **MVCC + S2PL** · snapshot, 4 mức isolation, deadlock qua wait-for graph | bảng 5 anomaly × 4 mức khớp lý thuyết từng ô, khẳng định **cả hai chiều** (có anomaly khi lý thuyết nói có, không khi nói không) |
| **Index + planner** · khóa giữ thứ tự byte, catalog nhiều bảng trong một cây, mô hình chi phí | 3 kế hoạch cho cùng kết quả; điểm hoà vốn selectivity **đo được**, không chép sách |
| **SQL** · lexer/parser/binder, logical vs physical plan, Grace hash join tràn đĩa, external merge sort, predicate pushdown | hai đường thực thi cho cùng kết quả dưới `-race`; parser fuzz: phải kết thúc, in-lại-đọc-lại phải bền |
| **Đối chiếu DB thật** · cùng câu hỏi chạy trên Postgres 17 / MySQL 8.4 / MariaDB 11.8 | 11 bảng số, nhiều bảng bác lại chính kết luận của tôi ở phase trước |

Đọc một thứ duy nhất? [`diary/phase5.md`](./diary/phase5.md): WAL, recovery, và vì sao
một bài crash-test chỉ đáng tin khi nó **biết báo sai**.

---

## Những chỗ tôi đã sai, và số đo đã bắt được

Phần tôi tự hào nhất không phải code chạy, mà là **chuỗi giả thuyết sai được ghi lại và bác bỏ bằng số**:

1. **Điểm hoà vốn của index đo được 36.6%**, sách nói 5–20%. Tôi giải thích "vì minidb không có I/O
   thật". Phase 9 chạy Postgres / MySQL / MariaDB **cũng trong RAM**, 1 triệu hàng, và vẫn hoà vốn ở
   **11.7 / 6.6 / 6.1%**. Giải thích sai. Nguyên nhân thật: phí *quét* mỗi hàng của minidb đắt hơn hẳn,
   trong khi phí *tra* ngang InnoDB (nợ P9-1).
2. **Trả nợ P9-1** bằng profile: chỗ chậm không nằm ở copy byte mà ở **cấp phát**. Trên Linux thuần,
   cấp phát + chép 1 KB tốn 154–193 ns, chép không thôi 25 ns (~7x). Trên WSL2 khoảng cách là ~50x vì
   page fault của GC, một đặc thù môi trường, không phải của minidb. Thêm API zero-copy `GetFunc` và sửa
   đường đọc chuỗi version. A/B 16 cặp trên Linux thuần: seq scan **404 → 357 ns/hàng**, allocs
   140k → 40k, tỉ số theo cặp 0.87, khoảng tứ phân vị [0.85, 0.90] nằm hẳn dưới 1.0. Hoà vốn 41% → 36.5%,
   **chưa** xuống dưới 15% như mục tiêu: sửa đúng chỗ nhưng chưa đủ, phần còn lại là con trỏ B+Tree (P4-3).
3. **Một luật optimizer còn thiếu.** Bảng pushdown ở phase 8 cho 1.18x, quá ít. Thiếu luật suy ra
   điều kiện qua equi-join. Thêm luật: **18x**. Lần đầu số đo tìm ra thứ *chưa có*, không phải thứ sai.
4. **"MariaDB purge nhanh hơn MySQL 40 lần"** hoá ra là **bộ đếm**, không phải purge: MySQL chỉ giảm
   `history_list` khi truncate rollback segment, mỗi 128 batch. Undo thật xong trong ~2 s, bộ đếm treo
   thêm 11–85 s. Ba giả thuyết (số thread, kích thước batch, tần suất truncate) được viết ra *trước* khi
   chạy, hai cái đầu bị bác.
5. **MySQL để lọt lost update ở REPEATABLE READ**, Postgres không. UUIDv4 làm khóa chính ghi page
   **26–32x** trên InnoDB nhưng chỉ **~1.3x** trên Postgres (heap không clustered). Cùng tên isolation,
   cùng tên metric, khác nghĩa.

Toàn bộ ở [`diary/`](./diary) (một file `phaseN.md` chốt + một `phaseN-log.md` ghi theo giờ, cả giả
thuyết sai) và [`docs/debts.md`](./docs/debts.md): **sổ nợ kỹ thuật**, mỗi món là một thứ tôi biết
còn thiếu kèm **lệnh để trả nó**, 19 món đã trả, mỗi món còn lại đều có lệnh chạy.

---

## Kiến trúc

```
 cmd/minidb REPL ─┐
                  ▼
 internal/sql     lexer → parser → AST            CHỈ cú pháp, không biết catalog
 internal/plan    binder → logical rewrite → physical planner (cost model)
 internal/exec    Volcano iterator: SeqScan, IndexScan, NestedLoop, GraceHashJoin (tràn đĩa), ExternalSort
 internal/query   3 kế hoạch seq / index / index-only + ước lượng selectivity
 internal/table   catalog, nhiều bảng + index trong MỘT B+Tree theo tiền tố oid; bất biến hàng ↔ index
 internal/keys    mã hoá khóa giữ thứ tự: composite, ASC/DESC bằng phép bù, canonical
 internal/txn     MVCC: chuỗi version, snapshot, 4 mức isolation, vacuum
 internal/lock    S2PL: S/X, khóa điểm + khoảng, deadlock detection
 internal/db      Begin/Commit/Abort, checkpoint mờ, recovery 3 pha ARIES
 internal/wal     log record + crc32c, diff theo khối, LSN = offset byte, group commit
 internal/btree   node = 1 page, split / merge / redistribute, cursor
 internal/bufpool pin/unpin, dirty, LRU / CLOCK / LRU-K, WAL hook (WAL-before-data)
 internal/page    slotted page
 internal/pager   file = mảng page 4 KB, meta page kép + crc32c, freelist
                  pread / pwrite / fsync / posix_fadvise
```

Thiết kế theo nhánh **B+Tree + WAL, update-in-place** (Postgres / InnoDB / SQLite). Nhánh LSM để sau.

Mỗi tầng có một `cmd/*lab` riêng để **soi** nó: `pagerlab` xxd meta page, `wallab` đếm bao nhiêu
phần trăm file WAL là thuế, `btreelab` vẽ hình cây theo thứ tự chèn, `txnlab` bảng anomaly...

---

## Chạy thử trong 5 phút

Yêu cầu: Go 1.26+, Linux (dùng `pread`/`pwrite`, `posix_fadvise`). Phase 9 cần thêm Docker.

```bash
make test               # toàn bộ unit + property test
make repl               # REPL SQL. Thử: CREATE TABLE t (a INT, b TEXT, PRIMARY KEY (a));
make crashlab           # 20 lần kill -9 ngẫu nhiên rồi kiểm durability
make crashlab-nowrite   # PHẢI ĐỎ: chứng minh bài test trên biết báo sai
make txnlab-anomaly     # bảng anomaly × isolation, thoát 1 nếu lệch khỏi lý thuyết
make sqllab-join        # nested loop vs hash join, điểm đổi vai
```

<details>
<summary>Toàn bộ target theo phase</summary>

```
make iolab / iolab-full / baseline DIR=...     phase 0: fsync, group commit, random vs seq, page cache
make pagerlab                                   phase 1: bảng commit + xxd meta page
make slotlab / fuzz                             phase 2: bản đồ page, compact; fuzz slotted page
make bufferlab / fuzz-pool                      phase 3: hit ratio + sequential flooding
make btreelab / bench-btree / fuzz-btree        phase 4: hình cây, 1M khóa, fuzz đối chiếu map
make crashlab / crashlab-full / crashlab-nowrite / wallab / bench-wal / fuzz-db      phase 5
make txnlab / txnlab-anomaly / txnlab-contention / test-txn / bench-txn / fuzz-txn   phase 6
make idxlab / idxlab-breakeven / idxlab-bytes / test-index / bench-index / fuzz-keys / fuzz-table   phase 7
make repl / sqllab / sqllab-join / sqllab-budget / sqllab-sort / test-sql / fuzz-sql phase 8
cd reallab && docker compose up -d && go run . -work breakeven|anomaly|pkorder|bloat|stats|crash|lograte|hashjoin|purge   phase 9
make check                                      fsck cho file minidb: meta, freelist, double free, page mồ côi
```
</details>

---

## Bản đồ repo

```
internal/        15 package, mỗi package một tầng (xem Kiến trúc)
cmd/             minidb (REPL), dbcheck (fsck), và một *lab cho mỗi phase
reallab/         phase 9: module Go riêng + docker-compose, 9 thí nghiệm trên Postgres/MySQL/MariaDB
diary/           nhật ký học: phaseN.md (chốt, có số) + phaseN-log.md (theo giờ, có giả thuyết sai)
docs/            debts.md (sổ nợ), vs-postgres.md, vs-innodb.md, linux-runbook.md (đo lại trên Linux thuần)
blog/            series "Mở nắp database", 13 bài cho dev mới, mỗi bài một câu hỏi và một bảng số
bench/baseline/  kết quả đo lưu theo máy + ngày
charts/          plot.py: vẽ biểu đồ cho blog từ output bench
scripts/         linux-baseline.sh, linux-phase4-9.sh, dm-flakey.sh (bơm lỗi thiết bị để tạo torn write)
skills/          quy ước làm việc trong repo: đọc trước khi ghi diary
ROADMAP.md       kế hoạch 10 phase, mỗi phase ghi deliverable chứng minh hiểu
```

## Giới hạn, nói thẳng

- Không dùng cho production. Không có network, không có auth, một file một process.
- Số trong README đo trên Linux thuần (CachyOS, AMD Ryzen 7 H 255, 2026-10-01), output gốc ở
  [`bench/`](./bench) theo máy + ngày. Phần lớn bảng trong `diary/` đo trên WSL2 trước đó: tỉ số
  giữ nguyên, số tuyệt đối lệch. [`docs/linux-runbook.md`](./docs/linux-runbook.md) là buổi 2 giờ đo lại toàn bộ.
- Torn write thật chưa tái tạo được (`kill -9` không tạo torn write). Công cụ `dm-flakey` đã dựng, chưa chạy trên máy có quyền.
- Còn các món nợ mở trong [`docs/debts.md`](./docs/debts.md), mỗi món có lệnh để trả.
