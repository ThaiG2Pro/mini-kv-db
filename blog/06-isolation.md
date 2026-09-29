# Bài 6 — Isolation level không phải là định nghĩa

> Series [Mở nắp database](README.md) · bài 6/11 · cần đọc trước: [bài 2](02-page.md)

Bạn viết một hàm trừ tiền: đọc số dư, kiểm tra đủ tiền, ghi số dư mới. Bạn biết phải bọc nó trong
transaction. Bạn đọc tài liệu và thấy mặc định là `REPEATABLE READ`, nghe có vẻ an toàn. Test
trên Postgres chạy đúng. Deploy lên MySQL, và vài tuần sau kế toán hỏi vì sao tổng tiền lệch.

Bài này đặt cùng 5 kịch bản kinh điển lên 4 mức isolation của 3 database, và cho thấy **cùng một
cái tên mức isolation có thể mang ba nghĩa khác nhau**.

## Năm kịch bản

Hai transaction A và B chạy xen kẽ nhau trên bảng `acc(id, bal)`:

| Anomaly | Kịch bản | "Lọt" nghĩa là |
|---|---|---|
| **dirty read** | A sửa `bal = 999` nhưng chưa commit; B đọc | B thấy 999, trong khi A rồi sẽ rollback |
| **non-repeatable read** | B đọc `bal`; A sửa và commit; B đọc lại | hai lần đọc của B ra hai số khác nhau |
| **phantom** | B đếm các hàng trong một khoảng; A chèn thêm một hàng vào khoảng đó rồi commit; B đếm lại | hai lần đếm khác nhau |
| **lost update** | A đọc 100, B đọc 100; A ghi 90, B ghi 90; cả hai commit | hai lần trừ 10 mà chỉ mất 10 |
| **write skew** | hai tài khoản mỗi cái 50, luật là tổng ≥ 0. A và B cùng đọc tổng (100), mỗi bên rút 100 từ tài khoản **của mình** | tổng thành −100 |

## Thí nghiệm

Bộ đo mở hai kết nối thật và cho chúng chạy xen kẽ đúng như bảng trên, giống hệt khi bạn mở hai
cửa sổ `psql` và gõ lần lượt:

```bash
cd reallab && go run . -work anomaly
```

Mỗi ô của bảng không chỉ nói anomaly **có** lọt hay không, mà còn nói DB chặn nó **bằng cách nào**:

- `X`: anomaly **xảy ra**
- `.`: không xảy ra, và không ai phải chờ ai (snapshot tự che)
- `.w`: không xảy ra vì một bên phải **chờ** khoá
- `.a`: không xảy ra vì DB **huỷ** một transaction

```text
-- pg
anomaly               read-uncomm     read-comm       repeat-read     serializable
dirty-read            .               .               .               .
non-repeatable-read   X               X               .               .
phantom               X               X               .               .
lost-update           X               X               .a              .a
write-skew            X               X               X               .a

-- mysql
anomaly               read-uncomm     read-comm       repeat-read     serializable
dirty-read            X               .               .               .w
non-repeatable-read   X               X               .               .w
phantom               X               X               .               .w
lost-update           X               X               X               .a
write-skew            X               X               X               .a

-- maria (innodb_snapshot_isolation = 1)
anomaly               read-uncomm     read-comm       repeat-read     serializable
dirty-read            X               .               .               .w
non-repeatable-read   X               X               .               .w
phantom               X               X               .               .w
lost-update           X               X               .a              .a
write-skew            X               X               X               .a
```

Hàng read-committed giống hệt nhau ở cả ba DB. Mọi khác biệt dồn vào ba chỗ.

### 1. Lost update ở `REPEATABLE READ`: ba DB, ba kết quả

Nhìn cột `repeat-read`, hàng `lost-update`:

- **Postgres chặn** (`.a`), và B nhận lỗi:
  `ERROR: could not serialize access due to concurrent update (SQLSTATE 40001)`
- **MySQL 8.4 để lọt** (`X`). Cả hai commit thành công, số dư cuối là 90 thay vì 80, và **không
  ai nhận được lỗi gì**.
