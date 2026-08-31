# Phase 3 — Buffer pool

- **Thời lượng dự kiến:** 2 ngày
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

Page cache trong RAM với pin/unpin, dirty flag, eviction policy và latch.

## Câu hỏi phải trả lời được khi xong

- Vì sao LRU thuần hỏng khi có sequential scan? CLOCK / LRU-K sửa nó thế nào?
- Điều gì xảy ra nếu evict một page đang pinned?
- Vì sao không được ghi dirty page ra đĩa trước khi WAL của nó fsync? (hook cho phase 5)
- Latch nào bảo vệ bảng hash, latch nào bảo vệ từng frame? Thứ tự lấy latch để không deadlock?

## Deliverable (bằng chứng đã hiểu)

Bench hit ratio zipfian: LRU vs CLOCK vs LRU-K, kèm một test chứng minh sequential scan làm sập hit-ratio của LRU.

## Reproduce toàn bộ phase này

```bash
go test ./internal/buffer -run TestPinnedNeverEvicted -v -count=1
go test ./internal/buffer -run '^$' -bench BenchmarkZipfian -benchmem
go test ./internal/buffer -run TestScanPollutesLRU -v   # scan quét sạch cache của LRU
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
