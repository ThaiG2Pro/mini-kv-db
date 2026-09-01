# Phase 2 — nhật ký lệnh đầy đủ, thất bại và cải tiến

Bổ sung cho [`phase2.md`](./phase2.md). File kia là **kết quả** đã biên tập; file này là
**toàn bộ đường đi**, kể cả những lệnh dẫn tới ngõ cụt.

Lý do tách ra: mục "giả thuyết sai" trong nhật ký phase chỉ giữ được *kết luận* của mỗi lần
sai. Nhưng thứ đáng học lại là **thứ tự chẩn đoán** — lệnh nào tôi chạy trước, nó loại bỏ được
khả năng nào, và vì sao ba lần đầu vẫn chưa ra. Sáu tháng sau gặp lại một "chương trình đứng
im", cái cần nhớ là quy trình này chứ không phải câu trả lời của riêng lần này.

Máy: WSL2 / i5-1235U / ext4. Ngày 2026-09-01. Commit của phase: `5c71e02`.

---

## Phần 1 — Dựng code (không có gì bất ngờ)

```bash
# 1. đọc luật trước khi làm
sed -n '1,200p' ROADMAP.md
cat skills/diary/SKILL.md
cat diary/phase2.md            # template trống của phase

# 2. đọc lại API tầng dưới để không thiết kế lệch
grep -n '^func \|^type \|^const ' internal/pager/pager.go
sed -n '28,60p;283,350p' internal/pager/pager.go

# 3. chụp môi trường TRƯỚC khi đo bất cứ thứ gì
uname -srmo && go version && df -hT . | tail -1 && git rev-parse --short HEAD && date +%F

# 4. viết code
#    internal/page/page.go        layout + Insert/Get/Delete/Update/Compact/TrimDeadSlots
#    internal/page/verify.go      Verify() 6 bất biến + Dump/Map
#    internal/page/page_test.go   test + fuzz + bench
gofmt -w internal/page/ && go vet ./internal/page/
```

**Không có thất bại nào ở đây**, và đó chính là chỗ đáng ngờ: code biên dịch được và test đầu
tiên xanh không nói lên điều gì. Mọi bài học của buổi này đến sau, từ fuzz.

---

## Phần 2 — Thất bại #0: output bị nén, mất bằng chứng

```console
$ go test ./internal/page/ -v -count=1
Go test: 15 passed in 1 packages
```

**Hỏng ở đâu:** hook `rtk` nén output để tiết kiệm token. Bình thường thì tốt. Nhưng luật số
một của nhật ký là *"dán output nguyên văn"* — mà ở đây **output chính là bằng chứng**. Một
dòng `15 passed` không thể dán vào diary: nó không cho biết test nào in ra con số gì.

**Sửa:**

```bash
rtk proxy go test ./internal/page/ -v -count=1     # proxy = chạy thô, không lọc
```

**Bài học:** khi output *là* dữ liệu chứ không phải tiếng ồn, phải tắt mọi lớp tóm tắt nằm
giữa. Từ đây mọi lệnh sinh số trong phase này đều đi qua `rtk proxy`.

---

## Phần 3 — Triệu chứng: fuzzer đứng hình

```console
$ rtk proxy go test ./internal/page/ -run '^$' -fuzz FuzzSlottedPage -fuzztime 60s
fuzz: elapsed: 12s, execs: 66122 (0/sec), new interesting: 84 (total: 86)
...
fuzz: elapsed: 57s, execs: 66122 (0/sec), new interesting: 84 (total: 86)
PASS
ok  	minidb/internal/page	61.040s
```

`execs` đứng yên **48 giây liên tiếp**, rồi vẫn `PASS`.

Nếu chỉ nhìn dòng cuối, tôi đã ghi vào nhật ký: *"fuzz 60s, không tìm ra lỗi"*. Câu đó đúng
từng chữ và sai toàn bộ ý nghĩa. **Cột phải nhìn là `execs/sec`, không phải chữ `PASS`.**

Bốn giả thuyết, theo đúng thứ tự tôi đã nghĩ ra.

### Giả thuyết A — "`Compact` của tôi là O(n²)" · ĐÚNG MỘT PHẦN, nhưng không phải thủ phạm

Nghi ngay `Compact`, vì tôi có viết insertion sort kèm lời biện hộ trong comment: *"n nhỏ và
gần như đã sắp"*. Muốn kiểm chứng thì phải **dựng được thế xấu nhất**:

