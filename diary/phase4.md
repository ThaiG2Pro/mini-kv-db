# Phase 4 — B+Tree (linh hồn của DB)

- **Thời lượng dự kiến:** 4-6 ngày · **thực tế:** 1 ngày, 4 lượt (2 code · 1 chạy+sửa · 1 nhật ký)
- **Bắt đầu:** 2026-09-01 · **Kết thúc:** 2026-09-01
- **Trạng thái:** ✅ xong
- **Commit:** `_(điền sau khi commit — xem phase4-log.md phần 8)_`
- **Nhật ký lệnh đầy đủ:** [`phase4-log.md`](./phase4-log.md)

> **Quy tắc ghi nhật ký:** mọi con số, mọi kết luận đều phải kèm **lệnh shell sinh ra nó**
> và **output thật** (dán nguyên, không tóm tắt). Sáu tháng sau đọc lại phải chạy lại được.
> Ghi trong lúc làm, không phải sau khi xong.

## Môi trường

```console
$ rtk proxy bash -c 'uname -srmo && go version && df -hT . | tail -1 && nproc && grep -m1 "model name" /proc/cpuinfo'
Linux 6.6.87.2-microsoft-standard-WSL2 x86_64 GNU/Linux
go version go1.26.2 linux/amd64
/dev/sdd       ext4 1007G   39G  918G   5% /
6
model name	: 12th Gen Intel(R) Core(TM) i5-1235U

$ lscpu | grep -i cache
L1d cache:                            144 KiB (3 instances)
L1i cache:                            96 KiB (3 instances)
L2 cache:                             3.8 MiB (3 instances)
L3 cache:                             12 MiB (1 instance)
```

Kích thước L2/L3 hoá ra **không** phải chi tiết thừa — nó giải thích một số đo trông vô lý ở
mục "Giả thuyết sai" dòng G7b.

## Mục tiêu phase

Access method chính: search, insert + split, delete + merge/redistribute, và cursor cho range scan.

## Câu hỏi phải trả lời được khi xong

- Fanout trên page 4KB của tôi là bao nhiêu? Cây 3 tầng chứa bao nhiêu key? Lookup tốn mấy I/O?
- Vì sao B+Tree (data chỉ ở leaf) chứ không phải B-Tree?
- Cây tăng chiều cao lúc nào, vì sao nó luôn cân bằng?
- Delete: khi nào redistribute, khi nào merge? Vì sao merge khó hơn split?
- Right-most insert optimization là gì? Vì sao UUIDv4 làm primary key là thảm họa?

Trả lời hết ở mục [Rút ra](#rút-ra-viết-như-thể-giải-thích-cho-người-khác).

## Giả thuyết đăng ký TRƯỚC khi đo

Viết ra đây trước khi chạy `make bench-btree` một lần nào, để lát nữa không tự lừa mình.
Toàn bộ code của phase đã viết xong tại thời điểm ghi mục này; **chưa lệnh nào được chạy**
ngoài `go build` và `go vet`.

| # | Giả thuyết | Tỉ số kỳ vọng |
|---|---|---|
| G1 | Khóa 16 byte, page 4KB: leaf chứa ~30-35 khóa (giá trị 100B mới là thứ chiếm chỗ), branch chứa ~180-200 con | branch fanout / leaf fanout > 5x |
| G2 | Chèn ngẫu nhiên và chèn tăng dần sinh **cùng số leaf split** (mỗi leaf đầy đều phải tách một lần) — khác biệt nằm ở ĐỘ ĐẦY còn lại, không ở số lần tách | splits ngẫu nhiên / tăng dần ≈ 1.0-1.2x |
| G3 | Tối ưu cực phải: khóa tăng dần lấp lá gần 100%, ngẫu nhiên dừng ở ~70% (dãy lá sinh ra từ split 50/50 rồi lấp dần trở lại) | độ đầy tăng dần / ngẫu nhiên ≈ 1.4x, số page ≈ 0.7x |
| G4 | Với pool ĐỦ LỚN chứa cả cây, hai thứ tự chèn nhanh xấp xỉ nhau; chênh lệch thật chỉ hiện ra khi pool nhỏ hơn cây | ghi/khóa ngẫu nhiên / tăng dần > 10x ở pool 64 frame, ≈ 1x ở pool 1024 |
| G5 | Xóa 90% khóa **không** trả lại 90% page: merge chỉ chạy khi node tụt dưới 50%, nên file co chậm hơn dữ liệu rất nhiều | page còn lại / khóa còn lại > 2x |
| G6 | Bất biến ">= 50%" là tuyệt đối khi tắt `RightmostSplit`, trừ đúng số lần `SkippedRebalance` (khóa phân tách mới dài hơn khóa cũ, cha không đủ chỗ) | SkippedRebalance ≈ 0 với khóa cùng độ dài, > 0 với khóa biến độ dài |
| G7 | Một `Get` trên cây 3 tầng chạm đúng 3 page; với pool nhỏ thì reads/op tiến tới ~2 (tầng trên gần như luôn hit) | reads/op ở pool lạnh < 3 |

**Kết quả: 4/7 sai (G1, G2, G4, G5, G6).** Đó là phần đáng đọc nhất của file này.

## Deliverable (bằng chứng đã hiểu)

1) **Property test** (`TestPropertyRandomOps`): 60 000 thao tác Put/Delete trộn lẫn trên không
   gian khóa hẹp (8 000 khóa) để merge/redistribute thật sự chạy; cứ 2 000 thao tác gọi
   `Verify()` kiểm 7 bất biến B1-B7 và đối chiếu số khóa với một `map` tham chiếu; cuối cùng so
   từng khóa một. Chạy với `RightmostSplit=false`. Sau khi G6 bị bác, test tách thành **hai hồ
   sơ**: `value ngắn` (cell tối đa 25 byte — bất biến ">= 50%" là tuyệt đối, khẳng định cứng)
   và `value dài` (cell tối đa 374 byte — chỉ khẳng định được sàn B7 thật).
2) **Bench 1 triệu khóa** (`make bench-btree`): tăng dần vs ngẫu nhiên, cùng khóa 16 byte, cùng
   giá trị 100 byte — khác đúng một thứ là thứ tự. Báo cáo `splits/1k`, `%fill`, `pages/1k`,
   `writes/op`, `height`.
3) `cmd/btreelab`: bảng thứ tự chèn -> hình dạng file, bảng pool nhỏ dần, bảng fanout theo độ
   dài khóa, và bảng xóa dần -> cây trả lại bao nhiêu page.
4) Test tự phủ quyết: `TestRightmostSplitFillsPages` **FAIL** nếu tối ưu cực phải không thật sự
   làm lá đầy hơn và không thật sự giảm số page — không cho phép báo PASS rỗng.
5) `TestDeleteEverythingShrinksBack`: xóa hết 5 000 khóa theo thứ tự ngẫu nhiên thì cây phải
   tụt về đúng 1 leaf và **trả lại toàn bộ page trừ một**.
6) `TestTinyPool`: 20 000 khóa trên pool 6/8/10 frame — mọi chỗ quên `Unpin` đều nổ thành
   `ErrNoFrame` ở đây. **Đã nổ thật**, xem bug #1.
7) `TestRoundTripThroughPager`: cây đi qua file thật — ghi, `FlushAll`, `Commit(root)`, đóng,
   mở lại, đọc đủ.
8) Fuzz `FuzzTreeOps`: chuỗi Put/Delete tùy ý × {khóa ngắn, khóa dài} × {bật, tắt tối ưu},
   đối chiếu map + `Verify()` + quét cursor. **Đã bắt được bug #3**, thứ mà 7 test kia bỏ lọt.

## Reproduce toàn bộ phase này

