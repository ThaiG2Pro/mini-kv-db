package query

import (
	"fmt"
	"testing"

	"minidb/internal/keys"
	"minidb/internal/table"
	"minidb/internal/txn"
)

// benchRows: một cây đủ lớn để chiều cao > 2 và pool không giữ nổi hết, nếu
// không thì mọi lần "tra bảng" đều là một lần tra RAM và tỉ số CFetch/CSeq đo
// ra sẽ bé hơn sự thật. Bài học P4-6 của phase 4: ns/op của một cây nằm gọn
// trong L2 không phải số đo của một cây.
const benchRows = 20000

// BenchmarkPlanSelectivity là DELIVERABLE của phase 7: cùng một truy vấn, ba
// đường, nhiều độ chọn lọc — điểm hoà vốn nằm ở đâu.
//
// Đọc kết quả: với mỗi độ chọn lọc, so ns/op của ba nhánh. Điểm hoà vốn là độ
// chọn lọc mà seq và index đổi vai. Mô hình chi phí (DefaultCost) dự đoán
// 4.85%; con số ĐO ĐƯỢC nằm trong diary/phase7.md.
//
// Need cố tình gồm cột `payload` (không nằm trong index) cho hai nhánh
// seq/index, và chỉ gồm cột được phủ cho nhánh only — vì đó là đúng hai loại
// truy vấn khác nhau, không phải hai cách chạy một truy vấn.
func BenchmarkPlanSelectivity(b *testing.B) {
	c, sc, _ := fixture(b, benchRows)
	ix, err := c.Index("ev_kind")
	if err != nil {
		b.Fatal(err)
	}
	// kind ∈ [0, 1000) đều -> width w cho chọn lọc w/1000.
	widths := []int{1, 5, 10, 25, 50, 100, 250, 500, 1000}
	kinds := []struct {
		name string
		plan Plan
		need []int
	}{
		{"seq", Plan{Kind: SeqScan}, []int{0, 3}},
		{"index", Plan{Kind: IndexScan, Index: ix}, []int{0, 3}},
		{"indexonly", Plan{Kind: IndexOnlyScan, Index: ix}, []int{0, 1}},
	}
	for _, w := range widths {
		for _, k := range kinds {
			name := fmt.Sprintf("sel=%05.1f%%/%s", float64(w)/10, k.name)
			b.Run(name, func(b *testing.B) {
				q := Query{Table: sc, Col: 1, Lo: keys.Int(0), Hi: keys.Int(int64(w)),
					Need: k.need}
				var rows, touched int
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					err := c.View(txn.RepeatableRead, func(tx *table.Tx) error {
						r, err := Run(tx, k.plan, q, func(row []keys.Value) bool { return true })
						rows, touched = r.Rows, r.Touched
						return err
					})
					if err != nil {
						b.Fatal(err)
					}
				}
				b.StopTimer()
				b.ReportMetric(float64(rows), "rows")
				b.ReportMetric(float64(touched), "touched")
			})
		}
	}
}

// BenchmarkIndexMaintenance là cái giá của việc CÓ index, đo ở đường ghi.
// Cùng một lượng chèn, khác số index. Đây là nửa mà mọi bài "thêm index cho
// nhanh" bỏ qua.
// benchBatch là số hàng chèn trong MỘT transaction của BenchmarkIndexMaintenance.
//
// Bản đầu của benchmark ấy chèn một hàng mỗi transaction, và nó ra một kết quả
// phẳng: 1.58 / 1.77 / 1.75 / 1.73 ms cho 0/1/2/3 index. Đọc thô thì nó "chứng
// minh" index gần như miễn phí ở đường ghi — ngược hẳn bảng 2 của cmd/idxlab
// (1.66x / 2.67x / 4.08x) trên cùng công việc.
//
// Khi hai số đo mâu thuẫn thì nghi bộ đo trước, và ở đây thủ phạm lộ ra ngay
// từ độ lớn: 1.58 ms là ĐÚNG cái giá một lần fsync đã đo ở phase 0, còn việc
// bảo trì index thì cỡ vài µs. Mỗi vòng lặp một transaction nghĩa là mỗi vòng
// một lần fsync, nên 99.7% con số là durability và benchmark đang đo cái khác
// hẳn cái nó tự nhận. Đây là bài học phase 5 quay lại lần thứ ba: khi phép đo
// bị một chi phí cố định lớn trùm lên, cái nó đo là chi phí cố định ấy.
//
// Gộp lô chia lần fsync cho benchBatch hàng, và group commit của phase 5 làm
// cho con số còn lại là công việc thật.
const benchBatch = 200

