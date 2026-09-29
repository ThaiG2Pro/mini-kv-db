-- Bài 10: đọc EXPLAIN — join, sort, và toán tử CHẶN tràn ra đĩa khi thiếu work_mem.
DROP TABLE IF EXISTS ev, dim;
CREATE TABLE dim (kind int PRIMARY KEY, name text);
CREATE TABLE ev (id int PRIMARY KEY, kind int, city text, amount int);
SELECT setseed(0.5);
INSERT INTO dim SELECT g, 'kind ' || g FROM generate_series(0, 999) g;
INSERT INTO ev SELECT g, (random()*999)::int, 'city ' || (random()*9999)::int, (random()*1000)::int
FROM generate_series(1, 1000000) g;
CREATE INDEX ev_kind ON ev (kind);
VACUUM ANALYZE dim; VACUUM ANALYZE ev;
SET max_parallel_workers_per_gather = 0;
\echo '== 1. join + điều kiện trên MỘT bảng: Postgres có suy ra điều kiện cho bảng kia không?'
EXPLAIN (COSTS OFF) SELECT ev.city, dim.name FROM ev JOIN dim ON ev.kind = dim.kind WHERE dim.kind < 5;
\echo '== 2. hash join + sort với work_mem = 64kB'
SET work_mem = '64kB';
EXPLAIN (ANALYZE, COSTS OFF, TIMING OFF) SELECT ev.city, dim.name FROM ev JOIN dim ON ev.kind = dim.kind WHERE ev.amount < 300 ORDER BY ev.city;
\echo '== 3. cùng câu, work_mem = 256MB'
SET work_mem = '256MB';
EXPLAIN (ANALYZE, COSTS OFF, TIMING OFF) SELECT ev.city, dim.name FROM ev JOIN dim ON ev.kind = dim.kind WHERE ev.amount < 300 ORDER BY ev.city;
RESET work_mem;
\echo '== 4. ORDER BY ... LIMIT 10 trên cột CÓ index: không cần Sort'
EXPLAIN (ANALYZE, COSTS OFF, TIMING OFF) SELECT * FROM ev ORDER BY kind LIMIT 10;
\echo '== 5. ORDER BY ... LIMIT 10 trên cột KHÔNG có index: phải đọc hết rồi mới trả hàng đầu tiên'
EXPLAIN (ANALYZE, COSTS OFF, TIMING OFF) SELECT * FROM ev ORDER BY amount LIMIT 10;
\echo '== 6. hash join với phía build LỚN (ép hash join), work_mem = 1MB rồi 256MB'
SET enable_mergejoin = off; SET enable_nestloop = off;
SET work_mem = '1MB';
EXPLAIN (ANALYZE, COSTS OFF, TIMING OFF) SELECT count(*) FROM ev a JOIN ev b ON a.id = b.id WHERE b.amount < 500;
SET work_mem = '256MB';
EXPLAIN (ANALYZE, COSTS OFF, TIMING OFF) SELECT count(*) FROM ev a JOIN ev b ON a.id = b.id WHERE b.amount < 500;
RESET work_mem; RESET enable_mergejoin; RESET enable_nestloop;
