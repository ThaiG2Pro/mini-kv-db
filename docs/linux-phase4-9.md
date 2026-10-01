# Đo lại phase 4–9 trên máy Linux thuần

> Bản đồ của cả buổi đo (thứ tự, thời gian, chỗ hay vấp): [`linux-runbook.md`](./linux-runbook.md).

Mọi con số **thời gian** của phase 4–9 đều đo trên WSL2, nơi số đơn lẻ có hôm dao động ±40%. Chúng
vẫn đủ để rút ra **thứ tự lớn nhỏ**, nhưng tỉ số nào dùng để ra quyết định thiết kế thì nên kiểm
lại trên máy thật. File này liệt kê từng tỉ số đó, kèm số WSL2 để so.

Các số **đếm** thì không cần đo lại, vì chúng không phụ thuộc máy: độ đầy lá 69.48%, số split, số
lần ghi page mỗi khoá, số record redo, số lần thử lại của OCC, độ phình version 18.07x, số commit
mất khi `kill -9`, và các số khác cùng loại. Chúng chỉ đổi khi code đổi.

| Phase | Có trong | Lệnh |
|---|---|---|
| 0–3 | [`linux-baseline.md`](./linux-baseline.md) | `./scripts/linux-baseline.sh` |
| **4–8**, và hai bảng đo giờ của phase 9 | **file này** | `./scripts/linux-phase4-9.sh` |
| P9-1, P9-7 | [`linux-phase9.md`](./linux-phase9.md) | `./scripts/p91-seqscan.sh`, `./scripts/p97-hashjoin.sh` |

## Chạy  ⏱ ~45 phút cho phase 4–8; phase 9 thêm ~10 phút, cần Docker

Chuẩn bị máy như trong [`linux-baseline.md`](./linux-baseline.md): Go 1.26+, máy rảnh, laptop cắm
sạc. Rồi:

```bash
./scripts/linux-phase4-9.sh                   # phase 4 5 6 7 8
PHASES="9" ./scripts/linux-phase4-9.sh        # thêm phase 9 (Postgres/MySQL/MariaDB qua Docker)
PHASES="5 6" DIR=/mnt/nvme ./scripts/linux-phase4-9.sh   # chỉ vài phase, đo trên ổ khác
```

Kết quả nằm ở `bench/phase4-9/<host>-<ngày>/`: `env.txt` và mỗi phép đo một file `p<N>-*.txt`.

**Đừng bỏ qua `TMPDIR`.** Script đặt nó về `$DIR/tmp`. Bench của `internal/db` và `internal/txn`
ghi file vào `b.TempDir()`, còn sqllab tràn đĩa vào `os.TempDir()`. Cả hai đều theo `$TMPDIR`.
Nếu `/tmp` là tmpfs, mọi tỉ số có fsync trong đó (261x, 453x, 4046x) sụp về khoảng 1x mà không có
gì báo lỗi. `env.txt` ghi lại filesystem của `TMPDIR` để kiểm lại sau.

## Các tỉ số cần so

Lấy trung vị ns/op của các lần chạy (`-count=3`). Cột "WSL2" lấy từ bảng tỉ số trong diary của
từng phase.

### Phase 4 — B+Tree (`diary/phase4.md`)

| Tỉ số | WSL2 | Tính từ |
|---|---|---|
| quét mỗi khoá / một lần tra điểm | **~15x** (91.23 ns/key vs ~1400 ns) | `p4-scan.txt` (`Scan`) và `p4-getpool.txt` |
| tìm trong page / một lần Get | **~9x** (154 vs ~1400 ns) | `p4-scan.txt` (`SearchInPage`) |
| `GetPool512` / `GetPool2048` | 1658 / 2768 ns: pool **to hơn** lại **chậm hơn** (nợ P4-6) | `p4-getpool.txt`. Đọc kèm cỡ L2 trong `env.txt` |
| cấp phát + chép / chỉ chép, 897 byte (bài 13) | 890–1865 / 18–27 ns | `p4-copyalloc.txt` |

`p4-insert.txt` chủ yếu cho số đếm (độ đầy, split, ghi mỗi khoá). Cột ns/op của nó lệch 28–43%
giữa hai lần chạy trên WSL2, nên chỉ ghi lại để tham khảo.

### Phase 5 — WAL (`diary/phase5.md`)

| Tỉ số | WSL2 | Tính từ |
|---|---|---|
| `InsertBatch1` / `InsertBatch1000` | **261x** | `p5-insert.txt` |
| `InsertBatch1` / `InsertNoSync` | **453x**: toàn bộ giá của durability | `p5-insert.txt` |
| `GetNoWAL` / `GetWithWAL` / `GetWithWALRaw` | **1.03x** và **1.06x** | `p5-get.txt` |
| `Diff` 4KB (`DiffGran*`) | 914 ns (sau khi bỏ vòng lặp tay; bản cũ 3193) | `p5-wal.txt` |
| `Recover*` | dùng số record (đếm); ns chỉ để tham khảo | `p5-recover.txt` |

Hai tỉ số đầu phụ thuộc vào ổ đĩa (fsync), nên đọc kèm `write_cache` trong `env.txt`.

### Phase 6 — transaction (`diary/phase6.md`, `diary/phase9.md` bảng 10)

