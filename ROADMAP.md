# minidb — Roadmap học DB internals bằng Go

> Mục tiêu: không học "lệnh SQL", mà học **invariant** và **failure mode** của một database.
> Sau roadmap này, đọc `EXPLAIN` của Postgres/MySQL bằng trực giác của người đã tự viết ra nó.

**Nguyên tắc xuyên suốt:** mỗi phase kết thúc bằng **một crash-test hoặc một benchmark**
chứng minh bạn hiểu — không phải bằng "code chạy được".

Môi trường: Go 1.26.2, Linux (WSL2).

---

## Kiến trúc mục tiêu (bottom-up)

```
SQL front-end      (phase 8)    <- lexer/parser/binder, logical vs physical plan
Query execution    (phase 8)    <- Volcano/iterator, join, sort, TOÁN TỬ CHẶN, tràn ra đĩa
Index + planner    (phase 7)    <- khóa giữ thứ tự, catalog, mô hình chi phí
Transaction/MVCC   (phase 6)    <- concurrency, isolation
Access method      (phase 4)    <- B+Tree, cursor, iterator
Recovery           (phase 5)    <- WAL, ARIES-lite, checkpoint
Buffer pool        (phase 3)    <- page cache, eviction, pin/latch
Storage/Pager      (phase 1-2)  <- page, slotted page, freelist
File (pread/pwrite, fsync)
```

Hướng thiết kế: **B+Tree + WAL (update-in-place)** = Postgres/MySQL/SQLite.
Hướng còn lại (**LSM-Tree**: memtable + SSTable + compaction = RocksDB/Cassandra) để dành sau —
xem mục "Sau roadmap" ở cuối.

---

## Bảng tổng quan

| Phase | Nội dung | Thời lượng | Deliverable chứng minh hiểu |
|---|---|---|---|
| 0 | ✅ Nền tảng vật lý: fsync, torn write, random vs seq I/O, latch vs lock | 0.5 ngày | `cmd/iolab` — xong, số đo ở [diary/phase0.md](diary/phase0.md) |
| 1 | ✅ Pager: file như mảng page, meta page, checksum, freelist | 1-2 ngày | 40 điểm crash mô phỏng -> mở lại luôn hợp lệ, [diary/phase1.md](diary/phase1.md) |
| 2 | ✅ Slotted page: record biến độ dài, compact, tuple id | 1 buổi | 1.42 triệu lần fuzz, bất biến không vỡ lần nào, [diary/phase2.md](diary/phase2.md) |
| 3 | ✅ Buffer pool: pin/unpin, dirty, CLOCK/LRU-K, latch | 1 buổi | Hit-ratio zipfian + sequential flooding, so cả với Belady, [diary/phase3.md](diary/phase3.md) |
| 4 | ✅ B+Tree: search/insert/split/delete/merge, cursor | 4-6 ngày (thực tế 1) | Property test 7 bất biến + bench 1M khóa: ngẫu nhiên/tăng dần = 33x writes/op, [diary/phase4.md](diary/phase4.md) · [log](diary/phase4-log.md) |
| 5 | ✅ **WAL + recovery**: ARIES-lite (analysis/redo/undo), checkpoint | 3-4 ngày (thực tế 2) | 200/200 lần `kill -9` ngẫu nhiên, 9194 txn đã commit được kiểm — **và** 10/10 báo SAI khi cố tình làm mất log ([diary/phase5.md](diary/phase5.md) · [log](diary/phase5-log.md)) |
| 6 | ✅ **Transaction & concurrency**: MVCC snapshot isolation + S2PL, 4 mức isolation, deadlock detection | 3-4 ngày (thực tế 1) | Bảng 5 anomaly × 4 mức khớp lý thuyết từng ô, khẳng định theo **cả hai chiều**; chuyển tiền vỡ ở mức thấp, giữ ở mức cao; và **chỗ MVCC thua lock** ([diary/phase6.md](diary/phase6.md) · [log](diary/phase6-log.md)) |
| 7 | ✅ **Secondary index + query**: bộ mã hoá khóa giữ thứ tự, catalog nhiều bảng trong một cây, 3 kế hoạch + mô hình chi phí đo được | 2-3 ngày (thực tế 2 buổi) | Điểm hoà vốn selectivity **đo được 36.8%** (không phải 5-20% như sách — vì ở quy mô này không có I/O thật), và bảng còn có cột **planner chọn sai** ([diary/phase7.md](diary/phase7.md) · [log](diary/phase7-log.md)) |
| 8 | ✅ **SQL front-end**: lexer/parser/binder, logical vs physical plan, nested loop + Grace hash join, external merge sort, predicate pushdown, `EXPLAIN` | 2-3 ngày (thực tế 1) | Sáu bảng số, mỗi bảng là một câu hỏi của phase — và bảng **pushdown** chỉ ra một **luật optimizer còn thiếu** (1.18x → 18.53x sau khi thêm nó): lần đầu trong 8 phase số đo tìm ra thứ **CHƯA CÓ**, không phải thứ sai ([diary/phase8.md](diary/phase8.md) · [log](diary/phase8-log.md)) |

