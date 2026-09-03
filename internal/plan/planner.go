package plan

import (
	"fmt"

	"minidb/internal/keys"
	"minidb/internal/query"
	"minidb/internal/sql"
	"minidb/internal/table"
)

// planner.go là chỗ chọn: đường đi nào tới mỗi quan hệ, thuật toán nào cho
// join, và có bỏ được bước sắp xếp không.
//
// Tách khỏi physical.go (chỉ định nghĩa các node và cách in chúng) vì hai thứ
// đổi vì những lý do khác nhau: thêm một toán tử là sửa physical.go, đổi cách
// định giá là sửa file này.

// ---------- planner ----------

// Planner là bối cảnh chọn kế hoạch: catalog, thống kê, mô hình chi phí, và
// hạn mức bộ nhớ.
//
// Stats theo OID bảng, và RỖNG là trạng thái bình thường: một bảng chưa
// ANALYZE thì Stats.Rows = 0, và query.Choose với Rows = 0 luôn ra SeqScan
// (mọi chi phí đều bằng 0, không cái nào nhỏ hơn cái nào). Nên **chưa ANALYZE
// thì planner không bao giờ chọn index** — không phải một bug mà là hệ quả
// trực tiếp, và là lý do mọi database thật đều phải có autoanalyze. Ở đây để
// lộ hẳn ra: EXPLAIN in "chưa có thống kê" để người dùng thấy vì sao.
type Planner struct {
	Cat    *table.Catalog
	Stats  map[uint32]query.Stats
	Cost   query.CostModel
	Budget int // số hàng giữ trong RAM cho sort và hash join
}

// DefaultBudget: nhỏ CÓ CHỦ Ý. Một hạn mức lớn hơn mọi bảng test thì nhánh
// tràn đĩa không bao giờ chạy, và một nhánh không chạy là một nhánh không
// đúng — bài học phase 5 và phase 7.
const DefaultBudget = 4096

func NewPlanner(c *table.Catalog) *Planner {
	// query.MeasuredCost, KHÔNG phải query.DefaultCost. DefaultCost là mô
	// hình đoán mà phase 7 cố ý giữ nguyên sau khi số đo bác bỏ nó (CFetch=20
	// thay vì 2.4), để cột "planner đoán chọn sai" của idxlab còn chỗ tồn tại.
	// Dùng nó ở đây thì mọi truy vấn có chọn lọc 5-25% sẽ chọn seq scan — và
	// ba bài test của phase 8 đã đỏ đúng vì thế trước khi dòng này được sửa.
	return &Planner{Cat: c, Stats: map[uint32]query.Stats{}, Cost: query.MeasuredCost,
		Budget: DefaultBudget}
}

// Plan biến cây logical đã tối ưu thành cây vật lý.
func (pl *Planner) Plan(b *Bound, n Node) (PNode, error) {
	need := neededCols(n, len(b.Rels))
	p, err := pl.build(b, n, need, nil)
	if err != nil {
		return nil, err
	}
	return pl.elideSort(p), nil
}

