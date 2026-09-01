# Phase 2 — Slotted page

- **Thời lượng thực tế:** 1 buổi
- **Bắt đầu:** 2026-09-01 · **Kết thúc:** 2026-09-01
- **Trạng thái:** ✅ xong
- **Commit:** `cf06483` — mọi số đo trong file này thuộc về cây làm việc của commit đó

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

CPU (Go tự khai trong output bench): `12th Gen Intel(R) Core(TM) i5-1235U`, `GOMAXPROCS=6`.

## Mục tiêu phase

Chứa record **biến độ dài** trong một page 4096 byte cố định: mảng slot mọc từ trái, cell data
mọc từ phải, có `Compact()`. Và hiểu vì sao *phải* có tầng gián tiếp slot.

## Câu hỏi phải trả lời được khi xong

1. Vì sao cần slot indirection thay vì trỏ thẳng vào offset?
2. Sau `compact()` thì tuple id có đổi không? Vì sao điều đó quan trọng với secondary index?
3. Fragmentation phát sinh từ đâu, khi nào thì đáng compact?
4. Liên hệ: vì sao Postgres cần VACUUM?

## Deliverable (bằng chứng đã hiểu)

- **1 421 899 lần chạy fuzz** trên chuỗi thao tác ngẫu nhiên, kiểm tra bất biến sau **mỗi**
  thao tác — không lần nào vỡ.
- Property test 200 000 thao tác có seed, đối chiếu từng byte với model trong RAM.
- Hai verifier độc lập (bitmap và sort) phải luôn đồng ý — 30 000 thao tác.
- `cmd/slotlab`: nhìn bằng mắt vùng trống vỡ vụn rồi liền lại.

## Reproduce toàn bộ phase này

```bash
git clone <repo> && cd db
go test ./internal/page/ -v -count=1                      # 15 test, ~6s
go test ./internal/page/ -run '^$' -bench . -benchmem -benchtime 2000x
go test ./internal/page/ -run '^$' -fuzz FuzzSlottedPage -fuzztime 120s -fuzzminimizetime 1s
go run ./cmd/slotlab -n 24 -size 120                      # chèn/xóa/compact, có bản đồ page
go run ./cmd/slotlab -n 24 -size 120 -delete random -seed 7 -churn 4
```

`-fuzzminimizetime 1s` **không** phải chi tiết vặt — xem mục "giả thuyết sai" #4.

---

## Nhật ký

### 2026-09-01 — dựng layout, test cơ bản

Chốt layout trước khi viết dòng nào:

```
 0                                                              4096
 +--------+------------------+--------------+--------------------+
 | header | slot0 slot1 ...  |  ...trống... | ... cell1  cell0   |
 +--------+------------------+--------------+--------------------+
 24 byte   4 byte/slot        FreeContiguous  cellStart -> 4096
```

Header 24 byte: `numSlots, numDead, cellStart, frag` (mỗi cái uint16), `type`, và **`pageLSN`
uint64 chừa sẵn cho phase 5** — WAL rule `flushedLSN >= pageLSN` cần chỗ này, đục lỗ sau sẽ
phải đổi format file.

Ba quyết định có hệ quả, ghi lại vì về sau dễ quên mất là *đã cân nhắc*:

1. **`Page` là `[]byte`, không phải struct.** Nó bọc thẳng buffer của pager: không copy, không
   giữ trạng thái nào ngoài cái đã nằm trên đĩa. Phase 3 (buffer pool) sẽ đưa buffer đó vào
   frame, tầng page không cần biết.
2. **Slot chết đánh dấu bằng `offset == 0`**, vì offset 0 nằm trong header nên không thể là
   cell hợp lệ. Nhờ vậy record rỗng (`len == 0`) vẫn là record hợp lệ.
3. **`Insert` không bao giờ tái dùng slot chết.** Tái dùng nghĩa là một tuple id cũ — có thể
   còn nằm trong secondary index — bỗng trỏ sang record khác. Đây chính xác là thứ Postgres
   phải chạy VACUUM xong mới dám làm.

