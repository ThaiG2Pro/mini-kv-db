# Phase 3 — Buffer pool

- **Thời lượng thực tế:** 1 buổi
- **Bắt đầu:** 2026-09-01 · **Kết thúc:** 2026-09-01
- **Trạng thái:** ✅ xong
- **Commit:** `5087157` — mọi số đo trong file này thuộc về cây làm việc của commit đó

> **Quy tắc ghi nhật ký:** mọi con số, mọi kết luận đều phải kèm **lệnh shell sinh ra nó**
> và **output thật** (dán nguyên, không tóm tắt). Ghi trong lúc làm, không phải sau khi xong.

## Môi trường

```console
$ uname -srmo && go version && df -hT . | tail -1 && nproc && grep -m1 "model name" /proc/cpuinfo
Linux 6.6.87.2-microsoft-standard-WSL2 x86_64 GNU/Linux
go version go1.26.2 linux/amd64
/dev/sdd       ext4 1007G   39G  918G   5% /
6
model name	: 12th Gen Intel(R) Core(TM) i5-1235U
```

## Mục tiêu phase

Một mảng frame cố định trong RAM đứng giữa tầng trên và `pager`. Ai cần page thì `Pin`, dùng
xong thì `Unpin`. Khi hết frame thì phải **chọn nạn nhân** — và chính chỗ chọn đó là toàn bộ
nội dung của phase này.

## Câu hỏi phải trả lời được khi xong

1. Vì sao **không** được dùng LRU thuần? Sequential scan phá nó bằng cơ chế nào, và cơ chế đó
   nhìn thấy được bằng số nào?
2. CLOCK xấp xỉ LRU tới mức nào? Chỗ nào nó *không* xấp xỉ được?
3. LRU-K chống scan bằng cách gì mà CLOCK không có?
4. Cách xa tối ưu (Belady) bao nhiêu? Tức là còn bao nhiêu phần trăm để mất công tối ưu tiếp?
5. Chi phí CPU của replacer có đáng kể so với một lần miss không?
6. Vì sao "không evict page đang pin" và "không evict dirty page trước khi WAL fsync" lại là
   **hai** bất biến khác nhau, không phải một?

## Giả thuyết đăng ký TRƯỚC khi đo

Viết ra đây trước, để lát nữa không tự lừa mình rằng "tôi đoán đúng mà".

| # | Giả thuyết | Tỉ số kỳ vọng |
|---|---|---|
| G1 | Zipfian thuần (không scan): LRU và CLOCK gần bằng nhau | chênh < 5 điểm hit ratio |
| G2 | Trộn sequential scan vào: **cả LRU lẫn CLOCK đều sụp**, LRU-2 thì không | LRU-2 / LRU > 1.5x |
| G3 | Khoảng cách tới Belady còn đáng kể | LRU / OPT ≈ 0.6-0.8 |
| G4 | Chi phí CPU chọn nạn nhân là không đáng kể so với một lần miss (phase 0: pread lạnh 69µs) | > 100x |
| G5 | CLOCK rẻ hơn LRU về CPU vì không phải move node mỗi lần truy cập | 1.5-3x |

Đo xong sẽ đối chiếu từng dòng ở mục "Giả thuyết sai".

## Deliverable (bằng chứng đã hiểu)

- `cmd/bufferlab`: bảng hit ratio của **lru / clock / lru-2 / Belady(OPT)** trên ba workload,
  kèm một cuộc quét cường độ scan cho thấy đúng chỗ LRU gãy.
- Test tự phủ quyết: nếu lần scan **không** thật sự phá được LRU thì test FAIL, không cho phép
  báo PASS rỗng.
- Property test 50 000 thao tác ngẫu nhiên × 3 chính sách, đối chiếu từng byte với model RAM,
  `Verify()` sau **mỗi** thao tác.
- Fuzz chuỗi thao tác (pin/unpin/flush/đẩy flushedLSN) × 3 chính sách.
- Test `-race` 8 goroutine × 4000 thao tác.
- Cài sẵn WAL rule như một điểm móc, và test chứng minh pool **thà chịu hết frame** chứ không
  ghi vòng qua nó.

