# Trả nợ Phase 9 trên máy Linux thuần

Phase 9 đo trên WSL2. Hầu hết kết luận ở đó là **tỉ số** hoặc **đếm sự kiện** (commit mất, page
ghi, hàng đọc), nên vẫn đứng được. Riêng một món cần phần cứng thật: **P9-7**, vì WSL2 không đưa
bộ đếm hiệu năng của CPU (PMU) vào VM, nên `perf` không đếm được cache miss.

File này là **hướng dẫn trả từng bước**, cùng khuôn với [`linux-baseline.md`](./linux-baseline.md)
của phase 0. Bối cảnh đầy đủ nằm ở `diary/phase9.md`, bảng 8.

## Chuẩn bị ⏱ 10 phút

```bash
git clone <repo> && cd db
go version                                  # cần 1.26+
docker compose version                      # cần compose v2
docker ps                                   # chạy được mà không cần sudo (user nằm trong nhóm docker)

# perf. Ubuntu:
sudo apt install -y linux-tools-$(uname -r) linux-tools-generic
# Debian: sudo apt install -y linux-perf
perf version
```

Kiểm tra máy **có PMU** hay không. Đây là lý do duy nhất phải rời WSL2, nên nếu máy mới cũng
không có thì dừng ở đây:

```bash
ls /sys/bus/event_source/devices/ | grep ^cpu
# cpu                  → CPU thường
# cpu_core cpu_atom    → CPU lai của Intel (nhân P + nhân E): mỗi sự kiện sẽ in ra hai dòng
# (không có gì)        → máy ảo không bật vPMU: đổi máy, hoặc bật vPMU trong hypervisor
```

Thử perf trên một lệnh thật. Phải thấy số đếm, không phải `<not supported>`:

```bash
sudo perf stat -e cycles,cache-misses,LLC-load-misses,dTLB-load-misses -- sleep 0.1
```

Quan trọng: **đo lúc máy rảnh**. Tắt trình duyệt và IDE, không để container nào khác chạy
(`docker ps`). Trên laptop thì cắm sạc, vì chạy pin thì CPU hạ xung nhịp.

---

## Nợ P9-7 — hash join tràn đĩa nhanh hơn trong RAM  ⏱ 10 phút, cần sudo

**Vì sao:** trên Postgres 17, cùng một câu join 500k × 1M hàng, `work_mem=1MB` (16 batch, tràn ra
file tạm) chạy **nhanh hơn** `work_mem=256MB` (1 batch, bảng băm 21.6MB nằm trọn trong RAM). Ở
minidb (phase 8), tràn đĩa đắt gấp 2.1x. Hiểu được vì sao thì mới biết minidb có nên học theo
hay không.

**Những gì đã biết từ WSL2 (bảng 8):**

| | Kết quả |
|---|---|
| Page fault (glibc trả bộ nhớ bảng băm lại cho OS sau mỗi câu) | Có thật, nhưng chỉ chiếm ~20ms |
| Khoảng chênh khi tăng probe từ 1M lên 4M hàng | Tăng tuyến tính: 67–98ns mỗi hàng probe |
| Độ trễ một lần trượt xuống RAM (microbenchmark) | 80–140ns |

Nghĩa là mỗi hàng probe trượt xuống RAM khoảng một lần. Nhưng đó là suy ra, chưa đếm trực tiếp.

**Viết dự báo trước khi chạy** (quy tắc của diary). H1 nói: bảng băm 1 batch vượt L3 hoặc vượt
tầm phủ của TLB, còn mỗi batch trong bản 16 batch thì nằm gọn trong cache.

**Chạy:**

```bash
./scripts/p97-hashjoin.sh              # REPEAT=11 mặc định
REPEAT=21 ./scripts/p97-hashjoin.sh    # nếu hai lượt chạy lệch nhau > 10%
```

Script làm 4 việc:

1. Dừng ngay nếu thiếu `perf` hoặc thiếu PMU. Hỏi mật khẩu sudo **một lần**, trước khi đo.
2. Chụp **môi trường** vào `env.txt`: `lscpu` (cỡ L2/L3), governor, transparent hugepage, RAM,
   tải máy.
3. Dựng `rl-pg` và `rl-pgm`. Hai container là cùng Postgres 17, chỉ khác `GLIBC_TUNABLES`: `rl-pgm`
   giữ bộ nhớ lại, không có page fault. Lần đầu nạp 1M + 4M hàng, mất khoảng 1 phút.
4. Chạy `reallab -work hashjoin`. Các phép A–D giống trên WSL2. **Phép E** là phần mới:
   `sudo perf stat -p <pid backend>` cho 1MB và 256MB, chia số đếm theo từng hàng probe.

