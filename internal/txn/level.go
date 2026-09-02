package txn

import "fmt"

// Level là mức isolation. Thứ tự tăng dần theo độ chặt, và so sánh bằng `>=`
// ở nhiều chỗ trong package — đừng chèn mức mới vào giữa.
type Level uint8

const (
	// ReadUncommitted đọc cả write set của transaction KHÁC đang chạy.
	//
	// Trong thiết kế này nó là mức KHÓ CÀI NHẤT, không phải dễ nhất — và đó
	// là phát hiện đáng giá của phase 6: vì write set nằm trong RAM riêng của
	// mỗi transaction cho tới lúc commit, dirty read *không thể xảy ra* dù có
	// muốn. Phải viết thêm code (Store.peekDirty) để dựng lại anomaly. Nói
	// cách khác: kiến trúc deferred-write đã chặn dirty read bằng hình dạng
	// của nó, không bằng một luật nào.
	ReadUncommitted Level = iota

	// ReadCommitted chụp snapshot MỚI cho mỗi câu lệnh. Chặn dirty read,
	// nhưng để lọt non-repeatable read, phantom và lost update.
	ReadCommitted

	// RepeatableRead chụp snapshot MỘT LẦN ở Begin, cộng first-committer-wins
	// khi commit. Đây là **snapshot isolation** thật, mạnh hơn RR của chuẩn
	// SQL (nó chặn cả phantom). Nhưng vẫn để lọt **write skew** — cái mà
	// version không bao giờ chặn được, vì hai transaction ghi hai khóa KHÁC
	// nhau nên chẳng có xung đột ghi-ghi nào để phát hiện.
	RepeatableRead

	// Serializable bỏ snapshot, quay về S2PL: đọc lấy lock S (điểm cho Get,
	// khoảng cho Scan), ghi lấy lock X, giữ tới commit. Đây là chỗ hai cơ chế
	// của phase 6 gặp nhau và ta thấy chúng trực giao: MVCC chặn được ba
	// anomaly đầu mà không chặn được write skew; lock chặn được write skew
	// bằng cách để hai transaction deadlock rồi giết một đứa.
	Serializable
)

func (l Level) String() string {
	switch l {
	case ReadUncommitted:
		return "read-uncommitted"
	case ReadCommitted:
		return "read-committed"
	case RepeatableRead:
		return "repeatable-read"
	case Serializable:
		return "serializable"
	}
	return fmt.Sprintf("level(%d)", uint8(l))
}

// AllLevels theo thứ tự tăng dần — bảng anomaly và cmd/txnlab đi theo nó.
var AllLevels = []Level{ReadUncommitted, ReadCommitted, RepeatableRead, Serializable}

func ParseLevel(s string) (Level, error) {
	for _, l := range AllLevels {
		if l.String() == s {
			return l, nil
		}
	}
	return 0, fmt.Errorf("txn: mức isolation không biết: %q", s)
}

// usesSnapshot: mức này đọc qua version hay đọc bản mới nhất.
func (l Level) usesSnapshot() bool {
	return l == ReadCommitted || l == RepeatableRead
}
