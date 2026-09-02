// Package lock cài lock manager cho 2PL nghiêm ngặt (S2PL).
//
// Đây là nửa "bi quan" của phase 6. Nửa còn lại (MVCC, trong internal/txn)
// tránh xung đột bằng cách cho mỗi người đọc một phiên bản riêng; nửa này
// tránh xung đột bằng cách BẮT NGƯỜI TA ĐỢI. Hai cơ chế trực giao nhau, và
// bản này dùng cả hai: MVCC cho ba mức isolation thấp, S2PL cho Serializable.
//
// Ba quyết định định hình file này:
//
//  1. **Lock là LOGICAL, không phải latch.** Nó giữ tới hết transaction (đó
//     là chữ "strict" trong S2PL), tính bằng mili giây tới giây, và vì thế
//     BẮT BUỘC phải có phát hiện deadlock. Latch của phase 3 sống vài chục ns
//     và không bao giờ deadlock vì luôn lấy theo một thứ tự. Đây là ranh giới
//     mà roadmap phase 0 mục 4 nói tới, giờ mới thành code.
//
//  2. **Đối tượng lock là một KHOẢNG khóa, không phải một page.** Lock theo
//     page thì phantom vẫn lọt (khóa mới có thể rơi vào page khác) và lại
//     chặn oan hai khóa vô can nằm cùng page. Khoảng `[lo, hi)` chính là
//     predicate lock ở dạng nghèo nhất mà vẫn đủ chặn phantom của một range
//     scan — xem `Span`.
//
//  3. **Phát hiện deadlock bằng wait-for graph, không phải bằng timeout.**
//     Timeout vẫn còn đó làm lưới an toàn, nhưng nếu nó là cơ chế chính thì
//     không phân biệt được "deadlock" với "máy đang chậm" — và một bài test
//     write skew sẽ đỏ vì lý do sai. Đồ thị cho câu trả lời tức thì và xác
//     định: nạn nhân luôn là transaction TRẺ nhất trong chu trình.
package lock