> Đường đi đầy đủ — mọi lệnh đã chạy, kể cả các ngõ cụt, và 7 thất bại + 8 cải tiến:
> [`phase3-log.md`](./phase3-log.md).

## Reproduce toàn bộ phase này

```bash
go test ./internal/bufpool/ -count=1 -v
go test ./internal/bufpool/ -race -run TestConcurrentPinUnpin -count=1
go test ./internal/bufpool/ -run '^$' -bench . -benchmem -count=1 -cpu 1,6
go test ./internal/bufpool/ -run '^$' -fuzz FuzzPoolOps -fuzztime 120s -fuzzminimizetime 1s
go run ./cmd/bufferlab
```

---

## Nhật ký

### 2026-09-01 — dựng pool, và cái bẫy đầu tiên nằm ngay ở WAL rule

Layout: một `arena []byte` liền mạch chia thành N frame, `map[PageID]int` tra frame, một
`Replacer` cắm rời. `Pin` trả về **con trỏ thẳng vào arena** chứ không copy — đó là điểm khác
biệt duy nhất so với `pager.ReadPage`, và cũng là nguồn gốc của bất biến số 1: page đang pin
mà bị đuổi thì con trỏ tầng trên đang cầm sẽ lặng lẽ trỏ vào nội dung của page khác.

Chạy test lần đầu thì **treo**, không phải fail:

```console
$ go test ./internal/bufpool/ -count=1 -run 'TestWALRule' -v -timeout 10s
=== RUN   TestWALRuleBlocksDirtyEviction
panic: test timed out after 10s
	running tests:
		TestWALRuleBlocksDirtyEviction (10s)
```

**Đọc kết quả:** vòng lặp vô hạn trong `victim()`. Khi WAL rule chặn một page bẩn, tôi trả nó
về hàng đợi rồi `continue` — replacer lại trả đúng nó ra, lại chặn, lại trả về. Nếu **mọi** ứng
viên đều bị chặn thì vòng đó quay mãi.

Sửa: đếm số lần bị chặn, đi hết một vòng frame mà ai cũng bị chặn thì trả `ErrNoFrame`.
Tuyệt đối không được "thôi thì ghi đại" — vi phạm WAL rule là mất recovery.

**Đang nghĩ gì:** đáng chú ý là `-timeout 10s` cho luôn stack dump chỉ đúng test đang treo.
Ở phase 2 cũng cờ này nhưng nó **không** quản được fuzz worker (tiến trình con), nên tôi mất
gần một tiếng. Cùng một cờ, hai thế giới khác nhau.

### 2026-09-01 — `rand.NewZipf` trả nil khi s <= 1

```console
$ go test ./internal/bufpool/ -count=1 -v
=== RUN   TestBeladyIsUpperBound
panic: rand: nil Zipf [recovered, repanicked]
math/rand.(*Zipf).Uint64(...)
	/usr/lib/go/src/math/rand/zipf.go:60 +0x1b4
minidb/internal/bufpool.Zipf(0xc8, 0x4e20, 0x3ff0000000000000, 0x5)
	.../internal/bufpool/workload.go:59 +0x12a
```

`rand.NewZipf` trả về **nil, không phải lỗi**, khi `s <= 1`; chỗ nổ nằm mãi tận lần gọi
`Uint64()` sau đó. Đã chặn ngay trong `Zipf()` bằng `panic` có câu chữ rõ ràng — sinh viên của
sáu tháng sau sẽ đỡ mất mười phút.

### 2026-09-01 — hit ratio: bảng chính của phase

```console
$ go run ./cmd/bufferlab
pool 64 frame / 2000 page (3.2% dữ liệu), zipf s=1.05, 200000 thao tác

== 1. hit ratio TỔNG theo workload ==
workload                    lru    clock    lru-2      OPT  lru/OPT
uniform                   0.032    0.032    0.031    0.229     0.14
zipf (không scan)         0.755    0.744    0.817    0.883     0.86
zipf + scan 500/200       0.182    0.181    0.233    0.252     0.72
```

**Đọc kết quả:** ba dòng, ba bài học khác hẳn nhau.

