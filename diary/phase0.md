# Phase 0 — Nền tảng vật lý

- **Thời lượng dự kiến:** 0.5 ngày
- **Bắt đầu:** 2026-08-31 · **Kết thúc:** 2026-08-31
- **Trạng thái:** ✅ xong
- **Commit:** _(chưa commit — repo mới `git init`)_

> **Quy tắc ghi nhật ký:** mọi con số, mọi kết luận đều phải kèm **lệnh shell sinh ra nó**
> và **output thật** (dán nguyên, không tóm tắt). Sáu tháng sau đọc lại phải chạy lại được.
> Ghi trong lúc làm, không phải sau khi xong.

## Môi trường

```console
$ uname -srmo && go version && df -hT . | tail -1 && nproc && free -g | sed -n 2p
Linux 6.6.87.2-microsoft-standard-WSL2 x86_64 GNU/Linux
go version go1.26.2 linux/amd64
/dev/sdd       ext4 1007G   38G  918G   4% /
6
Mem:              11           5           1           0           4           5
```

⚠️ **WSL2**: ext4 trên đĩa ảo, nằm trên NTFS của host. fsync ở đây **không** phải fsync trên
máy Linux thật. Mọi số tuyệt đối bên dưới chỉ có giá trị so sánh **tương đối với nhau**.

## Mục tiêu phase

Hiểu 4 sự thật vật lý quyết định mọi thiết kế DB: fsync, torn write, random vs sequential I/O,
latch vs lock. Chưa code DB.

## Câu hỏi phải trả lời được khi xong

- `write()` đã xong nghĩa là dữ liệu ở đâu? Vì sao chưa an toàn?
- Vì sao phải fsync cả directory entry khi tạo file mới?
- Torn write là gì, vì sao page 4KB an toàn hơn 16KB?
- Random 4KB chậm hơn sequential bao nhiêu lần trên máy tôi?
- Latch khác Lock ở điểm nào (thời gian sống, thứ được bảo vệ, ai quản lý)?

## Deliverable (bằng chứng đã hiểu)

`cmd/iolab/main.go` — đo 3 nhóm hiện tượng trên chính máy này.

## Reproduce toàn bộ phase này

```bash
make iolab                          # = go run ./cmd/iolab -filemb 512, ~40s
go run ./cmd/iolab -filemb 128      # bản nhanh, nhưng file nhỏ dễ vừa RAM -> số đọc lạc quan giả
```

---

## Nhật ký

### 2026-08-31 — dựng repo

```console
$ go mod init minidb
go: creating new go.mod: module minidb

$ mkdir -p cmd/iolab internal bench
$ ls -1
Makefile  README.md  ROADMAP.md  bench  cmd  diary  go.mod  internal
```

Đã tạo `.gitignore` (bỏ `/data/`, `*.db`, `*.wal`), `Makefile` (`make iolab` / `make` = fmt+vet+test),
`README.md`. Sau đó `git init` (chưa commit).

### 2026-08-31 — viết `cmd/iolab`, chạy lần 1 → **số đo vô lý**

```console
$ go vet ./...
$ go run ./cmd/iolab -filemb 512
...
=== 3+4. Random vs tuần tự, cache lạnh vs nóng ===
pread 4KB tuần tự, CACHE LẠNH              451220 ops/s       2µs
pread 4KB ngẫu nhiên, CACHE LẠNH            17405 ops/s      57µs
pread 4KB ngẫu nhiên, CACHE NÓNG            19812 ops/s      50µs   <-- ???
```

**Đọc kết quả:** "cache nóng" (50µs) ≈ "cache lạnh" (57µs), tỉ số 1.1x trong khi lý thuyết
phải cỡ 100x. Số đo sai, không phải máy lạ.

**Đang nghĩ gì:** file 512MB = 131 072 page. Lần đọc "lạnh" chỉ chạm 4 000 page ngẫu nhiên
= 3% file. Lần đọc "nóng" tôi lại **bốc offset ngẫu nhiên MỚI** → xác suất trúng page đã cache
cũng chỉ ~3%. Nghĩa là lần "nóng" vẫn gần như miss hoàn toàn. Working set quá thưa.

