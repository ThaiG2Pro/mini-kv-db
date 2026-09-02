# Phase 5 — nhật ký lệnh đầy đủ, thất bại và cải tiến

Bổ sung cho [`phase5.md`](./phase5.md). File kia là **kết quả** đã biên tập; file này là
**toàn bộ đường đi**, kể cả các ngõ cụt.

Hình dạng của phase này khác hẳn ba phase trước, và đó là điều đáng ghi nhất:

- Phase 3 bị chặn ở năm chỗ, phần lớn bởi thứ tôi tự cài từ phase trước.
- Phase 4 có **bốn trong bảy giả thuyết bị bác**, một cái sai ở tầng khái niệm.
- Phase 5: **bốn bug khác nhau hoá ra là một nguyên nhân gốc**, và trong lượt chạy tôi tìm ra
  **hai lỗi của bộ đo trên một lỗi của code**. Lần đầu trong repo này, phần lớn công sức của
  một lượt là đi sửa chính các dụng cụ đo.

Cấu trúc lượt: 1 lượt viết code (không chạy gì) → 1 lượt chạy + sửa bug → 1 lượt viết nhật ký.
Chia thế có chủ ý: **không cho phép vừa viết vừa chạy**, để phần "đo" không bị lẫn với phần
"sửa cho hết đỏ".

Máy: WSL2 / i5-1235U / 6 core / ext4 / go1.26.2. Ngày 2026-09-01 → 2026-09-02.
Commit gốc: `e97dddf`. Commit của phase: `6ac4450`.

---

## Lượt 1 — viết code, không chạy gì

Thứ tự dựng, và lý do của thứ tự ấy:

| # | Gói | Vì sao đứng ở đây |
|---|---|---|
| 1 | `internal/wal/record.go` | Định dạng record phải chốt trước mọi thứ; đổi sau là viết lại hết |
| 2 | `internal/wal/diff.go` | Payload physiological — quyết định hình dạng của cả redo lẫn undo |
| 3 | `internal/wal/log.go` | Ba mốc LSN, group commit, quét-và-cắt đuôi hỏng lúc mở |
| 4 | `internal/pager/wal.go` | Phần cấp phát mà WAL cần: `Extend`/`Reserve`/`ReleasePending`/`CommitMeta` |
| 5 | `internal/btree/journal.go` | Móc vào cây. `J == nil` ⇒ hành vi phase 4 nguyên vẹn |
| 6 | `internal/db/` | Transaction, checkpoint, ba pha recovery |
| 7 | `cmd/crashlab`, `cmd/wallab` | Deliverable và kính soi |

Nguyên tắc tự đặt trước khi viết dòng đầu: **mỗi invariant chỉ được có một điểm thực thi.** Nếu
một invariant cần hai chỗ để đúng thì nó sẽ sai ở chỗ thứ ba.

Hệ quả cụ thể: WAL rule chỉ nằm ở `bufpool.writeFrame`; quy tắc "record bù không bao giờ bị undo"
chỉ nằm ở một điều kiện `UndoNext != 0`; và cờ `FlagHasBefore`/`FlagHasAfter` bị **xoá** khỏi
`record.go` với chú thích tại chỗ:

> Hai nguồn sự thật cho cùng một sự việc là hai chỗ để lệch nhau.

(số đoạn trong payload đã mã hoá việc có/không có phía before/after; thêm cờ nữa là dư.)

### Lỗi biên dịch duy nhất của lượt 1

```console
$ go build ./...
internal/wal/log.go: invalid hex literal 0xD1DBW410
```

Magic number tôi gõ tay có chữ `W`. Đổi thành `0xD1DB1A05`.

---

## Lượt 2 — chạy và sửa: mười một lần đỏ

### 2.1 `TestAbortMatchesRecoveryUndo` — panic trong cây, không phải trong WAL

```console
panic: btree: cell 2: page: slot đã bị xóa
```