import (
	"bytes"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Mode là chế độ lock. Thứ tự có ý nghĩa: X > S, dùng để nhận ra nâng cấp.
type Mode uint8

const (
	S Mode = iota // shared — đọc
	X             // exclusive — ghi
)

func (m Mode) String() string {
	if m == X {
		return "X"
	}
	return "S"
}

// compatible: bảng tương thích kinh điển. Chỉ S-S là hòa nhau.
func compatible(a, b Mode) bool { return a == S && b == S }

var (
	ErrDeadlock = errors.New("lock: deadlock — transaction này bị chọn làm nạn nhân")
	ErrTimeout  = errors.New("lock: chờ lock quá lâu")
)

// Res là đối tượng bị lock: một khóa đơn, hoặc khoảng [Lo, Hi).
//
// Hi == nil với Point == false nghĩa là "tới vô cực" — đúng ngữ nghĩa của
// btree.Range(lo, nil). Vì thế phải có cờ Point riêng: nếu lấy Hi == nil để
// vừa mang nghĩa "điểm" vừa mang nghĩa "vô cực" thì một lock điểm sẽ lặng lẽ
// chặn cả cây.
type Res struct {
	Lo, Hi []byte
	Point  bool
}

// Key lock đúng một khóa.
func Key(k []byte) Res { return Res{Lo: k, Point: true} }

// Span lock khoảng [lo, hi). hi == nil = tới hết.
func Span(lo, hi []byte) Res { return Res{Lo: lo, Hi: hi} }

func (r Res) String() string {
	if r.Point {
		return fmt.Sprintf("key(%q)", r.Lo)
	}
	if r.Hi == nil {
		return fmt.Sprintf("span(%q..)", r.Lo)
	}
	return fmt.Sprintf("span(%q..%q)", r.Lo, r.Hi)
}

// endAfter: "đầu bên phải của r nằm sau khóa lo hay không".
//
// Một khóa đơn k được coi là khoảng [k, k⁺) — nên vế phải của nó "sau" lo khi
// và chỉ khi k >= lo. Viết tách ra thành hàm này vì đây là chỗ duy nhất mà ba
// hình dạng của Res (điểm / khoảng / mở tới vô cực) gặp nhau, và một lỗi lệch
// một đơn vị ở đây là lỗi im lặng: lock vẫn chạy, chỉ chặn thiếu.
func (r Res) endAfter(lo []byte) bool {
	if r.Point {
		return bytes.Compare(r.Lo, lo) >= 0
	}
	if r.Hi == nil {
		return true
	}
	return bytes.Compare(r.Hi, lo) > 0
}

// Overlaps: hai khoảng nửa mở giao nhau khi mỗi cái kết thúc sau chỗ cái kia
// bắt đầu.
func (r Res) Overlaps(o Res) bool {
	return r.endAfter(o.Lo) && o.endAfter(r.Lo)
}

// sameAs so hai Res theo đúng hình dạng — dùng cho đường tắt "đã giữ rồi".
func (r Res) sameAs(o Res) bool {
	return r.Point == o.Point && bytes.Equal(r.Lo, o.Lo) && bytes.Equal(r.Hi, o.Hi)
}

type holder struct {
	res  Res
	mode Mode
}

// Stats là số đếm để bench và diary nhìn. Deadlocks là con số đáng tin nhất
// của cả package: nó bằng số lần đồ thị THẬT SỰ có chu trình, không phải số
// lần chờ lâu.
type Stats struct {
	Acquires  int64
	Waits     int64
	Deadlocks int64
	Timeouts  int64
	Upgrades  int64
}

// Manager là lock manager. Một mutex + một condvar cho tất cả: đây là cấu
// trúc dữ liệu trong RAM nên latch thô là đúng, và mọi thao tác đều O(số
// lock đang giữ) — chấp nhận được vì số đó bị chặn bởi số transaction sống.
type Manager struct {
	// Timeout là lưới an toàn cho những chu trình mà đồ thị không thấy (ví dụ
	// một transaction bị treo ở ngoài lock manager). 0 -> DefaultTimeout.
	Timeout time.Duration

	mu    sync.Mutex
	cv    *sync.Cond
	held  map[uint64][]holder // txn -> lock đang giữ
	waits map[uint64][]uint64 // txn -> những txn nó đang đợi (wait-for graph)
	kill  map[uint64]bool     // txn đã bị chọn làm nạn nhân deadlock
	ages  map[uint64]uint64   // txn -> tuổi (mặc định: chính id của nó)
	st    Stats
}

const DefaultTimeout = 5 * time.Second

func New() *Manager {
	m := &Manager{
		held:  map[uint64][]holder{},
		waits: map[uint64][]uint64{},
		kill:  map[uint64]bool{},
		ages:  map[uint64]uint64{},
	}
	m.cv = sync.NewCond(&m.mu)
	return m
}

func (m *Manager) timeout() time.Duration {
	if m.Timeout <= 0 {
		return DefaultTimeout
	}
	return m.Timeout
}

// Acquire xin lock `mode` trên `res` cho transaction txn, chờ nếu phải chờ.
//
// Trả ErrDeadlock nếu txn bị chọn làm nạn nhân — người gọi PHẢI abort, không
// được thử lại trong cùng transaction: mọi lock của nó vẫn còn nguyên và
// chu trình vẫn còn đó.
func (m *Manager) Acquire(txn uint64, res Res, mode Mode) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	deadline := time.Now().Add(m.timeout())

	waiting := false
	for {
		if m.kill[txn] {
			delete(m.kill, txn)
			delete(m.waits, txn)
			return ErrDeadlock
		}
		if m.covered(txn, res, mode) {
			if !waiting {
				m.st.Acquires++
			}
			delete(m.waits, txn)
			return nil
		}
		blockers := m.conflicts(txn, res, mode)
		if len(blockers) == 0 {
			if m.upgrading(txn, res, mode) {
				m.st.Upgrades++
			}
			m.held[txn] = append(m.held[txn], holder{res: res, mode: mode})
			m.st.Acquires++
			delete(m.waits, txn)
			return nil
		}

		// Phải chờ. Ghi cạnh vào đồ thị TRƯỚC khi soi chu trình: chu trình
		// mới nào cũng đi qua cạnh vừa thêm.
		m.waits[txn] = blockers
		if !waiting {
			m.st.Waits++
			waiting = true
		}
		if victim, ok := m.findCycle(txn); ok {
			m.st.Deadlocks++
			if victim == txn {
				delete(m.waits, txn)
				return ErrDeadlock
			}
			// Nạn nhân là kẻ khác: đánh dấu rồi đánh thức nó. Nó đang ở trong
			// vòng lặp này (mọi đỉnh của chu trình đều là kẻ đang chờ) nên
			// chắc chắn nhận được tin.
			m.kill[victim] = true
			m.cv.Broadcast()
		}
		rem := time.Until(deadline)
		if rem <= 0 {
			delete(m.waits, txn)
			m.st.Timeouts++
			return fmt.Errorf("%w: txn %d xin %s trên %s", ErrTimeout, txn, mode, res)
		}

		// Hẹn giờ đánh thức chính mình, LẬP LẠI mỗi vòng.
		//
		// Một condvar không có timeout nên lưới an toàn phải đến từ bên ngoài.
		// Bản đầu đặt một time.AfterFunc duy nhất ở đầu hàm — và đó là một
		// lỗi treo thật: chỉ cần một lần bị Broadcast oan (một transaction thứ
		// ba thả lock) là timer đã tiêu, vòng sau Wait không còn ai đánh thức,
		// và cái deadline trở thành lời hứa suông. Đặt lại mỗi vòng thì đắt
		// hơn một cấp phát timer cho mỗi lần bị đánh thức — mà số đó bị chặn
		// bởi số lần thả lock, không phải bởi thời gian chờ.
		wake := time.AfterFunc(rem, func() {
			m.mu.Lock()
			m.cv.Broadcast()
			m.mu.Unlock()
		})
		m.cv.Wait()
		wake.Stop()
	}
}