**Sửa:** lưu lại đúng dãy offset của lần lạnh, lần nóng đọc lại **chính dãy đó**.

```go
// cmd/iolab/main.go
offsets := make([]int64, n)
for i := range offsets { offsets[i] = rng.Int63n(pages) * pageSize }
dropCache(f, size)
d = timeIt(n, func(i int) { f.ReadAt(buf, offsets[i]) })   // LẠNH
d = timeIt(n, func(i int) { f.ReadAt(buf, offsets[i]) })   // NÓNG, cùng offsets
```

### 2026-08-31 — bug thứ hai: random write trông như bằng sequential write

Cũng trong lần chạy 1:

```console
pwrite 4KB tuần tự                        1118608 ops/s       1µs
pwrite 4KB ngẫu nhiên                      815534 ops/s       1µs   <-- chỉ chậm 1.4x?
```

**Đọc kết quả:** cả hai chỉ **làm bẩn page cache**, chưa hề chạm đĩa. `fsync` nằm ngoài vòng đo
nên toàn bộ chi phí thật bị giấu đi.

**Sửa:** tách riêng đồng hồ cho `fsync` sau mỗi kiểu ghi:

```go
d = timeIt(n, func(i int) { f.WriteAt(buf, seqOffset(i)) })
out = append(out, result{"pwrite 4KB tuần tự (chưa fsync)", n, d, ...})
t0 := time.Now(); must(f.Sync())
out = append(out, result{"  -> fsync sau ghi tuần tự", n, time.Since(t0), ...})
```

### 2026-08-31 — chạy lại sau khi sửa

```console
$ gofmt -w cmd/iolab/main.go && go vet ./... && go run ./cmd/iolab -filemb 512
iolab — Phase 0
thư mục: ./data/iolab
page size: 4096 B
file thử: 512 MB

=== 1. write() KHÔNG phải durability ===
phép đo                                     ops/s       mỗi op         MB/s  ghi chú
write 4KB (không fsync)                    533471          2µs       2185.1  chỉ tới page cache — CHƯA durable
write 4KB + fsync mỗi lần                     690       1.45ms          2.8  = trần commit/s của 1 luồng
fsync khi không có gì bẩn                    6774        148µs         27.7  overhead nền của syscall
tạo file + fsync file + fsync dir             425      2.355ms          1.7  giá tạo 1 WAL segment mới

=== 2. Group commit: chia nhau một lần fsync ===
group commit, 1 txn / 1 fsync                 571      1.751ms          2.3  txn/s hiệu dụng
group commit, 4 txn / 1 fsync                1351        740µs          5.5  txn/s hiệu dụng
group commit, 16 txn / 1 fsync               5460        183µs         22.4  txn/s hiệu dụng
group commit, 64 txn / 1 fsync               9808        102µs         40.2  txn/s hiệu dụng
group commit, 256 txn / 1 fsync             57288         17µs        234.7  txn/s hiệu dụng

=== 3+4. Random vs tuần tự, cache lạnh vs nóng ===
pwrite 4KB tuần tự (chưa fsync)           1059590          1µs       4340.1  chỉ làm bẩn page cache
  -> fsync sau ghi tuần tự                 781443          1µs       3200.8  writeback các page liền kề
pwrite 4KB ngẫu nhiên (chưa fsync)         638359          2µs       2614.7  cùng số byte, vị trí rải rác
  -> fsync sau ghi ngẫu nhiên              258439          4µs       1058.6  <- giá thật của random write
pread 4KB tuần tự, CACHE LẠNH              340959          3µs       1396.6  kernel readahead giúp sức
pread 4KB ngẫu nhiên, CACHE LẠNH            14521         69µs         59.5  <- CHI PHÍ 1 LẦN CHẠM NODE B+TREE
pread 4KB ngẫu nhiên, CACHE NÓNG           891515          1µs       3651.6  <- cùng offset, giá trị của buffer pool
```

