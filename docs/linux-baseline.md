# Trả nợ Phase 0–3 trên máy Linux thuần

> Bản đồ của cả buổi đo (thứ tự, thời gian, chỗ hay vấp): [`linux-runbook.md`](./linux-runbook.md).

Bản đo đầu tiên chạy trên WSL2 (ext4 trên đĩa ảo trên NTFS). Nó đủ để rút ra **tỉ số**,
nhưng để lại 5 món nợ ở phase 0 và 3 món "chạy lại bench" ở phase 1–3. File này là **hướng dẫn
trả từng món**, mỗi món một lệnh cụ thể.

Trả dần cũng được — thứ tự khuyến nghị: **#3 → #5 → #2 → #4 → P1-6/P2-5/P3-5 → #1** (rẻ trước,
đắt sau). `linux-baseline.sh` ở nợ #4 chạy luôn cả ba món của phase 1–3.

**Các món 📏 cần Linux thuần, và ở đâu:**

| Món | Hướng dẫn | Lệnh |
|---|---|---|
| P0-1…P0-5 | file này | `./scripts/linux-baseline.sh`, `./scripts/dm-flakey.sh` |
| P1-6, P2-5, P3-5 | file này, mục cuối | `./scripts/linux-baseline.sh` (chạy chung) |
| mọi tỉ số **thời gian** của phase 4–8, và bảng 1 + group commit của phase 9 | [`linux-phase4-9.md`](./linux-phase4-9.md) | `./scripts/linux-phase4-9.sh` |
| P9-1 | [`linux-phase9.md`](./linux-phase9.md) | `./scripts/p91-seqscan.sh` |
| P9-7 | [`linux-phase9.md`](./linux-phase9.md) | `./scripts/p97-hashjoin.sh` |

Các món 📏 khác trong [`debts.md`](./debts.md) (P3-1, P4-3, P4-6, P4-7, P5-6, P6-6, P7-4, P7-5,
P7-7, P9-3) **không** cần đổi máy: chúng cần viết thêm code hoặc chạy lâu hơn, và
WSL2 làm được.

## Chuẩn bị (5 phút)

```bash
git clone https://github.com/ThaiG2Pro/mini-kv-db.git && cd mini-kv-db
go version                       # cần 1.26+
sudo apt install -y jq dmsetup   # jq để so JSON, dmsetup cho nợ #1
```

Quan trọng: **đo trên đúng ổ mà sau này minidb sẽ chạy**, và **đo lúc máy rảnh**.
Trỏ thư mục đo bằng biến `DIR`:

```bash
DIR=/mnt/nvme/iolab ./scripts/linux-baseline.sh
```

---

## Nợ #3 — p50/p99 thay vì chỉ trung bình  ⏱ 5 phút

**Vì sao:** với commit latency, **p99 mới là con số người dùng cảm nhận**. Trung bình 1.4ms
mà p99 40ms là một hệ thống hoàn toàn khác.

```bash
go run ./cmd/iolab -filemb 512 -repeat 5
```

Nhìn cột `p99`. Hai thứ cần rút ra:

- **`p99 / p50` của `write + fsync`** — độ nhiễu của ổ. > 5x nghĩa là số trung bình không đáng tin.
- **cột `p99` theo cỡ nhóm group commit** — nếu p99 **không** phình theo cỡ nhóm thì gộp
  commit gần như miễn phí, cứ gộp mạnh tay ở phase 5. Nếu p99 phình tuyến tính thì phải
  giới hạn cỡ nhóm.

Ghi vào diary: bảng có cột p50/p99, kèm tỉ số `p99/p50`.

## Nợ #5 — chứng minh `fadvise(DONTNEED)` thật sự có tác dụng  ⏱ 2 phút

**Vì sao:** kernel có quyền **im lặng bỏ qua** `DONTNEED` (page còn dirty, còn được tham chiếu).
Nếu nó bỏ qua thì số "cache lạnh" là giả.

```bash
go run ./cmd/iolab -filemb 512 -verify-cache
```

Cờ này dùng `mincore(2)` để đếm **% số page của file đang nằm trong page cache**, in ra
trước/sau khi drop:

```
[cache] residency 100.0% -> 0.0%  (OK — cache đã bị đẩy ra)
```

Nếu số sau > 5% → công cụ tự cảnh báo, và mọi số "cache lạnh" của lần chạy đó phải vứt.

## Nợ #2 — `O_DIRECT`: đo I/O thật, không qua page cache  ⏱ 10 phút

