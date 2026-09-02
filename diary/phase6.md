# Phase 6 — Transaction & concurrency control (MVCC + S2PL)

- **Thời lượng dự kiến:** 3-4 ngày · **thực tế:** 1 ngày
- **Bắt đầu:** 2026-09-02 · **Kết thúc:** 2026-09-02
- **Trạng thái:** ✅ xong
- **Commit:** `_(điền sau khi commit)_`

> **Quy tắc ghi nhật ký:** mọi con số, mọi kết luận đều phải kèm **lệnh shell sinh ra nó**
> và **output thật** (dán nguyên, không tóm tắt). Sáu tháng sau đọc lại phải chạy lại được.
> Ghi trong lúc làm, không phải sau khi xong.

## Môi trường

```console
$ uname -srmo && go version && df -hT . | tail -1
Linux 6.6.87.2-microsoft-standard-WSL2 x86_64 GNU/Linux
go version go1.26.2 linux/amd64
/dev/sdd       ext4 1007G   39G  917G   5% /
```

CPU: `12th Gen Intel(R) Core(TM) i5-1235U`, `GOMAXPROCS=6` (theo dòng `cpu:` mà `go test -bench` in ra).

⚠️ WSL2 trên ext4 trong file ảnh đĩa. `fsync` ở đây **không** phải `fsync` trên NVMe thật
(nợ P0-1/P0-4). Phase này còn một lý do nữa để chỉ tin tỉ số: mọi con số về **tranh chấp** phụ
thuộc vào số core và vào scheduler. `246 deadlock` là con số của máy này trong lần chạy này;
`OCC bỏ 26 lượt còn 2PL bỏ 0` là kết luận sống sót qua máy khác.

## Mục tiêu phase

Từ một writer vật lý duy nhất của phase 5 lên **nhiều transaction logic đồng thời**, với bốn mức
cô lập, và tái tạo được từng anomaly ở đúng mức mà lý thuyết nói nó xảy ra. Kèm theo: trả nốt
P1-2b (reader đồng thời), món nợ mà phase 5 để lại và ghi rõ "cách trả: MVCC ở phase 6".

## Câu hỏi phải trả lời được khi xong

Từ ROADMAP:

- Lock khác latch thế nào (lần này đã có code để chỉ tận nơi)?
- S2PL đảm bảo serializable bằng cơ chế gì? Cái giá là gì?
- Visibility rule của MVCC: tuple `(xmin, xmax)` hiển thị với snapshot nào?
- Vì sao trong MVCC reader không chặn writer?
- Write skew là gì, vì sao snapshot isolation **không** chặn được, SSI sửa thế nào?
- Rác MVCC (version cũ) ai dọn? (nối lại với VACUUM ở phase 2)

Năm câu tự thêm trong lúc làm, vì chúng chặn đường:

- Phase 5 chốt **một writer vật lý**. Muốn nhiều transaction logic thì phải bỏ chốt ấy, hay có
  đường khác? Bỏ nó thì pha undo của ARIES còn đúng không?
- `xmax` có thật sự cần không, hay nó là dữ liệu trùng lặp?
- Một transaction **chỉ đọc** có cần một transaction id không?
- Bộ dọn version dùng mốc nào? `min(Xmax)` hay `min(Xmin)` của các snapshot đang sống?
- "Reader không chặn writer" — câu đó đúng đến đâu? Có thế nào để reader **vẫn** làm writer
  chết không?

## Deliverable (bằng chứng đã hiểu)

Bốn bài, và bài (2) và (4) mới làm bài (1) có nghĩa:

1. `make txnlab-anomaly` — bảng 5 anomaly × 4 mức isolation, khớp từng ô với lý thuyết.
2. `make test-txn` — mỗi ô của bảng ấy được khẳng định **theo cả hai chiều**: mức thấp phải
   **để lọt** anomaly, mức cao phải chặn. Một bài test chưa bao giờ đỏ thì chưa phải bằng chứng
   (bài học `crashlab-nowrite` của phase 5).
3. `make txnlab` — N goroutine chuyển tiền: tổng số dư **vỡ** ở hai mức thấp, **giữ được** ở hai
   mức cao; cộng bảng phình version và vacuum.
4. `make fuzz-txn` — encoding chuỗi version phải canonical, và phải sống qua WAL/redo/undo.

## Reproduce toàn bộ phase này

```bash
git checkout <commit của phase 6>
make fmt vet
make test                 # 240 test
go test -race ./internal/... -count=1

# Deliverable
make txnlab               # ba bảng: anomaly, chuyển tiền, phình version
make txnlab-anomaly       # chỉ bảng anomaly (thoát 1 nếu có ô lệch khỏi lý thuyết)
make txnlab-contention    # 2 tài khoản × 12 goroutine — chỗ LẠC QUAN thua BI QUAN
make test-txn             # bảng anomaly khẳng định theo cả hai chiều
make fuzz-txn             # 2 target: codec chuỗi version, và chuỗi version qua crash

# Số đo
make bench-txn            # giá mỗi mức isolation, giá abort, giá phình version, lock manager

# Phase 5 phải không bị hồi quy
make crashlab             # 20 lần kill -9
make crashlab-nowrite     # vẫn PHẢI đỏ
make fuzz-db
```

---

## Nhật ký

### 2026-09-02 — quyết định kiến trúc, trước khi viết dòng nào (lượt code)

Phase 5 để lại một chốt cứng: **một writer vật lý**, do code bắt buộc, `ErrWriterBusy`. Phase 6
cần nhiều transaction cùng chạy. Có hai đường, và chọn sai thì phải viết lại phase 5.

**Đường (a): cho nhiều writer cùng sửa cây.** Đây là cái ROADMAP mặc định nghĩ tới. Nó cần hai
thứ, và cả hai đều đắt:

1. **Latch-coupling** — nợ P4-5, mà `docs/debts.md` ghi rõ là việc của phase 7.
2. **Bỏ pha undo physical.** Đây mới là chỗ chết. Nếu txn A và txn B cùng sửa một page, rồi A
   abort, thì pha undo của phase 5 dán ảnh-trước của A đè lên page — **xoá luôn việc của B**.
   Ảnh-trước là ảnh của cả page, nó không biết byte nào của ai.

Chỗ thứ (2) là một điều tôi chỉ hiểu ra khi bị chặn ở đây: **đó chính là lý do Postgres không có
pha undo.** Postgres không rollback bằng cách dán lại byte cũ; nó để tuple cũ nằm đó và ghi vào
clog rằng transaction ấy đã abort. Rollback thành một phép ghi 2 bit. Còn ARIES undo physical
buộc mỗi page chỉ được thuộc về một transaction dở dang tại một thời điểm.

**Đường (b): deferred write.** Mỗi transaction logic gom thay đổi vào một **write set trong
RAM**; lúc commit mới xin quyền ghi rồi áp **tất cả** trong **một** transaction vật lý của phase
5. Đọc thì đọc thẳng từ cây, qua luật visibility.

Chọn (b). Hệ quả:

| | Đường (a) | Đường (b) — đã chọn |
|---|---|---|
| WAL / checkpoint / recovery / undo của phase 5 | phải viết lại | **không đổi một dòng** |
| Latch-coupling (P4-5) | bắt buộc, ngay | vẫn để cho phase 7 |
| Abort | phải chạy undo physical | **không I/O, không log, không undo** |
| Nhiều writer thật song song ở tầng vật lý | có | không — tuần tự hoá ở lúc commit |

Nói thẳng cái giá, vì nó là thật: write set nằm trong RAM nên **transaction ghi không được to**,
và writer vẫn tuần tự hoá ở lúc commit. Phase 6 mua **tính đúng đắn khi có đồng thời**, không mua
throughput ghi.

**Đang nghĩ gì:** nếu (b) làm abort thành miễn phí, thì cái `undoChain` + CLR mà phase 5 dựng cực
khổ chỉ còn phục vụ **crash**, không còn phục vụ `Abort()` của người dùng nữa. Đó là một sự thật
đáng ghi: hai thứ trông giống nhau ("hoàn tác") mà cơ chế đúng lại khác nhau tuỳ tầng.

---

### 2026-09-02 — bỏ `xmax`, và câu hỏi xid cho transaction chỉ đọc

Sách vẽ tuple MVCC là `(xmin, xmax)`. Tôi định làm y thế, rồi nhận ra: nếu chuỗi version xếp
**mới nhất trước** trong cùng một entry, thì `xmax` của bản thứ *i* **luôn** bằng `xmin` của bản
thứ *i-1*. Hai nguồn sự thật cho một sự việc là hai chỗ để lệch nhau — đúng câu tôi đã viết vào
`wal/record.go` ở phase 5 khi xoá cờ `FlagHasBefore`. Bỏ `xmax`. Xoá là một version có cờ
`Deleted` (tombstone), không phải một `xmax` đặt lên bản cũ.

Cũng không có **clog**. Lý do: write set chỉ được áp vào cây *khi* transaction commit, nên
"version có mặt trong cây" **đã có nghĩa là** "transaction của nó đã commit". Postgres cần clog
vì nó ghi tuple vào page *trước* khi biết mình có commit được không.

Luật visibility gọn lại còn ba dòng:

```go
func (s Snapshot) Visible(xmin uint64) bool {
	if xmin >= s.Xmax { return false }  // bắt đầu sau ta
	if xmin < s.Xmin  { return true }   // già hơn mọi thứ còn có thể đang chạy
	return !s.Active[xmin]              // đang chạy lúc ta chụp -> commit sau ta
}
```

