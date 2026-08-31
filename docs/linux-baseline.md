# Trả nợ Phase 0 trên máy Linux thuần

Bản đo đầu tiên chạy trên WSL2 (ext4 trên đĩa ảo trên NTFS). Nó đủ để rút ra **tỉ số**,
nhưng để lại 5 món nợ. File này là **hướng dẫn trả từng món**, mỗi món một lệnh cụ thể.

Trả dần cũng được — thứ tự khuyến nghị: **#3 → #5 → #2 → #4 → #1** (rẻ trước, đắt sau).

## Chuẩn bị (5 phút)

```bash
git clone <repo> && cd db
go version                       # cần 1.26+
sudo apt install -y jq dmsetup   # jq để so JSON, dmsetup cho nợ #1
```

Quan trọng: **đo trên đúng ổ mà sau này minidb sẽ chạy**, và **đo lúc máy rảnh**.
Trỏ thư mục đo bằng biến `DIR`:

```bash
DIR=/mnt/nvme/iolab ./scripts/linux-baseline.sh
```

---

## Nợ #3 — p50/p99 thay vì chỉ trung bình  ⏱ 5 phút

**Vì sao:** với commit latency, **p99 mới là con số người dùng cảm nhận**. Trung bình 1.4ms
mà p99 40ms là một hệ thống hoàn toàn khác.

```bash
go run ./cmd/iolab -filemb 512 -repeat 5
```

Nhìn cột `p99`. Hai thứ cần rút ra:

- **`p99 / p50` của `write + fsync`** — độ nhiễu của ổ. > 5x nghĩa là số trung bình không đáng tin.
- **cột `p99` theo cỡ nhóm group commit** — nếu p99 **không** phình theo cỡ nhóm thì gộp
  commit gần như miễn phí, cứ gộp mạnh tay ở phase 5. Nếu p99 phình tuyến tính thì phải
  giới hạn cỡ nhóm.

Ghi vào diary: bảng có cột p50/p99, kèm tỉ số `p99/p50`.

## Nợ #5 — chứng minh `fadvise(DONTNEED)` thật sự có tác dụng  ⏱ 2 phút

**Vì sao:** kernel có quyền **im lặng bỏ qua** `DONTNEED` (page còn dirty, còn được tham chiếu).
Nếu nó bỏ qua thì số "cache lạnh" là giả.

```bash
go run ./cmd/iolab -filemb 512 -verify-cache
```

Cờ này dùng `mincore(2)` để đếm **% số page của file đang nằm trong page cache**, in ra
trước/sau khi drop:

```
[cache] residency 100.0% -> 0.0%  (OK — cache đã bị đẩy ra)
```

Nếu số sau > 5% → công cụ tự cảnh báo, và mọi số "cache lạnh" của lần chạy đó phải vứt.

## Nợ #2 — `O_DIRECT`: đo I/O thật, không qua page cache  ⏱ 10 phút

**Vì sao:** mọi số đo mặc định đều đi qua page cache của kernel. DB thật (InnoDB, Oracle)
thường bật `O_DIRECT` để **tự quản buffer pool**, tránh cache hai lần. Không có số `O_DIRECT`
thì đến phase 3 bạn sẽ bench buffer pool tự viết trong khi kernel vẫn đang cache bên dưới —
so sánh không công bằng.

```bash
go run ./cmd/iolab -filemb 512 -direct -repeat 3
```

Điều cần quan sát:

- `pread ngẫu nhiên` với `O_DIRECT` = **độ trễ thật của thiết bị**. So nó với số "cache lạnh"
  ở chế độ thường: chênh lệch chính là phần readahead + cache của kernel vẫn âm thầm giúp bạn.
- Ở chế độ `O_DIRECT`, cột "CACHE NÓNG" **phải** ngang với "cache lạnh". Nếu nó vẫn nhanh
  bất thường thì `O_DIRECT` đã không có hiệu lực (một số filesystem lặng lẽ lờ đi).
- `write` không fsync với `O_DIRECT` **vẫn chưa durable** — dữ liệu có thể còn trong
  **write cache của chính ổ đĩa**. Kiểm tra ổ có bật write cache không:

```bash
cat /sys/block/nvme0n1/queue/write_cache      # "write back" = ổ đang có cache riêng
```

Nếu là `write back` thì fsync còn phải ép ổ flush cache của nó — đó là lý do fsync đắt.