Tổng ~4-6 tuần với 2-3h/ngày.
**Bắt buộc: phase 1-5** (storage + B+Tree + WAL). Phase 6 là phần nâng bạn từ "biết DB" lên "thiết kế được DB".

---

## Phase 0 — Nền tảng vật lý (0.5 ngày)

Chưa code DB. Hiểu 4 sự thật vật lý quyết định **mọi** thiết kế về sau:

1. `fsync` là gì, vì sao `write()` không đủ (OS page cache), vì sao phải fsync cả directory entry khi tạo file mới.
2. **Torn write**: đĩa chỉ atomic ở mức sector (512B/4KB). Page 4KB "gần như" an toàn, 16KB thì không
   -> đây là nguồn gốc của WAL / double-write buffer.
3. **Random vs sequential I/O**: SSD 4KB random ~100us, sequential ~GB/s
   -> đây là toàn bộ lý do B+Tree tồn tại và vì sao fanout phải lớn.
4. **Latch** (bảo vệ cấu trúc dữ liệu trong RAM, sống vài ns) khác hoàn toàn **Lock**
   (bảo vệ dữ liệu logic, giữ đến hết transaction).

**Bài tập:** viết bench Go đo `write` vs `write+fsync` vs random `pwrite` 4KB.
Ghi lại con số — bạn sẽ dùng nó để giải thích mọi quyết định thiết kế sau này.

## Phase 1 — Pager: file như một mảng page (1-2 ngày)

- `Page = [4096]byte`, `PageID uint32`, file = meta page + N page.
- **Meta page**: magic, version, pageSize, root pageID, freelist pageID, txnID, **checksum (crc32c)**.
- Đọc/ghi bằng `pread`/`pwrite` — **không** `Seek`+`Read` (không thread-safe).
- **Freelist**: page rỗng được tái sử dụng.

**Invariant:** mọi thay đổi cấu trúc file phải để lại file ở trạng thái đọc được.
**Kỹ thuật cốt lõi:** 2 meta page ghi luân phiên (double-buffering); khi mở file chọn cái có
`txnID` lớn hơn **và** checksum hợp lệ. Đây chính là cách BoltDB đạt atomicity mà chưa cần WAL.

**Test:** ghi meta rồi mô phỏng crash (làm hỏng byte giữa chừng); mở lại phải rollback về meta cũ.

## Phase 2 — Slotted page (1-2 ngày)

```
[header | slot0 slot1 slot2 ->  ...free...  <- cell2 cell1 cell0]
```

- Slot array mọc từ trái, cell data mọc từ phải. Slot = `(offset, len)`.
- Xóa = đánh dấu slot dead -> **fragmentation** -> viết `compact()`.
- Vì sao cần slot indirection: record dịch chuyển khi compact nhưng **slot index không đổi**,
  nên con trỏ từ bên ngoài (index -> heap) vẫn hợp lệ.

Đây là lúc hiểu **tuple id = (PageID, SlotID)** và vì sao Postgres cần VACUUM.

## Phase 3 — Buffer pool (2 ngày)