```console
$ go test ./internal/page/ -v -count=1 2>&1 | head -20
=== RUN   TestInitEmptyPage
--- PASS: TestInitEmptyPage (0.00s)
=== RUN   TestInsertGetDelete
--- PASS: TestInsertGetDelete (0.00s)
=== RUN   TestSlotIDStableAcrossCompact
    page_test.go:135: 10/10 cell sống đã đổi offset, 0 SlotID đổi
--- PASS: TestSlotIDStableAcrossCompact (0.00s)
```

**Đọc kết quả:** dòng `10/10 cell sống đã đổi offset, 0 SlotID đổi` là toàn bộ phase 2 gói
trong một câu. Test tự thất bại nếu `moved == 0` — nếu không cell nào bị dời thì nó chẳng
chứng minh được gì.

**Đang nghĩ gì:** page đầy được bao nhiêu record? Đo luôn thay vì đoán.

```console
$ go test ./internal/page/ -run 'TestPageFullIsReal|TestNeedCompact' -v -count=1
    page_test.go:181: liền mạch=32 tổng=3632 frag=3600
--- PASS: TestNeedCompactThenInsertSucceeds (0.00s)
    page_test.go:208: nhét được 59 record 64B; dùng 4036/4096 byte, còn trống 60
--- PASS: TestPageFullIsReal (0.00s)
```

**Đọc kết quả:** 59 record 64B = 3776 byte data + 236 byte slot + 24 byte header = 4036, còn
thừa 60 byte (không đủ cho record thứ 60 vì cần 64+4). **Chi phí slot là 4/68 = 5.9%** với
record 64B. Với record 16B thì slot chiếm 20% page — đây là lý do B+Tree không lưu record bé
tí một cách vô tư, và là lý do phase 4 sẽ phải nghĩ về key size.

Dòng thứ nhất là tình huống trung tâm: **trống tổng 3632 nhưng trống liền mạch chỉ 32**. Một
page "còn 88% trống" vẫn có thể từ chối một record 400 byte. Đó là fragmentation, cụ thể và
đo được, không phải khái niệm.

### 2026-09-01 — nhìn bằng mắt: `cmd/slotlab`

Số thì thuyết phục, nhưng phân mảnh là thứ nên *nhìn*. Viết `slotlab` in bản đồ page (`H`
header, `s` slot array, `.` trống, `#` cell sống, `x` byte chết):

```console
$ go run ./cmd/slotlab -n 24 -size 120
== SAU KHI CHÈN 24 RECORD 120B ==
trống: liền mạch 1096, tổng 1096  -> phân mảnh 0.0%
[0Hs.................#############################################4096]

== SAU KHI XÓA (even) — chú ý: trống TỔNG lớn, trống LIỀN MẠCH thì không ==
trống: liền mạch 1096, tổng 2536  -> phân mảnh 56.8%
[0Hs.................##x###x###x###x##x###x###x###x##x###x###x###x4096]

== SAU COMPACT — frag về 0, mọi SlotID giữ nguyên ==
trống: liền mạch 2536, tổng 2536  -> phân mảnh 0.0%
[0Hs.......................................#######################4096]

kiểm chứng: 12 SlotID cũ vẫn đọc ra đúng record cũ sau khi cell bị dời
Verify: mọi bất biến còn nguyên
```

**Đọc kết quả:** hàng `x` xen kẽ `#` chính là fragmentation. Sau compact, 1440 byte cell sống
nằm liền một khối và **2536 byte trống về một chỗ** — nhưng slot 1, 3, 5... vẫn là slot 1, 3,
5. Cột `off=` trong bản đầy đủ đổi từ 3856 → 3976; `SlotID` không đổi.

**Đang nghĩ gì:** compact dọn được vùng cell. Nhưng còn *mảng slot*? Nó chỉ mọc dài ra.
Chạy vòng xóa-rồi-chèn-lại xem nó phình tới đâu.

```console
$ go run ./cmd/slotlab -n 24 -size 120 -delete random -seed 7 -churn 4
churn  1: slot=50 (sống 32) liền mạch=  32 frag=   0
churn  2: slot=64 (sống 31) liền mạch=  96 frag=   0
churn  3: slot=77 (sống 31) liền mạch=  44 frag=   0
churn  4: slot=91 (sống 30) liền mạch= 108 frag=   0
== SAU CHURN — mảng slot chỉ mọc thêm, không co lại ==
type=1 numSlots=91 (sống 30, chết 61)  cellStart=496
header 24 | slot array 364 | trống liền mạch 108 | frag 0 | cell sống 3600  (tổng 4096)
```

