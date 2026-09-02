// Package table đặt một tầng CÓ LƯỢC ĐỒ lên trên không gian khóa phẳng của
// internal/txn: bảng, primary key composite, và secondary index.
//
// Câu hỏi trung tâm của phase 7 và câu trả lời của package này:
//
//	Làm sao nhiều bảng và nhiều index cùng sống trong MỘT cây B+?
//
// Bằng tiền tố. Mỗi bảng và mỗi index được cấp một số (oid) và chiếm một
// KHOẢNG LIÊN TỤC của không gian khóa:
//
//	0x0a 't' <tên>                          -> lược đồ bảng   (catalog)
//	0x0a 'i' <tên>                          -> định nghĩa index
//	0x0b <oid bảng> <pk đã mã hoá>          -> hàng
//	0x0c <oid index> <cột index> [<pk>]     -> mục index
//
// Ba hệ quả, cả ba đều là thứ nhìn thấy trong DB thật:
//
//   - "quét cả bảng" là một range scan trên [prefix, PrefixEnd(prefix)), tức
//     là seq scan và index scan dùng CÙNG MỘT cơ chế. Khác biệt giữa chúng chỉ
//     là quét khoảng nào và có phải đi tra bảng lần nữa hay không.
//   - đánh số bằng oid chứ không bằng tên: đổi tên bảng thành một lần ghi
//     catalog, không phải viết lại từng khóa. Postgres gọi con số ấy là
//     relfilenode, và đó là lý do `ALTER TABLE ... RENAME` ở đó gần như miễn
//     phí còn `ALTER TABLE ... ADD COLUMN` với DEFAULT thì không.
//   - index của một bảng nằm CÁCH XA hàng của bảng đó trên đĩa (khác byte đầu
//     ⇒ khác nhánh cây ⇒ khác page). Đó chính là lý do một index scan có chọn
//     lọc thấp lại chậm hơn seq scan: nó đọc xen kẽ hai vùng page rời nhau.
package table

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"minidb/internal/keys"
	"minidb/internal/txn"
)

var (
	ErrNoTable      = errors.New("table: không có bảng này")
	ErrNoIndex      = errors.New("table: không có index này")
	ErrExists       = errors.New("table: tên đã tồn tại")
	ErrRowShape     = errors.New("table: hàng không khớp lược đồ")
	ErrNullInPK     = errors.New("table: primary key không được NULL")
	ErrDuplicateKey = errors.New("table: vi phạm ràng buộc duy nhất")
	ErrNoRow        = errors.New("table: không có hàng này")
)

// Tiền tố không gian khóa. Một byte, và KHÔNG phải 0x00 (không gian ấy là của
// metadata internal/txn, bị chặn ở checkKey).
const (
	prefixCatalog byte = 0x0a
	prefixRow     byte = 0x0b
	prefixIndex   byte = 0x0c
)

// Column là một cột: tên, kiểu, và chiều sắp KHI NÓ NẰM TRONG PRIMARY KEY.
type Column struct {
	Name string    `json:"name"`
	T    keys.Type `json:"type"`
	Desc bool      `json:"desc,omitempty"`
}

// Schema là một bảng.
type Schema struct {
	OID  uint32   `json:"oid"`
	Name string   `json:"name"`
	Cols []Column `json:"cols"`
	PK   []int    `json:"pk"` // chỉ số cột tạo primary key, theo đúng thứ tự
}

// Index là một secondary index.
//
// Unique quyết định HÌNH DẠNG KHÓA, không chỉ quyết định một phép kiểm:
//
//	unique     : khóa = <cột index>,        value = <pk>
//	non-unique : khóa = <cột index> <pk>,   value = rỗng
//
// Đây là một trong những chỗ đáng nhất của cả phase. Với index non-unique thì
// pk BẮT BUỘC nằm trong khóa — hai hàng khác nhau cùng giá trị index phải là
// hai entry khác nhau, mà cây chỉ phân biệt entry bằng khóa. Với index unique
// thì pk BẮT BUỘC không nằm trong khóa — nếu nằm thì hai hàng trùng giá trị
// sinh ra hai khóa khác nhau và cây nhận cả hai, tức ràng buộc duy nhất biến
// mất.
//
// Và từ đó ra một hệ quả không đoán trước được: với index unique, ràng buộc
// duy nhất được thực thi MIỄN PHÍ bởi bộ phát hiện xung đột ghi-ghi của phase
// 6 (first-committer-wins), vì hai transaction chèn trùng giá trị sẽ ghi vào
// ĐÚNG MỘT khóa của cây. Với index non-unique thì không bao giờ có xung đột.
// Xem TestUniqueIndexConflictsAcrossTxns.
type Index struct {
	OID    uint32 `json:"oid"`
	Name   string `json:"name"`
	Table  uint32 `json:"table"`
	Cols   []int  `json:"cols"`
	Desc   []bool `json:"desc,omitempty"`
	Unique bool   `json:"unique,omitempty"`
}

