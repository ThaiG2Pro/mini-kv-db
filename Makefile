.PHONY: all test vet fmt bench iolab iolab-full baseline torn pagerlab slotlab bufferlab btreelab bench-btree fuzz fuzz-pool fuzz-btree crashlab crashlab-full crashlab-nosync crashlab-nowrite wallab bench-wal fuzz-db fuzz-txn fuzz-keys fuzz-table idxlab idxlab-breakeven idxlab-bytes bench-index test-index txnlab txnlab-anomaly txnlab-contention bench-txn test-txn reallab-up reallab reallab-down check clean

all: fmt vet test

fmt:
	gofmt -w .

vet:
	go vet ./...

test:
	go test ./... -count=1

bench:
	go test ./... -run '^$$' -bench . -benchmem

# Phase 0: đo 4 sự thật vật lý (fsync, group commit, random vs seq, page cache)
iolab:
	go run ./cmd/iolab -filemb 512

# Bản đầy đủ: p50/p99 + kiểm chứng page cache bằng mincore
iolab-full:
	go run ./cmd/iolab -filemb 512 -repeat 5 -verify-cache

# Baseline có thể so giữa các máy -> bench/baseline/<host>-<ngày>/
# Dùng: make baseline DIR=/mnt/nvme/iolab
baseline:
	./scripts/linux-baseline.sh

# Phase 1: soi file pager trên đĩa — meta page luân phiên + freelist tái dùng
pagerlab:
	go run ./cmd/pagerlab -db data/test.db -commits 8 -alloc 4 -free 3
	@echo; echo "meta page A:"; xxd -l 48 data/test.db
	@echo "meta page B:"; xxd -s 4096 -l 48 data/test.db

# Phase 2: soi một slotted page — chèn, xóa, compact, và churn cho slot phình
slotlab:
	go run ./cmd/slotlab -n 24 -size 120
	@echo; echo "== churn: mảng slot chỉ mọc, không co =="
	go run ./cmd/slotlab -n 24 -size 120 -delete random -seed 7 -churn 4

# Fuzz slotted page. -fuzzminimizetime BẮT BUỘC: mặc định là 60s, và khi
# fuzztime hết trong lúc worker đang minimize thì Go vẫn in PASS dù gần như
# không chạy được gì. Xem diary/phase2.md, bảng giả thuyết sai #4.
fuzz:
	go test ./internal/page/ -run '^$$' -fuzz FuzzSlottedPage -fuzztime 120s -fuzzminimizetime 1s

# Phase 3: buffer pool — hit ratio của từng chính sách, và chỗ LRU gãy
bufferlab:
	go run ./cmd/bufferlab

fuzz-pool:
	go test ./internal/bufpool/ -run '^$$' -fuzz FuzzPoolOps -fuzztime 120s -fuzzminimizetime 1s

# Phase 4: B+Tree — hình dạng cây theo thứ tự chèn, fanout, xóa và trả page
btreelab:
	go run ./cmd/btreelab -n 200000
	@echo; echo "== khóa 8 byte, pool nhỏ =="
	go run ./cmd/btreelab -n 200000 -klen 8 -frames 64

# Deliverable của phase 4: 1 triệu khóa, tăng dần vs ngẫu nhiên, đếm split.
# -benchtime=1000000x để b.N là ĐÚNG 1 triệu, không phải con số Go tự chọn —
# nếu không thì splits/1k của hai kịch bản đo trên hai kích cỡ cây khác nhau.
bench-btree:
	go test ./internal/btree/ -run '^$$' -bench 'Insert' -benchtime=1000000x -benchmem -timeout 30m

fuzz-btree:
	go test ./internal/btree/ -run '^$$' -fuzz FuzzTreeOps -fuzztime 120s -fuzzminimizetime 1s

# ---------- Phase 5: WAL + recovery ----------

# Bài kiểm tra quyết định của phase 5. Chạy nhanh (20 vòng) để dùng thường
# xuyên; deliverable thật là crashlab-full.
crashlab:
	go run ./cmd/crashlab -n 20

# Deliverable: 200 lần kill -9 ở thời điểm ngẫu nhiên, durability không sai
# lần nào. Mỗi vòng đẻ một tiến trình con, để nó chạy 60-700ms rồi SIGKILL.
crashlab-full:
	go run ./cmd/crashlab -n 200 -keep

