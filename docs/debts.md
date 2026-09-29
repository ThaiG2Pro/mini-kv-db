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

### ⏳ P4-5 · Chưa có latch-coupling — **nửa ĐỌC đã trả ở phase 7, nửa GHI còn nguyên**

Món này bị **tách làm hai** ở phase 7, vì hoá ra hai nửa của nó có giá khác nhau hẳn.

**Nửa đọc — ĐÃ TRẢ.** Một cursor được nhả latch giữa hai bước `Next()` mà vẫn đúng, nhờ **số đời
cấu trúc** (`btree.Tree.gen`, tăng ở mọi `Put`/`Delete`/đổi root) + tìm lại chỗ **theo khóa** chứ
không theo `(page, slot)` (`Cursor.restore`). Đây là *cursor restoration* của InnoDB/SQLite, rẻ
hơn latch-coupling (crabbing) một bậc: không có thứ tự latch để làm sai, không có deadlock giữa
các latch để phát hiện. Chi tiết ở [`diary/phase7.md`](../diary/phase7.md).

```bash
go test ./internal/db/ -run 'TestIterSurvivesConcurrentWriter' -race -count=1 -v
# 400 khóa, 212 lần tìm lại chỗ, không lần nào lệch thứ tự
```

**Bài phản chứng** — chứng minh cơ chế trên thật sự đang được kiểm:

```bash
sed -i 's/if c.gen != c.t.gen {/if false {/' internal/btree/cursor.go
go test ./internal/btree/ ./internal/db/ ./internal/table/ -count=1 \
    -run 'CursorRestores|Iter|IndexScanSurvivesWriterMidScan'   # PHẢI đỏ
sed -i 's/if false {/if c.gen != c.t.gen {/' internal/btree/cursor.go  # phép nghịch — an toàn cả khi cursor.go chưa commit
```

Lần đầu chạy bài phản chứng ấy, `internal/txn`, `internal/table`, `internal/query` **xanh cả ba** —
mọi lần quét trong chúng chạy một mình nên `gen` không bao giờ đổi. Đã lấp bằng
`TestIndexScanSurvivesWriterMidScan`.

**Nửa ghi — CÒN NGUYÊN, và cố ý.** Nhiều goroutine cùng **sửa** cây vẫn là hành vi chưa định
nghĩa; hiện an toàn nhờ **một writer do code bắt buộc** (P1-2). Không trả vì lý lẽ của phase 6 còn
nguyên giá trị: nhiều writer vật lý buộc **bỏ pha undo physical** của phase 5 (A và B cùng sửa một
page, A abort, dán ảnh-trước của A là **xoá luôn việc của B**) — đó chính là lý do Postgres không
có pha undo. Trả món này là một quyết định kiến trúc, không phải một bản sửa.

**Ghi chú về cách ghi nợ.** Bản cũ của mục này viết *"Đây là phase 7"*, và mục P6-4 viết *"cách
trả: cần latch-coupling (P4-5) trước"*. Câu thứ hai **sai**: nó tìm cách trả một món nợ của phase 4
bằng công cụ của phase 4, trong khi thứ tháo được nó là **ảnh chụp MVCC** — một cơ chế ra đời ở
phase 6, **sau** khi món nợ được ghi. Bài học: ghi **hiện tượng** + **cách kiểm chứng**, đừng ghi
**cách trả**.

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

### 🔧 P7-1 · `keys.Decode` cấp phát trên đường đọc nóng nhất

```console
$ go test ./internal/keys/ -run '^$' -bench Decode -benchmem
BenchmarkDecode-6   	10522184	       117.2 ns/op	     152 B/op	       2 allocs/op
```

152 B + 2 alloc **mỗi hàng**, và nó nằm trên đường đọc nóng nhất trong máy: mỗi hàng của mỗi seq
scan. Nhân với 20000 hàng thì đó là 3 MB rác mỗi lần quét cả bảng.

```bash
# trả bằng: một API giải mã vào buffer có sẵn, rồi so lại
go test ./internal/keys/ -run '^$' -bench Decode -benchmem   # mục tiêu: 0 alloc
go test ./internal/query/ -run '^$' -bench SeqStep -benchtime=30x -count=3
```

