# minidb

Một database mini viết bằng Go để học **DB internals** — storage layer, B+Tree, WAL,
recovery, MVCC — chứ không phải để dùng thật.

- Hướng đi chung: [`ROADMAP.md`](./ROADMAP.md)
- Nhật ký từng phase: [`diary/`](./diary)
- Quy tắc ghi nhật ký: [`skills/diary/SKILL.md`](./skills/diary/SKILL.md)
- Sổ nợ kỹ thuật: [`docs/debts.md`](./docs/debts.md)

## Layout

```
cmd/iolab/      phase 0 — đo đặc tính I/O của máy (fsync, group commit, random vs seq, page cache)
cmd/tornlab/    phase 0 — dò torn write (dùng cùng scripts/dm-flakey.sh)
cmd/pagerlab/   phase 1 — soi file: meta page luân phiên, freelist, rollback bằng 1 byte
cmd/slotlab/    phase 2 — soi một slotted page: bản đồ page, phân mảnh, compact, churn
cmd/bufferlab/  phase 3 — hit ratio lru/clock/lru-2 vs Belady, sequential flooding
cmd/dbcheck/    fsck cho file minidb — meta, freelist, double free, page mồ côi
scripts/        linux-baseline.sh (đo baseline có thể so máy), dm-flakey.sh (bơm lỗi thiết bị)
docs/           debts.md (sổ nợ + lệnh trả từng món), linux-baseline.md (đo trên Linux thuần)
bench/baseline/ kết quả đo lưu theo máy + ngày (text + JSON)
internal/pager/ phase 1 — file = mảng page 4KB, meta page kép + crc32c, freelist
internal/page/  phase 2 — slotted page: record biến độ dài, slot indirection, compact
internal/bufpool/ phase 3 — buffer pool: pin/unpin, dirty, LRU/CLOCK/LRU-K, WAL hook
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
make test                           # toàn bộ test (40 điểm crash của pager + bất biến của page)
make check                          # fsck file data/test.db
```

Đo trên máy Linux thuần và trả dần các món nợ của phase 0:
[`docs/linux-baseline.md`](./docs/linux-baseline.md).

Yêu cầu: Go 1.26+, Linux (dùng `pread`/`pwrite` và `posix_fadvise`).
