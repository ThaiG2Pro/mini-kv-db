package pager

import (
	"fmt"
	"sort"
)

// Phần API mà WAL (phase 5) cần thêm ở pager.
//
// Phase 5 đổi vai trò của meta page. Trước đó meta là TOÀN BỘ cơ chế atomicity:
// commit = ghi meta, rollback = meta cũ vẫn còn. Từ khi có WAL thì không còn
// như vậy nữa — atomicity và durability chuyển hẳn sang log, còn meta tụt
// xuống làm hai việc khiêm tốn:
//
//  1. giữ trạng thái CẤP PHÁT (pageCount, freelist) — thứ WAL không log lại
//     từng bit vì nó không phải nội dung page;
//  2. giữ root của B+Tree ở lần checkpoint gần nhất, để recovery biết bắt đầu
//     từ cây nào.
//
// Hệ quả trực tiếp: dưới WAL, meta KHÔNG còn là một ảnh chụp nhất quán của dữ
// liệu. Mở file mà không chạy recovery là đọc phải một cây dở dang. Đây là
// khác biệt thật giữa BoltDB (shadow paging, meta là tất cả) và
// Postgres/InnoDB (WAL, meta chỉ là điểm xuất phát).

// Extend nới pageCount lên ít nhất count và kéo dài file tương ứng.
//
// Recovery cần nó: redo có thể chạm tới page mà transaction nào đó đã cấp SAU
// lần checkpoint cuối. Meta trên đĩa không biết những page ấy tồn tại, nên
// ReadPage/WritePage sẽ chặn chúng bằng ErrBadPageID. Pha analysis quét log,
// tìm pageID lớn nhất, rồi gọi hàm này trước khi redo.
func (p *Pager) Extend(count uint32) error {
	if count <= p.meta.pageCount {
		return nil
	}
	p.meta.pageCount = count
	return p.f.Truncate(int64(count) * PageSize)
}

// Reserve loại id khỏi danh sách cấp phát được.
//
// Dùng khi recovery phát hiện một page đã được cấp lại sau lần checkpoint cuối:
// freelist trong meta vẫn còn tên nó, nhưng log nói nó đang được dùng. Không
// gọi hàm này thì lần Allocate kế tiếp sẽ cấp lại một page đang nằm trong cây.
func (p *Pager) Reserve(id PageID) {
	for i, f := range p.free {
		if f == id {
			p.free = append(p.free[:i], p.free[i+1:]...)
			return
		}
	}
}

// ReleasePending chuyển các page đã Free sang danh sách cấp phát được.
//
// Dưới WAL, thời điểm gọi nó KHÔNG còn là pager.Commit mà là lúc transaction
// đã ghi xong commit record. Lý do là một bất biến của undo: nếu page P được
// txn A giải phóng rồi txn B cấp lại và ghi đè, mà sau đó A abort, thì undo
// của A sẽ khôi phục con trỏ của cha trỏ về P — lúc này P chứa dữ liệu của B.
// Cây hỏng mà mọi kiểm tra cục bộ vẫn xanh. Giữ P trong `pending` cho tới khi
// A commit chặn đúng kịch bản đó.
func (p *Pager) ReleasePending() {
	if len(p.pending) == 0 {
		return
	}
	pend := p.pending
	p.pending = nil
	for _, id := range pend {
		if !p.knows(id) {
			p.free = append(p.free, id)
		}
	}
	sort.Slice(p.free, func(i, j int) bool { return p.free[i] < p.free[j] })
}

// DropPending vứt danh sách pending đi mà KHÔNG đưa vào free: transaction đã
// abort, những page nó định giải phóng vẫn thuộc về cây.
func (p *Pager) DropPending() { p.pending = nil }

// MetaPendingCount là số page đang bị "khóa" vì meta hiện tại trỏ vào chúng.
func (p *Pager) MetaPendingCount() int { return len(p.metaPending) }

// MetaPending là danh sách page CHỨA freelist mà meta hiện tại trỏ tới.
//
// Recovery cần biết chúng: pha redo lặp lại lịch sử theo pageID, và một page
// từng là node của cây có thể ĐÃ được pager tái dùng làm page chứa freelist
// sau đó. Ghi đè lên nó là phá chính chuỗi freelist mà lần mở file sau phải
// đọc — mà pageLSN không cứu được, vì một page chứa freelist không có pageLSN
// (byte 16-24 của nó là id của các page rảnh).
func (p *Pager) MetaPending() []PageID {
	out := make([]PageID, len(p.metaPending))
	copy(out, p.metaPending)
	return out
}

