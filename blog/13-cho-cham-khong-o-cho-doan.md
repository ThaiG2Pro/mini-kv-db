# Bài 13 — Chỗ chậm không nằm ở chỗ bạn đoán

> Series [Mở nắp database](README.md) · bài 13 · bài thêm · cần đọc trước: [bài 7](07-long-txn.md)

Bài 7 kể rằng minidb giữ mọi phiên bản của một hàng **trong cùng một record**, xếp bản mới nhất
lên đầu. Một reader mở lâu làm chuỗi đó dài ra, và mọi lần đọc chậm theo. Ở phase 6, mình đo thấy
một hàng có 60 phiên bản thì đọc chậm hơn 8.5 lần so với hàng chỉ có 1 phiên bản. Rồi mình giải
thích con số đó, ghi lời giải thích vào nhật ký, và lặp lại nó trong bài 7.

Lời giải thích đó sai. Bài này kể vì sao nó nghe hợp lý, cái gì đã lật nó, và thói quen nào giúp
bạn khỏi mất một buổi chiều tối ưu nhầm chỗ.

Bài này không cần Docker, chỉ cần Go:

```bash
git clone https://github.com/ThaiG2Pro/mini-kv-db.git && cd mini-kv-db
go test ./internal/txn/ -run '^$' -bench GetChainDepth -benchtime=200000x
```

## Một chuỗi phiên bản trông như thế nào

Mỗi khoá trong cây B+Tree trỏ tới một value, và value đó là cả chuỗi:

```text
 [n=60] [cờ|xmin=160|len=3] "v59" [cờ|xmin=157|len=3] "v58" ... [cờ|xmin=3|len=2] "v0"
         └──── header 11 byte ───┘                                 └ bản cũ nhất
         bản mới nhất
```

Một lần đọc chọn bản **đầu tiên** mà snapshot của nó nhìn thấy được. Snapshot mới thì thấy ngay bản
đầu chuỗi. Snapshot cũ (của một transaction mở từ lâu) thì phải đi tới gần cuối.

## Phỏng đoán, và một dự báo đi kèm

Benchmark của phase 6 đo hai loại reader trên cùng một chuỗi:

- **`newest`**: snapshot mới, cần bản đầu tiên.
- **`oldest`**: snapshot cũ, cần bản cuối cùng.

```text
depth=60   newest 1450 ns   oldest 1480 ns   → oldest / newest = 0.98x
depth=1    newest  171 ns
```

Nếu chi phí nằm ở việc *đi dọc chuỗi tìm bản nhìn thấy được*, thì `oldest` phải đắt hơn `newest`
rõ rệt, vì nó đi xa hơn 59 bước. Hai con số lại bằng nhau. Kết luận của phase 6 nghe rất hợp lý:
*hàm `DecodeChain` giải mã cả chuỗi thành một mảng trước khi ai kịp hỏi cần bản nào, nên ai cũng
trả đủ giá.* Cách chữa cũng hiển nhiên: giải mã lười, chỉ dựng đúng bản cần.

Phần đáng giữ nhất của phase 6 không phải lời giải thích. Đó là **dự báo** ghi kèm theo nó:

> Sau khi sửa, `newest` ở depth=60 phải tụt về gần `newest` ở depth=1, còn `oldest` giữ nguyên.
> Nếu **cả hai** đều giảm thì bench đang đo cái khác.

Một giả thuyết có dự báo cụ thể thì kiểm được. Lời giải thích không kèm dự báo thì chỉ là một câu
chuyện.

## Lần sửa đầu tiên không đổi được gì

Gần một tháng sau, trong lúc làm việc khác, mình viết `VisibleRaw`: đi thẳng trên byte của chuỗi và
không dựng mảng nào. Đúng cách chữa mà phase 6 đề ra. Rồi chạy lại:

```text
depth=60   newest 2125–2211 ns   oldest 2355–2640 ns
```

(Máy hôm đó đang bận nên số tuyệt đối cao hơn. Hai cột được đo trong cùng một lượt, nên so với
nhau vẫn được.)

