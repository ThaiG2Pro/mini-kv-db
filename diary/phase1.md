# Phase 1 — Pager: file như một mảng page

- **Thời lượng dự kiến:** 1-2 ngày · **thực tế:** ~1 buổi
- **Bắt đầu:** 2026-08-31 · **Kết thúc:** 2026-08-31
- **Trạng thái:** ✅ xong
- **Commit:** _(repo đã `git init` nhưng chưa có commit nào — số đo dưới đây gắn với
  cây làm việc ngày 2026-08-31; commit đầu tiên sẽ ghi hash vào đây)_

> **Quy tắc ghi nhật ký:** mọi con số, mọi kết luận đều phải kèm **lệnh shell sinh ra nó**
> và **output thật** (dán nguyên, không tóm tắt). Sáu tháng sau đọc lại phải chạy lại được.
> Ghi trong lúc làm, không phải sau khi xong.

## Môi trường

```console
$ uname -srmo && go version && df -hT . | tail -1
Linux 6.6.87.2-microsoft-standard-WSL2 x86_64 GNU/Linux
go version go1.26.2 linux/amd64
/dev/sdd       ext4 1007G   38G  918G   4% /
```

CPU: `12th Gen Intel(R) Core(TM) i5-1235U` (theo header của `go test -bench`).

⚠️ **Vẫn là WSL2** (ext4 trên VHD trên NTFS). Như phase 0 đã chứng minh, số tuyệt đối ở đây
dao động tới 2.7x giữa các lần chạy. Mọi kết luận dưới đây **chốt bằng tỉ số**.

## Mục tiêu phase

Biến file thành mảng page 4KB có meta page, checksum và freelist. Đạt atomicity ở mức
"chuyển trạng thái file" mà **chưa cần WAL**.

## Câu hỏi phải trả lời được khi xong

- Vì sao dùng `pread`/`pwrite` chứ không `Seek`+`Read`?
- Hai meta page ghi luân phiên giải quyết vấn đề gì? Chọn meta nào khi mở file?
- Checksum bảo vệ khỏi loại lỗi nào, và **không** bảo vệ khỏi loại nào?
- Freelist lưu ở đâu, ai được phép tái sử dụng page vừa free?

## Deliverable (bằng chứng đã hiểu)

1. `TestCrashAtEveryWriteIsRecoverable` — mô phỏng mất điện tại **lời ghi thứ 1..10** × **4 kiểu
   ghi dở** (0 / 20 / 1000 / 4095 byte) = **40 điểm crash**. Mở lại phải luôn thành công và trạng
   thái chỉ được là **một trong hai**: commit cũ nguyên vẹn, hoặc commit mới trọn vẹn.
2. Demo bằng mắt: lật **1 byte** trong meta page mới nhất bằng `dd` → mở lại tự động rollback.

## Reproduce toàn bộ phase này

```bash
go vet ./...
go test ./internal/pager -count=1 -v                       # 10 test + 40 ca crash
go test ./internal/pager -bench . -benchtime=200x -run XXX  # giá của fsync

# nhìn tận mắt file trên đĩa
go run ./cmd/pagerlab -db data/test.db -commits 8 -alloc 4 -free 3
xxd -l 48 data/test.db            # meta page A
xxd -s 4096 -l 48 data/test.db    # meta page B

# demo rollback: lật 1 byte ở trường root của meta page mới nhất
printf '\xff' | dd of=data/test.db bs=1 seek=4108 count=1 conv=notrunc status=none
go run ./cmd/pagerlab -db data/test.db -commits 0 -fresh=false
```

---

## Nhật ký

### 2026-08-31 — dựng pager, và bug freelist tự trỏ vào chính nó

**Làm gì:** viết `internal/pager/pager.go` (meta page kép + crc32c + freelist theo chuỗi page),
rồi viết test **trước khi tin là nó đúng**. Lần chạy đầu:

```console
$ go test ./internal/pager -count=1
--- FAIL: TestFreelistSurvivesReopen (0.01s)
    pager_test.go:346: sau reopen FreeCount=30, trước khi đóng=29
--- FAIL: TestFreelistOverflowChain (0.01s)
    pager_test.go:403: sau reopen FreeCount=1072, muốn 1070
FAIL
FAIL	minidb/internal/pager	0.065s
```

**Đọc kết quả:** lệch đúng **bằng số page dùng để chứa freelist** (1 page ở test đầu, 2 page ở
test overflow). Không phải lỗi đếm — nó nói rằng **page chứa freelist đang nằm trong chính danh
sách free mà nó ghi ra**.

Nguyên nhân: `writeFreelist()` của tôi copy `p.free` ra rồi mới `Allocate()` page để chứa, mà
`Allocate()` lại **pop từ `p.free`**. Nên id vừa bị lấy làm "host" vẫn còn trong bản copy đã ghi
xuống đĩa.

**Đây là lỗi nghiêm trọng, không phải lỗi thẩm mỹ:** commit sau đọc freelist, cấp lại đúng page
đó, rồi **ghi đè lên freelist page mà meta hợp lệ hiện tại đang trỏ tới**. Crash lúc đó → rollback
về một meta trỏ tới freelist đã bị phá → mất luôn cả đích rollback. Toàn bộ tính chất mà phase
này tồn tại để có, bị chính freelist phá.

**Đã sửa:** cấp host **trước**, lặp vì mỗi lần `Allocate()` lại làm `p.free` ngắn đi nên số host
cần thiết phải tính lại:

```go
for {
    need := (len(p.free) + freelistPerPage - 1) / freelistPerPage
    if need <= len(hosts) { break }
    h, err := p.Allocate()   // có thể pop từ chính p.free
    ...
    hosts = append(hosts, h)
}
```

```console
$ go test ./internal/pager -count=1
ok  	minidb/internal/pager	0.371s
```

**Đang nghĩ gì:** bug này chỉ lộ ra vì test *reopen rồi so lại số đếm*. Nếu chỉ test "free rồi
allocate thấy tái dùng" thì nó **pass** và tôi mang mầm mất dữ liệu sang phase 4. Bài học: test
phải đi qua **đĩa** và qua **lần mở lại**, không chỉ qua trạng thái trong RAM.

### 2026-08-31 — crash tại mọi điểm ghi

Bổ sung `countingFlakey`: cho `n-1` lời ghi đầu đi qua, lời ghi thứ `n` **chỉ ghi được một phần**
rồi lỗi, mọi lời ghi sau đó bị bỏ, `Sync()` cũng lỗi. Đây là mô hình xấu nhất của một lần mất điện.

```console
$ go test ./internal/pager -count=1 -v 2>&1 | grep -E '^(=== RUN|--- )' | grep -v '/'
=== RUN   TestOpenCreatesTwoValidMetas
--- PASS: TestOpenCreatesTwoValidMetas (0.00s)
=== RUN   TestCommitAlternatesMetaPage
--- PASS: TestCommitAlternatesMetaPage (0.01s)
=== RUN   TestRollbackWhenMetaWriteLost
--- PASS: TestRollbackWhenMetaWriteLost (0.01s)
=== RUN   TestRollbackWhenMetaTorn
--- PASS: TestRollbackWhenMetaTorn (0.00s)
=== RUN   TestBothMetaCorruptIsError
--- PASS: TestBothMetaCorruptIsError (0.00s)
=== RUN   TestFreelistReusesPages
--- PASS: TestFreelistReusesPages (0.01s)
=== RUN   TestFreelistSurvivesReopen
--- PASS: TestFreelistSurvivesReopen (0.01s)
=== RUN   TestFreelistOverflowChain
--- PASS: TestFreelistOverflowChain (0.01s)
=== RUN   TestMetaPagesAreWriteProtected
--- PASS: TestMetaPagesAreWriteProtected (0.00s)
=== RUN   TestConcurrentReadAtIsSafe
--- PASS: TestConcurrentReadAtIsSafe (0.01s)

$ go test ./internal/pager -run TestCrashAtEveryWriteIsRecoverable -count=1 -v 2>&1 | grep -cE '^    --- PASS'
40
```