**Đọc kết quả:** 4 vòng churn → mảng slot từ 24 lên **91 slot, trong đó 61 chết**. 61 × 4 =
**244 byte (6% page) là con trỏ tới hư vô**. Compact không đụng tới chúng, vì đụng vào là
dịch SlotID.

Đây đúng là *line pointer bloat* của Postgres, và nó trả lời câu hỏi 4 rõ hơn mọi bài blog:
page prune (≈ `Compact` ở đây) dọn được **cell**, nhưng chỉ có VACUUM — cái biết chắc **không
index nào còn trỏ tới** — mới dám dọn **line pointer**.

Cái *duy nhất* dọn được mà không cần biết gì về index là các slot chết ở **đuôi** mảng: một
con trỏ ngoài trỏ vào slot đã cắt sẽ rơi vào nhánh `id >= numSlots` và nhận câu trả lời
"không còn record" — đúng bằng câu trả lời trước khi cắt. Đó là `TrimDeadSlots()`, và
`TestTrimDeadSlotsOnlyAtTail` khoá chặt việc nó **không** được cắt slot chết ở giữa.

### 2026-09-01 — property test và fuzz: chỗ mọi thứ vỡ ra

Viết `Verify()` kiểm 6 bất biến, gọi sau **mỗi** thao tác trong property test 200k bước:

```console
$ go test ./internal/page/ -run TestRandomOpsKeepInvariants -v -count=1
    page_test.go:405: 200000 thao tác: map[compact:10045 delete:39407 insert:39416 insert-full:88385 trim:38666 update:8325 update-full:11239]
    page_test.go:406: cuối cùng: 750 slot (9 sống), trống liền mạch 141, frag 19
--- PASS: TestRandomOpsKeepInvariants (0.67s)
```

**Đọc kết quả:** `insert-full: 88385` — 44% thao tác bị từ chối vì page đầy. Không phải lỗi:
workload này chạy ở trạng thái bão hoà, tức là đúng chỗ dễ vỡ nhất. Kết cục `750 slot / 9
sống` lại là bloat ở trên, lần này do máy sinh ra chứ không do tôi dựng.

Trước khi tin `Verify`, phải chứng minh nó **có răng**: bẻ tay một slot cho chồng lấn.

```console
$ go test ./internal/page/ -run TestVerifyCatchesOverlap -v -count=1
    page_test.go:312: Verify bắt được: page: page hỏng (I4): byte 3996 thuộc hai cell, một trong đó là slot 1 [3896,4046)
    page_test.go:320: Verify bắt được: page: page hỏng (I5): frag header=1, tính lại=0 (vùng cell 200 byte, sống 200 byte)
```

Rồi mới bật fuzz. Và fuzz là chỗ buổi làm việc này thật sự bắt đầu — bốn giả thuyết sai liên
tiếp, ghi ở bảng dưới.

### 2026-09-01 — chốt

```console
$ go test ./... -count=1
?   	minidb/cmd/iolab	[no test files]
?   	minidb/cmd/pagerlab	[no test files]
?   	minidb/cmd/slotlab	[no test files]
?   	minidb/cmd/tornlab	[no test files]
ok  	minidb/internal/page	6.633s
ok  	minidb/internal/pager	0.853s

$ go test ./internal/page/ -run '^$' -fuzz FuzzSlottedPage -fuzztime 120s -fuzzminimizetime 1s
fuzz: elapsed: 2m0s, execs: 1421899 (19697/sec), new interesting: 113 (total: 255)
PASS
ok  	minidb/internal/page	120.160s
```

---

## Giả thuyết sai / bug đã gặp