**Cần nhìn:** `SeqStep` (413-544 ns/hàng hiện tại) giảm bao nhiêu. Nếu giảm < 10% thì `Decode`
không phải chỗ nghẽn và **kết luận "không đáng làm" cũng là một cách trả** — nhưng phải có số.

### ⏳ P7-2 · `CreateIndex` back-fill trong MỘT transaction

Write set của **cả bảng** nằm trong RAM lúc back-fill, và không có index build đồng thời. Bảng 10
triệu hàng là hết bộ nhớ.

```bash
go run ./cmd/idxlab -work maintain -rows 4000    # hiện tại: 4000 hàng, vừa RAM
# trả bằng: back-fill theo lô + một trạng thái "index đang xây" trong catalog
```

**Câu hỏi quyết định khi trả:** trong lúc xây, DML phải ghi vào index đang xây hay không? Postgres
`CREATE INDEX CONCURRENTLY` cần **hai** lần quét + chờ mọi transaction cũ xong, đúng vì câu hỏi này.

### ⏳ P7-3 · Không có DDL locking

`CreateIndex` chạy song song với DML là hành vi **chưa định nghĩa**. Hiện không có bài test nào
chạy hai thứ đó cùng lúc, nên `-race` xanh **không** phải bằng chứng.

```bash
# trả bằng: một bài test chạy CreateIndex đồng thời với Upsert, PHẢI đỏ trước khi sửa
```

Cùng họ với P4-5 nửa ghi: cả hai đều là "an toàn nhờ chưa ai thử", không phải nhờ cơ chế.

### 📏 P7-4 · `FuzzTableIndex` chưa bão hoà

```console
$ make fuzz-table
fuzz: elapsed: 2m0s, execs: 1580 (3/sec), new interesting: 108 (total: 259)
```

108 hạt mới trong 1580 exec (**6.8%**) nghĩa là corpus **còn đang mọc** — 120 giây chưa đủ để nói
"sạch". Chỉ 3 exec/giây vì mỗi exec có crash + mở lại + `Verify()` toàn cây.

```bash
go test ./internal/table/ -run '^$' -fuzz FuzzTableIndex -fuzztime 30m -fuzzminimizetime 1s
```

**Cần nhìn:** `new interesting` phải **về gần 0** ở phút cuối. Nếu vẫn mọc thì bộ sinh input đang
tạo ra quá nhiều ca giống nhau, và phải sửa bộ sinh chứ không phải chạy lâu hơn.

### 📏 P7-5 · Điểm hoà vốn 36.8% chỉ đúng khi CẢ CÂY nằm trong buffer pool

Đây là **giới hạn của kết luận chính** của phase 7. Điểm hoà vốn đo được cao hơn hẳn cái "thường
5-20%" của sách, và lý do là ở quy mô này **không có I/O thật nào cả**: `CFetch/CSeq` đo được 4.5x
thay vì 20x.

```bash
go run ./cmd/idxlab -work breakeven -rows 200000 -frames 64 -repeat 10
```

**Kỳ vọng:** pool 64 frame (256 KB) trên 200000 hàng ⇒ mỗi lần tra bảng là một `pread` thật ⇒
`CFetch/CSeq` phải **tăng** và điểm hoà vốn phải **tụt** về dải 5-20%. Nếu **không** tụt thì hoặc
page cache của kernel đang đỡ hết (nợ P0-1/P0-2, cần `O_DIRECT`), hoặc mô hình chi phí thiếu một
số hạng.

### 🔧 P7-6 · Selectivity dùng phân bố đều, không có histogram

```console
$ go run ./cmd/idxlab -work estimate
city = 'HN' (lệch 99%)            204      19800     97.0x  IndexScan
```

Lệch **97x** trên cột lệch 99%, và hậu quả không nằm ở con số mà ở cột cuối: planner chọn
`IndexScan` cho một truy vấn lấy **99% cả bảng** — kế hoạch **tệ nhất có thể**. Mô hình chi phí
đúng vẫn cho kế hoạch tệ nếu ước lượng số hàng sai.

