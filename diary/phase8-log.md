# Phase 8 — nhật ký lệnh đầy đủ, thất bại và cải tiến

Bổ sung cho [`phase8.md`](./phase8.md). File kia là **kết quả** đã biên tập; file này là **toàn bộ
đường đi**, kể cả các ngõ cụt.

Hình dạng của phase này, so với bảy phase trước:

- Phase 3 bị chặn ở năm chỗ, phần lớn bởi thứ tôi tự cài từ phase trước.
- Phase 4 có **bốn trong bảy giả thuyết bị bác**, một cái sai ở tầng khái niệm.
- Phase 5: **bốn bug khác nhau hoá ra một nguyên nhân gốc**, và **hai lỗi của bộ đo trên một lỗi
  của code**.
- Phase 6: **một chuỗi ba giả thuyết** cho **một** hiện tượng, và fuzzer tìm ra trong 3 giây thứ
  mà ba lần đọc code không thấy.
- Phase 7: cả **bốn** con bug nằm trong **bộ đo**, không con nào nằm trong database.
- Phase 8: hai hình dạng mới, và cả hai đều đáng lo hơn phase 7.
  1. **Con bug tệ nhất là con bug tôi tự tạo ra bằng một trực giác mượn từ một database khác** —
     và nó không phải một dòng code sai, nó là một **câu đúng về Postgres** đặt vào một engine
     không phải Postgres.
  2. **Bảng số chỉ ra một luật optimizer còn THIẾU.** Bảy phase trước, số đo tìm ra thứ *sai*.
     Đây là lần đầu nó tìm ra thứ *chưa có*.

Và một hình dạng thứ ba, chỉ hiện ra ở lượt cuối: **con bug thứ năm được tìm ra trong lúc viết
nhật ký**, vì tôi cần một ví dụ EXPLAIN ngắn và gõ thử một câu vô nghiệm.

Cấu trúc lượt: 1 lượt viết code (không chạy gì) → 1 lượt chạy + sửa bug → 1 lượt viết nhật ký.

---

## Lượt 1 — viết code, không chạy một lệnh nào

### Việc đầu tiên không phải viết parser, mà là phát hiện phase 8 không xây được trên phase 7

Ngồi vẽ hình của nested loop join: *"với mỗi hàng vế ngoài, quét lại cả vế trong"*. Rồi mở
`internal/txn/txn.go` và đọc chữ ký của `Scan`:

```go
func (t *Txn) Scan(lo, hi []byte, fn func(key, val []byte) bool) error
```

Vòng lặp thuộc về `Scan`. Người gọi chỉ đưa vào một callback. Đây là **push**, và phase 6 với
phase 7 xây **mọi** đường đọc theo hình đó — `db.Iter` có tồn tại, nhưng tầng trên nó (`txn`,
`table`, `query`) đều push.

Nested loop cần **khởi động lại** vế trong. Hash join cần **rút cạn** vế build rồi mới chạy vế
probe. Cả hai đều là "tôi giữ vòng lặp" — tức **pull**.

Ngồi liệt kê hết cách làm join **bằng API push**, và đây là chỗ nửa giờ đầu của phase đi vào:

| Cách | Chết ở đâu |
|---|---|
| Đệm cả một vế vào RAM rồi join | Đúng cái mà phase 7 vừa **trả nợ P6-4 để bỏ**. Và nếu vế build đã ở trong RAM cả thì hash join spill **không còn nghĩa gì** — cả câu hỏi số 3 của roadmap bốc hơi |
| Mỗi vế một goroutine, nối bằng channel | `txn.Txn` **không** an toàn cho nhiều goroutine. Thứ tự latch `dbMu → mu` (phase 6) chỉ đúng khi một goroutine giữ một `Txn`. Sửa được, nhưng sửa nó là **làm lại phase 6** |
| Đảo ngược theo kiểu continuation | Join hai vế thành hai callback lồng nhau — viết được. Ba vế thì không ai đọc nổi, và `Sort` (toán tử **chặn**) thì không diễn tả được chút nào |

Nên kết luận của nửa giờ đầu: **phase 8 mở màn bằng một ca mổ ở tầng phase 6.** Không phải viết
lexer.

**Ghi lại vì đây là lần thứ hai một phase bị chặn bởi phase trước, và lần này khác lần trước.**
Phase 7 bị chặn bởi một món nợ **đã ghi trong sổ** (P6-4). Phase 8 bị chặn bởi một thứ **không ai
ghi**, vì nó không phải "thiếu" — `Scan(lo, hi, fn)` là một API hoàn toàn tốt cho mọi việc của
phase 6 và 7. Nó chỉ **không đủ** cho join. Nên bài học về cách ghi nợ:

> Không phải mọi món nợ đều là "cái tôi biết còn thiếu". Có món là **một quyết định đúng lúc đó,
> sẽ sai khi có thêm một yêu cầu**. Loại ấy không ghi được vào sổ nợ, vì lúc ghi thì nó chưa sai.

### SQLite giải cùng vấn đề bằng một đường tôi không đi

Đọc lại kiến trúc SQLite ở chỗ này thì thấy nó **không** có câu hỏi push-hay-pull: nó **biên dịch
câu SQL ra bytecode**, và vòng lặp nằm trong VM. `OP_Rewind`/`OP_Next` là lệnh máy, không phải
callback hay iterator. Postgres thì là Volcano thuần (`ExecProcNode`, pull).

Hai lời giải cho **cùng một** vấn đề. Tôi chọn Volcano vì nó cho phép `EXPLAIN` in ra một **cây**,
và cây là thứ đọc được bằng mắt — còn bytecode thì phải học đọc.

### Quyết định khó nhất của lượt 1: một bản merge, không phải hai

Sau khi có `Iter`, có hai đường:

- **(a)** giữ `Scan` cũ nguyên vẹn (nó đang chạy tốt, và đang được cả `internal/table`,
  `internal/query`, `cmd/idxlab` dùng), thêm `Iter` bên cạnh.
- **(b)** cài phép merge **một lần** trong `Iter`, rồi viết lại `Scan` thành lớp bọc.

