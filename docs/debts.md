# Sổ nợ kỹ thuật

Một món nợ = **một thứ tôi biết là còn thiếu**, kèm **lệnh để trả nó**. Không có lệnh thì
đó chỉ là lo lắng, không phải nợ.

Ba loại, và cách xử lý khác hẳn nhau:

| Loại | Nghĩa | Cách trả |
|---|---|---|
| 🔧 **code** | Sửa được ngay trên máy này, chỉ cần viết test trước | Viết test cho nó **fail**, rồi sửa cho **pass** |
| 📏 **đo** | Cần máy Linux thuần / phần cứng thật mới có số đáng tin | Công cụ dựng sẵn, chạy lệnh, dán output vào diary |
| ⏳ **phase sau** | Chưa đủ ngữ cảnh để quyết định | Ghi lại, đừng đoán non |

---

## Đang nợ

### 📏 P0-1 · Torn write thật

`kill -9` **không** tạo được torn write (kernel vẫn writeback đầy đủ). Cần bơm lỗi ở tầng thiết bị.

```bash
sudo ./scripts/dm-flakey.sh up          # tạo /mnt/flakey
go run ./cmd/tornlab -mode write -dir /mnt/flakey -pagesize 4096 -seconds 30 &
sudo ./scripts/dm-flakey.sh drop        # thiết bị ACK write rồi vứt đi — lừa được cả fsync
sudo ./scripts/dm-flakey.sh heal
go run ./cmd/tornlab -mode verify -dir /mnt/flakey
```

**Câu hỏi:** ở pagesize nào thì torn write bắt đầu xuất hiện trên ổ đó? Thử 4096 / 16384 / 65536.
Chi tiết + bảng 3 kết cục: [`linux-baseline.md`](./linux-baseline.md).

### 📏 P0-2 · `O_DIRECT`

```bash
go run ./cmd/iolab -filemb 512 -direct -repeat 3
cat /sys/block/<dev>/queue/write_cache
```

**Cần nhìn:** cột "nóng" phải **bằng** cột "lạnh". Nếu không bằng thì filesystem đã lặng lẽ lờ
`O_DIRECT` đi và mọi số đều vô nghĩa.

### 📏 P0-4 · Baseline so được giữa các máy

```bash
DIR=/mnt/nvme/iolab REPEAT=5 ./scripts/linux-baseline.sh
```

**Câu hỏi quyết định:** *tỉ số nào sống sót qua hai máy?* Cái sống sót là quy luật vật lý và đáng
để thiết kế dựa vào; cái đổi chỉ là đặc tính của một cái máy.

### 🔧 P1-1 · Page mồ côi sau rollback

Txn bị rollback đã nới file nhưng page đó không thuộc meta nào, cũng không nằm trong freelist.
Giờ **phát hiện được bằng lệnh**:

```console
$ go run ./cmd/dbcheck data/test.db
  ⚠ RÒ RỈ ĐUÔI FILE: 1 page nằm ngoài pageCount=13 — page mồ côi sau rollback
```

**Cách trả:** lúc `Open`, so `pageCount` trong meta với kích thước file thật; phần dư thì hoặc
đưa vào freelist, hoặc `Truncate` xuống. Trả xong thì `dbcheck` phải im lặng.
**Cẩn thận:** không được truncate khi còn reader đang mở file (liên quan P1-2).

### ⏳ P1-2 · Chưa có transaction thật

`Commit(root)` là API cấp thấp: chưa có `Begin()`, chưa chặn hai writer đồng thời, chưa có
`Rollback()` chủ động. Quy ước "một writer" hiện chỉ tồn tại trong đầu tôi — **không có gì trong
code bắt buộc nó**. Để phase 6.

### ⏳ P1-3 · Chính sách cấp phát (đã đo, giả thuyết ban đầu SAI)

Xem bảng đo trong [`../diary/phase1.md`](../diary/phase1.md): LIFO và lowest-first cho ra
**cùng một** `page-gap` = 1.875, tức cả hai đều ghi tuần tự. Lý do: freelist luôn được sort trước
khi ghi xuống đĩa, nên nó không bao giờ "lởm chởm".

```bash
go test ./internal/pager -bench BenchmarkAlloc -benchtime=2000x -run XXX
```

Nguồn ghi ngẫu nhiên thật sẽ là **phân mảnh do B+Tree** (phase 4), không phải chính sách cấp phát.
Đo lại ở đó, trên đĩa thật, cùng lúc với P0-4.

### 📏 P1-6 · Chạy lại bench của phase 1 trên Linux thuần

```bash
go test ./internal/pager -bench . -benchtime=200x -run XXX -count=5
```

**Câu hỏi:** tỉ số 481x (`Commit`/`CommitNoSync`) và 43x (gộp 64 page) có sống sót không?

### ⏳ P2-1 · Không có forwarding pointer

