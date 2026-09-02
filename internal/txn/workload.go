package txn

// workload.go chứa các bài dựng anomaly và bài chuyển tiền.
//
// Chúng nằm ở đây, KHÔNG nằm trong file _test.go, vì con số trong diary và con
// số trong bài test phải sinh ra từ cùng một đoạn code. Ở phase 3 đã có cùng
// quyết định này với bufpool/workload.go, và lý do vẫn thế: một bài lab in ra
// bảng đẹp mà chạy code khác với bài test là hai nguồn sự thật.

import (
	"fmt"
	"math/rand"
	"strconv"
	"sync"
	"time"
)

// ProbeWait là thời gian chờ để kết luận "kẻ kia đang bị chặn".
//
// Ở Serializable, "bị chặn" là kết cục ĐÚNG của nhiều ô trong bảng, nên bài
// dựng phải kết luận được điều đó trong vài trăm ms. Đặt Store.Locks().Timeout
// bằng đúng số này thì cái chờ luôn kết thúc trước khi ta hết kiên nhẫn.
const ProbeWait = 300 * time.Millisecond

// Anomaly là một bài dựng lại đúng một anomaly.
type Anomaly struct {
	Name string
	// Want[i] = anomaly có XẢY RA ở AllLevels[i] hay không.
	Want [4]bool
	Run  func(s *Store, l Level) (bool, error)
	Note string
}

// Anomalies là bảng deliverable của phase 6.
//
// Hàng cuối là ô quan trọng nhất của cả bảng: snapshot isolation KHÔNG chặn
// write skew. Nếu ô đó thành false thì hoặc bài dựng sai, hoặc ta đã vô tình
// cài SSI mà không biết — cả hai đều phải điều tra.
var Anomalies = []Anomaly{
	//                                      RU     RC     RR     SER
	{"dirty-read", [4]bool{true, false, false, false}, probeDirtyRead,
		"đọc được thay đổi của transaction chưa commit"},
	{"non-repeatable-read", [4]bool{true, true, false, false}, probeNonRepeatable,
		"đọc cùng một khóa hai lần, ra hai giá trị"},
	{"phantom", [4]bool{true, true, false, false}, probePhantom,
		"đếm cùng một khoảng hai lần, ra hai số"},
	{"lost-update", [4]bool{true, true, false, false}, probeLostUpdate,
		"hai transaction cùng đọc-rồi-ghi, một update bốc hơi"},
	{"write-skew", [4]bool{true, true, true, false}, probeWriteSkew,
		"hai transaction ghi hai khóa KHÁC nhau, cùng phá một bất biến"},
}

// seedKey ghi một giá trị bằng transaction riêng, ở mức cao nhất còn dùng
// MVCC — dựng sân chơi không được là phần của thí nghiệm.
func seedKey(s *Store, k, v string) error {
	return s.Update(RepeatableRead, func(tx *Txn) error { return tx.Put([]byte(k), []byte(v)) })
}

func readKey(s *Store, k string) (string, error) {
	var out string
	err := s.View(RepeatableRead, func(tx *Txn) error {
		v, ok, err := tx.Get([]byte(k))
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("khóa %q biến mất", k)
		}
		out = string(v)
		return nil
	})
	return out, err
}

func readInt(s *Store, k string) (int, error) {
	v, err := readKey(s, k)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(v)
}

// ---------- 1. dirty read ----------

func probeDirtyRead(s *Store, l Level) (bool, error) {
	if err := seedKey(s, "k", "sạch"); err != nil {
		return false, err
	}
	w, err := s.Begin(l)
	if err != nil {
		return false, err
	}
	if err := w.Put([]byte("k"), []byte("bẩn")); err != nil {
		return false, err
	}

	res := make(chan string, 1)
	go func() {
		r, err := s.Begin(l)
		if err != nil {
			res <- "lỗi-begin"
			return
		}
		defer r.Abort()
		v, _, err := r.Get([]byte("k"))
		if err != nil {
			res <- "chặn" // Serializable: lock X của w chặn lại
			return
		}
		res <- string(v)
	}()

	var got string
	select {
	case got = <-res:
	case <-time.After(2 * ProbeWait):
		got = "treo"
	}
	w.Abort()
	return got == "bẩn", nil
}

// ---------- 2. non-repeatable read ----------

func probeNonRepeatable(s *Store, l Level) (bool, error) {
	if err := seedKey(s, "k", "v0"); err != nil {
		return false, err
	}
	r, err := s.Begin(l)
	if err != nil {
		return false, err
	}
	first, _, err := r.Get([]byte("k"))
	if err != nil {
		return false, err
	}
	firstS := string(first)

	done := make(chan error, 1)
	go func() {
		done <- s.Update(l, func(tx *Txn) error { return tx.Put([]byte("k"), []byte("v1")) })
	}()
	// `blocked` phải có: kênh chỉ mang MỘT giá trị, nên nhận nó hai lần là
	// treo vĩnh viễn. Lần chạy đầu của bài này treo đủ 300 giây vì đúng lý do
	// đó — và cái sai nằm ở bài dựng, không ở lock manager.
	blocked := false
	select {
	case err := <-done:
		if err != nil {
			r.Abort()
			return false, err
		}
	case <-time.After(ProbeWait): // Serializable: writer bị lock S của r chặn
		blocked = true
	}

	second, _, err := r.Get([]byte("k"))
	if err != nil {
		return false, err
	}
	r.Abort()
	if blocked {
		<-done // thả reader ra rồi để writer đi cho hết, không bỏ goroutine lại
	}
	return firstS != string(second), nil
}

