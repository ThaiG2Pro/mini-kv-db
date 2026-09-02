package txn

import (
	"bytes"
	"fmt"
	"math"
	"path/filepath"
	"testing"

	"minidb/internal/db"
)

// FuzzChainCodec đánh vào hai chỗ mà mắt người đọc không nhìn ra được.
//
// Chỗ thứ nhất: DecodeChain đọc byte từ đĩa, tức đọc dữ liệu KHÔNG đáng tin.
// Một chuỗi version hỏng (torn write mà checksum sót, hoặc một bug ở tầng
// dưới) không được phép làm panic — nó phải thành một error có tên. Cả
// ChainStats.BadChains sinh ra là để đếm đúng loại này.
//
// Chỗ thứ hai, và là chỗ đáng giá hơn: bất biến ĐỊNH NGHĨA của Prune. Prune
// chỉ được xoá những version mà không snapshot hợp lệ nào còn nhìn thấy, nên
// với mọi horizon h và mọi snapshot s có s.Xmin >= h:
//
//	c.Visible(s) == c.Prune(h).Visible(s)
//
// Đây đúng là chỗ con bug min(Xmax) thay vì min(Xmin) đã nằm, và nó chỉ hiện
// ra khi có ba transaction gối nhau — một hình mà tôi phải dựng bằng tay ở
// version_test.go. Fuzzer thì không cần biết trước hình nào, nó thử hết.
func FuzzChainCodec(f *testing.F) {
	f.Add([]byte{1, 0, 0, 0})
	f.Add([]byte{3, 9, 5, 1})
	f.Add([]byte{5, 20, 20, 7, 7, 1})
	f.Add(make([]byte, 40))

	f.Fuzz(func(t *testing.T, script []byte) {
		if len(script) > 200 {
			script = script[:200]
		}

		// 1. Giải mã byte rác: được phép trả error, không được phép panic.
		//    Nếu giải mã ĐƯỢC thì mã hoá lại phải ra đúng byte cũ — layout là
		//    cố định nên nó phải canonical, không có chỗ cho hai cách viết
		//    cùng một chuỗi.
		if c, err := DecodeChain(script); err == nil {
			again := c.Encode(make([]byte, 0, c.EncodedSize()))
			if !bytes.Equal(again, script) {
				t.Fatalf("mã hoá lại khác byte gốc:\n gốc %x\n lại %x", script, again)
			}
		}

		// 2. Dựng một chuỗi HỢP LỆ từ script rồi thử mọi horizon.
		//    xmin phải giảm dần (mới nhất đứng đầu) và > 0.
		var c Chain
		xmin := uint64(len(script)*2 + 3)
		for i, b := range script {
			if i >= MaxVersions {
				break
			}
			if xmin <= 1 {
				break
			}
			step := uint64(b%3) + 1
			if step >= xmin {
				break
			}
			xmin -= step
			c = append(c, Version{
				Xmin:    xmin,
				Deleted: b%7 == 0,
				Val:     []byte{b},
			})
		}
		if len(c) == 0 {
			return
		}
		if _, err := DecodeChain(c.Encode(nil)); err != nil {
			t.Fatalf("chuỗi tự dựng lại không giải mã được: %v", err)
		}

		top := c[0].Xmin + 2
		for h := uint64(0); h <= top; h++ {
			pruned, dropped := c.Prune(h)
			if dropped < 0 || len(pruned)+dropped != len(c) {
				t.Fatalf("horizon %d: dọn %d version nhưng %d -> %d",
					h, dropped, len(c), len(pruned))
			}
			if len(pruned) == 0 {
				t.Fatalf("horizon %d: Prune xoá SẠCH chuỗi — không bao giờ được"+
					" phép, khóa sẽ biến mất khỏi cây", h)
			}
			// Chuỗi sau khi dọn vẫn phải giảm dần.
			for i := 1; i < len(pruned); i++ {
				if pruned[i-1].Xmin <= pruned[i].Xmin {
					t.Fatalf("horizon %d: thứ tự chuỗi vỡ sau Prune: %v",
						h, pruned)
				}
			}
			// Bất biến định nghĩa.
			for x := uint64(0); x <= top; x++ {
				if x < h {
					continue // snapshot không hợp lệ: Xmin > Xmax
				}
				s := Snapshot{Xmax: x, Xmin: h, Active: map[uint64]bool{}}
				vWant, okWant := c.Visible(s)
				vGot, okGot := pruned.Visible(s)
				if okWant != okGot {
					t.Fatalf("horizon %d snapshot Xmax=%d: trước Prune thấy=%v,"+
						" sau Prune thấy=%v\nchuỗi %v\nsau   %v",
						h, x, okWant, okGot, c, pruned)
				}
				if okWant && vWant.Xmin != vGot.Xmin {
					t.Fatalf("horizon %d snapshot Xmax=%d: Prune đổi version"+
						" nhìn thấy được, %d -> %d", h, x, vWant.Xmin, vGot.Xmin)
				}
			}
		}
	})
}