**Đọc kết quả:** cả 4 tỉ số giờ đã khớp lý thuyết. Nóng/lạnh = 891515/14521 ≈ **61x**.

**Đang nghĩ gì:** chạy 3 lần thì `write+fsync` ra 1.33ms / 1.45ms / 3.56ms — dao động **2.7x**
giữa các lần chạy. Kết luận: ghi lại **tỉ số**, đừng thờ **số tuyệt đối**.

### 2026-08-31 — dựng công cụ trả nợ, trả luôn #3 và #5

Sắp đổi sang máy Linux thuần để đo lại, nên làm công cụ trước để mỗi món nợ có **một lệnh**
trả nợ thay vì một lời nhắc suông.

**Nợ #3 (p50/p99) — TRẢ XONG.** Thêm cờ `-repeat N`, và đo **từng op riêng lẻ** thay vì chỉ đo
tổng, để có phân phối:

```console
$ go run ./cmd/iolab -filemb 128 -ops 1500 -verify-cache
=== 1. write() KHÔNG phải durability ===
phép đo                                    ops/s trung bình       p50       p99     MB/s
write 4KB + fsync mỗi lần                    858   1.166ms   1.067ms   2.382ms      3.5

=== 2. Group commit ===
group commit, 1 txn / 1 fsync                831   1.203ms   1.135ms   2.817ms      3.4
group commit, 16 txn / 1 fsync             14765   1.083ms   1.066ms   1.211ms     60.5
group commit, 256 txn / 1 fsync           116741   2.188ms   2.229ms   2.248ms    478.2
```

**Đọc kết quả — phát hiện quan trọng, không thấy được nếu chỉ nhìn trung bình:**
gộp 256 txn làm throughput gấp **140x** (831 → 116741) mà **p99 chỉ tăng 1.8x** (2.82ms → 2.25ms —
thực ra còn *giảm*). Nghĩa là **group commit gần như miễn phí về độ trễ**: txn không hề chờ lâu
hơn đáng kể, chỉ là nhiều txn cùng chia một lần fsync. Ở phase 5 cứ gộp mạnh tay.
Ngược lại, p99/p50 của `write+fsync` = 2.23x → ổ này nhiễu vừa phải.

**Nợ #5 (fadvise có thật sự tác dụng?) — TRẢ XONG.** Thêm `-verify-cache`, dùng `mincore(2)`
đếm **% page của file đang nằm trong page cache**, in trước/sau khi drop:

```console
  [cache] residency 100.0% -> 0.0%  (OK — cache đã bị đẩy ra)
  [cache] residency 4.7% -> 0.0%  (OK — cache đã bị đẩy ra)
```

Trước đây tôi chỉ *suy luận gián tiếp* rằng cache đã trống vì đọc chậm đi. Giờ có bằng chứng
trực tiếp: 100% → 0%. (Con số 4.7% ở lần thứ hai là hợp lý: lần đọc tuần tự trước đó chỉ nạp
1500/32768 page = 4.6%.) Công cụ tự cảnh báo nếu residency sau khi drop còn > 5%.

**Nợ #2 (O_DIRECT) — công cụ xong, chờ đo trên Linux thuần.** Thêm `-direct`, kèm buffer
aligned 4KB (Go không cấp phát aligned sẵn, phải cắt từ slice lớn hơn):

```console
$ go run ./cmd/iolab -filemb 64 -ops 500 -direct
pread 4KB ngẫu nhiên, CACHE LẠNH           22544    44.3µs      39µs      89µs
pread 4KB ngẫu nhiên, CACHE NÓNG           21809    45.7µs    39.1µs   128.2µs  (O_DIRECT: không có cache nên KHÔNG nóng)
```