```bash
go test ./internal/btree/ -run TestPropertyRandomOps -v -count=1
go test ./... -count=1                    # toàn repo
go test ./... -count=1 -race
make bench-btree        # 1 triệu khóa, tăng dần vs ngẫu nhiên, đếm split
make btreelab           # bảng hình dạng cây, fanout, xóa và trả page
make fuzz-btree         # nhớ -fuzzminimizetime, bài học phase 2
go test ./internal/btree/ -run '^$' -bench 'GetPool' -benchtime=200000x   # reads/op vs pool
go test ./internal/btree/ -run '^$' -bench 'Scan|SearchInPage' -benchtime=20x
go run ./cmd/btreelab -n 20000 -dump
```

---

## Nhật ký

### 2026-09-01 (lượt 1-2) — dựng cây (chưa chạy dòng nào)

**Làm gì:** viết trọn tầng access method trước khi chạy bất cứ thứ gì, để phần đo không bị
lẫn với phần sửa. Sáu quyết định thiết kế đáng ghi lại, tất cả đều là *chọn giữa hai đường*:

1. **Node dùng lại slotted page của phase 2**, không phát minh layout mới. Nhưng B+Tree cần
   mảng slot **chính là thứ tự khóa** (chèn thì dịch phải, xóa thì dịch trái) trong khi heap
   của phase 2 cần `SlotID` **ổn định** (tombstone, chỉ append) vì `SlotID` là nửa của một
   tuple id nằm trong index thứ cấp. Hai quy ước ngược nhau trên cùng một layout ->
   `internal/page/ordered.go` đặt cạnh phần cũ, có ghi rõ cái nào dùng cho ai.
2. **Quy ước khóa phân tách:** cell thứ i của branch là `(K_i, C_i)` với cây con `C_i` chứa
   khóa **< K_i**; con cực phải (không có cận trên) nằm ở trường `Link` của header. Nhờ vậy
   `setChildAt(numCells)` **chính là** "đặt con cực phải", và trường hợp đặc biệt "cực phải"
   biến mất khỏi cả split lẫn merge.
3. **Split và merge dùng chung một thân**: gom hết cell của node (và của anh em) vào một vùng
   nháp rồi dựng lại page, thay vì xáo cell tại chỗ. Split xảy ra cỡ 1 lần / 100 lần chèn nên
   ở đây rõ ràng quý hơn nhanh; đổi lại merge/redistribute chỉ còn một câu hỏi duy nhất:
   "gom hai bên lại có lọt một page không?".
4. **Đầy/thiếu tính theo BYTE** (`Used()*2 < PageSize`), không theo số cell — khóa dài ngắn
   khác nhau thì đếm cell là vô nghĩa.
5. **`NewPage` không đọc đĩa.** Nội dung cũ của page vừa cấp là rác. Bẫy đi kèm: pager tái
   dùng id từ freelist, nên id "mới" có thể vẫn còn một bản CŨ **và bẩn** trong pool — phải
   vứt bản đó đi, và đó là chỗ duy nhất trong pool được phép bỏ một dirty page.
6. **Kỷ luật pin:** đường đọc unpin từng tầng khi đi xuống; đường ghi giữ nguyên cả đường
   root->leaf (nên pool phải >= height+2 frame); cursor **không giữ pin nào** giữa hai lần
   `Next()` — nó chép khóa/giá trị ra ngoài. Giá phải trả là re-pin mỗi `Next()`, sẽ đo ở
   `BenchmarkScan`.

**Đọc kết quả:** chưa có số nào. `go build ./... && go vet ./...` sạch, nhưng đó **không phải**
bằng chứng gì cả — phase này chỉ đóng lại khi property test và bench nói chuyện.

**Đang nghĩ gì:** hai chỗ nghi ngờ nhất, đoán trước là sẽ nổ ở đây:
- Xóa mà node bị gộp vào **anh em trái** thì chính node đang nằm trong đường đi bị giải phóng.
  Đã đánh dấu `crumb.gone` để `release()` không unpin một page đã rời pool — nhưng đây đúng
  kiểu lỗi chỉ hiện ra sau vài nghìn thao tác.
- `MaxEntrySize` phải đảm bảo **hai** entry lọt một node; sai chỗ này thì split lặp vô hạn chứ
  không báo lỗi.

> Ghi lại vì nó vui: **cả hai linh cảm đều trúng**, và cả hai đều nổ trong lần chạy test đầu
> tiên. Nhưng linh cảm chỉ chỉ đúng *chỗ*, không chỉ đúng *cái gì* — chi tiết ở dưới.

---

### 2026-09-01 (lượt 3) — chạy lần đầu: 10 FAIL, hai nguyên nhân gốc

```console
$ go test ./internal/btree/ -count=1
--- FAIL: TestDeleteEverythingShrinksBack
    bufpool: Unpin một page không hề được pin
--- FAIL: TestTinyPool/6frame
    Free page 15 đang pin=1
--- FAIL: TestPropertyRandomOps
    còn 1 frame bị pin
--- FAIL: TestEntryTooLarge
    Put entry sát ngưỡng: entry 2030 byte > 2028
... (10 FAIL)
```

**Đọc kết quả:** 10 dòng đỏ, nhưng chỉ **hai** nguyên nhân. Năm dòng là test so sánh
`err != ErrKeyNotFound` trong khi code bọc lỗi bằng `%w` (câu chữ có kèm khóa gây lỗi) — lỗi
nằm ở **test**, và cái đúng là giữ nguyên bọc lỗi rồi đổi test sang `errors.Is`. Bốn dòng còn
lại đều là **một** bug pin thật.

**Đang nghĩ gì:** phản xạ "test đỏ ⇒ sửa code" là sai, đúng như bài học thất bại #4 của phase
3. Câu hỏi đúng luôn là *cái nào mới đúng?* — ở đây câu trả lời khác nhau cho hai nhóm.

#### Bug #1 (thật) — gộp vào anh em TRÁI thì quên `Unpin` anh em

`balancePair` luôn ghi vào **cả hai** node. Khi node hiện tại chết (gộp sang trái), tôi unpin
`c` và giải phóng page — nhưng anh em trái, thứ vừa được ghi và **vẫn đang pin**, thì không ai
nhả. Pool 6 frame nên nó nổ sau vài chục thao tác; pool 256 frame thì nó sẽ âm thầm rò rỉ tới
lúc nào đó rất xa.

```go
// internal/btree/delete.go  fixUnderfull
if merged {
	if rightID == c.id {
		c.gone = true
		t.unpin(c.id, false)
		t.unpin(sibID, true) // anh em trái sống và vừa bị ghi  <-- THIẾU
	} else {
		t.unpin(sibID, false) // anh em phải chết, không cần ghi
		c.dirty = true        // <-- THIẾU
	}
	...
}
t.unpin(sibID, true)
c.dirty = true                // <-- THIẾU
```

Ba dòng thiếu, không phải một. Hai dòng `c.dirty = true` là bug **thứ hai nằm chồng lên bug
thứ nhất**: ở tầng branch, `c` chưa hề bẩn (lần xóa chỉ chạm tới leaf), nên redistribute/merge
sửa nó xong mà page vẫn bị coi là sạch → thay đổi bốc hơi khi frame bị đuổi. Bug này
**không** làm test đỏ; nó chỉ làm cây hỏng ở một lần chạy dài nào đó. Nó lộ ra vì tôi đang đọc
kỹ đoạn code quanh chỗ pin bị rò.

#### Bug #2 (test sai, không phải code sai) — `TestEntryTooLarge`

```console
Put entry sát ngưỡng: entry 2030 byte > 2028
```

