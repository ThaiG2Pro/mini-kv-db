# Phase 7 — Secondary index & query cơ bản

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

Secondary index trỏ về tuple id, composite key, iterator model, và chọn giữa index scan vs seq scan.

## Câu hỏi phải trả lời được khi xong

- Secondary index lưu gì ở leaf? Vì sao không lưu thẳng cả row?
- Covering index tiết kiệm được gì (đếm theo số I/O)?
- Left-most prefix: vì sao index (a,b) vô dụng với WHERE b = ?
- Selectivity bao nhiêu thì seq scan thắng index scan trên máy tôi? Vì sao?
- Volcano model: chi phí ẩn của nó là gì (gợi ý: vì sao có vectorized execution)?

## Deliverable (bằng chứng đã hiểu)

Bench tìm điểm hòa vốn selectivity giữa index scan và seq scan; và một cost estimator chọn đúng plan ở cả hai phía điểm hòa vốn.

## Reproduce toàn bộ phase này

```bash
go run ./cmd/scanlab -rows 1000000 -sel 0.001,0.01,0.05,0.1,0.3,1.0
go test ./internal/index -run TestLeftmostPrefix -v -count=1
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
