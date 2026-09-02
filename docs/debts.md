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

**Bằng chứng mới từ phase 5** — món nợ này không chỉ là "chưa đo được torn write", nó làm
**mọi bài test crash bằng `kill -9` mất khả năng phát hiện thiếu fsync**:

```console
$ go run ./cmd/crashlab -n 20 -nosync
20/20 vòng đúng, 0 sai. 10641 transaction đã commit được kiểm, 13.079s.
```

Tắt **sạch** fsync mà 20/20 vòng vẫn đúng, vì `kill -9` giết tiến trình chứ không giết page cache
của kernel. Phase 5 đi vòng bằng `wal.Log.NoWrite` (giữ byte log trong buffer **tiến trình** nên
`kill -9` mang chúng đi cùng) → `make crashlab-nowrite` báo 10/10 SAI. Nhưng đó là mô phỏng ở tầng
ứng dụng, **không** thay được việc bơm lỗi ở tầng thiết bị: nó không tạo được page bị ghi **một
nửa**, chỉ tạo được page **không** được ghi.

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

### 🔧 P4-1 · Không có overflow page

Giá trị lớn hơn `MaxEntrySize` (2028 byte) bị từ chối thẳng bằng `ErrEntryTooLarge`. DB thật
tràn phần dư sang một chuỗi overflow page.

```bash
go test ./internal/btree -run TestPutHugeValue -count=1   # value 100KB phải PASS, không phải báo lỗi
```

**Câu hỏi quyết định:** ngưỡng nào thì đáng tràn? SQLite giữ lại một phần payload trong leaf
(`minLocal`/`maxLocal`) để tra cứu không phải đi thêm một page cho giá trị vừa vừa.

### 🔧 P4-2 · Branch node chưa có prefix compression / suffix truncation

Khóa phân tách đang lưu **nguyên vẹn**, trong khi nó chỉ cần đủ dài để phân biệt hai bên.

```bash
make btreelab   # so cột `branch/page` của mục 3 trước và sau
```

**Số hiện tại để so:** khóa 16 byte → `branch/page = 77` (trần lý thuyết 156, branch chỉ đầy
~50%). **Câu hỏi:** với khóa có tiền tố chung dài (đường dẫn, tên miền ngược) thì đẩy được
fanout lên bao nhiêu, và chiều cao cây có tụt một tầng không?

### 📏 P4-3 · Cursor re-pin mỗi `Next()`, chưa tách được giá của nó

```bash
go test ./internal/btree -run '^$' -bench 'Scan' -benchtime=20x
```

**Số hiện tại:** `91.23 ns/key`. **Câu hỏi:** bao nhiêu phần trong đó là pin/unpin? Trả bằng
một biến thể cursor giữ pin suốt một leaf rồi so ns/key. Có thể kết luận "không đáng làm" —
đó cũng là một cách trả.

### ⏳ P4-5 · Chưa có latch-coupling: cây không an toàn khi nhiều goroutine cùng ghi

`go test -race` xanh chỉ vì mọi test hiện tại đều đơn luồng — đó **không** phải bằng chứng an
toàn. Đây là phase 7.

### 📏 P4-6 · `ns/op` của `BenchmarkGetPool*` không phải số đo I/O

`MemDB` chỉ memcpy 4KB nên `ns/op` đang đo footprint cache CPU, không đo đĩa. Bằng chứng:
pool 512 frame (2MB, lọt L2) = 1658 ns/op, pool 2048 frame (8MB, vượt L2) = **2768 ns/op** —
pool to hơn mà chậm hơn, trong khi `reads/op` giảm đều.

```bash
lscpu | grep -i cache        # L2 3.8MiB, L3 12MiB trên máy này
# trả bằng: chạy lại benchGet trên pager THẬT, dùng -verify-cache của phase 0 ép cache lạnh
```

**Câu hỏi:** `reads/op × thời gian một pread` có khớp `ns/op` thực đo không? Lệch thì phần
lệch là chi phí của chính buffer pool.

### 📏 P4-7 · Chưa đo xóa theo CỤM

