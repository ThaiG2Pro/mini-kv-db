.PHONY: all test vet fmt bench iolab iolab-full baseline torn pagerlab slotlab bufferlab btreelab bench-btree fuzz fuzz-pool fuzz-btree crashlab crashlab-full crashlab-nosync crashlab-nowrite wallab bench-wal fuzz-db check clean

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

# fsck: soi file database, thoát 1 nếu có lỗi nghiêm trọng
check:
	go run ./cmd/dbcheck data/test.db

# Torn write (cần root + dm-flakey) — xem docs/linux-baseline.md
torn:
	@echo "Đọc docs/linux-baseline.md mục 'Nợ #1' — cần sudo ./scripts/dm-flakey.sh"

clean:
	rm -rf data bin
