package txn

import "math"

// Snapshot là "thế giới mà một transaction được phép nhìn thấy".
//
// Ba trường, và cả ba đều cần thiết — bỏ trường nào cũng sinh một lỗi thấy
// được bằng test:
//
//   - Xmax: id sẽ cấp tiếp theo tại lúc chụp. Mọi transaction có id >= đây
//     bắt đầu SAU ta, nên chưa tồn tại trong thế giới của ta.
//   - Active: những transaction đang chạy tại lúc chụp. Chúng có id < Xmax
//     nhưng commit SAU khi ta chụp, nên cũng vô hình. Thiếu tập này là
//     non-repeatable read.
//   - Xmin: id nhỏ nhất còn có thể vô hình = min(Active ∪ {Xmax}). Thuần
//     dẫn xuất, nhưng nó là con số mà *bộ dọn version* dùng (xem horizon):
//     mọi version có xmin < Xmin thì hoặc nhìn thấy được, hoặc đã bị một
//     version mới hơn nhìn thấy được che đi.
//
// Không có trường "danh sách txn đã commit". Đó là chỗ thiết kế này rẽ khỏi
// Postgres và đơn giản hơn hẳn: ở đây một version chỉ CHẠM tới cây khi
// transaction của nó đã commit (xem Txn.Commit — write set nằm trong RAM cho
// tới lúc đó). Nên "có mặt trong cây" ĐÃ LÀ "đã commit", và cả cái clog biến
// mất. Cái giá: không có dirty read tự nhiên, phải dựng lại nó bằng tay để
// tái tạo anomaly (xem peekDirty).
type Snapshot struct {
	Xmax   uint64
	Xmin   uint64
	Active map[uint64]bool
}

// Latest nhìn thấy mọi thứ đã commit — không cách ly gì cả. Đây là snapshot
// của Serializable (nó cách ly bằng lock, không bằng version) và của
// ReadUncommitted.
var Latest = Snapshot{Xmax: math.MaxUint64}

// Visible: version do transaction xmin tạo ra có nhìn thấy được không.
func (s Snapshot) Visible(xmin uint64) bool {
	if xmin >= s.Xmax {
		return false // bắt đầu sau ta
	}
	if xmin < s.Xmin {
		return true // già hơn mọi thứ còn có thể đang chạy
	}
	return !s.Active[xmin] // đang chạy lúc ta chụp -> commit sau ta -> vô hình
}