- `map[PageID]*Frame` + mảng frame cố định (ví dụ 128MB).
- `Pin` / `Unpin`, cờ `dirty`, eviction bằng **CLOCK hoặc LRU-K** —
  đừng dùng LRU thuần, và phải giải thích được vì sao (sequential scan làm hỏng LRU).
- Latch: `sync.RWMutex` mỗi frame + một latch cho bảng hash.

**Invariant:** không evict page đang pinned; không evict dirty page trước khi WAL của nó đã fsync
(WAL rule — cài ở phase 5, nhưng chừa sẵn điểm móc từ bây giờ).

**Bench:** hit ratio với workload zipfian, so LRU vs CLOCK.

## Phase 4 — B+Tree (4-6 ngày) — linh hồn của DB

Phase dài nhất, đừng vội.

1. Node = 1 page. Internal: `[key, childPtr]`; leaf: `[key, value]` + `nextLeaf` (sibling pointer cho range scan).
2. `Search` (binary search trong page), `Insert` với **split**, `Delete` với **merge/redistribute**
   (nhiều người bỏ delete — đừng, merge mới là chỗ khó).
3. Split lan từ dưới lên; cây chỉ tăng chiều cao khi root split -> **luôn cân bằng**.
4. `Cursor`: `Seek(key)`, `Next()` — nền tảng cho mọi iterator và `WHERE x > ?`.

Hiểu sâu = trả lời được:
- Fanout = pageSize/(keySize+8) ~ 200-400 -> cây 3 tầng chứa ~64 triệu key -> **mọi lookup <= 3 I/O**.
  Đây là toàn bộ lý do B+Tree thắng.
- Vì sao B+Tree (data chỉ ở leaf) chứ không phải B-Tree: range scan + fanout cao hơn.
- Prefix compression / suffix truncation ở internal node.
- **Right-most insert optimization** (khóa auto-increment) -> vì sao UUIDv4 làm PK là thảm họa:
  random insert -> page split khắp nơi -> write amplification.

**Test:** property test — sau N thao tác ngẫu nhiên, verify: mọi leaf cùng độ sâu, key sorted,
mọi node (trừ root) đầy >= 50%.
**Bench:** insert 1M key sequential vs random, đếm số page split.

Nâng cao (để sau phase 7 nếu còn sức): **Blink-tree** hoặc latch-coupling (crabbing) cho truy cập đồng thời.

## Phase 5 — WAL + recovery (3-4 ngày)

- Log record: `{LSN, txnID, type, pageID, offset, before[], after[]}` — **physiological logging**.
- **WAL rule (bất biến số 1 của DB):** log record của một thay đổi phải nằm trên đĩa (đã fsync)
  **trước** khi page dữ liệu tương ứng được ghi. Mỗi page mang `pageLSN`;
  chỉ flush page khi `flushedLSN >= pageLSN`.
- Commit = fsync log tới commit record. Data page có thể ghi rất lâu sau đó.
- **Recovery 3 pha (ARIES rút gọn):**
  1. **Analysis** — dựng lại danh sách txn dở dang + dirty page table.
  2. **Redo** — *repeat history*: redo tất cả, kể cả txn sẽ bị abort.
  3. **Undo** — rollback txn chưa commit, dùng CLR để undo idempotent.
- **Checkpoint** (fuzzy) để rút ngắn thời gian recovery.

**Test quyết định:** chạy workload, `kill -9` ở thời điểm ngẫu nhiên, mở lại, kiểm tra **D trong ACID**:
mọi txn đã báo commit đều còn, mọi txn chưa commit biến mất hoàn toàn. Lặp 200 lần với seed ngẫu nhiên.

**Bench:** group commit (gộp fsync cho nhiều txn) -> throughput tăng 10-50x.

### Đã làm — và ba chỗ khác với dự kiến

- **Đạt:** `make crashlab-full` 200/200 vòng. `InsertBatch1` / `InsertBatch1000` = **261x**, gần
  hết khoảng cách ấy là **một** cái fsync; `TestGroupCommit` 200 commit → **1** fsync. Vượt xa
  khoảng 10-50x dự kiến, vì dự kiến đó tính cho nhiều writer, còn đây là gộp trong một writer.