G5 của phase 4 sai (dự đoán file co chậm hơn dữ liệu > 2x; đo được **89% page được trả lại**,
tỉ số 1.07x) — nhưng lab chỉ xóa **ngẫu nhiên đều tay**, kịch bản làm mọi leaf cạn cùng nhịp
nên merge nổ liên tục. Xóa hết một **dải khóa liên tiếp** là kịch bản mà giả thuyết cũ có thể
đúng.

```bash
# trả bằng: thêm -delmode range vào cmd/btreelab mục 4, rồi
make btreelab
```

**Câu hỏi:** xóa 90% khóa theo cụm thì trả lại bao nhiêu % page? Nếu thấp hơn hẳn 89% thì
"file không co" là vấn đề của **phân bố xóa**, không phải của thuật toán merge.

### 🔧 P4-8 · `fixUnderfull` chỉ xét MỘT anh em

Anh em bên đó không gộp được thì bỏ cuộc, dù bên kia có thể gộp được.

```bash
go test ./internal/btree -run TestPropertyRandomOps -v -count=1   # đọc dòng MergeMissed
```

**Số hiện tại:** `MergeMissed = 0` ở cả hai hồ sơ ⇒ **có thể món nợ này không đáng trả**.
Nhưng phải đo lại ở workload xóa theo cụm của P4-7 trước khi kết luận — đúng theo quy tắc 2:
nợ 📏 không được đoán.

---

### 🔧 P5-1 · Không có background page cleaner

Checkpoint **mờ** kéo được điểm bắt đầu redo nhưng **không** chặn được độ dài redo, vì
`redoLSN = min(recLSN)` bị ghim bởi page bẩn cũ nhất — root của cây B+ nằm mãi trong pool và bẩn
liên tục. Đo được:

```console
$ go test ./internal/db/ -run '^$' -bench 'Recover' -benchtime=10x -timeout 30m
BenchmarkRecoverCkpt1M-6    	      10	  90260761 ns/op	     11593 logKiB	     13033 redo
BenchmarkRecoverCkpt64K-6   	      10	 102961883 ns/op	     13214 logKiB	     13059 redo
BenchmarkRecoverCleaner-6   	      10	  66931825 ns/op	     11653 logKiB	         0 redo
```

Checkpoint dày hơn **16 lần** không cắt được record redo nào; thêm người dọn page thì redo về 0.

**Cách trả:** một goroutine dọn page chạy nền, ghi page bẩn theo thứ tự `recLSN` tăng dần, có hạn
mức IO. Trả xong thì `BenchmarkRecoverCkpt1M` phải tiến về phía `RecoverCleaner`, và thêm một
benchmark đo **độ trễ** mà người dọn page gây cho đường ghi.

### 🔧 P5-2 · Log không bao giờ được cắt hay tái dùng

```console
$ make wallab
độ dài    52103476 byte, 36135 record, checkpoint gần nhất tại LSN 52103404
```

52 MB log cho một database 12 KB dữ liệu = **234x**. Không có cắt đầu log, không có xoay vòng
segment, không có xoá record đã nằm trước checkpoint bền.

**Vướng ở đâu:** LSN **là** offset byte trong file (chọn ở phase 5, kiểu Postgres), nên không cắt
được đầu file mà không đổi nghĩa của mọi LSN đã ghi trong `pageLSN`. Đây là cái giá của lựa chọn ấy.

**Cách trả:** chia log thành nhiều **segment** file (`wal.000001`, ...), LSN vẫn là offset **toàn
cục**; xoá segment nào nằm hoàn toàn trước `redoLSN` của master record. Trả xong thì `wallab` phải
báo số segment, và một test phải chứng minh recovery vẫn đúng sau khi xoá segment cũ.

### ⏳ P5-3 · Undo physical nên phải mang ảnh-trước; FREE mang trọn ảnh page

Undo dán lại **byte**, nên mỗi record UPDATE phải chở cả phía before. Và record FREE phải chở
**trọn** 4KB (bản sửa ở phase 5: `pool.FreePage` vứt nội dung bẩn đi nên undo cần nguồn khác).

```console
$ make wallab
loại               số     byte log byte dữ liệu byte/record
UPDATE          30761     46987552     44941248       1528
FREE              691      2869032      2830336       4152
```

691 record FREE = 2.87 MB trên 52 MB.