- **uniform**: mọi chính sách đều về 0.032 ≈ đúng tỉ lệ pool/dữ liệu (3.2%). Không có locality
  thì buffer pool **không phải là bộ nhớ đệm, nó chỉ là một mảng**. Nhưng OPT đạt 0.229 — gấp
  **7.2x**. Tức là ngay cả trong workload "vô vọng" vẫn còn rất nhiều thứ để ăn, chỉ là phải
  biết trước tương lai mới ăn được.
- **zipf**: đây mới là hình dạng của workload thật. 3.2% dữ liệu trong RAM cho **75.5%** hit.
- **zipf + scan**: hit ratio tổng rơi từ 0.755 xuống 0.182. Nhưng con số này **gây hiểu lầm** —
  phần lớn cú rơi là do bản thân các page bị quét (mỗi page chạm đúng một lần, luôn miss).
  Muốn nhìn đúng thì phải tách vùng nóng ra.

### 2026-09-01 — sequential flooding, và một test tự phủ quyết đã bắt tôi

Lần đầu dựng workload scan, test tự FAIL:

```console
$ go test ./internal/bufpool/ -count=1 -run TestScanFloods -v
    bufpool_test.go:515: hit ratio vùng nóng: lru không scan 0.873 | lru có scan 0.827 | clock có scan 0.823 | lru-2 có scan 0.905
    bufpool_test.go:521: scan chỉ làm LRU tụt từ 0.873 xuống 0.827 — chưa dựng được sequential flooding, bench vô nghĩa
--- FAIL: TestScanFloodsLRUButNotLRUK
```

**Đọc kết quả:** zipf s=1.2 trên 100 page dồn gần hết xác suất vào vài page đầu, mà pool có
tới 64 frame — vùng nóng thật sự nhỏ hơn pool, nên scan có phá cũng chẳng phá được gì.
Đây **không phải** thí nghiệm chứng minh LRU gãy, nó là thí nghiệm chứng minh pool đủ to.

Chỉnh lại cho giống thực tế (vùng nóng **lớn hơn** pool): 200 page nóng / 64 frame, s=1.05,
quét 500 page lạnh sau mỗi 200 thao tác:

```console
$ go test ./internal/bufpool/ -count=1 -run TestScanFloods -v
    bufpool_test.go:515: hit ratio vùng nóng: lru không scan 0.755 | lru có scan 0.636 | clock có scan 0.634 | lru-2 có scan 0.815
--- PASS: TestScanFloodsLRUButNotLRUK (0.28s)
```

**Đọc kết quả:** LRU-2 **dưới scan** (0.815) còn cao hơn LRU **không có scan** (0.755). Nó
không chỉ chống được scan, nó còn tốt hơn LRU ngay từ đầu.

Quét theo cường độ scan:

```console
$ go run ./cmd/bufferlab
== 2. sequential flooding: hit ratio của RIÊNG vùng nóng ==
scan/200 op         lru    clock    lru-2  lru-2/lru
0                 0.755    0.744    0.817       1.08
50                0.648    0.638    0.816       1.26
100               0.634    0.633    0.817       1.29
200               0.636    0.634    0.816       1.28
500               0.636    0.634    0.815       1.28
1000              0.638    0.637    0.813       1.27
```

**Đọc kết quả:** thứ đáng chú ý nhất là **cột lru bão hoà**. Quét 50 page đã lấy đi gần hết
thiệt hại (0.755 → 0.648); quét 1000 page cũng không tệ hơn (0.638). Lý do: một lần quét dài
hơn số frame thì pool đã bị dọn sạch một lượt rồi, quét thêm cũng chỉ là đuổi các page vừa quét
đuổi lẫn nhau. **Thiệt hại của sequential scan bị chặn trên bởi kích thước pool, không phải bởi
độ dài lần quét.**

Và LRU-2 gần như **không nhúc nhích** (0.817 → 0.813) qua toàn bộ dải đó. Cơ chế rất đơn giản:
page bị quét chỉ được chạm **một** lần, chưa có mốc truy cập thứ 2, nên khoảng cách lùi K của
nó là vô hạn — nó bị đuổi *trước* mọi page đã được dùng từ 2 lần trở lên. Với LRU thuần thì
"vừa chạm một lần" và "chạm liên tục" đều quy về cùng một thứ: *vừa dùng*. **Đó là toàn bộ lỗi
của LRU: nó đo thời điểm gần nhất, chứ không đo tần suất.**

