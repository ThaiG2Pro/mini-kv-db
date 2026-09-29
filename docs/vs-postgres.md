# minidb so với PostgreSQL

> Viết ngày 2026-09-04, sau khi đọc lại toàn bộ cây nguồn (29.595 dòng Go, 15 package `internal/`, 13 `cmd/`)
> tại commit `afbd2fd` (hết phase 8). Khi trả một món nợ trong [debts.md](debts.md), sửa lại dòng tương ứng ở đây.

Bốn phần: **minidb có gì** → **so từng tầng với Postgres** → **cố tình đơn giản hoá ở đâu, và chỗ nào sự đơn giản hoá làm MẤT bài học** → chấm điểm tổng thể.

---

# Phần 1 — Toàn bộ tính năng minidb đang có

### Tầng vật lý & pager (`internal/pager`, phase 0-1)
| Có | Chi tiết |
|---|---|
| File = mảng page cố định 4096B | `PageSize = 4096` — bằng page của kernel |
| `pread`/`pwrite` | Không `Seek+Read` (không thread-safe) |
| **Meta page kép luân phiên** + crc32c | `MetaPageOf(txnID) = txnID % 2`; mở file chọn cái txnID lớn **và** checksum hợp lệ — cơ chế atomicity của BoltDB |
| Freelist có phân trang | `Allocate`/`Free`, `pending` (page freed nhưng chưa commit) |
| Chính sách cấp phát | `AllocPolicy` (đã đo, giả thuyết ban đầu sai — nợ P1-3) |
| `posix_fadvise` | Điều khiển page cache trong `iolab` |
| fsck | `cmd/dbcheck`: meta, freelist, double-free, page mồ côi |

### Slotted page (`internal/page`, phase 2)
Record biến độ dài · slot array mọc từ trái / cell mọc từ phải · slot indirection (compact không làm hỏng con trỏ ngoài) · `compact()` · dead slot · `Verify()` bất biến · fuzz 1,42 triệu lần.

### Buffer pool (`internal/bufpool`, phase 3)
`Pin`/`Unpin`, cờ dirty, latch mỗi frame + latch bảng hash · **ba replacer**: `NewLRU`, `NewClock`, `NewLRUK(n,k)` · so với **Belady** (optimal) · hook WAL-rule (không evict dirty page trước khi log của nó fsync).

### B+Tree (`internal/btree`, phase 4)
Node = 1 page · search nhị phân trong page · insert + **split lan từ dưới lên** · **delete + merge/redistribute** (không bỏ nửa khó) · `nextLeaf` sibling pointer · `Cursor.Seek/Next` · `Verify()` 7 bất biến · journal hook để WAL log được · bench 1M khoá (tăng dần vs ngẫu nhiên = 33x writes/op).

### WAL + recovery (`internal/wal`, `internal/db`, phase 5)
| Có | Chi tiết |
|---|---|
| 10 loại record | BEGIN, UPDATE, **CLR**, COMMIT, ABORT, ALLOC, FREE, ROOT, CKPT-BEGIN, CKPT-END |
| LSN = offset byte | pageLSN trên mỗi page |
| **Full page writes** | Bật mặc định; `NoFullPageWrites` để chứng minh vì sao cần |
| Diff theo khối | Chỉ log phần byte đổi |
| Group commit | Đo được ở phase 0 và 5 |
| **ARIES 3 pha** | analysis → redo → undo, undo dùng max-heap theo LSN, sinh CLR |
| Fuzzy checkpoint | ATT + DPT ghi vào CKPT-END; auto mỗi 4 MiB log |
| Chứng minh | **200/200 lần `kill -9`**, và `-nowrite` phải **ĐỎ 10/10** (bài phản chứng) |

