# Phase 6 — nhật ký lệnh đầy đủ, thất bại và cải tiến

Bổ sung cho [`phase6.md`](./phase6.md). File kia là **kết quả** đã biên tập; file này là
**toàn bộ đường đi**, kể cả các ngõ cụt.

Hình dạng của phase này, so với năm phase trước:

- Phase 3 bị chặn ở năm chỗ, phần lớn bởi thứ tôi tự cài từ phase trước.
- Phase 4 có **bốn trong bảy giả thuyết bị bác**, một cái sai ở tầng khái niệm.
- Phase 5: **bốn bug khác nhau hoá ra một nguyên nhân gốc**, và **hai lỗi của bộ đo trên một lỗi
  của code**.
- Phase 6: hình dạng mới. **Một chuỗi ba giả thuyết** cho **một** hiện tượng, trong đó cái thứ
  nhất bị bác **bằng số đo của chính bản sửa nó** (70 → 68), và **fuzzer tìm ra thứ mà ba lần
  đọc lại code không thấy** — trong 3 giây.

Một điều nữa đáng ghi vì nó lặp lại: phase này lại là **quyết định kiến trúc chặn tất cả**. Chọn
xong đường đi thì phần lớn code viết một lượt là chạy; nhưng nếu chọn sai thì phải viết lại
phase 5.

Cấu trúc lượt: 1 lượt viết code (không chạy gì) → 1 lượt chạy + sửa bug → 1 lượt viết nhật ký.
Chia thế có chủ ý: **không cho phép vừa viết vừa chạy**, để phần "đo" không bị lẫn với phần "sửa
cho hết đỏ".

Máy: WSL2 / i5-1235U / 6 core / ext4 / go1.26.2. Ngày 2026-09-02.
Commit gốc: `e97dddf` (phase 5 = `6ac4450`). Commit của phase: `4dedc4f`.

---

## Lượt 1 — viết code, không chạy gì

### Quyết định chặn tất cả, chốt trước dòng code đầu tiên

Phase 5 để lại **một writer vật lý** do code bắt buộc. Hai đường đi:

| | (a) nhiều writer vật lý | (b) deferred write — **đã chọn** |
|---|---|---|
| Latch-coupling (P4-5) | bắt buộc, ngay | vẫn để phase 7 |
| Pha undo physical của phase 5 | **phải bỏ** | không đổi một dòng |
| Abort | chạy undo physical | ném map đi |
| Nhiều writer song song ở tầng vật lý | có | không |

Chỗ chết của (a): A và B cùng sửa một page, A abort ⇒ dán ảnh-trước của A **xoá luôn việc của
B**. Ảnh-trước là ảnh của cả page, nó không biết byte nào của ai. Đây chính là lý do Postgres
không có pha undo — nó rollback bằng 2 bit trong clog.

### Thứ tự dựng, và lý do của thứ tự ấy

| # | File | Vì sao đứng ở đây |
|---|---|---|
| 1 | `txn/snapshot.go` | Luật visibility phải chốt trước; mọi thứ khác đọc nó |
| 2 | `txn/version.go` | Encoding chuỗi version — đổi sau là viết lại hết (bài học `wal/record.go`) |
| 3 | `txn/level.go` | Bốn mức, và **cái gì** phân biệt chúng |
| 4 | `lock/lock.go` | S2PL độc lập hoàn toàn với MVCC; viết và test riêng được |
| 5 | `txn/store.go` | Bộ đếm xid, bảng active, horizon, vòng thử lại |
| 6 | `txn/txn.go` | Write set, đường đọc, `apply` |
| 7 | `txn/vacuum.go` | Bộ dọn toàn cây + `ChainStats` |
| 8 | `txn/workload.go` | Probe anomaly + workload chuyển tiền — **không** phải file test |
| 9 | `cmd/txnlab` | Ba bảng deliverable |

Điểm (8) là một quyết định: `workload.go` **không** phải `_test.go`, để `cmd/txnlab` và bộ test
chạy **đúng cùng một** đoạn mã. Nếu lab và test có hai bản probe riêng thì bảng in ra và bảng
được khẳng định là hai thứ khác nhau, và cái đỏ sẽ không nói gì về cái in.

### Hai thứ bỏ đi, cùng một lý lẽ