Vẫn bằng nhau. Đã bỏ hẳn cái mà giả thuyết gọi là thủ phạm, mà con số giả thuyết dự báo vẫn không
nhúc nhích. Đây là bằng chứng mạnh nhất mà bạn có thể có rằng **giả thuyết sai**. Thủ phạm thật
nằm ở một việc khác, cũng chung cho cả `newest` lẫn `oldest`.

## Thí nghiệm: hỏi profiler thay vì hỏi trực giác

```bash
go test ./internal/txn/ -run '^$' -bench 'GetChainDepth/depth=60$/newest' \
    -benchtime=300000x -benchmem -cpuprofile gc60.prof -o txn.test
go tool pprof -top -cum -focus 'Txn..Get$' txn.test gc60.prof
```

```text
BenchmarkGetChainDepth/depth=60/newest   300000   5520 ns/op   897 B/op   2 allocs/op

     1.50s  txn.(*Txn).Get
     0.84s    txn.VisibleRaw              ← 56%
     0.43s      txn.(*chainReader).next
     0.64s    db.(*DB).Get                ← 43%
     0.51s      runtime.memmove
```

Hai khoản, và cả hai đều chẳng liên quan gì tới "tìm bản nhìn thấy được":

1. **`db.Get` chiếm 43%**, gần hết là `memmove`. Cây B+Tree trả value bằng cách **chép cả chuỗi**
   ra một slice mới (897 byte), vì byte trong page có thể thành của page khác ngay khi buffer pool
   đuổi page đó đi. Người gọi chỉ cần 3 byte trong số đó.
2. **`VisibleRaw` chiếm 56%.** Xem từng dòng (`go tool pprof -list chainReader`): 490 trên 640ms nằm
   ở **một dòng**, `return v, nil`. Hàm `next()` dựng một struct `Version` 48 byte cho **từng**
   phiên bản trong 60 phiên bản, chỉ để vòng lặp bên ngoài đọc một trường rồi vứt đi.

Hai việc này xảy ra ở **mọi** lần đọc, bất kể snapshot là mới hay cũ. Đó là lý do `newest` và
`oldest` luôn bằng nhau.

## Nghi bộ đo: chép 900 byte mất 1.7 micro giây?

Dòng `copy(out, v)` trong `btree.Get` bị tính 0.51s cho 300000 lần, tức **1.7µs** cho mỗi lần chép
897 byte. Bộ nhớ hiện đại chép được hàng chục GB mỗi giây; 900 byte phải mất cỡ vài chục nano
giây. Khi profiler đưa ra một con số vô lý, đừng tối ưu theo nó vội. Đo thẳng cái nó chỉ vào:

```bash
go test ./internal/btree -run '^$' -bench CopyVsAlloc
```

Hai nhánh giống hệt nhau, chỉ khác một dòng `make`: nhánh `alloc+copy` cấp phát 897 byte mới rồi
chép vào (đúng việc `btree.Get` làm), nhánh `copy` chép vào một buffer có sẵn.

```text
BenchmarkCopyVsAlloc/alloc+copy   890 – 1865 ns/op   1024 B/op   1 allocs/op
BenchmarkCopyVsAlloc/copy          18 –   27 ns/op      0 B/op   0 allocs/op
```

Riêng phép chép chỉ tốn khoảng **20ns**. Cái đắt là **lần cấp phát**: bộ cấp phát, công việc của
GC, và lần đầu chạm vào vùng nhớ vừa nhận. Profiler tính tất cả vào `memmove`, vì `memmove` là
người đầu tiên chạm vào vùng nhớ đó. Con số cao bất thường còn vì máy hôm đó đang chạy hai tiến
trình nền ăn gần bốn nhân, nhưng thứ tự lớn nhỏ thì không đổi: cấp phát đắt gấp vài chục lần chép.

## Sửa: đọc tại chỗ, chỉ dựng cái cần trả

**Khoản 1: đừng chép cả chuỗi.** Thêm vào cây một lối đọc mà người gọi được nhìn vào byte của page
trong lúc page vẫn còn bị pin:

```go
// GetFunc là Get không chép: fn nhận value trỏ THẲNG vào page, trong lúc page
// còn bị pin. v chỉ sống trong fn; giữ nó lâu hơn là đọc phải page khác.
func (t *Tree) GetFunc(key []byte, fn func(v []byte) error) error {
	// ... đi từ root xuống leaf như Get ...
	err := fn(leafVal(n.cell(i)))
	t.unpin(id, false)
	return err
}
```

`Txn.Get` giờ đọc chuỗi ngay trong page và chỉ chép **một** phiên bản, cái mà snapshot nhìn thấy.

**Khoản 2: đừng dựng struct cho bản bị bỏ qua.** Vòng lặp chỉ đọc ba trường thô từ header, và chỉ
dựng `Version` cho đúng bản trả về:

```go
for r.more() {
	f, xmin, body, err := r.head()         // kiểm header, nhảy qua; không dựng gì
	if err != nil {
		return Version{}, false, err
	}
	if !found && s.Visible(xmin) {
		out, found = Version{Xmin: xmin, Deleted: f&flagDeleted != 0, Val: body}, true
	}
}
return out, found, r.end()                 // vẫn kiểm: chuỗi không được thừa byte
```

Vòng lặp vẫn đi hết chuỗi sau khi đã tìm thấy bản cần, và đó là cố ý. Byte đọc từ đĩa là dữ liệu
không đáng tin. Nếu đuôi chuỗi bị hỏng, lỗi phải nổi lên ở lần đọc đầu tiên chạm vào nó, không được
nằm im cho tới khi một reader cũ đi tới đó. Để chắc bản sửa không lén bỏ bước kiểm này, một fuzz
test đối chiếu kết quả với bản cũ ở mọi snapshot. Khi mình cố tình cài lỗi "dừng sớm" vào, fuzz bắt
được sau 0.08 giây.

## Kết quả

Máy vẫn bận, số đơn lẻ nhảy ±40%. Nên mình không so hai con số chạy cách nhau vài phút, mà chạy hai
bản (trước và sau khi sửa) **xen kẽ** nhau 10 cặp, rồi lấy tỉ số của **từng cặp**. Hai bản trong
một cặp chịu cùng điều kiện máy, nên nhiễu chung bị trừ đi.

```text
                  cũ        mới      tỉ số mới/cũ, trung vị [tứ phân vị]
depth=1 newest    348 ns    299 ns   0.85 [0.80, 0.88]
depth=60 newest  5213 ns    708 ns   0.14 [0.13, 0.16]
depth=60 oldest  3878 ns    714 ns   0.18 [0.17, 0.19]
byte cấp phát mỗi lần Get ở depth=60:  897 → 4
```

Đọc lại dự báo của phase 6: *"`newest` tụt, `oldest` giữ nguyên; cả hai đều giảm thì bench đang
đo cái khác."* **Cả hai đều giảm**, 5–7 lần. Bench không sai. Sai là **mô hình**: phase 6 tưởng
chi phí nằm ở việc *đọc hiểu* chuỗi, trong khi nó nằm ở hai việc làm cho mọi phiên bản: chép cả
chuỗi ra khỏi page, và dựng struct cho từng bản.

Depth 60 vẫn đắt hơn depth 1 khoảng 2.4 lần. Phần đó là bước kiểm đuôi chuỗi mà mình cố ý giữ. Phần
còn lại của bài toán phình phiên bản là việc của vacuum, đúng như bài 7 đã nói.

## Một phản ví dụ cùng tuần: giảm cấp phát không có nghĩa là nhanh hơn

Cũng tuần đó, mình sửa đường quét bảng của minidb: số lần cấp phát mỗi hàng giảm từ **7 xuống 2**.
Profile đã chỉ ra hàm giải mã hàng chiếm 54% thời gian quét, nên mình chắc mẩm sẽ thấy nhanh lên rõ.

