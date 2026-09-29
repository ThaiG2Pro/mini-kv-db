-- Bài 5: một commit sinh ra bao nhiêu byte WAL? Và vì sao lần sửa ĐẦU TIÊN sau checkpoint đắt hơn hẳn?
DROP TABLE IF EXISTS w5;
CREATE TABLE w5 (id int PRIMARY KEY, v int, pad text);
INSERT INTO w5 SELECT g, 0, repeat('x', 100) FROM generate_series(1, 1000) g;
CHECKPOINT;
CREATE TEMP TABLE m (step text, lsn pg_lsn);
INSERT INTO m VALUES ('start', pg_current_wal_lsn());
UPDATE w5 SET v = 1 WHERE id = 1;   INSERT INTO m VALUES ('1. UPDATE hàng 1 (lần chạm ĐẦU TIÊN vào page đó sau checkpoint)', pg_current_wal_lsn());
UPDATE w5 SET v = 1 WHERE id = 2;   INSERT INTO m VALUES ('2. UPDATE hàng 2 (cùng page, lần chạm thứ hai)', pg_current_wal_lsn());
UPDATE w5 SET v = 1 WHERE id = 3;   INSERT INTO m VALUES ('3. UPDATE hàng 3 (cùng page)', pg_current_wal_lsn());
UPDATE w5 SET v = 1 WHERE id = 900; INSERT INTO m VALUES ('4. UPDATE hàng 900 (page KHÁC, lần chạm đầu tiên)', pg_current_wal_lsn());
UPDATE w5 SET v = 2 WHERE id = 900; INSERT INTO m VALUES ('5. UPDATE hàng 900 lần nữa', pg_current_wal_lsn());
-- Mỗi dòng m cũng tự sinh một ít WAL (bảng tạm không ghi WAL, nhưng commit thì có), nên số dưới đây gồm cả phần commit.
SELECT step, lsn - lag(lsn) OVER (ORDER BY lsn) AS wal_bytes FROM m ORDER BY lsn;
SHOW full_page_writes;