Chọn (b), và chọn vì một lý do có tên: `internal/query/workload.go` của phase 7. Ở đó tôi từng
giữ **hai** bản của cùng một phép duyệt, và chúng **lệch nhau** — bảng deliverable của phase 7 có
một dòng phản bác đúng dòng kết luận của chính nó vì lý do đó.

> **Hai bản của cùng một phép merge là hai chỗ để lệch nhau.**

Cái giá của (b) là **rủi ro hồi quy trên cả phase 6 và 7**, và nó có thật — nên nghi vấn hàng đầu
mang sang lượt 2 không phải "SQL có chạy không" mà **"lưới anomaly còn đúng từng ô không, crashlab
còn 20/20 không"**.

### Ngõ cụt nhỏ: tôi định chép hàng ra buffer riêng

Bản `Iter` đầu tiên có hai `[]byte` buffer và một `copy()` mỗi lần `Next()`. Lý do đúng: người gọi
có quyền giữ `Key()`/`Value()` suốt thời gian giữa hai lần `Next()`, còn con trỏ cây thì đã đi
tiếp. Chép 20000 lần cho một lần quét.

Cái mở được nút không phải một cơ chế mà là một **câu hỏi lại**: *vì sao* con trỏ cây đã đi tiếp?
Vì `Next()` gọi `advance()` ở cuối. Nếu **hoãn** `advance()` sang **đầu** lần `Next()` kế tiếp thì
`tk`/`tv` còn nguyên suốt thời gian người gọi cầm chúng — và **không cần chép gì cả**.

```go
// pend: lần Next tới phải đẩy con trỏ cây lên một bước trước khi quyết
// định. Có cờ này để KHÔNG phải chép tk/tv ra buffer riêng: chừng nào
// chưa advance thì tk/tv còn nguyên, nên người gọi cầm được Key()/Value()
// suốt thời gian giữa hai lần Next. Bản push cũ giải cùng vấn đề bằng
// cách gọi fn TRƯỚC advance — cùng một bất biến, phát biểu ngược lại.
pend bool
```

Câu cuối của chú thích là điều đáng nhớ: bản push đã giải **đúng** vấn đề này rồi, chỉ là phát
biểu theo chiều khác. Chuyển push→pull không tạo ra vấn đề mới; nó **đổi chiều phát biểu** của
cùng một bất biến.

### `IterRowsKeys` — một API tồn tại vì API kia không diễn tả được phép BẰNG

Viết `table.RowIter`/`IndexIter` ở mức **giá trị** (`keys.Value`) thì gọn, nhưng phase 8 cần một
khoảng mà mức giá trị **không diễn tả được**: khoảng của một phép **bằng** trên khoá nhiều cột.

"Mọi hàng có cột đầu = v" là

```
[ Encode(v), PrefixEnd(Encode(v)) )
```

và `PrefixEnd` **không phải một giá trị** — nó là một phép toán **trên byte** (tăng byte cuối
không phải 0xff, cắt đuôi 0xff). Không có `keys.Value` nào bằng nó.

Nên có thêm `IterRowsKeys`/`IterIndexKeys` nhận thẳng `[]byte`. Ghi lại vì nó là một ví dụ sạch
của **chỗ một tầng trừu tượng phải hở ra một lỗ**, và lỗ ấy có lý do phát biểu được bằng một dòng.

### Ba package, và lằn ranh là câu trả lời cho câu hỏi số 1 của roadmap

```
internal/sql/     cú pháp. KHÔNG biết catalog
internal/plan/    ngữ nghĩa + tối ưu. Biết catalog, kiểu, thống kê, chi phí
internal/exec/    thi hành. KHÔNG biết SQL
```

Và bên trong `plan`, lằn ranh thứ hai — `opt.go` (logical→logical) khác `planner.go` (physical) —
là chỗ tôi đã suy nghĩ lâu nhất của lượt 1. Phát biểu chốt lại được thành một câu:

> **Một phép biến đổi logical là một ĐỊNH LÝ. Một phép biến đổi physical là một TÌNH HUỐNG.**

σ_p(L ⋈ R) = L ⋈ σ_p(R) là đẳng thức trên tập hợp: đúng bất kể chạy bằng gì. "Bỏ ORDER BY" chỉ
đúng nếu đường đi **đã chọn** tình cờ cho thứ tự ấy.

### Một chỗ tôi cố ý làm khác Volcano

`nestLoopOp` **dựng lại cả cây con** vế trong cho mỗi hàng vế ngoài, thay vì `rewind` nó. Volcano
thật có `rewind`. Tôi không, vì một cây con có thể chứa `Sort` hoặc một hash join khác — và cả hai
đều **không rewind được** (một cái đã ghi run ra đĩa, một cái đã băm xong vế build). Dựng lại thì
luôn đúng, chỉ đắt.

Và vì nó đắt, nó **phải đếm được**: `Stat.InnerScans`. Toàn bộ điểm yếu của thuật toán hiện ra
trong **thống kê** thay vì ẩn trong thời gian — và đó là điều làm bảng số 1 của `sqllab` đọc được.

---

## Lượt 2 — chạy, và năm con bug

### Việc đầu tiên không phải `go test` mà là đo cái giá của ca mổ

Và ngay ở đây có một ngõ cụt **được tránh**. Tôi đã định lấy baseline từ diary phase 6:

```
BenchmarkScan   688-732 µs
```

Kịp dừng vì một câu hỏi: *phase 7 có chạm vào `Scan` không?* Có — nó **viết lại thành streaming**
để trả nợ P6-4, và **không đo lại `BenchmarkScan`**. Nên con số 688-732µs là số của **một hàm
không còn tồn tại**.

```console
$ git worktree add /tmp/p7 12ad70c
$ cd /tmp/p7 && go test ./internal/txn/ -run '^$' -bench BenchmarkScan -benchtime=300x -count=3
BenchmarkScan-6   	     300	    333353 ns/op
BenchmarkScan-6   	     300	    363481 ns/op
BenchmarkScan-6   	     300	    317915 ns/op
```