Kết quả lưu ở `bench/p97/<host>-<ngày>/`: `env.txt` và `hashjoin.txt`.

**Tuỳ chọn, nếu vẫn nhiễu:** ghim hai container vào cùng một nhân. Trên CPU lai thì chọn nhân P,
là nhân có `MAXMHZ` cao nhất trong `lscpu -e`:

```bash
docker update --cpuset-cpus=2 rl-pg rl-pgm
```

### Đọc bảng E

Đọc theo thứ tự, dừng ở dòng đầu tiên khớp với kết quả:

| Thấy gì | Kết luận | Ghi vào diary |
|---|---|---|
| `LLC-load-misses` / probe: 256MB ≥ ~1, còn 1MB thấp hơn hẳn | **H1 đúng, nguyên nhân là L3.** Kiểm chéo: (số miss / probe) × độ trễ ở phép D ≈ ns/probe ở phép B | Tick P9-7 |
| LLC gần bằng nhau, nhưng `dTLB-load-misses` của 256MB cao hơn hẳn | **H1 đúng một phần, nguyên nhân là TLB chứ không phải L3.** Thử lại với THP khác (xem dưới) | Một dòng "giả thuyết sai": L3 → TLB |
| Cả hai loại miss gần bằng nhau, `instructions` / probe khác nhau | **H1 sai.** Bản 1 batch chạy nhiều lệnh hơn, ví dụ chuỗi bucket dài hơn | Một dòng "giả thuyết sai", mở nợ mới |
| Mọi số đều gần bằng nhau, chỉ IPC (instructions / cycles) khác | **H1 sai theo cách khó hơn**: rẽ nhánh đoán sai hoặc chờ bộ nhớ ở chỗ perf không đếm. Thêm `-e branch-misses` rồi chạy lại | Như trên |

Kiểm tra THP (transparent hugepage) cho dòng thứ hai. Page 2MB phủ mảng bucket 4MB bằng 2 mục
TLB thay vì 1024 mục:

```bash
cat /sys/kernel/mm/transparent_hugepage/enabled              # ghi lại giá trị ban đầu
echo always | sudo tee /sys/kernel/mm/transparent_hugepage/enabled
./scripts/p97-hashjoin.sh
echo madvise | sudo tee /sys/kernel/mm/transparent_hugepage/enabled   # trả lại giá trị ban đầu
```

TLB là thủ phạm thì khoảng chênh ở phép B phải co lại rõ khi đặt `always`.

**Chỗ có thể vấp:**

- **CPU lai** (`cpu_core` và `cpu_atom`): bảng E in mỗi sự kiện hai dòng. Đọc dòng của loại nhân
  có số đếm lớn hơn, vì backend chạy chủ yếu trên nhân đó. Muốn chỉ một dòng thì ghim nhân như trên.
- **`perf` báo `No permission`**: script đã gọi perf qua `sudo -n`. Nếu vẫn lỗi thì chạy
  `sudo -v` trước, rồi chạy lại script.
- **Phần đọc output của perf mới thử với một bản perf giả** (`reallab/hashjoin.go`, hàm
  `perfAround`). Nếu bảng E trống hoặc báo lỗi parse, xem file CSV mà perf ghi ra (in kèm trong
  thông báo lỗi) rồi sửa phần parse. Việc này cũng phải ghi vào diary.

**Dọn:**

```bash
docker compose -f reallab/docker-compose.yml --profile p97 stop pgm
docker update --cpuset-cpus="" rl-pg rl-pgm     # nếu đã ghim
```

---

## Sau khi đo xong

1. Thêm một mục mới vào `diary/phase9.md`: **"bảng 8, lượt Linux thuần"**. Dán nguyên `env.txt`
   và `hashjoin.txt`. Giữ lại số của WSL2 để so.
2. So phép B và phép D giữa hai máy. **Độ dốc ns/probe** đổi theo máy là chuyện bình thường. Điều
   phải giữ nguyên là quan hệ: ns/probe ≈ miss/probe × độ trễ một lần trượt.
3. Tick P9-7 trong mục "Nợ kỹ thuật" của diary, rồi chuyển P9-7 trong
   [`debts.md`](./debts.md) sang mục "Đã trả". Nếu kết quả khác dự báo thì thêm một dòng vào bảng
   **giả thuyết sai**.
4. Viết `blog/13` (bài đã hoãn để chờ số này), rồi link từ `blog/10-explain.md` và `blog/README.md`.
5. Quy tắc ghi đầy đủ: [`skills/diary/SKILL.md`](../skills/diary/SKILL.md).
