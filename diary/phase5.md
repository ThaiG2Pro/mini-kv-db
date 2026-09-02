# Phase 5 — WAL + recovery (ARIES-lite)

- **Thời lượng dự kiến:** 3-4 ngày · **thực tế:** 2 ngày
- **Bắt đầu:** 2026-09-01 · **Kết thúc:** 2026-09-02
- **Trạng thái:** ✅ xong
- **Commit:** `6ac4450`

> **Quy tắc ghi nhật ký:** mọi con số, mọi kết luận đều phải kèm **lệnh shell sinh ra nó**
> và **output thật** (dán nguyên, không tóm tắt). Sáu tháng sau đọc lại phải chạy lại được.
> Ghi trong lúc làm, không phải sau khi xong.

## Môi trường

```console
$ uname -srmo && go version && df -hT . | tail -1
Linux 6.6.87.2-microsoft-standard-WSL2 x86_64 GNU/Linux
go version go1.26.2 linux/amd64
/dev/sdd       ext4 1007G   40G  917G   5% /
```

CPU: `12th Gen Intel(R) Core(TM) i5-1235U`, `GOMAXPROCS=6` (theo dòng `cpu:` mà `go test -bench` in ra).

⚠️ Đây là WSL2 trên ext4 trong file ảnh đĩa. `fsync` ở đây **không** phải `fsync` trên NVMe thật:
xem P0-1/P0-4 trong [`docs/debts.md`](../docs/debts.md). Mọi kết luận dưới đây đều phát biểu bằng
**tỉ số**, chính vì lý do này.

## Mục tiêu phase

Write-ahead log, pageLSN, checkpoint, và recovery 3 pha: Analysis → Redo → Undo.
Kèm theo: trả những món nợ của phase trước mà **phải** có WAL mới quyết được
(P1-1 page mồ côi, P1-2 transaction thật, P2-3 torn page, P4-4 root đổi PageID).

## Câu hỏi phải trả lời được khi xong

- Phát biểu chính xác WAL rule. Nó được thực thi ở dòng code nào?
- Vì sao pha Redo phải *repeat history*, redo cả txn sẽ bị abort?
- CLR giải quyết vấn đề gì? (gợi ý: crash trong lúc đang undo)
- Fuzzy checkpoint khác consistent checkpoint ra sao, vì sao chọn fuzzy?
- Group commit làm throughput tăng bằng cách nào? (số đo đã có từ phase 0)

Ba câu tự thêm trong lúc làm, vì chúng chặn đường:

- LSN nên là số thứ tự hay là **offset byte**? Chọn cái nào thì mất gì?
- Một page bị **torn** thì `pageLSN` của nó là rác — mà redo lại dựa vào `pageLSN` để quyết định
  bỏ qua. Vòng lặp chết này thoát bằng gì?
- Freelist của pager có cần được log không? Nếu không thì nó xung đột với WAL ở đâu?

## Deliverable (bằng chứng đã hiểu)

Hai bài, và **bài thứ hai mới làm bài thứ nhất có nghĩa**:

1. `make crashlab-full` — 200 lần `kill -9` ở thời điểm ngẫu nhiên, durability không sai lần nào.
2. `make crashlab-nowrite` — cùng bộ kiểm tra ấy, nhưng giữ byte log trong RAM tiến trình.
   Bài này **phải đỏ**. Nếu nó xanh thì bài (1) không kiểm được gì cả.

## Reproduce toàn bộ phase này

```bash
git checkout 6ac4450
make test                 # 167 test, gồm 12 test của internal/db và 7 của internal/wal
make vet fmt

# Deliverable
make crashlab             # 20 vòng, bản nhanh
make crashlab-full        # 200 vòng — bài chốt phase
make crashlab-nowrite     # PHẢI đỏ: chứng minh bộ kiểm tra biết báo SAI
make crashlab-nosync      # tắt fsync mà VẪN xanh — xem mục "kill -9 không phải mất điện"

# Số đo
make bench-wal            # insert / recovery / read path / diff
make wallab               # soi một file log thật: gồm gì, bao nhiêu phần trăm là thuế
make fuzz-db              # 120s fuzz chuỗi thao tác + crash + mở lại
go test -race ./internal/...
```

---

## Nhật ký

### 2026-09-01 — dựng WAL, journal, ba pha recovery (turn code)

Thứ tự dựng có chủ ý: `internal/wal` (record + diff + log) → `internal/pager/wal.go` (phần cấp
phát mà WAL cần) → `internal/btree/journal.go` (móc, `J == nil` thì y hệt phase 4) →
`internal/db` (transaction + checkpoint + recovery).

Ba quyết định chốt ngay từ đầu, vì đổi sau thì phải viết lại nhiều:

**LSN là offset byte trong file log**, không phải số thứ tự (kiểu Postgres). Được: `Read(lsn)` là
một `pread`, không cần bảng tra LSN→offset. Mất: không cắt được đầu log mà không đổi LSN, và
`FirstLSN = 32` (độ dài header file log) nên LSN 0 tự nhiên là "không hợp lệ" — page mới toanh có
`pageLSN = 0` nghĩa là "chưa từng bị ghi".

**Ba mốc LSN phải tách bạch**, không được nhập lại thành một:

| mốc | nghĩa | ai so với nó |
|---|---|---|
| `end` | đã cấp LSN, byte còn trong buffer RAM | `Append` |
| `written` | `write(2)` xong, đang ở page cache — **chưa bền** | không ai |
| `flushed` | đã `fsync` | **WAL rule so với mốc này** |

Nhập `written` với `flushed` là cách phổ biến nhất để có một cái WAL trông đúng mà không bền.
Mục "kill -9 không phải mất điện" dưới đây cho thấy tại sao lỗi ấy khó bị phát hiện.

**Logging là physiological, diff theo khối 32 byte.** Một lần chèn vào slotted page động vào ba
vùng rời nhau (header, một slot, một cell), nên ghi từng vùng rẻ hơn hẳn ghi trọn 4KB. Payload
mang **hai** danh sách đoạn (`nB | segsB | nA | segsA | before | after`) vì ảnh trọn page chỉ cần
trọn ở phía **redo** — undo chạy sau redo, lúc đó page đã lành.

**WAL rule chỉ được thực thi ở đúng một chỗ:** `bufpool.Pool.FlushLog`, gọi từ `writeFrame` —
chỗ DUY NHẤT một page rời khỏi RAM.

```console
$ go test ./internal/db -run TestWALRuleHolds -count=1 -v
=== RUN   TestWALRuleHolds
--- PASS: TestWALRuleHolds (0.02s)
PASS
ok  	minidb/internal/db	0.024s
```

**Đọc kết quả:** một điểm thực thi, một test. Nếu invariant này cần hai chỗ để đúng thì nó sẽ
sai ở chỗ thứ ba.

**Đang nghĩ gì:** phần khó không phải ba pha ARIES — sách viết rõ. Phần khó là chỗ nối: cây B+
biết page nào bẩn, journal biết byte nào đổi, pager biết page nào còn sống. Ba cái đó không đồng ý
với nhau thì recovery dựng lại một cái cây không tồn tại.

---

### 2026-09-01 — "cờ dirty của cây là gợi ý, byte mới là sự thật"

Bug tốn nhiều thời gian nhất của turn code. `Abort` **im lặng làm mất lệnh xóa**, 8/12 seed.

Cách khoanh vùng: ma trận 2×2 (abort × crash), vì tôi tưởng đây là bug của recovery.

| | không crash | có crash |
|---|---|---|
| `abort=false` | 0/12 sai | 0/12 sai |
| `abort=true` | **8/12 sai** | **8/12 sai** |

**Đọc kết quả:** giống nhau hoàn toàn giữa hai cột → **không liên quan gì đến crash**. Đây là bug
của `Abort` lúc đang chạy, tức là của chuỗi undo, tức là của cái *record* mà nó đi tìm. Cột crash
chỉ là chỗ tôi đi tìm sai suốt nửa tiếng.

Nguyên nhân: một leaf bị xóa một khóa **rồi bị gộp đi** trong cùng transaction được thả pin với
`dirty=false`. Cây nói không đổi (đúng, theo cách cây hiểu: nó sắp bỏ page này), nên journal không
ghi record UPDATE nào, nên `Abort` không có gì để quay ngược.

Sửa: `Journal.PageOut` trả về **các byte có đổi thật không**, `emit` bỏ hẳn gợi ý `dirty` của cây,
và `t.unpin` nâng `dirty` lên true khi journal nói có đổi.

```go
// internal/btree/btree.go
if t.J != nil && t.J.PageOut(id, dirty) { dirty = true }
```

**Đang nghĩ gì:** đây là bài học chung, không phải chi tiết vụn. Cờ `dirty` là *tối ưu* của tầng
trên cho tầng dưới; nó được phép bảo thủ sai theo hướng "quên bật". WAL thì không được phép sai
theo hướng đó. Nên WAL phải có nguồn sự thật riêng: so byte.