Nếu tôi dùng số cũ thì kết luận sẽ là **"push→pull nhanh hơn 2x"** — sai hoàn toàn về nhân quả:
2x ấy là của **phase 7**, không phải của phase 8.

> **Một con số cũ trong nhật ký là một con số VÔ HÌNH.** Nó không sai lộ ra; nó im lặng và đúng
> về một quá khứ đã bị thay.

Đây là món SKILL chưa có quy tắc, nên phase 8 đề nghị thêm: **một phase làm đổi một con số của
phase trước thì phải đo lại con số đó và ghi cả hai.**

### Ngõ cụt: `-benchtime=200000x` cho một benchmark quét

Lần đầu tôi chạy đúng lệnh của `make bench-txn`:

```
go test ./internal/txn/ -bench 'BenchmarkGet|BenchmarkScan' -benchtime=200000x
```

Nó chạy mãi. Lý do: `-benchtime` tính **theo phép toán**, và `BenchmarkScan` quét **2000 khoá mỗi
phép** — tức **1.2 tỉ** bước khoá. `BenchmarkGet` thì 200000 phép là hợp lý.

Phải hủy. Và hủy cũng có một cái bẫy:

```
$ pkill -f 'go test'
```

Lệnh này **tự giết cái shell của chính nó**, vì dòng lệnh của shell có chứa chuỗi `go test`. Mất
một vòng, và mất luôn một lần vá file mà tôi tưởng đã áp (xem dưới).

**Kết luận về bộ đo:** `bench-txn` gộp `Get|Scan` dưới **một** `-benchtime` là một **bug của
Makefile** — đúng cho bench theo khoá, vô lý cho bench theo lần quét. Ghi vào sổ, chưa sửa.

### Bug 1 — và nó là con bug tệ nhất của cả phase, vì tôi tự tạo ra nó

```console
$ go run ./cmd/minidb -db data/sql/t.db -e "EXPLAIN SELECT city FROM ev WHERE id < 5;"
    -> SeqScan ev filter=(ev.id < 5)
$ ... -e "SELECT city FROM ev WHERE id < 5;"
(5 hàng, 4.062ms · đọc 5100 hàng bảng, ...)
```

**5100 hàng đọc để trả 5 hàng.** Và không phải vì tôi quên viết gì — vì tôi **cố ý viết**: trong
`scan()` bản đầu, mỗi khi đường đi là SeqScan thì tôi **đẩy khoảng trở lại thành residual**.

Lý do tôi viết thế, viết ra thành lời thì thấy ngay nó sai ở đâu:

> "Seq scan là quét cả bảng. Bảng không có thứ tự nào. Nên không có khoảng nào để dùng."

Câu đó **đúng với Postgres**, nơi bảng là một **heap** và index là một cấu trúc **rời**. Engine này
là **clustered index** — từ phase 7, hàng nằm **bên trong** cây pk. Nên "seq scan" ở đây thực chất
là **quét cây pk theo thứ tự khoá chính**, và `WHERE pk < v` **là** một phép quét khoảng thật, với
điểm bắt đầu và điểm dừng.

> **Loại bug đắt nhất không phải một dòng code sai. Nó là một câu ĐÚNG về một database KHÁC.**

Và nó đắt gấp đôi vì **không test nào bắt được**: kết quả đúng, chỉ chậm. Nó lộ ra vì tôi đọc
`EXPLAIN` của chính mình và thấy chữ `filter=` ở chỗ đáng lẽ là `[id < 5]`.

Sửa xong:

```console
$ ... -e "SELECT city FROM ev WHERE id < 5;"
(5 hàng, 88µs · đọc 105 hàng bảng, ...)
```

**5100 → 105 hàng, 4.062ms → 88µs = 46x.**

### Bug 1 kéo theo một cuộc mổ lớn hơn: `query.Choose` phải chết

Sửa bug 1 tử tế thì phát hiện `query.Choose` của phase 7 **không diễn tả nổi** thứ cần diễn tả.
Chữ ký của nó trả về **một** kế hoạch. Nhưng phase 8 cần hai thứ mà một kế hoạch không nói được:

1. Với engine clustered index, **seq scan cũng là một access path CÓ khoảng**. `Choose` chỉ liệt kê
   `c.Indexes(oid)` — tức chỉ index **phụ** — nên nó thừa hưởng đúng cái điểm mù đã gây ra bug 1.
2. Một đường đi còn trả về một **THỨ TỰ**, và thứ tự ấy có thể làm cho `Sort` biến mất. Một hàm
   trả về một kế hoạch không có chỗ nào để nói *"đường này còn tặng bạn thứ tự theo cột kind"*.

Nên `Choose` bị thay bằng `type path` + một hàm **liệt kê**. Và điều đáng ghi:

> **Cái sống sót từ phase 7 là MÔ HÌNH (`CostModel` + `Selectivity`), không phải hàm chọn. Và cái
> giết `Choose` không phải join — mà là ORDER BY.**

Lúc lên kế hoạch phase 8 tôi đã đoán rằng join sẽ là thứ làm vỡ planner cũ. Sai. Join chỉ **thêm**
một loại node. ORDER BY thì **đổi chữ ký**: nó buộc "thuộc tính vật lý bắt buộc" phải đi **xuống**
qua `build`, tức đổi cách cả cây được dựng.

Chi tiết dễ sai nhất ở chỗ này, và tôi đã sai một lần rồi sửa: `Join` phải truyền **nil** xuống,
không truyền `req`. Nested loop **chỉ** bảo toàn thứ tự vế ngoài nếu mỗi hàng vế ngoài khớp **≤1**
hàng vế trong — không chứng minh được ở đây; hash join thì không hứa gì. Truyền `req` xuống là hứa
một thứ mình không giữ được.

Và một chi tiết thứ hai: `sortCost` phải dùng **đúng công thức mà node `PSort` dùng**, không phải
một công thức gần đúng khác. Nếu hai chỗ khác nhau thì phép so sánh "đường A + sort" với "đường B"
đang so **hai đơn vị khác nhau**, và kết quả vô nghĩa.

### Bug 2 — ba bài test của `internal/engine` đỏ cùng lúc

