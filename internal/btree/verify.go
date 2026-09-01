package btree

import (
	"bytes"
	"fmt"

	"minidb/internal/page"
	"minidb/internal/pager"
)

// Report là kết quả soi toàn cây. Verify() là hàm mà property test gọi sau
// mỗi loạt thao tác: nếu nó im lặng thì cây không thể tự mâu thuẫn.
type Report struct {
	Height    int // số tầng, leaf-only = 1
	Branches  int
	Leaves    int
	Keys      int
	LeafBytes int // tổng byte đang dùng ở tầng lá
	Errors    []string

	// Underfull là các node (không phải root, không nằm trên sườn cực phải)
	// dùng chưa tới nửa page. Tách riêng khỏi Errors vì có BA lý do hợp lệ để
	// nó khác rỗng: RightmostSplit, SkippedRebalance, và hạt cell (xem B7).
	Underfull []pager.PageID

	// CrossParentPairs đếm cặp leaf kề nhau nhưng khác cha — chỗ B8 buộc phải
	// làm ngơ, tức là chỗ phân mảnh được phép tồn tại vĩnh viễn.
	CrossParentPairs int

	// MergeMissed: cặp leaf kề nhau CÙNG CHA mà cả hai dưới nửa page.
	MergeMissed int

	MaxCell int     // cell lớn nhất gặp trong cây — hạt của mọi phép chia
	MinFill float64 // node non-root đặc ít nhất (0..1)
}

func (r *Report) OK() bool { return len(r.Errors) == 0 }

func (r *Report) errf(format string, a ...any) {
	r.Errors = append(r.Errors, fmt.Sprintf(format, a...))
}

// LeafFill là độ đặc trung bình của tầng lá — con số trả lời trực tiếp câu
// "vì sao UUIDv4 làm PK là thảm họa".
func (r *Report) LeafFill() float64 {
	if r.Leaves == 0 {
		return 0
	}
	return float64(r.LeafBytes) / float64(r.Leaves*page.PageSize)
}