// build dựng cây vật lý. req là thứ tự mà tầng TRÊN cần — nó đi xuống, khác
// mọi thứ khác trong hàm này. Trong sách optimizer, req gọi là một "physical
// property" được yêu cầu, và việc truyền nó xuống là toàn bộ cơ chế để một
// access path đắt hơn được phép thắng vì nó trả về sẵn thứ tự.
func (pl *Planner) build(b *Bound, n Node, need [][]int, req []SortKey) (PNode, error) {
	switch x := n.(type) {
	case *Scan:
		return pl.scan(b, x, need[x.Rel], req)

	case *Filter:
		// Lọc không đổi thứ tự hàng còn lại, nên yêu cầu về thứ tự đi xuyên qua.
		in, err := pl.build(b, x.In, need, req)
		if err != nil {
			return nil, err
		}
		e := in.Est()
		// Mỗi vế của AND ước lượng lọt 1/3 — con số quy ước của System R, giữ
		// nguyên vì phase 8 không đo nó. Ghi ra đây để nó là một GIẢ THUYẾT
		// nhìn thấy được, không phải một hằng số trốn trong code.
		for range x.Preds {
			e.Rows /= 3
		}
		return &PFilter{In: in, Preds: x.Preds, E: e}, nil

	case *Sort:
		// Từ đây trở xuống, thứ tự CẦN là khoá sắp của chính node này — không
		// phải req của tầng trên (một Sort xoá mọi yêu cầu thứ tự phía trên nó).
		in, err := pl.build(b, x.In, need, x.By)
		if err != nil {
			return nil, err
		}
		e := in.Est()
		// n log n, chặn dưới ở n: với n <= 1 thì log âm.
		e.Cost += e.Rows * log2(maxf(e.Rows, 2))
		if e.Rows > float64(pl.Budget) {
			// Tràn đĩa: ghi ra rồi đọc lại. Hệ số 2 là số LẦN đi qua dữ liệu,
			// không phải một con số điều chỉnh.
			e.Cost += 2 * e.Rows
		}
		return &PSort{In: in, By: x.By, Budget: pl.Budget, E: e}, nil

	case *Project:
		in, err := pl.build(b, x.In, need, req)
		if err != nil {
			return nil, err
		}
		return &PProject{In: in, Out: x.Out, E: in.Est()}, nil

	case *Limit:
		in, err := pl.build(b, x.In, need, req)
		if err != nil {
			return nil, err
		}
		e := in.Est()
		// LIMIT KHÔNG chỉ cắt số hàng ra, nó cắt cả CHI PHÍ — nhưng chỉ khi
		// cây bên dưới là streaming. Nếu dưới có Sort thì phải sắp hết mới
		// biết hàng đầu, nên chi phí không giảm. Phân biệt được hai ca ấy là
		// phân biệt được "LIMIT nhanh" với "LIMIT chỉ trả về ít".
		if e.Rows > float64(x.N) {
			frac := float64(x.N) / e.Rows
			if _, blocking := x.In.(*Sort); !blocking {
				e.Cost *= frac
			}
			e.Rows = float64(x.N)
		}
		return &PLimit{In: in, N: x.N, E: e}, nil

	case *Join:
		// Join KHÔNG truyền req xuống: nested loop giữ thứ tự vế ngoài nhưng
		// chỉ khi mỗi hàng ngoài khớp nhiều nhất một hàng trong (không chứng
		// minh được ở đây), và hash join thì không hứa gì. Nên một ORDER BY
		// trên kết quả join luôn phải sắp thật. Đây là hạn chế thật, xem nợ.
		return pl.join(b, x, need)
	}
	return nil, fmt.Errorf("plan: node logical lạ %T", n)
}

// path là MỘT đường đi khả dĩ tới một quan hệ, đã tính chi phí.
//
// Vì sao phải liệt kê thay vì gọi query.Choose của phase 7: Choose trả về đúng
// một kế hoạch, và chữ ký ấy không diễn tả nổi hai thứ mà phase 8 cần.
//
//  1. Với một engine CLUSTERED INDEX, seq scan không phải "đường không có
//     access path" — hàng nằm TRONG cây khóa chính, nên `WHERE pk < v` là một
//     khoảng quét thật, không cần index phụ nào. Choose chỉ xét c.Indexes(),
//     tức chỉ xét index PHỤ, nên nó tính một truy vấn trên pk thành quét cả
//     bảng. Đây là chỗ trực giác "heap của Postgres" sai với hình của repo này.
//  2. Một đường đi còn trả về một THỨ TỰ, và thứ tự ấy có thể xoá hẳn một
//     toán tử Sort ở trên. Giá trị của nó không nằm trong chi phí của lá, nên
//     một hàm chọn-rồi-trả-về không có chỗ nào để cân nó.
//
// Nên cái sống sót từ phase 7 là MÔ HÌNH (CostModel + Selectivity), không phải
// hàm chọn. Và cái giết Choose không phải join — mà là ORDER BY.
type path struct {
	kind     query.Kind
	ix       *table.Index
	span     Span
	residual []Expr
	rows     float64
	cost     float64
	order    []SortKey
	why      string
}

