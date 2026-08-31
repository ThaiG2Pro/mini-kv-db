# Phase 8 — SQL front-end (tùy chọn)

- **Thời lượng dự kiến:** 2-3 ngày
- **Bắt đầu:** _(YYYY-MM-DD)_ · **Kết thúc:** _(YYYY-MM-DD)_
- **Trạng thái:** ⬜ chưa bắt đầu / 🟡 đang làm / ✅ xong
- **Commit:** `_(git rev-parse --short HEAD tại lúc chốt phase)_`

> **Quy tắc ghi nhật ký:** mọi con số, mọi kết luận đều phải kèm **lệnh shell sinh ra nó**
> và **output thật** (dán nguyên, không tóm tắt). Sáu tháng sau đọc lại phải chạy lại được.
> Ghi trong lúc làm, không phải sau khi xong.

## Môi trường

```console
$ uname -srmo && go version && df -hT . | tail -1
(dán output — máy khác thì mọi con số bên dưới đều khác)
```

## Mục tiêu phase

Lexer -> parser -> AST -> planner -> executor. CREATE TABLE / INSERT / SELECT ... WHERE / ORDER BY / JOIN đơn giản.

## Câu hỏi phải trả lời được khi xong

- Logical plan khác physical plan ở đâu?
- Nested loop join vs hash join: khi nào cái nào thắng?
- Hash join cần memory budget để làm gì, spill to disk hoạt động ra sao?
- ORDER BY khi dữ liệu không vừa RAM: external merge sort làm thế nào?
- Predicate pushdown giúp được gì trong engine của tôi?

## Deliverable (bằng chứng đã hiểu)

Chạy được SELECT ... JOIN ... WHERE ... ORDER BY end-to-end, và in ra EXPLAIN plan của chính mình.

## Reproduce toàn bộ phase này

```bash
go test ./internal/sql -run TestParse -v -count=1
echo 'EXPLAIN SELECT a.x FROM a JOIN b ON a.id=b.id WHERE a.x>10 ORDER BY a.x;' | go run ./cmd/minidb -db data/test.db
```

---

## Nhật ký

### YYYY-MM-DD — <việc chính của buổi>

**Làm gì:**

```console
$ <lệnh đã chạy>
<output thật, dán nguyên>
```

**Đọc kết quả:** <con số này nói lên điều gì, có khớp với dự đoán không>

**Đang nghĩ gì:** <nghi vấn, hướng tiếp theo>

---

## Giả thuyết sai / bug đã gặp

| Tôi tưởng là | Thực tế là | Lệnh / output đã lật tẩy nó | Đã sửa thế nào |
|---|---|---|---|
|  |  |  |  |

## Số đo

Mỗi bảng số phải ghi rõ: **lệnh**, **ngày**, **commit**, **máy**. Không có ba thứ đó thì số vô nghĩa.

```console
$ <lệnh bench>
<output nguyên văn>
```

Tỉ số cần nhớ (tỉ số bền hơn số tuyệt đối — số tuyệt đối đổi theo máy và theo lần chạy):

| Tỉ số | Giá trị | Ý nghĩa |
|---|---|---|
|  |  |  |

## Invariant tôi đã cài và lệnh kiểm chứng nó

| Invariant | Cài ở đâu (file:hàm) | Lệnh kiểm chứng | Kết quả |
|---|---|---|---|
|  |  |  |  |

## Đọc gì

-

## Rút ra (viết như thể giải thích cho người khác)

-

## Nợ kỹ thuật / để dành cho sau

- [ ]
