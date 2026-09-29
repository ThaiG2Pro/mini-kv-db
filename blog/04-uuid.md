# Bài 4 — Vì sao UUID làm chậm insert (trên MySQL, mà không phải trên Postgres)

> Series [Mở nắp database](README.md) · bài 4/11 · cần đọc trước: [bài 0](00-ban-do.md)

Câu tranh luận này xuất hiện ở gần như mọi dự án: *"PK nên là số tự tăng hay UUID?"* UUID tiện
thật: sinh được ở client, không lộ số lượng bản ghi, gộp dữ liệu từ nhiều nguồn không bị trùng.
Rồi có người nói "UUID làm chậm database", người khác bảo "tôi dùng UUID mấy năm có sao đâu".

Cả hai đều đúng. Bài này đo xem **đúng ở đâu, và vì sao**.

## Thí nghiệm

Nạp 2 triệu hàng vào một bảng `(id 16 byte PRIMARY KEY, pad 100 byte)`, mỗi lô 1000 hàng là một
commit. Hai chế độ, cùng độ dài khoá, **chỉ khác thứ tự**:

- **ngẫu nhiên:** UUIDv4, loại `gen_random_uuid()` / `uuid.New()` mà ai cũng dùng.
- **tăng dần:** 16 byte tăng dần theo thời gian, đúng hình dạng của UUIDv7.

Tổng dữ liệu (~300MB) cố ý lớn hơn buffer pool (256MB) của DB, để thấy chuyện gì xảy ra khi bảng
không còn nằm gọn trong RAM. Tự chạy lại:

```bash
docker compose -f reallab/docker-compose.yml up -d
cd reallab && go run . -work pkorder
```

Kết quả trên MySQL 8.4 (`h/s` = số hàng mỗi giây, đo riêng cho từng phần tư của 2 triệu hàng):

```text
khoá           ¼ h/s     ½ h/s     ¾ h/s   4/4 h/s   tổng s  bảng MB   đọc MB   ghi MB
tăng dần       47207     46655     47404     43829     43.3      294        0      336
ngẫu nhiên     33352     20459      8045      2757    282.9      476     2460    10680
```

Và trên Postgres 17, cùng dữ liệu:

```text
khoá           ¼ h/s     ½ h/s     ¾ h/s   4/4 h/s   tổng s  bảng MB   idx MB   ghi MB
tăng dần      151486    129671    141921     98119     15.8      298       63       80
ngẫu nhiên    111479    113487    104094     70026     20.8      298       81      125
```

Gom lại (MariaDB 11.8 dùng cùng engine InnoDB với MySQL; Postgres chạy 5 lượt vì dao động nhiều):

| UUIDv4 so với tăng dần | Postgres | MySQL | MariaDB |
|---|---|---|---|
| tổng thời gian nạp | **1.0–1.8x** | **6.5–8.3x** | **9.7–11.0x** |
| tốc độ ở phần tư cuối | 0.7–2.2x | 16–19x | 32–34x |
| page ghi xuống đĩa | 1.2–1.6x | **32x** | **26x** |
| kích thước bảng | 1.0x (index PK 1.3x) | **1.62x** | **1.43x** |
| đọc từ đĩa trong lúc nạp | 1–3 MB | **2.4 GB** | **2.9 GB** |

Trên MySQL, cùng 2 triệu hàng, chỉ đổi thứ tự khoá mà **chậm hơn 6–11 lần**, bảng **to hơn
40–60%**, và DB phải **đọc 2.4GB từ đĩa** để nạp 300MB dữ liệu. Trên Postgres thì gần như không sao.

Còn một chi tiết quan trọng hơn cả con số tổng: trên MySQL, phần tư đầu tiên chỉ chậm hơn **1.4
lần**. Lúc đó bảng còn nằm gọn trong RAM. Chỉ khi bảng lớn hơn buffer pool thì tốc độ mới rơi
xuống 16–34 lần. **Trên máy dev với bảng nhỏ, bạn sẽ không bao giờ thấy vấn đề này.** Nó chỉ
hiện ra trên production, khi dữ liệu đã lớn hơn RAM.

## Bên trong: hàng của bạn nằm ở đâu?

Câu trả lời nằm ở câu hỏi của bài 0: *hàng nằm ở đâu?*

**Ở MySQL (InnoDB), bảng CHÍNH LÀ một cây B+Tree xếp theo PK.** Hàng không nằm "trong bảng" rồi
có index trỏ tới. Hàng nằm ngay trong các lá của cây PK, xếp theo thứ tự PK. Người ta gọi đó là
**clustered index**.

**Ở Postgres, bảng là một đống (heap) không có thứ tự.** Hàng mới luôn được thêm vào page cuối,
bất kể PK là gì. Index PK là một cây B+Tree **riêng**, chỉ chứa (khoá, địa chỉ hàng).

Giờ hình dung 2 triệu lần chèn vào một cây B+Tree:

```text
KHOÁ TĂNG DẦN — mọi lần chèn rơi vào đúng MỘT lá: lá cực phải

  [lá đầy][lá đầy][lá đầy][lá đầy] ... [lá đầy][lá đang ghi ◄── mọi INSERT]
     │ không bao giờ bị chạm lại           │ chỉ 1 page "nóng", luôn nằm trong RAM

KHOÁ NGẪU NHIÊN — mỗi lần chèn rơi vào một lá bất kỳ trong cây

  [lá ◄][lá][lá ◄][lá][lá][lá ◄][lá][lá] ... [lá ◄][lá][lá ◄]
    ▲ mọi lá đều "nóng": cần cả cây nằm trong RAM
    ▲ cây lớn hơn RAM ⇒ mỗi INSERT: đọc lá từ đĩa, sửa, đẩy lá khác ra đĩa
```

