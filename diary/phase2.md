# Phase 2 — Slotted page

- **Thời lượng dự kiến:** 1-2 ngày
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

Chứa record biến độ dài trong một page: slot array mọc từ trái, cell data mọc từ phải, có compact().

## Câu hỏi phải trả lời được khi xong

- Vì sao cần slot indirection thay vì trỏ thẳng vào offset?
- Sau compact() thì tuple id có đổi không? Vì sao điều đó quan trọng với secondary index?
- Fragmentation phát sinh từ đâu, khi nào thì đáng compact?
- Liên hệ: vì sao Postgres cần VACUUM?

## Deliverable (bằng chứng đã hiểu)

Fuzz test insert/delete/compact: slot array và vùng cell không bao giờ chồng lấn; mọi tuple id còn sống đọc lại đúng nội dung.

## Reproduce toàn bộ phase này

```bash
go test ./internal/page -run TestSlottedInvariant -v -count=1
go test ./internal/page -fuzz FuzzSlottedPage -fuzztime 60s
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
