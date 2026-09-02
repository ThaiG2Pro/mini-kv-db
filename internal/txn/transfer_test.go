package txn

import (
	"fmt"
	"strconv"
	"testing"
)

// transfer_test.go khẳng định deliverable của phase 6: N goroutine chạy đồng
// thời bài toán chuyển tiền, bất biến "tổng số dư không đổi" phải giữ được ở
// hai mức cao và phải VỠ ở hai mức thấp. Bộ workload nằm ở workload.go để
// cmd/txnlab chạy đúng cùng đoạn code.

// TestTransferInvariantPerLevel là deliverable. Nó khẳng định CẢ HAI chiều:
// hai mức cao giữ được tổng, hai mức thấp phá tổng. Nếu vế thứ hai xanh thì
// bài test này không chứng minh gì — nó chỉ chứng minh rằng workload chưa đủ
// đồng thời để anomaly kịp xảy ra.
func TestTransferInvariantPerLevel(t *testing.T) {
	const (
		workers  = 6
		ops      = 60
		accounts = 4
		initial  = 1000
		amount   = 7
	)
	type want struct {
		holds bool
	}
	expect := map[Level]want{
		ReadUncommitted: {false},
		ReadCommitted:   {false},
		RepeatableRead:  {true},
		Serializable:    {true},
	}
	for _, l := range AllLevels {
		t.Run(l.String(), func(t *testing.T) {
			s, _ := openStore(t)
			s.Locks().Timeout = 2 * ProbeWait
			res, err := RunTransfers(s, l, workers, ops, accounts, initial, amount, 42)
			if err != nil {
				t.Fatal(err)
			}
			t.Log(res.String())
			if res.OK() != expect[l].holds {
				if expect[l].holds {
					t.Fatalf("%s phải giữ được tổng, nhưng lệch %+d", l, res.Total-res.Want)
				}
				t.Fatalf("%s giữ được tổng — workload chưa đủ đồng thời để anomaly xảy ra, "+
					"nên bài test này chưa chứng minh gì", l)
			}
			if res.Committed == 0 {
				t.Fatalf("%s: không có transaction nào commit được", l)
			}
		})
	}
}

// TestTransferSerializableCommitsEverything: ở Serializable, vòng thử lại của
// Store.Update phải làm cho MỌI lượt chuyển tiền cuối cùng đều thành công.
// Nếu không, "đúng" đang được mua bằng cách âm thầm bỏ việc.
func TestTransferSerializableCommitsEverything(t *testing.T) {
	s, _ := openStore(t)
	s.Locks().Timeout = 2 * ProbeWait
	res, err := RunTransfers(s, Serializable, 4, 40, 4, 1000, 7, 7)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(res.String())
	if res.Failed != 0 {
		t.Fatalf("%d/%d lượt chuyển tiền bỏ cuộc sau %d lần thử",
			res.Failed, res.Ops, MaxRetries)
	}
	st := s.Stats()
	if st.Retries == 0 {
		t.Fatal("Retries = 0 — không có xung đột nào thì bài test này không đo được gì")
	}
	t.Logf("stats: %+v", st)
}

// TestTransferRepeatableReadRetriesNotLoses: cùng khẳng định cho snapshot
// isolation, nhưng ở đây thủ phạm bắt phải thử lại là ErrConflict chứ không
// phải deadlock. Hai cơ chế khác nhau, cùng một kết cục.
//
// 16 tài khoản, không phải 4. Với 4 tài khoản và 6 worker thì bài này đỏ
// (1/300 lượt bỏ cuộc sau 50 lần thử) — và đó không phải bug, đó là tính chất
// của điều khiển đồng thời LẠC QUAN: nó không có hàng đợi, không có thứ tự,
// nên khi tranh chấp đủ cao thì thử lại không hội tụ. Tính chất ấy được đo
// riêng ở TestOptimisticDegradesUnderContention; ở đây ta muốn đo cái khác.
func TestTransferRepeatableReadRetriesNotLoses(t *testing.T) {
	s, _ := openStore(t)
	res, err := RunTransfers(s, RepeatableRead, 6, 50, 16, 1000, 7, 11)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(res.String())
	if res.Failed != 0 {
		t.Fatalf("%d/%d lượt bỏ cuộc", res.Failed, res.Ops)
	}
	st := s.Stats()
	if st.Conflicts == 0 {
		t.Fatal("Conflicts = 0 — first-committer-wins chưa lần nào phải ra tay")
	}
	if st.Deadlocks != 0 {
		t.Fatalf("Deadlocks = %d ở RepeatableRead — mức này không lấy lock nào", st.Deadlocks)
	}
}