// scan liệt kê mọi đường đi tới một lá, cộng chi phí sắp xếp mà mỗi đường
// TRÁNH ĐƯỢC, rồi chọn tổng nhỏ nhất.
//
// req là thứ tự mà tầng trên cần (nil = không cần). Đưa nó XUỐNG tới lá là
// điều kiện để "bỏ ORDER BY" là một quyết định có cân đo, chứ không phải một
// phép dọn dẹp gặp may sau khi đã chọn xong.
func (pl *Planner) scan(b *Bound, s *Scan, need []int, req []SortKey) (PNode, error) {
	st := pl.Stats[s.Sc.OID]
	cands := extractSpans(s)
	n := float64(st.Rows)

	out := &PScan{Rel: s.Rel, RelRef: s.RelRef, Sc: s.Sc, Need: need,
		Span: Span{Col: -1, Lo: keys.Null(), Hi: keys.Null()}}

	// Khoảng rỗng: không phải một đường đi rẻ, mà là không có gì để đọc.
	for _, c := range cands {
		if c.span.Empty() {
			out.Kind, out.Why = query.SeqScan, "khoảng rỗng theo điều kiện — không đọc gì"
			out.Empty = true
			out.E = Est{Rows: 0, Cost: 0}
			return out, nil
		}
	}

	if st.Rows == 0 {
		out.Kind = query.SeqScan
		out.Why = "chưa ANALYZE bảng này nên planner không có thống kê — mọi chi phí bằng 0"
		out.Residual = s.Preds
		out.E = Est{Rows: 1, Cost: 1}
		out.Order = pkOrder(b, s.Rel, s.Sc)
		return out, nil
	}

	var paths []path

	// (a) quét tuần tự cả bảng.
	paths = append(paths, path{kind: query.SeqScan, span: Span{Col: -1, Lo: keys.Null(), Hi: keys.Null()},
		residual: s.Preds, rows: n, cost: n * pl.Cost.CSeq,
		order: pkOrder(b, s.Rel, s.Sc), why: "quét tuần tự cả bảng"})

	for _, c := range cands {
		sel := st.Selectivity(c.span.Col, c.span.Lo, selHi(c.span))
		rows := sel * n

		// (b) quét tuần tự CÓ KHOẢNG, khi điều kiện nằm trên cột dẫn đầu của
		// khóa chính. Hàng nằm trong cây pk nên đây là một range scan thật.
		if len(s.Sc.PK) > 0 && s.Sc.PK[0] == c.span.Col {
			paths = append(paths, path{kind: query.SeqScan, span: c.span,
				residual: c.rest, rows: rows, cost: maxf(rows, 1) * pl.Cost.CSeq,
				order: pkOrder(b, s.Rel, s.Sc),
				why:   fmt.Sprintf("khoảng trên khóa chính: quét đúng %.0f/%.0f hàng của cây pk", rows, n)})
		}

		// (c) index phụ có cột dẫn đầu là cột bị ràng buộc (luật tiền tố bên
		// trái — hệ quả của hình dạng byte, xem internal/keys).
		for _, ix := range pl.Cat.Indexes(s.Sc.OID) {
			if len(ix.Cols) == 0 || ix.Cols[0] != c.span.Col {
				continue
			}
			ord := deliveredOrder(b, s.Rel, s.Sc, query.IndexScan, ix)
			if covering(s.Sc, ix, need) {
				paths = append(paths, path{kind: query.IndexOnlyScan, ix: ix, span: c.span,
					residual: c.rest, rows: rows, cost: rows * pl.Cost.CIndex, order: ord,
					why: "index phủ đủ cột cần, không phải tra bảng"})
				continue
			}
			paths = append(paths, path{kind: query.IndexScan, ix: ix, span: c.span,
				residual: c.rest, rows: rows, cost: rows * (pl.Cost.CIndex + pl.Cost.CFetch),
				order: ord, why: "quét index rồi tra bảng"})
		}
	}

	// (d) index dùng CHỈ ĐỂ SẮP: không có điều kiện nào đẩy vào nó, nhưng thứ
	// tự nó trả về xoá được toán tử Sort ở trên. Postgres gọi ca này là một
	// full index scan, và nó chỉ có nghĩa khi có ORDER BY (hoặc LIMIT + ORDER
	// BY, chỗ nó thắng đậm nhất).
	if len(req) > 0 {
		for _, ix := range pl.Cat.Indexes(s.Sc.OID) {
			ord := deliveredOrder(b, s.Rel, s.Sc, query.IndexScan, ix)
			if !prefixMatches(req, ord) {
				continue
			}
			kind, cost, why := query.IndexScan, n*(pl.Cost.CIndex+pl.Cost.CFetch),
				"quét cả index để lấy THỨ TỰ, tránh được bước sắp xếp"
			if covering(s.Sc, ix, need) {
				kind, cost = query.IndexOnlyScan, n*pl.Cost.CIndex
				why = "index phủ đủ cột VÀ cho luôn thứ tự — không tra bảng, không sắp xếp"
			}
			paths = append(paths, path{kind: kind, ix: ix,
				span:     Span{Col: -1, Lo: keys.Null(), Hi: keys.Null()},
				residual: s.Preds, rows: n, cost: cost, order: ord, why: why})
		}
	}

	// Chọn theo TỔNG: chi phí đọc cộng chi phí sắp xếp mà đường ấy không
	// tránh được. Đây là chỗ một đường đi đắt hơn vẫn thắng vì nó trả về sẵn
	// thứ tự — và là lý do req phải đi xuống tới đây.
	best, bestTotal := -1, 0.0
	for i, p := range paths {
		total := p.cost
		if len(req) > 0 && !prefixMatches(req, p.order) {
			total += sortCost(p.rows, pl.Budget)
		}
		if best < 0 || total < bestTotal {
			best, bestTotal = i, total
		}
	}
	p := paths[best]
	out.Kind, out.Index, out.Span, out.Residual = p.kind, p.ix, p.span, p.residual
	out.Order, out.Why = p.order, p.why
	out.E = Est{Rows: p.rows, Cost: p.cost}
	// Một khoảng đã thành khoảng QUÉT thì không kiểm lại từng hàng — đó chính
	// là chỗ access path tiết kiệm. Nhưng chỉ đúng khi khoảng thật sự được
	// dùng: với đường (d) thì span.Col = -1 và mọi điều kiện vẫn phải kiểm.
	if len(paths) > 1 && best == 0 {
		out.Why = fmt.Sprintf("%s (có %d đường khác, đều đắt hơn)", p.why, len(paths)-1)
	}
	return out, nil
}

