package pager

import (
	"encoding/binary"
	"fmt"
	"os"
)

// MetaInfo là nội dung một meta page đọc thẳng từ đĩa, KHÔNG qua logic chọn
// meta. Dùng để soi file bằng mắt và để dạy/kiểm chứng: sau mỗi commit phải
// thấy đúng một page đổi.
type MetaInfo struct {
	PageID    PageID
	Valid     bool
	Err       string
	Root      PageID
	Freelist  PageID
	TxnID     uint64
	PageCount uint32
	Checksum  uint32
}

// InspectMetas trả về cả hai meta page của file.
func InspectMetas(path string) ([]MetaInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	out := make([]MetaInfo, 0, 2)
	for _, id := range []PageID{metaPageA, metaPageB} {
		info := MetaInfo{PageID: id}
		buf := make([]byte, PageSize)
		if _, err := f.ReadAt(buf, int64(id)*PageSize); err != nil {
			info.Err = err.Error()
			out = append(out, info)
			continue
		}
		info.Checksum = binary.LittleEndian.Uint32(buf[offChecksum:])
		m, err := decodeMeta(buf)
		if err != nil {
			info.Err = err.Error()
			out = append(out, info)
			continue
		}
		info.Valid = true
		info.Root, info.Freelist, info.TxnID, info.PageCount = m.root, m.freelist, m.txnID, m.pageCount
		out = append(out, info)
	}
	return out, nil
}

// InspectFreelist đi hết chuỗi freelist từ head, trả về số page trong chuỗi và
// tổng số PageID đang rỗng.
func InspectFreelist(path string, head PageID) (chain int, ids []PageID, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, nil, err
	}
	defer f.Close()

	buf := make([]byte, PageSize)
	seen := map[PageID]bool{}
	for id := head; id != 0; chain++ {
		if seen[id] {
			return chain, ids, fmt.Errorf("chuỗi freelist có vòng lặp tại page %d", id)
		}
		seen[id] = true
		if _, err := f.ReadAt(buf, int64(id)*PageSize); err != nil {
			return chain, ids, err
		}
		n := binary.LittleEndian.Uint32(buf[offFLCount:])
		if n > freelistPerPage {
			return chain, ids, fmt.Errorf("freelist page %d: count=%d vô lý", id, n)
		}
		for i := uint32(0); i < n; i++ {
			ids = append(ids, PageID(binary.LittleEndian.Uint32(buf[offFLIDs+4*int(i):])))
		}
		id = PageID(binary.LittleEndian.Uint32(buf[offFLNext:]))
	}
	return chain, ids, nil
}

// FreelistPerPage cho biết một freelist page chứa được bao nhiêu PageID.
func FreelistPerPage() int { return freelistPerPage }