```bash
# trả bằng: histogram equi-depth trong catalog + một lệnh ANALYZE
go test ./internal/query/ -run TestEstimateIsWrongOnSkew -count=1   # PHẢI đỏ sau khi trả
```

`TestEstimateIsWrongOnSkew` khẳng định **đúng cái giới hạn này**, nên nó sẽ đỏ khi món nợ được
trả — và đỏ đúng lúc. Đó là chủ ý, không phải sơ suất.

**Phase 9 thêm:** MySQL 8.4 mắc **đúng** lỗi này ở truy cập `ref`: cardinality của `st_status` là 2,
nên ước lượng N/2 cho giá trị chiếm 98% bảng, chọn index, chậm **7x** so với quét (1337 vs 190ms,
`reallab -work stats`). Và có một cách trả **rẻ hơn histogram**: *index dive*. Với khoảng quét
trên cột có index, đi xuống cây và đếm số khoá trong `Span` bằng cursor có sẵn. MySQL/MariaDB
nhờ cách này mà không bị thống kê cũ lừa, trong khi Postgres (chỉ tin `pg_statistic`) ước lượng
1 hàng cho 100000 hàng thật.

### 📏 P7-7 · `DefaultCost` là ba con số ĐOÁN, và đã biết sai vì HAI lý do độc lập

```go
var DefaultCost = CostModel{CSeq: 1, CIndex: 0.6, CFetch: 20}
```

Hai lý do sai, **độc lập với nhau**:

1. **Không có I/O thật** ở quy mô này ⇒ `CFetch/CSeq` đo được **4.5x**, không phải 20x (lệch 4.4x).
2. **Không tính tương quan** ⇒ ngay cả hằng số đo được cũng lệch **1.6-2.2x**, vì `CFetch` đo bằng
   khóa nhảy lung tung còn index scan tra bảng **theo thứ tự index**.

```bash
make idxlab-breakeven    # cột planner(đoán) và planner(đo) đều có dòng CHỌN SAI
```

**Cố ý giữ nguyên hằng số sai** — nó là cột `planner(đoán)` của bảng deliverable, và một cột cho
thấy hằng số sai làm planner chọn sai ở dải nào thì đúng ở **mọi** máy, còn một hằng số đúng thì
chỉ đúng ở máy này. Trả bằng: đo lúc mở database rồi lưu vào catalog — tức `ANALYZE`, và lúc đó
phải lưu **cả hai** `CFetch` cùng một hệ số tương quan cho từng index (đúng `indexCorrelation` của
Postgres).

### 🔧 P7-8 · Bất biến của workload chuyển tiền là ĐỐI XỨNG nên gần như mù

Phát hiện khi truy một bài test đỏ 1/6 lần của phase 6. "Tổng số dư không đổi" là bất biến đối
xứng ⇒ hai lost update **triệt tiêu** nhau và tổng lại đúng. Với `accounts=2` (đối xứng tối đa),
tổng **đúng ở 5/6 lần chạy**.

```bash
go run ./cmd/txnlab -work transfer -accounts 2 -workers 12 -ops 100
```

**Trả bằng:** thêm một bất biến **không đối xứng** — ví dụ *không tài khoản nào được âm* — rồi đo
lại sức phát hiện của từng mức isolation. Kỳ vọng: mức thấp vỡ ở **mọi** lần chạy thay vì 1/6, và
lúc đó `maxTries` ở `TestTransferInvariantPerLevel` bỏ được.

## Nợ của phase 8

### ⏳ P8-1 · Không có `BEGIN`/`COMMIT` — mỗi câu là một transaction riêng

**Món nợ lớn nhất của phase 8**, và nó đau vì cái thiếu không phải cơ chế mà chỉ là **mặt ngoài**:
phase 6 đã có 4 mức isolation, khóa điểm và khoảng, phát hiện deadlock qua wait-for graph — nhưng
**không viết được** một transaction nhiều câu **bằng SQL**. Cả `internal/txn` chỉ dùng được từ Go.

