# minidb so với MySQL / MariaDB / InnoDB

> Viết ngày 2026-09-29, tại commit `afbd2fd` (hết phase 8). Bản anh em: [vs-postgres.md](vs-postgres.md).
> Khi trả một món nợ trong [debts.md](debts.md), sửa lại dòng tương ứng ở cả hai file.
>
> Mốc so sánh: **MySQL 8.4 LTS** (server layer + InnoDB) và **MariaDB 11.x** (fork của InnoDB đã tách
> nhánh khá xa từ 10.5 trở đi). Chỗ nào hai bên khác nhau thì ghi rõ.

Toàn bộ tính năng của minidb đã liệt kê ở [vs-postgres.md — Phần 1](vs-postgres.md#phần-1--toàn-bộ-tính-năng-minidb-đang-có),
không chép lại. File này gồm: **vì sao phép so này khác phép so với Postgres** → **so từng tầng** →
**đơn giản hoá ở đâu** → **chấm điểm**.

---

# Phần 0 — Vì sao so với InnoDB lại cho kết quả khác

minidb **có hình dạng của InnoDB**, không có hình dạng của Postgres. Hai quyết định kiến trúc mà
[vs-postgres.md §3.C](vs-postgres.md#c-không-phải-thiếu--mà-là-chọn-kiến-trúc-khác-và-chọn-đúng) phải bào chữa là
"khác Postgres nhưng đúng" thì ở đây **trùng hẳn**:

| Quyết định | Postgres | InnoDB | minidb |
|---|---|---|---|
| Dữ liệu nằm ở đâu | Heap rời, index trỏ bằng `ctid` | **Clustered index** theo PK | **Clustered index** theo PK |
| Secondary index trỏ tới hàng bằng gì | Địa chỉ vật lý `ctid` | **Giá trị PK** → phải xuống cây lần hai | **Giá trị PK** → phải xuống cây lần hai (`IndexIter` trả PK, ai cần cột khác tự gọi `Get`) |
| Recovery | Redo-only | Redo, rồi **rollback txn dở bằng undo** | ARIES đủ **analysis/redo/undo** |
| Tự động commit từng câu | Có (autocommit) | Có (`autocommit=1` mặc định) | Có, và **chỉ có thế** (nợ P8-1) |
| Serializable | SSI (lạc quan) | **Khoá** (mọi `SELECT` thành `LOCK IN SHARE MODE`) | **S2PL** (khoá) |

Vì vậy phần lõi được điểm cao hơn so với khi so với Postgres. Đổi lại, lộ ra hai **khác biệt thật**
mà khi so với Postgres còn bị che:

1. **Version cũ để ở đâu.** InnoDB để bản mới nhất tại chỗ trong clustered index, còn bản cũ đẩy ra
   **undo log** và nối lại bằng `DB_ROLL_PTR`. minidb để **cả chuỗi version trong chính record của B+Tree**
   → trần ~2KB một khoá (nợ P6-1). Đây là bài học lớn nhất file này chỉ ra được.
2. **Một cây hay nhiều cây.** InnoDB cho **mỗi index một B+Tree riêng** (một segment trong tablespace).
   minidb nhét **mọi bảng và mọi index vào MỘT cây**, tách nhau bằng tiền tố `(kind, oid)`. Cách này
   không giống InnoDB mà giống **MyRocks** (MariaDB, dữ liệu trên RocksDB, khoá mang tiền tố index id) và
   **TiDB** (tương thích MySQL, khoá `t{tableID}_r…` / `t{tableID}_i{indexID}…`).

---

# Phần 2 — So từng tầng

Thang **mức đại diện** giống vs-postgres: ★★★★★ = cùng nguyên lý *và* cùng dạng failure mode ·
★★★ = đúng ý tưởng, đơn giản hoá kỹ thuật · ★ = có tên gọi nhưng không có bản chất · ✗ = không có.

### 2.1 Storage & file

| Tầng | minidb | InnoDB (MySQL 8.4) | MariaDB khác gì | Đại diện |
|---|---|---|---|---|
| **Page** | 4KB, crc32c | **16KB** mặc định (4–64KB), checksum crc32 | Như MySQL | ★★★★ |
| **Tổ chức file** | 1 file = mảng page + freelist | Tablespace → **segment → extent (1MB = 64 page) → page**; `file_per_table` (`.ibd`); header FSP + XDES quản lý extent | Như MySQL | ★★ *(freelist từng page, không có extent — nợ P1-3 đo đúng chỗ này)* |
| **Chống torn page** | **Full page writes** vào WAL (kiểu Postgres) | **Doublewrite buffer**: ghi page vào vùng dblwr + fsync, rồi mới ghi tại chỗ | Có thể tắt doublewrite khi SSD hỗ trợ **atomic write** | ★★★ *(cùng bài toán, lời giải khác — xem §3.B)* |
| **Định dạng record** | Slotted page, slot indirection, compact | Record nối thành **danh sách liên kết đơn theo thứ tự khoá** + **page directory thưa** (mỗi slot giữ 4–8 record); định dạng COMPACT/DYNAMIC | Như MySQL | ★★★ *(indirection có, nhưng InnoDB đổi "mỗi record một slot" lấy "tìm nhị phân trên slot thưa rồi đi tuyến tính")* |
| **Cột ẩn mỗi hàng** | Không | `DB_TRX_ID` (6B), `DB_ROLL_PTR` (7B), `DB_ROW_ID` (6B, khi không có PK) | Như MySQL | ★★ *(minidb bắt buộc có PK, và version nằm trong chuỗi chứ không ở cột ẩn)* |
| **Record lớn** | ✗ không có overflow page (nợ P4-1) | Mỗi hàng tối đa ~nửa page nằm tại chỗ; BLOB/TEXT dài ra **page tràn**, để lại con trỏ 20B (DYNAMIC) | Như MySQL | ✗ |
| **fsck** | `cmd/dbcheck` | `innochecksum`, `CHECK TABLE` | + `mariadb-check` | ★★★ |

### 2.2 Buffer pool

| Tầng | minidb | InnoDB | MariaDB khác gì | Đại diện |
|---|---|---|---|---|
| **Replacer** | LRU / CLOCK / LRU-K, so với Belady | **LRU có điểm chèn giữa**: sublist *old* (3/8) + *young*; page mới vào *old*, chỉ lên *young* nếu bị chạm lại sau `innodb_old_blocks_time` (1s) | Như MySQL | ★★★★★ *(đây chính là lời giải cho sequential flooding mà phase 3 đo được — cùng họ với LRU-K/2Q)* |
| **Nhiều instance** | 1 pool, latch từng frame + latch bảng hash | `innodb_buffer_pool_instances` chia pool để giảm tranh chấp mutex | **Bỏ** nhiều instance từ 10.5 (một pool, latch tốt hơn) | ★★★ |
| **Flush nền** | ✗ (nợ P5-1) | **Page cleaner threads**, flush list theo `oldest_modification`, **adaptive flushing** theo tốc độ sinh redo | Như MySQL | ✗ |
| **Read-ahead** | ✗ (nợ P3-4) | Linear read-ahead (theo extent), random read-ahead | Như MySQL | ✗ |
| **Adaptive hash index** | ✗ | Hash tự dựng trên page nóng; **8.4 tắt mặc định** | Tắt mặc định từ 10.5 | — *(cả hai vendor đã thôi tin vào nó)* |
| **Change buffer** | ✗ | Hoãn ghi vào secondary index khi page không có trong pool; **8.4 tắt mặc định** | **Xoá hẳn** từ 11.0 | — *(cùng trên)* |

### 2.3 B+Tree

| Tầng | minidb | InnoDB | Đại diện |
|---|---|---|---|
| **Cấu trúc** | Split/merge/redistribute, sibling pointer, cursor | Như vậy + liên kết anh em **hai chiều** | ★★★★ |
| **Chèn tăng dần** | Đo được: tăng dần vs ngẫu nhiên = **33x writes/op** | **Tách page ở điểm chèn** khi thấy chèn tuần tự (không chia 50/50) → page đầy ~15/16; đây là lý do InnoDB khuyên PK `AUTO_INCREMENT` | ★★★★★ *(minidb có đúng heuristic này: `RightmostSplit` cắt 100/0 ở leaf cực phải — `internal/btree/split.go`; phase 4 đo được lá đặc ~100% vs ~70%)* |
| **Ngưỡng merge** | Dưới nửa page | `MERGE_THRESHOLD` = 50%, **chỉnh được theo từng index** | ★★★★ |
| **Latch khi ghi** | Nửa đọc có latch-coupling, nửa ghi còn nợ (P4-5) | Lạc quan trước (chỉ latch lá), lỗi thì chạy lại bi quan với **SX-lock trên index** | ★★ |
| **Nén khoá** | ✗ (nợ P4-2) | ✗ trên định dạng thường; chỉ có ở bảng `COMPRESSED` (zlib cả page) | ★★★★ *(InnoDB cũng không làm prefix compression — phần này minidb không kém)* |
| **Dựng index** | Chèn từng khoá trong một txn (nợ P7-2) | **Sorted index build**: sắp xếp trước rồi dựng cây từ dưới lên | ★★ |

### 2.4 Redo, undo, recovery

| Tầng | minidb | InnoDB | MariaDB khác gì | Đại diện |
|---|---|---|---|---|
| **LSN** | = offset byte trong log | = số byte redo đã sinh (cũng là offset byte) | Như MySQL | ★★★★★ |
| **Kiểu log** | Physical (diff theo khối) + ảnh page | **Physiological**: nhắm một page, mô tả thao tác logic trong page | Đổi định dạng redo ở 10.8 | ★★★★ |
| **Nguyên tử nhiều page** | Một txn nhiều record | **Mini-transaction (mtr)**: gom các thay đổi của một thao tác cấu trúc (vd split) thành một nhóm redo không chia cắt | Như MySQL | ★★★ *(minidb không tách "nguyên tử vật lý" với "nguyên tử logic")* |
| **Log vòng** | ✗ log mọc mãi (nợ P5-2) | **Vòng tròn**, dung lượng cố định (`innodb_redo_log_capacity`); đầy → **ép flush đồng bộ, câu ghi bị treo** | Một file `ib_logfile0` | ✗ *(failure mode kinh điển ở đây là "checkpoint age quá cao", không phải "đầy đĩa")* |
| **Mức bền khi commit** | fsync mỗi commit + group commit | `innodb_flush_log_at_trx_commit` = 1 / 2 / 0 | Như MySQL | ★★★★ *(phase 0 và 5 đã đo chính cái giá này)* |
| **Undo** | Ảnh-trước **trong WAL**, chỉ dùng để rollback | **Undo log riêng** trong undo tablespace; dùng cho **cả** rollback **lẫn** MVCC; bản thân undo cũng được ghi redo | Như MySQL | ★★ *(khác biệt lớn nhất — xem §3.B)* |
| **Recovery** | Analysis → redo → undo, sinh CLR | Quét redo → áp redo → rollback txn dở bằng undo (**chạy nền**, DB mở cho truy cập trước khi rollback xong) | Như MySQL | ★★★★ *(cùng dạng ARIES; InnoDB không gọi là CLR nhưng thao tác undo được ghi redo như thay đổi thường)* |
| **Checkpoint** | Fuzzy, ATT + DPT ghi vào log | Fuzzy; không có DPT tường minh — checkpoint LSN = `oldest_modification` nhỏ nhất trên flush list | Như MySQL | ★★★★ |
| **2PC với binlog** | ✗ | Commit = **XA nội bộ** giữa binlog và redo, để replica và máy chính không lệch nhau sau crash | Như MySQL | ✗ *(bài học riêng của MySQL: hai log, một quyết định commit)* |

### 2.5 Transaction, MVCC, khoá

| Tầng | minidb | InnoDB | MariaDB khác gì | Đại diện |
|---|---|---|---|---|
| **Chỗ để version cũ** | **Cả chuỗi trong record** của B+Tree → trần ~2KB (nợ P6-1), `DecodeChain` giải mã cả chuỗi (nợ P6-2) | Bản mới nhất tại chỗ; bản cũ **dựng lại từ undo** theo `DB_ROLL_PTR` | Như MySQL | ★★ |
| **Snapshot** | Snapshot theo txn | **Read view** (`m_low_limit_id`, `m_up_limit_id`, danh sách txn đang chạy) | Như MySQL | ★★★★ |
| **Version trên secondary index** | Mọi index đều là khoá trong cùng cây có chuỗi version | Secondary index **không có version**: xoá = đánh dấu xoá; nếu `PAGE_MAX_TRX_ID` mới hơn read view thì **phải quay về clustered index** để kiểm | Như MySQL | ★★ *(minidb index-only scan rẻ hơn InnoDB ở chỗ này)* |
| **ReadUncommitted** | Dirty read thật | **Dirty read thật** (khác Postgres) | Như MySQL | ★★★★★ |
| **ReadCommitted** | Snapshot mỗi câu | Snapshot mỗi câu; tắt gap lock | Như MySQL | ★★★★★ |
| **RepeatableRead** | **Snapshot isolation thật**, first-committer-wins | **Mặc định.** Đọc thường = snapshot, nhưng `UPDATE`/`SELECT … FOR UPDATE` đọc **bản mới nhất** ("current read") → **không** có first-committer-wins, **lost update xảy ra được** | **11.6+: `innodb_snapshot_isolation=ON` mặc định** → báo lỗi khi ghi đè version mới hơn snapshot, tức là có first-committer-wins | ★★★ *(RR của minidb **chặt hơn** MySQL, đúng bằng MariaDB mới)* |
| **Serializable** | **S2PL**: bỏ snapshot, dùng lock manager | RR + **mọi `SELECT` thường thành `LOCK IN SHARE MODE`** + next-key lock | Như MySQL | ★★★★★ *(cùng thuật toán — nợ P6-7 "không phải SSI" không phải là nợ khi so với InnoDB)* |
| **Loại khoá hàng** | S/X, **khoá điểm + khoá khoảng** | S/X trên **index record**; record / **gap** / **next-key** / insert-intention lock; khoá intention IS/IX ở mức bảng | Như MySQL | ★★★★ *(khoá khoảng của minidb là next-key lock ở dạng tổng quát)* |
| **Khoá ngầm** | ✗ | Hàng vừa chèn được khoá **ngầm** bằng `DB_TRX_ID`, chỉ tạo lock struct khi có kẻ tranh chấp | Như MySQL | ✗ |
| **Tổ chức lock table** | Không có chỉ mục theo đối tượng (nợ P6-3) | Hash theo **(space, page)**, mỗi lock struct là **bitmap theo heap_no** — một struct khoá được cả page | Như MySQL | ★★ *(đây là lời giải có sẵn cho P6-3)* |
| **Deadlock** | Wait-for graph + wound-wait theo tuổi | Wait-for graph dò **ngay lúc chờ** (`innodb_deadlock_detect`), nạn nhân = txn **ít undo nhất**; tắt đi thì chỉ còn `innodb_lock_wait_timeout` | Như MySQL | ★★★★ |
| **Dọn version** | `Vacuum()` thủ công (nợ P6-5) | **Purge threads** chạy nền; failure mode: txn chạy lâu → **history list length** phình, câu đọc chậm dần vì phải đi chuỗi undo dài | Như MySQL | ★★ |

### 2.6 Catalog, khoá, index

| Tầng | minidb | MySQL / InnoDB | MariaDB khác gì | Đại diện |
|---|---|---|---|---|
| **Catalog** | Lưu **trong chính DB** | 8.0+: **data dictionary là bảng InnoDB** (`mysql.ibd`) — trước 8.0 là file `.frm` | **Vẫn dùng `.frm`** + bảng hệ thống | ★★★★★ vs MySQL · ★★★ vs MariaDB |
| **So sánh khoá** | **Codec giữ thứ tự**: `bytes.Compare` == thứ tự logic | So từng cột bằng **comparator theo kiểu + collation** (không phải memcmp) | **MyRocks** dùng đúng định dạng *memcomparable* như minidb | ★★ vs InnoDB · ★★★★★ vs MyRocks |
| **Secondary index** | Mục = (cột index, PK); unique → PK nằm ở value, non-unique → PK nằm trong khoá | Mục = (cột index, PK); unique kiểm trên phần cột index | Như MySQL | ★★★★★ |
| **Covering / index-only** | `indexOnlyOp`, không cần hỏi gì thêm | "Using index", nhưng phải xét `PAGE_MAX_TRX_ID` (§2.5) | Như MySQL | ★★★★ |
| **Loại index** | B+Tree | B+Tree, FULLTEXT, SPATIAL (R-tree); hash chỉ ở engine MEMORY | + vector index (11.7) | ★★★ |
| **DDL an toàn** | ✗ không có DDL locking (nợ P7-3); back-fill một txn (nợ P7-2) | **Metadata lock (MDL)**; online DDL (`ALGORITHM=INPLACE/INSTANT`) ghi **row log** trong lúc dựng index rồi áp nốt | Như MySQL (+ atomic DDL từ 10.6) | ✗ *(MDL + row log là lời giải trực tiếp của P7-3 và P7-2)* |

### 2.7 Optimizer & executor (server layer)

| Tầng | minidb | MySQL 8.4 | MariaDB khác gì | Đại diện |
|---|---|---|---|---|
| **Mô hình thi hành** | Volcano / pull | **Iterator** (Volcano) từ 8.0.18 | Executor cũ, lồng vòng theo bảng | ★★★★★ vs MySQL |
| **Mô hình chi phí** | 3 hằng số, **hiệu chuẩn bằng bench** | Hằng số trong bảng `mysql.server_cost` / `mysql.engine_cost`, **sửa được bằng SQL** | Hằng số đặt bằng biến `optimizer_*_cost`, đã hiệu chuẩn lại ở 11.0 | ★★★★ |
| **Ước lượng selectivity** | `ANALYZE` → min/max, **phân bố đều** (nợ P7-6) | **Index dive**: với range trên cột có index, xuống cây thật để đếm (tới `eq_range_index_dive_limit`=200 khoảng); thống kê InnoDB lấy mẫu 20 page; histogram (8.0+) chỉ dùng cho cột **không** có index | **Engine-independent stats** + histogram từ 10.0, JSON_HB từ 10.8 | ★★ *(index dive là đường tắt rẻ cho P7-6: minidb đã có cursor, chỉ việc đếm)* |
| **Thứ tự join** | 2 bảng, không tìm kiếm | Tìm kiếm **tham lam** có cắt tỉa, độ sâu `optimizer_search_depth` | Như MySQL | ★ |
| **Thuật toán join** | Nested loop + **Grace hash join tràn đĩa** | NL (+ BKA/MRR); **hash join từ 8.0.18** (thay BNL từ 8.0.20), tràn đĩa theo chunk; **không có merge join** | BNL, BNLH (hash theo khối), BKA — chọn qua `join_cache_level` | ★★★★ |
| **Sắp xếp** | External merge sort, run file + k-way merge | **filesort**: `sort_buffer_size`, merge pass trên file tạm | Như MySQL | ★★★★★ |
| **Loại bỏ Sort bằng index** | Thuộc tính vật lý bắt buộc truyền xuống | Dùng index để khỏi "Using filesort" | Như MySQL | ★★★★★ |
| **Đẩy điều kiện** | Pushdown logic + transitive propagation | + **Index Condition Pushdown** ("Using index condition"): đẩy điều kiện **xuống storage engine** để lọc trước khi tra clustered index | Như MySQL, + split-materialized | ★★★ |
| **Plan cache** | ✗ (nợ P8-6) | **Không có** — prepared statement chỉ giữ cây parse, tối ưu lại mỗi lần chạy; query cache (cache *kết quả*) **đã xoá ở 8.0** | Query cache **vẫn còn** (tắt mặc định) | ★★★★ *(thiếu giống hệt MySQL — nhưng minidb đã đo được cái giá của việc thiếu)* |
| **`EXPLAIN`** | In **cả cây logical trước/sau optimizer** + physical | Bảng, `FORMAT=TREE`, `FORMAT=JSON`, `EXPLAIN ANALYZE`; **optimizer trace** (`information_schema.OPTIMIZER_TRACE`) in từng bước viết lại và từng phương án bị loại | `ANALYZE <câu>`, optimizer trace từ 10.4 | ★★★ *(optimizer trace **dạy nhiều hơn** minidb: nó in cả phương án thua kèm chi phí)* |

### 2.8 Mặt SQL & vận hành

| Tầng | minidb | MySQL / MariaDB | Đại diện |
|---|---|---|---|
| **Transaction trong SQL** | ✗ `BEGIN`/`COMMIT` (nợ P8-1) — tương đương `autocommit=1` bị khoá cứng | Đủ + savepoint, `SET TRANSACTION ISOLATION LEVEL` | ✗ |
| **DML** | ✗ `UPDATE`/`DELETE` (nợ P8-2) | Đủ + `INSERT … ON DUPLICATE KEY UPDATE`, `REPLACE` | ✗ |
| **`GROUP BY`, aggregate, subquery, CTE, window** | ✗ (nợ P8-11) | Đủ (CTE/window từ MySQL 8.0 / MariaDB 10.2) | ✗ |
| **Kiểu dữ liệu** | NULL, BOOL, INT, UINT, TEXT | ~30 kiểu + JSON; **collation** là một chiều riêng của mọi phép so chuỗi | ★ |
| **Nhiều storage engine** | Một "engine" duy nhất | **Handler API**: InnoDB, MyISAM, MEMORY… | MariaDB thêm **Aria**, **MyRocks**, ColumnStore, Spider | ★★ *(`engine` ↔ `table/btree` của minidb đã có sẵn đường ranh server/engine, chỉ chưa thành interface)* |
| **Replication** | ✗ | Binlog (statement/row), GTID, Group Replication | + **Galera** (đồng bộ nhiều master) | ✗ |
| **Client/server, quyền** | ✗ thư viện nhúng + REPL | Thread-per-connection, wire protocol, `GRANT` | + thread pool có sẵn | ✗ |

---

# Phần 3 — Đơn giản hoá ở đâu (nhìn từ InnoDB)

### A. Đơn giản hoá đúng chỗ
Giống vs-postgres §3.A (ít kiểu dữ liệu, ít loại index, không có client/server hay replication). Có thêm
ba chỗ mà **InnoDB cũng đã bỏ hoặc tự thừa nhận là sai**:

- **Không có adaptive hash index, không có change buffer.** MySQL 8.4 tắt cả hai theo mặc định, MariaDB xoá hẳn change buffer. Không làm là không mất gì.
- **Không có prefix compression trong B+Tree** (nợ P4-2). InnoDB định dạng thường cũng không có.
- **Không có plan cache** (nợ P8-6). MySQL cũng không có. Dù vậy, vẫn nên trả khoản này vì minidb đã đo được front-end đắt gấp 2.9x exec.

### B. Đơn giản hoá làm mất một bài học ⚠️

| Thiếu | Mất cái gì | Lời giải của InnoDB |
|---|---|---|
| **Version cũ nằm trong record** (P6-1, P6-2) | Không thấy được vì sao DB thật tách "dữ liệu hiện tại" khỏi "lịch sử"; trần 2KB/khoá; mỗi lần đọc phải giải mã cả chuỗi | **Undo log** + `DB_ROLL_PTR`: record chỉ giữ bản mới nhất, bản cũ dựng lại khi có read view cần. Cùng một undo phục vụ **cả rollback lẫn MVCC** — minidb đang giữ hai cơ chế riêng (ảnh-trước trong WAL cho rollback, chuỗi trong record cho MVCC) |
| **Không có purge nền** (P6-5) | Không gặp được **history list length** phình vì một txn chạy lâu — failure mode số một của InnoDB khi vận hành | Purge threads + chỉ số `trx_rseg_history_len` |
| **Log không vòng** (P5-2) | Không gặp **checkpoint age** và cảnh câu ghi bị treo vì redo đầy | Redo vòng dung lượng cố định + adaptive flushing |
| **Không có page cleaner** (P5-1) | Không thấy mối quan hệ tốc độ sinh redo ↔ tốc độ flush | Page cleaner + flush list |
| **Không có DDL locking** (P7-3) | Không thấy `CREATE INDEX` chạy song song với ghi thì hỏng gì | **MDL** + online DDL có row log |
| **RR của minidb chặt hơn MySQL** | Người học dễ tưởng mọi RepeatableRead đều chặn lost update. MySQL mặc định **không chặn** | Thêm một ô vào bảng anomaly của phase 6: "RR kiểu MySQL (current read)". Đây là **bài học mới**, không phải nợ |
| **Torn page chỉ chống bằng full page writes** | Chưa thấy lời giải thứ hai là doublewrite, và cái giá của nó: ghi mọi page **hai lần** chứ không chỉ page chạm đầu tiên sau checkpoint | Doublewrite buffer. Nợ P0-1 (torn write thật) là chỗ để đo hai cách cạnh nhau |

### C. Chọn kiến trúc khác (và chọn đúng)

1. **Một cây cho mọi bảng và index.** InnoDB tách mỗi index ra một cây. minidb chọn kiểu keyspace phẳng của **MyRocks / TiDB / CockroachDB**: codec giữ thứ tự + tiền tố. Cái giá là một cây rất sâu, thêm tranh chấp latch ở gốc. Đổi lại, catalog, bảng và index đi chung một đường WAL/MVCC/lock mà không cần code riêng. Với một repo để học thì đổi như vậy là đúng.
2. **Codec memcomparable thay cho comparator theo kiểu.** InnoDB phải gọi comparator của từng kiểu và từng collation ở mỗi lần so. minidb chỉ cần `bytes.Compare`. Đây cũng là lựa chọn của MyRocks. Lý do không phải cho nhanh, mà vì sort, merge và cây dùng chung được **một** phép so.
3. **Undo tách khỏi MVCC** (ngược với §B dòng 1) vẫn là cách dạy dễ hiểu hơn, vì mỗi cơ chế chỉ làm một việc. Nhưng cần ghi rõ đây là **phiên bản giản lược cho việc học**, không phải một kiến trúc DB thật nào cũng dùng.

---

# Phần 4 — Chấm mức đại diện tổng thể

| Nhóm tầng | vs InnoDB | vs Postgres | Vì sao lệch |
|---|---|---|---|
| **Storage / pager / slotted page** | **~65%** | ~85% | InnoDB có extent/segment, page directory thưa, doublewrite, page tràn — nhiều cấu trúc hơn heap của Postgres |
| **Buffer pool** | **~70%** | ~75% | LRU-K/Belady trúng đúng ý tưởng midpoint insertion; thiếu page cleaner + read-ahead |
| **B+Tree** | **~75%** | ~70% | Có tách lệch cực phải như InnoDB; thiếu latch ghi, sorted index build |
| **Redo + undo + recovery** | **~75%** | ~85% | Cùng dạng ARIES (cao hơn Postgres về *hình dạng*), nhưng undo không phải một kho riêng, log không vòng, không có 2PC với binlog |
| **Transaction / MVCC / lock** | **~70%** | ~70% | Serializable trùng thuật toán, RU thật; nhưng chỗ để version cũ khác hẳn, thiếu purge, lock table chưa theo page |
| **Catalog + codec + index** | **~80%** | ~70% | Clustered + secondary-trỏ-bằng-PK **trùng khít**; codec giống MyRocks hơn InnoDB |
| **Planner** | **~45%** | ~45% | Cost model sửa được như MySQL; thiếu index dive / histogram, thiếu tìm thứ tự join |
| **Executor** | **~65%** | ~55% | MySQL cũng không có merge join; iterator + hash join tràn đĩa + filesort trùng khít |
| **SQL surface** | **~20%** | ~20% | Như nhau |
| **Vận hành** | **~0%** | ~0% | Không đặt ra mục tiêu |

**Kết luận:** về *hình dạng kiến trúc*, minidb gần InnoDB hơn gần Postgres: clustered index, secondary
index trỏ bằng PK, recovery có undo, Serializable bằng khoá, autocommit từng câu. Về *chi tiết cài đặt*,
nó lại giản lược hơn InnoDB ở ba chỗ đúng là xương sống của InnoDB: **undo log là nơi chứa lịch sử**,
**redo vòng + page cleaner**, và **lock table theo page**. Còn nếu chỉ xét riêng keyspace và codec thì
minidb là một **MyRocks thu nhỏ đặt trên B+Tree**.

Ba món trả nợ **đáng nhất khi nhìn từ phía InnoDB** (khác thứ tự trong vs-postgres):

1. **P8-1 `BEGIN`/`COMMIT`**: vẫn đứng đầu, lý do như cũ. Sau khi làm xong, thêm được ngay ô "RR kiểu MySQL" (§3.B) vào bảng anomaly.
2. **P6-1 + P6-5: chuyển version cũ ra một vùng undo, rồi thêm purge nền.** Đây là món đắt nhất (vài buổi), nhưng là bài học riêng mà chỉ phép so với InnoDB mới chỉ ra. Nó cũng gỡ luôn trần 2KB/khoá.
3. **P7-6 bằng index dive** thay cho histogram: minidb đã có cursor, chỉ cần đếm số khoá trong `Span`. Ước tính khoảng 1 giờ, và planner hết chọn sai trên dữ liệu lệch.

---

# Phần 5 — Đã kiểm bằng số đo (phase 9)

Các nhận định ở trên được kiểm trên MySQL 8.4.11 và MariaDB 11.8.9 thật. Lệnh và output nằm ở
[`diary/phase9.md`](../diary/phase9.md).

| Nhận định | Kết quả đo |
|---|---|
| Secondary index trỏ bằng PK → mỗi lần tra là thêm một lần xuống cây | Phí tra một hàng: minidb 2142ns, MySQL 2060ns, MariaDB 1830ns; Postgres (`ctid`) 725ns |
| RR của MySQL để lọt lost update; MariaDB 11.6+ chặn nhờ `innodb_snapshot_isolation` | ✅ MySQL `X`, MariaDB `.a` với `Error 1020 (HY000)`; tắt biến thì MariaDB ra `X` |
| Serializable của InnoDB là khoá, giống minidb | ✅ `.w` (chờ) ở 3 anomaly, `.a` chỉ khi deadlock thật (`1213`) |
| Chèn ngẫu nhiên vào clustered B+Tree | ✅ ghi page 26-32x (minidb 33x), bảng to hơn 1.43-1.62x, nạp chậm hơn 6.5-11x |
| Version cũ ở undo → chỉ phiên cũ trả giá | ✅ bảng 1.0x, người đọc mới 1.0x, người đọc cũ 18-35x; history list đếm **transaction**, không đếm hàng |
| Index dive thay cho histogram | ✅ MySQL/MariaDB không bị thống kê cũ lừa; nhưng truy cập `ref` dùng cardinality (giả định đều) và chọn index cho 98% bảng, chậm 7x |
