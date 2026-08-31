# Sổ nợ kỹ thuật

Một món nợ = **một thứ tôi biết là còn thiếu**, kèm **lệnh để trả nó**. Không có lệnh thì
đó chỉ là lo lắng, không phải nợ.

Ba loại, và cách xử lý khác hẳn nhau:

| Loại | Nghĩa | Cách trả |
|---|---|---|
| 🔧 **code** | Sửa được ngay trên máy này, chỉ cần viết test trước | Viết test cho nó **fail**, rồi sửa cho **pass** |
| 📏 **đo** | Cần máy Linux thuần / phần cứng thật mới có số đáng tin | Công cụ dựng sẵn, chạy lệnh, dán output vào diary |
| ⏳ **phase sau** | Chưa đủ ngữ cảnh để quyết định | Ghi lại, đừng đoán non |

---

## Đang nợ

### 📏 P0-1 · Torn write thật

`kill -9` **không** tạo được torn write (kernel vẫn writeback đầy đủ). Cần bơm lỗi ở tầng thiết bị.

```bash
sudo ./scripts/dm-flakey.sh up          # tạo /mnt/flakey
go run ./cmd/tornlab -mode write -dir /mnt/flakey -pagesize 4096 -seconds 30 &
sudo ./scripts/dm-flakey.sh drop        # thiết bị ACK write rồi vứt đi — lừa được cả fsync
sudo ./scripts/dm-flakey.sh heal
go run ./cmd/tornlab -mode verify -dir /mnt/flakey
```

**Câu hỏi:** ở pagesize nào thì torn write bắt đầu xuất hiện trên ổ đó? Thử 4096 / 16384 / 65536.
Chi tiết + bảng 3 kết cục: [`linux-baseline.md`](./linux-baseline.md).

### 📏 P0-2 · `O_DIRECT`

```bash
go run ./cmd/iolab -filemb 512 -direct -repeat 3
cat /sys/block/<dev>/queue/write_cache
```

**Cần nhìn:** cột "nóng" phải **bằng** cột "lạnh". Nếu không bằng thì filesystem đã lặng lẽ lờ
`O_DIRECT` đi và mọi số đều vô nghĩa.

### 📏 P0-4 · Baseline so được giữa các máy

```bash
DIR=/mnt/nvme/iolab REPEAT=5 ./scripts/linux-baseline.sh
```

**Câu hỏi quyết định:** *tỉ số nào sống sót qua hai máy?* Cái sống sót là quy luật vật lý và đáng
để thiết kế dựa vào; cái đổi chỉ là đặc tính của một cái máy.

### 🔧 P1-1 · Page mồ côi sau rollback

Txn bị rollback đã nới file nhưng page đó không thuộc meta nào, cũng không nằm trong freelist.
Giờ **phát hiện được bằng lệnh**:

```console
$ go run ./cmd/dbcheck data/test.db
  ⚠ RÒ RỈ ĐUÔI FILE: 1 page nằm ngoài pageCount=13 — page mồ côi sau rollback
```

**Cách trả:** lúc `Open`, so `pageCount` trong meta với kích thước file thật; phần dư thì hoặc
đưa vào freelist, hoặc `Truncate` xuống. Trả xong thì `dbcheck` phải im lặng.
**Cẩn thận:** không được truncate khi còn reader đang mở file (liên quan P1-2).

### ⏳ P1-2 · Chưa có transaction thật

`Commit(root)` là API cấp thấp: chưa có `Begin()`, chưa chặn hai writer đồng thời, chưa có
`Rollback()` chủ động. Quy ước "một writer" hiện chỉ tồn tại trong đầu tôi — **không có gì trong
code bắt buộc nó**. Để phase 6.

### ⏳ P1-3 · Chính sách cấp phát (đã đo, giả thuyết ban đầu SAI)

Xem bảng đo trong [`../diary/phase1.md`](../diary/phase1.md): LIFO và lowest-first cho ra
**cùng một** `page-gap` = 1.875, tức cả hai đều ghi tuần tự. Lý do: freelist luôn được sort trước
khi ghi xuống đĩa, nên nó không bao giờ "lởm chởm".

```bash
go test ./internal/pager -bench BenchmarkAlloc -benchtime=2000x -run XXX
```

Nguồn ghi ngẫu nhiên thật sẽ là **phân mảnh do B+Tree** (phase 4), không phải chính sách cấp phát.
Đo lại ở đó, trên đĩa thật, cùng lúc với P0-4.

### 📏 P1-6 · Chạy lại bench của phase 1 trên Linux thuần

```bash
go test ./internal/pager -bench . -benchtime=200x -run XXX -count=5
```

**Câu hỏi:** tỉ số 481x (`Commit`/`CommitNoSync`) và 43x (gộp 64 page) có sống sót không?

---

## Đã trả

| Món | Trả bằng | Bằng chứng |
|---|---|---|
| 📏 P0-3 · p50/p99 | `iolab` đo từng op + cờ `-repeat N` | Phát hiện: group commit 256 làm throughput ×140 mà p99 **không** tăng |
| 📏 P0-5 · `fadvise` có thật sự đẩy cache ra? | cờ `-verify-cache` dùng `mincore(2)` | `residency 100.0% -> 0.0%` |
| 🔧 P1-4 · `WriteAt` trả `n < len(p)` mà `err == nil` | `writeFull()` trong `pager.go` | `TestShortWriteIsAnError` — trước khi sửa: *"Commit báo THÀNH CÔNG dù lời ghi chỉ đi được 4095/4096 byte"* |
| 🔧 P1-5 · Không có cách kiểm tra file từ bên ngoài | `pager.Verify()` + `cmd/dbcheck` | Bắt được: double free, freelist tự trỏ vào chính nó, meta page bị liệt kê là rỗng, chuỗi có vòng lặp, file cắt giữa page, rò rỉ đuôi file |

---

## Quy tắc

1. **Nợ 🔧 phải được viết thành test fail trước khi sửa.** Không có test đỏ thì không biết mình
   đã sửa cái gì. P1-4 là ví dụ: test in ra đúng câu *"Commit báo THÀNH CÔNG dù..."* — đó mới là
   bằng chứng, không phải lời hứa.
2. **Nợ 📏 không được đoán.** Viết tỉ số kỳ vọng ra trước, đo, rồi so. Lệch xa thì **nghi bench
   sai trước**, đừng nghi máy lạ.
3. **Trả xong thì chuyển xuống bảng "Đã trả" kèm bằng chứng**, và tick checkbox trong diary của
   phase tương ứng.
