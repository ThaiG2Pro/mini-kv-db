# Skill: Ghi nhật ký phase của minidb

Áp dụng mỗi khi làm việc trong repo này và có bất kỳ thứ gì cần ghi vào `diary/phase*.md`.

## Nguyên tắc gốc

> **Mọi con số và mọi kết luận phải kèm lệnh shell sinh ra nó, và output thật dán nguyên văn.**

Lý do: nhật ký này không phải báo cáo thành tích, nó là **bản ghi thí nghiệm**. Sáu tháng sau
đọc lại phải **chạy lại được**; trên máy khác phải **biết vì sao số khác**. Một con số không có
lệnh đi kèm là một con số không kiểm chứng được — tức là vô giá trị.

Hệ quả trực tiếp:

1. **Ghi trong lúc làm, không phải sau khi xong.** Giả thuyết sai lúc còn nóng mới là thứ đáng giá.
   Viết lại sau khi xong sẽ chỉ còn kết quả đẹp, mất sạch quá trình.
2. **Dán output nguyên văn, không tóm tắt.** "nhanh hơn nhiều" là vô nghĩa; `69µs vs 3µs` thì không.
3. **Chốt bằng TỈ SỐ, không phải số tuyệt đối.** Số tuyệt đối đổi theo máy và dao động giữa các
   lần chạy (trên WSL2 đã thấy fsync dao động 2.7x). Tỉ số thì bền và mang ý nghĩa thiết kế.
4. **Ghi cả cái sai, ghi trước cái đúng.** Bảng "giả thuyết sai" là cột giá trị nhất của cả file.
5. **Không tin số đo trước khi so với tỉ số lý thuyết kỳ vọng.** Thấy 1.1x ở chỗ đáng lẽ 100x
   thì nghi **bench sai**, đừng nghi máy lạ. (Đã dính đúng bẫy này ở phase 0.)

## Cấu trúc bắt buộc của `diary/phaseN.md`

| Mục | Bắt buộc có gì |
|---|---|
| Header | thời lượng, ngày bắt đầu/kết thúc, trạng thái, **commit hash** lúc chốt phase |
| **Môi trường** | output của `uname -srmo && go version && df -hT . \| tail -1` — không có thì mọi bench vô nghĩa |
| Mục tiêu phase | 1-2 câu |
| Câu hỏi phải trả lời được | danh sách câu hỏi tự chấm điểm, viết **trước** khi làm |
| Deliverable | test/bench cụ thể chứng minh đã hiểu, không phải "code chạy được" |
| **Reproduce toàn bộ phase** | khối bash copy-paste chạy lại được từ đầu |
| **Nhật ký theo ngày** | mỗi mục: ` ```console ` block (lệnh + output thật) → *Đọc kết quả* → *Đang nghĩ gì* |
| **Giả thuyết sai** | bảng 4 cột: tôi tưởng là / thực tế là / **lệnh + output đã lật tẩy** / đã sửa thế nào |
| **Số đo** | kèm **lệnh, ngày, commit, máy**; kết lại bằng bảng **tỉ số** |
| **Invariant + lệnh kiểm chứng** | bảng: invariant / cài ở `file:hàm` / lệnh kiểm chứng / kết quả |
| Đọc gì | tài liệu đã đọc *trong* phase này |
| **Rút ra** | viết như thể đang giải thích cho người khác — không phải gạch đầu dòng từ khoá |
| Nợ kỹ thuật | checkbox những thứ **biết là còn thiếu** |

Mẫu chuẩn: xem `diary/phase1.md` (template trống) và `diary/phase0.md` (bản đã điền thật).

## Khuôn một mục nhật ký

````markdown
### YYYY-MM-DD — <việc chính của buổi>

```console
$ go run ./cmd/iolab -filemb 512
pread 4KB ngẫu nhiên, CACHE LẠNH            14521 ops/s      69µs
pread 4KB ngẫu nhiên, CACHE NÓNG           891515 ops/s       1µs
```

**Đọc kết quả:** nóng/lạnh = 61x, khớp kỳ vọng lý thuyết (~100x).

**Đang nghĩ gì:** <nghi vấn còn lại, hướng tiếp theo>
````

Nếu output dài, cắt bớt **phần không liên quan** và ghi rõ `...`, nhưng **không bao giờ**
sửa hay làm tròn con số.

## Khi số đo trông vô lý

Đây là tình huống quan trọng nhất, và nó phải để lại dấu vết trong nhật ký:

1. Viết ra **tỉ số kỳ vọng theo lý thuyết** trước.
2. Nếu đo lệch xa → **giả định bench sai**, không phải máy lạ.
3. Truy ra "cái tôi tưởng đang đo" ≠ "cái máy thật sự làm".
   Hai thủ phạm kinh điển đã gặp: đo memcpy trong page cache mà tưởng đo đĩa;
   đo làm-bẩn-page-cache mà tưởng đo ghi đĩa (quên tách đồng hồ cho `fsync`).
4. Sửa bench, **giữ lại cả output sai lẫn output đúng** trong nhật ký, và điền một dòng vào
   bảng giả thuyết sai.

## Chống các thói quen xấu

| Đừng | Hãy |
|---|---|
| "chậm hơn đáng kể" | `69µs vs 3µs → 23x` |
| "đã test, chạy ổn" | dán lệnh `go test ... -v` và output |
| "buffer pool giúp tăng tốc" | `pread cache lạnh/nóng = 61x → đây là lý do buffer pool tồn tại` |
| Viết nhật ký sau khi phase xong | viết ngay lúc bug còn nóng |
| Giấu benchmark đã đo sai | ghi nó vào bảng giả thuyết sai — đó là phần đáng giá nhất |
| Ghi số mà không ghi máy/ngày/commit | ba thứ đó là điều kiện để số có nghĩa |

## Checklist trước khi đánh dấu phase ✅

- [ ] Mục **Môi trường** có output thật
- [ ] Khối **Reproduce** chạy lại được từ máy sạch (đã thử chạy lại ít nhất 1 lần)
- [ ] Mọi bảng số đều ghi lệnh + ngày + commit + máy
- [ ] Có ít nhất một dòng trong bảng **giả thuyết sai** (nếu không có, khả năng cao là bạn
      chưa thật sự thử gì khó, hoặc đã quên ghi)
- [ ] Mọi **câu hỏi phải trả lời được** ở đầu file đã được trả lời ở mục **Rút ra**
- [ ] Bảng **invariant** chỉ đúng `file:hàm` và lệnh kiểm chứng
- [ ] Cập nhật cột trạng thái của phase trong `ROADMAP.md`
