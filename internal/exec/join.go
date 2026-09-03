package exec

import (
	"hash/fnv"

	"minidb/internal/keys"
	"minidb/internal/plan"
)

// join.go cài hai thuật toán join, và cả phase 8 tồn tại để so hai cái này.
//
// Chúng khác nhau ở đúng một điều: **cái gì được đọc nhiều lần.**
//
//	nested loop : vế TRONG đọc lại |L| lần, bộ nhớ O(1)
//	hash join   : mỗi vế đọc MỘT lần, bộ nhớ O(|build|)
//
// Nên câu "hash join nhanh hơn" chỉ đúng khi bộ nhớ đủ. Hết bộ nhớ thì hash
// join phải chia vế build thành từng phần, ghi ra đĩa, đọc lại — và lúc ấy nó
// đọc mỗi vế ba lần (đọc, ghi, đọc lại) chứ không phải một. Đó là chỗ hai
// đường có thể đổi vai lần thứ hai, và là lý do memory budget không phải một
// tham số tinh chỉnh mà là một tham số ĐỔI THUẬT TOÁN.

// ---------- nested loop ----------

// nestLoopOp: với mỗi hàng vế ngoài, DỰNG LẠI vế trong từ đầu.
//
// Dựng lại chứ không "quay đầu con trỏ": một cây con có thể chứa Sort hoặc một
// hash join khác, và những thứ đó không có phép quay đầu. Dựng lại thì luôn
// đúng, và cái giá của nó chính là con số mà InnerScans đếm — nó phải HIỆN RA
// trong thống kê, vì đó là toàn bộ nhược điểm của thuật toán này.
type nestLoopOp struct {
	r     *Runner
	outer Op
	inner Op
	right plan.PNode
	on    []plan.Expr
	begun bool
}

func (r *Runner) newNestLoop(p *plan.PNestLoop) (Op, error) {
	l, err := r.Build(p.L)
	if err != nil {
		return nil, err
	}
	return &nestLoopOp{r: r, outer: l, right: p.R, on: p.On}, nil
}

func (n *nestLoopOp) Next() (bool, error) {
	for {
		if n.inner == nil {
			ok, err := n.outer.Next()
			if err != nil || !ok {
				return false, err
			}
			in, err := n.r.Build(n.right)
			if err != nil {
				return false, err
			}
			n.inner = in
			n.r.St.InnerScans++
		}
		ok, err := n.inner.Next()
		if err != nil {
			return false, err
		}
		if !ok {
			if err := n.inner.Close(); err != nil {
				return false, err
			}
			n.inner = nil
			continue
		}
		pass, err := evalAll(n.r, n.on)
		if err != nil {
			return false, err
		}
		if pass {
			return true, nil
		}
	}
}

func (n *nestLoopOp) Close() error {
	var err error
	if n.inner != nil {
		err = n.inner.Close()
	}
	if e := n.outer.Close(); e != nil && err == nil {
		err = e
	}
	return err
}

// ---------- hash join ----------

// numParts là số phần khi phải tràn ra đĩa.
//
// Cố định 32, không tính theo dữ liệu. Bản thật (Grace hash join) chia sao cho
// mỗi phần vừa bộ nhớ, và nếu một phần vẫn không vừa thì chia ĐỆ QUY phần ấy.
// Ở đây không đệ quy: một phần quá to thì vẫn nạp vào RAM và vượt hạn mức.
// Ghi rõ ra thay vì im lặng, vì nó là một điều kiện để kết luận của cmd/sqllab
// còn đúng — với dữ liệu lệch nặng trên khóa join, hạn mức sẽ bị phá và bảng
// số nói dối. Xem nợ P8.
const numParts = 32

type storedRow []keys.Value

type hashJoinOp struct {
	r      *Runner
	build  Op
	probe  Op
	keys   []plan.JoinKey
	extra  []plan.Expr
	budget int

	bSlots []int // slot của vế build trong tuple phẳng
	pSlots []int
	bKey   []int // slot dùng làm khoá, phía build
	pKey   []int

	opened bool
	// đường trong RAM
	tbl map[string][]storedRow
	// đường tràn đĩa
	bParts, pParts []*rowFile
	part           int
	partTbl        map[string][]storedRow

	// trạng thái đang trả các hàng khớp của một hàng probe
	match []storedRow
	mi    int

	spilled bool
	done    bool
	kbuf    []byte
}

