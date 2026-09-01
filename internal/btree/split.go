package btree

import (
	"fmt"

	"minidb/internal/page"
	"minidb/internal/pager"
)

// scratch là vùng tạm dùng chung cho split/merge: chép toàn bộ cell của một
// node ra ngoài rồi dựng lại hai page từ đầu.
//
// Vì sao chép ra ngoài thay vì dịch cell tại chỗ: cell là byte biến độ dài
// nằm rải trong page, dịch tại chỗ phải xử lý chồng lấn và phân mảnh — đúng
// loại code sai một chỗ là hỏng cây mà test khó bắt. Split chỉ xảy ra một lần
// mỗi ~100 insert, nên đây là chỗ đáng đánh đổi tốc độ lấy sự chắc chắn.
type scratch struct {
	buf   []byte
	cells [][]byte
	sep   [page.PageSize]byte
}

func newScratch() scratch {
	return scratch{
		// Cap cố định, không bao giờ append quá: nội dung một page (<4096) +
		// một entry (<2048). Nếu buf phải cấp lại thì MỌI slice trong cells
		// sẽ trỏ vào mảng cũ — nên cap phải đủ, và có kiểm tra ở add().
		buf:   make([]byte, 0, 2*page.PageSize),
		cells: make([][]byte, 0, page.PageSize/8),
	}
}

func (s *scratch) reset() {
	s.buf = s.buf[:0]
	s.cells = s.cells[:0]
}

func (s *scratch) add(c []byte) {
	if len(s.buf)+len(c) > cap(s.buf) {
		panic("btree: scratch tràn — cell của một page không thể vượt 2 page")
	}
	off := len(s.buf)
	s.buf = append(s.buf, c...)
	s.cells = append(s.cells, s.buf[off:off+len(c)])
}

// collect chép mọi cell của n ra scratch, chèn thêm `extra` ở vị trí `at`.
func (t *Tree) collect(n node, at int, extra []byte) {
	t.sc.reset()
	nc := n.numCells()
	for k := 0; k <= nc; k++ {
		if k == at {
			t.sc.add(extra)
		}
		if k < nc {
			t.sc.add(n.cell(k))
		}
	}
}

// midpoint chọn chỗ cắt sao cho hai nửa xấp xỉ bằng nhau về BYTE.
// Trả về chỉ số cell đầu tiên thuộc nửa phải.
func midpoint(cells [][]byte) int {
	total := 0
	for _, c := range cells {
		total += len(c) + 4 // + slot
	}
	half, run := total/2, 0
	for i, c := range cells {
		prev := run
		run += len(c) + 4
		if run >= half {
			// Hai ứng viên: cắt TRƯỚC cell i (nửa trái = prev byte) hoặc SAU
			// nó (run byte). Lấy cái gần half hơn. Luôn cắt sau thì nửa phải
			// hụt tới trọn một cell — với cell 350 byte đó là 8% page, đủ để
			// đẩy nửa phải xuống dưới 50%. Đo được: xem diary phase 4.
			cut := i + 1
			if i > 0 && half-prev < run-half {
				cut = i
			}
			// Không bao giờ để một nửa rỗng: cây phải luôn có chỗ nhét khóa.
			if cut < 1 {
				cut = 1
			}
			if cut > len(cells)-1 {
				cut = len(cells) - 1
			}
			return cut
		}
	}
	return len(cells) / 2
}

// fill dựng lại một node từ danh sách cell. Giữ nguyên pageLSN: Init xóa sạch
// header, mà pageLSN là thứ phase 5 dùng để biết log của page này đã fsync
// chưa — mất nó là mất recoverability, im lặng.
func fill(n node, typ uint8, cells [][]byte) error {
	lsn := n.p.LSN()
	page.Init(n.p, typ)
	n.p.SetLSN(lsn)
	for k, c := range cells {
		if err := n.p.InsertAt(page.SlotID(k), c); err != nil {
			return fmt.Errorf("btree: dựng lại node, cell %d/%d: %w", k, len(cells), err)
		}
	}
	return nil
}

