# Một buổi trên máy Linux thuần: từ `git clone` tới ảnh cho blog

Đây là bản đồ của cả buổi đo. Mỗi bước trỏ tới hướng dẫn chi tiết khi cần đọc sâu, nhưng chỉ làm
theo file này từ trên xuống cũng đủ.

**Tổng thời gian:** khoảng **2 giờ**, phần lớn là ngồi chờ máy chạy. Làm một lần, theo đúng thứ tự:
bước sau dùng kết quả của bước trước, và các phép đo thời gian cần máy yên tĩnh.

| # | Việc | ⏱ | Cần |
|---|---|---|---|
| 1 | Cài công cụ | 10 phút | sudo |
| 2 | Kiểm tra máy | 5 phút | |
| 3 | Phase 0–3 | 25 phút | |
| 4 | Phase 4–8 | 45 phút | |
| 5 | Phase 9 (DB thật) | 10 phút | Docker |
| 6 | P9-1: seq scan | 15 phút | |
| 7 | P9-7: hash join + perf | 10 phút | Docker, perf, sudo |
| 8 | Vẽ biểu đồ | 2 phút | uv |
| 9 | Ghi vào diary, debts, blog | 30 phút | |
| 10 | Commit, push | 2 phút | |
| (tuỳ chọn) | P0-1: torn write thật | 30 phút | root |

---

## 1. Cài công cụ  ⏱ 10 phút

Lệnh cho Ubuntu/Debian. Bản khác thì đổi tên gói tương ứng.

```bash
# Go 1.26+ (go.mod đòi 1.26.2). Gói của apt thường cũ, nên lấy bản chính thức:
#   https://go.dev/dl/  →  giải nén vào /usr/local/go, thêm /usr/local/go/bin vào PATH
go version

# Docker và compose v2, chạy được mà không cần sudo
sudo apt install -y docker.io docker-compose-v2
sudo usermod -aG docker $USER        # rồi đăng xuất, đăng nhập lại
docker ps

# perf (cho bước 7). Ubuntu:
sudo apt install -y linux-tools-$(uname -r) linux-tools-generic
# Debian: sudo apt install -y linux-perf

# jq (so JSON ở phase 0), python3 (in tóm tắt ở P9-1), uv (vẽ biểu đồ)
sudo apt install -y jq python3
curl -LsSf https://astral.sh/uv/install.sh | sh

git clone https://github.com/ThaiG2Pro/mini-kv-db.git && cd mini-kv-db
```

## 2. Kiểm tra máy  ⏱ 5 phút

Bốn điều này quyết định số đo có dùng được hay không. Nếu vấp điều nào thì sửa trước, đừng đo.

```bash
# a. Có PMU không? Không có thì bước 7 không chạy được (đây là lý do rời WSL2).
ls /sys/bus/event_source/devices/ | grep ^cpu        # cpu, hoặc cpu_core + cpu_atom

# b. perf đếm được thật không? Phải ra số, không phải <not supported>.
sudo perf stat -e cycles,cache-misses,LLC-load-misses,dTLB-load-misses -- sleep 0.1

# c. Ổ đo là ổ thật, không phải tmpfs. Các script đã tự đặt TMPDIR, nhưng thư mục DIR thì bạn chọn.
findmnt -no SOURCE,FSTYPE -T .                        # mong: ext4 / xfs / btrfs trên /dev/...

# d. Máy rảnh: không có gì trên ~20% CPU.
ps -eo pcpu,comm --sort=-pcpu | head -6
```

Thêm vài việc để số ít nhiễu:

- Tắt trình duyệt, IDE, công cụ đánh index chạy nền. Trên WSL2, chính `chroma-mcp` và indexer của
  codegraph đã làm hỏng một buổi đo (diary phase 9, bảng 9).
- Laptop thì cắm sạc. Có `cpufreq` thì đặt governor về `performance`:
  `echo performance | sudo tee /sys/devices/system/cpu/cpu*/cpufreq/scaling_governor`
- Muốn đo trên một ổ cụ thể (NVMe chẳng hạn) thì thêm `DIR=/mnt/nvme/minidb` vào trước lệnh ở các
  bước 3 và 4.

## 3. Phase 0–3  ⏱ 25 phút

