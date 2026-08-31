# Phase 4 — B+Tree (linh hồn của DB)

- **Thời lượng dự kiến:** 4-6 ngày
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

Access method chính: search, insert + split, delete + merge/redistribute, và cursor cho range scan.

## Câu hỏi phải trả lời được khi xong

- Fanout trên page 4KB của tôi là bao nhiêu? Cây 3 tầng chứa bao nhiêu key? Lookup tốn mấy I/O?
- Vì sao B+Tree (data chỉ ở leaf) chứ không phải B-Tree?
- Cây tăng chiều cao lúc nào, vì sao nó luôn cân bằng?
- Delete: khi nào redistribute, khi nào merge? Vì sao merge khó hơn split?
- Right-most insert optimization là gì? Vì sao UUIDv4 làm primary key là thảm họa?

## Deliverable (bằng chứng đã hiểu)

1) Property test: sau N thao tác ngẫu nhiên -> mọi leaf cùng độ sâu, key sorted, mọi node (trừ root) đầy >= 50%.
2) Bench: insert 1M key sequential vs random, đếm số page split.

## Reproduce toàn bộ phase này

```bash
go test ./internal/btree -run TestTreeInvariants -v -count=1
go test ./internal/btree -fuzz FuzzInsertDelete -fuzztime 120s
go run ./cmd/treelab -n 1000000 -mode seq
go run ./cmd/treelab -n 1000000 -mode random    # so số split và thời gian
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
