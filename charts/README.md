# Biểu đồ cho blog

Biến kết quả bench từ máy Linux thuần thành ảnh, mang thẳng vào bài viết trên nền tảng blog của
bạn. Chỉ cần [`uv`](https://docs.astral.sh/uv/): script tự kéo matplotlib về lần chạy đầu, không
phải cài gì vào hệ thống.

```bash
uv run charts/plot.py                  # đọc kết quả mới nhất trong bench/, ghi vào charts/out/
uv run charts/plot.py --sample         # xem trước bằng dữ liệu mẫu WSL2 (charts/sample/)
uv run charts/plot.py --out ~/blog/img --label "Ryzen 7, NVMe"
```

## Quy trình

1. Trên máy Linux, chạy các script đo (hướng dẫn: [`docs/linux-baseline.md`](../docs/linux-baseline.md),
   [`docs/linux-phase4-9.md`](../docs/linux-phase4-9.md), [`docs/linux-phase9.md`](../docs/linux-phase9.md)).
   Kết quả nằm ở `bench/<loại>/<host>-<ngày>/`.
2. `uv run charts/plot.py`. Script lấy thư mục **mới nhất** của mỗi loại. Muốn chọn thư mục khác thì
   dùng `--baseline`, `--phase49`, `--p91`, `--p97`.
3. Thiếu dữ liệu cho biểu đồ nào thì biểu đồ đó bị bỏ qua và script in lý do, nên chạy được cả khi
   mới đo xong một phần.
4. Lấy file trong `charts/out/` (thư mục này không được commit).

## Mỗi biểu đồ ra 5 file

| File | Dùng khi |
|---|---|
| `<tên>.light.svg` | nền tảng nhận SVG (sắc nét ở mọi cỡ, nhẹ). Chữ là chữ thật, tìm kiếm được |
| `<tên>.light.png` | nền tảng chỉ nhận ảnh raster. 1600×900, đủ nét trên màn hình retina |
| `<tên>.dark.svg`, `<tên>.dark.png` | blog có giao diện tối. Màu tối là một bộ riêng đã kiểm tra, không phải đảo màu |
| `<tên>.csv` | muốn vẽ lại bằng công cụ của nền tảng (Datawrapper, Flourish, Google Sheets, Notion chart...) |

Nền tảng đổi theo theme người đọc thì dùng cặp sáng/tối với thẻ `<picture>`:

```html
<picture>
  <source srcset="chain-depth.dark.svg" media="(prefers-color-scheme: dark)">
  <img src="chain-depth.light.svg" alt="ns mỗi lần Get theo số phiên bản trong chuỗi">
</picture>
```

## Các biểu đồ

| Tên | Cho bài | Cần file | Nói gì |
|---|---|---|---|
| `ratios` | mọi bài có số đo | `baseline/p1-pager.txt`, `p2-page.txt`, `phase4-9/p4…p7-*.txt` | 13 tỉ số: WSL2 (trong diary) so với máy mới. Tỉ số nào sống sót khi đổi máy |
| `chain-depth` | [bài 13](../blog/13-cho-cham-khong-o-cho-doan.md) | `phase4-9/p6-get.txt` | ns mỗi lần Get theo độ dài chuỗi version, snapshot mới vs cũ |
| `copy-vs-alloc` | bài 13 | `phase4-9/p4-copyalloc.txt` | cấp phát + chép vs chỉ chép 897 byte |
| `commit-batch` | [bài 1](../blog/01-commit.md) | `phase4-9/p5-insert.txt` | µs mỗi khoá theo số khoá mỗi transaction (log-log), đường ngang là "không fsync" |
| `group-commit` | bài 1 | `phase4-9/p9-commit.txt` (`PHASES="9"`) | số commit dùng chung một fsync, theo số client |
| `getpool` | [bài 3](../blog/03-buffer-pool.md), nợ P4-6 | `phase4-9/p4-getpool.txt` (+ `env.txt`) | ns mỗi lần Get theo cỡ buffer pool, đường dọc là cỡ L2/L3 của máy |
| `p91-pairs` | bài 13 | `p91/pairs.txt` | tỉ số sau/trước của từng cặp chạy xen kẽ, dải tô là khoảng tứ phân vị |
| `p97-probe-gap` | [bài 10](../blog/10-explain.md), nợ P9-7 | `p97/hashjoin.txt` | ms chậm thêm của hash join 1 batch theo số hàng probe, pg vs pgm |
| `p97-perf-<db>` | bài 10, nợ P9-7 | `p97/hashjoin.txt` có phép E (cần PMU) | cache/TLB miss mỗi hàng probe, 1 batch chia 16 batch |

Tiêu đề nào là một câu khẳng định thì chỉ in ra khi dữ liệu cho thấy điều đó (ví dụ `p91-pairs`
chọn giữa "nhanh hơn thật", "chậm hơn thật" và "chưa kết luận được" theo khoảng tứ phân vị). Các
tiêu đề còn lại là câu hỏi trung tính, để số trên máy mới tự trả lời.

## Dữ liệu mẫu (`charts/sample/`)

Số thật đo trên WSL2 ngày 2026-10-01, lúc máy đang bận (xem `diary/phase9.md`, bảng 9), đặt theo
đúng cấu trúc thư mục mà các script Linux sinh ra. Chỉ dùng để xem trước bố cục. **Đừng đưa ảnh
dựng từ dữ liệu mẫu vào bài viết.** Số bench Go chạy với `-benchtime` ngắn; `p91/pairs.txt` là 16
cặp của bảng 9; `p97/hashjoin.txt` lấy từ bảng 8; `p9-commit.txt` chỉ có hai dòng từ blog bài 1.

## Màu và nét

Theo reference palette của dataviz skill: ba slot đầu (xanh `#2a78d6`, cam `#eb6834`, aqua
`#1baf7a`; bản tối `#3987e5`, `#d95926`, `#199e70`) qua mọi kiểm tra mù màu ở cả hai chế độ
(`validate_palette.js`: CVD ΔE 9.2 sáng / 9.4 tối). Đường 2px, marker 8px có vòng màu nền, lưới
hairline liền, chữ không bao giờ dùng màu của series. Biểu đồ nào có từ hai series trở lên đều có
chú giải, cộng thêm nhãn trực tiếp ở cuối đường.

Đổi sang màu thương hiệu của bạn: sửa `THEMES` ở đầu `plot.py`, rồi chạy lại validator cho bộ màu
mới ở cả hai chế độ.
