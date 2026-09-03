package plan

import "minidb/internal/sql"

// opt.go là hai phép viết lại LOGICAL -> LOGICAL: SUY RA điều kiện qua phép
// bằng của join, rồi ĐẨY ĐIỀU KIỆN XUỐNG. Cái thứ hai là luật quan trọng nhất
// trong mọi optimizer.
//
// Luật: một vế của AND chỉ dùng cột của MỘT quan hệ thì được chuyển xuống sát
// quan hệ ấy, vào Scan.Preds.
//
// Vì sao nó đúng bất kể cách chạy: inner join là phép chọn trên tích Descartes,
// và với p chỉ phụ thuộc R thì
//
//	σ_p(L ⋈ R) = L ⋈ σ_p(R)
//
// Đó là một đẳng thức về TẬP HỢP, nên nó đúng cho seq scan, index scan, nested
// loop, hash join, và cho cả những thuật toán chưa viết. Đây là ý nghĩa thật
// của chữ "logical" trong logical plan — và là lý do phép này nằm ở file này
// chứ không ở physical.go.
//
// Vì sao nó ĂN TIỀN, và ăn ở đâu thì khác nhau theo thuật toán:
//
//   - với nested loop: lọc vế TRONG trước làm vòng trong ngắn lại, mà vòng
//     trong chạy lại một lần cho mỗi hàng vế ngoài — nên tiết kiệm nhân lên
//     theo số hàng vế ngoài;
//   - với hash join: lọc vế BUILD làm bảng băm nhỏ lại, tức là làm ít khả năng
//     phải tràn ra đĩa hơn. Đây là chỗ pushdown đổi từ "nhanh hơn một chút"
//     thành "không phải ghi đĩa" — một bậc thang, không phải một hệ số;
//   - và với cả hai: một điều kiện xuống được tới Scan là điều kiện có thể
//     thành KHOẢNG QUÉT trên index (physical.go làm việc đó), tức là không
//     đọc hàng chứ không phải đọc rồi bỏ.
//
// Vế còn lại — điều kiện dùng cột của HAI quan hệ mà không phải phép bằng —
// nằm lại ở Filter phía trên join. Không có cách nào khác: nó cần cột của cả
// hai bên nên nó không tồn tại trước khi join xảy ra.
//
// Chỗ luật này KHÔNG áp dụng được, và nó đáng nhớ: `a.x=1 OR b.y=2`. Vế OR
// không tách được, và toàn bộ biểu thức dùng cột của hai bảng, nên nó phải
// chờ tới sau join. Bất đối xứng giữa AND và OR ấy có mặt trong mọi optimizer,
// và nó là lý do một câu WHERE viết bằng OR đôi khi chậm hơn hai câu nối bằng
// UNION — hai câu thì mỗi câu đẩy được điều kiện của mình xuống.

// Optimize viết lại cây logical. Trả về cây mới; cây cũ không bị sửa tại chỗ
// (EXPLAIN in cả hai, nên cây trước khi tối ưu phải còn nguyên).
//
// Hai luật, chạy lồng nhau: SUY RA điều kiện qua phép bằng của join, rồi ĐẨY
// mọi điều kiện xuống. Thứ tự bắt buộc là suy trước đẩy sau — một điều kiện
// suy ra được rồi mới có chỗ để đẩy xuống.
func Optimize(n Node) Node { return pushdown(n) }

// propagate SUY RA điều kiện qua phép bằng của join.
//
//	a.x = b.y  AND  b.y < 5   =>  thêm  a.x < 5
//
// Vì sao đúng: với inner join, mọi hàng RA đều thoả a.x = b.y, nên mọi hàng ra
// cũng thoả a.x < 5. Thêm nó vào không bỏ mất hàng nào — hàng bị nó loại là
// hàng không join được, và hàng ấy vốn đã bị loại. (Với LEFT JOIN thì SAI: hàng
// bên trái không khớp vẫn phải ra, và điều kiện suy ra sẽ giết nó. Đây là lý do
// thứ hai vì sao phase 8 dừng ở inner join.)
//
// Luật này được thêm ở LƯỢT CHẠY, không phải lượt code, và chính bảng số chỉ ra
// nó. Mục 4 của cmd/sqllab đo `ON ev.kind=dim.kind AND dim.kind < 5` và pushdown
// chỉ ăn 1.18x, trong khi hai câu còn lại ăn 60-80x. Lý do: đẩy `dim.kind<5`
// xuống làm vế build teo từ 200 xuống 5 hàng, mà vế PROBE vẫn 20000 hàng — nên
// tiết kiệm gần như vô hình. Điều kiện thật sự đáng đẩy là `ev.kind < 5`, và nó
// KHÔNG có trong câu người gõ: phải suy ra.
//
// Sách gọi đây là transitive predicate propagation. Nó là ví dụ sạch nhất cho
// việc một phép viết lại logical đáng giá bao nhiêu: nó không đổi cách chạy
// nào cả, nó chỉ thêm một câu ĐÚNG mà người gõ không viết ra.
func propagate(all []Expr) []Expr {
	type equiv struct{ a, b *ColExpr }
	var eqs []equiv
	for _, e := range all {
		c, ok := e.(*CmpExpr)
		if !ok || c.Op != sql.OpEq {
			continue
		}
		l, lok := c.L.(*ColExpr)
		r, rok := c.R.(*ColExpr)
		if lok && rok && l.Rel != r.Rel && l.T == r.T {
			eqs = append(eqs, equiv{l, r})
		}
	}
	if len(eqs) == 0 {
		return all
	}
	seen := map[string]bool{}
	for _, e := range all {
		seen[e.String()] = true
	}
	out := all
	for _, q := range eqs {
		for _, e := range all {
			c, ok := e.(*CmpExpr)
			if !ok {
				continue
			}
			col, cok := c.L.(*ColExpr)
			lit, lok := c.R.(*LitExpr)
			if !cok || !lok {
				continue
			}
			var other *ColExpr
			switch col.Slot {
			case q.a.Slot:
				other = q.b
			case q.b.Slot:
				other = q.a
			default:
				continue
			}
			derived := &CmpExpr{Op: c.Op, L: other, R: lit}
			// Trùng thì bỏ: nếu không, mỗi lần Optimize lại sinh thêm một bản
			// sao và một câu chạy hai lần sẽ có kế hoạch khác nhau.
			if seen[derived.String()] {
				continue
			}
			seen[derived.String()] = true
			out = append(out, derived)
		}
	}
	return out
}