- **`xmax`** — nó luôn bằng `xmin` của bản mới hơn liền kề. Xoá là tombstone.
- **clog** — write set chỉ vào cây khi đã commit, nên "có mặt trong cây" **đã có nghĩa là** "đã
  commit".

Lý lẽ, đã viết vào `wal/record.go` ở phase 5 khi xoá cờ `FlagHasBefore`:
> Hai nguồn sự thật cho cùng một sự việc là hai chỗ để lệch nhau.

### Bảng "ai chặn ai", viết vào doc của package trước khi cài

| | reader | writer |
|---|---|---|
| **reader** | không bao giờ | không bao giờ |
| **writer** | không bao giờ | RR: chỉ lúc commit · SER: từ lúc `Put` |

Ô "reader / writer = không bao giờ" chính là **P1-2b đã trả**.

### Kiểm cuối lượt 1

```console
$ gofmt -l . && go vet ./...
(im lặng)
$ wc -l internal/lock/*.go internal/txn/*.go cmd/txnlab/main.go | tail -1
  4282 total
```

Không chạy một bài test nào. Đó là luật của lượt này.

---

## Lượt 2 — chạy và sửa

### Lần chạy đầu: bốn thứ đỏ ở bốn tầng khác nhau

| Đỏ ở đâu | Nguyên nhân thật | Tầng |
|---|---|---|
| `TestReadOnlyCommitTouchesNothing` | Transaction chỉ đọc tiêu xid ⇒ 64 lượt đọc = 1 lần ghi log | **khái niệm** |
| `TestReadYourOwnWrites` | `gcXmin` thiếu vế "xid của chính mình" ⇒ tự coi tombstone của mình là rác | code |
| `TestChainFullIsReported` | Trần thật là trần **byte** (2028), không phải `MaxVersions` (64) | **khái niệm** |
| `TestDeadlockUpgradeCycle`, `...ThreeCycle` | **Bug của bài test**: không nhả khóa của nạn nhân deadlock | bộ đo |

```console
$ go test ./internal/txn/ -run TestReadOnlyCommitTouchesNothing -count=1
--- FAIL: TestReadOnlyCommitTouchesNothing
    50 transaction chỉ đọc sinh 220 byte log, phải là 0

$ go test ./internal/txn/ -run TestReadYourOwnWrites -count=1
--- FAIL: TestReadYourOwnWrites
    txn: thu hồi khóa "k3": btree: không có khóa: "k3"

$ go test ./internal/txn/ -run TestChainFullIsReported -count=1
--- FAIL: TestChainFullIsReported
    version thứ 41: btree: entry lớn quá: 2 + 3 + 2051 = 2056 > 2028
```

Cái đầu là bug đáng giá nhất của cả phase, vì nó không phải lỗi cài đặt mà là **một khái niệm tôi
chưa có**: *virtual transaction id*. Tôi đã đọc mục ấy trong tài liệu Postgres và không hiểu nó
để làm gì. Con số 220 byte tự giải thích.

### Ngõ cụt dài nhất: 300 giây treo ở đúng một ô

Bảng anomaly treo hết `-timeout` ở ô `non-repeatable-read × read-uncommitted`.

Vấn đề dụng cụ trước: **`rtk` lọc mất dump panic của `go test`**, nên trên terminal chỉ thấy
"timeout" mà không thấy stack. Phải đi đọc file tee thô:

```console
$ python3 -c "import glob;print(open(sorted(glob.glob('$HOME/.local/share/rtk/tee/*.log'))[-1]).read())" | grep -A 20 'goroutine.*chan receive'
```

Từ đó về sau, mọi lệnh cần output nguyên văn trong phase này đều chạy qua **`rtk proxy`**.

Nguyên nhân: **kênh một giá trị bị nhận hai lần.** `select` có timeout, rồi cuối hàm vẫn `<-done`
lần nữa; khi `select` **không** timeout thì lần nhận thứ hai chờ mãi. Sửa bằng `blocked bool`.
Cùng lỗi trong `probePhantom`.

**Và chính lúc đọc stack ấy mới lộ ra một lỗi treo THẬT của code:** `lock.Manager.Acquire` đặt
**một** `time.AfterFunc` ở đầu hàm rồi vào vòng `cv.Wait()`. Một lần bị `Broadcast` oan là timer
tiêu, lần `Wait()` sau không còn ai đánh thức. Sửa: hẹn giờ lập lại mỗi vòng theo
`rem := time.Until(deadline)`, `wake.Stop()` sau mỗi `Wait()`.