- **Thêm vào, không có trong kế hoạch:** `make crashlab-nowrite` — bài **phản chứng**, bắt buộc
  đỏ. Hoá ra `crashlab -nosync` **không** chứng minh được bộ kiểm tra biết báo SAI: `kill -9`
  không giết page cache nên tắt sạch fsync vẫn 20/20 đúng (đúng nợ P0-1). Một bài test chưa bao
  giờ đỏ thì chưa phải bằng chứng.
- **Khác dự kiến:** kế hoạch viết "checkpoint (fuzzy) để rút ngắn thời gian recovery". Checkpoint
  mờ rút ngắn được **điểm bắt đầu** redo (1.67x) nhưng **không** rút ngắn được **độ dài** redo:
  checkpoint dày hơn 16 lần cho cùng số record (13033 vs 13059), vì `redoLSN = min(recLSN)` bị
  ghim bởi page bẩn cũ nhất. Thứ quyết định độ dài redo là **người dọn page** → nợ P5-1.
- **Trả nợ phase trước:** P1-1 (page mồ côi), P1-2 (transaction thật), P2-3 (torn page — trả bằng
  ảnh trọn page, **không** thêm checksum cho page), P4-4 (root vẫn di chuyển, nhưng mỗi lần đổi
  được log). Nợ mới: P5-1 → P5-6.

## Phase 6 — Transaction & concurrency control (3-4 ngày)

- Bắt đầu bằng **2PL nghiêm ngặt (S2PL)**: lock manager, shared/exclusive, hàng đợi,
  **deadlock detection** (wait-for graph) hoặc wait-die.
- Rồi cài **MVCC snapshot isolation** — cách các DB hiện đại thực sự làm:
  - Mỗi tuple mang `(xmin, xmax)`; txn giữ `snapshot = {xmin, xmax, activeTxnIDs}`.
  - Visibility rule: tuple nhìn thấy được nếu creator đã commit trước snapshot và deleter chưa.
  - Reader không chặn writer, writer không chặn reader.
- **Tái tạo được bằng test** từng anomaly: dirty read, non-repeatable read, phantom,
  và **write skew** — cái mà snapshot isolation *không* chặn (cần SSI/serializable).

**Test:** N goroutine chạy đồng thời bài toán chuyển tiền, verify invariant "tổng số dư không đổi"
ở mỗi mức isolation; đồng thời chứng minh mức thấp hơn **phá** invariant đó.

### Đã làm — và bốn chỗ khác với dự kiến

- **Đạt:** bảng 5 anomaly × 4 mức khớp lý thuyết từng ô (`make txnlab-anomaly`), và mỗi ô được
  khẳng định theo **cả hai chiều** — mức thấp phải **để lọt**, mức cao phải chặn (`make test-txn`).
  Chuyển tiền: hai mức thấp làm lệch tổng số dư, hai mức cao giữ đúng 80000. 240 test, race-clean.

- **Khác dự kiến #1 — thứ tự đi ngược.** Kế hoạch viết *"bắt đầu bằng S2PL, **rồi** cài MVCC"*.
  Thực tế MVCC phải đi **trước**, vì nó quyết định hình dạng dữ liệu **trên đĩa**; S2PL viết sau
  như một module độc lập (`internal/lock`) chỉ phục vụ mức `Serializable`. Hai thứ không xếp tầng
  lên nhau — chúng là hai lựa chọn **song song** cho cùng một câu hỏi.

- **Khác dự kiến #2 — bỏ `xmax`, bỏ luôn clog.** Kế hoạch viết tuple mang `(xmin, xmax)`. Nhưng
  khi chuỗi version xếp **mới-nhất-trước** thì `xmax` của bản *i* **luôn** bằng `xmin` của bản
  *i-1*: dữ liệu trùng lặp. Và clog không cần, vì write set chỉ vào cây **khi đã commit** ⇒ "có
  mặt trong cây" đã có nghĩa là "đã commit". Cùng lý lẽ đã dùng ở phase 5 khi xoá cờ
  `FlagHasBefore`: *hai nguồn sự thật cho cùng một sự việc là hai chỗ để lệch nhau.*