| Tôi tưởng là | Thực tế là | Lệnh + output đã lật tẩy | Đã sửa thế nào |
|---|---|---|---|
| Trong `Compact`, offset của cell gần như đã giảm dần theo chỉ số slot, nên insertion sort là đủ | `Update` cấp cell mới ở mép trái mà **giữ nguyên slot**, nên chỉ vài lần update là hai thứ tự lệch hoàn toàn. Dựng được thế 299/300 nghịch thế | `go test -run TestCompactOrderIsScrambled -v` → `300 cell sống, 299 chỗ offset KHÔNG giảm dần theo slot`; `-bench CompactScrambled -benchtime 500x` → `229810 ns/op` | Thay bằng `slices.SortFunc` trên mảng `uint32` gói `(offset, slot)` nằm trên **stack** → `9169 ns/op`, **25x**, 0 alloc |
| Fuzzer đứng hình 48s ⇒ code có **vòng lặp vô hạn** | Không có vòng lặp nào. 3000 chương trình ngẫu nhiên chạy hết trong 6.3s | `go test -run TestProgramStress -timeout 40s` → `ok 6.326s` | Tách thân fuzz thành `runProgram()` dùng chung cho fuzz và stress test có seed — giờ mọi nghi vấn về fuzz đều kiểm tra được bằng một test thường |
| Đứng hình ⇒ **môi trường WSL2** hỏng | Fuzz một hàm rỗng trên đúng máy đó chạy 76 780 exec/s, không hề khựng | thí nghiệm đối chứng: `go test -fuzz FuzzNothing -fuzztime 20s` → `1423015 execs (76780/sec)` | Loại bỏ giả thuyết môi trường, quay lại nghi chính mình |
| Đứng hình ⇒ một input **rất dài** | Worker chạy **106% CPU** liên tục — nó đang *minimize* một input mới. Ngân sách mặc định `-fuzzminimizetime` là **60s**, và `-fuzztime` hết trước khi minimize xong nên Go in ra... **PASS** | `ps -o pcpu` trên worker → `106`; `kill -QUIT` → `fuzzing process hung or terminated unexpectedly while minimizing: EOF`; rồi `-fuzzminimizetime 1s` → hết khựng, `128182 execs / 45s` | Luôn chạy fuzz với `-fuzzminimizetime 1s`, và ghi vào Makefile. **Bài học nặng nhất buổi này: một chữ `PASS` từ `-fuzz` khi worker đang khựng KHÔNG phải bằng chứng gì cả** |
| `Verify` chỉ là hàm kiểm tra, viết sao cũng được | Nó chạy sau **mỗi** thao tác nên giá của nó nhân với toàn bộ chiều dài fuzz. Bản dùng `sort.Slice` tốn 20 576 B và 4 alloc mỗi lần gọi | `-bench Verify -benchmem -count=3` → `VerifyRefFullPage 19768 ns/op 20576 B/op` vs `VerifyFullPage 3583 ns/op 0 B/op` | Đổi sang bitmap 512 byte trên stack: **5.5x nhanh hơn, 0 cấp phát**. Giữ lại bản cũ làm `verifyRef` để (a) kiểm tra chéo và (b) tỉ số này còn tái lập được về sau |
| Test "ghi thiếu 1 byte thì hỏng" kiểu phase 1 áp dụng được ở đây | Không liên quan: page này chưa có checksum riêng. Tính toàn vẹn của nó **thừa hưởng** từ atomicity của pager (phase 1) — page chỉ tồn tại khi meta trỏ tới nó | `TestPageRoundTripsThroughPager` → `file 12288 byte, root=page 2, 33 record sống qua được reopen` | Không thêm checksum ở tầng page. Ghi vào nợ P2-3: khi có buffer pool + WAL, chỗ đặt checksum sẽ khác |

Cả bốn dòng đầu là **cùng một triệu chứng** ("fuzzer đứng im") với bốn nguyên nhân giả định
khác nhau. Ba cái đầu sai. Thứ tự chẩn đoán đã đưa tới đáp án: *đo giá thật* → *dựng lại bằng
test thường* → *thí nghiệm đối chứng* → *nhìn thẳng vào tiến trình*.

---

## Số đo

Máy: WSL2 / i5-1235U / ext4 trên /dev/sdd. Ngày 2026-09-01. Commit `cf06483`.

```console
$ go test ./internal/page/ -run '^$' -bench . -benchmem -benchtime 2000x -count=1
goos: linux
goarch: amd64
pkg: minidb/internal/page
cpu: 12th Gen Intel(R) Core(TM) i5-1235U
BenchmarkInsert-6             	    2000	       107.4 ns/op	       2 B/op	       0 allocs/op
BenchmarkGet-6                	    2000	         6.005 ns/op	       0 B/op	       0 allocs/op
BenchmarkCompact-6            	    2000	      2055 ns/op	1992.95 MB/s	        29.00 cell-sống	       0 B/op	       0 allocs/op
BenchmarkCompactScrambled-6   	    2000	      8469 ns/op	       300.0 cell-sống	       0 B/op	       0 allocs/op
BenchmarkVerifyFullPage-6     	    2000	      3459 ns/op	       814.0 slot	       0 B/op	       0 allocs/op
```