Ghi lại vì nó là một hình dạng đáng nhớ: **đi truy lỗi treo của bài test thì tìm ra lỗi treo của
code.** Nếu bài test không hỏng, tôi đã không đọc stack, và bug kia còn ngồi đó.

### Ngõ cụt đắt nhất: ba giả thuyết cho một hiện tượng

```console
$ go test ./internal/txn/ -run TestTransferSerializableCommitsEverything -count=1
--- FAIL: TestTransferSerializableCommitsEverything
    serializable: 70/160 lượt chuyển bỏ cuộc sau 50 lần thử, hết 0.19s
```

Chỗ đáng nghi **trước tiên** là con số **thời gian**, không phải con số thất bại: 50 lần thử
trong 0.19s ⇒ chưa tới 25µs mỗi lần ⇒ chúng **không chờ gì cả**, chỉ quay.

| # | Giả thuyết | Sửa gì | Kết quả | Kết luận |
|---|---|---|---|---|
| 1 | **Starvation** — nạn nhân deadlock luôn là đứa trẻ nhất, mà mỗi lần thử lại là một `vid` mới nên nó **luôn** trẻ nhất | Tách **tuổi** khỏi **danh tính**: `lock.Manager.SetAge`, tuổi giữ nguyên qua các lần thử lại | 70 → **68** | **BỊ BÁC bằng số đo của chính bản sửa nó.** Starvation có thật (bản sửa vẫn giữ) nhưng không phải nguyên nhân chính |
| 2 | **Conversion deadlock** — `Get` lấy S, `Put` nâng lên X ⇒ hai txn cùng đọc một khóa thì deadlock **chắc chắn** | `Txn.GetForUpdate` (= `SELECT ... FOR UPDATE`), dùng trong `transferOnce` | 68 → **10** | **ĐÚNG** — nguyên nhân chính |
| 3 | **Thrash** — 50 lần thử hết 0.22s ⇒ mọi contender thử lại **đồng pha** | Backoff có **jitter** (`rand.Int63n`) | 10 → **0** | ĐÚNG — phần **ngẫu nhiên** mới là phần phá đồng pha, không phải phần chờ |

Chú thích tôi **tự viết** trong `findCycle` ở lượt 1 — *"chọn nạn nhân trẻ nhất ⇒ không
starvation"* — là sai. Nó đúng cho 2PL sách vở, và sai ngay khi thêm một vòng thử lại. Chú thích
sai còn tệ hơn không có chú thích, nên nó được thay bằng một đoạn ghi lại **con số** đã bác nó.

```console
$ go test ./internal/txn/ -run 'TestTransfer' -count=1
ok  	minidb/internal/txn	3.1s
```

### Hai bài test tố oan

**(1) `TestSnapshotReadDoesNotBlockWriter`**

```console
$ go test ./internal/txn/ -run TestSnapshotReadDoesNotBlockWriter -count=1
--- FAIL: TestSnapshotReadDoesNotBlockWriter
    writer thứ 63: txn: chuỗi version đã đầy, không dọn thêm được
```

Tôi khẳng định "reader không chặn writer" và cây bác lại. **Không sửa code** — đây là sự thật về
MVCC-tại-chỗ. Sửa bài test cho nó nói đúng cái nó chứng minh được, và tách thành hai bài nói hai
vế:

- `TestSnapshotReadDoesNotBlockWriter` — 500 khóa **khác nhau** + `MaxVersions/2` lần vào cùng
  một khóa. Khẳng định: reader không **lấy khóa** của writer.
- `TestOldReaderStarvesWriterOnSameKey` (mới) — khẳng định writer **chết** ở trần, và chỉ thuốc
  duy nhất: đóng reader → `Vacuum`.

Phát biểu đúng: *"reader không **lấy khóa** của writer"*, **không** phải *"reader không **làm
hại** writer"*.

**(2) `TestTransferRepeatableReadRetriesNotLoses`** → `299/300` ở 4 tài khoản × 6 goroutine.
Cũng là tính chất thật của OCC. Sửa bài test (16 tài khoản), và thêm một bài **so sánh** đo chính
điểm giao: `TestOptimisticDegradesUnderContention` ở 2 tài khoản × 8 goroutine.

### Fuzzer: 3 giây để tìm thứ ba lần đọc lại code không thấy

Hai target mới, `internal/txn/fuzz_test.go`.

