package btree

import (
	"errors"
	"fmt"

	"minidb/internal/page"
	"minidb/internal/pager"
)

// underfull: node dùng chưa tới nửa page.
//
// Đo bằng byte chứ không phải số cell, vì với khóa biến độ dài "một nửa số
// cell" chẳng nói gì về chỗ trống. Root được miễn: một cây có 3 khóa vẫn phải
// hợp lệ.
func underfull(n node) bool { return n.p.Used()*2 < page.PageSize }

// Delete xóa key. Sau khi xóa, node có thể tụt dưới 50% -> cân bằng lại bằng
// redistribute (mượn của anh em) hoặc merge (gộp với anh em). Đây là nửa khó
// của B+Tree: insert chỉ đẩy thông tin LÊN, còn delete phải đẩy LÊN (khóa
// phân tách đổi) và XUỐNG (con biến mất) cùng lúc.
func (t *Tree) Delete(key []byte) error {
	if len(key) == 0 {
		return ErrEmptyKey
	}
	path, err := t.descend(key)
	if err != nil {
		return err
	}
	defer func() { t.release(path) }()

	leaf := &path[len(path)-1]
	i, exact := leaf.n.search(key)
	if !exact {
		return fmt.Errorf("%w: %q", ErrKeyNotFound, key)
	}
	if err := leaf.n.p.RemoveAt(page.SlotID(i)); err != nil {
		return err
	}
	leaf.dirty = true
	t.st.Deletes++
	return t.rebalance(path)
}

// rebalance đi ngược từ leaf lên root, sửa mọi node bị thiếu.
func (t *Tree) rebalance(path []crumb) error {
	for lv := len(path) - 1; lv >= 1; lv-- {
		c := &path[lv]
		if c.gone || !underfull(c.n) {
			// Node đủ đầy -> mọi tổ tiên cũng không đổi kích thước -> dừng.
			// (Chỉ merge mới làm cha mất một cell; redistribute thì không.)
			if !c.gone {
				return nil
			}
			continue
		}
		merged, err := t.fixUnderfull(path, lv)
		if err != nil {
			return err
		}
		if !merged {
			// Redistribute không làm cha nhỏ đi -> không cần đi tiếp.
			return nil
		}
	}
	return t.maybeShrinkRoot(path)
}

// fixUnderfull xử lý một node thiếu ở bậc lv. Trả về merged=true nếu đã gộp
// (khi đó cha mất một cell và có thể tới lượt cha bị thiếu).
func (t *Tree) fixUnderfull(path []crumb, lv int) (bool, error) {
	c := &path[lv]
	parent := &path[lv-1]
	j := parent.idx

	// Chọn anh em: ưu tiên bên trái, vì gộp luôn gộp về node TRÁI và khóa
	// phân tách cần sửa khi đó nằm ngay tại cell j-1 của cha.
	var sibIdx int
	if j > 0 {
		sibIdx = j - 1
	} else if j < parent.n.numCells() {
		sibIdx = j + 1
	} else {
		// Cha chỉ có đúng một con: chỉ xảy ra với root sau khi gộp, và
		// maybeShrinkRoot sẽ dọn.
		return false, nil
	}

	sibID := parent.n.childAt(sibIdx)
	sib, err := t.pin(sibID)
	if err != nil {
		return false, err
	}

	// Chuẩn hóa thành cặp (trái, phải) liền kề, và sepIdx là cell của cha giữ
	// khóa phân tách giữa hai đứa — luôn là cell mang chỉ số của đứa TRÁI.
	var left, right node
	var leftID, rightID pager.PageID
	var sepIdx int
	if sibIdx < j {
		left, leftID, right, rightID, sepIdx = sib, sibID, c.n, c.id, sibIdx
	} else {
		left, leftID, right, rightID, sepIdx = c.n, c.id, sib, sibID, j
	}

	merged, err := t.balancePair(path, lv, left, leftID, right, rightID, sepIdx)
	if err != nil {
		t.unpin(sibID, true)
		return false, err
	}

	// balancePair luôn ghi vào CẢ HAI node (gộp: vào trái; chia lại: vào cả
	// hai). Đứa nào sống sót thì phải được đánh dấu bẩn — kể cả `c`, thứ mà ở
	// tầng branch chưa hề bẩn vì lần xóa chỉ chạm tới leaf.
	if merged {
		// `right` chết. Nó có thể chính là node trong path (khi gộp về anh em
		// bên trái) — lúc đó phải rút nó khỏi path trước khi trả về freelist,
		// nếu không release() sẽ Unpin một page không còn trong pool.
		if rightID == c.id {
			c.gone = true
			t.unpin(c.id, false)
			t.unpin(sibID, true) // anh em trái sống và vừa bị ghi
		} else {
			t.unpin(sibID, false) // anh em phải chết, không cần ghi
			c.dirty = true
		}
		if err := t.freePage(rightID); err != nil {
			return false, err
		}
		return true, nil
	}
	t.unpin(sibID, true)
	c.dirty = true
	return false, nil
}

