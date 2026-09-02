package query

import (
	"fmt"

	"minidb/internal/keys"
	"minidb/internal/table"
)

// Result là việc THẬT SỰ đã làm, để so với việc kế hoạch nói sẽ làm.
//
// Hai cột EstRows/Rows nằm cạnh nhau có chủ ý: khoảng cách giữa chúng là sai
// số ước lượng, và mọi quyết định tồi của một planner đều bắt đầu từ đó chứ
// không từ mô hình chi phí. Đó là thứ `EXPLAIN ANALYZE` in ra, và là lý do nó
// có chữ ANALYZE.
type Result struct {
	Plan         Plan
	Rows         int
	IndexEntries int
	RowFetches   int
	RowsScanned  int
	Touched      int // số entry của cây đã chạm = IndexEntries + RowsScanned + RowFetches
}

func (r Result) String() string {
	return fmt.Sprintf("%-14s rows=%-6d est=%-6.0f idx=%-6d fetch=%-6d seqread=%-6d touched=%d",
		r.Plan.Kind, r.Rows, r.Plan.Rows, r.IndexEntries, r.RowFetches,
		r.RowsScanned, r.Touched)
}

// Run chạy một kế hoạch. fn nhận các cột trong q.Need, theo đúng thứ tự ấy.
//
// Một hàm, ba đường đi, cùng một chữ ký — đó là điều kiện để so chúng bằng
// benchmark. Nếu ba đường có ba API khác nhau thì mọi so sánh sau đó đều lẫn
// cả chi phí của API vào.
func Run(t *table.Tx, p Plan, q Query, fn func(row []keys.Value) bool) (Result, error) {
	before := t.St
	res := Result{Plan: p}
	sc := q.Table

	match := func(v keys.Value) bool {
		if !q.Lo.IsNull() && keys.Compare(v, q.Lo) < 0 {
			return false
		}
		if !q.Hi.IsNull() && keys.Compare(v, q.Hi) >= 0 {
			return false
		}
		return true
	}

	// inner giữ lỗi phát sinh TRONG callback. Không dùng chung biến với lỗi
	// của ScanIndex: phép gán err = t.ScanIndex(...) chạy SAU callback, nên
	// nó sẽ ghi nil lên lỗi mà callback vừa đặt — một lỗi bị nuốt hoàn toàn
	// im lặng. Đúng cái bẫy mà Txn.Scan của phase 6 đã dính một lần và phải
	// ghi chú thích cảnh báo tại chỗ.
	var err, inner error
	switch p.Kind {
	case SeqScan:
		// Quét cả bảng và lọc. Chú ý: điều kiện được kiểm SAU khi hàng đã
		// được đọc và giải mã — đó chính là cái mà một index tránh được, và
		// cũng là cái mà "predicate pushdown" trong các engine thật cố đẩy
		// xuống sâu hơn nữa.
		err = t.ScanRows(sc, nil, nil, func(pk, row []keys.Value) bool {
			if !match(row[q.Col]) {
				return true
			}
			res.Rows++
			return fn(project(row, q.Need))
		})

	case IndexScan:
		lo, hi := boundVals(q)
		err = t.ScanIndex(p.Index, lo, hi, func(vals, pk []keys.Value) bool {
			// Không cần kiểm lại điều kiện trên cột đầu: khoảng quét ĐÃ là
			// điều kiện. Đây là toàn bộ chỗ index tiết kiệm được.
			row, ok, gerr := t.Get(sc, pk)
			if gerr != nil {
				inner = gerr
				return false
			}
			if !ok {
				// Mục index trỏ tới hàng không còn = bất biến của tầng bảng
				// đã vỡ. Không được im lặng bỏ qua: im lặng ở đây nghĩa là
				// một truy vấn trả về ít hàng hơn sự thật mà không ai biết.
				inner = fmt.Errorf("index %s: mục trỏ tới hàng %v không tồn tại",
					p.Index.Name, pk)
				return false
			}
			res.Rows++
			return fn(project(row, q.Need))
		})

	case IndexOnlyScan:
		lo, hi := boundVals(q)
		err = t.ScanIndex(p.Index, lo, hi, func(vals, pk []keys.Value) bool {
			row, perr := projectIndex(sc, p.Index, vals, pk, q.Need)
			if perr != nil {
				inner = perr
				return false
			}
			res.Rows++
			return fn(row)
		})
	}

	if err == nil {
		err = inner
	}
	d := t.St
	res.IndexEntries = d.IndexEntries - before.IndexEntries
	res.RowFetches = d.RowFetches - before.RowFetches
	res.RowsScanned = d.RowsScanned - before.RowsScanned
	res.Touched = res.IndexEntries + res.RowFetches + res.RowsScanned
	return res, err
}