**Đang nghĩ gì:** `Snapshot.Xmin = min(Active ∪ {Xmax})` chính là `OldestXmin` của Postgres, và
nó phải là mốc của bộ dọn. Tôi ghi lại nghi vấn này trước khi viết `Prune`, vì cảm giác là chỗ
đây dễ sai: dùng `min(Xmax)` thay `min(Xmin)` thì trông vẫn hợp lý.

---

### 2026-09-02 — bốn bug đầu, lượt chạy thứ nhất

Chạy test lần đầu sau khi viết xong. Bốn thứ đỏ, và chúng ở bốn tầng khác nhau.

#### (1) Transaction chỉ đọc vẫn tiêu một xid

```console
$ go test ./internal/txn/ -run TestReadOnlyCommitTouchesNothing -count=1
--- FAIL: TestReadOnlyCommitTouchesNothing
    50 transaction chỉ đọc sinh 220 byte log, phải là 0
```

**Đọc kết quả:** xid được cấp theo lô 64 cái và mốc lô được ghi bền vào meta. Nên cứ **64 lượt
đọc** là **một lần ghi log** — một database chỉ-đọc vẫn làm bẩn đĩa.

**Đã sửa:** tách danh tính khỏi id bền, đúng cái Postgres gọi là **virtual transaction id**:

- `vnext` — bộ đếm **chỉ nằm trong RAM**. Là khoá của bảng active, là danh tính trong lock
  manager, là tuổi khi bắt deadlock.
- `next`/`persisted` — không gian xid **bền**, cấp **lười ở lần ghi đầu tiên**.

Một transaction chỉ đọc không bao giờ được cấp xid, nên nó **không chạm đĩa lần nào**.

**Đang nghĩ gì:** đây là bug đáng giá nhất của lượt này, vì nó không phải lỗi cài đặt mà là một
khái niệm tôi chưa có. Tôi đã đọc "virtual xid" trong tài liệu Postgres và không hiểu nó để làm
gì. Giờ thì cái test in ra con số 220 byte và nó tự giải thích.

#### (2) `gcXmin` thiếu vế "xid của chính mình"

```console
$ go test ./internal/txn/ -run TestReadYourOwnWrites -count=1
--- FAIL: TestReadYourOwnWrites
    txn: thu hồi khóa "k3": btree: không có khóa: "k3"
```

**Đọc kết quả:** `Begin` tăng `s.next` **trước** khi chụp snapshot, nên `gcXmin` của transaction
lớn hơn xid của chính nó. Kết quả: transaction ghi một tombstone, rồi bộ dọn opportunistic (chạy
trong cùng `apply`) coi tombstone ấy là rác đã chết, `Chain.Dead` trả true, và `apply` đi xoá một
khóa **chưa từng tồn tại** trong cây.

**Đã sửa:** gộp xid của chính mình vào mốc, hai vế, thiếu vế nào cũng sai:

```go
func (t *Txn) setGCXmin() {
	g := uint64(math.MaxUint64)
	if t.iso.usesSnapshot() { g = t.snap.Xmin }
	if x := t.xid.Load(); x != 0 && x < g { g = x }
	t.gcXmin.Store(g)
}
```

Và theo dõi `inTree` để `ptx.Delete` chỉ chạy khi khóa có thật.

#### (3) Trần thật của chuỗi version là trần BYTE, không phải `MaxVersions`

```console
$ go test ./internal/txn/ -run TestChainFullIsReported -count=1
--- FAIL: TestChainFullIsReported
    version thứ 41: btree: entry lớn quá: 2 + 3 + 2051 = 2056 > 2028
```

**Đọc kết quả:** `MaxVersions = 64` **không bao giờ** chạm tới với value 40 byte. Cái chạm trước
là `btree.MaxEntrySize` = 2028 byte của phase 4. Lỗi hiện ra dưới dạng thông báo của tầng B+Tree —
tức nói đúng **triệu chứng** mà không nói **nguyên nhân**.

**Đã sửa:** kiểm cả hai trần trong `Txn.apply`, để thông báo mang tên đúng của việc đã xảy ra:

```go
if n := 2 + len(key) + len(buf); len(nc) > MaxVersions || n > btree.MaxEntrySize {
	return fmt.Errorf("%w: khóa %q có %d version / %d byte ...", ErrChainFull, ...)
}
```

**Đang nghĩ gì:** đây là **giới hạn cứng của MVCC-tại-chỗ**, và nó là lý do thật sự vì sao DB
thật không để bản cũ tại chỗ: Postgres tạo tuple mới ở **page khác**, InnoDB đẩy bản cũ sang
**undo segment**. Tôi chọn tại chỗ vì nó làm luật visibility hiện ra rõ nhất. Cái giá vừa được
đo bằng một con số.

#### (4) `time.AfterFunc` đặt một lần là một lỗi treo thật

Phát hiện khi đi truy một bài test treo (xem mục *Bug của bộ đo* bên dưới). Bản đầu của
`lock.Manager.Acquire` đặt **một** `time.AfterFunc` ở đầu hàm rồi vào vòng `cv.Wait()`. Chỉ cần
**một lần bị `Broadcast` oan** là timer đã tiêu, và lần `Wait()` sau đó không còn ai đánh thức →
treo tới hết `-timeout` của `go test`.

**Đã sửa:** hẹn giờ **lập lại mỗi vòng**, tính theo deadline còn lại:

```go
rem := time.Until(deadline)
if rem <= 0 { delete(m.waits, txn); m.st.Timeouts++; return fmt.Errorf("%w: ...", ErrTimeout) }
wake := time.AfterFunc(rem, func() { m.mu.Lock(); m.cv.Broadcast(); m.mu.Unlock() })
m.cv.Wait()
wake.Stop()
```

---

### 2026-09-02 — chuỗi ba giả thuyết: vì sao Serializable bỏ mất transaction

Đây là mục đáng giá nhất của cả phase, vì **giả thuyết đầu tiên sai, giả thuyết thứ hai sai một
nửa, và chỉ cái thứ ba đóng được vấn đề** — mà cả ba đều nghe hợp lý.

Hiện tượng:

```console
$ go test ./internal/txn/ -run TestTransferSerializableCommitsEverything -count=1
--- FAIL: TestTransferSerializableCommitsEverything
    serializable: 70/160 lượt chuyển bỏ cuộc sau 50 lần thử, hết 0.19s
```

**Đọc kết quả:** 0.19 giây cho 50 lần thử × 160 lượt. Con số **thời gian** là chỗ đáng nghi
trước tiên: 50 lần thử trong 0.19s nghĩa là mỗi lần thử chưa tới 25µs — tức chúng **không chờ
gì cả**, chỉ quay.

#### Giả thuyết 1 (SAI): starvation, vì nạn nhân deadlock luôn là transaction trẻ nhất

Trong `lock.findCycle` tôi đã tự viết một chú thích rằng "chọn nạn nhân trẻ nhất ⇒ không
starvation". Chú thích ấy **sai**, và nó sai ngay khi có vòng lặp thử lại: mỗi lần thử lại,
transaction nhận một `vid` **mới**, nên nó **luôn** là đứa trẻ nhất, nên nó **luôn** là nạn
nhân. Nó không bao giờ già đi.

Sửa: tách **tuổi** khỏi **danh tính**. `lock.Manager.SetAge(vid, age)`, và `Store.Update` giữ
nguyên tuổi qua các lần thử lại:

```go
if age == 0 { age = t.vid }
s.lk.SetAge(t.vid, age)
```

Đo lại:

```console
$ go test ./internal/txn/ -run TestTransferSerializableCommitsEverything -count=1
--- FAIL: TestTransferSerializableCommitsEverything
    serializable: 68/160 lượt chuyển bỏ cuộc sau 50 lần thử
```

**Đọc kết quả:** 70 → **68**. Gần như không đổi. Giả thuyết bị bác bằng số, không bằng lý lẽ.
Starvation **có thật** (và bản sửa vẫn giữ, vì nó vẫn đúng), nhưng nó **không phải** nguyên nhân
chính.

#### Giả thuyết 2 (ĐÚNG, nhưng chưa đủ): conversion deadlock

Nhìn lại `transferOnce`: nó `Get(from)`, `Get(to)`, tính, rồi `Put` cả hai. Ở mức Serializable,
`Get` lấy khóa **S**, `Put` nâng lên **X**.

Hai transaction cùng đọc cùng một khóa thì **cả hai** giữ S. Rồi cả hai xin X. Cả hai phải chờ
nhau nhả S. Đây không phải deadlock *có thể xảy ra* — nó là deadlock **chắc chắn xảy ra**, và nó
có tên riêng trong sách: **conversion deadlock**.

Sửa: thêm `Txn.GetForUpdate` — đọc mà lấy **X** ngay. Đây đúng là `SELECT ... FOR UPDATE`, và
đây là lần đầu tôi hiểu vì sao câu SQL ấy tồn tại.

```console
$ go test ./internal/txn/ -run TestTransferSerializableCommitsEverything -count=1
--- FAIL: TestTransferSerializableCommitsEverything
    serializable: 10/160 lượt chuyển bỏ cuộc sau 50 lần thử
```

**Đọc kết quả:** 68 → **10**. Đây là nguyên nhân chính.