---

### 2026-09-01 — bốn cái bug, cùng một nguyên nhân gốc

Bốn lần sửa riêng lẻ, mãi tới lần thứ tư mới nhìn ra chúng là **một** vấn đề:

> **WAL bảo vệ page dữ liệu. Freelist của pager thì không. Mà hai bên dùng chung một không
> gian PageID.**

| biểu hiện | sửa nhầm triệu chứng | nguyên nhân thật |
|---|---|---|
| `đọc freelist page N: EOF`, 4/20 vòng crashlab | — | page chứa freelist nằm trong `pager.pending`, mà WAL rút `pending` ra ở lúc **commit txn** chứ không ở lúc ghi meta → host bị cấp lại trong khi meta bền vẫn đang trỏ vào nó |
| `page 44 kiểu 10 không phải node B+Tree` | | ↑ cùng một cái |
| recovery làm hỏng cây (16 frame, có xóa, có txn béo) | thử pool 1024 frame → xanh; bỏ checkpoint cuối → xanh (cả hai đều chỉ che bug) | recovery đăng ký `Reserve` rồi `FreeNow` bằng **hai vòng rời** → page bị free ở LSN 100 rồi cấp lại ở LSN 200 rơi vào freelist trong khi đang sống |
| redo ghi đè page chứa freelist | | page free từ lâu, pager đã lấy làm host, nhưng log cũ vẫn còn record UPDATE cho nó — và chốt `pageLSN >= rec.LSN` vô dụng vì page freelist **không có** pageLSN |

Ba thứ chốt lại:

1. `metaPending` — danh sách page chứa freelist của meta **hiện tại**, chỉ được nhả ở cuối lần
   `CommitMeta` **tiếp theo**.
2. `skipRedo` = `metaPending` ∪ những page mà **sự kiện cuối cùng** về nó là FREE đã commit.
   Kèm với đó, recovery dựng lại trạng thái cấp phát bằng **một** lượt `lastEv map[PageID]pageEvent`
   thay vì hai vòng rời.
3. `bufpool.Discard(id)` — bỏ frame khỏi pool mà không ghi và không chạm allocator; gọi trước
   `pg.FreeNow(id)` trong cả `Txn.Abort` lẫn `recover()`.

Và một chốt chặn **thường trực** trong `pager.WritePage`:

```go
if p.inMetaPending(id) {
    return fmt.Errorf("%w: page %d đang là page chứa freelist của meta hiện tại", ErrMetaPageBusy, id)
}
```

**Đang nghĩ gì:** chốt chặn này là thứ đáng giá nhất trong cả phase. Cách tôi tìm ra bug cuối cùng
là cắm tạm một `panic` (biến môi trường `MINIDB_GUARD`) vào đúng chỗ ghi, và stack trace chỉ thẳng
vào `bufpool.victim → writeFrame`. Nó biến một triệu chứng xuất hiện **sau đó hàng trăm
millisecond và ở một file khác** (`đọc freelist page 4096: EOF`) thành một dòng số. Giữ nó lại
dưới dạng error, không phải panic.

---

### 2026-09-02 — chạy hết, và bộ đo tự lộ ra nhiều lỗi hơn code (turn run & fix)

```console
$ go test ./... -count=1
ok  	minidb/internal/btree	0.469s
ok  	minidb/internal/bufpool	0.978s
ok  	minidb/internal/db	20.139s
ok  	minidb/internal/page	4.347s
ok  	minidb/internal/pager	1.119s
ok  	minidb/internal/wal	0.140s

$ go test -race ./internal/... -count=1
ok  	minidb/internal/btree	17.973s
ok  	minidb/internal/bufpool	30.794s
ok  	minidb/internal/db	55.050s
ok  	minidb/internal/page	33.331s
ok  	minidb/internal/pager	2.281s
ok  	minidb/internal/wal	1.131s
```

**Đọc kết quả:** xanh, nhưng "xanh" chưa nói được gì cho tới khi biết bộ đo có nhạy hay không.
Phần còn lại của ngày là đi kiểm tra chính các bộ đo — và chúng hỏng nhiều hơn code.

---

### 2026-09-02 — fuzz "đứng hình" 40 giây: bài học cũ, học lại lần hai

```console
$ go test ./internal/db -run=NONE -fuzz=FuzzCrashRecover -fuzztime=120s
fuzz: elapsed: 6s, execs: 225 (16/sec), new interesting: 0 (total: 140)
fuzz: elapsed: 9s, execs: 225 (0/sec), new interesting: 0 (total: 140)
...
fuzz: elapsed: 39s, execs: 225 (0/sec), new interesting: 0 (total: 140)
...
fuzz: elapsed: 2m2s, execs: 794 (0/sec), new interesting: 41 (total: 256)
PASS
```

Bốn giả thuyết, ba sai:

1. *Một exec chậm bệnh hoạn.* → Cắm `panic` nếu một exec quá 5 giây. **Panic không hề nổ.** Vậy
   worker không kẹt trong fuzz target.
2. *6 worker tranh nhau fsync trên WSL2.* → `-parallel=1`, vẫn đứng 20 giây. Sai.
3. *fsync/writeback.* → `NoSync: true`, bùng lên 425/sec rồi vẫn đứng. Sai.
4. Nhìn lại log: **mỗi lần đứng đều ngay sau một dòng `new interesting`.** Đó là bộ **rút gọn**
   (minimizer) của Go, mặc định `-fuzzminimizetime=1m`, và exec của nó không được đếm.

```console
$ go test ./internal/db -run=NONE -fuzz=FuzzCrashRecover -fuzztime=60s -fuzzminimizetime=2s
fuzz: elapsed: 9s, execs: 398 (40/sec), new interesting: 4 (total: 154)
fuzz: elapsed: 21s, execs: 720 (34/sec), new interesting: 18 (total: 168)
fuzz: elapsed: 33s, execs: 843 (3/sec), new interesting: 33 (total: 183)
fuzz: elapsed: 45s, execs: 1139 (37/sec), new interesting: 45 (total: 195)
fuzz: elapsed: 57s, execs: 1179 (4/sec), new interesting: 61 (total: 211)
PASS
```

**Đọc kết quả:** hết đứng, và 60 giây tìm được **61** input mới thay vì 6 trong 120 giây — hơn
**20x** hiệu suất khám phá. Không có bug nào trong code cả.

**Đang nghĩ gì:** phần đáng ghi nhất là chỗ này — cái bẫy ấy **đã nằm trong repo từ phase 2**:

```console
$ grep -n fuzzminimizetime README.md Makefile
README.md:make fuzz                           # phase 2: fuzz slotted page 120s (chú ý -fuzzminimizetime)
Makefile:	go test ./internal/db/ -run '^$' -fuzz FuzzCrashRecover -fuzztime 120s -fuzzminimizetime 1s
```

Target `fuzz-db` trong Makefile **đã có** cờ đúng. Tôi mất năm phút vì gõ `go test` bằng tay thay
vì `make fuzz-db`. Bài học không phải về Go fuzzing, mà về việc: đã bỏ công gói một lệnh vào
Makefile thì đừng chạy vòng qua nó.

```console
$ go test ./internal/db/ -run '^$' -fuzz FuzzCrashRecover -fuzztime 120s -fuzzminimizetime 1s
fuzz: elapsed: 2m2s, execs: 794 (0/sec), new interesting: 41 (total: 256)
PASS
ok  	minidb/internal/db	122.230s
```

Corpus đi từ 146 lên **256** mẫu, không lần nào sai.

---

### 2026-09-02 — `benchRecover` đo checkpoint mà tôi tưởng đang đo recovery

```console
$ go test ./internal/db/ -run '^$' -bench 'Insert|Recover' -benchtime=2000x -timeout 30m
BenchmarkRecoverNoCkpt-6     	    2000	  64096861 ns/op	     11491 logKiB	         0 redo	         0 skip
BenchmarkRecoverCkpt1M-6     	    2000	  46350185 ns/op	     11593 logKiB	         0 redo	         0 skip
BenchmarkRecoverCkpt64K-6   	--- (hết 10 phút, tôi cắt)
```

**Đọc kết quả:** `redo 0`. Một bài đo recovery mà redo **không** áp dụng record nào thì nó đang đo
cái khác. Đây đúng bẫy số 5 của SKILL: thấy số vô lý thì nghi bench trước, đừng nghi máy.

Nguyên nhân: `Open()` **kết thúc** recovery bằng một checkpoint. Vòng lặp `for i := 0; i < b.N; i++`
mở rồi crash lại cùng một file, nên từ vòng **thứ hai** trở đi master record đã nằm ở cuối log,
không còn gì để redo. `b.N-1` vòng còn lại đo chi phí của checkpoint.