Nghĩ 10 phút rằng undo dán sai byte. Thật ra: `undoChain` khôi phục `d.root`, nhưng `Tree` giữ
**bản sao root riêng**, nên sau `Abort` cây vẫn đọc từ root mà txn vừa hủy đã tạo.

Thêm `Tree.SetRoot` (không log — dùng cho abort và recovery), gọi trong `Txn.Abort`.

**Cải tiến rút ra:** hai bản sao của cùng một con trỏ root là một nguồn lỗi cố hữu. Ghi vào
`journal.go` phân biệt rõ hai hàm: `setRoot` (có log, dùng khi cây cao/thấp đi) và `SetRoot`
(không log, dùng khi quay ngược lịch sử).

### 2.2 `TestUncommittedVanishes` thấy 0 loser — không phải bug

Test khẳng định "transaction dở dang không sống sót", và nó xanh. Nhưng đếm ra **0 loser**, nghĩa
là chẳng có gì để undo: pool 256 frame nên không page bẩn nào bị đuổi, log của loser chưa xuống
đĩa, recovery không thấy nó.

Một test xanh vì điều kiện tiền đề không xảy ra thì nó đang khẳng định một chuyện rỗng. Sửa
**test**, không sửa code: `Frames: 8`, và ghi chú tại chỗ vì sao pool rộng làm khẳng định trở nên
hiển nhiên đúng.

**Cải tiến rút ra:** mọi test crash trong phase này đều ghi rõ **số frame** và lý do chọn con số
ấy. `crashlab` mặc định 16 frame với đúng lý do đó.

### 2.3 `undo: đọc lsn=332959: EOF`

```console
--- FAIL: TestRandomOpsThenCrash
    undo: đọc lsn=332959: EOF
```

`Log.readLocked` chặn phạm vi đọc theo `l.end - lsn`. Nhưng `end` **bao gồm** cả byte còn trong
buffer RAM, nên nó tưởng file dài hơn thực tế. Chặn theo `l.bufBase - lsn`.

Đây là hệ quả trực tiếp của việc có ba mốc LSN: dùng nhầm một mốc là ra một lỗi trông như file hỏng.

### 2.4 Test ảnh-trọn-page kỳ vọng sai

Test đòi thấy `FlagFullPage` ở chỗ **không** hợp lệ để có nó. Record ALLOC đã là một rào chắn
trọn page rồi: redo của nó `Init` lại page từ zero. Nên một page được cấp mới rồi ghi đầy **không
bao giờ** cần ảnh trọn page. Sửa test: checkpoint trước, rồi mới chạm page.

**Cải tiến rút ra:** "chạm đầu tiên sau mỗi checkpoint" phải hiểu là *chạm đầu tiên mà trạng thái
trước đó không tự tái tạo được*. ALLOC tự tái tạo được.

### 2.5 Undo sinh page hỏng khi txn free page giữa đường

`pool.FreePage` vứt nội dung bẩn đi. Nên bản trên đĩa **cũ hơn** trạng thái lúc crash, và các
ảnh-trước cũ hơn nữa bị dán lên một cái nền sai.

Sửa: record FREE mang **trọn ảnh page**; `t.freePage` pin lại để chụp trước khi bỏ; undo của FREE
khôi phục ảnh ấy trước, rồi các ảnh-trước cũ hơn mới áp lên đúng nền.

Giá phải trả (đo được ở lượt sau bằng `wallab`): FREE tốn **4152 byte/record**. → nợ P5-3.

### 2.6 Bug tốn nhiều thời gian nhất: `Abort` im lặng làm mất lệnh xóa

8/12 seed sai. Tôi đi tìm trong recovery suốt nửa tiếng. Cách thoát ra là dựng **ma trận 2×2**
thay vì đọc code tiếp:

| | không crash | có crash |
|---|---|---|
| `abort=false` | 0/12 sai | 0/12 sai |
| `abort=true` | **8/12 sai** | **8/12 sai** |