### Transaction & concurrency (`internal/txn`, `internal/lock`, phase 6)
| Có | Chi tiết |
|---|---|
| MVCC | Chuỗi version tại chỗ theo khoá, snapshot, deferred write (write set trong RAM tới lúc commit) |
| **4 mức isolation** | ReadUncommitted, ReadCommitted, RepeatableRead (= **snapshot isolation** thật), Serializable |
| First-committer-wins | Ở RepeatableRead |
| **S2PL** | Serializable bỏ snapshot, dùng lock manager |
| Lock manager | Mode S/X, **khoá điểm và khoá KHOẢNG**, wait-for graph, **deadlock detection**, wound-wait theo `SetAge` |
| Vacuum | `Vacuum()` thủ công + `ChainStats()` đo phình version |
| `GetForUpdate`, `Count` | Đường lấy lock X khi đọc |
| Chứng minh | Bảng 5 anomaly × 4 mức khớp lý thuyết **từng ô, khẳng định hai chiều**; chuyển tiền N goroutine; và **chỗ MVCC thua lock** |

### Khoá, catalog, index (`internal/keys`, `internal/table`, phase 7)
| Có | Chi tiết |
|---|---|
| **Codec giữ thứ tự** | `bytes.Compare` == thứ tự logic. NULL/BOOL/INT (đảo bit dấu)/UINT/BYTES (escape `0x00`→`0x00 0xff`) |
| Composite + ASC/DESC | DESC = phép **bù byte**; `Order []bool` |
| Canonical | Fuzz: mã hoá lại phải ra đúng byte cũ |
| Catalog | Bảng + index lưu trong **chính DB**, `CREATE TABLE`/`CREATE INDEX`, OID |
| **Clustered index** | Mọi bảng & index nằm trong **MỘT** B+Tree, tách nhau bằng tiền tố `(kind, oid)` — như SQLite/InnoDB |
| Secondary index | Composite, **UNIQUE** (unique đổi *hình dạng khoá*, không chỉ thêm một phép kiểm) |
| Bất biến hàng↔index | Fuzz + sống qua crash + mở lại |
| Mô hình chi phí | `CostModel{CSeq, CIndex, CFetch}`, `DefaultCost` (đoán) vs **`MeasuredCost`** (hiệu chuẩn từ bench), `BreakEven()` |
| `Stats`/`Selectivity` | Phân bố **đều** (không histogram) |