Sửa: chụp đúng cặp file (`data.db` + `.wal`) ở khoảnh khắc crash, dán chúng về chỗ cũ trước mỗi
vòng, đồng hồ tạm dừng. Và `-benchtime=2000x` là vô nghĩa cho một bài 100ms/op → tách target riêng
`-benchtime=10x`.

```console
$ go test ./internal/db/ -run '^$' -bench 'Recover' -benchtime=10x -timeout 30m
BenchmarkRecoverNoCkpt-6    	      10	 142748398 ns/op	     11491 logKiB	     21725 redo	         0 skip
BenchmarkRecoverCkpt1M-6    	      10	  90260761 ns/op	     11593 logKiB	     13033 redo	        63.00 skip
BenchmarkRecoverCkpt64K-6   	      10	 102961883 ns/op	     13214 logKiB	     13059 redo	        37.00 skip
BenchmarkRecoverCleaner-6   	      10	  66931825 ns/op	     11653 logKiB	         0 redo	         0 skip
```

**Đọc kết quả:** giờ số có nghĩa, và nó nói một điều tôi không lường:
**checkpoint 1MB và checkpoint 64KB cho redo GIỐNG NHAU** (13033 vs 13059). Tăng tần suất
checkpoint **16 lần** mà không cắt được record redo nào.

Vì `redoLSN = min(recLSN)` trên toàn bảng page bẩn, và checkpoint tự động là **mờ** (không flush):

```console
$ grep -n 'return d.checkpointLocked' internal/db/checkpoint.go
25:	return d.checkpointLocked(false)
33:	return d.checkpointLocked(true)
44:	return d.checkpointLocked(false)      # <- maybeCheckpoint
```

Root và các node trong nằm mãi trong pool, bẩn liên tục, `recLSN` của chúng không tiến. Nên
`redoLSN` bị **ghim**, và checkpoint mờ dù dày đến đâu cũng không kéo nó lên.

Để biến suy luận này thành số, thêm điểm đo thứ tư: `BenchmarkRecoverCleaner` — cùng tần suất
checkpoint như `Ckpt1M`, chỉ khác là cứ 2000 txn thì ép một checkpoint **có** flush, đóng vai
người dọn page (page cleaner) mà minidb chưa có. Kết quả: **redo 0**.

**Đang nghĩ gì:** đây là lý do các DB thật có **background page cleaner** tách khỏi checkpoint.
Checkpoint mờ trả lời câu "đọc log từ đâu"; nó **không** trả lời câu "đọc bao nhiêu". Cái thứ hai
do người dọn page quyết. Ghi thành nợ P5-1.

---

### 2026-09-02 — `bench-wal` trỏ vào những benchmark không tồn tại

```console
$ go test ./internal/wal/ -run '^$' -bench . -benchmem
PASS
ok  	minidb/internal/wal	0.010s
```

**Đọc kết quả:** `PASS` với 0 benchmark. Cả thiết kế physiological logging đứng trên hằng số
`DiffGran` mà chưa từng có một dòng số nào về nó. Viết 8 benchmark, và cái đầu tiên đã nói chuyện:

```console
$ go test ./internal/wal/ -run '^$' -bench . -benchmem
BenchmarkDiffGran16-6       	  355807	      3443 ns/op	        80.00 dataB	         3.000 seg	        51.20 x-vs-fullpage	      16 B/op	       1 allocs/op
BenchmarkDiffGran32-6       	  348818	      3193 ns/op	       128.0 dataB	         3.000 seg	        32.00 x-vs-fullpage	      16 B/op	       1 allocs/op
BenchmarkDiffGran64-6       	  386835	      3299 ns/op	       192.0 dataB	         3.000 seg	        21.33 x-vs-fullpage	      16 B/op	       1 allocs/op
BenchmarkDiffGran128-6      	  381878	      3215 ns/op	       384.0 dataB	         2.000 seg	        10.67 x-vs-fullpage	       8 B/op	       1 allocs/op
BenchmarkDiffIdentical-6    	  405228	      3027 ns/op	       0 B/op	       0 allocs/op
BenchmarkEncodePayload-6    	36916476	        34.39 ns/op	       284.0 payloadB	       0 B/op	       0 allocs/op
BenchmarkEncodeFullPage-6   	23882408	        52.42 ns/op	      4244 payloadB	       0 B/op	       0 allocs/op
BenchmarkApply-6            	80868369	        16.21 ns/op	       0 B/op	       0 allocs/op
```

**Đọc kết quả:** hai điều vô lý.

1. **Thời gian gần như không đổi theo `gran`** (3443 / 3193 / 3299 / 3215). Nếu `gran` là một đánh
   đổi thật thì gran 128 phải nhanh hơn gran 16 khoảng 8 lần vì gọi ít lần so hơn 8 lần. Nó không
   → chi phí không nằm ở số lần gọi, mà ở **bên trong** phép so.
2. 3µs để so 4096 byte = **1.3 GB/s**. Quá chậm cho một phép so bộ nhớ.

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

Vòng lặp từng byte, viết tay, trong khi `bytes.Equal` là assembly SIMD. Và chỗ này nằm trên đường
ghi **nóng nhất**: mỗi `Put` pin cả đường từ root xuống lá, các node trong **không đổi gì** nhưng
vẫn bị diff (`BenchmarkDiffIdentical` = 3µs mỗi cái).

```console
$ go test ./internal/wal/ -run '^$' -bench 'Diff' -benchmem
BenchmarkDiffGran16-6      	 1000000	      1188 ns/op	        80.00 dataB	         3.000 seg	        51.20 x-vs-fullpage	      16 B/op	       1 allocs/op
BenchmarkDiffGran32-6      	 1536907	       914.2 ns/op	       128.0 dataB	         3.000 seg	        32.00 x-vs-fullpage	      16 B/op	       1 allocs/op
BenchmarkDiffGran64-6      	 2944390	       466.3 ns/op	       192.0 dataB	         3.000 seg	        21.33 x-vs-fullpage	      16 B/op	       1 allocs/op
BenchmarkDiffGran128-6     	 4053180	       287.3 ns/op	       384.0 dataB	         2.000 seg	        10.67 x-vs-fullpage	       8 B/op	       1 allocs/op
BenchmarkDiffIdentical-6   	 1388196	       857.5 ns/op	       0 B/op	       0 allocs/op
```

**Đọc kết quả:** `Diff` ở gran 32 nhanh **3.5x** (3193 → 914 ns). Và bây giờ `gran` mới **thật sự**
là một đánh đổi: gran 16 tốn 1188 ns cho 80 byte log, gran 128 tốn 287 ns cho 384 byte. Trước khi
sửa, bảng số nói dối rằng gran nhỏ là lựa chọn miễn phí.

Đường ghi hưởng trực tiếp:

```console
$ go test ./internal/db/ -run '^$' -bench 'Insert' -benchtime=2000x -timeout 30m
BenchmarkInsertBatch1-6      	    2000	   1372130 ns/op	         1.000 fsync/op	...
BenchmarkInsertBatch10-6     	    2000	    162247 ns/op	         0.1000 fsync/op...
BenchmarkInsertBatch100-6    	    2000	     25153 ns/op	         0.01000 fsync/o...
BenchmarkInsertBatch1000-6   	    2000	      5260 ns/op	         0.001000 fsync/...
BenchmarkInsertNoSync-6      	    2000	      3025 ns/op	         0 fsync/op	    ...
```

`InsertNoSync` 11154 → **3025 ns/op = 3.7x**; `Batch1000` 9842 → 5260 = 1.87x. Đúng như dự đoán:
càng bỏ fsync ra khỏi phép đo thì phần `Diff` càng lộ.

**Đang nghĩ gì:** giữ `DiffGran = 32`. Nó không còn là "chỗ cân bằng" theo cảm tính nữa mà là một
điểm trên đường cong đã đo: 914 ns cho 128 byte log. Đổi sang 64 tiết kiệm 448 ns CPU nhưng tốn
thêm 64 byte log **mỗi page mỗi lần ghi** — và byte log là byte phải fsync.

---

### 2026-09-02 — đường đọc có trả gì cho WAL không: 1.34x hay 1.03x?

Chú thích trong `bench_test.go` nói "đường đọc không được trả một xu nào cho WAL. Nếu hai con số
này lệch nhau thì journal đang bật nhầm ở đường đọc" — và so `BenchmarkGetWithWAL` với
`BenchmarkGetNoWAL`. `BenchmarkGetNoWAL` **không tồn tại**. Lời khẳng định chưa từng được đo.

Viết nó. Lần chạy đầu:

```console
$ go test ./internal/db/ -run '^$' -bench 'Get' -benchtime=200000x
BenchmarkGetWithWAL-6   	  200000	       466.9 ns/op
BenchmarkGetNoWAL-6     	  200000	       349.6 ns/op
```