**Đọc kết quả:** đúng như kỳ vọng — với `O_DIRECT` thì "nóng" ≈ "lạnh" (45.7µs vs 44.3µs),
xác nhận page cache đã bị bỏ qua thật. Đây cũng là **cách tự kiểm tra `O_DIRECT` có hiệu lực**:
nếu cột "nóng" vẫn nhanh bất thường thì filesystem đã lặng lẽ lờ `O_DIRECT` đi.

**Nợ #1 (torn write) — công cụ xong, cần root + máy thí nghiệm.** Viết `cmd/tornlab`:
mỗi page ghi **toàn bộ bằng một "thế hệ"** (mọi byte thân page = `byte(gen)` + CRC), nên page
hợp lệ luôn đồng nhất; page pha trộn hai thế hệ = torn write.

```console
$ go run ./cmd/tornlab -mode write -file ./data/torn.dat -pagesize 16384 -pages 256 -seconds 2
hết giờ, đã ghi 364187 page
$ go run ./cmd/tornlab -mode verify -file ./data/torn.dat -pagesize 16384
  ok        256
=> Không phát hiện torn write LẦN NÀY. Chưa chứng minh được là không thể xảy ra —
   chỉ có nghĩa là cửa sổ lỗi chưa trúng.
```

**Đang nghĩ gì:** đây mới chỉ là kiểm tra công cụ chạy đúng, chưa phải thí nghiệm.
Điều học được khi viết nó: **`kill -9` KHÔNG BAO GIỜ tạo ra torn write** — tiến trình chết
nhưng page cache vẫn thuộc kernel và kernel vẫn writeback đầy đủ. Muốn mô phỏng mất điện phải
cắt ở **tầng thiết bị**: `dm-flakey` với `drop_writes` (nhận write, báo thành công, không ghi gì
— lừa được cả fsync). Đã viết `scripts/dm-flakey.sh` cho việc đó.

**Còn dựng thêm:** `scripts/linux-baseline.sh` (chụp môi trường gồm cả `write_cache` của ổ và
I/O scheduler, chạy cả hai bộ buffered/direct, lưu text + JSON vào `bench/baseline/<host>-<ngày>/`),
cờ `-json` để so giữa các máy bằng `jq`, và `docs/linux-baseline.md` — hướng dẫn trả từng món nợ.

---

## Giả thuyết sai / bug đã gặp

| Tôi tưởng là | Thực tế là | Lệnh / output đã lật tẩy nó | Đã sửa thế nào |
|---|---|---|---|
| Đọc ngẫu nhiên lần 2 trên cùng file = cache nóng | Working set 4 000 page / 131 072 page = 3% → lần "nóng" vẫn miss | `go run ./cmd/iolab -filemb 512` → nóng 50µs ≈ lạnh 57µs (tỉ số 1.1x, phải là ~100x) | Lưu dãy `offsets` của lần lạnh, lần nóng đọc lại đúng dãy đó → 1µs (61x) |
| `pwrite` random chậm hơn `pwrite` sequential | Per-op bằng nhau vì cả hai chỉ làm bẩn page cache; chênh lệch nằm ở **writeback lúc fsync** | Cùng output trên: random 1µs ≈ seq 1µs | Tách đồng hồ riêng cho `f.Sync()` → fsync sau ghi random đắt **~4x** |
| Số tuyệt đối là đáng tin | 3 lần chạy: fsync = 1.33ms / 1.45ms / 3.56ms | Chạy `go run ./cmd/iolab` 3 lần liên tiếp | Trong nhật ký chỉ chốt **tỉ số**; ghi rõ đây là WSL2 |
| `fadvise(DONTNEED)` chắc chắn có tác dụng | **Đúng, và đã chứng minh được** — nhưng chỉ khi Sync() trước, vì kernel im lặng bỏ qua page còn dirty | `go run ./cmd/iolab -verify-cache` → `[cache] residency 100.0% -> 0.0%` (dùng `mincore(2)`) | `dropCache()` gọi `f.Sync()` **trước** `fadvise`; thêm `-verify-cache` để không bao giờ phải đoán lại |

## Số đo

