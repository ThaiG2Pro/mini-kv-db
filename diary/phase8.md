# Phase 8 — SQL front-end: parser → planner → executor

- **Thời lượng dự kiến:** 2-3 ngày · **thực tế:** 1 ngày (3 lượt)
- **Bắt đầu:** 2026-09-03 · **Kết thúc:** 2026-09-03
- **Trạng thái:** ✅ xong
- **Commit:** `_(điền sau khi commit)_`

> **Quy tắc ghi nhật ký:** mọi con số, mọi kết luận đều phải kèm **lệnh shell sinh ra nó**
> và **output thật** (dán nguyên, không tóm tắt). Sáu tháng sau đọc lại phải chạy lại được.
> Ghi trong lúc làm, không phải sau khi xong.

Nhật ký lệnh đầy đủ, kể cả các ngõ cụt: [`phase8-log.md`](./phase8-log.md).

## Môi trường

```console
$ uname -srmo && go version && df -hT . | tail -1
Linux 6.6.87.2-microsoft-standard-WSL2 x86_64 GNU/Linux
go version go1.26.2 linux/amd64
/dev/sdd       ext4 1007G   40G  917G   5% /

$ nproc && grep -m1 'model name' /proc/cpuinfo
6
model name	: 12th Gen Intel(R) Core(TM) i5-1235U
```

**Một điều phải nói ngay, vì nó quyết định cách đọc mọi con số dưới đây:** máy này có 16GB RAM
và một SSD sau lớp ext4 của WSL2. Ở quy mô 20000 hàng thì **không có I/O đĩa thật** — dữ liệu
nằm gọn trong buffer pool và page cache. Nên mọi tỉ số ở đây là tỉ số **CPU và số lần chạm cấu
trúc dữ liệu**, không phải tỉ số I/O. Đây cũng là lý do điểm hoà vốn selectivity của phase 7 đo
được 36.8% thay vì 5-20% như sách: sách giả định mỗi lần tra bảng là một lần seek.

## Mục tiêu phase

Lexer → parser → AST → binder → optimizer → planner → executor. `CREATE TABLE` / `CREATE INDEX` /
`INSERT` / `ANALYZE` / `SELECT ... WHERE ... ORDER BY ... LIMIT` / `JOIN` hai bảng, và `EXPLAIN`
in ra kế hoạch của chính mình.

## Câu hỏi phải trả lời được khi xong

1. Logical plan khác physical plan ở đâu?
2. Nested loop join vs hash join: khi nào cái nào thắng?
3. Hash join cần memory budget để làm gì, spill to disk hoạt động ra sao?
4. ORDER BY khi dữ liệu không vừa RAM: external merge sort làm thế nào?
5. Predicate pushdown giúp được gì trong engine của tôi?

