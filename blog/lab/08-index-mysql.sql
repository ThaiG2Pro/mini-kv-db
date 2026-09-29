-- Bài 8 (MySQL): cùng các cách viết WHERE, thêm ca so cột chuỗi với số.
DROP TABLE IF EXISTS u8, d10, d1000;
CREATE TABLE d10 (n int); INSERT INTO d10 VALUES (0),(1),(2),(3),(4),(5),(6),(7),(8),(9);
CREATE TABLE d1000 AS SELECT a.n*100+b.n*10+c.n AS n FROM d10 a, d10 b, d10 c;
CREATE TABLE u8 (id int PRIMARY KEY, email varchar(50), city varchar(10), age int, code varchar(10),
  KEY u8_email (email), KEY u8_city_age (city, age), KEY u8_code (code)) ENGINE=InnoDB;
INSERT INTO u8 SELECT g, CONCAT('user', g, '@mail.com'), CONCAT('c', g % 50), FLOOR(RAND(3)*81), LPAD(g, 8, '0')
FROM (SELECT a.n*1000+b.n+1 AS g FROM d1000 a, d1000 b WHERE a.n < 200) s;
DROP TABLE d10, d1000;
ANALYZE TABLE u8;
SELECT '1b. lower(email) = ...' AS ca; EXPLAIN SELECT * FROM u8 WHERE lower(email) = 'user42@mail.com';
SELECT '2b. chỉ cột thứ hai age = 30' AS ca; EXPLAIN SELECT * FROM u8 WHERE age = 30;
SELECT '3a. LIKE tiền tố' AS ca; EXPLAIN SELECT * FROM u8 WHERE email LIKE 'user42@%';
SELECT '3b. LIKE bắt đầu bằng %' AS ca; EXPLAIN SELECT * FROM u8 WHERE email LIKE '%42@mail.com';
SELECT '5. OR hai cột' AS ca; EXPLAIN SELECT * FROM u8 WHERE email = 'user42@mail.com' OR code = '00000077';
SELECT '7a. code = chuỗi' AS ca; EXPLAIN SELECT * FROM u8 WHERE code = '00000077';
SELECT '7b. code = SỐ (cột varchar so với số)' AS ca; EXPLAIN SELECT * FROM u8 WHERE code = 77;
SELECT '7b. kết quả' AS ca; SELECT id, code FROM u8 WHERE code = 77;