# Chứng minh bài test biết báo SAI. Tắt fsync rồi chạy lại — nếu vẫn xanh thì
# bài test không kiểm được gì cả. (Kết quả đáng ngạc nhiên: kill -9 KHÔNG đủ
# để lộ ra việc thiếu fsync, vì page cache của kernel sống lâu hơn tiến trình.
# Xem diary/phase5.md.)
crashlab-nosync:
	-go run ./cmd/crashlab -n 20 -nosync

# Bài phản chứng THẬT. -nowrite giữ byte log trong buffer của tiến trình, nên
# kill -9 mang chúng đi cùng — đúng cái mà mất điện làm. Target này PHẢI đỏ:
# nếu nó xanh thì bộ kiểm tra không kiểm gì cả và con số "200/200 đúng" ở trên
# vô giá trị. Dấu - ở đầu để make không dừng vì exit code 1 mong đợi.
crashlab-nowrite:
	-go run ./cmd/crashlab -n 10 -nowrite

# Soi một file WAL: log dài bao nhiêu, gồm gì, bao nhiêu phần trăm là thuế.
wallab:
	@rm -rf data/wal && mkdir -p data/wal
	go run ./cmd/crashlab -child -dir data/wal -seed 1 -txns 400 -frames 16
	go run ./cmd/wallab -tail 12 data/wal/data.db.wal

# Giá của durability, và group commit: cùng số khóa, khác số khóa mỗi txn.
bench-wal:
	go test ./internal/db/ -run '^$$' -bench 'Insert' -benchtime=2000x -timeout 30m
	go test ./internal/db/ -run '^$$' -bench 'Recover' -benchtime=10x -timeout 30m
# Get cần chạy đủ lâu để page nóng lên: ở 2000 vòng mỗi khóa chỉ được tra một
# lần nên đo ra chi phí đọc đĩa, không phải chi phí của đường đọc.
	go test ./internal/db/ -run '^$$' -bench 'Get' -benchtime=300000x -count=3
	go test ./internal/wal/ -run '^$$' -bench . -benchmem

fuzz-db:
	go test ./internal/db/ -run '^$$' -fuzz FuzzCrashRecover -fuzztime 120s -fuzzminimizetime 1s

# ---------- Phase 6: transaction & concurrency control ----------

# Deliverable của phase 6, ba bảng:
#   1. anomaly nào xảy ra ở mức isolation nào — kể cả write skew ở snapshot
#      isolation, cái mà MVCC không bao giờ chặn được
#   2. N goroutine chuyển tiền: tổng số dư VỠ ở hai mức thấp, giữ được ở hai
#      mức cao
#   3. MVCC phình bao nhiêu khi có một reader mở lâu, và vacuum thu lại được gì
txnlab:
	go run ./cmd/txnlab

txnlab-anomaly:
	go run ./cmd/txnlab -work anomaly

# Tranh chấp cực cao (2 tài khoản, 12 goroutine): chỗ điều khiển đồng thời LẠC
# QUAN (snapshot isolation) thoái hoá và BI QUAN (S2PL) thắng. "MVCC luôn nhanh
# hơn lock" là một câu sai, và đây là lệnh chứng minh điều đó.
txnlab-contention:
	go run ./cmd/txnlab -work transfer -accounts 2 -workers 12 -ops 100

# Giá của mỗi mức isolation, giá của abort, và giá của phình version trên
# đường đọc. -benchtime lớn cho Get vì nó ~200ns: ở số vòng nhỏ thì đo ra
# chi phí nạp page, không phải chi phí của mức isolation (bài học phase 5).
bench-txn:
	go test ./internal/txn/ -run '^$$' -bench 'Get|Scan' -benchtime=200000x -count=3
	go test ./internal/txn/ -run '^$$' -bench 'Commit|Abort' -benchtime=500x
	go test ./internal/lock/ -run '^$$' -bench . -benchmem

# Bài test đối chứng của phase 6: bảng anomaly phải đỏ theo CẢ HAI chiều. Mỗi ô
# đều khẳng định mức thấp ĐỂ LỌT chứ không chỉ khẳng định mức cao chặn — một
# bài test chưa bao giờ đỏ thì chưa phải bằng chứng (bài học crashlab-nowrite).
test-txn:
	go test ./internal/txn/ ./internal/lock/ -count=1 -race -v -run 'Anomaly|Transfer|Deadlock'