`Update` làm record to ra quá chỗ trống thì trả `ErrPageFull` và bỏ mặc tầng trên.

```bash
go test ./internal/page -run TestUpdateKeepsSlotID -v
```

DB thật có hai lối: dời record sang page khác rồi để lại con trỏ (Oracle *row migration*), hoặc
tạo phiên bản mới ở page khác (Postgres). Chọn lối nào là hệ quả của mô hình MVCC — **để phase 6**,
đoán bây giờ là đoán non.

### 🔧 P2-2 · Không tái dùng được lỗ hổng nếu chưa compact

SQLite giữ danh sách *freeblock* ngay trong page nên nhét vừa record mới vào một lỗ cũ mà không
phải dồn cả page. Bản này chỉ có tổng `frag`: muốn dùng lại thì compact hết.

```bash
go test ./internal/page -bench 'Compact' -benchmem -count=3
```

**Đo trước khi sửa:** `Compact` chỉ tốn ~2µs, còn `pager.Commit` tốn 1.37ms — **668x**. Rất có thể
freeblock *không thắng ở workload nào cả*. Trả nợ này = dựng được một workload mà freeblock thắng,
kèm số; nếu không dựng được thì đóng nợ bằng kết luận "không đáng làm", cũng là trả.

### ⏳ P2-3 · Page không có checksum riêng

Toàn vẹn của slotted page hiện **thừa hưởng** từ atomicity của meta page (phase 1): page chỉ tồn
tại khi meta trỏ tới nó.

```bash
go test ./internal/page -run TestPageRoundTripsThroughPager -v
```

Khi buffer pool (phase 3) ghi page ra đĩa **ngoài** luồng commit, giả định đó vỡ. Xét lại ở đó,
cùng lúc với chỗ đặt `pageLSN` (đã chừa sẵn 8 byte trong header).

### ⏳ P2-4 · `TrimDeadSlots` chưa được ai gọi tự động

Đang là API thủ công (`cmd/slotlab` gọi để minh hoạ). Gọi lúc nào là chính sách của tầng access
method — **phase 4**.

```bash
go run ./cmd/slotlab -n 24 -size 120 -delete random -seed 7 -churn 4
```

**Số hiện tại:** 4 vòng churn để lại 61/91 slot chết = 244 byte = **6% page** là con trỏ tới hư vô.

### 📏 P2-5 · Chạy lại bench của phase 2 trên Linux thuần

```bash
go test ./internal/page -bench . -benchmem -count=5 -run XXX
```

**Câu hỏi:** bảng `-count=3` trên WSL2 cho thấy `VerifyRef` dao động tới 43% giữa các lần chạy.
Tỉ số 5.5x (`verifyRef`/`Verify`) và 25x (insertion sort/`slices.SortFunc`) có sống sót không?
Đo cùng lúc với P0-4.

### 📏 P3-1 · Chưa đo được lợi thế thật của CLOCK (đa luồng)

Cả pool đi qua **một** `p.mu`. Chính sách thay thế nằm bên trong khoá đó nên nó rẻ hay đắt cũng
không đổi được thông lượng — đo được: 6 luồng cùng pin một page nóng chỉ nhanh hơn 1 luồng
**2.3x** (62ns → 159ns mỗi thao tác), và CLOCK chỉ hơn LRU 3%.

```bash
go test ./internal/bufpool -run '^$' -bench PinHitParallel -benchmem -count=5 -cpu 1,2,4,6
```

**Cách trả:** chia bảng tra thành N mảnh (mỗi mảnh một mutex, chọn mảnh theo `pageID % N`), rồi
đo lại đúng lệnh trên. **Câu hỏi quyết định:** lúc đó CLOCK có tách khỏi LRU không? Nếu vẫn
không thì lý thuyết "CLOCK thắng nhờ không phải sửa cấu trúc chung" là sai *ở quy mô này*, và
đó cũng là một kết luận đáng ghi.

### 🔧 P3-2 · Giữ `p.mu` trong suốt lúc đọc đĩa

`Pin` gọi `store.ReadPage` khi **đang giữ** `p.mu`. Với `MemStore` thì không thấy gì, nhưng với
đĩa thật thì mọi miss trong toàn hệ thống nối đuôi nhau qua một khoá — 69µs mỗi lần (phase 0).

```bash
go test ./internal/bufpool -run '^$' -bench PinMiss -benchmem -count=3
```

**Cách trả:** đánh dấu frame là "đang nạp", nhả `p.mu`, đọc, rồi lấy lại khoá — luồng khác gặp
frame "đang nạp" thì chờ frame đó chứ không chờ cả pool. **Viết test đỏ trước:** một store cố
tình chậm (sleep 1ms), 2 goroutine đọc 2 page khác nhau; đo tổng thời gian — hiện tại phải ra
~2ms, trả nợ xong phải ra ~1ms.

### 🔧 P3-3 · `Victim` của LRU-K là O(số frame)