- **MariaDB 11.8 chặn** (`.a`), với lỗi
  `Error 1020 (HY000): Record has changed since last read in table 'acc'`.
  Đặt `innodb_snapshot_isolation = 0` (mặc định trước bản 11.6) thì ô này quay về `X`, giống hệt
  MySQL.

Đây chính là câu chuyện mở đầu bài: code trừ tiền kiểu *đọc rồi ghi* chạy đúng trên Postgres, và
âm thầm mất tiền trên MySQL, **ở cùng một mức isolation cùng tên**.

Tự dựng lại trên MySQL bằng hai cửa sổ:

```sql
-- chuẩn bị
CREATE TABLE acc (id int PRIMARY KEY, bal int); INSERT INTO acc VALUES (1, 100);

-- cửa sổ A                                  -- cửa sổ B
START TRANSACTION;                           START TRANSACTION;
SELECT bal FROM acc WHERE id = 1;  -- 100
                                             SELECT bal FROM acc WHERE id = 1;  -- 100
UPDATE acc SET bal = 90 WHERE id = 1;
                                             UPDATE acc SET bal = 90 WHERE id = 1;  -- chờ A...
COMMIT;
                                             -- ...rồi chạy tiếp, KHÔNG báo lỗi
                                             COMMIT;
SELECT bal FROM acc WHERE id = 1;  -- 90. Hai lần trừ 10, chỉ mất 10.
```

Làm y hệt trên Postgres, câu `UPDATE` của B sẽ báo lỗi ngay khi A commit.

### 2. Postgres không có dirty read, kể cả khi bạn xin

Ô `dirty-read × read-uncomm` của Postgres là `.`. Postgres chấp nhận cú pháp
`READ UNCOMMITTED` nhưng lặng lẽ nâng nó lên thành `READ COMMITTED`. InnoDB thì đọc được dữ liệu
chưa commit thật.

### 3. Serializable: một bên chờ, một bên huỷ

Nhìn cột `serializable`:

- **Postgres chỉ có `.` và `.a`, không bao giờ `.w`.** Không ai phải chờ ai. Postgres để mọi
  transaction chạy tự do, theo dõi xem ai đã đọc thứ mà người khác vừa ghi, và khi thấy một vòng
  phụ thuộc nguy hiểm thì huỷ một bên:
  `could not serialize access due to read/write dependencies among transactions`.
  Cách này tên là **SSI** (Serializable Snapshot Isolation), một cách *lạc quan*.
- **InnoDB có `.w` ở ba hàng đầu.** Ở mức serializable, mọi `SELECT` thường của InnoDB đều lấy khoá
  đọc, và người muốn ghi phải chờ tới khi người đọc commit. Hai bên cùng đọc rồi cùng muốn ghi thì
  chờ nhau vòng tròn, và DB giết một bên: `Error 1213 (40001): Deadlock found`. Cách này là **khoá
  hai pha** (2PL), một cách *bi quan*.

## Bên trong: cùng một cái tên, hai cơ chế

Có hai cách để hai transaction không giẫm lên nhau:

- **Khoá** (bi quan): ai muốn đọc hay ghi thì phải giữ khoá, người khác phải chờ.
- **Phiên bản** (MVCC, lạc quan): mỗi transaction đọc một **bản chụp** (snapshot) của dữ liệu tại
  một thời điểm. Người đọc không chặn người ghi, vì người ghi tạo phiên bản mới chứ không sửa bản
  người đọc đang xem. Bài 2 đã thấy điều này trong page: `UPDATE` ở Postgres ghi một hàng mới và
  giữ nguyên hàng cũ.

Snapshot tự chặn được ba hàng đầu của bảng: bạn đọc bản chụp thì không thể thấy thứ chưa commit
(dirty read), và đọc lại vẫn là bản chụp đó (non-repeatable read, phantom). Lost update thì khác:
nó xảy ra ở lúc **ghi**. A và B cùng đọc bản chụp có `bal = 100`, cùng tính ra 90. Muốn chặn thì
lúc B ghi, DB phải hỏi: *"hàng này có bị ai sửa **sau** lúc tôi chụp không?"* Có thì B thua. Luật
này tên là **first-committer-wins**, và trong minidb nó là đúng một câu `if`
(`internal/txn/txn.go`):