// selHi là chặn trên dùng cho việc ƯỚC LƯỢNG. Phép bằng phải quy thành
// [v, v+1) vì Stats.Selectivity làm việc trên khoảng nửa mở; với kiểu không
// đếm được thì trả Null và Selectivity rơi về 1/Distinct.
func selHi(sp Span) keys.Value {
	if sp.Eq {
		return nextAfter(sp.Lo)
	}
	return sp.Hi
}

// sortCost phải dùng ĐÚNG công thức mà node Sort dùng, nếu không thì phép so
// ở trên là so hai đơn vị khác nhau — và một planner so sai đơn vị thì mọi
// quyết định của nó là ngẫu nhiên.
func sortCost(rows float64, budget int) float64 {
	c := rows * log2(maxf(rows, 2))
	if rows > float64(budget) {
		c += 2 * rows
	}
	return c
}

func covering(sc *table.Schema, ix *table.Index, need []int) bool {
	for _, c := range need {
		if !inIndex(sc, ix, c) {
			return false
		}
	}
	return true
}

func inIndex(sc *table.Schema, ix *table.Index, col int) bool {
	for _, ic := range ix.Cols {
		if ic == col {
			return true
		}
	}
	for _, pc := range sc.PK {
		if pc == col { // pk luôn có trong mục index
			return true
		}
	}
	return false
}