// split tách node ở crumb c, có tính cả cell đang chờ chèn (cell, ở vị trí i).
// Trả về khóa phân tách đẩy lên cha và PageID của nửa phải.
//
// LƯU Ý: sep chỉ còn hiệu lực tới lần gọi split tiếp theo (nó nằm trong
// scratch dùng chung). Người gọi phải chép nó vào cell của cha ngay.
func (t *Tree) split(c *crumb, i int, cell []byte) ([]byte, pager.PageID, error) {
	t.collect(c.n, i, cell)
	cells := t.sc.cells

	mid := midpoint(cells)
	// c.n.next() == 0 nghĩa là leaf này là leaf CỰC PHẢI của cả cây, không chỉ
	// là "khóa mới đứng cuối trong leaf này". Thiếu vế đó thì với khóa ngẫu
	// nhiên, cứ ~1/fanout lần chèn lại rơi vào cuối một leaf nào đó giữa cây
	// và cắt 100/0 ở đấy — đẻ ra một page gần rỗng mà không bao giờ có khóa
	// nào lấp lại (khóa lớn hơn đi sang phải, nhỏ hơn đi sang trái). Đo được:
	// page 174/4096 byte = 4%, xem diary phase 4.
	if c.n.isLeaf() && t.RightmostSplit && i == len(cells)-1 && c.n.next() == 0 {
		// Chèn cực phải: khóa mới lớn hơn mọi khóa đang có. Tách 50/50 ở đây
		// là tự bắn vào chân — nửa trái sẽ không bao giờ nhận thêm khóa nào
		// nữa (mọi khóa sau đều lớn hơn), nên nó vĩnh viễn đầy 50%. Cắt 100/0
		// để nửa trái đóng lại khi đã đầy, nửa phải nhận tiếp.
		//
		// Đây chính là lý do khóa auto-increment cho cây đặc ~100% còn UUIDv4
		// cho cây đặc ~70%: UUIDv4 rơi ngẫu nhiên nên gần như không bao giờ
		// đi vào nhánh này.
		mid = len(cells) - 1
	}

	right, err := t.pool.NewPage(c.n.p.Type())
	if err != nil {
		return nil, 0, err
	}
	rid := right.PageID()
	rn := node{p: right.Data}

	var sep []byte
	if c.n.isLeaf() {
		// Leaf: khóa ở chỗ cắt được NHÂN BẢN lên cha chứ không bị lấy đi —
		// mọi khóa vẫn còn nguyên ở tầng lá. Đó là đặc điểm của B+Tree, và là
		// lý do range scan chỉ cần đi ngang tầng lá.
		nextID := c.n.next()
		if err := fill(rn, page.TypeLeaf, cells[mid:]); err != nil {
			t.pool.Unpin(rid, true)
			return nil, 0, err
		}
		rn.setNext(nextID)
		if err := fill(c.n, page.TypeLeaf, cells[:mid]); err != nil {
			t.pool.Unpin(rid, true)
			return nil, 0, err
		}
		c.n.setNext(rid)
		sep = leafKey(cells[mid])
		t.st.LeafSplits++
	} else {
		// Branch: khóa ở chỗ cắt ĐI HẲN lên cha. Con của nó thành con cực
		// phải của nửa trái — nếu quên bước này, một cây con biến mất khỏi
		// cây mà mọi bất biến cục bộ vẫn đúng.
		oldRight := c.n.rightmost()
		promoted := cells[mid]
		if err := fill(rn, page.TypeBranch, cells[mid+1:]); err != nil {
			t.pool.Unpin(rid, true)
			return nil, 0, err
		}
		rn.setRightmost(oldRight)
		if err := fill(c.n, page.TypeBranch, cells[:mid]); err != nil {
			t.pool.Unpin(rid, true)
			return nil, 0, err
		}
		c.n.setRightmost(branchChild(promoted))
		sep = branchKey(promoted)
	}

	// Chép sep ra khỏi vùng cell (fill() vừa ghi đè page, và scratch sẽ bị
	// reset ở lần split kế tiếp trên đường lan lên).
	n := copy(t.sc.sep[:], sep)
	t.unpin(rid, true)
	return t.sc.sep[:n], rid, nil
}
