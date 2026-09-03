package sql

import (
	"minidb/internal/keys"
)

// Parser là recursive-descent, nhìn trước ĐÚNG MỘT token (LL(1)).
//
// Ngữ pháp của phase 8 được chọn để LL(1) là đủ — đó là một quyết định về
// PHẠM VI, không phải về kỹ thuật. Nơi SQL thật không còn LL(1) là những chỗ
// như `(a, b) IN (SELECT ...)` hay biểu thức bắt đầu bằng '(' mà chưa biết là
// nhóm hay là danh sách; ngữ pháp ở đây tránh hết. Postgres dùng bison
// (LALR(1)) và vẫn phải có ~30 chỗ đánh dấu %prec để gỡ nhập nhằng.
//
// Độ ưu tiên toán tử làm bằng leo tầng (precedence climbing), không bằng bảng:
// chỉ có ba tầng (OR < AND < so sánh) nên một bảng là nhiều máy móc hơn cần.
type Parser struct {
	lx  *Lexer
	tok Token
	err error
}

func NewParser(src string) *Parser {
	p := &Parser{lx: NewLexer(src)}
	p.advance()
	return p
}

// Parse phân tích MỘT câu và đòi hết chuỗi sau đó (cho phép một ';' cuối).
func Parse(src string) (Stmt, error) {
	p := NewParser(src)
	s := p.stmt()
	if p.err != nil {
		return nil, p.err
	}
	if p.tok.Kind == Punct && p.tok.Text == ";" {
		p.advance()
	}
	if p.tok.Kind != EOF {
		return nil, p.errf("còn dư %s sau khi câu đã hết", p.tok)
	}
	if p.err != nil {
		return nil, p.err
	}
	return s, nil
}

// ParseMany phân tích một chuỗi nhiều câu cách nhau bởi ';'. Dùng cho REPL và
// cho các file .sql của test.
func ParseMany(src string) ([]Stmt, error) {
	p := NewParser(src)
	var out []Stmt
	for {
		for p.tok.Kind == Punct && p.tok.Text == ";" {
			p.advance()
		}
		if p.tok.Kind == EOF {
			return out, p.err
		}
		s := p.stmt()
		if p.err != nil {
			return nil, p.err
		}
		out = append(out, s)
	}
}

// ---------- máy móc ----------

func (p *Parser) advance() {
	if p.err != nil {
		return
	}
	t, err := p.lx.Next()
	if err != nil {
		p.err = err
		p.tok = Token{Kind: EOF, Pos: p.tok.Pos}
		return
	}
	p.tok = t
}

// errf ghi lỗi ĐẦU TIÊN rồi ĐẶT TOKEN VỀ EOF.
//
// Việc đặt về EOF không phải dọn dẹp cho gọn — nó là điều kiện để parser DỪNG.
// Bản đầu chỉ ghi p.err rồi trả về, mà advance() thì không làm gì khi đã có
// lỗi; nên p.tok đứng nguyên và mọi vòng lặp được điều khiển bởi việc TIÊU THỤ
// token quay vô hạn. Cụ thể `WHERE x = = 1`: binary() thấy mãi một toán tử ở
// p.tok và không bao giờ thoát — bài test cú pháp TREO thay vì đỏ.
//
// Bài học có hình dạng quen: một đường lỗi không tiến lên được thì không phải
// một lỗi, nó là một cái treo. Cùng loại với lần duyệt không hữu hạn ở phase 7
// (writer chèn mãi vào phía trước cursor) — cả hai lần, thứ thiếu là một chốt
// bảo đảm MỖI bước đều đi tới.
func (p *Parser) errf(f string, a ...any) error {
	if p.err == nil {
		p.err = p.lx.errf(p.tok.Pos, f, a...) // lỗi ĐẦU TIÊN mới đáng
	}
	p.tok = Token{Kind: EOF, Pos: p.tok.Pos}
	return p.err
}

func (p *Parser) isKw(kw string) bool { return p.tok.Kind == Keyword && p.tok.Text == kw }

func (p *Parser) acceptKw(kw string) bool {
	if p.isKw(kw) {
		p.advance()
		return true
	}
	return false
}

func (p *Parser) expectKw(kw string) {
	if !p.acceptKw(kw) {
		p.errf("cần %s, gặp %s", kw, p.tok)
	}
}