### 2026-09-01 — pool to bao nhiêu thì đủ

```console
$ go run ./cmd/bufferlab
== 3. pool to bao nhiêu thì đủ? (zipf, không scan) ==
frames     %dữ liệu        lru    clock    lru-2      OPT
8          4.0           0.318    0.296    0.454    0.540
16         8.0           0.460    0.439    0.582    0.666
32         16.0          0.607    0.590    0.704    0.781
64         32.0          0.755    0.744    0.817    0.883
128        64.0          0.906    0.902    0.926    0.965
```

**Đọc kết quả:** gấp đôi pool chỉ mua thêm ~15 điểm hit ratio, và càng về sau càng ít
(0.755 → 0.906 khi gấp đôi từ 64 lên 128). Nhưng **đổi chính sách** từ LRU sang LRU-2 ở pool
8 frame cho 0.318 → 0.454, tức là bằng với việc gấp đôi RAM (0.460 ở 16 frame). Một dòng suy
ra thẳng từ bảng: **đổi chính sách rẻ hơn mua RAM**, ít nhất ở vùng pool nhỏ.

Cũng để ý khoảng cách tới OPT **không** co lại khi pool to ra: 0.755/0.883 = 0.86 ở 64 frame,
0.906/0.965 = 0.94 ở 128. Có co, nhưng chậm.

### 2026-09-01 — chi phí CPU: replacer có đắt không

```console
$ go test ./internal/bufpool/ -run '^$' -bench . -benchmem -count=1
cpu: 12th Gen Intel(R) Core(TM) i5-1235U
BenchmarkPinHit/lru-6            21076452     50.51 ns/op    0 B/op   0 allocs/op
BenchmarkPinHit/clock-6          28780314     45.41 ns/op    0 B/op   0 allocs/op
BenchmarkPinHit/lru-2-6          22764864     46.49 ns/op    0 B/op   0 allocs/op
BenchmarkPinMiss/lru-6            2886759    448.9  ns/op    0 B/op   0 allocs/op
BenchmarkPinMiss/clock-6          2654425    447.1  ns/op    0 B/op   0 allocs/op
BenchmarkPinMiss/lru-2-6          2458190    500.3  ns/op    0 B/op   0 allocs/op
BenchmarkVictim/lru/64-6        144849998      8.605 ns/op   0 B/op   0 allocs/op
BenchmarkVictim/clock/64-6       85209267     14.25  ns/op   0 B/op   0 allocs/op
BenchmarkVictim/lru-2/64-6       10632694    108.0   ns/op   0 B/op   0 allocs/op
BenchmarkVictim/lru/1024-6      136074880      8.662 ns/op   0 B/op   0 allocs/op
BenchmarkVictim/clock/1024-6     91062385     13.41  ns/op   0 B/op   0 allocs/op
BenchmarkVictim/lru-2/1024-6       921991   1339     ns/op   0 B/op   0 allocs/op
```

**Đọc kết quả:** ba điều, hai trong đó ngược với dự đoán của tôi.

1. Một lần **hit** tốn ~50ns. Phase 0 đo `pread` 4KB cache lạnh **69µs**. Tỉ số **1366x** —
   nghĩa là một lần tránh miss trả tiền cho **hơn một nghìn** lần pin. Chi phí CPU của buffer
   pool là thứ không cần bàn tới, chỉ hit ratio mới đáng bàn.
2. `Victim` của LRU-2 tăng từ 108ns (64 frame) lên **1339ns** (1024 frame) — gấp **12.4x** khi
   frame gấp 16x. Nó quét toàn mảng, đúng như cài đặt. Ngoại suy: pool 400MB = 100k frame thì
   một lần chọn nạn nhân sẽ tốn ~130µs, **đắt hơn cả cú đọc đĩa mà nó định tiết kiệm**. Đây là
   lý do LRU-K trong DB thật không quét tuyến tính mà dùng heap/xấp xỉ.
