package exec

import (
	"fmt"

	"minidb/internal/keys"
	"minidb/internal/plan"
	"minidb/internal/query"
	"minidb/internal/table"
)

// scan.go là ba lá của cây: seq scan, index scan, index-only scan.
//
// Ba đường này chính là ba kế hoạch mà phase 7 đã đo điểm hoà vốn cho. Cái
// phase 8 thêm vào chỉ là hình dạng KÉO và việc dựng khoảng quét từ một điều
// kiện SQL thay vì từ một struct viết tay.

// keyRange dựng cặp khóa thô của một Span.
//
// Đây là chỗ khoảng GIÁ TRỊ của planner thành khoảng BYTE, và là chỗ phép BẰNG
// hiện ra là một ca riêng: [v, v) rỗng, [v, v+1) chỉ tính được cho số, nên
// đúng đắn duy nhất là [Encode(v), PrefixEnd(Encode(v))) — một phép toán trên
// byte, không có tương ứng ở mức giá trị. Xem table.IterIndexKeys.
func indexKeyRange(ix *table.Index, sp plan.Span) (lo, hi []byte) {
	prefix := ix.IndexPrefix()
	lo = append([]byte(nil), prefix...)
	if !sp.Lo.IsNull() {
		lo = ix.SeekKey(nil, []keys.Value{sp.Lo})
	}
	hi = keys.PrefixEnd(prefix)
	if !sp.Hi.IsNull() {
		hi = ix.SeekKey(nil, []keys.Value{sp.Hi})
		if sp.HiIncl {
			hi = keys.PrefixEnd(hi)
		}
	}
	return lo, hi
}

func rowKeyRange(sc *table.Schema, sp plan.Span) (lo, hi []byte) {
	prefix := sc.RowPrefix()
	lo = append([]byte(nil), prefix...)
	hi = keys.PrefixEnd(prefix)
	if sp.Col < 0 || len(sc.PK) == 0 || sc.PK[0] != sp.Col {
		return lo, hi
	}
	ord := sc.PKOrder()
	if !sp.Lo.IsNull() {
		lo = keys.Encode(append([]byte(nil), prefix...), []keys.Value{sp.Lo}, ord)
	}
	if !sp.Hi.IsNull() {
		hi = keys.Encode(append([]byte(nil), prefix...), []keys.Value{sp.Hi}, ord)
		if sp.HiIncl {
			hi = keys.PrefixEnd(hi)
		}
	}
	return lo, hi
}

// emptyOp không đọc gì. Nó tồn tại vì "kế hoạch nói 0 hàng" và "không chạm
// vào cây" là hai chuyện khác nhau, và chỉ chuyện thứ hai mới tiết kiệm.
type emptyOp struct{}

func (emptyOp) Next() (bool, error) { return false, nil }
func (emptyOp) Close() error        { return nil }

func (r *Runner) newScan(p *plan.PScan) (Op, error) {
	if p.Empty {
		return emptyOp{}, nil
	}
	base := r.Rels[p.Rel].Base
	switch p.Kind {
	case query.SeqScan:
		lo, hi := rowKeyRange(p.Sc, p.Span)
		return &seqScanOp{r: r, p: p, base: base,
			it: r.Tx.IterRowsKeys(p.Sc, lo, hi)}, nil
	case query.IndexScan:
		lo, hi := indexKeyRange(p.Index, p.Span)
		return &indexScanOp{r: r, p: p, base: base,
			it: r.Tx.IterIndexKeys(p.Index, lo, hi)}, nil
	case query.IndexOnlyScan:
		lo, hi := indexKeyRange(p.Index, p.Span)
		return &indexOnlyOp{r: r, p: p, base: base,
			it: r.Tx.IterIndexKeys(p.Index, lo, hi)}, nil
	}
	return nil, fmt.Errorf("exec: kiểu scan lạ %v", p.Kind)
}

// seqScanOp quét bảng theo thứ tự primary key.
//
// Chú ý: điều kiện trong p.Residual được kiểm SAU khi hàng đã đọc và giải mã.
// Đó chính là cái mà một index tránh được, và là lý do "predicate pushdown"
// vào access path (thành khoảng quét) khác hẳn "predicate pushdown" xuống một
// Filter sát bảng — cái sau vẫn phải đọc hàng.
type seqScanOp struct {
	r    *Runner
	p    *plan.PScan
	base int
	it   *table.RowIter
}