Phản xạ đầu tiên: "`MaxEntrySize` tính sai". Kiểm lại bằng tay: 2 cell × 2028 + header 24 +
2 slot × 4 = 4088 ≤ 4096 ✓ — hằng số **đúng**. Cái sai là số học trong test: nó quên 2 byte
`keyLen` mà `Put` có đếm.

```go
big := make([]byte, MaxEntrySize-2-1+1)          // phải vượt ngưỡng đúng 1 byte
fit := make([]byte, MaxEntrySize-2-len(k(1)))    // phải vừa khít
```

**Bài học lặp lại lần thứ hai trong repo này:** một test đỏ không chứng minh code sai. Ở đây
thông báo lỗi của code (`2030 > 2028`) hoàn toàn chính xác — nó đang tố cáo cái test.

---

### 2026-09-01 (lượt 3, tiếp) — siết property test, và bất biến sách giáo khoa sụp đổ

Suite đã xanh. Nhưng xanh với `value` 2-3 byte thì không chứng minh được gì về cell biến độ
dài, nên tôi kéo `value` lên 200 byte và ép cây cao ≥ 3 tầng. Kết quả:

```console
$ go test ./internal/btree/ -run TestPropertyRandomOps -count=1
--- FAIL: TestPropertyRandomOps
    op 2000: 4 node dưới 50% nhưng chỉ có 0 lần SkippedRebalance
```

**Đọc kết quả:** đây chính là G6, và nó đang sai. Nhưng sai ở đâu? Hai khả năng: rebalance bỏ
sót, hay bất biến vô lý. Không đoán — viết một test **chỉ chèn, không xóa bao giờ**:

```console
$ go test ./internal/btree/ -run TestDiagSplitOnly -v -count=1
    CHỈ CHÈN: underfull=34 splits=312 merges=0 redistributes=0
```

**Đọc kết quả:** dứt điểm. Không có lần xóa nào, nên rebalance không thể "bỏ sót" gì cả —
34 node dưới nửa là do **split** đẻ ra. Truy tiếp vào `midpoint`: nó cắt ở cell đầu tiên khiến
nửa trái vượt `total/2`, tức là luôn cắt **quá tay** về bên trái.

```go
// internal/btree/split.go  midpoint — hai ứng viên, chọn cái GẦN half hơn
cut := i + 1
if i > 0 && half-prev < run-half {
	cut = i
}
```

`underfull` 34 → **17**, giảm một nửa. Nhưng vẫn không về 0, và đến đây mới là phần đáng giá:

> **Bất biến "mọi node trừ root đầy >= 50%" ngầm giả định cell có kích thước CỐ ĐỊNH.**
> Với cell biến độ dài, ranh giới split chỉ có thể rơi **giữa hai cell**, nên nửa nhẹ hơn
> có thể hụt tối đa đúng một cell. Sàn thật đạt được là `(PageSize − maxCell)/2`.

Đo để chứng minh, không phải để minh hoạ:

```console
$ go test ./internal/btree/ -run TestPropertyRandomOps -v -count=1
    60000 thao tác: 4334 khóa còn lại, height=2 fill=68.0%
    splits=56 merges=18 redistributes=76 shrinks=0 skipped=0 (số loạt thấy node <50%: 0)
    maxCell=25 byte -> sàn B7 = 49.7%; node đặc ít nhất = 52.5%; số node <50% = 0
--- PASS: TestPropertyRandomOps/value_ngắn
    60000 thao tác: 4334 khóa còn lại, height=3 fill=67.2%
    splits=1091 merges=656 redistributes=1907 shrinks=0 skipped=0 (số loạt thấy node <50%: 29)
    maxCell=374 byte -> sàn B7 = 45.4%; node đặc ít nhất = 49.1%; số node <50% = 4
--- PASS: TestPropertyRandomOps/value_dài
```

Hai dòng này là toàn bộ luận điểm, cạnh nhau: **cell 25 byte → sàn 49.7%, 0 node dưới 50%;
cell 374 byte → sàn 45.4%, 4 node dưới 50%.** Sàn tụt đúng theo `maxCell`, và node đặc ít
nhất luôn nằm **trên** sàn. Bất biến không hề mất, nó chỉ chưa bao giờ là 50%.

**Đang nghĩ gì:** cám dỗ lớn nhất lúc này là nới lỏng assertion cho test hết đỏ. Cách chống:
mỗi lần nới một khẳng định thì phải **thay bằng một khẳng định chặt hơn về mặt logic** cộng
một số đo. Nên `TestPropertyRandomOps` giữ nguyên khẳng định `>= 50%` **tuyệt đối** ở hồ sơ
`value ngắn` (chỗ nó đúng thật), và B7 trong `Verify()` gánh phần tổng quát.

#### Bất biến B8 tôi đã thử thêm rồi phải gỡ

Ý tưởng: "hai leaf **kề nhau** không thể cùng dưới nửa — nếu thế thì đã phải gộp". Nghe rất
chắc. Nó fail ngay. Nghi thủ phạm là cặp leaf khác cha (rebalance chỉ nhìn anh em cùng cha),
nên tôi luồn `parentOf` qua toàn bộ hàm `walk` để loại trừ — và cặp gây lỗi hoá ra **cùng
cha**. B8 sai thật, vì hai lý do độc lập:

1. `rebalance` chỉ chạy trên đường **Delete**. Một node dưới nửa do **split** đẻ ra mà không
   lần xóa nào chạm tới thì không ai đi sửa nó.
2. Ngay cả khi có xóa, `fixUnderfull` chỉ xét **một** anh em.

Nên B8 bị hạ cấp từ bất biến xuống **số đo**: `MergeMissed` (cặp cùng cha) và
`CrossParentPairs` (cặp khác cha), in ra mỗi lần chạy property test. Công đoạn luồn `parentOf`
không phí — nó chính là thứ chứng minh giả thuyết "tại khác cha" là sai.

```console
    cặp leaf kề nhau cùng dưới nửa: 0 cùng cha (gộp được mà chưa gộp) + 0 khác cha (không gộp được)
```

---

### 2026-09-01 (lượt 3, tiếp) — hai bug nữa, và cả hai đều do một cái test khác bắt

#### Bug #3 — `RightmostSplit` bắn nhầm vào leaf giữa cây

Sau khi sửa `midpoint`, `TestVariableKeySize` chuyển đỏ:

```console
--- FAIL: TestVariableKeySize
    B7: page 180 đặc 174/4096 byte, dưới sàn (PageSize-maxCell)/2
```

**174/4096 = 4%.** Một page đầy 4% thì không phải "hơi lệch chuẩn", nó là hỏng. Điều kiện cũ:

```go
if c.n.isLeaf() && t.RightmostSplit && i == len(cells)-1 {
```

`i == len(cells)-1` chỉ nói *"khóa mới đứng cuối trong leaf NÀY"* — điều đó đúng với rất nhiều
leaf giữa cây khi khóa ngẫu nhiên. Cắt 100/0 ở cực phải của cả cây thì lành (leaf trái đầy
100% và sẽ không bao giờ được chèn thêm nữa); cắt 100/0 ở giữa cây thì để lại một leaf rỗng
mà **không có gì lấp lại được**. Điều kiện đúng phải hỏi *"đây có phải leaf cực phải của cả
cây không"*, và câu trả lời nằm sẵn ở chuỗi sibling:

```go
if c.n.isLeaf() && t.RightmostSplit && i == len(cells)-1 && c.n.next() == 0 {
```

**Đang nghĩ gì:** bug này đã tồn tại từ lượt 1 và **7 test không bắt được**. Cái bắt được nó
là B7 — một bất biến tôi vừa mới siết lại xong ở bước trước. Bất biến đúng thì tự đi tìm bug.

#### Bug #4 — ghi đè bằng value NGẮN HƠN là một lần xóa trá hình