// join chọn thuật toán. Hai ứng viên, cùng một công thức chi phí đã ghi ở
// PNestLoop/PHashJoin, và cái rẻ hơn thắng.
func (pl *Planner) join(b *Bound, j *Join, need [][]int) (PNode, error) {
	l, err := pl.build(b, j.L, need, nil)
	if err != nil {
		return nil, err
	}
	r, err := pl.build(b, j.R, need, nil)
	if err != nil {
		return nil, err
	}
	le, re := l.Est(), r.Est()

	var jkeys []JoinKey
	var extra []Expr
	for _, e := range j.On {
		if k, ok := joinKeyOf(e); ok {
			jkeys = append(jkeys, k)
		} else {
			extra = append(extra, e)
		}
	}

	// Số hàng ra: |L|·|R| / max(distinct) — ước lượng kinh điển của System R
	// cho equi-join. Không có phép bằng nào thì đó là tích Descartes.
	rows := le.Rows * re.Rows
	if len(jkeys) > 0 {
		d := maxf(le.Rows, re.Rows)
		rows = le.Rows * re.Rows / maxf(d, 1)
	}

	nlCost := le.Cost + le.Rows*maxf(re.Cost, 1)
	nl := &PNestLoop{L: l, R: r, On: j.On, E: Est{Rows: rows, Cost: nlCost}}
	if len(jkeys) == 0 {
		return nl, nil // không băm được phép không-bằng
	}

	// Vế BUILD là vế ước lượng ÍT hàng hơn. Đây là quyết định quan trọng nhất
	// của một hash join, và nó phụ thuộc hoàn toàn vào ước lượng — chọn sai vế
	// thì bảng băm to gấp nhiều lần và tràn đĩa vô ích. Phase 7 đã đo rằng sai
	// số ước lượng tới 97x là chuyện thường trên cột lệch, nên đây chính là
	// chỗ một kế hoạch tồi được sinh ra.
	build, probe := l, r
	if re.Rows < le.Rows {
		build, probe = r, l
	}
	be, pe := build.Est(), probe.Est()
	hjCost := be.Cost + pe.Cost + be.Rows + pe.Rows
	if be.Rows > float64(pl.Budget) {
		hjCost += 2 * (be.Rows + pe.Rows) // tràn: ghi ra rồi đọc lại cả hai vế
	}
	hj := &PHashJoin{Build: build, Probe: probe, Keys: jkeys, Extra: extra,
		Budget: pl.Budget, E: Est{Rows: rows, Cost: hjCost}}
	if nlCost <= hjCost {
		return nl, nil
	}
	return hj, nil
}

// elideSort bỏ PSort nếu cây bên dưới ĐÃ trả về đúng thứ tự cần.
//
// Đây là nợ P7-9, và nó là một quyết định VẬT LÝ: cùng một cây logical, bỏ
// được hay không phụ thuộc access path đã chọn. Cụ thể: một index scan trả về
// theo thứ tự (cột index..., pk...), một seq scan trả về theo thứ tự pk. Nếu
// khoá sắp là một TIỀN TỐ của thứ tự ấy, cùng chiều, thì bước sắp xếp là việc
// vô ích.
//
// Chỉ bỏ khi cây dưới Sort không có toán tử nào PHÁ thứ tự. Hash join phá
// (thứ tự ra theo thứ tự probe... nhưng chỉ khi mọi hàng build khớp, nên
// không hứa được gì); nested loop giữ thứ tự của vế NGOÀI. Ở đây chỉ nhận ca
// an toàn nhất — Sort ngay trên một PScan, có thể qua PFilter và PProject —
// và ghi rõ vì sao không đi xa hơn.
func (pl *Planner) elideSort(p PNode) PNode {
	switch x := p.(type) {
	case *PLimit:
		x.In = pl.elideSort(x.In)
		return x
	case *PProject:
		x.In = pl.elideSort(x.In)
		return x
	case *PSort:
		if ord, ok := orderOf(x.In); ok && prefixMatches(x.By, ord) {
			return x.In
		}
		return x
	}
	return p
}

// orderOf là thứ tự mà một cây con HỨA trả về. Chỉ đi qua những toán tử giữ
// nguyên thứ tự.
func orderOf(p PNode) ([]SortKey, bool) {
	switch x := p.(type) {
	case *PScan:
		return x.Order, len(x.Order) > 0
	case *PFilter:
		return orderOf(x.In) // lọc bỏ hàng, không đổi thứ tự hàng còn lại
	case *PNestLoop:
		// Giữ thứ tự vế NGOÀI, nhưng chỉ khi mỗi hàng ngoài khớp nhiều nhất
		// một hàng trong — không chứng minh được ở đây, nên không nhận.
		return nil, false
	}
	return nil, false
}

