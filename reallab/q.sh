#!/usr/bin/env bash
# q.sh <pg|mysql|maria> [file.sql]  — chạy SQL (stdin hoặc file) trên một DB trong lab.
set -euo pipefail
db=$1; shift
in=${1:-/dev/stdin}
case $db in
  pg)    docker exec -i rl-pg psql -U postgres -d lab -X -q -v ON_ERROR_STOP=1 -P pager=off < "$in" ;;
  mysql) docker exec -i rl-mysql mysql -uroot -plab lab -t 2>/dev/null < "$in" ;;
  maria) docker exec -i rl-maria mariadb -uroot -plab lab -t < "$in" ;;
  *) echo "db phải là pg|mysql|maria" >&2; exit 2 ;;
esac