// TestTransferDataStillReadableAfterReopen: tiền phải còn đó sau khi đóng mở
// lại file. Nối phase 6 với phase 5 — nếu chuỗi version không bền thì mọi bảng
// bất biến ở trên chỉ đúng trong RAM.
func TestTransferDataStillReadableAfterReopen(t *testing.T) {
	s, path := openStore(t)
	res, err := RunTransfers(s, RepeatableRead, 4, 40, 4, 1000, 7, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK() {
		t.Fatalf("tổng đã lệch trước khi đóng: %+d", res.Total-res.Want)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2 := openAt(t, path)
	defer s2.Close()
	total := 0
	if err := s2.View(RepeatableRead, func(tx *Txn) error {
		for i := 0; i < 4; i++ {
			v, ok, err := tx.Get(acct(i))
			if err != nil || !ok {
				return fmt.Errorf("tài khoản %d: ok=%v %w", i, ok, err)
			}
			n, _ := strconv.Atoi(string(v))
			total += n
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if total != res.Want {
		t.Fatalf("mở lại: tổng = %d, muốn %d", total, res.Want)
	}
}

// TestOptimisticDegradesUnderContention là chỗ hai cơ chế của phase 6 đổi vai.
//
// Hai tài khoản, tám worker: MỌI transaction tranh chấp với MỌI transaction.
// Ở mức này, snapshot isolation (lạc quan: cứ làm, tới lúc commit mới kiểm)
// phải thử lại rất nhiều và có kẻ bỏ cuộc; S2PL (bi quan: xin lock trước) thì
// xếp hàng và đi được. Đây chính là điểm giao kinh điển giữa OCC và 2PL, và
// là lý do vì sao "MVCC luôn nhanh hơn lock" là một câu sai.
//
// Bài test khẳng định theo hướng SO SÁNH chứ không theo con số tuyệt đối: số
// lượt bỏ cuộc đổi theo máy và theo lịch của Go, nhưng thứ tự giữa hai mức thì
// là hệ quả của cơ chế.
func TestOptimisticDegradesUnderContention(t *testing.T) {
	const (
		workers  = 8
		ops      = 40
		accounts = 2
		initial  = 5000
	)
	run := func(l Level) TransferResult {
		s, _ := openStore(t)
		s.Locks().Timeout = 2 * ProbeWait
		res, err := RunTransfers(s, l, workers, ops, accounts, initial, 3, 5)
		if err != nil {
			t.Fatal(err)
		}
		t.Log(res.String())
		if !res.OK() {
			t.Fatalf("%s làm lệch tổng %+d ở mức tranh chấp cao", l, res.Total-res.Want)
		}
		return res
	}
	rr := run(RepeatableRead)
	ser := run(Serializable)

	if rr.Failed == 0 {
		t.Fatalf("RepeatableRead không bỏ lượt nào — %d worker trên %d tài khoản "+
			"vẫn chưa đủ tranh chấp, nên bài test này chưa chứng minh gì", workers, accounts)
	}
	if ser.Failed > rr.Failed {
		t.Fatalf("bi quan bỏ %d lượt, lạc quan bỏ %d — ngược với dự đoán, phải điều tra",
			ser.Failed, rr.Failed)
	}
	t.Logf("tranh chấp cực cao: lạc quan bỏ %d lượt, bi quan bỏ %d", rr.Failed, ser.Failed)
}