Kiểm tra độ ổn định (benchtime mặc định, 3 lần) — các số dao động dưới 12%, trừ `VerifyRef`
lần thứ ba nhảy 43% (nhiễu của WSL2, đã gặp ở phase 0):

```console
$ go test ./internal/page/ -run '^$' -bench 'Verify' -benchmem -count=3
BenchmarkVerifyFullPage-6      	  318538	      3583 ns/op	       814.0 slot	       0 B/op	       0 allocs/op
BenchmarkVerifyFullPage-6      	  417914	      3721 ns/op	       814.0 slot	       0 B/op	       0 allocs/op
BenchmarkVerifyFullPage-6      	  291291	      3730 ns/op	       814.0 slot	       0 B/op	       0 allocs/op
BenchmarkVerifyRefFullPage-6   	   66211	     19768 ns/op	       814.0 slot	   20576 B/op	       4 allocs/op
BenchmarkVerifyRefFullPage-6   	   53694	     20159 ns/op	       814.0 slot	   20576 B/op	       4 allocs/op
BenchmarkVerifyRefFullPage-6   	   52720	     28799 ns/op	       814.0 slot	   20576 B/op	       4 allocs/op
```

### Bảng tỉ số (phần đáng nhớ)

| Tỉ số | Giá trị | Ý nghĩa thiết kế |
|---|---|---|
| `Insert` / `Get` | 107.4 / 6.005 = **18x** | Đọc là copy con trỏ; ghi phải sửa header + slot + chép cell |
| `pager.ReadPage` / `page.Get` | 329.4 / 6.005 = **55x** | Lấy page ra khỏi đĩa đắt hơn *mọi* thao tác bên trong page ⇒ phase 3 (buffer pool) mua được nhiều hơn mọi tối ưu vi mô ở tầng này |
| `pager.Commit` / `page.Compact` | 1 373 722 / 2055 = **668x** | **Compact gần như miễn phí so với một fsync.** Đừng bao giờ tiếc compact để tránh ghi thêm — trong khi ngược lại, tránh được một page ghi ra đĩa thì đáng giá |
| `Compact` insertion sort / `slices.SortFunc` | 229 810 / 9169 = **25x** | Thế xấu nhất (299/300 nghịch thế) do `Update` sinh ra, không phải trường hợp giả tưởng |
| `verifyRef` / `Verify` | 19 768 / 3583 = **5.5x**, và 20 576 B → **0 B** | Checker chạy sau mỗi thao tác thì tốc độ của nó *là* độ phủ của fuzz |
| Chi phí slot, record 64B | 4 / 68 = **5.9%** | Với record 16B con số này thành 20% ⇒ ràng buộc thật lên kích thước key ở phase 4 |
| Slot chết sau 4 vòng churn | 61/91 slot = **244 B = 6% page** | Chính là line pointer bloat ⇒ vì sao Postgres cần VACUUM chứ không chỉ page prune |
| Fuzz | **1 421 899 exec / 120s**, 0 lần vỡ bất biến | Với `-fuzzminimizetime 1s`; không có cờ này thì chỉ đạt ~63k và một chữ PASS vô nghĩa |

---

## Invariant + lệnh kiểm chứng