**Lệnh:** `go run ./cmd/iolab -filemb 512` · **Ngày:** 2026-08-31 · **Máy:** WSL2/ext4-trên-VHD,
Go 1.26.2, 6 core, 11GB RAM · **Output nguyên văn:** xem mục nhật ký ngay trên.

Tỉ số cần nhớ (tỉ số bền hơn số tuyệt đối):

| Tỉ số | Giá trị | Ý nghĩa |
|---|---|---|
| write / (write+fsync) | **773x** (533471 / 690) | cái giá của chữ **D** trong ACID |
| group 256 / group 1 | **100x** (57288 / 571) | đòn bẩy của group commit → phase 5 |
| pread random / sequential, cache lạnh | **23x** (69µs / 3µs) | vì sao B+Tree phải fanout lớn để cây thấp |
| pread cache lạnh / cache nóng | **61x** (69µs / 1µs) | vì sao buffer pool tồn tại → phase 3 |
| fsync sau ghi random / sau ghi seq | **4x** | write amplification của insert khoá ngẫu nhiên → phase 4 |

Trần commit một luồng trên máy này: **~690 txn/s**. Giá tạo một file mới (fsync file + fsync dir):
**~2.4ms**.

## Invariant tôi đã cài và lệnh kiểm chứng nó

Phase này chưa có invariant của DB, nhưng đã chốt **ràng buộc thiết kế** cho các phase sau:

| Ràng buộc chốt cho minidb | Vì số đo nào | Sẽ kiểm chứng ở |
|---|---|---|
| Commit = fsync WAL tới commit record (không tính `write()` là durable) | write/fsync = 773x | phase 5, `cmd/crashtest` |
| Phải có group commit, không phải tối ưu vặt | 1 txn/fsync = 571/s → 256 txn/fsync = 57288/s | phase 5, `BenchmarkGroupCommit` |
| WAL segment **cấp phát trước và tái sử dụng**, không tạo file mới trong hot path | tạo file + 2 fsync = 2.4ms ≈ 1.6 lần commit | phase 5 |
| Page = 4KB, mỗi page mang checksum | sector atomic 512B/4KB → page lớn dễ torn write | phase 1, test crash meta page |
| Fanout phải lớn để cây thấp | random cold read 69µs/lần chạm | phase 4, `TestTreeInvariants` |

## Đọc gì

- CMU 15-445 lecture 03 (Database Storage I) — vì sao DB không phó thác cho `mmap`/page cache
  của OS mà tự quản buffer pool.
- Database Internals ch.1 & ch.3 — durability, file layout.
- `man 2 fsync`, `man 2 posix_fadvise`, `man 2 pwrite`.

## Rút ra (viết như thể giải thích cho người khác)

1. **`write()` là lời hứa, `fsync()` mới là chữ ký** — chênh nhau 773x. Toàn bộ ngành thiết kế DB
   là nghệ thuật fsync **ít lần nhất có thể** mà vẫn không nói dối về durability. WAL chính là
   câu trả lời: thay vì fsync các data page rải rác khắp file, chỉ fsync **một** file log ghi tuần tự.
2. **Group commit là kiến trúc, không phải tối ưu vặt.** 256 txn chia nhau một fsync → throughput
   gấp 100 lần. fsync có chi phí gần như cố định bất kể 4KB hay 1MB, nên độ trễ mỗi txn gần như
   không tăng mà thông lượng tăng tuyến tính. Đây là lý do Postgres có `commit_delay`.
3. **Random I/O đắt vì SỐ LẦN round-trip, không phải vì số byte.** 4000 lần đọc 4KB: ngẫu nhiên
   69µs/lần vs tuần tự 3µs/lần — cùng 16MB. Luật thiết kế rút ra: **giảm số lần chạm đĩa**.
   B+Tree tồn tại đúng vì thế: fanout = 4096/(16+8) ≈ 170 → cây 3 tầng chứa ~4.9 triệu key,
   4 tầng ~835 triệu. Lookup = 3-4 lần chạm ≈ 210-280µs khi cache lạnh.