// covered: txn đã giữ một lock đủ mạnh trên đúng đối tượng này chưa. Chỉ nhận
// trường hợp Res GIỐNG HỆT — nhận cả "chứa trong" thì phải tự viết phép bao
// hàm cho ba hình dạng, và một lỗi ở đó làm lock chặn thiếu mà không ai thấy.
func (m *Manager) covered(txn uint64, res Res, mode Mode) bool {
	for _, h := range m.held[txn] {
		if h.res.sameAs(res) && h.mode >= mode {
			return true
		}
	}
	return false
}

func (m *Manager) upgrading(txn uint64, res Res, mode Mode) bool {
	if mode != X {
		return false
	}
	for _, h := range m.held[txn] {
		if h.res.sameAs(res) && h.mode == S {
			return true
		}
	}
	return false
}

// conflicts trả về danh sách txn KHÁC đang giữ lock không tương thích trên
// một đối tượng giao với res.
//
// Lock của CHÍNH txn bị bỏ qua — nhờ đó S -> X là nâng cấp tại chỗ chứ không
// phải tự deadlock với mình. Cái giá: sau khi nâng cấp, txn giữ cả S lẫn X
// trên cùng đối tượng. Không sao vì ReleaseAll thả tất; nhưng đây cũng chính
// là chỗ sinh ra deadlock nâng cấp kinh điển của write skew (hai txn cùng
// giữ S, cùng xin X), và đó là hành vi ĐÚNG mong muốn.
func (m *Manager) conflicts(txn uint64, res Res, mode Mode) []uint64 {
	var out []uint64
	for other, hs := range m.held {
		if other == txn {
			continue
		}
		for _, h := range hs {
			if !compatible(h.mode, mode) && h.res.Overlaps(res) {
				out = append(out, other)
				break
			}
		}
	}
	return out
}

