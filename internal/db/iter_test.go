package db

import (
	"bytes"
	"sync"
	"testing"
)

// TestIterCallbackCanReadBack là bài test của nợ P6-4 ở dạng nhỏ nhất, và là
// cửa vào của cả phase 7.
//
// Trước phase 7, Range giữ d.mu suốt lần duyệt, nên gọi d.Get từ trong callback
// là TỰ KHOÁ CHẾT — bài test này treo cho tới hết timeout. Đó chính là lý do
// index scan không thể tồn tại: "với mỗi entry của index, đi tra bảng theo
// primary key" là đúng cái hình này.
func TestIterCallbackCanReadBack(t *testing.T) {
	d := openTmp(t, t.TempDir(), Options{})
	defer d.Close()
	for i := 0; i < 200; i++ {
		if err := d.Update(func(tx *Txn) error { return tx.Put(key(i), val(i, 40)) }); err != nil {
			t.Fatal(err)
		}
	}
	n := 0
	err := d.Range(nil, nil, func(k, _ []byte) bool {
		// Đọc lại một khóa KHÁC từ trong callback: đây là phép truy cây lồng
		// nhau mà một index scan bắt buộc phải làm được.
		if _, err := d.Get(key(0)); err != nil {
			t.Errorf("đọc lồng nhau từ trong callback: %v", err)
			return false
		}
		n++
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 200 {
		t.Fatalf("duyệt được %d khóa, mong 200", n)
	}
}

// TestIterSurvivesWriterBetweenSteps là bản TIỀN ĐỊNH của bài trên: nó ghi
// vào cây từ chính goroutine đang duyệt, giữa hai bước Next().
//
// Bản đầu của bài test này dựa vào thời gian (một goroutine writer chạy tự do,
// rồi khẳng định Restores > 0) và nó ĐỎ NGẪU NHIÊN: lần quét 400 khóa nhanh
// hơn một lần commit có fsync, nên có lần writer không chen được vào giữa lần
// nào. Cái đỏ ấy là bộ đo sai, không phải code sai — nhưng một bài test đỏ
// ngẫu nhiên thì vô giá trị, nên nó được chia làm hai: bài này tiền định, bài
// dưới thật sự đồng thời và có bắt tay để bảo đảm có chen.
func TestIterSurvivesWriterBetweenSteps(t *testing.T) {
	d := openTmp(t, t.TempDir(), Options{Frames: 32})
	defer d.Close()
	const n = 100
	for i := 0; i < n; i++ {
		if err := d.Update(func(tx *Txn) error { return tx.Put(key(2*i), val(i, 60)) }); err != nil {
			t.Fatal(err)
		}
	}
	seen := 0
	var last []byte
	it := d.Iter(nil, nil)
	step := 0
	for it.Next() {
		k := append([]byte(nil), it.Key()...)
		if last != nil && bytes.Compare(k, last) <= 0 {
			t.Fatalf("khóa không tăng ngặt: %x sau %x", k, last)
		}
		last = k
		seen++
		step++
		// Ghi TRONG lúc duyệt: chỉ làm được vì Next() không giữ latch khi nó
		// trả về. Với bản cũ của Range thì dòng này khoá chết.
		//
		// Ghi đè khóa ĐẦU TIÊN, tức là một khóa nằm SAU lưng cursor. Bản đầu
		// của bài test này chèn key(2*step+1) — một khóa mới, luôn nằm PHÍA
		// TRƯỚC cursor — nên mỗi bước lại sinh thêm một bước, và lần duyệt
		// không bao giờ kết thúc: test treo tới hết timeout 60s. Đó là một
		// bug của bộ đo, nhưng nó dạy đúng một điều về ngữ nghĩa: một lần
		// duyệt có nhả latch KHÔNG được hứa là hữu hạn nếu writer cứ chèn vào
		// phía trước nó. Postgres gọi ca ấy là một seq scan bị "ghi đuổi", và
		// cách chặn là chốt chặn trên tại thời điểm bắt đầu — đúng cái mà một
		// snapshot MVCC làm.
		if err := d.Update(func(tx *Txn) error { return tx.Put(key(0), val(step, 60)) }); err != nil {
			t.Fatal(err)
		}
	}
	if err := it.Err(); err != nil {
		t.Fatal(err)
	}
	if it.Restores() == 0 {
		t.Fatal("Restores == 0 dù có ghi giữa MỌI bước — cơ chế tìm lại chỗ chưa chạy")
	}
	if seen != n {
		t.Fatalf("thấy %d khóa, mong đúng %d", seen, n)
	}
	t.Logf("thấy %d khóa, tìm lại chỗ %d lần", seen, it.Restores())
}

// TestIterSurvivesConcurrentWriter: writer chen vào GIỮA hai bước của một lần
// duyệt, từ một goroutine KHÁC (nên -race có việc để làm). Đây là chỗ nợ P4-5
// nói tới, và cách trả ở phase 7 là số đời cấu trúc + tìm lại chỗ theo khóa,
// chứ không phải latch-coupling.
//
// Có BẮT TAY qua channel: mỗi bước duyệt chờ writer ghi xong một khóa. Không
// có nó thì bài test phụ thuộc vào việc quét chậm hơn commit, và nó đã đỏ
// ngẫu nhiên đúng vì thế.
//
// Bất biến khẳng định: khóa trả về luôn TĂNG NGẶT (không lặp, không lùi), và
// mọi khóa có từ trước lần duyệt đều xuất hiện. Khóa được thêm giữa đường thì
// có thể thấy hoặc không — ở tầng btree thuần đó là hành vi đã ghi vào doc, và
// là lý do phải có tầng MVCC ở trên nếu muốn ảnh chụp.
func TestIterSurvivesConcurrentWriter(t *testing.T) {
	d := openTmp(t, t.TempDir(), Options{Frames: 32})
	defer d.Close()
	const n = 200
	for i := 0; i < n; i++ {
		if err := d.Update(func(tx *Txn) error { return tx.Put(key(2*i), val(i, 60)) }); err != nil {
			t.Fatal(err)
		}
	}

	wrote := make(chan struct{})
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			// Khóa lẻ: chèn vào GIỮA những khóa chẵn đang được duyệt, nên nó
			// làm split đúng cái leaf mà cursor đang đứng.
			if err := d.Update(func(tx *Txn) error {
				return tx.Put(key(2*(i%n)+1), val(i, 60))
			}); err != nil {
				t.Error(err)
				return
			}
			select {
			case wrote <- struct{}{}:
			case <-stop:
				return
			}
		}
	}()

	seen := map[string]bool{}
	var last []byte
	it := d.Iter(nil, nil)
	for {
		<-wrote // bảo đảm có đúng một lần ghi giữa hai bước
		if !it.Next() {
			break
		}
		k := append([]byte(nil), it.Key()...)
		if last != nil && bytes.Compare(k, last) <= 0 {
			t.Fatalf("khóa không tăng ngặt: %x sau %x", k, last)
		}
		last = k
		seen[string(k)] = true
	}
	close(stop)
	// Rút nốt một lần ghi có thể đang chờ gửi, để writer thoát được.
	select {
	case <-wrote:
	default:
	}
	wg.Wait()
	if err := it.Err(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if !seen[string(key(2*i))] {
			t.Fatalf("khóa %d có từ trước lần duyệt mà không thấy", 2*i)
		}
	}
	if it.Restores() == 0 {
		t.Fatal("Restores == 0: writer chưa từng chen được vào giữa," +
			" nên bài test này chưa kiểm được cái nó định kiểm")
	}
	t.Logf("thấy %d khóa, tìm lại chỗ %d lần", len(seen), it.Restores())
}
