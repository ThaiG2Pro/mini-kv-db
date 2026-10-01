package txn

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// version.go cài MVCC ở dạng thô nhất có thể: **chuỗi version nằm ngay trong
// value của B+Tree**, bản mới nhất trước.
//
//	value = [u8 n] { [u8 flags][u64 xmin][u16 len][val...] } × n
//
// Ba điều đáng nói:
//
//  1. **Không có xmax.** Roadmap phase 6 viết "mỗi tuple mang (xmin, xmax)",
//     nhưng trong một chuỗi xếp theo thứ tự thì xmax của version i CHÍNH LÀ
//     xmin của version i-1 — hai chỗ ghi cùng một sự thật là hai chỗ để lệch
//     nhau (đúng lý lẽ đã dùng ở wal/record.go khi bỏ cờ "có ảnh trước").
//     Xóa là một version có cờ Deleted, không phải một xmax đặt lên bản cũ.
//
//  2. **Chuỗi phải nhét vừa MỘT entry của B+Tree** (btree.MaxEntrySize ~2KB).
//     Đây là giới hạn cứng, và là lý do thật sự vì sao DB thật KHÔNG để bản cũ
//     tại chỗ: Postgres tạo tuple mới ở page khác, InnoDB đẩy bản cũ sang undo
//     segment. Bản này chọn tại chỗ vì nó làm luật visibility hiện ra rõ ràng
//     nhất; cái giá là một trần cứng, đo được ở TestChainFull → nợ P6-*.
//
//  3. **Version cũ phải được dọn**, nếu không mỗi lần ghi lại một khóa là một
//     lần chuỗi dài thêm và cuối cùng khóa đó không ghi được nữa. Prune chạy
//     ngay trong đường ghi (như HOT prune của Postgres), Vacuum chạy toàn cây.
//     Đây là chỗ câu hỏi "vì sao Postgres cần VACUUM" của phase 2 được trả.

// MaxVersions chặn trên số version một khóa mang. Chặn này tồn tại để lỗi
// hiện ra dưới dạng "chuỗi đầy" thay vì "entry lớn hơn một page" — một thông
// báo nói đúng nguyên nhân thì đáng giá hơn một thông báo nói đúng triệu chứng.
const MaxVersions = 64

var (
	ErrBadChain  = errors.New("txn: chuỗi version hỏng")
	ErrChainFull = errors.New("txn: chuỗi version đã đầy, không dọn thêm được")
)

const flagDeleted uint8 = 1 << 0

// flagsKnown là mặt nạ của những bit CÓ NGHĨA. Bit nào ngoài mặt nạ thì
// DecodeChain từ chối, chứ không bỏ qua.
//
// Chỗ này do fuzzer tìm ra, không phải do đọc lại code: bản đầu chỉ xét
// f&flagDeleted nên byte cờ 0x30 giải mã êm rồi mã hoá lại thành 0x00 — tức
// encoding KHÔNG canonical, và một chuỗi hỏng đi qua ChainStats mà BadChains
// vẫn bằng 0. Cùng lý lẽ như vùng dự trữ trong header WAL của phase 5: bit
// chưa dùng phải được kiểm, vì cái giá của việc kiểm là một phép AND, còn cái
// giá của việc không kiểm là một bản ghi tương lai bị bản cũ đọc sai lặng lẽ.
const flagsKnown = flagDeleted

// Version là một bản của giá trị. Val của bản Deleted luôn nil.
type Version struct {
	Xmin    uint64
	Deleted bool
	Val     []byte
}

// Chain là chuỗi version của một khóa, MỚI NHẤT TRƯỚC.
type Chain []Version

// EncodedSize là số byte chuỗi chiếm khi ghi.
func (c Chain) EncodedSize() int {
	n := 1
	for _, v := range c {
		n += 1 + 8 + 2
		if !v.Deleted {
			n += len(v.Val)
		}
	}
	return n
}