// findCycle tìm chu trình đi qua start trong đồ thị wait-for, và trả về
// transaction TRẺ nhất trong chu trình làm nạn nhân.
//
// "Trẻ nhất" = tuổi lớn nhất theo ageOf, KHÔNG phải id lớn nhất — xem SetAge
// để biết vì sao hai thứ đó phải tách nhau. Kẻ già nhất đã làm nhiều việc
// nhất nên giết nó là hủy nhiều công nhất; và vì tuổi được giữ qua các lần
// thử lại, kẻ bị giết già dần lên và cuối cùng chắc chắn được đi.
func (m *Manager) findCycle(start uint64) (uint64, bool) {
	seen := map[uint64]bool{start: true}
	var path []uint64
	var dfs func(u uint64) bool
	dfs = func(u uint64) bool {
		path = append(path, u)
		for _, v := range m.waits[u] {
			if v == start {
				return true
			}
			if !seen[v] {
				seen[v] = true
				if dfs(v) {
					return true
				}
			}
		}
		path = path[:len(path)-1]
		return false
	}
	if !dfs(start) {
		return 0, false
	}
	victim := path[0]
	best := m.ageOf(victim)
	for _, u := range path {
		if a := m.ageOf(u); a > best || (a == best && u > victim) {
			victim, best = u, a
		}
	}
	return victim, true
}

// SetAge khai báo TUỔI của một transaction, tách khỏi danh tính của nó. Số
// nhỏ hơn = già hơn.
//
// Vì sao phải tách hai thứ này ra — đây là một bug đã đo được, không phải một
// khả năng lý thuyết. Bản đầu lấy luôn id làm tuổi, với lý lẽ: "chọn nạn nhân
// là kẻ trẻ nhất thì không starvation, vì một transaction chỉ bị giết bởi kẻ
// già hơn, và nó già dần lên". Lý lẽ đó SAI ngay khi có vòng thử lại: lần thử
// thứ hai là một transaction MỚI với id MỚI, nên nó trẻ lại từ đầu và lại là
// nạn nhân. Đo được: bài chuyển tiền ở Serializable bỏ 70/160 lượt sau 50 lần
// thử, hết trong 0.19 giây — không phải chờ lâu, mà là chết đi chết lại.
//
// Giữ tuổi qua các lần thử lại thì kẻ bị giết già dần và cuối cùng thắng. Đây
// chính là điều mà thuật toán wound-wait/wait-die đòi: một *timestamp* gắn với
// transaction LOGIC, không gắn với lần thử.
func (m *Manager) SetAge(txn, age uint64) {
	m.mu.Lock()
	m.ages[txn] = age
	m.mu.Unlock()
}

// ageOf gọi khi đang giữ m.mu.
func (m *Manager) ageOf(txn uint64) uint64 {
	if a, ok := m.ages[txn]; ok {
		return a
	}
	return txn
}

// ReleaseAll thả mọi lock của txn. Đây là ĐIỂM DUY NHẤT thả lock, và đó chính
// là chữ "strict" của S2PL: không có Release lẻ, nên không thể vô tình thả
// sớm và mở đường cho cascading abort.
func (m *Manager) ReleaseAll(txn uint64) {
	m.mu.Lock()
	delete(m.held, txn)
	delete(m.waits, txn)
	delete(m.kill, txn)
	delete(m.ages, txn)
	m.mu.Unlock()
	m.cv.Broadcast()
}

// Killed cho biết txn đã bị chọn làm nạn nhân deadlock trong lúc nó không
// chờ lock nào. Tầng trên phải hỏi câu này trước khi commit: một transaction
// đã bị giết mà vẫn commit được thì phát hiện deadlock thành vô nghĩa.
func (m *Manager) Killed(txn uint64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.kill[txn]
}

func (m *Manager) Stats() Stats {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.st
}

// Held là số lock txn đang giữ — chỉ để test soi.
func (m *Manager) Held(txn uint64) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.held[txn])
}

// Waiters là số transaction đang chờ — để test đợi đến khi đối phương đã
// thật sự chặn, thay vì ngủ một khoảng đoán bừa.
func (m *Manager) Waiters() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.waits)
}
