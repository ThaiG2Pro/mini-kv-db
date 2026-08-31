#!/usr/bin/env bash
# Chạy toàn bộ bộ đo Phase 0 trên một máy Linux thuần và lưu kết quả có thể so sánh.
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

echo
echo "==> xong. So với baseline cũ:"
echo "    diff <(jq -r '.results[]|\"\\(.name) \\(.p50_ns)\"' bench/baseline/CŨ/buffered.json) \\"
echo "         <(jq -r '.results[]|\"\\(.name) \\(.p50_ns)\"' $OUT/buffered.json)"
echo "    Nhớ dán env.txt + bảng tỉ số vào diary/phase0.md."