// Encode nối chuỗi vào dst.
func (c Chain) Encode(dst []byte) []byte {
	dst = append(dst, uint8(len(c)))
	var num [8]byte
	for _, v := range c {
		var f uint8
		if v.Deleted {
			f = flagDeleted
		}
		dst = append(dst, f)
		binary.LittleEndian.PutUint64(num[:], v.Xmin)
		dst = append(dst, num[:]...)
		// Tombstone không mang thân, kể cả khi người gọi đưa Val khác nil.
		// Chuẩn hoá ở ĐÂY chứ không chỉ kiểm ở DecodeChain: một bộ mã hoá
		// sinh ra được thứ mà bộ giải mã của chính nó từ chối là một cái bẫy,
		// và fuzzer đã sập vào nó ngay ở seed đầu tiên.
		body := v.Val
		if v.Deleted {
			body = nil
		}
		dst = append(dst, uint8(len(body)), uint8(len(body)>>8))
		dst = append(dst, body...)
	}
	return dst
}

// DecodeChain đọc chuỗi. Val trỏ THẲNG vào b — người gọi tự chép nếu giữ lâu.
// (db.DB.Get đã trả về một bản chép riêng, nên trong package này là an toàn.)
func DecodeChain(b []byte) (Chain, error) {
	r, err := newChainReader(b)
	if err != nil {
		return nil, err
	}
	c := make(Chain, 0, r.n)
	for r.more() {
		v, err := r.next()
		if err != nil {
			return nil, err
		}
		c = append(c, v)
	}
	return c, r.end()
}

// VisibleRaw là DecodeChain rồi Visible, nhưng không dựng Chain: đi thẳng trên
// byte đã mã hoá và chỉ trả version mà snapshot nhìn thấy. Đường đọc nóng (mỗi
// hàng của mỗi lần quét, mỗi Get) dùng nó; trước nợ P9-1 / P6-2, mỗi hàng là
// một lần cấp phát Chain chỉ để đọc phần tử đầu tiên nhìn thấy được.
//
// Nó VẪN đi hết mọi header và kiểm như DecodeChain (cờ lạ, độ dài, byte thừa),
// dù đã tìm thấy bản cần trả: một chuỗi hỏng ở đuôi phải nổi lỗi ở lần đọc
// đầu tiên chạm vào nó, không phải lặng lẽ trả về bản ở đầu. Header 11 byte,
// bước nhảy O(1), nên phần đi tiếp chỉ là vài phép so mỗi version.
func VisibleRaw(b []byte, s Snapshot) (Version, bool, error) {
	r, err := newChainReader(b)
	if err != nil {
		return Version{}, false, err
	}
	var out Version
	found := false
	for r.more() {
		v, err := r.next()
		if err != nil {
			return Version{}, false, err
		}
		if !found && s.Visible(v.Xmin) {
			out, found = v, true
		}
	}
	return out, found, r.end()
}

// chainReader là bộ đọc chuỗi version dùng chung cho DecodeChain và
// VisibleRaw: một chỗ duy nhất biết định dạng và kiểm tính canonical.
type chainReader struct {
	b    []byte
	n, i int
}

func newChainReader(b []byte) (chainReader, error) {
	if len(b) < 1 {
		return chainReader{}, fmt.Errorf("%w: rỗng", ErrBadChain)
	}
	return chainReader{b: b[1:], n: int(b[0])}, nil
}

func (r *chainReader) more() bool { return r.i < r.n }