## Nợ #4 — baseline sạch, so được giữa các máy  ⏱ 15 phút

```bash
DIR=/mnt/nvme/iolab REPEAT=5 ./scripts/linux-baseline.sh
```

Script làm 3 việc:

1. Chụp **môi trường** vào `env.txt`: kernel, CPU, RAM, filesystem + mount options,
   `rota` (ổ quay hay SSD), **write_cache của ổ**, I/O scheduler.
2. Chạy bộ đo buffered (`-repeat 5 -verify-cache`) và bộ `O_DIRECT`.
3. Lưu cả text lẫn JSON vào `bench/baseline/<host>-<ngày>/`.

So hai máy:

```bash
diff <(jq -r '.results[]|"\(.name) \(.p50_ns)"' bench/baseline/wsl-.../buffered.json) \
     <(jq -r '.results[]|"\(.name) \(.p50_ns)"' bench/baseline/linux-.../buffered.json)
```

Điều cần rút ra: **tỉ số nào giữ nguyên qua hai máy thì đó là quy luật vật lý** (đáng tin để
thiết kế). Tỉ số nào đổi thì đó là đặc tính của máy, không phải của database.

## Nợ #1 — torn write thật  ⏱ 30 phút, cần root

**Vì sao:** cả quyết định "page 4KB + checksum" đang dựa trên lý thuyết. Đây là món duy nhất
cần đụng tới tầng thiết bị.

**Điều phải hiểu trước:** `kill -9` **không bao giờ** tạo ra torn write. Tiến trình chết nhưng
page cache vẫn thuộc kernel và kernel vẫn writeback đầy đủ. Muốn mô phỏng mất điện phải cắt ở
tầng thấp hơn tiến trình — đó là việc của `dm-flakey`.

```bash
sudo ./scripts/dm-flakey.sh up          # loop device + dm-flakey + ext4, mount /mnt/flakey

go run ./cmd/tornlab -mode write -file /mnt/flakey/torn.dat -pagesize 16384 -pages 4096 &
sleep 5
sudo ./scripts/dm-flakey.sh drop        # từ giây này write bị NUỐT im lặng = mất điện
kill %1

sudo ./scripts/dm-flakey.sh heal        # thiết bị lành, fsck, remount
go run ./cmd/tornlab -mode verify -file /mnt/flakey/torn.dat -pagesize 16384

sudo ./scripts/dm-flakey.sh down        # dọn
```

`tornlab` ghi mỗi page **toàn bộ bằng một "thế hệ"** (mọi byte thân page = `byte(gen)`, kèm CRC),
nên một page hợp lệ luôn đồng nhất. Nếu verify tìm thấy page pha trộn **hai thế hệ** → đó chính
là torn write, bằng chứng thực nghiệm rằng page lớn hơn sector **không** atomic.

Lặp lại với `-pagesize 4096`, `16384`, `65536`. Câu hỏi cần trả lời:
**kích thước page nào bắt đầu xuất hiện torn write trên chính ổ này?**

Ba kết cục đều là dữ liệu đáng ghi:

| Kết quả verify | Nghĩa là |
|---|---|
| có `torn` | đã tái tạo được. Ghi rõ pagesize nào bắt đầu hỏng. |
| chỉ có `crc` sai | không pha trộn thế hệ nhưng dữ liệu vẫn hỏng — checksum vẫn là thứ bắt được |
| toàn `ok` | **chưa** chứng minh được là không thể xảy ra, chỉ là cửa sổ lỗi chưa trúng. Thử page lớn hơn, bỏ `-sync` |

Cũng để ý `fsck` trong bước `heal`: nếu **chính filesystem** hỏng sau khi nuốt write, đó là lời
nhắc rằng DB không được phó thác durability cho filesystem.

---

## Sau khi đo xong

1. Dán `env.txt` vào mục **Môi trường** của `diary/phase0.md` (thay cho phần WSL2, hoặc thêm
   một mục "đo lại trên Linux thuần" — giữ cả hai để so).
2. Cập nhật bảng **tỉ số**, ghi rõ lệnh + ngày + commit + máy.
3. Mỗi món nợ trả xong thì **tick checkbox** ở mục "Nợ kỹ thuật", và ghi một dòng vào bảng
   **giả thuyết sai** nếu số thật khác dự đoán — đó mới là phần đáng giá.
4. Quy tắc ghi đầy đủ: [`skills/diary/SKILL.md`](../skills/diary/SKILL.md).