func (s *seqScanOp) Next() (bool, error) {
	for {
		if !s.it.Next() {
			return false, s.it.Err()
		}
		row := s.it.Row()
		copy(s.r.Row[s.base:], row)
		ok, err := evalAll(s.r, s.p.Residual)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
}

func (s *seqScanOp) Close() error { return s.it.Close() }

// indexScanOp quét index rồi tra bảng theo primary key cho mỗi mục khớp.
//
// Phép tra bảng LỒNG trong lần duyệt index là chỗ nợ P6-4 phải trả trước khi
// phase 7 tồn tại được; ở đây nó còn là chỗ phase 8 dựa vào lần thứ hai, vì
// nested loop join làm đúng cùng một hình.
type indexScanOp struct {
	r    *Runner
	p    *plan.PScan
	base int
	it   *table.IndexIter
}

func (s *indexScanOp) Next() (bool, error) {
	for {
		if !s.it.Next() {
			return false, s.it.Err()
		}
		row, ok, err := s.r.Tx.Get(s.p.Sc, s.it.PK())
		if err != nil {
			return false, err
		}
		if !ok {
			// Mục index trỏ tới hàng không còn = bất biến tầng bảng đã vỡ.
			// Không im lặng bỏ qua: im lặng ở đây là một truy vấn trả về ít
			// hàng hơn sự thật mà không ai biết.
			return false, fmt.Errorf("index %s: mục trỏ tới hàng %v không tồn tại",
				s.p.Index.Name, s.it.PK())
		}
		copy(s.r.Row[s.base:], row)
		pass, err := evalAll(s.r, s.p.Residual)
		if err != nil {
			return false, err
		}
		if pass {
			return true, nil
		}
	}
}

func (s *indexScanOp) Close() error { return s.it.Close() }

// indexOnlyOp dựng hàng TỪ MỤC INDEX, không tra bảng.
//
// Nó chỉ điền được những cột index phủ. Các slot còn lại của quan hệ này được
// đặt NULL thay vì để nguyên giá trị của hàng TRƯỚC: nếu để nguyên thì một
// sai sót trong tính toán "cột nào cần" (plan.neededCols) sẽ cho ra kết quả
// SAI mà trông hợp lệ — dữ liệu của hàng khác. NULL thì ít nhất là nhìn thấy
// được. Đây là chọn "sai lộ" thay vì "sai kín", cùng một nguyên tắc với việc
// projectIndex của phase 7 trả lỗi thay vì trả NULL.
type indexOnlyOp struct {
	r    *Runner
	p    *plan.PScan
	base int
	it   *table.IndexIter
}

func (s *indexOnlyOp) Next() (bool, error) {
	sc, ix := s.p.Sc, s.p.Index
	for {
		if !s.it.Next() {
			return false, s.it.Err()
		}
		for i := range sc.Cols {
			s.r.Row[s.base+i] = keys.Null()
		}
		vals, pk := s.it.Vals(), s.it.PK()
		for j, ic := range ix.Cols {
			s.r.Row[s.base+ic] = vals[j]
		}
		for j, pc := range sc.PK {
			s.r.Row[s.base+pc] = pk[j]
		}
		// Kiểm rằng mọi cột CẦN đều đã điền. Kế hoạch nói index phủ đủ; đây
		// là chỗ khẳng định lại điều đó lúc chạy, vì cái giá của việc kế hoạch
		// sai ở đây là một kết quả sai chứ không phải một kế hoạch chậm.
		for _, need := range s.p.Need {
			if !covers(sc, ix, need) {
				return false, fmt.Errorf("index-only scan trên %s: index %s không phủ cột %q",
					sc.Name, ix.Name, sc.Cols[need].Name)
			}
		}
		pass, err := evalAll(s.r, s.p.Residual)
		if err != nil {
			return false, err
		}
		if pass {
			return true, nil
		}
	}
}

func (s *indexOnlyOp) Close() error { return s.it.Close() }

func covers(sc *table.Schema, ix *table.Index, col int) bool {
	for _, ic := range ix.Cols {
		if ic == col {
			return true
		}
	}
	for _, pc := range sc.PK {
		if pc == col {
			return true
		}
	}
	return false
}

func evalAll(r *Runner, preds []plan.Expr) (bool, error) {
	for _, p := range preds {
		v, err := p.Eval(r.Row)
		if err != nil {
			return false, err
		}
		if !plan.True(v) {
			return false, nil
		}
	}
	return true, nil
}