**Đang nghĩ gì:** cái giá thật của 2PL không phải là "chậm hơn MVCC". Cái giá thật là nó buộc
**ứng dụng** phải khai báo trước ý định ghi. MVCC không đòi gì cả — và đó là lý do nó thắng về
mặt trải nghiệm người dùng, chứ không hẳn về mặt throughput.

#### Giả thuyết 3: 10 lượt còn lại là thrash, và thiếu backoff

50 lần thử hết 0.22s ⇒ mọi contender thử lại **đồng pha**. Chúng va nhau, cùng abort, cùng thử
lại ngay, va lại đúng như thế.

Sửa: backoff có **jitter**. Phần *ngẫu nhiên* mới là phần phá được sự đồng pha, không phải phần
*chờ*:

```go
func backoff(attempt int) {
	d := time.Duration(attempt+1) * 100 * time.Microsecond
	if d > 5*time.Millisecond { d = 5 * time.Millisecond }
	time.Sleep(time.Duration(rand.Int63n(int64(d) + 1)))
}
```

```console
$ go test ./internal/txn/ -run 'TestTransfer' -count=1
ok  	minidb/internal/txn	3.1s
```

10 → **0**. Serializable commit đủ 360/360 và 160/160.

---

### 2026-09-02 — "reader không chặn writer" đúng đến đâu

Bài test tôi viết ra để chứng minh P1-2b đã trả **thất bại**, và nó thất bại theo cách hay:

```console
$ go test ./internal/txn/ -run TestSnapshotReadDoesNotBlockWriter -count=1
--- FAIL: TestSnapshotReadDoesNotBlockWriter
    writer thứ 63: txn: chuỗi version đã đầy, không dọn thêm được
```

**Đọc kết quả:** tôi khẳng định "reader không chặn writer" và cây tự bác lại. Reader **không**
lấy khóa của writer — đúng. Nhưng reader cũ **ghim horizon**, nên chuỗi version của khóa đang bị
ghi lại **không dọn được**, và ở lần ghi thứ 63 nó chạm trần rồi writer chết.

Đây **không phải bug**, đây là một sự thật về MVCC-tại-chỗ. Nên sửa **bài test**, không sửa code,
và sửa theo hướng nói ra cả hai vế:

- `TestSnapshotReadDoesNotBlockWriter` — ghi 500 khóa **khác nhau** cộng `MaxVersions/2` lần vào
  cùng một khóa. Khẳng định đúng cái nó khẳng định được: reader không lấy khóa của writer.
- `TestOldReaderStarvesWriterOnSameKey` (mới) — khẳng định writer **chết** ở trần, và chỉ ra
  thuốc duy nhất: đóng reader → `Vacuum`.

**Đang nghĩ gì:** phát biểu đúng phải là *"reader không **lấy khóa** của writer"*, không phải
*"reader không **làm hại** writer"*. Câu thứ hai là sai, và nó sai ở mọi bản MVCC — Postgres gọi
biểu hiện của nó là **bloat**, và `idle_in_transaction_session_timeout` tồn tại vì lý do này.

---

### 2026-09-02 — bug của bộ đo, và một bug thật lộ ra sau nó

Bảng anomaly treo hết 300 giây ở đúng một ô: `non-repeatable-read` × `read-uncommitted`.

Truy được nguyên nhân là một **kênh một giá trị bị nhận hai lần**: `probeNonRepeatable` dùng
`select` có timeout, rồi cuối hàm vẫn `<-done` lần nữa. Khi `select` **không** timeout thì lần
`<-done` thứ hai chờ mãi mãi. Sửa bằng một biến `blocked bool` để `<-done` cuối chỉ chạy khi
`select` đã timeout thật. Cùng lỗi ấy trong `probePhantom`.

Chi tiết đáng ghi về **dụng cụ**: `rtk` lọc mất dump panic của `go test`, nên phải đi đọc
`~/.local/share/rtk/tee/*.log` bằng python mới thấy được stack. Từ đó về sau, mọi lệnh cần output
nguyên văn trong phase này đều chạy qua `rtk proxy`.

Và chính lúc đọc stack ấy mới lộ ra bug (4) ở trên — `time.AfterFunc` đặt một lần. Đó là một lỗi
**treo thật của code**, và nó chỉ hiện ra vì tôi đang đi truy một lỗi treo của **bài test**.

Bug thứ hai của bộ đo, cùng họ: `TestDeadlockUpgradeCycle` và `TestDeadlockThreeCycle` timeout.
Hợp đồng của lock manager nói rõ: transaction **nhận `ErrDeadlock` thì phải abort**. Bài test
nhận `ErrDeadlock` rồi... không nhả khóa của nạn nhân. Ở ca ba vòng thì **người thắng** lại thành
vật cản mới. Sửa bằng một helper `tryLock` tự gọi `ReleaseAll` khi thấy `ErrDeadlock`.

**Đang nghĩ gì:** phase 5 tôi ghi "tìm ra hai lỗi của bộ đo trên một lỗi của code". Phase 6 lặp
lại đúng tỉ lệ ấy. Nó không còn là tai nạn; nó là hình dạng bình thường của việc đo một hệ đồng
thời.

---

### 2026-09-02 — lượt chạy: bảng anomaly

```console
$ go run ./cmd/txnlab -work anomaly
== anomaly nào xảy ra ở mức nào ==
  X = anomaly XẢY RA (mức này không chặn)   . = bị chặn

anomaly               read-uncomm       read-comm         repeat-read       serializable
----------------------------------------------------------------------------------------------
dirty-read            X                 .                 .                 .
non-repeatable-read   X                 X                 .                 .
phantom               X                 X                 .                 .
lost-update           X                 X                 .                 .
write-skew            X                 X                 X                 .
```

**Đọc kết quả:** khớp lý thuyết từng ô. Hàng đáng chú ý nhất là **write-skew**: snapshot
isolation (`repeatable-read`) **không** chặn nó, vì hai transaction ghi **hai khóa khác nhau**
nên chẳng có xung đột ghi-ghi nào để phát hiện. Chặn nó cần biết về chỗ **đọc** — tức lock
(S2PL) hoặc SSI.

Hai chỗ khác dự kiến, cả hai đều nói điều gì đó về kiến trúc:

- **`repeatable-read` chặn cả phantom.** Nó **mạnh hơn** RR của chuẩn SQL, vì snapshot là snapshot
  của cả cây, không phải của từng dòng đã đọc. Đây là hành vi của Postgres, không phải của chuẩn.
- **`lost-update` bị chặn ở `repeatable-read`** nhờ *first-committer-wins*; nhưng ở
  `read-committed` thì **lọt** — và điều kiện chấm của bài kiểm là `final != 100-10*commits`,
  **không** phải `final != 80`. Lý do: ở mức cao đúng một transaction commit, nên **90 mới là
  đáp án đúng**. Nếu chấm bằng `!= 80` thì mức cao cũng bị tính là "vỡ".

---

### 2026-09-02 — lượt chạy: chuyển tiền, và chỗ dirty read phải được THÊM VÀO

```console
$ go run ./cmd/txnlab
== chuyển tiền: 6 goroutine × 200 lượt trên 8 tài khoản, mỗi lượt 7 ==
   bất biến: tổng số dư luôn = 80000

read-uncommitted   commit  1200/ 1200  bỏ    0  tổng   80105/  80000  VỠ (lệch +105)
                        807 txn/s   xung đột 0   deadlock 0   thử lại 0   version ghi 2408, dọn 2389

read-committed     commit  1200/ 1200  bỏ    0  tổng   80021/  80000  VỠ (lệch +21)
                        813 txn/s   xung đột 0   deadlock 0   thử lại 0   version ghi 2408, dọn 2388

repeatable-read    commit  1200/ 1200  bỏ    0  tổng   80000/  80000  GIỮ ĐƯỢC
                        796 txn/s   xung đột 1216   deadlock 0   thử lại 1216   version ghi 2408, dọn 2392

serializable       commit  1200/ 1200  bỏ    0  tổng   80000/  80000  GIỮ ĐƯỢC
                        800 txn/s   xung đột 0   deadlock 246   thử lại 97   version ghi 2408, dọn 2392
```

**Đọc kết quả:** hai mức thấp làm **lệch** tổng số dư — tiền bốc hơi hoặc sinh ra từ không khí.
Hai mức cao giữ đúng 80000. Throughput bốn mức **gần như bằng nhau** (796-813 txn/s) vì ở mức
tranh chấp này thứ quyết định là **fsync lúc commit**, không phải cơ chế cô lập.

Chú ý: `repeatable-read` phải thử lại **1216** lần cho 1200 lượt, còn `serializable` chỉ **97**.
Bên lạc quan làm việc nhiều hơn bên bi quan ngay ở mức tranh chấp trung bình.

**Đang nghĩ gì:** cái làm tôi bất ngờ nhất trong cả phase này là **dirty read phải được viết
thêm mã để tái tạo**. Trong kiến trúc deferred write, write set là RAM riêng của từng transaction
tới lúc commit, nên đọc được thay đổi chưa commit là việc **bất khả** — không phải việc "được
phép". `Store.peekDirty` tồn tại **thuần** để dựng lại một anomaly mà kiến trúc đã tự chặn.

Điều đó nói lên hai chuyện. Một, `read-uncommitted` là mức **khó cài nhất** ở đây, ngược hoàn
toàn với trực giác. Hai, và quan trọng hơn: nó chứng minh mấy dòng đầu file — anomaly nào xảy ra
là **hệ quả của kiến trúc**, không phải của một cái công tắc. Nên `TestReadUncommittedNeedsExtraCode`
khẳng định bộ đếm `DirtyReads` tăng ở **đúng một** mức.

