package exec

import (
	"fmt"

	"minidb/internal/keys"
	"minidb/internal/plan"
	"minidb/internal/table"
)

// Op là một toán tử Volcano. Ba phương thức, và cái quan trọng nhất là cái
// KHÔNG có: không có phương thức nào trả về "tất cả các hàng".
//
// Next đặt hàng kế tiếp vào tuple phẳng dùng chung (Runner.Row) rồi trả true.
// Trả (false, nil) là hết; (false, err) là lỗi. Không dùng lối Err() tách
// riêng như db.Iter vì ở đây mỗi bước có thể sinh lỗi từ nhiều nguồn (đĩa,
// giải mã, transaction) và một lỗi bị nuốt ở giữa cây thì không truy được.
type Op interface {
	Next() (bool, error)
	Close() error
}

// Stat là việc THẬT SỰ đã làm — để đặt cạnh ước lượng của planner.
//
// Khoảng cách giữa Est và Stat là thứ EXPLAIN ANALYZE in ra, và phase 7 đã
// chứng minh nó là nguồn của mọi kế hoạch tồi, chứ không phải mô hình chi phí.
type Stat struct {
	RowsOut      int   // hàng ra khỏi đỉnh cây
	InnerScans   int   // số lần nested loop chạy lại vế trong
	HashBuild    int   // số hàng vào bảng băm
	HashProbe    int   // số hàng dò
	SortRows     int   // số hàng đưa vào sắp xếp
	SortRuns     int   // số run đã ghi ra đĩa (0 = sắp trong RAM)
	SpillFiles   int   // số file tạm đã tạo
	SpillBytes   int64 // số byte đã ghi ra đĩa
	SpillRereads int   // số hàng đọc LẠI từ đĩa
}

// Runner giữ bối cảnh thi hành: transaction, tuple phẳng, hạn mức, thư mục tạm.
type Runner struct {
	Tx     *table.Tx
	Rels   []plan.Rel
	Row    []keys.Value
	Budget int
	TmpDir string
	St     Stat
}

// New dựng một Runner cho một câu đã bind.
func New(tx *table.Tx, b *plan.Bound, budget int, tmpDir string) *Runner {
	return &Runner{Tx: tx, Rels: b.Rels, Row: make([]keys.Value, b.Width),
		Budget: budget, TmpDir: tmpDir}
}

// Build dựng cây toán tử từ cây vật lý.
func (r *Runner) Build(p plan.PNode) (Op, error) {
	switch x := p.(type) {
	case *plan.PScan:
		return r.newScan(x)
	case *plan.PFilter:
		in, err := r.Build(x.In)
		if err != nil {
			return nil, err
		}
		return &filterOp{r: r, in: in, preds: x.Preds}, nil
	case *plan.PProject:
		in, err := r.Build(x.In)
		if err != nil {
			return nil, err
		}
		return &projectOp{r: r, in: in, out: x.Out}, nil
	case *plan.PLimit:
		in, err := r.Build(x.In)
		if err != nil {
			return nil, err
		}
		return &limitOp{in: in, n: x.N}, nil
	case *plan.PSort:
		in, err := r.Build(x.In)
		if err != nil {
			return nil, err
		}
		return r.newSort(x, in)
	case *plan.PNestLoop:
		return r.newNestLoop(x)
	case *plan.PHashJoin:
		return r.newHashJoin(x)
	}
	return nil, fmt.Errorf("exec: node vật lý lạ %T", p)
}

// ---------- toán tử một hàng vào, một hàng ra ----------

type filterOp struct {
	r     *Runner
	in    Op
	preds []plan.Expr
}

func (f *filterOp) Next() (bool, error) {
	for {
		ok, err := f.in.Next()
		if err != nil || !ok {
			return false, err
		}
		pass := true
		for _, p := range f.preds {
			v, err := p.Eval(f.r.Row)
			if err != nil {
				return false, err
			}
			// Chỉ TRUE mới lọt: NULL bị loại. Xem plan.True.
			if !plan.True(v) {
				pass = false
				break
			}
		}
		if pass {
			return true, nil
		}
	}
}

func (f *filterOp) Close() error { return f.in.Close() }

// projectOp là toán tử duy nhất KHÔNG viết vào tuple phẳng: nó viết ra một
// slice riêng, vì kết quả có thể chứa cùng một cột hai lần hoặc theo thứ tự
// khác, nên nó không còn là một "hàng của một quan hệ" nữa.
type projectOp struct {
	r   *Runner
	in  Op
	out []plan.OutCol
	res []keys.Value
}

func (p *projectOp) Next() (bool, error) {
	ok, err := p.in.Next()
	if err != nil || !ok {
		return false, err
	}
	if p.res == nil {
		p.res = make([]keys.Value, len(p.out))
	}
	for i, o := range p.out {
		v, err := o.E.Eval(p.r.Row)
		if err != nil {
			return false, err
		}
		p.res[i] = v
	}
	p.r.St.RowsOut++
	return true, nil
}

func (p *projectOp) Close() error { return p.in.Close() }

// Result là hàng kết quả của lần chạy — chỉ có nghĩa sau một Next thành công.
func (p *projectOp) Result() []keys.Value { return p.res }

type limitOp struct {
	in Op
	n  int64
	i  int64
}

func (l *limitOp) Next() (bool, error) {
	if l.i >= l.n {
		return false, nil
	}
	ok, err := l.in.Next()
	if err != nil || !ok {
		return false, err
	}
	l.i++
	return true, nil
}

func (l *limitOp) Close() error { return l.in.Close() }

// Rows chạy cây tới hết và gọi fn cho mỗi hàng kết quả.
//
// fn nhận slice CHỈ dùng được tới lần gọi sau — cùng hợp đồng với mọi iterator
// trong repo này. Ai muốn giữ thì tự chép; nói rõ vì đây là API mà cmd/minidb
// và mọi test dùng.
func (r *Runner) Rows(top Op, fn func(row []keys.Value) bool) error {
	pr, _ := top.(*projectOp)
	for {
		ok, err := top.Next()
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		var row []keys.Value
		if pr != nil {
			row = pr.Result()
		} else {
			row = r.Row
		}
		if !fn(row) {
			return nil
		}
	}
}
