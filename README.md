# minidb

Một database mini viết bằng Go để học **DB internals** — storage layer, B+Tree, WAL,
recovery, MVCC, secondary index, query planner và một front-end SQL — chứ không phải để dùng thật.

- Hướng đi chung: [`ROADMAP.md`](./ROADMAP.md)
- Nhật ký từng phase: [`diary/`](./diary)
- Quy tắc ghi nhật ký: [`skills/diary/SKILL.md`](./skills/diary/SKILL.md)
- Sổ nợ kỹ thuật: [`docs/debts.md`](./docs/debts.md)
- So với DB thật: [`docs/vs-postgres.md`](./docs/vs-postgres.md), [`docs/vs-innodb.md`](./docs/vs-innodb.md), và các thí nghiệm ở [`reallab/`](./reallab) (phase 9)
- Series blog cho dev mới: [`blog/`](./blog)

## Layout

```
cmd/iolab/      phase 0 — đo đặc tính I/O của máy (fsync, group commit, random vs seq, page cache)
cmd/tornlab/    phase 0 — dò torn write (dùng cùng scripts/dm-flakey.sh)
cmd/pagerlab/   phase 1 — soi file: meta page luân phiên, freelist, rollback bằng 1 byte
cmd/slotlab/    phase 2 — soi một slotted page: bản đồ page, phân mảnh, compact, churn
cmd/bufferlab/  phase 3 — hit ratio lru/clock/lru-2 vs Belady, sequential flooding
cmd/btreelab/   phase 4 — hình dạng cây theo thứ tự chèn, fanout, split, xóa và trả page
cmd/crashlab/   phase 5 — 200 lần kill -9 ngẫu nhiên rồi kiểm durability; -nowrite = bài phản chứng
cmd/wallab/     phase 5 — soi một file WAL: gồm record gì, bao nhiêu phần trăm là thuế
cmd/txnlab/     phase 6 — ba bảng: anomaly × mức isolation, chuyển tiền N goroutine, phình version
cmd/idxlab/     phase 7 — năm bảng: điểm hoà vốn selectivity, thuế của index, hình byte của khóa
cmd/minidb/     phase 8 — REPL SQL: gõ câu, xem kết quả, xem EXPLAIN cả cây logical lẫn physical
cmd/sqllab/     phase 8 — sáu bảng: join, hạn mức bộ nhớ, sắp ngoài, pushdown, bỏ Sort, front-end
cmd/dbcheck/    fsck cho file minidb — meta, freelist, double free, page mồ côi
reallab/        phase 9 — năm bảng trên Postgres/MySQL/MariaDB thật (module Go riêng + docker-compose)
blog/           series "Mở nắp database" cho dev mới
scripts/        linux-baseline.sh (đo baseline có thể so máy), dm-flakey.sh (bơm lỗi thiết bị)
docs/           debts.md (sổ nợ + lệnh trả từng món), linux-baseline.md + linux-phase4-9.md + linux-phase9.md (đo trên Linux thuần)
bench/baseline/ kết quả đo lưu theo máy + ngày (text + JSON)
internal/pager/ phase 1 — file = mảng page 4KB, meta page kép + crc32c, freelist
internal/page/  phase 2 — slotted page: record biến độ dài, slot indirection, compact
internal/bufpool/ phase 3 — buffer pool: pin/unpin, dirty, LRU/CLOCK/LRU-K, WAL hook
internal/btree/ phase 4 — B+Tree: node = 1 page, split, merge/redistribute, cursor
internal/wal/   phase 5 — log record + crc32c, diff theo khối, LSN = offset byte, group commit
internal/db/    phase 5 — transaction (Begin/Commit/Abort), checkpoint mờ, recovery 3 pha ARIES
internal/lock/  phase 6 — lock manager S2PL: S/X, khóa điểm và khoảng, deadlock qua wait-for graph
internal/txn/   phase 6 — MVCC: chuỗi version, snapshot, 4 mức isolation, vacuum, deferred write
internal/keys/  phase 7 — bộ mã hoá khóa giữ thứ tự: composite, ASC/DESC bằng phép bù, canonical
internal/table/ phase 7 — catalog + nhiều bảng/index trong MỘT cây theo tiền tố oid; bất biến hàng<->index
internal/query/ phase 7 — 3 kế hoạch (seq/index/index-only), mô hình chi phí, ước lượng selectivity
internal/sql/   phase 8 — lexer + parser + AST. CHỈ cú pháp: không biết catalog, nên câu sai tên chết ở tầng sau
internal/plan/  phase 8 — binder (tên/kiểu/logic 3 giá trị) + opt.go (viết lại LOGICAL) + planner.go (chọn đường VẬT LÝ)
internal/exec/  phase 8 — Volcano/iterator: scan, nested loop, Grace hash join (tràn đĩa), external merge sort
internal/engine/ phase 8 — ghép cả đường ống; EXPLAIN in CẢ HAI cây, trước và sau optimizer
diary/          nhật ký học: giả thuyết sai, số đo, invariant
skills/         quy ước làm việc trong repo (đọc trước khi ghi diary)
```