**Cách trả:** một mô hình **phiên** (session) giữ `*txn.Txn` giữa hai lần gọi, cộng ba câu lệnh
(`BEGIN [ISOLATION LEVEL ...]`, `COMMIT`, `ROLLBACK`). Có vậy mới viết được bằng SQL bài chuyển
tiền của `cmd/txnlab` — và đó mới là bằng chứng đã trả.

### ⏳ P8-2 · Không có `UPDATE`/`DELETE`

`internal/txn` có đủ (`Put`/`Del` + chuỗi version); chỗ thiếu là parser và một toán tử **ghi**.
Chưa làm vì `DELETE` còn phải cập nhật **mọi** index phụ, nên bất biến hàng↔index của phase 7
phải được khẳng định lại ở đường đi mới — tức `fuzz-table` phải sinh cả `DELETE`, không chỉ `Put`.

### ⏳ P8-3 · Không có `NOT`, không có `IS NULL`

**Không phải "chưa làm" mà là "đã làm phần khó, thiếu phần dễ"** — và đó là lý do không ai phát
hiện ra: phần khó có test. Logic **ba giá trị** đã cài đúng ở `internal/plan/bind.go`
(`CmpExpr.Eval`, `LogicExpr.Eval`, bảng chân lý trong `TestThreeValuedTruthTable`), và `NOT`
**đã là** một token trong `internal/sql/token.go` — nhưng parser không có phép một toán tử, nên
**không gõ ra được từ SQL**.

```console
$ go run ./cmd/minidb -e "SELECT id FROM ev WHERE NOT (kind < 5);"
cú pháp: cần một giá trị hoặc tên cột, gặp "NOT"
```

`IS NULL` nặng hơn: nó là cách **duy nhất** tìm hàng có NULL, và hiện **không có cách nào** — vì
`WHERE x = NULL` cho 0 hàng (đúng theo chuẩn), còn `x < v` và `x >= v` thì **cùng** loại hàng NULL ra.

### ⏳ P8-4 · Chỉ join **hai** bảng, và chỉ **inner** join

`plan.BindSelect` báo lỗi **rõ ràng** khi có bảng thứ ba, thay vì làm sai. Ba bảng đòi **chọn thứ
tự join** — lập trình động trên tập con — và đó là một phase riêng.

Outer join **khó hơn nó trông**, vì nó làm **sai hai phép tối ưu đã có**: `plan.propagate` (suy ra
điều kiện qua phép bằng — chỉ đúng vì **mọi** hàng ra của inner join thoả `a.x = b.y`) và `push`
(đẩy điều kiện của vế giữ xuống dưới outer join là đổi kết quả). Nên trả món này **phải** kèm
hai bài test khẳng định hai phép ấy **không** chạy cho LEFT JOIN.

### ⏳ P8-5 · `numParts` cố định 32, không chia phần đệ quy

Grace hash join thật chia lại phần nào **vẫn** không vừa RAM. Bản này không, nên mọi kết luận của
bảng số 2 trong `cmd/sqllab` là **có điều kiện** trên chỗ này. Đã ghi rõ tại `internal/exec/join.go`.

**Cách trả:** đệ quy trên phần, cộng một bài test dựng dữ liệu **lệch nặng** (một giá trị khóa
chiếm 90%) để một phần **chắc chắn** tràn — không có bài đó thì phép đệ quy không bao giờ chạy.

### ⏳ P8-6 · Không có plan cache, dù đã **tự đo được** lý do phải có

```console
$ go run ./cmd/sqllab -work pipeline -rows 20000 -dim 200 -repeat 7
câu                                              lex(µs)    parse*      bind  optimize      plan    thi hành
SELECT id, city FROM ev WHERE id = 12345            0.94      1.09      0.96      0.11      2.10       1.82µs
```

Front-end (parse\* + bind + optimize + plan = 5.30µs) là **2.9x** phần thi hành (1.82µs) của một
truy vấn **điểm**, và nó lặp **y nguyên** mỗi lần chạy cùng câu ấy.