Với khoá tăng dần, cả 2 triệu lần chèn chỉ làm bẩn **một** page tại một thời điểm. Page đó đầy
thì đóng lại, không bao giờ bị chạm tới nữa, và page mới được mở ra bên phải. Buffer pool chỉ
cần giữ đúng một page nóng.

Với khoá ngẫu nhiên, lần chèn nào cũng có thể rơi vào **bất kỳ** lá nào. Muốn nhanh thì cả cây
phải nằm trong RAM. Khi cây lớn hơn buffer pool, mỗi lần chèn phải đọc một lá từ đĩa lên (2.4GB
đọc), sửa nó, và đẩy một lá bẩn khác xuống đĩa (32 lần nhiều page ghi hơn). Đó là toàn bộ câu
chuyện của bảng số ở trên.

Ở Postgres, heap luôn được ghi vào cuối như trường hợp tăng dần. Chỉ riêng cây index PK bị chèn
lung tung, và cây đó nhỏ (80MB, vì chỉ chứa khoá và địa chỉ), vẫn vừa trong RAM. **Postgres không
miễn nhiễm, nó chỉ đau muộn hơn**: khi riêng index PK lớn hơn RAM, chuyện y hệt sẽ xảy ra với
index đó.

### Vì sao bảng lại to hơn?

Vì cách B+Tree tách một page đầy. Đây là đoạn code tách page của minidb
(`internal/btree/split.go`, bỏ bớt phần phụ):

```go
mid := midpoint(cells) // mặc định: cắt đôi, mỗi nửa ~50%

// Chèn cực phải: khóa mới lớn hơn mọi khóa đang có. Tách 50/50 ở đây
// là tự bắn vào chân — nửa trái sẽ không bao giờ nhận thêm khóa nào
// nữa (mọi khóa sau đều lớn hơn), nên nó vĩnh viễn đầy 50%. Cắt 100/0
// để nửa trái đóng lại khi đã đầy, nửa phải nhận tiếp.
if c.n.isLeaf() && i == len(cells)-1 && c.n.next() == 0 {
	mid = len(cells) - 1
}
```

- **Khoá tăng dần** luôn chèn vào cuối lá cực phải, nên luôn rơi vào nhánh `if`: page cũ được
  giữ **đầy 100%**, page mới nhận các khoá tiếp theo. InnoDB có đúng heuristic này.
- **Khoá ngẫu nhiên** gần như không bao giờ rơi vào nhánh đó, nên page bị cắt đôi. Hai nửa, mỗi
  nửa 50%, rồi đầy dần trở lại, và dừng ở khoảng 70%.

minidb đo được ở phase 4: lá đầy **98.87%** với khoá tăng dần, **69.48%** với khoá ngẫu nhiên. Con
số 69% không phải ngẫu nhiên: đó là hằng số nổi tiếng của B-tree chèn ngẫu nhiên, xấp xỉ
ln 2 ≈ 69.3%. Tỉ số 98.87 / 69.48 = **1.42x**, và MariaDB cho bảng to hơn đúng **1.43x**.

Page chỉ đầy 70% thì cần nhiều page hơn: 1.43–1.62x như bảng trên. Bảng to hơn thì càng khó nằm
vừa RAM, và vòng luẩn quẩn bắt đầu.

minidb, một database đồ chơi 30000 dòng Go, cho ra **33 lần** nhiều page ghi hơn khi chèn ngẫu
nhiên so với tăng dần. InnoDB, sau 25 năm phát triển, cho ra **26–32 lần**. Cùng kiến trúc
(clustered B+Tree) thì cùng tỉ số. Tỉ số này là tính chất của **cấu trúc dữ liệu**, không phải
của code tốt hay dở.

## Mang về dùng

1. **Trên MySQL/MariaDB, đừng dùng UUIDv4 làm PRIMARY KEY của bảng lớn.** Nếu cần UUID:
   - dùng **UUIDv7** (tăng dần theo thời gian), hoặc
   - giữ PK là `BIGINT AUTO_INCREMENT`, thêm một cột `UUID` có `UNIQUE` cho bên ngoài dùng. Cột
     unique đó vẫn là một index bị chèn ngẫu nhiên, nhưng nhỏ hơn nhiều so với cả bảng.
   - Nếu buộc phải dùng UUID do MySQL sinh (`UUID()` là v1), lưu bằng `UUID_TO_BIN(UUID(), 1)`:
     tham số `1` đổi chỗ các byte thời gian lên đầu, nên khoá thành tăng dần.
2. **Trên Postgres, UUIDv4 ít đau hơn nhiều, nhưng UUIDv7 vẫn tốt hơn:** index PK nhỏ hơn ~28%,
   và bạn không phải lo tới ngày index vượt RAM. Postgres 18 có sẵn hàm `uuidv7()`.
3. **Đừng kết luận từ benchmark trên máy dev.** Ở phần tư đầu, khi dữ liệu còn vừa RAM, UUIDv4 chỉ
   chậm 1.0–1.4x. Muốn biết một thiết kế có chịu được production không, phải test với dữ liệu
   **lớn hơn buffer pool**.
4. **Câu hỏi đáng hỏi về mọi database:** *"nó để hàng theo thứ tự nào?"* Câu trả lời cho biết
   insert kiểu nào là rẻ, kiểu nào là đắt.

---

Số đo gốc, lệnh và cả những lượt đo sai: [`diary/phase9.md`](../diary/phase9.md), bảng 3.
Thí nghiệm tương ứng trên minidb: [`diary/phase4.md`](../diary/phase4.md).

**Bài tiếp theo:** [Bài 5 — Vì sao mất điện không mất dữ liệu?](05-wal.md)
