-- Bài 3 (MySQL): midpoint insertion. Nạp dữ liệu một lần bằng file này, rồi chạy 03-buffer-mysql-run.sql
-- hai lần: sau `SET GLOBAL innodb_old_blocks_time = 1000` (mặc định) và sau `= 0`.
DROP TABLE IF EXISTS hot, big, d10, d1000;
CREATE TABLE d10 (n int); INSERT INTO d10 VALUES (0),(1),(2),(3),(4),(5),(6),(7),(8),(9);
CREATE TABLE d1000 AS SELECT a.n*100+b.n*10+c.n AS n FROM d10 a, d10 b, d10 c;
CREATE TABLE hot (id int PRIMARY KEY, pad varchar(100)) ENGINE=InnoDB;
INSERT INTO hot SELECT a.n*1000+b.n+1, REPEAT('h',100) FROM d1000 a, d1000 b WHERE a.n < 200;
CREATE TABLE big (id int PRIMARY KEY, pad varchar(100)) ENGINE=InnoDB;
INSERT INTO big SELECT x.n*1000000+a.n*1000+b.n+1, REPEAT('b',100) FROM d1000 a, d1000 b, (SELECT n FROM d10 WHERE n < 3) x;  -- big ~600MB > buffer pool 256MB
