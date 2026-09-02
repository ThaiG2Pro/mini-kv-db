# Phase 7 — nhật ký lệnh đầy đủ, thất bại và cải tiến

Bổ sung cho [`phase7.md`](./phase7.md). File kia là **kết quả** đã biên tập; file này là **toàn bộ
đường đi**, kể cả các ngõ cụt.

Hình dạng của phase này, so với sáu phase trước:

- Phase 3 bị chặn ở năm chỗ, phần lớn bởi thứ tôi tự cài từ phase trước.
- Phase 4 có **bốn trong bảy giả thuyết bị bác**, một cái sai ở tầng khái niệm.
- Phase 5: **bốn bug khác nhau hoá ra một nguyên nhân gốc**, và **hai lỗi của bộ đo trên một lỗi
  của code**.
- Phase 6: **một chuỗi ba giả thuyết** cho **một** hiện tượng, và fuzzer tìm ra trong 3 giây thứ
  mà ba lần đọc code không thấy.
- Phase 7: hình dạng mới, và nó là hình dạng **đáng lo nhất**. Lượt chạy tìm ra **bốn** con bug —
  và **không con nào nằm trong database**. Cả bốn nằm trong **bộ đo**: một bảng deliverable phản
  bác dòng kết luận của chính nó, một benchmark đo fsync mà nhãn ghi "index maintenance", một lời
  đọc bảng suy nhân quả ngược, và một bộ test xanh với một cursor hỏng.

Thêm một điểm khác biệt về **thứ tự**: sáu phase trước, việc đầu tiên là viết code mới. Phase này,
việc đầu tiên là **trả nợ** — và nếu không trả thì không có gì để viết.

Cấu trúc lượt: 1 lượt viết code (không chạy gì) → 1 lượt chạy + sửa bug → 1 lượt viết nhật ký.

---

## Lượt 1 — viết code, không chạy một lệnh nào

### Việc đầu tiên không phải viết index, mà là phát hiện phase 7 chưa tồn tại được

Ngồi vẽ hình của index scan: *"với mỗi mục của index, lấy pk rồi đi tra bảng"*. Viết ra thành code
thì nó là một phép **truy cây nằm trong callback của một phép duyệt cây**.

Rồi mở `internal/db/db.go` và đọc lại `Range`. Nó giữ `d.mu` suốt lần duyệt, và có chú thích tại
chỗ nói rõ vì sao. Nên hình trên **tự khoá chết**.

Đó là lúc phải dừng viết code và đi đọc sổ nợ của chính mình. `docs/debts.md`, mục P6-4:

> `Txn.Scan` gom hết cặp khóa/giá trị nhìn thấy được vào RAM rồi mới gọi `fn`. [...]
> **Cách trả:** cần **latch-coupling (P4-5)** trước.

Nếu câu cuối đúng thì đường đi là: latch-coupling → nhiều writer vật lý → (kết luận phase 6) bỏ
pha undo physical → **viết lại phase 5**. Để làm một secondary index.

Tỉ lệ đó sai tới mức phải nghi cái tiền đề, và đó là ngõ cụt đầu tiên **được tránh** thay vì được
đi vào.

### Ngõ cụt đã đi vào: nghĩ rằng nhả latch cần một cơ chế của phase 4

Nửa giờ đầu tôi tìm cách làm cursor **pin** được leaf mình đang đứng, hoặc giữ một latch nhẹ hơn.
Cả hai đều dẫn tới cùng một chỗ: phải có thứ tự latch, phải có phát hiện deadlock giữa các latch,
tức latch-coupling dưới một cái tên khác.

Cái mở được nút không phải một cơ chế mới mà một **câu hỏi lại**: *vì sao* `Range` phải giữ latch?
Để cursor còn đúng. Nhưng "đúng" nghĩa là gì? Ở phase 4 nó có nghĩa "reader thấy một trạng thái
nhất quán". Ở **phase 6** thì việc reader thấy gì đã do **ảnh chụp** quyết định — latch không còn
việc ấy nữa. Nó chỉ còn phải bảo vệ **tính đúng của vị trí cursor**.