```go
// first-committer-wins. Chỉ từ RepeatableRead trở lên:
//
//   - ReadCommitted CỐ Ý không kiểm, vì đó chính là chỗ sinh ra
//     lost update.
//   - Serializable không cần kiểm: lock X đã giữ từ lúc Put nên
//     không ai chen vào được.
if t.iso == RepeatableRead {
	if nv, ok := c.Newest(); ok && !snap.Visible(nv.Xmin) {
		return fmt.Errorf("%w: khóa %q đã bị txn %d ghi sau snapshot %d",
			ErrConflict, key, nv.Xmin, snap.Xmax)
	}
}
```

*Phiên bản mới nhất của khoá này được ghi bởi một transaction mà snapshot của tôi không thấy?*
Vậy là có người đã ghi sau khi tôi chụp, và tôi phải thua.

Postgres và minidb có câu `if` này. MySQL không có. Ở `REPEATABLE READ`, `SELECT` của InnoDB đọc
bản chụp, nhưng `UPDATE` đọc **bản mới nhất** (InnoDB gọi là *current read*) rồi ghi đè lên. Nên
B ghi đè kết quả của A mà không ai phát hiện ra. MariaDB 11.6 thêm đúng câu `if` đó, dưới tên
`innodb_snapshot_isolation`.

Còn **write skew** thì không câu `if` nào kiểu này chặn được: A ghi tài khoản 1, B ghi tài khoản 2,
**không ai ghi cùng một hàng với ai**. Muốn chặn thì phải biết về những gì hai bên đã **đọc**. Đó
là việc của khoá đọc (InnoDB, minidb) hoặc của SSI (Postgres). Vì vậy cột `repeat-read` của cả ba
DB đều là `X` ở hàng cuối.

minidb chạy cùng bảng này ở phase 6 và ra đúng như Postgres ở cột `repeat-read`, đúng như InnoDB ở
cột `serializable` (cũng dùng khoá). Ba database, ba tổ hợp khác nhau của cùng hai cơ chế.

## Mang về dùng

1. **Đừng dựa vào mức isolation cho mẫu *đọc → tính → ghi*.** Viết sao cho đúng ở mọi DB:
   - Để DB tự tính: `UPDATE acc SET bal = bal - 10 WHERE id = 1 AND bal >= 10`, rồi kiểm tra
     số hàng bị ảnh hưởng.
   - Hoặc khoá hàng khi đọc: `SELECT bal FROM acc WHERE id = 1 FOR UPDATE`.
   - Hoặc dùng *optimistic locking*: thêm cột `version`, rồi
     `UPDATE … SET bal = ?, version = version + 1 WHERE id = ? AND version = ?`.
2. **Code phải biết retry.** Ở mức isolation cao, bị DB huỷ là **hành vi bình thường**, không phải
   lỗi. Và bắt đúng mã lỗi của từng DB:
   - Postgres: SQLSTATE `40001` (serialization failure), `40P01` (deadlock)
   - MySQL/MariaDB: `1213` (deadlock, SQLSTATE `40001`)
   - **MariaDB: `1020`**, SQLSTATE `HY000`. Code chỉ bắt `40001` sẽ **bỏ sót** lỗi này.
3. **Biết mặc định của DB mình:** Postgres là `READ COMMITTED`, MySQL/MariaDB là `REPEATABLE READ`.
   Và `REPEATABLE READ` của hai bên **không giống nhau**.
4. **Muốn chặn write skew** (ví dụ luật "luôn phải có ít nhất một bác sĩ trực"), phải dùng
   `SERIALIZABLE` hoặc khoá tường minh (`SELECT … FOR UPDATE` trên **mọi** hàng mà luật đó đọc).
   Không mức nào thấp hơn làm được.

---

Bộ đo: [`reallab/anomaly.go`](../reallab/anomaly.go) · số đo gốc và hai bug của bộ lập lịch:
[`diary/phase9.md`](../diary/phase9.md), bảng 2 · MVCC + khoá của minidb:
[`internal/txn/`](../internal/txn), [`internal/lock/`](../internal/lock) · [`diary/phase6.md`](../diary/phase6.md).

**Bài tiếp theo:** [Bài 7 — Transaction quên `COMMIT`](07-long-txn.md)