1.34x. Suýt ghi vào diary như một phát hiện. Nhưng `DB.Get` còn một `d.mu.Lock()` mà cây trần
không có, nên phép so này lẫn hai thứ. Thêm điểm đo giữa (`d.tree.Get` trực tiếp, không qua khóa)
và chạy `-count=3`:

```console
$ go test ./internal/db/ -run '^$' -bench 'Get' -benchtime=300000x -count=3
BenchmarkGetWithWAL-6      	  300000	       382.0 ns/op
BenchmarkGetWithWAL-6      	  300000	       385.9 ns/op
BenchmarkGetWithWAL-6      	  300000	       373.3 ns/op
BenchmarkGetNoWAL-6        	  300000	       359.2 ns/op
BenchmarkGetNoWAL-6        	  300000	       363.4 ns/op
BenchmarkGetNoWAL-6        	  300000	       356.3 ns/op
BenchmarkGetWithWALRaw-6   	  300000	       368.8 ns/op
BenchmarkGetWithWALRaw-6   	  300000	       370.1 ns/op
BenchmarkGetWithWALRaw-6   	  300000	       364.8 ns/op
```

**Đọc kết quả:** 359 → 368 (**1.03x**, journal) → 380 (**1.06x**, thêm `d.mu`). Con số 1.34x là
**nhiễu của một lần chạy**, không phải phát hiện. `-count=1` trên một bài 400ns không đủ để kết luận gì.

Và vì tỉ số 3% thì đổi theo máy, chốt lại bằng một khẳng định không phụ thuộc máy — 0 là 0:

```console
$ go test ./internal/db -run TestReadPathTakesNoSnapshot -count=1 -v
=== RUN   TestReadPathTakesNoSnapshot
--- PASS: TestReadPathTakesNoSnapshot (0.02s)
PASS
```

Một bẫy nữa cùng chỗ: ở `-benchtime=2000x`, `Get` ra **5109 ns/op** — vì 2000 vòng lặp trên 20000
khóa thì mỗi khóa chỉ được tra một lần, tức là đang đo đọc đĩa. Phải chạy đủ lâu để page nóng lên.
Tách thành dòng riêng trong `bench-wal` với `-benchtime=300000x -count=3`.

---

### 2026-09-02 — `kill -9` không phải mất điện, và bộ kiểm tra chưa từng được kiểm tra

```console
$ go run ./cmd/crashlab -n 20 -nosync
...
20          212      460      2757  ok (redo 3747/4570, undo 0, loser 0, mồ côi 0)

20/20 vòng đúng, 0 sai. 10641 transaction đã commit được kiểm, 13.079s.
```

**Đọc kết quả:** tắt **hoàn toàn** fsync mà 20/20 vòng vẫn đúng. Đúng như P0-1 đã ghi từ phase 0:
`kill -9` giết tiến trình, **không** giết page cache của kernel; byte đã `write(2)` vẫn tới đĩa.

Nhưng hệ quả nghiêm trọng hơn: nghĩa là bài `crashlab-nosync` **không** chứng minh được rằng bộ
kiểm tra biết báo SAI. Và nếu bộ kiểm tra chưa bao giờ báo SAI thì con số "200/200 vòng đúng"
không phải bằng chứng — nó chỉ là một hàm luôn trả về "ok".

Cần một chế độ mất dữ liệu **thật**: giữ byte log trong buffer của **tiến trình**, không
`write(2)` — để `kill -9` mang chúng đi cùng. Thêm `wal.Log.NoWrite` + cờ `-nowrite`:

```console
$ go run ./cmd/crashlab -n 10 -nowrite
crashlab: 10 vòng kill -9, pool 16 frame, chế độ: log GIỮ TRONG RAM — mong đợi THẤT BẠI
vòng   sống(ms)      txn      khóa  kết quả
1           565      294         0  SAI: panic khi kiểm tra: btree: cell 0: page: page hỏng: slot 0 trỏ ra ngoài (off=17 len=0)
2           405      192         0  SAI: cây hỏng: [B3: page 83 khóa "K\x00\x00\x00\x80\x94\x1c\x90VNo/..." vượt cận trên ...
3           558      277         0  SAI: Verify lỗi: btree: node hỏng: page 43 kiểu 5 không phải node B+Tree
4           367      108         0  SAI: cây hỏng: [B3: page 43 khóa ... < cận dưới ...
5           640      203         0  SAI: cây hỏng: [B3: page 73 khóa ... vượt cận trên ...
6           354      186         0  SAI: cây hỏng: [B3: page 22 khóa ... vượt cận trên ...
7           689      371         0  SAI: Verify lỗi: btree: node hỏng: page 179 kiểu 4 không phải node B+Tree
8           391      228         0  SAI: cây hỏng: [B3: page 134 khóa ... vượt cận trên ...
9            76       33         0  SAI: cây hỏng: [B6: bước 7 của chuỗi sibling là page 23, thứ tự khóa nói phải là 32 B7: page 25 đặc 2
10          107       52         0  SAI: panic khi kiểm tra: btree: cell 0: page: page hỏng: slot 0 trỏ ra ngoài (off=8 len=0)

0/10 vòng đúng, 10 sai. 1944 transaction đã commit được kiểm, 4.353s.
exit status 1
```

**Đọc kết quả:** 10/10 báo SAI, exit code 1. Và nó bắt được **cả hai** hướng hỏng: mất dữ liệu đã
commit, *và* page dữ liệu xuống đĩa trước log của nó (nên cây hỏng chứ không chỉ thiếu khóa) —
tức là đúng cái mà WAL rule tồn tại để ngăn.

**Đang nghĩ gì:** giá trị của một bài test bằng khả năng nó **thất bại** khi cần. Trước hôm nay,
`crashlab` là một bài test chưa bao giờ được kiểm tra. `crashlab-nowrite` giờ là target bắt buộc
đỏ, đứng ngay cạnh nó trong Makefile.

---

### 2026-09-02 — deliverable

```console
$ go run ./cmd/crashlab -n 200 -keep
crashlab: 200 vòng kill -9, pool 16 frame, chế độ: bình thường (fsync bật)
vòng   sống(ms)      txn      khóa  kết quả
...
198         266       21       423  ok (redo 1953/1998, undo 131, loser 1, mồ côi 0)
199         396       47       452  ok (redo 792/1053, undo 62, loser 1, mồ côi 0)
200          85       15        32  ok (redo 356/1317, undo 0, loser 0, mồ côi 0)

200/200 vòng đúng, 0 sai. 9194 transaction đã commit được kiểm, 1m23.157s.
```

**Đọc kết quả:** deliverable đạt. Cột `loser 1` xuất hiện ở gần hết các vòng nghĩa là pha undo
**thật sự** phải chạy (pool 16 frame ép page bẩn của txn dở dang xuống đĩa), không phải xanh vì
chẳng có gì để làm.

Workload cố tình khó: mỗi txn thứ 13 là txn **béo** (400 khóa) để ép đuổi page bẩn; mỗi txn thứ 7
**abort** và ghi khóa độc tiền tố `P` — chúng không được phép xuất hiện; txn *i* xóa khóa của txn
*i-4*. Toàn bộ workload suy ra được từ `(seed, số txn)` nên tiến trình con bị giết không cần báo
lại gì ngoài số đếm tiến độ.

---

### 2026-09-02 — trả nợ P1-1 bằng test, và test ấy tố oan

Quy tắc trong sổ nợ: nợ 🔧 phải viết thành test **đỏ** trước khi tuyên bố trả. P1-1 (page mồ côi
sau rollback) chưa có test nào — chỉ phát hiện được bằng `cmd/dbcheck` sau khi sự đã rồi.

```console
$ go test ./internal/db -run TestAbortLeavesNoOrphanPage -count=1 -v
=== RUN   TestAbortLeavesNoOrphanPage
    db_test.go:571: abort nới 50 page nhưng chỉ trả 49 page về freelist: 1 page mồ côi
--- FAIL: TestAbortLeavesNoOrphanPage (0.03s)
```

**Đọc kết quả:** một page rò. Suýt đi sửa `Abort`. Nhưng 1 page, đúng bằng 1, ở ngay sau một
`CheckpointFlush` — đó là **page chứa freelist**: `CommitMeta` lấy một page từ chính freelist ra
làm host, nên page ấy rời `free` mà vẫn thuộc quyền sở hữu của meta. Phép đếm của tôi sai, không
phải code.

```console
$ go test ./internal/db -run TestAbortLeavesNoOrphanPage -count=1 -v
=== RUN   TestAbortLeavesNoOrphanPage
--- PASS: TestAbortLeavesNoOrphanPage (0.04s)
PASS
```

Và bên ngoài nhìn vào cũng sạch:

```console
$ go run ./cmd/crashlab -child -dir data/p5 -seed 3 -txns 120 -frames 16 && go run ./cmd/dbcheck data/p5/data.db
data/p5/data.db: 380928 byte = 93 page
  meta 0:* txnID=6    root=41   freelist=91   pageCount=93   crc32c=0xdca56d59
  meta 1:  txnID=5    root=41   freelist=92   pageCount=93   crc32c=0x5acbdd11
  (* = meta đang có hiệu lực; cái còn lại là đích rollback)
  freelist: 1 page trong chuỗi [91], 40 PageID rỗng
  chưa phân loại được: 50 page (cần B+Tree ở phase 4 mới truy được)
  ✓ không có lỗi nghiêm trọng
```

**Đang nghĩ gì:** đây là lần thứ hai trong ngày một "phát hiện" hoá ra là lỗi của phép đo (lần
đầu: 1.34x đường đọc). Tỉ lệ 2 lỗi-đo / 1 lỗi-code trong turn này đủ để đổi thói quen: trước khi
tin một con số bất thường, đọc lại **cái đang đếm** trước khi đọc code.

---

### 2026-09-02 — một file log thật gồm những gì

```console
$ make wallab
go run ./cmd/crashlab -child -dir data/wal -seed 1 -txns 400 -frames 16
go run ./cmd/wallab -tail 12 data/wal/data.db.wal
file      data/wal/data.db.wal
độ dài    52103476 byte, 36135 record, checkpoint gần nhất tại LSN 52103404
page bị chạm 238, ảnh trọn page 1710 record

loại               số     byte log byte dữ liệu byte/record
BEGIN             401        19248            0         48
UPDATE          30761     46987552     44941248       1528
CLR              2736      2150020      1982976        786
COMMIT            344        16512            0         48
ABORT              57         2736            0         48
ALLOC            1112        54488            0         49
FREE              691      2869032      2830336       4152
ROOT                9          504            0         56
CKPT-BEGIN         12          576            0         48
CKPT-END           12         2808            0        234
TỔNG            36135     52103476     49754560

UPDATE: 1528 byte log / record, 1461 byte thật sự đổi, 3.3 đoạn / record
so với ghi trọn page 4096B: 2.7x tiết kiệm
```

**Đọc kết quả:** ba con số đáng nhớ.

- **2.7x** tiết kiệm nhờ diff theo đoạn — dưới con số 32x của benchmark một-lần-chèn, vì txn béo
  gây `Compact()` làm đổi cả vùng cell.
- **FREE tốn 4152 byte/record** (mang trọn ảnh page), 691 cái = 2.87 MB trên 52 MB. Đây là cái giá
  của bản sửa "FREE phải mang trọn ảnh page" ở turn code. → nợ P5-3.
- 52 MB log cho **12 KB** dữ liệu:

```console
$ go test ./internal/db -run TestLogGrowth -v   # test tạm, đã xoá
    sau 4000 thao tác: data=12288 B  wal=2873417 B
```

**234x**. Log không bao giờ được cắt. → nợ P5-2.

---

## Giả thuyết sai / bug đã gặp

| Tôi tưởng là | Thực tế là | Lệnh / output đã lật tẩy nó | Đã sửa thế nào |
|---|---|---|---|
| Cờ `dirty` của cây là nguồn sự thật đủ tốt cho WAL | Leaf bị xóa khóa rồi bị gộp đi được thả pin với `dirty=false` → không có record nào để undo; `Abort` im lặng làm mất lệnh xóa 8/12 seed | Ma trận abort×crash: `abort=false` 0/12 sai, `abort=true` **8/12 sai**, y hệt nhau có/không crash → bug của `Abort`, không của recovery | `PageOut` trả về "byte có đổi thật không"; `emit` bỏ hẳn gợi ý `dirty`; `t.unpin` nâng dirty theo journal |
| `Abort` chỉ cần đặt lại `DB.root` | `Tree` giữ bản sao root riêng → sau `Abort` cây đọc từ root mà txn vừa hủy đã tạo | `TestAbortMatchesRecoveryUndo`: `panic: btree: cell 2: page: slot đã bị xóa` | Thêm `Tree.SetRoot` (không log), gọi trong `Txn.Abort` |
| `pool.FreePage` đủ để bỏ một page giữa txn | Nó vứt luôn nội dung bẩn → bản trên đĩa cũ hơn trạng thái lúc crash, undo dán ảnh-trước lên nền sai | Undo sinh page hỏng khi txn free page giữa đường | Record FREE mang **trọn ảnh page**; `t.freePage` pin lại để chụp; undo của FREE khôi phục trước rồi mới áp các ảnh-trước cũ hơn |
| `checkpoint` có flush thì tự nhiên rút ngắn redo | `checkpointLocked` chụp bảng page bẩn **trước** `FlushAll` → CKPT-END mang `recLSN` lỗi thời, recovery vẫn bắt đầu từ đúng chỗ cũ | `TestCheckpointShortensRecovery`: 844 vs 844 record | Flush **trước**, chụp bảng **sau**. 844 → 105 = **8.0x** |
| Page chứa freelist nằm chung `pending` với page thường là được | WAL rút `pending` ở lúc **commit txn**, không phải lúc ghi meta → host bị cấp lại trong khi meta bền vẫn trỏ vào nó | `crashlab -n 20`: 4/20 vòng `đọc freelist page N: EOF`, `page 44 kiểu 10 không phải node B+Tree` | Danh sách `metaPending` riêng, chỉ nhả ở cuối `CommitMeta` **lần sau**; `Allocate` lọc nó; `WritePage` từ chối ghi đè nó |
| Recovery dựng lại trạng thái cấp phát bằng `Reserve` mọi ALLOC rồi `FreeNow` mọi FREE | Hai vòng rời → page free ở LSN 100 rồi cấp lại ở LSN 200 rơi vào freelist **trong khi đang sống**; checkpoint cuối lấy nó làm host | Pool 1024 frame → xanh, bỏ checkpoint cuối → xanh (hai lần che bug, không phải sửa) | Một lượt `lastEv map[PageID]pageEvent` theo **sự kiện cuối cùng** của mỗi page |
| Chốt `pageLSN >= rec.LSN` đủ để redo không ghi bừa | Page freelist **không có** pageLSN; page free từ lâu rồi bị pager lấy làm host vẫn còn record UPDATE cũ trong log | Cây hỏng sau recovery ở đúng những page mà `dbcheck` gọi là host freelist | `skipRedo` = `metaPending` ∪ page có sự kiện cuối là FREE-đã-commit |
| `pg.FreeNow` là đủ để bỏ một page mồ côi sau abort | Frame **bẩn** của nó vẫn nằm trong pool; checkpoint sau đó lấy page ấy làm host, rồi pool đuổi frame cũ ghi đè lên | Cắm `panic` tạm (`MINIDB_GUARD`) vào `pager.WritePage` → stack trace chỉ thẳng `bufpool.victim → writeFrame` | `bufpool.Discard(id)` gọi trước `pg.FreeNow(id)` ở cả `Abort` lẫn `recover()`; chốt chặn giữ lại thành error thường trực |
| Fuzz đứng 40 giây = có exec chậm bệnh hoạn trong code | Bộ **rút gọn** của Go, `-fuzzminimizetime` mặc định 1 phút, exec của nó không được đếm | `panic` nếu exec > 5s: **không nổ**. `-parallel=1`: vẫn đứng. `NoSync`: vẫn đứng. `-fuzzminimizetime=2s`: hết đứng, 61 input mới / 60s thay vì 6 / 120s | Không sửa gì trong code — cờ **đã có** trong `make fuzz-db` từ trước; lỗi là gõ `go test` bằng tay |
| `benchRecover` đang đo recovery | `Open()` kết thúc recovery bằng một checkpoint → từ vòng 2 trở đi không còn gì để redo; nó đang đo chi phí checkpoint | `redo 0` trên cả `NoCkpt` lẫn `Ckpt1M` — một bài đo recovery mà redo không áp dụng record nào | Chụp cặp file lúc crash, dán về trước mỗi vòng (đồng hồ tạm dừng); `-benchtime=10x` riêng |
| Checkpoint dày hơn thì recovery ngắn hơn | Checkpoint **mờ** không flush → `redoLSN = min(recLSN)` bị ghim bởi page bẩn cũ nhất; root nằm mãi trong pool | Ckpt1M **13033** redo vs Ckpt64K **13059** redo — dày hơn 16 lần, không cắt được record nào. Thêm `RecoverCleaner` → **0** | Không sửa (đúng hành vi ARIES); ghi nợ P5-1: cần page cleaner tách khỏi checkpoint |
| `gran` nhỏ là lựa chọn gần như miễn phí | Bảng số nói thế chỉ vì `Diff` dùng vòng lặp so từng byte, nên chi phí không nằm ở số lần gọi | Thời gian gần như phẳng theo gran: 3443 / 3193 / 3299 / 3215 ns — nếu là đánh đổi thật thì gran 128 phải nhanh ~8x | `equal` → `bytes.Equal` (SIMD). `Diff` 3.5x, `InsertNoSync` **3.7x**. Giờ gran mới là đánh đổi thật |
| Đường đọc trả 1.34x cho WAL | Nhiễu của một lần chạy, và phép so lẫn cả `d.mu` của `DB.Get` | `-count=3` + điểm đo giữa: 359 → 368 (**1.03x**) → 380 (**1.06x**) | Thêm `GetWithWALRaw`; chốt bằng `TestReadPathTakesNoSnapshot` (0 ảnh, không phụ thuộc máy) |
| `crashlab -nosync` chứng minh bộ kiểm tra biết báo SAI | `kill -9` không giết page cache → **20/20 vẫn đúng** dù tắt sạch fsync. Bộ kiểm tra chưa từng được kiểm tra | `go run ./cmd/crashlab -n 20 -nosync` → `20/20 vòng đúng, 0 sai` | Thêm `wal.Log.NoWrite` (log ở lại RAM tiến trình) + `make crashlab-nowrite` → **0/10 đúng, 10 sai, exit 1** |
| `TestAbortLeavesNoOrphanPage` bắt được 1 page rò | Đúng bằng 1 page, ngay sau `CheckpointFlush` — là **page chứa freelist**, rời `free` nhưng vẫn thuộc meta. Phép đếm sai, không phải code | `abort nới 50 page nhưng chỉ trả 49 page về freelist: 1 page mồ côi` | Đếm `len(FreeList()) + MetaPendingCount()` ở **cả hai** đầu |
| `make wallab` chạy được trên cây sạch | Tiến trình con không tự tạo thư mục làm việc | `child: open data/wal/data.db: no such file or directory` | `os.MkdirAll` trong `child()` — nó phải gọi được một mình |
| `-benchtime=2000x` dùng chung cho mọi bench trong package được | `Get` ở 2000 vòng trên 20000 khóa thì mỗi khóa chỉ tra một lần → đo đọc đĩa (**5109 ns**), không phải đường đọc (**382 ns**) | Cùng một benchmark, hai benchtime, lệch **13x** | Tách ba dòng trong `bench-wal`: Insert `2000x`, Recover `10x`, Get `300000x -count=3` |