3. CLOCK **không** rẻ hơn LRU khi chọn nạn nhân (14.25ns vs 8.605ns) — ngược hẳn giả thuyết G5.

### 2026-09-01 — chỗ CLOCK thật sự thắng không nằm ở một luồng

```console
$ go test ./internal/bufpool/ -run '^$' -bench 'PinHit' -benchmem -count=1 -cpu 1,6
BenchmarkPinHitParallel/lru        19846176     62.24 ns/op   0 B/op   0 allocs/op
BenchmarkPinHitParallel/lru-6       8444562    158.8  ns/op   0 B/op   0 allocs/op
BenchmarkPinHitParallel/clock      19945245     56.50 ns/op   0 B/op   0 allocs/op
BenchmarkPinHitParallel/clock-6     7872613    153.7  ns/op   0 B/op   0 allocs/op
```

**Đọc kết quả:** 6 luồng cùng pin **một** page nóng: 62ns → 159ns mỗi thao tác, tức
**2.55x chậm hơn** cho mỗi thao tác dù có gấp 6 lần lõi. Thông lượng tổng chỉ tăng
6/159 ÷ 1/62 = **2.3x** trên 6 lõi.

Và CLOCK **không** cứu được gì (153.7 vs 158.8 — chênh 3%). Vì trong thiết kế hiện tại, mọi
thao tác đều đi qua **một** `p.mu` chung; chính sách thay thế nằm *bên trong* khoá đó nên nó
rẻ hay đắt cũng không đổi được gì.

**Đang nghĩ gì:** lợi thế thật của CLOCK trong DB thật là *không cần sửa cấu trúc dữ liệu dùng
chung khi truy cập* — chỉ set một bit, có thể làm bằng atomic, không cần giữ latch của danh
sách LRU. Muốn đo được điều đó thì phải chia nhỏ `p.mu` trước (bảng tra phân mảnh). Chưa làm
được trong phase này -> ghi vào sổ nợ **P3-1**, kèm lệnh đo.

### 2026-09-01 — fuzz và chốt

```console
$ go test ./internal/bufpool/ -run '^$' -fuzz FuzzPoolOps -fuzztime 120s -fuzzminimizetime 1s
fuzz: elapsed: 2m0s, execs: 342407 (3647/sec), new interesting: 194 (total: 196)
PASS
ok  	minidb/internal/bufpool	120.138s
```

342 407 chương trình thao tác ngẫu nhiên (mỗi chương trình chạy trên **cả ba** chính sách,
`Verify()` sau từng thao tác) — không vỡ bất biến lần nào. Bài học phase 2 đã nằm sẵn trong
lệnh: `-fuzzminimizetime 1s`, và `execs/sec` là thứ phải nhìn, không phải chữ `PASS`.

Trong lúc dựng fuzz thì lộ ra một câu hỏi ngữ nghĩa mà tôi chưa nghĩ tới: **`FlushAll` có được
phép thất bại không?** Chương trình fuzz gọi `FlushAll` lúc `flushedLSN` còn thấp và nó trả
`ErrWALRule`. Đúng hay sai?

Đúng — và nó buộc tôi phải chốt hợp đồng của cái hook: `FlushLog(pageLSN)` nghĩa là **"hãy làm
cho log bền tới LSN này"**, không phải "kiểm tra hộ xem đã bền chưa". Người gọi thật (phase 5)
sẽ fsync log rồi trả `nil`; chỉ khi *không thể* mới trả lỗi, và lúc đó pool phải chịu thua chứ
không được ghi. Test fuzz dùng một hook chỉ-kiểm-tra nên phải chấp nhận `ErrWALRule` là câu trả
lời hợp lệ.

```console
$ go test ./... -count=1
ok  	minidb/internal/bufpool	0.806s
ok  	minidb/internal/page	3.739s
ok  	minidb/internal/pager	0.614s
```

---

## Giả thuyết sai