### SQL front-end (`internal/sql`, `plan`, `exec`, `engine`, phase 8)
| Tầng | Có |
|---|---|
| `sql` | Lexer (33 từ khoá) + parser precedence-climbing + AST. **Chỉ cú pháp**, không biết catalog. Lỗi có **vị trí + dấu caret**. `String()` in lại được (round-trip, có escape `''`) |
| Câu lệnh | `CREATE TABLE` (PK dạng table-constraint), `CREATE [UNIQUE] INDEX`, `INSERT ... VALUES`, `SELECT`, `EXPLAIN`, `ANALYZE` |
| `SELECT` | Cột/`*`/alias `AS`, `FROM a [alias]`, `INNER JOIN ... ON`, `WHERE` (AND/OR, `= <> < <= > >=`), `ORDER BY ... ASC/DESC`, `LIMIT` |
| `plan` — binder | Phân giải tên/alias, kiểm kiểu, **logic ba giá trị** (`NULL = NULL` → NULL, AND không short-circuit trên NULL) |
| `plan` — `opt.go` (**logical→logical**) | **Predicate pushdown**, **transitive propagation** (`a.x=b.y AND b.y<5` ⟹ `a.x<5`), dedup idempotent |
| `plan` — `planner.go` (**vật lý**) | Liệt kê access path (seq / index / **index-only**), `Span` (lo/hi, đóng/mở), **phát hiện vô nghiệm → `NoScan`**, chọn thứ tự join, **thuộc tính vật lý bắt buộc** (`req []SortKey`) truyền xuống để **loại bỏ Sort** khi index đã cho sẵn thứ tự |
| `exec` — Volcano/**pull** | `seqScan`, `indexScan`, `indexOnly`, `empty`, `filter`, `project`, `limit`, **nested loop join**, **Grace hash join có tràn đĩa** (32 partition), **external merge sort** (run file + k-way merge bằng `bytes.Compare` thuần) |
| `engine` | Cả đường ống + **`EXPLAIN` in CẢ HAI cây** (logical trước/sau optimizer, và physical kèm `rows≈`/`cost≈`) |
| Giao diện | `cmd/minidb` REPL |

### Công cụ đo (thứ Postgres **không** có tương đương)
13 `cmd/*lab` — mỗi phase một bộ bảng số: `iolab`, `tornlab`, `pagerlab`, `slotlab`, `bufferlab`, `btreelab`, `crashlab`, `wallab`, `txnlab`, `idxlab`, `sqllab`, `dbcheck`. Cộng 8 fuzz target, `-race`, 358 test / 28 package.

---

# Phần 2 — So với PostgreSQL, từng tầng

Thang **mức đại diện**: ★★★★★ = cùng nguyên lý *và* cùng dạng failure mode · ★★★ = đúng ý tưởng, đơn giản hoá kỹ thuật · ★ = có tên gọi nhưng không có bản chất · ✗ = không có.

| Tầng | minidb | PostgreSQL | Đại diện |
|---|---|---|---|
| **Page & file** | 4KB, page tự mô tả, crc32c | 8KB, checksum tuỳ chọn, FSM + visibility map riêng | ★★★★ |
| **Cách để dữ liệu** | **Clustered index**: mọi thứ trong 1 B+Tree | **Heap** rời + index trỏ bằng `ctid`; KHÔNG có index-organized table | ★★ *(khác kiến trúc — xem §3.C)* |
| **Slotted page** | Có, + compact | Có (`ItemId` array), + HOT update, line pointer redirect | ★★★★ |
| **Record lớn** | ✗ không có overflow page (nợ P4-1) → trần ~2KB/khoá | **TOAST**: out-of-line + nén, tới 1GB/field | ✗ |
| **Buffer pool** | LRU / CLOCK / LRU-K, so với Belady | clock-sweep + `usage_count`, **ring buffer** cho seq scan, bgwriter | ★★★★ |
| **Prefetch / async I/O** | ✗ (nợ P3-4) | `effective_io_concurrency`, prefetch khi bitmap scan, AIO (PG18) | ✗ |
| **B+Tree** | split/merge/redistribute, cursor | Lehman-Yao B-link (**đọc không cần khoá cha**), prefix truncation, dedup, page deletion 2 pha | ★★★ |
| **Latch-coupling ghi** | ✗ nửa ghi còn nợ (P4-5) | Có | ★★ |
| **WAL** | LSN=offset, 10 loại record, diff khối, **full page writes**, group commit | LSN, ~200 rmgr record type, full page writes, `synchronous_commit` 5 mức, `commit_delay` | ★★★★ |
| **Recovery** | **ARIES đủ 3 pha, có CLR + undo** | **Redo-only**! Postgres không undo lúc recovery — abort là ghi CLOG rồi để MVCC + VACUUM dọn | ★★★ *(minidb giống InnoDB/ARIES hơn Postgres)* |
| **Checkpoint** | Fuzzy, ATT+DPT, auto theo byte log | Fuzzy, spread checkpoint, restartpoint | ★★★★ |
| **Cắt/tái dùng log** | ✗ log mọc mãi (nợ P5-2) | WAL segment 16MB, recycle, archive | ✗ |
| **PITR / backup / replica** | ✗ | pg_basebackup, PITR, streaming + logical replication, slot | ✗ |
| **MVCC** | Chuỗi version **tại chỗ theo khoá**, deferred write | Version là **tuple trong heap** (`xmin`/`xmax`), snapshot = (xmin, xmax, xip[]) | ★★★ |
| **4 mức isolation** | RU / RC / RR(=SI) / SER | RU **bị nâng thành RC** (Postgres không có dirty read); RC / RR(=SI) / SER | ★★★★ *(minidb làm được RU thật — hơn Postgres một ô)* |
| **Serializable** | **S2PL** (lock, bi quan) | **SSI** (lạc quan, dò rw-antidependency + dangerous structure) | ★★ *(nợ P6-7 — hai thuật toán khác hẳn)* |
| **Lock manager** | S/X, điểm + khoảng, wait-for graph, deadlock detect | 8 lock mode, row lock qua `xmax`, predicate lock (SIREAD), deadlock detect sau timeout | ★★★★ |
| **Vacuum** | `Vacuum()` thủ công + đo phình chuỗi | **autovacuum**, freeze + chống wraparound, visibility map, index cleanup, bloat | ★★ |
| **Kiểu dữ liệu** | NULL, BOOL, INT, UINT, TEXT | ~40 kiểu built-in + numeric/date/interval/array/JSONB/range/geometry + **kiểu do người dùng định nghĩa** | ★ |
| **Ràng buộc** | PK, UNIQUE, NOT NULL (qua `Schema.Check`) | + FOREIGN KEY, CHECK, EXCLUDE, DEFERRABLE | ★★ |
| **Loại index** | B+Tree | B-tree, Hash, **GiST, SP-GiST, GIN, BRIN** + partial / expression / covering `INCLUDE` | ★★ |
| **Index-only scan** | Có (`indexOnlyOp`) | Có — **nhưng phải hỏi visibility map** (minidb không cần: version nằm ngay trong cây) | ★★★★ |
| **Thống kê** | `ANALYZE` → n_rows + min/max, selectivity **phân bố đều** | histogram + MCV list + n_distinct + correlation + extended statistics (đa cột) | ★★ *(nợ P7-6)* |
| **Mô hình chi phí** | 3 hằng số, **hiệu chuẩn bằng bench thật** | ~8 hằng số (`seq_page_cost`, `random_page_cost`, `cpu_*`) + `effective_cache_size` | ★★★★ |
| **Chọn thứ tự join** | 2 bảng, không có tìm kiếm | Quy hoạch động tới 12 bảng, rồi **GEQO** (di truyền) | ★ |
| **Luật viết lại logic** | pushdown + **transitive propagation** | + subquery pull-up, outer-join reordering, constant folding, `IN`→semi-join, partition pruning, ~30 luật | ★★★ |
| **Toán tử thi hành** | seq/index/index-only scan, NL join, **Grace hash join tràn đĩa**, **external merge sort**, filter/project/limit | + **merge join**, bitmap heap scan, **hash/group aggregate**, Materialize, **Memoize**, Gather (**song song**), WindowAgg, SetOp, Recursive CTE | ★★★ |
| **Toán tử chặn & tràn đĩa** | Đo được thành **bậc thang**, không phải dốc | `work_mem`, batch của hash join, tape sort | ★★★★★ |
| **Song song / JIT** | ✗ | Parallel seq scan/hash join, JIT biểu thức bằng LLVM | ✗ |
| **Plan cache** | ✗ (nợ P8-6, **đã tự đo được lý do phải có**: front-end đắt hơn exec 2.9x ở point query) | Prepared statement, generic vs custom plan | ✗ |
| **`EXPLAIN`** | In **cả cây logical trước/sau optimizer** lẫn physical + `rows≈`/`cost≈` | `EXPLAIN (ANALYZE, BUFFERS, VERBOSE)` — nhưng **chỉ physical** | ★★★★ *(minidb dạy nhiều hơn ở chỗ này)* |
| **DML** | ✗ **không có `UPDATE`/`DELETE` trong SQL** (nợ P8-2) — có ở tầng KV | Đủ + `RETURNING`, upsert, MERGE | ✗ |
| **Transaction trong SQL** | ✗ **không có `BEGIN`/`COMMIT`** (nợ P8-1) — 4 mức isolation **không gõ tới được từ SQL** | Đủ + savepoint, `SET TRANSACTION` | ✗ |
| **`GROUP BY` / hàm tổng hợp / `DISTINCT` / subquery / view / CTE** | ✗ (nợ P8-11) | Đủ | ✗ |
| **`NOT`, `IS NULL`, `LIKE`, `IN`, `BETWEEN`** | ✗ (nợ P8-3) — nhưng **logic 3 giá trị bên dưới đã đúng** | Đủ | ★ |
| **Client/server** | ✗ thư viện nhúng + REPL | Multi-process, wire protocol, TLS, auth, role/GRANT, pooling | ✗ |
| **Extension / FDW / partition / trigger / stored proc** | ✗ | Đủ | ✗ |

---

# Phần 3 — Cố tình đơn giản hoá ở đâu

Chia làm ba loại, và loại B mới là loại đáng lo.

### A. Đơn giản hoá **đúng chỗ** — bỏ đi mà không mất bài học nào
Vì bài học nằm ở *nguyên lý*, còn thứ bị bỏ chỉ là *quy mô kỹ thuật*:

- **1 kiểu index thay vì 6.** GIN/BRIN là cấu trúc khác, không phải hiểu biết khác về "index là gì".
- **4 kiểu dữ liệu.** Cái phải hiểu là *codec giữ thứ tự* — và nó đã đúng ở INT (đảo bit dấu), BYTES (escape) và DESC (bù byte). Thêm `numeric` chỉ là thêm một `AppendField`.
- **Không client/server, không auth, không replication.** Không tầng nào ở trên đổi vì thiếu chúng.
- **2 bảng trong join.** Bài học của join order search cần ≥4 bảng, nhưng bài học của *hash join vs nested loop* thì 2 bảng đã đủ và đã đo ra điểm đổi vai W≈3.
- **Không song song, không JIT.** Là tối ưu hằng số, không đổi asymptotic.
- **Không TOAST.** Trần 2KB/khoá là phiền, nhưng "record lớn thì để ra ngoài" là một câu, không phải một bài học.

### B. Đơn giản hoá **làm mất một bài học** ⚠️
Đây là danh sách thật sự đáng để ý:

| Thiếu | Mất cái gì |
|---|---|
| **`BEGIN`/`COMMIT` trong SQL** (P8-1) | Cả phase 6 — 4 mức isolation, deadlock, S2PL — **không chạm tới được từ mặt SQL**. Hai nửa hay nhất của repo không nối vào nhau. Đây là món nợ lớn nhất. |
| **`UPDATE`/`DELETE`** (P8-2) | Không thấy được *chuỗi version lớn lên*, không thấy vì sao cần VACUUM từ góc người dùng, không có write-write conflict ở mức SQL. |
| **`GROUP BY` / aggregate** (P8-11) | Mất toán tử chặn thứ hai (hash aggregate) và bài học "aggregate cũng tràn đĩa". Rẻ nhất để trả — dùng lại được `spill.go`. |
| **Serializable = S2PL, không phải SSI** (P6-7) | Không thấy được *lạc quan* khác *bi quan* ở mức serializable; không thấy false-positive abort của SSI. |
| **Selectivity phân bố đều, không histogram** (P7-6) | Không thấy được vì sao planner *đoán sai* trong đời thật — mà đoán sai vì thống kê lệch mới là nguyên nhân #1 của plan xấu ở Postgres. |
| **Không cắt WAL** (P5-2) | Không gặp được "đĩa đầy vì WAL" — một failure mode kinh điển. |
| **Không autovacuum** (P6-5) | Không gặp bloat, không gặp wraparound. |
| **Không plan cache** (P8-6) | Đã **tự đo được** rằng front-end đắt hơn exec 2.9x ở point query — biết lý do mà chưa làm. |
| **Không có ring buffer cho seq scan** (P3-4) | Phase 3 đo được *sequential flooding* làm hỏng LRU, nhưng chưa cài cách chữa mà Postgres dùng. |

### C. Không phải "thiếu" — mà là **chọn kiến trúc khác** (và chọn đúng)
Đừng chấm điểm hai chỗ này là khuyết:

1. **Clustered index thay vì heap.** minidb giống **SQLite/InnoDB**, không giống Postgres. Hệ quả thật sự khác nhau, và repo đã đo được: `WHERE pk < v` là **range scan thật** dù không có secondary index nào — trực giác "heap Postgres thì seq scan không dùng được range" là **sai** ở đây, và chính chỗ đó từng gây một con bug 46x. Cũng vì thế index-only scan không cần visibility map.
2. **ARIES có undo + CLR.** Postgres *không* undo lúc recovery (abort = ghi CLOG, để MVCC dọn). minidb ở đây học được thứ **Postgres không dạy được** — đúng bài của InnoDB/SQL Server. Điểm trừ duy nhất: nó khiến người học dễ tưởng mọi DB đều undo lúc recovery.

---

# Phần 4 — Chấm mức đại diện tổng thể

| Nhóm tầng | Đại diện | Nhận xét |
|---|---|---|
| **Storage / pager / slotted page** | **~85%** | Thiếu đúng TOAST + FSM |
| **Buffer pool** | **~75%** | Thiếu ring buffer + prefetch + bgwriter |
| **B+Tree** | **~70%** | Thiếu B-link, prefix truncation, latch-coupling ghi |
| **WAL + recovery** | **~85%** | Cao nhất repo. Thiếu cắt log, archive, PITR |
| **Transaction / MVCC / lock** | **~70%** | 4 mức thật, deadlock thật; thiếu SSI và autovacuum |
| **Index + catalog + codec khoá** | **~70%** | Codec rất chuẩn; thiếu histogram và các loại index khác |
| **Planner** | **~45%** | Khung đúng (logical/physical, cost, thuộc tính vật lý), nhưng thống kê thô và không tìm thứ tự join |
| **Executor** | **~55%** | Volcano + tràn đĩa rất thật; thiếu aggregate, merge join, song song |
| **SQL surface** | **~20%** | Yếu nhất — và là chỗ người ngoài nhìn vào |
| **Vận hành (server, replica, backup, quyền)** | **~0%** | Không đặt ra mục tiêu |

**Kết luận thẳng:** minidb đại diện **rất tốt cho nửa dưới** của một RDBMS — nếu tính từ file lên tới query planner, nó là một **~70-85% Postgres về mặt nguyên lý và failure mode**, và ở hai chỗ (WAL/ARIES đủ 3 pha, `EXPLAIN` in cả cây logical) nó còn **dạy được nhiều hơn** bản Postgres thật. Nửa trên — mặt SQL — mới là **~20%**, và điều đó **đảo ngược** cảm giác thông thường: người dùng thấy `SELECT` chạy sẽ tưởng đây là một DB non, trong khi thứ non thật sự chỉ là lớp vỏ, còn lõi thì gần thật.

Ba món trả nợ **rẻ nhất mà nâng mức đại diện nhiều nhất**, theo đúng thứ tự lợi/chi phí:
1. **P8-1 `BEGIN`/`COMMIT`** — nối phase 6 vào mặt SQL. Gần như không có code mới, chỉ là nối dây.
2. **P8-11 `GROUP BY` + aggregate** — dùng lại `spill.go`, được thêm một toán tử chặn.
3. **P8-2 `UPDATE`/`DELETE`** — mở ra chuỗi version, VACUUM và write-write conflict *nhìn thấy được*.

---

# Phần 5 — Đã kiểm bằng số đo (phase 9)

Kiểm trên PostgreSQL 17.11. Lệnh và output nằm ở [`diary/phase9.md`](../diary/phase9.md).

| Nhận định | Kết quả đo |
|---|---|
| Postgres nâng RU thành RC, không có dirty read | ✅ ô `dirty-read × read-uncomm` = `.` |
| Serializable là SSI (lạc quan) | ✅ không bao giờ `.w`; write skew bị huỷ với `could not serialize access due to read/write dependencies` |
| Heap không theo thứ tự PK | ✅ UUIDv4 vs tăng dần: heap 1.00x, index PK 1.27x, tổng thời gian 1.02-1.81x (InnoDB 6.5-11x) |
| Version cũ nằm trong heap, VACUUM không trả dung lượng | ✅ 1 triệu version cũ: heap 11x, **mọi** người đọc chậm 5x; sau `VACUUM` vẫn 155MB, phải `VACUUM FULL` |
| Thống kê: MCV + `CREATE STATISTICS` | ✅ đúng với phân bố lệch và (sau khi tạo) với cột tương quan; **nhưng** tin thống kê cũ: ước lượng 1 hàng cho 100000 hàng thật |
| Điểm hoà vốn của minidb (36.8%) cao vì "không có I/O" | ❌ **sai**: Postgres chạy trong RAM vẫn hoà vốn ở 4.7%. minidb cao vì seq scan đắt 15x mỗi hàng (P9-1) |