func prefixMatches(want, got []SortKey) bool {
	if len(want) > len(got) {
		return false
	}
	for i := range want {
		if want[i].Slot < 0 || want[i].Slot != got[i].Slot || want[i].Desc != got[i].Desc {
			return false
		}
	}
	return true
}

// deliveredOrder là thứ tự mà một access path trả về.
func deliveredOrder(b *Bound, rel int, sc *table.Schema, k query.Kind, ix *table.Index) []SortKey {
	if k == query.SeqScan || ix == nil {
		return pkOrder(b, rel, sc)
	}
	base := b.Rels[rel].Base
	var out []SortKey
	for i, c := range ix.Cols {
		desc := i < len(ix.Desc) && ix.Desc[i]
		out = append(out, SortKey{Slot: base + c, Rel: rel, Idx: c, Desc: desc,
			E: &ColExpr{Slot: base + c, T: sc.Cols[c].T, Rel: rel, Idx: c,
				Name: b.Rels[rel].Ref + "." + sc.Cols[c].Name}})
	}
	// Sau các cột index là pk — với index non-unique thì pk NẰM TRONG KHÓA nên
	// thứ tự ấy là thật; với index unique thì pk nằm ở value, nên không hứa
	// được. Hình dạng khóa lại quyết định một tính chất của planner.
	if !ix.Unique {
		out = append(out, pkOrder(b, rel, sc)...)
	}
	return out
}

func pkOrder(b *Bound, rel int, sc *table.Schema) []SortKey {
	base := b.Rels[rel].Base
	var out []SortKey
	for _, c := range sc.PK {
		out = append(out, SortKey{Slot: base + c, Rel: rel, Idx: c, Desc: sc.Cols[c].Desc,
			E: &ColExpr{Slot: base + c, T: sc.Cols[c].T, Rel: rel, Idx: c,
				Name: b.Rels[rel].Ref + "." + sc.Cols[c].Name}})
	}
	return out
}

// ---------- trích khoảng từ điều kiện ----------

// candidate là một khoảng trên MỘT cột, cùng phần điều kiện còn lại.
type candidate struct {
	span Span
	rest []Expr
}

