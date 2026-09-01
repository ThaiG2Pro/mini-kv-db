.PHONY: all test vet fmt bench iolab iolab-full baseline torn pagerlab slotlab bufferlab fuzz fuzz-pool check clean

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

# fsck: soi file database, thoát 1 nếu có lỗi nghiêm trọng
check:
	go run ./cmd/dbcheck data/test.db

# Torn write (cần root + dm-flakey) — xem docs/linux-baseline.md
torn:
	@echo "Đọc docs/linux-baseline.md mục 'Nợ #1' — cần sudo ./scripts/dm-flakey.sh"

clean:
	rm -rf data bin
