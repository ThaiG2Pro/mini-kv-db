# Bài 10 — Đọc `EXPLAIN` như người viết ra nó

> Series [Mở nắp database](README.md) · bài 10/11 · cần đọc trước: [bài 8](08-index.md), [bài 9](09-planner.md)

`EXPLAIN` in ra một cái cây với những cái tên như `Hash Join`, `Bitmap Heap Scan`,
`Sort Method: external merge`, và phần lớn chúng ta chỉ lướt tìm chữ `Seq Scan` rồi thôi. Bài này
đọc `EXPLAIN` từ phía người viết ra nó: mỗi dòng là một đoạn code đang chạy, và khi biết đoạn code
đó làm gì thì bạn đọc được cả những con số bên cạnh.

## Mỗi dòng là một toán tử, và mọi toán tử có cùng một hình dạng

Cây trong `EXPLAIN` là cây **toán tử**. Mỗi toán tử là một vòng lặp nhỏ: xin hàng từ toán tử con,
làm gì đó với hàng, rồi đưa cho toán tử cha. Cả Postgres, MySQL 8 và minidb đều dùng một kiểu thiết
kế gọi là **Volcano** (hay *iterator model*). Trong minidb, mọi toán tử chỉ có đúng hai phương thức
(`internal/exec/exec.go`):

```go
// Op là một toán tử Volcano. Ba phương thức, và cái quan trọng nhất là cái
// KHÔNG có: không có phương thức nào trả về "tất cả các hàng".
type Op interface {
	Next() (bool, error)
	Close() error
}
```

Toán tử gốc gọi `Next()` trên con của nó, con gọi `Next()` trên cháu, và cứ thế tới tận lá, nơi
toán tử quét đọc page thật. Mỗi lần gọi đưa lên **một** hàng. Toán tử lọc (`Filter`) là một vòng
lặp gọi `Next()` cho tới khi gặp hàng thoả điều kiện:

```go
func (f *filterOp) Next() (bool, error) {
	for {
		ok, err := f.in.Next()           // xin một hàng từ con
		if err != nil || !ok {
			return false, err
		}
		// ... tính điều kiện; chỉ TRUE mới lọt, NULL bị loại
		if pass {
			return true, nil             // đưa lên cha
		}
	}
}
```

Vì vậy **đọc `EXPLAIN` từ dưới lên**: lá là nơi dữ liệu bắt đầu, và hàng chảy dần lên gốc.

## minidb cho bạn thấy thứ mà Postgres giấu đi

Trước khi thành cây toán tử, câu SQL đi qua hai bước mà `EXPLAIN` của Postgres không in ra: bước
**viết lại logic** (optimizer đổi câu truy vấn thành một câu tương đương nhưng rẻ hơn) và bước
**chọn cách chạy** (planner chọn index hay quét, join kiểu gì). `EXPLAIN` của minidb in cả hai:

```console
$ go run ./cmd/minidb -db data/sql/blog10.db -e "EXPLAIN SELECT ev.city, dim.name FROM ev
    JOIN dim ON ev.kind = dim.kind WHERE dim.kind < 5 ORDER BY ev.city;"

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
    -> Sort by=ev.city budget=4096 hàng  (rows≈1 cost≈11)
      -> NestedLoopJoin on=(ev.kind = dim.kind)  (rows≈1 cost≈10)
        -> SeqScan ev filter=(ev.kind < 5)  (rows≈5 cost≈5) — quét tuần tự cả bảng (có 1 đường khác, đều đắt hơn)
        -> SeqScan dim [kind < 5]  (rows≈1 cost≈1) — khoảng trên khóa chính: quét đúng 1/3 hàng của cây pk
```

Nhìn cây `gốc` và cây `tối ưu`. Điều kiện `dim.kind < 5` được **đẩy xuống** sát chỗ đọc bảng
(*predicate pushdown*), để lọc bớt hàng trước khi join thay vì sau. Và optimizer còn **suy ra**
thêm một điều kiện không hề có trong câu SQL: `ev.kind < 5`. Vì `ev.kind = dim.kind` và
`dim.kind < 5`, nên chắc chắn `ev.kind < 5`. Phase 8 của minidb đo được riêng luật suy ra này làm
câu truy vấn nhanh hơn **18.53 lần**.

Postgres có làm vậy không? Chạy câu tương tự trên 1 triệu hàng:

```text
 Nested Loop
   ->  Index Scan using dim_pkey on dim
         Index Cond: (kind < 5)
   ->  Bitmap Heap Scan on ev
         Recheck Cond: (kind = dim.kind)
         ->  Bitmap Index Scan on ev_kind
               Index Cond: (kind = dim.kind)
```

