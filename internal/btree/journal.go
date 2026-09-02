package btree

import (
	"minidb/internal/page"
	"minidb/internal/pager"
)

// Journal là chỗ cây báo cáo mọi thứ nó làm với page, để tầng trên ghi WAL.
//
// Vì sao đặt móc ở ĐÂY chứ không ở buffer pool: để log undo được, phải có ảnh
// TRƯỚC của page, mà pool chỉ biết một page đã bẩn ở lúc Unpin — tức là sau
// khi nội dung cũ đã mất. Cây thì biết sớm hơn: nó pin page rồi mới sửa. Nên
// PageIn (chụp ảnh trước) và PageOut (so ảnh, sinh record) kẹp đúng khoảng
// thời gian một page có thể thay đổi.
//
// Đơn vị ở đây là "mini-transaction" theo nghĩa của InnoDB: một lệnh Put/Delete
// chạm nhiều page (leaf, các cha trên đường split, page mới cấp), và mỗi page
// sinh ĐÚNG MỘT record diff cho cả lệnh — chứ không phải một record cho mỗi
// lần chạm byte. Redo vì thế luôn đưa page từ một trạng thái hợp lệ sang một
// trạng thái hợp lệ khác.
//
// Nil Journal = hành vi của phase 4, không log gì cả. Đó là điều kiện để toàn
// bộ test và bench của phase 4 chạy nguyên vẹn, và để bench đo được đúng cái
// giá của WAL bằng cách bật/tắt một field.
type Journal interface {
	// PageIn: cây vừa pin page và có thể sắp sửa nó. Gọi lồng nhau được (một
	// page nằm trên đường đi có thể bị pin nhiều lần); chỉ lần ngoài cùng mới
	// chụp ảnh.
	PageIn(id pager.PageID, p page.Page)

	// PageOut: cây sắp thả pin. dirty là ĐIỀU CÂY NGHĨ; giá trị trả về là điều
	// các byte nói. Cây được quyền sai theo hướng "quên báo bẩn" — nó có vài
	// chỗ thả pin với dirty=false cho một node vừa bị sửa rồi sắp bị gộp đi —
	// và journal sửa lại giúp. Chiều ngược lại (báo bẩn mà không đổi byte)
	// thì vô hại, chỉ tốn một lần ghi.
	PageOut(id pager.PageID, dirty bool) bool

	// PageAlloc: page vừa được cấp và đã Init xong. Gọi TRƯỚC PageIn tương ứng.
	PageAlloc(id pager.PageID, typ uint8, p page.Page)

	// PageFree: page sắp được trả về freelist. Nhận cả NỘI DUNG page vì undo
	// cần nó — xem chú thích ở t.freePage.
	PageFree(id pager.PageID, p page.Page)

	// RootChanged: cây cao thêm hoặc thấp đi. Root không nằm trong page nào
	// cả nên nó phải được log riêng — xem nợ P4-4.
	RootChanged(old, new pager.PageID)

	// Err trả lỗi đầu tiên gặp phải. Cây không truyền lỗi từ PageOut ngược lên
	// (unpin nằm trong defer, không có chỗ trả), nên tầng trên phải hỏi sau
	// mỗi lệnh. Bỏ qua nó = ghi vào cây mà không có log tương ứng.
	Err() error
}

// ---------- móc nối trong cây ----------

func (t *Tree) newPage(typ uint8) (pager.PageID, node, error) {
	f, err := t.pool.NewPage(typ)
	if err != nil {
		return 0, node{}, err
	}
	id := f.PageID()
	if t.J != nil {
		t.J.PageAlloc(id, typ, f.Data)
		t.J.PageIn(id, f.Data)
	}
	return id, node{p: f.Data}, nil
}

// freePage trả page về freelist.
//
// Trước khi trả, nội dung page được đưa cho journal. Nghe thừa — page sắp
// chết mà — nhưng đây là chỗ đã dính bug: pool.FreePage VỨT cờ dirty đi, nên
// trạng thái cuối cùng của page không bao giờ tới đĩa. Khi transaction bị
// hủy, undo lần ngược chuỗi ảnh-trước của chính page đó, mà mỗi ảnh-trước chỉ
// đúng khi page đang ở trạng thái ngay-trước-khi-crash. Trên đĩa nó lại là
// một bản cũ hơn, và các đoạn diff dán lên đó cho ra một page lai — panic
// "slot đã bị xóa" khi cursor đi qua. Ghi trọn ảnh page vào record FREE là
// cách rẻ nhất để undo có mốc đúng mà không phải fsync thêm lần nào.
func (t *Tree) freePage(id pager.PageID) error {
	if t.J != nil {
		f, err := t.pool.Pin(id)
		if err != nil {
			return err
		}
		t.J.PageFree(id, f.Data)
		if err := t.pool.Unpin(id, false); err != nil {
			return err
		}
	}
	return t.pool.FreePage(id)
}

func (t *Tree) setRoot(id pager.PageID) {
	if t.J != nil && t.root != id {
		t.J.RootChanged(t.root, id)
	}
	t.root = id
}

// SetRoot đặt lại root mà KHÔNG sinh log record.
//
// Chỉ hai chỗ được dùng: abort (quay lại root trước khi transaction chạy) và
// recovery (đặt root mà pha analysis/undo đã tính ra). Cả hai đều đã có record
// tương ứng trong log rồi — ghi thêm ở đây là ghi hai lần cùng một sự việc.
//
// Bẫy đã dính: Tree giữ root trong RAM, còn undo chỉ khôi phục root của DB.
// Quên đồng bộ hai chỗ thì sau một lần Abort, cây vẫn đọc từ root mà
// transaction vừa bị hủy đã tạo ra — page đó đã bị thu hồi, nên nội dung nó
// trả về là bất kỳ thứ gì được cấp vào chỗ ấy sau đó.
func (t *Tree) SetRoot(id pager.PageID) { t.root = id }