```
--- FAIL: TestOrderByUsesIndex
--- FAIL: TestIndexScanIsChosenWhenSelective
--- FAIL: TestIndexOnlyScanIsChosen
```

Planner **luôn** chọn seq. Truy ra `query.DefaultCost`: `CFetch = 20`. Và đó là con số phase 7
**cố tình giữ sai** — `cmd/idxlab` có một cột tên là "planner đoán sai", và cột ấy tồn tại được
nhờ `DefaultCost` sai.

Nên không được sửa `DefaultCost`. Thêm bản mới, hiệu chỉnh từ số đo phase 7:

```go
// MeasuredCost: ba hằng số lấy từ SỐ ĐO của phase 7, không từ sách.
// Điểm hoà vốn selectivity mà bộ hằng này cho ra: 33.3% (mô hình)
// so với 36.8% (đo được ở cmd/idxlab). Lệch 10% tương đối — đủ tốt.
var MeasuredCost = CostModel{CSeq: 1, CIndex: 0.6, CFetch: 2.4}
```

`NewPlanner` dùng `MeasuredCost`; `DefaultCost` **vẫn sai có chủ ý**, và giờ có một chú thích nói
rõ nó sai để làm gì. Hai bộ hằng, hai mục đích — chứ không phải một bộ hằng "đúng hơn".

### Bug 3 — con bug tệ nhất về mặt **triệu chứng**: một cái treo, không phải một cái đỏ

```console
$ go test ./internal/sql/ -count=1
panic: test timed out after 120s
```

`WHERE x = = 1`. Truy: `advance()` là **no-op** khi `p.err` đã được set (để không chồng lỗi), còn
vòng lặp phân tích biểu thức thì chờ token đổi. Không đổi ⇒ quay mãi.

> **Một đường lỗi không tiến được thì không phải một lỗi. Nó là một cái TREO.**

Và cùng hình dạng với bug của phase 7 (phép quét không kết thúc vì writer chèn vào phía trước
cursor): cả hai lần, thứ còn thiếu là **bảo đảm rằng MỌI bước đều tiến**.

Sửa:

```go
// errf ghi lỗi ĐẦU TIÊN rồi ĐẶT TOKEN VỀ EOF.
//
// Việc đặt về EOF không phải dọn dẹp cho gọn — nó là điều kiện để parser DỪNG.
func (p *Parser) errf(f string, a ...any) error {
	if p.err == nil {
		p.err = p.lx.errf(p.tok.Pos, f, a...) // lỗi ĐẦU TIÊN mới đáng
	}
	p.tok = Token{Kind: EOF, Pos: p.tok.Pos}
	return p.err
}
```

**Hai lỗi cộng dồn của riêng tôi trên đường sửa con này**, ghi lại vì cả hai đều là lỗi về **quy
trình**, không về code:

1. Lần vá đầu **không bao giờ áp vào file**, vì `pkill -f 'go test'` đã giết cái shell đang chạy nó.
2. Và tôi **tin rằng nó đã áp**, nên chạy lại test và bối rối một vòng.

Bài học: sau mỗi lần vá, **đọc lại file** hoặc `grep` cái tên vừa thêm. `go build` xanh **không**
chứng minh bản vá đã vào — nó chứng minh file hiện tại biên dịch được.

Và bất biến ấy được nâng lên thành bất biến của fuzz, chứ không chỉ một bài test:

```go
// FuzzParse: ba bất biến. Không panic. LUÔN KẾT THÚC (watchdog 2 giây mỗi đầu
// vào — đây là bất biến đắt nhất, vì một cái treo làm cả bộ test đứng thay vì
// đỏ). Và in lại rồi phân tích lại thì bền.
```

### Bug 4 — `extractSpan` chọn theo **số điều kiện** thay vì theo **selectivity**

```
SELECT city FROM ev WHERE kind = 3 AND id < 20000
```

Mỗi cột đúng **một** điều kiện. Bản đầu của tôi tie-break bằng "cột nào nhiều điều kiện hơn", nên
với thế hoà nó lấy cột đầu tiên — `id` — tức **cả bảng** (20000 hàng), thay vì `kind = 3` (0.5%).

Kết quả vẫn **đúng**. Chỉ chậm. Nên nó chỉ lộ ra khi có một lời khẳng định về **số hàng đọc**:

```go
// TestSelectivityNotPredicateCount: hai cột, mỗi cột MỘT điều kiện, nên phép
// đếm không phân biệt được. Chỉ thống kê phân biệt được.
```

Sửa bằng cách **dời quyết định**: `extractSpans` trả về **mọi** ứng viên (một cho mỗi cột), việc
chọn đi vào `scan()` — chỗ có `CostModel` và `Selectivity`.

> Một hàm không có thống kê thì **không được** quyết định chuyện cần thống kê. Nó phải **liệt kê**
> và giao lên trên.

### Bug 5 — "chi phí 0" không có nghĩa là "không đọc gì"

Thêm nhánh nhận ra khoảng rỗng (`x > 5 AND x < 3`), chạy, và:

```
(0 hàng, 4.1ms · đọc 20000 hàng bảng, ...)
```

Ước lượng `Est{Rows: 0, Cost: 0}` **đúng**. Toán tử vẫn quét và lọc cả bảng.

> **"Chi phí 0" là một phát biểu về ƯỚC LƯỢNG. "Không đọc gì" là một phát biểu về THI HÀNH. Chúng
> không tự đồng bộ với nhau.**

Cần **cả** `PScan.Empty` (để planner nói ra) **và** `exec.emptyOp` (để executor làm theo). Và
`EXPLAIN` in ra `NoScan` — vì nếu người đọc EXPLAIN thấy `SeqScan (rows≈0)` thì họ sẽ tin sai.

### Bốn con bug của **bộ đo** `sqllab` — đúng hình dạng của phase 7

Phase 7 dạy: nghi bộ đo trước. Nên khi bảng đầu tiên in ra, tôi đọc **bảng** trước khi đọc **kết
luận**. Bốn lỗi:

1. **Cột "vs tốt nhất" là một min chạy dần** ⇒ hàng đầu **luôn** 1.00x, dù nó tệ nhất. Sửa: đo hết
   rồi mới in.
2. **Danh sách hạn mức không sắp**, nên bảng đọc như thể thời gian nhảy loạn. Sửa: `uniqSorted`.
3. **`parse − lex` in ra số ÂM.** `Tokens` cắt sẵn cả câu vào một slice; `Parse` lex **theo yêu
   cầu**. Trừ hai đại lượng khác nhau. Sửa: đổi nhãn thành `parse*` và ghi rõ *"đã bao gồm lex"* —
   **không cột nào được trừ cho cột nào**.
4. **Tôi đọc `inner-scan` như thể nó là `W`.** Nó là "số lần vế trong bị chạy lại" = số hàng vế
   **ngoài**, và planner chọn vế **ít hàng** làm vòng ngoài. Nên ba dòng giữa bảng có nó **đứng
   yên ở 200**. Sửa: thêm cột **`đọc(nl)`** đo thật, để bảng **tự giải thích được**.

Lỗi thứ 4 đáng nói nhất, vì nó không phải lỗi code mà **lỗi đọc**, và cách sửa không phải "đọc kỹ
hơn" mà **thêm một cột số**. Một bảng cần đủ cột để một người đọc sai bị bảng **phản bác**.

### Hai con "bug" hoá ra là nhiễu — và lần chạy lại cho kết quả **tốt hơn** cái nhiễu đã che

`-repeat 1` rồi `-repeat 3`:

```
SELECT ev.city, dim.name FROM ev JOIN dim ...    11.3      15.7    0.72x
```

**Pushdown làm chậm đi?** Và bảng sort thì phi đơn điệu: hạn mức 625 → 2.50x nhưng 512 → 1.83x.

Cả hai tan ở `-repeat 7`. Nhưng chỗ đáng ghi là bảng sort: cái nhiễu đã **che một phát hiện**.

```
   20000    10000    46.404ms      1.46x        2 run
   20000      512    48.575ms      1.53x       40 run
   20000       64    77.199ms      2.42x      313 run
```

Giảm hạn mức **20 lần**, số run tăng **20 lần**, thời gian đi từ 1.46x lên **1.53x**. Một **bậc
thang**, không phải một đường dốc.

Và từ chỗ **phẳng** ấy suy ra được một điều tôi **không đo trực tiếp**: 2 run → 40 run là
log₂40/log₂2 = **5.3 lần** nhiều so sánh hơn trong bước trộn, mà thời gian không đổi ⇒ **so sánh
không phải chỗ tốn**; ghi và đọc đĩa mới là.

Và so sánh rẻ vì một quyết định của **phase 7**: khoá run ghi ra bằng `keys.Encode` — bộ mã hoá
**giữ thứ tự** — nên phép trộn là `bytes.Compare`, memcmp trên byte thô, đo riêng ở phase 7 là
**5.7x** rẻ hơn so theo kiểu (2.468 vs 14.08 ns). `DESC` là **phép bù byte**, nên khoá nhiều cột
ASC/DESC lẫn nhau vẫn so bằng **một** `bytes.Compare`.

Đó là **cổ tức thứ hai, không dự tính** của bộ mã hoá giữ thứ tự. Cổ tức thứ nhất là index scan
của phase 7.

> Một lần đo thiếu repeat không chỉ cho số sai. Nó có thể **che một kết luận đúng**.

### Con bug thứ sáu, và nó không phải bug — nó là một luật optimizer còn THIẾU

Bảng pushdown ở `-repeat 3`:

```
SELECT city FROM ev WHERE kind = 3 AND id < 20000              14.309      0.179   79.97x   20000    200
SELECT ev.city, dim.name FROM ev JOIN dim ON ev.kind=dim.…     16.235      0.271   59.94x   20200    400
SELECT ev.city, dim.name FROM ev JOIN dim ON ev.kind=dim.…     15.686     13.287    1.18x   20200  20005
```

Theo quy tắc số 5 của SKILL — *thấy 1.1x ở chỗ đáng lẽ 100x thì nghi bench sai* — tôi đi tìm lỗi
bộ đo. **Không có.** Và cột `hàng(bật)` nói thật: **20005**. Engine đọc gần cả bảng dù pushdown
đang bật.

Câu ấy là `... ON ev.kind = dim.kind AND dim.kind < 5`. Đẩy `dim.kind < 5` xuống thì nó thu vế
**build** — 200 hàng. Vế **probe** 20000 hàng **không ai chạm**.

Và đây là chỗ nhận ra: điều kiện đáng đẩy là `ev.kind < 5`, và nó **không có trong câu người ta
gõ**. Phải **suy ra** nó.

```go
// propagate SUY RA điều kiện qua phép bằng của join.
//	a.x = b.y  AND  b.y < 5   =>  thêm  a.x < 5
//
// Luật này được thêm ở LƯỢT CHẠY, không phải lượt code, và chính bảng số chỉ ra
// nó. Mục 4 của cmd/sqllab đo `ON ev.kind=dim.kind AND dim.kind < 5` và pushdown
// chỉ ăn 1.18x, trong khi hai câu còn lại ăn 60-80x.
```

Sau khi thêm: **1.18x → 18.53x**, hàng đọc **20005 → 1005**.

Hai chi tiết phải làm cho đúng:

- **Dedup theo `String()`**, để luật **idempotent**. Không có nó thì mỗi lần chạy `push` lại sinh
  thêm một bản của cùng điều kiện, và với một optimizer chạy tới điểm bất động thì đó là một
  vòng lặp không dừng.
- **Chỉ đúng với inner join.** Mọi hàng ra của inner join thoả `a.x = b.y`, nên hàng bị điều kiện
  suy ra loại bỏ là hàng **vốn đã không join được**. Với LEFT JOIN thì hàng bên trái **sống sót**
  kể cả khi không có bạn bên phải, nên loại nó là **đổi kết quả**. Một phép tối ưu đúng cho một
  phép join là **bug** cho một phép join khác.