## Chạy

```
make iolab                          # phase 0: bảng số đo I/O của máy này
make iolab-full                     # + p50/p99 (repeat 5) + kiểm chứng page cache bằng mincore
make baseline DIR=/mnt/nvme/iolab   # baseline đầy đủ -> bench/baseline/<host>-<ngày>/
make pagerlab                       # phase 1: bảng commit + xxd meta page
make slotlab                        # phase 2: bản đồ page trước/sau xóa và compact
make fuzz                           # phase 2: fuzz slotted page 120s (chú ý -fuzzminimizetime)
make bufferlab                      # phase 3: bảng hit ratio + sequential flooding + bản đồ pool
make fuzz-pool                      # phase 3: fuzz chuỗi thao tác pin/unpin/flush
make btreelab                       # phase 4: thứ tự chèn -> số split, độ đầy lá, số page
make bench-btree                    # phase 4: chèn 1 triệu khóa tăng dần vs ngẫu nhiên
make crashlab                       # phase 5: 20 lần kill -9 (bản nhanh)
make crashlab-full                  # phase 5: 200 lần kill -9 — bài chốt phase
make crashlab-nowrite               # phase 5: PHẢI ĐỎ — chứng minh bài test trên biết báo SAI
make wallab                         # phase 5: một file WAL thật gồm những gì
make bench-wal                      # phase 5: giá của durability, recovery, diff, đường đọc
make fuzz-db                        # phase 5: fuzz chuỗi thao tác + crash + mở lại
make txnlab                         # phase 6: anomaly × mức isolation, chuyển tiền, phình version
make txnlab-anomaly                 # phase 6: chỉ bảng anomaly (thoát 1 nếu lệch khỏi lý thuyết)
make txnlab-contention              # phase 6: chỗ MVCC (lạc quan) THUA lock (bi quan)
make test-txn                       # phase 6: bảng anomaly khẳng định theo CẢ HAI chiều
make bench-txn                      # phase 6: giá mỗi mức isolation, giá abort, giá phình version
make fuzz-txn                       # phase 6: codec chuỗi version phải canonical + sống qua crash
make idxlab                         # phase 7: 5 bảng — hoà vốn selectivity, thuế index, ước lượng
make idxlab-breakeven               # phase 7: chỉ bảng hoà vốn, 50000 hàng (số ổn định hơn)
make idxlab-bytes                   # phase 7: hình BYTE của khóa composite — vì sao chỉ tiền tố bên trái
make test-index                     # phase 7: ba kế hoạch phải cho CÙNG kết quả
make bench-index                    # phase 7: ba hằng số của mô hình chi phí + giá index ở đường ghi
make fuzz-keys                      # phase 7: thứ tự byte phải BẰNG thứ tự logic, ở mọi kiểu/chiều sắp
make fuzz-table                     # phase 7: bất biến hàng<->index phải sống qua crash + mở lại
make repl                           # phase 8: REPL SQL. Thử: CREATE TABLE t (a INT, b TEXT, PRIMARY KEY (a));
make sqllab                         # phase 8: 6 bảng — join, hạn mức, sắp ngoài, pushdown, bỏ Sort, front-end
make sqllab-join                    # phase 8: chỉ bảng nested loop vs hash join (điểm đổi vai ở W≈3)
make sqllab-budget                  # phase 8: chỗ hash join BUỘC phải tràn ra đĩa, và cái giá
make sqllab-sort                    # phase 8: cái giá của tràn đĩa là một BẬC THANG, không phải đường dốc
make test-sql                       # phase 8: HAI ĐƯỜNG PHẢI CHO CÙNG MỘT KẾT QUẢ (-race)
make fuzz-sql                       # phase 8: parser phải KẾT THÚC, và in-lại-rồi-đọc-lại phải bền
make fuzz-btree                     # phase 4: fuzz chuỗi Put/Delete, đối chiếu map + Verify()
make test                           # toàn bộ test (40 điểm crash của pager + bất biến của page)
make check                          # fsck file data/test.db
```

Đo trên máy Linux thuần và trả dần các món nợ của phase 0:
[`docs/linux-baseline.md`](./docs/linux-baseline.md). Phase 4–9 (mọi tỉ số thời gian): [`docs/linux-phase4-9.md`](./docs/linux-phase4-9.md). Nợ của phase 9 (P9-1, và
đếm cache miss của hash join, WSL2 không có PMU): [`docs/linux-phase9.md`](./docs/linux-phase9.md).

Yêu cầu: Go 1.26+, Linux (dùng `pread`/`pwrite` và `posix_fadvise`).
