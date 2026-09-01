package bufpool

import "math"

// Replacer là chính sách chọn nạn nhân. Nó chỉ biết chỉ số frame — không biết
// gì về PageID, dirty, hay nội dung. Tách ra thế này để đổi chính sách trong
// bench mà không đụng vào pool.
//
// Hợp đồng: Victim() KHÔNG BAO GIỜ được trả về frame đang bị pin.
type Replacer interface {
	// Access: frame vừa được truy cập (hit hoặc vừa nạp xong).
	Access(fr int)
	// Pin: frame có người đang dùng -> rút khỏi tập ứng viên.
	Pin(fr int)
	// Unpin: pin count về 0 -> quay lại tập ứng viên.
	Unpin(fr int)
	// Victim: chọn một frame để đuổi. false = không còn ứng viên nào.
	Victim() (int, bool)
	// Evictable dùng cho Verify(): pool tự kiểm tra rằng frame đang pin thì
	// replacer cũng phải coi là không đuổi được. Hai bên giữ trạng thái riêng,
	// nên phải có chỗ đối chiếu.
	Evictable(fr int) bool
	Name() string
}

// ---------- LRU thuần ----------

// lruReplacer: danh sách liên kết đôi cài trên hai mảng int32 (không cấp phát,
// không con trỏ). Chỉ frame KHÔNG bị pin mới nằm trong danh sách.
//
// head = ít dùng gần đây nhất (nạn nhân tiếp theo), tail = vừa dùng.
type lruReplacer struct {
	prev, next []int32
	in         []bool
	head, tail int32
}

const nilFrame int32 = -1

func NewLRU(n int) Replacer {
	r := &lruReplacer{
		prev: make([]int32, n), next: make([]int32, n), in: make([]bool, n),
		head: nilFrame, tail: nilFrame,
	}
	for i := range r.prev {
		r.prev[i], r.next[i] = nilFrame, nilFrame
	}
	return r
}

func (r *lruReplacer) Name() string { return "lru" }

func (r *lruReplacer) remove(i int32) {
	if !r.in[i] {
		return
	}
	if r.prev[i] != nilFrame {
		r.next[r.prev[i]] = r.next[i]
	} else {
		r.head = r.next[i]
	}
	if r.next[i] != nilFrame {
		r.prev[r.next[i]] = r.prev[i]
	} else {
		r.tail = r.prev[i]
	}
	r.prev[i], r.next[i], r.in[i] = nilFrame, nilFrame, false
}

func (r *lruReplacer) pushBack(i int32) {
	if r.in[i] {
		return
	}
	r.prev[i], r.next[i] = r.tail, nilFrame
	if r.tail != nilFrame {
		r.next[r.tail] = i
	} else {
		r.head = i
	}
	r.tail, r.in[i] = i, true
}

// Access khi frame đang bị pin là no-op: nó không nằm trong danh sách. Thứ tự
// LRU của nó được xác lập lúc Unpin — đây chính là chỗ CLOCK và LRU khác nhau
// về mặt cài đặt, và là lý do LRU phải đụng vào danh sách ở mọi lần Unpin.
func (r *lruReplacer) Access(fr int) {
	i := int32(fr)
	if r.in[i] {
		r.remove(i)
		r.pushBack(i)
	}
}

func (r *lruReplacer) Evictable(fr int) bool { return r.in[fr] }

func (r *lruReplacer) Pin(fr int)   { r.remove(int32(fr)) }
func (r *lruReplacer) Unpin(fr int) { i := int32(fr); r.remove(i); r.pushBack(i) }

func (r *lruReplacer) Victim() (int, bool) {
	if r.head == nilFrame {
		return 0, false
	}
	i := r.head
	r.remove(i)
	return int(i), true
}

// ---------- CLOCK (second chance) ----------

// clockReplacer: mỗi frame một bit "vừa dùng". Kim quay vòng; gặp bit 1 thì
// xoá bit và đi tiếp (cho một cơ hội thứ hai), gặp bit 0 thì đuổi.
//
// Vì sao DB thật dùng cái này thay LRU: nó không cần đụng vào cấu trúc dữ liệu
// nào lúc truy cập — chỉ set một bit. Không có node để move, không có latch
// nóng ở đầu danh sách LRU khi nhiều luồng cùng chạy.
type clockReplacer struct {
	ref       []bool
	evictable []bool
	hand      int
}