// balancePair gộp hoặc chia lại hai node anh em liền kề.
//
// Cả hai việc dùng chung một đường: đổ hết cell của hai đứa vào một danh sách
// rồi hỏi "có vừa một page không?". Vừa -> gộp; không vừa -> cắt lại giữa
// danh sách. Nhờ vậy redistribute không phải là "chuyển k cell" với k chọn
// bằng cảm tính, mà luôn cho ra hai nửa cân nhau theo byte.
func (t *Tree) balancePair(path []crumb, lv int, left node, leftID pager.PageID,
	right node, rightID pager.PageID, sepIdx int) (bool, error) {

	parent := &path[lv-1]
	isLeaf := left.isLeaf()

	t.sc.reset()
	for k := 0; k < left.numCells(); k++ {
		t.sc.add(left.cell(k))
	}
	if !isLeaf {
		// Branch: khóa phân tách đang nằm ở CHA phải tụt xuống, kèm con cực
		// phải của nửa trái làm con của nó. Quên cell này là mất trắng một
		// cây con — và mọi kiểm tra cục bộ vẫn sẽ báo xanh.
		sepKey := parent.n.keyAt(sepIdx)
		t.sc.add(encodeBranch(t.cellBuf[:0], left.rightmost(), sepKey))
	}
	for k := 0; k < right.numCells(); k++ {
		t.sc.add(right.cell(k))
	}
	cells := t.sc.cells

	total := page.HeaderSize
	for _, c := range cells {
		total += len(c) + 4
	}

	if total <= page.PageSize {
		// ---- GỘP ----
		if err := fill(left, left.p.Type(), cells); err != nil {
			return false, err
		}
		if isLeaf {
			left.setNext(right.next())
		} else {
			left.setRightmost(right.rightmost())
		}
		// Cha: bỏ cell phân tách, rồi trỏ vị trí đó về node trái. Khi phải là
		// con cực phải, RemoveAt làm numCells giảm đúng 1 nên setChildAt tại
		// sepIdx == numCells mới = đặt lại con cực phải. Một nhánh cho cả hai.
		if err := parent.n.p.RemoveAt(page.SlotID(sepIdx)); err != nil {
			return false, err
		}
		if err := parent.n.setChildAt(sepIdx, leftID); err != nil {
			return false, err
		}
		parent.dirty = true
		t.st.Merges++
		return true, nil
	}

	// ---- CHIA LẠI ----
	mid := midpoint(cells)
	var newSep []byte
	if isLeaf {
		newSep = leafKey(cells[mid])
	} else {
		newSep = branchKey(cells[mid])
	}

	// Kiểm tra tính khả thi TRƯỚC khi động vào hai node con: khóa phân tách
	// mới có thể dài hơn khóa cũ và không nhét vừa cha. Nếu đã sửa con rồi mới
	// phát hiện thì không lùi lại được.
	newParentCell := encodeBranch(t.cellBuf[:0], leftID, newSep)
	old, err := parent.n.p.Get(page.SlotID(sepIdx))
	if err != nil {
		return false, err
	}
	if len(newParentCell) > len(old) && len(newParentCell)-len(old) > parent.n.p.FreeTotal() {
		// Chịu: để node thiếu chỗ đó. Cây vẫn đúng, chỉ tốn chỗ. Đây là lý do
		// mọi B+Tree thật đều thích khóa ngắn và cố định ở internal node
		// (suffix truncation).
		t.st.SkippedRebalance++
		return false, nil
	}

	if isLeaf {
		nextID := right.next()
		if err := fill(right, page.TypeLeaf, cells[mid:]); err != nil {
			return false, err
		}
		right.setNext(nextID)
		if err := fill(left, page.TypeLeaf, cells[:mid]); err != nil {
			return false, err
		}
		left.setNext(rightID)
	} else {
		oldRight := right.rightmost()
		promoted := cells[mid]
		if err := fill(right, page.TypeBranch, cells[mid+1:]); err != nil {
			return false, err
		}
		right.setRightmost(oldRight)
		if err := fill(left, page.TypeBranch, cells[:mid]); err != nil {
			return false, err
		}
		left.setRightmost(branchChild(promoted))
	}

	if err := parent.n.p.SetAt(page.SlotID(sepIdx), newParentCell); err != nil {
		// Đã kiểm tra chỗ ở trên nên nhánh này chỉ chạy khi page phân mảnh
		// đúng kiểu Update không tự dồn được; xóa rồi chèn lại vẫn an toàn vì
		// chỗ tổng đã đủ.
		if !errors.Is(err, page.ErrPageFull) {
			return false, err
		}
		if err := parent.n.p.RemoveAt(page.SlotID(sepIdx)); err != nil {
			return false, err
		}
		if err := parent.n.p.InsertAt(page.SlotID(sepIdx), newParentCell); err != nil {
			return false, err
		}
	}
	parent.dirty = true
	t.st.Redistributes++
	return false, nil
}

// maybeShrinkRoot: root là branch mà chỉ còn đúng một con -> tầng đó vô dụng,
// con lên làm root. Cây chỉ THẤP đi ở đây, đối xứng với growRoot.
func (t *Tree) maybeShrinkRoot(path []crumb) error {
	root := &path[0]
	if root.gone || root.n.isLeaf() || root.n.numCells() > 0 {
		return nil
	}
	child := root.n.rightmost()
	oldRoot := root.id
	root.gone = true
	t.unpin(oldRoot, false)
	if err := t.freePage(oldRoot); err != nil {
		return err
	}
	t.setRoot(child)
	t.st.Shrinks++
	return nil
}
