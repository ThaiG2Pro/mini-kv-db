# Phase 7 — Secondary index & query cơ bản

- **Thời lượng dự kiến:** 2-3 ngày · **thực tế:** 2 buổi
- **Bắt đầu:** 2026-09-02 · **Kết thúc:** 2026-09-03
- **Trạng thái:** ✅ xong
- **Commit:** `_(git rev-parse --short HEAD tại lúc chốt phase)_`

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

Máy: 12th Gen Intel Core i5-1235U, 12 luồng logic (`cpu:` dòng của mọi output `go test -bench`
bên dưới). WSL2 — **không** phải Linux thuần, nên mọi số tuyệt đối chỉ có nghĩa **so với nhau**
trong cùng một bảng (nợ P0-1/P0-4).

## Mục tiêu phase

Secondary index trỏ về primary key, composite key giữ thứ tự, iterator model, và một planner
chọn giữa index scan / index-only scan / seq scan bằng một mô hình chi phí **đo được**.

## Câu hỏi phải trả lời được khi xong

- Secondary index lưu gì ở leaf? Vì sao không lưu thẳng cả row?
- Covering index tiết kiệm được gì (đếm theo số I/O)?
- Left-most prefix: vì sao index `(a,b)` vô dụng với `WHERE b = ?`
- Selectivity bao nhiêu thì seq scan thắng index scan **trên máy tôi**? Vì sao?
- Volcano model: chi phí ẩn của nó là gì (gợi ý: vì sao có vectorized execution)?