Câu trả lời ở mục [Rút ra](#rút-ra-viết-như-thể-giải-thích-cho-người-khác). Cả năm đều được trả
lời bằng **số đo trong chính engine này**, không phải bằng câu trong sách — và câu số 5 là câu
mà số đo đã **sửa lại** cho tôi.

## Deliverable (bằng chứng đã hiểu)

Ba thứ, không phải "code chạy được":

1. **`make test-sql`** — nguyên tắc chỉ có một câu: **hai đường phải cho CÙNG một kết quả.**
   Nested loop vs hash join, tràn đĩa vs không tràn, có Sort vs bỏ Sort, đẩy điều kiện vs không
   đẩy. Bài test dựng kế hoạch **bằng tay** để chạy được cả đường mà planner **không** chọn — một
   phép tối ưu làm đổi kết quả không phải phép tối ưu, nó là con bug.
2. **`make sqllab`** — sáu bảng số, mỗi bảng là đúng một câu hỏi ở trên.
3. **`make fuzz-sql`** — parser nhận đầu vào của **người**, nên mọi chuỗi byte là hợp lệ. Ba bất
   biến: không panic, **luôn kết thúc**, và in-lại-rồi-phân-tích-lại thì bền. Bất biến thứ hai
   đắt nhất, và bất biến thứ ba tìm ra bug thật trong **13 giây**.

## Reproduce toàn bộ phase này

```bash
# 1. toàn bộ test, có -race trên ba package mới
go test ./... -count=1
make test-sql

# 2. sáu bảng số (repeat 7 để hết nhiễu — xem bảng giả thuyết sai, dòng "nhiễu")
go run ./cmd/sqllab -rows 20000 -dim 200 -repeat 7

# 3. fuzz: 120s parser + 60s lexer
make fuzz-sql

# 4. cái giá của việc đổi push -> pull, so với đúng HEAD của phase 7
git worktree add /tmp/p7 12ad70c
(cd /tmp/p7 && go test ./internal/txn/ -run '^$' -bench BenchmarkScan -benchtime=300x -count=3)
go test ./internal/txn/ -run '^$' -bench BenchmarkScan -benchtime=300x -count=3
git worktree remove /tmp/p7

# 5. hồi quy phase 5/6/7 sau ca mổ push->pull
go run ./cmd/crashlab -n 20
go run ./cmd/crashlab -n 10 -nowrite   # PHẢI ĐỎ (exit 1)
make test-txn && make test-index && make fuzz-keys && make fuzz-table

# 6. EXPLAIN của chính mình
rm -f data/sql/diary.db*
go run ./cmd/minidb -db data/sql/diary.db -e "
CREATE TABLE ev (id INT, kind INT, city TEXT, PRIMARY KEY (id));
CREATE TABLE dim (kind INT, name TEXT, PRIMARY KEY (kind));
CREATE INDEX ev_kind ON ev (kind);
INSERT INTO ev VALUES (1,3,'ha noi'),(2,7,'da nang'),(3,3,'hue'),(4,9,'can tho'),(5,3,'sa pa');
INSERT INTO dim VALUES (3,'ba'),(7,'bay'),(9,'chin');
EXPLAIN SELECT ev.city, dim.name FROM ev JOIN dim ON ev.kind = dim.kind WHERE dim.kind < 5 ORDER BY ev.city;
"
```

---

## Nhật ký

### 2026-09-03, lượt 1 — viết code, không chạy một lệnh nào

**Việc đầu tiên không phải viết parser.** Ngồi vẽ hình của nested loop join: *"với mỗi hàng vế
ngoài, quét lại cả vế trong"*. Rồi mở `internal/txn/txn.go` và đọc `Scan`:

```go
func (t *Txn) Scan(lo, hi []byte, fn func(key, val []byte) bool) error
```

Đây là API **push**: vòng lặp thuộc về `Scan`, người gọi chỉ được đưa vào một callback. Phase 6
và phase 7 xây **mọi** đường đọc theo hình đó. Nhưng nested loop join cần **khởi động lại** vế
trong, và hash join cần **rút cạn** vế build rồi mới bắt đầu vế probe. Cả hai đều là "tôi giữ
vòng lặp", tức **pull**.

Với API push, chỉ còn ba lựa chọn, và cả ba đều tệ:

| Cách | Vì sao tệ |
|---|---|
| Đệm cả một vế vào RAM | đúng cái mà phase 7 vừa trả nợ để **bỏ** (P6-4), và làm hash join spill trở thành vô nghĩa |
| Một goroutine + channel cho mỗi vế | `txn.Txn` **không** an toàn cho nhiều goroutine (`dbMu → mu`, xem phase 6) |
| Đảo ngược theo kiểu continuation | join hai vế thành hai callback lồng nhau; ba vế thì không đọc nổi |

Nên phase 8 mở màn bằng một **ca mổ**: đổi `Txn.Scan` và `table.ScanRows`/`ScanIndex` từ push
sang pull. Và một quyết định quan trọng hơn: `Iter` là **bản duy nhất** cài phép merge, còn
`Scan` chỉ là một lớp bọc mỏng:

```go
func (t *Txn) Scan(lo, hi []byte, fn func(key, val []byte) bool) error {
	it := t.Iter(lo, hi)
	for it.Next() {
		if !fn(it.Key(), it.Value()) {
			return it.Close()
		}
	}
	return it.Err()
}
```

Không giữ hai bản. Bài học `internal/query/workload.go` của phase 7: **hai bản của cùng một phép
merge là hai chỗ để lệch nhau.**

**Chỗ khó duy nhất của ca mổ** là một chi tiết nhỏ và nó đáng ghi lại. Một iterator kiểu pull
thường phải **chép** hàng hiện tại ra buffer riêng, vì người gọi có quyền giữ `Key()`/`Value()`
suốt thời gian giữa hai lần `Next()`. Chép mỗi hàng là chép 20000 lần cho một lần quét. Cách
thoát: **hoãn** `advance()` sang **đầu** lần `Next()` kế tiếp.

```go
// pend: lần Next tới phải đẩy con trỏ cây lên một bước trước khi quyết
// định. Có cờ này để KHÔNG phải chép tk/tv ra buffer riêng: chừng nào
// chưa advance thì tk/tv còn nguyên, nên người gọi cầm được Key()/Value()
// suốt thời gian giữa hai lần Next. Bản push cũ giải cùng vấn đề bằng
// cách gọi fn TRƯỚC advance — cùng một bất biến, phát biểu ngược lại.
pend bool
```

**Đọc kết quả:** chưa đo gì (lượt này không chạy lệnh). Nhưng bất biến thì đã phát biểu được:
*bản push gọi `fn` trước `advance`; bản pull hoãn `advance` tới `Next` sau*. Cùng một câu, đọc
ngược lại.

**Đang nghĩ gì:** ca mổ này chạm vào đường đọc của **cả** phase 6 và phase 7, nên nghi vấn lớn
nhất của lượt 2 không phải "SQL có chạy không" mà **"lưới anomaly còn đúng từng ô không, và
crashlab còn xanh không"**.

---

### 2026-09-03, lượt 1 (tiếp) — ba package, và lằn ranh giữa chúng là câu trả lời cho câu hỏi số 1

```
internal/sql/     cú pháp. KHÔNG biết catalog
internal/plan/    ngữ nghĩa + tối ưu. Biết catalog, kiểu, thống kê, chi phí
internal/exec/    thi hành. KHÔNG biết SQL
```

Lằn ranh thứ nhất — `sql` không biết catalog — có một hệ quả đo được: một câu **sai tên bảng**
chết ở `bind`, **không tốn một lần xuống cây nào**. Và một câu sai **cú pháp** thì chết còn sớm
hơn nữa.

Lằn ranh thứ hai, bên trong `plan`, là chỗ tôi tách `opt.go` khỏi `planner.go`, và đây mới là
câu trả lời thật cho *"logical khác physical ở đâu"*:

- **`opt.go` viết lại logical.** Đẩy điều kiện xuống là một **định lý**: σ_p(L ⋈ R) = L ⋈ σ_p(R)
  khi p chỉ nói về R. Đó là một đẳng thức trên tập hợp, **đúng bất kể chạy thế nào** — seq scan,
  index scan, hash join, đều đúng.
- **`planner.go` chọn đường vật lý.** Bỏ bước `ORDER BY` là một **tình huống**: chỉ bỏ được nếu
  đường đi đã chọn **tình cờ** trả về đúng thứ tự ấy. Đổi đường đi thì phép tối ưu đó biến mất.

Một cái là định lý, một cái là tình huống. Đó là toàn bộ lằn ranh.

---

### 2026-09-03, lượt 2 — chạy, và năm con bug

Lượt này là lượt đắt nhất, nên nó có mục riêng: [bảng giả thuyết sai](#giả-thuyết-sai--bug-đã-gặp).
Tóm hình dạng của nó, để so với bảy phase trước:

| Phase | Hình dạng của lượt chạy |
|---|---|
| 4 | **4/7 giả thuyết bị bác**, một cái sai ở tầng khái niệm |
| 5 | **4 bug hoá ra một nguyên nhân gốc**; 2 lỗi bộ đo trên 1 lỗi code |
| 6 | **một chuỗi ba giả thuyết** cho **một** hiện tượng; fuzzer bắt trong 3 giây |
| 7 | **cả bốn bug nằm trong BỘ ĐO**, không con nào trong database |
| **8** | **bug tệ nhất là bug tôi tự tạo ra bằng trực giác mượn từ Postgres**, và **bảng số chỉ ra một luật optimizer còn thiếu** |

Việc đầu tiên của lượt 2 không phải chạy test mà **đo cái giá của ca mổ**:

```console
$ git worktree add /tmp/p7 12ad70c
$ cd /tmp/p7 && go test ./internal/txn/ -run '^$' -bench BenchmarkScan -benchtime=300x -count=3
BenchmarkScan-6   	     300	    333353 ns/op
BenchmarkScan-6   	     300	    363481 ns/op
BenchmarkScan-6   	     300	    317915 ns/op

$ cd - && go test ./internal/txn/ -run '^$' -bench BenchmarkScan -benchtime=300x -count=3
BenchmarkScan-6   	     300	    329262 ns/op
BenchmarkScan-6   	     300	    380667 ns/op
BenchmarkScan-6   	     300	    357590 ns/op
```

**Đọc kết quả:** hai dải chồng lên nhau; cận xấu nhất là 380667/317915 = **1.20x**, trung vị
357590/333353 = **1.07x**. Chốt: đổi push → pull tốn **≤1.08x** ở trung vị, và với hai dải chồng
nhau như thế thì kết luận đúng phải là *"không đo được sự khác biệt ngoài nhiễu"*.

**Đang nghĩ gì:** lúc chuẩn bị đo, tôi định lấy baseline từ **diary phase 6** (ghi 688-732µs). Kịp
dừng lại — xem dòng "baseline cũ" trong bảng giả thuyết sai. Đó là lý do lệnh trên có
`git worktree`, chứ không phải một con số chép lại.

---

### 2026-09-03, lượt 2 (tiếp) — bảng số chỉ ra một luật optimizer còn thiếu

Đây là mục đáng giá nhất của cả phase, vì nó là lần đầu trong tám phase mà **số đo tìm ra một
thứ THIẾU, không phải một thứ SAI**.

Bảng pushdown lần chạy đầu:

```console
$ go run ./cmd/sqllab -work pushdown -rows 20000 -dim 200 -repeat 3
câu                                                           tắt(ms)    bật(ms)    tỉ số   hàng(tắt)   hàng(bật)
SELECT city FROM ev WHERE kind = 3 AND id < 20000              14.309      0.179   79.97x       20000         200
SELECT ev.city, dim.name FROM ev JOIN dim ON ev.kind=dim.…     16.235      0.271   59.94x       20200         400
SELECT ev.city, dim.name FROM ev JOIN dim ON ev.kind=dim.…     15.686     13.287    1.18x       20200       20005
```

**Đọc kết quả:** hai dòng đầu 60-80x, dòng thứ ba **1.18x**. Theo quy tắc số 5 của SKILL — *thấy
1.1x ở chỗ đáng lẽ 100x thì nghi bench sai* — tôi đi tìm lỗi bộ đo. Không có. Cột `hàng(bật)` nói
thật: **20005** — engine đọc gần như cả bảng dù pushdown đang bật.

Câu thứ ba là `... ON ev.kind = dim.kind AND dim.kind < 5`. Đẩy `dim.kind < 5` xuống thì nó thu
vế **build** — 200 hàng. Vế **probe** 20000 hàng **không ai chạm**. Và điều kiện đáng đẩy,
`ev.kind < 5`, **không có trong câu người ta gõ**.

Nên nó không phải bug. Nó là một **luật còn thiếu**: suy ra điều kiện qua phép bằng của join.

```go
// propagate SUY RA điều kiện qua phép bằng của join.
//	a.x = b.y  AND  b.y < 5   =>  thêm  a.x < 5
```

Đúng với **inner join**, vì mọi hàng ra đều thoả `a.x = b.y`, nên hàng mà điều kiện suy ra loại
bỏ là hàng vốn đã không join được. **Sai với LEFT JOIN** — ở đó hàng bên trái sống sót kể cả khi
không có bạn bên phải, nên loại nó đi là đổi kết quả. Đây là chỗ một phép tối ưu đúng cho một
phép join lại là bug cho một phép join khác.

Sau khi thêm luật:

```console
$ go run ./cmd/sqllab -work pushdown -rows 20000 -dim 200 -repeat 7
SELECT city FROM ev WHERE kind = 3 AND id < 20000              14.479      0.173   83.86x       20000         200
SELECT ev.city, dim.name FROM ev JOIN dim ON ev.kind=dim.…     17.812      0.393   45.35x       20200         300
SELECT ev.city, dim.name FROM ev JOIN dim ON ev.kind=dim.…     18.277      0.986   18.53x       20200        1005
```

**Đọc kết quả:** 1.18x → **18.53x**, hàng đọc 20005 → **1005**.

**Đang nghĩ gì:** ba dòng này là **ba cơ chế khác nhau**, không phải ba lần đo cùng một thứ — và
đó mới là câu trả lời cho câu hỏi số 5. Xem mục Rút ra.

---

### 2026-09-03, lượt 2 (tiếp) — fuzz bắt bug thật trong 13 giây

```console
$ make fuzz-sql
fuzz: elapsed: 0s, gathering baseline coverage: 268/268 completed, now fuzzing with 6 workers
fuzz: minimizing 65-byte failing input file
--- FAIL: FuzzParse (13.01s)
    --- FAIL: FuzzParse (0.00s)
        fuzz_test.go:84: in lại ra câu KHÔNG phân tích được:
              gốc:    "SELECT*FROM A WHERE''''"
              in lại: "SELECT * FROM A WHERE '''"
              lỗi: cú pháp: chuỗi chưa đóng nháy (cột 23)
              SELECT * FROM A WHERE '''
                                    ^
    Failing input written to testdata/fuzz/FuzzParse/41ccdf9bacf73daf
```

**Đọc kết quả:** `''''` là cách SQL viết một hằng chuỗi **chứa một dấu nháy**. Lexer **giải** phép
thoát đúng (`''` → `'`), nhưng hàm in làm ngược lại mà **không thoát**:

```go
return "'" + string(e.V.B) + "'"   // sai
```

Ba dấu nháy in ra không phải một câu SQL hợp lệ. Đây là **cùng một hình dạng** với phép thoát
`0x00 → 0x00 0xff` của `internal/keys` ở phase 7:

> Ai viết bộ mã hoá phải viết phép thoát, và nửa dễ quên luôn là nửa **GHI RA** — vì nửa đọc vào
> sai một cái là lỗi ngay, còn nửa ghi ra thì sai **lặng lẽ** cho tới khi có ai đọc lại.

Sau khi sửa (`quoteStr` trong `internal/sql/ast.go`, và phép thoát tương ứng cho EXPLAIN trong
`internal/plan/bind.go`):

```console
$ make fuzz-sql
fuzz: elapsed: 2m0s, execs: 3582747 (36346/sec), new interesting: 67 (total: 379)
PASS
ok  	minidb/internal/sql	120.203s
```

**Đang nghĩ gì:** bất biến bị vỡ là **in-lại-rồi-đọc-lại**, và không một bài test viết tay nào
trong bộ này chạm tới nó. Bốn dấu nháy liền nhau là thứ không ai nghĩ ra để gõ vào.

---

### 2026-09-03, lượt 3 — con bug thứ năm, tìm ra khi đang dựng ví dụ cho nhật ký

Đang viết mục Rút ra thì cần một ví dụ EXPLAIN ngắn. Gõ thử một câu **vô nghiệm**:

```console
$ go run ./cmd/minidb -db data/sql/diary.db -q -e "
EXPLAIN SELECT city FROM ev WHERE id < 3 AND id > 9;
EXPLAIN SELECT city FROM ev WHERE kind = 3 AND kind = 9;"
physical:
  Project city
    -> NoScan ev  (rows≈0 cost≈0) — khoảng rỗng theo điều kiện — không đọc gì
physical:
  Project city
    -> SeqScan ev filter=(ev.kind = 3) AND (ev.kind = 9)  (rows≈1 cost≈1)
```

**Đọc kết quả:** hai câu **cùng vô nghiệm**, hai kế hoạch **khác nhau**. Khoảng × khoảng thì nhận
ra rỗng; **bằng × bằng** thì không. Thu hẹp thêm một bước:

```console
$ ... "EXPLAIN SELECT city FROM ev WHERE id = 3 AND id > 9;"
    -> NoScan ev  (rows≈0 cost≈0) — khoảng rỗng theo điều kiện — không đọc gì
```

Nên chỉ đúng hình **bằng ∧ bằng** thoát được. Mở `extractSpans`:

```go
case sql.OpEq:
	a.lo, a.hi, a.hiIncl, a.eq = lit.V, lit.V, true, true   // GÁN
case sql.OpGt:
	if a.lo.IsNull() || keys.Compare(nv, a.lo) > 0 { a.lo = nv }   // GIAO
```

Nhánh `>` và `<` **giao** vào bộ tích luỹ; nhánh `=` **ghi đè** nó. Nên `kind = 3` bị `kind = 9`
xoá, khoảng ra `[9,9]` — không rỗng — và `kind = 3` tụt xuống làm residual. Kết quả vẫn **đúng**
(0 hàng), chỉ là engine đọc cả bảng để ra 0 hàng ấy.

**Vì sao cả bộ test xanh:** vì mọi bài test của phase 8 khẳng định về **kết quả**, và kết quả
đúng. Chỉ một lời khẳng định về **số hàng đọc** mới bắt được — cùng bài học với
`TestPushdownKeepsResults` và `TestSelectivityNotPredicateCount`. Đã sửa, và đã viết bài test
theo đúng hình đó:

```console
$ go test ./internal/plan/ -run 'TestContradictory|TestSingleEquality' -v -count=1
=== RUN   TestContradictoryEqualitiesGiveEmptySpan
=== RUN   TestContradictoryEqualitiesGiveEmptySpan/=_và_=
=== RUN   TestContradictoryEqualitiesGiveEmptySpan/<_rồi_=_ở_đúng_chặn_trên
=== RUN   TestContradictoryEqualitiesGiveEmptySpan/=_rồi_>_cao_hơn
--- PASS: TestContradictoryEqualitiesGiveEmptySpan (0.00s)
=== RUN   TestSingleEqualityStillGivesPointSpan
--- PASS: TestSingleEqualityStillGivesPointSpan (0.00s)
ok  	minidb/internal/plan	0.004s

$ go run ./cmd/minidb -db data/sql/diary.db -q -e "EXPLAIN SELECT city FROM ev WHERE kind = 3 AND kind = 9;"
    -> NoScan ev  (rows≈0 cost≈0) — khoảng rỗng theo điều kiện — không đọc gì
```

**Đang nghĩ gì:** phép bằng **trông như** một phép gán, nên tôi viết nó thành phép gán. Ba nhánh
cạnh nó đều là phép giao. Đây là bug rẻ nhất của cả phase để sửa và đắt nhất để **thấy** — và
việc nó xuất hiện ở **lượt viết nhật ký** là lập luận mạnh nhất tôi có cho quy tắc *"phải dựng
được ví dụ chạy thật cho mọi điều mình định viết ra"*.

---

### 2026-09-03, lượt 3 (tiếp) — EXPLAIN của chính mình, và ba giá trị của SQL

```console
$ go run ./cmd/minidb -db data/sql/diary.db -e "EXPLAIN SELECT ev.city, dim.name FROM ev JOIN dim ON ev.kind = dim.kind WHERE dim.kind < 5 ORDER BY ev.city;"
câu:      SELECT ev.city, dim.name FROM ev JOIN dim ON (ev.kind = dim.kind) WHERE (dim.kind < 5) ORDER BY ev.city

logical (sau khi đẩy điều kiện xuống):
  gốc:  Project ev.city, dim.name
            -> Sort by=ev.city
              -> Filter (dim.kind < 5)
                -> Join on=(ev.kind = dim.kind)
                  -> Scan ev
                  -> Scan dim
  tối ưu: Project ev.city, dim.name
               -> Sort by=ev.city
                 -> Join on=(ev.kind = dim.kind)
                   -> Scan ev  preds=(ev.kind < 5)
                   -> Scan dim  preds=(dim.kind < 5)

physical:
  Project ev.city, dim.name
    -> Sort by=ev.city budget=4096 hàng  (rows≈1 cost≈3)
      -> NestedLoopJoin on=(ev.kind = dim.kind)  (rows≈1 cost≈2)
        -> SeqScan ev filter=(ev.kind < 5)  (rows≈1 cost≈1) — chưa ANALYZE bảng này nên planner không có thống kê
        -> SeqScan dim filter=(dim.kind < 5)  (rows≈1 cost≈1) — chưa ANALYZE bảng này nên planner không có thống kê
```

**Đọc kết quả:** ba điều nhìn thấy được ở đây mà `EXPLAIN` của Postgres **không** cho thấy:

1. Cây **trước** và **sau** optimizer, cạnh nhau. Nên `Filter (dim.kind < 5)` **biến mất** khỏi
   giữa cây và **xuất hiện** dưới hai `Scan` — đó là pushdown, nhìn thấy trực tiếp. Postgres chỉ
   in cây physical, nên phép đẩy của nó chỉ thấy được **gián tiếp** qua hai dòng `Index Cond` và
   `Filter` — và đúng hai dòng ấy là chỗ người đọc EXPLAIN hay lẫn.
2. `preds=(ev.kind < 5)` dưới `Scan ev` — điều kiện **người gõ không viết**. Luật `propagate`.
3. `— chưa ANALYZE bảng này nên planner không có thống kê`. Một planner không có thống kê thì
   đoán, và câu này nói ra điều đó thay vì im lặng đoán sai.

Rồi ba giá trị của SQL, đo được ở mặt ngoài:

```console
$ go run ./cmd/minidb -db data/sql/diary.db -e "
INSERT INTO ev VALUES (6, NULL, 'null-kind');
SELECT id, kind FROM ev WHERE kind = NULL;
SELECT id, kind FROM ev WHERE kind < 5;
SELECT id, kind FROM ev WHERE kind >= 5;"
chèn 1 hàng vào ev
(0 hàng, 22µs · đọc 6 hàng bảng, 0 mục index, 0 lần tra bảng)
 1  | 3
 3  | 3
 5  | 3
(3 hàng, 7µs · đọc 6 hàng bảng, 0 mục index, 0 lần tra bảng)
 2  | 7
 4  | 9
(2 hàng, 8µs · đọc 6 hàng bảng, 0 mục index, 0 lần tra bảng)
```

**Đọc kết quả:** bảng có **6** hàng. `kind < 5` cho 3, `kind >= 5` cho 2, tổng **5**. Hàng có
`kind` là NULL **không thuộc bên nào** — một điều kiện và phủ định của nó **cùng** loại nó ra, nên
hợp của hai tập **không** phải cả bảng. Và `WHERE kind = NULL` cho **0 hàng**, không phải một hàng.

**Đang nghĩ gì:** chỗ này là bẫy thật, vì `internal/keys` của phase 7 định nghĩa
`Compare(Null, Null) == 0` — và **đúng**, vì nó là một phép **THỨ TỰ**: một cây B+Tree phải sắp
được NULL vào đâu đó cố định. Còn `NULL = NULL` trong SQL là NULL — cũng **đúng**, vì nó là một
phép **SO SÁNH**. Gộp hai phép ấy thành một thì `WHERE x = NULL` trả về hàng, và đó là lỗi ngữ
nghĩa chứ không phải lỗi cài đặt.

---

## Giả thuyết sai / bug đã gặp

| Tôi tưởng là | Thực tế là | Lệnh / output đã lật tẩy nó | Đã sửa thế nào |
|---|---|---|---|
| **Phase 8 xây trên nền phase 6/7 là được** | Mọi đường đọc của phase 6/7 là **push**; không join algorithm nào diễn tả được bằng callback mà không đệm cả một vế, sinh goroutine (`txn.Txn` không an toàn nhiều goroutine), hoặc đảo ngược kiểu continuation | Đọc `internal/txn/txn.go:202` (`Scan(lo, hi, fn)`) rồi thử viết `nestLoopOp` bằng nó | Đổi push → pull: `internal/txn/iter.go`, `internal/table/iter.go`. `Iter` là **bản duy nhất** cài phép merge, `Scan` là lớp bọc. Đo giá: **≤1.08x** |
| **Seq scan không dùng được khoảng** (trực giác mượn từ Postgres, nơi bảng là **heap** rời index) | Engine này là **clustered index** — hàng nằm **trong** cây pk (phase 7). Nên `WHERE pk < v` **là** một phép quét khoảng thật, không cần index phụ nào | `EXPLAIN SELECT city FROM ev WHERE id < 5` in ra `SeqScan ev filter=(ev.id < 5)`, đọc **5100** hàng để trả 5 hàng, **4.062ms** | Viết lại `scan()` thành bộ **liệt kê đường đi** có tính chi phí, thêm đường "khoảng trên tiền tố pk". Đo lại: **105** hàng, **88µs** = **46x** |
| **`query.Choose` của phase 7 dùng lại được** | Một hàm trả về **một** kế hoạch không diễn tả nổi *"đường này còn tặng bạn một thứ tự sắp xếp"*. Và cái giết nó **không phải join** — mà là **ORDER BY** | Ba bài `TestOrderByUsesIndex`/`TestOrderByDescAndMultiKey` không viết được bằng API `Choose` | `type path` + hàm liệt kê; **giữ lại** `query.CostModel` + `Selectivity`. Cái sống sót từ phase 7 là **mô hình**, không phải hàm chọn |
| **Cột nào có nhiều điều kiện hơn thì đẩy cột đó** | Phải chọn theo **selectivity**. `kind = 3 AND id < 20000`: mỗi cột đúng một điều kiện, nên phép đếm chọn `id` — tức **cả bảng** — thay vì `kind` (0.5%) | Kết quả vẫn **đúng**, chỉ chậm. Chỉ một lời khẳng định về **số hàng đọc** bắt được: `TestSelectivityNotPredicateCount` | `extractSpans` trả về **mọi** ứng viên; việc chọn dời vào `scan()` — chỗ có thống kê |
| **`Est{Rows:0, Cost:0}` nghĩa là nó không đọc gì** | "Chi phí 0" là phát biểu về **ước lượng**; "không đọc gì" là phát biểu về **thi hành**. Hai thứ **không tự đồng bộ với nhau** | Câu `id < 3 AND id > 9` ước lượng 0 hàng nhưng toán tử vẫn quét và lọc cả bảng | Thêm cờ `PScan.Empty` + toán tử `exec.emptyOp` (in ra `NoScan`) |
| **Khoảng rỗng thì `Span.Empty` nhận ra hết** | Nhánh `OpEq` của `extractSpans` là phép **GÁN**, ba nhánh cạnh nó là phép **GIAO**. Nên `x = 3 AND x = 9` ra `[9,9]` — không rỗng — và quét cả bảng cho một câu vô nghiệm | `EXPLAIN ... WHERE kind = 3 AND kind = 9` → `SeqScan`, trong khi `WHERE id = 3 AND id > 9` → `NoScan`. **Tìm ra ở lượt 3, lúc dựng ví dụ cho nhật ký** | Cho `OpEq` giao đúng như `>`/`<`; thêm `TestContradictoryEqualitiesGiveEmptySpan` (3 hình) + `TestSingleEqualityStillGivesPointSpan` (không được làm hỏng ca thường) |
| **Đường lỗi của parser thì cứ trả về lỗi là xong** | `advance()` là **no-op** khi `p.err` đã set, nên vòng lặp không tiến — **treo vô hạn**. Một đường lỗi không tiến được thì không phải lỗi, nó là một cái **treo** | `WHERE x = = 1` làm cả bộ test đứng **120s** rồi timeout, thay vì đỏ | `errf` đặt `p.tok = Token{Kind: EOF}`. Cùng hình với phép quét không kết thúc của phase 7: cả hai lần, thứ còn thiếu là **bảo đảm mọi bước đều tiến** |
| **`query.DefaultCost` của phase 7 dùng được cho planner mới** | `CFetch = 20` là con số phase 7 **cố tình giữ sai** để idxlab có cột "planner đoán sai". Dùng nó thì planner **luôn** chọn seq | Ba bài test của `internal/engine` đỏ cùng lúc | Thêm `query.MeasuredCost{CSeq:1, CIndex:0.6, CFetch:2.4}` từ số đo phase 7 (hoà vốn **mô hình 33.3%** vs **đo 36.8%**); `NewPlanner` dùng bản này, `DefaultCost` **vẫn sai có chủ ý** |
| **"Pushdown làm chậm đi 0.72x"** và **thời gian sắp xếp không đơn điệu theo số run** | Cả hai là **nhiễu** của `-repeat 1` / `-repeat 3` | Chạy lại `-repeat 3` rồi `-repeat 7`: cả hai hiện tượng tan | Nâng repeat. Và lần chạy lại cho **kết quả tốt hơn** cái nhiễu đã che: bảng sort là một **bậc thang**, không phải đường dốc |
| **Pushdown cho join là một hệ số, đo là xong** | Với `ON ev.kind=dim.kind AND dim.kind<5`, đẩy `dim.kind<5` chỉ thu vế **build** (200 hàng); vế **probe** (20000) không ai chạm. Điều kiện đáng đẩy, `ev.kind<5`, **không có trong câu người gõ** | `sqllab -work pushdown`: dòng thứ ba **1.18x** trong khi hai dòng kia 60-80x; cột hàng(bật) = **20005** | Thêm luật logical `plan.propagate` (suy ra qua phép bằng; **sai với LEFT JOIN**, có ghi rõ). Dòng ấy thành **18.53x**, hàng đọc 20005 → **1005**. Đây là lần đầu **số đo tìm ra thứ THIẾU** |
| **Baseline của `BenchmarkScan` là con số trong diary phase 6** (688-732µs) | Phase 7 viết lại `Scan` thành streaming (trả nợ P6-4) và **không đo lại**. **Một con số cũ trong nhật ký là một con số vô hình** | `git worktree add /tmp/p7 12ad70c` rồi chạy cùng lệnh: **317-363µs**, không phải 688-732µs | Đo trong worktree ở đúng HEAD phase 7. Phát hiện kèm theo: phase 6 → 7 là một cú thắng **ngầm ≈2.0x** chưa ai ghi |
| **Hàm in một hằng chuỗi thì có gì mà sai** | Lexer **giải** `''` → `'`, hàm in làm ngược lại mà **không thoát**. `''''` (một dấu nháy) in ra `'''` — không phân tích lại được | `make fuzz-sql` đỏ sau **13 giây** ở lần chạy đầu; input lưu ở `testdata/fuzz/FuzzParse/41ccdf9bacf73daf` | `quoteStr` trong `ast.go` + phép thoát tương ứng trong `bind.go`. Cùng hình với `0x00 → 0x00 0xff` của phase 7 |
| **`parse − lex` cho biết chi phí phân tích thuần** | `Tokens` cắt sẵn cả câu vào một slice; `Parse` lex **theo yêu cầu**. Hai đại lượng khác nhau, trừ nhau ra **số âm** | Cột `parse-lex` in ra giá trị âm | Đổi nhãn thành `parse*` và ghi rõ *"đã bao gồm lex"*. Không cột nào được trừ cho cột nào |
| **Cột "vs tốt nhất" so với bản tốt nhất** | Tôi cài nó bằng **min chạy dần**, nên **hàng đầu luôn 1.00x** dù nó tệ nhất | Bảng hạn mức: hàng `budget=1` in `1.00x` | Đo hết rồi mới in. Cùng loại với bốn lỗi bộ đo của phase 7 |
| **`-benchtime=200000x` dùng chung cho `Get` và `Scan` được** | `-benchtime` tính **theo phép toán**, và `BenchmarkScan` quét **2000 khoá mỗi phép** → **1.2 tỉ** bước khoá | Lệnh chạy mãi không xong; phải hủy | `-benchtime=300x` cho Scan. `bench-txn` gộp `Get\|Scan` dưới một benchtime là một **bug của bộ đo**: đúng cho bench theo khoá, vô lý cho bench theo lần quét |
| **Hai kỳ vọng test của tôi sai, code đúng** (2 lần) | (a) `SELECT a FORM t` báo lỗi ở `t`, không ở `FORM` — vì alias SQL không cần `AS`, nên từ khoá viết sai bị **nuốt thành alias**; Postgres cũng báo y hệt. (b) Điều kiện OR nằm lại trong `Join.On` chứ không thành `Filter` bên trên — **tương đương** với inner join | Chạy test và đọc kỹ output thay vì sửa code | Ghi lại thành `TestMisspelledKeywordPointsElsewhere`; và phát biểu lại bất biến thật: *"không phần nào của OR chạm tới Scan"*, thêm `TestOrOnOneRelationIsPushed` cho chiều ngược |

## Số đo

Mọi bảng dưới đây: **máy** i5-1235U / 6 core / WSL2 ext4 / go1.26.2, **ngày** 2026-09-03,
**commit** phase 8 (xem header), baseline đối chiếu **`12ad70c`** (HEAD phase 7).

### Cái giá của ca mổ push → pull

```console
$ go test ./internal/txn/ -run '^$' -bench BenchmarkScan -benchtime=300x -count=3
```

| | lần 1 | lần 2 | lần 3 |
|---|---|---|---|
| phase 7 HEAD (`12ad70c`, qua `git worktree`) | 333353 ns | 363481 ns | 317915 ns |
| phase 8 (pull) | 329262 ns | 380667 ns | 357590 ns |

Hai dải **chồng nhau**. Trung vị 357590/333353 = **1.07x**. Cận xấu nhất 1.20x. Chốt: **≤1.08x**,
và phát biểu đúng là *"không đo được khác biệt ngoài nhiễu"*.

Con số cũ trong diary phase 6 là **688-732µs** — đã lỗi thời từ phase 7. Nên phase 6 → 7 là
**≈2.0x** chưa ai ghi lại.

### 1. Nested loop vs hash join

```console
$ go run ./cmd/sqllab -rows 20000 -dim 200 -repeat 7
       W   hàng ra   nested(ms)     hash(ms)   nl/hash  vòng ngoài      đọc(nl) planner chọn
       1         1        0.087        0.138     0.63x           1          201   NestedLoop
       2         2        0.174        0.138     1.25x           2          402   NestedLoop
       5         5        1.038        0.115     9.00x         200         1200     HashJoin
      20        20        3.076        0.117    26.34x         200         4200     HashJoin
     100       100       13.325        0.231    57.64x         200        20200     HashJoin
     500       500       49.798        0.555    89.79x         500       100500     HashJoin
    2000      2000      206.090        1.700   121.25x        2000       402000     HashJoin
   10000     10000        quá đắt        8.433         —           -            -     HashJoin
   20000     20000        quá đắt       17.068         —           -            -     HashJoin
```

Điểm đổi vai nằm giữa **W=2 và W=5**. Cột `đọc(nl)` là chỗ đáng đọc nhất: ba dòng giữa có **vòng
ngoài đứng yên ở 200** mà thời gian tăng **13x** — vì vòng **trong** lớn dần theo W. Nested loop
trả giá bằng một phép **nhân**.

### 2. Hạn mức bộ nhớ của hash join

```console
$ go run ./cmd/sqllab -work budget -rows 20000 -dim 200 -repeat 7
   hạn mức  thời gian vs tốt nhất     file tạm     byte ghi hàng đọc lại    tràn?
         1    38.406ms       2.12x           64      1475090        20200       CÓ
         4    38.941ms       2.15x           64      1475090        20200       CÓ
        16    39.090ms       2.16x           64      1475090        20200       CÓ
        64    39.091ms       2.16x           64      1475090        20200       CÓ
       100    37.996ms       2.10x           64      1475090        20200       CÓ
       200    18.116ms       1.00x            0            0            0    không
       400    19.302ms       1.07x            0            0            0    không
      4096    19.197ms       1.06x            0            0            0    không
```

Ngưỡng nằm **đúng ở kích thước vế build** (200 hàng). Dưới ngưỡng: **2.10-2.16x**, y như nhau dù
hạn mức đi từ 100 xuống 1. Trên ngưỡng: thêm bộ nhớ **không mua được gì** (200 → 4096 hàng là
1.00x → 1.06x, tức nhiễu).

### 3. ORDER BY: trong RAM vs external merge sort

```console
$ go run ./cmd/sqllab -work sort -rows 20000 -dim 200 -repeat 7
       W  hạn mức  thời gian     vs RAM   số run     file tạm hàng đọc lại
    5000     5001     7.536ms      1.00x        0            0            0
    5000     2500    11.575ms      1.54x        2            2         5000
    5000      625    11.585ms      1.54x        8            8         5000
    5000      512    11.518ms      1.53x       10           10         5000
    5000      156    14.049ms      1.86x       33           33         5000
    5000       64    16.291ms      2.16x       79           79         5000

   20000    20001    31.850ms      1.00x        0            0            0
   20000    10000    46.404ms      1.46x        2            2        20000
   20000     2500    45.867ms      1.44x        8            8        20000
   20000      625    48.004ms      1.51x       32           32        20000
   20000      512    48.575ms      1.53x       40           40        20000
   20000       64    77.199ms      2.42x      313          313        20000
```

**Đây là bảng cho nhiều thông tin nhất của cả phase**, và nó chỉ hiện ra ở `-repeat 7`:

- Bước qua ngưỡng "không vừa RAM" tốn **1.46x**.
- Rồi giảm hạn mức thêm **20 lần** (10000 → 512), số run tăng **20 lần** (2 → 40), thời gian đi
  từ 1.46x lên **1.53x** — tức **không tốn thêm gì**.
- Chỉ khi số run lên **313** thì mới thành 2.42x.

**Suy ra được một điều từ chính chỗ phẳng ấy:** phép **so sánh** không phải chỗ tốn. Đi từ 2 run
lên 40 run là log₂40/log₂2 = **5.3 lần** nhiều so sánh hơn trong bước trộn, mà thời gian **không
đổi**. Nên cái tốn là **ghi và đọc đĩa**. Và trộn rẻ vì khoá run được ghi ra ở dạng mã hoá **giữ
thứ tự** của phase 7, nên so sánh là `bytes.Compare` — memcmp trên byte thô, đo riêng ở phase 7:
**2.468 vs 14.08 ns = 5.7x** rẻ hơn so theo kiểu.

Đây là lý do tinh chỉnh `work_mem` của Postgres có một **vách** chứ không có **độ dốc**: điều đáng
biết là ở **trên hay dưới** ngưỡng, không phải **cách ngưỡng bao xa**.

### 4. Predicate pushdown, trong chính engine này

```console
$ go run ./cmd/sqllab -work pushdown -rows 20000 -dim 200 -repeat 7
câu                                                           tắt(ms)    bật(ms)    tỉ số   hàng(tắt)   hàng(bật)
SELECT city FROM ev WHERE kind = 3 AND id < 20000              14.479      0.173   83.86x       20000         200
SELECT ev.city, dim.name FROM ev JOIN dim ON ev.kind=dim.…     17.812      0.393   45.35x       20200         300
SELECT ev.city, dim.name FROM ev JOIN dim ON ev.kind=dim.…     18.277      0.986   18.53x       20200        1005
```

Ba dòng, **ba cơ chế khác nhau** (xem Rút ra). Dòng thứ ba trước khi có `plan.propagate`: **1.18x**
với **20005** hàng đọc.

### 5. Bỏ bước ORDER BY nhờ thứ tự index (trả nợ P7-9)

```console
$ go run ./cmd/sqllab -work order -rows 20000 -dim 200 -repeat 7
câu                                               LIMIT     có Sort     bỏ Sort    tỉ số
SELECT id, kind FROM ev ORDER BY kind                 -     24.604ms      7.880ms    3.12x
SELECT id, kind FROM ev ORDER BY kind LIMIT 10       10     15.581ms      0.005ms 3284.33x
SELECT id, city FROM ev ORDER BY id                   -     40.319ms     14.036ms    2.87x
SELECT id, city FROM ev ORDER BY id LIMIT 10         10     24.698ms      0.007ms 3344.74x
```

Không LIMIT: **2.87-3.12x** (một hệ số). Có `LIMIT 10`: **3284-3345x** (một **bậc**). Vì `Sort`
là toán tử **CHẶN** — phải đọc và sắp cả bảng trước khi trả hàng đầu tiên. Bỏ được nó thì
`LIMIT k` đi từ O(n log n) xuống **O(k)**.

### 6. Chi phí front-end

```console
$ go run ./cmd/sqllab -work pipeline -rows 20000 -dim 200 -repeat 7
câu                                              lex(µs)    parse*      bind  optimize      plan    thi hành
SELECT id, city FROM ev WHERE id = 12345            0.94      1.09      0.96      0.11      2.10       1.82µs
SELECT city FROM ev WHERE kind = 3 AND id < 2…      0.95      1.15      1.21      0.11      3.25     165.23µs
SELECT ev.city, dim.name FROM ev JOIN dim ON …      2.05      2.15      1.57      1.14      3.02     217.26µs
```

`parse*` **đã bao gồm** lex — hai cột đầu không trừ được cho nhau.

Với một truy vấn **điểm**: front-end (parse\* + bind + optimize + plan = 5.30µs) so với thi hành
(1.82µs) = **2.9x**. Và nó lặp lại **y nguyên** mỗi lần chạy cùng câu ấy. Với truy vấn quét thì
tỉ số đảo ngược: 5.72µs so với 165.23µs = **0.035x**.

### Fuzz và test

```console
$ make fuzz-sql
fuzz: elapsed: 2m0s, execs: 3582747 (36346/sec), new interesting: 67 (total: 379)
PASS
ok  	minidb/internal/sql	120.203s

$ go test ./... -count=1
Go test: 358 passed in 28 packages
```

### Hồi quy sau ca mổ push → pull

```console
$ go run ./cmd/crashlab -n 20
20/20 vòng đúng. 709 transaction đã commit được kiểm, 8.161s.

$ go run ./cmd/crashlab -n 10 -nowrite
0/10 vòng đúng, 10 sai. 210 transaction đã commit được kiểm, 5.363s.
exit status 1
```

Bài phản chứng cho **hai** kiểu chết, cả hai đều đáng đọc: `thiếu 430, thừa 0` (mất transaction
đã commit — vỡ D) và `panic: btree: cell 0: slot 0 trỏ ra ngoài (off=7 len=0)` (**page hỏng nửa
vời** — đúng thứ WAL tồn tại để ngăn).

`make test-txn` và `make test-index` đều exit 0; lưới anomaly × mức isolation **giống từng ô** so
với phase 6, gồm cả hai ô **VỠ (lệch +7)** ở read-uncommitted và read-committed — hai mức ấy
**được phép** sai, và một bài test chỉ chứng minh được điều gì khi nó **biết đỏ**.

### Tỉ số cần nhớ

| Tỉ số | Giá trị | Ý nghĩa |
|---|---|---|
| push → pull, `BenchmarkScan` | **≤1.08x** | API pull không đắt hơn push. Cái đắt là **không có** nó |
| phase 6 → 7, `BenchmarkScan` | **≈2.0x** | Một cú thắng ngầm, phát hiện ra vì đi đo baseline thật thay vì chép số cũ |
| nested loop / hash, W=2 → W=2000 | **1.25x → 121.25x** | `\|L\|·\|R\|` vs `\|L\|+\|R\|`. Điểm đổi vai ở W≈3 |
| hash join, dưới / trên ngưỡng tràn | **2.10-2.16x** | Hạn mức là tham số **đổi thuật toán** |
| hash join, trên ngưỡng: 200 → 4096 hàng | **1.06x** | Trên ngưỡng, thêm RAM **không mua được gì** |
| sort, vào ngưỡng tràn (0 → 2 run) | **1.46x** | Cái giá của **bậc thang** |
| sort, 2 run → 40 run (hạn mức /20) | **1.46x → 1.53x** | Chỗ **phẳng** → so sánh không phải chỗ tốn |
| sort, 40 → 313 run | **1.53x → 2.42x** | Bề rộng phép trộn chỉ hiện ra ở hàng trăm run |
| memcmp vs so theo kiểu (đo ở phase 7) | **5.7x** | Vì sao phép trộn k đường rẻ: khoá đã mã hoá giữ thứ tự |
| pushdown một bảng (khoảng quét trên index) | **83.86x** | Hàng không khớp **không được đọc** |
| pushdown vào vế build của join | **45.35x** | Thu bảng băm → giảm khả năng tràn đĩa |
| pushdown **suy ra** qua phép bằng | **1.18x → 18.53x** | Luật `propagate`. Số đo tìm ra thứ **THIẾU** |
| bỏ Sort, không LIMIT | **2.87-3.12x** | Một hệ số |
| bỏ Sort, có `LIMIT 10` | **3284-3345x** | Một **bậc** — vì Sort là toán tử **chặn** |
| khoảng pk: trước / sau khi sửa bug seq-scan | **46x** (5100→105 hàng, 4.062ms→88µs) | Bug tôi tự tạo bằng trực giác mượn từ Postgres |
| front-end / thi hành, truy vấn **điểm** | **2.9x** | Vì sao prepared statement và plan cache tồn tại |
| front-end / thi hành, truy vấn **quét** | **0.035x** | Cùng một front-end, tỉ số đảo ngược |
| hoà vốn selectivity: mô hình / đo (phase 7) | **33.3% / 36.8%** | `MeasuredCost` hiệu chỉnh từ số đo, không từ sách |

## Invariant tôi đã cài và lệnh kiểm chứng nó

| Invariant | Cài ở đâu (file:hàm) | Lệnh kiểm chứng | Kết quả |
|---|---|---|---|
| **Hai đường phải cho CÙNG một kết quả** (nested loop vs hash, tràn vs không, có Sort vs bỏ, đẩy vs không đẩy) | `internal/engine/engine_test.go:runPlanFull` (dựng kế hoạch **bằng tay**, có hook `rewrite` để chạy đường planner **không** chọn) | `make test-sql` | PASS, `-race` |
| Parser **luôn kết thúc** với mọi chuỗi byte | `internal/sql/parser.go:errf` (đặt `p.tok` về EOF) | `make fuzz-sql` (watchdog 2s/đầu vào trong `fuzz_test.go`) | 3 582 747 exec, PASS |
| In lại rồi phân tích lại thì **bền** | `internal/sql/ast.go:quoteStr` | `go test ./internal/sql -run 'FuzzParse/41ccdf9bacf73daf'` | PASS (trước khi sửa: đỏ sau 13s) |
| Mỗi `Lexer.Next` hoặc trả EOF hoặc **đẩy con trỏ** | `internal/sql/lexer.go:Next` | `make fuzz-sql` (FuzzLexer 60s) | exit 0 |
| `Iter` là **bản duy nhất** cài phép merge write-set × cây; `Scan` chỉ bọc | `internal/txn/iter.go:Txn.Scan` | `make test-txn`, `go test ./internal/txn -bench BenchmarkScan` | exit 0; ≤1.08x |
| Khoá khoảng của Serializable lấy **lúc mở** iterator (predicate lock ⇒ chặn phantom) | `internal/txn/iter.go:Txn.Iter` | `make test-txn` (lưới anomaly, cột phantom) | exit 0, giống phase 6 từng ô |
| Khoảng **rỗng** thì không đọc gì (ước lượng **và** thi hành) | `internal/plan/planner.go:extractSpans` + `physical.go:Span.Empty` + `internal/exec/scan.go:emptyOp` | `go test ./internal/plan -run 'TestSpanEmpty\|TestContradictory\|TestSingleEquality'` | PASS (5 hình) |
| Đẩy điều kiện **không đổi kết quả**, và phải đọc **ít hàng hơn** | `internal/plan/opt.go:push` | `go test ./internal/engine -run TestPushdownKeepsResults` | PASS |
| Điều kiện **suy ra** qua phép bằng chỉ áp cho **inner join** | `internal/plan/opt.go:propagate` (dedup theo `String()` ⇒ **idempotent**) | `go test ./internal/plan -run TestPropagate` | PASS |
| Logic **ba giá trị**: `NULL = NULL` là NULL; `AND` **không** short-circuit trên NULL | `internal/plan/bind.go:CmpExpr.Eval`, `LogicExpr.Eval`, `True` | `go test ./internal/plan -run 'TestThreeValuedTruthTable\|TestCompareWithNullIsNull'` | PASS |
| Sắp ngoài cho **cùng** thứ tự với sắp trong RAM, kể cả khoá nhiều cột ASC/DESC lẫn nhau | `internal/exec/sort.go:keyOf` (mã hoá theo chiều ⇒ một `bytes.Compare` là đủ) | `go test ./internal/engine -run 'TestExternalSortAgrees\|TestOrderByDescAndMultiKey'` | PASS |
| Hash join **tràn đĩa** cho cùng kết quả với bản trong RAM — **và bài test phải chứng minh là nó có tràn** | `internal/exec/join.go:startSpill`/`probeSpill` | `go test ./internal/engine -run TestHashJoinSpillAgrees` (khẳng định `SpillFiles == 0` khi hạn mức lớn **và** `> 0` khi hạn mức 2) | PASS |
| File tạm không sống sót một lần crash | `internal/exec/spill.go:newRowFile` (`os.CreateTemp` rồi `os.Remove` ngay) | `ls /tmp` sau khi hủy giữa truy vấn | không còn gì |
| Durability sống qua ca mổ push → pull | `internal/db`, `internal/wal` (không sửa) | `go run ./cmd/crashlab -n 20` | 20/20, 709 txn |
| Bộ đo crash **biết báo SAI** | `cmd/crashlab -nowrite` | `go run ./cmd/crashlab -n 10 -nowrite` | **0/10, exit 1** |

## Đọc gì

- **Volcano/Iterator model** — Graefe. Đọc *sau* khi tự đụng phải vách push/pull, và lúc đó ba
  chữ "open/next/close" mới có nghĩa. Nó cũng cho biết cái tôi làm **không** phải Volcano thuần:
  `nestLoopOp` **dựng lại** cả cây con cho mỗi hàng vế ngoài thay vì `rewind`, vì một cây con có
  thể chứa `Sort` hoặc một hash join khác — và cả hai đều **không rewind được**.
- **SQLite: bytecode engine** — chỗ đáng đọc là **vì sao** nó không phải Volcano: biên dịch ra
  bytecode thì vòng lặp nằm trong VM, nên câu hỏi push-hay-pull **biến mất**. Postgres là Volcano;
  hai hướng giải **cùng một** vấn đề mà lượt 1 của phase này gặp.
- **Grace hash join** — và chỗ tôi **cố ý làm sai**: `numParts` cố định **32**, không chia phần
  **đệ quy**. Nên nếu một phần vẫn không vừa RAM thì bản này thua, và mọi kết luận của bảng số 2
  là **có điều kiện** trên chỗ ấy. Ghi rõ tại `internal/exec/join.go`.
- **Three-valued logic trong chuẩn SQL** — đọc vì `internal/keys` của phase 7 buộc phải có
  `Compare(Null, Null) == 0`, và cần biết chắc rằng đó **không** mâu thuẫn: một cái là **thứ tự**,
  một cái là **so sánh**.
- **Architecture of a Database System** (Hellerstein), mục query optimizer — đọc lại sau khi
  `plan.propagate` xuất hiện, để biết cái mình vừa tự suy ra có tên gọi là gì.

## Rút ra (viết như thể giải thích cho người khác)

### 1. Logical plan khác physical plan ở đâu?

Không phải "một cái trừu tượng hơn". Lằn ranh sắc và kiểm chứng được:

> **Một phép biến đổi logical là một ĐỊNH LÝ. Một phép biến đổi physical là một TÌNH HUỐNG.**

Đẩy điều kiện xuống là định lý: σ_p(L ⋈ R) = L ⋈ σ_p(R) là đẳng thức trên tập hợp, đúng bất kể
sau đó chạy bằng gì. Bỏ bước `ORDER BY` là tình huống: chỉ đúng nếu **đường đi đã chọn** tình cờ
trả về thứ tự ấy; đổi đường đi thì phép tối ưu bốc hơi.

Hệ quả cho **cách viết code**: `opt.go` và `planner.go` là hai file, không phải một. Và hệ quả cho
**cách đọc EXPLAIN**: `minidb` in **cả hai** cây, trước và sau optimizer. Postgres chỉ in cây
physical — nên phép đẩy của nó chỉ thấy được **gián tiếp** qua `Index Cond` (đã thành khoảng quét)
và `Filter` (còn phải lọc từng hàng). Đúng hai dòng ấy là chỗ người đọc EXPLAIN hay lẫn, và biết
vì sao chúng khác nhau là biết pushdown đã làm được đến đâu.

Một hệ quả nữa mà tôi không lường trước: **thuộc tính vật lý bắt buộc phải đi XUỐNG.** `req
[]SortKey` được luồn xuống qua `build`, vì "hãy trả về theo thứ tự này" là một yêu cầu mà **lá**
mới trả lời được. Và đúng chỗ ấy có một chi tiết dễ sai: `Join` truyền **nil** xuống, không truyền
`req` — nested loop chỉ bảo toàn thứ tự vế ngoài nếu mỗi hàng vế ngoài khớp **≤1** hàng vế trong,
điều không chứng minh được ở đây; còn hash join thì không hứa gì cả.

### 2. Nested loop vs hash join: khi nào cái nào thắng?

Nested loop là `|L| · |R|`, hash join là `|L| + |R|`. Nên trên máy này, với vế trong 200 hàng,
điểm đổi vai ở **W ≈ 3**: 1.25x tại W=2, 9.00x tại W=5, **121.25x** tại W=2000.

Nhưng con số đáng nhớ không phải cái đó, mà là cột `đọc(nl)`. Ba dòng giữa bảng có **vòng ngoài
đứng yên ở 200** trong khi thời gian tăng **13x**. Nếu chỉ nhìn "số lần chạy lại vế trong" thì
bảng ấy **vô nghĩa**. Vòng ngoài đứng yên vì planner chọn vế **ít hàng** làm vòng ngoài; cái lớn
dần là vòng **trong**. Nested loop trả giá bằng một phép **nhân**, và một bảng số chỉ nói được
điều đó khi nó đo **cả hai thừa số**.

Và một điều về cài đặt mà sách không nói: `nestLoopOp` của tôi **dựng lại** cả cây con vế trong
cho mỗi hàng vế ngoài, vì cây con có thể chứa `Sort` hoặc hash join — hai thứ **không rewind
được**. Nó đếm `InnerScans`, nên **toàn bộ điểm yếu của thuật toán hiện ra trong thống kê** thay
vì ẩn trong thời gian.

### 3. Hash join cần memory budget để làm gì?

> **Hạn mức bộ nhớ không phải một tham số tinh chỉnh. Nó là một tham số ĐỔI THUẬT TOÁN.**

Trên ngưỡng: một bảng băm trong RAM, mỗi vế đi qua **một** lần. Dưới ngưỡng: Grace hash join chia
cả hai vế thành phần, **ghi ra đĩa**, rồi nạp lại từng cặp phần — mỗi vế đi qua **ba** lần.

Số đo cho thấy đúng một **bậc**, không phải một đường dốc: dưới ngưỡng là **2.10-2.16x**, y như
nhau dù hạn mức đi từ 100 xuống 1; trên ngưỡng thì thêm RAM **không mua được gì** (200 → 4096
hàng = 1.06x, tức nhiễu). Ngưỡng nằm **đúng ở kích thước vế build**.

Đây là lý do một bài test tràn đĩa **phải khẳng định là nó có tràn**. `TestHashJoinSpillAgrees`
kiểm `SpillFiles == 0` khi hạn mức lớn **và** `> 0` khi hạn mức 2 — thiếu vế thứ hai thì bài test
xanh mà chẳng chứng minh gì.

### 4. ORDER BY khi dữ liệu không vừa RAM?

Sinh **run** đã sắp, mỗi run một file tạm, rồi **trộn k đường**. Nhưng cái đáng nhớ là hình dạng
của cái giá:

> **Cái giá của việc tràn ra đĩa là một BẬC THANG, không phải một đường dốc.**

Bước vào ngưỡng: **1.46x**. Rồi giảm hạn mức thêm **20 lần** (2 → 40 run): **1.53x**, tức không
tốn thêm gì. Chỉ ở **313 run** mới thành 2.42x.

Từ chính chỗ phẳng đó suy ra được thứ mà tôi **không đo trực tiếp**: 2 run → 40 run là **5.3 lần**
nhiều so sánh hơn trong bước trộn, mà thời gian không đổi ⇒ **so sánh không phải chỗ tốn**, ghi và
đọc đĩa mới là. Và so sánh rẻ vì một quyết định của **phase 7**: khoá run được ghi ra bằng
`keys.Encode`, bộ mã hoá **giữ thứ tự**, nên `bytes.Compare(a, b) == keys.CompareTuple(...)` và
phép trộn là **memcmp trên byte thô** — đo riêng ở phase 7: rẻ hơn **5.7x**. `DESC` là **phép bù
byte**, nên khoá nhiều cột ASC/DESC lẫn nhau vẫn so bằng **một** `bytes.Compare`.

Đó là **cổ tức thứ hai, không dự tính** của bộ mã hoá giữ thứ tự. Cổ tức thứ nhất là index scan
của phase 7.

Và đây là lý do tinh chỉnh `work_mem` của Postgres có một **vách** chứ không có độ dốc: điều đáng
biết là ở **trên hay dưới** ngưỡng, không phải cách ngưỡng bao xa.

Một chỗ nữa: `internal/exec/spill.go` là **chỗ duy nhất trong cả repo ghi ra đĩa mà KHÔNG fsync**.
Một lần fsync là **1.58ms** (số đo phase 0) trả cho một bảo đảm **vô nghĩa** — file tạm không cần
sống sót qua crash, và nó còn được `os.Remove` ngay sau khi tạo. Sau bốn phase dạy điều ngược lại,
**biết KHI NÀO không cần fsync** cũng là một phần của việc hiểu fsync.

### 5. Predicate pushdown giúp được gì trong engine của tôi?

Đây là câu mà số đo **sửa lại** cho tôi. Tôi tưởng nó là một hệ số. Nó là **ba cơ chế**:

1. **Một bảng: 83.86x.** Điều kiện thành **khoảng quét** trên index (hoặc trên cây pk). Hàng không
   khớp **không được đọc** — khác hẳn "đọc rồi bỏ". Đây là chỗ ăn nhiều nhất, và nó đo được ở cột
   `hàng(bật)`: 20000 → 200.
2. **Join, điều kiện trên vế build: 45.35x.** Vế build teo lại ⇒ bảng băm nhỏ hơn ⇒ **ít khả năng
   phải tràn đĩa**. Nên cơ chế này thực chất là mục 3 của phần trên: nó dịch chuyển bạn qua bên
   **đúng** của cái bậc thang.
3. **Join, điều kiện SUY RA qua phép bằng: 1.18x → 18.53x.** Và đây là bài học lớn nhất của cả
   phase.

Cơ chế thứ ba **không có trong thiết kế của tôi**. Nó xuất hiện vì bảng số in ra **1.18x** ở chỗ
hai dòng kia in 60-80x. Theo quy tắc số 5 của SKILL tôi đi tìm lỗi bộ đo — không có; cột
`hàng(bật)` nói thật là **20005**. Chẩn đoán: câu là `ON ev.kind = dim.kind AND dim.kind < 5`, và
đẩy `dim.kind < 5` chỉ thu vế **build** 200 hàng, còn vế **probe** 20000 hàng không ai chạm. Điều
kiện đáng đẩy — `ev.kind < 5` — **không có trong câu người ta gõ**. Phải **suy ra** nó.

Nên bài học không phải "pushdown ăn bao nhiêu x", mà:

> **Một bảng số tốt không chỉ trả lời câu hỏi bạn đặt ra. Nó chỉ cho bạn thấy chỗ bạn chưa đặt
> câu hỏi nào.**

Bảy phase trước, số đo tìm ra thứ **SAI** (phase 4: 4/7 giả thuyết bị bác; phase 7: bốn bug của bộ
đo). Phase 8 là lần đầu số đo tìm ra thứ **THIẾU**.

Và luật ấy có một giới hạn phải nói ra: nó **đúng với inner join** vì mọi hàng ra đều thoả
`a.x = b.y`, nên hàng bị loại vốn đã không join được; nó **sai với LEFT JOIN**, nơi hàng bên trái
sống sót kể cả khi không có bạn bên phải. Một phép tối ưu đúng cho một phép join là **bug** cho
một phép join khác — nên `propagate` phải kiểm loại join, không phải chỉ kiểm hình dạng điều kiện.

### Ba điều học được mà không nằm trong năm câu hỏi

**(a) Trực giác mượn từ một engine khác là loại bug đắt nhất.** Bug tệ nhất của phase này do tôi tự
tạo ra: tôi **cố ý** đẩy một khoảng trở lại thành `Filter` mỗi khi đường đi là seq scan, vì trong
Postgres bảng là **heap** rời index nên seq scan không có khoảng nào để dùng. Engine này là
**clustered index** — hàng nằm **trong** cây pk từ phase 7 — nên `WHERE pk < v` **là** một phép quét
khoảng thật. Tôi đã tự tắt phép tối ưu của chính mình: **5100 hàng thay vì 105, 4.062ms thay vì
88µs = 46x**. Và nó không phải một dòng sai — nó là một **câu đúng về một database khác**.

**(b) "Chi phí 0" và "không đọc gì" là hai phát biểu khác nhau, và chúng không tự đồng bộ.** Cái
đầu nói về **ước lượng**, cái sau nói về **thi hành**. Nhận ra một khoảng rỗng cần cả `PScan.Empty`
**và** `exec.emptyOp`, không chỉ `Est{Rows:0, Cost:0}`. Và lần thứ hai của cùng bài học là con bug
tìm ra ở **lượt 3**: nhánh `OpEq` của `extractSpans` viết thành phép **gán** trong khi ba nhánh
cạnh nó là phép **giao**, nên `x = 3 AND x = 9` cho khoảng `[9,9]` không rỗng và quét cả bảng cho
một câu vô nghiệm. Kết quả vẫn đúng. Chỉ một lời khẳng định về **số hàng đọc** bắt được nó.

Nên quy tắc: **một bài test cho optimizer phải khẳng định về CÔNG VIỆC ĐÃ LÀM, không chỉ về kết
quả.** Ba bài của phase này làm đúng thế: `TestPushdownKeepsResults`, `TestSelectivityNotPredicateCount`,
`TestContradictoryEqualitiesGiveEmptySpan`.

**(c) Một con số cũ trong nhật ký là một con số vô hình.** Diary phase 6 ghi `BenchmarkScan` =
688-732µs. Phase 7 viết lại `Scan` thành streaming (trả nợ P6-4) và **không đo lại**. Nếu tôi lấy
số ấy làm baseline thì kết luận "push → pull nhanh hơn 2x" — sai hoàn toàn về nhân quả. Phải
`git worktree add /tmp/p7 12ad70c` và đo tại đúng HEAD phase 7: **317-363µs**.

Đây là món nợ mà SKILL chưa có quy tắc cho, nên thêm vào đây: **một phase làm đổi một con số của
phase trước thì phải đo lại con số đó và ghi cả hai.**

## Nợ kỹ thuật / để dành cho sau

- [ ] **P8-1 · Không có `BEGIN`/`COMMIT` — mỗi câu là một transaction riêng (autocommit).** Đây là
      **món nợ lớn nhất của phase**, và nó đau vì phase 6 đã có 4 mức isolation, khoá khoảng, và
      phát hiện deadlock đầy đủ — nhưng **không viết được** một transaction nhiều câu **bằng SQL**.
      Cả `internal/txn` chỉ dùng được từ Go.
- [ ] **P8-2 · Không có `UPDATE`/`DELETE`.** `internal/txn` có đủ (`Put`/`Del` + chuỗi version), chỗ
      thiếu là parser và một toán tử ghi. Chưa làm vì `DELETE` còn phải cập nhật **mọi** index phụ,
      và bất biến hàng↔index của phase 7 phải được khẳng định lại ở đường đi mới.
- [ ] **P8-3 · Không có `NOT`, không có `IS NULL`.** `NOT` **đã là** một token trong
      `internal/sql/token.go` nhưng parser không có phép một toán tử. Chua ở chỗ: logic ba giá trị
      đã cài **đúng** ở `internal/plan/bind.go` và có bảng chân lý test, nhưng **không gõ ra được**
      từ SQL. `IS NULL` là cách duy nhất tìm hàng có NULL, và hiện chưa có cách nào.
- [ ] **P8-4 · Chỉ join **hai** bảng, và chỉ **inner** join.** `BindSelect` báo lỗi rõ ràng khi có
      bảng thứ ba. Ba bảng đòi **chọn thứ tự join** — tức lập trình động trên tập con, và đó là một
      phase riêng. Outer join đòi kiểm lại `propagate` (xem trên) và cả `push`.
- [ ] **P8-5 · `numParts` cố định 32, không chia phần đệ quy.** Grace hash join thật chia lại phần
      nào **vẫn** không vừa RAM. Nên mọi kết luận của bảng số 2 là **có điều kiện** trên chỗ này.
      Ghi rõ tại `internal/exec/join.go`.
- [ ] **P8-6 · Không có plan cache, dù đã tự đo được lý do phải có.** Front-end là **2.9x** phần
      thi hành cho một truy vấn điểm, và nó lặp **y nguyên** mỗi lần. `prepare` đã tách riêng
      (bind + optimize + plan, không chạm dữ liệu) nên chỗ để cắm cache **đã sẵn**; thiếu là khoá
      cache (câu đã chuẩn hoá + phiên bản catalog) và phép **vô hiệu hoá** khi `CREATE INDEX` /
      `ANALYZE` chạy.
- [ ] **P8-7 · Không có tham số truy vấn (`?` / `$1`).** Đi liền P8-6: plan cache không có tham số
      thì chỉ ăn được với câu **giống nhau từng byte**, tức gần như vô dụng ngoài benchmark.
- [ ] **P8-8 · `CREATE INDEX` không nhận `DESC`.** `internal/keys` đã hỗ trợ chiều sắp **bằng phép
      bù byte** từ phase 7, và `exec.sort` dùng nó; chỗ thiếu chỉ là cú pháp và một cột trong
      catalog. Nên `ORDER BY x DESC` hiện luôn phải **sắp**, dù cây đã có sẵn thứ tự ngược.
- [ ] **P8-9 · `os.Remove` ngay sau `os.CreateTemp` không chạy trên Windows.** Cố ý: mẹo này là
      cách rẻ nhất để file tạm không sống sót một lần crash, và cả repo đã `pread`/`pwrite`/
      `posix_fadvise` — tức đã chỉ chạy Linux từ phase 0.
- [ ] **P8-10 · `LIMIT` chưa đẩy được xuống dưới `Join`.** `LIMIT` trên một `Scan` thì dừng đúng
      lúc (đó là chỗ ăn 3300x), nhưng qua một `Join` thì nó chỉ cắt ở **trên**. Với nested loop
      thì đẩy xuống được; với hash join thì phải rút cạn vế build trước, nên không.
- [ ] **P8-11 · Chưa có `GROUP BY`, `HAVING`, hàm tổng hợp, `DISTINCT`.** Ngoài phạm vi roadmap
      phase 8. Ghi lại vì hash aggregate dùng **đúng** cơ chế tràn đĩa của hash join, nên nó là
      món rẻ nhất còn lại trong danh sách này.
- [ ] **P8-12 · `bench-txn` gộp `Get|Scan` dưới một `-benchtime`.** Lỗi của **bộ đo**, phát hiện ở
      phase này: `-benchtime` tính **theo phép toán**, và `BenchmarkScan` quét **2000 khoá mỗi
      phép** — nên `-benchtime=200000x` là **1.2 tỉ** bước khoá. Đúng cho `BenchmarkGet`, vô lý cho
      `BenchmarkScan`. Chưa sửa vì sửa là chạm vào deliverable của phase 6.

Nợ **P7-9 đã trả đầy đủ** ở phase này (`ORDER BY` dùng thứ tự index, và `Filter` đẩy xuống — cộng
`plan.propagate`, luật **không có trong kế hoạch**). Bằng chứng: bảng số 4 và 5. Xem
[`docs/debts.md`](../docs/debts.md).
