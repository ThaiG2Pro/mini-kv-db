// dbcheck: fsck cho file minidb. Không sửa gì, chỉ soi và báo.
//
//	go run ./cmd/dbcheck data/test.db
//
// Thoát với mã 1 nếu có lỗi nghiêm trọng — dùng được trong script/CI.
package main

import (
	"fmt"
	"os"

	"minidb/internal/pager"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "dùng: dbcheck <file.db> [file.db ...]")
		os.Exit(2)
	}
	bad := false
	for _, path := range os.Args[1:] {
		r, err := pager.Verify(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", path, err)
			bad = true
			continue
		}
		report(r)
		if !r.OK() {
			bad = true
		}
	}
	if bad {
		os.Exit(1)
	}
}

func report(r *pager.Report) {
	fmt.Printf("%s: %d byte = %d page\n", r.Path, r.FileBytes, r.FilePages)
	for _, m := range r.Metas {
		if !m.Valid {
			fmt.Printf("  meta %d: HỎNG — %s\n", m.PageID, m.Err)
			continue
		}
		mark := " "
		if m.PageID == r.ActiveMeta {
			mark = "*"
		}
		fmt.Printf("  meta %d:%s txnID=%-4d root=%-4d freelist=%-4d pageCount=%-4d crc32c=%#08x\n",
			m.PageID, mark, m.TxnID, m.Root, m.Freelist, m.PageCount, m.Checksum)
	}
	fmt.Printf("  (* = meta đang có hiệu lực; cái còn lại là đích rollback)\n")
	fmt.Printf("  freelist: %d page trong chuỗi %v, %d PageID rỗng\n",
		len(r.FreelistChain), r.FreelistChain, len(r.FreeIDs))
	fmt.Printf("  chưa phân loại được: %d page (cần B+Tree ở phase 4 mới truy được)\n", r.Unclassified)

	if r.TailLeak > 0 {
		fmt.Printf("  ⚠ RÒ RỈ ĐUÔI FILE: %d page nằm ngoài pageCount=%d — page mồ côi sau rollback\n",
			r.TailLeak, r.PageCount)
	}
	for _, e := range r.Errors {
		fmt.Printf("  ✗ %s\n", e)
	}
	if r.OK() {
		fmt.Printf("  ✓ không có lỗi nghiêm trọng\n")
	}
}