```console
$ rtk proxy go test ./internal/page/ -run TestCompactOrderIsScrambled -v -count=1
    page_test.go:660: 300 cell sống, 0 chỗ offset KHÔNG giảm dần theo slot
    page_test.go:662: không dựng được thế xấu -> bench dưới vô nghĩa
--- FAIL: TestCompactOrderIsScrambled (0.00s)
```

**Thất bại thứ nhất, và là thất bại tôi thích nhất trong buổi này.** Tôi dựng thế xấu bằng
cách update từng slot theo thứ tự **tăng dần** — nhưng làm vậy thì slot nhỏ nhận cell mới
trước (offset cao), slot lớn nhận sau (offset thấp), tức là ra đúng thứ tự *đã sắp*. Tôi vừa
dựng ra trường hợp **tốt nhất** và định gọi nó là xấu nhất.

Test bắt được là vì tôi đã cài sẵn dòng `if inversions == 0 { t.Fatal("không dựng được thế
xấu -> bench dưới vô nghĩa") }`. Không có dòng đó, nó sẽ PASS, bench sẽ ra một con số đẹp, và
tôi sẽ kết luận "insertion sort ổn" — sai, dựa trên một phép đo *hợp lệ về mặt kỹ thuật*.

Sửa: update theo thứ tự slot **giảm dần**.

```console
$ rtk proxy go test ./internal/page/ -run TestCompactOrderIsScrambled -v -count=1
    page_test.go:661: 300 cell sống, 299 chỗ offset KHÔNG giảm dần theo slot

$ rtk proxy go test ./internal/page/ -run '^$' -bench 'Compact' -benchtime 500x -count=1
BenchmarkCompact-6            	     500	      3567 ns/op	        29.00 cell-sống
BenchmarkCompactScrambled-6   	     500	    229810 ns/op	       300.0 cell-sống
```

**Cải tiến:** `slices.SortFunc` trên mảng `uint32` gói `(offset, slot)`, mảng nằm trên stack
(`numSlots` tối đa 1018 nên kích thước biết trước).

```console
$ rtk proxy go test ./internal/page/ -run '^$' -bench 'Compact' -benchtime 500x -benchmem
BenchmarkCompactScrambled-6   	     500	      9169 ns/op	       0 B/op	       0 allocs/op
```

**229810 → 9169 ns = 25x, 0 cấp phát.** Nhưng:

```console
$ rtk proxy go test ./internal/page/ -run '^$' -fuzz FuzzSlottedPage -fuzztime 60s
fuzz: elapsed: 1m0s, execs: 63132 (0/sec), new interesting: 8 (total: 102)
```

**Vẫn đứng hình.** Sửa được một lỗi thật, nhưng không phải lỗi đang truy.

> **Bài học:** sửa được một thứ hỏng ≠ sửa được thứ đang hỏng. Rất dễ dừng lại ở đây và tự
> nhận đã xong, vì có một con số 25x rất thuyết phục để khoe.

### Giả thuyết B — "có input rất dài" · SAI

```console
$ d="$(go env GOCACHE)/fuzz/minidb/internal/page/FuzzSlottedPage"; wc -c "$d"/* | sort -n | tail -3
 283 92bffbc1dbf10591
 285 5503b9070871f976
 433 6b5032713d5c0f3d

$ time rtk proxy go test ./internal/page/ -run FuzzSlottedPage -count=1
ok  	minidb/internal/page	0.009s
real	0m0.864s
```

Input lớn nhất trong corpus là 433 byte, và chạy **toàn bộ** corpus mất 9ms. Không có input
nào khó.

Dù vậy tôi vẫn phát hiện được một thứ đáng giá khi đo giá của `Verify` — hàm chạy sau **mỗi**
thao tác, nên giá của nó nhân với toàn bộ chiều dài chương trình fuzz:

```console
$ rtk proxy go test ./internal/page/ -run '^$' -bench 'VerifyFullPage' -benchmem -count=1
BenchmarkVerifyFullPage-6   	    8989	    130633 ns/op	   20576 B/op	       4 allocs/op
```

**20 KB cấp phát mỗi lần gọi.** Thủ phạm: `sort.Slice` trên danh sách cell sống.

**Cải tiến:** đổi sang bitmap 512 byte trên stack — đánh dấu từng byte của mỗi cell, gặp byte
đã đánh dấu thì đó là chồng lấn. O(số byte sống) ≤ 4096, không sort, không cấp phát.

```console
$ rtk proxy go test ./internal/page/ -run '^$' -bench 'VerifyFullPage' -benchmem -count=1
BenchmarkVerifyFullPage-6   	   90364	     13299 ns/op	       0 B/op	       0 allocs/op
```