Hai cột **giống nhau hoàn toàn** → không liên quan gì đến crash. Bug nằm ở `Abort` lúc chạy.

Nguyên nhân: leaf bị xóa một khóa rồi bị **gộp đi** trong cùng txn được thả pin với `dirty=false`.

**Cải tiến rút ra — và là bài học chung nhất của phase:** cờ `dirty` là *tối ưu* của tầng trên cho
buffer pool, được phép sai theo hướng "quên bật". WAL không được phép sai theo hướng đó. Nên
journal bỏ hẳn gợi ý ấy và **so byte**:

```go
// internal/btree/btree.go — unpin
if t.J != nil && t.J.PageOut(id, dirty) { dirty = true }
```

Giá: `Diff` chạy trên mọi page được pin, kể cả page không đổi. Đúng chỗ này quay lại đòi tiền ở
lượt sau (`bytes.Equal`, 3.7x).

### 2.7 Checkpoint có flush mà không rút ngắn được redo: 844 vs 844

```console
--- FAIL: TestCheckpointShortensRecovery
    844 record redo khi có checkpoint, 844 khi không — checkpoint không cắt được gì
```

`checkpointLocked` chụp bảng page bẩn **trước** `FlushAll`, nên CKPT-END mang `recLSN` lỗi thời và
recovery bắt đầu lại từ đúng chỗ cũ. Đổi thứ tự: flush **trước**, chụp bảng **sau**.

```console
$ go test ./internal/db -run TestCheckpointShortensRecovery -count=1 -v
--- PASS   (844 -> 105 record redo)
```

**8.0x.** Thứ tự của hai dòng lệnh trong một hàm checkpoint là toàn bộ giá trị của checkpoint.

### 2.8–2.11 Bốn bug, một nguyên nhân gốc

Ghi lại theo đúng thứ tự tôi gặp, vì bản thân thứ tự ấy là bài học: tôi sửa nhầm triệu chứng ba
lần trước khi nhìn ra chúng là **một** vấn đề.

**(2.8)** `crashlab -n 20` → 4/20 vòng đỏ:

```console
đọc freelist page 44: EOF
page 44 kiểu 10 không phải node B+Tree
```

Page chứa freelist nằm chung `pager.pending` với page thường, mà WAL rút `pending` ra ở lúc
**commit txn** chứ không ở lúc ghi meta. Host bị cấp lại trong khi meta bền vẫn trỏ vào nó.
→ danh sách `metaPending` riêng, chỉ nhả ở cuối `CommitMeta` **lần sau**.

**(2.9)** Recovery làm hỏng cây (16 frame, có xóa, có txn béo). Hai lần "sửa" đầu đều chỉ **che**
bug: pool 1024 frame → xanh; bỏ checkpoint cuối của recovery → xanh. Cả hai đều là bằng chứng tốt
(chúng khoanh vùng: liên quan tới đuổi page và tới checkpoint) nhưng không phải bản sửa.

Nguyên nhân thật: recovery dựng lại trạng thái cấp phát bằng **hai vòng rời** — `Reserve` trên mọi
ALLOC, rồi `FreeNow` trên mọi FREE. Page bị free ở LSN 100 rồi cấp lại ở LSN 200 rơi vào freelist
**trong khi đang sống**. → một lượt `lastEv map[PageID]pageEvent` theo **sự kiện cuối cùng**.

**(2.10)** Redo ghi đè page chứa freelist. Chốt `pageLSN >= rec.LSN` vô dụng vì page freelist
**không có** pageLSN. → `skipRedo` = `metaPending` ∪ page có sự kiện cuối là FREE-đã-commit.

**(2.11)** Vẫn còn hỏng. Cách tìm: cắm tạm một `panic` sau biến môi trường `MINIDB_GUARD` vào
`pager.WritePage`, rồi đọc stack trace:

```
bufpool.victim → bufpool.writeFrame → pager.WritePage
```

Page mồ côi sau abort được trả về bằng `pg.FreeNow`, nhưng **frame bẩn của nó vẫn nằm trong pool**.
Checkpoint sau đó lấy page ấy làm host freelist, rồi pool đuổi frame cũ ghi đè lên.
→ `bufpool.Discard(id)` gọi **trước** `pg.FreeNow(id)`, ở cả `Txn.Abort` lẫn `recover()`.

**Cải tiến rút ra:** giữ chốt chặn lại **thường trực**, dưới dạng error thay vì panic:

```go
if p.inMetaPending(id) {
    return fmt.Errorf("%w: page %d đang là page chứa freelist của meta hiện tại", ErrMetaPageBusy, id)
}
```

Nó biến một triệu chứng xuất hiện sau đó hàng trăm millisecond ở một file khác thành một dòng số
ngay tại chỗ gây lỗi. Đây là thứ đáng giá nhất tôi viết trong cả phase.

### 2.12 Ngõ cụt có ích: dựng lại crash trong tiến trình

Khi một bug chỉ tái hiện dưới `kill -9`, tôi dựng `DB.SimulateCrash()` (đóng log không flush) và
một bản replay **tất định** của workload crashlab, dừng ở txn chọn trước. Rồi bisect theo **tính
năng của workload** chứ theo code:

| biến thể | kết quả | suy ra |
|---|---|---|
| `-nodel` (bỏ xóa) | xanh | dính tới xóa |
| `-nofat` (bỏ txn béo) | xanh | dính tới đuổi page bẩn |
| pool 1024 frame | xanh | dính tới đuổi page bẩn |
| bỏ checkpoint cuối | xanh | dính tới checkpoint |

Bốn dòng này khoanh bug về đúng giao của bốn điều kiện, và giao đó là chỗ freelist gặp WAL. Các
test tạm (`zz_repro_test.go`, `zz_repro2_test.go`, `zz_diag_test.go`) xoá sau khi dùng.

### 2.13 Ma sát công cụ

`rtk` lọc output của `go test`, nên các dòng `println` chẩn đoán biến mất. Đi vòng bằng
`rtk proxy go test ... > file 2>&1` rồi đọc file. Ghi ra đây để lần sau không mất 5 phút.

---

## Lượt 3 — chạy toàn bộ, và bộ đo hỏng nhiều hơn code

Đây là phần khác biệt nhất của phase 5 so với các phase trước.

### 3.1 Nền xanh

```console
$ go test ./... -count=1
ok  	minidb/internal/btree	0.469s
ok  	minidb/internal/bufpool	0.978s
ok  	minidb/internal/db	20.139s
ok  	minidb/internal/page	4.347s
ok  	minidb/internal/pager	1.119s
ok  	minidb/internal/wal	0.140s

$ go test -race ./internal/... -count=1
ok  (6/6 gói)

$ gofmt -l . && go vet ./...
(im lặng)

$ go test ./... -count=1 -v | grep -c '^--- PASS'
167
```

### 3.2 Fuzz "đứng hình": bốn giả thuyết, ba sai, và bài học cũ

Chi tiết ở [`phase5.md`](./phase5.md). Ghi thêm ở đây **thứ tự bác bỏ**, vì nó là phần đáng giá:

| # | Giả thuyết | Cách bác | Chi phí |
|---|---|---|---|
| 1 | Một exec chậm bệnh hoạn | Cắm `panic` nếu exec > 5s → **không nổ** | 3 phút |
| 2 | 6 worker tranh nhau fsync | `-parallel=1` → vẫn đứng 20s | 1 phút |
| 3 | fsync/writeback của WSL2 | `NoSync: true` → bùng 425/sec rồi vẫn đứng | 1 phút |
| 4 | Bộ rút gọn của Go | Đọc lại log: **mỗi lần đứng đều ngay sau một `new interesting`** | 0 phút |