// ---------- 3. phantom ----------

func probePhantom(s *Store, l Level) (bool, error) {
	for _, k := range []string{"p1", "p2"} {
		if err := seedKey(s, k, "x"); err != nil {
			return false, err
		}
	}
	r, err := s.Begin(l)
	if err != nil {
		return false, err
	}
	n1, err := r.Count([]byte("p"), []byte("q"))
	if err != nil {
		return false, err
	}

	done := make(chan error, 1)
	go func() {
		done <- s.Update(l, func(tx *Txn) error { return tx.Put([]byte("p3"), []byte("x")) })
	}()
	blocked := false
	select {
	case err := <-done:
		if err != nil {
			r.Abort()
			return false, err
		}
	case <-time.After(ProbeWait): // Serializable: lock khoảng [p,q) chặn p3
		blocked = true
	}

	n2, err := r.Count([]byte("p"), []byte("q"))
	if err != nil {
		return false, err
	}
	r.Abort()
	if blocked {
		<-done
	}
	return n1 != n2, nil
}

// ---------- 4. lost update ----------

// probeLostUpdate: hai transaction cùng đọc số dư rồi cùng ghi số dư mới do
// CHÍNH CHÚNG tính ra. Đây là mẫu SELECT-rồi-UPDATE, và nó mất update ngay cả
// trên Postgres ở ReadCommitted — không phải khiếm khuyết của bản này.
//
// Tiêu chí phải là "kẻ nào BÁO THÀNH CÔNG thì phải được tính": số dư cuối
// phải bằng 100 - 10 × (số transaction commit được). Lấy tiêu chí "phải bằng
// 80" là sai, vì ở mức cao đúng một kẻ commit được và 90 là kết quả ĐÚNG.
func probeLostUpdate(s *Store, l Level) (bool, error) {
	if err := seedKey(s, "acct", "100"); err != nil {
		return false, err
	}
	var (
		barrier sync.WaitGroup // cả hai phải ĐỌC XONG trước khi ai được ghi
		wg      sync.WaitGroup
		mu      sync.Mutex
		commits int
	)
	barrier.Add(2)
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			released := false
			release := func() {
				if !released {
					released = true
					barrier.Done()
				}
			}
			defer release()

			tx, err := s.Begin(l)
			if err != nil {
				return
			}
			v, _, err := tx.Get([]byte("acct"))
			if err != nil {
				tx.Abort()
				return
			}
			n, _ := strconv.Atoi(string(v))
			release()
			barrier.Wait()
			if err := tx.Put([]byte("acct"), []byte(strconv.Itoa(n-10))); err != nil {
				tx.Abort()
				return
			}
			if err := tx.Commit(); err == nil {
				mu.Lock()
				commits++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	final, err := readInt(s, "acct")
	if err != nil {
		return false, err
	}
	return final != 100-10*commits, nil
}

// ---------- 5. write skew ----------

// probeWriteSkew: bất biến là wa+wb >= 0. Mỗi transaction đọc CẢ HAI, thấy
// tổng còn 100 nên rút 100 từ tài khoản CỦA RIÊNG NÓ.
//
// Đây là anomaly mà MVCC không bao giờ chặn được, và lý do rất gọn: hai
// transaction ghi hai khóa KHÁC nhau, nên không có xung đột ghi-ghi nào để
// phát hiện. Snapshot isolation kiểm "ai ghi cùng khóa với tôi"; write skew
// không phạm luật đó. Chặn nó cần biết về chỗ ĐỌC — tức lock (S2PL) hoặc SSI.
func probeWriteSkew(s *Store, l Level) (bool, error) {
	if err := seedKey(s, "wa", "50"); err != nil {
		return false, err
	}
	if err := seedKey(s, "wb", "50"); err != nil {
		return false, err
	}
	var (
		barrier sync.WaitGroup
		wg      sync.WaitGroup
	)
	barrier.Add(2)
	wg.Add(2)
	for _, self := range []string{"wa", "wb"} {
		go func(self string) {
			defer wg.Done()
			released := false
			release := func() {
				if !released {
					released = true
					barrier.Done()
				}
			}
			defer release()

			tx, err := s.Begin(l)
			if err != nil {
				return
			}
			sum, bad := 0, false
			for _, k := range []string{"wa", "wb"} {
				v, _, err := tx.Get([]byte(k))
				if err != nil {
					bad = true
					break
				}
				n, _ := strconv.Atoi(string(v))
				sum += n
			}
			release()
			barrier.Wait()
			if bad || sum < 100 {
				tx.Abort()
				return
			}
			v, _, err := tx.Get([]byte(self))
			if err != nil {
				tx.Abort()
				return
			}
			mine, _ := strconv.Atoi(string(v))
			if err := tx.Put([]byte(self), []byte(strconv.Itoa(mine-100))); err != nil {
				tx.Abort()
				return
			}
			tx.Commit()
		}(self)
	}
	wg.Wait()

	total := 0
	for _, k := range []string{"wa", "wb"} {
		n, err := readInt(s, k)
		if err != nil {
			return false, err
		}
		total += n
	}
	return total < 0, nil
}

// ---------- bài chuyển tiền ----------

// TransferResult là kết quả một lượt chạy.
type TransferResult struct {
	Level     Level
	Workers   int
	Ops       int
	Committed int
	Failed    int
	Total     int // tổng số dư đo được sau khi chạy
	Want      int // tổng số dư phải có
	Elapsed   time.Duration
}

func (r TransferResult) OK() bool { return r.Total == r.Want }

func (r TransferResult) String() string {
	verdict := "GIỮ ĐƯỢC"
	if !r.OK() {
		verdict = fmt.Sprintf("VỠ (lệch %+d)", r.Total-r.Want)
	}
	return fmt.Sprintf("%-18s commit %5d/%5d  bỏ %4d  tổng %7d/%7d  %s",
		r.Level, r.Committed, r.Ops, r.Failed, r.Total, r.Want, verdict)
}

// RunTransfers chạy workers goroutine, mỗi cái ops lần chuyển tiền ngẫu nhiên
// giữa accounts tài khoản, rồi đo lại tổng số dư.
//
// Vì sao chuyển tiền là bài toán đúng để đo isolation: nó là mẫu đọc-rồi-ghi
// trên hai khóa. Số tiền mới do ỨNG DỤNG tính ra từ giá trị vừa đọc, nên nếu
// giá trị ấy cũ đi giữa lúc đọc và lúc ghi thì tiền bốc hơi — và không có
// ràng buộc nào ở tầng dưới cứu được. Tổng số dư là bất biến kiểm được bằng
// một phép cộng, không cần biết đường đi nào đã dẫn tới đó.
func RunTransfers(s *Store, l Level, workers, ops, accounts, initial, amount int, seed int64) (TransferResult, error) {
	res := TransferResult{Level: l, Workers: workers, Ops: workers * ops, Want: accounts * initial}
	for i := 0; i < accounts; i++ {
		if err := seedKey(s, string(acct(i)), strconv.Itoa(initial)); err != nil {
			return res, err
		}
	}

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		com, bad int
	)
	start := time.Now()
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed + int64(w)))
			local, lbad := 0, 0
			for i := 0; i < ops; i++ {
				from := rng.Intn(accounts)
				to := rng.Intn(accounts)
				if from == to {
					to = (to + 1) % accounts
				}
				if err := s.Update(l, func(tx *Txn) error {
					return transferOnce(tx, from, to, amount)
				}); err != nil {
					lbad++
					continue
				}
				local++
			}
			mu.Lock()
			com += local
			bad += lbad
			mu.Unlock()
		}(w)
	}
	wg.Wait()
	res.Elapsed = time.Since(start)
	res.Committed, res.Failed = com, bad

	total := 0
	for i := 0; i < accounts; i++ {
		n, err := readInt(s, string(acct(i)))
		if err != nil {
			return res, err
		}
		total += n
	}
	res.Total = total
	return res, nil
}