**Đọc kết quả:** 40/40 điểm crash đều mở lại được, và `txnID` sau khi mở lại **luôn** là `oldTxn`
hoặc `oldTxn+1` — không có trạng thái thứ ba. Đó chính là định nghĩa atomicity ở mức file.

**Đang nghĩ gì:** `kill -9` (phase 0 đã kết luận) không tạo được lỗi này, nên chèn lỗi ở tầng
`File` interface là cách **duy nhất** test được đường rollback trong unit test. Việc kiểm chứng
với thiết bị thật vẫn nằm ở `scripts/dm-flakey.sh` (nợ #1 của phase 0).

### 2026-08-31 — nhìn tận mắt: meta page luân phiên, page được tái dùng

```console
$ go run ./cmd/pagerlab -db data/test.db -commits 8 -alloc 4 -free 3
mở data/test.db: txnID=1 pageCount=2 root=0 freelistHead=0
một freelist page chứa được 1022 PageID

txn   metaPage  root   pageCount  phình   freeList free   pending  tái dùng
2     0         5      6          +4      4        2      1        0 page lấy từ freelist | free 2 free 3 free 4
3     1         7      8          +2      2        3      1        2 page lấy từ freelist | free 5 free 3 free 2
4     0         8      9          +1      3        3      1        3 page lấy từ freelist | free 6 free 7 free 3
5     1         9      10         +1      8        3      1        3 page lấy từ freelist | free 5 free 4 free 8
6     0         10     11         +1      2        3      1        3 page lấy từ freelist | free 7 free 6 free 2
7     1         11     12         +1      5        3      1        3 page lấy từ freelist | free 9 free 4 free 5
8     0         12     13         +1      6        3      1        3 page lấy từ freelist | free 3 free 10 free 6
9     1         13     14         +1      11       3      1        3 page lấy từ freelist | free 7 free 8 free 11

meta page 0: txnID=8    root=12   freelist=6    pageCount=13   crc32c=0x5a2940ec
meta page 1: txnID=9    root=13   freelist=11   pageCount=14   crc32c=0x83d4646a
-> chênh txnID giữa hai meta page phải là 1: cái cũ chính là đích rollback

freelist trên đĩa: 1 page trong chuỗi, 3 PageID rỗng: [6 7 8]
file: 57344 byte = 14 page
```

**Đọc kết quả:** ba điều đọc được ngay từ bảng này:

1. cột `metaPage` chạy `0,1,0,1,...` — đúng `txnID % 2`, và hai meta page ở cuối chênh nhau
   **đúng 1 txn**. Cái cũ (txn 8) là đích rollback.
2. cấp 4 – free 3 → về trạng thái dừng file chỉ **phình +1 page/commit**, đúng bằng phần dữ liệu
   thật tăng thêm. Freelist đang làm việc của nó.
3. `pageCount` trong meta (13, 14) **nhỏ hơn** số page thật của file — vì `Allocate()` nới file
   ngay nhưng commit có thể chưa dùng hết. Không sai, nhưng là chỗ sinh page mồ côi (xem nợ #1).

**Rồi soi thẳng byte trên đĩa:**

```console
$ xxd -l 48 data/test.db
00000000: 01db dbd1 0100 0000 0010 0000 0c00 0000  ................
00000010: 0600 0000 0800 0000 0000 0000 0d00 0000  ................
00000020: ec40 295a 0000 0000 0000 0000 0000 0000  .@)Z............
```

Đọc từng trường (little-endian): magic `d1dbdb01` · version `1` · pageSize `0x1000 = 4096` ·
root `0x0c = 12` · freelist `6` · txnID `8` · pageCount `13` · crc32c `0x5a2940ec`.
Khớp đúng dòng "meta page 0" mà `pagerlab` in ra. Toàn bộ meta nằm trong **36 byte đầu**.

### 2026-08-31 — rollback bằng 1 byte

```console
$ printf '\xff' | dd of=data/test.db bs=1 seek=4108 count=1 conv=notrunc status=none
$ go run ./cmd/pagerlab -db data/test.db -commits 0 -fresh=false
mở data/test.db: txnID=8 pageCount=13 root=12 freelistHead=6
...
meta page 0: txnID=8    root=12   freelist=6    pageCount=13   crc32c=0x5a2940ec
meta page 1: KHÔNG HỢP LỆ (checksum sai: file=0x83d4646a tính lại=0x50e7c2a3)
```

**Đọc kết quả:** offset `4108 = 4096 + 12` = trường `root` của meta page B. Lật **một** byte ở đó
→ checksum không khớp → meta B bị loại → pager mở ra ở txn **8** thay vì 9. Rollback không phải
một hàm nào cả; nó là **hệ quả của việc chọn meta**.

**Đang nghĩ gì:** để ý con số `file: 57344 byte = 14 page` trong khi `pageCount=13`. Sau khi
rollback, page mà txn 9 đã cấp trở thành **mồ côi**: không meta nào trỏ tới, cũng không nằm trong
freelist. Đây là rò rỉ dung lượng thật (BoltDB cũng có, và cũng chỉ thu hồi khi nới lại file).
Ghi vào nợ kỹ thuật.

---

## Giả thuyết sai / bug đã gặp

| Tôi tưởng là | Thực tế là | Lệnh / output đã lật tẩy nó | Đã sửa thế nào |
|---|---|---|---|
| Serialize freelist rồi cấp page để chứa nó — thứ tự nào cũng được | Page chứa freelist bị ghi vào chính danh sách free của nó → commit sau ghi đè freelist page mà meta hợp lệ đang trỏ tới | `go test ./internal/pager` → `sau reopen FreeCount=30, trước khi đóng=29` (lệch đúng bằng số host page) | `writeFreelist()` cấp host **trước**, lặp lại phép tính `need` sau mỗi `Allocate()` vì `p.free` ngắn dần |
| Ghi dở meta page ở byte nào cũng bị checksum bắt | Toàn bộ meta nằm trong **36 byte đầu**; ghi dở từ byte 36 trở đi thì checksum vẫn khớp (và vô hại, vì phần sau là 0) | `xxd -l 48 data/test.db` — mọi trường có nghĩa nằm trong `0x00..0x23` | Không sửa code; **sửa test**: `tornPrefix = 20` để chắc chắn đi vào nhánh checksum sai. Ghi nhận: với ổ có sector 512B, torn write *bên trong* meta gần như không xảy ra — checksum ở đây chủ yếu chống bit rot / misdirected write |
| Cột "file KHÔNG phình" của `pagerlab` chứng minh freelist hoạt động | Tôi chụp `pageCount` **sau** khi đã `Allocate()` xong, nên nó gần như luôn báo "không phình" — một cột luôn đúng thì không chứng minh gì | Bảng in ra `pageCount` tăng 6→8→9 mà vẫn kèm "file KHÔNG phình" | Chụp `pageCount` ở **đầu** vòng lặp, in cột `phình = +N` và `số page lấy từ freelist` |
| Cấp phát LIFO làm page nhảy lung tung → ghi ngẫu nhiên → `fsync` đắt hơn (phase 0 đo 4x) | **Sai.** LIFO và lowest-first cho ra **cùng** `page-gap = 1.875` — cả hai đều ghi tuần tự, chỉ khác chiều. Vì freelist luôn được sort trước khi ghi xuống đĩa nên nó không bao giờ lởm chởm | `go test -bench BenchmarkAlloc -benchtime=2000x` → `BenchmarkAllocLIFO 1.875 page-gap` / `BenchmarkAllocLowest 1.875 page-gap` | Không sửa code. Đổi lại kết luận: nguồn ghi ngẫu nhiên thật sẽ là **phân mảnh do B+Tree** (phase 4), không phải chính sách cấp phát. Giữ cả 2 chính sách để đo lại ở đó |
| Bench đầu tiên của tôi so được hai chính sách | Workload cấp 8 – free 7 nên file **cứ phình**, gần như không lấy page nào từ freelist → hai chính sách chạy trên cùng một đường code | Cả hai đều ra `226.6 page-gap`, giống nhau tới chữ số thập phân đầu — dấu hiệu bench không chạm vào thứ cần đo | Thêm warm-up: cấp 4096 page, commit, free gần hết, commit — rồi mới churn cấp 8/free 8 để tổng số page không đổi |
| `Commit` có 2 `fsync` nên tốn ~2× một `fsync` | Tốn ~1.2× (1.374ms vs 1.135ms của phase 0) — `fsync` **thứ hai** gần như miễn phí vì gần như không còn dữ liệu bẩn | `go test -bench BenchmarkCommit` vs bảng `write+fsync` p50 ở `diary/phase0.md` | Không sửa code; sửa lại kỳ vọng. Cái đắt là **có** một `fsync` ở đó, không phải số lượng |

## Số đo

**Lệnh:** `go test ./internal/pager -bench . -benchtime=200x -run XXX -count=1` ·
**ngày:** 2026-08-31 · **máy:** WSL2 ext4-trên-VHD, i5-1235U · **commit:** _(chưa commit)_

```console
goos: linux
goarch: amd64
pkg: minidb/internal/pager
cpu: 12th Gen Intel(R) Core(TM) i5-1235U
BenchmarkCommit-6          	     200	   1373722 ns/op
BenchmarkReadPage-6        	     200	       329.4 ns/op
BenchmarkCommitNoSync-6    	     200	      2852 ns/op
BenchmarkCommitBatch64-6   	     200	   2048422 ns/op	     32005 ns/page
```

Tỉ số cần nhớ (tỉ số bền hơn số tuyệt đối):

| Tỉ số | Giá trị | Ý nghĩa |
|---|---|---|
| `Commit` / `CommitNoSync` | 1373722 / 2852 = **481x** | Đây là **giá của durability**, không phải giá của việc ghi. Toàn bộ phần còn lại của pager (encode meta, crc32c, serialize freelist, `pwrite`) cộng lại chỉ chiếm 1/481 |
| `Commit` / `ReadPage` (cache nóng) | 1373722 / 329 = **~4200x** | Một commit đắt bằng ~4000 lần đọc page nóng. Mọi thiết kế sau này phải **gộp commit**, không gộp đọc |
| `Commit` / `write+fsync` p50 phase 0 | 1373722 / 1135000 = **1.2x** | 2 `fsync` mà chỉ đắt hơn 1 `fsync` 20% → `fsync` thứ hai gần như miễn phí (ít dữ liệu bẩn). Cái đắt là **lần chạm đĩa đầu tiên** |
| `Commit` / `Batch64` mỗi page | 1373722 / 32005 = **43x** | Gộp 64 page vào 1 commit làm mỗi page rẻ đi 43 lần. Cùng một quy luật với group commit ở phase 0 (140x) — chỉ khác tầng |

**Chính sách cấp phát** (`go test ./internal/pager -bench BenchmarkAlloc -benchtime=2000x -run XXX`,
cùng ngày/máy; `NoSync=true` để tách layout khỏi `fsync`):

```console
BenchmarkAllocLIFO-6     	    2000	     65840 ns/op	         1.875 page-gap	         0 page-phình
BenchmarkAllocLowest-6   	    2000	    262037 ns/op	         1.875 page-gap	         0 page-phình
```

`page-gap` = khoảng cách trung bình giữa hai page được ghi liên tiếp. **Bằng nhau** → giả thuyết
"LIFO gây ghi ngẫu nhiên" **sai**. Chênh lệch CPU 4x là lỗi cài đặt của tôi chứ không phải bản
chất: `AllocLowest` cắt đầu slice (`p.free = p.free[1:]`) nên `append` sau đó phải copy lại cả
~4000 phần tử mỗi commit. Không tối ưu vội — theo tỉ số 481x ở trên, CPU trong pager là thứ
không đáng tối ưu.

**Kết luận thiết kế mang sang phase sau:** `Commit()` là **đơn vị đắt duy nhất** trong pager.
Mọi lớp bên trên (buffer pool phase 3, B+Tree phase 4, WAL phase 5) phải coi nó là tài nguyên
khan hiếm: gom nhiều thay đổi rồi commit một lần, chứ không commit mỗi thao tác.

## Invariant tôi đã cài và lệnh kiểm chứng nó

| Invariant | Cài ở đâu (file:hàm) | Lệnh kiểm chứng | Kết quả |
|---|---|---|---|
| Luôn tồn tại ≥1 meta page hợp lệ, kể cả ngay sau khi tạo file | `internal/pager/pager.go:initFile` (ghi cả 2 meta, txnID 0 và 1) | `go test ./internal/pager -run TestOpenCreatesTwoValidMetas -v` | PASS |
| Commit txn N ghi vào meta page `N%2`; page còn lại giữ txn `N-1` (đích rollback) | `pager.go:Commit` + `pager.go:MetaPageOf` | `go test ./internal/pager -run TestCommitAlternatesMetaPage -v` | PASS |
| Meta sai checksum bị loại; chọn txnID lớn nhất **trong số hợp lệ** | `pager.go:pickMeta`, `pager.go:decodeMeta` | `go test ./internal/pager -run 'TestRollbackWhenMeta' -v` | PASS (cả 2 ca: mất lời ghi, và ghi dở) |
| Cả 2 meta hỏng → báo lỗi, **không** đoán bừa | `pager.go:pickMeta` (`ErrCorrupt`) | `go test ./internal/pager -run TestBothMetaCorruptIsError -v` | PASS |
| Page `Free` trong txn N không được cấp lại trước khi commit N xong | `pager.go:Free` (vào `pending`), `pager.go:Commit` (`pending`→`free` ở đầu commit sau) | `go test ./internal/pager -run TestFreelistReusesPages -v` | PASS |
| Freelist page **không bao giờ** tự nằm trong danh sách free mà nó ghi | `pager.go:writeFreelist` (cấp host trước) | `go test ./internal/pager -run 'TestFreelist' -v` | PASS (3 test) |
| Không ai ghi trực tiếp lên meta page ngoài `Commit` | `pager.go:WritePage`, `pager.go:Free` (`ErrMetaPageBusy`) | `go test ./internal/pager -run TestMetaPagesAreWriteProtected -v` | PASS |
| Crash ở bất kỳ lời ghi nào → mở lại được, và chỉ có 2 trạng thái hợp lệ | toàn bộ thứ tự trong `pager.go:Commit` | `go test ./internal/pager -run TestCrashAtEveryWriteIsRecoverable -v` | PASS 40/40 |
| Chuỗi freelist không có vòng lặp, `count` không vượt `freelistPerPage` | `internal/pager/inspect.go:InspectFreelist`, `pager.go:loadFreelist` | được gọi ở cuối mỗi ca crash test | PASS |

## Đọc gì

- **BoltDB** `db.go` / `freelist.go` / `tx.go` — nguồn của toàn bộ ý tưởng meta page kép. Bolt
  cũng có `pending` (nó gọi là `freelist.pending` theo txid), và cũng vì lý do y hệt.
- **Database Internals** ch.3 (File formats) — layout page, checksum, tại sao slot/offset chứ
  không phải struct cố định (dùng ở phase 2).
- `man 2 pread`, `man 2 fsync` — `pread` không đụng vào file offset dùng chung.
- Lại đọc `diary/phase0.md` để so số `Commit` với `write+fsync` p50. Đây là lần đầu số của phase
  trước **dùng để kiểm tra** số của phase sau — đúng như mục đích của nhật ký.

## Rút ra (viết như thể giải thích cho người khác)

**1. Vì sao `pread`/`pwrite` chứ không `Seek`+`Read`.** File offset không thuộc file descriptor
mà thuộc *file description* — thứ được chia sẻ. Hai goroutine cùng `Seek` rồi `Read` sẽ chen vào
giữa nhau: goroutine A seek tới page 10, goroutine B seek tới page 99, A đọc → A nhận page 99.
`pread` mang offset đi kèm ngay trong lời gọi nên không có trạng thái chia sẻ nào để đua.
`TestConcurrentReadAtIsSafe` chạy 64 goroutine × 200 lượt đọc để chốt điều đó. Trong Go,
`f.ReadAt`/`f.WriteAt` chính là `pread`/`pwrite`.

**2. Hai meta page giải quyết vấn đề gì.** Vấn đề: khi bạn ghi đè trạng thái cũ bằng trạng thái
mới, có một khoảng thời gian mà trên đĩa **không tồn tại trạng thái nào đúng**. Mất điện trong
khoảng đó là mất database. Cách thoát: đừng bao giờ ghi đè trạng thái đang có hiệu lực — ghi
trạng thái mới sang **chỗ khác**, rồi mới lật công tắc. Ở đây "chỗ khác" là meta page còn lại, và
"lật công tắc" là `fsync` cuối cùng. Khi mở file, chọn meta có `txnID` lớn nhất **trong số các
meta hợp lệ**. Không cần hàm rollback nào: rollback chính là kết quả của phép chọn đó.

**3. Thứ tự trong `Commit` LÀ tính đúng đắn, không phải chi tiết cài đặt.** Ghi data → `fsync` →
ghi meta → `fsync`. Bỏ `fsync` đầu là cho phép đĩa hoàn thành lời ghi meta **trước** lời ghi data
(nó có toàn quyền sắp xếp lại) → sinh ra một meta hợp lệ trỏ tới dữ liệu rác: hỏng **âm thầm**,
tệ hơn hẳn crash. Đó là cùng một luật sẽ xuất hiện ở phase 5 dưới tên WAL rule
(`flushedLSN >= pageLSN`): **cái được trỏ tới phải durable trước cái trỏ**.

**4. Checksum chống được gì và không chống được gì.** Chống: ghi dở nửa page, bit rot, misdirected
write, file bị cắt ngắn — tức là mọi thứ làm byte **khác** với lúc ghi. Không chống: một meta page
được ghi **trọn vẹn và đúng checksum** nhưng nội dung sai logic (bug của tôi, không phải của đĩa);
cũng không chống mất lời ghi mà thiết bị **báo là đã ghi** (`dm-flakey drop_writes` — nợ #1 phase
0) — trường hợp đó meta cũ vẫn đúng nên ta rollback, nhưng người dùng đã được báo "commit xong".
Và một điều tôi chỉ thấy khi `xxd`: cả meta gói trong **36 byte**, tức nằm trong **một sector
512B**. Với ổ nguyên tử ở mức sector, meta của tôi hoặc xuống trọn hoặc không xuống gì —
torn write bên trong meta gần như không thể xảy ra. Checksum ở đây rẻ, nên vẫn giữ, nhưng phải
biết nó đang canh loại lỗi nào.

**5. Freelist và câu hỏi "ai được tái dùng page vừa free".** Freelist nằm **trong chính file**,
thành chuỗi page (1022 `PageID`/page), head lưu trong meta — nên nó tự động được bảo vệ bởi đúng
cơ chế atomicity ở trên. Câu trả lời cho "ai được tái dùng": **không phải txn hiện tại**. Page vừa
`Free` trong txn N vẫn đang được meta N-1 trỏ tới, mà meta N-1 chính là chỗ ta sẽ rollback về nếu
commit N chết. Ghi đè nó = phá đích rollback. Nên page đi vào `pending`, và chỉ nhập vào `free` ở
**đầu commit kế tiếp** — lúc đó đích rollback đã là meta N, meta N-1 không còn ai cần. Bug tôi
dính hôm nay (freelist page tự nằm trong danh sách free của nó) là **đúng cùng một sai lầm** ở
dạng khác: ghi đè một page mà trạng thái hợp lệ vẫn đang trỏ tới.

**6. `fsync` vẫn là toàn bộ cái giá.** 481x giữa `Commit` và `CommitNoSync`. Nghĩa là mọi tối ưu
CPU trong pager (crc32c nhanh hơn, encode gọn hơn, sort freelist) đều **vô nghĩa về hiệu năng** —
chúng chỉ có giá trị về tính đúng đắn. Chỗ duy nhất đáng tối ưu là **số lần chạm đĩa**, và cách
duy nhất là gộp (43x khi gộp 64 page). Đây là bài học của phase 0 lặp lại ở tầng cao hơn, và nó
sẽ còn lặp lại ở phase 5.

## Nợ kỹ thuật / để dành cho sau

Sổ nợ đầy đủ (cả phase 0 lẫn phase 1, kèm lệnh trả từng món): [`docs/debts.md`](../docs/debts.md).

- [x] **P1-4 · `WriteAt` ghi thiếu byte mà không báo lỗi.** POSIX cho phép `write()` trả về
      `n < len(p)` với `err == nil`. Pager của tôi bỏ qua `n` → commit một page ghi dở rồi tưởng
      là thành công. Test viết ra đã đỏ ngay:
      `pager_test.go:773: Commit báo THÀNH CÔNG dù lời ghi chỉ đi được 4095/4096 byte`.
      Sửa bằng `pager.go:writeFull`.
      *Ghi chú:* ca `keep=4095` **không** rollback, và điều đó là đúng — 36 byte đầu đã đủ tạo
      một meta hợp lệ, phần sau là padding vốn bằng 0. Chỉ ca `keep=20` mới rollback. Tôi đã
      viết assertion sai lần đầu và phải sửa **test**, không phải code.
- [x] **P1-5 · Không có cách kiểm tra file từ bên ngoài.** Đã có `pager.Verify()` + `cmd/dbcheck`
      (`make check`). Nó bắt: double free, freelist tự trỏ vào chính nó (**đúng cái bug tôi dính
      hôm nay**), meta page bị liệt kê là rỗng, chuỗi freelist có vòng lặp, file cắt giữa page,
      và rò rỉ đuôi file.
- [ ] **P1-1 · Page mồ côi sau rollback.** Giờ đã *phát hiện được bằng lệnh* thay vì chỉ nghi ngờ:
      ```console
      $ go run ./cmd/dbcheck data/test.db
        ⚠ RÒ RỈ ĐUÔI FILE: 1 page nằm ngoài pageCount=13 — page mồ côi sau rollback
      ```
      Cách trả: lúc `Open` so `pageCount` với kích thước file thật, phần dư đưa vào freelist hoặc
      `Truncate`. Trả xong thì `dbcheck` phải im.
- [ ] **P1-2 · Chưa có transaction thật** (`Begin`/`Rollback`, chặn hai writer). Phase 6.
- [ ] **P1-3 · Chính sách cấp phát** — đã đo, giả thuyết ban đầu sai (xem bảng trên). Đo lại ở
      phase 4 khi B+Tree tạo ra phân mảnh thật, trên đĩa thật.
- [ ] **P1-6 · Chạy lại bench phase 1 trên Linux thuần** cùng lúc với nợ P0-4: tỉ số 481x và 43x
      có sống sót qua máy khác không?
- [ ] `PageSize` là hằng số biên dịch nhưng meta lại lưu và kiểm `pageSize` trong file. Đúng cho
      hiện tại; muốn đổi page size thì phải bỏ hằng số đi.