```bash
REPEAT=5 ./scripts/linux-baseline.sh
```

Kết quả nằm ở `bench/baseline/<host>-<ngày>/`.
**Kiểm nhanh:** trong `p1-pager.txt`, `BenchmarkCommit` phải chậm hơn `BenchmarkCommitNoSync` hàng
trăm lần. Nếu chỉ chậm hơn vài lần thì fsync đang miễn phí (tmpfs, hoặc ổ có cache ghi không được
bảo vệ). Mở `env.txt`, xem dòng `findmnt` và `write_cache`.
Chi tiết: [`linux-baseline.md`](./linux-baseline.md).

## 4. Phase 4–8  ⏱ 45 phút

```bash
./scripts/linux-phase4-9.sh
```

Kết quả nằm ở `bench/phase4-9/<host>-<ngày>/`, mỗi phép đo một file `p<N>-*.txt`.
**Kiểm nhanh:** `p5-insert.txt` có `BenchmarkInsertBatch1` chậm hơn `BenchmarkInsertNoSync` hàng
trăm lần (WSL2: 453x).
Chi tiết và bảng tỉ số cần so: [`linux-phase4-9.md`](./linux-phase4-9.md).

## 5. Phase 9: các bảng đo giờ trên DB thật  ⏱ 10 phút

```bash
PHASES="9" ./scripts/linux-phase4-9.sh
```

Script dựng Postgres, MySQL và MariaDB qua Docker, ở các cổng 55432, 53306, 53307. Cổng nào đang bị
chiếm thì xem mục "Khi vấp" ở cuối file. Kết quả ghi vào cùng thư mục với bước 4
(`p9-breakeven.txt`, `p9-commit.txt`).

## 6. P9-1: sửa giải mã hàng có làm seq scan nhanh lên không  ⏱ 15 phút

```bash
./scripts/p91-seqscan.sh
```

**Đọc ngay ở cuối output** dòng `tỉ số sau/trước theo cặp: trung vị …, khoảng […]`. Khoảng nằm trọn
dưới 1.0 nghĩa là nhanh hơn thật; khoảng vắt qua 1.0 thì chạy lại với `PAIRS=32`.
Chi tiết: [`linux-phase9.md`](./linux-phase9.md), mục P9-1.

## 7. P9-7: hash join, và perf  ⏱ 10 phút

```bash
./scripts/p97-hashjoin.sh            # hỏi mật khẩu sudo một lần, trước khi đo
```

**Đọc bảng E** ở cuối output: số `LLC-load-misses` / `dTLB-load-misses` mỗi hàng probe, ở 256MB so
với 1MB. Bảng quyết định nằm ở [`linux-phase9.md`](./linux-phase9.md), mục "Đọc bảng E".

Xong thì dừng container phụ: `docker compose -f reallab/docker-compose.yml --profile p97 stop pgm`.

## 8. Vẽ biểu đồ  ⏱ 2 phút

```bash
uv run charts/plot.py --label "$(lscpu | sed -n 's/Model name: *//p' | head -1)"
```

Script đọc kết quả mới nhất của các bước 3–7 và ghi 8 biểu đồ vào `charts/out/`. Mỗi biểu đồ có
SVG và PNG, bản sáng và bản tối, kèm một file CSV.
**Mở `ratios.light.png` trước tiên:** nó cho biết trên một trang những tỉ số nào sống sót khi đổi
máy. Biểu đồ nào bị bỏ qua thì script in lý do.
Chi tiết, và cách nhúng ảnh vào blog: [`../charts/README.md`](../charts/README.md).

## 9. Ghi lại  ⏱ 30 phút

Quy tắc của repo: **mỗi con số kèm lệnh và output gốc**, và **viết ra chỗ nào lệch so với dự báo**
([`skills/diary/SKILL.md`](../skills/diary/SKILL.md)). Làm theo thứ tự này, mỗi dòng một việc:

- [ ] `diary/phase0.md`: thêm mục "đo lại trên Linux thuần", dán `bench/baseline/…/env.txt` và bảng
      tỉ số. Giữ nguyên phần WSL2 để so.