4. **Buffer pool biến 69µs thành 1µs.** Và vì các node **trên** của B+Tree bị chạm ở mọi lookup
   nên chúng gần như luôn nằm trong cache — thực tế chỉ **leaf** mới miss. Đó là lý do một lookup
   thật chỉ tốn ~1 lần I/O chứ không phải 3-4. Cũng là lý do eviction policy đáng nghĩ kỹ:
   một lần seq scan có thể đẩy hết internal node ra khỏi cache (phase 3 sẽ đo).
5. **fsync sau ghi random đắt gấp 4 lần fsync sau ghi tuần tự.** Đây chính là write amplification —
   và là lý do khoá chính ngẫu nhiên (UUIDv4) phá hoại B+Tree: page split rải khắp file, mỗi
   checkpoint phải writeback dirty page rải rác. Phase 4 sẽ đo trực tiếp bằng số page split.
6. **Latch vs Lock** (chưa đo được, mới là khái niệm — kiểm chứng ở phase 3 và 6):
   *latch* bảo vệ **cấu trúc dữ liệu trong RAM** (một page, bảng hash của buffer pool), sống vài
   chục ns, do chính code cấu trúc dữ liệu quản, **không** có deadlock detection → phải tránh
   deadlock bằng thứ tự lấy latch. *Lock* bảo vệ **dữ liệu logic** (một hàng, một khoảng key),
   giữ tới cuối transaction (ms→s), do lock manager quản, **có** deadlock detection, và là thứ
   quyết định isolation level.
7. **Bài học về phương pháp, ngang tầm quan trọng với bài học kỹ thuật:** hai lần đầu đo đều
   sai vì **cái tôi tưởng đang đo không phải cái máy thật sự làm** (đo memcpy tưởng là đo đĩa;
   đo làm-bẩn-cache tưởng là đo ghi đĩa). Cách phát hiện: **so số đo với tỉ số lý thuyết kỳ vọng
   trước khi tin nó.** Tỉ số 1.1x ở chỗ đáng lẽ 100x là tín hiệu bench sai, không phải máy lạ.

## Nợ kỹ thuật / để dành cho sau

Công cụ trả nợ đã dựng xong — hướng dẫn từng món: [`docs/linux-baseline.md`](../docs/linux-baseline.md).

- [x] **#3 p50/p99** — xong 2026-08-31: `-repeat N`, đo từng op. Phát hiện: group commit 256
      cho throughput 140x mà p99 gần như không đổi.
- [x] **#5 kiểm chứng `fadvise`** — xong 2026-08-31: `-verify-cache` dùng `mincore(2)`,
      residency 100% → 0%. Không còn phải suy luận gián tiếp.
- [ ] **#2 `O_DIRECT`** — công cụ xong (`-direct`, buffer aligned). Chờ đo trên Linux thuần:
      `go run ./cmd/iolab -filemb 512 -direct -repeat 3`. Cần đọc thêm
      `/sys/block/<dev>/queue/write_cache` — nếu ổ bật write-back cache thì fsync còn phải ép
      ổ flush cache riêng của nó.
- [ ] **#4 baseline sạch trên Linux thuần** — `DIR=/mnt/nvme/iolab REPEAT=5 ./scripts/linux-baseline.sh`.
      Sau đó `diff` JSON hai máy. Câu hỏi cần trả lời: **tỉ số nào giữ nguyên qua hai máy?**
      Cái giữ nguyên là quy luật vật lý, đáng để thiết kế dựa vào; cái đổi chỉ là đặc tính máy.
- [ ] **#1 torn write thật** — `cmd/tornlab` + `scripts/dm-flakey.sh` đã sẵn sàng, cần root và
      máy thí nghiệm. Câu hỏi cần trả lời: **pagesize nào bắt đầu xuất hiện torn write trên ổ này?**
      (thử 4096 / 16384 / 65536). Đã học được khi viết công cụ: `kill -9` không tạo được torn
      write, phải bơm lỗi ở tầng thiết bị bằng `dm-flakey drop_writes`.