// extractSpans tìm MỌI cột có thể cho một khoảng, không chọn sẵn cột nào.
//
// Bản đầu của hàm này trả về đúng một khoảng và chọn cột có NHIỀU điều kiện
// nhất. Nó sai, và sai theo cách chỉ thấy được bằng số đo: với
// `WHERE kind = 3 AND id < 20000` thì hai cột đều có một điều kiện, hòa, và
// luật phá hòa "cột nhỏ nhất" chọn id — tức là chọn cái khoảng bao trùm cả
// bảng và bỏ cái khoảng lọc còn 0.5%. Số lượng điều kiện không nói gì về ĐỘ
// CHỌN LỌC, mà chỉ độ chọn lọc mới quyết định. Nên việc chọn phải để cho chỗ
// có thống kê làm, tức là scan().
func extractSpans(s *Scan) []candidate {
	type acc struct {
		lo, hi keys.Value
		hiIncl bool
		eq     bool
		used   map[int]bool
		keep   bool // có điều kiện không diễn tả hết được bằng khoảng
	}
	byCol := map[int]*acc{}
	for i, p := range s.Preds {
		c, ok := p.(*CmpExpr)
		if !ok {
			continue
		}
		col, isCol := c.L.(*ColExpr)
		lit, isLit := c.R.(*LitExpr)
		if !isCol || !isLit || c.Op == sql.OpNe || lit.V.IsNull() {
			continue // <> không cho khoảng liên tục; NULL thì mọi so sánh là NULL
		}
		a := byCol[col.Idx]
		if a == nil {
			a = &acc{lo: keys.Null(), hi: keys.Null(), used: map[int]bool{}}
			byCol[col.Idx] = a
		}
		switch c.Op {
		case sql.OpEq:
			// `=` phải GIAO vào bộ tích luỹ, không được GHI ĐÈ nó. Bản đầu viết
			// thẳng `a.lo, a.hi = lit.V, lit.V` và thế là `x = 3 AND x = 9` cho
			// ra khoảng [9,9] — không rỗng — nên `Span.Empty` không nhận ra, còn
			// `x = 3` tụt xuống làm residual: kết quả vẫn ĐÚNG (0 hàng) nhưng
			// quét cả bảng. Hai nhánh `>` và `<` bên dưới đã giao đúng ngay từ
			// đầu; chỉ nhánh này sai, vì phép bằng trông như một phép GÁN.
			//
			// Tìm ra khi dựng ví dụ EXPLAIN cho nhật ký, không phải khi chạy
			// test — vì không một bài test nào khẳng định về SỐ HÀNG ĐỌC của
			// một câu vô nghiệm.
			if a.lo.IsNull() || keys.Compare(lit.V, a.lo) > 0 {
				a.lo = lit.V
			}
			// Giao chặn trên với (v, đóng): chỉ hạ khi v THẤP HƠN chặn đang có.
			// Bằng đúng thì giữ nguyên tính đóng/mở đang có — nhờ vậy
			// `x < 9 AND x = 9` ra [9,9) và Span.Empty nhận ra là rỗng.
			if a.hi.IsNull() || keys.Compare(lit.V, a.hi) < 0 {
				a.hi, a.hiIncl = lit.V, true
			}
			a.eq = true
		case sql.OpGe:
			if a.lo.IsNull() || keys.Compare(lit.V, a.lo) > 0 {
				a.lo = lit.V
			}
		case sql.OpGt:
			// `x > v` trên số nguyên là `x >= v+1`. Trên kiểu không đếm được
			// (chuỗi) thì không nâng được, nên giữ v làm chặn dưới rồi để
			// residual kiểm lại — thà quét thừa một khóa còn hơn bỏ sót.
			if nv, ok := nextOf(lit.V); ok {
				if a.lo.IsNull() || keys.Compare(nv, a.lo) > 0 {
					a.lo = nv
				}
			} else {
				if a.lo.IsNull() || keys.Compare(lit.V, a.lo) > 0 {
					a.lo = lit.V
				}
				a.keep = true
			}
		case sql.OpLt:
			if a.hi.IsNull() || keys.Compare(lit.V, a.hi) < 0 {
				a.hi, a.hiIncl = lit.V, false
			}
		case sql.OpLe:
			if a.hi.IsNull() || keys.Compare(lit.V, a.hi) < 0 ||
				(keys.Compare(lit.V, a.hi) == 0 && !a.hiIncl) {
				a.hi, a.hiIncl = lit.V, true
			}
		}
		a.used[i] = true
	}

	cols := make([]int, 0, len(byCol))
	for col := range byCol {
		cols = append(cols, col)
	}
	sortInts(cols) // thứ tự tiền định: cùng câu phải ra cùng kế hoạch

	out := make([]candidate, 0, len(cols))
	for _, col := range cols {
		a := byCol[col]
		var rest []Expr
		for i, p := range s.Preds {
			if a.keep || !a.used[i] {
				rest = append(rest, p)
			}
		}
		out = append(out, candidate{
			span: Span{Col: col, Lo: a.lo, Hi: a.hi, HiIncl: a.hiIncl, Eq: a.eq},
			rest: rest,
		})
	}
	return out
}

// joinKeyOf nhận ra `cột = cột` giữa hai quan hệ khác nhau.
func joinKeyOf(e Expr) (JoinKey, bool) {
	c, ok := e.(*CmpExpr)
	if !ok || c.Op != sql.OpEq {
		return JoinKey{}, false
	}
	l, lok := c.L.(*ColExpr)
	r, rok := c.R.(*ColExpr)
	if !lok || !rok || l.Rel == r.Rel || l.T != r.T {
		return JoinKey{}, false
	}
	if l.Rel > r.Rel {
		l, r = r, l
	}
	return JoinKey{L: l.Slot, R: r.Slot, T: l.T}, true
}