func (p *Parser) isPunct(s string) bool { return p.tok.Kind == Punct && p.tok.Text == s }

func (p *Parser) acceptPunct(s string) bool {
	if p.isPunct(s) {
		p.advance()
		return true
	}
	return false
}

func (p *Parser) expectPunct(s string) {
	if !p.acceptPunct(s) {
		p.errf("cần %q, gặp %s", s, p.tok)
	}
}

// ident nhận một tên. Từ khoá KHÔNG được dùng làm tên — thông báo lỗi nói rõ
// điều đó thay vì chỉ bảo "cần tên", vì đó là lỗi người dùng gặp nhiều nhất
// khi có một cột tên `key` hay `order`.
func (p *Parser) ident(what string) string {
	if p.tok.Kind == Keyword {
		p.errf("%s không được là từ khoá %s", what, p.tok.Text)
		return ""
	}
	if p.tok.Kind != Ident {
		p.errf("cần %s, gặp %s", what, p.tok)
		return ""
	}
	s := p.tok.Text
	p.advance()
	return s
}

// ---------- câu lệnh ----------

func (p *Parser) stmt() Stmt {
	switch {
	case p.isKw("SELECT"):
		return p.selectStmt()
	case p.acceptKw("CREATE"):
		return p.createStmt()
	case p.acceptKw("INSERT"):
		return p.insertStmt()
	case p.acceptKw("EXPLAIN"):
		e := &Explain{Analyze: p.acceptKw("ANALYZE")}
		e.Stmt = p.stmt()
		return e
	case p.acceptKw("ANALYZE"):
		return &AnalyzeStmt{Table: p.ident("tên bảng")}
	}
	p.errf("cần SELECT / CREATE / INSERT / EXPLAIN / ANALYZE, gặp %s", p.tok)
	return nil
}

func (p *Parser) createStmt() Stmt {
	if p.acceptKw("TABLE") {
		return p.createTable()
	}
	unique := p.acceptKw("UNIQUE")
	if !p.acceptKw("INDEX") {
		p.errf("cần TABLE hoặc INDEX sau CREATE, gặp %s", p.tok)
		return nil
	}
	ci := &CreateIndex{Unique: unique}
	ci.Name = p.ident("tên index")
	p.expectKw("ON")
	ci.Table = p.ident("tên bảng")
	p.expectPunct("(")
	for {
		c := IndexCol{Name: p.ident("tên cột")}
		if p.acceptKw("DESC") {
			c.Desc = true
		} else {
			p.acceptKw("ASC")
		}
		ci.Cols = append(ci.Cols, c)
		if !p.acceptPunct(",") {
			break
		}
	}
	p.expectPunct(")")
	return ci
}

func (p *Parser) createTable() Stmt {
	ct := &CreateTable{Name: p.ident("tên bảng")}
	p.expectPunct("(")
	for {
		// PRIMARY KEY (...) là một mục trong danh sách, không phải một hậu tố.
		// Chỉ nhận dạng này (không nhận `a INT PRIMARY KEY`) để primary key
		// composite là ca THƯỜNG chứ không phải ca đặc biệt — bảng của phase 7
		// có pk composite và đó là cái đúng để mặc định.
		if p.acceptKw("PRIMARY") {
			p.expectKw("KEY")
			p.expectPunct("(")
			for {
				ct.PK = append(ct.PK, p.ident("tên cột trong primary key"))
				if !p.acceptPunct(",") {
					break
				}
			}
			p.expectPunct(")")
		} else {
			cd := ColDef{Pos: p.tok.Pos}
			cd.Name = p.ident("tên cột")
			cd.Type = p.colType()
			ct.Cols = append(ct.Cols, cd)
		}
		if !p.acceptPunct(",") {
			break
		}
	}
	p.expectPunct(")")
	if len(ct.PK) == 0 {
		p.errf("bảng %s không có PRIMARY KEY — tầng bảng của phase 7 lưu hàng"+
			" TRONG cây khóa chính, nên không có pk thì không có chỗ đặt hàng", ct.Name)
	}
	return ct
}