---

### 2026-09-02 — lượt chạy: chỗ MVCC thua lock

```console
$ go run ./cmd/txnlab -work transfer -accounts 2 -workers 12 -ops 100
== chuyển tiền: 12 goroutine × 100 lượt trên 2 tài khoản, mỗi lượt 7 ==
   bất biến: tổng số dư luôn = 20000

read-uncommitted   commit  1200/ 1200  bỏ    0  tổng   20007/  20000  VỠ (lệch +7)
                        831 txn/s   xung đột 0   deadlock 0   thử lại 0   version ghi 2402, dọn 2396

read-committed     commit  1200/ 1200  bỏ    0  tổng   20140/  20000  VỠ (lệch +140)
                        748 txn/s   xung đột 0   deadlock 0   thử lại 0   version ghi 2402, dọn 2398

repeatable-read    commit  1174/ 1200  bỏ   26  tổng   20000/  20000  GIỮ ĐƯỢC
                        741 txn/s   xung đột 2670   deadlock 0   thử lại 2670   version ghi 2350, dọn 2346

serializable       commit  1200/ 1200  bỏ    0  tổng   20000/  20000  GIỮ ĐƯỢC
                        756 txn/s   xung đột 0   deadlock 122   thử lại 122   version ghi 2402, dọn 2398
```

**Đọc kết quả:** đây là **điểm giao**. Ở 2 tài khoản × 12 goroutine:

- `repeatable-read` (lạc quan) **bỏ 26/1200 lượt** và đốt **2670** lần thử lại.
- `serializable` (bi quan) commit **1200/1200** với **122** deadlock.

Tỉ số công việc vô ích: **2670/122 = 21.9x**, và bên thua là bên lạc quan.

**Vì sao:** điều khiển đồng thời lạc quan **không có hàng đợi và không có thứ tự**. Ai va thì
abort rồi thử lại, và không có gì đảm bảo lần sau đến lượt mình. S2PL thì có hàng đợi: chờ xong
là được vào. Nên câu **"MVCC luôn nhanh hơn locking" là một câu sai**, và `make txnlab-contention`
là cái lệnh chứng minh nó sai.

Đây cũng là chỗ một bài test của tôi đã tố oan: `TestTransferRepeatableReadRetriesNotLoses` đỏ ở
299/300 với 4 tài khoản × 6 goroutine. Không sửa code — sửa bài test cho nói đúng sự thật: nó
chuyển sang 16 tài khoản, và một bài mới `TestOptimisticDegradesUnderContention` đo **chính điểm
giao** ở 2 tài khoản × 8 goroutine bằng một khẳng định **so sánh**.

---

### 2026-09-02 — lượt chạy: phình version, và câu trả lời cho "vì sao Postgres cần VACUUM"

```console
$ go run ./cmd/txnlab -work bloat
== phình version: 200 khóa, mỗi khóa ghi lại 20 lần ==

reader cũ CÒN mở : 200 khóa, 4000 version, chuỗi dài nhất 20, 54200 byte value
vacuum khi reader còn mở: dọn 0 version (horizon 1) — đúng như dự đoán: không dọn được gì
đóng reader rồi vacuum : dọn 3800 version, 0 khóa được thu hồi
sau vacuum       : 200 khóa, 200 version, chuỗi dài nhất 1, 3000 byte value

tỉ số phình = 18.07x  (byte value trước / sau)
thời gian ghi 3800 lượt: 7.67424476s
```

**Đọc kết quả:** **một** transaction chỉ đọc, mở suốt, làm cây phình **18.07x** và làm vacuum dọn
được **0** version. `horizon 1` là bằng chứng trực tiếp: mốc bị ghim ở snapshot của reader ấy.

Đây là toàn bộ câu trả lời cho câu hỏi tôi đặt ở **phase 2** ("vì sao Postgres cần VACUUM") và
lý do `idle_in_transaction` là thứ phải theo dõi trên production. Không phải vì bộ dọn yếu — mà
vì **không có gì để dọn một cách hợp pháp**.

---

### 2026-09-02 — lượt chạy: bench, và một "hiện tượng" hoá ra là noise

Ở lượt viết code tôi chạy bench với `-benchtime=200x` cho nhanh và thấy một thứ vô lý:
`depth=60` **nhanh hơn** `depth=32`. Theo luật của repo này thì thấy số vô lý là **nghi bài đo
trước, nghi máy sau**. Chạy lại đủ vòng:

```console
$ go test ./internal/txn/ -run '^$' -bench 'Get|Scan' -benchtime=200000x -count=3
goos: linux
goarch: amd64
pkg: minidb/internal/txn
cpu: 12th Gen Intel(R) Core(TM) i5-1235U
BenchmarkGetPerLevel/read-uncommitted-6         	  200000	       355.7 ns/op
BenchmarkGetPerLevel/read-uncommitted-6         	  200000	       360.8 ns/op
BenchmarkGetPerLevel/read-uncommitted-6         	  200000	       361.3 ns/op
BenchmarkGetPerLevel/read-committed-6           	  200000	       360.8 ns/op
BenchmarkGetPerLevel/read-committed-6           	  200000	       350.1 ns/op
BenchmarkGetPerLevel/read-committed-6           	  200000	       381.6 ns/op
BenchmarkGetPerLevel/repeatable-read-6          	  200000	       327.8 ns/op
BenchmarkGetPerLevel/repeatable-read-6          	  200000	       322.3 ns/op
BenchmarkGetPerLevel/repeatable-read-6          	  200000	       324.7 ns/op
BenchmarkGetPerLevel/serializable-6             	  200000	       763.4 ns/op
BenchmarkGetPerLevel/serializable-6             	  200000	       756.7 ns/op
BenchmarkGetPerLevel/serializable-6             	  200000	       761.8 ns/op
BenchmarkGetChainDepth/depth=1/newest-6         	  200000	       173.2 ns/op
BenchmarkGetChainDepth/depth=1/newest-6         	  200000	       167.2 ns/op
BenchmarkGetChainDepth/depth=1/newest-6         	  200000	       164.3 ns/op
BenchmarkGetChainDepth/depth=1/oldest-6         	  200000	       176.1 ns/op
BenchmarkGetChainDepth/depth=1/oldest-6         	  200000	       172.9 ns/op
BenchmarkGetChainDepth/depth=1/oldest-6         	  200000	       171.9 ns/op
BenchmarkGetChainDepth/depth=8/newest-6         	  200000	       338.9 ns/op
BenchmarkGetChainDepth/depth=8/newest-6         	  200000	       327.1 ns/op
BenchmarkGetChainDepth/depth=8/newest-6         	  200000	       326.2 ns/op
BenchmarkGetChainDepth/depth=8/oldest-6         	  200000	       334.7 ns/op
BenchmarkGetChainDepth/depth=8/oldest-6         	  200000	       344.3 ns/op
BenchmarkGetChainDepth/depth=8/oldest-6         	  200000	       322.2 ns/op
BenchmarkGetChainDepth/depth=32/newest-6        	  200000	       896.3 ns/op
BenchmarkGetChainDepth/depth=32/newest-6        	  200000	       901.4 ns/op
BenchmarkGetChainDepth/depth=32/newest-6        	  200000	       801.7 ns/op
BenchmarkGetChainDepth/depth=32/oldest-6        	  200000	       862.9 ns/op
BenchmarkGetChainDepth/depth=32/oldest-6        	  200000	       867.9 ns/op
BenchmarkGetChainDepth/depth=32/oldest-6        	  200000	       850.6 ns/op
BenchmarkGetChainDepth/depth=60/newest-6        	  200000	      1434 ns/op
BenchmarkGetChainDepth/depth=60/newest-6        	  200000	      1444 ns/op
BenchmarkGetChainDepth/depth=60/newest-6        	  200000	      1384 ns/op
BenchmarkGetChainDepth/depth=60/oldest-6        	  200000	      1476 ns/op
BenchmarkGetChainDepth/depth=60/oldest-6        	  200000	      1449 ns/op
BenchmarkGetChainDepth/depth=60/oldest-6        	  200000	      1514 ns/op
BenchmarkScan-6                                 	  200000	    732477 ns/op	      2000 keys/scan
BenchmarkScan-6                                 	  200000	    715140 ns/op	      2000 keys/scan
BenchmarkScan-6                                 	  200000	    688600 ns/op	      2000 keys/scan
PASS
ok  	minidb/internal/txn	450.630s
```

**Đọc kết quả:** ba điều.

1. **"Hiện tượng" biến mất.** 171 → 331 → 866 → 1450 ns, đơn điệu. 200 vòng **không phải một
   phép đo**. Đây là lần thứ hai trong repo này tôi dính đúng cái bẫy của phase 0.
2. **`newest` ≈ `oldest` ở mọi độ sâu** (1450 vs 1480 ở depth=60, tức 0.98x). Nếu chi phí nằm ở
   vòng lặp visibility thì đọc bản **cũ nhất** phải đắt hơn đọc bản **mới nhất** rõ rệt. Nó
   không. Vậy chi phí nằm ở `DecodeChain` — nó giải mã **trọn chuỗi** trước khi ai đó hỏi cần
   version nào. → nợ P6-2, giải mã lười.