Fuzz tìm ra, sau khi tôi cho thông báo lỗi in kèm `tr.Stats()`:

```console
--- FAIL: FuzzTreeOps
    B7: page 2 đặc 1849/4096 byte, dưới sàn ... maxCell=273
    stats: {Splits:1 Merges:0 Redistributes:0 Overwrites:2 ...}
```

`Merges:0` mà node vẫn tụt dưới sàn — vậy không phải Delete làm co node. `Overwrites:2` là
manh mối: `Put` lên một khóa đã có, với value ngắn hơn, đi đường `SetAt` tại chỗ và **không
gọi rebalance**. Node co lại y hệt như bị xóa.

```go
// internal/btree/btree.go  Put
if err := leaf.n.p.SetAt(page.SlotID(i), cell); err == nil {
	leaf.dirty = true
	t.st.Overwrites++
	if underfull(leaf.n) {      // <-- THIẾU
		return t.rebalance(path)
	}
	return nil
}
```

Hậu quả nếu bỏ qua: workload toàn update thu nhỏ (rất thường gặp — cột `status` từ
`"processing"` xuống `"done"`) để lại các leaf gần rỗng vĩnh viễn. Cây vẫn **đúng**, chỉ là
file phình mãi không co. Đây đúng loại bug mà không test thủ công nào của tôi nghĩ ra được;
fuzz nghĩ ra vì nó không có định kiến "Put thì làm node to lên".

---

### 2026-09-01 (lượt 3, cuối) — bug #5 nằm trong chính dụng cụ đo

`make btreelab` chạy quá 10 phút mà chưa in xong mục 4:

```console
$ rtk proxy make btreelab
...
== 4. xóa: cây có trả lại page không ==
trạng thái                 khóa    pages   height   đầy lá
sau khi chèn             200000     8751        3    69.2%
   (treo ở đây)
```

Thủ phạm nằm ngay trong `cmd/btreelab/main.go`:

```go
for _, i := range rng.Perm(len(ks)) {
	cur, _ := tr.Count()      // <-- quét TOÀN BỘ leaf, mỗi lần xóa
	if cur <= want { break }
	_ = tr.Delete(ks[i])
}
```

`Count()` là một lần duyệt cursor hết cây. Gọi nó trong thân vòng lặp biến 200k lần xóa thành
**O(n²)**. Sửa: một hoán vị duy nhất dùng chung cho cả ba vòng + một biến `live` đếm cục bộ.
Mục 4 giờ chạy **dưới một giây**.

**Đang nghĩ gì:** đây không phải bug của B+Tree, và chính vì thế nó đáng ghi. Cái treo không
phải cây — cái treo là **dụng cụ đo**. Ghi vào cùng bảng với 4 bug kia, vì bài học của nó
tổng quát hơn: *một hàm O(n) đặt trong vòng lặp nóng thì thứ bạn đo là hàm đo, không phải
thứ cần đo.*

---

## Giả thuyết sai / bug đã gặp

### Giả thuyết đăng ký trước: 4/7 sai

| # | Tôi tưởng là | Thực tế là | Lệnh / output đã lật tẩy nó | Rút ra |
|---|---|---|---|---|
| **G1 ✗** | branch fanout / leaf fanout **> 5x** (branch 180-200 con) | **2.3x** — branch chứa 77 con, không phải 180-200 | `make btreelab` mục 3, khóa 16B: `leaf/page 33, branch/page 77` | Hai sai chồng nhau: (a) trần lý thuyết chỉ là `4072/(6+16+4) = 156` chứ không phải 200; (b) branch thật chỉ đầy ~50% vì split để lại thế, nên `156 × 0.5 ≈ 77`. **Fanout thực tế ≈ fanout lý thuyết × độ đầy** — tôi đã quên nhân với độ đầy. |
| **G2 ✗** | splits ngẫu nhiên / tăng dần ≈ **1.0-1.2x** ("mỗi leaf đầy đều phải tách một lần") | **1.42x** | `make bench-btree`: `splits/1k` = 43.71 (ngẫu) vs 30.68 (tăng) | Tiền đề sai. Số split không phải hằng số theo số khóa, nó tỉ lệ với **số page**, mà số page = khóa / (fanout × độ đầy). Độ đầy khác nhau ⇒ số split khác nhau. Bằng chứng phụ đắt giá: tắt tối ưu cực phải thì chèn tăng dần đẻ **59.56** splits/1k — **nhiều hơn cả ngẫu nhiên**. |
| G3 ✓ | độ đầy tăng/ngẫu ≈ 1.4x, page ≈ 0.7x | **1.42x** và **0.70x** | `make bench-btree`: `%fill` 98.87 vs 69.48; `pages/1k` 30.69 vs 43.71 | Đúng đến hai chữ số. 69% là hằng số nổi tiếng của B-tree ngẫu nhiên (≈ ln 2 = 69.3%) — trùng khớp này không phải may. |
| **G4 ✗** | ghi/khóa ngẫu/tăng > 10x ở pool 64, **≈ 1x ở pool 1024** | 35.0x ở pool 16 → **22.0x ở pool 1024**, không hề về 1x | `make btreelab` mục 2: `16 → 35.0x`, `64 → 33.8x`, `256 → 30.2x`, `1024 → 22.0x` | Vế đầu đúng, vế sau sai vì tôi chọn "pool lớn" quá nhỏ: cây 200k khóa = **8751 page**, nên 1024 frame vẫn chỉ là 12% của cây. Muốn thấy 1x phải cấp pool ≥ cây. Bench 1M khóa còn tệ hơn: cây ~44k page nên 512 và 64 frame **đều là hạt cát** (writes/op 1.012 vs 1.076) — G4 **không đo được** bằng bench đó. |
| **G5 ✗** | page còn lại / khóa còn lại **> 2x** (file co chậm hơn dữ liệu nhiều) | **1.07x** — gần như co đúng theo dữ liệu | `make btreelab` mục 4: xóa còn 10% khóa → `20000 khóa, 940 page`; đỉnh 8751 page → trả lại **89%** | Tôi đánh giá thấp merge. Xóa ngẫu nhiên đều tay làm **mọi** leaf cạn cùng nhịp, nên merge nổ liên tục (`merges=7811 redistributes=19140`) và độ đầy giữ nguyên 64.7%. Giả thuyết của tôi mới đúng cho xóa **theo cụm** (xoá hết một dải khóa) — kịch bản mà lab này không hề chạy. |
| **G6 ✗** | ">= 50%" là tuyệt đối, trừ đúng số lần `SkippedRebalance` | `SkippedRebalance = 0` ở **cả hai** hồ sơ, mà vẫn có node dưới 50%. Sàn thật là `(PageSize − maxCell)/2` | `go test -run TestDiagSplitOnly`: `CHỈ CHÈN: underfull=34 splits=312 merges=0` | **Phát hiện lớn nhất của phase.** "50%" của sách giáo khoa ngầm giả định cell cố định. Cell biến độ dài ⇒ ranh giới cắt chỉ rơi giữa hai cell ⇒ nửa nhẹ hụt tối đa một cell. Đo: cell 25B → sàn 49.7%, 0 vi phạm; cell 374B → sàn 45.4%, 4 node dưới 50%. |
| G7 ✓ | reads/op ở pool lạnh **< 3** | **1.804** ở pool 32 frame | `go test -bench GetPool -benchtime=200000x`: `BenchmarkGetPool32 ... 1.804 reads/op 39.87 %hit` | Đúng, và lý do đúng thì đáng nhớ hơn con số: cây 4 tầng nhưng root + tầng 2 gần như luôn nằm trong pool dù pool bé tí, nên chỉ 2 tầng dưới mới thật sự tốn read. |

### G7b — một số đo trông vô lý, và nó không phải lỗi bench