| Tỉ số | WSL2 | Tính từ |
|---|---|---|
| commit / abort (0 khoá) | **4046x** | `p6-commit.txt` (`CommitPerLevel`, `Abort/keys=0`) |
| abort (1 khoá) / abort (0 khoá) | **71x**: một xid bền ≈ 1/64 fsync | `p6-commit.txt` |
| commit: mức cô lập cao / thấp | **1.03x** | `p6-commit.txt` |
| Get: serializable / repeatable-read | **2.35x** (762 / 325 ns) | `p6-get.txt` (`GetPerLevel`) |
| chuỗi version, depth=60 / depth=1 | **2.4x** (708 / 299 ns) **sau** P6-2. Phase 6 trước khi sửa: 8.5x | `p6-get.txt` (`GetChainDepth`) |
| chuỗi version, `oldest` / `newest` ở depth=60 | **~1.0x** (714 / 708 ns) | `p6-get.txt` |
| `AcquireDisjoint` 256 / 1 holder | **22.3x** (nợ P6-3) | `p6-lock.txt` |

### Phase 7 — index và planner (`diary/phase7.md`)

| Tỉ số | WSL2 | Tính từ |
|---|---|---|
| điểm hoà vốn index scan / seq scan | **36.8%** (phase 7). Sau P9-1: 13–21%, chưa ổn định | `p7-breakeven.txt` |
| `CFetch` / `CSeq` (đo được, so với 20x đoán sẵn) | **4.5x**. Sau P9-1: 4.3–7.0x | `p7-breakeven.txt` |
| `CFetch` không tương quan / tương quan | **1.6–2.2x** | `p7-idxlab.txt` |
| index-only / index scan ở 0.1% | **3.0x** (bench), 2.96x / 5.3x (idxlab) | `p7-plansel.txt`, `p7-idxlab.txt` |
| thuế index ở đường chèn (1 / 2 / 3 index) | **1.54x / 2.72x / 5.39x** | `p7-maint.txt` |
| `LIMIT 1` / quét cả bảng | **5032x** | `p7-lookup.txt` (`ScanLimit`) và `p7-seqstep.txt` |
| `memcmp` / so theo kiểu | **5.7x** (2.47 vs 14.08 ns) | `p7-keys.txt` (`CompareVsBytes`) |

Bước quét (`SeqStep`) đã đổi ở P9-1. Muốn so phần trước và sau bản sửa đó thì dùng
`./scripts/p91-seqscan.sh`, đừng so với số trong `diary/phase7.md`.

### Phase 8 — SQL executor (`diary/phase8.md`)

| Tỉ số | WSL2 | Tính từ |
|---|---|---|
| nested loop / hash join, W=2 → 2000 | **1.25x → 121.25x**, đổi vai ở W≈3 | `p8-join.txt` |
| hash join dưới / trên ngưỡng tràn | **2.10–2.16x** | `p8-budget.txt` |
| sort, 0 → 2 → 40 → 313 run | **1.46x / 1.53x / 2.42x** | `p8-sort.txt` |
| pushdown một bảng / vào vế build / suy ra qua phép bằng | **83.86x / 45.35x / 18.53x** | `p8-pushdown.txt` |
| bỏ Sort, không `LIMIT` / có `LIMIT 10` | **2.87–3.12x / 3284–3345x** | `p8-order.txt` |
| front-end / thi hành, truy vấn điểm | **2.9x** | `p8-pipeline.txt` |
| `BenchmarkScan` pull / push | **≤1.08x** | `p8-scan.txt`, so với số trong diary |

### Phase 9 — hai bảng đo giờ trên DB thật (`diary/phase9.md`, blog bài 1 và 8)

| Tỉ số | WSL2 | Tính từ |
|---|---|---|
| hoà vốn index / seq: Postgres / MySQL / MariaDB | **4.7 / 4.9 / 7.0%** | `p9-breakeven.txt` |
| phí quét / phí tra mỗi hàng (Postgres) | 31 / 725 ns | `p9-breakeven.txt`, tính như mục "tách phí" của bảng 1 |
| commit từng dòng / theo lô (blog bài 1) | **170–310x** | `p9-commit.txt` |
| group commit: số commit chia một fsync, 64 client | **27** | `p9-commit.txt` (`wal_fsyncs`) |

Các bảng còn lại của phase 9 (anomaly, UUID, bloat, stats, crash, lograte) là số đếm, không cần đo
lại.

## Cách đọc

1. **So tỉ số, không so ns/op.** Số tuyệt đối đổi theo máy là chuyện bình thường.
2. Tỉ số lệch trong khoảng **2x** so với WSL2 thì kết luận của phase đó vẫn đứng.
3. Lệch hơn nhiều, hoặc **đổi dấu** (ví dụ pool to hơn mà vẫn chậm hơn, hay ngược lại) thì thêm một
   dòng vào bảng **giả thuyết sai** trong diary của phase đó, và xem lại quyết định thiết kế nào
   dựa trên tỉ số này.
4. Tỉ số nào có fsync thì đọc kèm `write_cache` và filesystem của `TMPDIR` trong `env.txt`.

## Sau khi đo xong

0. Vẽ biểu đồ cho blog: `uv run charts/plot.py` (xem [`charts/README.md`](../charts/README.md)).
1. Thêm vào diary của mỗi phase một mục "đo lại trên Linux thuần": dán `env.txt` và bảng tỉ số mới
   đặt cạnh số WSL2. Giữ cả hai để so.
2. Ghi rõ lệnh, ngày, commit (có sẵn trong `env.txt`) và máy.
3. Quy tắc ghi đầy đủ: [`skills/diary/SKILL.md`](../skills/diary/SKILL.md).