3. **`serializable` đắt gấp 2.35x `repeatable-read`** trên đường đọc (762 vs 325 ns). Đó là giá
   thật của việc lấy một khóa S ở **mỗi** lần đọc — và nó là con số duy nhất trong phase này
   nói được "cái giá của S2PL" mà không lẫn với fsync.

```console
$ go test ./internal/txn/ -run '^$' -bench 'Commit|Abort' -benchtime=500x
BenchmarkCommitPerLevel/read-uncommitted-6         	     500	   1186290 ns/op
BenchmarkCommitPerLevel/read-committed-6           	     500	   1150252 ns/op
BenchmarkCommitPerLevel/repeatable-read-6          	     500	   1182085 ns/op
BenchmarkCommitPerLevel/serializable-6             	     500	   1189498 ns/op
BenchmarkAbort/keys=0-6                            	     500	       293.2 ns/op
BenchmarkAbort/keys=1-6                            	     500	     20791 ns/op
BenchmarkAbort/keys=100-6                          	     500	     39939 ns/op
PASS
ok  	minidb/internal/txn	2.537s
```

**Đọc kết quả:** đây là bảng số đẹp nhất của phase.

- **Commit giống nhau ở cả bốn mức** (1150-1189 µs, lệch 1.03x): mức cô lập **miễn phí** lúc
  commit, vì cái quyết định là **một cái fsync**.
- **Abort 0 khóa = 293 ns.** Commit/abort = **1186290/293.2 = 4046x**. Đây là con số của MVCC:
  hoàn tác không có I/O, không có log, không có undo — chỉ ném write set đi.
- **Abort 1 khóa = 20791 ns, gấp 71x abort 0 khóa.** Vì sao? Ca `keys=0` được thêm vào **đúng
  để** trả lời câu này: chỉ cần ghi **một** khóa là transaction phải xin **một xid bền**, và xid
  cấp theo lô 64 nên nó gánh **1/64 của một fsync**. Kiểm: `1186290/64 = 18536 ns`, đo được
  `20791 ns`. **Khớp.** Một giả thuyết được xác nhận bằng số học, không bằng cảm giác.
- **Abort 100 khóa chỉ 1.9x abort 1 khóa** (39939 vs 20791): việc ném write set đi gần như miễn
  phí, đúng như thiết kế nói.

```console
$ go test ./internal/lock/ -run '^$' -bench . -benchmem
BenchmarkAcquireUncontended-6   	 9136304	       128.4 ns/op	      64 B/op	       1 allocs/op
BenchmarkAcquireShared/holders=1-6         	 7395241	       162.3 ns/op	      64 B/op	       1 allocs/op
BenchmarkAcquireShared/holders=16-6        	 3866162	       310.2 ns/op	      64 B/op	       1 allocs/op
BenchmarkAcquireShared/holders=256-6       	  508204	      2201 ns/op	      64 B/op	       1 allocs/op
BenchmarkAcquireDisjoint/holders=1-6       	 7244980	       168.7 ns/op	      64 B/op	       1 allocs/op
BenchmarkAcquireDisjoint/holders=16-6      	 3023480	       410.7 ns/op	      64 B/op	       1 allocs/op
BenchmarkAcquireDisjoint/holders=256-6     	  332197	      3762 ns/op	      64 B/op	       1 allocs/op
BenchmarkOverlaps-6                        	100000000	        13.34 ns/op	       0 B/op	       0 allocs/op
PASS
ok  	minidb/internal/lock	11.021s
```

**Đọc kết quả:** `AcquireDisjoint` 256/1 = **3762/168.7 = 22.3x** với **256 khóa không chồng
nhau** — tức không có tranh chấp nào, chỉ có việc **đi hết danh sách** để biết là không tranh
chấp. Đó là cái giá của việc lock manager không có **chỉ mục theo đối tượng** → nợ P6-3. Bản thân
phép `Overlaps` là 13.34 ns và **0 alloc**, nên vấn đề là O(n), không phải hằng số.

---

### 2026-09-02 — lượt chạy: fuzzer tìm ra thứ mắt không thấy

Đây là mục cuối, và nó đổi một dòng code mà tôi đã đọc lại ba lần mà không thấy gì sai.

Hai target mới. Target `FuzzChainCodec` kiểm hai chuyện: encoding phải **canonical** (giải mã
được thì mã hoá lại phải ra đúng byte cũ), và **bất biến định nghĩa của `Prune`** — với mọi
horizon `h` và mọi snapshot có `Xmin >= h`:

```go
c.Visible(s) == c.Prune(h).Visible(s)
```

Đây đúng là chỗ con bug `min(Xmax)` thay vì `min(Xmin)` đã nằm. Ở lượt code tôi phải **dựng bằng
tay** hình ba transaction gối nhau để bắt nó (`TestPruneHorizonIsXminNotXmax`). Fuzzer không cần
biết trước hình nào.

```console
$ go test ./internal/txn/ -run '^$' -fuzz FuzzChainCodec -fuzztime 120s -fuzzminimizetime 1s
fuzz: elapsed: 0s, gathering baseline coverage: 4/4 completed, now fuzzing with 6 workers
fuzz: elapsed: 3s, execs: 30553 (12066/sec), new interesting: 43 (total: 47)
--- FAIL: FuzzChainCodec (2.53s)
    --- FAIL: FuzzChainCodec (0.00s)
        fuzz_test.go:47: mã hoá lại khác byte gốc:
             gốc 013030303030303030300000
             lại 010030303030303030300000
```

**Đọc kết quả:** **3 giây.** Byte thứ hai: `0x30` vào, `0x00` ra. `DecodeChain` chỉ xét
`f&flagDeleted` nên mọi bit cờ khác bị **nuốt im lặng**. Hệ quả không phải là "hơi bẩn": một
chuỗi hỏng đi qua `ChainStats` mà `BadChains` **vẫn bằng 0**, tức cái đồng hồ tôi dựng riêng để
phát hiện chuỗi hỏng lại **không thấy** loại hỏng này.

Sửa: mặt nạ `flagsKnown`, bit ngoài mặt nạ → error. Cùng lý lẽ với vùng dự trữ trong header WAL
của phase 5: kiểm tốn một phép AND, **không** kiểm tốn một bản ghi tương lai bị bản cũ đọc sai
lặng lẽ.

Vá xong, chạy lại, và seed **đầu tiên** đỏ:

```console
$ go test ./internal/txn/ -count=1
--- FAIL: FuzzChainCodec/seed#0
    chuỗi tự dựng lại không giải mã được: txn: chuỗi version hỏng: version 1 là tombstone nhưng nói 1 byte thân
```

**Đọc kết quả:** và đây mới là bug thật, sâu hơn cái đầu. `Encode` ghi `len(v.Val)` **kể cả với
tombstone**, dù tài liệu ngay trên `type Version` nói "Val của bản Deleted luôn nil". Nghĩa là:
**bộ mã hoá sinh ra được thứ mà bộ giải mã của chính nó từ chối.** Tài liệu nói một luật mà không
có dòng code nào thực thi luật ấy — đúng cái mà nguyên tắc "mỗi invariant chỉ có một điểm thực
thi" của phase 5 tồn tại để chặn.

Sửa ở **Encode** (chuẩn hoá tombstone thành thân rỗng), không chỉ ở Decode. Rồi:

```console
$ go test ./internal/txn/ -run '^$' -fuzz FuzzChainCodec -fuzztime 120s -fuzzminimizetime 1s
fuzz: elapsed: 2m0s, execs: 2099803 (14592/sec), new interesting: 15 (total: 63)
PASS
ok  	minidb/internal/txn	120.156s

$ go test ./internal/txn/ -run '^$' -fuzz FuzzTxnCrash -fuzztime 120s -fuzzminimizetime 1s
fuzz: elapsed: 2m0s, execs: 3012 (42/sec), new interesting: 169 (total: 172)
PASS
ok  	minidb/internal/txn	121.045s
```

**Đọc kết quả:** 2.099.803 lượt sạch cho codec; 3012 lượt sạch cho đường crash. `FuzzTxnCrash`
trả lời một câu mà suy luận không trả lời được: chuỗi version là một encoding **mới** nằm trong
value của B+Tree, nó đi qua WAL, checkpoint, redo và undo mà **không tầng nào biết nó là gì** —
sau crash có chuỗi nào giải mã ra rác không? Không. `BadChains == 0` sau mọi lần mở lại.

**Nói thẳng một chỗ, để không nhận công quá:** `FuzzTxnCrash` crash bằng `db.SimulateCrash()`
trong tiến trình, **không** phải `kill -9`. Cho phase 6 đi qua `cmd/crashlab` thật là việc còn
lại → nợ **P6-6**.

---

### 2026-09-02 — kiểm hồi quy: phase 5 còn nguyên không

```console
$ go run ./cmd/crashlab -n 20 | tail -3
19          596       96       873  ok (redo 5028/5257, undo 0, loser 0, mồ côi 0)
20          119       18       422  ok (redo 1797/1797, undo 0, loser 0, mồ côi 0)

20/20 vòng đúng, 0 sai. 1247 transaction đã commit được kiểm, 8.982s.

$ go run ./cmd/crashlab -n 10 -nowrite | tail -2
8           247      147         0  SAI: cây hỏng: [B3: page 2 khóa "K\x00\x00\x00\x05..." vượt cận trên ...]
   (thoát 1 — ĐÚNG như mong đợi)
```

