// wallab — soi một file WAL: log dài bao nhiêu, gồm những gì, và bao nhiêu
// phần trăm trong đó là "thuế" chứ không phải dữ liệu.
//
// Câu hỏi nó sinh ra để trả lời: một lần chèn khóa tốn bao nhiêu byte log so
// với 4096 byte của page mà nó sửa? Đó là write amplification của tầng
// recovery, và là lý do physiological logging tồn tại thay vì log trọn page.
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"

	"minidb/internal/wal"
)

func main() {
	var (
		tail = flag.Int("tail", 0, "in N record cuối")
		head = flag.Int("head", 0, "in N record đầu")
		page = flag.Int("page", -1, "chỉ in record của page này")
	)
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "dùng: wallab [-tail N] [-head N] [-page ID] <file.wal>")
		os.Exit(2)
	}
	l, err := wal.Open(flag.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer l.Close()

	type agg struct {
		n, bytes, payload, dataBytes int64
	}
	byType := map[wal.Type]*agg{}
	var all []wal.Record
	var nFull, nSeg, totalRec int64
	pages := map[uint32]int{}

	err = l.Scan(wal.FirstLSN, func(r *wal.Record) error {
		totalRec++
		a := byType[r.Type]
		if a == nil {
			a = &agg{}
			byType[r.Type] = a
		}
		a.n++
		a.bytes += int64(r.Size())
		a.payload += int64(len(r.Payload))
		if r.Flags&wal.FlagFullPage != 0 {
			nFull++
		}
		if r.PageID != 0 {
			pages[uint32(r.PageID)]++
		}
		if sb, sa, _, _, err := wal.DecodePayload(r.Payload); err == nil {
			nSeg += int64(len(sb) + len(sa))
			a.dataBytes += int64(wal.SegBytes(sb) + wal.SegBytes(sa))
		}
		if *tail > 0 || *head > 0 {
			c := *r
			c.Payload = nil
			all = append(all, c)
		}
		return nil
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	fmt.Printf("file      %s\n", flag.Arg(0))
	fmt.Printf("độ dài    %d byte, %d record, checkpoint gần nhất tại LSN %d\n",
		l.End()-wal.FirstLSN, totalRec, l.Checkpoint())
	fmt.Printf("page bị chạm %d, ảnh trọn page %d record\n\n", len(pages), nFull)

	fmt.Printf("%-12s %8s %12s %12s %10s\n", "loại", "số", "byte log", "byte dữ liệu", "byte/record")
	types := make([]wal.Type, 0, len(byType))
	for k := range byType {
		types = append(types, k)
	}
	sort.Slice(types, func(i, j int) bool { return types[i] < types[j] })
	var sumBytes, sumData int64
	for _, tp := range types {
		a := byType[tp]
		sumBytes += a.bytes
		sumData += a.dataBytes
		fmt.Printf("%-12s %8d %12d %12d %10.0f\n", tp, a.n, a.bytes, a.dataBytes, float64(a.bytes)/float64(a.n))
	}
	fmt.Printf("%-12s %8d %12d %12d\n", "TỔNG", totalRec, sumBytes, sumData)
	if u := byType[wal.TypeUpdate]; u != nil && u.n > 0 {
		fmt.Printf("\nUPDATE: %.0f byte log / record, %.0f byte thật sự đổi, %.1f đoạn / record\n",
			float64(u.bytes)/float64(u.n), float64(u.dataBytes)/float64(u.n), float64(nSeg)/float64(totalRec))
		fmt.Printf("so với ghi trọn page 4096B: %.1fx tiết kiệm\n",
			4096.0*float64(u.n)/float64(u.bytes))
	}

	if *tail > 0 || *head > 0 {
		fmt.Printf("\n%-10s %-11s %6s %8s %9s %9s %8s\n", "lsn", "loại", "txn", "page", "prev", "undoNext", "cờ")
		lo, hi := 0, len(all)
		if *head > 0 && *head < hi {
			hi = *head
		}
		if *tail > 0 && len(all)-*tail > lo {
			lo = len(all) - *tail
			hi = len(all)
		}
		for _, r := range all[lo:hi] {
			if *page >= 0 && int(r.PageID) != *page {
				continue
			}
			fmt.Printf("%-10d %-11s %6d %8d %9d %9d %8d\n",
				r.LSN, r.Type, r.TxnID, r.PageID, r.PrevLSN, r.UndoNext, r.Flags)
		}
	}
}