Giả thuyết 1 là cái quan trọng nhất **vì nó bác được sạch**: panic không nổ ⇒ worker không ở trong
fuzz target ⇒ đừng tìm trong code nữa. Ba phút để loại bỏ toàn bộ codebase khỏi danh sách nghi vấn
là ba phút rẻ.

Và cái đắt nhất: cờ đúng **đã nằm trong Makefile từ phase 2**.

```console
$ grep -n fuzzminimizetime README.md Makefile
README.md:make fuzz                           # phase 2: fuzz slotted page 120s (chú ý -fuzzminimizetime)
Makefile:	go test ./internal/db/ -run '^$' -fuzz FuzzCrashRecover -fuzztime 120s -fuzzminimizetime 1s
```

Tôi mất năm phút vì gõ `go test` bằng tay. **Đã gói một lệnh vào Makefile thì đừng chạy vòng qua nó.**

### 3.3 Bốn lỗi của dụng cụ đo

| Dụng cụ | Nó nói gì | Nó thật sự đo gì | Sửa |
|---|---|---|---|
| `benchRecover` | `redo 0` ở mọi cấu hình | chi phí **checkpoint** — vì `Open()` kết thúc recovery bằng một checkpoint, nên từ vòng 2 không còn gì để redo | chụp cặp file lúc crash, dán về trước mỗi vòng (đồng hồ tạm dừng) |
| `-benchtime=2000x` dùng chung | `Recover` chạy 2000 × ~100ms | 10 phút timeout, chưa xong dòng thứ ba | tách target: Insert `2000x`, Recover `10x`, Get `300000x` |
| `bench-wal` (nhánh `wal`) | `PASS  0.010s` | **không có benchmark nào** — cả thiết kế `DiffGran` chưa từng có một dòng số | viết 8 benchmark |
| `BenchmarkGetNoWAL` | — | **không tồn tại**; lời khẳng định "đường đọc không trả xu nào cho WAL" chưa từng được đo | viết nó, thêm điểm đo giữa `GetWithWALRaw`, `-count=3` |

### 3.4 Lỗi code duy nhất mà bộ đo mới tìm ra — và nó lớn

Benchmark `Diff` vừa viết ra đã nói hai chuyện vô lý: thời gian **phẳng** theo `gran`
(3443/3193/3299/3215 ns), và 3µs để so 4096 byte = 1.3 GB/s.

```console
$ sed -n '/^func equal/,/^}/p' internal/wal/diff.go
func equal(a, b []byte) bool {
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
```

Vòng lặp so từng byte, viết tay, trong khi `bytes.Equal` là assembly SIMD. Và nó nằm trên đường
ghi nóng nhất — vì bản sửa 2.6 bắt journal diff **mọi** page được pin, kể cả page không đổi.

| | trước | sau | tỉ số |
|---|---|---|---|
| `Diff` 4KB, gran 32 | 3193 ns | 914 ns | **3.5x** |
| `InsertNoSync` | 11154 ns/op | 3025 ns/op | **3.7x** |
| `InsertBatch1000` | 9842 ns/op | 5260 ns/op | 1.87x |

Và sau khi sửa, bảng số mới **thật sự** là một đánh đổi: gran 16 = 1188 ns / 80 byte log,
gran 128 = 287 ns / 384 byte. Trước khi sửa nó nói dối rằng gran nhỏ là miễn phí.

Giữ `DiffGran = 32`: không còn là "chỗ cân bằng" theo cảm tính mà là một điểm đã đo.

### 3.5 Hai "phát hiện" hoá ra là lỗi của phép đo

**Đường đọc chậm 1.34x.** Suýt ghi vào diary. `-count=3` + một điểm đo giữa (`d.tree.Get`, không
qua `d.mu`) → 359 / 368 / 380 ns, tức **1.03x** cho journal và 1.06x cho cả `d.mu`. Con số 1.34x
là nhiễu của một lần chạy trên một bài 400ns.