// FuzzTxnCrash nối phase 6 vào phase 5. Chuỗi version là một encoding mới nằm
// TRONG value của B+Tree, nên nó đi qua WAL, qua checkpoint, qua pha redo và
// pha undo mà không tầng nào biết nó là gì. Câu hỏi cần trả lời bằng thí
// nghiệm chứ không bằng suy luận: sau kill -9, có chuỗi nào giải mã ra rác?
//
// Bất biến kiểm sau mỗi lần crash:
//
//  1. cây hợp lệ (bảy bất biến của phase 4 — recovery không được làm hỏng cây);
//  2. MỌI value trong cây giải mã được thành chuỗi version (BadChains == 0);
//  3. đọc ở Latest ra đúng những gì các transaction đã commit ghi — không
//     thiếu (mất durability) và không thừa (transaction dở dang sống sót).
func FuzzTxnCrash(f *testing.F) {
	f.Add([]byte{1, 2, 3, 4, 5, 6, 7, 8})
	f.Add([]byte{9, 9, 4, 4, 9, 9, 4, 4, 1, 1})
	f.Add(make([]byte, 48))

	f.Fuzz(func(t *testing.T, script []byte) {
		if len(script) > 600 {
			script = script[:600]
		}
		path := filepath.Join(t.TempDir(), "f.db")
		// Pool 6 frame: page bẩn bị ép ra đĩa liên tục nên pha undo thật sự
		// phải chạy, không chỉ pha redo. Cùng lý do như FuzzCrashRecover.
		opt := db.Options{Frames: 6, CheckpointBytes: 8 << 10}
		d, err := db.Open(path, opt)
		if err != nil {
			t.Fatal(err)
		}
		s, err := Wrap(d)
		if err != nil {
			t.Fatal(err)
		}

		want := map[string]string{} // chỉ những gì đã commit
		level := AllLevels[0]

		reopen := func() {
			if err := d.SimulateCrash(); err != nil {
				t.Fatal(err)
			}
			d, err = db.Open(path, opt)
			if err != nil {
				t.Fatalf("mở lại: %v", err)
			}
			if s, err = Wrap(d); err != nil {
				t.Fatalf("wrap lại: %v", err)
			}
			rep, err := d.Tree().Verify()
			if err != nil {
				t.Fatal(err)
			}
			if !rep.OK() {
				t.Fatalf("cây hỏng sau recovery: %v", rep.Errors)
			}
			cs, err := s.ChainStats()
			if err != nil {
				t.Fatalf("soi chuỗi: %v", err)
			}
			if cs.BadChains != 0 {
				t.Fatalf("%d/%d chuỗi version giải mã ra rác sau recovery",
					cs.BadChains, cs.Keys)
			}
			// Đọc ở Latest: thấy version mới nhất của mọi khóa còn sống.
			got := map[string]string{}
			if err := s.View(RepeatableRead, func(tx *Txn) error {
				return tx.Scan(nil, nil, func(k, v []byte) bool {
					got[string(k)] = string(v)
					return true
				})
			}); err != nil {
				t.Fatalf("quét sau recovery: %v", err)
			}
			if len(got) != len(want) {
				t.Fatalf("sau recovery đọc được %d khóa, mong %d", len(got), len(want))
			}
			for k, v := range want {
				if got[k] != v {
					t.Fatalf("khóa %q sau recovery = %q, mong %q", k, got[k], v)
				}
			}
		}

		for i := 0; i < len(script); i++ {
			op := script[i]
			k := []byte(fmt.Sprintf("k%02d", int(op)%20))
			v := fmt.Sprintf("v%d-%d", op, i)
			switch op % 8 {
			case 0, 1, 2:
				if err := s.Update(level, func(tx *Txn) error {
					return tx.Put(k, []byte(v))
				}); err != nil {
					t.Fatalf("put: %v", err)
				}
				want[string(k)] = v
			case 3:
				if err := s.Update(level, func(tx *Txn) error {
					return tx.Delete(k)
				}); err != nil {
					t.Fatalf("delete: %v", err)
				}
				delete(want, string(k))
			case 4:
				// Transaction bỏ dở: write set phải bốc hơi không để lại gì.
				err := s.Update(level, func(tx *Txn) error {
					if err := tx.Put(k, []byte(v)); err != nil {
						return err
					}
					return errFuzzAbort
				})
				if err != errFuzzAbort {
					t.Fatalf("abort: mong errFuzzAbort, được %v", err)
				}
			case 5:
				if _, err := s.Vacuum(); err != nil {
					t.Fatalf("vacuum: %v", err)
				}
			case 6:
				level = AllLevels[int(op)%len(AllLevels)]
			case 7:
				reopen()
			}
		}
		reopen()
		if err := s.Close(); err != nil {
			t.Fatalf("đóng: %v", err)
		}
	})
}

var errFuzzAbort = fmt.Errorf("fuzz: bỏ dở có chủ ý")

// Chặn một sai sót lặng lẽ: nếu MaxVersions vượt quá cái mà một entry B+Tree
// chứa nổi thì trần thật là trần BYTE, và fuzzer ở trên sẽ dựng những chuỗi
// dài hơn mức apply() cho phép mà không ai biết. Con số 2028 là
// btree.MaxEntrySize, đã đo ở phase 4.
func TestMaxVersionsFitsAnEntry(t *testing.T) {
	c := make(Chain, MaxVersions)
	for i := range c {
		c[i] = Version{Xmin: uint64(MaxVersions - i)}
	}
	if n := c.EncodedSize(); n > 2028 {
		t.Fatalf("chuỗi %d version rỗng đã chiếm %d byte, vượt MaxEntrySize",
			MaxVersions, n)
	}
	if math.MaxUint8 < MaxVersions {
		t.Fatalf("MaxVersions=%d không lọt vào byte đếm của encoding", MaxVersions)
	}
}
