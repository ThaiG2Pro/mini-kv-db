// idxlab là bài lab của phase 7: secondary index, khóa composite, và câu hỏi
// mà cả phase tồn tại để trả lời — index scan thắng seq scan tới độ chọn lọc
// nào.
//
// Năm bảng, năm câu hỏi:
//
//	-work breakeven : điểm hoà vốn selectivity — DELIVERABLE của phase
//	-work maintain  : cái giá của việc CÓ index, đo ở đường ghi
//	-work bytes     : hình dạng byte của khóa composite (vì sao chỉ tiền tố
//	                  bên trái dùng được, và DESC/NULL nằm ở đâu)
//	-work estimate  : ước lượng vs thật trên cột lệch — vì sao planner sai
//	-work stream    : LIMIT 1 trên khoảng rộng, cái mà nợ P6-4 mua được
//
// Mọi con số sinh ra từ internal/query/workload.go, đúng đoạn code mà
// `go test ./internal/query/` khẳng định.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"minidb/internal/db"
	"minidb/internal/keys"
	"minidb/internal/query"
	"minidb/internal/table"
	"minidb/internal/txn"
)

func main() {
	var (
		dir    = flag.String("dir", "data/idx", "thư mục làm việc")
		work   = flag.String("work", "all", "breakeven | maintain | bytes | estimate | stream | all")
		rows   = flag.Int("rows", 20000, "số hàng của bảng events")
		repeat = flag.Int("repeat", 20, "số lần chạy mỗi truy vấn (chống nhiễu đồng hồ)")
		frames = flag.Int("frames", 256, "số frame của buffer pool")
	)
	flag.Parse()

	if err := os.MkdirAll(*dir, 0o755); err != nil {
		die(err)
	}
	bad := false
	run := func(name string, fn func() error) {
		if *work != "all" && *work != name {
			return
		}
		if err := fn(); err != nil {
			fmt.Fprintf(os.Stderr, "\n%s: %v\n", name, err)
			bad = true
		}
	}

	// bytes không cần database.
	run("bytes", workBytes)
	run("breakeven", func() error { return workBreakEven(*dir, *rows, *repeat, *frames) })
	run("maintain", func() error { return workMaintain(*dir, *frames) })
	run("estimate", func() error { return workEstimate(*dir, *rows, *frames) })
	run("stream", func() error { return workStream(*dir, *rows, *frames) })

	if bad {
		os.Exit(1)
	}
}