`engine.prepare` **đã tách riêng** (bind + optimize + plan, không chạm dữ liệu) nên chỗ để cắm cache
đã sẵn. Thiếu: khóa cache (câu đã chuẩn hoá + phiên bản catalog) và phép **vô hiệu hoá** khi
`CREATE INDEX` / `ANALYZE` chạy. **Bằng chứng phải có khi trả:** một bài test `CREATE INDEX` rồi
chạy lại **cùng** câu và đòi kế hoạch **đổi** — một plan cache không vô hiệu hoá đúng lúc tệ hơn
không có cache.

### ⏳ P8-7 · Không có tham số truy vấn (`?` / `$1`)

Đi liền P8-6: plan cache không có tham số thì chỉ ăn được với câu **giống nhau từng byte**, tức
gần như vô dụng ngoài benchmark. Cũng là điều kiện để nói chuyện được về SQL injection — hiện
cách duy nhất để truyền giá trị là nối chuỗi, và `quoteStr` (xem `internal/sql/ast.go`) chỉ lo
nửa **in ra**, không lo nửa **nhận vào từ người dùng**.

### ⏳ P8-8 · `CREATE INDEX` không nhận `DESC`

`internal/keys` đã hỗ trợ chiều sắp **bằng phép bù byte** từ phase 7, và `internal/exec/sort.go`
dùng nó. Chỗ thiếu chỉ là cú pháp và một cột trong catalog. Nên `ORDER BY x DESC` hiện **luôn**
phải sắp, dù cây đã có sẵn thứ tự ngược — tức mất đúng cái **3284x** mà bảng số 5 đo được cho
`LIMIT 10`.

### ⏳ P8-9 · `os.Remove` ngay sau `os.CreateTemp` không chạy trên Windows

**Cố ý.** Mẹo này là cách rẻ nhất để file tạm của spill **không sống sót** một lần crash, và cả repo
đã `pread`/`pwrite`/`posix_fadvise` — tức đã chỉ chạy Linux từ phase 0. Ghi lại để không ai tưởng
đó là sơ sót.

### ⏳ P8-10 · `LIMIT` chưa đẩy được xuống dưới `Join`

`LIMIT` trên một `Scan` thì dừng đúng lúc — đó là chỗ ăn **3284-3345x** ở bảng số 5 — nhưng qua một
`Join` thì nó chỉ cắt ở **trên**. Với nested loop thì đẩy xuống được; với hash join thì **không**,
vì vế build phải rút cạn trước — một ví dụ nữa của *"toán tử CHẶN phải báo rằng nó chặn"*.

### ⏳ P8-11 · Chưa có `GROUP BY`, `HAVING`, hàm tổng hợp, `DISTINCT`

Ngoài phạm vi roadmap phase 8. Ghi lại vì **hash aggregate dùng đúng cơ chế tràn đĩa của hash
join**, nên nó là món **rẻ nhất** còn lại trong danh sách này — và nó sẽ tái sử dụng được ngay
`internal/exec/spill.go`.

### ⏳ P8-12 · `bench-txn` gộp `Get|Scan` dưới một `-benchtime`

Lỗi của **bộ đo**, phát hiện ở phase 8: `-benchtime` tính **theo phép toán**, và `BenchmarkScan`
quét **2000 khóa mỗi phép**. Nên `-benchtime=200000x` là **1.2 tỉ** bước khóa — đúng cho
`BenchmarkGet`, vô lý cho `BenchmarkScan`. Chưa sửa vì sửa là chạm vào deliverable của phase 6.

**Cách trả:** tách thành hai target, hoặc để `-benchtime` theo **thời gian** thay vì theo số lần.

---

## Nợ của phase 9

### 📏 P9-1 · Seq scan của minidb đắt gấp 15 lần Postgres cho mỗi hàng

```console
$ cd reallab && go run . -work breakeven -repeat 9      # rồi tách phí mỗi hàng, xem diary/phase9.md
                    quét/hàng ns  tra/hàng ns  tra/quét    hoà vốn ≈ quét/tra
minidb (phase 7)             480         2142      4.5x                22.4%
postgres 17                   31          725     23.5x                 4.3%
mysql 8.4                    113         2060     18.3x                 5.5%
```