| Tôi tưởng là | Thực tế là | Lệnh + output đã lật tẩy | Đã sửa thế nào |
|---|---|---|---|
| Chặn WAL rule thì chỉ cần "trả ứng viên về hàng đợi rồi tìm cái khác" | Nếu **mọi** ứng viên đều bị chặn thì vòng đó quay vô hạn | `go test -run TestWALRule -timeout 10s` → `panic: test timed out after 10s` | Đếm số lần bị chặn, hết một vòng frame thì trả `ErrNoFrame` |
| Dựng workload scan là chuyện hiển nhiên | Vùng nóng (100 page) **nhỏ hơn** pool (64 frame) thì scan chẳng phá được gì | `lru không scan 0.873 \| lru có scan 0.827` → test tự FAIL vì tụt < 10% | Vùng nóng 200 page > 64 frame, s=1.05 → 0.755 → 0.636 |
| **G5**: CLOCK rẻ hơn LRU về CPU 1.5-3x | Chọn nạn nhân: CLOCK **đắt hơn** (14.25ns vs 8.605ns). Chỗ CLOCK thắng là *lúc truy cập* (45.4 vs 50.5ns), và lợi thế thật của nó là đa luồng — chưa đo được vì còn một `p.mu` chung | `BenchmarkVictim/clock/64 14.25 ns/op` vs `BenchmarkVictim/lru/64 8.605 ns/op`; `PinHitParallel/clock-6 153.7` vs `lru-6 158.8` (chênh 3%) | Ghi nợ **P3-1**: chia nhỏ bảng tra rồi đo lại |
| **G2**: dưới scan thì LRU-2 hơn LRU > 1.5x | Chỉ **1.28x** — và tỉ số này bão hoà, quét 50 page hay 1000 page cũng vậy | bảng "cường độ scan" của `cmd/bufferlab`: cột `lru-2/lru` = 1.26 → 1.29 → 1.28 → 1.28 → 1.27 | Giữ nguyên số thật; hiểu ra thiệt hại của scan bị chặn trên bởi **kích thước pool** |
| `rand.NewZipf(s<=1)` sẽ báo lỗi | Trả về **nil**, panic mãi tận lần `Uint64()` sau đó | `panic: rand: nil Zipf` trong `TestBeladyIsUpperBound` | `panic` có câu chữ rõ ngay trong `Zipf()` |
| `FlushAll` luôn thành công | Nó có thể bị chính WAL rule chặn — và **phải** bị chặn | `FlushAll: vi phạm WAL rule — page 8 pageLSN=90` trong `TestPoolProgramStress` | Chốt hợp đồng của `FlushLog`; fuzz chấp nhận `ErrWALRule` |

## Số đo — kết bằng tỉ số

Máy: WSL2 / i5-1235U / GOMAXPROCS=6, ngày 2026-09-01, commit của phase này.
Lệnh: `go run ./cmd/bufferlab`, `go test ./internal/bufpool/ -run '^$' -bench . -benchmem -count=1 -cpu 1,6`.

| Tỉ số | Giá trị | Nghĩa thiết kế |
|---|---|---|
| hit ~50ns / `pread` cache lạnh 69µs (phase 0) | **1366x** | chi phí CPU của buffer pool là chuyện vặt; chỉ hit ratio đáng bàn |
| OPT / LRU trên workload uniform | **7.2x** (0.229 / 0.032) | không có locality thì chỉ khả năng biết trước tương lai mới cứu được |
| LRU trên zipf / trên uniform | **23x** (0.755 / 0.032) | *locality*, chứ không phải kích thước RAM, mới là thứ tạo ra hit ratio |
| LRU-2 / LRU dưới sequential scan (vùng nóng) | **1.28x** (0.815 / 0.636) | và tỉ số này **không đổi** theo độ dài lần quét |
| LRU có scan / không scan | **0.84x** (0.636 / 0.755) | scan lấy đi 16% hit ratio của vùng nóng, bão hoà ngay từ lần quét ngắn |
| LRU-2 có scan / không scan | **1.00x** (0.815 / 0.817) | LRU-K miễn nhiễm, không phải "đỡ hơn" |
| `Victim` LRU-2 ở 1024 frame / ở 64 frame | **12.4x** (1339ns / 108ns) khi frame ×16 | LRU-K quét tuyến tính là O(n) — pool thật phải dùng heap |
| `Victim` LRU-2 ở 1024 frame / `pread` lạnh | **0.019x** (1.34µs / 69µs) | vẫn còn rẻ chán ở quy mô này; hỏng ở quy mô ~100k frame |
| Thông lượng 6 luồng / 1 luồng, cùng một page nóng | **2.3x** trên 6 lõi | một `p.mu` chung là nút cổ chai; DB thật chia mảnh bảng tra |
| LRU-2 ở 8 frame / LRU ở 8 frame | **1.43x** (0.454 / 0.318), bằng với gấp đôi RAM | **đổi chính sách rẻ hơn mua RAM** |

