#!/usr/bin/env bash
# Trả nợ P9-1 trên máy Linux thuần: sửa giải mã hàng (commit e48552f, 7 → 2 lần cấp phát mỗi hàng)
# có làm seq scan nhanh lên thật không, và điểm hoà vốn có xuống dưới 15% không?
# Trên WSL2 không trả lời được: số đơn lẻ dao động ±40%, 16 cặp chạy xen kẽ ra tỉ số 1.07.
#
#   ./scripts/p91-seqscan.sh                         # BEFORE=348f120 AFTER=HEAD, PAIRS=16
#   BEFORE=<commit> AFTER=<commit> PAIRS=24 ./scripts/p91-seqscan.sh
#
# Kết quả: bench/p91/<host>-<ngày>/  gồm env.txt, pairs.txt, breakeven-*.txt — dán vào diary/phase9.md.
set -euo pipefail
cd "$(dirname "$0")/.."

BEFORE=${BEFORE:-348f120}
AFTER=${AFTER:-HEAD}
PAIRS=${PAIRS:-16}
ROUNDS=${ROUNDS:-3}
OUT="bench/p91/$(hostname)-$(date +%Y%m%d)"
WT=$(mktemp -d)
mkdir -p "$OUT"
trap 'git worktree remove --force "$WT/before" 2>/dev/null || true; git worktree remove --force "$WT/after" 2>/dev/null || true; rm -rf "$WT"' EXIT

# --- 1. Môi trường. Máy phải rảnh: đây là phép đo thời gian, không phải đếm sự kiện ---
{
  echo "# ngày: $(date -Is)"
  echo "# host: $(hostname)"
  echo "# before: $(git rev-parse --short "$BEFORE")  after: $(git rev-parse --short "$AFTER")"
  echo; echo '$ uname -srmo';  uname -srmo
  echo; echo '$ go version';   go version
  echo; echo '$ lscpu | grep -E "Model name|^CPU\(s\)|cache"'; lscpu | grep -E "Model name|^CPU\(s\)|cache" || true
  echo; echo '# governor'; cat /sys/devices/system/cpu/cpu0/cpufreq/scaling_governor 2>/dev/null || echo "(không có cpufreq)"
  echo; echo '$ uptime'; uptime
  echo; echo '# 5 tiến trình ăn CPU nhất lúc bắt đầu: có gì > 20% thì tắt đi rồi chạy lại'
  ps -eo pcpu,comm --sort=-pcpu | head -6
} | tee "$OUT/env.txt"

# --- 2. Dựng hai bản test binary từ hai commit, để chạy xen kẽ trong cùng điều kiện máy ---
for v in before after; do
  rev=$BEFORE; [ $v = after ] && rev=$AFTER
  git worktree add -q "$WT/$v" "$rev"
  (cd "$WT/$v" && go test -c -o "$WT/q-$v" ./internal/query)
done

# --- 3. BenchmarkSeqStep theo cặp, đổi thứ tự mỗi cặp. Lấy trung vị của tỉ số TỪNG CẶP ---
one() { (cd internal/query && "$WT/q-$1" -test.run '^$' -test.bench SeqStep -test.benchtime 200x -test.benchmem) \
          | awk '/Benchmark/{printf "%.1f %s\n", $3/20000, $(NF-1)}'; }
echo; echo "==> BenchmarkSeqStep, $PAIRS cặp (ns/hàng, allocs/op)"
: > "$OUT/pairs.txt"
for i in $(seq 1 "$PAIRS"); do
  if [ $((i % 2)) = 1 ]; then b=$(one before); a=$(one after); else a=$(one after); b=$(one before); fi
  echo "$b $a" | tee -a "$OUT/pairs.txt"
done
python3 - "$OUT/pairs.txt" <<'EOF' | tee "$OUT/summary.txt"
import statistics as st, sys
r = [l.split() for l in open(sys.argv[1])]
b = [float(x[0]) for x in r]; a = [float(x[2]) for x in r]
ratio = sorted(y / x for x, y in zip(b, a))
print(f"trước: trung vị {st.median(b):.0f} ns/hàng, min {min(b):.0f}, allocs {r[0][1]}")
print(f"sau:   trung vị {st.median(a):.0f} ns/hàng, min {min(a):.0f}, allocs {r[0][3]}")
print(f"tỉ số sau/trước theo cặp: trung vị {st.median(ratio):.3f}, khoảng [{ratio[len(ratio)//4]:.3f}, {ratio[3*len(ratio)//4]:.3f}]")
print("khoảng tứ phân vị nằm trọn dưới 1.0 thì bản sửa nhanh hơn thật; vắt qua 1.0 thì chưa kết luận được")
EOF

# --- 4. Điểm hoà vốn (tiêu chí trả nợ P9-1: < 15%), ROUNDS lượt mỗi bản ---
for v in before after; do
  for r in $(seq 1 "$ROUNDS"); do
    echo; echo "==> idxlab breakeven, $v, lượt $r"
    (cd "$WT/$v" && go run ./cmd/idxlab -work breakeven -rows 50000 -repeat 30) \
      | tee -a "$OUT/breakeven-$v.txt" | grep -E "CFetch/CSeq|TÍNH RA"
  done
done

echo
echo "==> xong: $OUT"
echo "    Hướng dẫn đọc: docs/linux-phase9.md, mục \"Nợ P9-1\"."