```console
$ go test ./internal/txn/ -run '^$' -fuzz FuzzChainCodec -fuzztime 120s -fuzzminimizetime 1s
fuzz: elapsed: 3s, execs: 30553 (12066/sec), new interesting: 43 (total: 47)
--- FAIL: FuzzChainCodec (2.53s)
        fuzz_test.go:47: mã hoá lại khác byte gốc:
             gốc 013030303030303030300000
             lại 010030303030303030300000
```

Byte thứ hai: `0x30` vào, `0x00` ra. `DecodeChain` chỉ xét `f&flagDeleted`, mọi bit cờ khác bị
**nuốt im lặng**. Hệ quả thật: một chuỗi hỏng đi qua `ChainStats` mà `BadChains` **vẫn 0** — cái
đồng hồ dựng riêng để bắt chuỗi hỏng lại không thấy loại hỏng này.

Vá xong `Decode`, chạy lại, **seed đầu tiên** đỏ:

```console
$ go test ./internal/txn/ -count=1
--- FAIL: FuzzChainCodec/seed#0
    chuỗi tự dựng lại không giải mã được: txn: chuỗi version hỏng: version 1 là tombstone nhưng nói 1 byte thân
```

Đây mới là bug thật, và nó sâu hơn: **`Encode` sinh ra được thứ mà `Decode` của chính nó từ
chối** — nó ghi `len(v.Val)` kể cả với tombstone, dù tài liệu ngay trên `type Version` nói `Val`
của bản `Deleted` **luôn nil**. Một luật được **viết** mà không có dòng code nào **thực thi** nó.
Đúng cái mà nguyên tắc của phase 5 tồn tại để chặn: *mỗi invariant chỉ được có một điểm thực
thi*. Ở đây nó có **không** điểm thực thi, chỉ có một câu văn.

Sửa ở **`Encode`** (chuẩn hoá tombstone thành thân rỗng), không chỉ kiểm ở `Decode`.

```console
$ go test ./internal/txn/ -run '^$' -fuzz FuzzChainCodec -fuzztime 120s -fuzzminimizetime 1s
fuzz: elapsed: 2m0s, execs: 2099803 (14592/sec), new interesting: 15 (total: 63)
PASS

$ go test ./internal/txn/ -run '^$' -fuzz FuzzTxnCrash -fuzztime 120s -fuzzminimizetime 1s
fuzz: elapsed: 2m0s, execs: 3012 (42/sec), new interesting: 169 (total: 172)
PASS
```

`FuzzChainCodec` cũng kiểm **bất biến định nghĩa của `Prune`** trên mọi cặp (horizon, snapshot).
Ở lượt 1 tôi phải **dựng bằng tay** hình ba transaction gối nhau để bắt con bug `min(Xmax)` vs
`min(Xmin)` (`TestPruneHorizonIsXminNotXmax`). Fuzzer không cần biết trước hình nào — nó thử hết.

`FuzzTxnCrash` trả lời câu mà suy luận không trả lời được: chuỗi version là encoding **mới** nằm
trong value của B+Tree, đi qua WAL/checkpoint/redo/undo mà **không tầng nào biết nó là gì**. Sau
crash: `BadChains == 0` sau mọi lần mở lại.

**Nói thẳng để không nhận công quá:** target này crash bằng `db.SimulateCrash()` **trong tiến
trình**, không phải `kill -9`. Cho phase 6 đi qua `cmd/crashlab` thật là việc còn lại → nợ P6-6.

### Một "hiện tượng" hoá ra là noise

Ở lượt 1 tôi chạy bench với `-benchtime=200x` cho nhanh và thấy `depth=60` **nhanh hơn**
`depth=32`. Theo luật của repo: thấy số vô lý thì **nghi bài đo trước, nghi máy sau**.

```console
$ go test ./internal/txn/ -run '^$' -bench 'GetChainDepth' -benchtime=200000x -count=3 | grep newest
BenchmarkGetChainDepth/depth=1/newest-6         	  200000	       173.2 ns/op
BenchmarkGetChainDepth/depth=8/newest-6         	  200000	       338.9 ns/op
BenchmarkGetChainDepth/depth=32/newest-6        	  200000	       896.3 ns/op
BenchmarkGetChainDepth/depth=60/newest-6        	  200000	      1434 ns/op
```