// Order là chiều sắp của các cột index.
func (ix *Index) Order() keys.Order { return keys.Order(ix.Desc) }

// PKOrder là chiều sắp của primary key.
func (sc *Schema) PKOrder() keys.Order {
	ord := make(keys.Order, len(sc.PK))
	for i, c := range sc.PK {
		ord[i] = sc.Cols[c].Desc
	}
	return ord
}

// ColIndex tra chỉ số cột theo tên.
func (sc *Schema) ColIndex(name string) (int, bool) {
	for i := range sc.Cols {
		if sc.Cols[i].Name == name {
			return i, true
		}
	}
	return 0, false
}

// Check kiểm một hàng có khớp lược đồ. Kiểu bị kiểm ở ĐÂY, một chỗ duy nhất:
// nếu một giá trị sai kiểu lọt xuống được tới bộ mã hoá khóa thì nó sẽ nằm
// trong cây ở vị trí do TAG quyết định, và một index scan sẽ lặng lẽ bỏ qua
// nó — sai kết quả mà không sai bất biến nào của cây.
func (sc *Schema) Check(row []keys.Value) error {
	if len(row) != len(sc.Cols) {
		return fmt.Errorf("%w: %d giá trị cho %d cột", ErrRowShape, len(row), len(sc.Cols))
	}
	for i, v := range row {
		if v.T == keys.TypeNull {
			continue
		}
		want := sc.Cols[i].T
		if want == keys.TypeTrue || want == keys.TypeFalse {
			if v.T != keys.TypeTrue && v.T != keys.TypeFalse {
				return fmt.Errorf("%w: cột %q cần bool, được %s",
					ErrRowShape, sc.Cols[i].Name, v.T)
			}
			continue
		}
		if v.T != want {
			return fmt.Errorf("%w: cột %q cần %s, được %s",
				ErrRowShape, sc.Cols[i].Name, want, v.T)
		}
	}
	for _, c := range sc.PK {
		if row[c].T == keys.TypeNull {
			return fmt.Errorf("%w: cột %q", ErrNullInPK, sc.Cols[c].Name)
		}
	}
	return nil
}

// PKOf rút primary key ra khỏi một hàng đầy đủ.
func (sc *Schema) PKOf(row []keys.Value) []keys.Value {
	pk := make([]keys.Value, len(sc.PK))
	for i, c := range sc.PK {
		pk[i] = row[c]
	}
	return pk
}

// IndexValsOf rút các cột được index ra khỏi một hàng đầy đủ.
func (ix *Index) IndexValsOf(row []keys.Value) []keys.Value {
	out := make([]keys.Value, len(ix.Cols))
	for i, c := range ix.Cols {
		out[i] = row[c]
	}
	return out
}

// ---------- khóa ----------

func be32(dst []byte, v uint32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return append(dst, b[:]...)
}

// RowPrefix là tiền tố của mọi hàng trong bảng — chặn dưới của một seq scan.
func (sc *Schema) RowPrefix() []byte { return be32([]byte{prefixRow}, sc.OID) }

// RowKey dựng khóa của một hàng từ primary key.
func (sc *Schema) RowKey(dst []byte, pk []keys.Value) []byte {
	dst = be32(append(dst, prefixRow), sc.OID)
	return keys.Encode(dst, pk, sc.PKOrder())
}

// IndexPrefix là tiền tố của mọi mục của index.
func (ix *Index) IndexPrefix() []byte { return be32([]byte{prefixIndex}, ix.OID) }

// EntryKey dựng khóa của một mục index. pk bị BỎ QUA nếu index là unique —
// xem chú thích của Index.
func (ix *Index) EntryKey(dst []byte, vals, pk []keys.Value, pkOrd keys.Order) []byte {
	dst = be32(append(dst, prefixIndex), ix.OID)
	dst = keys.Encode(dst, vals, ix.Order())
	if !ix.Unique {
		dst = keys.Encode(dst, pk, pkOrd)
	}
	return dst
}

