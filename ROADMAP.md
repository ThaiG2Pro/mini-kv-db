# minidb — Roadmap học DB internals bằng Go

> Mục tiêu: không học "lệnh SQL", mà học **invariant** và **failure mode** của một database.
> Sau roadmap này, đọc `EXPLAIN` của Postgres/MySQL bằng trực giác của người đã tự viết ra nó.

**Nguyên tắc xuyên suốt:** mỗi phase kết thúc bằng **một crash-test hoặc một benchmark**
chứng minh bạn hiểu — không phải bằng "code chạy được".

Môi trường: Go 1.26.2, Linux (WSL2).

---

## Kiến trúc mục tiêu (bottom-up)

```
SQL-ish layer      (phase 8, optional)
Transaction/MVCC   (phase 6-7)  <- concurrency, isolation
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
| 2 | Slotted page: record biến độ dài, compact, tuple id | 1-2 ngày | Fuzz insert/delete/compact, offset không bao giờ chồng lấn |
| 3 | Buffer pool: pin/unpin, dirty, CLOCK/LRU-K, latch | 2 ngày | Hit-ratio bench zipfian: LRU vs CLOCK |
| 4 | **B+Tree**: search/insert/split/delete/merge, cursor | 4-6 ngày | Property test cây cân bằng + bench sequential vs random insert |
| 5 | **WAL + recovery**: ARIES-lite (analysis/redo/undo), checkpoint | 3-4 ngày | 200 lần `kill -9` ngẫu nhiên -> durability không sai lần nào |
| 6 | Transaction & concurrency: 2PL, deadlock, MVCC snapshot isolation | 3-4 ngày | Tái tạo được từng anomaly, và chứng minh mức isolation cao chặn nó |
| 7 | Secondary index, composite key, iterator, index scan vs seq scan | 2-3 ngày | Bench tìm điểm hòa vốn selectivity |
| 8 | (tùy chọn) SQL front-end: parser -> planner -> executor, join | 2-3 ngày | Chạy được `SELECT ... JOIN ... WHERE` |

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

## Phase 7 — Index nâng cao + query cơ bản (2-3 ngày)

- Secondary index: `index key -> (PageID, SlotID)`; non-unique key; covering index.
- Composite key và **quy tắc left-most prefix** — vì sao index `(a,b)` vô dụng với `WHERE b = ?`.
- Volcano/iterator model: `Scan -> Filter -> Project`.
- **Index scan vs seq scan**: viết cost estimate đơn giản (selectivity từ histogram),
  rồi bench để tìm điểm hòa vốn (thường ~5-20% số hàng thì seq scan lại nhanh hơn).

Đây chính là kỹ năng "tối ưu DB cho công ty lớn".

## Phase 8 — (tùy chọn) SQL front-end (2-3 ngày)

Lexer -> parser -> AST -> planner -> executor.
Chỉ cần `CREATE TABLE`, `INSERT`, `SELECT ... WHERE`, `ORDER BY`, và JOIN đơn giản
(nested loop + hash join). Mục tiêu: hiểu join algorithm và vì sao hash join cần memory budget
-> spill to disk.

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
Quy tắc đầy đủ + checklist chốt phase: [`skills/diary-skill.md`](./skills/diary-skill.md).

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