// colType ánh xạ tên kiểu SQL sang keys.Type.
//
// Bốn kiểu, và không có kiểu nào có tham số (không VARCHAR(n), không NUMERIC).
// Lý do: bộ mã hoá của phase 7 có đúng bốn hình byte, và thêm một tên kiểu SQL
// không có hình byte tương ứng là hứa một thứ không giữ được.
func (p *Parser) colType() keys.Type {
	if p.tok.Kind != Keyword {
		p.errf("cần kiểu cột (INT/UINT/TEXT/BOOL), gặp %s", p.tok)
		return 0
	}
	t := p.tok.Text
	p.advance()
	switch t {
	case "INT":
		return keys.TypeInt
	case "UINT":
		return keys.TypeUint
	case "TEXT":
		return keys.TypeBytes
	case "BOOL":
		// Bool trong bộ mã hoá là HAI tag (TypeFalse/TypeTrue), không phải một
		// kiểu có hai giá trị. Ở lược đồ ta khai TypeFalse làm đại diện, và
		// Schema.Check phải nhận cả hai — xem nợ P8 về việc đó chưa được kiểm.
		return keys.TypeFalse
	}
	p.errf("kiểu %s chưa hỗ trợ", t)
	return 0
}

func (p *Parser) insertStmt() Stmt {
	ins := &Insert{Pos: p.tok.Pos}
	p.expectKw("INTO")
	ins.Table = p.ident("tên bảng")
	// Danh sách cột là TÙY CHỌN. Bỏ nó đi thì thứ tự cột của bảng thành một
	// phần của câu lệnh — tiện khi gõ tay, nguy hiểm trong script, đúng như
	// SQL thật.
	if p.acceptPunct("(") {
		for {
			ins.Cols = append(ins.Cols, p.ident("tên cột"))
			if !p.acceptPunct(",") {
				break
			}
		}
		p.expectPunct(")")
	}
	p.expectKw("VALUES")
	for {
		p.expectPunct("(")
		var row []Expr
		for {
			row = append(row, p.expr())
			if !p.acceptPunct(",") {
				break
			}
		}
		p.expectPunct(")")
		ins.Rows = append(ins.Rows, row)
		if !p.acceptPunct(",") {
			break
		}
	}
	return ins
}

func (p *Parser) selectStmt() Stmt {
	p.expectKw("SELECT")
	s := &Select{Limit: -1}
	for {
		s.Cols = append(s.Cols, p.resultCol())
		if !p.acceptPunct(",") {
			break
		}
	}
	p.expectKw("FROM")
	s.From = p.tableRef()
	for {
		p.acceptKw("INNER")
		if !p.acceptKw("JOIN") {
			break
		}
		j := Join{Pos: p.tok.Pos}
		j.Right = p.tableRef()
		p.expectKw("ON")
		j.On = p.expr()
		s.Joins = append(s.Joins, j)
	}
	if p.acceptKw("WHERE") {
		s.Where = p.expr()
	}
	if p.acceptKw("ORDER") {
		p.expectKw("BY")
		for {
			it := OrderItem{E: p.expr()}
			if p.acceptKw("DESC") {
				it.Desc = true
			} else {
				p.acceptKw("ASC")
			}
			s.OrderBy = append(s.OrderBy, it)
			if !p.acceptPunct(",") {
				break
			}
		}
	}
	if p.acceptKw("LIMIT") {
		if p.tok.Kind != Num {
			p.errf("cần số sau LIMIT, gặp %s", p.tok)
			return s
		}
		s.Limit = p.tok.Val
		p.advance()
	}
	return s
}

func (p *Parser) resultCol() ResultCol {
	if p.acceptPunct("*") {
		return ResultCol{Star: true}
	}
	// `t.*`: phải nhìn trước hai token, mà parser chỉ nhìn trước một. Giải
	// bằng cách phân tích như biểu thức rồi phát hiện `.` + `*` bên trong
	// primary() — chỗ duy nhất trong ngữ pháp này cần đến mẹo, và đây là
	// phiên bản nhỏ của cái làm SQL không còn LL(1).
	if p.tok.Kind == Ident {
		name := p.tok.Text
		save := p.tok
		p.advance()
		if p.isPunct(".") {
			p.advance()
			if p.acceptPunct("*") {
				return ResultCol{Star: true, StarTable: name}
			}
			col := p.ident("tên cột")
			return p.aliasOf(ResultCol{E: &ColRefExpr{Table: name, Name: col, Pos: save.Pos}})
		}
		// Không phải `t.` — tiếp tục biểu thức với tên đã đọc làm gốc.
		e := p.exprFrom(&ColRefExpr{Name: name, Pos: save.Pos})
		return p.aliasOf(ResultCol{E: e})
	}
	return p.aliasOf(ResultCol{E: p.expr()})
}

