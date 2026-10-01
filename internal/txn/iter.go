package txn

import (
	"bytes"
	"fmt"

	"minidb/internal/db"
	"minidb/internal/lock"
)

// iter.go biến đường đọc từ PUSH thành PULL.
//
// Phase 6 và phase 7 dựng mọi phép quét theo kiểu push: `Scan(lo, hi, fn)`
// nắm vòng lặp và gọi callback. Nó gọn và không cấp phát, và với một truy vấn
// một-bảng thì không thiếu gì.
//
// Phase 8 phá được cái đó ngay ở toán tử đầu tiên. Một nested loop join phải
// duyệt lại vòng TRONG một lần cho mỗi hàng của vòng NGOÀI; một hash join phải
// hút cạn build side rồi mới bắt đầu đọc probe side. Cả hai đều cần quyền
// **hỏi hàng kế tiếp**, mà một callback thì không có quyền đó — nó chỉ được
// gọi. Với API push, chỉ còn ba đường, và cả ba đều tệ:
//
//   - đệm trọn một vế vào RAM: mất hẳn tính streaming, và một join hai bảng
//     lớn thì không chạy nổi;
//   - dựng goroutine + channel cho mỗi vế: một lần chuyển ngữ cảnh mỗi hàng,
//     và tệ hơn là txn.Txn không an toàn để dùng từ nhiều goroutine;
//   - đảo ngược điều khiển bằng continuation: đúng cái mà Volcano tránh.
//
// Nên phase 8 mở đầu bằng việc trả nợ hình dạng: một Iter kéo (pull) làm
// **bản cài duy nhất** của phép trộn, và Scan trở thành lớp vỏ mỏng trên nó.
// Hai bản sao của một phép trộn hai luồng là hai chỗ để lệch nhau — bài học
// từ internal/query/workload.go của phase 7.
//
// SQLite chọn đường khác cho cùng vấn đề: nó biên dịch truy vấn thành bytecode
// cho một máy ảo, nên "vòng lặp" nằm ở VM và mọi toán tử đều là coroutine.
// Postgres thì chính là Volcano: ExecProcNode kéo từng tuple.

// Iter là con trỏ đọc theo thứ tự khóa tăng dần, có ngữ nghĩa MVCC đầy đủ:
// nó trộn hai luồng đã sắp thứ tự — cây (qua ảnh chụp) và write set của chính
// transaction này.
//
// Ba tính chất của phép trộn ấy, giữ nguyên từ phase 7 và giờ là tính chất
// của Iter:
//
//   - bộ nhớ O(số khóa BẨN của chính transaction), không phải O(số khóa trong
//     khoảng). Quét cả bảng với write set rỗng là O(1).
//   - người gọi được phép GỌI LẠI vào Get/Iter trong lúc đang duyệt. Không có
//     tính chất này thì không có index scan, và cũng không có nested loop join.
//   - hàng đầu tiên có sau ~một lần xuống cây, không phải sau khi đã đọc hết
//     khoảng. Đó là khác biệt giữa "trả về một mảng" và "trả về một cursor",
//     và là lý do LIMIT trong SQL có nghĩa.
//
// Write set VẪN được chép ra và sắp lại lúc mở, không duyệt trực tiếp trên
// t.ws: người gọi có quyền Put trong lúc đang duyệt, và ngữ nghĩa đúng là một
// câu lệnh nhìn thấy trạng thái lúc nó BẮT ĐẦU. Duyệt trực tiếp thì vừa sai
// ngữ nghĩa vừa panic ngay.
//
// Không giữ latch nào giữa hai lần Next: cursor của phase 4 tự tìm lại chỗ
// theo khóa nếu cấu trúc cây đổi (nợ P4-5, trả ở phase 7). Đó là điều kiện để
// một toán tử ở tầng trên gọi Get lồng trong lúc đang duyệt — chính là hình
// của index scan và của nested loop join.
type Iter struct {
	t    *Txn
	it   *db.Iter
	own  []ownKV
	snap Snapshot

	// tk/tv là hàng ĐỌC TRƯỚC từ cây; tvalid = còn hàng.
	tk, tv []byte
	tvalid bool

	// pend: lần Next tới phải đẩy con trỏ cây lên một bước trước khi quyết
	// định. Có cờ này để KHÔNG phải chép tk/tv ra buffer riêng: chừng nào
	// chưa advance thì tk/tv còn nguyên, nên người gọi cầm được Key()/Value()
	// suốt thời gian giữa hai lần Next. Bản push cũ giải cùng vấn đề bằng
	// cách gọi fn TRƯỚC advance — cùng một bất biến, phát biểu ngược lại.
	pend bool

	curK, curV []byte
	err        error
	done       bool
}