**Cách trả (và vì sao chưa):** undo **logical** (ghi "xoá khóa K" thay vì "dán lại byte") bỏ được
phía before, nhưng đòi undo phải gọi lại được thao tác cây ở trạng thái bất kỳ — tức là đòi
latch-coupling (P4-5) và một khái niệm mini-transaction chặt hơn. Để sau MVCC.

### 🔧 P5-4 · Freelist của pager vẫn chưa được WAL log

Đây là nguyên nhân gốc của **bốn** bug trong phase 5 (xem `diary/phase5-log.md` mục 2.8–2.11):
WAL bảo vệ page dữ liệu, freelist thì không, mà hai bên dùng chung một không gian PageID.

Hiện chống đỡ bằng ba lớp, không phải bằng log:

```bash
grep -n metaPending internal/pager/wal.go internal/pager/pager.go
grep -n skipRedo internal/db/recover.go
grep -n ErrMetaPageBusy internal/pager/pager.go   # chốt chặn thường trực
```

**Cách trả:** log cả thao tác freelist (ALLOC/FREE đã có, còn thiếu chính chuỗi page chứa
freelist), rồi bỏ `skipRedo`. Trả xong thì `crashlab -n 200` phải vẫn 200/200 **sau khi** đã xoá
`skipRedo`.

### 🔧 P5-5 · `d.dpt` và `OnFlush` truy cập map không có latch riêng

`OnFlush` được gọi từ trong `bufpool.writeFrame`, tức là có thể từ luồng nào gọi `Pin`; nó ghi vào
`d.dpt`. Hiện an toàn **chỉ nhờ** một-writer (P1-2b/P4-5), không nhờ latch.

```bash
go test -race ./internal/db -count=1    # hiện xanh, vì test chưa có nhiều writer
```

**Cách trả:** latch riêng cho DPT, hoặc dồn mọi thay đổi DPT về một luồng. Trả xong thì phải có
một test `-race` với nhiều goroutine cùng `Pin`/`Unpin` mà vẫn xanh.

### 📏 P5-6 · Chưa đo recovery trên log lớn, và Analysis không có giới hạn bộ nhớ

Bảng ATT/DPT nằm hết trong RAM và pha Analysis quét từ master record tới cuối log. Log lớn nhất
từng đo là 52 MB — chưa biết hình dạng ở mức GB.

```bash
# trả bằng: thêm cờ -logmb vào cmd/wallab hoặc một bench sinh log N GB, rồi
go test ./internal/db/ -run '^$' -bench 'Recover' -benchtime=3x
```

**Kỳ vọng cần viết ra trước khi đo:** thời gian Analysis tuyến tính theo độ dài log; bộ nhớ tuyến
tính theo **số page bị chạm** chứ không theo độ dài log. Lệch khỏi cái thứ hai là có rò.

### 🔧 P6-1 · Chuỗi version nằm TẠI CHỖ: trần cứng ~2KB một khóa

Cả chuỗi version của một khóa phải nhét vừa **một** entry B+Tree (`btree.MaxEntrySize` = 2028).
Với value 40 byte thì chạm trần ở lần ghi lại thứ **41**; `MaxVersions = 64` không bao giờ tới.
Và nếu có một reader cũ ghim `horizon` thì writer chết ở lần thứ **63**.

```bash
go test ./internal/txn/ -run 'TestChainFullIsReported|TestOldReaderStarvesWriterOnSameKey' -count=1 -v
```

Đây là **giới hạn cứng của MVCC-tại-chỗ**, và là lý do thật sự vì sao DB thật không để bản cũ tại
chỗ: Postgres tạo tuple mới ở **page khác**, InnoDB đẩy bản cũ sang **undo segment**. Bản này chọn
tại chỗ vì nó làm luật visibility hiện ra rõ nhất.

**Cách trả:** đẩy bản cũ ra khỏi entry — hoặc overflow page (nợ P4-1, cùng một cơ chế), hoặc một
undo segment riêng. Cả hai đều đổi trần cứng thành một lần truy đĩa thêm cho reader bản cũ.
**Kỳ vọng cần viết ra trước khi đo:** `BenchmarkGetChainDepth/oldest` sẽ đắt hơn `newest` rõ rệt
sau khi trả — hiện tại chúng bằng nhau (0.98x), xem P6-2.