- **Khác dự kiến #3 — không cần nhiều writer vật lý, nên không cần P4-5.** Kiến trúc **deferred
  write**: write set trong RAM, áp tất cả trong **một** transaction vật lý của phase 5 lúc commit.
  Nhờ vậy WAL/checkpoint/recovery/undo của phase 5 **không đổi một dòng**, và latch-coupling
  (P4-5) vẫn để cho phase 7. Đường kia — nhiều writer cùng sửa cây — buộc phải **bỏ pha undo
  physical**: A và B cùng sửa một page, A abort, dán ảnh-trước của A là **xoá luôn việc của B**.
  Đó chính là lý do Postgres không có pha undo.

- **Khác dự kiến #4 — `read-uncommitted` là mức KHÓ CÀI NHẤT.** Ngược hoàn toàn với trực giác.
  Write set là RAM riêng tới lúc commit, nên dirty read là việc **bất khả**; phải viết
  `Store.peekDirty` **thêm vào** chỉ để tái tạo nó. Bài học tổng quát: anomaly nào xảy ra là **hệ
  quả của kiến trúc**, không phải của một cái công tắc.

- **Thêm vào, không có trong kế hoạch:** `make txnlab-contention` — 2 tài khoản × 12 goroutine.
  Ở đó `repeatable-read` (lạc quan) **bỏ 26/1200** lượt với 2670 lần thử lại, còn `serializable`
  (bi quan) commit **1200/1200** với 122 deadlock. **"MVCC luôn nhanh hơn locking" là một câu
  sai**, và đây là lệnh chứng minh nó sai. Cùng với `make fuzz-txn`, mà target đầu tìm ra bug
  trong **3 giây**: encoding chuỗi version không canonical.

- **Trả nợ phase trước:** **P1-2b** (cô lập cho reader đồng thời) — đúng bằng cách mà
  `docs/debts.md` đã dự đoán từ phase 5. Nợ mới: **P6-1 → P6-7**. **Không** trả và cố ý: P4-5.

## Phase 7 — Index nâng cao + query cơ bản (2-3 ngày)

- Secondary index: `index key -> (PageID, SlotID)`; non-unique key; covering index.
- Composite key và **quy tắc left-most prefix** — vì sao index `(a,b)` vô dụng với `WHERE b = ?`.
- Volcano/iterator model: `Scan -> Filter -> Project`.
- **Index scan vs seq scan**: viết cost estimate đơn giản (selectivity từ histogram),
  rồi bench để tìm điểm hòa vốn (thường ~5-20% số hàng thì seq scan lại nhanh hơn).

Đây chính là kỹ năng "tối ưu DB cho công ty lớn".

### Đã làm — và năm chỗ khác với dự kiến

- **Đạt:** `make idxlab-breakeven` — cùng một truy vấn, **ba** đường đi (seq / index / index-only),
  chín độ chọn lọc, điểm hoà vốn **nội suy từ chính số đo**. `make test-index`: ba kế hoạch phải
  cho **cùng** một multiset hàng (một planner đổi kết quả là một database **sai**, không phải chậm).
  `make fuzz-keys`: 10.5M + 11.1M exec sạch. 22 test phase 7 xanh dưới `-race`.

- **Khác dự kiến #1 — nợ P6-4 là CỬA VÀO của cả phase, không phải một món tối ưu.** Index scan là
  *"với mỗi mục index, đi tra bảng theo pk"*, tức một phép truy cây nằm **trong callback** của một
  phép duyệt cây. Với `db.Range` cũ (giữ `d.mu` suốt lần duyệt) việc đó **tự khoá chết**. Nên
  phase 7 **không tồn tại được** trước khi P6-4 được trả. Một món nợ ghi là "🔧 tối ưu" hoá ra là
  "chặn cả phase sau".

- **Khác dự kiến #2 — không cần latch-coupling, và câu "cần P4-5 trước" trong sổ nợ là SAI.** Cái
  quyết định một reader **thấy gì** là **ảnh chụp MVCC (phase 6)**, không phải latch (phase 4).
  Latch chỉ còn phải bảo vệ **vị trí** cursor, và một vị trí không cần được bảo vệ — nó cần một
  cách **biết mình đã hết đúng**. Đó là `btree.Tree.gen` + tìm lại chỗ **theo khóa** (*cursor
  restoration* của InnoDB/SQLite). **Cái latch phase 4 phải giữ được tháo bằng một cơ chế của
  phase 6.** Bài học về cách ghi nợ: ghi hiện tượng + cách kiểm chứng, đừng ghi **cách trả**.