Phí **tra** của minidb ngang InnoDB, phí **quét** thì đắt gấp 4x InnoDB và 15x Postgres. Đây là lý
do thật của điểm hoà vốn 36.8% (không phải "không có I/O" như phase 7 viết). **Trả bằng:** một
bench quét của `internal/txn` tách hai phần `keys.Decode` (P7-1) và `DecodeChain` (P6-2), rồi sửa
phần lớn hơn. **Bằng chứng phải có:** `idxlab -work breakeven` cho điểm hoà vốn tụt về dưới 15%.

### ⏳ P9-2 · `txnlab` chưa có ô "RR kiểu MySQL"

MySQL ở repeatable-read đọc bằng snapshot nhưng `UPDATE` trên bản **mới nhất**, nên để lọt lost
update (`reallab -work anomaly`). minidb chỉ có một kiểu RR (snapshot isolation thật). Thêm một
mức `RepeatableReadCurrentWrite` sẽ tái tạo được ô đó bằng chính code của minidb.

### 📏 P9-3 · Chưa đo UUID trên Postgres khi index PK lớn hơn RAM

Ở 2 triệu hàng, index PK của Postgres (80MB) vẫn vừa `shared_buffers`. **Trả bằng:** chạy
`reallab -work pkorder -db pg` với container giới hạn `--memory` (cgroup tính cả page cache) và
`shared_buffers` nhỏ, hoặc tăng số hàng tới khi index > RAM. **Kỳ vọng viết trước:** tỉ số ghi
page tiến về phía InnoDB.

### 📏 P9-4 · Purge của MariaDB nhanh hơn MySQL 40x

`reallab -work bloat`: sau khi phiên cũ đóng, MySQL mất 8s để history list về 0, MariaDB mất 200ms,
cùng 1000 transaction × 1000 hàng. Chưa biết do số luồng purge, do cách MariaDB viết lại purge
từ 10.6, hay do cách đo (vòng chờ 100ms).

### 🔧 P9-5 · Cột `ms` của `reallab -work stats` là một lần chạy

Không làm nóng, không lấy trung vị: chỉ dùng được để xem thứ tự độ lớn. **Trả bằng:** gọi
`estimate` hai lần, lấy lần sau (hoặc trung vị của 5 lần).

### 📏 P9-7 · Hash join tràn đĩa nhanh hơn trong RAM (Postgres)

`blog/lab/10-explain-pg.sql` mục 6: 16 batch 268–291ms, 1 batch 344–366ms. **Đã trả một nửa trên
WSL2** (diary/phase9.md, bảng 8): page fault do glibc trả bộ nhớ cho OS chỉ chiếm ~20ms. Phần
chính là 67–98ns mỗi hàng probe, tăng tuyến tính theo số hàng probe, khớp độ trễ một lần trượt
xuống RAM đo bằng microbenchmark. Bằng chứng mới gián tiếp: WSL2 không có PMU nên không đếm được
cache miss.

**Còn lại, trả trên Linux thuần** (⏱ ~10 phút, lần đầu nạp dữ liệu ~1 phút):

```bash
sudo apt install -y linux-tools-$(uname -r) linux-tools-generic
./scripts/p97-hashjoin.sh            # REPEAT=21 nếu máy vẫn nhiễu
```