func (p *Parser) aliasOf(rc ResultCol) ResultCol {
	if p.acceptKw("AS") {
		rc.Alias = p.ident("bí danh cột")
	} else if p.tok.Kind == Ident {
		rc.Alias = p.tok.Text // bí danh không có AS, như SQL cho phép
		p.advance()
	}
	return rc
}

func (p *Parser) tableRef() TableRef {
	r := TableRef{Pos: p.tok.Pos}
	r.Name = p.ident("tên bảng")
	if p.acceptKw("AS") {
		r.Alias = p.ident("bí danh bảng")
	} else if p.tok.Kind == Ident {
		r.Alias = p.tok.Text
		p.advance()
	}
	return r
}

// ---------- biểu thức: leo tầng ----------

// precOf: OR thấp nhất, rồi AND, rồi so sánh. 0 = không phải toán tử nhị phân.
func precOf(t Token) (BinOp, int) {
	if t.Kind == Keyword {
		switch t.Text {
		case "OR":
			return OpOr, 1
		case "AND":
			return OpAnd, 2
		}
	}
	if t.Kind == Op {
		switch t.Text {
		case "=":
			return OpEq, 3
		case "<>":
			return OpNe, 3
		case "<":
			return OpLt, 3
		case "<=":
			return OpLe, 3
		case ">":
			return OpGt, 3
		case ">=":
			return OpGe, 3
		}
	}
	return 0, 0
}

func (p *Parser) expr() Expr { return p.binary(p.primary(), 1) }

// exprFrom tiếp tục một biểu thức mà toán hạng đầu đã được đọc — cần cho mẹo
// `t.*` ở resultCol.
func (p *Parser) exprFrom(lhs Expr) Expr { return p.binary(lhs, 1) }

func (p *Parser) binary(lhs Expr, minPrec int) Expr {
	for {
		op, prec := precOf(p.tok)
		if prec < minPrec {
			return lhs
		}
		pos := p.tok.Pos
		p.advance()
		rhs := p.primary()
		// Mọi toán tử ở đây kết hợp trái, nên vế phải chỉ gom các toán tử ưu
		// tiên CAO HƠN (prec+1).
		for {
			_, next := precOf(p.tok)
			if next <= prec {
				break
			}
			rhs = p.binary(rhs, prec+1)
		}
		lhs = &BinExpr{Op: op, L: lhs, R: rhs, Pos: pos}
	}
}

func (p *Parser) primary() Expr {
	t := p.tok
	switch {
	case p.acceptPunct("("):
		e := p.expr()
		p.expectPunct(")")
		return e

	case t.Kind == Num:
		p.advance()
		// Số nguyên không dấu trong câu SQL thành TypeInt, không phải TypeUint.
		// Việc ép sang kiểu của cột là việc của binder — parser không biết kiểu
		// cột. Nếu đoán ở đây thì `WHERE u = 5` với u kiểu UINT sẽ so một
		// TypeInt với một TypeUint và keys.Compare trả về thứ tự theo TAG, tức
		// là luôn sai. Đây là loại bug im lặng đắt nhất trong cả tầng này.
		return &LitExpr{V: keys.Int(t.Val), Pos: t.Pos}

	case t.Kind == Str:
		p.advance()
		return &LitExpr{V: keys.Str(t.Text), Pos: t.Pos}

	case p.isKw("NULL"):
		p.advance()
		return &LitExpr{V: keys.Null(), Pos: t.Pos}

	case p.isKw("TRUE"):
		p.advance()
		return &LitExpr{V: keys.Bool(true), Pos: t.Pos}

	case p.isKw("FALSE"):
		p.advance()
		return &LitExpr{V: keys.Bool(false), Pos: t.Pos}

	case t.Kind == Ident:
		p.advance()
		if p.acceptPunct(".") {
			return &ColRefExpr{Table: t.Text, Name: p.ident("tên cột"), Pos: t.Pos}
		}
		return &ColRefExpr{Name: t.Text, Pos: t.Pos}
	}
	p.errf("cần một giá trị hoặc tên cột, gặp %s", p.tok)
	return &LitExpr{V: keys.Null(), Pos: t.Pos}
}