- **Khác dự kiến #3 — leaf lưu PRIMARY KEY, không lưu `(PageID, SlotID)`.** Kế hoạch viết
  `index key -> (PageID, SlotID)`. Nhưng hàng ở đây **nằm trong** index của primary key (kiểu
  InnoDB/SQLite), nên nó **di chuyển** mỗi lần leaf split ⇒ một con trỏ vật lý sẽ hỏng, và sửa nó
  thì phải đi sửa **mọi** secondary index. Primary key là một **giá trị logic**: nó không di
  chuyển. Hệ quả kèm theo: truy vấn theo pk **không bao giờ** cần secondary index, và seq scan trả
  về **theo thứ tự pk**.

- **Khác dự kiến #4 — điểm hoà vốn 36.8%, không phải 5-20%, và VÌ SAO quan trọng hơn con số.**
  ROADMAP viết "thường ~5-20%", và hằng số đoán sẵn trong code (`CFetch/CSeq = 20`) cho đúng 4.85%.
  Đo được: `CFetch/CSeq = 4.5x`, hoà vốn **36.8%** — sai 7.5 lần. Lý do: ở quy mô này **không có
  I/O thật nào cả**, cả cây nằm trong buffer pool. Con số 20 ấy **chính là `random_page_cost` của
  Postgres**. **Cố ý không sửa hằng số**, và thêm cột `planner(đoán)` để bảng tự trưng ra ba dòng
  (5%, 10%, 25%) nơi nó chọn kế hoạch chậm hơn **7.7x / 3.9x / 1.4x**.

- **Khác dự kiến #5 — một hằng số chi phí không phải thuộc tính của phép toán.** Nó là thuộc tính
  của phép toán **cộng với thứ tự truy cập**: `CFetch` đo bằng khóa nhảy lung tung đắt hơn đo bằng
  khóa tăng dần **1.6-2.2x**, và **cả hai đều đúng** — cho hai loại index khác nhau. Đó là vì sao
  Postgres lưu `correlation` **riêng cho từng cột**, và vì sao nó không suy ra được từ
  `seq_page_cost`/`random_page_cost`. Phát hiện này đến từ việc bảng deliverable **phản bác dòng
  kết luận của chính nó**.

- **Thêm vào, không có trong kế hoạch:** `make idxlab-bytes` — in **hình dạng byte** của khóa
  composite, không cần database, chạy trong một phần nghìn giây. Nó trả lời *"vì sao index chỉ
  dùng được cho tiền tố bên trái"* bằng **byte thật**: `city='HN'` là khoảng liên tục
  `[06484e0000, 06484e0001)`, còn `age=30` **không là khoảng nào cả**. Nên "leftmost prefix rule"
  không phải một quy ước của MySQL — nó là hệ quả của việc phép so duy nhất mà B+Tree biết là
  `memcmp`.

- **Bốn con bug của lượt chạy, và KHÔNG con nào trong database.** Cả bốn nằm trong **bộ đo**:
  bảng deliverable phản bác kết luận của chính nó; `BenchmarkIndexMaintenance` đo **fsync** suốt
  (1 txn/vòng ⇒ 99.7% con số là durability); lời đọc bảng suy nhân quả ngược (giá UPDATE do **số
  index chứa cột bị đổi** quyết định, ×2 — đúng tối ưu **HOT** của Postgres); và bộ test **xanh
  với một cursor hỏng** (tắt cơ chế `gen` thì `txn`/`table`/`query` xanh cả ba, vì mọi lần quét
  trong chúng chạy **một mình**). Ba trong bốn cái chỉ lộ ra vì có **hai phép đo độc lập** cùng
  nói về một việc.