> **Bảy phase trước, số đo tìm ra thứ SAI. Đây là lần đầu số đo tìm ra thứ CHƯA CÓ.**
>
> Và bài học rộng hơn: **một bảng số tốt không chỉ trả lời câu hỏi bạn đặt ra — nó chỉ cho bạn
> thấy chỗ bạn chưa đặt câu hỏi nào.**

### Hai lần kỳ vọng test của tôi sai, code đúng

**(a) `SELECT a FORM t`.** Tôi mong lỗi trỏ vào `FORM`. Nó trỏ vào `t`:

```
cú pháp: cần "FROM", gặp "t"
   SELECT a FORM t
                 ^
```

Và code **đúng**: trong SQL, alias **không cần** `AS`, nên `a FORM` phân tích được thành *"cột `a`,
alias `FORM`"*. Từ khoá viết sai bị **nuốt thành alias**, và lỗi chỉ lộ ra ở token **sau**.
Postgres báo y hệt kiểu ấy. Ghi lại thành một bài test có tên nói đúng hiện tượng:
`TestMisspelledKeywordPointsElsewhere`.

**(b) Điều kiện OR sau `WHERE`.** Tôi mong nó thành một `Filter` bên trên `Join`. Nó **nằm lại
trong `Join.On`**. Và với inner join thì hai hình ấy **tương đương** — `Join.On` và một `Filter`
ngay trên nó cho cùng một tập kết quả.

Nên kỳ vọng của tôi quá chặt. Bất biến **thật** là: *không phần nào của biểu thức OR chạm tới
`Scan`* — vì đẩy một nửa của OR xuống một bảng là **sai**. Viết lại bài test theo đúng câu ấy,
rồi thêm `TestOrOnOneRelationIsPushed` cho **chiều ngược**: nếu cả hai nhánh OR chỉ nói về **một**
bảng thì nó **được** đẩy.

Hai lần này đều là ví dụ của cùng một thứ: **một bài test đỏ có hai cách sửa, và chọn sai cách là
làm code tệ đi trong khi vẫn có bộ test xanh.**

### Fuzz — 13 giây

```console
$ make fuzz-sql
fuzz: minimizing 65-byte failing input file
--- FAIL: FuzzParse (13.01s)
        fuzz_test.go:84: in lại ra câu KHÔNG phân tích được:
              gốc:    "SELECT*FROM A WHERE''''"
              in lại: "SELECT * FROM A WHERE '''"
              lỗi: cú pháp: chuỗi chưa đóng nháy (cột 23)
    Failing input written to testdata/fuzz/FuzzParse/41ccdf9bacf73daf
```

`''''` là cách SQL viết một hằng chuỗi **chứa một dấu nháy**. Lexer **giải** phép thoát đúng
(`''` → `'`). Hàm in làm ngược lại mà **không thoát**:

```go
return "'" + string(e.V.B) + "'"   // sai
```

> **Ai viết bộ mã hoá phải viết phép thoát, và nửa dễ quên luôn là nửa GHI RA** — vì nửa đọc vào
> sai một cái là lỗi ngay, còn nửa ghi ra thì sai **lặng lẽ** cho tới khi có ai đọc lại.

Cùng hình dạng chính xác với `0x00 → 0x00 0xff` của `internal/keys` ở phase 7.

**Ngõ cụt trên đường sửa:** bản vá bằng heredoc Python **chết** với
`SyntaxError: invalid character '—' (U+2014)` — một dấu gạch dài trong chính đoạn chú thích tiếng
Việt mà tôi đang nhúng vào mã Python. Lần thứ hai công cụ vá bằng heredoc gây rắc rối trong phase
này (lần đầu: `pkill` tự sát). Chuyển sang sửa bằng công cụ `Edit` là xong.

Sau khi sửa:

```console
$ make fuzz-sql
fuzz: elapsed: 2m0s, execs: 3582747 (36346/sec), new interesting: 67 (total: 379)
PASS
ok  	minidb/internal/sql	120.203s
```

### Hồi quy — câu hỏi lớn nhất của lượt 1 được trả lời

```console
$ go run ./cmd/crashlab -n 20
20/20 vòng đúng. 709 transaction đã commit được kiểm, 8.161s.

$ make test-txn && make test-index && make fuzz-keys && make fuzz-table
(exit 0 cả bốn)
```

Lưới anomaly × mức isolation **giống từng ô** so với phase 6 — gồm cả hai ô **VỠ (lệch +7)** ở
read-uncommitted và read-committed. Hai mức ấy **được phép** sai; một bài test chỉ chứng minh được
điều gì khi nó **biết đỏ**.

Và bài phản chứng, chạy sau cùng vì nó là thứ tôi lo nhất khi mổ vào đường đọc:

```console
$ go run ./cmd/crashlab -n 10 -nowrite
vòng   sống(ms)      txn      khóa  kết quả
1           101        4         0  SAI: ... thiếu 19, thừa 0, có 0 khóa (mong 19)
2           515       36         0  SAI: panic khi kiểm tra: btree: cell 0: page: page hỏng: slot 0 trỏ ra ngoài (off=7 len=0)
...
0/10 vòng đúng, 10 sai. 210 transaction đã commit được kiểm, 5.363s.
exit status 1
```

**Hai** kiểu chết, và cả hai đều đáng đọc: `thiếu 430, thừa 0` là **mất transaction đã commit**
(vỡ D), còn `slot 0 trỏ ra ngoài` là một **page hỏng nửa vời** — đúng thứ WAL tồn tại để ngăn. Sau
tám phase, bộ đo crash vẫn báo được mất dữ liệu thật.

**Một lần công cụ chặn tôi**, ghi lại cho đủ: lần chạy `crashlab -nowrite` đầu tiên bị từ chối với
*"claude-sonnet-5[1m] is temporarily unavailable (timed out), so auto mode cannot determine the
safety of Bash right now"*. Chạy lại ở lượt sau thì được.

---

## Lượt 3 — viết nhật ký, và tìm ra con bug thứ năm

Đang viết mục "Rút ra" thì cần một ví dụ `EXPLAIN` ngắn cho câu hỏi số 1. Dựng một database 5 hàng
rồi gõ thử vài câu, trong đó có hai câu **vô nghiệm**:

```console
$ go run ./cmd/minidb -db data/sql/diary.db -q -e "
EXPLAIN SELECT city FROM ev WHERE id < 3 AND id > 9;
EXPLAIN SELECT city FROM ev WHERE kind = 3 AND kind = 9;"
    -> NoScan ev  (rows≈0 cost≈0) — khoảng rỗng theo điều kiện — không đọc gì
    -> SeqScan ev filter=(ev.kind = 3) AND (ev.kind = 9)  (rows≈1 cost≈1)
```

**Hai câu cùng vô nghiệm, hai kế hoạch khác nhau.** Thu hẹp:

```console
$ ... "EXPLAIN SELECT city FROM ev WHERE id = 3 AND id > 9;"
    -> NoScan ev  — khoảng rỗng theo điều kiện — không đọc gì
$ ... "EXPLAIN SELECT city FROM ev WHERE id = 3 AND id = 9;"
    -> SeqScan ev filter=(ev.id = 3) AND (ev.id = 9)
```

Nên chỉ đúng hình **bằng ∧ bằng** thoát được — không liên quan gì tới cột nào hay index nào. Mở
`extractSpans`, và bốn nhánh nằm cạnh nhau tự tố cáo:

```go
case sql.OpEq:
	a.lo, a.hi, a.hiIncl, a.eq = lit.V, lit.V, true, true          // GÁN
case sql.OpGt:
	if a.lo.IsNull() || keys.Compare(nv, a.lo) > 0 { a.lo = nv }   // GIAO
case sql.OpLt:
	if a.hi.IsNull() || keys.Compare(lit.V, a.hi) < 0 { ... }      // GIAO
```

`>` và `<` **giao** vào bộ tích luỹ. `=` **ghi đè** nó. Nên `kind = 3` bị `kind = 9` **xoá**,
khoảng ra `[9,9]` — không rỗng — và `kind = 3` tụt xuống làm residual.

**Vì sao viết sai:** phép bằng **trông như** một phép gán. `x = 3` đọc lên là "x bằng 3", và tay
gõ ra `a.lo, a.hi = v, v`. Ba nhánh cạnh nó đều là phép giao, và tôi không thấy sự bất đối xứng ấy
suốt cả lượt 1 lẫn lượt 2.

**Vì sao cả bộ test xanh:** vì mọi bài test khẳng định về **kết quả**, và kết quả **đúng** — 0
hàng. Engine chỉ đọc cả bảng để ra 0 hàng ấy.

Sửa, và chỗ dễ sai khi sửa: giao chặn trên phải giữ **tính đóng/mở đang có** khi giá trị bằng
nhau, không được nới nó ra. Nhờ vậy `x < 9 AND x = 9` ra `[9,9)` và `Span.Empty` nhận ra là rỗng.
Bản vá đầu của tôi có thêm `|| !a.hiIncl` và nó **nới** chặn trên — sai, bắt được ngay khi viết bài
test cho đúng hình đó.

```console
$ go test ./internal/plan/ -run 'TestContradictory|TestSingleEquality' -v -count=1
=== RUN   TestContradictoryEqualitiesGiveEmptySpan/=_và_=
=== RUN   TestContradictoryEqualitiesGiveEmptySpan/<_rồi_=_ở_đúng_chặn_trên
=== RUN   TestContradictoryEqualitiesGiveEmptySpan/=_rồi_>_cao_hơn
--- PASS: TestContradictoryEqualitiesGiveEmptySpan (0.00s)
--- PASS: TestSingleEqualityStillGivesPointSpan (0.00s)
```

Bài thứ hai có mặt vì phép giao mới **không được làm hỏng ca thường**: một phép bằng vẫn phải ra
khoảng **điểm** `[v,v]`.

> **Việc con bug này xuất hiện ở LƯỢT VIẾT NHẬT KÝ là lập luận mạnh nhất tôi có cho quy tắc "phải
> dựng được ví dụ chạy thật cho mọi điều mình định viết ra".** Nếu tôi viết mục "Rút ra" bằng cách
> nhớ lại thay vì bằng cách gõ lệnh, nó vẫn ở trong code.

### Và một phát hiện thứ hai của lượt 3: `NOT` không gõ ra được

Định viết một ví dụ về logic ba giá trị dùng `NOT (kind < 5)`:

```console
cú pháp: cần một giá trị hoặc tên cột, gặp "NOT" (cột 143)
```

`NOT` **đã là** một token trong `internal/sql/token.go` từ lượt 1, nhưng parser không có phép một
toán tử. Nên logic ba giá trị đã cài **đúng** ở `internal/plan/bind.go`, có bảng chân lý test đầy
đủ — mà **không gõ ra được từ SQL**. Cùng cảnh với `IS NULL`: đó là cách **duy nhất** tìm hàng có
NULL, và hiện chưa có cách nào.

Ghi thành nợ **P8-3**. Chua ở chỗ: nợ này không phải "chưa làm", mà **"đã làm phần khó, thiếu phần
dễ"** — và không ai phát hiện ra vì phần khó có test.

Viết lại ví dụ bằng thứ gõ được, và nó vẫn nói đúng điều cần nói:

```console
$ go run ./cmd/minidb -db data/sql/diary.db -e "
INSERT INTO ev VALUES (6, NULL, 'null-kind');
SELECT id, kind FROM ev WHERE kind = NULL;
SELECT id, kind FROM ev WHERE kind < 5;
SELECT id, kind FROM ev WHERE kind >= 5;"
(0 hàng, 22µs · đọc 6 hàng bảng, ...)
(3 hàng, 7µs · ...)
(2 hàng, 8µs · ...)
```

Bảng có **6** hàng; 3 + 2 = **5**. Hàng `kind IS NULL` **không thuộc bên nào** — một điều kiện và
phủ định của nó **cùng** loại nó ra, nên hợp của hai tập **không** phải cả bảng. Và
`WHERE kind = NULL` cho **0 hàng**.

