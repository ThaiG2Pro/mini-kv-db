// slotlab — soi một slotted page bằng mắt: chèn, xóa, compact, và xem vùng
// trống vỡ vụn ra sao. Xem diary/phase2.md.
package main

import (
	"flag"
	"fmt"
	"math/rand"
	"os"

	"minidb/internal/page"
)

func main() {
	var (
		n       = flag.Int("n", 24, "số record chèn ban đầu")
		size    = flag.Int("size", 120, "kích thước record (byte)")
		delete_ = flag.String("delete", "even", "xóa: even | random | none")
		seed    = flag.Int64("seed", 1, "seed cho -delete random")
		steps   = flag.Int("churn", 0, "số vòng xóa-rồi-chèn-lại để xem phân mảnh tích tụ")
	)
	flag.Parse()

	p := page.New(page.TypeHeap)
	var ids []page.SlotID
	for i := 0; i < *n; i++ {
		rec := make([]byte, *size)
		copy(rec, fmt.Sprintf("record-%02d", i))
		for j := len(fmt.Sprintf("record-%02d", i)); j < *size; j++ {
			rec[j] = '.'
		}
		id, err := p.Insert(rec)
		if err != nil {
			fmt.Printf("chèn được %d record thì đầy: %v\n\n", i, err)
			break
		}
		ids = append(ids, id)
	}
	show(p, fmt.Sprintf("SAU KHI CHÈN %d RECORD %dB", len(ids), *size))

	r := rand.New(rand.NewSource(*seed))
	switch *delete_ {
	case "even":
		for i := 0; i < len(ids); i += 2 {
			_ = p.Delete(ids[i])
		}
	case "random":
		for _, id := range ids {
			if r.Intn(2) == 0 {
				_ = p.Delete(id)
			}
		}
	}
	if *delete_ != "none" {
		show(p, "SAU KHI XÓA ("+*delete_+") — chú ý: trống TỔNG lớn, trống LIỀN MẠCH thì không")
	}

	before := map[page.SlotID]int{}
	for i := 0; i < p.NumSlots(); i++ {
		if p.Alive(page.SlotID(i)) {
			b, _ := p.Get(page.SlotID(i))
			before[page.SlotID(i)] = len(b)
		}
	}
	p.Compact()
	show(p, "SAU COMPACT — frag về 0, mọi SlotID giữ nguyên")
	for id, ln := range before {
		got, err := p.Get(id)
		if err != nil || len(got) != ln {
			fmt.Printf("SAI: slot %d sau compact: %v\n", id, err)
			os.Exit(1)
		}
	}
	fmt.Printf("kiểm chứng: %d SlotID cũ vẫn đọc ra đúng record cũ sau khi cell bị dời\n\n", len(before))

	if trimmed := p.TrimDeadSlots(); trimmed > 0 {
		show(p, fmt.Sprintf("SAU TrimDeadSlots — cắt %d slot chết ở đuôi, thu %d byte",
			trimmed, trimmed*4))
	}

	for i := 0; i < *steps; i++ {
		for j := 0; j < p.NumSlots(); j++ {
			if p.Alive(page.SlotID(j)) && r.Intn(2) == 0 {
				_ = p.Delete(page.SlotID(j))
			}
		}
		for {
			rec := make([]byte, *size)
			if _, err := p.Insert(rec); err != nil {
				break
			}
		}
		fmt.Printf("churn %2d: slot=%d (sống %d) liền mạch=%4d frag=%4d\n",
			i+1, p.NumSlots(), p.NumLive(), p.FreeContiguous(), p.Frag())
	}
	if *steps > 0 {
		show(p, "SAU CHURN — mảng slot chỉ mọc thêm, không co lại")
	}

	if err := p.Verify(); err != nil {
		fmt.Println("BẤT BIẾN VỠ:", err)
		os.Exit(1)
	}
	fmt.Println("Verify: mọi bất biến còn nguyên")
}

func show(p page.Page, title string) {
	fmt.Printf("== %s ==\n%s\n", title, p.Dump(6))
}
