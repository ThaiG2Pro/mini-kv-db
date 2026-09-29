-- Bài 2: soi một page của Postgres bằng pageinspect.
CREATE EXTENSION IF NOT EXISTS pageinspect;
DROP TABLE IF EXISTS p2;
CREATE TABLE p2 (id int PRIMARY KEY, name text) WITH (autovacuum_enabled = off);
INSERT INTO p2 VALUES (1, 'an'), (2, 'binh'), (3, 'chi');
\echo '== 1. header của page 0: lower = cuối mảng con trỏ, upper = đầu vùng dữ liệu'
SELECT lower, upper, special, pagesize FROM page_header(get_raw_page('p2', 0));
\echo '== 2. mảng con trỏ (line pointer): lp_off = hàng nằm ở byte nào trong page'
SELECT lp, lp_off, lp_len, t_xmin, t_xmax, t_ctid FROM heap_page_items(get_raw_page('p2', 0));
\echo '== 3. UPDATE một hàng: địa chỉ (ctid) của nó đổi'
SELECT ctid, * FROM p2 WHERE id = 2;
UPDATE p2 SET name = 'binh moi' WHERE id = 2;
SELECT ctid, * FROM p2 WHERE id = 2;
SELECT lp, lp_off, lp_len, t_xmin, t_xmax, t_ctid FROM heap_page_items(get_raw_page('p2', 0));
\echo '== 4. DELETE: hàng vẫn còn nguyên trong page, chỉ bị đánh dấu t_xmax'
DELETE FROM p2 WHERE id = 3;
SELECT lp, lp_off, lp_len, t_xmin, t_xmax, t_ctid FROM heap_page_items(get_raw_page('p2', 0));
\echo '== 5. VACUUM: dọn xác, dồn các hàng còn sống lại (compact), con trỏ vẫn giữ chỗ'
VACUUM p2;
SELECT lp, lp_off, lp_len, lp_flags, t_xmin, t_xmax, t_ctid FROM heap_page_items(get_raw_page('p2', 0));
SELECT lower, upper FROM page_header(get_raw_page('p2', 0));