Chốt lại bằng khẳng định không phụ thuộc máy: `TestReadPathTakesNoSnapshot` — đường đọc chụp **0**
ảnh page. 0 là 0 trên mọi máy.

**`TestAbortLeavesNoOrphanPage` tố oan 1 page.** Đúng bằng 1, ngay sau `CheckpointFlush` — là
**page chứa freelist**: `CommitMeta` lấy một page từ chính freelist ra làm host, nên nó rời `free`
mà vẫn thuộc quyền sở hữu của meta. Phép đếm sai, không phải code. Đếm
`len(FreeList()) + MetaPendingCount()` ở **cả hai** đầu.

**Tỉ lệ của lượt này: 2 lỗi-đo / 1 lỗi-code.** Quy tắc số 5 của SKILL cần thêm một mệnh đề: nghi
cả **cái mình đang đếm**, trước khi nghi code.

### 3.6 `make wallab` hỏng trên cây sạch

```console
$ go run ./cmd/crashlab -child -dir data/wal -seed 1 -txns 400 -frames 16
child: open data/wal/data.db: no such file or directory
```

Tiến trình con không tự tạo thư mục làm việc. Nó phải gọi được một mình (đó là toàn bộ mục đích
của `make wallab`) → `os.MkdirAll` trong `child()`.

### 3.7 Bộ kiểm tra chưa từng được kiểm tra

```console
$ go run ./cmd/crashlab -n 20 -nosync
20/20 vòng đúng, 0 sai. 10641 transaction đã commit được kiểm, 13.079s.
```

Tắt **sạch** fsync mà vẫn 20/20. Đúng P0-1 (`kill -9` không giết page cache của kernel). Nhưng hệ
quả nghiêm trọng: `crashlab-nosync` được viết để chứng minh bộ kiểm tra biết báo SAI, và nó
**không** làm được việc ấy. Con số "200/200 vòng đúng" có thể chỉ là một hàm luôn trả về "ok".

Thêm `wal.Log.NoWrite`: giữ byte log trong buffer của **tiến trình**, không `write(2)`, để
`kill -9` mang chúng đi cùng. Nhân chứng (`note()` ghi tiến độ) vẫn `fsync` bình thường.

```console
$ go run ./cmd/crashlab -n 10 -nowrite
0/10 vòng đúng, 10 sai. 1944 transaction đã commit được kiểm, 4.353s.
exit status 1
```

Và nó bắt **cả hai** hướng hỏng: thiếu khóa đã commit, *và* cây hỏng vì page dữ liệu xuống đĩa
trước log của nó (`page 43 kiểu 5 không phải node B+Tree`) — tức đúng cái mà WAL rule tồn tại để
ngăn.

`make crashlab-nowrite` giờ là target **bắt buộc đỏ**, đứng ngay cạnh `crashlab-full` trong
Makefile với chú thích nói rõ: nếu nó xanh thì con số 200/200 vô giá trị.

### 3.8 Deliverable

```console
$ go run ./cmd/crashlab -n 200 -keep
200/200 vòng đúng, 0 sai. 9194 transaction đã commit được kiểm, 1m23.157s.
```

Cột `loser 1` ở gần hết các vòng ⇒ pha undo **thật sự** phải chạy, không xanh vì rỗng việc.

---

## Bảng tổng: thất bại và cải tiến

### Thất bại (16)

