// Package exec là tầng THI HÀNH: nó chạy một cây vật lý của internal/plan
// theo mô hình Volcano (kéo từng hàng).
//
// Ba tính chất định hình cả package:
//
//  1. **Một tuple phẳng dùng chung.** Cả cây làm việc trên đúng một
//     []keys.Value rộng bằng tổng số cột của mọi quan hệ; mỗi quan hệ sở hữu
//     một dải slot liền nhau. Không cấp phát hàng mỗi bước, và các toán tử
//     lồng nhau không đè lên nhau vì dải slot của chúng rời nhau. Postgres
//     gọi cái này là TupleTableSlot.
//
//  2. **Toán tử CHẶN phải nói ra là nó chặn.** Sort và vế build của hash join
//     phải đọc HẾT đầu vào trước khi trả hàng đầu tiên. Đó là lý do LIMIT
//     không giúp được gì khi có ORDER BY không dùng index, và là lý do một
//     truy vấn có hash join không bao giờ trả về hàng đầu nhanh.
//
//  3. **Hết bộ nhớ là một trạng thái bình thường, không phải một lỗi.** Mọi
//     toán tử chặn đều có đường tràn ra đĩa. Cái giá của việc tràn được ĐO,
//     không ước lượng: xem cmd/sqllab.
package exec

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"

	"minidb/internal/keys"
)

// rowFile là một file tạm chứa các cặp (khóa, hàng).
//
// Định dạng mỗi mục: uvarint(len khóa) khóa uvarint(len hàng) hàng.
//
// Khóa được mã hoá bằng internal/keys — và ở đây bộ mã hoá của phase 7 trả về
// một khoản lãi không hề nằm trong dự tính của nó. Nó GIỮ THỨ TỰ, nên với một
// run của external merge sort thì
//
//	bytes.Compare(khóa_a, khóa_b) == keys.CompareTuple(hàng_a, hàng_b, order)
//
// tức là phép trộn k đường so sánh bằng **memcmp trên byte thô**, không phải
// giải mã ra rồi so theo kiểu. Phase 7 đo được khoảng cách ấy là 5.7x
// (2.468 ns vs 14.08 ns). Một bộ mã hoá được thiết kế cho B+Tree hoá ra là bộ
// mã hoá đúng cho sắp xếp ngoài — cùng một lý do: cả hai đều là "so sánh rất
// nhiều lần trên dữ liệu không cần hiểu".
//
// Với hash join thì khóa là khóa join đã mã hoá, và tính giữ thứ tự không
// dùng tới; ở đó cái đáng là tính TỰ PHÂN ĐỊNH — một khóa nhiều cột nối đuôi
// nhau vẫn so bằng byte được mà không cần lưu độ dài từng cột.
type rowFile struct {
	f    *os.File
	w    *bufio.Writer
	r    *bufio.Reader
	n    int
	size int64
	hdr  [binary.MaxVarintLen64]byte
	kbuf []byte
	rbuf []byte
}

func newRowFile(dir, pattern string) (*rowFile, error) {
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return nil, err
	}
	// Xoá ngay khi vừa tạo: file vẫn dùng được qua descriptor cho tới khi
	// đóng, nhưng không còn tên trong thư mục. Nhờ vậy một lần crash giữa
	// truy vấn KHÔNG để lại rác — bài học phase 1 về việc đừng tin vào một
	// bước dọn dẹp chạy sau. (Không chạy được trên Windows; ở đó phải dọn
	// bằng Close, xem nợ P8.)
	if err := os.Remove(f.Name()); err != nil {
		f.Close()
		return nil, err
	}
	return &rowFile{f: f, w: bufio.NewWriterSize(f, 64<<10)}, nil
}

func (rf *rowFile) Append(key []byte, row []keys.Value) error {
	if rf.w == nil {
		return fmt.Errorf("exec: ghi vào rowFile đã chuyển sang đọc")
	}
	rowb := keys.Encode(nil, row, nil)
	n := binary.PutUvarint(rf.hdr[:], uint64(len(key)))
	if _, err := rf.w.Write(rf.hdr[:n]); err != nil {
		return err
	}
	if _, err := rf.w.Write(key); err != nil {
		return err
	}
	n = binary.PutUvarint(rf.hdr[:], uint64(len(rowb)))
	if _, err := rf.w.Write(rf.hdr[:n]); err != nil {
		return err
	}
	if _, err := rf.w.Write(rowb); err != nil {
		return err
	}
	rf.n++
	rf.size += int64(len(key) + len(rowb) + 2)
	return nil
}