---

## Số đo

**Lệnh:** `make bench-wal` · **Ngày:** 2026-09-02 · **Commit:** `6ac4450` ·
**Máy:** WSL2 / ext4 trên file ảnh đĩa / i5-1235U / GOMAXPROCS=6.

### Giá của durability và hình dạng của group commit

```console
$ go test ./internal/db/ -run '^$' -bench 'Insert' -benchtime=2000x -timeout 30m
BenchmarkInsertBatch1-6      	    2000	   1372130 ns/op	         1.000 fsync/op	         0 fullpg/op	       587.0 logB/op	         0 pagewr/op
BenchmarkInsertBatch10-6     	    2000	    162247 ns/op	         0.1000 fsync/op	         0 fullpg/op	       500.6 logB/op	         0 pagewr/op
BenchmarkInsertBatch100-6    	    2000	     25153 ns/op	         0.01000 fsync/op	         0 fullpg/op	       491.9 logB/op	         0 pagewr/op
BenchmarkInsertBatch1000-6   	    2000	      5260 ns/op	         0.001000 fsync/op	         0 fullpg/op	       491.0 logB/op	         0 pagewr/op
BenchmarkInsertNoSync-6      	    2000	      3025 ns/op	         0 fsync/op	         0 fullpg/op	       587.0 logB/op	         0 pagewr/op
BenchmarkInsertNoFPW-6       	    2000	     25152 ns/op	         0.01000 fsync/op	         0 fullpg/op	       491.9 logB/op	         0 pagewr/op
BenchmarkInsertFPW-6         	    2000	     19712 ns/op	         0.01000 fsync/op	         0 fullpg/op	       491.9 logB/op	         0 pagewr/op
```

Cột `pagewr/op` **bằng 0 ở mọi dòng**: 2000 lần chèn, không một page dữ liệu nào xuống đĩa. Đó
chính là câu "WAL đổi ghi ngẫu nhiên thành ghi tuần tự" ở dạng số đo.

### Recovery: checkpoint cắt được gì và không cắt được gì

```console
$ go test ./internal/db/ -run '^$' -bench 'Recover' -benchtime=10x -timeout 30m
BenchmarkRecoverNoCkpt-6    	      10	 142748398 ns/op	     11491 logKiB	     21725 redo	         0 skip
BenchmarkRecoverCkpt1M-6    	      10	  90260761 ns/op	     11593 logKiB	     13033 redo	        63.00 skip
BenchmarkRecoverCkpt64K-6   	      10	 102961883 ns/op	     13214 logKiB	     13059 redo	        37.00 skip
BenchmarkRecoverCleaner-6   	      10	  66931825 ns/op	     11653 logKiB	         0 redo	         0 skip
```

### Đường đọc

```console
$ go test ./internal/db/ -run '^$' -bench 'Get' -benchtime=300000x -count=3
BenchmarkGetWithWAL-6      	  300000	  382.0 / 385.9 / 373.3 ns/op
BenchmarkGetNoWAL-6        	  300000	  359.2 / 363.4 / 356.3 ns/op
BenchmarkGetWithWALRaw-6   	  300000	  368.8 / 370.1 / 364.8 ns/op
```

### Diff: chi phí và mức tiết kiệm

```console
$ go test ./internal/wal/ -run '^$' -bench . -benchmem
BenchmarkDiffGran16-6      	 1000000	      1188 ns/op	        80.00 dataB	         3.000 seg	        51.20 x-vs-fullpage
BenchmarkDiffGran32-6      	 1536907	       914.2 ns/op	       128.0 dataB	         3.000 seg	        32.00 x-vs-fullpage
BenchmarkDiffGran64-6      	 2944390	       466.3 ns/op	       192.0 dataB	         3.000 seg	        21.33 x-vs-fullpage
BenchmarkDiffGran128-6     	 4053180	       287.3 ns/op	       384.0 dataB	         2.000 seg	        10.67 x-vs-fullpage
BenchmarkDiffIdentical-6   	 1388196	       857.5 ns/op	       0 B/op	       0 allocs/op
BenchmarkEncodePayload-6   	36916476	        34.39 ns/op	       284.0 payloadB
BenchmarkEncodeFullPage-6  	23882408	        52.42 ns/op	      4244 payloadB
BenchmarkApply-6           	80868369	        16.21 ns/op	       0 B/op	       0 allocs/op
```

### Group commit, nhìn từ trong log

```console
$ go test ./internal/wal -run TestGroupCommit -count=1 -v
=== RUN   TestGroupCommit
    wal_test.go:197: 200 commit -> 1 fsync (199 lần được người khác fsync hộ, 0 lần thấy đã xong sẵn)
--- PASS: TestGroupCommit (0.00s)
PASS
ok  	minidb/internal/wal	0.007s
```

### Tỉ số cần nhớ

Tỉ số bền hơn số tuyệt đối — số tuyệt đối đổi theo máy và theo lần chạy.

| Tỉ số | Giá trị | Ý nghĩa |
|---|---|---|
| `InsertBatch1` / `InsertBatch1000` | **261x** | 1 khóa/txn vs 1000 khóa/txn. Gần hết khoảng cách này là **một** cái fsync. Đây là hình dạng thật của group commit khi chỉ có một writer |
| `InsertBatch1` / `InsertNoSync` | **453x** | toàn bộ cái giá của durability trên máy này |
| `pagewr/op` mọi dòng Insert | **0** | 2000 lần chèn, không page dữ liệu nào xuống đĩa. Commit = một fsync một vùng log tuần tự |
| commit / fsync khi group commit | **200 / 1** | `TestGroupCommit`: 200 commit đồng thời gộp thành 1 fsync nhờ coalescing trên condvar |
| redo: không ckpt / có ckpt 1MB | **1.67x** | checkpoint **mờ** kéo được điểm bắt đầu redo, nhưng chỉ tới đó |
| redo: ckpt 1MB / ckpt 64KB | **1.00x** (13033 vs 13059) | dày hơn **16 lần** mà không cắt được record nào — `redoLSN` bị ghim bởi page bẩn cũ nhất |
| redo: ckpt 1MB / có page cleaner | **13033 → 0** | thứ quyết định *độ dài* redo là người dọn page, không phải tần suất checkpoint |
| `TestCheckpointShortensRecovery` | **8.0x** (844 → 105) | cùng một cơ chế, nhìn từ checkpoint **có** flush |
| log / dữ liệu (workload 4000 thao tác) | **234x** (2.87 MB / 12 KB) | log không bao giờ được cắt → nợ P5-2 |
| log thật: diff / ghi trọn page | **2.7x** | trên workload crashlab (có txn béo gây `Compact`). Trên một lần chèn đơn lẻ: **32x** |
| `Diff` 4KB: vòng lặp tay / `bytes.Equal` | **3.5x** (3193 → 914 ns) | và `InsertNoSync` **3.7x** (11154 → 3025) — `Diff` từng chiếm phần lớn đường ghi thuần CPU |
| đường đọc: cây trần / qua journal | **1.03x** | cổng `enter/leave` đóng đúng: đọc chụp **0** ảnh page |
| đường đọc: cây trần / `DB.Get` | **1.06x** | 3% còn lại là `d.mu`, không phải WAL |
| `Get` ở `2000x` / ở `300000x` | **13x** (5109 / 382 ns) | cùng một benchmark, khác benchtime — đây là cái bẫy "đo cache lạnh mà tưởng đo đường đọc" |
| fuzz: đúng `-fuzzminimizetime` / mặc định | **>20x** input mới | 61 mẫu / 60s vs 6 mẫu / 120s |

