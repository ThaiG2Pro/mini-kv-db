#!/usr/bin/env bash
# Chạy lại mọi phép đo THỜI GIAN của phase 4–9 trên một máy Linux thuần.
# Các số ĐẾM (độ đầy lá, số split, số record redo, số lần thử lại, phình version...) không phụ
# thuộc máy nên không có ở đây. Danh sách tỉ số cần so: docs/linux-phase4-9.md.
#
#   ./scripts/linux-phase4-9.sh                  # phase 4 5 6 7 8, ~45 phút
#   PHASES="6 7" ./scripts/linux-phase4-9.sh     # chỉ vài phase
#   PHASES="9" ./scripts/linux-phase4-9.sh       # bảng đo giờ của phase 9 (cần Docker)
#   DIR=/mnt/nvme ./scripts/linux-phase4-9.sh    # đo trên ổ khác
#
# Kết quả: bench/phase4-9/<host>-<ngày>/  gồm env.txt và p<N>-*.txt
set -euo pipefail
cd "$(dirname "$0")/.."

DIR=${DIR:-./data/bench}
PHASES=${PHASES:-"4 5 6 7 8"}
OUT="bench/phase4-9/$(hostname)-$(date +%Y%m%d)"
mkdir -p "$OUT" "$DIR/tmp"
DIR=$(cd "$DIR" && pwd)
# b.TempDir() và os.TempDir() đều theo $TMPDIR. /tmp là tmpfs thì fsync miễn phí và mọi tỉ số
# có fsync (phase 5: 261x, 453x; phase 6: commit/abort 4046x) sụp mà không báo gì.
export TMPDIR="$DIR/tmp"
trap 'rm -rf "$DIR/tmp"' EXIT

has() { [[ " $PHASES " == *" $1 "* ]]; }
run() { # $1=tên file  $2...=lệnh
  local name=$1; shift
  echo; echo "==> $name: $*"
  "$@" 2>&1 | tee "$OUT/$name.txt"
}

{
  echo "# ngày: $(date -Is)"
  echo "# host: $(hostname)   commit: $(git rev-parse --short HEAD)   phases: $PHASES"
  echo; echo '$ uname -srmo';  uname -srmo
  echo; echo '$ go version';   go version
  echo; echo '$ lscpu | grep -E "Model name|^CPU\(s\)|cache"'; lscpu | grep -E "Model name|^CPU\(s\)|cache" || true
  echo; echo '# governor'; cat /sys/devices/system/cpu/cpu0/cpufreq/scaling_governor 2>/dev/null || echo "(không có cpufreq)"
  echo; echo "# TMPDIR=$TMPDIR"
  echo "\$ findmnt -no SOURCE,FSTYPE,OPTIONS -T $TMPDIR"; findmnt -no SOURCE,FSTYPE,OPTIONS -T "$TMPDIR" 2>/dev/null || true
  echo; echo '# write cache của ổ (ảnh hưởng thẳng tới mọi số có fsync)'
  for d in /sys/block/*/queue/write_cache; do [ -e "$d" ] && echo "$d = $(cat "$d")"; done
  echo; echo '$ uptime'; uptime
  echo; echo '# 5 tiến trình ăn CPU nhất: có gì > 20% thì tắt rồi chạy lại'
  ps -eo pcpu,comm --sort=-pcpu | head -6
} | tee "$OUT/env.txt"

# --- Phase 4: B+Tree. Tỉ số thời gian: quét / tra điểm ~15x, tìm trong page / một Get ~9x ---
if has 4; then
  run p4-scan       go test ./internal/btree/ -run '^$' -bench 'Scan|SearchInPage' -benchtime=20x -count=3
  run p4-getpool    go test ./internal/btree/ -run '^$' -bench 'GetPool' -benchtime=200000x -count=3
  run p4-copyalloc  go test ./internal/btree/ -run '^$' -bench 'CopyVsAlloc' -count=5
  run p4-insert     go test ./internal/btree/ -run '^$' -bench 'Insert' -benchtime=1000000x -benchmem -timeout 30m
fi

# --- Phase 5: WAL. Giá của durability (261x, 453x), Diff 3.5x, đường đọc 1.03x / 1.06x ---
if has 5; then
  run p5-insert     go test ./internal/db/ -run '^$' -bench 'Insert' -benchtime=2000x -count=3 -timeout 30m
  run p5-recover    go test ./internal/db/ -run '^$' -bench 'Recover' -benchtime=10x -timeout 30m
  run p5-get        go test ./internal/db/ -run '^$' -bench 'Get' -benchtime=300000x -count=3
  run p5-wal        go test ./internal/wal/ -run '^$' -bench . -benchmem -count=3
fi

# --- Phase 6: transaction. commit/abort 4046x, serializable/RR 2.35x, chuỗi version (bài 13), lock 22.3x ---
if has 6; then
  run p6-get        go test ./internal/txn/ -run '^$' -bench 'Get|Scan' -benchtime=200000x -count=3
  run p6-commit     go test ./internal/txn/ -run '^$' -bench 'Commit|Abort' -benchtime=500x -count=3
  run p6-lock       go test ./internal/lock/ -run '^$' -bench . -benchmem -count=3
fi

# --- Phase 7: index + planner. Hoà vốn, CFetch/CSeq, thuế index, memcmp 5.7x, LIMIT 5032x ---
if has 7; then
  run p7-idxlab     go run ./cmd/idxlab -rows 20000 -repeat 20
  run p7-breakeven  go run ./cmd/idxlab -work breakeven -rows 50000 -repeat 30
  run p7-lookup     go test ./internal/query/ -run '^$' -bench 'PointLookup|ScanLimit' -benchtime=20000x -count=3
  run p7-seqstep    go test ./internal/query/ -run '^$' -bench 'SeqStep' -benchtime=30x -count=3
  run p7-plansel    go test ./internal/query/ -run '^$' -bench 'PlanSelectivity' -benchtime=20x
  run p7-maint      go test ./internal/query/ -run '^$' -bench 'IndexMaintenance' -benchtime=6000x
  run p7-keys       go test ./internal/keys/ -run '^$' -bench . -benchmem -count=3
fi

# --- Phase 8: SQL executor. Join, tràn đĩa, sort, pushdown, LIMIT qua Sort, front-end ---
if has 8; then
  for w in join budget sort order pushdown pipeline; do
    run "p8-$w"     go run ./cmd/sqllab -work "$w" -rows 20000 -dim 200 -repeat 7
  done
  run p8-scan       go test ./internal/txn/ -run '^$' -bench 'BenchmarkScan' -benchtime=300x -count=3
fi

# --- Phase 9: các bảng đo giờ trên DB thật (bảng 1, và group commit của blog bài 1) ---
#     Không gồm crash/lograte (đếm, không đo giờ) và P9-1 / P9-7 (có script riêng).
if has 9; then
  docker compose -f reallab/docker-compose.yml up -d pg mysql maria
  for c in rl-pg; do until docker exec "$c" pg_isready -U postgres -q; do sleep 1; done; done
  sleep 15   # MySQL / MariaDB khởi động chậm hơn Postgres
  run p9-breakeven  bash -c 'cd reallab && go run . -work breakeven -repeat 9'
  run p9-commit     bash -c 'reallab/q.sh pg < blog/lab/01-commit-pg.sql && blog/lab/01-group-commit.sh'
fi

echo
echo "==> xong: $OUT"
echo "    So với số WSL2: docs/linux-phase4-9.md (bảng tỉ số từng phase)."
