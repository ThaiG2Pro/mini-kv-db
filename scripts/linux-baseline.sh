#!/usr/bin/env bash
# Chạy toàn bộ bộ đo Phase 0–3 trên một máy Linux thuần và lưu kết quả có thể so sánh.
# Phase 0: nợ P0-2, P0-3, P0-4, P0-5. Phase 1–3: nợ P1-6, P2-5, P3-5 (bench chạy lại).
# Nợ phase 9 có script riêng: scripts/p97-hashjoin.sh.
#
#   ./scripts/linux-baseline.sh            # bộ mặc định
#   DIR=/mnt/nvme ./scripts/linux-baseline.sh    # đo trên ổ khác
#
# Kết quả: bench/baseline/<host>-<ngày>/  gồm env.txt, *.txt và *.json
set -euo pipefail
cd "$(dirname "$0")/.."

DIR=${DIR:-./data/iolab}
FILEMB=${FILEMB:-512}
REPEAT=${REPEAT:-5}
OUT="bench/baseline/$(hostname)-$(date +%Y%m%d)"
mkdir -p "$OUT"

echo "==> ghi vào $OUT (thư mục đo: $DIR)"

# --- 1. Môi trường: không có phần này thì mọi con số vô nghĩa ---
{
  echo "# ngày: $(date -Is)"
  echo "# host: $(hostname)"
  echo
  echo '$ uname -srmo';            uname -srmo
  echo; echo '$ go version';       go version
  echo; echo '$ nproc';            nproc
  echo; echo '$ free -h | head -2'; free -h | head -2
  echo; echo "\$ df -hT $DIR";     df -hT "$DIR" 2>/dev/null || df -hT .
  echo; echo '# /tmp là tmpfs thì fsync trong đó không tốn gì: bench phase 1 phải chạy trên $DIR'
  echo '$ findmnt -no SOURCE,FSTYPE -T /tmp'; findmnt -no SOURCE,FSTYPE -T /tmp 2>/dev/null || true
  echo; echo "\$ findmnt -no SOURCE,FSTYPE,OPTIONS -T $DIR"
  findmnt -no SOURCE,FSTYPE,OPTIONS -T "$DIR" 2>/dev/null || echo "(không có findmnt)"
  echo; echo '# thiết bị khối (rota=1 là đĩa quay, 0 là SSD/NVMe)'
  echo '$ lsblk -o NAME,ROTA,MODEL,SIZE,TYPE'
  lsblk -o NAME,ROTA,MODEL,SIZE,TYPE 2>/dev/null || echo "(không có lsblk)"
  echo; echo '# write cache của ổ đang bật hay tắt? (ảnh hưởng TRỰC TIẾP tới số fsync)'
  for d in /sys/block/*/queue/write_cache; do
    [ -e "$d" ] && echo "$d = $(cat "$d")"
  done
  echo; echo '# scheduler I/O'
  for d in /sys/block/*/queue/scheduler; do
    [ -e "$d" ] && echo "$d = $(cat "$d")"
  done
} | tee "$OUT/env.txt"

run() { # $1=tên  $2...=cờ
  local name=$1; shift
  echo; echo "==> $name"
  go run ./cmd/iolab -dir "$DIR" -filemb "$FILEMB" -json "$OUT/$name.json" "$@" \
    | tee "$OUT/$name.txt"
}

# --- 2. Bộ đo chuẩn: nhiều vòng để p50/p99 có ý nghĩa (nợ #3) ---
run buffered -repeat "$REPEAT" -verify-cache

# --- 3. O_DIRECT: bỏ qua page cache, đo I/O thật (nợ #2) ---
#     Một số filesystem (tmpfs, overlayfs, một số cấu hình btrfs) không hỗ trợ.
if run direct -repeat 3 -direct; then :; else
  echo "!! O_DIRECT thất bại trên $DIR — ghi lại điều đó vào diary, nó cũng là một kết quả."
fi

# --- 4. Bench của phase 1–3: tỉ số nào sống sót qua máy khác? (nợ P1-6, P2-5, P3-5) ---
#     b.TempDir() dùng $TMPDIR. Mặc định là /tmp, và trên nhiều bản Linux /tmp là tmpfs:
#     fsync thành miễn phí, tỉ số Commit/CommitNoSync 481x sụp về ~1x mà không báo gì.
#     Nên ép TMPDIR về đúng ổ đang đo.
mkdir -p "$DIR/tmp"
bench() { # $1=tên  $2...=đối số của go test
  local name=$1; shift
  echo; echo "==> $name"
  TMPDIR="$DIR/tmp" go test "$@" | tee "$OUT/$name.txt"
}
bench p1-pager   ./internal/pager   -run XXX  -bench . -benchtime=200x -count=5
bench p2-page    ./internal/page    -run XXX  -bench . -benchmem -count=5
bench p3-bufpool ./internal/bufpool -run '^$' -bench . -benchmem -count=5 -cpu 1,6
rm -rf "$DIR/tmp"

echo
echo "==> xong. So với baseline cũ:"
echo "    diff <(jq -r '.results[]|\"\\(.name) \\(.p50_ns)\"' bench/baseline/CŨ/buffered.json) \\"
echo "         <(jq -r '.results[]|\"\\(.name) \\(.p50_ns)\"' $OUT/buffered.json)"
echo "    Nhớ dán env.txt + bảng tỉ số vào diary/phase0.md."
echo "    Phase 1–3: tính lại tỉ số từ p1-pager.txt, p2-page.txt, p3-bufpool.txt theo bảng trong"
echo "    docs/linux-baseline.md (mục \"Nợ P1-6, P2-5, P3-5\"), dán vào diary của từng phase."
