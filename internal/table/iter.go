package table

import (
	"fmt"

	"minidb/internal/keys"
	"minidb/internal/txn"
)

// iter.go là bản KÉO của hai phép quét ở tầng bảng, và là bản cài duy nhất:
// ScanRows/ScanIndex của phase 7 giờ là hai lớp vỏ ở cuối file này.
//
// Lý do đổi nằm ở internal/txn/iter.go — join cần quyền hỏi hàng kế tiếp. Ở
// tầng này có thêm một lý do thứ hai, nhỏ hơn nhưng đắt hơn khi sai: một hash
// join hút cạn build side rồi mới đọc probe side, và nếu cả hai vế cùng là
// một callback thì hai vòng lặp lồng nhau ấy chia sẻ cùng một Tx.St. Với API
// kéo, mỗi bên có con trỏ riêng và bộ đếm vẫn nằm ở một chỗ.

// RowIter duyệt bảng theo thứ tự PRIMARY KEY trong [lo, hi). lo/hi là primary
// key MỘT PHẦN cũng được (tiền tố bên trái); nil = từ đầu / tới hết.
//
// Đây chính là seq scan — và nó là một range scan trên cây, không phải một
// phép đọc kiểu khác. Chỗ đáng nhớ: seq scan ở đây đi theo thứ tự PRIMARY KEY
// vì hàng được lưu TRONG cây index của pk (clustered index, kiểu InnoDB/SQLite
// chứ không phải heap của Postgres). Hai hệ quả đo được: một truy vấn ràng
// buộc pk KHÔNG BAO GIỜ cần secondary index (phase 7), và một `ORDER BY pk`
// KHÔNG cần bước sắp xếp (phase 8 — chính là chỗ nợ P7-9).
type RowIter struct {
	t      *Tx
	sc     *Schema
	it     *txn.Iter
	plen   int
	ord    keys.Order
	pk     []keys.Value
	row    []keys.Value
	err    error
	closed bool
}

// IterRows mở một RowIter trên khoảng [lo, hi) của primary key.
//
// Chặn trên là NGẶT, và đó là một hạn chế thật chứ không phải một quy ước:
// `WHERE pk = v` KHÔNG viết được bằng API này, vì [v, v) là khoảng rỗng còn
// [v, v+1) chỉ tính được cho kiểu số. Ai cần khoảng ĐÓNG thì dùng
// IterRowsKeys với keys.PrefixEnd — xem chú thích ở đó.
func (t *Tx) IterRows(sc *Schema, lo, hi []keys.Value) *RowIter {
	prefix := sc.RowPrefix()
	loKey := append([]byte(nil), prefix...)
	if len(lo) > 0 {
		loKey = keys.Encode(loKey, lo, sc.PKOrder())
	}
	hiKey := keys.PrefixEnd(prefix)
	if len(hi) > 0 {
		hiKey = keys.Encode(append([]byte(nil), prefix...), hi, sc.PKOrder())
	}
	return t.IterRowsKeys(sc, loKey, hiKey)
}

// IterRowsKeys là IterRows với khoảng cho bằng KHÓA THÔ.
//
// Có mặt vì phase 8 cần một khoảng mà API giá trị không diễn tả được: khoảng
// của một phép BẰNG. Với khóa nhiều cột, "mọi hàng có cột đầu = v" là
//
//	[ Encode(v), PrefixEnd(Encode(v)) )
//
// và PrefixEnd không phải một giá trị — nó là một phép toán trên byte. Đây
// đúng là chỗ mà "khóa là byte, không phải tuple" trở thành một tiện lợi thay
// vì một cái giá: phép bằng trên tiền tố và phép khoảng trên tiền tố là CÙNG
// một phép quét, chỉ khác hai đầu mút.
func (t *Tx) IterRowsKeys(sc *Schema, loKey, hiKey []byte) *RowIter {
	return &RowIter{t: t, sc: sc, it: t.tx.Iter(loKey, hiKey),
		plen: len(sc.RowPrefix()), ord: sc.PKOrder()}
}

func (r *RowIter) Next() bool {
	if r.err != nil || r.closed {
		return false
	}
	if !r.it.Next() {
		r.err = r.it.Err()
		return false
	}
	r.t.St.RowsScanned++
	pk, _, err := keys.Decode(r.it.Key()[r.plen:], len(r.sc.PK), r.ord)
	if err != nil {
		r.err = fmt.Errorf("table %s: khóa hàng %x: %w", r.sc.Name, r.it.Key(), err)
		return false
	}
	row, _, err := keys.Decode(r.it.Value(), len(r.sc.Cols), nil)
	if err != nil {
		r.err = fmt.Errorf("table %s: hàng %v: %w", r.sc.Name, pk, err)
		return false
	}
	r.pk, r.row = pk, row
	return true
}