- [ ] `diary/phase1.md`, `phase2.md`, `phase3.md`: các tỉ số 481x, 43x, 5.5x, 25x, 1366x, 2.3x (bảng
      ở [`linux-baseline.md`](./linux-baseline.md)). Tick P1-6, P2-5, P3-5.
- [ ] `diary/phase4.md` … `phase8.md`: các tỉ số trong [`linux-phase4-9.md`](./linux-phase4-9.md).
      Ảnh `ratios` gom sẵn 13 tỉ số trong số đó.
- [ ] `diary/phase9.md`: hai mục mới, "bảng 8, lượt Linux thuần" (P9-7) và "bảng 9, lượt Linux
      thuần" (P9-1). Tick nợ nào đã trả theo bảng quyết định trong
      [`linux-phase9.md`](./linux-phase9.md).
- [ ] `docs/debts.md`: chuyển các món đã trả sang mục "Đã trả", kèm bằng chứng (lệnh và con số).
- [ ] Tỉ số nào **lệch quá 2x** hoặc **đổi chiều**: thêm một dòng vào bảng "giả thuyết sai" của
      diary phase đó. Đây là phần đáng giá nhất của cả buổi.
- [ ] Blog: thay số WSL2 bằng số mới ở những bài có biểu đồ (bài 1, 3, 10, 13), chèn ảnh từ
      `charts/out/`, và sửa câu "máy đo là một laptop chạy WSL2" trong
      [`../blog/README.md`](../blog/README.md). Bài hash join (P9-7) thì giờ viết được.

## 10. Commit, push  ⏱ 2 phút

```bash
git add bench/ diary/ docs/ blog/
git commit -m "đo lại trên Linux thuần: <tên máy>"
git push
```

`bench/` được commit để lần sau so lại được. `charts/out/` thì không: ảnh sinh lại được bất cứ lúc
nào bằng bước 8.

## (Tuỳ chọn) P0-1: torn write thật  ⏱ 30 phút, cần root

Đây là món duy nhất phải đụng tới tầng thiết bị (`dm-flakey` giả lập mất điện giữa lúc ghi), nên
tách riêng, làm sau cùng. Các bước nằm trong [`linux-baseline.md`](./linux-baseline.md), mục
"Nợ #1".

---

## Khi vấp

| Triệu chứng | Nguyên nhân | Sửa |
|---|---|---|
| `Commit / CommitNoSync` chỉ vài lần, không phải hàng trăm | fsync miễn phí: thư mục đo nằm trên tmpfs, hoặc ổ báo xong trước khi ghi thật | Xem `findmnt` và `write_cache` trong `env.txt`; đặt `DIR=` sang ổ thật |
| `!! không có PMU` | máy ảo không bật vPMU | Đổi máy, hoặc bật vPMU trong hypervisor (KVM: `-cpu host`) |
| `perf` báo `No permission` / `not supported` | perf chưa khớp kernel, hoặc thiếu quyền | Cài đúng `linux-tools-$(uname -r)`; chạy `sudo -v` trước script |
| bảng E có mỗi sự kiện hai dòng `cpu_core/…` và `cpu_atom/…` | CPU lai của Intel | Đọc dòng có số lớn hơn, hoặc ghim nhân: `docker update --cpuset-cpus=2 rl-pg rl-pgm` |
| `docker: permission denied` | user chưa vào nhóm docker | `sudo usermod -aG docker $USER`, rồi đăng nhập lại |
| `port is already allocated` | cổng 55432 / 53306 / 53307 / 55433 đang bị chiếm | Dừng dịch vụ đang chiếm, hoặc đổi cổng trong `reallab/docker-compose.yml` và `dsns` trong `reallab/main.go` |
| `go: go.mod requires go >= 1.26.2` | Go của apt quá cũ | Cài bản từ go.dev/dl |
| P9-1: khoảng tứ phân vị vắt qua 1.0 | máy còn nhiễu | Xem lại bước 2d; chạy với `PAIRS=32` |
| `uv: command not found` sau khi cài | PATH chưa cập nhật | Mở terminal mới, hoặc `source $HOME/.local/bin/env` |
| biểu đồ bị bỏ qua: `thiếu …/<file>.txt` | bước tương ứng chưa chạy hoặc dừng giữa chừng | Chạy lại đúng bước đó (các bước độc lập nhau) |