| # | Thất bại | Lượt | Tầng |
|---|---|---|---|
| 1 | `0xD1DBW410` không phải hex | 1 | gõ sai |
| 2 | `Abort` không kéo `Tree.root` theo `DB.root` | 2 | code |
| 3 | `TestUncommittedVanishes` xanh vì tiền đề không xảy ra | 2 | **test** |
| 4 | `readLocked` chặn theo `end` thay vì `bufBase` | 2 | code |
| 5 | Test FPW kỳ vọng FPW ở chỗ ALLOC đã lo | 2 | **test** |
| 6 | `pool.FreePage` vứt page bẩn → undo dán lên nền sai | 2 | code |
| 7 | `Abort` mất lệnh xóa (cờ `dirty` là gợi ý) | 2 | **khái niệm** |
| 8 | Checkpoint chụp DPT trước khi flush | 2 | code |
| 9 | Page chứa freelist nằm chung `pending` | 2 | **khái niệm** |
| 10 | Recovery dựng cấp phát bằng hai vòng rời | 2 | code |
| 11 | Redo tin `pageLSN` trên page không có pageLSN | 2 | **khái niệm** |
| 12 | Page mồ côi còn frame bẩn trong pool | 2 | code |
| 13 | `benchRecover` đo checkpoint | 3 | **bộ đo** |
| 14 | `bench-wal` trỏ vào benchmark không tồn tại | 3 | **bộ đo** |
| 15 | `Diff` so từng byte một | 3 | code |
| 16 | `crashlab-nosync` không chứng minh được gì | 3 | **bộ đo** |

Ba dòng chỉ là lỗi của **phép đếm/phép đo**, không phải bug: fuzz "đứng hình", đường đọc "1.34x",
`TestAbortLeavesNoOrphanPage` "1 page mồ côi". Chúng suýt trở thành ba kết luận sai trong diary.

### Cải tiến (14)

| # | Cải tiến | Bằng chứng |
|---|---|---|
| 1 | `PageOut` trả về "byte có đổi thật không"; WAL có nguồn sự thật riêng | `Abort` 8/12 sai → 0/12 |
| 2 | Record FREE mang trọn ảnh page | undo qua được txn free page giữa đường |
| 3 | Checkpoint: flush **trước**, chụp bảng **sau** | 844 → 105 record redo = **8.0x** |
| 4 | `metaPending` — page chứa freelist có danh sách riêng | crashlab 16/20 → 20/20 |
| 5 | `lastEv` một lượt theo sự kiện cuối của mỗi page | recovery không đưa page đang sống vào freelist |
| 6 | `skipRedo` = `metaPending` ∪ FREE-đã-commit | redo không ghi đè host freelist |
| 7 | `bufpool.Discard` — bỏ frame không ghi, không chạm allocator | page mồ côi không còn frame bẩn |
| 8 | Chốt chặn thường trực trong `pager.WritePage` | biến triệu chứng xa thành một dòng số tại chỗ |
| 9 | `bytes.Equal` trong `Diff` | `Diff` **3.5x**, `InsertNoSync` **3.7x** |
| 10 | `benchRecover` chụp/dán cặp file mỗi vòng | `redo 0` → `redo 21725`, số có nghĩa |
| 11 | `BenchmarkRecoverCleaner` — điểm đo thứ tư | chỉ ra checkpoint mờ không chặn được độ dài redo |
| 12 | 8 benchmark cho `internal/wal` | `DiffGran` từ cảm tính thành một điểm trên đường cong |
| 13 | `GetNoWAL` + `GetWithWALRaw` + `TestReadPathTakesNoSnapshot` | 1.34x (nhiễu) → **1.03x** + khẳng định "0 ảnh" |
| 14 | `wal.Log.NoWrite` + `make crashlab-nowrite` | **0/10 đúng, 10 sai, exit 1** — bộ kiểm tra biết báo SAI |

### Ba con số của cả phase

| | |
|---|---|
| Dòng code phase 5 | 4345 (`internal/wal` + `internal/db` + `pager/wal.go` + `btree/journal.go` + `cmd/crashlab` + `cmd/wallab`) |
| Test | 167 toàn repo, trong đó 12 của `internal/db` và 7 của `internal/wal` |
| Deliverable | 200/200 vòng `kill -9`, 9194 transaction đã commit được kiểm — **và** 10/10 vòng báo SAI khi cố tình làm mất log |