func die(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

func openStore(dir, name string, frames int) (*txn.Store, error) {
	path := filepath.Join(dir, name)
	for _, suffix := range []string{"", ".wal"} {
		if err := os.Remove(path + suffix); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
	return txn.Open(path, db.Options{Frames: frames})
}

func hr(title string) {
	fmt.Printf("\n== %s ==\n\n", title)
}

// ---------- 1. điểm hoà vốn ----------

func workBreakEven(dir string, rows, repeat, frames int) error {
	s, err := openStore(dir, "breakeven.db", frames)
	if err != nil {
		return err
	}
	defer s.Close()
	hr(fmt.Sprintf("1. điểm hoà vốn selectivity (%d hàng, %d lần mỗi truy vấn)", rows, repeat))
	d, err := query.BuildDataset(s, rows)
	if err != nil {
		return err
	}

	// Mô hình chi phí ĐO ĐƯỢC trên máy này, thay vì ba con số đoán trong code:
	// một bước quét, một bước index, một lần tra bảng.
	c, err := measureConstants(d)
	if err != nil {
		return err
	}
	cm := query.CostFrom(c.seqStep, c.idxStep, c.fetchRand)
	cmCorr := query.CostFrom(c.seqStep, c.idxStep, c.fetchSeq)
	fmt.Printf("hằng số đo được: một bước quét %.0f ns, một bước index %.0f ns\n",
		c.seqStep, c.idxStep)
	fmt.Printf("  một lần tra bảng: %.0f ns nếu khóa NHẢY LUNG TUNG,"+
		" %.0f ns nếu khóa TĂNG DẦN (%.1fx)\n",
		c.fetchRand, c.fetchSeq, c.fetchRand/c.fetchSeq)
	fmt.Printf("  -> CFetch/CSeq = %.1fx / %.1fx"+
		" (mô hình mặc định trong code đoán %.1fx)\n",
		cm.CFetch, cmCorr.CFetch, query.DefaultCost.CFetch)
	fmt.Printf("  -> điểm hoà vốn TÍNH RA: %.2f%% / %.2f%%"+
		" (mô hình mặc định: %.2f%%)\n\n",
		query.BreakEven(cm)*100, query.BreakEven(cmCorr)*100,
		query.BreakEven(query.DefaultCost)*100)

	widths := []int{1, 5, 10, 25, 50, 100, 250, 500, 1000}
	sw, err := d.SweepBreakEven(widths, repeat, cm)
	if err != nil {
		return err
	}
	fmt.Printf("%8s %7s %10s %10s %10s %8s %8s  %-13s %-13s %s\n",
		"sel", "hàng", "seq µs", "index µs", "only µs", "idx/seq", "only/seq",
		"planner(đo)", "planner(đoán)", "thật")
	prev := ""
	for _, r := range sw {
		mark := ""
		cur := r.Best.String()
		if prev != "" && cur != prev {
			mark = "  <- ĐỔI VAI"
		}
		prev = cur
		if r.ChosenGuess != r.Best {
			mark += "  <- planner(đoán) CHỌN SAI"
		}
		if r.Chosen != r.Best {
			mark += "  <- planner(đo) CŨNG CHỌN SAI"
		}
		fmt.Printf("%7.1f%% %7d %10.1f %10.1f %10.1f %8.2f %8.2f  %-13v %-13v %v%s\n",
			r.Sel*100, r.Rows,
			float64(r.SeqNs)/1000, float64(r.IdxNs)/1000, float64(r.OnlyNs)/1000,
			r.Ratio(), r.RatioOnly(), r.Chosen, r.ChosenGuess, r.Best, mark)
	}
	fmt.Printf("\nĐọc bảng:\n")
	fmt.Printf("  · idx/seq < 1 là index thắng; only/seq < 1 ở MỌI dòng —" +
		" index phủ không có điểm hoà vốn.\n")
	fmt.Printf("  · số entry đã chạm đổi vai ở 50%%; THỜI GIAN đổi vai ở"+
		" %.1f%% — con số này\n    nội suy từ chính bảng trên, không lấy từ"+
		" mô hình nào.\n", query.CrossOver(sw)*100)
	fmt.Printf("  · ba mô hình đoán điểm ấy là %.1f%% (CFetch đo bằng khóa nhảy"+
		" lung tung),\n    %.1f%% (CFetch đo bằng khóa tăng dần) và %.1f%%"+
		" (hằng số đoán sẵn trong code).\n",
		query.BreakEven(cm)*100, query.BreakEven(cmCorr)*100,
		query.BreakEven(query.DefaultCost)*100)
	fmt.Printf("    Số đo thật %.1f%% nằm GẦN cái thứ hai. Lý do: một index"+
		" scan tra bảng theo\n    thứ tự index, và ở đây thứ tự ấy tương quan"+
		" với thứ tự primary key, nên mỗi\n    lần tra phần lớn trúng page vừa"+
		" nạp. Đo CFetch bằng khóa nhảy lung tung là\n    đo đúng cái ca xấu"+
		" nhất và đắt hơn %.1fx — chính vì thế planner(đo) vẫn chọn\n    sai ở"+
		" dải quanh %.0f%%. Postgres giải bằng một thống kê riêng cho việc này:\n"+
		"    `correlation`, tương quan giữa thứ tự index và thứ tự vật lý của"+
		" hàng.\n", query.CrossOver(sw)*100, c.fetchRand/c.fetchSeq,
		query.BreakEven(cm)*100)
	fmt.Printf("  · cột planner(đoán) dùng CFetch/CSeq = %.0f viết sẵn trong"+
		" code; số đo là %.1f.\n", query.DefaultCost.CFetch, cm.CFetch)
	fmt.Printf("    Lệch ấy làm planner chọn seq ở dải mà index còn thắng vài" +
		" lần. Đúng là\n    random_page_cost của Postgres, và là lý do mọi hướng" +
		" dẫn tuning bảo hạ nó\n    xuống trên SSD.\n")
	return nil
}

// costConsts là các hằng số của mô hình chi phí, đo trên máy đang chạy. Đo,
// không đoán — quy tắc 2 của sổ nợ.
//
// fetchRand và fetchSeq là CÙNG một phép tra bảng đo theo hai thứ tự khóa, và
// khoảng cách giữa chúng là chỗ đáng nhất của cả bảng: một hằng số chi phí
// không phải thuộc tính của phép toán, nó là thuộc tính của phép toán CỘNG VỚI
// thứ tự truy cập.
type costConsts struct {
	seqStep   float64
	idxStep   float64
	fetchRand float64 // tra bảng theo khóa nhảy lung tung: ca xấu nhất
	fetchSeq  float64 // tra bảng theo khóa tăng dần: ca một index tương quan
}

func measureConstants(d *query.Dataset) (costConsts, error) {
	var c costConsts
	ix, err := d.Cat.Index("ev_kind")
	if err != nil {
		return c, err
	}
	const reps = 5
	// một bước quét tuần tự
	t0 := time.Now()
	n := 0
	for i := 0; i < reps; i++ {
		err = d.Cat.View(txn.RepeatableRead, func(tx *table.Tx) error {
			n = 0
			return tx.ScanRows(d.Schema, nil, nil, func(pk, row []keys.Value) bool {
				n++
				return true
			})
		})
		if err != nil {
			return c, err
		}
	}
	c.seqStep = float64(time.Since(t0).Nanoseconds()) / float64(reps*n)

	// một bước quét index
	t0 = time.Now()
	m := 0
	for i := 0; i < reps; i++ {
		err = d.Cat.View(txn.RepeatableRead, func(tx *table.Tx) error {
			m = 0
			return tx.ScanIndex(ix, nil, nil, func(vals, pk []keys.Value) bool {
				m++
				return true
			})
		})
		if err != nil {
			return c, err
		}
	}
	c.idxStep = float64(time.Since(t0).Nanoseconds()) / float64(reps*m)

	// Một lần tra bảng theo pk, đo hai lần theo hai thứ tự khóa.
	//
	// Bản đầu chỉ đo ca "nhảy lung tung" (bước 7919, một số nguyên tố, để hai
	// lần tra liền nhau không rơi vào cùng một leaf) với lý lẽ viết ra hẳn
	// trong code: đo trên khóa liền nhau thì mọi lần tra đều trúng page vừa
	// nạp và con số "ra bé hơn sự thật".
	//
	// Lý lẽ ấy SAI, và cái bảng ở trên là chỗ nó bị bác: một index scan tra
	// bảng theo thứ tự index, nên nếu thứ tự ấy tương quan với thứ tự primary
	// key thì việc "trúng page vừa nạp" chính LÀ sự thật của nó. Không có một
	// CFetch đúng — có hai, và cái nào đúng phụ thuộc vào index nào.
	fetch := func(next func(i int) uint64) (float64, error) {
		const lookups = 2000
		t0 := time.Now()
		err := d.Cat.View(txn.RepeatableRead, func(tx *table.Tx) error {
			for i := 0; i < lookups; i++ {
				id := next(i)
				if _, ok, err := tx.Get(d.Schema, []keys.Value{keys.Uint(id)}); err != nil {
					return err
				} else if !ok {
					return fmt.Errorf("không thấy hàng %d", id)
				}
			}
			return nil
		})
		return float64(time.Since(t0).Nanoseconds()) / lookups, err
	}
	if c.fetchRand, err = fetch(func(i int) uint64 {
		return uint64((i * 7919) % d.Rows)
	}); err != nil {
		return c, err
	}
	// Tăng dần nhưng KHÔNG phải 0,1,2,...: bước đúng bằng d.Rows/lookups để
	// quét hết cả khoảng khóa như một index scan đầy đủ, chứ không dồn cả 2000
	// lần tra vào một góc bé của cây.
	step := uint64(d.Rows / 2000)
	if step == 0 {
		step = 1
	}
	if c.fetchSeq, err = fetch(func(i int) uint64 {
		return (uint64(i) * step) % uint64(d.Rows)
	}); err != nil {
		return c, err
	}
	return c, nil
}

// ---------- 2. thuế của index ở đường ghi ----------

func workMaintain(dir string, frames int) error {
	hr("2. cái giá của việc CÓ index (đường ghi)")
	const n = 4000
	names := []string{"m_kind", "m_city", "m_payload"}
	cols := []string{"kind", "city", "payload"}
	fmt.Printf("%8s %12s %12s %10s %13s %13s\n",
		"index", "chèn µs/hàng", "sửa µs/hàng", "so 0 index",
		"ghi idx/chèn", "ghi idx/sửa")
	var base float64
	for nIdx := 0; nIdx <= 3; nIdx++ {
		s, err := openStore(dir, fmt.Sprintf("maintain%d.db", nIdx), frames)
		if err != nil {
			return err
		}
		c, err := table.Load(s)
		if err != nil {
			s.Close()
			return err
		}
		sc, err := c.CreateTable("events", []table.Column{
			{Name: "id", T: keys.TypeUint},
			{Name: "kind", T: keys.TypeInt},
			{Name: "city", T: keys.TypeBytes},
			{Name: "payload", T: keys.TypeBytes},
		}, []string{"id"})
		if err != nil {
			s.Close()
			return err
		}
		for i := 0; i < nIdx; i++ {
			if _, err := c.CreateIndex("events", names[i], []string{cols[i]}, false); err != nil {
				s.Close()
				return err
			}
		}
		// Chèn.
		var idxWrites int
		t0 := time.Now()
		err = c.Update(txn.RepeatableRead, func(tx *table.Tx) error {
			for i := 0; i < n; i++ {
				if err := tx.Insert(sc, query.Row(i)); err != nil {
					return err
				}
			}
			idxWrites = tx.St.IndexWrites
			return nil
		})
		if err != nil {
			s.Close()
			return err
		}
		insNs := float64(time.Since(t0).Nanoseconds()) / float64(n)

		// Sửa cột payload. Ở nIdx = 0,1,2 nó KHÔNG được index; ở nIdx = 3 thì
		// có (cols[2] == "payload"). Cái ranh giới ấy là toàn bộ nội dung của
		// bảng này, và bản đầu đã đọc sai nó — xem lời đọc bảng bên dưới.
		//
		// Cột "ghi idx/sửa" được đo, không được lập luận: nó là số mục index
		// mà Upsert thật sự ghi, nên nó nói thẳng cột nào đổi có ảnh hưởng.
		var updIdxWrites int
		t0 = time.Now()
		err = c.Update(txn.RepeatableRead, func(tx *table.Tx) error {
			for i := 0; i < n; i++ {
				r := query.Row(i)
				r[3] = keys.Str(fmt.Sprintf("payload-%d-yyyy", i))
				if err := tx.Upsert(sc, r); err != nil {
					return err
				}
			}
			updIdxWrites = tx.St.IndexWrites
			return nil
		})
		if err != nil {
			s.Close()
			return err
		}
		updNs := float64(time.Since(t0).Nanoseconds()) / float64(n)
		s.Close()

		if nIdx == 0 {
			base = insNs
		}
		fmt.Printf("%8d %12.2f %12.2f %10.2fx %13.1f %13.1f\n",
			nIdx, insNs/1000, updNs/1000, insNs/base,
			float64(idxWrites)/float64(n), float64(updIdxWrites)/float64(n))
	}
	fmt.Println("\nĐọc bảng, cột CHÈN: mỗi index thêm vào làm phép chèn đắt" +
		" thêm gần đúng một lần,\nvà cột 'ghi idx/chèn' nói vì sao — một hàng" +
		" mới thì MỌI index đều phải có thêm\nmột mục. Ở đây không có gì để" +
		" tránh: đó là cái giá không thương lượng của index.\n\n" +
		"Đọc bảng, cột SỬA — và đây là chỗ bản đầu của bảng này ĐỌC SAI dữ" +
		" liệu của chính\nnó. Lời cũ viết: 'sửa vẫn đắt lên theo số index, vì" +
		" Upsert phải ĐỌC hàng cũ để\nbiết mục nào cần xoá'. Dữ liệu bác bỏ" +
		" cái nhân quả ấy: ở nIdx = 1 và 2 giá gần\nnhư không đổi so với nIdx" +
		" = 0 (chênh trong khoảng nhiễu, và không đơn điệu giữa\ncác lần" +
		" chạy), rồi nhảy hẳn một bậc ở nIdx = 3.\n\n" +
		"Cột 'ghi idx/sửa' chỉ đúng thủ phạm, và nó là số ĐO chứ không phải" +
		" lập luận:\n0, 0, 0, 2. Ba dòng đầu payload không nằm trong index nào" +
		" nên Upsert không ghi\nmục index nào cả. Dòng cuối thì cols[2] ==" +
		" \"payload\", tức cột bị sửa CHÍNH LÀ\ncột được index — và con số 2," +
		" chứ không phải 1, là vì một mục index không sửa\nđược tại chỗ: khóa" +
		" của nó CHỨA giá trị cũ, nên phải xoá mục cũ rồi chèn mục mới.\n" +
		"Đổi giá trị một cột được index là đổi VỊ TRÍ của nó trong cây.\n\n" +
		"Nên kết luận đúng ngược với kết luận cũ: giá của một UPDATE không do" +
		" SỐ index của\nbảng quyết định, mà do số index CÓ CHỨA CỘT BỊ ĐỔI —" +
		" nhân hai. Việc đọc hàng cũ có\nthật và không tránh được, nhưng nó rẻ" +
		" tới mức không hiện ra trong phép đo. Đây\nđúng là tối ưu HOT" +
		" (heap-only tuple) của Postgres: một update không đổi cột nào\nđược" +
		" index thì không sinh mục index nào — và cột 'ghi idx/sửa' = 0 là bằng" +
		"\nchứng rằng tầng bảng ở đây cũng làm thế.")
	return nil
}

// ---------- 3. hình dạng byte ----------

func workBytes() error {
	hr("3. hình dạng byte của khóa composite")
	type row struct {
		name string
		vals []keys.Value
		ord  keys.Order
	}
	fmt.Println("a) cùng một schema (city ASC, age DESC), thứ tự byte = thứ tự logic:")
	ord := keys.Order{false, true}
	rows := []row{
		{"(HN, 30)", []keys.Value{keys.Str("HN"), keys.Int(30)}, ord},
		{"(HN, 25)", []keys.Value{keys.Str("HN"), keys.Int(25)}, ord},
		{"(HN, -5)", []keys.Value{keys.Str("HN"), keys.Int(-5)}, ord},
		{"(HNX, 30)", []keys.Value{keys.Str("HNX"), keys.Int(30)}, ord},
		{"(NULL, 30)", []keys.Value{keys.Null(), keys.Int(30)}, ord},
	}
	for _, r := range rows {
		fmt.Printf("  %-11s -> %x\n", r.name, keys.Encode(nil, r.vals, r.ord))
	}
	fmt.Println("\n  age DESC hiện ra thành phép BÙ TỪNG BYTE: 30 thành ...ff-e1," +
		" 25 thành ...ff-e6.\n  Vì bù đảo thứ tự byte, DESC không cần một phép so" +
		" riêng — và cùng phép bù ấy\n  đẩy NULL xuống cuối, tức DESC ở đây là" +
		" NULLS LAST, không phải do chọn mà do hình.")

	fmt.Println("\nb) vì sao index chỉ dùng được cho TIỀN TỐ BÊN TRÁI:")
	fmt.Println("   index (city, age). Điều kiện city='HN' là một khoảng liên tục:")
	lo := keys.Encode(nil, []keys.Value{keys.Str("HN")}, ord)
	fmt.Printf("     [%x, %x)\n", lo, keys.PrefixEnd(lo))
	fmt.Println("   Điều kiện age=30 KHÔNG là khoảng nào cả — byte của age nằm SAU")
	fmt.Println("   byte của city, nên các hàng age=30 rải khắp cây:")
	for _, city := range []string{"DN", "HN", "SG"} {
		fmt.Printf("     (%s, 30) -> %x\n", city,
			keys.Encode(nil, []keys.Value{keys.Str(city), keys.Int(30)}, ord))
	}
	fmt.Println("\n   Đây là toàn bộ nội dung của 'leftmost prefix rule'. Nó không" +
		" phải một quy ước của\n   MySQL — nó là hệ quả của việc phép so duy nhất" +
		" mà B+Tree biết là memcmp.")

	fmt.Println("\nc) escape byte 0x00, và vì sao tiền tố tự đứng trước:")
	for _, s := range []string{"a", "a\x00", "a\x00b", "ab", "b"} {
		fmt.Printf("     %-8q -> %x\n", s, keys.AppendField(nil, keys.Str(s), false))
	}
	fmt.Println("   Dấu kết thúc 00-00 nhỏ hơn mọi byte nội dung, nên \"a\" <" +
		" \"ab\" mà không cần\n   lưu độ dài. Lưu độ dài lên trước là để độ dài" +
		" tham gia phép so, và \"aa\" sẽ\n   đứng SAU \"b\" — một index sai thứ tự" +
		" mà không test nào ngoài fuzz bắt được.")
	return nil
}

// ---------- 4. ước lượng vs thật ----------

func workEstimate(dir string, rows, frames int) error {
	s, err := openStore(dir, "estimate.db", frames)
	if err != nil {
		return err
	}
	defer s.Close()
	hr("4. ước lượng vs thật: cái giá của mô hình phân bố đều")
	d, err := query.BuildDataset(s, rows)
	if err != nil {
		return err
	}
	cases := []struct {
		name   string
		col    int
		lo, hi keys.Value
	}{
		{"kind < 10 (đều)", 1, keys.Int(0), keys.Int(10)},
		{"kind < 500 (đều)", 1, keys.Int(0), keys.Int(500)},
		{"city = 'HN' (lệch 99%)", 2, keys.Str("HN"), keys.Str("HO")},
		{"city = 'c001' (đuôi)", 2, keys.Str("c001"), keys.Str("c002")},
	}
	fmt.Printf("%-26s %10s %10s %9s  %s\n", "điều kiện", "ước lượng", "thật", "lệch", "planner chọn")
	for _, c := range cases {
		q := query.Query{Table: d.Schema, Col: c.col, Lo: c.lo, Hi: c.hi, Need: []int{0, 3}}
		p := query.Choose(d.Cat, d.Stats, q, query.DefaultCost)
		actual := 0
		err := d.Cat.View(txn.RepeatableRead, func(tx *table.Tx) error {
			return tx.ScanRows(d.Schema, nil, nil, func(pk, row []keys.Value) bool {
				v := row[c.col]
				if keys.Compare(v, c.lo) >= 0 && keys.Compare(v, c.hi) < 0 {
					actual++
				}
				return true
			})
		})
		if err != nil {
			return err
		}
		ratio := float64(actual) / p.Rows
		fmt.Printf("%-26s %10.0f %10d %8.1fx  %v\n", c.name, p.Rows, actual, ratio, p.Kind)
	}
	fmt.Println("\nĐọc bảng: hai dòng đầu (cột phân bố đều) khớp tới 1.0x; hai dòng" +
		" sau lệch hàng chục\nlần. Hậu quả không nằm ở con số mà ở cột cuối: với" +
		" city='HN' — 99% cả bảng —\nplanner vẫn chọn IndexScan, tức là quét index" +
		" rồi tra bảng 4950 lần để lấy gần\nnhư mọi hàng. Đó là kế hoạch TỆ NHẤT" +
		" có thể, và nó được chọn không phải vì mô hình\nchi phí sai mà vì ước" +
		" lượng số hàng sai. Histogram là chỗ Postgres bỏ tiền vào,\nvà cột 'lệch'" +
		" là lý do nó phải bỏ.")
	return nil
}

// ---------- 5. stream ----------

func workStream(dir string, rows, frames int) error {
	s, err := openStore(dir, "stream.db", frames)
	if err != nil {
		return err
	}
	defer s.Close()
	hr("5. LIMIT 1 trên khoảng rộng — cái mà nợ P6-4 mua được")
	d, err := query.BuildDataset(s, rows)
	if err != nil {
		return err
	}
	const reps = 200
	measure := func(limit int) float64 {
		t0 := time.Now()
		for i := 0; i < reps; i++ {
			n := 0
			d.Cat.View(txn.RepeatableRead, func(tx *table.Tx) error {
				return tx.ScanRows(d.Schema, nil, nil, func(pk, row []keys.Value) bool {
					n++
					return n < limit
				})
			})
		}
		return float64(time.Since(t0).Nanoseconds()) / reps
	}
	one := measure(1)
	all := measure(rows + 1)
	fmt.Printf("%-24s %12s\n", "truy vấn", "µs")
	fmt.Printf("%-24s %12.1f\n", "LIMIT 1", one/1000)
	fmt.Printf("%-24s %12.1f\n", fmt.Sprintf("quét cả %d hàng", rows), all/1000)
	fmt.Printf("\ntỉ số = %.0fx. Bản materialize của phase 6 đọc CẢ khoảng trước"+
		" khi gọi fn lần đầu,\nnên với nó hai dòng trên BẰNG NHAU — tỉ số 1x."+
		" Con số %.0fx là toàn bộ giá trị của\nviệc đổi Scan từ 'trả về một mảng'"+
		" sang 'trả về một cursor'.\n", all/one, all/one)
	return nil
}