// SeekKey dựng chặn dưới cho một lần quét index theo tiền tố các cột đầu.
//
// vals có thể NGẮN HƠN số cột của index — và đó là chỗ luật "index chỉ dùng
// được cho tiền tố bên trái" hiện ra thành code: ràng buộc cột thứ hai mà
// không ràng buộc cột thứ nhất thì không có tiền tố nào để dựng, nên không có
// khoảng nào để quét.
func (ix *Index) SeekKey(dst []byte, vals []keys.Value) []byte {
	dst = be32(append(dst, prefixIndex), ix.OID)
	return keys.Encode(dst, vals, ix.Order())
}

// ---------- catalog ----------

// Catalog là bộ nhớ đệm trong RAM của lược đồ, và là chỗ cấp oid.
//
// Lược đồ được ghi bằng JSON, khác hẳn mọi encoding khác trong repo này. Có
// chủ ý: catalog được đọc MỘT LẦN lúc mở database và ghi khi có DDL, nên nó
// không nằm trên đường nóng nào cả. Tự viết một encoding nhị phân cho nó là
// thêm một bộ codec phải fuzz để đổi lấy vài chục byte và không đổi lấy một
// nano giây nào.
type Catalog struct {
	s *txn.Store

	mu      sync.RWMutex
	tables  map[string]*Schema
	byOID   map[uint32]*Schema
	indexes map[string]*Index
	idxOID  map[uint32]*Index
	idxOf   map[uint32][]*Index
	nextOID uint32
}

func catKey(kind byte, name string) []byte {
	return append([]byte{prefixCatalog, kind}, name...)
}

// Load đọc catalog từ database. Với database mới thì catalog rỗng.
func Load(s *txn.Store) (*Catalog, error) {
	c := &Catalog{
		s:       s,
		tables:  map[string]*Schema{},
		byOID:   map[uint32]*Schema{},
		indexes: map[string]*Index{},
		idxOID:  map[uint32]*Index{},
		idxOf:   map[uint32][]*Index{},
		nextOID: 1,
	}
	lo := []byte{prefixCatalog}
	hi := keys.PrefixEnd(lo)
	var loadErr error
	err := s.View(txn.RepeatableRead, func(tx *txn.Txn) error {
		return tx.Scan(lo, hi, func(k, v []byte) bool {
			if len(k) < 2 {
				loadErr = fmt.Errorf("catalog: khóa %q quá ngắn", k)
				return false
			}
			switch k[1] {
			case 't':
				var sc Schema
				if err := json.Unmarshal(v, &sc); err != nil {
					loadErr = fmt.Errorf("catalog: bảng %q: %w", k[2:], err)
					return false
				}
				c.putTable(&sc)
			case 'i':
				var ix Index
				if err := json.Unmarshal(v, &ix); err != nil {
					loadErr = fmt.Errorf("catalog: index %q: %w", k[2:], err)
					return false
				}
				c.putIndex(&ix)
			default:
				loadErr = fmt.Errorf("catalog: loại mục lạ %q", k[1])
				return false
			}
			return true
		})
	})
	if err != nil {
		return nil, err
	}
	if loadErr != nil {
		return nil, loadErr
	}
	return c, nil
}

// Store là Store bên dưới — cần cho lab và cho bộ dọn version.
func (c *Catalog) Store() *txn.Store { return c.s }

func (c *Catalog) putTable(sc *Schema) {
	c.tables[sc.Name] = sc
	c.byOID[sc.OID] = sc
	if sc.OID >= c.nextOID {
		c.nextOID = sc.OID + 1
	}
}

func (c *Catalog) putIndex(ix *Index) {
	c.indexes[ix.Name] = ix
	c.idxOID[ix.OID] = ix
	c.idxOf[ix.Table] = append(c.idxOf[ix.Table], ix)
	if ix.OID >= c.nextOID {
		c.nextOID = ix.OID + 1
	}
}

// Table tra một bảng theo tên.
func (c *Catalog) Table(name string) (*Schema, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	sc, ok := c.tables[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrNoTable, name)
	}
	return sc, nil
}

// Index tra một index theo tên.
func (c *Catalog) Index(name string) (*Index, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	ix, ok := c.indexes[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrNoIndex, name)
	}
	return ix, nil
}