## Bất biến + lệnh kiểm chứng

| Bất biến | Cài ở | Lệnh kiểm chứng | Kết quả |
|---|---|---|---|
| Không bao giờ đuổi page đang pin | `bufpool.go:victim` + `Verify` I4 | `go test ./internal/bufpool -run TestPinnedPageNeverEvicted -v` | PASS ×3 chính sách |
| Một PageID không nằm ở hai frame | `verify.go:Verify` I2 | `go test ./internal/bufpool -run TestRandomOpsKeepInvariants` | 50 000 thao tác ×3 |
| Bảng tra và frame khớp hai chiều | `verify.go:Verify` I1 | `go test ./internal/bufpool -run TestVerifyCatchesBrokenTable -v` | checker bắt được khi cố tình bẻ |
| Cờ bẩn chỉ mất đi khi page đã được ghi | `bufpool.go:writeFrame` | `go test ./internal/bufpool -run TestUnpinCleanDoesNotClearDirty` | PASS |
| Page sạch không bao giờ bị ghi | `bufpool.go:victim` | `go test ./internal/bufpool -run TestCleanPageIsNeverWritten` | `store.Writes = 0` |
| WAL rule: không ghi page có `pageLSN` chưa fsync | `bufpool.go:writeFrame` | `go test ./internal/bufpool -run TestWALRuleBlocksDirtyEviction -v` | thà `ErrNoFrame` còn hơn ghi |
| Nội dung page qua pool = nội dung trên đĩa sau `FlushAll` | `bufpool.go:FlushAll` | `go test ./internal/bufpool -run TestPoolRoundTrips` | so từng byte sau khi mở lại file |
| Không rò rỉ pin khi chạy đa luồng | `Pin`/`Unpin` | `go test ./internal/bufpool -race -run TestConcurrentPinUnpin -v` | `-race` sạch, `PinnedCount()=0` |

## Đọc gì trong phase này

- CMU 15-445, lecture *Buffer Pools* — phần LRU-K và "sequential flooding".
- O'Neil & O'Neil, *The LRU-K Page Replacement Algorithm* (1993) — chỗ định nghĩa khoảng cách
  lùi K và cách phá hoà khi chưa đủ K lần truy cập.
- Belady (1966) — thuật toán tối ưu offline, dùng ở đây làm **trần**, không phải để cài thật.

## Rút ra

**1. Vì sao không dùng LRU thuần.** Không phải vì LRU "kém", mà vì nó đo **nhầm đại lượng**: nó
đo *thời điểm chạm gần nhất*, trong khi thứ dự đoán được tương lai là *tần suất*. Một lần
sequential scan tạo ra hàng nghìn page có "thời điểm chạm gần nhất" rất mới nhưng tần suất bằng
1 — và LRU không phân biệt nổi chúng với các page nóng thật. Số đo: vùng nóng tụt từ 0.755 xuống
0.636 và **tụt hết ngay ở lần quét 50 page**; quét dài hơn cũng không tệ hơn, vì một lần quét
dài bằng số frame đã dọn sạch pool rồi.

**2. CLOCK xấp xỉ LRU tới đâu.** Rất sát: 0.744 vs 0.755 trên zipf, 0.634 vs 0.636 dưới scan —
chênh dưới 1.5%. Chỗ nó *không* xấp xỉ được cũng chính là chỗ LRU sai: CLOCK vẫn chỉ có **một
bit** thông tin về mỗi page, nên nó cũng không phân biệt được "chạm một lần" với "chạm liên tục".
Nó là LRU rẻ hơn, không phải LRU tốt hơn. Và cái "rẻ hơn" ấy tôi **chưa đo được** trong bản này
(xem P3-1) — ở một luồng thì nó còn đắt hơn LRU lúc chọn nạn nhân.