func acct(i int) []byte { return []byte(fmt.Sprintf("acct%04d", i)) }

// transferOnce là chỗ anomaly sinh ra hoặc không sinh ra: ĐỌC cả hai tài
// khoản, TÍNH trong ứng dụng, rồi GHI cả hai.
func transferOnce(tx *Txn, from, to, amount int) error {
	// GetForUpdate, không phải Get: ta ĐÃ BIẾT mình sẽ ghi hai khóa này. Ở
	// Serializable, đọc bằng S rồi nâng lên X là conversion deadlock chắc
	// chắn — xem Txn.GetForUpdate. Ở ba mức kia dòng này không khác gì Get.
	fv, ok, err := tx.GetForUpdate(acct(from))
	if err != nil || !ok {
		return err
	}
	tv, ok2, err := tx.GetForUpdate(acct(to))
	if err != nil || !ok2 {
		return err
	}
	fn, err := strconv.Atoi(string(fv))
	if err != nil {
		return err
	}
	tn, err := strconv.Atoi(string(tv))
	if err != nil {
		return err
	}
	if fn < amount {
		return nil // không đủ tiền: không phải lỗi, chỉ là không chuyển
	}
	if err := tx.Put(acct(from), []byte(strconv.Itoa(fn-amount))); err != nil {
		return err
	}
	return tx.Put(acct(to), []byte(strconv.Itoa(tn+amount)))
}