// Iter mở một con trỏ trên [lo, hi). lo == nil nghĩa là từ đầu không gian khóa
// người dùng; hi == nil nghĩa là tới hết.
//
// Ở Serializable, lock lấy trên cả KHOẢNG, không phải trên từng khóa gặp
// được: khóa chưa tồn tại thì không lock được, nên phải lock chỗ nó SẼ nằm.
// Đây là predicate lock ở dạng nghèo nhất còn dùng được, và là toàn bộ cơ chế
// chặn phantom.
//
// Ở Serializable, khoảng được khoá S NGAY LÚC MỞ, không phải lúc đọc tới —
// nếu không thì một phantom chen vào giữa lần duyệt vẫn lọt, và cái tên
// Serializable thành lời nói dối. Đây cũng là chỗ khác nhau duy nhất về ngữ
// nghĩa giữa Iter và Scan.
func (t *Txn) Iter(lo, hi []byte) *Iter {
	if t.done {
		return &Iter{err: ErrTxnDone, done: true}
	}
	if lo == nil {
		lo = []byte{0x01} // vượt qua không gian metadata 0x00
	}
	if err := checkKey(lo); err != nil {
		return &Iter{err: err, done: true}
	}
	if t.iso == Serializable {
		if err := t.s.lk.Acquire(t.vid, lock.Span(lo, hi), lock.S); err != nil {
			return &Iter{err: err, done: true}
		}
	}
	return &Iter{
		t:    t,
		it:   t.s.d.Iter(lo, hi),
		own:  t.ownSorted(lo, hi),
		snap: t.readSnap(),
		pend: true,
	}
}

// advance đẩy con trỏ cây tới entry NHÌN THẤY ĐƯỢC kế tiếp.
//
// Lỗi giải mã chuỗi version phải nổi ra ngoài, không được nuốt: nuốt nó là
// biến một chuỗi hỏng thành một lần quét lặng lẽ thiếu khóa — loại lỗi mà
// không bài test nào bắt được vì kết quả vẫn "hợp lệ".
func (i *Iter) advance() error {
	for i.it.Next() {
		k := i.it.Key()
		if reserved(k) {
			continue
		}
		v, has, err := VisibleRaw(i.it.Value(), i.snap)
		if err != nil {
			return fmt.Errorf("txn: khóa %q: %w", k, err)
		}
		if !has || v.Deleted {
			continue
		}
		i.tk = append(i.tk[:0], k...)
		i.tv = append(i.tv[:0], v.Val...)
		i.tvalid = true
		return nil
	}
	i.tvalid = false
	return i.it.Err()
}

// Next đưa con trỏ tới hàng kế tiếp. Trả false khi hết hoặc khi có lỗi —
// phải kiểm Err() sau vòng lặp, đúng lệ của bufio.Scanner và của db.Iter.
func (i *Iter) Next() bool {
	if i.done || i.err != nil {
		return false
	}
	for {
		if i.pend {
			if err := i.advance(); err != nil {
				i.err, i.done = err, true
				return false
			}
			i.pend = false
		}
		switch {
		case !i.tvalid && len(i.own) == 0:
			i.done = true
			return false

		case len(i.own) == 0 || (i.tvalid && bytes.Compare(i.tk, i.own[0].k) < 0):
			// Chỉ có trong cây.
			i.curK, i.curV = i.tk, i.tv
			i.pend = true
			return true

		case !i.tvalid || bytes.Compare(i.own[0].k, i.tk) < 0:
			// Chỉ có trong write set: một khóa transaction này vừa tạo.
			w := i.own[0]
			i.own = i.own[1:]
			if w.v.Deleted {
				continue
			}
			i.curK, i.curV = w.k, w.v.Val
			return true

		default:
			// Cùng khóa: write set thắng (read-your-own-writes). Bản của cây
			// phải bị bỏ qua, nên vẫn đặt pend. Nếu write set là một lệnh xóa
			// thì khóa BIẾN MẤT khỏi lần quét này — nhánh này chính là chỗ
			// phép xóa được cài.
			w := i.own[0]
			i.own = i.own[1:]
			i.pend = true
			if w.v.Deleted {
				continue
			}
			i.curK, i.curV = w.k, w.v.Val
			return true
		}
	}
}

// Key và Value giá trị TỚI LẦN Next KẾ TIẾP. Ai cần giữ lâu hơn phải tự chép —
// nói rõ ở đây vì đó là toàn bộ lý do Iter không cấp phát mỗi hàng.
func (i *Iter) Key() []byte   { return i.curK }
func (i *Iter) Value() []byte { return i.curV }
func (i *Iter) Err() error    { return i.err }

// Restores là số lần con trỏ cây phải tìm lại chỗ vì cấu trúc đổi giữa hai
// bước (cơ chế của phase 7). Đưa ra ngoài để test khẳng định được rằng nó ĐÃ
// chạy — một cơ chế không đo được là một cơ chế không kiểm được.
func (i *Iter) Restores() int {
	if i.it == nil {
		return 0
	}
	return i.it.Restores()
}

// Close hiện không giải phóng gì (con trỏ không giữ pin giữa hai bước), nhưng
// mọi toán tử ở tầng trên đều gọi nó. Có nó từ đầu để cái ngày Iter cần giữ
// tài nguyên thật thì không phải sửa mọi chỗ gọi.
func (i *Iter) Close() error { i.done = true; return i.err }

// Scan giữ nguyên API push của phase 6, giờ là lớp vỏ trên Iter.
//
// Giữ lại vì có ba chục chỗ gọi và vì với truy vấn một-bảng nó vẫn là cách
// gọn nhất. Điều đáng nói là nó KHÔNG còn là bản cài: cái giá của việc chuyển
// sang pull được đo ở BenchmarkScan (phase 6) chứ không ước lượng.
func (t *Txn) Scan(lo, hi []byte, fn func(key, val []byte) bool) error {
	it := t.Iter(lo, hi)
	for it.Next() {
		if !fn(it.Key(), it.Value()) {
			return it.Close()
		}
	}
	return it.Err()
}
