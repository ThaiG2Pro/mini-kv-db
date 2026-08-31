# Phase 6 — Transaction & concurrency control

- **Thời lượng dự kiến:** 3-4 ngày
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

Từ 2PL nghiêm ngặt (lock manager + deadlock detection) tới MVCC snapshot isolation.

## Câu hỏi phải trả lời được khi xong

- Lock khác latch thế nào (lần này đã có code để chỉ tận nơi)?
- S2PL đảm bảo serializable bằng cơ chế gì? Cái giá là gì?
- Visibility rule của MVCC: tuple (xmin, xmax) hiển thị với snapshot nào?
- Vì sao trong MVCC reader không chặn writer?
- Write skew là gì, vì sao snapshot isolation KHÔNG chặn được, SSI sửa thế nào?
- Rác MVCC (version cũ) ai dọn? (nối lại với VACUUM ở phase 2)

## Deliverable (bằng chứng đã hiểu)

Test N goroutine chuyển tiền: invariant 'tổng số dư không đổi' giữ ở mức isolation cao VÀ bị phá ở mức thấp — tái tạo được dirty read, non-repeatable read, phantom, write skew.

## Reproduce toàn bộ phase này

```bash
go test ./internal/txn -run TestAnomalies -v -count=1     # mỗi anomaly một subtest
go test ./internal/txn -run TestBankInvariant -race -count=5
go test ./internal/txn -run TestDeadlockDetect -v -timeout 30s
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