**Không.** Không có dòng `ev.kind < 5` nào. Postgres chỉ suy ra điều kiện qua các phép **bằng**
(`a = b AND b = 5` thì `a = 5`), không suy ra qua bất đẳng thức. Nó bù lại bằng cách chọn nested
loop: lấy 5 hàng `dim` qua index, rồi với mỗi hàng tra `ev` qua index `ev_kind`. Kết quả vẫn nhanh,
nhưng bằng một đường khác. Đọc `EXPLAIN` của hai DB cạnh nhau, bạn thấy được cả những việc
optimizer **không** làm.

## Đọc những con số bên cạnh

Dữ liệu: `ev` 1 triệu hàng, `dim` 1000 hàng ([`blog/lab/10-explain-pg.sql`](lab/10-explain-pg.sql)).

### Hash Join: build và probe

```text
   ->  Hash Join (actual rows=298880 loops=1)
         Hash Cond: (ev.kind = dim.kind)
         ->  Seq Scan on ev (actual rows=298880 loops=1)       ← PROBE: chảy qua từng hàng
               Filter: (amount < 300)
               Rows Removed by Filter: 701120
         ->  Hash (actual rows=1000 loops=1)                   ← BUILD: đọc HẾT trước, dựng bảng băm
               Buckets: 1024  Batches: 1  Memory Usage: 55kB
               ->  Seq Scan on dim (actual rows=1000 loops=1)
```

Hash join có hai vế. Vế **build** (dòng `Hash`) phải được đọc **hết** vào một bảng băm trong bộ nhớ
trước khi có hàng kết quả nào. Sau đó vế **probe** chảy qua từng hàng một, tra bảng băm. Planner
luôn muốn bảng **nhỏ** ở vế build: ở đây là `dim` 1000 hàng, chỉ tốn 55kB.

`Batches: 1` nghĩa là bảng băm vừa trong `work_mem`. Nếu vế build quá lớn thì sao? Ép một hash join
tự nối `ev` với chính nó (vế build là 500000 hàng), với hai mức `work_mem`:

```text
work_mem = 1MB     Buckets: 65536   Batches: 16  Memory Usage: 1615kB    temp read=4131 written=4131
                   Execution Time: 291.194 / 267.840 / 273.814 ms      (3 lượt)
work_mem = 256MB   Buckets: 524288  Batches: 1   Memory Usage: 21657kB
                   Execution Time: 343.816 / 346.164 / 366.124 ms
```

Với 1MB, Postgres chia cả hai vế thành 16 phần (*batch*) theo giá trị băm, ghi ra file tạm (4131
page), rồi join từng cặp phần một. Kỹ thuật này tên là **Grace hash join**, và minidb cũng cài nó.

Điều bất ngờ là **bản tràn ra đĩa lại nhanh hơn** bản chạy hoàn toàn trong RAM, 1.2–1.3 lần, lặp
lại ổn định qua 3 lượt. Mình chưa chứng minh được vì sao. Một giải thích hợp lý là bảng băm 21.6MB
lớn hơn L3 cache của CPU (12MB), nên mỗi lần tra là một lần trượt cache; còn mỗi batch chỉ 1.6MB,
vừa trong cache. Trong thí nghiệm này, file tạm lại nằm trong page cache của hệ điều hành nên gần
như không tốn I/O thật. Bài học: **"tràn ra đĩa" không tự động có nghĩa là chậm.** Phải đo.

### Sort: toán tử chặn

```text
work_mem = 64kB    Sort Method: external merge  Disk: 8488kB     Execution Time: 602.843 ms
work_mem = 256MB   Sort Method: quicksort  Memory: 23957kB       Execution Time: 591.549 ms
```

`external merge` nghĩa là dữ liệu không vừa bộ nhớ: Postgres sắp từng đoạn, ghi ra đĩa, rồi trộn
các đoạn lại (*external merge sort*). Ở đây nó chỉ chậm hơn 2%, vì cùng lý do như trên: file tạm
nằm trong page cache.

Điều quan trọng hơn về `Sort` là: nó là một toán tử **chặn** (*blocking*). Nó không thể đưa lên hàng
đầu tiên cho tới khi đã đọc **hết** hàng từ con, vì hàng nhỏ nhất có thể nằm ở cuối. Đây là đoạn
`Next()` của sort trong minidb:

```go
func (s *sortOp) Next() (bool, error) {
	if !s.opened {
		if err := s.open(); err != nil { // open() đọc HẾT đầu vào, sắp, tràn ra đĩa nếu cần
			return false, err
		}
		s.opened = true
	}
	...
}
```

Lần gọi `Next()` đầu tiên phải đọc cả bảng. Hệ quả thấy rõ nhất khi có `LIMIT`:

```text
== ORDER BY kind LIMIT 10   (kind CÓ index)
 Limit (actual rows=10 loops=1)
   ->  Index Scan using ev_kind on ev (actual rows=10 loops=1)          ← đọc đúng 10 hàng
 Execution Time: 0.337 ms

== ORDER BY amount LIMIT 10   (amount KHÔNG có index)
 Limit (actual rows=10 loops=1)
   ->  Sort (actual rows=10 loops=1)
         Sort Method: top-N heapsort  Memory: 26kB
         ->  Seq Scan on ev (actual rows=1000000 loops=1)               ← đọc 1 triệu hàng để lấy 10
 Execution Time: 74.649 ms
```

Cùng `LIMIT 10`, chênh nhau **221 lần**. Có index trên cột `ORDER BY` thì index đã cho sẵn thứ tự,
không cần `Sort`, và `Limit` dừng sau 10 hàng. Không có index thì `Sort` phải đọc cả 1 triệu hàng
mới biết 10 hàng nhỏ nhất là những hàng nào. (Postgres còn khéo ở chỗ dùng `top-N heapsort`: chỉ
giữ 10 hàng tốt nhất trong một heap 26kB, thay vì sắp cả triệu hàng.) Phase 8 của minidb đo được
đúng hiện tượng này: bỏ được `Sort` nhờ index nhanh hơn 3 lần khi không có `LIMIT`, và hơn **3000
lần** khi có `LIMIT 10`.

## Bảng tra nhanh

| Bạn thấy | Nghĩa là | Để ý gì |
|---|---|---|
| `Seq Scan` / `type: ALL` | đọc mọi page của bảng | bình thường nếu lấy nhiều hơn khoảng 5% bảng (bài 8) |
| `Index Scan` | đi xuống index, tra từng hàng | đắt theo **số hàng**, xem `Buffers` |
| `Index Only Scan` / `Using index` | mọi cột cần đều có trong index, không phải tra bảng | nhanh nhất |
| `Bitmap Heap Scan` | gom địa chỉ từ index, sắp theo page, rồi đọc bảng | tốt cho khoảng 1–30% bảng |
| `Nested Loop` | với mỗi hàng bên ngoài, tìm hàng khớp ở bên trong | tốt khi bên ngoài **ít** hàng. Ước lượng sai là thảm hoạ (bài 9) |
| `Hash Join` | dựng bảng băm từ vế nhỏ, rồi cho vế lớn chảy qua | `Batches > 1` = tràn ra đĩa |
| `Merge Join` | hai vế đã sắp sẵn, đi song song | cần cả hai vế có thứ tự |
| `Sort` | toán tử **chặn**: đọc hết rồi mới trả hàng đầu | `external merge Disk` = tràn ra đĩa; có `LIMIT` thì cân nhắc index |
| `rows=` vs `actual rows=` | ước lượng vs thực tế | lệch từ 10 lần trở lên = planner đoán mò (bài 9) |
| `loops=N` | node này chạy N lần | số `actual` là **trung bình mỗi lần**, nhân với `loops` mới ra tổng |

## Mang về dùng

1. **Dùng `EXPLAIN (ANALYZE, BUFFERS)`** (Postgres) hoặc `EXPLAIN ANALYZE` (MySQL 8), đừng dùng
   `EXPLAIN` trơn. `EXPLAIN` trơn chỉ in ra dự định; `ANALYZE` chạy thật và in ra những gì đã xảy ra.
2. **Đọc từ lá lên gốc**, và ở mỗi node hỏi hai câu: nó chạm bao nhiêu page (`Buffers`), và ước
   lượng có khớp thực tế không (`rows` vs `actual rows`).
3. **`ORDER BY … LIMIT` trên bảng lớn thì cần index trên cột `ORDER BY`.** Không có thì `Sort` phải
   đọc cả bảng, dù bạn chỉ cần 10 hàng.
4. **`work_mem` là cho mỗi toán tử, không phải cho mỗi câu truy vấn.** Một câu có 3 `Sort` và 2
   `Hash` có thể dùng 5 lần `work_mem`, nhân thêm với số kết nối. Tăng nó thì đo trước, và như
   thí nghiệm trên cho thấy, to hơn chưa chắc đã nhanh hơn.
5. **Muốn thấy optimizer đã viết lại câu truy vấn ra sao**, MySQL có `optimizer_trace`
   (`SET optimizer_trace = 'enabled=on'`, rồi đọc `information_schema.OPTIMIZER_TRACE`). Postgres
   chỉ có các tham số `debug_print_rewritten` / `debug_print_plan`, ghi cây nội bộ ra log dưới
   dạng rất khó đọc. minidb in thẳng ra trong `EXPLAIN`.

---

Script: [`blog/lab/10-explain-pg.sql`](lab/10-explain-pg.sql) · executor của minidb:
[`internal/exec/`](../internal/exec), optimizer: [`internal/plan/`](../internal/plan) · nhật ký:
[`diary/phase8.md`](../diary/phase8.md).

**Bài tiếp theo:** [Bài 11 — Bản đồ mang theo](11-ban-do-mang-theo.md)