Và một vị trí thì không cần được **bảo vệ**, nó cần được **kiểm tra**. Đó là `gen`.

**Ghi lại vì nó là bài học về cách ghi nợ:** tôi bị chặn nửa giờ bởi một câu **do chính tôi viết**.
Câu ấy đúng lúc viết (phase 6, chưa có ai nghĩ tới cursor restoration) và sai lúc đọc. Sổ nợ nên
ghi **hiện tượng** + **cách kiểm chứng**, không nên ghi **cách trả** — cách trả phụ thuộc vào
những thứ chưa tồn tại lúc ghi.

### Quyết định nhỏ nhưng phải ghi: tăng `gen` ở MỌI `Put`, không chỉ ở split

Bản đầu tôi định chỉ tăng ở split/merge/đổi root — chỗ entry thật sự **dịch chuyển**. Kịp nghĩ lại
trước khi viết: `Put` ghi đè một khóa cũng có thể **compact** cả page (phase 2), và compact dồn
lại **mọi** offset. Slot index không đổi, nhưng **khóa nằm ở slot ấy** thì đổi.

Đếm hẹp hơn thì đúng ở nhiều ca và **sai lặng lẽ** ở vài ca. Đúng cái loại lỗi mà phase 5 gọi là
*hai nguồn sự thật cho cùng một sự việc*.

Và một dòng comment nữa được viết ra thành chữ vì nó là **điều kiện tiên quyết đang ngầm**:
`gen` **không** dùng `atomic`, vì mọi lối vào cây đều đã ở dưới `d.mu` (một writer, do code bắt
buộc — P1-2). Ngày nào bỏ được ràng buộc ấy thì đây là dòng **đầu tiên** phải đổi.

### Bốn lỗi tự bắt trong lượt code, không cần chạy

1. **Compile error** ở `internal/query/query.go`: `cannot call span (variable of type float64)` —
   rác còn lại của một lần viết dở trong `Selectivity`. Viết lại thành nhánh
   `whole := spanOf(cs.Min, cs.Max)` với đường lùi `1/Distinct` cho kiểu không phải số, và xoá
   luôn hàm `span` không còn ai gọi.

2. **Lỗi bị nuốt hoàn toàn im lặng** ở `internal/query/run.go`. Callback đặt lỗi vào `err`, rồi
   `err = t.ScanIndex(...)` chạy **sau** callback và ghi `nil` lên đúng cái lỗi đó. Sửa bằng một
   biến `inner` riêng + `if err == nil { err = inner }`. **Phase 6 đã dính đúng bẫy này một lần**
   ở `Txn.Scan`, nên comment tại chỗ ghi rõ điều đó — đây là lần thứ hai của cùng một hình.