**Đọc kết quả:** phase 5 không hồi quy, và bài phản chứng **vẫn đỏ**. Vế thứ hai quan trọng
ngang vế thứ nhất: nó chứng minh 20/20 ở trên vẫn là một phép kiểm thật, chứ không phải một bộ
kiểm tra đã tê liệt.

```console
$ go test ./... -count=1 | tail -8
ok  	minidb/internal/btree	0.531s
ok  	minidb/internal/bufpool	1.050s
ok  	minidb/internal/db	23.931s
ok  	minidb/internal/lock	0.092s
ok  	minidb/internal/page	4.284s
ok  	minidb/internal/pager	1.136s
ok  	minidb/internal/txn	27.409s
ok  	minidb/internal/wal	0.109s

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
```

---

## Giả thuyết sai / bug đã gặp

| Tôi tưởng là | Thực tế là | Lệnh / output đã lật tẩy nó | Đã sửa thế nào |
|---|---|---|---|
| Muốn nhiều transaction thì phải cho nhiều writer cùng sửa cây, tức phải làm latch-coupling (P4-5) trước | Deferred write cho **nhiều transaction logic** trên **một writer vật lý**. Và đường (a) còn buộc **bỏ pha undo physical**: A và B cùng sửa một page, A abort, dán ảnh-trước của A là xoá việc của B — đây chính là lý do Postgres không có pha undo | Suy luận từ `db.Txn.Abort` của phase 5, xác nhận bằng việc phase 5 **không đổi một dòng**: `go run ./cmd/crashlab -n 20` → `20/20 vòng đúng` | Kiến trúc (b): write set trong RAM, áp tất cả trong một `d.Update` lúc commit. P4-5 vẫn ở phase 7 |
| Dirty read là hành vi **mặc định**; mức cao mới phải viết mã để chặn | Ngược hẳn. Write set là RAM riêng tới lúc commit ⇒ dirty read là việc **bất khả**. Phải viết `Store.peekDirty` **thêm vào** chỉ để tái tạo nó. `read-uncommitted` là mức **khó cài nhất** | `go run ./cmd/txnlab -work anomaly` cho ô `dirty-read × read-uncomm` = `X`, và nó chỉ `X` được vì có `peekDirty`; `TestReadUncommittedNeedsExtraCode` khẳng định `DirtyReads` tăng ở **đúng một** mức | Giữ `peekDirty`, ghi lý do vào `level.go` ngay trên hằng `ReadUncommitted` |
| Mọi transaction cần một transaction id | Transaction **chỉ đọc** không cần. Xid cấp theo lô 64 và mốc lô ghi bền ⇒ cứ 64 lượt **đọc** là một lần **ghi log** | `go test -run TestReadOnlyCommitTouchesNothing` → `50 transaction chỉ đọc sinh 220 byte log, phải là 0` | Tách **virtual xid** (`vnext`, RAM, danh tính) khỏi xid bền (`next`/`persisted`, cấp **lười** ở lần ghi đầu). Đúng *virtual transaction id* của Postgres |
| Chọn nạn nhân deadlock là đứa **trẻ nhất** thì không có starvation — tôi đã tự viết câu này thành chú thích trong `findCycle` | Sai ngay khi có vòng thử lại: mỗi lần thử lại là một `vid` **mới**, nên nó **luôn** trẻ nhất, nên nó **luôn** là nạn nhân. Nó không bao giờ già đi | `TestTransferSerializableCommitsEverything` → `70/160 lượt bỏ cuộc`; sau khi sửa starvation → `68/160`. **Gần như không đổi** ⇒ giả thuyết bị bác **bằng số** | Tách **tuổi** khỏi **danh tính**: `lock.Manager.SetAge`, và `Store.Update` giữ nguyên tuổi qua các lần thử lại |
| Sửa starvation xong thì Serializable sẽ commit hết | Nguyên nhân chính là **conversion deadlock**: `Get` lấy S, `Put` nâng lên X ⇒ hai txn cùng đọc một khóa thì deadlock **chắc chắn**, không phải *có thể* | 68 → **10/160** sau khi thêm `GetForUpdate` | `Txn.GetForUpdate` (= `SELECT ... FOR UPDATE`), dùng trong `transferOnce`. Bài học: giá thật của 2PL là nó buộc **ứng dụng** khai báo ý định ghi; MVCC không đòi gì |
| Có vòng thử lại là đủ; backoff chỉ là tối ưu hoá | 50 lần thử hết **0.22s** ⇒ chưa tới 25µs/lần ⇒ mọi contender thử lại **đồng pha** và va lại đúng như thế | 10 → **0/160** sau khi thêm backoff có jitter. `go test -run TestTransfer` → `ok 3.1s`, 360/360 và 160/160 | `backoff(attempt)` với `rand.Int63n`. Phần **ngẫu nhiên** mới là phần phá đồng pha, không phải phần chờ |
| "Reader không chặn writer" ⇒ reader không làm hại writer | Reader **không lấy khóa** của writer — đúng. Nhưng reader cũ **ghim horizon** ⇒ chuỗi version không dọn được ⇒ writer **chết** ở lần ghi thứ 63 | `TestSnapshotReadDoesNotBlockWriter` → `writer thứ 63: txn: chuỗi version đã đầy` | Không sửa code (đây là sự thật về MVCC). Tách thành **hai** bài nói **hai** vế; thêm `TestOldReaderStarvesWriterOnSameKey` chỉ ra thuốc duy nhất: đóng reader → `Vacuum` |
| `MaxVersions = 64` là trần của chuỗi version | Trần thật là trần **byte**: `btree.MaxEntrySize` = 2028. Với value 40 byte thì chạm ở version **41**, `MaxVersions` không bao giờ tới | `TestChainFullIsReported` → `version thứ 41: btree: entry lớn quá: 2 + 3 + 2051 = 2056 > 2028` — thông báo của **tầng dưới**, nói triệu chứng chứ không nói nguyên nhân | Kiểm **cả hai** trần trong `Txn.apply`, lỗi mang tên `ErrChainFull`. Đây là giới hạn cứng của MVCC-tại-chỗ → nợ P6-1 |
| `depth=60` nhanh hơn `depth=32` là một **hiện tượng** cần giải thích | Là **noise**. `-benchtime=200x` không phải một phép đo | `-benchtime=200000x -count=3` → 171 / 331 / 866 / 1450 ns, **đơn điệu** | Không sửa code. `bench-txn` chốt `-benchtime` lớn + `-count=3`. Lần thứ hai trong repo dính đúng bẫy của phase 0 |
| Encoding chuỗi version chỉ cần "đọc lại được" là đủ | Nó phải **canonical**. Bit cờ lạ bị nuốt im lặng ⇒ chuỗi hỏng đi qua `ChainStats` mà `BadChains` **vẫn 0** — cái đồng hồ dựng riêng để bắt chuỗi hỏng lại không thấy loại hỏng này | `-fuzz FuzzChainCodec`, **3 giây**: `gốc 0130...` / `lại 0100...` | Mặt nạ `flagsKnown`; bit ngoài mặt nạ → `ErrBadChain` |
| Vá Decode là xong | Bug sâu hơn: **`Encode` sinh ra được thứ `Decode` của chính nó từ chối** — nó ghi `len(Val)` cả với tombstone, dù tài liệu ngay trên `type Version` nói `Val` của bản `Deleted` luôn nil. Một luật được **viết** mà không được **thực thi** | Sau khi vá Decode, `go test ./internal/txn/` đỏ ngay ở `seed#0`: `version 1 là tombstone nhưng nói 1 byte thân` | Chuẩn hoá ở **`Encode`**, không chỉ kiểm ở `Decode` |
| "MVCC luôn nhanh hơn locking" | Sai. Điều khiển đồng thời lạc quan **không có hàng đợi, không có thứ tự** ⇒ dưới tranh chấp cao, thử lại không hội tụ | `txnlab -accounts 2 -workers 12`: `repeatable-read` **bỏ 26/1200** với **2670** lần thử lại; `serializable` **bỏ 0** với **122** deadlock ⇒ **21.9x** công vô ích, bên thua là bên lạc quan | `make txnlab-contention` thành một target riêng, có chú thích nói rõ nó tồn tại để chứng minh câu trên là sai. Thêm `TestOptimisticDegradesUnderContention` |
| *(bug của bộ đo)* Bài test treo ⇒ code treo | Kênh một giá trị bị **nhận hai lần**: `select` có timeout, rồi cuối hàm vẫn `<-done` lần nữa. Không timeout ⇒ lần nhận thứ hai chờ mãi | Bảng anomaly treo hết **300s** ở đúng ô `non-repeatable-read × read-uncommitted`. Phải đọc `~/.local/share/rtk/tee/*.log` bằng python mới thấy stack, vì `rtk` lọc mất dump panic | Biến `blocked bool`; `<-done` cuối chỉ chạy khi `select` đã timeout thật. Cùng lỗi trong `probePhantom`. Từ đó mọi lệnh cần output nguyên văn đều chạy qua `rtk proxy` |
| *(bug của bộ đo, và nó che một bug thật)* — | Đi truy lỗi treo của **bài test** thì lộ ra lỗi treo của **code**: `Acquire` đặt **một** `time.AfterFunc` ở đầu hàm; một lần bị `Broadcast` oan là timer tiêu, lần `Wait()` sau không còn ai đánh thức | Stack đọc từ log của `rtk` cho thấy goroutine đứng ở `cv.Wait()` sau deadline | Hẹn giờ **lập lại mỗi vòng** theo `rem := time.Until(deadline)`, `wake.Stop()` sau mỗi `Wait()` |
| *(bug của bộ đo)* Lock manager sai vì test deadlock timeout | Hợp đồng nói txn nhận `ErrDeadlock` **phải abort**. Bài test nhận `ErrDeadlock` rồi **không nhả khóa** của nạn nhân; ở ca ba vòng thì **người thắng** lại thành vật cản mới | `TestDeadlockUpgradeCycle`, `TestDeadlockThreeCycle` timeout | Helper `tryLock` tự `ReleaseAll` khi thấy `ErrDeadlock`; closure `run` kết thúc mỗi txn sau khi lấy được khóa |
| *(bài test tố oan)* Snapshot isolation phải commit hết mọi lượt chuyển tiền | Ở 4 tài khoản × 6 goroutine, 299/300 là **tính chất thật** của OCC, không phải bug | `TestTransferRepeatableReadRetriesNotLoses` → `299/300` | Sửa **bài test**, không sửa code: chuyển sang 16 tài khoản; thêm bài **so sánh** `TestOptimisticDegradesUnderContention` đo chính điểm giao |