```bash
go test ./internal/bufpool -run '^$' -bench 'Victim/lru-2' -benchmem -count=3
```

**Số hiện tại:** 108ns ở 64 frame → **1339ns** ở 1024 frame (frame ×16 thì chi phí ×12.4).
Ngoại suy tới pool 400MB (100k frame) thì mỗi lần chọn nạn nhân ~130µs, **đắt hơn cú đọc đĩa
69µs mà nó định tiết kiệm**. Trả bằng heap theo khoảng cách lùi K, hoặc xấp xỉ kiểu CLOCK-2 bit.
**Chỉ đáng trả khi pool thật sự lớn** — ghi lại đây để không quên rằng con số 0.817 hit ratio
của LRU-2 là số đo ở pool 64 frame.

### ⏳ P3-4 · Chưa có prefetch, chưa có ring buffer cho scan

Phase này chỉ chứng minh được LRU-K *chống* được scan. Postgres đi đường khác: cho tầng trên
**tự khai báo** "tôi đang seq scan" rồi giam nó vào một ring buffer nhỏ. Đường đó cần một access
method biết nó đang quét — **phase 4**. Cùng lúc đó mới đo được prefetch (đọc trước n page kế
tiếp), vì trước khi có B+Tree thì không có khái niệm "page kế tiếp".

### 📏 P3-5 · Chạy lại bench của phase 3 trên Linux thuần

```bash
go test ./internal/bufpool -run '^$' -bench . -benchmem -count=5 -cpu 1,6
```

**Câu hỏi:** tỉ số 1366x (miss/hit) và 2.3x (6 luồng/1 luồng) có sống sót không? Hit ratio thì
**không cần** đo lại — nó là hàm của workload và chính sách, không phụ thuộc máy. Đó cũng là
cách phân biệt số nào cần đo lại: số nào có đơn vị thời gian thì cần, số nào là tỉ lệ thuần thì không.

---

## Đã trả

| Món | Trả bằng | Bằng chứng |
|---|---|---|
| 📏 P0-3 · p50/p99 | `iolab` đo từng op + cờ `-repeat N` | Phát hiện: group commit 256 làm throughput ×140 mà p99 **không** tăng |
| 📏 P0-5 · `fadvise` có thật sự đẩy cache ra? | cờ `-verify-cache` dùng `mincore(2)` | `residency 100.0% -> 0.0%` |
| 🔧 P1-4 · `WriteAt` trả `n < len(p)` mà `err == nil` | `writeFull()` trong `pager.go` | `TestShortWriteIsAnError` — trước khi sửa: *"Commit báo THÀNH CÔNG dù lời ghi chỉ đi được 4095/4096 byte"* |
| 🔧 P1-5 · Không có cách kiểm tra file từ bên ngoài | `pager.Verify()` + `cmd/dbcheck` | Bắt được: double free, freelist tự trỏ vào chính nó, meta page bị liệt kê là rỗng, chuỗi có vòng lặp, file cắt giữa page, rò rỉ đuôi file |
| 🔧 P2-0 · `Compact` dùng insertion sort, giả định offset đã gần sắp xếp | `slices.SortFunc` trên mảng nằm trên stack | `TestCompactOrderIsScrambled` dựng được thế 299/300 nghịch thế; `BenchmarkCompactScrambled` 229810 → 9169 ns/op = **25x**, 0 alloc |
| 🔧 P2-0b · `Verify` cấp phát 20KB mỗi lần gọi, bóp nghẹt fuzz | bitmap 512 byte trên stack; giữ bản cũ làm `verifyRef` để kiểm tra chéo | `BenchmarkVerifyRefFullPage` 19768 ns / 20576 B vs `BenchmarkVerifyFullPage` 3583 ns / **0 B**; fuzz đi từ 63k lên **1 421 899** exec |
| 🔧 P3-0 · `victim()` quay vô hạn khi WAL rule chặn mọi ứng viên | đếm số lần bị chặn, hết một vòng frame thì `ErrNoFrame` | `go test -run TestWALRule -timeout 10s` trước khi sửa: `panic: test timed out after 10s`; sau khi sửa: PASS, `store.Writes = 0` |

---

## Quy tắc

1. **Nợ 🔧 phải được viết thành test fail trước khi sửa.** Không có test đỏ thì không biết mình
   đã sửa cái gì. P1-4 là ví dụ: test in ra đúng câu *"Commit báo THÀNH CÔNG dù..."* — đó mới là
   bằng chứng, không phải lời hứa.
2. **Nợ 📏 không được đoán.** Viết tỉ số kỳ vọng ra trước, đo, rồi so. Lệch xa thì **nghi bench
   sai trước**, đừng nghi máy lạ.
3. **Trả xong thì chuyển xuống bảng "Đã trả" kèm bằng chứng**, và tick checkbox trong diary của
   phase tương ứng.