// PK và Row: keys.Decode đã cấp phát slice mới mỗi hàng (nợ P7-1), nên hai
// giá trị này KHÔNG bị lần Next sau ghi đè — khác với txn.Iter ở tầng dưới.
// Sự khác biệt ấy là một điều đáng biết chứ không phải một tiện lợi: nó chính
// là 152 byte × mỗi hàng mà P7-1 nói tới, và là lý do vectorized execution
// tồn tại.
func (r *RowIter) PK() []keys.Value  { return r.pk }
func (r *RowIter) Row() []keys.Value { return r.row }
func (r *RowIter) Err() error        { return r.err }
func (r *RowIter) Close() error      { r.closed = true; return r.err }

// IndexIter duyệt index trong khoảng [lo, hi) của các CỘT ĐẦU của index.
//
// Nó trả (giá trị cột index, primary key) chứ không trả hàng: đó là toàn bộ sự
// khác nhau giữa index scan và index-only scan. Ai cần cột khác thì tự gọi
// Get — và đúng lúc gọi Get ấy mới trả cái giá của một lần xuống cây nữa. Bắt
// người gọi tự làm việc đó là có chủ ý: nó làm cái giá HIỆN RA trong code,
// thay vì trốn trong một hàm tiện lợi.
type IndexIter struct {
	t         *Tx
	ix        *Index
	sc        *Schema
	it        *txn.Iter
	plen      int
	nIdx, nPK int
	pkOrd     keys.Order
	vals, pkv []keys.Value
	err       error
	closed    bool
}

// IterIndex mở một IndexIter trên khoảng [lo, hi) của các cột ĐẦU của index.
// Chặn trên ngặt — cùng hạn chế và cùng cách gỡ như IterRows.
func (t *Tx) IterIndex(ix *Index, lo, hi []keys.Value) *IndexIter {
	loKey := ix.SeekKey(nil, lo)
	hiKey := keys.PrefixEnd(ix.IndexPrefix())
	if len(hi) > 0 {
		hiKey = ix.SeekKey(nil, hi)
	}
	return t.IterIndexKeys(ix, loKey, hiKey)
}

// IterIndexKeys là IterIndex với khoảng cho bằng KHÓA THÔ.
func (t *Tx) IterIndexKeys(ix *Index, loKey, hiKey []byte) *IndexIter {
	sc, err := t.c.TableByOID(ix.Table)
	if err != nil {
		return &IndexIter{err: err, closed: true}
	}
	return &IndexIter{
		t: t, ix: ix, sc: sc, it: t.tx.Iter(loKey, hiKey),
		plen: len(ix.IndexPrefix()), nIdx: len(ix.Cols), nPK: len(sc.PK), pkOrd: sc.PKOrder(),
	}
}

func (r *IndexIter) Next() bool {
	if r.err != nil || r.closed {
		return false
	}
	if !r.it.Next() {
		r.err = r.it.Err()
		return false
	}
	r.t.St.IndexEntries++
	k := r.it.Key()
	body := k[r.plen:]
	vals, used, err := keys.Decode(body, r.nIdx, r.ix.Order())
	if err != nil {
		r.err = fmt.Errorf("index %s: khóa %x: %w", r.ix.Name, k, err)
		return false
	}
	var pk []keys.Value
	// Unique hay không quyết định chỗ pk NẰM, không phải một chi tiết cài đặt:
	// index non-unique buộc phải nhét pk vào KHÓA để hai hàng cùng giá trị
	// không đè nhau; index unique buộc phải KHÔNG nhét, vì có nhét thì hai
	// hàng trùng giá trị lại thành hai khóa khác nhau và ràng buộc unique
	// biến mất. Phase 7 đo cả hai chiều của câu đó.
	if r.ix.Unique {
		pk, _, err = keys.Decode(r.it.Value(), r.nPK, r.pkOrd)
	} else {
		pk, _, err = keys.Decode(body[used:], r.nPK, r.pkOrd)
	}
	if err != nil {
		r.err = fmt.Errorf("index %s: pk trong mục %x: %w", r.ix.Name, k, err)
		return false
	}
	r.vals, r.pkv = vals, pk
	return true
}

func (r *IndexIter) Vals() []keys.Value { return r.vals }
func (r *IndexIter) PK() []keys.Value   { return r.pkv }
func (r *IndexIter) Err() error         { return r.err }
func (r *IndexIter) Close() error       { r.closed = true; return r.err }

// ---------- lớp vỏ push, giữ cho ba chục chỗ gọi của phase 6-7 ----------

// ScanRows là RowIter ở dạng callback.
func (t *Tx) ScanRows(sc *Schema, lo, hi []keys.Value, fn func(pk, row []keys.Value) bool) error {
	it := t.IterRows(sc, lo, hi)
	defer it.Close()
	for it.Next() {
		if !fn(it.PK(), it.Row()) {
			return it.Err()
		}
	}
	return it.Err()
}

// ScanIndex là IndexIter ở dạng callback.
func (t *Tx) ScanIndex(ix *Index, lo, hi []keys.Value, fn func(vals, pk []keys.Value) bool) error {
	it := t.IterIndex(ix, lo, hi)
	defer it.Close()
	for it.Next() {
		if !fn(it.Vals(), it.PK()) {
			return it.Err()
		}
	}
	return it.Err()
}