3. **Đọc map không đồng bộ**: `ScanIndex` đọc `t.c.byOID` mà không qua RWMutex. Thêm
   `Catalog.TableByOID`. `-race` sẽ bắt được, nhưng chỉ khi có bài test đồng thời đi qua đúng
   dòng ấy — mà lúc đó tôi chưa có bài test nào như thế (và đó chính là chủ đề của bug #4 ở lượt 2).

4. **Che tên**: `cursor_test.go` đặt một biến `k` che mất helper `k` của package. Đổi thành `cur`.

### Chỗ suy nghĩ lâu nhất của lượt code, và nó không phải code

`Unique` là một trường `bool`, nhưng nó **đổi hình dạng khóa**:

- non-unique **phải** nhét pk vào khóa (hai hàng cùng giá trị = hai mục),
- unique **không được** nhét (nhét thì tính duy nhất bốc hơi).

Ngồi thêm mười phút để đi tiếp hệ quả, và hệ quả đáng hơn cái quyết định: nếu unique không nhét pk
thì hai transaction chèn cùng một giá trị sẽ ghi **đúng một** khóa cây ⇒ **first-committer-wins
của phase 6 tự động thực thi ràng buộc duy nhất**, không cần thêm một dòng nào. Và index
non-unique thì **không bao giờ** xung đột được.

Viết luôn hai bài test cho hai chiều. Vế thứ hai (`TestNonUniqueIndexNeverConflicts`) mới là vế
làm cho vế thứ nhất có nghĩa — nếu **mọi** index đều xung đột thì bài test unique xanh mà chẳng
chứng minh gì.

Một chi tiết nhỏ của `TestUniqueIndexConflictsAcrossTxns` phải ghi: nó dùng `Store().Begin` thô
chứ không dùng `Update`, vì `Update` **tự thử lại** khi gặp xung đột — tức là nó sẽ **giấu** đúng
cái hiện tượng đang cần quan sát.

### Catalog bằng JSON, và lý do nó không phải sự lười

Cân nhắc tự viết một encoding nhị phân cho catalog. Quyết định **không**: catalog được đọc **một
lần** lúc mở database. Một bộ codec tự viết là một bộ codec nữa **phải fuzz** (phase 6 và phase 7
đều có một target chỉ để lo việc canonical), đổi lấy vài chục byte và **không** đổi lấy một nano
giây nào trên đường nóng.

---

## Lượt 2 — chạy và sửa bug

### Thứ tự chạy, và vì sao không bắt đầu bằng deliverable

`gofmt` → `go vet` → `go build` trước, vì ba lệnh ấy rẻ và nếu đỏ thì mọi số phía sau vô nghĩa.
Rồi mới `idxlab`. Không bắt đầu bằng `go test ./...` vì bộ test đầy đủ mất ~80 giây và tôi muốn
thấy bảng deliverable trước — nó là thứ có thể làm tôi phải viết lại code, còn test đỏ thì chỉ làm
tôi phải sửa test.

Hoá ra thứ tự đó đúng vì lý do khác: bảng deliverable **tự tố giác** ngay ở lần chạy đầu.

### Bug 1 — bảng deliverable phản bác dòng kết luận của chính nó

Lần chạy đầu, `-rows 20000 -repeat 20`. Đọc hàng 25%: `idx/seq = 0.63`, index còn thắng **1.6x**.
Rồi đọc dòng ngay dưới bảng: *"thời gian đổi vai ở khoảng 19% theo mô hình đo"*.

Mất một phút để tin là mình đọc đúng. Hai câu ấy nằm cách nhau **bốn dòng** trong cùng một output
và không thể cùng đúng.

Truy ra thủ phạm nhanh, vì nó là loại lỗi rất cụ thể: con số 19% **không hề được tính từ số đo**.
`fmt.Printf` in `query.BreakEven(cm)*100` — tức **dự đoán của mô hình** — dưới một cái nhãn nói
rằng đó là **quan sát**. Bảng in số đo, dòng kết luận in dự đoán, và không có ai đối chiếu.

Sửa: `query.CrossOver(sw)` nội suy từ **tỉ số của hai dòng kề nhau** của chính bảng. Không dùng
giả định "chi phí seq phẳng" (tôi đã dùng nó trong phép tính tay và nó cho 38.4% thay vì 37.5%).

Rồi thêm cột `planner(đo) CŨNG CHỌN SAI`, vì hoá ra mô hình với hằng số **đo được** cũng chọn sai
ở hàng 25%. Trước khi thêm cột đó, bảng chỉ trưng ra chỗ mô hình **đoán** sai — tức nó chỉ tố cáo
cái sai mà tôi đã biết, và im lặng về cái sai tôi chưa biết. Một bảng như thế dễ đọc thành "mô
hình đo thì đúng".

### Truy tiếp: vì sao mô hình ĐO ĐƯỢC vẫn sai 2 lần

Đây là nửa giờ đáng nhất của cả phase.

```console
$ python3 -c '...'   # (dán đầy đủ trong phase7.md)
chi phí mỗi hàng của index scan: 1173 ns
  trừ một bước index đã đo 241 ns -> tra bảng TRONG scan = 932 ns
  nhưng đo riêng bằng point lookup:                      1908 ns
  -> đo riêng ĐẮT hơn 2.0x
```

Cùng một phép toán, đo hai cách, chênh **2x**. Nên một trong hai cách đo sai — hoặc **cả hai đều
đúng và tôi đang hiểu sai câu hỏi**.

Mở `measureConstants` và đọc lý lẽ của cách đo, do chính tôi viết ở lượt 1:

> Khóa đi kiểu bước nhảy lớn để không đi tuần tự trong cùng một leaf — nếu đo trên khóa liền nhau
> thì mọi lần tra đều trúng page vừa nạp và con số ra **bé hơn sự thật**.

Chữ **"sự thật"** ở đó là chỗ sai, và nó sai vì một giả định không nói ra: rằng "sự thật" của một
phép tra bảng là **một** con số. Một index scan tra bảng **theo thứ tự index**. Nếu thứ tự ấy
tương quan với thứ tự pk thì "trúng page vừa nạp" **chính là** cái nó làm thật.

Nên không có **một** `CFetch` đúng: có **hai**, và cái nào đúng phụ thuộc **index nào**. Đo cả hai,
in cả hai, và để bảng nói. Dự đoán từ ca tương quan: **30.1%**; đo được: **36.8%**. Từ 7.5x lệch
xuống 1.2x lệch.

Đi đọc `costsize.c` của Postgres **sau** khi đã đo, và thấy `indexCorrelation`: nó **nội suy** chi
phí giữa ca tuần tự và ca ngẫu nhiên theo bình phương tương quan. Tôi vừa đo được hai đầu của cái
nội suy ấy mà không biết nó có tên.

**Ghi lại vì nó tổng quát:** một hằng số chi phí không phải thuộc tính của **phép toán**, nó là
thuộc tính của **phép toán cộng với thứ tự truy cập**. Đó là vì sao `correlation` phải lưu **riêng
cho từng cột** và không suy ra được từ hai hằng số kia.

### Bug 2 — một benchmark đo fsync suốt 3000 vòng

`make bench-index` in ra:

```console
BenchmarkIndexMaintenance/indexes=0-6         	    3000	   1584814 ns/op
BenchmarkIndexMaintenance/indexes=1-6         	    3000	   1773932 ns/op
BenchmarkIndexMaintenance/indexes=2-6         	    3000	   1754438 ns/op
BenchmarkIndexMaintenance/indexes=3-6         	    3000	   1734841 ns/op
```

Đọc thô: 1.00x / 1.12x / 1.11x / 1.09x, tức **index gần như miễn phí ở đường ghi**. Nhưng bảng 2
của `idxlab`, cùng công việc, cho 1.54x / 2.72x / 5.39x.

Hai số đo mâu thuẫn ⇒ **nghi bộ đo trước** (quy tắc 5 của skill). Và ở đây không cần suy luận
nhiều, chỉ cần nhìn **độ lớn**: 1.58 ms là con số tôi đã thấy ở phase 0 và phase 5 nhiều lần —
đó là **một lần fsync**. Việc bảo trì index thì cỡ vài µs. Mỗi vòng lặp một `c.Update` = một
transaction = một fsync. **99.7% con số là durability.**

Sửa: gộp 200 hàng/transaction. Group commit của phase 5 làm phần còn lại.

```console
BenchmarkIndexMaintenance/indexes=0-6         	    6000	     17158 ns/op
BenchmarkIndexMaintenance/indexes=1-6         	    6000	     32595 ns/op
BenchmarkIndexMaintenance/indexes=2-6         	    6000	     40550 ns/op
BenchmarkIndexMaintenance/indexes=3-6         	    6000	     49244 ns/op
```

1.00 / 1.90 / 2.36 / 2.87x. Không **bằng** `idxlab` (1.54/2.72/5.39x — khác lô, khác số hàng,
khác thứ tự cột) nhưng **cùng hình**: đơn điệu tăng, cùng bậc độ lớn. Trước khi sửa thì hai bộ đo
mâu thuẫn; sau khi sửa thì chúng xác nhận nhau.

**Bài học phase 5, lần thứ ba:** khi một chi phí cố định lớn trùm lên phép đo, cái phép đo đo được
là chi phí cố định ấy. Lần một: `BenchmarkGet` ở 2000 vòng đo chi phí nạp page. Lần hai: bench mức
isolation ở phase 6. Lần ba là đây. Ba lần cùng một hình mà lần nào cũng phải mất mười phút mới
nhận ra.

### Bug 3 — lời đọc bảng suy nhân quả ngược, và cột đo được chỉ đúng thủ phạm

Bảng 2, cột `sửa`, ba lần chạy khác nhau:

```
lần 1:  12.44   15.71   11.62   29.74
lần 2:  12.48   12.26   12.17   29.29
lần 3:  16.48   13.30   14.09   35.05
```

Lời đọc bảng viết: *"nó vẫn đắt lên theo số index, vì `Upsert` phải đọc hàng cũ"*. Ba dòng đầu
**không** đắt lên theo số index; chúng dao động quanh nhau và **không đơn điệu** giữa các lần chạy.
Chỉ dòng cuối nhảy một bậc, mọi lần.

Nên câu hỏi đúng không phải "vì sao nó đắt lên" mà **"vì sao chỉ dòng cuối đắt lên"**. Mở code:
`cols := []string{"kind", "city", "payload"}` — nên ở `nIdx = 3`, cột bị sửa (`payload`) **chính
là** một cột được index. Comment ngay trên vòng lặp còn ghi *"sửa một cột KHÔNG được index
(payload)"*, và câu đó **đúng ở ba dòng đầu, sai ở dòng cuối** — loại comment tệ nhất.

Không sửa lời đọc bảng bằng lập luận mới. Thay vào đó **thêm một cột đo**: `ghi idx/sửa`, lấy từ
`tx.St.IndexWrites`. Nó ra **0, 0, 0, 2**.

Số **2** là chỗ tôi học được thứ mới: một mục index **không sửa được tại chỗ**, vì khóa của nó
**chứa** giá trị cũ. Đổi giá trị một cột được index là **đổi vị trí của nó trong cây** ⇒ xoá + chèn.

Và ba số **0** đầu là bằng chứng của một tối ưu mà tôi đã cài **vì một lý do khác**: `Tx.write`
không ghi lại mục index nếu giá trị không đổi, và tôi làm thế để **tránh nối dài chuỗi version**
(trần ~2KB của P6-1). Hoá ra nó có tên: **HOT (heap-only tuple)** của Postgres.

**Ghi lại vì nó là một cái bẫy về thể loại:** ba lần chạy đều cho dữ liệu **đúng**. Cái sai là
**câu chữ đọc dữ liệu**, và nó sai theo hướng nghe rất hợp lý — "đọc hàng cũ tốn tiền" là một câu
đúng về cơ chế, chỉ có điều chi phí ấy nhỏ tới mức không hiện ra trong phép đo. Một lập luận đúng
về cơ chế vẫn có thể là một lời giải thích **sai** cho một con số.

### Bug 4 — bộ test xanh với một cursor hỏng

Ở lượt 1 tôi ghi lại một việc còn nợ: *"chưa kiểm rằng `TestCursorRestoresAfterSplit` đỏ khi tắt
cơ chế `gen`"*. Trả nó bằng một dòng `sed`:

```console
$ sed -i '153s/if c.gen != c.t.gen {/if false {/' internal/btree/cursor.go
$ go test ./internal/btree/ -count=1 -run 'CursorRestores'
--- FAIL: TestCursorRestoresAfterSplit (0.00s)
    cursor_test.go:49: sau khi cây đổi, khóa kế = "k000051", mong "k000101"
--- FAIL: TestCursorRestoresAfterDeleteOfOwnKey (0.00s)
    cursor_test.go:95: khóa kế sau khi 50 và 51 bị xóa = "k000053", mong "k000052"
```

Đỏ, và **đỏ đúng kiểu**: mất đúng nửa leaf sau một split (51 → 101), và nhảy qua một khóa sau xóa.
Đó là bằng chứng bài test biết báo sai.

Rồi vì đang có sẵn cây bị hỏng, chạy luôn cả bộ:

```console
$ go test ./internal/db/ ./internal/txn/ ./internal/table/ ./internal/query/ -count=1 -run 'Iter|Scan|Index|Plan'
FAIL	minidb/internal/db	2.592s
ok  	minidb/internal/txn	2.175s
ok  	minidb/internal/table	0.786s
ok  	minidb/internal/query	0.156s
```

Và đây là chỗ lượt chạy đổi hướng. **Ba trong năm package xanh với một cursor hỏng.** Tức toàn bộ
bài test của phase 7 — index, planner, ba kế hoạch, unique, catalog, fuzz bảng — **không** kiểm
được cái cơ chế mà cả phase dựa lên.

Lý do đơn giản tới mức khó chấp nhận: mọi lần quét trong các bài test ấy chạy **một mình**. Không
có ai ghi vào cây giữa hai bước ⇒ `gen` không bao giờ đổi ⇒ nhánh `restore()` **không bao giờ
chạy**. Tôi đã có bài test đồng thời (ở `internal/db`), nên tôi đã **tin** là mình có phủ. Nhưng
phủ ở tầng dưới không phủ cho tầng trên: tầng trên có đường đi riêng (`Txn.Scan` → merge join →
`table.ScanIndex` → `Get`), và đường đó chưa từng bị ai chen vào giữa.

Thêm `TestIndexScanSurvivesWriterMidScan`: ghi từ chính goroutine đang quét, giữa **mọi** bước của
một index scan. Kiểm nó theo **cả hai chiều** ngay:

```console
$ go test ./internal/table/ -count=1 -run 'IndexScanSurvivesWriterMidScan' -v   # gen BẬT
    table_test.go:493: quét 200 mục, chèn 200 mục mới trong lúc quét
--- PASS: TestIndexScanSurvivesWriterMidScan (0.89s)

$ # gen TẮT
--- FAIL: TestIndexScanSurvivesWriterMidScan (0.56s)
    table_test.go:479: khóa index không tăng ngặt: ["c00000"] sau ["c00000"]
```

Đỏ ở một **kiểu thứ ba**: không nhảy qua khóa mà **lặp** khóa — slot cũ trỏ về một khóa đã đi qua.

Bài test này còn tình cờ là **bằng chứng ngược** của bài test đã treo 60 giây ở lượt 1: ở đây mỗi
bước chèn một mục nằm **phía trước** cursor, mà lần quét **vẫn kết thúc**. Vì `Txn.Scan` đi qua ảnh
chụp MVCC, nên mục vừa chèn **không nhìn thấy được**. Cùng một hình, hai kết cục, và cái làm nên
khác biệt là **một cơ chế của phase 6**.

### Dọn dẹp và một chi tiết nhỏ đáng ghi

`go build ./cmd/idxlab` để lại một file binary `idxlab` ở gốc repo, và `.gitignore` chỉ có `/bin/`
`/data/` `*.db` `*.wal` `*.test`. Xoá tay. (Nợ nhỏ: `.gitignore` chưa che binary build ở gốc.)

Còn một chi tiết về công cụ: `rtk` chiếm quyền `grep`/`head` trần nên chúng trả "0 matches" hoặc
in usage. Mọi lệnh cần output **nguyên văn** đều phải đi qua `rtk proxy "..."`. Và `rtk proxy` nhận
cả chuỗi làm lệnh nên `rtk proxy "make bench-index 2>&1"` biến `2>&1` thành một make target:
`make: *** No rule to make target '2>&1'`.

---

## Bảng tổng kết lượt 2

| # | Hiện tượng | Bug ở đâu | Cách phát hiện |
|---|---|---|---|
| 1 | dòng kết luận nói 19%, bảng ngay trên nó nói index còn thắng ở 25% | **bộ đo** — in dự đoán của mô hình dưới nhãn của một quan sát | đọc hai dòng cách nhau 4 dòng trong cùng output |
| 1b | mô hình với hằng số **đo được** vẫn sai 2x | **cách đo** — `CFetch` đo bằng khóa nhảy lung tung, còn index scan tra bảng theo thứ tự tương quan | nội suy tay từ chính bảng: 932 ns vs 1908 ns |
| 2 | index "gần như miễn phí" ở đường ghi | **bộ đo** — 1 transaction/vòng ⇒ 99.7% con số là fsync | mâu thuẫn với `idxlab` trên cùng công việc |
| 3 | cột `sửa` được giải thích bằng "phải đọc hàng cũ" | **câu chữ** — dữ liệu đúng, lời đọc dữ liệu sai | ba dòng đầu không đơn điệu giữa các lần chạy |
| 4 | bộ test xanh | **lỗ phủ** — mọi lần quét ở tầng trên chạy một mình | tắt hẳn cơ chế cần kiểm, xem ai đỏ |

**Không con nào trong database.** Đó là điều đáng lo hơn là đáng mừng: nó có nghĩa là ở phase này,
thứ dễ sai nhất không còn là code, mà là **cái tôi dùng để tin rằng code đúng**.

---

## Số lần chạy lại của lượt 2

| Lệnh | Số lần | Vì sao phải chạy lại |
|---|---|---|
| `go run ./cmd/idxlab` (các biến thể) | 6 | 1 lần đầu → 1 sau khi thêm `CrossOver` → 1 ở 50k để kiểm tỉ số bền → 3 lần cho bảng `maintain` sau hai lần sửa lời đọc bảng |
| `go test ./internal/query/ -bench IndexMaintenance` | 2 | trước và sau khi gộp lô |
| `go test ./internal/table/ -run IndexScanSurvives...` | 3 | viết → xanh với gen bật → đỏ với gen tắt |
| `go test ./... -count=1` | 2 | 1 lần giữa lượt, 1 lần sau tất cả sửa đổi |
| `go test -race ./internal/...` | 1 | 11/11, ~110s |
| `TestTransferInvariantPerLevel` | 3 | kiểm ổn định sau khi de-flake ở lượt 1 |
| fuzz (3 target × 120s) | 3 | mỗi target một lần, đủ 120s |

---

## Những thứ KHÔNG làm, và lý do

- **Không** sửa `DefaultCost` từ 20 thành 4.5. Một hằng số đúng trên máy này là một hằng số sai
  trên máy khác; một **cột trong bảng** cho thấy hằng số sai làm planner chọn sai ở dải nào thì
  đúng ở mọi máy. Ghi thành nợ P7-7.
- **Không** trả nửa **ghi** của P4-5 (nhiều writer vật lý cùng sửa cây). Lý lẽ của phase 6 còn
  nguyên: nó buộc bỏ pha undo physical của phase 5. Phase này chỉ cần nửa **đọc**.
- **Không** thêm histogram. `TestEstimateIsWrongOnSkew` đang khẳng định **đúng cái giới hạn ấy** —
  nó là một bài test **của một giới hạn**, và nó sẽ đỏ khi ai đó trả P7-6. Đỏ đúng lúc.
- **Không** viết vectorized execution. Nhưng đã định vị được nó **nằm ở đâu trong số đo**: 413-544
  ns cho một bước quét trên một hàng 4 cột, trong đó `keys.Decode` một mình chiếm 117 ns + 2 alloc.
- **Không** giữ bản sửa `runtime.Gosched()` cho bài test chuyển tiền. Nó không sửa được gì (vẫn 2/8
  đỏ) và nó xáo trộn số đo của phase 6. Một bản sửa dựa trên chẩn đoán sai thì phải **trả lại
  nguyên trạng**, không phải để đó cho chắc.
