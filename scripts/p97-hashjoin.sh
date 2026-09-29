#!/usr/bin/env bash
# Trả nợ P9-7 trên máy Linux thuần: hash join 16 batch nhanh hơn 1 batch (Postgres) —
# có phải do cache/TLB không? WSL2 không có PMU nên không đếm được cache miss; ở đây thì được.
#
#   ./scripts/p97-hashjoin.sh              # REPEAT=11
#   REPEAT=21 ./scripts/p97-hashjoin.sh
#
# Cần: docker (compose v2), go 1.26+, perf (linux-tools-$(uname -r)), sudo (perf phải gắn vào
# backend chạy dưới uid postgres của container).
# Kết quả: bench/p97/<host>-<ngày>/  gồm env.txt và hashjoin.txt — dán cả hai vào diary/phase9.md.
set -euo pipefail
cd "$(dirname "$0")/.."

REPEAT=${REPEAT:-11}
OUT="bench/p97/$(hostname)-$(date +%Y%m%d)"
mkdir -p "$OUT"
COMPOSE="docker compose -f reallab/docker-compose.yml"

# --- 0. Điều kiện tiên quyết: thiếu cái nào thì dừng ngay, đừng đo nửa vời ---
command -v perf >/dev/null || { echo "!! thiếu perf: sudo apt install linux-tools-\$(uname -r) linux-tools-generic"; exit 1; }
ls -d /sys/bus/event_source/devices/cpu* >/dev/null 2>&1 \
  || { echo "!! không có PMU (/sys/bus/event_source/devices/cpu*): máy ảo? Đây đúng là lý do rời WSL2."; exit 1; }
sudo -v   # hỏi mật khẩu một lần, trước khi đo

# --- 1. Môi trường: số cache miss vô nghĩa nếu không biết cache to bao nhiêu ---
{
  echo "# ngày: $(date -Is)"
  echo "# host: $(hostname)"
  echo; echo '$ uname -srmo';        uname -srmo
  echo; echo '$ go version';         go version
  echo; echo '$ perf version';       perf version
  echo; echo '$ lscpu | grep -E "Model name|^CPU\(s\)|Thread|cache|MHz"'
  lscpu | grep -E "Model name|^CPU\(s\)|Thread|cache|MHz" || true
  echo; echo '# PMU có mặt (cpu_core + cpu_atom = CPU lai, sự kiện in hai dòng)'
  ls /sys/bus/event_source/devices/ | grep -E '^cpu' || true
  echo; echo '# governor: "performance" thì xung nhịp ít nhảy hơn "powersave"'
  cat /sys/devices/system/cpu/cpu0/cpufreq/scaling_governor 2>/dev/null || echo "(không có cpufreq)"
  echo; echo '# transparent hugepage: bật "always" thì mảng bucket 4MB có thể nằm trên page 2MB → ít TLB miss'
  cat /sys/kernel/mm/transparent_hugepage/enabled 2>/dev/null || true
  echo; echo '$ cat /proc/sys/kernel/perf_event_paranoid'; cat /proc/sys/kernel/perf_event_paranoid
  echo; echo '$ free -h | head -2';  free -h | head -2
  echo; echo '$ uptime';             uptime
  echo; echo '$ docker version --format {{.Server.Version}}'; docker version --format '{{.Server.Version}}'
} | tee "$OUT/env.txt"

# --- 2. Hai Postgres giống hệt nhau, chỉ khác malloc (xem reallab/docker-compose.yml) ---
echo; echo "==> dựng rl-pg và rl-pgm"
$COMPOSE --profile p97 up -d pg pgm
for c in rl-pg rl-pgm; do
  until docker exec "$c" pg_isready -U postgres -q; do sleep 1; done
done

# --- 3. Đo. Lần chạy đầu nạp dữ liệu (1M + 4M hàng, ~1 phút); A–D như trên WSL2, E là perf ---
echo; echo "==> reallab -work hashjoin (REPEAT=$REPEAT)"
(cd reallab && PERF="sudo -n perf" go run . -work hashjoin -db pg,pgm -repeat "$REPEAT") \
  | tee "$OUT/hashjoin.txt"

echo
echo "==> xong: $OUT"
echo "    Đọc bảng E trước. H1 (cache/TLB) đúng thì cột 256MB của cache-misses hoặc"
echo "    dTLB-load-misses phải cao hơn hẳn 1MB (dự báo: ≥ 1 lần trượt / hàng probe)."
echo "    So cột D (độ trễ bộ nhớ) với số của WSL2 trong diary/phase9.md, bảng 8."
echo "    Dừng rl-pgm khi xong: $COMPOSE --profile p97 stop pgm"