```text
16 cặp chạy xen kẽ: tỉ số sau/trước = 1.07 (trung vị)
```

Không phân biệt được với nhiễu. Số lần cấp phát giảm hơn ba lần, nhưng **số byte** chỉ giảm 11%
(352 → 312 byte mỗi hàng). Công việc của GC tỉ lệ với số byte, không tỉ lệ với số lần gọi. Bài học
đi kèm: **số đếm (allocs/op) đáng tin hơn số giờ trên một máy ồn, nhưng số đếm giảm không chứng
minh được rằng thời gian giảm.** Phép đo thời gian đó đang chờ chạy lại trên một máy Linux yên
tĩnh, bằng script [`scripts/p91-seqscan.sh`](../scripts/p91-seqscan.sh).

## Database thật làm gì

Cả hai DB lớn đều tránh hai khoản mà minidb vừa trả:

- **Postgres** đọc tuple **tại chỗ** trong shared buffer, giữ pin trên buffer suốt lúc dùng, và
  chỉ chép ra (`heap_copytuple`) khi cần giữ lâu hơn. Khi giải mã, nó chỉ tách đúng những cột mà
  câu truy vấn cần, không tách cả hàng. Postgres gọi bước này là *deform*.
- **InnoDB** để bản mới nhất **tại chỗ** trong page. Bản cũ chỉ được dựng lại từ undo log khi một
  snapshot cũ thật sự cần nó. Reader mới **không trả gì** cho các phiên bản cũ, vì nó không bao giờ
  chạm tới chúng.

`GetFunc` của minidb chính là bài học "đọc tại chỗ, giữ pin" của Postgres. Còn cách của InnoDB
(đẩy bản cũ ra khỏi record) là món nợ lớn hơn mà bài 7 đã chỉ ra.

## Mang về dùng

1. **Viết dự báo trước khi sửa, và tin nó hơn tin lời giải thích.** "Sau khi sửa, X phải giảm, Y
   phải giữ nguyên." Nếu bạn đã gỡ đúng cái mà giả thuyết gọi là thủ phạm mà con số không đổi, thì
   giả thuyết sai, dù nó nghe hợp lý đến đâu.
2. **Hai nhánh chậm bằng nhau là manh mối mạnh.** Nó cho biết chi phí nằm ở phần mà **cả hai** cùng
   làm, chứ không phải ở chỗ chúng khác nhau. Hãy tìm thủ phạm ở phần chung đó.
3. **Con số vô lý trong profile thì đo thẳng nó.** "Chép 900 byte mất 1.7µs" hoá ra là "cấp phát
   1KB mất cả micro giây". Một benchmark 5 dòng tách được hai thứ mà profiler gộp làm một.
4. **Cấp phát thường đắt hơn chép.** Trả về một *view* vào dữ liệu có sẵn (như `GetFunc`) thay vì
   một bản chép. Cái giá là một hợp đồng về thời gian sống: view chỉ dùng được trong callback. Phá
   hợp đồng đó là đọc phải dữ liệu của page khác, và không có lỗi nào báo cho bạn.
5. **Trên máy ồn, đo theo cặp.** Chạy hai bản xen kẽ và so tỉ số của từng cặp, đừng so hai con số
   đo cách nhau vài phút. Nếu khoảng tứ phân vị của tỉ số vắt qua 1.0, câu trả lời trung thực là
   "chưa biết".

---

Code: [`internal/txn/version.go`](../internal/txn/version.go) (`VisibleRaw`, `chainReader.head`),
[`internal/btree/btree.go`](../internal/btree/btree.go) (`GetFunc`),
[`internal/txn/bench_test.go`](../internal/txn/bench_test.go) (`BenchmarkGetChainDepth`),
[`internal/btree/bench_test.go`](../internal/btree/bench_test.go) (`BenchmarkCopyVsAlloc`) ·
nhật ký: [`diary/phase9.md`](../diary/phase9.md) (bảng 9, bảng 10), [`diary/phase6.md`](../diary/phase6.md) (số đo gốc).

**Về mục lục:** [README](README.md)
