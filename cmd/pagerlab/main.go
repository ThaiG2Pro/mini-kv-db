// pagerlab: soi file database do package pager tạo ra.
//
// Mục đích không phải benchmark mà là NHÌN TẬN MẮT hai thứ khó tin nếu chỉ đọc
// lý thuyết:
//   - sau mỗi commit, đúng MỘT trong hai meta page đổi (luân phiên theo txnID%2)
//   - page đã Free được tái dùng, nên file không phình mãi
//
// Ví dụ:
//
//	go run ./cmd/pagerlab -db data/test.db -commits 5
//	xxd -l 48 data/test.db          # 48 byte đầu = meta page A
//	xxd -s 4096 -l 48 data/test.db  # meta page B
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"minidb/internal/pager"
)

func main() {
	var (
		db      = flag.String("db", "data/test.db", "đường dẫn file database")
		commits = flag.Int("commits", 3, "số commit sẽ thực hiện")
		alloc   = flag.Int("alloc", 4, "số page cấp mới mỗi commit")
		free    = flag.Int("free", 2, "số page free mỗi commit")
		fresh   = flag.Bool("fresh", true, "xoá file cũ trước khi chạy")
	)
	flag.Parse()

	if err := run(*db, *commits, *alloc, *free, *fresh); err != nil {
		fmt.Fprintln(os.Stderr, "lỗi:", err)
		os.Exit(1)
	}
}

func run(path string, commits, alloc, free int, fresh bool) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if fresh {
		os.Remove(path)
	}

	p, err := pager.Open(path)
	if err != nil {
		return err
	}
	defer p.Close()

	fmt.Printf("mở %s: txnID=%d pageCount=%d root=%d freelistHead=%d\n",
		path, p.TxnID(), p.PageCount(), p.Root(), p.FreelistHead())
	fmt.Printf("một freelist page chứa được %d PageID\n\n", pager.FreelistPerPage())

	fmt.Printf("%-5s %-9s %-6s %-10s %-7s %-8s %-6s %-8s %s\n",
		"txn", "metaPage", "root", "pageCount", "phình", "freeList", "free", "pending", "tái dùng")

	live := []pager.PageID{}
	for i := 0; i < commits; i++ {
		startCount := p.PageCount()
		var newest pager.PageID
		for j := 0; j < alloc; j++ {
			id, err := p.Allocate()
			if err != nil {
				return err
			}
			buf := make([]byte, pager.PageSize)
			for k := range buf {
				buf[k] = byte(id)
			}
			if err := p.WritePage(id, buf); err != nil {
				return err
			}
			live = append(live, id)
			newest = id
		}
		note := ""
		for j := 0; j < free && len(live) > 1; j++ {
			id := live[0]
			live = live[1:]
			if err := p.Free(id); err != nil {
				return err
			}
			note += fmt.Sprintf("free %d ", id)
		}
		if err := p.Commit(newest); err != nil {
			return err
		}
		// phình = số page file dài thêm. Cần alloc page dữ liệu + (có thể) 1
		// page freelist; phần chênh lệch là số page lấy lại được từ freelist.
		grew := int(p.PageCount() - startCount)
		reused := alloc - grew
		tag := fmt.Sprintf("%d page lấy từ freelist", max(reused, 0))
		fmt.Printf("%-5d %-9d %-6d %-10d %-7s %-8d %-6d %-8d %s | %s\n",
			p.TxnID(), pager.MetaPageOf(p.TxnID()), p.Root(), p.PageCount(),
			fmt.Sprintf("+%d", grew), p.FreelistHead(), p.FreeCount(), p.PendingCount(),
			tag, note)
	}

	fmt.Println()
	metas, err := pager.InspectMetas(path)
	if err != nil {
		return err
	}
	for _, m := range metas {
		if !m.Valid {
			fmt.Printf("meta page %d: KHÔNG HỢP LỆ (%s)\n", m.PageID, m.Err)
			continue
		}
		fmt.Printf("meta page %d: txnID=%-4d root=%-4d freelist=%-4d pageCount=%-4d crc32c=%#08x\n",
			m.PageID, m.TxnID, m.Root, m.Freelist, m.PageCount, m.Checksum)
	}
	fmt.Println("-> chênh txnID giữa hai meta page phải là 1: cái cũ chính là đích rollback")

	chain, ids, err := pager.InspectFreelist(path, p.FreelistHead())
	if err != nil {
		return err
	}
	fmt.Printf("\nfreelist trên đĩa: %d page trong chuỗi, %d PageID rỗng: %v\n", chain, len(ids), ids)

	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	fmt.Printf("file: %d byte = %d page\n", st.Size(), st.Size()/pager.PageSize)
	return nil
}
