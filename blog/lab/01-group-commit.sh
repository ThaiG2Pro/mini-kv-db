#!/usr/bin/env bash
# Bài 1: group commit — nhiều client cùng commit thì chia nhau bao nhiêu lần fsync?
# Cần bảng c1 từ 01-commit-pg.sql. Chạy: blog/lab/01-group-commit.sh
docker exec rl-pg bash -c '
echo "INSERT INTO c1 VALUES (1, '"'"'x'"'"');" > /tmp/ins.sql
q(){ psql -U postgres -d lab -Atqc "$1"; }
for c in 1 4 16 64; do
  a=$(q "select wal_sync from pg_stat_wal")
  n=$(pgbench -U postgres -n -f /tmp/ins.sql -c $c -j 4 -T 5 lab 2>/dev/null | grep "actually processed" | grep -o "[0-9]*$")
  sleep 2   # pg_stat_wal được cập nhật trễ
  b=$(q "select wal_sync from pg_stat_wal")
  echo "clients=$c commits=$n wal_fsyncs=$((b-a)) tps=$((n/5))"
done'