---

## Invariant tôi đã cài và lệnh kiểm chứng nó

| Invariant | Cài ở đâu (file:hàm) | Lệnh kiểm chứng | Kết quả |
|---|---|---|---|
| **WAL rule:** log của một thay đổi phải `fsync` xong **trước** khi page dữ liệu của nó rời RAM | `bufpool/bufpool.go:writeFrame` → `bufpool.Pool.FlushLog` (một điểm duy nhất) | `go test ./internal/db -run TestWALRuleHolds -count=1` | PASS |
| Cái gì đã báo commit thì phải còn sau khi mất điện, dù **không** page dữ liệu nào kịp xuống đĩa | `db/db.go:Txn.Commit` (append COMMIT → `log.Flush(lsn+1)`) | `go test ./internal/db -run TestCommitSurvivesCrash -count=1` | PASS |
| Transaction dở dang không được sống sót | `db/recover.go:undoChain` | `go test ./internal/db -run TestUncommittedVanishes -count=1` (pool 8 frame để log của loser thật sự xuống đĩa) | PASS |
| `Abort` lúc chạy và undo lúc recovery cho **cùng** một kết quả | `db/recover.go:undoChain` dùng chung cho cả hai | `go test ./internal/db -run TestAbortMatchesRecoveryUndo -count=1` | PASS |
| Recovery idempotent — crash **trong lúc** đang recovery vẫn ra đúng | CLR có `UndoNext`; quy tắc: record nào có `UndoNext != 0` là record bù, **không bao giờ** bị undo | `go test ./internal/db -run TestRecoveryIsIdempotent -count=1` | PASS |
| Một page bị torn vẫn lành lại được, dù `pageLSN` của nó là rác | `db/journal.go:emit` — lần chạm đầu sau mỗi checkpoint ghi `FlagFullPage`, redo áp **vô điều kiện** | `go test ./internal/db -run TestFullPageWriteAppearsOncePerCheckpoint -count=1` | PASS |
| Đuôi log bị ghi dở không bao giờ được kẹp giữa hai record thật | `wal/log.go:Open` → `scanTail` **cắt** file về record hợp lệ cuối cùng | `go test ./internal/db -run TestLogTailTornIsIgnored -count=1` · `go test ./internal/wal -run TestTornTailStopsScan -count=1` | PASS |
| Một bit lật trong record phải bị bắt | crc32c ở 4 byte cuối mỗi record, `wal/record.go` | `go test ./internal/wal -run TestOneBitFlipIsCaught -count=1` | PASS |
| Chỉ **một** writer, do code bắt buộc chứ không do quy ước | `db/db.go:beginLocked` → `ErrWriterBusy` | `go test ./internal/db -run TestSingleWriter -count=1` | PASS |
| `Flush` đơn điệu: `flushed` không bao giờ tụt | `wal/log.go:Flush` | `go test ./internal/wal -run TestFlushIsMonotonic -count=1` | PASS |
| Page đang là host freelist của meta hiện tại **không bao giờ** bị ghi đè | `pager/pager.go:WritePage` → `ErrMetaPageBusy` (chốt chặn thường trực) | `go run ./cmd/crashlab -n 200` | 200/200 |
| Abort không để lại page mồ côi | `db/db.go:Txn.Abort` → `pool.Discard` + `pg.FreeNow` | `go test ./internal/db -run TestAbortLeavesNoOrphanPage -count=1` | PASS |
| Đường **đọc** không chụp một ảnh page nào | `db/journal.go:enter/leave` (cổng `j.on`) | `go test ./internal/db -run TestReadPathTakesNoSnapshot -count=1` | PASS |
| Bảy bất biến của cây (phase 4) vẫn đúng **sau** recovery | `btree/verify.go:Verify`, gọi trong crashlab và fuzz | `go run ./cmd/crashlab -n 200` · `make fuzz-db` | 200/200 · PASS |
| **Bộ kiểm tra biết báo SAI** | `wal/log.go:Log.NoWrite` + `cmd/crashlab -nowrite` | `make crashlab-nowrite` | **0/10 đúng, 10 sai, exit 1** ✅ (đỏ là đúng) |

---

## Đọc gì

- Mohan et al., *ARIES: A Transaction Recovery Method Supporting Fine-Granularity Locking and
  Partial Rollbacks Using Write-Ahead Logging* (1992) — ba pha, CLR, `UndoNext`, fuzzy checkpoint.
  Đọc kỹ mục 6 (recovery độc lập với khoá) mới hiểu vì sao redo phải *repeat history*.
- *Database System Concepts* ch. 19 (Recovery) — bản diễn giải dễ vào hơn ARIES gốc.
- Postgres: `src/backend/access/transam/xlog.c` — LSN là offset byte, `full_page_writes`, master
  record trong control file. Hai chi tiết vay trực tiếp: LSN-là-offset và ảnh trọn page sau
  checkpoint.
- InnoDB: khái niệm **mini-transaction** — một lệnh cấp cao chạm nhiều page, mỗi page một record,
  lấy ở đúng biên pin/unpin. Đây là thứ định hình `internal/btree/journal.go`.
- `go doc testing.F` + tài liệu `go help testflag` về `-fuzzminimizetime` — lần thứ hai phải đọc.

---

## Rút ra (viết như thể giải thích cho người khác)

**WAL rule, phát biểu chính xác.** Trước khi một page dữ liệu bị ghi xuống đĩa, mọi record log
mô tả thay đổi trên page ấy phải đã `fsync` xong. Không phải "đã `write(2)`" — đã `fsync`. Đó là
lý do phải có ba mốc LSN tách bạch, và WAL rule so với mốc `flushed`. Trong minidb nó được thực
thi ở **đúng một** chỗ: `bufpool.writeFrame` gọi `FlushLog` trước khi giao page cho pager. Một
điểm thực thi là điều kiện để invariant này có thể đúng — nếu nó cần hai chỗ để đúng, nó sẽ sai ở
chỗ thứ ba.

**Vì sao redo phải *repeat history*, kể cả txn sắp bị undo.** Vì undo là **physical**: nó dán
ảnh-trước lên page. Ảnh-trước ấy chỉ đúng nếu page đang ở đúng trạng thái mà lúc ghi log nó đã ở.
Nếu redo bỏ qua txn thua, page sẽ ở một trạng thái trung gian không khớp với bất kỳ ảnh-trước nào,
và undo sẽ dán lên nền sai. Ở minidb tôi đã gặp đúng chuyện này ở dạng khác: `pool.FreePage` vứt
nội dung bẩn đi, khiến nền dưới ảnh-trước cũ hơn thực tế → undo sinh page hỏng. Redo lặp lại lịch
sử chính là để cái nền ấy luôn đúng.

**CLR giải quyết gì.** Crash **trong lúc** đang undo. Không có CLR, lần recovery sau sẽ undo lại
những thay đổi đã undo — với undo physical thì đó là dán ảnh-trước lên một page đã lùi rồi, tức
là làm sai. CLR là một record nói "tôi đã hoàn tác tới đây, lần sau tiếp tục từ `UndoNext`". Quy
tắc tôi chốt cho gọn: **record nào có `UndoNext != 0` là record bù, không bao giờ bị undo.** Một
quy tắc, một câu, kiểm được bằng `TestRecoveryIsIdempotent`.

**Fuzzy vs consistent checkpoint, và cái tôi hiểu sai.** Consistent checkpoint chặn ghi, flush hết
page bẩn, rồi ghi một mốc. Fuzzy checkpoint không chặn gì: nó ghi CKPT-BEGIN, chụp bảng txn đang
chạy và bảng page bẩn, ghi CKPT-END, rồi mới đặt master record. Chọn fuzzy vì nó không dừng hệ
thống. Nhưng chỗ tôi hiểu sai — và chỉ số đo mới chỉ ra — là **tôi tưởng checkpoint dày hơn thì
recovery ngắn hơn.** Không. `redoLSN = min(recLSN)` trên toàn bảng page bẩn, nên nó bị ghim bởi
page bẩn **cũ nhất**; root của cây B+ nằm mãi trong pool và bẩn liên tục. Checkpoint 64KB và
checkpoint 1MB cho redo bằng nhau (13059 vs 13033) — dày hơn 16 lần, không cắt được record nào.
Thứ quyết định *độ dài* redo là **người dọn page**: thêm một checkpoint có flush định kỳ thì redo
về 0. Nói gọn: checkpoint mờ trả lời "đọc log **từ đâu**"; nó không trả lời "đọc **bao nhiêu**".
Đó là lý do các DB thật có background page cleaner tách khỏi checkpoint.

