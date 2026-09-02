package db

import (
	"os"
	"path/filepath"
	"testing"

	"minidb/internal/btree"
	"minidb/internal/bufpool"
	"minidb/internal/pager"
)

// Ba câu hỏi mà phase 5 phải trả lời bằng số:
//
//  1. WAL làm một lần chèn đắt lên bao nhiêu lần? (log + fsync mỗi commit)
//  2. Gộp nhiều thay đổi vào MỘT transaction rẻ hơn bao nhiêu? (đây là hình
//     dạng thật của "group commit" khi chỉ có một writer)
//  3. Ảnh-trọn-page tốn bao nhiêu phần trăm dung lượng log?

func benchDir(b *testing.B) string {
	d := b.TempDir()
	return filepath.Join(d, "b.db")
}

// benchInsert chèn n khóa, mỗi transaction batch khóa.
func benchInsert(b *testing.B, batch int, opt Options) {
	path := benchDir(b)
	d, err := Open(path, opt)
	if err != nil {
		b.Fatal(err)
	}
	d.log.ResetStats()
	b.ResetTimer()
	i := 0
	for i < b.N {
		tx, err := d.Begin()
		if err != nil {
			b.Fatal(err)
		}
		for k := 0; k < batch && i < b.N; k++ {
			if err := tx.Put(key(i), val(i, 100)); err != nil {
				b.Fatal(err)
			}
			i++
		}
		if err := tx.Commit(); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	st := d.log.Stats()
	ps := d.pool.Stats()
	b.ReportMetric(float64(st.Bytes)/float64(b.N), "logB/op")
	b.ReportMetric(float64(st.Syncs)/float64(b.N), "fsync/op")
	b.ReportMetric(float64(ps.Writes)/float64(b.N), "pagewr/op")
	b.ReportMetric(float64(st.FullPages)/float64(b.N), "fullpg/op")
	d.Close()
	os.Remove(path)
}

// BenchmarkInsertBatch* : cùng số khóa, khác số khóa trên một transaction.
// Đây là chỗ nhìn thấy fsync đắt tới mức nào — mọi thứ khác gần như không đổi.
func BenchmarkInsertBatch1(b *testing.B)    { benchInsert(b, 1, Options{}) }
func BenchmarkInsertBatch10(b *testing.B)   { benchInsert(b, 10, Options{}) }
func BenchmarkInsertBatch100(b *testing.B)  { benchInsert(b, 100, Options{}) }
func BenchmarkInsertBatch1000(b *testing.B) { benchInsert(b, 1000, Options{}) }

// BenchmarkInsertNoSync: cùng workload, bỏ fsync. Hiệu số giữa nó và
// InsertBatch1 CHÍNH LÀ giá của durability, không lẫn thứ gì khác.
func BenchmarkInsertNoSync(b *testing.B) { benchInsert(b, 1, Options{NoSync: true}) }

// BenchmarkInsertNoFPW: tắt ảnh-trọn-page. Hiệu số là giá của việc chống torn
// write (nợ P2-3).
func BenchmarkInsertNoFPW(b *testing.B) {
	benchInsert(b, 100, Options{NoFullPageWrites: true})
}
func BenchmarkInsertFPW(b *testing.B) { benchInsert(b, 100, Options{}) }

// BenchmarkRecover đo thời gian mở lại một database sau crash, theo lượng log
// phải đọc lại. Đây là con số mà checkpoint sinh ra để kéo xuống.
//
// Cái bẫy ở đây đã cắn một lần: Open() KẾT THÚC recovery bằng một checkpoint.
// Nếu cứ Open rồi SimulateCrash trong vòng lặp thì từ vòng thứ hai trở đi
// master record đã nằm ở cuối log, không còn gì để redo, và b.N-1 vòng còn
// lại đo chi phí của checkpoint chứ không phải của recovery. Nên phải chụp
// lại đúng cặp file (data + wal) ở khoảnh khắc crash, và dán chúng về chỗ cũ
// trước mỗi vòng — với đồng hồ đã tạm dừng.
func benchRecover(b *testing.B, ckptEvery int64) { benchRecoverN(b, ckptEvery, 0) }

// cleanEvery > 0: cứ ngần ấy transaction thì ép một checkpoint CÓ flush, đóng
// vai người dọn page (page cleaner) mà minidb chưa có.
func benchRecoverN(b *testing.B, ckptEvery int64, cleanEvery int) {
	path := benchDir(b)
	d, err := Open(path, Options{Frames: 64, CheckpointBytes: ckptEvery})
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 20000; i++ {
		if err := d.Update(func(tx *Txn) error { return tx.Put(key(i), val(i, 100)) }); err != nil {
			b.Fatal(err)
		}
		if cleanEvery > 0 && i%cleanEvery == cleanEvery-1 {
			if err := d.CheckpointFlush(); err != nil {
				b.Fatal(err)
			}
		}
	}
	logLen := d.log.End()
	d.SimulateCrash()

	snapData, snapLog := mustRead(b, path), mustRead(b, path+".wal")
	restore := func() {
		if err := os.WriteFile(path, snapData, 0o644); err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(path+".wal", snapLog, 0o644); err != nil {
			b.Fatal(err)
		}
	}

	var applied, skipped int64
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		restore()
		b.StartTimer()

		d2, err := Open(path, Options{Frames: 64, CheckpointBytes: -1})
		if err != nil {
			b.Fatal(err)
		}
		applied, skipped = d2.RedoApplied, d2.RedoSkipped

		b.StopTimer()
		d2.SimulateCrash()
		b.StartTimer()
	}
	b.StopTimer()
	b.ReportMetric(float64(logLen)/1024, "logKiB")
	b.ReportMetric(float64(applied), "redo")
	b.ReportMetric(float64(skipped), "skip")
	os.Remove(path)
	os.Remove(path + ".wal")
}