// Verify kiểm tra mọi bất biến của B+Tree:
//
//	B1 mọi leaf cùng độ sâu           -> cây cân bằng
//	B2 khóa trong một node tăng dần   -> binary search hợp lệ
//	B3 mọi khóa của cây con nằm trong khoảng [low, high) mà cha quy định
//	B4 branch có đúng numCells+1 con
//	B5 không page nào xuất hiện hai lần (không có chu trình, không dùng chung)
//	B6 chuỗi sibling của leaf đi đúng thứ tự khóa và đủ mọi leaf
//	B7 node không phải root đặc >= (PageSize - cell lớn nhất)/2
//
// B7 KHÔNG phải "đầy >= 50%". Sách giáo khoa nói 50% vì nó ngầm giả định cell
// cùng kích thước. Chia một danh sách cell độ dài khác nhau thành hai nửa thì
// biên chia chỉ rơi được vào khoảng GIỮA hai cell: nửa nhẹ hơn hụt tối đa một
// cell. Với cell 350 byte trên page 4KB, đó là 4% page — 50% là bất khả thi,
// còn (PageSize-maxCell)/2 thì đạt được. Đo được, xem diary phase 4.
func (t *Tree) Verify() (*Report, error) {
	r := &Report{}
	type fillCheck struct {
		id      pager.PageID
		used    int
		isRoot  bool
		onRight bool
	}
	var underfullNodes []fillCheck
	seen := make(map[pager.PageID]bool)
	var chain []pager.PageID
	parentOf := make(map[pager.PageID]pager.PageID)

	var walk func(id, parent pager.PageID, depth int, low, high []byte, isRoot, onRight bool) error
	walk = func(id, parent pager.PageID, depth int, low, high []byte, isRoot, onRight bool) error {
		parentOf[id] = parent
		if seen[id] {
			r.errf("B5: page %d xuất hiện hai lần trong cây", id)
			return nil
		}
		seen[id] = true

		n, err := t.pin(id)
		if err != nil {
			return err
		}
		defer t.unpin(id, false)

		if err := n.p.Verify(); err != nil {
			r.errf("page %d: %v", id, err)
		}
		if !isRoot {
			if fillFrac := float64(n.p.Used()) / float64(page.PageSize); r.MinFill == 0 || fillFrac < r.MinFill {
				r.MinFill = fillFrac
			}
			if n.p.Used()*2 < page.PageSize && !onRight {
				r.Underfull = append(r.Underfull, id)
			}
		}
		for i := 0; i < n.numCells(); i++ {
			if sz := len(n.cell(i)) + 4; sz > r.MaxCell {
				r.MaxCell = sz
			}
		}
		underfullNodes = append(underfullNodes, fillCheck{id, n.p.Used(), isRoot, onRight})

		// B2 + B3 trên chính node này.
		for i := 0; i < n.numCells(); i++ {
			k := n.keyAt(i)
			if i > 0 && bytes.Compare(n.keyAt(i-1), k) >= 0 {
				r.errf("B2: page %d cell %d khóa %q <= cell trước %q", id, i, k, n.keyAt(i-1))
			}
			if low != nil && bytes.Compare(k, low) < 0 {
				r.errf("B3: page %d khóa %q < cận dưới %q", id, k, low)
			}
			// Với branch, khóa phân tách chính là cận trên của cây con bên
			// trái nên nó được phép bằng high; với leaf thì không.
			if high != nil {
				if cmp := bytes.Compare(k, high); cmp > 0 || (cmp == 0 && n.isLeaf()) {
					r.errf("B3: page %d khóa %q vượt cận trên %q", id, k, high)
				}
			}
		}

		if n.isLeaf() {
			r.Leaves++
			r.Keys += n.numCells()
			r.LeafBytes += n.p.Used()
			chain = append(chain, id)
			if r.Height == 0 {
				r.Height = depth
			} else if r.Height != depth {
				r.errf("B1: leaf %d ở độ sâu %d, leaf khác ở %d", id, depth, r.Height)
			}
			return nil
		}

		r.Branches++
		if n.rightmost() == 0 {
			r.errf("B4: branch %d không có con cực phải", id)
			return nil
		}
		// B4: numCells cell + 1 con cực phải.
		lo := low
		for i := 0; i < n.numCells(); i++ {
			k := n.keyAt(i)
			if err := walk(n.childAt(i), id, depth+1, lo, k, false, false); err != nil {
				return err
			}
			lo = k
		}
		return walk(n.rightmost(), id, depth+1, lo, high, false, onRight)
	}

	if err := walk(t.root, 0, 1, nil, nil, true, true); err != nil {
		return nil, err
	}

	// B6: chuỗi sibling phải đi qua đúng những leaf đó, đúng thứ tự.
	if len(chain) > 0 {
		id := chain[0]
		for k := 0; ; k++ {
			if k >= len(chain) {
				r.errf("B6: chuỗi sibling dài hơn số leaf (%d) — có chu trình", len(chain))
				break
			}
			if id != chain[k] {
				r.errf("B6: bước %d của chuỗi sibling là page %d, thứ tự khóa nói phải là %d", k, id, chain[k])
				break
			}
			n, err := t.pin(id)
			if err != nil {
				return nil, err
			}
			next := n.next()
			t.unpin(id, false)
			if next == 0 {
				if k != len(chain)-1 {
					r.errf("B6: chuỗi sibling đứt ở leaf %d, mới đi được %d/%d", id, k+1, len(chain))
				}
				break
			}
			id = next
		}
	}

	// MergeMissed đếm cặp leaf kề nhau, CÙNG CHA, cùng dưới nửa page — hợp của
	// chúng chắc chắn vừa một page nên về lý thuyết đã có thể gộp.
	//
	// Đây là SỐ ĐO, không phải bất biến. Đã thử biến nó thành bất biến (B8) và
	// nó fail ngay: rebalance chỉ chạy trên đường đi của Delete, còn split thì
	// tự nó cũng đẻ ra được node dưới nửa (midpoint cắt ở biên gần half nhất,
	// nên nửa TRÁI cũng có thể hụt). Hai node như vậy nằm cạnh nhau mà không
	// lần xóa nào chạm tới thì không ai gộp chúng. Muốn số này về 0 phải quét
	// nền hoặc rebalance cả khi chèn — cái giá không đáng ở đây, nhưng phải
	// ĐO được thì mới biết mình đang trả giá bao nhiêu.
	for k := 0; k+1 < len(chain); k++ {
		a, b := chain[k], chain[k+1]
		ua, err := t.usedOf(a)
		if err != nil {
			return nil, err
		}
		ub, err := t.usedOf(b)
		if err != nil {
			return nil, err
		}
		if ua*2 >= page.PageSize || ub*2 >= page.PageSize {
			continue
		}
		if parentOf[a] != parentOf[b] {
			// Khác cha thì rebalance KHÔNG được phép gộp: nó chỉ nhìn anh em
			// cùng cha. Giới hạn có thật của B+Tree, và là một nguồn phân mảnh
			// riêng — ranh giới giữa hai cha là chỗ hai page nửa rỗng được
			// phép nằm cạnh nhau mãi mãi.
			r.CrossParentPairs++
			continue
		}
		r.MergeMissed++
	}

	// B7 chạy CUỐI vì nó cần MaxCell của toàn cây — hạt của mọi phép chia chỉ
	// biết được sau khi đã đi hết. Sườn cực phải được miễn: RightmostSplit cố
	// ý đóng page trái đầy và để page phải gần rỗng.
	floor := page.PageSize - r.MaxCell
	for _, f := range underfullNodes {
		if f.isRoot || f.onRight {
			continue
		}
		if f.used*2 < floor {
			r.errf("B7: page %d đặc %d/%d byte, dưới sàn (PageSize-maxCell)/2 = %d (maxCell=%d)",
				f.id, f.used, page.PageSize, floor/2, r.MaxCell)
		}
	}
	return r, nil
}

// Dump vẽ cây ra chữ (cmd/btreelab dùng).
func (t *Tree) Dump(maxPerLevel int) (string, error) {
	var b []byte
	level := []pager.PageID{t.root}
	depth := 0
	for len(level) > 0 {
		var next []pager.PageID
		b = append(b, fmt.Sprintf("tầng %d (%d node):", depth, len(level))...)
		for i, id := range level {
			n, err := t.pin(id)
			if err != nil {
				return "", err
			}
			if i < maxPerLevel {
				kind := "L"
				if !n.isLeaf() {
					kind = "B"
				}
				b = append(b, fmt.Sprintf("  %s%d[%d cell, %.0f%%]", kind, id, n.numCells(), 100*n.fillRatio())...)
			}
			if !n.isLeaf() {
				for c := 0; c <= n.numCells(); c++ {
					next = append(next, n.childAt(c))
				}
			}
			t.unpin(id, false)
		}
		if len(level) > maxPerLevel {
			b = append(b, fmt.Sprintf("  ... còn %d node", len(level)-maxPerLevel)...)
		}
		b = append(b, '\n')
		level = next
		depth++
	}
	return string(b), nil
}

// usedOf trả về số byte đang dùng của một page (pin/unpin gọn cho B8).
func (t *Tree) usedOf(id pager.PageID) (int, error) {
	n, err := t.pin(id)
	if err != nil {
		return 0, err
	}
	u := n.p.Used()
	t.unpin(id, false)
	return u, nil
}