Đếm: **8 bug của code**, **4 bug của bộ đo**, **1 bài test tố oan**, **2 giả thuyết bị bác bằng
số đo** (starvation, và `depth=60`). Tỉ lệ "lỗi dụng cụ / lỗi code" lặp lại đúng hình dạng của
phase 5 — nó không còn là tai nạn.

## Số đo

Mọi bảng dưới đây: **máy** WSL2 / i5-1235U / 6 core / ext4 / go1.26.2 · **ngày** 2026-09-02 ·
**commit** phase 6. Lệnh ghi ngay trên mỗi khối output ở mục *Nhật ký*.

Tỉ số cần nhớ (tỉ số bền hơn số tuyệt đối):

| Tỉ số | Giá trị | Ý nghĩa |
|---|---|---|
| commit / abort(0 khóa) | 1186290 / 293.2 = **4046x** | Con số của MVCC: hoàn tác không có I/O, không log, không undo |
| abort(1 khóa) / abort(0 khóa) | 20791 / 293.2 = **71x** | Ghi **một** khóa là phải xin một xid bền, tức gánh **1/64 fsync** |
| abort(1 khóa) so với fsync/64 | 20791 vs 1186290/64 = 18536 → **1.12x** | Giả thuyết "một xid = 1/64 fsync" được **xác nhận bằng số học** |
| abort(100 khóa) / abort(1 khóa) | 39939 / 20791 = **1.9x** | Ném write set đi gần như miễn phí; chi phí nằm ở xid |
| commit: mức cao / mức thấp | 1189498 / 1150252 = **1.03x** | Mức cô lập **miễn phí** lúc commit — quyết định là một cái fsync |
| Get: serializable / repeatable-read | 762 / 325 = **2.35x** | Giá thật của việc lấy một khóa S ở **mỗi** lần đọc |
| Get: `oldest` / `newest` ở depth=60 | 1480 / 1450 = **0.98x** | Chi phí **không** ở vòng visibility mà ở `DecodeChain` (giải mã trọn chuỗi) → P6-2 |
| Get: depth=60 / depth=1 | 1450 / 171 = **8.5x** | Chuỗi dài 60x mà chỉ đắt 8.5x: tra B+Tree (~150ns) là phần cố định |
| `AcquireDisjoint` 256 / 1 holder | 3762 / 168.7 = **22.3x** | Cái giá của lock manager **không có chỉ mục theo đối tượng** → P6-3. `Overlaps` chỉ 13.34ns / 0 alloc ⇒ vấn đề là O(n), không phải hằng số |
| Phình version với **một** reader mở lâu | **18.07x** byte value | Toàn bộ câu trả lời cho "vì sao Postgres cần VACUUM" |
| Vacuum khi reader còn mở | dọn **0** version (`horizon 1`) | Bộ dọn không yếu — **không có gì để dọn hợp pháp** |
| Tranh chấp cao: thử lại của OCC / deadlock của 2PL | 2670 / 122 = **21.9x**, và OCC **bỏ 26** lượt còn 2PL **bỏ 0** | **"MVCC luôn nhanh hơn locking" là câu sai** |
| Tranh chấp trung bình: thử lại RR / thử lại SER | 1216 / 97 = **12.5x** | Bên lạc quan làm việc vô ích nhiều hơn **ngay từ** mức tranh chấp trung bình |
| Trần chuỗi version | **41** lần ghi lại (value 40 byte) hoặc **63** khi có reader ghim | Giới hạn cứng của MVCC-tại-chỗ → P6-1 |

## Invariant tôi đã cài và lệnh kiểm chứng nó

| Invariant | Cài ở đâu (file:hàm) | Lệnh kiểm chứng | Kết quả |
|---|---|---|---|
| Version chỉ vào cây khi txn của nó đã commit ⇒ "có mặt trong cây" = "đã commit", nên **không cần clog** | `txn/txn.go:Txn.apply` (chạy trong đúng một `d.Update`) | `go test ./internal/txn/ -run TestAbortWritesNothing -count=1` | PASS — abort sinh **0 byte** log |
| Luật visibility: `xmin >= Xmax` ⇒ không thấy; `xmin < Xmin` ⇒ thấy; còn lại tra `Active` | `txn/snapshot.go:Snapshot.Visible` | `go test ./internal/txn/ -run 'TestVisible' -count=1` | PASS |
| `Prune(h)` không đổi cái mà bất kỳ snapshot hợp lệ nào (`Xmin >= h`) nhìn thấy | `txn/version.go:Chain.Prune` | `go test ./internal/txn/ -run '^$' -fuzz FuzzChainCodec -fuzztime 120s` | PASS, **2 099 803** exec |
| `Prune` không bao giờ xoá **sạch** chuỗi (khóa không được biến mất khỏi cây) | `txn/version.go:Chain.Prune` (giữ đúng một version có `xmin < horizon`) | cùng lệnh fuzz trên | PASS |
| Encoding chuỗi version là **canonical**: giải mã được ⇒ mã hoá lại ra đúng byte cũ | `txn/version.go:Chain.Encode` + `DecodeChain` (mặt nạ `flagsKnown`, tombstone không thân) | cùng lệnh fuzz trên | PASS — **đỏ ở 3 giây** trước khi sửa |
| Transaction **chỉ đọc** không chạm đĩa lần nào | `txn/store.go:Store.Begin` (không cấp xid) + `txn/txn.go:Txn.ensureXID` (cấp **lười**) | `go test ./internal/txn/ -run TestReadOnlyCommitTouchesNothing -count=1` | PASS — 0 byte log cho 50 txn |
| Bộ đếm xid bền qua đóng/mở lại, và bằng đúng `max(xmin)` trong cây | `txn/store.go:Store.allocXID` (`metaNext`, lô 64) | `go test ./internal/txn/ -run TestIDCounterSurvivesReopen -count=1` | PASS — đối chiếu bộ đếm O(1) với một lần quét O(n) |
| Mốc dọn = `min(Xmin của mọi txn đang sống, xid của chính mình)` — **thiếu vế nào cũng sai** | `txn/txn.go:Txn.setGCXmin`, `txn/store.go:Store.horizon` | `go test ./internal/txn/ -run 'TestReadYourOwnWrites\|TestVacuumBlockedByOldReader' -count=1` | PASS |
| Chuỗi version phải nhét vừa **một** entry B+Tree, và lỗi phải mang tên `ErrChainFull` | `txn/txn.go:Txn.apply` (kiểm cả `MaxVersions` lẫn `btree.MaxEntrySize`) | `go test ./internal/txn/ -run 'TestChainFullIsReported\|TestMaxVersionsFitsAnEntry' -count=1` | PASS |
| Thứ tự latch một chiều, không ngoại lệ: `dbMu` → `mu`; không bao giờ giữ `mu` mà xin `dbMu` | `txn/store.go` (chú thích tại chỗ khai báo) | `go test -race ./internal/txn/ -count=1` | PASS, race-clean |
| Tầng vật lý **vẫn** chỉ có một writer — phase 5 không bị nới lỏng | `db/db.go:DB.Update` (`ErrWriterBusy` giữ nguyên) | `go test ./internal/txn/ -run TestPhysicalLayerStaysSingleWriter -count=1` | PASS |
| Nạn nhân deadlock là đứa **trẻ nhất theo tuổi**, và tuổi **sống sót qua các lần thử lại** | `lock/lock.go:Manager.findCycle` + `Manager.SetAge` | `go test ./internal/lock/ -run Deadlock -count=1 -v` | PASS — và khẳng định `Timeouts == 0`: chặn bằng timeout là chặn bằng may mắn |
| Khoá span `[Lo,Hi)` chặn được cả khóa **chưa tồn tại** (predicate lock nghèo nhất dùng được) | `lock/lock.go:Res.Overlaps` | `go test ./internal/lock/ -run 'TestOverlaps\|TestSpanBlocksKeyInside' -count=1 -v` | PASS |
| Chuỗi version sống qua WAL/redo/undo: sau crash không chuỗi nào giải mã ra rác | `txn/vacuum.go:Store.ChainStats` (`BadChains`) | `go test ./internal/txn/ -run '^$' -fuzz FuzzTxnCrash -fuzztime 120s` | PASS, 3012 exec, `BadChains == 0` |
| Phase 5 không hồi quy | — | `go run ./cmd/crashlab -n 20` và `-n 10 -nowrite` | 20/20 đúng; nowrite **vẫn đỏ** |