| Invariant | Cài ở | Lệnh kiểm chứng | Kết quả |
|---|---|---|---|
| I1 — mảng slot và vùng cell không bao giờ chồng nhau (`slotEnd <= cellStart`) | `internal/page/verify.go:Verify` | `go test ./internal/page/ -run TestRandomOpsKeepInvariants` | PASS, 200k thao tác |
| I2 — `numDead <= numSlots` | `verify.go:Verify` | như trên | PASS |
| I3 — mọi cell sống nằm trong `[cellStart, 4096)` | `verify.go:Verify` | như trên | PASS |
| I4 — không hai cell sống nào chồng lấn | `verify.go:Verify` (bitmap) | `go test -run TestVerifyCatchesOverlap -v` | bắt được: `byte 3996 thuộc hai cell` |
| I5 — `frag == (4096 - cellStart) - Σ len(cell sống)` | `verify.go:Verify` | `go test -run TestVerifyCatchesOverlap -v` | bắt được: `frag header=1, tính lại=0` |
| I6 — số slot có `offset == 0` đúng bằng `numDead` | `verify.go:Verify` | `TestRandomOpsKeepInvariants` | PASS |
| Verify nhanh và Verify tham chiếu luôn đồng ý | `page_test.go:verifyRef` | `go test -run TestVerifyAgreesWithReference` | PASS, 30k thao tác |
| **SlotID không đổi khi `Compact` dời cell** | `page.go:Compact` (không đụng mảng slot) | `go test -run TestSlotIDStableAcrossCompact -v` | `10/10 cell sống đã đổi offset, 0 SlotID đổi` |
| **`Delete` không rút mảng slot** | `page.go:Delete` | `go test -run TestDeleteDoesNotShiftSlots` | PASS |
| **`Insert` không tái dùng slot chết** | `page.go:InsertNoCompact` | `TestDeleteDoesNotShiftSlots` (đòi id mới = 5) | PASS |
| **`TrimDeadSlots` chỉ cắt ở đuôi** | `page.go:TrimDeadSlots` | `go test -run TestTrimDeadSlotsOnlyAtTail` | PASS: cắt 2, giữ 1 slot chết ở giữa |
| `Update` giữ nguyên tuple id kể cả khi record to ra | `page.go:Update` | `go test -run TestUpdateKeepsSlotID` | PASS, `numSlots` vẫn 2 |
| Page qua pager rồi đọc lại vẫn hợp lệ | `page_test.go:TestPageRoundTripsThroughPager` | `go test -run TestPageRoundTripsThroughPager -v` | `33 record sống qua được reopen` |
| `Compact` không cấp phát heap | `page.go:Compact` (mảng trên stack) | `-bench Compact -benchmem` | `0 B/op 0 allocs/op` |

## Đọc gì trong phase này

- **Database Internals** (Petrov), ch. 3 "File Formats" — mục *Slotted Pages* và *Cell Layout*.
- Mã nguồn SQLite, `btreeInt.h`: SQLite gọi vùng này là *cell content area* và có `nFree` +
  danh sách freeblock **bên trong** page — tức là nó tái dùng lỗ hổng mà không cần compact
  toàn bộ. Bản của tôi đơn giản hơn: chỉ có `frag` tổng, muốn dùng lại thì compact hết.
  (Nợ P2-2.)
- Postgres `src/backend/storage/page/bufpage.c`: `PageRepairFragmentation` = `Compact()` ở
  đây; `ItemIdData` = slot; và chỗ nó **không** dám rút mảng `ItemId` chính là lý do có VACUUM.
- `go doc testing.F` + `go help testflag` cho `-fuzzminimizetime` — cái mà tôi lẽ ra nên đọc
  *trước* khi mất một giờ nghi oan cho WSL2.

---

## Rút ra

**1. Vì sao cần slot indirection thay vì trỏ thẳng vào offset?**
Vì offset là thứ *phải* thay đổi, còn con trỏ từ bên ngoài thì *không được phép* thay đổi.
Ngay khi page cần dồn lại chỗ trống, mọi record đều dịch chỗ — đo được: `10/10 cell sống đã
đổi offset`. Nếu secondary index lưu `(PageID, offset)` thì một lần compact làm hỏng toàn bộ
index. Slot là một mức gián tiếp **nằm trong chính page**, nên page tự dọn dẹp được mà không
phải báo cho ai. Cái giá là 4 byte/record (5.9% với record 64B) và một lần đọc bộ nhớ thêm.

**2. Sau `compact()` thì tuple id có đổi không?**
Không, và đó là điều kiện để tuple id tồn tại. `tuple id = (PageID, SlotID)`: `PageID` do
pager giữ ổn định (phase 1), `SlotID` do page giữ ổn định (phase 2). Nhờ vậy secondary index
ở phase 7 chỉ cần lưu 6 byte và không bao giờ phải cập nhật khi heap tự dọn. Hệ quả ngược
lại cũng phải nhớ: *bất cứ thao tác nào làm dịch SlotID đều là thao tác phá index* — nên
`Delete` chỉ đánh dấu, `Insert` không tái dùng, và `TrimDeadSlots` chỉ dám đụng vào đuôi.