// nextOf là giá trị kế tiếp của một kiểu ĐẾM ĐƯỢC. Chuỗi không đếm được.
func nextOf(v keys.Value) (keys.Value, bool) {
	switch v.T {
	case keys.TypeInt:
		if v.I == 1<<63-1 {
			return v, false
		}
		return keys.Int(v.I + 1), true
	case keys.TypeUint:
		if v.U == ^uint64(0) {
			return v, false
		}
		return keys.Uint(v.U + 1), true
	}
	return v, false
}

// nextAfter dùng cho ước lượng độ chọn lọc của phép bằng: khoảng [v, v+1).
func nextAfter(v keys.Value) keys.Value {
	if n, ok := nextOf(v); ok {
		return n
	}
	return keys.Null()
}

// neededCols là những cột của mỗi quan hệ mà tầng TRÊN cần tới.
//
// Nó quyết định index có PHỦ được truy vấn hay không, tức quyết định giữa
// IndexScan và IndexOnlyScan. Phải gom từ cả Project, Sort, Filter và On —
// bỏ sót một chỗ nào thì planner tưởng index phủ đủ, rồi exec đọc một cột
// không có trong index và trả NULL. Đó là lý do projectIndex của phase 7 TRẢ
// LỖI chứ không trả NULL khi thiếu cột.
func neededCols(n Node, nrels int) [][]int {
	sets := make([]map[int]bool, nrels)
	for i := range sets {
		sets[i] = map[int]bool{}
	}
	var walkE func(Expr)
	walkE = func(e Expr) {
		switch x := e.(type) {
		case *ColExpr:
			sets[x.Rel][x.Idx] = true
		case *CmpExpr:
			walkE(x.L)
			walkE(x.R)
		case *LogicExpr:
			walkE(x.L)
			walkE(x.R)
		}
	}
	var walk func(Node)
	walk = func(n Node) {
		switch x := n.(type) {
		case *Scan:
			for _, p := range x.Preds {
				walkE(p)
			}
		case *Filter:
			for _, p := range x.Preds {
				walkE(p)
			}
			walk(x.In)
		case *Join:
			for _, p := range x.On {
				walkE(p)
			}
			walk(x.L)
			walk(x.R)
		case *Sort:
			for _, k := range x.By {
				walkE(k.E)
			}
			walk(x.In)
		case *Project:
			for _, o := range x.Out {
				walkE(o.E)
			}
			walk(x.In)
		case *Limit:
			walk(x.In)
		}
	}
	walk(n)
	out := make([][]int, nrels)
	for i, s := range sets {
		for c := range s {
			out[i] = append(out[i], c)
		}
		sortInts(out[i])
	}
	return out
}

func sortInts(a []int) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}

func maxf(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func log2(x float64) float64 {
	n := 0.0
	for x > 1 {
		x /= 2
		n++
	}
	return n
}

// JoinKeysOf tách một điều kiện ON thành các cặp `cột = cột` (băm được) và
// phần còn lại (chỉ nested loop kiểm được).
//
// Đưa ra ngoài để cmd/sqllab dựng được BẰNG TAY cả hai thuật toán trên cùng
// một cây con và so chúng. Không có nó thì lab chỉ đo được cái mà planner đã
// chọn — tức là đo chính quyết định của mình, không đo hai đường.
func JoinKeysOf(on []Expr) ([]JoinKey, []Expr) {
	var ks []JoinKey
	var rest []Expr
	for _, e := range on {
		if k, ok := joinKeyOf(e); ok {
			ks = append(ks, k)
		} else {
			rest = append(rest, e)
		}
	}
	return ks, rest
}

// EqOf dựng lại biểu thức `cột = cột` từ một JoinKey.
//
// Cần cho phép đo ngược của cmd/sqllab: biến một hash join thành nested loop
// trên cùng hai cây con. Kiểu của cột lấy từ JoinKey nên không cần tra lược đồ.
func EqOf(k JoinKey) Expr {
	return &CmpExpr{Op: sql.OpEq,
		L: &ColExpr{Slot: k.L, T: k.T, Name: fmt.Sprintf("slot%d", k.L)},
		R: &ColExpr{Slot: k.R, T: k.T, Name: fmt.Sprintf("slot%d", k.R)}}
}