Trả lời ở mục [Rút ra](#rút-ra-viết-như-thể-giải-thích-cho-người-khác).

## Deliverable (bằng chứng đã hiểu)

1. `make idxlab-breakeven` — cùng một truy vấn, **ba** đường đi, chín độ chọn lọc, và **điểm hoà
   vốn nội suy từ chính số đo**. Bảng còn hai cột planner (mô hình đo / mô hình đoán) để thấy một
   hằng số lệch làm planner chọn sai ở đúng dải nào.
2. `make test-index` — ba kế hoạch phải cho **cùng** kết quả. Một planner đổi kết quả là một
   database sai, không phải một database chậm.
3. `make fuzz-keys` — bất biến định nghĩa của bộ mã hoá khóa: `thứ tự byte == thứ tự logic`.
4. Bài **phản chứng của phase này**: tắt cơ chế số đời cấu trúc thì bài test nào đỏ, bài test nào
   vẫn xanh. Kết quả của nó làm lộ một lỗ phủ và sinh ra thêm một bài test.

## Reproduce toàn bộ phase này

```bash
# Deliverable
make idxlab                 # 5 bảng: hoà vốn, thuế ghi, hình byte của khóa, ước lượng, LIMIT
make idxlab-breakeven       # chỉ bảng 1, 50000 hàng
make idxlab-bytes           # không cần database: hình byte của khóa composite

# Đối chứng (phải xanh) và số đo
make test-index
make bench-index
make fuzz-keys              # 2 x 120s
make fuzz-table             # 120s, ~3 exec/s vì mỗi exec có crash + mở lại

# Bài phản chứng: PHẢI đỏ. Tắt cơ chế số đời cấu trúc rồi chạy lại.
sed -i 's/if c.gen != c.t.gen {/if false {/' internal/btree/cursor.go
go test ./internal/btree/ ./internal/db/ ./internal/table/ -count=1 \
    -run 'CursorRestores|Iter|IndexScanSurvivesWriterMidScan'
sed -i 's/if false {/if c.gen != c.t.gen {/' internal/btree/cursor.go  # phép nghịch — an toàn cả khi cursor.go chưa commit

# Phase 5 và 6 không được hồi quy
go run ./cmd/crashlab -n 20
make crashlab-nowrite       # PHẢI đỏ
make txnlab-anomaly
```

---

## Nhật ký

### 2026-09-02 — trước khi viết dòng nào: nợ P6-4 là CỬA VÀO, không phải món xa xỉ

Định nghĩa của index scan: *duyệt index theo khoảng, mỗi mục lấy primary key rồi đi tra bảng.*
Viết ra thành hình thì nó là **một phép truy cây nằm TRONG callback của một phép duyệt cây**.

Còn `db.DB.Range` của phase 5 thì giữ `d.mu` **suốt** lần duyệt. Nên phép hình trên là **tự khoá
chết**, không phải chậm — chết.

Đó là lúc món nợ P6-4 đổi loại. Sổ nợ ghi nó là "🔧 code, tối ưu": `Txn.Scan` gom hết vào RAM rồi
mới gọi `fn`, tốn RAM và không có `LIMIT`. Sự thật là **phase 7 không tồn tại được** trước khi nó
được trả. Một món nợ ghi là "tối ưu" hoá ra là "chặn cả phase sau".

Và sổ nợ còn ghi thêm một câu, chính câu đó là chỗ tôi đã nghĩ sai:

> **Cách trả:** cần **latch-coupling (P4-5)** trước.

**Đang nghĩ gì:** nếu câu ấy đúng thì phase 7 phải bắt đầu bằng latch-coupling, mà latch-coupling
thì kéo theo nhiều writer vật lý, mà nhiều writer vật lý thì (kết luận của phase 6) buộc **bỏ pha
undo physical** của phase 5. Tức là để làm secondary index tôi phải viết lại phase 5. Tỉ lệ đó
sai tới mức phải nghi cái tiền đề.

### 2026-09-02 — cái latch phase 4 phải giữ được tháo bằng một cơ chế của PHASE 6

Nghĩ lại từ đầu: **vì sao** `Range` phải giữ latch suốt lần duyệt?

Để cursor còn đúng. Cursor giữ `(page, slot)`; một writer làm split thì slot ấy trỏ vào chỗ khác,
và lần `Next()` sau trả về khóa sai — hoặc nhảy qua cả nửa leaf.

Nhưng đó là câu trả lời của **phase 4**, thời chưa có MVCC. Ở phase 6 thì **cái quyết định một
reader được thấy gì không còn là latch, mà là ảnh chụp**. Latch chỉ còn phải bảo vệ **một** thứ:
tính đúng của **vị trí** cursor. Và vị trí thì không cần latch để bảo vệ — nó cần một cách **biết
mình đã hết đúng**.

Nên đường đi là:

1. `btree.Tree.gen` — **số đời cấu trúc**, tăng ở **mọi** `Put`/`Delete`/đổi root.
2. `Cursor` chụp `gen` lúc lấy vị trí. Lệch ⇒ **đi lại từ root** tới khóa nhỏ nhất **lớn hơn hẳn
   khóa vừa trả về**.
3. Định vị bằng **khóa** — một giá trị logic — chứ không bằng `(page, slot)`, một địa chỉ vật lý.

Đây là *cursor restoration* của InnoDB và SQLite, và nó rẻ hơn latch-coupling (crabbing) đúng một
bậc về độ phức tạp: không có thứ tự latch nào để làm sai, không có deadlock nào để phát hiện.

Vì sao tăng `gen` ở **mọi** `Put` chứ không chỉ ở split/merge — ghi ra vì nó là loại lỗi im lặng:
`Put` ghi đè một khóa cũng có thể **compact** cả page (phase 2), và compact dồn lại mọi offset.
Slot index không đổi nhưng **khóa ở slot ấy** đổi. Đếm hẹp hơn thì đúng ở nhiều ca và **sai lặng
lẽ** ở vài ca — đúng cái loại bug mà phase 5 gọi là *hai nguồn sự thật*.

**Đọc kết quả:** P4-5 được trả **một nửa** — nửa **đọc**. Nửa **ghi** (nhiều writer cùng sửa cây)
cố ý để nguyên, vì lý lẽ của phase 6 vẫn còn nguyên giá trị.

**Đang nghĩ gì:** câu trong sổ nợ *"cần P4-5 trước"* sai, và nó sai theo một kiểu đáng ghi:
**tôi đã tìm cách trả một món nợ của phase 4 bằng công cụ của phase 4.** Cái tháo được nó là một
cơ chế của phase 6, ra đời sau khi món nợ được ghi. Sổ nợ nên ghi *hiện tượng*, đừng ghi *cách
trả*.

### 2026-09-02 — `Txn.Scan` thành merge join của hai dòng đã sắp

Bản cũ gom cả kết quả vào RAM. Bản mới là một **merge join** của hai dòng đã sắp xếp: cursor trên
cây, và **bản sao đã sắp của phần write set nằm trong khoảng**. Cùng khóa thì write set thắng —
đó là *read-your-own-writes*, và cũng là chỗ cài phép xóa.

Được ba thứ: RAM O(số khóa bẩn) thay vì O(số hàng khớp); callback **tái nhập được**; và hàng đầu
tiên ra sau ~một lần đi xuống cây, tức `LIMIT` bắt đầu có nghĩa.

Một chi tiết phải ghi vì nó là bẫy: `fn` được gọi **trước** `advance()`. Vì `advance` ghi lại
đúng hai buffer `tk`/`tv` mà `fn` đang cầm.

### 2026-09-02 — bộ mã hoá khóa: B+Tree biết đúng MỘT phép so

`internal/keys`. Tiền đề: cây chỉ biết `memcmp`. Nên **mọi** ngữ nghĩa — kiểu, nhiều cột, ASC/DESC,
NULL — phải đẩy vào **hình dạng byte**, tuyệt đối không đẩy vào comparator. Một comparator riêng
cho index là một cây thứ hai phải bảo trì.

Ba luật, cả ba đều được fuzz:

| Luật | Nghĩa |
|---|---|
| giữ thứ tự | `bytes.Compare(Encode(a), Encode(b)) == CompareTuple(a, b)` |
| canonical | giải mã được thì mã hoá lại phải ra **đúng** byte cũ |
| tự phân định | biết chỗ một field kết thúc mà **không** cần độ dài ở ngoài |

Ba quyết định và lý do:

- **byte tag CHÍNH LÀ thứ tự giữa các kiểu.** NULL `0x01` < false < true < int < uint < bytes. Không
  dùng `0x00` làm tag, nên một field không bao giờ bắt đầu bằng `0x00` — giữ nguyên khoảng
  metadata `0x00` mà `internal/txn` đã đặt trước từ phase 6 (bất biến liên package, có test riêng).
- **int64: đảo bit dấu.** Số âm phải đứng trước số dương trong `memcmp`.
- **bytes: escape `0x00 → 0x00 0xff`, kết bằng `0x00 0x00`.** Dấu kết nhỏ hơn **mọi** byte nội
  dung, nên tiền tố tự đứng trước mà **không** cần độ dài. Nếu lưu độ dài lên trước thì độ dài
  tham gia phép so và `"aa"` sẽ đứng **sau** `"b"` — một index sai thứ tự mà không bài test viết
  tay nào bắt được, chỉ fuzz bắt.
- **DESC = bù từng byte** của cả tag + payload. Bù thì đảo thứ tự **miễn phí**, và cùng phép bù ấy
  kéo NULL xuống cuối. Tức DESC ở đây là NULLS LAST **không phải do chọn mà do hình**.

Từ đó **quy tắc left-most prefix là một hệ quả, không phải một quy ước**: các cột là byte **nối
nhau**, nên chỉ một **tiền tố bên trái** mới là một **khoảng liên tục** của cây.

### 2026-09-02 — nhiều bảng trong một cây, và `Unique` quyết định hình dạng khóa

`internal/table`. Một B+Tree duy nhất, chia bằng tiền tố:

```
0x0a  catalog
0x0b  <be32 tableOID> <pk>            hàng
0x0c  <be32 indexOID> <cột index> [<pk>]   mục index
```

Ba hệ quả rơi ra, không phải ba tính năng phải làm:

- Quét cả bảng **chính là** một range scan trên `0x0b <oid>`.
- Dùng **oid** chứ không dùng tên ⇒ đổi tên bảng là **miễn phí** (đúng cái `relfilenode` của
  Postgres).
- Byte của index nằm **xa** byte của hàng ⇒ đó là gốc rễ vì sao một index scan độ chọn lọc thấp
  **thua**: nó nhảy qua nhảy lại giữa hai vùng.
- Hàng **nằm trong** index của primary key (kiểu InnoDB/SQLite, không phải heap kiểu Postgres) ⇒
  truy vấn theo pk không bao giờ cần secondary index, và seq scan trả về **theo thứ tự pk**.

Chỗ đáng nhất của cả package: `Unique` **đổi hình dạng khóa**, và từ hình dạng ấy mọi thứ khác rơi
ra.

- **non-unique BẮT BUỘC** phải nhét pk vào khóa: hai hàng cùng giá trị = hai mục.
- **unique BẮT BUỘC KHÔNG** được nhét: nếu nhét thì tính duy nhất bốc hơi.

Hệ quả: hai transaction chèn cùng một giá trị unique sẽ ghi **đúng một** khóa cây ⇒ **first-
committer-wins của phase 6 thực thi ràng buộc duy nhất MIỄN PHÍ**, không cần thêm một dòng nào.
Còn index non-unique thì **không bao giờ** xung đột được. Khẳng định cả hai chiều
(`TestUniqueIndexConflictsAcrossTxns`, `TestNonUniqueIndexNeverConflicts`) — vế thứ hai mới là vế
chứng minh vế thứ nhất không xanh nhờ may.

### 2026-09-02 — bài test tự treo hết 60 giây, và nó dạy một điều thật

```console
$ go test ./internal/db/ -run TestIterSurvivesWriterBetweenSteps -count=1
panic: test timed out after 1m0s
...
wal.(*Log).Flush(...)
```

Bài test ghi `key(2*step+1)` sau mỗi bước — một khóa **mới**, luôn nằm **phía trước** cursor. Nên
mỗi bước lại sinh thêm một bước. Stack chỉ vào `fsync`, tức là **chậm-nhưng-còn-sống**, không phải
deadlock.

**Đọc kết quả:** bug của bộ đo. Nhưng nó dạy đúng một điều về ngữ nghĩa mà tôi chưa viết ra ở đâu:
**một lần duyệt có nhả latch KHÔNG được hứa là hữu hạn** nếu writer cứ chèn vào phía trước nó.
Postgres gọi ca ấy là seq scan bị "ghi đuổi", và cách chặn duy nhất là **chốt chặn trên tại thời
điểm bắt đầu** — đúng cái mà một ảnh chụp MVCC làm.

**Đã sửa:** ghi đè `key(0)`, tức một khóa nằm **sau lưng** cursor. Và giữ nguyên đoạn lý lẽ trên
thành comment tại chỗ, vì nó là nội dung chứ không phải chú thích.

### 2026-09-02 — bài test của phase 6 đỏ ngẫu nhiên 1/6, và nó có TỪ TRƯỚC

`TestTransferInvariantPerLevel` đỏ, ở đúng một ô: `read-uncommitted`. Nghi ngay là phase 7 làm gãy
— `Txn.Scan` vừa bị viết lại. Kiểm bằng cách chạy ở HEAD của phase 6:

```console
$ git worktree add /tmp/p6check HEAD && cd /tmp/p6check
$ for i in 1 2 3 4 5 6; do go test ./internal/txn/ -run TestTransferInvariantPerLevel -count=1 | tail -1; done
ok / ok / FAIL / ok / ok / ok
```

**Đọc kết quả:** có từ trước. Không phải hồi quy.

Giả thuyết đầu (**SAI**): thiếu điểm nhường lượt nên hai transaction không kịp chồng nhau. Thêm
`runtime.Gosched()` giữa phần đọc và phần ghi của `transferOnce` → vẫn 2/8 đỏ. **Bác bỏ, và trả
lại nguyên trạng** — giữ một bản sửa vừa không sửa được gì vừa xáo trộn số đo của phase 6 thì tệ
hơn là không sửa.

Nguyên nhân thật, và nó đáng hơn bản sửa: **hai bất biến khác nhau có SỨC PHÁT HIỆN khác nhau.**

- *"Tổng số dư không đổi"* là bất biến **đối xứng**: một lượt chuyển cộng ở một chỗ, trừ ở chỗ
  khác, nên hai update bị mất có thể **triệt tiêu** nhau và tổng lại đúng. Thử với `accounts=2`
  (mọi lượt đều là 0↔1, đối xứng tối đa) thì tổng **đúng ở 5/6 lần** — bất biến gần như **mù**.
- Ở `read-uncommitted` còn một đường tự vá nữa, ngược hoàn toàn trực giác: **dirty read VÁ được
  lost update.** B đọc được số dư mới **chưa commit** của A rồi tính tiếp từ đó, nên update của A
  không mất. **Mức isolation yếu nhất đôi khi cho kết quả đúng nhờ chính cái tính chất làm nó yếu.**

Nên ở hai mức thấp, anomaly là chuyện **xác suất theo bản chất**. Bản sửa: chạy tới 5 lần và đòi
anomaly xảy ra **ít nhất một** lần ((1/6)^5 ≈ 1/7776); hai mức cao thì vẫn phải giữ tổng ở **mọi**
lần. Đòi mức thấp vỡ ở **mọi** lần là một **khẳng định sai về hệ thống**.

```console
$ for i in 1 2 3; do go test ./internal/txn/ -count=1 -run TestTransferInvariantPerLevel | tail -1; done
ok  	minidb/internal/txn	3.079s
ok  	minidb/internal/txn	4.119s
ok  	minidb/internal/txn	2.879s
```

**Đang nghĩ gì:** một bài test đỏ 1/6 lần **vô giá trị đúng bằng** một bài test chưa bao giờ đỏ —
cả hai đều không phân biệt được code đúng với code sai. Phase 5 học điều này từ hướng ngược lại
(`crashlab-nowrite`).

### 2026-09-03 — lượt chạy: bảng điểm hoà vốn, và mô hình ĐOÁN sai 7-10 lần

Trước khi chạy, mô hình chi phí trong code là ba con số **đoán**: `CSeq 1, CIndex 0.6,
CFetch 20`. Giả thuyết viết sẵn thành comment: điểm hoà vốn ở **4.85%**, khớp cái "thường ~5-20%"
mà ROADMAP viết.

```console
$ go run ./cmd/idxlab -work breakeven -rows 50000 -repeat 30
== 1. điểm hoà vốn selectivity (50000 hàng, 30 lần mỗi truy vấn) ==

hằng số đo được: một bước quét 480 ns, một bước index 285 ns
  một lần tra bảng: 2142 ns nếu khóa NHẢY LUNG TUNG, 1308 ns nếu khóa TĂNG DẦN (1.6x)
  -> CFetch/CSeq = 4.5x / 2.7x (mô hình mặc định trong code đoán 20.0x)
  -> điểm hoà vốn TÍNH RA: 19.76% / 30.10% (mô hình mặc định: 4.85%)

     sel    hàng     seq µs   index µs    only µs  idx/seq only/seq  planner(đo)   planner(đoán) thật
    0.1%      50    23764.0       76.4       14.3     0.00     0.00  IndexScan     IndexScan     IndexScan
    0.5%     250    24975.5      432.5       90.5     0.02     0.00  IndexScan     IndexScan     IndexScan
    1.0%     500    30473.1      675.6      225.7     0.02     0.01  IndexScan     IndexScan     IndexScan
    2.5%    1250    27699.2     2172.9      485.7     0.08     0.02  IndexScan     IndexScan     IndexScan
    5.0%    2500    23449.2     3018.3      677.1     0.13     0.03  IndexScan     SeqScan       IndexScan  <- planner(đoán) CHỌN SAI
   10.0%    5000    23264.7     6027.9     1350.9     0.26     0.06  IndexScan     SeqScan       IndexScan  <- planner(đoán) CHỌN SAI
   25.0%   12500    23780.9    16631.0     3815.8     0.70     0.16  SeqScan       SeqScan       IndexScan  <- planner(đoán) CHỌN SAI  <- planner(đo) CŨNG CHỌN SAI
   50.0%   25000    24506.0    32751.9     6617.1     1.34     0.27  SeqScan       SeqScan       SeqScan  <- ĐỔI VAI
  100.0%   50000    24314.8    62526.1    14955.3     2.57     0.62  SeqScan       SeqScan       SeqScan
```

**Đọc kết quả:** giả thuyết bị bác, và cái lý do đáng nhớ hơn con số. `CFetch/CSeq` đoán **20x**,
đo được **4.5x** — sai 4.4 lần, và điểm hoà vốn theo đó sai 7-10 lần (4.85% so với ~37%). Lý do:
**ở quy mô này KHÔNG CÓ I/O nào cả.** Cả cây nằm trong buffer pool, nên "một lần tra bảng" là một
lần đi xuống cây trong RAM, không phải một lần seek. Hằng số 20 là hằng số của một thế giới có
đĩa quay.

Con số 20 đó **chính là `random_page_cost` của Postgres** (mặc định 4.0, và mọi hướng dẫn tuning
SSD đều bảo hạ xuống 1.1). Ở đây tôi vừa tự tay dựng lại đúng cái bẫy mà tham số ấy sinh ra để
chữa.

**Cố ý KHÔNG sửa hằng số** trong code thành 4.5. Thay vào đó thêm cột `planner(đoán)`: nó chọn
`SeqScan` ở 5%, 10%, 25% — nơi index còn thắng **7.7x / 3.9x / 1.4x**. Một bảng có cột "planner
chọn sai" đắt giá hơn một hằng số đúng, vì hằng số đúng chỉ đúng trên máy này.

### 2026-09-03 — bảng deliverable PHẢN BÁC dòng tổng kết của chính nó

Đọc lại bảng trên, hàng 25%: `idx/seq = 0.70`, tức index **còn thắng 1.4x**. Mà dòng tổng kết bên
dưới bảng thì viết *"thời gian đổi vai ở khoảng 19% theo mô hình đo"*.

Hai câu ấy không thể cùng đúng. Truy ra: **con số 19% không hề được tính từ số đo** — nó được
**chép lại từ mô hình**. Bảng in ra số đo, rồi dòng kết luận in ra dự đoán, và không có ai đối
chiếu hai thứ. Đó là lý do một mâu thuẫn hiển nhiên nằm ngay cạnh nhau mà vẫn lọt.

Tính điểm hoà vốn **thật** bằng nội suy từ chính bảng:

```console
$ python3 -c '
seq = 9000.0
per = 11730.4/10000*1000
print(f"chi phí mỗi hàng của index scan: {per:.0f} ns")
print(f"  trừ một bước index đã đo 241 ns -> tra bảng TRONG scan = {per-241:.0f} ns")
print(f"  nhưng đo riêng bằng point lookup:                      1908 ns")
print(f"  -> đo riêng ĐẮT hơn {1908/(per-241):.1f}x")
print(f"điểm hoà vốn thực = {seq/per*1000/20000*100:.1f}%")'
chi phí mỗi hàng của index scan: 1173 ns
  trừ một bước index đã đo 241 ns -> tra bảng TRONG scan = 932 ns
  nhưng đo riêng bằng point lookup:                      1908 ns
  -> đo riêng ĐẮT hơn 2.0x
điểm hoà vốn thực = 38.4%
```

(Con số 38.4% này là phép tính tay trên số liệu 20000 hàng, giả định chi phí seq **phẳng** đúng
9000 µs. `query.CrossOver` viết ngay sau đó nội suy từ **tỉ số** của hai dòng kề nhau nên không
cần giả định ấy, và cho **37.5%** trên cùng dữ liệu.)

Và đây là chỗ tìm ra nguyên nhân gốc. `CFetch` được đo bằng 2000 lần tra pk với **bước nhảy
7919** (một số nguyên tố, để hai lần tra liền nhau không rơi vào cùng một leaf). Lý lẽ của cách
đo ấy được viết **hẳn thành comment trong code**:

> Khóa đi kiểu bước nhảy lớn để không đi tuần tự trong cùng một leaf — nếu đo trên khóa liền nhau
> thì mọi lần tra đều trúng page vừa nạp và con số ra **bé hơn sự thật**.

**Lý lẽ ấy sai.** Một index scan tra bảng **theo thứ tự index**; nếu thứ tự đó **tương quan** với
thứ tự primary key thì việc "trúng page vừa nạp" **chính là** sự thật của nó. Không có **một**
`CFetch` đúng — có **hai**, và cái nào đúng phụ thuộc **index nào**.

Đo cả hai (`fetchRand`, `fetchSeq`) và để bảng tự nói:

| `CFetch` đo bằng | ns | `CFetch/CSeq` | hoà vốn **dự đoán** | hoà vốn **đo được** |
|---|---|---|---|---|
| khóa nhảy lung tung (ca xấu nhất) | 2142 | 4.5x | 19.8% | **36.8%** |
| khóa tăng dần (ca index tương quan) | 1308 | 2.7x | **30.1%** | |
| hằng số đoán sẵn trong code | — | 20.0x | 4.9% | |

**Đọc kết quả:** dự đoán từ `fetchSeq` (30.1%) sai 1.2 lần; dự đoán từ `fetchRand` (19.8%) sai 1.9
lần; hằng số đoán (4.9%) sai 7.5 lần. Và điểm hoà vốn đo được ổn định qua hai kích cỡ bảng:
**36.8%** ở 50000 hàng, **37.5%** ở 20000 hàng — tỉ số bền, đúng như quy tắc 3.

Đúng thứ Postgres lưu riêng cho từng cột và gọi là **`correlation`**: tương quan giữa thứ tự
index và thứ tự vật lý của hàng. Trước phase này tôi biết tham số ấy tồn tại; giờ tôi biết **vì
sao nó không thể suy ra từ hai tham số kia**.

**Đã sửa:** thêm `query.CrossOver()` nội suy điểm hoà vốn **từ chính bảng sweep**, và bảng có thêm
cột `planner(đo) CŨNG CHỌN SAI`.

### 2026-09-03 — một benchmark đo fsync suốt mà tưởng đang đo index

```console
$ go test ./internal/query/ -run '^$' -bench 'IndexMaintenance' -benchtime=3000x
BenchmarkIndexMaintenance/indexes=0-6         	    3000	   1584814 ns/op
BenchmarkIndexMaintenance/indexes=1-6         	    3000	   1773932 ns/op
BenchmarkIndexMaintenance/indexes=2-6         	    3000	   1754438 ns/op
BenchmarkIndexMaintenance/indexes=3-6         	    3000	   1734841 ns/op
```

**Đọc kết quả:** đọc thô thì con số này "chứng minh" index gần như **miễn phí** ở đường ghi —
1.00x / 1.12x / 1.11x / 1.09x. Nhưng bảng 2 của `cmd/idxlab`, trên **cùng** công việc, cho
1.54x / 2.72x / 5.39x.

Hai số đo mâu thuẫn ⇒ **nghi bộ đo trước** (quy tắc 5). Thủ phạm lộ ra ngay từ **độ lớn**: 1.58 ms
là **đúng** cái giá một lần `fsync` đã đo ở phase 0, còn việc bảo trì index thì cỡ **vài µs**. Mỗi
vòng lặp một `c.Update` = mỗi vòng một transaction = mỗi vòng **một lần fsync**. 99.7% con số là
durability. Benchmark đang đo một thứ khác hẳn thứ nó tự nhận.

Gộp 200 hàng vào một transaction (group commit của phase 5 làm phần còn lại):

```console
$ go test ./internal/query/ -run '^$' -bench 'IndexMaintenance' -benchtime=6000x
BenchmarkIndexMaintenance/indexes=0-6         	    6000	     17158 ns/op
BenchmarkIndexMaintenance/indexes=1-6         	    6000	     32595 ns/op
BenchmarkIndexMaintenance/indexes=2-6         	    6000	     40550 ns/op
BenchmarkIndexMaintenance/indexes=3-6         	    6000	     49244 ns/op
```

1.00x / **1.90x** / **2.36x** / **2.87x** — giờ **cùng hình** với `idxlab` thay vì mâu thuẫn với
nó. Bài học phase 5 quay lại **lần thứ ba**: khi một chi phí cố định lớn trùm lên phép đo, cái nó
đo là chi phí cố định ấy.

**Đang nghĩ gì:** hai phép đo mâu thuẫn nhau là món quà, không phải rắc rối. Nếu chỉ có benchmark
này thì tôi đã kết luận "index gần như miễn phí" và viết luôn vào diary.

### 2026-09-03 — lời đọc bảng suy nhân quả sai, và cột đo được chỉ đúng thủ phạm

```console
$ go run ./cmd/idxlab -work maintain
   index chèn µs/hàng  sửa µs/hàng so 0 index  ghi idx/chèn   ghi idx/sửa
       0         8.70        16.48       1.00x           0.0           0.0
       1        13.42        13.30       1.54x           1.0           0.0
       2        23.64        14.09       2.72x           2.0           0.0
       3        46.85        35.05       5.39x           3.0           2.0
```

Lời đọc bảng của bản đầu viết: *"cột 'sửa' đổi cột payload, không phải cột được index. Nó vẫn đắt
lên theo số index, vì `Upsert` phải **ĐỌC hàng cũ** để biết mục index nào cần xoá."*

**Dữ liệu bác bỏ cái nhân quả ấy.** Cột `sửa` ở nIdx = 0,1,2 là 16.5 / 13.3 / 14.1 µs — **phẳng**,
thậm chí hơi giảm, và **không đơn điệu** giữa các lần chạy (một lần khác cho 12.5 / 12.3 / 12.2).
Chỉ dòng cuối nhảy một bậc.

Thêm cột `ghi idx/sửa` — số mục index mà `Upsert` **thật sự ghi**, tức số **đo** chứ không phải
lập luận — và nó chỉ đúng thủ phạm: **0, 0, 0, 2**. Vì `cols[2] == "payload"`, nên **riêng ở
nIdx = 3** cái cột bị sửa mới **là** một cột được index.

Và con số **2**, chứ không phải 1, là chỗ đáng nhất: một mục index **không sửa được tại chỗ**, vì
khóa của nó **chứa** giá trị cũ. Phải xoá mục cũ rồi chèn mục mới. **Đổi giá trị một cột được index
là đổi VỊ TRÍ của nó trong cây.**

**Kết luận đúng ngược với kết luận cũ:** giá của một UPDATE không do **số index của bảng** quyết
định, mà do **số index có chứa cột bị đổi** — nhân hai. Việc đọc hàng cũ có thật và không tránh
được, nhưng nó rẻ tới mức **không hiện ra trong phép đo**.

Đó đúng là tối ưu **HOT (heap-only tuple)** của Postgres: một update không đổi cột nào được index
thì không sinh mục index nào. Cột `ghi idx/sửa = 0` là bằng chứng tầng bảng ở đây cũng làm thế —
và tôi làm nó ở `Tx.write` vì một lý do khác hẳn (tránh nối dài chuỗi version, trần ~2KB của
P6-1), rồi mới phát hiện ra nó có tên.

### 2026-09-03 — bài phản chứng: tắt cơ chế `gen`, và CẢ PHASE 7 vẫn xanh

Phase 5 dạy: một bài test chưa bao giờ đỏ thì chưa phải bằng chứng. Nên tắt hẳn cơ chế số đời cấu
trúc (`if c.gen != c.t.gen` → `if false`) rồi chạy lại.

```console
$ go test ./internal/btree/ -count=1 -run 'CursorRestores'
--- FAIL: TestCursorRestoresAfterSplit (0.00s)
    cursor_test.go:49: sau khi cây đổi, khóa kế = "k000051", mong "k000101"
--- FAIL: TestCursorRestoresAfterDeleteOfOwnKey (0.00s)
    cursor_test.go:95: khóa kế sau khi 50 và 51 bị xóa = "k000053", mong "k000052"
FAIL

$ go test ./internal/db/ ./internal/txn/ ./internal/table/ ./internal/query/ -count=1 -run 'Iter|Scan|Index|Plan'
--- FAIL: TestIterSurvivesWriterBetweenSteps (0.51s)
    iter_test.go:93: Restores == 0 dù có ghi giữa MỌI bước — cơ chế tìm lại chỗ chưa chạy
--- FAIL: TestIterSurvivesConcurrentWriter (1.46s)
    iter_test.go:178: Restores == 0: writer chưa từng chen được vào giữa, ...
FAIL	minidb/internal/db	2.592s
ok  	minidb/internal/txn	2.175s
ok  	minidb/internal/table	0.786s
ok  	minidb/internal/query	0.156s
```

**Đọc kết quả:** hai tầng dưới đỏ **đúng kiểu** — cursor nhảy từ `k000051` lên `k000101`, mất
**đúng nửa leaf** sau một split; ca xóa thì nhảy qua một khóa. Đó là bằng chứng bài test biết báo
sai.

Nhưng `txn`, `table`, `query` **xanh cả ba**. Tức là **toàn bộ bài test của phase 7 sẽ xanh với
một cursor hỏng.**

Lý do đơn giản và cũng rất dễ mắc lại: mọi lần quét trong các bài test ấy đều chạy **một mình**.
Không có ai ghi vào cây giữa hai bước, nên số đời không bao giờ đổi, nên nhánh code cần kiểm
**không bao giờ chạy**. **Một bài test chỉ kiểm được cái mà nó làm cho XẢY RA.**

Thêm `TestIndexScanSurvivesWriterMidScan`: ghi từ chính goroutine đang quét, giữa **mọi** bước của
một index scan. Xanh khi cơ chế bật; và đỏ khi tắt, **ở một kiểu khác** nữa:

```console
$ go test ./internal/table/ -count=1 -run 'IndexScanSurvivesWriterMidScan'   # với gen bị tắt
--- FAIL: TestIndexScanSurvivesWriterMidScan (0.56s)
    table_test.go:479: khóa index không tăng ngặt: ["c00000"] sau ["c00000"]
FAIL
```

Không phải nhảy qua khóa, mà **lặp** khóa: slot cũ trỏ về một khóa đã đi qua.

Bài test này còn là **bằng chứng ngược** của bài test từng treo 60 giây: ở đây mỗi bước chèn một
mục nằm **phía trước** cursor, mà lần quét **vẫn kết thúc** — vì ảnh chụp MVCC của phase 6 chốt
chặn trên ngay lúc bắt đầu, nên mục vừa chèn **không nhìn thấy được**. Đúng cái mà tầng btree
thuần không có.

### 2026-09-03 — fuzz, và kiểm hồi quy phase 5 / phase 6

```console
$ make fuzz-keys
fuzz: elapsed: 2m0s, execs: 10479214 (64240/sec), new interesting: 5 (total: 104)
ok  	minidb/internal/keys	120.203s
fuzz: elapsed: 2m0s, execs: 11144867 (90034/sec), new interesting: 7 (total: 35)
ok  	minidb/internal/keys	120.082s

$ make fuzz-table
fuzz: elapsed: 2m0s, execs: 1580 (3/sec), new interesting: 108 (total: 259)
ok  	minidb/internal/table	121.672s
```

**Đọc kết quả:** 10.48M + 11.14M exec sạch trên bộ mã hoá khóa. Đáng chú ý là **không** có bug nào
— khác hẳn phase 6, nơi `FuzzChainCodec` đỏ ở **giây thứ 3**. Lý do không phải là may: luật
canonical lần này được viết **trước**, vì phase 6 đã trả học phí cho nó bằng hai con bug.

`FuzzTableIndex` chỉ **3 exec/giây** vì mỗi exec có crash + mở lại + `Verify()` toàn cây. 108 hạt
mới trong 1580 exec (6.8%) nghĩa là corpus **còn đang mọc** — bộ fuzz này chưa bão hoà, ghi thành
nợ P7-4.

```console
$ go run ./cmd/crashlab -n 20
20/20 vòng đúng, 0 sai. 765 transaction đã commit được kiểm, 7.606s.

$ make crashlab-nowrite
7           394      156         0  SAI: Verify lỗi: btree: node hỏng: page 8 kiểu 4 không phải node B+Tree
exit status 1

$ go run ./cmd/txnlab -work anomaly
anomaly               read-uncomm       read-comm         repeat-read       serializable
dirty-read            X                 .                 .                 .
non-repeatable-read   X                 X                 .                 .
phantom               X                 X                 .                 .
lost-update           X                 X                 .                 .
write-skew            X                 X                 X                 .
```

**Đọc kết quả:** phase 5 nguyên vẹn (và bài phản chứng của nó **vẫn còn biết đỏ**), phase 6 nguyên
vẹn từng ô — kể cả sau khi `Txn.Scan` bị viết lại hoàn toàn từ materialize sang stream.

---

## Giả thuyết sai / bug đã gặp

| Tôi tưởng là | Thực tế là | Lệnh / output đã lật tẩy nó | Đã sửa thế nào |
|---|---|---|---|
| Trả P6-4 phải **cần latch-coupling (P4-5) trước** — câu này tôi tự viết vào `docs/debts.md` ở phase 6 | Không cần. Cái quyết định một reader **thấy gì** là **ảnh chụp MVCC** (phase 6), không phải latch (phase 4). Latch chỉ còn phải bảo vệ **vị trí** cursor, và vị trí thì cần một cách **biết mình đã hết đúng**, không cần latch | `TestIterSurvivesConcurrentWriter` xanh dưới `-race`: 400 khóa, **212** lần tìm lại chỗ, không lần nào lệch thứ tự | `btree.Tree.gen` + `Cursor.restore()`, định vị bằng **khóa** chứ không bằng `(page, slot)` |
| `CFetch/CSeq = 20` (viết sẵn thành hằng số) ⇒ hoà vốn ở **4.85%**, khớp "thường ~5-20%" của ROADMAP | Đo được **4.5x**, hoà vốn **36.8%**. Ở quy mô này **không có I/O nào** — cả cây trong buffer pool, nên "tra bảng" là một lần đi xuống cây trong RAM | `make idxlab-breakeven`: cột `planner(đoán)` chọn `SeqScan` ở 5/10/25% nơi index thắng **7.7x / 3.9x / 1.4x** | **Giữ nguyên hằng số sai** và thêm cột `planner(đoán)`. Đó chính là `random_page_cost` của Postgres |
| Đo `CFetch` phải dùng khóa **nhảy lung tung**, vì đo khóa liền nhau thì "trúng page vừa nạp và con số bé hơn **sự thật**" (lý lẽ viết hẳn thành comment) | Không có **một** `CFetch` đúng, có **hai**. Index scan tra bảng **theo thứ tự index**; nếu thứ tự đó tương quan với pk thì trúng page vừa nạp **chính là** sự thật của nó. Chênh **1.6-2.2x** | `python3` nội suy từ chính bảng: tra bảng **trong** scan = 932 ns, đo riêng = 1908 ns → **2.0x**. Dự đoán từ `fetchSeq` = 30.1% vs đo được 36.8% | Đo cả hai ca, in cả hai, và thêm `query.CrossOver()`. Đây là thống kê **`correlation`** của Postgres |
| Dòng *"thời gian đổi vai ở khoảng 19%"* dưới bảng deliverable là một kết luận từ số đo | Nó được **chép lại từ mô hình**, không tính từ số đo — nên nó **phản bác chính cái bảng ngay trên nó** (hàng 25% có `idx/seq = 0.70`) | `make idxlab-breakeven`: hàng `25.0% ... 0.70 ... SeqScan` nằm ngay trên dòng kết luận "19%" | `query.CrossOver()` nội suy từ bảng sweep; thêm cột `planner(đo) CŨNG CHỌN SAI` |
| `BenchmarkIndexMaintenance` đo giá bảo trì index ⇒ index gần như **miễn phí** ở đường ghi (1.00/1.12/1.11/1.09x) | Nó đo **fsync**. 1.58 ms/op đúng bằng giá một fsync đo ở phase 0; việc bảo trì index cỡ **vài µs** ⇒ **99.7%** con số là durability | Mâu thuẫn với bảng 2 của `idxlab` (1.54/2.72/5.39x) trên **cùng** công việc | Gộp lô **200 hàng/transaction** → 1.00/**1.90**/**2.36**/**2.87x**, cùng hình với `idxlab` |
| Cột `sửa` của bảng 2 đắt lên theo **số index**, vì `Upsert` phải **đọc hàng cũ** | Đọc hàng cũ rẻ tới mức **không hiện ra**. Cột `sửa` **phẳng** ở nIdx = 0,1,2 (16.5/13.3/14.1 µs) và chỉ nhảy ở nIdx = 3, đúng chỗ `cols[2] == "payload"` — cột bị sửa **là** cột được index | Thêm cột đo `ghi idx/sửa`: **0, 0, 0, 2**. Số **2** vì khóa index **chứa** giá trị cũ nên phải xoá + chèn | Viết lại lời đọc bảng theo hướng ngược: giá UPDATE do **số index chứa cột bị đổi** × 2. Đó là tối ưu **HOT** của Postgres |
| Bài test cursor xanh ⇒ cơ chế `gen` được kiểm | Tắt hẳn cơ chế thì `btree` và `db` đỏ, còn **`txn`, `table`, `query` xanh cả ba** — mọi lần quét ở các tầng ấy chạy **một mình**, nên số đời không bao giờ đổi | `sed -i 's/if c.gen != c.t.gen {/if false {/'` rồi `go test ./internal/...`: `ok txn / ok table / ok query` | Thêm `TestIndexScanSurvivesWriterMidScan` — ghi giữa **mọi** bước của một index scan; đỏ khi tắt (`khóa index không tăng ngặt: ["c00000"] sau ["c00000"]`) |
| `TestIterSurvivesWriterBetweenSteps` treo ⇒ deadlock, tức cơ chế nhả latch sai | Stack chỉ vào `wal.Flush` → fsync, tức **chậm-nhưng-còn-sống**. Bài test chèn khóa luôn nằm **phía trước** cursor nên mỗi bước sinh thêm một bước | `panic: test timed out after 1m0s` + stack `wal.(*Log).Flush` | Ghi đè khóa **sau lưng** cursor. Giữ lý lẽ thành comment: **một lần duyệt có nhả latch không được hứa là hữu hạn** nếu writer chèn vào phía trước — vì thế snapshot phải chốt chặn trên |
| `TestTransferInvariantPerLevel` đỏ ⇒ phase 7 làm hồi quy phase 6 | Có **từ trước**, đỏ ~1/6 lần ở đúng ô `read-uncommitted`. Hai lý do: bất biến "tổng số dư" **đối xứng** nên lost update **triệt tiêu** nhau; và **dirty read VÁ được lost update** | `git worktree add /tmp/p6check HEAD` rồi chạy 6 lần: `ok/ok/FAIL/ok/ok/ok` | `maxTries = 5` ở hai mức thấp, **mọi lần** ở hai mức cao. Giả thuyết đầu (thiếu `runtime.Gosched`) bị **bác bởi bản sửa của chính nó** (vẫn 2/8 đỏ) và đã **trả lại nguyên trạng** |
| `TestThreePlansSameRows` báo "hàng 200/200 lệch (-1)" ⇒ một kế hoạch trả sai kết quả | Lỗi của **bộ đo**: một multiset dùng chung bị trừ cho **cả hai** kế hoạch index | Đổi sang so **từng đôi** với `SeqScan` → xanh | So từng đôi. Đúng cái bẫy mà phase 0 gọi là *nghi bài test trước* |

## Số đo

**Lệnh · ngày 2026-09-03 · commit `_(chốt phase)_` · máy:** i5-1235U, WSL2, ext4, go1.26.2.

### Ba hằng số của mô hình chi phí — hai bộ đo độc lập phải khớp

```console
$ go test ./internal/query/ -run '^$' -bench 'SeqStep' -benchtime=30x -count=3
BenchmarkSeqStep-6   	      30	   8909553 ns/op	     20000 rows
BenchmarkSeqStep-6   	      30	   8488609 ns/op	     20000 rows
BenchmarkSeqStep-6   	      30	   9369076 ns/op	     20000 rows

$ go test ./internal/query/ -run '^$' -bench 'PointLookup|ScanLimit' -benchtime=20000x -count=3
BenchmarkPointLookup-6   	   20000	      1076 ns/op
BenchmarkPointLookup-6   	   20000	       987.8 ns/op
BenchmarkPointLookup-6   	   20000	      1242 ns/op
BenchmarkScanLimit-6     	   20000	      1668 ns/op
BenchmarkScanLimit-6     	   20000	      1558 ns/op
BenchmarkScanLimit-6     	   20000	      1423 ns/op
```

| Hằng số | benchmark | `cmd/idxlab` | khớp? |
|---|---|---|---|
| một bước quét | 424-468 ns/hàng | 413-544 ns | ✓ |
| một bước index | — | 241-295 ns | |
| tra bảng, **tương quan** | 988-1242 ns | 1063-1308 ns | ✓ |
| tra bảng, **không tương quan** | — | 2142-2327 ns | |

`BenchmarkPointLookup` đi khóa `i % benchRows`, tức **tăng dần** ⇒ nó đo ca **tương quan**. Ghi
điều kiện ấy thành comment tại chỗ, vì thiếu nó thì hai bộ đo trông như mâu thuẫn.

### Bộ mã hoá khóa

```console
$ go test ./internal/keys/ -run '^$' -bench . -benchmem
BenchmarkEncode/uint-6      	100000000	        11.90 ns/op	       0 B/op	       0 allocs/op
BenchmarkEncode/str16-6     	38999023	        29.67 ns/op	       0 B/op	       0 allocs/op
BenchmarkEncode/str16-desc-6         	39457563	        30.64 ns/op	       0 B/op	       0 allocs/op
BenchmarkEncode/composite3-6         	37479062	        32.43 ns/op	       0 B/op	       0 allocs/op
BenchmarkDecode-6                    	10522184	       117.2 ns/op	     152 B/op	       2 allocs/op
BenchmarkCompareVsBytes/tuple-6      	82377576	        14.08 ns/op	       0 B/op	       0 allocs/op
BenchmarkCompareVsBytes/memcmp-6     	528309124	         2.468 ns/op	       0 B/op	       0 allocs/op
```

`DESC` đắt hơn `ASC` **1.03x** (30.64 vs 29.67) — vì nó chỉ là một phép XOR. Đó là toàn bộ cái giá
của việc chọn "bù từng byte" thay cho "một comparator riêng".

### Ba kế hoạch, chín độ chọn lọc

```console
$ go test ./internal/query/ -run '^$' -bench 'PlanSelectivity' -benchtime=20x
BenchmarkPlanSelectivity/sel=000.1%/seq-6         	      20	  10705514 ns/op	        20.00 rows	     20000 touched
BenchmarkPlanSelectivity/sel=000.1%/index-6       	      20	     45465 ns/op	        20.00 rows	        40.00 touched
BenchmarkPlanSelectivity/sel=000.1%/indexonly-6   	      20	     15000 ns/op	        20.00 rows	        20.00 touched
...
BenchmarkPlanSelectivity/sel=050.0%/seq-6         	      20	  10567881 ns/op	     10000 rows	     20000 touched
BenchmarkPlanSelectivity/sel=050.0%/index-6       	      20	  13644457 ns/op	     10000 rows	     20000 touched
BenchmarkPlanSelectivity/sel=050.0%/indexonly-6   	      20	   3223680 ns/op	     10000 rows	     10000 touched
BenchmarkPlanSelectivity/sel=100.0%/seq-6         	      20	  11382623 ns/op	     20000 rows	     20000 touched
BenchmarkPlanSelectivity/sel=100.0%/index-6       	      20	  26132866 ns/op	     20000 rows	     40000 touched
BenchmarkPlanSelectivity/sel=100.0%/indexonly-6   	      20	   6358466 ns/op	     20000 rows	     20000 touched
```

Cột `touched` là chỗ đọc ra **cơ chế**, không chỉ đọc ra thời gian: index scan chạm **2×** số hàng
khớp (một mục index + một hàng), index-only chạm **1×**, seq scan chạm **cả bảng** bất kể điều
kiện hẹp cỡ nào. **Điểm hoà vốn theo `touched` là 50%; theo thời gian là ~37%.** Hai con số khác
nhau và không được lẫn: cái đầu là **entry**, cái sau là **thời gian**, và thời gian còn tính cả
locality.

### Tỉ số cần nhớ

| Tỉ số | Giá trị | Ý nghĩa |
|---|---|---|
| điểm hoà vốn index scan vs seq scan (**đo**) | **36.8%** (50k) / **37.5%** (20k) | Ổn định qua hai kích cỡ. Cao hơn "thường 5-20%" **vì không có I/O thật** |
| điểm hoà vốn theo **entry đã chạm** | **50%** | Khác con số thời gian; index scan chạm 2× số hàng khớp |
| hằng số đoán vs đo (`CFetch/CSeq`) | 20.0x vs **4.5x** = **4.4x lệch** | Kéo điểm hoà vốn lệch **7.5x** (4.9% vs 36.8%) |
| `CFetch` không tương quan / tương quan | **1.6-2.2x** | Đây là thống kê `correlation` của Postgres, và nó **không** suy ra được từ hai hằng số kia |
| index-only vs seq, ở **100%** cả bảng | **0.55-0.62** | Index phủ thắng ở **mọi** độ chọn lọc — **không có điểm hoà vốn** |
| index-only vs index scan, ở 0.1% | **3.0x** (bench) · **2.96x** / **5.3x** (idxlab 20k / 50k) | Cái mà `Query.Need` mua được, và cái mà `SELECT *` phá |
| thuế của index ở đường **chèn** | 1.54x / 2.72x / **5.39x** (1/2/3 index) | Một hàng mới thì **mọi** index phải có thêm một mục |
| thuế của index ở đường **sửa** | **1.0x** nếu cột bị đổi không được index | Tối ưu HOT. `ghi idx/sửa = 0, 0, 0, 2` |
| `LIMIT 1` vs quét cả bảng | **5032x** | Cái mà nợ P6-4 mua được. Bản materialize cho tỉ số **1x** |
| `memcmp` vs so theo kiểu | **5.7x** (2.47 vs 14.08 ns) | Lý do mọi DB ép khóa thành byte |
| `DESC` vs `ASC` khi mã hoá | **1.03x** | Bù từng byte gần như miễn phí |
| ước lượng sai trên cột lệch 99% | **97x** | Và planner vẫn chọn `IndexScan` — kế hoạch **tệ nhất có thể** |

### Ước lượng: chỗ mô hình chi phí đúng mà kế hoạch vẫn tệ

```console
$ go run ./cmd/idxlab -work estimate
điều kiện                   ước lượng       thật      lệch  planner chọn
kind < 10 (đều)                   200        200      1.0x  IndexScan
kind < 500 (đều)                10010      10000      1.0x  SeqScan
city = 'HN' (lệch 99%)            204      19800     97.0x  IndexScan
city = 'c001' (đuôi)              204          2      0.0x  IndexScan
```

**Đọc kết quả:** hai dòng đầu (phân bố đều) khớp tới **1.0x** — mô hình phân bố đều không hề tệ
*khi phân bố đúng là đều*. Hai dòng sau lệch hàng chục lần. Hậu quả nằm ở **cột cuối**: với
`city='HN'` (99% cả bảng) planner vẫn chọn `IndexScan`, tức quét index rồi tra bảng gần **20000**
lần để lấy gần như **mọi** hàng — **kế hoạch tệ nhất có thể**.

Và nó được chọn **không phải vì mô hình chi phí sai, mà vì ước lượng số hàng sai**. Đó là chỗ
histogram tồn tại để chữa, và cột `lệch` là lý do Postgres bỏ tiền vào `ANALYZE`.
`TestEstimateIsWrongOnSkew` khẳng định **đúng cái sai này**: nó là một bài test **của một giới
hạn**, nên nó sẽ **đỏ** khi ai đó thêm histogram — và đỏ đúng lúc.

## Invariant tôi đã cài và lệnh kiểm chứng nó

| Invariant | Cài ở đâu (file:hàm) | Lệnh kiểm chứng | Kết quả |
|---|---|---|---|
| thứ tự **byte** == thứ tự **logic**, ở mọi kiểu / mọi chiều sắp / mọi ca tiền tố và byte `0x00` | `internal/keys/keys.go:AppendField`, `Encode` | `make fuzz-keys` | 10 479 214 exec sạch |
| **canonical**: giải mã được ⇒ mã hoá lại ra đúng byte cũ | `internal/keys/keys.go:DecodeField` | `make fuzz-keys` (target 2) | 11 144 867 exec sạch |
| một field **không bao giờ** bắt đầu bằng `0x00` (giữ nguyên khoảng metadata của `internal/txn`) | `internal/keys/keys.go` — không dùng `0x00` làm tag | `go test ./internal/keys/ -run TestNoLeadingZeroByte` | ok |
| mọi hàng có **đúng một** mục trong mỗi index; **không** mục nào trỏ tới hàng không còn | `internal/table/rows.go:(*Tx).write` — điểm thực thi **duy nhất** | `go test ./internal/table/ -run TestIndexStaysConsistentWithRows -race` | ok |
| bất biến trên còn đúng **sau crash + mở lại** | như trên, đi qua WAL/redo/undo của phase 5 | `make fuzz-table` | 1580 exec, `BadChains == 0`, `Verify()` sạch |
| **đổi kế hoạch KHÔNG được đổi kết quả** (ba đường, cùng multiset hàng) | `internal/query/run.go:Run` — một signature, ba kế hoạch | `go test ./internal/query/ -run TestThreePlansSameRows -race` | ok |
| cursor còn đúng khi cây đổi hình **giữa** hai `Next()` | `internal/btree/cursor.go:restore`, `btree.go:Tree.gen` | `go test ./internal/btree/ ./internal/db/ -run 'CursorRestores\|Iter' -race` | ok · và **đỏ** khi tắt cơ chế |
| index scan còn đúng khi có writer chen giữa **mọi** bước | như trên, kiểm ở tầng bảng | `go test ./internal/table/ -run IndexScanSurvivesWriterMidScan -race` | ok · và **đỏ** khi tắt cơ chế |
| `Txn.Scan` giữ **ảnh chụp** dù có commit thật xen vào giữa | `internal/txn/txn.go:(*Txn).Scan` | `go test ./internal/txn/ -run TestScanKeepsSnapshotDespiteConcurrentCommit` | ok |
| ràng buộc **unique** được thực thi, và index **non-unique** không bao giờ xung đột | `internal/table/table.go:(*Index).EntryKey` — hình dạng khóa | `make test-index` | ok, cả hai chiều |
| phase 5 không hồi quy | — | `go run ./cmd/crashlab -n 20` + `make crashlab-nowrite` | 20/20 đúng · phản chứng **vẫn đỏ** |
| phase 6 không hồi quy | — | `make txnlab-anomaly` | 5 × 4 ô khớp lý thuyết |

## Đọc gì

- **Postgres `src/backend/optimizer/path/costsize.c`** — `cost_index()`. Đọc **sau** khi bảng của
  tôi tự bác dòng kết luận của nó, và đó là lúc `indexCorrelation` có nghĩa: Postgres nội suy chi
  phí giữa **ca tuần tự** và **ca ngẫu nhiên** theo bình phương tương quan. Tôi vừa đo được hai
  đầu của cái nội suy ấy mà không biết nó có tên.
- **`random_page_cost` / `seq_page_cost`** trong tài liệu tuning Postgres — và vì sao hướng dẫn
  SSD bảo hạ `random_page_cost` từ 4.0 xuống 1.1. Hằng số `CFetch = 20` của tôi là cùng một lỗi,
  phóng đại lên.
- **HOT (heap-only tuple)** — `src/backend/access/heap/README.HOT`. Tìm đọc **sau** khi cột
  `ghi idx/sửa = 0` bắt tôi phải giải thích vì sao cột `sửa` phẳng.
- **InnoDB / SQLite cursor restoration** — `btr_pcur_restore_position`, `sqlite3BtreeCursorRestore`.
  Đây là cái tôi tự dựng lại từ đầu ở `Cursor.restore`, và biết nó có tên làm tôi tin hơn vào việc
  định vị bằng **khóa** thay vì `(page, slot)`.
- **CMU 15-445, lecture "Query Execution"** — Volcano/iterator model và vì sao vectorized execution
  ra đời.
- Đọc lại **`docs/debts.md` của chính mình**, mục P6-4. Nó là tài liệu quan trọng nhất của phase
  này, và nó **sai** ở đúng câu "cách trả".

## Rút ra (viết như thể giải thích cho người khác)

**Secondary index lưu gì ở leaf, và vì sao không lưu cả row.** Lưu **primary key**, không lưu
`(PageID, SlotID)` như ROADMAP viết, và cũng không lưu cả hàng. Ba lý do, xếp theo độ nặng. Thứ
nhất: hàng ở đây **nằm trong** index của primary key (kiểu InnoDB/SQLite), nên nó **di chuyển**
khi leaf split — một con trỏ vật lý sẽ hỏng sau mỗi lần split, và sửa nó lại thì phải đi sửa
**mọi** secondary index. Primary key là một **giá trị logic**, nó không di chuyển. Thứ hai: lưu cả
hàng là nhân bản dữ liệu ⇒ mỗi UPDATE phải ghi ở n+1 chỗ. Thứ ba, và đây là chỗ chỉ thấy được sau
khi đo: mục index **nhỏ** là điều kiện để index-only scan **thắng ở mọi độ chọn lọc** (0.55-0.62
ngay cả khi lấy 100% bảng) — vì nó đọc ít **byte** hơn, không phải vì nó đọc ít **hàng** hơn.

**Covering index tiết kiệm được gì.** Đếm bằng cột `touched`: index scan chạm **2×** số hàng khớp
(một mục + một hàng), index-only chạm **1×**. Nhưng tỉ số thời gian (3.0-5.3x ở 0.1%) **lớn hơn** tỉ số
số lần chạm (2x), vì hai lần chạm ấy nằm ở **hai vùng xa nhau** của cây (`0x0c...` và `0x0b...`),
nên nó còn phá cả locality. Và cái quyết định một index có "phủ" hay không là `Query.Need` — tức
là **`SELECT *` là câu lệnh phá index-only scan**. Đó là toàn bộ lý do lời khuyên "đừng
`SELECT *`" tồn tại, và giờ tôi có con số của nó.

**Left-most prefix, vì sao index `(a,b)` vô dụng với `WHERE b = ?`.** Vì các cột là byte **nối
nhau**, và cây biết đúng **một** phép so: `memcmp`. `a='HN'` là một **khoảng liên tục**
`[06484e0000, 06484e0001)`. `b=30` thì **không là khoảng nào cả** — byte của `b` nằm **sau** byte
của `a`, nên các hàng `b=30` rải khắp cây: `06444e0000fb...e1`, `06484e0000fb...e1`,
`0653470000fb...e1`. `make idxlab-bytes` in ra đúng ba dòng ấy trong một phần nghìn giây. Nên
"leftmost prefix rule" **không phải một quy ước của MySQL** — nó là hệ quả trực tiếp của việc
khóa là byte và phép so là memcmp. Biết được điều này thì không cần học thuộc quy tắc nữa.

**Selectivity bao nhiêu thì seq scan thắng, trên máy tôi, và vì sao.** **~37%** — cao hơn nhiều cái
"thường 5-20%" mà ROADMAP viết, và **vì sao** thì quan trọng hơn con số: ở quy mô này **không có
I/O thật nào cả**. Cả cây nằm trong buffer pool, nên "tra bảng theo pk" là một lần đi xuống cây
trong RAM (~1 µs), không phải một lần seek (~10 ms trên đĩa quay). Tỉ số `CFetch/CSeq` đo được
**4.5x** thay vì 20x, và điểm hoà vốn tỉ lệ nghịch với nó. Muốn thấy con số 5-20% thì phải làm cây
lớn hơn RAM — đó là nợ P7-5.

Nhưng bài học thật của câu hỏi này không phải con số. Là **ba** điều:

1. **Mô hình chi phí sai vẫn còn ít nguy hiểm hơn ước lượng số hàng sai.** Hằng số lệch 4.4x làm
   planner chọn sai ở một **dải** (5-25%) nơi nó thua 1.4-7.7x. Ước lượng lệch 97x làm planner
   chọn **kế hoạch tệ nhất có thể** cho một truy vấn lấy 99% bảng. Đó là lý do Postgres có
   `ANALYZE` và histogram, chứ không chỉ có `random_page_cost`.
2. **Một hằng số chi phí không phải thuộc tính của phép toán.** Nó là thuộc tính của phép toán
   **cộng với thứ tự truy cập**. `CFetch` đo bằng khóa nhảy lung tung đắt hơn 1.6-2.2x so với đo
   bằng khóa tăng dần, và **cả hai đều đúng** — cho hai loại index khác nhau. Đó là vì sao
   Postgres phải lưu `correlation` **riêng cho từng cột**, và vì sao nó không suy ra được từ
   `seq_page_cost` và `random_page_cost`.
3. **Giá của một UPDATE do số index CHỨA CỘT BỊ ĐỔI quyết định, không do số index của bảng.** Thêm
   một index vào bảng làm INSERT đắt thêm gần một lần (không tránh được), nhưng làm UPDATE đắt
   thêm **0 đồng** nếu cột được đổi không nằm trong index đó. Và khi nó có nằm trong thì giá là
   **2** mục, không phải 1 — vì khóa index **chứa** giá trị cũ nên không sửa được tại chỗ.

**Volcano model, chi phí ẩn của nó.** Ở đây "Volcano" là callback lồng nhau
(`Run → ScanIndex → Get → fn`), và cái giá lộ ra ngay trong số đo: một bước quét **413-544 ns**
cho một hàng chỉ có 4 cột, trong khi `memcmp` một khóa mất **2.5 ns**. Phần lớn thời gian đi vào
**mỗi hàng một lần gọi hàm, mỗi hàng một lần giải mã** (`keys.Decode`: 117 ns, 152 B, 2 alloc —
nợ P7-1). Nhân với 20000 hàng thì cái "chi phí mỗi hàng" ấy **chính là** cả thời gian của seq scan.
Đó là lý do vectorized execution ra đời: trả về **một lô 1024 hàng** mỗi lần gọi, để chi phí gọi
hàm chia cho 1024 và để phép giải mã chạy trên một mảng liền chứ không trên một hàng lẻ. Tôi
không cài nó, nhưng giờ tôi biết cái nó chữa **nằm ở đâu trong số đo của mình**.

**Và bài học lớn nhất của phase này không nằm trong danh sách câu hỏi ở đầu file.**

Bốn con bug của lượt chạy: **cả bốn đều là bug của bộ đo, không phải của database.** Một bảng
deliverable phản bác dòng kết luận của chính nó. Một benchmark đo fsync suốt mà nhãn ghi là "index
maintenance". Một lời đọc bảng suy nhân quả ngược. Và một bộ test xanh với một cursor hỏng.

Ba trong bốn cái đó **chỉ lộ ra khi có hai phép đo độc lập cùng nói về một việc**. Benchmark nói
index gần như miễn phí, `idxlab` nói nó đắt 5.39x — mâu thuẫn ấy là món quà. Nếu chỉ có một trong
hai thì tôi đã viết con số sai vào diary và tin nó sáu tháng.

Cái thứ tư thì lộ ra bằng một câu hỏi khác: **"bài test của tôi có biết báo sai không?"** Phase 5
học câu ấy bằng `crashlab-nowrite`. Ở đây nó có dạng: tắt cơ chế đang cần kiểm, xem ai đỏ. Kết quả
là `txn`, `table`, `query` **xanh cả ba** — vì mọi lần quét trong chúng chạy **một mình**, nên
nhánh code cần kiểm không bao giờ chạy. **Một bài test chỉ kiểm được cái mà nó làm cho XẢY RA**, và
"code này có nhánh xử lý đồng thời" không có nghĩa là "test này chạy nhánh ấy".

Còn một điều nữa, về sổ nợ. Món P6-4 được ghi là "🔧 code, tối ưu" và hoá ra nó **chặn cả một
phase**. Kèm với nó là một câu tôi tự viết: *"cách trả: cần latch-coupling (P4-5) trước"* — và câu
ấy **sai**, vì nó tìm cách trả một món nợ của phase 4 bằng công cụ của phase 4. Thứ tháo được nó
là **ảnh chụp MVCC**, một cơ chế ra đời ở phase 6, **sau** khi món nợ được ghi. Bài học cho cách
ghi nợ: ghi **hiện tượng** và ghi **cách kiểm chứng**; đừng ghi **cách trả**, vì cách trả phụ
thuộc vào những thứ chưa tồn tại lúc ghi.

## Nợ kỹ thuật / để dành cho sau

- [ ] **P7-1** · `keys.Decode` cấp phát **152 B / 2 alloc** mỗi hàng, và nó nằm trên đường đọc
      **nóng nhất** (mỗi hàng của mỗi seq scan). Trả bằng một API giải mã vào buffer có sẵn.
- [ ] **P7-2** · `CreateIndex` back-fill trong **một** transaction: write set của cả bảng nằm
      trong RAM, và không có index build đồng thời. Bảng 10 triệu hàng là hết bộ nhớ.
- [ ] **P7-3** · **Không có DDL locking.** `CreateIndex` chạy song song với DML là hành vi chưa
      định nghĩa.
- [ ] **P7-4** · `FuzzTableIndex` mới **1580 exec** và corpus còn mọc **6.8%/phút** ⇒ chưa bão
      hoà. Cần một lần chạy dài (`-fuzztime 30m`) ở chỗ có thời gian.
- [ ] **P7-5** · Điểm hoà vốn 36.8% chỉ đúng khi **cả cây nằm trong buffer pool**. Đo lại với
      `-frames` nhỏ để cây lớn hơn pool — đó mới là ca mà `random_page_cost = 4` nói tới.
- [ ] **P7-6** · Selectivity dùng **phân bố đều**, không có histogram ⇒ lệch **97x** trên cột
      lệch 99%. `TestEstimateIsWrongOnSkew` sẽ đỏ khi trả món này, và đó là chủ ý.
- [ ] **P7-7** · `DefaultCost` là ba con số **đoán**, và giờ đã biết nó sai vì **hai** lý do độc
      lập: không có I/O thật (4.4x), và không tính tương quan (1.6-2.2x). Cố ý giữ lại làm cột
      `planner(đoán)`. Trả bằng: đo lúc mở database rồi lưu vào catalog (tức `ANALYZE`).
- [ ] **P7-8** · Bất biến "tổng số dư" của workload chuyển tiền là **đối xứng** nên nó gần như mù
      với lost update (đúng 5/6 lần với `accounts=2`). Cần thêm một bất biến **không đối xứng**
      (ví dụ: không tài khoản nào âm) mới đo được sức chặn thật của từng mức isolation.
- [ ] **P7-9** · Chưa có `ORDER BY` dùng index để **bỏ** bước sắp xếp, và chưa có `Filter` đẩy
      xuống dưới `Scan`. Cả hai là phase 8.