func NewClock(n int) Replacer {
	return &clockReplacer{ref: make([]bool, n), evictable: make([]bool, n)}
}

func (r *clockReplacer) Name() string          { return "clock" }
func (r *clockReplacer) Access(fr int)         { r.ref[fr] = true }
func (r *clockReplacer) Pin(fr int)            { r.evictable[fr] = false }
func (r *clockReplacer) Evictable(fr int) bool { return r.evictable[fr] }
func (r *clockReplacer) Unpin(fr int)          { r.evictable[fr] = true }

func (r *clockReplacer) Victim() (int, bool) {
	n := len(r.ref)
	// Tối đa 2 vòng: vòng 1 xoá hết bit ref, vòng 2 chắc chắn tìm được nếu
	// còn ứng viên nào.
	for scan := 0; scan < 2*n; scan++ {
		i := r.hand
		r.hand = (r.hand + 1) % n
		if !r.evictable[i] {
			continue
		}
		if r.ref[i] {
			r.ref[i] = false
			continue
		}
		r.evictable[i] = false
		return i, true
	}
	return 0, false
}

// ---------- LRU-K ----------

// lrukReplacer giữ K mốc thời gian truy cập gần nhất của mỗi frame và đuổi
// frame có **khoảng cách lùi K** lớn nhất — tức là frame mà lần truy cập thứ K
// tính ngược lại đã xa nhất.
//
// Đây chính là cơ chế chống sequential scan: một page bị quét qua đúng MỘT lần
// thì chưa có mốc thứ K, nên nó bị coi là "khoảng cách vô hạn" và bị đuổi
// TRƯỚC mọi page đã được dùng >= K lần. LRU thuần không phân biệt được hai
// loại đó: với nó, "vừa chạm một lần" và "chạm liên tục" đều là "vừa dùng".
type lrukReplacer struct {
	k         int
	hist      [][]uint64 // hist[i] = tối đa k mốc, cũ -> mới
	evictable []bool
	now       uint64
}

func NewLRUK(n, k int) Replacer {
	if k < 1 {
		panic("bufpool: LRU-K cần k >= 1")
	}
	r := &lrukReplacer{k: k, hist: make([][]uint64, n), evictable: make([]bool, n)}
	for i := range r.hist {
		r.hist[i] = make([]uint64, 0, k)
	}
	return r
}

func (r *lrukReplacer) Name() string {
	if r.k == 2 {
		return "lru-2"
	}
	return "lru-k"
}

func (r *lrukReplacer) Access(fr int) {
	r.now++
	h := r.hist[fr]
	if len(h) == r.k {
		copy(h, h[1:])
		h = h[:r.k-1]
	}
	r.hist[fr] = append(h, r.now)
}

func (r *lrukReplacer) Pin(fr int)            { r.evictable[fr] = false }
func (r *lrukReplacer) Evictable(fr int) bool { return r.evictable[fr] }
func (r *lrukReplacer) Unpin(fr int)          { r.evictable[fr] = true }

func (r *lrukReplacer) Victim() (int, bool) {
	// Hai nhóm ứng viên, nhóm đầu luôn thắng:
	//   - chưa đủ K lần truy cập -> khoảng cách lùi = vô hạn; trong nhóm này
	//     phá hoà bằng mốc CŨ NHẤT còn nhớ được (đúng như bài báo LRU-K).
	//   - đủ K lần -> so mốc thứ K tính ngược lại, cũ hơn thì bị đuổi.
	bestInf, bestInfEarliest := -1, uint64(math.MaxUint64)
	bestFin, bestFinKth := -1, uint64(math.MaxUint64)
	for i := range r.hist {
		if !r.evictable[i] {
			continue
		}
		h := r.hist[i]
		if len(h) < r.k {
			e := uint64(0) // chưa truy cập lần nào -> cũ nhất có thể
			if len(h) > 0 {
				e = h[0]
			}
			if e < bestInfEarliest {
				bestInf, bestInfEarliest = i, e
			}
			continue
		}
		if h[0] < bestFinKth {
			bestFin, bestFinKth = i, h[0]
		}
	}
	best := bestInf
	if best == -1 {
		best = bestFin
	}
	if best == -1 {
		return 0, false
	}
	r.evictable[best] = false
	r.hist[best] = r.hist[best][:0]
	return best, true
}
