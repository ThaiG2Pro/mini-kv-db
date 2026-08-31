# Phase 5 — WAL + recovery (ARIES-lite)

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

Write-ahead log, pageLSN, checkpoint, và recovery 3 pha: Analysis -> Redo -> Undo.

## Câu hỏi phải trả lời được khi xong

- Phát biểu chính xác WAL rule. Nó được thực thi ở dòng code nào?
- Vì sao pha Redo phải *repeat history*, redo cả txn sẽ bị abort?
- CLR giải quyết vấn đề gì? (gợi ý: crash trong lúc đang undo)
- Fuzzy checkpoint khác consistent checkpoint ra sao, vì sao chọn fuzzy?
- Group commit làm throughput tăng bằng cách nào? (số đo đã có từ phase 0)

## Deliverable (bằng chứng đã hiểu)

Crash test: chạy workload rồi kill -9 ngẫu nhiên, mở lại, kiểm tra durability. Lặp 200 lần, không được sai lần nào.

## Reproduce toàn bộ phase này

```bash
go run ./cmd/crashtest -iters 200 -seed 1    # tự kill -9 con của nó ở thời điểm ngẫu nhiên
go test ./internal/wal -run TestRedoUndo -v -count=1
go test ./internal/wal -run '^$' -bench BenchmarkGroupCommit -benchmem
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