func (r *Runner) newHashJoin(p *plan.PHashJoin) (Op, error) {
	b, err := r.Build(p.Build)
	if err != nil {
		return nil, err
	}
	pr, err := r.Build(p.Probe)
	if err != nil {
		return nil, err
	}
	h := &hashJoinOp{r: r, build: b, probe: pr, keys: p.Keys, extra: p.Extra,
		budget: p.Budget}
	if r.Budget > 0 {
		h.budget = r.Budget
	}
	if h.budget < 1 {
		h.budget = 1
	}
	h.bSlots = r.slotsOf(p.Build)
	h.pSlots = r.slotsOf(p.Probe)
	// Mỗi cặp khoá có một đầu ở build và một đầu ở probe. Không giả định đầu
	// nào là đầu nào: planner có thể đã đổi vai build/probe theo ước lượng.
	inB := map[int]bool{}
	for _, s := range h.bSlots {
		inB[s] = true
	}
	for _, k := range p.Keys {
		if inB[k.L] {
			h.bKey = append(h.bKey, k.L)
			h.pKey = append(h.pKey, k.R)
		} else {
			h.bKey = append(h.bKey, k.R)
			h.pKey = append(h.pKey, k.L)
		}
	}
	return h, nil
}

// slotsOf là những slot mà một cây con SỞ HỮU.
func (r *Runner) slotsOf(p plan.PNode) []int {
	var rels []int
	collectRels(p, &rels)
	var out []int
	for _, rel := range rels {
		base := r.Rels[rel].Base
		for i := range r.Rels[rel].Sc.Cols {
			out = append(out, base+i)
		}
	}
	return out
}

func collectRels(p plan.PNode, out *[]int) {
	switch x := p.(type) {
	case *plan.PScan:
		*out = append(*out, x.Rel)
	case *plan.PFilter:
		collectRels(x.In, out)
	case *plan.PProject:
		collectRels(x.In, out)
	case *plan.PLimit:
		collectRels(x.In, out)
	case *plan.PSort:
		collectRels(x.In, out)
	case *plan.PNestLoop:
		collectRels(x.L, out)
		collectRels(x.R, out)
	case *plan.PHashJoin:
		collectRels(x.Build, out)
		collectRels(x.Probe, out)
	}
}

// keyBytes mã hoá khoá join của hàng hiện tại theo danh sách slot cho trước.
//
// Dùng cùng bộ mã hoá của phase 7 chứ không dùng fmt.Sprint hay một hàm băm
// tự viết trên từng cột, vì tính TỰ PHÂN ĐỊNH mới bảo đảm hai tuple khác nhau
// không cho cùng một chuỗi byte: ("a","bc") và ("ab","c") phải khác nhau, và
// nối chuỗi thô thì chúng bằng nhau. Một va chạm như thế trong khoá join là
// một kết quả SAI, không phải chậm.
func (h *hashJoinOp) keyBytes(slots []int) []byte {
	vals := make([]keys.Value, len(slots))
	for i, s := range slots {
		vals[i] = h.r.Row[s]
	}
	h.kbuf = keys.Encode(h.kbuf[:0], vals, nil)
	return h.kbuf
}

func (h *hashJoinOp) save(slots []int) storedRow {
	out := make(storedRow, len(slots))
	for i, s := range slots {
		out[i] = h.r.Row[s]
	}
	return out
}

func (h *hashJoinOp) restore(slots []int, row storedRow) {
	for i, s := range slots {
		h.r.Row[s] = row[i]
	}
}

// buildPhase hút cạn vế build. Đây là chỗ toán tử này CHẶN.
func (h *hashJoinOp) buildPhase() error {
	h.tbl = map[string][]storedRow{}
	n := 0
	for {
		ok, err := h.build.Next()
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		key := h.keyBytes(h.bKey)
		h.r.St.HashBuild++
		if !h.spilled && n >= h.budget {
			// Vượt hạn mức: chuyển sang chia phần. Những gì đã có trong RAM
			// phải được đẩy ra file luôn, nếu không thì một nửa dữ liệu nằm
			// hai chỗ và phép dò phải xử lý hai đường — hai đường là hai chỗ
			// để sai.
			if err := h.startSpill(); err != nil {
				return err
			}
		}
		if h.spilled {
			if err := h.appendPart(h.bParts, key, h.save(h.bSlots)); err != nil {
				return err
			}
			continue
		}
		h.tbl[string(key)] = append(h.tbl[string(key)], h.save(h.bSlots))
		n++
	}
	return nil
}

