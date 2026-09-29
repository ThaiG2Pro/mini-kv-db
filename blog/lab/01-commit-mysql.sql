-- Bài 1 (MySQL): cùng 5000 INSERT, khác nhau ở số lần COMMIT.
DROP TABLE IF EXISTS c1;
CREATE TABLE c1 (id int, v varchar(10));
DROP PROCEDURE IF EXISTS ins;
DELIMITER //
CREATE PROCEDURE ins(n int, every int)
BEGIN
  DECLARE i int DEFAULT 1;
  SET autocommit = 0;
  WHILE i <= n DO
    INSERT INTO c1 VALUES (i, 'x');
    IF i % every = 0 THEN COMMIT; END IF;
    SET i = i + 1;
  END WHILE;
  COMMIT;
  SET autocommit = 1;
END //
DELIMITER ;
SET @t = NOW(6); CALL ins(5000, 1);    SELECT 'commit sau MỖI dòng' ca, TIMESTAMPDIFF(MICROSECOND, @t, NOW(6))/1000 ms;
SET @t = NOW(6); CALL ins(5000, 100);  SELECT 'commit mỗi 100 dòng' ca, TIMESTAMPDIFF(MICROSECOND, @t, NOW(6))/1000 ms;
SET @t = NOW(6); CALL ins(5000, 5000); SELECT 'commit MỘT lần' ca, TIMESTAMPDIFF(MICROSECOND, @t, NOW(6))/1000 ms;
SET GLOBAL innodb_flush_log_at_trx_commit = 2; SET GLOBAL sync_binlog = 0;
SET @t = NOW(6); CALL ins(5000, 1);    SELECT 'mỗi dòng, flush_log_at_trx_commit=2, sync_binlog=0' ca, TIMESTAMPDIFF(MICROSECOND, @t, NOW(6))/1000 ms;
SET GLOBAL innodb_flush_log_at_trx_commit = 1; SET GLOBAL sync_binlog = 1;
