package pager

import (
	"fmt"
	"os"
)

// Report là kết quả soi TOÀN BỘ file — "fsck" của minidb.
//
// Mục đích: biến các invariant từ "tôi tin là đúng" thành "kiểm được bằng lệnh"
// trên một file bất kỳ, kể cả file do bản code cũ (có bug) sinh ra.
type Report struct {
	Path       string
	FileBytes  int64
	FilePages  uint32
	Metas      []MetaInfo
	ActiveMeta PageID
	PageCount  uint32 // theo meta đang có hiệu lực
	Root       PageID

	FreelistChain []PageID // các page dùng để CHỨA freelist
	FreeIDs       []PageID // các PageID đang rỗng

	// Các vấn đề tìm thấy.
	TailLeak     uint32   // số page nằm ngoài pageCount -> mồ côi (rò rỉ dung lượng)
	Duplicates   []PageID // xuất hiện >1 lần trong freelist -> double free
	OutOfRange   []PageID // free id >= pageCount
	MetaInFree   []PageID // meta page bị liệt kê là rỗng
	SelfInFree   []PageID // freelist page tự nằm trong danh sách nó ghi ra
	Unclassified uint32   // page không rỗng, không phải meta/freelist — cần phase 4 mới phân loại được
	Errors       []string
}

func (r *Report) errf(format string, a ...any) {
	r.Errors = append(r.Errors, fmt.Sprintf(format, a...))
}

// OK là true khi không có lỗi NGHIÊM TRỌNG (rò rỉ đuôi file chỉ là cảnh báo).
func (r *Report) OK() bool { return len(r.Errors) == 0 }

// Verify soi một file database mà KHÔNG sửa gì.
func Verify(path string) (*Report, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	r := &Report{Path: path, FileBytes: st.Size(), FilePages: uint32(st.Size() / PageSize)}
	if st.Size()%PageSize != 0 {
		r.errf("kích thước file %d không chia hết cho PageSize %d — file bị cắt giữa page",
			st.Size(), PageSize)
	}

	r.Metas, err = InspectMetas(path)
	if err != nil {
		return nil, err
	}
	active, found := MetaInfo{}, false
	for _, m := range r.Metas {
		if m.Valid && (!found || m.TxnID > active.TxnID) {
			active, found = m, true
		}
	}
	if !found {
		r.errf("cả hai meta page đều không hợp lệ — file không mở được")
		return r, nil
	}
	r.ActiveMeta, r.PageCount, r.Root = active.PageID, active.PageCount, active.Root

	// 1. meta nói file có bao nhiêu page, so với file thật.
	switch {
	case r.FilePages < r.PageCount:
		r.errf("file chỉ có %d page nhưng meta nói pageCount=%d — thiếu %d page",
			r.FilePages, r.PageCount, r.PageCount-r.FilePages)
	case r.FilePages > r.PageCount:
		r.TailLeak = r.FilePages - r.PageCount // cảnh báo, không phải lỗi
	}

	// 2. đi hết chuỗi freelist (InspectFreelist đã bắt vòng lặp).
	inChain := map[PageID]bool{}
	buf := make([]byte, PageSize)
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	for id := active.Freelist; id != 0; {
		if inChain[id] {
			r.errf("chuỗi freelist có vòng lặp tại page %d", id)
			break
		}
		inChain[id] = true
		r.FreelistChain = append(r.FreelistChain, id)
		if _, err := f.ReadAt(buf, int64(id)*PageSize); err != nil {
			r.errf("đọc freelist page %d: %v", id, err)
			break
		}
		n := le32(buf[offFLCount:])
		if n > freelistPerPage {
			r.errf("freelist page %d: count=%d > sức chứa %d", id, n, freelistPerPage)
			break
		}
		for i := uint32(0); i < n; i++ {
			r.FreeIDs = append(r.FreeIDs, PageID(le32(buf[offFLIDs+4*int(i):])))
		}
		id = PageID(le32(buf[offFLNext:]))
	}

	// 3. từng PageID rỗng có hợp lệ không.
	seen := map[PageID]int{}
	for _, id := range r.FreeIDs {
		seen[id]++
		if seen[id] == 2 {
			r.Duplicates = append(r.Duplicates, id)
		}
		if uint32(id) >= r.PageCount {
			r.OutOfRange = append(r.OutOfRange, id)
		}
		if id == metaPageA || id == metaPageB {
			r.MetaInFree = append(r.MetaInFree, id)
		}
		if inChain[id] {
			// ĐÂY là bug tôi đã dính ở phase 1: page chứa freelist tự nằm
			// trong danh sách nó ghi ra -> commit sau ghi đè nó.
			r.SelfInFree = append(r.SelfInFree, id)
		}
	}
	for _, e := range []struct {
		list []PageID
		msg  string
	}{
		{r.Duplicates, "double free: page %v xuất hiện nhiều lần trong freelist"},
		{r.OutOfRange, "freelist chứa page %v nằm ngoài pageCount"},
		{r.MetaInFree, "freelist chứa META page %v"},
		{r.SelfInFree, "freelist page %v tự nằm trong danh sách nó ghi ra"},
	} {
		if len(e.list) > 0 {
			r.errf(e.msg, e.list)
		}
	}

	// 4. phần còn lại: chưa phân loại được cho tới khi có B+Tree (phase 4).
	if r.PageCount >= 2 {
		used := uint32(2 + len(r.FreelistChain) + len(r.FreeIDs))
		if r.PageCount > used {
			r.Unclassified = r.PageCount - used
		}
	}
	return r, nil
}

func le32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}
