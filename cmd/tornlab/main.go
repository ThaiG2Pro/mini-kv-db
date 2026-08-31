// tornlab — Phase 0, món nợ #1: có tồn tại torn write thật không, và ở kích thước page nào?
//
// Ý tưởng: mỗi page được ghi TOÀN BỘ bằng một "thế hệ" (generation) duy nhất —
// mọi byte thân page đều bằng byte(gen), kèm CRC. Một page hợp lệ vì vậy luôn
// đồng nhất. Nếu sau sự cố mà tìm thấy page chứa byte của HAI thế hệ khác nhau,
// đó chính là torn write: thiết bị đã ghi được một phần page rồi dừng.
//
//	tornlab -mode write  -file /mnt/x/torn.dat -pagesize 16384 -pages 4096
//	tornlab -mode verify -file /mnt/x/torn.dat -pagesize 16384
//
// QUAN TRỌNG — đọc trước khi kết luận:
//
//	kill -9 KHÔNG tạo ra torn write. Khi tiến trình chết, page cache vẫn thuộc về
//	kernel và kernel vẫn hoàn tất writeback bình thường. Muốn thấy torn write thật
//	phải cắt điện ở tầng thấp hơn tiến trình:
//	  (a) mất điện thật / rút điện máy vật lý
//	  (b) dm-flakey drop_writes — xem scripts/dm-flakey.sh  (khuyên dùng)
//	  (c) VM bị hard-reset:  virsh destroy <vm>  /  qemu: quit đột ngột
//	Do đó `-mode write` được thiết kế để chạy tới khi bị giết từ bên ngoài.
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"hash/crc32"
	"math/rand"
	"os"
	"time"
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

const magic uint32 = 0x544F524E // "TORN"

var (
	flagMode     = flag.String("mode", "verify", "write | verify")
	flagFile     = flag.String("file", "./data/torn.dat", "file thí nghiệm")
	flagPageSize = flag.Int("pagesize", 4096, "kích thước page (4096 / 8192 / 16384 / 65536)")
	flagPages    = flag.Int("pages", 4096, "số page trong file")
	flagSync     = flag.Bool("sync", false, "fsync sau mỗi page (chậm, nhưng thu hẹp cửa sổ torn)")
	flagSeconds  = flag.Int("seconds", 0, "dừng sau N giây (0 = chạy tới khi bị giết)")
)

func fill(buf []byte, gen uint64) {
	body := buf[16:]
	b := byte(gen)
	for i := range body {
		body[i] = b
	}
	binary.LittleEndian.PutUint32(buf[0:4], magic)
	binary.LittleEndian.PutUint64(buf[4:12], gen)
	binary.LittleEndian.PutUint32(buf[12:16], crc32.Checksum(body, castagnoli))
}

// check phân loại một page: ok | torn | crc | empty | badmagic.
func check(buf []byte) string {
	allZero := true
	for _, b := range buf {
		if b != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		return "empty"
	}
	if binary.LittleEndian.Uint32(buf[0:4]) != magic {
		return "badmagic"
	}
	gen := binary.LittleEndian.Uint64(buf[4:12])
	body := buf[16:]

	// Dấu hiệu TRỰC TIẾP của torn write: thân page lẽ ra đồng nhất mà lại pha trộn.
	want := byte(gen)
	mixed := false
	for _, b := range body {
		if b != want {
			mixed = true
			break
		}
	}
	if mixed {
		return "torn"
	}
	if crc32.Checksum(body, castagnoli) != binary.LittleEndian.Uint32(buf[12:16]) {
		return "crc"
	}
	return "ok"
}

func doWrite() {
	f, err := os.OpenFile(*flagFile, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		panic(err)
	}
	defer f.Close()

	ps := int64(*flagPageSize)
	if err := f.Truncate(ps * int64(*flagPages)); err != nil {
		panic(err)
	}

	buf := make([]byte, ps)
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	deadline := time.Time{}
	if *flagSeconds > 0 {
		deadline = time.Now().Add(time.Duration(*flagSeconds) * time.Second)
	}

	fmt.Printf("tornlab write: %s, page %d B, %d page. Ctrl-C hoặc cắt điện để dừng.\n",
		*flagFile, ps, *flagPages)

	var gen uint64
	var n uint64
	tick := time.Now()
	for {
		gen++
		fill(buf, gen)
		off := rng.Int63n(int64(*flagPages)) * ps
		if _, err := f.WriteAt(buf, off); err != nil {
			panic(err)
		}
		if *flagSync {
			if err := f.Sync(); err != nil {
				panic(err)
			}
		}
		n++
		if time.Since(tick) > 2*time.Second {
			fmt.Printf("  ...đã ghi %d page\n", n)
			tick = time.Now()
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			fmt.Printf("hết giờ, đã ghi %d page\n", n)
			return
		}
	}
}

func doVerify() {
	f, err := os.Open(*flagFile)
	if err != nil {
		panic(err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		panic(err)
	}

	ps := int64(*flagPageSize)
	pages := st.Size() / ps
	buf := make([]byte, ps)
	counts := map[string]int{}
	var tornOffsets []int64

	for i := int64(0); i < pages; i++ {
		if _, err := f.ReadAt(buf, i*ps); err != nil {
			panic(err)
		}
		st := check(buf)
		counts[st]++
		if (st == "torn" || st == "crc") && len(tornOffsets) < 10 {
			tornOffsets = append(tornOffsets, i*ps)
		}
	}

	fmt.Printf("tornlab verify: %s, page %d B, %d page\n", *flagFile, ps, pages)
	for _, k := range []string{"ok", "empty", "torn", "crc", "badmagic"} {
		if counts[k] > 0 {
			fmt.Printf("  %-9s %d\n", k, counts[k])
		}
	}
	if len(tornOffsets) > 0 {
		fmt.Printf("  offset hỏng đầu tiên: %v\n", tornOffsets)
	}

	switch {
	case counts["torn"] > 0:
		fmt.Printf("\n=> CÓ TORN WRITE ở page %d B. Đây là bằng chứng thực nghiệm rằng\n"+
			"   page lớn hơn sector KHÔNG atomic -> minidb cần checksum + WAL để phát hiện và sửa.\n", ps)
	case counts["crc"] > 0:
		fmt.Printf("\n=> Không thấy page pha trộn thế hệ, nhưng CRC sai: dữ liệu hỏng ở dạng khác\n" +
			"   (bit rot, hoặc ghi thiếu phần đuôi). Checksum vẫn là thứ bắt được nó.\n")
	default:
		fmt.Printf("\n=> Không phát hiện torn write LẦN NÀY. Chưa chứng minh được là không thể xảy ra —\n" +
			"   chỉ có nghĩa là cửa sổ lỗi chưa trúng. Thử page lớn hơn (-pagesize 65536),\n" +
			"   bỏ -sync, hoặc dùng dm-flakey (scripts/dm-flakey.sh) thay vì kill tiến trình.\n")
	}
}

func main() {
	flag.Parse()
	if *flagPageSize < 32 {
		panic("pagesize quá nhỏ")
	}
	switch *flagMode {
	case "write":
		doWrite()
	case "verify":
		doVerify()
	default:
		fmt.Println("-mode phải là write hoặc verify")
		os.Exit(2)
	}
}