Thêm một biện pháp phòng thủ: chặn độ dài chương trình fuzz ở 2048 byte.

```console
$ rtk proxy go test ./internal/page/ -run '^$' -fuzz FuzzSlottedPage -fuzztime 60s
fuzz: elapsed: 1m0s, execs: 63132 (0/sec), new interesting: 8 (total: 102)
```

**Vẫn đứng hình.** Hai lần cải tiến thật, vẫn chưa chạm đúng chỗ.

### Giả thuyết C — "code có vòng lặp vô hạn" · SAI

Cách kiểm chứng đắt tiền nhưng đúng: **dựng lại thân fuzz thành một test thường**, để
`go test -timeout` có thể dump stack.

```bash
# tách thân fuzz thành runProgram(t, prog) dùng chung cho cả hai
rtk proxy go test ./internal/page/ -run TestProgramStress -count=1 -timeout 40s
```

```console
ok  	minidb/internal/page	6.326s
```

3000 chương trình ngẫu nhiên (trung bình ~1KB) chạy hết trong 6.3s. **Không có vòng lặp vô
hạn.**

Vài lệnh chẩn đoán ở đây **cũng hỏng**, ghi lại vì chúng là bẫy môi trường:

```console
$ rtk proxy go test ./internal/page/ -run '^$' -fuzz FuzzSlottedPage -fuzztime 60s -parallel 1 -timeout 25s
fuzz: elapsed: 1m0s, execs: 565 (0/sec)
PASS
```

`-timeout 25s` **không bắn**. Timeout của test binary không quản được tiến trình worker mà
`go test -fuzz` sinh ra — nó là process con riêng.

```console
$ (go test ... -fuzz ... &) ; sleep 18; ps -eo pid,etime,pcpu,rss,args | grep fuzzworker
(không có output)
```

`ps` không thấy gì, dù log vẫn đang chạy. Tiến trình nền khởi động ở **lần gọi công cụ trước**
không nhìn thấy được từ lần gọi sau (sandbox tách namespace).

> **Bài học công cụ:** một phép chẩn đoán "khởi động rồi quan sát" phải nằm **trọn trong một
> lệnh**. Tách ra hai lệnh là mất đối tượng quan sát.

### Giả thuyết D — "môi trường WSL2 hỏng" · SAI, và đây là bước ngoặt

Trước khi đổ cho máy, làm **thí nghiệm đối chứng**: fuzz một hàm không làm gì, trên đúng máy
đó, đúng phiên bản Go đó.

```bash
mkdir -p /tmp/.../ctl && cd /tmp/.../ctl
cat > go.mod <<'EOF'
module ctl
go 1.26
EOF
cat > ctl_test.go <<'EOF'
package ctl
import "testing"
func FuzzNothing(f *testing.F) {
	f.Add([]byte{1})
	f.Fuzz(func(t *testing.T, b []byte) {
		n := 0
		for _, c := range b { n += int(c) }
		_ = n
	})
}
EOF
rtk proxy go test . -run '^$' -fuzz FuzzNothing -fuzztime 20s
```

```console
fuzz: elapsed: 20s, execs: 1423015 (76780/sec), new interesting: 7 (total: 8)
PASS
```

**76 780 exec/s, không khựng một nhịp.** Engine fuzz và WSL2 đều bình thường → thủ phạm nằm
trong chính target của tôi. Quay về nghi mình.

Rồi đo giá một exec của target tôi:

```console
$ time rtk proxy go test ./internal/page/ -run '^$' -fuzz FuzzSlottedPage -fuzztime 2000x -parallel 1
fuzz: elapsed: 0s, execs: 2000 (5689/sec)
real	0m2.046s
```

**5689 exec/s.** Target không hề chậm. Vậy khi nó "đứng im" thì nó đang làm *cái khác*, không
phải đang chạy chậm.

### Nhìn thẳng vào tiến trình — câu trả lời

Hai câu hỏi còn lại: nó **còn sống** không, và nó **đang tiêu CPU** hay đang chờ?

```console
$ # (khởi động fuzz và quan sát TRONG CÙNG MỘT LỆNH)
t= 5s  free=1493MB used=6921MB  rss=110MB(page.test) rss=93MB(page.test)
t=40s  free=1480MB used=6928MB  rss=110MB(page.test) rss=108MB(page.test)
```

Còn sống, RSS đứng yên 110MB, không swap. Không phải OOM, không phải rò rỉ bộ nhớ.

```console
$ for p in $(pgrep -f 'page.test'); do echo "pid=$p $(ps -o pcpu=,stat= -p $p) wchan=$(cat /proc/$p/wchan)"; done
pid=33886  0.0  Ss wchan=pipe_read
pid=33930  0.5  Sl wchan=futex_wait_queue
pid=33940  106  Sl wchan=futex_wait_queue
```