Đơn điệu. **200 vòng không phải một phép đo.** Lần thứ hai trong repo này dính đúng bẫy của
phase 0. `bench-txn` chốt `-benchtime` lớn + `-count=3` kèm chú thích lý do.

### Lỗi lặt vặt, ghi cho đủ

- `math.MaxUint64` tràn `int` ở một tham chiếu còn sót trong `store.go`.
- Một dòng `(&Txn{}).s.chainCount()` bỏ quên trong `store_test.go`.
- `strconv`/`time` thành import không dùng sau khi gộp code trùng.
- Một lần `gofmt` xuống dòng lại làm phép thay thế bằng python trượt — sau đó mọi phép thay thế
  đều chạy `gofmt -l .` ngay sau.
- Chuỗi `sleep 45 && cat` bị harness chặn; chuyển sang chạy nền + đọc output.

### Kiểm cuối lượt 2

```console
$ gofmt -l . && go vet ./...
(im lặng)

$ go test ./... -count=1 -v | grep -c '^--- PASS\|^    --- PASS'
240

$ go test -race ./internal/... -count=1 | tail -8
ok  	minidb/internal/btree	17.016s
ok  	minidb/internal/bufpool	30.114s
ok  	minidb/internal/db	53.768s
ok  	minidb/internal/lock	1.107s
ok  	minidb/internal/page	33.609s
ok  	minidb/internal/pager	2.481s
ok  	minidb/internal/txn	41.549s
ok  	minidb/internal/wal	1.196s

$ go run ./cmd/crashlab -n 20 | tail -1
20/20 vòng đúng, 0 sai. 1247 transaction đã commit được kiểm, 8.982s.

$ go run ./cmd/crashlab -n 10 -nowrite | tail -1
   (SAI — thoát 1, ĐÚNG như mong đợi)

$ go test ./internal/db/ -run '^$' -fuzz FuzzCrashRecover -fuzztime 60s -fuzzminimizetime 1s | tail -2
PASS
ok  	minidb/internal/db	61.140s

$ wc -l internal/lock/*.go internal/txn/*.go cmd/txnlab/main.go | tail -1
  4582 total
```

---

## Bảng tổng kết lượt 2

| Loại | Số | Ghi chú |
|---|---|---|
| Bug của **code** | 8 | 3 trong đó ở tầng **khái niệm**, không phải tầng cài đặt |
| Bug của **bộ đo** | 4 | 1 trong đó **che** một bug thật của code |
| Bài test **tố oan** | 2 | Cả hai đều thành **hai** bài nói **hai** vế, không phải một bài nới lỏng |
| Giả thuyết bị **bác bằng số** | 2 | Starvation (70→68), và "depth=60 nhanh hơn" (noise) |
| Fuzz | 2 target mới | 2 099 803 + 3 012 exec; bug đầu tiên tìm ra trong **3 giây** |

Tỉ lệ "lỗi dụng cụ / lỗi code" ≈ 1/2, lặp lại đúng hình dạng của phase 5. Nó không còn là tai
nạn — nó là hình dạng bình thường của việc đo một hệ đồng thời.

---

## Lượt 3 — viết nhật ký

Viết `phase6.md` + file này, cập nhật `docs/debts.md` (trả **P1-2b**, ghi **P6-1 → P6-7**),
`ROADMAP.md` (đánh dấu phase 6 ✅ + mục *Đã làm — và những chỗ khác với dự kiến*), `README.md`
(layout + `make` target mới), rồi commit và điền lại commit hash vào hai file diary.

### Ba chỗ khác với những gì ROADMAP dự kiến

1. ROADMAP viết *"Bắt đầu bằng 2PL nghiêm ngặt, **rồi** cài MVCC"*. Thực tế đi **ngược**: MVCC
   trước (vì nó quyết định hình dạng dữ liệu trên đĩa), S2PL sau như một **module độc lập** chỉ
   phục vụ mức `Serializable`. Hai thứ không xếp tầng lên nhau; chúng là hai lựa chọn song song
   cho cùng một câu hỏi.
2. ROADMAP viết tuple mang `(xmin, xmax)`. Thực tế **bỏ `xmax`**, và **bỏ luôn clog**.
3. ROADMAP không nói gì về việc **`read-uncommitted` là mức khó cài nhất**. Nó là mục bất ngờ
   nhất của cả phase, và nó chỉ hiện ra vì bảng anomaly bắt buộc khẳng định theo **cả hai
   chiều**.