### 🔧 P6-2 · `DecodeChain` giải mã trọn chuỗi dù chỉ cần một version

```bash
go test ./internal/txn/ -run '^$' -bench 'GetChainDepth' -benchtime=200000x -count=3
```

Đã đo: ở depth=60, nhánh `oldest` / `newest` = 1480/1450 = **0.98x**. Nếu chi phí nằm ở vòng lặp
visibility thì đọc bản **cũ nhất** phải đắt hơn đọc bản **mới nhất** rõ rệt. Nó không ⇒ chi phí
nằm ở `DecodeChain`, nó giải mã **cả chuỗi** trước khi ai đó hỏi cần version nào.

**Cách trả:** giải mã **lười** — đi con trỏ qua header từng version, chỉ dựng `Version` cho bản
thật sự được trả về. Header cố định 11 byte nên bước nhảy là O(1).
**Kỳ vọng:** `newest` ở depth=60 tiến gần về `newest` ở depth=1 (~171ns); `oldest` giữ nguyên.
Nếu **cả hai** đều giảm thì bench đang đo cái khác — nghi bench trước.

### 🔧 P6-3 · Lock manager không có chỉ mục theo đối tượng

```bash
go test ./internal/lock/ -run '^$' -bench 'AcquireDisjoint' -benchmem
```

Đã đo: 256 holder **không chồng nhau** = 3762 ns vs 1 holder = 168.7 ns ⇒ **22.3x**, mà **không
có tranh chấp nào** — chỉ có việc đi hết danh sách để biết là không tranh chấp. `Res.Overlaps`
bản thân nó là 13.34 ns / **0 alloc**, nên vấn đề là O(n), không phải hằng số.

**Cách trả:** băm khóa điểm vào một map; giữ danh sách tuyến tính **chỉ** cho khoá span (span thì
không băm được). Khi đó đường phổ biến (khóa điểm) thành O(1).
**Kỳ vọng:** `AcquireDisjoint/holders=256` về gần `holders=1`; `AcquireShared/holders=256` **không
đổi** (chúng chồng nhau thật, phải xét thật).

### ⏳ P6-4 · `Txn.Scan` materialize cả kết quả thay vì stream

`Txn.Scan` gom hết cặp khóa/giá trị nhìn thấy được vào RAM rồi mới gọi `fn`. Lý do lịch sử: nó
gọi `db.DB.Range`, mà `Range` giữ `d.mu` **suốt** lần duyệt (xem chú thích tại chỗ), nên không
được gọi lại vào tầng txn từ trong callback.

```bash
go test ./internal/txn/ -run '^$' -bench 'Scan' -benchtime=200000x -count=3   # 700µs / 2000 khóa
```

**Cách trả:** cần **latch-coupling (P4-5)** trước. Chừng nào cursor còn thả pin giữa hai bước
`Next()` thì stream qua một writer đồng thời là hành vi chưa định nghĩa. Đây là một chỗ **đo được
cái giá của việc chưa có P4-5**.

### 🔧 P6-5 · Không có vacuum nền

`Vacuum()` chỉ chạy khi có ai gọi. Bộ dọn opportunistic trong `Txn.apply` gần như miễn phí nhưng
chỉ chạm những khóa **đang được ghi** — một khóa ghi 20 lần rồi không ai chạm nữa sẽ giữ 20
version mãi mãi.

```bash
go run ./cmd/txnlab -work bloat    # tỉ số phình = 18.07x
```

Cùng họ với **P5-1** (không có background page cleaner): cả hai đều là "có cơ chế dọn, không có
người dọn". Trả chung được: một goroutine nền, một ngân sách I/O, và một cách đo được là nó
không đói cũng không ngốn.

### 📏 P6-6 · Phase 6 chưa đi qua `kill -9` thật

`FuzzTxnCrash` crash bằng `db.SimulateCrash()` **trong tiến trình**. Phase 5 đã học được rằng
`kill -9` **không** đủ để lộ việc thiếu fsync (page cache của kernel sống lâu hơn tiến trình —
nợ P0-1), nên `SimulateCrash` không phải một lựa chọn tồi. Nhưng nó cũng không phải cùng một bài
kiểm tra.