Quét pool từ 32 lên 8192 frame thì `reads/op` giảm đều như kỳ vọng, nhưng `ns/op` thì **không**:

```console
$ go test ./internal/btree/ -run '^$' -bench 'GetPool' -benchtime=200000x
BenchmarkGetPool32-6     	  200000	      1415 ns/op	        39.87 %hit	         1.804 reads/op
BenchmarkGetPool128-6    	  200000	      1539 ns/op	        55.43 %hit	         1.337 reads/op
BenchmarkGetPool512-6    	  200000	      1658 ns/op	        68.95 %hit	         0.9316 reads/op
BenchmarkGetPool2048-6   	  200000	      2768 ns/op	        77.49 %hit	         0.6753 reads/op
BenchmarkGetPool4096-6   	  200000	      2413 ns/op	        88.75 %hit	         0.3375 reads/op
BenchmarkGetPool8192-6   	  200000	      1381 ns/op	       100.0 %hit	         0 reads/op
```

Pool **to hơn** mà **chậm hơn**: 512 frame (1658ns) → 2048 frame (2768ns), dù hit ratio tăng
từ 69% lên 77%. Theo luật của skill, gặp số vô lý thì **nghi bench trước**. Đã kiểm:
`lruReplacer.Victim()` là O(1) (danh sách liên kết đôi trên hai mảng `int32`), nên không phải
chi phí tìm nạn nhân. Đối chiếu với mục Môi trường thì hình dạng khớp chính xác:

| frames | RAM của mảng frame | so với cache | ns/op |
|---|---|---|---|
| 32 | 128 KB | lọt L2 (3.8MB) | 1415 |
| 512 | 2 MB | lọt L2 | 1658 |
| 2048 | 8 MB | vượt L2, lọt L3 (12MB) | **2768** |
| 4096 | 16 MB | **vượt L3** | 2413 |
| 8192 | 32 MB | vượt L3 nhưng **0 read** | 1381 |

**Đọc kết quả:** `MemDB` không có I/O thật — một "read" chỉ là memcpy 4KB. Nên `ns/op` ở đây
đo **footprint cache CPU**, không đo I/O. Vùng tệ nhất là chỗ pool đủ lớn để văng khỏi cache
CPU nhưng chưa đủ lớn để hết miss. Trên đĩa thật một read ~100µs sẽ nuốt trọn mọi hiệu ứng
cache và chỉ còn `reads/op` có nghĩa.

**Kết luận đúng đắn:** con số đáng tin của bench này là `reads/op` và `%hit`; `ns/op` thì
không, và đã ghi thẳng câu đó vào comment của bench để lần sau không ai (kể cả tôi) trích nhầm.
Nợ 📏 P4-6 để đo lại trên `pager` thật.

### Năm bug thật đã sửa

| # | Bug | Triệu chứng | Cái gì bắt được nó | Sửa ở đâu |
|---|---|---|---|---|
| 1 | Gộp vào anh em trái không `Unpin` anh em; và branch bị sửa mà không đánh dấu `dirty` (2 chỗ) | `bufpool: Unpin một page không hề được pin`, `Free page 15 đang pin=1`, `còn 1 frame bị pin` | `TestTinyPool` (pool 6 frame) + `TestPropertyRandomOps` | `delete.go:fixUnderfull` |
| 2 | *(test sai, không phải code)* số học biên trong test quên 2 byte `keyLen` | `entry 2030 byte > 2028` | chính nó | `btree_test.go:TestEntryTooLarge` |
| 3 | `RightmostSplit` bắn ở **mọi** leaf mà khóa mới đứng cuối, không chỉ leaf cực phải của cây | `B7: page 180 đặc 174/4096 byte` = **4%** | bất biến B7 vừa siết xong, qua `TestVariableKeySize` | `split.go` — thêm `&& c.n.next() == 0` |
| 4 | `Put` ghi đè bằng value ngắn hơn làm node co mà không rebalance | `B7: page 2 đặc 1849/4096`, `Merges:0 Overwrites:2` | `FuzzTreeOps` | `btree.go:Put` |
| 5 | `tr.Count()` (O(n)) gọi trong thân vòng lặp xóa → O(n²) | `make btreelab` treo > 10 phút ở mục 4 | chính người chạy | `cmd/btreelab/main.go` |
| — | `midpoint` luôn cắt quá tay về trái | `underfull=34` với **0** lần xóa | `TestDiagSplitOnly` (test dùng một lần rồi xoá) | `split.go:midpoint` — chọn ranh giới gần `half` hơn; 34 → **17** |

## Số đo

**Máy:** WSL2 / i5-1235U / 6 core / ext4 (xem mục Môi trường). **Ngày:** 2026-09-01.
**Commit:** working tree của phase 4 trên `a97700a`.

### Deliverable chính — 1 triệu khóa, tăng dần vs ngẫu nhiên

```console
$ make bench-btree
go test ./internal/btree/ -run '^$' -bench 'Insert' -benchtime=1000000x -benchmem -timeout 30m
goos: linux
goarch: amd64
pkg: minidb/internal/btree
cpu: 12th Gen Intel(R) Core(TM) i5-1235U
BenchmarkInsertSequential-6            	 1000000	       816.6 ns/op	        98.87 %fill	         4.000 height	        30.69 pages/1k	        30.68 splits/1k	         0.03069 writes/op	     516 B/op	       1 allocs/op
BenchmarkInsertSequentialNoOpt-6       	 1000000	       874.5 ns/op	        51.22 %fill	         4.000 height	        59.57 pages/1k	        59.56 splits/1k	         0.05957 writes/op	     641 B/op	       1 allocs/op
BenchmarkInsertRandom-6                	 1000000	      2452 ns/op	        69.48 %fill	         4.000 height	        43.71 pages/1k	        43.71 splits/1k	         1.012 writes/op	     572 B/op	       1 allocs/op
BenchmarkInsertSequentialSmallPool-6   	 1000000	       619.0 ns/op	        98.87 %fill	         4.000 height	        30.69 pages/1k	        30.68 splits/1k	         0.03069 writes/op	     516 B/op	       1 allocs/op
BenchmarkInsertRandomSmallPool-6       	 1000000	      2243 ns/op	        69.48 %fill	         4.000 height	        43.71 pages/1k	        43.71 splits/1k	         1.076 writes/op	     572 B/op	       1 allocs/op
PASS
ok  	minidb/internal/btree	8.985s
```

**`-benchtime=1000000x` là bắt buộc, không phải trang trí.** Nếu để Go tự chọn `b.N`, hai kịch
bản sẽ đo trên hai cây **khác kích cỡ**, và `splits/1k` mất hết ý nghĩa so sánh.

Bench này đã chạy **hai lần** cách nhau vài giờ, ở hai trạng thái code khác nhau của
`cmd/btreelab`. Đối chiếu:

| Đại lượng | Lần 1 | Lần 2 | Chênh |
|---|---|---|---|
| `%fill` ngẫu nhiên | 69.48 | 69.48 | **0** |
| `splits/1k` ngẫu nhiên | 43.71 | 43.71 | **0** |
| `writes/op` ngẫu nhiên | 1.012 | 1.012 | **0** |
| `ns/op` tăng dần | 572.6 | 816.6 | **+43%** |
| `ns/op` ngẫu nhiên | 1920 | 2452 | **+28%** |

Số **cấu trúc** (đầy, split, page, ghi) là tất định — lặp lại chính xác đến chữ số cuối. Số
**thời gian** dao động 28-43% trên WSL2 giữa hai lần chạy cùng một binary. Đây là minh hoạ
sống cho luật "chốt bằng tỉ số, đừng chốt bằng số tuyệt đối", và cũng là lý do mọi kết luận
dưới đây dựa trên `splits/pages/fill/writes` chứ không dựa trên `ns/op`.