// boundVals biến khoảng trên MỘT cột thành khoảng trên tiền tố của index.
// Cột đầu của index là cột bị ràng buộc (Choose đã bảo đảm), nên khoảng là
// một tiền tố dài một cột.
func boundVals(q Query) (lo, hi []keys.Value) {
	if !q.Lo.IsNull() {
		lo = []keys.Value{q.Lo}
	}
	if !q.Hi.IsNull() {
		hi = []keys.Value{q.Hi}
	}
	return lo, hi
}

func project(row []keys.Value, need []int) []keys.Value {
	out := make([]keys.Value, len(need))
	for i, c := range need {
		out[i] = row[c]
	}
	return out
}

// projectIndex dựng các cột cần từ MỤC INDEX, không đọc hàng. Nó chỉ chạy
// được khi index phủ đủ — và nó trả lỗi (chứ không trả NULL) khi không phủ,
// vì một index-only scan trả NULL cho cột thiếu là một kết quả sai im lặng.
func projectIndex(sc *table.Schema, ix *table.Index, vals, pk []keys.Value, need []int) ([]keys.Value, error) {
	out := make([]keys.Value, len(need))
	for i, c := range need {
		found := false
		for j, ic := range ix.Cols {
			if ic == c {
				out[i], found = vals[j], true
				break
			}
		}
		if !found {
			for j, pc := range sc.PK {
				if pc == c {
					out[i], found = pk[j], true
					break
				}
			}
		}
		if !found {
			return nil, fmt.Errorf("index %s không phủ cột %q", ix.Name, sc.Cols[c].Name)
		}
	}
	return out, nil
}

// Analyze là `ANALYZE`: quét bảng, đếm hàng, lấy min/max và số giá trị khác
// nhau của các cột được yêu cầu.
//
// Nó quét TOÀN BỘ bảng — không lấy mẫu. DB thật lấy mẫu vì bảng của chúng
// không nằm trong RAM, và cái giá của việc lấy mẫu là thống kê lệch trên phân
// bố có đuôi. Ở đây không lấy mẫu để tách được hai nguồn sai số: sai vì thống
// kê cũ/lệch, và sai vì mô hình phân bố đều. Phase 7 chỉ muốn đo nguồn thứ hai.
func Analyze(t *table.Tx, sc *table.Schema, cols []int) (Stats, error) {
	st := Stats{Cols: map[int]ColStats{}}
	distinct := make([]map[string]bool, len(cols))
	mins := make([]keys.Value, len(cols))
	maxs := make([]keys.Value, len(cols))
	for i := range cols {
		distinct[i] = map[string]bool{}
	}
	err := t.ScanRows(sc, nil, nil, func(pk, row []keys.Value) bool {
		st.Rows++
		for i, c := range cols {
			v := row[c]
			if st.Rows == 1 {
				mins[i], maxs[i] = v, v
			} else {
				if keys.Compare(v, mins[i]) < 0 {
					mins[i] = v
				}
				if keys.Compare(v, maxs[i]) > 0 {
					maxs[i] = v
				}
			}
			distinct[i][string(keys.AppendField(nil, v, false))] = true
		}
		return true
	})
	if err != nil {
		return st, err
	}
	for i, c := range cols {
		st.Cols[c] = ColStats{Min: mins[i], Max: maxs[i], Distinct: len(distinct[i])}
	}
	return st, nil
}