**Vì sao:** mọi số đo mặc định đều đi qua page cache của kernel. DB thật (InnoDB, Oracle)
thường bật `O_DIRECT` để **tự quản buffer pool**, tránh cache hai lần. Không có số `O_DIRECT`
thì đến phase 3 bạn sẽ bench buffer pool tự viết trong khi kernel vẫn đang cache bên dưới —
so sánh không công bằng.

```bash
go run ./cmd/iolab -filemb 512 -direct -repeat 3
```

Điều cần quan sát:

- `pread ngẫu nhiên` với `O_DIRECT` = **độ trễ thật của thiết bị**. So nó với số "cache lạnh"
  ở chế độ thường: chênh lệch chính là phần readahead + cache của kernel vẫn âm thầm giúp bạn.
- Ở chế độ `O_DIRECT`, cột "CACHE NÓNG" **phải** ngang với "cache lạnh". Nếu nó vẫn nhanh
  bất thường thì `O_DIRECT` đã không có hiệu lực (một số filesystem lặng lẽ lờ đi).
- `write` không fsync với `O_DIRECT` **vẫn chưa durable** — dữ liệu có thể còn trong
  **write cache của chính ổ đĩa**. Kiểm tra ổ có bật write cache không:

```bash
cat /sys/block/nvme0n1/queue/write_cache      # "write back" = ổ đang có cache riêng
```

Nếu là `write back` thì fsync còn phải ép ổ flush cache của nó — đó là lý do fsync đắt.

## Nợ #4 — baseline sạch, so được giữa các máy  ⏱ 15 phút

```bash
DIR=/mnt/nvme/iolab REPEAT=5 ./scripts/linux-baseline.sh
```

Script làm 3 việc:

1. Chụp **môi trường** vào `env.txt`: kernel, CPU, RAM, filesystem + mount options,
   `rota` (ổ quay hay SSD), **write_cache của ổ**, I/O scheduler.
2. Chạy bộ đo buffered (`-repeat 5 -verify-cache`) và bộ `O_DIRECT`.
3. Lưu cả text lẫn JSON vào `bench/baseline/<host>-<ngày>/`.

So hai máy:

```bash
diff <(jq -r '.results[]|"\(.name) \(.p50_ns)"' bench/baseline/wsl-.../buffered.json) \
     <(jq -r '.results[]|"\(.name) \(.p50_ns)"' bench/baseline/linux-.../buffered.json)
```

Điều cần rút ra: **tỉ số nào giữ nguyên qua hai máy thì đó là quy luật vật lý** (đáng tin để
thiết kế). Tỉ số nào đổi thì đó là đặc tính của máy, không phải của database.

## Nợ #1 — torn write thật  ⏱ 30 phút, cần root

**Vì sao:** cả quyết định "page 4KB + checksum" đang dựa trên lý thuyết. Đây là món duy nhất
cần đụng tới tầng thiết bị.

**Điều phải hiểu trước:** `kill -9` **không bao giờ** tạo ra torn write. Tiến trình chết nhưng
page cache vẫn thuộc kernel và kernel vẫn writeback đầy đủ. Muốn mô phỏng mất điện phải cắt ở
tầng thấp hơn tiến trình — đó là việc của `dm-flakey`.

```bash
sudo ./scripts/dm-flakey.sh up          # loop device + dm-flakey + ext4, mount /mnt/flakey

go run ./cmd/tornlab -mode write -file /mnt/flakey/torn.dat -pagesize 16384 -pages 4096 &
sleep 5
sudo ./scripts/dm-flakey.sh drop        # từ giây này write bị NUỐT im lặng = mất điện
kill %1

sudo ./scripts/dm-flakey.sh heal        # thiết bị lành, fsck, remount
go run ./cmd/tornlab -mode verify -file /mnt/flakey/torn.dat -pagesize 16384

sudo ./scripts/dm-flakey.sh down        # dọn
```

`tornlab` ghi mỗi page **toàn bộ bằng một "thế hệ"** (mọi byte thân page = `byte(gen)`, kèm CRC),
nên một page hợp lệ luôn đồng nhất. Nếu verify tìm thấy page pha trộn **hai thế hệ** → đó chính
là torn write, bằng chứng thực nghiệm rằng page lớn hơn sector **không** atomic.

Lặp lại với `-pagesize 4096`, `16384`, `65536`. Câu hỏi cần trả lời:
**kích thước page nào bắt đầu xuất hiện torn write trên chính ổ này?**

Ba kết cục đều là dữ liệu đáng ghi:

| Kết quả verify | Nghĩa là |
|---|---|
| có `torn` | đã tái tạo được. Ghi rõ pagesize nào bắt đầu hỏng. |
| chỉ có `crc` sai | không pha trộn thế hệ nhưng dữ liệu vẫn hỏng — checksum vẫn là thứ bắt được |
| toàn `ok` | **chưa** chứng minh được là không thể xảy ra, chỉ là cửa sổ lỗi chưa trúng. Thử page lớn hơn, bỏ `-sync` |