```bash
# trả bằng: thêm chế độ -txn vào cmd/crashlab (workload đi qua txn.Store), rồi
go run ./cmd/crashlab -txn -n 200
```

**Kỳ vọng:** `BadChains == 0` sau cả 200 vòng, và số version sau recovery bằng đúng số version
của các transaction đã commit. Lệch ⇒ có chuỗi bị áp **một nửa**, tức `apply` không thật sự
nguyên tử như thiết kế nói.

### ⏳ P6-7 · `Serializable` cài bằng S2PL, không phải SSI

Mức `Serializable` chặn write skew bằng cách giữ khóa S trên cái đã đọc, nên nó chặn bằng
**deadlock** (`Deadlocks != 0` ở `TestSerializableBlocksWriteSkewByDeadlock`). SSI thì theo dõi
phụ thuộc đọc-ghi và chỉ abort khi thấy một **hình** nguy hiểm, không phải khi thấy một xung đột
thật — rẻ hơn nhiều, và không cần `GetForUpdate`.

```bash
go run ./cmd/txnlab -work transfer -accounts 2 -workers 12 -ops 100
```

**Câu hỏi quyết định khi trả:** SSI có xoá được **cả hai** cột yếu của bảng hiện tại không — vừa
tránh việc OCC bỏ lượt (26/1200 ở tranh chấp cao), vừa tránh việc S2PL bắt ứng dụng khai
`GetForUpdate`? Nếu không thì nó chỉ là một điểm khác trên cùng đường đánh đổi.

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
| 🔧 P1-1 · Page mồ côi sau rollback | `bufpool.Discard(id)` trước `pg.FreeNow(id)` trong cả `Txn.Abort` lẫn `recover()` | `TestAbortLeavesNoOrphanPage` — và nó **tố oan một lần**: "abort nới 50 page nhưng chỉ trả 49" hoá ra là page chứa freelist, phép đếm sai chứ không phải code. `dbcheck` im lặng sau 120 txn có abort |
| ⏳ P1-2b · Chưa có cô lập cho reader đồng thời | MVCC snapshot isolation trong `internal/txn` — **đúng cách mà mục này đã dự đoán từ phase 5**. Chuỗi version trong value của B+Tree, mới-nhất-trước; **không** có `xmax` (trùng lặp với `xmin` bản kế) và **không** có clog (version chỉ vào cây khi đã commit) | `TestSnapshotReadDoesNotBlockWriter`. Và nó **tố oan một lần**: bản đầu đỏ ở *"writer thứ 63: chuỗi version đã đầy"* — reader **không lấy khóa** của writer (đúng) nhưng reader cũ **ghim horizon** nên writer trên cùng một khóa vẫn chết. Tách thành hai bài nói hai vế + `TestOldReaderStarvesWriterOnSameKey`. Phình đo được **18.07x** (`txnlab -work bloat`) |
| ⏳ P1-2 · Chưa có transaction thật | `Begin`/`Commit`/`Abort` trong `internal/db`; một writer **do code bắt buộc** | `TestSingleWriter` → `ErrWriterBusy`. Phần reader đồng thời tách thành **P1-2b** |
| ⏳ P2-3 · Page không có checksum riêng | **Chọn không** thêm checksum cho page: crc32c mỗi record log + **ảnh trọn page** ở lần chạm đầu sau mỗi checkpoint (`full_page_writes` của Postgres), redo áp vô điều kiện | `TestFullPageWriteAppearsOncePerCheckpoint`. Lý do phải thế: page bị torn thì `pageLSN` là **rác**, nên chốt `pageLSN >= rec.LSN` sẽ bỏ qua đúng cái page đang hỏng. Giá: payload 4244 vs 284 byte (`BenchmarkEncodeFullPage`) |
| ⏳ P4-4 · Root đổi `PageID` mỗi lần cây cao thêm | **Quyết định: giữ cho root di chuyển.** Mỗi lần đổi sinh một record ROOT được log; meta chỉ giữ root ở lần checkpoint cuối | `make wallab`: `ROOT  9 record  504 byte  56 byte/record`. Không cố định root vào một page id, vì như thế phải **copy nội dung** mỗi lần cây cao thêm — mà việc copy ấy lại phải log trọn page, đắt hơn 56 byte |
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
