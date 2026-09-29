-- Bài 1: cùng 5000 INSERT, khác nhau ở số lần COMMIT.
-- Vòng lặp chạy TRONG server (procedure), nên không có chi phí mạng.
DROP TABLE IF EXISTS c1;
CREATE TABLE c1 (id int, v text);
CREATE OR REPLACE PROCEDURE ins(n int, every int) LANGUAGE plpgsql AS $$
BEGIN
  FOR i IN 1..n LOOP
    INSERT INTO c1 VALUES (i, 'x');
    IF i % every = 0 THEN COMMIT; END IF;
  END LOOP;
END $$;
\timing on
\echo '== commit sau MỖI dòng (5000 commit)'
CALL ins(5000, 1);
\echo '== commit mỗi 100 dòng (50 commit)'
CALL ins(5000, 100);
\echo '== commit MỘT lần (1 commit)'
CALL ins(5000, 5000);
\echo '== commit sau mỗi dòng, synchronous_commit = off (không chờ fsync)'
SET synchronous_commit = off;
CALL ins(5000, 1);