Cũng để ý `fsck` trong bước `heal`: nếu **chính filesystem** hỏng sau khi nuốt write, đó là lời
nhắc rằng DB không được phó thác durability cho filesystem.

## Nợ P1-6, P2-5, P3-5 — tỉ số của phase 1–3 có sống sót không?  ⏱ 10 phút

**Vì sao:** các tỉ số dưới đây là nền cho thiết kế của các phase sau. Chúng đo trên WSL2, nơi
`VerifyRef` dao động tới 43% giữa hai lần chạy. Tỉ số nào sống sót qua máy khác thì mới dựa vào
được.

`linux-baseline.sh` đã chạy cả ba (bước 4). Muốn chạy riêng:

```bash
mkdir -p $DIR/tmp
TMPDIR=$DIR/tmp go test ./internal/pager   -run XXX  -bench . -benchtime=200x -count=5   # P1-6
TMPDIR=$DIR/tmp go test ./internal/page    -run XXX  -bench . -benchmem -count=5         # P2-5
TMPDIR=$DIR/tmp go test ./internal/bufpool -run '^$' -bench . -benchmem -count=5 -cpu 1,6  # P3-5
```

**Đừng bỏ `TMPDIR`.** Bench của pager ghi file vào `b.TempDir()`, mặc định là `/tmp`. Trên
Fedora, Arch và nhiều bản khác, `/tmp` là tmpfs: fsync ở đó không tốn gì, và tỉ số 481x sụp về
khoảng 1x mà không có gì báo lỗi. `env.txt` có ghi `findmnt -T /tmp` để kiểm lại sau.

**Tính lại các tỉ số** (lấy trung vị ns/op của 5 lần chạy):

| Nợ | Tỉ số | Trên WSL2 | Tính từ |
|---|---|---|---|
| P1-6 | `Commit` / `CommitNoSync` | **481x** (1373722 / 2852) | `p1-pager.txt` |
| P1-6 | `Commit` / mỗi page của `CommitBatch64` | **43x** (1373722 / 32005) | `p1-pager.txt` |
| P2-5 | `VerifyRefFullPage` / `VerifyFullPage` | **5.5x** (19768 / 3583) | `p2-page.txt` |
| P2-5 | `CompactScrambled` insertion sort / `slices.SortFunc` | **25x** (229810 / 9169) | `p2-page.txt`. Bản cũ không còn trong code: so ns/op mới với 9169 |
| P3-5 | `pread` cache lạnh (phase 0) / hit của pool | **1366x** (69µs / ~50ns) | `buffered.txt` + `p3-bufpool.txt` |
| P3-5 | `PinHitParallel` 6 luồng / 1 luồng | **2.3x** | `p3-bufpool.txt`, `-cpu 1,6` |

**Cách đọc:** số tuyệt đối (ns/op) đổi theo máy là chuyện bình thường. Điều cần xem là **tỉ số**.
Lệch trong khoảng 2x thì kết luận của phase đó còn đứng. Lệch hơn nhiều thì ghi một dòng
"giả thuyết sai" vào diary của phase đó, và xem lại quyết định thiết kế nào dựa trên tỉ số này.
Riêng 481x phụ thuộc vào ổ đĩa (fsync), nên đọc kèm `write_cache` trong `env.txt`.

Hit ratio và các con số đếm (số page, số lần đọc) thì **không cần** đo lại: chúng không có đơn
vị thời gian, và không phụ thuộc máy.

---

## Sau khi đo xong

0. Vẽ biểu đồ cho blog: `uv run charts/plot.py` (xem [`charts/README.md`](../charts/README.md)).
1. Dán `env.txt` vào mục **Môi trường** của `diary/phase0.md` (thay cho phần WSL2, hoặc thêm
   một mục "đo lại trên Linux thuần" — giữ cả hai để so). Tỉ số của phase 1–3 thì dán vào
   `diary/phase1.md`, `phase2.md`, `phase3.md`, kèm một dòng trỏ về `env.txt`.
2. Cập nhật bảng **tỉ số**, ghi rõ lệnh + ngày + commit + máy.
3. Mỗi món nợ trả xong thì **tick checkbox** ở mục "Nợ kỹ thuật", và ghi một dòng vào bảng
   **giả thuyết sai** nếu số thật khác dự đoán — đó mới là phần đáng giá.
4. Quy tắc ghi đầy đủ: [`skills/diary/SKILL.md`](../skills/diary/SKILL.md).
