# Phase 3 — nhật ký lệnh đầy đủ, thất bại và cải tiến

Bổ sung cho [`phase3.md`](./phase3.md). File kia là **kết quả** đã biên tập; file này là
**toàn bộ đường đi**, kể cả các ngõ cụt.

Điểm khác so với [`phase2-log.md`](./phase2-log.md): phase 2 là một cuộc điều tra dài về *một*
triệu chứng. Phase 3 không có cuộc điều tra nào dài như thế — bù lại nó có **năm lần bị chặn
lại ở năm chỗ khác nhau**, và bốn trong số đó bị chặn bởi thứ tôi đã tự cài từ phase trước.
Đó mới là điều đáng ghi: bài học chỉ có giá trị khi nó nằm trong **lệnh** và trong **test**,
chứ không nằm trong trí nhớ.

Máy: WSL2 / i5-1235U / ext4. Ngày 2026-09-01. Commit của phase: `5087157`.

---

## Phần 0 — Đăng ký giả thuyết TRƯỚC khi viết dòng code nào

Việc đầu tiên không phải là code, mà là ghi vào `diary/phase3.md` năm giả thuyết kèm tỉ số kỳ
vọng (G1-G5). Lý do: nếu đo trước rồi mới viết nhận định, tôi sẽ luôn "giải thích được" mọi con
số vừa nhìn thấy, và mục *giả thuyết sai* sẽ trống rỗng vĩnh viễn.

Kết quả cuối buổi: **2 trong 5 sai** (G2 và G5). Cả hai đều nằm trong diary chính vì chúng đã
được viết ra trước.

```bash
sed -n '1,200p' ROADMAP.md          # đọc lại yêu cầu của phase 3
cat skills/diary/SKILL.md           # đọc lại luật ghi nhật ký
grep -n '^func \|^type \|^const ' internal/pager/pager.go
sed -n '28,145p;283,345p;439,520p' internal/pager/pager.go   # API tầng dưới
grep -n 'LSN\|SetLSN' internal/page/page.go                  # chỗ móc WAL của phase 2
```

### Thất bại #1 — `rtk proxy` và một cặp nháy đặt sai chỗ

```console
$ rtk proxy 'uname -srmo && go version && df -hT . | tail -1'
/usr/bin/uname: invalid option -- 'h'
Try '/usr/bin/uname --help' for more information.
```

Chuỗi lệnh được truyền vào như **các tham số của `uname`**, nên `-h` của `df` rơi vào `uname`.
`rtk proxy` nhận argv, không nhận một dòng shell.

```bash
rtk proxy bash -c 'uname -srmo && go version && df -hT . | tail -1 && nproc && grep -m1 "model name" /proc/cpuinfo'
```

**Bài học:** công cụ nào nhận argv thì phải tự dựng shell (`bash -c`) nếu muốn có `&&` và `|`.
Mất 30 giây, nhưng nếu không đọc kỹ thông báo lỗi thì rất dễ kết luận nhầm là "rtk hỏng".

---

## Phần 1 — Dựng code

```bash
mkdir -p internal/bufpool
# internal/bufpool/bufpool.go    Pool, Frame, Pin/Unpin, victim, writeFrame, Flush/FlushAll
# internal/bufpool/replacer.go   Replacer + LRU / CLOCK / LRU-K
# internal/bufpool/verify.go     Verify() 5 bất biến + Dump + Resident
# internal/bufpool/workload.go   MemStore, Uniform/Zipf/ZipfWithScan, Replay, Belady
gofmt -w internal/bufpool/ && go build ./...
```

### Thất bại #2 — thư mục làm việc dính lại giữa các lần gọi công cụ

```console
$ cd internal/bufpool && python3 - <<'PY' ... PY
ok
$ cd internal/bufpool && sed -i ... 
/bin/bash: line 1: cd: internal/bufpool: No such file or directory
```

Lần gọi trước đã `cd` vào đó rồi. Trạng thái thư mục **sống qua các lần gọi**, còn biến môi
trường thì không — một sự bất đối xứng rất dễ quên.