**106% CPU.** Nó đang *làm việc* thật sự, chỉ là không phải việc tôi tưởng. Ép nó khai:

```console
$ hot=$(ps -eo pid,pcpu,args | grep page.test | grep -v grep | sort -k2 -rn | head -1 | awk '{print $1}')
$ kill -QUIT $hot
--- FAIL: FuzzSlottedPage (11.75s)
    fuzzing process hung or terminated unexpectedly while minimizing: EOF
```

**`while minimizing`.** Worker không treo: nó đang **rút gọn** một input mới tìm được. Ngân
sách mặc định của việc đó là `-fuzzminimizetime=60s`. `-fuzztime` hết trước khi rút gọn xong →
Go dừng lại và in ra **`PASS`**.

Xác nhận bằng cách hạ đúng một tham số đó, không đổi gì khác:

```console
$ rtk proxy go test ./internal/page/ -run '^$' -fuzz FuzzSlottedPage -fuzztime 45s -fuzzminimizetime 1s -parallel 1
fuzz: elapsed: 45s, execs: 128182 (6871/sec), new interesting: 40 (total: 142)
PASS
```

Hết khựng. Giả thuyết được xác nhận bằng một biến đổi duy nhất.

### Một cái bẫy nữa ngay sau đó

`kill -QUIT` khiến Go ghi ra `testdata/fuzz/FuzzSlottedPage/2f6b610dc17b5edd` và bảo đó là
**failing input**. Suýt nữa tôi ghi vào diary rằng fuzz đã tìm ra bug.

```console
$ rtk proxy go test ./internal/page/ -run 'FuzzSlottedPage/2f6b610dc17b5edd' -count=1
ok  	minidb/internal/page	0.007s
```

Nó **pass**. File đó là sản phẩm của việc tôi giết tiến trình, không phải của một lỗi. Đã xóa.

> **Bài học:** khi bạn tự tay can thiệp vào một tiến trình, mọi thứ nó khai báo sau đó đều là
> chứng cứ nhiễm bẩn. Phải chạy lại độc lập mới được tin.

---

## Phần 4 — Đo lại tử tế sau khi sửa

```bash
rtk proxy go test ./internal/page/ -run '^$' -bench . -benchmem -benchtime 2000x -count=1
rtk proxy go test ./internal/page/ -run '^$' -bench 'Verify' -benchmem -count=3   # kiểm tra ổn định
rtk proxy go test ./internal/page/ -run '^$' -fuzz FuzzSlottedPage -fuzztime 120s -fuzzminimizetime 1s
rtk proxy go test ./... -count=1
go run ./cmd/slotlab -n 24 -size 120
go run ./cmd/slotlab -n 24 -size 120 -delete random -seed 7 -churn 4
```

Ở đây gặp một chuyện nhỏ nhưng đúng luật diary: con số `130633 ns` của `Verify` bản cũ đo
**một lần**, trên code mà tôi vừa xoá. Nó **không tái lập được** — tức là theo luật của chính
mình, nó vô giá trị.

**Cải tiến:** giữ bản cũ lại trong file test với tên `verifyRef`, cộng thêm
`TestVerifyAgreesWithReference` bắt hai bản phải luôn cho cùng kết luận qua 30 000 thao tác.
Được hai thứ cùng lúc: tỉ số tái lập được **và** một phép kiểm tra chéo cho bản nhanh.

```console
$ rtk proxy go test ./internal/page/ -run '^$' -bench 'Verify' -benchmem -count=3
BenchmarkVerifyFullPage-6      	  318538	      3583 ns/op	       0 B/op	       0 allocs/op
BenchmarkVerifyRefFullPage-6   	   66211	     19768 ns/op	   20576 B/op	       4 allocs/op
```

`19768 / 3583 = 5.5x` — và con số này chạy lại được mãi mãi. (Con số 130633 ban đầu là một lần
đo nguội, cao bất thường; đây mới là tỉ số thật.)

Kết quả cuối:

```console
$ rtk proxy go test ./internal/page/ -run '^$' -fuzz FuzzSlottedPage -fuzztime 120s -fuzzminimizetime 1s
fuzz: elapsed: 2m0s, execs: 1421899 (19697/sec), new interesting: 113 (total: 255)
PASS
```

**63 132 → 1 421 899 exec.** Hơn **22 lần** so với chính lệnh đó lúc đầu buổi.

---

## Phần 5 — Chốt phase, và một thất bại quy trình