- **Trả nợ phase trước:** **P6-4** (đầy đủ) và **nửa đọc của P4-5**. Nợ mới: **P7-1 → P7-9**.
  **Không** trả và cố ý: nửa **ghi** của P4-5 (nhiều writer vật lý buộc bỏ pha undo physical của
  phase 5), và histogram — vì `TestEstimateIsWrongOnSkew` đang khẳng định **đúng cái giới hạn ấy**
  và sẽ đỏ khi ai đó trả P7-6.

## Phase 8 — SQL front-end (2-3 ngày) — ✅ xong

Lexer -> parser -> AST -> binder -> optimizer -> planner -> executor. `CREATE TABLE`,
`CREATE INDEX`, `INSERT`, `ANALYZE`, `SELECT ... WHERE ... ORDER BY ... LIMIT`, JOIN hai bảng
(nested loop + Grace hash join), và `EXPLAIN` in ra kế hoạch của chính mình.

**Điều mở màn phase không phải parser.** Phase 6 và 7 xây **mọi** đường đọc theo API **push**
(`Scan(lo, hi, fn)` — vòng lặp thuộc về `Scan`). Không join algorithm nào diễn tả được bằng
callback mà không đệm cả một vế vào RAM (đúng cái P6-4 vừa trả nợ để bỏ), sinh một goroutine mỗi
vế (`txn.Txn` không an toàn nhiều goroutine), hay đảo ngược kiểu continuation. Nên phase 8 mở màn
bằng một **ca mổ**: đổi push -> pull ở `internal/txn` và `internal/table`, với `Iter` là **bản
duy nhất** cài phép merge và `Scan` là lớp bọc. Đo cái giá: **≤1.08x** (so với đúng HEAD phase 7
qua `git worktree`, không so với số cũ trong diary phase 6 — số ấy đã lỗi thời từ phase 7).

**Ba package, và lằn ranh giữa chúng là câu trả lời cho "logical khác physical ở đâu":**

```
internal/sql/     cú pháp. KHÔNG biết catalog -> câu sai tên chết trước khi tốn một lần xuống cây
internal/plan/    ngữ nghĩa + tối ưu. opt.go (logical, ĐỊNH LÝ) tách khỏi planner.go (physical, TÌNH HUỐNG)
internal/exec/    thi hành. KHÔNG biết SQL. Volcano/iterator, có toán tử CHẶN (Sort, build của hash join)
internal/engine/  ghép lại + EXPLAIN in CẢ HAI cây (trước và sau optimizer)
```

**Deliverable:** `make test-sql` (nguyên tắc một câu: *hai đường phải cho CÙNG một kết quả* —
dựng kế hoạch bằng tay để chạy được cả đường planner **không** chọn), `make sqllab` (6 bảng),
`make fuzz-sql` (parser luôn **kết thúc**, và in-lại-rồi-đọc-lại thì bền — bất biến này bắt bug
thật trong **13 giây**).

**Kết quả:** hash join vs nested loop **1.25x → 121.25x** theo kích thước vế ngoài; hạn mức bộ
nhớ là tham số **đổi thuật toán** (dưới/trên ngưỡng tràn = **2.10-2.16x**, còn trên ngưỡng thì
thêm RAM mua được **1.06x** tức nhiễu); cái giá của tràn đĩa là một **bậc thang** không phải
đường dốc (vào ngưỡng 1.46x, rồi giảm hạn mức thêm **20 lần** chỉ lên 1.53x — từ đó suy ra
**so sánh không phải chỗ tốn**, vì khoá run là bộ mã hoá giữ thứ tự của phase 7 nên trộn là
memcmp); bỏ được `Sort` nhờ thứ tự index **2.87x** không LIMIT nhưng **3284-3345x** với
`LIMIT 10` (một **bậc**, vì Sort là toán tử **chặn**); và front-end là **2.9x** phần thi hành của
một truy vấn **điểm** — tự đo được lý do prepared statement và plan cache tồn tại.

**Ba bài học không nằm trong năm câu hỏi:**

1. **Loại bug đắt nhất là một câu ĐÚNG về một database KHÁC.** Tôi cố ý hạ mọi khoảng thành
   `Filter` khi đường đi là seq scan — đúng với Postgres (bảng là **heap**), sai ở đây (engine là
   **clustered index**, hàng nằm **trong** cây pk từ phase 7). Tự tắt phép tối ưu của chính mình:
   **5100 hàng thay vì 105, 4.062ms thay vì 88µs = 46x**. Không test nào bắt được — kết quả đúng.