### btreelab, 200 000 khóa

```console
$ make btreelab
page 4096B, khóa 16B, giá trị 100B, pool 256 frame, 200000 khóa
entry tối đa lọt một node: 2028 byte

== 1. thứ tự chèn quyết định hình dạng file ==
kịch bản                     thời gian    splits  height    pages   đầy lá  ghi/khóa
tăng dần (auto-increment)        151ms      6134       3     6137    98.9%      0.03
tăng dần, tắt tối ưu phải        177ms     11909       3    11912    51.2%      0.06
giảm dần                         177ms     11912       3    11915    51.2%      0.06
ngẫu nhiên (UUIDv4)              344ms      8748       3     8751    69.2%      0.93

tỉ số ngẫu nhiên / tăng dần: split 1.43x, page 1.43x, thời gian 2.28x, độ đầy 0.70x
cùng 200000 khóa: tăng dần tốn 6137 page, ngẫu nhiên tốn 8751 page (+43% dung lượng file)

== 2. pool nhỏ dần: chèn ngẫu nhiên biến thành I/O ==
frames      ghi/khóa tăng  ghi/khóa ngẫu      tỉ số
16                  0.031          1.073      35.0x
64                  0.031          1.038      33.8x
256                 0.031          0.925      30.2x
1024                0.031          0.675      22.0x

== 3. fanout: một page chứa được bao nhiêu khóa ==
khóa (byte)   leaf/page  branch/page  khóa ở 3 tầng
8                    35          115         464022
16                   33           77         194088
32                   29           50          73086
64                   23           29          19376
128                  17           16           4392
(cột cuối: sức chứa của một cây 3 tầng — mọi tra cứu <= 3 lần chạm page)

== 4. xóa: cây có trả lại page không ==
trạng thái                 khóa    pages   height   đầy lá
sau khi chèn             200000     8751        3    69.2%
còn 50% khóa             100000     4636        3    65.4%
còn 25% khóa              50000     2346        3    64.8%
còn 10% khóa              20000      940        3    64.7%
merges=7811 redistributes=19140 shrinks=0 bỏ cân bằng=0
đỉnh 8751 page -> 940 page: trả lại 89% chỗ đã cấp
```

Cột **"tăng dần, tắt tối ưu phải"** là cột quan trọng nhất của bảng 1 và suýt thì tôi không
viết nó: nó cho thấy 98.9% **không** đến từ việc khóa tăng dần, mà đến từ *tối ưu split cực
phải*. Tắt tối ưu đi thì chèn tăng dần rơi xuống 51.2%, đúng bằng chèn giảm dần. Không có cột
đối chứng này thì tôi đã kết luận sai nguyên nhân.

### Tra cứu và quét

```console
$ go test ./internal/btree/ -run '^$' -bench 'Scan|SearchInPage' -benchtime=20x
BenchmarkScan-6           	      20	  18246531 ns/op	        91.23 ns/key
BenchmarkSearchInPage-6   	      20	       154.0 ns/op
    page chứa 135 khóa 16 byte
```

### Tỉ số cần nhớ

Tỉ số bền hơn số tuyệt đối — số tuyệt đối đổi theo máy và theo lần chạy (xem bảng dao động
28-43% ở trên).

| Tỉ số | Giá trị | Ý nghĩa |
|---|---|---|
| độ đầy lá: tăng dần / ngẫu nhiên | **1.42x** (98.87% vs 69.48%) | Câu trả lời cho "vì sao UUIDv4 làm PK là thảm họa", vế dung lượng |
| số page: ngẫu nhiên / tăng dần | **1.42x** | Cùng dữ liệu, file to hơn 42% — và mọi lần quét về sau đọc thêm 42% |
| số split: ngẫu nhiên / tăng dần | **1.42x** | Split không phải hằng số theo số khóa; nó tỉ lệ với số page |
| **ghi/khóa: ngẫu nhiên / tăng dần** | **33x** (1.012 vs 0.03069) | **Tỉ số đau nhất.** Tăng dần chỉ làm bẩn cùng một leaf cực phải, 32 lần chèn mới sinh 1 lần ghi. Ngẫu nhiên làm bẩn một leaf khác nhau mỗi lần → mỗi khóa ≈ một lần ghi page |
| ghi/khóa ngẫu/tăng theo pool | 35.0x (16 frame) → **22.0x** (1024 frame) | Pool lớn có giúp, nhưng không cứu được: 1024 frame vẫn chỉ là 12% của cây 8751 page |
| độ đầy: bật / tắt `RightmostSplit` | **1.93x** (98.87% vs 51.22%) | Tối ưu cực phải mới là nguồn của 98.9%, không phải bản thân thứ tự khóa |
| số split: tắt / bật `RightmostSplit` | **1.94x** (59.56 vs 30.68) | Split 100/0 đẻ ra một page đầy; split 50/50 đẻ ra hai page nửa vời |
| độ đầy ngẫu nhiên | **69.48%** ≈ **ln 2 = 69.3%** | Hằng số cổ điển của B-tree chèn ngẫu nhiên — trùng khớp này xác nhận cài đặt không có gì lệch chuẩn |
| quét / tra cứu điểm | **~15x** (91.23 ns/key vs ~1400 ns/op) | Vì sao data chỉ nằm ở leaf: quét đi ngang chuỗi sibling, không phải leo lại cây mỗi khóa |
| binary search trong page / một lần Get | **~9x** (154 ns vs ~1400 ns) | CPU trong page **không** phải nút cổ chai; chi phí nằm ở pin/unpin và chạm page |
| fanout: branch / leaf | **2.3x** (77 vs 33) | Thấp hơn nhiều so với dự đoán — vì fanout thực = fanout lý thuyết × độ đầy |
| reads/op pool 32 frame | **1.804** | Cây 4 tầng nhưng chỉ tốn ~1.8 read/lần tra cứu: tầng trên gần như luôn nằm sẵn trong pool |
| trả page sau khi xóa 90% | **89%** (8751 → 940) | Merge/redistribute làm việc tốt hơn tôi tưởng — với xóa ngẫu nhiên đều tay |

## Invariant tôi đã cài và lệnh kiểm chứng nó

Tất cả cài trong `internal/btree/verify.go:Verify`, gọi lại từ property test, mọi test cấu
trúc, và fuzz.

