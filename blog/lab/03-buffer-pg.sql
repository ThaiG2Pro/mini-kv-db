-- Bài 3: một lần quét bảng lớn có đuổi bảng "nóng" ra khỏi shared_buffers không?
CREATE EXTENSION IF NOT EXISTS pg_buffercache;
DROP TABLE IF EXISTS hot, big;
CREATE TABLE hot AS SELECT g AS id, repeat('h', 100) AS pad FROM generate_series(1, 200000) g;   -- ~25MB
CREATE TABLE big AS SELECT g AS id, repeat('b', 100) AS pad FROM generate_series(1, 3000000) g; -- ~400MB > shared_buffers 256MB
VACUUM ANALYZE hot; VACUUM ANALYZE big;
SET max_parallel_workers_per_gather = 0;
-- làm nóng bảng hot
SELECT count(*) FROM hot; SELECT count(*) FROM hot;
\echo '== trước khi quét big: bao nhiêu page của mỗi bảng đang nằm trong shared_buffers'
SELECT c.relname, count(*) AS buffers, pg_relation_size(c.oid)/8192 AS pages_on_disk
FROM pg_buffercache b JOIN pg_class c ON b.relfilenode = pg_relation_filenode(c.oid)
WHERE c.relname IN ('hot', 'big') GROUP BY c.relname, c.oid ORDER BY 1;
\echo '== quét cả bảng big (400MB) một lần'
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF, TIMING OFF) SELECT count(*) FROM big;
\echo '== sau khi quét big'
SELECT c.relname, count(*) AS buffers, pg_relation_size(c.oid)/8192 AS pages_on_disk
FROM pg_buffercache b JOIN pg_class c ON b.relfilenode = pg_relation_filenode(c.oid)
WHERE c.relname IN ('hot', 'big') GROUP BY c.relname, c.oid ORDER BY 1;
\echo '== đọc lại bảng hot: hit hay read?'
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF, TIMING OFF) SELECT count(*) FROM hot;