# Fuzz phase 6, hai target:
#   FuzzChainCodec — encoding của chuỗi version phải CANONICAL và phải từ chối
#     byte rác. Target này đỏ ngay ở giây thứ 3 lần đầu chạy: bit cờ lạ bị nuốt
#     im lặng, và Encode sinh ra được tombstone có thân mà Decode từ chối. Nó
#     cũng kiểm bất biến định nghĩa của Prune trên mọi (horizon, snapshot).
#   FuzzTxnCrash   — chuỗi version đi qua WAL/redo/undo của phase 5 mà không
#     tầng nào biết nó là gì; sau crash không được có chuỗi nào giải mã ra rác.
fuzz-txn:
	go test ./internal/txn/ -run '^$$' -fuzz FuzzChainCodec -fuzztime 120s -fuzzminimizetime 1s
	go test ./internal/txn/ -run '^$$' -fuzz FuzzTxnCrash -fuzztime 120s -fuzzminimizetime 1s

# ---------- Phase 7: secondary index + query ----------

# Deliverable của phase 7, năm bảng. Bảng số 1 là bài chính: cùng một truy vấn,
# ba đường đi (seq scan / index scan / index-only scan), chín độ chọn lọc — và
# ĐIỂM HOÀ VỐN. Bảng còn in hai cột planner: một dùng mô hình chi phí ĐO ĐƯỢC,
# một dùng mô hình ĐOÁN sẵn trong code, để thấy một hằng số lệch làm planner
# chọn sai ở đúng dải nào (= random_page_cost của Postgres).
idxlab:
	go run ./cmd/idxlab -rows 20000 -repeat 20

# Chỉ bảng điểm hoà vốn, bảng to hơn cho số ổn định hơn.
idxlab-breakeven:
	go run ./cmd/idxlab -work breakeven -rows 50000 -repeat 30

# Không cần database: in hình dạng byte của khóa composite. Đây là bảng trả lời
# "vì sao index chỉ dùng được cho tiền tố bên trái" bằng byte thật, không bằng
# lời — và nó chạy trong một phần nghìn giây.
idxlab-bytes:
	go run ./cmd/idxlab -work bytes

# Ba hằng số của mô hình chi phí (một bước quét / một bước index / một lần tra
# bảng), cái giá của index ở đường ghi, và cái mà nợ P6-4 mua được (LIMIT 1).
# -benchtime lớn cho PointLookup vì nó ~1µs: ở số vòng nhỏ thì đo ra chi phí
# nạp page, không phải chi phí của đường đọc (bài học phase 5 và 6).
bench-index:
	go test ./internal/query/ -run '^$$' -bench 'PointLookup|ScanLimit' -benchtime=20000x -count=3
	go test ./internal/query/ -run '^$$' -bench 'SeqStep' -benchtime=30x -count=3
	go test ./internal/query/ -run '^$$' -bench 'PlanSelectivity' -benchtime=20x
	go test ./internal/query/ -run '^$$' -bench 'IndexMaintenance' -benchtime=6000x
	go test ./internal/keys/ -run '^$$' -bench . -benchmem

# Bài test đối chứng của phase 7: ba kế hoạch phải cho CÙNG kết quả (một
# planner đổi kết quả là một database sai), và bất biến hàng<->index phải đúng
# sau chèn/sửa/xóa/crash.
test-index:
	go test ./internal/keys/ ./internal/table/ ./internal/query/ -count=1 -race -v \
		-run 'Order|RoundTrip|Canonical|Index|Plan|Choose|Estimate|Unique|Catalog'

# Fuzz phase 7, hai target:
#   FuzzKeyOrder  — bất biến ĐỊNH NGHĨA của bộ mã hoá khóa: thứ tự byte phải
#     bằng thứ tự logic, ở mọi kiểu, mọi chiều sắp, mọi ca tiền tố và byte
#     0x00. Đây là chỗ một bài test viết tay không bao giờ đủ.
#   FuzzKeyCodec  — canonical: giải mã được thì mã hoá lại phải ra byte cũ.
fuzz-keys:
	go test ./internal/keys/ -run '^$$' -fuzz FuzzKeyOrder -fuzztime 120s -fuzzminimizetime 1s
	go test ./internal/keys/ -run '^$$' -fuzz FuzzKeyCodec -fuzztime 120s -fuzzminimizetime 1s