| Invariant | Cài ở đâu | Lệnh kiểm chứng | Kết quả |
|---|---|---|---|
| **B1** mọi leaf cùng độ sâu (cây cân bằng) | `verify.go:Verify` → `walk` | `go test ./internal/btree/ -run TestPropertyRandomOps -count=1` | PASS — deliverable chính của roadmap |
| **B2** khóa trong một node tăng dần (binary search hợp lệ) | `verify.go:Verify` → `walk` | như trên | PASS |
| **B3** mọi khóa của cây con nằm trong `[low, high)` cha quy định | `verify.go:Verify` → `walk` | như trên | PASS — đây là thứ bắt được lỗi khóa phân tách |
| **B4** branch có đúng `numCells+1` con | `verify.go:Verify` → `walk` | như trên | PASS |
| **B5** không page nào xuất hiện hai lần (không chu trình, không dùng chung) | `verify.go:Verify` → `walk` | như trên | PASS |
| **B6** chuỗi sibling của leaf đi đúng thứ tự khóa và đủ mọi leaf | `verify.go:Verify` (sau `walk`) | `go test ./internal/btree/ -run TestCursorScanMatchesSortedOrder -count=1` | PASS |
| **B7** node không phải root đặc `>= (PageSize − maxCell)/2` | `verify.go:Verify` (chạy **cuối**, cần `MaxCell` toàn cây) | `go test ./internal/btree/ -run 'TestPropertyRandomOps\|TestVariableKeySize' -v -count=1` | PASS; log in ra sàn thật: `maxCell=25B → 49.7%`, `maxCell=374B → 45.4%` |
| **B7 dạng cứng** ">= 50% tuyệt đối" | `btree_test.go:propertyRandomOps(strict=true)` | `go test ./internal/btree/ -run 'TestPropertyRandomOps/value_ngắn' -v -count=1` | PASS — `số node <50% = 0`. Chỉ khẳng định ở hồ sơ cell nhỏ, nơi nó **đúng thật** |
| ~~B8~~ hai leaf kề nhau không cùng dưới nửa | **đã gỡ** — hạ cấp thành số đo `MergeMissed` + `CrossParentPairs` | `go test ./internal/btree/ -run TestPropertyRandomOps -v -count=1` | `0 cùng cha + 0 khác cha`. Không phải bất biến: split đẻ node dưới nửa mà không lần Delete nào chạm tới |
| Không rò pin sau **mọi** thao tác | `btree_test.go:noPins` + `bufpool.Verify` | `go test ./internal/btree/ -run TestTinyPool -v -count=1` (pool 6/8/10 frame) | PASS — chính nó bắt được bug #1 |
| Cây khớp `map` tham chiếu từng khóa một | `btree_test.go:propertyRandomOps`, `fuzz_test.go:runTreeProgram` | `make fuzz-btree` | PASS, 730 872 exec |
| Bền qua file thật (ghi → `Commit` → đóng → mở lại) | `btree_test.go:TestRoundTripThroughPager` | `go test ./internal/btree/ -run TestRoundTripThroughPager -v -count=1` | PASS — `20000 khóa -> 405504 byte (99 page, 20.3 byte/khóa)` |

### Test tự phủ quyết (không cho phép PASS rỗng)

| Test | Nó FAIL khi nào |
|---|---|
| `TestRightmostSplitFillsPages` | khi tối ưu cực phải **không** làm lá đầy hơn và **không** giảm số page. Đo được: `false → fill=50.5% pages=489` vs `true → fill=99.5% pages=247` |
| `TestDeleteEverythingShrinksBack` | khi xóa hết mà cây không tụt về đúng 1 leaf. Đo được: `đỉnh 25 page (height 2) → 1 page; merges=23 redistributes=69 shrinks=1` |
| `propertyRandomOps` | khi `Merges == 0` hoặc `Redistributes == 0` — nghĩa là kịch bản chưa hề chạm tới đường code khó nhất, và một PASS như thế là vô nghĩa |
| `propertyRandomOps` | khi `Height < minHeight` — cây phẳng thì không kiểm được gì về split lan lên trên |

### Toàn bộ suite

```console
$ go test ./... -count=1
ok  	minidb/internal/btree	0.597s
ok  	minidb/internal/bufpool	1.064s
ok  	minidb/internal/page	5.364s
ok  	minidb/internal/pager	0.728s

$ go test ./... -count=1 -race
ok  	minidb/internal/btree	13.683s
ok  	minidb/internal/bufpool	28.659s
ok  	minidb/internal/page	31.068s
ok  	minidb/internal/pager	1.852s

$ make fuzz-btree
fuzz: elapsed: 2m1s, execs: 730872 (6039/sec), new interesting: 396 (total: 509)
PASS
ok  	minidb/internal/btree	121.005s

$ go vet ./... && gofmt -l .
(sạch)
```

## Đọc gì

- Database Internals (Alex Petrov), ch. 2 "B-Tree Basics" + ch. 4 "Implementing B-Trees" —
  chỗ nói về *sibling pointers*, *rightmost append optimization*, và **overflow/underflow theo
  byte chứ không theo số khóa**. Chương 4 là chỗ tôi lấy ý tưởng "gom cell ra vùng nháp rồi
  dựng lại page".
- SQLite `btree.c` — quy ước "con cực phải nằm ở header" (`RightChild`) mà tôi bê nguyên,
  và cách nó xử lý `balance_deeper` / `balance_shallower` (tương ứng `growRoot` /
  `maybeShrinkRoot` của tôi).
- Nguồn của "69%": độ đầy tiệm cận của B-tree khi chèn ngẫu nhiên là **ln 2 ≈ 69.3%**. Đo được
  69.48% nên tôi tin cài đặt không lệch chuẩn ở chỗ nào lớn.

## Rút ra (viết như thể giải thích cho người khác)

**Fanout, và vì sao lookup chỉ tốn ~2 lần chạm đĩa.** Page 4KB, khóa 16 byte, giá trị 100 byte:
một leaf chứa 33 khóa, một branch chứa 77 con (đo thật, đã tính cả việc branch chỉ đầy ~50%).
Cây 3 tầng chứa 194 088 khóa; cây 4 tầng — chính là cây 1 triệu khóa trong bench — chứa thoải
mái. Nhưng chiều cao **không** bằng số lần đọc đĩa: đo `reads/op = 1.804` với pool chỉ 32
frame, vì root và tầng 2 gần như luôn nằm sẵn trong buffer pool. Đây là chỗ phase 3 và phase 4
khớp vào nhau: fanout lớn làm cây thấp, cây thấp làm **tầng trên đủ nhỏ để luôn thường trú**,
và chỉ 1-2 tầng dưới mới thật sự tốn I/O.

**Vì sao B+Tree chứ không phải B-Tree.** Hai lý do, cả hai đều đo được. (1) *Quét*: dữ liệu chỉ
nằm ở leaf và các leaf nối nhau bằng chuỗi sibling, nên quét khoảng đi ngang một mạch —
**91 ns/khóa**, so với ~1400 ns cho một lần tra cứu điểm, tức **15x**. B-Tree có data ở node
trong thì mỗi khóa kế tiếp phải leo lại cây. (2) *Fanout*: branch của B+Tree chỉ chứa
`(khóa, con)` chứ không chứa giá trị, nên nhét được 77 con vào một page thay vì bị giá trị
100 byte chiếm chỗ. Fanout cao ⇒ cây thấp ⇒ tầng trên nhỏ ⇒ luôn cached.

**Cây cao thêm lúc nào, và vì sao luôn cân bằng.** Không bao giờ có ai "làm cho cây cân bằng"
cả — nó cân bằng vì cây **chỉ mọc từ gốc**. Split đẩy một khóa phân tách lên cha; nếu cha đầy
thì cha lại split và đẩy tiếp lên; khi cái đẩy lên chạm tới root và root cũng đầy, `growRoot`
tạo một root **mới** có đúng hai con. Đó là thao tác duy nhất làm chiều cao tăng, và nó tăng
cho **mọi** đường đi cùng một lúc. Không có nhánh nào dài ra một mình được. Chiều dài đường đi
là một tính chất **toàn cục** thay đổi bởi một thao tác **toàn cục** duy nhất — đó là toàn bộ
mẹo của B-tree.