**Group commit tăng throughput bằng cách nào.** Không phải bằng cách làm fsync nhanh hơn — bằng
cách để nhiều commit **dùng chung một** fsync. Cụ thể: ai thấy `syncing == true` thì đợi trên
condvar thay vì phát thêm một fsync; và `writeLocked` luôn đẩy tới `l.end`, nên cái fsync đang bay
thường đã phủ luôn phần của người đợi. `TestGroupCommit`: 200 commit → **1** fsync. Nhìn từ phía
workload cũng thấy cùng một thứ: `InsertBatch1` / `InsertBatch1000` = **261x**, và gần hết khoảng
cách ấy là một cái fsync.

**LSN nên là offset byte hay số thứ tự.** Chọn offset byte (kiểu Postgres). Được: `Read(lsn)` là
một `pread`, khỏi bảng tra; và LSN 0 tự nhiên không hợp lệ nên page mới có `pageLSN = 0` mang
nghĩa "chưa từng bị ghi" mà không cần cờ riêng. Mất: không cắt được đầu log mà không đổi LSN —
đúng thứ tôi đang nợ (P5-2), vì log đã phình 234x so với dữ liệu.

**Torn page và vòng lặp chết của `pageLSN`.** Redo bỏ qua record khi `pageLSN >= rec.LSN`. Nhưng
một page bị torn thì `pageLSN` của nó là rác — có thể là rác **lớn**, và thế là redo bỏ qua đúng
cái page đang hỏng. Thoát khỏi vòng lặp này bằng **ảnh trọn page**: lần chạm đầu tiên của mỗi page
sau mỗi checkpoint ghi một ảnh đầy đủ vào phía redo của record, và redo áp nó **vô điều kiện**,
không hỏi `pageLSN`. Đây chính là `full_page_writes` của Postgres, và là cách phase 5 trả nợ P2-3
mà không cần thêm checksum riêng cho page. Chi tiết đắt: `EncodeFullPage` payload 4244 byte vs
284 byte — nên chỉ trả một lần mỗi page mỗi checkpoint, và chỉ ở phía redo (undo chạy sau redo,
lúc đó page đã lành).

**Freelist của pager là hệ thống thứ hai ghi vào cùng không gian PageID, và nó không được log.**
Đây là bài học lớn nhất của phase, và tôi phải sửa bốn lần mới nhìn ra bốn bug ấy là **một**. WAL
biết mọi thứ về nội dung page, nhưng không biết gì về việc pager coi page nào là còn sống. Nên có
đủ kiểu cách để hai bên đá nhau: page chứa freelist bị cấp lại trong khi meta bền vẫn trỏ vào nó;
recovery đưa một page đang sống vào freelist; redo ghi record cũ lên một page giờ đang là host
freelist; frame bẩn mồ côi bị pool đuổi ra ghi đè lên host. Cách thoát không phải sửa từng cái mà
là **dựng một chốt chặn ở chỗ hẹp nhất**: `pager.WritePage` từ chối ghi vào page đang là host. Chốt
ấy biến một triệu chứng xuất hiện sau đó hàng trăm millisecond ở một file khác
(`đọc freelist page 4096: EOF`) thành một dòng số ngay tại chỗ gây lỗi. Nợ còn lại — freelist vẫn
chưa được WAL log — ghi thành P5-4.

**Cờ `dirty` của tầng trên là gợi ý; WAL phải có nguồn sự thật riêng.** Cây B+ đặt `dirty` để tối
ưu cho buffer pool, và nó được phép bảo thủ sai theo hướng "quên bật" — với buffer pool thì hậu quả
chỉ là mất một lần ghi không cần thiết. Với WAL thì hậu quả là **mất một lệnh xóa**. Nên journal
bỏ hẳn gợi ý ấy và **so byte** để tự quyết định. Giá phải trả là `Diff` trên mọi page được pin, kể
cả page không đổi — và đó chính là lý do `bytes.Equal` đáng giá 3.7x trên đường ghi.

**Và bài học phương pháp, đắt nhất trong turn hai:** trong ngày hôm nay tôi tìm ra **hai** lỗi của
bộ đo và **một** lỗi của code. `benchRecover` đo chi phí checkpoint mà tôi tưởng đang đo recovery
(dấu hiệu: `redo 0`). Đường đọc "chậm 1.34x" chỉ là nhiễu một lần chạy. `TestAbortLeavesNoOrphanPage`
tố oan một page mồ côi mà thật ra là page chứa freelist. Cả ba đều đã suýt trở thành một dòng kết
luận sai trong diary này. Quy tắc số 5 của SKILL — "thấy số vô lý thì nghi bench sai trước, đừng
nghi máy lạ" — cần thêm một mệnh đề: **và nghi cả cái mình đang đếm, trước khi nghi code.**

**Cuối cùng: một bài test chưa bao giờ đỏ thì chưa phải bằng chứng.** `crashlab -nosync` được viết
từ đầu để chứng minh bộ kiểm tra biết báo SAI, và nó **không** làm được việc ấy — tắt sạch fsync mà
20/20 vòng vẫn xanh, vì `kill -9` giết tiến trình chứ không giết page cache của kernel (đúng P0-1
đã ghi từ phase 0). Nghĩa là trước hôm nay, con số "200/200 vòng đúng" có thể chỉ là một hàm luôn
trả về "ok". Phải dựng một chế độ mất dữ liệu thật — `NoWrite`: giữ byte log trong buffer của
**tiến trình** để `kill -9` mang chúng đi cùng — và xem bộ kiểm tra có bắt được không. Nó bắt được
10/10, và bắt cả hai hướng hỏng: thiếu khóa đã commit, *và* cây hỏng vì page dữ liệu xuống đĩa
trước log của nó. `make crashlab-nowrite` giờ là một target **bắt buộc đỏ** đứng ngay cạnh
`crashlab-full`. Không có nó, deliverable của phase này chỉ là một lời tự khen.

---

## Nợ kỹ thuật / để dành cho sau

- [ ] **P5-1** · Không có background page cleaner → checkpoint mờ không chặn được độ dài redo
      (đo được: ckpt 1MB vs 64KB cho redo bằng nhau, 13033 vs 13059)
- [ ] **P5-2** · Log không bao giờ được cắt/tái dùng: 52 MB log cho 12 KB dữ liệu = **234x**
- [ ] **P5-3** · Undo là physical nên phải mang ảnh-trước; record FREE mang **trọn** ảnh page
      (4152 byte/record, 691 record = 2.87 MB trên 52 MB)
- [ ] **P5-4** · Freelist của pager vẫn **chưa** được WAL log — hiện phải chống đỡ bằng
      `skipRedo` + `metaPending` + chốt chặn trong `WritePage`
- [ ] **P5-5** · `d.dpt` và `OnFlush` truy cập map không có latch riêng — hiện an toàn chỉ nhờ
      một-writer (P4-5), sẽ vỡ khi có nhiều writer
- [ ] **P5-6** · Chưa đo được recovery trên log **lớn** (GB) và chưa có giới hạn bộ nhớ cho pha
      Analysis (ATT/DPT nằm hết trong RAM)

### Nợ phase trước đã trả trong phase này

- [x] **P1-1** · Page mồ côi sau rollback → `Abort`/`recover` trả page về freelist
      (`bufpool.Discard` + `pg.FreeNow`), có test: `TestAbortLeavesNoOrphanPage`, và `dbcheck` im lặng
- [x] **P1-2** · Transaction thật → `Begin`/`Commit`/`Abort`, một writer **do code bắt buộc**
      (`ErrWriterBusy`), có test `TestSingleWriter`. *Còn lại:* chưa có cô lập cho reader đồng
      thời (MVCC) → phase 6
- [x] **P2-3** · Torn page → ảnh trọn page sau mỗi checkpoint, redo áp vô điều kiện
      (`TestFullPageWriteAppearsOncePerCheckpoint`). *Chọn không* thêm checksum riêng cho page:
      crc32c của record log + ảnh trọn page đã đủ, và rẻ hơn
- [x] **P4-4** · Root đổi `PageID` mỗi lần cây cao thêm → **quyết định: giữ nguyên cho root di
      chuyển**, nhưng mỗi lần đổi phát sinh một record ROOT được log, và meta chỉ giữ root ở lần
      checkpoint cuối. Không cố định root vào một page id, vì như thế phải copy nội dung mỗi lần
      cây cao thêm — mà việc ấy lại phải log trọn page