Kết quả nằm ở `bench/p97/<host>-<ngày>/`. Đọc bảng E: H1 đúng thì cột 256MB có ≥ 1 lần
`LLC-load-misses` hoặc `dTLB-load-misses` trên mỗi hàng probe, cột 1MB thấp hơn hẳn. Nếu hai cột
bằng nhau thì H1 sai, và 70–95ns kia đến từ chỗ khác (số lệnh, rẽ nhánh: xem IPC =
instructions/cycles). Sau đó mới xem minidb có hưởng được điều này không (ở phase 8, minidb tràn
đĩa đắt 2.1x).

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
| ⏳ P6-4 · `Txn.Scan` materialize cả kết quả | `db.Iter` (latch chỉ giữ **một bước** `Next()`) + `Txn.Scan` viết lại thành **merge join** của hai dòng đã sắp: cursor trên cây, và bản sao đã sắp của phần write set trong khoảng. Cùng khóa thì write set thắng (*read-your-own-writes*) | `BenchmarkScanLimit` 1423-1668 ns vs quét cả bảng ~9.9 ms = **5032x** (`go run ./cmd/idxlab -work stream`). Bản materialize cho tỉ số **1x** vì nó đọc cả khoảng trước khi gọi `fn` lần đầu. Và món nợ này hoá ra **chặn cả phase 7**: index scan là một phép truy cây **trong** callback của một phép duyệt cây, nên với `Range` cũ nó **tự khoá chết** — `TestIterCallbackCanReadBack` là bài test của đúng hình đó |
| ⏳ P7-9 · Chưa có `ORDER BY` dùng index, chưa có `Filter` đẩy xuống | Thuộc tính vật lý bắt buộc (`req []SortKey`) **đi XUỐNG** qua `plan.build`, và `scan()` liệt kê thêm một đường "dùng index CHỈ để lấy thứ tự"; `Filter` đẩy xuống bằng `plan.opt.push` — cộng một luật **không có trong kế hoạch**: `plan.propagate` suy ra điều kiện qua phép bằng của join | `go run ./cmd/sqllab -work order`: bỏ được `Sort` = **2.87-3.12x** không LIMIT, nhưng **3284-3345x** với `LIMIT 10` — một **bậc**, không phải một hệ số, vì `Sort` là toán tử **CHẶN**. Và `-work pushdown`: **83.86x** (một bảng, điều kiện thành khoảng quét) / **45.35x** (thu vế build của join) / **18.53x** (suy ra qua phép bằng). Dòng thứ ba **trước** khi có `propagate` chỉ đo được **1.18x**, và chính con số ấy chỉ ra luật còn thiếu — lần đầu trong 8 phase số đo tìm ra thứ **CHƯA CÓ**. Món này còn tố oan tôi một lần: bản đầu hạ **mọi** khoảng thành `Filter` khi đường đi là seq scan (trực giác đúng với Postgres, nơi bảng là **heap**; sai ở đây, nơi hàng nằm **trong** cây pk) — **5100 hàng thay vì 105, 4.062ms thay vì 88µs = 46x**, và không test nào bắt được vì kết quả vẫn đúng |
| 🔧 P3-0 · `victim()` quay vô hạn khi WAL rule chặn mọi ứng viên | đếm số lần bị chặn, hết một vòng frame thì `ErrNoFrame` | `go test -run TestWALRule -timeout 10s` trước khi sửa: `panic: test timed out after 10s`; sau khi sửa: PASS, `store.Writes = 0` |
| 📏 P9-6 · MySQL `=0` + `sync_binlog=0` chỉ mất 6 commit khi `kill -9`, MariaDB mất 9123 | `reallab -work lograte`: đọc bộ đếm redo mỗi 5ms để đo khoảng giữa hai lần write(); thêm chế độ `innodb_log_writer_threads=OFF` vào `-work crash` | MySQL write() mỗi ≤6ms (luồng `log_writer`), MariaDB mỗi 1002ms. Tắt `log_writer` thì mất **3447** thay vì **6** (575x), khớp dự báo 767–844 mỗi lần kill. Và nó **tố oan một lần**: `Innodb_os_log_written` của MariaDB đếm LSN chứ không đếm byte đã ghi, nên lượt đầu báo MariaDB "ghi mỗi 5.6ms" |

---

## Quy tắc

1. **Nợ 🔧 phải được viết thành test fail trước khi sửa.** Không có test đỏ thì không biết mình
   đã sửa cái gì. P1-4 là ví dụ: test in ra đúng câu *"Commit báo THÀNH CÔNG dù..."* — đó mới là
   bằng chứng, không phải lời hứa.
2. **Nợ 📏 không được đoán.** Viết tỉ số kỳ vọng ra trước, đo, rồi so. Lệch xa thì **nghi bench
   sai trước**, đừng nghi máy lạ.
3. **Trả xong thì chuyển xuống bảng "Đã trả" kèm bằng chứng**, và tick checkbox trong diary của
   phase tương ứng.