// Rewind chuyển từ chế độ ghi sang đọc từ đầu.
//
// KHÔNG fsync. Đây là chỗ duy nhất trong cả repo ghi ra đĩa mà không cần bền:
// dữ liệu tạm của một truy vấn không có ai đọc lại sau khi tiến trình chết,
// nên một lần fsync ở đây là 1.58 ms (số đo phase 0/7) trả cho một sự bảo đảm
// vô nghĩa. Nói rõ ra vì bốn phase trước dạy điều ngược lại, và biết KHI NÀO
// không cần fsync cũng là một phần của việc hiểu fsync.
func (rf *rowFile) Rewind() error {
	if rf.w != nil {
		if err := rf.w.Flush(); err != nil {
			return err
		}
		rf.w = nil
	}
	if _, err := rf.f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	rf.r = bufio.NewReaderSize(rf.f, 64<<10)
	return nil
}

// Next đọc mục kế tiếp. Khóa và hàng trả về CHỈ có giá trị tới lần Next sau.
func (rf *rowFile) Next(width int) (key []byte, row []keys.Value, ok bool, err error) {
	if rf.r == nil {
		return nil, nil, false, fmt.Errorf("exec: đọc rowFile chưa Rewind")
	}
	kl, err := binary.ReadUvarint(rf.r)
	if err == io.EOF {
		return nil, nil, false, nil
	}
	if err != nil {
		return nil, nil, false, err
	}
	rf.kbuf = grow(rf.kbuf, int(kl))
	if _, err := io.ReadFull(rf.r, rf.kbuf); err != nil {
		return nil, nil, false, err
	}
	rl, err := binary.ReadUvarint(rf.r)
	if err != nil {
		return nil, nil, false, err
	}
	rf.rbuf = grow(rf.rbuf, int(rl))
	if _, err := io.ReadFull(rf.r, rf.rbuf); err != nil {
		return nil, nil, false, err
	}
	vals, _, err := keys.Decode(rf.rbuf, width, nil)
	if err != nil {
		return nil, nil, false, fmt.Errorf("exec: giải mã hàng tạm: %w", err)
	}
	return rf.kbuf, vals, true, nil
}

func (rf *rowFile) Close() error {
	if rf.f == nil {
		return nil
	}
	err := rf.f.Close()
	rf.f = nil
	return err
}

func grow(b []byte, n int) []byte {
	if cap(b) < n {
		return make([]byte, n)
	}
	return b[:n]
}

// ---------- trộn k đường ----------

// mergeItem là một mục đang chờ trong heap trộn.
type mergeItem struct {
	key []byte
	row []keys.Value
	src int
}

// mergeHeap là min-heap trên khóa THÔ. Không dùng container/heap để tránh
// interface{} và một lần cấp phát mỗi lần Push/Pop — trộn là vòng lặp chạy
// nhiều nhất trong cả phép sắp xếp ngoài.
type mergeHeap []mergeItem

func (h mergeHeap) less(i, j int) bool { return bytes.Compare(h[i].key, h[j].key) < 0 }

func (h *mergeHeap) push(it mergeItem) {
	*h = append(*h, it)
	i := len(*h) - 1
	for i > 0 {
		p := (i - 1) / 2
		if !h.less(i, p) {
			break
		}
		(*h)[i], (*h)[p] = (*h)[p], (*h)[i]
		i = p
	}
}

func (h *mergeHeap) pop() mergeItem {
	old := *h
	top := old[0]
	last := len(old) - 1
	old[0] = old[last]
	*h = old[:last]
	h.down(0)
	return top
}

func (h *mergeHeap) down(i int) {
	n := len(*h)
	for {
		l, r := 2*i+1, 2*i+2
		m := i
		if l < n && h.less(l, m) {
			m = l
		}
		if r < n && h.less(r, m) {
			m = r
		}
		if m == i {
			return
		}
		(*h)[i], (*h)[m] = (*h)[m], (*h)[i]
		i = m
	}
}