func (r *chainReader) next() (Version, error) {
	b, i := r.b, r.i
	if len(b) < 11 {
		return Version{}, fmt.Errorf("%w: thiếu header version %d", ErrBadChain, i)
	}
	f := b[0]
	if f&^flagsKnown != 0 {
		return Version{}, fmt.Errorf("%w: version %d có bit cờ lạ %#02x", ErrBadChain, i, f)
	}
	xmin := binary.LittleEndian.Uint64(b[1:])
	ln := int(b[9]) | int(b[10])<<8
	b = b[11:]
	if len(b) < ln {
		return Version{}, fmt.Errorf("%w: version %d nói %d byte, còn %d", ErrBadChain, i, ln, len(b))
	}
	v := Version{Xmin: xmin, Deleted: f&flagDeleted != 0}
	if v.Deleted {
		// Tombstone không mang thân. Nếu nó nói có thì byte trên đĩa
		// không phải cái mà Encode sinh ra được — cũng do fuzzer tìm
		// ra, cùng một họ với bit cờ lạ ở trên.
		if ln != 0 {
			return Version{}, fmt.Errorf("%w: version %d là tombstone nhưng nói %d byte thân",
				ErrBadChain, i, ln)
		}
	} else {
		v.Val = b[:ln]
	}
	r.b, r.i = b[ln:], i+1
	return v, nil
}

func (r *chainReader) end() error {
	if len(r.b) != 0 {
		return fmt.Errorf("%w: còn %d byte thừa", ErrBadChain, len(r.b))
	}
	return nil
}

// Visible trả về version mới nhất mà snapshot nhìn thấy.
//
// Luật gọn đúng một dòng vì chuỗi đã xếp theo thứ tự: bản đầu tiên nhìn thấy
// được cũng là bản mới nhất nhìn thấy được. Bản đó là tombstone thì khóa
// KHÔNG tồn tại trong thế giới của snapshot ấy — khác hẳn "không tìm thấy
// version nào", dù người gọi xử lý hai ca như nhau.
func (c Chain) Visible(s Snapshot) (Version, bool) {
	for _, v := range c {
		if s.Visible(v.Xmin) {
			return v, true
		}
	}
	return Version{}, false
}

// Newest là version mới nhất bất kể snapshot — dùng cho kiểm tra xung đột
// ghi-ghi (first-committer-wins).
func (c Chain) Newest() (Version, bool) {
	if len(c) == 0 {
		return Version{}, false
	}
	return c[0], true
}

// Prepend thêm một version mới lên đầu chuỗi.
func (c Chain) Prepend(v Version) Chain {
	out := make(Chain, 0, len(c)+1)
	out = append(out, v)
	return append(out, c...)
}

// Prune bỏ mọi version không ai còn nhìn thấy được nữa.
//
// Luật: giữ hết những version có xmin >= horizon, cộng thêm ĐÚNG MỘT version
// đầu tiên có xmin < horizon. Version thứ hai trở đi dưới ngưỡng đã bị bản
// trên nó che với mọi snapshot còn sống, nên là rác.
//
// horizon = min của Snapshot.Xmin trên mọi transaction còn sống (chính là
// OldestXmin của Postgres). Đây là chỗ dễ sai nhất của cả phase: nếu lấy
// horizon = min(Xmax) thay vì min(Xmin) thì một version bị một reader coi là
// "đang chạy" vẫn nằm dưới ngưỡng, và ta xóa mất bản mà reader ấy cần đọc —
// một lỗi chỉ hiện ra khi có đúng ba transaction gối nhau.
//
// Trả về chuỗi mới và số version đã bỏ.
func (c Chain) Prune(horizon uint64) (Chain, int) {
	keep := len(c)
	for i, v := range c {
		if v.Xmin < horizon {
			keep = i + 1
			break
		}
	}
	if keep >= len(c) {
		return c, 0
	}
	return c[:keep], len(c) - keep
}

// Dead cho biết chuỗi đã có thể xóa khỏi cây hẳn: chỉ còn một tombstone, và
// tombstone đó không ai còn cần thấy. Đây là lúc VACUUM thật sự thu hồi chỗ.
func (c Chain) Dead(horizon uint64) bool {
	return len(c) == 1 && c[0].Deleted && c[0].Xmin < horizon
}
