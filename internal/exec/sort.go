package exec

import (
	"bytes"
	"sort"

	"minidb/internal/keys"
	"minidb/internal/plan"
)

// sort.go cài ORDER BY, và nó là toán tử CHẶN đầu tiên trong cây: nó phải đọc
// hết đầu vào trước khi trả hàng đầu tiên.
//
// Đó không phải một chi tiết cài đặt mà là một tính chất người dùng thấy
// được: `SELECT ... ORDER BY x LIMIT 10` trên một cột không có index phải đọc
// và sắp CẢ bảng để biết 10 hàng đầu. Đây là lý do phép bỏ Sort ở
// plan.elideSort không phải một phép tối ưu nhỏ — nó biến một truy vấn chặn
// thành một truy vấn streaming, tức là đổi độ phức tạp của LIMIT từ O(n log n)
// thành O(k).
//
// Khi số hàng vượt hạn mức thì sắp NGOÀI:
//
//	1. đọc đầy hạn mức, sắp trong RAM, ghi ra một RUN đã sắp;
//	2. lặp lại tới hết đầu vào;
//	3. trộn k run bằng một min-heap.
//
// Đây đúng là external merge sort của sách, và điều đáng ghi lại là chỗ nó rẻ
// hơn dự kiến: khóa sắp được ghi ra ở dạng mã hoá GIỮ THỨ TỰ của phase 7, nên
// bước 3 so sánh bằng memcmp trên byte thô. Xem chú thích của rowFile.

type sortOp struct {
	r      *Runner
	in     Op
	by     []plan.SortKey
	ord    keys.Order
	budget int
	width  int

	opened bool
	// trong RAM
	mem []memRow
	pos int
	// ngoài RAM
	runs []*rowFile
	h    mergeHeap
	done bool
}

type memRow struct {
	key []byte
	row []keys.Value
}

func (r *Runner) newSort(p *plan.PSort, in Op) (Op, error) {
	ord := make(keys.Order, len(p.By))
	for i, k := range p.By {
		ord[i] = k.Desc
	}
	budget := p.Budget
	if r.Budget > 0 {
		budget = r.Budget
	}
	if budget < 2 {
		budget = 2
	}
	return &sortOp{r: r, in: in, by: p.By, ord: ord, budget: budget, width: len(r.Row)}, nil
}

// keyOf mã hoá khoá sắp của hàng hiện tại.
//
// DESC được cài bằng cách để keys.Encode LẤY BÙ BYTE của cột ấy (phase 7), nên
// mọi khoá — dù trộn ASC và DESC — vẫn so được bằng một phép bytes.Compare duy
// nhất. Không có tính chất đó thì heap trộn phải mang theo chiều của từng cột
// và so theo từng cột, tức là mất đúng cái 5.7x mà memcmp cho.
func (s *sortOp) keyOf() ([]byte, error) {
	vals := make([]keys.Value, len(s.by))
	for i, k := range s.by {
		v, err := k.E.Eval(s.r.Row)
		if err != nil {
			return nil, err
		}
		vals[i] = v
	}
	return keys.Encode(nil, vals, s.ord), nil
}

func (s *sortOp) open() error {
	for {
		ok, err := s.in.Next()
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		key, err := s.keyOf()
		if err != nil {
			return err
		}
		row := make([]keys.Value, s.width)
		copy(row, s.r.Row)
		s.mem = append(s.mem, memRow{key: key, row: row})
		s.r.St.SortRows++
		if len(s.mem) >= s.budget {
			if err := s.flushRun(); err != nil {
				return err
			}
		}
	}
	if len(s.runs) == 0 {
		s.sortMem()
		return nil
	}
	// Còn hàng trong RAM thì nó là run cuối. Không giữ lại một nửa trong RAM
	// và một nửa trên đĩa rồi trộn lẫn hai loại: một đường code duy nhất cho
	// mọi run là một đường code được kiểm.
	if len(s.mem) > 0 {
		if err := s.flushRun(); err != nil {
			return err
		}
	}
	return s.openMerge()
}

func (s *sortOp) sortMem() {
	sort.SliceStable(s.mem, func(i, j int) bool {
		return bytes.Compare(s.mem[i].key, s.mem[j].key) < 0
	})
}

func (s *sortOp) flushRun() error {
	s.sortMem()
	rf, err := newRowFile(s.r.TmpDir, "minidb-sort-*")
	if err != nil {
		return err
	}
	for _, m := range s.mem {
		if err := rf.Append(m.key, m.row); err != nil {
			rf.Close()
			return err
		}
	}
	s.runs = append(s.runs, rf)
	s.r.St.SortRuns++
	s.r.St.SpillFiles++
	s.r.St.SpillBytes += rf.size
	s.mem = s.mem[:0]
	return nil
}

func (s *sortOp) openMerge() error {
	for i, rf := range s.runs {
		if err := rf.Rewind(); err != nil {
			return err
		}
		key, row, ok, err := rf.Next(s.width)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		// Phải CHÉP: rowFile.Next trả buffer dùng lại, mà heap giữ mục này
		// qua nhiều lần Next của các run khác. Đây đúng là loại lỗi mà một
		// bài test nhỏ (một run, ít hàng) không bao giờ bắt được.
		s.h.push(mergeItem{key: append([]byte(nil), key...),
			row: append([]keys.Value(nil), row...), src: i})
		s.r.St.SpillRereads++
	}
	return nil
}

func (s *sortOp) Next() (bool, error) {
	if !s.opened {
		if err := s.open(); err != nil {
			return false, err
		}
		s.opened = true
	}
	if len(s.runs) == 0 {
		if s.pos >= len(s.mem) {
			return false, nil
		}
		copy(s.r.Row, s.mem[s.pos].row)
		s.pos++
		return true, nil
	}
	if len(s.h) == 0 {
		return false, nil
	}
	it := s.h.pop()
	copy(s.r.Row, it.row)
	key, row, ok, err := s.runs[it.src].Next(s.width)
	if err != nil {
		return false, err
	}
	if ok {
		s.h.push(mergeItem{key: append([]byte(nil), key...),
			row: append([]keys.Value(nil), row...), src: it.src})
		s.r.St.SpillRereads++
	}
	return true, nil
}

func (s *sortOp) Close() error {
	err := s.in.Close()
	for _, rf := range s.runs {
		if e := rf.Close(); e != nil && err == nil {
			err = e
		}
	}
	return err
}
