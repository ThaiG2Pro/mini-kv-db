-- làm nóng hot: đọc 2 lần cách nhau > 1 giây để page lên vùng "young"
SELECT sum(length(pad)) FROM hot; SELECT SLEEP(1.5); SELECT sum(length(pad)) FROM hot;
SELECT 'trước khi quét big' AS luc, SUM(IS_OLD='NO') AS hot_young, SUM(IS_OLD='YES') AS hot_old FROM information_schema.INNODB_BUFFER_PAGE_LRU WHERE TABLE_NAME = '`lab`.`hot`';
SELECT sum(length(pad)) FROM big;
SELECT 'sau khi quét big (590MB)' AS luc, SUM(IS_OLD='NO') AS hot_young, SUM(IS_OLD='YES') AS hot_old FROM information_schema.INNODB_BUFFER_PAGE_LRU WHERE TABLE_NAME = '`lab`.`hot`';
