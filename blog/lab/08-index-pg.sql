-- Bài 8: năm cách viết WHERE làm index "biến mất".
DROP TABLE IF EXISTS u8;
CREATE TABLE u8 (id int PRIMARY KEY, email text, city text, age int, code varchar(10));
SELECT setseed(0.3);
INSERT INTO u8 SELECT g, 'user' || g || '@mail.com', 'c' || (g % 50), (random()*80)::int, lpad(g::text, 8, '0')
FROM generate_series(1, 200000) g;
CREATE INDEX u8_email ON u8 (email);
CREATE INDEX u8_city_age ON u8 (city, age);
CREATE INDEX u8_code ON u8 (code);
VACUUM ANALYZE u8;
SET max_parallel_workers_per_gather = 0;
\echo '== 1a. so bằng trực tiếp'
EXPLAIN (COSTS OFF) SELECT * FROM u8 WHERE email = 'user42@mail.com';
\echo '== 1b. bọc cột trong một hàm'
EXPLAIN (COSTS OFF) SELECT * FROM u8 WHERE lower(email) = 'user42@mail.com';
\echo '== 2a. index (city, age): lọc theo cột ĐẦU'
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF, TIMING OFF) SELECT * FROM u8 WHERE city = 'c7' AND age = 30;
\echo '== 2b. index (city, age): chỉ lọc theo cột THỨ HAI'
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF, TIMING OFF) SELECT * FROM u8 WHERE age = 30;
\echo '== 3a. LIKE có tiền tố'
EXPLAIN (COSTS OFF) SELECT * FROM u8 WHERE email LIKE 'user42@%';
\echo '== 3b. LIKE bắt đầu bằng %'
EXPLAIN (COSTS OFF) SELECT * FROM u8 WHERE email LIKE '%42@mail.com';
\echo '== 4. vì sao 3a không dùng index: collation của database'
SELECT datcollate FROM pg_database WHERE datname = current_database();
CREATE INDEX u8_email_pat ON u8 (email text_pattern_ops); ANALYZE u8;
EXPLAIN (COSTS OFF) SELECT * FROM u8 WHERE email LIKE 'user42@%';
\echo '== 5. OR trên hai cột khác nhau'
EXPLAIN (COSTS OFF) SELECT * FROM u8 WHERE email = 'user42@mail.com' OR code = '00000077';
\echo '== 6. chữa 1b bằng expression index'
CREATE INDEX u8_lower_email ON u8 (lower(email)); ANALYZE u8;
EXPLAIN (COSTS OFF) SELECT * FROM u8 WHERE lower(email) = 'user42@mail.com';