## Đọc gì

- Postgres, *Transaction Processing*: `heapam_visibility.c` cho luật visibility, và tài liệu về
  **virtual transaction id** — mục này chỉ có nghĩa với tôi **sau** khi cái test in ra "220 byte
  log cho 50 transaction chỉ đọc".
- Postgres, `SELECT ... FOR UPDATE` và `idle_in_transaction_session_timeout`. Hai thứ trước đây
  tôi coi là chi tiết vận hành; giờ mỗi thứ có một con số đi kèm trong file này.
- Berenson et al., *A Critique of ANSI SQL Isolation Levels* — chỗ **write skew** được đặt tên,
  và chỗ giải thích vì sao "repeatable read" của chuẩn khác của Postgres.
- Kung & Robinson, *On Optimistic Methods for Concurrency Control* — và cái mà bài đó **không**
  nói: điểm giao ở đâu. `make txnlab-contention` là câu trả lời cho máy này.
- ARIES (đọc lại từ phase 5), riêng phần undo — để hiểu vì sao **undo physical buộc mỗi page chỉ
  thuộc một transaction dở dang**, và vì sao Postgres không có pha undo.

## Rút ra (viết như thể giải thích cho người khác)

**Lock khác latch thế nào.** Lần này có code để chỉ tận nơi. Latch là `sync.Mutex` trong
`bufpool` và `d.mu` trong `internal/db`: nó bảo vệ **một cấu trúc dữ liệu trong RAM**, giữ trong
vài chục nanosecond, không có phát hiện deadlock, và ai giữ nó thì phải nhả trong cùng một hàm.
Lock là `internal/lock`: nó bảo vệ **một đối tượng logic** (một khóa, hoặc một khoảng
`[Lo,Hi)`), giữ **suốt transaction** tới `ReleaseAll`, có hàng đợi, và **phải** có phát hiện
deadlock — chữ "strict" trong S2PL chính là "chỉ nhả ở một điểm duy nhất, lúc kết thúc". Nhìn
`BenchmarkAcquireUncontended` = 128ns và `d.mu.Lock()` = vài ns thì thấy hai thứ không cùng loại.

**S2PL đảm bảo serializable bằng cơ chế gì, giá là gì.** Cơ chế: giữ mọi khóa tới lúc kết thúc,
nên không có transaction nào quan sát được trạng thái giữa của transaction khác; lịch trình đồ
thị xung đột không có vòng. Giá thì có **ba** phần, và tôi chỉ biết trước một phần:

1. Giá thấy được: 2.35x trên đường đọc, vì mỗi `Get` phải lấy một khóa S.
2. Giá không thấy được: nó buộc **ứng dụng** phải khai báo trước ý định ghi. Không có
   `GetForUpdate` thì mẫu đọc-rồi-ghi cho **conversion deadlock chắc chắn** — 68/160 lượt bỏ
   cuộc. MVCC không đòi ứng dụng khai gì cả. Đây là lý do thật vì sao MVCC thắng, và nó không
   phải lý do về throughput.
3. Giá của việc **không** có nó: xem đoạn về write skew bên dưới.

**Visibility rule của MVCC.** Ba dòng trong `Snapshot.Visible`. Nhưng chỗ đáng nói không phải ba
dòng đó, mà là hai thứ **bỏ đi** được: `xmax` là dữ liệu trùng lặp (nó luôn bằng `xmin` của bản
mới hơn liền kề, khi chuỗi xếp mới-nhất-trước), và **clog không cần** vì write set chỉ vào cây
khi đã commit. Hai lần bỏ, cùng một lý lẽ đã viết ở phase 5: *hai nguồn sự thật cho cùng một sự
việc là hai chỗ để lệch nhau.*

**Vì sao reader không chặn writer — và câu đó đúng đến đâu.** Reader đọc bản cũ trong chuỗi,
writer thêm bản mới; không ai cần khóa của ai. `TestSnapshotReadDoesNotBlockWriter` chứng minh
điều đó. **Nhưng** phát biểu đúng là *"reader không **lấy khóa** của writer"*, chứ không phải
*"reader không **làm hại** writer"*. Câu thứ hai sai, và nó sai ở **mọi** bản MVCC: một reader
mở lâu ghim `horizon`, nên version cũ không dọn được, nên cây phình **18.07x** và writer trên
cùng một khóa **chết ở lần ghi thứ 63**. Đây là hai mặt của cùng một đồng xu, và tôi chỉ hiểu
mặt thứ hai vì có một bài test đỏ.

**Write skew, và vì sao snapshot isolation không chặn được.** Hai transaction đọc cùng một tập,
rồi mỗi bên ghi **một khóa khác nhau**, và mỗi bên nhìn riêng thì hợp lệ, mà hợp lại thì phá bất
biến. Snapshot isolation phát hiện xung đột bằng cách hỏi *"có ai ghi khóa **tôi cũng ghi** sau
snapshot của tôi không?"* — với write skew, câu trả lời là **không**, vì hai bên ghi hai khóa
khác nhau. **Không có gì để phát hiện.** Chặn nó buộc phải biết về chỗ **đọc**: S2PL biết (nó
giữ khóa S trên cái đã đọc, nên bài test bị chặn bằng **deadlock** — `Deadlocks != 0`, chứ không
phải bằng timeout, và tôi khẳng định đúng chỗ ấy vì chặn bằng timeout là chặn bằng may mắn);
SSI thì rẻ hơn — nó theo dõi phụ thuộc đọc-ghi và abort khi thấy một hình nguy hiểm, không phải
khi thấy một xung đột thật.

**Ai dọn rác MVCC** (câu hỏi treo từ phase 2). Hai bộ dọn, và cả hai **bị chặn bởi cùng một
thứ**: opportunistic (HOT prune của Postgres) chạy ngay trong `Txn.apply`, gần như miễn phí,
nhưng chỉ chạm những khóa **đang được ghi**; toàn cây (`Vacuum`) chạm cả những khóa không ai ghi
nữa, theo lô 256 khóa một transaction vật lý. Cả hai đều gác bởi `horizon`, nên **một** transaction
chỉ đọc đang nằm im làm **cả hai** vô ích: `vacuum khi reader còn mở: dọn 0 version (horizon 1)`.
Không phải bộ dọn yếu — mà không có gì để dọn một cách hợp pháp. Đó là toàn bộ nội dung của
`idle_in_transaction`.

**Hai điều bất ngờ, ngoài danh sách câu hỏi.**

Thứ nhất: **dirty read phải được thêm mã để tái tạo**. Trong kiến trúc deferred write, đọc được
thay đổi chưa commit là việc **bất khả**, nên `read-uncommitted` là mức **khó cài nhất** — ngược
hoàn toàn với trực giác "mức thấp thì dễ". Bài học tổng quát hơn: anomaly nào xảy ra là **hệ quả
của kiến trúc**, không phải của một cái công tắc. Và nó là lý do mọi ô của bảng phải được khẳng
định theo **cả hai chiều**: một probe hỏng làm **mọi** mức trông có vẻ an toàn.

Thứ hai: **abort là chỗ MVCC và ARIES khác nhau nhất.** Cùng một chữ "hoàn tác", hai cơ chế khác
hẳn. `db.Txn.Abort` của phase 5: đi hết chuỗi undo, dán từng ảnh-trước, ghi một CLR cho mỗi page,
thu hồi page mồ côi. `txn.Txn.Abort` của phase 6: ném map đi — **293 nanosecond**, rẻ hơn commit
**4046x**. Và vì cái giá ấy khác nhau, cách đúng để rollback ở hai tầng cũng khác nhau; đó là lý
do Postgres rollback bằng 2 bit trong clog chứ không bằng cách dán lại byte cũ.

## Nợ kỹ thuật / để dành cho sau

- [ ] **P6-1** · Chuỗi version nằm **tại chỗ**, trần cứng ~2KB một khóa
- [ ] **P6-2** · `DecodeChain` giải mã **trọn chuỗi** dù chỉ cần một version (`newest`/`oldest` = 0.98x)
- [ ] **P6-3** · Lock manager không có **chỉ mục theo đối tượng** (256 holder rời rạc = 22.3x)
- [ ] **P6-4** · `Txn.Scan` **materialize** cả kết quả thay vì stream
- [ ] **P6-5** · Không có **vacuum nền** (cùng họ với P5-1: thiếu người dọn page)
- [ ] **P6-6** · Phase 6 chưa đi qua **`kill -9` thật**; `FuzzTxnCrash` dùng `SimulateCrash` trong tiến trình
- [ ] **P6-7** · Chưa có **SSI**: `Serializable` cài bằng S2PL, không bằng theo dõi phụ thuộc đọc-ghi

Đã trả trong phase này: **P1-2b** (cô lập cho reader đồng thời) — đúng bằng cách mà
`docs/debts.md` đã dự đoán từ phase 5: MVCC ở phase 6.

**Không** trả, và cố ý: **P4-5** (latch-coupling) vẫn ở phase 7. Kiến trúc deferred write làm nó
chưa cần thiết, và `DB.Range` giữ `d.mu` suốt lần duyệt chính là chỗ **đo được cái giá của việc
chưa có nó**.