// pushdown gom mọi điều kiện gặp trên đường đi xuống rồi phát lại ở chỗ thấp
// nhất mà nó còn tính được.
func pushdown(n Node) Node {
	switch x := n.(type) {
	case *Limit:
		return &Limit{In: pushdown(x.In), N: x.N}
	case *Project:
		return &Project{In: pushdown(x.In), Out: x.Out}
	case *Sort:
		return &Sort{In: pushdown(x.In), By: x.By}
	case *Filter:
		in, left := push(x.In, x.Preds)
		if len(left) == 0 {
			return in
		}
		return &Filter{In: in, Preds: left}
	case *Join:
		in, left := push(x, nil)
		if len(left) == 0 {
			return in
		}
		return &Filter{In: in, Preds: left}
	}
	return n
}

// push cố đẩy preds vào trong node n. Trả về (cây mới, những vế KHÔNG đẩy
// được).
func push(n Node, preds []Expr) (Node, []Expr) {
	switch x := n.(type) {
	case *Scan:
		// Tới sát quan hệ: mọi vế đều nhận, kể cả vế không dùng cột nào
		// (`WHERE 1=1`) — nó rẻ nhất khi tính ở đây.
		cp := &Scan{Rel: x.Rel, RelRef: x.RelRef, Sc: x.Sc}
		cp.Preds = append(append([]Expr(nil), x.Preds...), preds...)
		return cp, nil

	case *Join:
		// Điều kiện ON được đối xử ĐÚNG NHƯ điều kiện WHERE của một inner
		// join. Với inner join hai chỗ ấy tương đương về ngữ nghĩa, và gộp
		// chúng lại ở đây là cách để `ON a.id=b.id AND b.kind=3` cũng được
		// đẩy. (Với LEFT JOIN thì KHÔNG tương đương — đó là một trong những
		// lý do phase 8 dừng ở inner join.)
		all := propagate(append(append([]Expr(nil), x.On...), preds...))
		lm := relMask(x.L)
		rm := relMask(x.R)
		var toL, toR, keep []Expr
		for _, p := range all {
			r := p.Refs()
			switch {
			case r != 0 && r&^lm == 0:
				toL = append(toL, p)
			case r != 0 && r&^rm == 0:
				toR = append(toR, p)
			default:
				keep = append(keep, p)
			}
		}
		l, lLeft := push(x.L, toL)
		rr, rLeft := push(x.R, toR)
		keep = append(append(keep, lLeft...), rLeft...)

		// Vế nào là điều kiện JOIN (dùng cả hai bên) thì ở lại On; vế nào
		// không thì thành Filter phía trên. Phân biệt ở đây vì physical.go
		// chỉ dùng được On dạng `cột = cột` để làm hash join.
		var on, above []Expr
		for _, p := range keep {
			r := p.Refs()
			if r&lm != 0 && r&rm != 0 {
				on = append(on, p)
			} else {
				above = append(above, p)
			}
		}
		return &Join{L: l, R: rr, On: on}, above
	}
	return n, preds
}

// relMask là bitmask các quan hệ nằm dưới một node.
func relMask(n Node) uint64 {
	switch x := n.(type) {
	case *Scan:
		return 1 << uint(x.Rel)
	case *Join:
		return relMask(x.L) | relMask(x.R)
	case *Filter:
		return relMask(x.In)
	case *Sort:
		return relMask(x.In)
	case *Project:
		return relMask(x.In)
	case *Limit:
		return relMask(x.In)
	}
	return 0
}