// FreeNow đưa page thẳng vào danh sách cấp phát được, bỏ qua `pending`.
//
// Chỉ recovery được dùng: cuối pha undo, page mà một transaction thua cuộc đã
// cấp phát trở thành page mồ côi (ALLOC là redo-only, không undo được — xem
// recover.go). Không ai trỏ tới chúng nữa nên thu hồi ngay là an toàn, và đó
// chính là cách trả nợ P1-1 cho đường đi có WAL.
func (p *Pager) FreeNow(id PageID) error {
	if id == metaPageA || id == metaPageB {
		return ErrMetaPageBusy
	}
	if uint32(id) >= p.meta.pageCount {
		return fmt.Errorf("%w: id=%d", ErrBadPageID, id)
	}
	// Bỏ qua nếu page đã nằm trong một trong ba danh sách. Quan trọng nhất là
	// metaPending: một page từng là node của cây, bị free, rồi được pager tái
	// dùng làm page CHỨA freelist. Log vẫn còn record FREE cũ của nó, nên
	// recovery gọi FreeNow — và nếu ta cứ thế thêm vào `free`, page ấy có mặt
	// hai lần: một lần trong free, một lần trong metaPending. Checkpoint cấp
	// nó làm host (rút một bản khỏi free), rồi cuối CommitMeta lại rót
	// metaPending vào free — bản thứ hai quay lại. Lần Allocate sau cấp nó cho
	// một leaf, ghi đè lên chính page chứa freelist mà meta đang trỏ tới. Lần
	// mở file kế tiếp: "đọc freelist page 4096: EOF" (4096 = cellStart của một
	// page B+Tree rỗng đọc nhầm thành trường `next`). crashlab bắt được 4/40.
	if p.knows(id) {
		return nil
	}
	p.free = append(p.free, id)
	sort.Slice(p.free, func(i, j int) bool { return p.free[i] < p.free[j] })
	return nil
}

// knows: page đã nằm trong một danh sách cấp phát nào đó chưa.
func (p *Pager) knows(id PageID) bool {
	for _, l := range [][]PageID{p.free, p.pending, p.metaPending} {
		for _, f := range l {
			if f == id {
				return true
			}
		}
	}
	return false
}

// CommitMeta ghi meta page mới mà KHÔNG rotate pending.
//
// Đây là phần "ghi meta" của Commit, tách ra để checkpoint dùng: checkpoint
// chốt trạng thái cấp phát + root, nhưng nó không phải ranh giới transaction
// nên không được động vào pending.
func (p *Pager) CommitMeta(root PageID) error {
	sort.Slice(p.free, func(i, j int) bool { return p.free[i] < p.free[j] })
	flHead, flHosts, err := p.writeFreelist()
	if err != nil {
		return err
	}
	if err := p.sync(); err != nil {
		return err
	}
	m := meta{root: root, freelist: flHead, txnID: p.meta.txnID + 1, pageCount: p.meta.pageCount}
	target := MetaPageOf(m.txnID)
	p.Writes++
	if err := writeFull(p.f, encodeMeta(m), int64(target)*PageSize); err != nil {
		return err
	}
	if err := p.sync(); err != nil {
		return err
	}
	p.meta = m
	// Meta mới đã durable -> các freelist page của meta CŨ không còn ai trỏ
	// tới, cấp lại được ngay. Các host vừa ghi thì ngược lại: meta hiện tại
	// đang trỏ vào chúng, nên chúng phải đợi tới lần ghi meta sau.
	if len(p.metaPending) > 0 {
		old := p.metaPending
		p.metaPending = nil
		for _, id := range old {
			if !p.knows(id) {
				p.free = append(p.free, id)
			}
		}
		sort.Slice(p.free, func(i, j int) bool { return p.free[i] < p.free[j] })
	}
	p.metaPending = flHosts
	return nil
}

// FreeList trả về bản sao danh sách page cấp phát được (để test/dbcheck xem).
func (p *Pager) FreeList() []PageID {
	out := make([]PageID, len(p.free))
	copy(out, p.free)
	return out
}