**Delete: khi nào redistribute, khi nào merge — và vì sao merge khó hơn split.** Node tụt dưới
sàn thì nhìn sang một anh em **cùng cha** và hỏi đúng một câu: *gom cả hai lại có lọt một page
không?* Lọt thì **merge** (hai node thành một, cha mất một khóa — và cha có thể tụt dưới sàn,
nên nó lan lên trên); không lọt thì **redistribute** (chia lại byte cho đều, cha chỉ đổi khóa
phân tách). Merge khó hơn split vì tính bất đối xứng: split chỉ **thêm** — node cũ vẫn còn đó,
chỉ có một page mới ra đời. Merge thì **xóa** — một page biến mất khỏi cây trong lúc nó có thể
đang nằm ngay trên đường đi mà bạn đang giữ pin (bug #1 của tôi), phải rút nó khỏi chuỗi
sibling, phải trả lại freelist, và phải quyết định ai sống ai chết. Split có một trường hợp;
merge có bốn (gộp trái/gộp phải × node hiện tại sống/chết), và ba trong bốn nhánh đó tôi viết
sai ngay lần đầu.

**Right-most insert optimization, và vì sao UUIDv4 làm PK là thảm họa.** Khi khóa mới lớn hơn
mọi khóa đang có **và** leaf đang chèn là leaf cực phải của cả cây, split 50/50 là lãng phí:
nửa trái sẽ không bao giờ được chèn thêm nữa. Nên cắt 100/0 — giữ nguyên leaf cũ **đầy**, mở
một leaf rỗng bên phải. Đo được: độ đầy **98.87%** thay vì 51.22%, tức **1.93x**. Chú ý đây là
công của *tối ưu*, không phải của *thứ tự khóa*: tắt tối ưu thì chèn tăng dần rơi xuống 51.2%,
đúng bằng chèn giảm dần.

UUIDv4 giết chết cả ba thứ cùng lúc:
- **Dung lượng:** độ đầy 69.48% thay vì 98.87%. Cùng 1 triệu khóa: 43.71 vs 30.69 page/1k
  khóa — **file to hơn 42%**, và mọi lần quét về sau đọc thêm 42% số page.
- **Ghi:** đây mới là chỗ chí mạng. `writes/op` **1.012 vs 0.03069 = 33x**. Chèn tăng dần chỉ
  làm bẩn đúng một leaf cực phải, nên 32 lần chèn mới sinh ra một lần ghi page thật. Chèn
  ngẫu nhiên làm bẩn một leaf **khác nhau** mỗi lần, và khi cây lớn hơn pool thì mỗi lần chèn
  ≈ một lần ghi page. Với page 4KB cho một hàng 116 byte, đó là **write amplification 35x**.
- **Buffer pool:** không cứu được bằng cách mua thêm RAM. Đo được tỉ số vẫn là 22.0x ở pool
  1024 frame, vì 1024 frame vẫn chỉ là 12% của cây 8751 page. Muốn về 1x thì pool phải chứa
  cả cây — tức là RAM ≥ database.

Cách chữa cũng đọc ra được từ đúng ba con số đó: dùng khóa **đơn điệu tăng** (bigserial,
Snowflake, ULID, UUIDv7 — cái sau đúng là UUID nhưng 48 bit đầu là timestamp), hoặc chấp nhận
UUIDv4 làm khóa logic nhưng để PK vật lý là một khóa tăng dần.

**Ba bài học về phương pháp, không về B-tree:**

*Một, bất biến sách giáo khoa đi kèm giả định không được viết ra.* "Mọi node trừ root đầy
>= 50%" đúng khi cell cố định. Cell biến độ dài thì ranh giới split chỉ rơi được **giữa hai
cell**, nên nửa nhẹ hụt tối đa một cell, và sàn thật là `(PageSize − maxCell)/2`. Tôi mất một
lúc lâu đi tìm bug không tồn tại trước khi chịu nghi ngờ chính bất biến. Dấu hiệu nhận ra:
`SkippedRebalance = 0` mà vẫn có node dưới sàn — nghĩa là **không có thao tác nào bỏ sót**,
tức lỗi phải nằm ở định nghĩa chứ không ở thao tác.

*Hai, nới một khẳng định thì phải trả lại một khẳng định chặt hơn.* Khi B7 dạng "50%" sụp đổ,
cách dễ là hạ ngưỡng cho test hết đỏ. Cách tôi làm: thay bằng sàn `(PageSize − maxCell)/2`
đúng về mặt lý thuyết, **giữ nguyên** khẳng định 50% tuyệt đối ở hồ sơ cell nhỏ nơi nó đúng
thật, và in ra bốn số đo (`MaxCell`, `MinFill`, `MergeMissed`, `CrossParentPairs`) mỗi lần
chạy. Phần thưởng đến ngay: B7 mới siết xong đã tự đi bắt được bug #3 — một leaf đầy **4%**
mà 7 test khác bỏ lọt.

*Ba, thứ bạn tưởng đang đo thường không phải thứ máy đang làm.* Ba lần dính trong phase này:
`ns/op` của `BenchmarkGetPool` đo footprint cache CPU chứ không đo I/O; `make btreelab` đo
`Count()` chứ không đo `Delete()`; và bench 1M khóa **không** đo được G4 vì cả hai pool đều là
hạt cát so với cây. Chỉ có một cách chống: viết tỉ số kỳ vọng ra **trước**, rồi khi thấy lệch
thì nghi bench trước khi nghi máy.

## Nợ kỹ thuật / để dành cho sau

- [ ] **🔧 P4-1 · Không có overflow page.** Giá trị lớn hơn `MaxEntrySize` (2028 byte) bị từ
      chối thẳng bằng `ErrEntryTooLarge`. DB thật tràn phần dư sang chuỗi overflow page.
      Trả bằng: `TestPutHugeValue` với value 100KB phải PASS thay vì báo lỗi.
- [ ] **🔧 P4-2 · Branch node chưa có prefix compression / suffix truncation.** Khóa phân tách
      đang lưu **nguyên vẹn**, trong khi nó chỉ cần đủ dài để phân biệt hai bên. Đo được
      branch fanout hiện tại là 77; suffix truncation trên khóa 16 byte có tiền tố chung dài
      nên có thể đẩy lên đáng kể. Trả bằng: `make btreelab` mục 3 trước/sau, so cột
      `branch/page`.
- [ ] **📏 P4-3 · Cursor re-pin mỗi `Next()`, chưa tách được giá của nó.** `BenchmarkScan` cho
      91.23 ns/key nhưng chưa biết bao nhiêu phần trong đó là pin/unpin. Trả bằng: một biến
      thể cursor giữ pin trong suốt một leaf, so ns/key.
- [ ] **⏳ P4-4 · Root đổi `PageID` mỗi lần cây cao thêm** — tầng trên phải tự nhớ root mới.
      Cách thường dùng là cố định root ở một page id không đổi và copy nội dung. Để phase 5
      (WAL) quyết, vì nó liên quan tới chỗ ghi root vào meta page.
- [ ] **⏳ P4-5 · Chưa có latch-coupling: cây không an toàn khi nhiều goroutine cùng ghi.**
      `-race` xanh chỉ vì mọi test hiện tại đều đơn luồng. Đây là phase 7.
- [ ] **📏 P4-6 · `ns/op` của `BenchmarkGetPool*` không phải số đo I/O.** `MemDB` chỉ memcpy,
      nên số đó đang đo cache CPU (xem G7b). Trả bằng: chạy lại `benchGet` trên `pager` thật
      với `-verify-cache` của phase 0 để ép cache lạnh, rồi so `reads/op × thời gian một
      pread` với `ns/op` thực đo.
- [ ] **📏 P4-7 · Chưa đo xóa theo CỤM.** G5 sai vì lab chỉ xóa ngẫu nhiên đều tay (trả lại
      89% page). Xóa hết một **dải** khóa liên tiếp là kịch bản mà giả thuyết "file không co"
      của tôi có thể đúng. Trả bằng: thêm `-delmode range` vào `cmd/btreelab` mục 4.
- [ ] **🔧 P4-8 · `fixUnderfull` chỉ xét MỘT anh em.** Nếu anh em bên đó không gộp được thì bỏ
      cuộc, dù bên kia có thể gộp được. Đo được ảnh hưởng bằng `MergeMissed` (hiện tại = 0 ở
      cả hai hồ sơ property test, nên **có thể món nợ này không đáng trả** — nhưng phải đo ở
      workload xóa theo cụm của P4-7 trước khi kết luận).