**3. LRU-K chống scan bằng gì.** Bằng đúng một thay đổi: nhớ **K** mốc truy cập thay vì 1. Page
chưa đủ K lần chạm bị coi là "khoảng cách lùi vô hạn" và bị đuổi trước tất cả. Nghĩa là nó chia
page thành hai hạng — *đã chứng minh được mình* và *chưa* — thứ mà một bit của CLOCK hay một mốc
của LRU không biểu diễn nổi. Kết quả: 0.817 → 0.813 qua toàn dải cường độ scan, coi như miễn nhiễm.

**4. Còn cách tối ưu bao xa.** LRU đạt 86% của Belady trên zipf, 72% dưới scan, và **14%** trên
uniform. Con số 14% mới là con số đáng nhớ: nó nói rằng phần lớn "trí tuệ" trong bài toán này
nằm ở việc *biết trước tương lai*, và mọi chính sách online đều chỉ đang đoán. Cũng vì thế mà
hướng đi thật của DB hiện đại không phải là bịa ra chính sách tinh vi hơn, mà là **cho tầng trên
nói ra ý định của nó** — Postgres dùng *ring buffer* cho seq scan chính là để tự khai báo
"tôi đang quét, đừng giữ mấy page này".

**5. Chi phí CPU có đáng kể không.** Không, và không phải một chút. Một lần pin trúng tốn ~50ns,
một lần đọc đĩa lạnh tốn 69µs — **1366x**. Nghĩa là được phép tiêu tới cả nghìn lần pin cho một
lần giảm miss. Nhưng "không đáng kể" là một tuyên bố có **điều kiện**, và điều kiện đó gãy ở
LRU-K: `Victim` của nó là O(số frame), 108ns ở 64 frame nhưng 1.34µs ở 1024 frame; ngoại suy tới
pool 400MB thì mỗi lần chọn nạn nhân đắt ngang một lần đọc đĩa. Đó chính là lý do LRU-K trong DB
thật không bao giờ quét tuyến tính.

**6. Vì sao "không evict page đang pin" và "không evict page bẩn chưa fsync WAL" là hai bất biến
khác nhau.** Chúng bảo vệ hai thứ hoàn toàn khác nhau, ở hai thang thời gian khác nhau:

- *Pin* bảo vệ **tính đúng đắn trong RAM, ngay lúc này**: `Pin` trả về con trỏ thẳng vào arena,
  đuổi page đang pin ra là tầng trên đang đọc/ghi vào nội dung của một page khác. Vi phạm nó
  hỏng **ngay**, và một `Verify()` sau mỗi thao tác nhìn thấy được.
- *WAL rule* bảo vệ **khả năng phục hồi sau khi tiến trình đã chết**: ghi page trước khi log của
  nó bền thì lúc crash, trên đĩa có một thay đổi mà không có gì để undo nó. Vi phạm nó **không
  hỏng gì cả** cho tới lần crash tiếp theo — không test nào trong RAM nhìn thấy được, chỉ có
  crash test của phase 5 mới thấy.

Cái thứ nhất là **latch**, cái thứ hai là **giao thức**. Trộn hai thứ đó làm một là đường thẳng
dẫn tới một DB chạy rất tốt cho tới lúc mất điện.

## Nợ kỹ thuật

Chi tiết + lệnh trả từng món: [`../docs/debts.md`](../docs/debts.md).

- [ ] **P3-1** — một `p.mu` chung; chưa đo được lợi thế đa luồng thật của CLOCK
- [ ] **P3-2** — giữ `p.mu` trong suốt lúc đọc đĩa: mọi miss nối đuôi nhau
- [ ] **P3-3** — `Victim` của LRU-K là O(n), hỏng ở pool lớn
- [ ] **P3-4** — chưa có prefetch / ring buffer cho sequential scan
- [ ] **P3-5** — chạy lại bench của phase 3 trên Linux thuần
