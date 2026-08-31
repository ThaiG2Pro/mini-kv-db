# minidb

Một database mini viết bằng Go để học **DB internals** — storage layer, B+Tree, WAL,
recovery, MVCC — chứ không phải để dùng thật.

- Hướng đi chung: [`ROADMAP.md`](./ROADMAP.md)
- Nhật ký từng phase: [`diary/`](./diary)
- Quy tắc ghi nhật ký: [`skills/diary-skill.md`](./skills/diary-skill.md)

## Layout

```
cmd/iolab/      phase 0 — đo đặc tính I/O của máy (fsync, group commit, random vs seq, page cache)
cmd/tornlab/    phase 0 — dò torn write (dùng cùng scripts/dm-flakey.sh)
scripts/        linux-baseline.sh (đo baseline có thể so máy), dm-flakey.sh (bơm lỗi thiết bị)
docs/           hướng dẫn đo trên máy Linux thuần
bench/baseline/ kết quả đo lưu theo máy + ngày (text + JSON)
internal/       các lớp của DB, thêm dần theo từng phase
diary/          nhật ký học: giả thuyết sai, số đo, invariant
skills/         quy ước làm việc trong repo (đọc trước khi ghi diary)
```

## Chạy

```
make iolab                          # phase 0: bảng số đo I/O của máy này
make iolab-full                     # + p50/p99 (repeat 5) + kiểm chứng page cache bằng mincore
make baseline DIR=/mnt/nvme/iolab   # baseline đầy đủ -> bench/baseline/<host>-<ngày>/
```

Đo trên máy Linux thuần và trả dần các món nợ của phase 0:
[`docs/linux-baseline.md`](./docs/linux-baseline.md).

Yêu cầu: Go 1.26+, Linux (dùng `pread`/`pwrite` và `posix_fadvise`).