func mustRead(b *testing.B, path string) []byte {
	p, err := os.ReadFile(path)
	if err != nil {
		b.Fatal(err)
	}
	return p
}

func BenchmarkRecoverNoCkpt(b *testing.B)  { benchRecover(b, -1) }
func BenchmarkRecoverCkpt1M(b *testing.B)  { benchRecover(b, 1<<20) }
func BenchmarkRecoverCkpt64K(b *testing.B) { benchRecover(b, 64<<10) }

// Cùng tần suất checkpoint như Ckpt1M, chỉ khác: có người dọn page. Chênh lệch
// giữa hai dòng này chính là phần recovery mà checkpoint MỜ không cắt nổi.
func BenchmarkRecoverCleaner(b *testing.B) { benchRecoverN(b, 1<<20, 2000) }

// Ba dòng Get đo cùng một phép tra khóa qua ba lớp bọc, để tách chi phí của
// WAL khỏi chi phí của cái bọc quanh nó:
//
//	NoWAL       cây trần, không journal
//	WithWALRaw  cây có journal móc sẵn, gọi thẳng, không qua khóa của DB
//	WithWAL     qua DB.Get, tức là có thêm d.mu
//
// Kỳ vọng: NoWAL ≈ WithWALRaw. Nếu hai con số ấy lệch nhau thì cổng
// enter/leave đang hở và đường đọc phải chụp ảnh 4KB cho mỗi lần pin. Phần
// chênh của WithWAL so với WithWALRaw là giá của d.mu, không phải của WAL.
func BenchmarkGetWithWAL(b *testing.B) {
	path := benchDir(b)
	d, err := Open(path, Options{Frames: 512})
	if err != nil {
		b.Fatal(err)
	}
	defer d.Close()
	const n = 20000
	tx, _ := d.Begin()
	for i := 0; i < n; i++ {
		tx.Put(key(i), val(i, 100))
	}
	tx.Commit()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := d.Get(key(i % n)); err != nil {
			b.Fatal(err)
		}
	}
}

// Cùng cây, cùng dữ liệu, chỉ khác: không có journal móc vào. Nếu WithWAL chậm
// hơn đáng kể thì cổng enter/leave đang hở và đường đọc đang phải chụp ảnh
// 4KB cho mỗi lần pin.
func BenchmarkGetNoWAL(b *testing.B) {
	path := benchDir(b)
	pg, err := pager.Open(path)
	if err != nil {
		b.Fatal(err)
	}
	defer pg.Close()
	pool := bufpool.New(pg, 512, bufpool.NewLRU(512))
	pool.Alloc = pg
	t, err := btree.Create(pool)
	if err != nil {
		b.Fatal(err)
	}
	const n = 20000
	for i := 0; i < n; i++ {
		if err := t.Put(key(i), val(i, 100)); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := t.Get(key(i % n)); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkGetWithWALRaw(b *testing.B) {
	path := benchDir(b)
	d, err := Open(path, Options{Frames: 512})
	if err != nil {
		b.Fatal(err)
	}
	defer d.Close()
	const n = 20000
	tx, _ := d.Begin()
	for i := 0; i < n; i++ {
		tx.Put(key(i), val(i, 100))
	}
	tx.Commit()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := d.tree.Get(key(i % n)); err != nil {
			b.Fatal(err)
		}
	}
}