func (h *hashJoinOp) startSpill() error {
	h.spilled = true
	h.bParts = make([]*rowFile, numParts)
	h.pParts = make([]*rowFile, numParts)
	for k, rows := range h.tbl {
		for _, row := range rows {
			if err := h.appendPart(h.bParts, []byte(k), row); err != nil {
				return err
			}
		}
	}
	h.tbl = nil
	return nil
}

func (h *hashJoinOp) appendPart(parts []*rowFile, key []byte, row storedRow) error {
	p := partOf(key)
	if parts[p] == nil {
		rf, err := newRowFile(h.r.TmpDir, "minidb-hash-*")
		if err != nil {
			return err
		}
		parts[p] = rf
		h.r.St.SpillFiles++
	}
	before := parts[p].size
	if err := parts[p].Append(key, row); err != nil {
		return err
	}
	h.r.St.SpillBytes += parts[p].size - before
	return nil
}

func partOf(key []byte) int {
	x := fnv.New32a()
	x.Write(key)
	return int(x.Sum32() % numParts)
}

// probeSpill hút cạn vế probe vào các phần. Phải làm SAU khi biết đã tràn:
// nếu không tràn thì vế probe được stream, không bao giờ chạm đĩa.
func (h *hashJoinOp) probeSpill() error {
	for {
		ok, err := h.probe.Next()
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		key := h.keyBytes(h.pKey)
		h.r.St.HashProbe++
		if err := h.appendPart(h.pParts, key, h.save(h.pSlots)); err != nil {
			return err
		}
	}
}

func (h *hashJoinOp) open() error {
	if err := h.buildPhase(); err != nil {
		return err
	}
	if h.spilled {
		if err := h.probeSpill(); err != nil {
			return err
		}
		h.part = -1
	}
	return nil
}

func (h *hashJoinOp) Next() (bool, error) {
	if !h.opened {
		if err := h.open(); err != nil {
			return false, err
		}
		h.opened = true
	}
	for {
		// Còn hàng khớp của lần dò trước thì trả tiếp.
		for h.mi < len(h.match) {
			row := h.match[h.mi]
			h.mi++
			h.restore(h.bSlots, row)
			pass, err := evalAll(h.r, h.extra)
			if err != nil {
				return false, err
			}
			if pass {
				return true, nil
			}
		}
		if !h.spilled {
			ok, err := h.probe.Next()
			if err != nil || !ok {
				return false, err
			}
			h.r.St.HashProbe++
			h.match = h.tbl[string(h.keyBytes(h.pKey))]
			h.mi = 0
			continue
		}
		// Đường tràn: đi từng phần một.
		ok, err := h.nextSpilled()
		if err != nil || !ok {
			return false, err
		}
	}
}

// nextSpilled lấy hàng probe kế tiếp từ các phần đã ghi ra đĩa, nạp bảng băm
// của phần tương ứng nếu cần.
func (h *hashJoinOp) nextSpilled() (bool, error) {
	for {
		if h.part >= 0 && h.pParts[h.part] != nil {
			key, row, ok, err := h.pParts[h.part].Next(len(h.pSlots))
			if err != nil {
				return false, err
			}
			if ok {
				h.r.St.SpillRereads++
				h.restore(h.pSlots, row)
				h.match = h.partTbl[string(key)]
				h.mi = 0
				return true, nil
			}
		}
		// Sang phần kế tiếp.
		h.part++
		if h.part >= numParts {
			return false, nil
		}
		h.partTbl = map[string][]storedRow{}
		if bf := h.bParts[h.part]; bf != nil {
			if err := bf.Rewind(); err != nil {
				return false, err
			}
			for {
				key, row, ok, err := bf.Next(len(h.bSlots))
				if err != nil {
					return false, err
				}
				if !ok {
					break
				}
				h.r.St.SpillRereads++
				h.partTbl[string(key)] = append(h.partTbl[string(key)],
					append(storedRow(nil), row...))
			}
		}
		if pf := h.pParts[h.part]; pf != nil {
			if err := pf.Rewind(); err != nil {
				return false, err
			}
		}
	}
}

func (h *hashJoinOp) Close() error {
	err := h.build.Close()
	if e := h.probe.Close(); e != nil && err == nil {
		err = e
	}
	for _, rf := range h.bParts {
		if rf != nil {
			if e := rf.Close(); e != nil && err == nil {
				err = e
			}
		}
	}
	for _, rf := range h.pParts {
		if rf != nil {
			if e := rf.Close(); e != nil && err == nil {
				err = e
			}
		}
	}
	return err
}