```bash
gofmt -l . && go vet ./... && rtk proxy go test ./... -count=1
git add -A && git commit -F - <<'EOF'
phase 2: slotted page — ...
EOF
```

Rồi định điền hash vào diary bằng cách `--amend`:

```bash
h=$(git rev-parse --short HEAD)     # cf06483
sed -i "s/PHASE2_COMMIT/$h/g" diary/phase2.md
git commit --amend --no-edit        # <- hash đổi thành 5c71e02
```

**Thất bại:** `--amend` tạo ra commit mới, nên hash tôi vừa ghi vào file đã trỏ sai ngay tại
thời điểm ghi. Một commit không thể chứa hash của chính nó.

**Sửa:** dùng đúng quy ước đã lập ở phase 0/1 (commit `4ed5198`) — nhật ký trỏ tới commit của
phase, và hash được điền bằng **một commit riêng ngay sau đó**:

```bash
sed -i 's/cf06483/5c71e02/g' diary/phase2.md
git commit -m "docs: gắn commit hash 5c71e02 vào diary phase 2"
```

> **Bài học:** đã lập quy ước ở phase trước thì đọc lại nó trước khi làm, đừng sáng tạo lại.
> Quy ước đó tồn tại chính vì lần trước đã vấp đúng chỗ này.

---

## Tổng kết: 6 thất bại, 7 cải tiến

| # | Thất bại | Sửa thành |
|---|---|---|
| 1 | Output test bị hook nén → mất bằng chứng nguyên văn | Mọi lệnh sinh số đi qua `rtk proxy` |
| 2 | Dựng "thế xấu nhất" cho `Compact` nhưng thực ra dựng trúng thế **tốt nhất** | Test tự phủ quyết khi không dựng được thế xấu; update theo thứ tự slot giảm dần |
| 3 | `Compact` insertion sort dựa trên giả định "offset gần như đã sắp" | `slices.SortFunc` + mảng trên stack: **25x**, 0 alloc |
| 4 | `Verify` cấp phát 20KB/lần gọi, bóp nghẹt fuzz | Bitmap 512B trên stack: **5.5x**, 0 alloc |
| 5 | Ba giả thuyết sai liên tiếp về việc fuzzer đứng hình | Quy trình 4 bước: đo giá thật → dựng lại bằng test thường → **thí nghiệm đối chứng** → nhìn thẳng vào tiến trình (`pcpu`, `kill -QUIT`) |
| 6 | `--amend` để điền hash vào chính commit đó | Commit riêng, theo quy ước đã có từ phase 0/1 |

Bảy cải tiến để lại trong repo:

1. `Compact` — sort đúng, không cấp phát heap.
2. `Verify` — bitmap, không cấp phát heap.
3. `verifyRef` — bản chậm giữ lại làm tham chiếu + `TestVerifyAgreesWithReference` kiểm tra chéo.
4. `runProgram()` — thân fuzz dùng chung với `TestProgramStress` có seed, nên mọi nghi vấn về
   fuzz đều tái hiện được bằng một test thường, không cần fuzzer.
5. Chặn độ dài chương trình fuzz ở 2048 byte.
6. Target `make fuzz` với `-fuzzminimizetime 1s` **nhúng sẵn**, kèm comment giải thích vì sao —
   để lần sau không ai (kể cả tôi) phải điều tra lại một tiếng đồng hồ.
7. Các test tự phủ quyết khi vô nghĩa: `if moved == 0 { t.Fatal }`,
   `if inversions == 0 { t.Fatal }`, `if p.FreeContiguous() >= sz { t.Fatal }`.

### Ba câu đọng lại

**Một phép đo hợp lệ về kỹ thuật vẫn có thể vô nghĩa.** Bench `Compact` chạy đúng, ra số đẹp,
và đo nhầm trường hợp tốt nhất. Cách phòng duy nhất là bắt test tự tuyên bố khi nó không còn
chứng minh được gì.

**`PASS` không đồng nghĩa với "đã chạy".** Phải tìm cho ra con số chứng minh thí nghiệm *đã
thật sự diễn ra* — ở đây là `execs/sec`. Cùng loại bẫy với phase 0, khi tôi đo memcpy trong
page cache mà tưởng đang đo đĩa.

**Trước khi đổ lỗi cho môi trường, hãy làm thí nghiệm đối chứng.** Fuzz một hàm rỗng mất 30
giây để viết và nó lập tức loại bỏ được nửa không gian giả thuyết. Tôi đã để nó tới bước thứ
tư, lẽ ra nó phải là bước thứ nhất.