2. **"Chi phí 0" và "không đọc gì" là hai phát biểu khác nhau**, và chúng không tự đồng bộ. **Năm**
   trong mười hai con bug của phase này **không làm sai kết quả** — chúng chỉ làm engine tốn công.
   Nên: **một bài test cho optimizer phải khẳng định về CÔNG VIỆC ĐÃ LÀM, không chỉ về kết quả.**
3. **Một con số cũ trong nhật ký là một con số vô hình.** Diary phase 6 ghi `BenchmarkScan` =
   688-732µs; phase 7 viết lại `Scan` thành streaming và **không đo lại** (thật ra là **317-363µs**).
   Dùng số cũ thì kết luận sai về nhân quả. Quy tắc mới: **một phase làm đổi một con số của phase
   trước thì phải đo lại và ghi cả hai.**

- **Trả nợ phase trước:** **P7-9** (đầy đủ — `ORDER BY` dùng thứ tự index, và `Filter` đẩy xuống).
  Nợ mới: **P8-1 → P8-12**. Món lớn nhất là **P8-1**: chưa có `BEGIN`/`COMMIT`, nên phase 6 có 4
  mức isolation mà **không viết được** một transaction nhiều câu **bằng SQL**.

---

## Tài liệu (đọc đúng lúc, đừng đọc trước)

- **Database Internals** — Alex Petrov: phase 1-5, sát roadmap này nhất.
- **CMU 15-445** (Andy Pavlo, YouTube): xem lecture tương ứng ngay *trước* mỗi phase.
- **BoltDB** source (~4k dòng Go): đọc hết trong 1 ngày — chính là kiến trúc phase 1-4,
  dùng COW thay cho WAL. Sau đó liếc **badger** (LSM) để thấy đánh đổi ngược lại.
- **Architecture of a Database System** (Hellerstein, ~40 trang): đọc sau phase 6.

## Sau roadmap

Viết thêm một **LSM-Tree mini** trong ~1 tuần. Lúc đó bạn sẽ hiểu đánh đổi thật sự:
**read amplification vs write amplification vs space amplification** —
câu hỏi đầu tiên khi thiết kế DB cho hệ thống lớn.

## Nhật ký — quy tắc ghi

Mỗi phase có một file trong `diary/`. Ghi **trong lúc làm**, không phải sau khi xong.
Quy tắc đầy đủ + checklist chốt phase: [`skills/diary/SKILL.md`](./skills/diary/SKILL.md).

Nguyên tắc bất di bất dịch: **mọi con số và mọi kết luận phải kèm lệnh shell sinh ra nó
và output thật, dán nguyên văn.** Sáu tháng sau mở lại phải chạy lại được, trên máy khác
phải biết vì sao số khác.

Cụ thể, mỗi file nhật ký bắt buộc có:

- **Môi trường** — output của `uname -srmo && go version && df -hT . | tail -1`.
  Không có nó thì mọi benchmark là vô nghĩa.
- **Reproduce toàn bộ phase** — khối lệnh copy-paste chạy lại được từ đầu.
- **Nhật ký theo ngày** — mỗi mục là `console` block: lệnh đã gõ + output thật,
  rồi mới tới "đọc kết quả" và "đang nghĩ gì".
- **Bảng giả thuyết sai** — 4 cột: *tôi tưởng là* / *thực tế là* / **lệnh + output đã lật tẩy nó** /
  *đã sửa thế nào*. Đây là cột giá trị nhất của cả file.
- **Số đo** — kèm lệnh, ngày, commit, máy. Chốt lại bằng **tỉ số**, không phải số tuyệt đối
  (số tuyệt đối đổi theo máy và dao động giữa các lần chạy).
- **Invariant + lệnh kiểm chứng** — invariant nào, cài ở `file:hàm` nào, lệnh nào chứng minh nó.
- **Rút ra** — viết như thể đang giải thích cho người khác.
- **Nợ kỹ thuật** — checkbox những thứ biết là còn thiếu.