func BenchmarkIndexMaintenance(b *testing.B) {
	for _, nIdx := range []int{0, 1, 2, 3} {
		b.Run(fmt.Sprintf("indexes=%d", nIdx), func(b *testing.B) {
			c, sc, _ := fixtureBare(b)
			names := []string{"m_kind", "m_city", "m_payload"}
			cols := []string{"kind", "city", "payload"}
			for i := 0; i < nIdx; i++ {
				if _, err := c.CreateIndex("events", names[i], []string{cols[i]}, false); err != nil {
					b.Fatal(err)
				}
			}
			row := func(i int) []keys.Value {
				return []keys.Value{
					keys.Uint(uint64(i)),
					keys.Int(int64(i % 1000)),
					keys.Str(fmt.Sprintf("c%02d", i%50)),
					keys.Str(fmt.Sprintf("payload-%d-xxxxxxxxxxxxxxxx", i)),
				}
			}
			b.ResetTimer()
			for i := 0; i < b.N; i += benchBatch {
				lo := i
				hi := i + benchBatch
				if hi > b.N {
					hi = b.N
				}
				err := c.Update(txn.RepeatableRead, func(tx *table.Tx) error {
					for j := lo; j < hi; j++ {
						if err := tx.Insert(sc, row(j)); err != nil {
							return err
						}
					}
					return nil
				})
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkPointLookup tách riêng CFetch: một lần tra bảng theo primary key.
// Nó là hằng số quan trọng nhất của mô hình chi phí, nên nó phải được đo chứ
// không được đoán.
//
// Đọc con số này phải kèm một điều kiện: khóa ở đây đi TĂNG DẦN (i % benchRows),
// nên nó đo ca một index có thứ tự TƯƠNG QUAN với thứ tự primary key — mỗi lần
// tra phần lớn trúng cái leaf vừa nạp. cmd/idxlab đo cả ca ngược lại (bước
// nhảy 7919 để hai lần tra liền nhau không rơi vào cùng leaf) và ra con số đắt
// hơn 1.6-2.2x. Không có MỘT CFetch đúng: có hai, và cái nào đúng phụ thuộc
// vào index đang xét. Postgres gọi thống kê ấy là `correlation` và lưu riêng
// cho từng cột — số đo ở đây là lý do nó phải lưu.
func BenchmarkPointLookup(b *testing.B) {
	c, sc, _ := fixture(b, benchRows)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		err := c.View(txn.RepeatableRead, func(tx *table.Tx) error {
			_, ok, err := tx.Get(sc, []keys.Value{keys.Uint(uint64(i % benchRows))})
			if err == nil && !ok {
				b.Fatalf("không thấy hàng %d", i%benchRows)
			}
			return err
		})
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSeqStep tách riêng CSeq: một bước đi ngang trong một lần quét.
// Chia ns/op cho số hàng để ra giá một bước.
func BenchmarkSeqStep(b *testing.B) {
	c, sc, _ := fixture(b, benchRows)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		n := 0
		err := c.View(txn.RepeatableRead, func(tx *table.Tx) error {
			return tx.ScanRows(sc, nil, nil, func(pk, row []keys.Value) bool { n++; return true })
		})
		if err != nil {
			b.Fatal(err)
		}
		if n != benchRows {
			b.Fatalf("quét được %d hàng, mong %d", n, benchRows)
		}
	}
	b.ReportMetric(float64(benchRows), "rows")
}

// BenchmarkScanLimit đo cái mà việc trả nợ P6-4 mua được: một truy vấn LIMIT 1
// trên một khoảng rộng. Bản materialize của phase 6 đọc cả khoảng rồi mới gọi
// fn lần đầu, nên nó là O(số hàng trong khoảng) cả về thời gian lẫn bộ nhớ;
// bản stream là O(1).
func BenchmarkScanLimit(b *testing.B) {
	c, sc, _ := fixture(b, benchRows)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		err := c.View(txn.RepeatableRead, func(tx *table.Tx) error {
			n := 0
			return tx.ScanRows(sc, nil, nil, func(pk, row []keys.Value) bool {
				n++
				return n < 1
			})
		})
		if err != nil {
			b.Fatal(err)
		}
	}
}