// TableByOID tra bảng theo oid — index chỉ giữ oid của bảng chủ.
func (c *Catalog) TableByOID(oid uint32) (*Schema, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	sc, ok := c.byOID[oid]
	if !ok {
		return nil, fmt.Errorf("%w: oid %d", ErrNoTable, oid)
	}
	return sc, nil
}

// Indexes là mọi index của một bảng — danh sách mà mỗi lần ghi phải cập nhật.
func (c *Catalog) Indexes(table uint32) []*Index {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]*Index(nil), c.idxOf[table]...)
}

// Tables liệt kê tên bảng (để lab in ra).
func (c *Catalog) Tables() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]string, 0, len(c.tables))
	for n := range c.tables {
		out = append(out, n)
	}
	return out
}

// CreateTable tạo bảng. pk là tên các cột tạo primary key, theo thứ tự.
func (c *Catalog) CreateTable(name string, cols []Column, pk []string) (*Schema, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.tables[name]; ok {
		return nil, fmt.Errorf("%w: bảng %q", ErrExists, name)
	}
	sc := &Schema{OID: c.nextOID, Name: name, Cols: append([]Column(nil), cols...)}
	for _, pn := range pk {
		i := -1
		for j := range sc.Cols {
			if sc.Cols[j].Name == pn {
				i = j
			}
		}
		if i < 0 {
			return nil, fmt.Errorf("%w: cột pk %q không có", ErrRowShape, pn)
		}
		sc.PK = append(sc.PK, i)
	}
	if len(sc.PK) == 0 {
		return nil, fmt.Errorf("%w: bảng %q không có primary key", ErrRowShape, name)
	}
	blob, err := json.Marshal(sc)
	if err != nil {
		return nil, err
	}
	if err := c.s.Update(txn.RepeatableRead, func(tx *txn.Txn) error {
		return tx.Put(catKey('t', name), blob)
	}); err != nil {
		return nil, err
	}
	c.putTable(sc)
	return sc, nil
}

// CreateIndex tạo index và LẤP ĐẦY nó từ dữ liệu đang có.
//
// Việc lấp đầy chạy trong MỘT transaction logic, nên write set của nó bằng số
// hàng của bảng. Đó là một món nợ có thật (DB thật xây index đồng thời, theo
// lô, và có một pha "bắt kịp" cuối cùng), được ghi vào docs/debts.md thay vì
// giấu đi.
func (c *Catalog) CreateIndex(table, name string, cols []string, unique bool) (*Index, error) {
	sc, err := c.Table(table)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if _, ok := c.indexes[name]; ok {
		c.mu.Unlock()
		return nil, fmt.Errorf("%w: index %q", ErrExists, name)
	}
	ix := &Index{OID: c.nextOID, Name: name, Table: sc.OID, Unique: unique}
	for _, cn := range cols {
		i, ok := sc.ColIndex(cn)
		if !ok {
			c.mu.Unlock()
			return nil, fmt.Errorf("%w: cột %q không có trong bảng %q", ErrRowShape, cn, table)
		}
		ix.Cols = append(ix.Cols, i)
		ix.Desc = append(ix.Desc, sc.Cols[i].Desc)
	}
	if len(ix.Cols) == 0 {
		c.mu.Unlock()
		return nil, fmt.Errorf("%w: index %q không có cột nào", ErrRowShape, name)
	}
	blob, err := json.Marshal(ix)
	if err != nil {
		c.mu.Unlock()
		return nil, err
	}
	c.mu.Unlock()

	// Ghi định nghĩa VÀ lấp đầy trong cùng một transaction: một index chỉ có
	// một nửa dữ liệu là một index nói dối, và nó nói dối theo cách tệ nhất —
	// truy vấn qua nó trả về ÍT hàng hơn sự thật.
	err = c.s.Update(txn.RepeatableRead, func(tx *txn.Txn) error {
		if err := tx.Put(catKey('i', name), blob); err != nil {
			return err
		}
		t := &Tx{c: c, tx: tx}
		var scanErr error
		serr := t.ScanRows(sc, nil, nil, func(pk, row []keys.Value) bool {
			key := ix.EntryKey(nil, ix.IndexValsOf(row), pk, sc.PKOrder())
			val := []byte(nil)
			if ix.Unique {
				val = keys.Encode(nil, pk, sc.PKOrder())
			}
			if err := tx.Put(key, val); err != nil {
				scanErr = err
				return false
			}
			return true
		})
		if serr != nil {
			return serr
		}
		return scanErr
	})
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.putIndex(ix)
	c.mu.Unlock()
	return ix, nil
}