Chỗ này là bẫy thật, vì `internal/keys` của phase 7 định nghĩa `Compare(Null, Null) == 0` — và
**đúng**, vì nó là một phép **THỨ TỰ**: B+Tree phải sắp NULL vào một chỗ cố định. Còn `NULL = NULL`
trong SQL là NULL — cũng **đúng**, vì nó là một phép **SO SÁNH**. Gộp hai phép ấy thành một thì
`WHERE x = NULL` trả về hàng, và đó là lỗi **ngữ nghĩa**, không phải lỗi cài đặt.

---

## Bảng tổng kết: mỗi con bug và thứ đã bắt được nó

| # | Bug | Cái bắt được nó | Nếu không có thứ đó thì sao |
|---|---|---|---|
| 1 | Khoảng pk bị hạ thành `Filter` (trực giác Postgres) | **Đọc `EXPLAIN` của chính mình** | Vĩnh viễn không lộ — kết quả đúng, 46x chậm |
| 2 | `DefaultCost` làm planner luôn chọn seq | Ba bài test `internal/engine` đỏ cùng lúc | Lộ ngay, không đáng lo |
| 3 | Parser treo vô hạn ở `x = = 1` | **Watchdog trong FuzzParse** | Bộ test **đứng 120s** thay vì đỏ — loại triệu chứng tệ nhất |
| 4 | Chọn span theo số điều kiện | `TestSelectivityNotPredicateCount` (khẳng định **số hàng đọc**) | Không lộ — kết quả đúng |
| 5 | `Est{0,0}` mà vẫn quét cả bảng | Cột **hàng đọc** trong dòng thống kê của REPL | Không lộ — kết quả đúng |
| 6 | `OpEq` **gán** thay vì **giao** ⇒ câu vô nghiệm quét cả bảng | **Gõ thử một câu vô nghiệm lúc viết nhật ký** | Không lộ — kết quả đúng |
| 7 | Hàm in chuỗi không thoát dấu nháy | **FuzzParse, 13 giây** | Không lộ — không ai gõ bốn dấu nháy liền |
| 8-11 | Bốn lỗi của bộ đo `sqllab` | Đọc **bảng** trước khi đọc **kết luận** (bài học phase 7) | Kết luận sai được ghi vào nhật ký như sự thật |
| 12 | Pushdown join chỉ 1.18x | **Cột `hàng(bật)` = 20005** | Ghi vào nhật ký một tỉ số 1.18x và một lời giải thích bịa ra |

Đọc dọc cột thứ tư thì thấy hình dạng của cả phase: **năm** trong mười hai con bug **không hề làm
sai kết quả**. Chúng chỉ làm engine tốn công. Và không một bài test nào khẳng định về **kết quả**
bắt được bất kỳ con nào trong năm con ấy.

> **Một bộ test cho optimizer phải khẳng định về CÔNG VIỆC ĐÃ LÀM, không chỉ về kết quả.**

Ba bài của phase này làm đúng thế, và ba cái tên ấy đáng nhớ hơn ba dòng code:
`TestPushdownKeepsResults`, `TestSelectivityNotPredicateCount`,
`TestContradictoryEqualitiesGiveEmptySpan`.

---

## Những thứ tôi cố ý **không** làm, và lý do

| Không làm | Lý do |
|---|---|
| `BEGIN`/`COMMIT` trong SQL | Phase 8 là front-end; nối nó vào `internal/txn` đòi một mô hình phiên (session) mà roadmap không có. Nhưng đây là **nợ lớn nhất của phase**, vì phase 6 có 4 mức isolation mà **không viết được** một transaction nhiều câu bằng SQL (P8-1) |
| Chia phần **đệ quy** cho Grace hash join | `numParts` cố định 32. Grace thật chia lại phần nào vẫn không vừa RAM. Ghi rõ trong code rằng **mọi kết luận của bảng số 2 là có điều kiện** trên chỗ này (P8-5) |
| Plan cache | Front-end đo được là **2.9x** phần thi hành của một truy vấn điểm, tức lý do phải có cache đã **tự đo được**. `prepare` đã tách riêng nên chỗ cắm đã sẵn; thiếu là khoá cache + phép vô hiệu hoá khi `CREATE INDEX`/`ANALYZE` chạy (P8-6) |
| Join **ba** bảng | Đòi **chọn thứ tự join** — lập trình động trên tập con. Đó là một phase riêng. `BindSelect` báo lỗi **rõ ràng** thay vì làm sai (P8-4) |
| `fsync` cho file tạm của spill | **Chỗ duy nhất trong cả repo ghi ra đĩa mà không cần bền.** Một lần fsync là **1.58ms** (số đo phase 0) trả cho một bảo đảm vô nghĩa. Nói rõ trong code, vì bốn phase trước dạy điều ngược lại — và **biết KHI NÀO không cần fsync** cũng là một phần của việc hiểu fsync |
| Sửa `bench-txn` (gộp `Get\|Scan` một benchtime) | Đã biết là bug của bộ đo, nhưng sửa nó là chạm vào deliverable của phase 6. Ghi vào sổ |
| Histogram cho `Selectivity` | Giống phase 7: `TestEstimateIsWrongOnSkew` đang khẳng định **đúng cái giới hạn ấy** và sẽ **đỏ** khi ai đó trả P7-6 |

---

## Ba câu mang sang phase sau

1. **Không phải mọi món nợ đều ghi được vào sổ nợ.** `Scan(lo, hi, fn)` không "thiếu" gì cả — nó
   đúng cho phase 6 và 7, và **không đủ** cho phase 8. Loại nợ ấy chỉ hiện ra khi có thêm một yêu
   cầu, nên cách phòng duy nhất là: khi thêm một API, hỏi *"ai cần GIỮ vòng lặp?"*.
2. **Một con số cũ trong nhật ký là một con số vô hình.** Phải đo lại con số của phase trước khi
   phase này chạm vào nó, và ghi **cả hai**.
3. **Một bảng số tốt chỉ cho bạn thấy chỗ bạn chưa đặt câu hỏi nào.** Đó là cách `plan.propagate`
   xuất hiện — và nó là thứ duy nhất trong tám phase mà **số đo tìm ra trước khi thiết kế nghĩ
   ra**.