**Sửa:** từ đó mọi lệnh đều bắt đầu bằng `...` hoặc dùng
đường dẫn tuyệt đối. **Bài học:** đây là họ hàng gần của bài học phase 2 (*"khởi động rồi quan
sát phải nằm trọn trong một lệnh"*) — cùng một gốc: đừng giả định trạng thái giữa hai lần gọi.

### Cải tiến #1 — viết lại `Victim` của LRU-K thay vì gỡ nó

Bản đầu tiên của `lrukReplacer.Victim` có một khối `if` lồng ba tầng để xử lý nhóm "chưa đủ K
lần truy cập". Nó **biên dịch được**, và tôi không chứng minh nổi nó đúng.

Không debug. Viết lại thành hai nhóm ứng viên tách bạch, nhóm vô hạn luôn thắng:

```go
bestInf, bestInfEarliest := -1, uint64(math.MaxUint64)   // chưa đủ K lần -> khoảng cách vô hạn
bestFin, bestFinKth      := -1, uint64(math.MaxUint64)   // đủ K lần -> so mốc thứ K
```

**Bài học:** logic mà không phát biểu được thành **một câu** thì viết lại, đừng gỡ. Ở đây một
câu đó là: *"ai chưa chứng minh được mình thì đi trước, trong nhóm đó thì ai cũ nhất đi trước"*.

---

## Phần 2 — Chạy test lần đầu: treo, không phải fail

```console
$ rtk proxy go test ./internal/bufpool/ -count=1 -v
(quá 120s, bị đẩy xuống nền)

$ sleep 20; tail -30 <file-output>
(Bash completed with no output)
```

File output **rỗng** — `go test` gom hết output rồi mới in một lượt, nên một test treo trông
giống hệt một test đang chạy lâu. Không có tín hiệu nào để đọc.

Cách ra khỏi bế tắc: **chia đôi tập test và đặt `-timeout` ngắn**, để chính Go chỉ mặt.

```console
$ timeout 40 go test ./internal/bufpool/ -count=1 -run 'TestPinnedPageNeverEvicted' -v -timeout 20s
--- PASS: TestPinnedPageNeverEvicted (0.00s)

$ timeout 60 go test ./internal/bufpool/ -count=1 -run 'TestUnpinClean|TestDirtyEviction|TestCleanPage|TestWALRule|TestVerifyCatches' -v -timeout 10s
=== RUN   TestWALRuleBlocksDirtyEviction
panic: test timed out after 10s
	running tests:
		TestWALRuleBlocksDirtyEviction (10s)
```

**Đối chiếu thẳng với phase 2:** cũng là cờ `-timeout`, ở phase 2 nó **không** bắn vì fuzz
worker là tiến trình con; ở đây nó bắn ngay và chỉ đúng tên test. Cùng một cờ, hai thế giới —
biết được sự khác nhau đó tiết kiệm đúng cái tiếng đồng hồ đã mất lần trước.

### Thất bại #3 — vòng lặp vô hạn trong `victim()` khi WAL rule chặn mọi ứng viên

Code sai:

```go
if f.dirty {
    if err := p.writeFrame(f); err != nil {
        if errors.Is(err, ErrWALRule) {
            p.repl.Unpin(i)   // trả về hàng đợi
            continue          // <- tìm ứng viên khác
        }
```

Replacer trả nó ra, tôi trả nó về, replacer lại trả nó ra. Khi **mọi** frame đều bẩn và đều bị
WAL rule chặn thì vòng đó quay mãi mãi.

**Cải tiến #2:** đếm số lần bị chặn; đi hết một vòng frame mà ai cũng bị chặn thì chịu thua.

```go
for blocked := 0; ; {
    ...
        blocked++
        if blocked >= len(p.frames) {
            return 0, fmt.Errorf("%w: mọi ứng viên đều bị WAL rule chặn", ErrNoFrame)
        }
```

Điều quan trọng hơn cả cách sửa: **hướng** sửa. Lối thoát dễ chịu là "thôi thì ghi đại một
cái" — và nó sẽ chạy trơn tru cho tới lần mất điện đầu tiên, rồi mất luôn khả năng recovery.
Thà `ErrNoFrame` còn hơn.

### Thất bại #4 — sửa xong vẫn FAIL, nhưng lần này lỗi nằm ở **test**

```console
$ timeout 60 rtk proxy go test ./internal/bufpool/ -count=1 -run 'TestWALRule' -v -timeout 20s
    bufpool_test.go:183: bufpool: hết frame — tất cả đều đang bị pin: mọi ứng viên đều bị WAL rule chặn
--- FAIL: TestWALRuleBlocksDirtyEviction (0.00s)
```

Phản xạ đầu tiên là nghi bộ đếm `blocked` vừa viết. Nhưng đọc kỹ kịch bản: pool có 2 frame,
frame 1 giữ page bẩn bị WAL chặn, frame 2 giữ page 3 — mà **tôi quên `Unpin(3)`**. Vậy đúng là
không còn frame nào. Thông báo lỗi hoàn toàn chính xác.

```diff
+	if err := p.Unpin(3, false); err != nil {
+		t.Fatal(err)
+	}
```

> **Bài học:** một test đỏ **không** chứng minh code sai. Ở đây hai lỗi nằm chồng lên nhau —
> một trong code (vòng lặp vô hạn), một trong test (quên thả pin) — và cả hai đều biểu hiện ở
> cùng một dòng. Đọc kịch bản trước, đừng sửa cái vừa viết chỉ vì nó còn nóng.

### Thất bại #5 — `rand.NewZipf` trả `nil` khi `s <= 1`

```console
$ timeout 120 rtk proxy go test ./internal/bufpool/ -count=1 -v -timeout 100s
=== RUN   TestBeladyIsUpperBound
panic: rand: nil Zipf [recovered, repanicked]
math/rand.(*Zipf).Uint64(...)
	.../src/math/rand/zipf.go:60 +0x1b4
minidb/internal/bufpool.Zipf(0xc8, 0x4e20, 0x3ff0000000000000, 0x5)
	.../internal/bufpool/workload.go:59 +0x12a
```

`NewZipf` **không trả lỗi** khi tham số vô lý, nó trả `nil`; chỗ nổ nằm mãi tận lần
`Uint64()` sau đó, trong một hàm khác.

**Cải tiến #3:** chặn tại nguồn, bằng câu chữ mà người đọc hiểu ngay.

```go
if s <= 1 {
    panic("bufpool: rand.NewZipf cần s > 1")
}
```

---

## Phần 3 — Thất bại #6: workload "chống scan" không hề có scan

Đây là chỗ bài học lớn nhất của phase 2 tự động trả cổ tức.

```console
$ timeout 200 rtk proxy go test ./internal/bufpool/ -count=1 -run TestScanFloods -v
    bufpool_test.go:515: hit ratio vùng nóng: lru không scan 0.873 | lru có scan 0.827 | clock có scan 0.823 | lru-2 có scan 0.905
    bufpool_test.go:521: scan chỉ làm LRU tụt từ 0.873 xuống 0.827 — chưa dựng được sequential flooding, bench vô nghĩa
--- FAIL: TestScanFloodsLRUButNotLRUK (0.18s)
```

Số liệu này **trông rất ổn**: LRU-2 (0.905) cao hơn LRU (0.827), đúng như lý thuyết nói. Nếu
không có dòng tự phủ quyết, test đã PASS và tôi đã ghi vào diary rằng "đã chứng minh LRU-2
chống được sequential flooding" — trong khi thứ tôi thật sự dựng ra là một pool **to hơn vùng
nóng**, nơi chẳng có gì để đuổi cả.

Nguyên nhân: vùng nóng 100 page với `s=1.2` dồn gần hết xác suất vào chục page đầu, mà pool có
tới 64 frame. Chỉnh cho giống thực tế — **vùng nóng phải lớn hơn pool**:

```diff
-		hot    = 100
-		cold   = 900
-		frames = 48
-	clean := Zipf(hot, ops, 1.2, 11)
-	dirtyTrace := ZipfWithScan(hot, cold, ops, 1.2, 11, 200, 500)
+		hot    = 200
+		cold   = 1800
+		frames = 64
+	clean := Zipf(hot, ops, 1.05, 11)
+	dirtyTrace := ZipfWithScan(hot, cold, ops, 1.05, 11, 500, 200)
```

```console
    bufpool_test.go:515: hit ratio vùng nóng: lru không scan 0.755 | lru có scan 0.636 | clock có scan 0.634 | lru-2 có scan 0.815
--- PASS: TestScanFloodsLRUButNotLRUK (0.28s)
```

Đúng cái dòng bảo vệ đã cứu phase 2 (`if inversions == 0 { t.Fatal }`) lại cứu tiếp phase 3:

```go
if lruScan > lruClean*0.9 {
    t.Fatalf("scan chỉ làm LRU tụt từ %.3f xuống %.3f — chưa dựng được sequential flooding, bench vô nghĩa", ...)
}
```

**Cải tiến #4** kèm theo: `ReplayHot()` — đếm hit **riêng cho vùng nóng**. Tỉ lệ tổng bị các
page bị quét (mỗi page chạm đúng một lần, luôn miss) làm loãng: nó rơi từ 0.755 xuống 0.182,
một con số đúng nhưng nói về chuyện khác.

---

## Phần 4 — Đo, và hai giả thuyết đăng ký trước bị bác

```bash
timeout 300 rtk proxy go run ./cmd/bufferlab
timeout 400 rtk proxy go test ./internal/bufpool/ -run '^$' -bench . -benchmem -count=1
timeout 300 rtk proxy go test ./internal/bufpool/ -run '^$' -bench 'PinHit' -benchmem -count=1 -cpu 1,6
timeout 300 rtk proxy go test ./internal/bufpool/ -run TestConcurrent -race -count=1 -v -timeout 240s
```

**G5 sai** (*"CLOCK rẻ hơn LRU 1.5-3x"*):

```console
BenchmarkVictim/lru/64-6         144849998      8.605 ns/op
BenchmarkVictim/clock/64-6        85209267     14.25  ns/op
BenchmarkPinHit/lru-6             21076452     50.51  ns/op
BenchmarkPinHit/clock-6           28780314     45.41  ns/op
```

CLOCK **đắt hơn** khi chọn nạn nhân, rẻ hơn khi truy cập. Tôi đã gộp hai đường khác nhau vào
một giả thuyết duy nhất. Và kiểm tra tiếp trên đường đa luồng thì lợi thế cũng không hiện ra:

```console
BenchmarkPinHitParallel/lru        19846176     62.24 ns/op
BenchmarkPinHitParallel/lru-6       8444562    158.8  ns/op
BenchmarkPinHitParallel/clock      19945245     56.50 ns/op
BenchmarkPinHitParallel/clock-6     7872613    153.7  ns/op
```

6 lõi: mỗi thao tác chậm đi **2.55x**, thông lượng tổng chỉ được **2.3x**, CLOCK hơn LRU 3%.
Vì mọi thứ đi qua một `p.mu` chung nên chính sách nằm *bên trong* khoá — nó rẻ hay đắt đều
không đổi được gì.

> **Bài học:** khi kết quả không hiện ra như lý thuyết, câu hỏi đúng không phải "lý thuyết sai
> à?" mà là **"thiết kế của tôi có tạo ra điều kiện để lý thuyết đó phát biểu được không?"**.
> Ở đây là không. Đó là nợ **P3-1**, kèm lệnh đo và một câu hỏi quyết định — chứ không phải một
> câu kết luận vội.

**G2 sai** (*"LRU-2/LRU > 1.5x dưới scan"*): thực tế **1.28x**, và bảng quét cường độ scan cho
thấy tỉ số này gần như không đổi từ scan 50 page tới scan 1000 page — thiệt hại của scan bị
chặn trên bởi **kích thước pool**, không phải bởi độ dài lần quét. Đây là thứ tôi hoàn toàn
không lường trước, và nó là kết quả thú vị nhất của cả buổi.

---

## Phần 5 — Thất bại #7: `FlushAll` bị chính WAL rule chặn

Fuzz vừa dựng xong, chạy bản test có seed trước (đúng thói quen đã lập ở phase 2):

```console
$ timeout 200 rtk proxy go test ./internal/bufpool/ -run TestPoolProgramStress -count=1 -v
    fuzz_test.go:116: FlushAll: bufpool: vi phạm WAL rule — pageLSN chưa nằm trên đĩa: page 8 pageLSN=90: log chưa fsync tới đó
--- FAIL: TestPoolProgramStress (0.00s)
```

Câu hỏi không phải "sửa thế nào" mà là **"cái nào đúng?"** — và tôi nhận ra mình chưa hề định
nghĩa rõ hợp đồng của cái hook. `FlushLog(pageLSN)` nghĩa là gì?

- *"kiểm tra hộ xem log đã bền tới đây chưa"* → `FlushAll` thất bại là hợp lý, người gọi phải
  tự lo fsync log trước.
- *"hãy làm cho log bền tới LSN này"* → người gọi thật (phase 5) sẽ fsync rồi trả `nil`; chỉ
  khi *không thể* mới trả lỗi.

Chọn nghĩa thứ hai — đúng như cái tên `FlushLog`, và đúng với ARIES. Hệ quả: `FlushAll` **có
quyền** thất bại với `ErrWALRule`, và test fuzz (dùng hook chỉ-kiểm-tra) phải chấp nhận đó là
câu trả lời hợp lệ:

```diff
-			if err := p.FlushAll(); err != nil {
+			if err := p.FlushAll(); err != nil && !errors.Is(err, ErrWALRule) {
```

> **Bài học:** fuzz không chỉ tìm crash. Ở đây nó tìm ra một **chỗ tôi chưa quyết định**, bằng
> cách đi vào tổ hợp trạng thái mà tôi chưa từng nghĩ tới lúc viết API. Một quyết định chưa
> được phát biểu thì sớm muộn cũng có người (hoặc cái gì đó) phát biểu hộ.

---

## Phần 6 — Fuzz: lần này không có cuộc điều tra nào

```console
$ timeout 200 rtk proxy go test ./internal/bufpool/ -run '^$' -fuzz FuzzPoolOps -fuzztime 120s -fuzzminimizetime 1s
fuzz: elapsed: 1m27s, execs: 215535 (2729/sec), new interesting: 194 (total: 196)
...
fuzz: elapsed: 2m0s, execs: 342407 (3647/sec), new interesting: 194 (total: 196)
PASS
ok  	minidb/internal/bufpool	120.138s
```

**Không có một nhịp khựng nào.** Toàn bộ cuộc điều tra một tiếng của phase 2 rút gọn lại thành
**một tham số nằm sẵn trong lệnh**: `-fuzzminimizetime 1s`. Và cột đáng nhìn vẫn là
`execs/sec`, không phải chữ `PASS` — đó là lý do nó được dán vào đây nguyên văn.

Lệnh này cũng vượt quá 120s nên bị đẩy xuống chạy nền. **Cải tiến #5 (quy trình):** thay vì
ngồi chờ, tôi viết phần thân nhật ký trong lúc nó chạy — đúng luật *"ghi trong lúc làm"*, và
lần chờ đó không tốn gì cả.

---

## Phần 7 — Chốt phase, không lặp lại lỗi `--amend`

```bash
gofmt -l . && go vet ./... && timeout 300 rtk proxy go test ./... -count=1
git add -A && git commit -F - <<'EOF'
phase 3: buffer pool — pin/unpin, dirty, LRU/CLOCK/LRU-K, móc sẵn WAL rule
...
EOF
# -> 5087157
sed -i 's/^- \*\*Commit:\*\* _(điền khi chốt)_/- **Commit:** `5087157` .../' diary/phase3.md
git commit -m "docs: gắn commit hash 5087157 vào diary phase 3"
# -> 0095150
```

Hai commit, không `--amend`, theo đúng quy ước lập ở phase 0/1 và vấp phải ở phase 2. **Chi phí
lần này: 0 giây.** Đó là toàn bộ giá trị của việc ghi lại thất bại lần trước.

---

## Tổng kết: 7 thất bại, 8 cải tiến

| # | Thất bại | Sửa thành |
|---|---|---|
| 1 | `rtk proxy 'a && b'` — cả chuỗi thành argv của `uname` | `rtk proxy bash -c '...'` khi cần `&&` / `\|` |
| 2 | `cd` dính lại giữa hai lần gọi công cụ → đường dẫn tương đối hỏng | Đường dẫn tuyệt đối, hoặc `cd <root> &&` mở đầu mọi lệnh |
| 3 | `victim()` quay vô hạn khi WAL rule chặn **mọi** ứng viên | Bộ đếm `blocked`, hết một vòng frame thì `ErrNoFrame` — **không bao giờ** ghi vòng qua WAL |
| 4 | Sửa xong vẫn FAIL, nhưng lỗi nằm ở test (quên `Unpin(3)`) | Đọc kịch bản trước khi nghi code; hai lỗi có thể chồng lên cùng một dòng |
| 5 | `rand.NewZipf(s<=1)` trả `nil`, panic ở chỗ khác | `panic` có câu chữ rõ ngay trong `Zipf()` |
| 6 | Workload "chống scan" thực ra **không có scan** (pool to hơn vùng nóng) | Test tự phủ quyết bắt được; vùng nóng 200 page > 64 frame, `s=1.05` |
| 7 | Chưa định nghĩa `FlushAll` có được phép thất bại không | Chốt hợp đồng `FlushLog` = *"làm cho log bền tới LSN này"*; fuzz chấp nhận `ErrWALRule` |

Tám cải tiến để lại trong repo:

1. `victim()` có bộ đếm chặn — vòng lặp không thể vô hạn nữa.
2. `Replacer.Evictable(fr)` — để `Verify()` **đối chiếu chéo** trạng thái pool với trạng thái
   replacer (bất biến I4). Hai bên giữ trạng thái riêng thì phải có chỗ so.
3. `Zipf()`/`ZipfWithScan()` panic sớm với câu chữ rõ khi `s <= 1`.
4. `ReplayHot()` — đo hit ratio **riêng vùng nóng**, thứ duy nhất nhìn ra được sequential flooding.
5. `Belady()` — trần lý thuyết, để mọi con số hit ratio có thước đo tuyệt đối chứ không chỉ so
   với nhau.
6. `runPoolProgram()` — thân fuzz dùng chung với `TestPoolProgramStress` có seed (khuôn mẫu
   bê nguyên từ phase 2: mọi nghi vấn về fuzz phải tái hiện được bằng test thường).
7. `make fuzz-pool` nhúng sẵn `-fuzzminimizetime 1s` ngay từ đầu — bài học phase 2 nằm trong
   lệnh, không nằm trong trí nhớ.
8. Năm món nợ P3-1..P3-5 trong `docs/debts.md`, mỗi món **kèm lệnh trả và câu hỏi quyết định** —
   kể cả món có thể kết thúc bằng "không đáng làm", vì đó cũng là một cách trả.

### Ba câu đọng lại

**Viết giả thuyết ra trước khi đo, kèm con số.** 2/5 giả thuyết của tôi sai, và cả hai đều là
phần đáng đọc nhất của diary. Nếu đo trước rồi mới viết nhận định, tôi sẽ luôn "giải thích
được" mọi con số vừa thấy — và sẽ không bao giờ biết mình từng nghĩ sai.

**Một test đỏ không chứng minh code sai.** Thất bại #4: lỗi nằm trong kịch bản test, còn thông
báo lỗi của code thì hoàn toàn chính xác. Phản xạ "sửa cái vừa viết" là sai, phản xạ đúng là
đọc lại kịch bản và hỏi *cái nào mới đúng?*.

**Bài học của phase trước chỉ có giá trị khi nó nằm trong lệnh hoặc trong test.** Phase 2 mất
gần một tiếng vì `-fuzzminimizetime`; phase 3 không mất giây nào vì tham số đó đã nằm sẵn trong
`make fuzz-pool`. Phase 2 mất một lần `--amend` hỏng; phase 3 chốt bằng hai commit, mất 0 giây.
Còn dòng `if inversions == 0 { t.Fatal }` học được ở phase 2 thì tái sinh thành
`if lruScan > lruClean*0.9 { t.Fatal }` — và nó **lại** bắt được tôi đúng một lần nữa, ở một
chỗ hoàn toàn khác.