**3. Fragmentation phát sinh từ đâu, khi nào thì đáng compact?**
Từ việc **cell có độ dài khác nhau**: xóa một record 120B rồi chèn một record 400B thì lỗ
120B đó không dùng được. Đo được: sau khi xóa xen kẽ, `trống tổng 3632 / trống liền mạch 32`
— page còn 88% trống nhưng từ chối mọi record quá 32 byte.
Khi nào compact: câu trả lời không đến từ tầng page mà từ **bảng tỉ số**. `Commit` (2 lần
fsync) đắt gấp **668 lần** một `Compact`. Nên chính sách đúng là *compact ngay khi phần liền
mạch không đủ mà tổng thì đủ* — đó chính là nhánh `ErrNeedCompact` trong `Insert`. Tiếc 2µs
CPU để rồi phải cấp thêm một page và ghi thêm một page ra đĩa là một vụ đổi chác tệ hại.
Đây là lần đầu một quyết định thiết kế của tôi được rút ra **từ số của phase trước**, chứ
không phải từ trực giác.

**4. Vì sao Postgres cần VACUUM?**
Vì có hai loại rác trong một page, và chúng đòi hai mức hiểu biết khác nhau:
- **Rác trong vùng cell** (byte của record đã chết). Dọn được ngay tại chỗ, vì không ai ở
  ngoài biết offset của nó. Postgres gọi là *page prune*, ở đây là `Compact()`.
- **Rác trong mảng slot** (line pointer trỏ tới hư vô). Đo được ở trên: 4 vòng churn để lại
  **61 slot chết = 244 byte = 6% page**. Muốn dọn phải chứng minh **không index nào còn trỏ
  tới** slot đó — mà page thì không thể tự biết. Chỉ một tiến trình quét *toàn bộ* index mới
  biết: đó là VACUUM.

Còn hai thứ không nằm trong danh sách câu hỏi nhưng đắt giá hơn cả:

**5. Tốc độ của cái đi kiểm tra chính là độ phủ của cái được kiểm tra.**
`Verify` chạy sau mỗi thao tác. Bản đầu tốn 20KB cấp phát mỗi lần gọi, và nó — chứ không phải
code cần kiểm — mới là thứ giới hạn fuzz. Sửa xuống 0 alloc, fuzz đi từ 63k lên 1.42 triệu
exec. Một checker chậm không chỉ chạy chậm: nó **làm bạn nghĩ mình đã test kỹ** trong khi
thật ra chưa.

**6. `PASS` không đồng nghĩa với "đã chạy".**
`go test -fuzz` in ra `PASS` trong khi worker của nó đứng im 48/60 giây để minimize một input.
Nếu tôi chỉ nhìn dòng cuối, tôi đã ghi vào nhật ký này rằng "fuzz 60s không tìm ra lỗi" —
một câu đúng chữ, sai nghĩa hoàn toàn. Cột `execs/sec` mới là thứ phải nhìn. Cùng loại bẫy
với phase 0 (đo memcpy trong page cache mà tưởng đo đĩa): **luôn hỏi con số nào chứng minh
rằng thí nghiệm đã thật sự diễn ra.**

---

## Nợ kỹ thuật

Chi tiết + lệnh trả từng món nằm ở sổ nợ: [`../docs/debts.md`](../docs/debts.md).

- [ ] ⏳ **P2-1** — `Update` to ra quá chỗ trống chỉ trả `ErrPageFull`, chưa có forwarding pointer.
- [ ] 🔧 **P2-2** — chưa tái dùng được lỗ hổng nếu chưa compact (SQLite có freeblock list).
- [ ] ⏳ **P2-3** — page không có checksum riêng, đang dựa vào atomicity của meta page.
- [ ] ⏳ **P2-4** — `TrimDeadSlots` chưa được gọi tự động; churn để lại 6% page là slot chết.
- [ ] 📏 **P2-5** — mọi số ở trên là của WSL2, nhiễu tới 43%; chạy lại trên Linux thuần.

Hai món đã trả ngay trong phase này (`Compact` insertion sort, `Verify` cấp phát 20KB) nằm ở
bảng **Đã trả** của sổ nợ.