# FuzzTableIndex — hàng và mục index đi qua chuỗi version (phase 6) rồi qua
# WAL/redo/undo (phase 5) mà không tầng nào biết bên trên có "index". Sau crash
# không được có mục nào trỏ tới hàng không còn, và số mục phải bằng số hàng.
fuzz-table:
	go test ./internal/table/ -run '^$$' -fuzz FuzzTableIndex -fuzztime 120s -fuzzminimizetime 1s

# ---------- phase 8: SQL front-end ----------

# REPL. Gõ SQL, kết thúc câu bằng ';'. `make repl` rồi thử:
#   CREATE TABLE t (a INT, b TEXT, PRIMARY KEY (a));
#   INSERT INTO t VALUES (1,'x'),(2,'y');
#   ANALYZE t;
#   EXPLAIN ANALYZE SELECT b FROM t WHERE a = 1;
repl:
	go run ./cmd/minidb -db data/sql/minidb.db

# Sáu bảng số của phase 8. Mỗi bảng là một câu hỏi của roadmap:
#   join      — nested loop vs hash join, điểm đổi vai
#   budget    — hạn mức bộ nhớ: chỗ hash join buộc phải tràn ra đĩa, và cái giá
#   sort      — ORDER BY trong RAM vs external merge sort, số run
#   pushdown  — predicate pushdown ăn tiền ở đâu trong CHÍNH engine này
#   order     — bỏ bước ORDER BY nhờ thứ tự index (nợ P7-9), và chỗ LIMIT đổi bậc
#   pipeline  — chi phí lex/parse/bind/optimize/plan so với thi hành
sqllab:
	go run ./cmd/sqllab -rows 20000 -dim 200

sqllab-join:
	go run ./cmd/sqllab -work join -rows 20000 -dim 200

sqllab-budget:
	go run ./cmd/sqllab -work budget -rows 20000 -dim 200

sqllab-sort:
	go run ./cmd/sqllab -work sort -rows 20000 -dim 200

# Bài test đối chứng của phase 8, và nguyên tắc của nó chỉ có một câu: HAI
# ĐƯỜNG PHẢI CHO CÙNG MỘT KẾT QUẢ. Nested loop vs hash join, tràn đĩa vs không
# tràn, có Sort vs bỏ Sort, đẩy điều kiện vs không đẩy. Một phép tối ưu làm đổi
# kết quả không phải phép tối ưu, nó là con bug — và nó chỉ lộ ra khi chạy cả
# đường mà planner KHÔNG chọn.
test-sql:
	go test ./internal/sql/ ./internal/plan/ ./internal/engine/ -count=1 -race -v

# FuzzParse — parser nhận đầu vào của NGƯỜI, nên mọi chuỗi byte là hợp lệ. Ba
#   bất biến: không panic, luôn KẾT THÚC, và in lại rồi phân tích lại thì bền.
#   Bất biến "luôn kết thúc" là bất biến đắt nhất: lượt code của phase 8 có một
#   con bug thuộc loại ấy (`WHERE x = = 1` treo vô hạn) và một bài test thường
#   chỉ đỏ khi kết quả sai, còn cái treo thì làm cả bộ test đứng.
# FuzzLexer  — mỗi Next hoặc trả EOF hoặc đẩy con trỏ đi. Khẳng định trực tiếp,
#   thay vì tin rằng mọi nhánh của switch đều có pos++.
fuzz-sql:
	go test ./internal/sql/ -run '^$$' -fuzz FuzzParse -fuzztime 120s -fuzzminimizetime 2s
	go test ./internal/sql/ -run '^$$' -fuzz FuzzLexer -fuzztime 60s -fuzzminimizetime 2s

# fsck: soi file database, thoát 1 nếu có lỗi nghiêm trọng
check:
	go run ./cmd/dbcheck data/test.db

# Torn write (cần root + dm-flakey) — xem docs/linux-baseline.md
torn:
	@echo "Đọc docs/linux-baseline.md mục 'Nợ #1' — cần sudo ./scripts/dm-flakey.sh"

clean:
	rm -rf data bin

# Phase 9: đối chiếu với Postgres / MySQL / MariaDB thật (cần Docker)
reallab-up:
	docker compose -f reallab/docker-compose.yml up -d

reallab:
	cd reallab && go run . -work all -repeat 9

reallab-down:
	docker compose -f reallab/docker-compose.yml down -v
