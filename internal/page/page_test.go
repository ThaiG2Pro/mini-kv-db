package page

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"minidb/internal/pager"
)

func rec(n int, b byte) []byte {
	r := make([]byte, n)
	for i := range r {
		r[i] = b
	}
	return r
}

func mustVerify(t *testing.T, p Page, where string) {
	t.Helper()
	if err := p.Verify(); err != nil {
		t.Fatalf("%s: bất biến vỡ: %v\n%s", where, err, p.Dump(16))
	}
}

func TestInitEmptyPage(t *testing.T) {
	p := New(TypeHeap)
	mustVerify(t, p, "sau Init")
	if p.NumSlots() != 0 || p.NumDead() != 0 || p.Frag() != 0 {
		t.Fatalf("page mới không rỗng: %s", p.Dump(0))
	}
	if got, want := p.FreeContiguous(), PageSize-HeaderSize; got != want {
		t.Fatalf("chỗ trống ban đầu %d, muốn %d", got, want)
	}
}

func TestInsertGetDelete(t *testing.T) {
	p := New(TypeHeap)
	ids := make([]SlotID, 3)
	for i := range ids {
		id, err := p.Insert([]byte(fmt.Sprintf("record-%d", i)))
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = id
		mustVerify(t, p, "sau Insert")
	}
	for i, id := range ids {
		got, err := p.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if want := fmt.Sprintf("record-%d", i); string(got) != want {
			t.Fatalf("slot %d đọc ra %q, muốn %q", id, got, want)
		}
	}
	if err := p.Delete(ids[1]); err != nil {
		t.Fatal(err)
	}
	mustVerify(t, p, "sau Delete")
	if _, err := p.Get(ids[1]); !errors.Is(err, ErrSlotDead) {
		t.Fatalf("đọc slot đã xóa trả %v, muốn ErrSlotDead", err)
	}
	if err := p.Delete(ids[1]); !errors.Is(err, ErrSlotDead) {
		t.Fatalf("xóa hai lần trả %v, muốn ErrSlotDead", err)
	}
	// Hai record còn lại không được suy suyển.
	for _, i := range []int{0, 2} {
		got, err := p.Get(ids[i])
		if err != nil {
			t.Fatal(err)
		}
		if want := fmt.Sprintf("record-%d", i); string(got) != want {
			t.Fatalf("slot %d đọc ra %q, muốn %q", ids[i], got, want)
		}
	}
}

// Đây là bài học trung tâm của phase 2: compact dời TOÀN BỘ cell nhưng SlotID
// không đổi -> con trỏ ngoài (tuple id trong secondary index) vẫn dùng được.
func TestSlotIDStableAcrossCompact(t *testing.T) {
	p := New(TypeHeap)
	var ids []SlotID
	for i := 0; i < 20; i++ {
		id, err := p.Insert(rec(100, byte('A'+i)))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	// Xóa xen kẽ để tạo lỗ hổng giữa vùng cell.
	for i := 0; i < len(ids); i += 2 {
		if err := p.Delete(ids[i]); err != nil {
			t.Fatal(err)
		}
	}
	if p.Frag() != 10*100 {
		t.Fatalf("frag=%d, muốn 1000", p.Frag())
	}

	before := map[SlotID]int{}
	for i := 1; i < len(ids); i += 2 {
		off, _ := p.slot(int(ids[i]))
		before[ids[i]] = off
	}

	p.Compact()
	mustVerify(t, p, "sau Compact")

	if p.Frag() != 0 {
		t.Fatalf("compact xong frag=%d, muốn 0", p.Frag())
	}
	moved := 0
	for i := 1; i < len(ids); i += 2 {
		off, _ := p.slot(int(ids[i]))
		if off != before[ids[i]] {
			moved++
		}
		got, err := p.Get(ids[i]) // <- vẫn dùng ĐÚNG SlotID cũ
		if err != nil {
			t.Fatalf("slot %d sau compact: %v", ids[i], err)
		}
		if want := rec(100, byte('A'+i)); !bytes.Equal(got, want) {
			t.Fatalf("slot %d sau compact đọc sai nội dung", ids[i])
		}
	}
	if moved == 0 {
		t.Fatal("không cell nào bị dời -> test này không chứng minh được gì")
	}
	t.Logf("%d/%d cell sống đã đổi offset, 0 SlotID đổi", moved, 10)
}

// Xóa không được rút mảng slot: SlotID phía sau phải đứng yên.
func TestDeleteDoesNotShiftSlots(t *testing.T) {
	p := New(TypeHeap)
	var ids []SlotID
	for i := 0; i < 5; i++ {
		id, _ := p.Insert([]byte{byte('a' + i)})
		ids = append(ids, id)
	}
	if err := p.Delete(ids[0]); err != nil {
		t.Fatal(err)
	}
	if p.NumSlots() != 5 {
		t.Fatalf("numSlots=%d sau khi xóa, muốn vẫn 5", p.NumSlots())
	}
	got, err := p.Get(ids[4])
	if err != nil || got[0] != 'e' {
		t.Fatalf("slot cuối sau khi xóa slot đầu: %q %v", got, err)
	}
	// Insert mới phải lấy id mới, KHÔNG tái dùng slot chết.
	id, _ := p.Insert([]byte("z"))
	if id != 5 {
		t.Fatalf("insert sau xóa lấy id=%d, muốn 5 (không tái dùng slot chết)", id)
	}
}

func TestNeedCompactThenInsertSucceeds(t *testing.T) {
	p := New(TypeHeap)
	const sz = 400
	var ids []SlotID
	for {
		id, err := p.InsertNoCompact(rec(sz, 'x'))
		if err != nil {
			break
		}
		ids = append(ids, id)
	}
	// Xóa hết trừ cái cuối -> tổng trống lớn, liền mạch vẫn bé.
	for _, id := range ids[:len(ids)-1] {
		if err := p.Delete(id); err != nil {
			t.Fatal(err)
		}
	}
	mustVerify(t, p, "sau khi xóa hàng loạt")
	t.Logf("liền mạch=%d tổng=%d frag=%d", p.FreeContiguous(), p.FreeTotal(), p.Frag())
	if p.FreeContiguous() >= sz {
		t.Fatalf("chưa phân mảnh đủ để test có ý nghĩa (liền mạch=%d)", p.FreeContiguous())
	}
	if _, err := p.InsertNoCompact(rec(sz, 'y')); !errors.Is(err, ErrNeedCompact) {
		t.Fatalf("InsertNoCompact trả %v, muốn ErrNeedCompact", err)
	}
	if _, err := p.Insert(rec(sz, 'y')); err != nil {
		t.Fatalf("Insert (tự compact) vẫn lỗi: %v", err)
	}
	mustVerify(t, p, "sau Insert tự compact")
}

func TestPageFullIsReal(t *testing.T) {
	p := New(TypeHeap)
	n := 0
	for {
		if _, err := p.Insert(rec(64, 'q')); err != nil {
			if !errors.Is(err, ErrPageFull) {
				t.Fatalf("lỗi lạ khi đầy page: %v", err)
			}
			break
		}
		n++
	}
	mustVerify(t, p, "khi page đầy")
	used := HeaderSize + p.NumSlots()*slotSize + (PageSize - p.cellStart())
	t.Logf("nhét được %d record 64B; dùng %d/%d byte, còn trống %d", n, used, PageSize, p.FreeContiguous())
	if p.FreeContiguous() >= 64+slotSize {
		t.Fatalf("báo đầy nhưng còn %d byte liền mạch", p.FreeContiguous())
	}
	if _, err := p.Insert(rec(MaxRecordSize+1, 'q')); !errors.Is(err, ErrRecordTooLarge) {
		t.Fatalf("record quá khổ trả %v, muốn ErrRecordTooLarge", err)
	}
}

func TestMaxRecordFitsExactly(t *testing.T) {
	p := New(TypeHeap)
	id, err := p.Insert(rec(MaxRecordSize, 'm'))
	if err != nil {
		t.Fatalf("record đúng MaxRecordSize=%d phải vừa: %v", MaxRecordSize, err)
	}
	mustVerify(t, p, "record lớn nhất")
	if p.FreeContiguous() != 0 {
		t.Fatalf("còn thừa %d byte -> MaxRecordSize tính sai", p.FreeContiguous())
	}
	got, _ := p.Get(id)
	if len(got) != MaxRecordSize {
		t.Fatalf("đọc lại dài %d", len(got))
	}
}

func TestUpdateKeepsSlotID(t *testing.T) {
	p := New(TypeHeap)
	id, _ := p.Insert(rec(100, 'a'))
	other, _ := p.Insert(rec(100, 'b'))

	// nhỏ lại: ghi tại chỗ, phần dư thành frag
	if err := p.Update(id, rec(40, 'c')); err != nil {
		t.Fatal(err)
	}
	mustVerify(t, p, "update nhỏ lại")
	if p.Frag() != 60 {
		t.Fatalf("frag=%d, muốn 60", p.Frag())
	}

	// to ra: cell mới, slot cũ, cell cũ thành frag
	if err := p.Update(id, rec(300, 'd')); err != nil {
		t.Fatal(err)
	}
	mustVerify(t, p, "update to ra")
	got, err := p.Get(id)
	if err != nil || !bytes.Equal(got, rec(300, 'd')) {
		t.Fatalf("đọc lại sau update: %q %v", got[:min(8, len(got))], err)
	}
	if o, _ := p.Get(other); !bytes.Equal(o, rec(100, 'b')) {
		t.Fatal("update làm hỏng record hàng xóm")
	}
	if p.NumSlots() != 2 {
		t.Fatalf("update đẻ thêm slot: numSlots=%d", p.NumSlots())
	}
}

func TestTrimDeadSlotsOnlyAtTail(t *testing.T) {
	p := New(TypeHeap)
	var ids []SlotID
	for i := 0; i < 6; i++ {
		id, _ := p.Insert(rec(10, byte('a'+i)))
		ids = append(ids, id)
	}
	// giết slot 1 (giữa) và 4,5 (đuôi)
	for _, i := range []int{1, 4, 5} {
		if err := p.Delete(ids[i]); err != nil {
			t.Fatal(err)
		}
	}
	if got := p.TrimDeadSlots(); got != 2 {
		t.Fatalf("cắt được %d slot, muốn 2 (chỉ đuôi)", got)
	}
	mustVerify(t, p, "sau TrimDeadSlots")
	if p.NumSlots() != 4 {
		t.Fatalf("numSlots=%d, muốn 4", p.NumSlots())
	}
	if p.NumDead() != 1 {
		t.Fatalf("numDead=%d, muốn 1 (slot chết ở giữa vẫn phải giữ)", p.NumDead())
	}
	// Con trỏ ngoài tới slot đã cắt vẫn cho câu trả lời cũ: "không còn record".
	if _, err := p.Get(ids[5]); !errors.Is(err, ErrBadSlot) {
		t.Fatalf("đọc slot đã cắt trả %v, muốn ErrBadSlot (vẫn là 'không có')", err)
	}
	// Slot chết ở giữa KHÔNG được cắt, nếu không id 2..3 sẽ tụt.
	if got, _ := p.Get(ids[3]); !bytes.Equal(got, rec(10, 'd')) {
		t.Fatal("SlotID bị dịch sau khi trim")
	}
}

// Verify phải thật sự có răng: bẻ tay một slot cho chồng lấn thì nó phải kêu.
func TestVerifyCatchesOverlap(t *testing.T) {
	p := New(TypeHeap)
	a, _ := p.Insert(rec(100, 'a'))
	b, _ := p.Insert(rec(100, 'b'))
	mustVerify(t, p, "trước khi bẻ")

	offA, _ := p.slot(int(a))
	offB, lnB := p.slot(int(b))
	p.setSlot(int(b), offB, lnB+50) // cho cell b thò sang cell a
	_ = offA
	err := p.Verify()
	if err == nil {
		t.Fatal("Verify im lặng trước cell chồng lấn -> mọi test khác vô nghĩa")
	}
	t.Logf("Verify bắt được: %v", err)

	p.setSlot(int(b), offB, lnB)
	mustVerify(t, p, "sau khi trả lại")
	p.setU16(offFrag, uint16(p.Frag()+1)) // sai sổ sách frag
	if err := p.Verify(); err == nil {
		t.Fatal("Verify không bắt được frag lệch")
	} else {
		t.Logf("Verify bắt được: %v", err)
	}
}

// Property test: 200k thao tác ngẫu nhiên, sau MỖI thao tác kiểm tra bất biến
// và đối chiếu toàn bộ record sống với model trong RAM.
func TestRandomOpsKeepInvariants(t *testing.T) {
	const ops = 200000
	r := rand.New(rand.NewSource(20260901))
	p := New(TypeHeap)
	model := map[SlotID][]byte{}
	stat := map[string]int{}

	for i := 0; i < ops; i++ {
		switch r.Intn(100) {
		case 0, 1, 2, 3, 4: // 5% compact
			p.Compact()
			stat["compact"]++
		case 5: // 1% trim
			stat["trim"] += p.TrimDeadSlots()
			for id := range model {
				if int(id) >= p.NumSlots() {
					t.Fatalf("trim cắt mất slot sống %d", id)
				}
			}
		case 6, 7, 8, 9, 10, 11, 12, 13, 14, 15: // 10% update
			if len(model) == 0 {
				continue
			}
			id := pickLive(r, model)
			buf := rec(1+r.Intn(500), byte(r.Intn(256)))
			if err := p.Update(id, buf); err != nil {
				if !errors.Is(err, ErrPageFull) {
					t.Fatalf("op %d Update: %v", i, err)
				}
				stat["update-full"]++
				continue
			}
			model[id] = buf
			stat["update"]++
		case 16, 17, 18, 19, 20, 21, 22, 23, 24, 25,
			26, 27, 28, 29, 30, 31, 32, 33, 34, 35: // 20% delete
			if len(model) == 0 {
				continue
			}
			id := pickLive(r, model)
			if err := p.Delete(id); err != nil {
				t.Fatalf("op %d Delete(%d): %v", i, id, err)
			}
			delete(model, id)
			stat["delete"]++
		default: // ~63% insert
			buf := rec(1+r.Intn(500), byte(r.Intn(256)))
			id, err := p.Insert(buf)
			if err != nil {
				if !errors.Is(err, ErrPageFull) {
					t.Fatalf("op %d Insert: %v", i, err)
				}
				stat["insert-full"]++
				continue
			}
			if _, dup := model[id]; dup {
				t.Fatalf("op %d: Insert trả lại SlotID %d đang sống", i, id)
			}
			model[id] = buf
			stat["insert"]++
		}

		if err := p.Verify(); err != nil {
			t.Fatalf("op %d: %v\n%s", i, err, p.Dump(8))
		}
		if p.NumLive() != len(model) {
			t.Fatalf("op %d: page có %d record sống, model có %d", i, p.NumLive(), len(model))
		}
	}
	// Đối chiếu toàn bộ ở cuối.
	for id, want := range model {
		got, err := p.Get(id)
		if err != nil {
			t.Fatalf("slot %d: %v", id, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("slot %d nội dung sai (dài %d vs %d)", id, len(got), len(want))
		}
	}
	t.Logf("%d thao tác: %v", ops, stat)
	t.Logf("cuối cùng: %d slot (%d sống), trống liền mạch %d, frag %d",
		p.NumSlots(), p.NumLive(), p.FreeContiguous(), p.Frag())
}

func pickLive(r *rand.Rand, model map[SlotID][]byte) SlotID {
	n := r.Intn(len(model))
	for id := range model {
		if n == 0 {
			return id
		}
		n--
	}
	panic("không tới đây")
}

// runProgram diễn giải `prog` thành chuỗi thao tác trên một page và kiểm tra
// bất biến sau mỗi bước. Dùng chung cho fuzz và cho stress test có seed.
func runProgram(t *testing.T, prog []byte) {
	p := New(TypeHeap)
	model := map[SlotID][]byte{}
	live := []SlotID{}
	for i := 0; i+1 < len(prog); i += 2 {
		op, arg := prog[i]%5, int(prog[i+1])
		switch op {
		case 0: // insert
			buf := rec(1+arg*4, byte(i))
			if len(buf) > MaxRecordSize {
				continue
			}
			id, err := p.Insert(buf)
			if err == nil {
				model[id] = buf
				live = append(live, id)
			} else if !errors.Is(err, ErrPageFull) {
				t.Fatalf("Insert: %v", err)
			}
		case 1: // delete
			if len(live) == 0 {
				continue
			}
			k := arg % len(live)
			id := live[k]
			if err := p.Delete(id); err != nil {
				t.Fatalf("Delete(%d): %v", id, err)
			}
			delete(model, id)
			live = append(live[:k], live[k+1:]...)
		case 2: // compact
			p.Compact()
		case 3: // trim
			p.TrimDeadSlots()
			for id := range model {
				if int(id) >= p.NumSlots() {
					t.Fatalf("trim cắt mất slot sống %d", id)
				}
			}
		case 4: // update
			if len(live) == 0 {
				continue
			}
			id := live[arg%len(live)]
			buf := rec(1+arg*3, byte(i+1))
			if len(buf) > MaxRecordSize {
				continue
			}
			if err := p.Update(id, buf); err == nil {
				model[id] = buf
			} else if !errors.Is(err, ErrPageFull) {
				t.Fatalf("Update: %v", err)
			}
		}
		if err := p.Verify(); err != nil {
			t.Fatalf("sau op %d: %v", op, err)
		}
	}
	for id, want := range model {
		got, err := p.Get(id)
		if err != nil {
			t.Fatalf("slot %d: %v", id, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("slot %d nội dung sai", id)
		}
	}
}

// Fuzz: fuzzer lái chuỗi thao tác, ta chỉ giữ bất biến + model.
func FuzzSlottedPage(f *testing.F) {
	f.Add([]byte{0, 5, 1, 0, 2, 0, 3})
	f.Add([]byte{0, 200, 0, 200, 0, 200, 1, 1, 4, 0, 100})
	f.Fuzz(func(t *testing.T, prog []byte) {
		// Chặn độ dài: Verify chạy sau MỖI thao tác, nên một input vài trăm KB
		// chiếm worker rất lâu mà không thăm dò thêm nhánh nào.
		if len(prog) > 2048 {
			prog = prog[:2048]
		}
		runProgram(t, prog)
	})
}

// Chạy chính thân fuzz với hàng nghìn chương trình ngẫu nhiên có seed cố định.
// Mục đích: nếu có vòng lặp vô hạn thì `go test -timeout` sẽ dump stack đúng
// chỗ, thay vì fuzzer chỉ im lặng đứng hình.
func TestProgramStress(t *testing.T) {
	r := rand.New(rand.NewSource(2))
	for i := 0; i < 3000; i++ {
		prog := make([]byte, 2+r.Intn(2046))
		r.Read(prog)
		runProgram(t, prog)
	}
}

// Ghép tầng: slotted page đi qua pager, crash-safe của phase 1 phải giữ nguyên.
func TestPageRoundTripsThroughPager(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.db")
	pg, err := pager.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	id, err := pg.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	p := New(TypeHeap)
	var slots []SlotID
	for i := 0; i < 50; i++ {
		s, err := p.Insert([]byte(fmt.Sprintf("hàng số %d", i)))
		if err != nil {
			t.Fatal(err)
		}
		slots = append(slots, s)
	}
	for i := 0; i < 50; i += 3 {
		if err := p.Delete(slots[i]); err != nil {
			t.Fatal(err)
		}
	}
	p.Compact()
	if err := pg.WritePage(id, p); err != nil {
		t.Fatal(err)
	}
	if err := pg.Commit(id); err != nil {
		t.Fatal(err)
	}
	if err := pg.Close(); err != nil {
		t.Fatal(err)
	}

	pg2, err := pager.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer pg2.Close()
	buf := make(Page, pager.PageSize)
	if err := pg2.ReadPage(pg2.Root(), buf); err != nil {
		t.Fatal(err)
	}
	mustVerify(t, buf, "sau khi đọc lại từ đĩa")
	if buf.NumLive() != 33 {
		t.Fatalf("đọc lại có %d record sống, muốn 33", buf.NumLive())
	}
	for i := 0; i < 50; i++ {
		got, err := buf.Get(slots[i])
		if i%3 == 0 {
			if !errors.Is(err, ErrSlotDead) {
				t.Fatalf("slot %d phải chết sau reopen, được %v", i, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("slot %d: %v", i, err)
		}
		if want := fmt.Sprintf("hàng số %d", i); string(got) != want {
			t.Fatalf("slot %d đọc %q muốn %q", i, got, want)
		}
	}
	st, _ := os.Stat(path)
	t.Logf("file %d byte, root=page %d, %d record sống qua được reopen", st.Size(), pg2.Root(), buf.NumLive())
}

// ---------- benchmark ----------

func BenchmarkInsert(b *testing.B) {
	r := rec(64, 'x')
	p := New(TypeHeap)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := p.InsertNoCompact(r); err != nil {
			b.StopTimer()
			Init(p, TypeHeap)
			b.StartTimer()
		}
	}
}

func BenchmarkGet(b *testing.B) {
	p := New(TypeHeap)
	var ids []SlotID
	for {
		id, err := p.InsertNoCompact(rec(64, 'x'))
		if err != nil {
			break
		}
		ids = append(ids, id)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := p.Get(ids[i%len(ids)]); err != nil {
			b.Fatal(err)
		}
	}
}

// Compact trên page đầy record 64B: đây là "giá của VACUUM" ở mức một page.
func BenchmarkCompact(b *testing.B) {
	tmpl := New(TypeHeap)
	var ids []SlotID
	for {
		id, err := tmpl.InsertNoCompact(rec(64, 'x'))
		if err != nil {
			break
		}
		ids = append(ids, id)
	}
	for i := 0; i < len(ids); i += 2 {
		_ = tmpl.Delete(ids[i])
	}
	p := New(TypeHeap)
	b.SetBytes(PageSize)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		copy(p, tmpl)
		b.StartTimer()
		p.Compact()
	}
	b.ReportMetric(float64(len(ids)/2), "cell-sống")
}

// Trường hợp xấu nhất của Compact: offset của cell theo thứ tự slot bị ĐẢO
// NGƯỢC. Dựng bằng cách update lần lượt từng slot cho to ra — mỗi lần cấp cell
// mới ở mép trái, nên slot càng lớn offset càng nhỏ... rồi lặp lại lần nữa để
// đảo hẳn. Đây là input mà fuzzer tìm ra và làm nó đứng hình.
func buildScrambled(tb testing.TB, n int) Page {
	p := New(TypeHeap)
	var ids []SlotID
	for i := 0; i < n; i++ {
		id, err := p.InsertNoCompact([]byte{1})
		if err != nil {
			tb.Fatalf("chỉ nhét được %d record", i)
		}
		ids = append(ids, id)
	}
	// Update theo thứ tự slot GIẢM DẦN: slot lớn được cell mới trước (offset
	// cao), slot nhỏ nhận sau (offset thấp) -> offset tăng dần theo slot,
	// tức đảo ngược hoàn toàn thứ tự mà Compact cần.
	for i := len(ids) - 1; i >= 0; i-- {
		if err := p.Update(ids[i], rec(2, 'z')); err != nil {
			tb.Fatal(err)
		}
	}
	return p
}

func TestCompactOrderIsScrambled(t *testing.T) {
	p := buildScrambled(t, 300)
	inversions, prev, n := 0, PageSize+1, 0
	for i := 0; i < p.NumSlots(); i++ {
		off, _ := p.slot(i)
		if off == deadOffset {
			continue
		}
		n++
		if off > prev {
			inversions++
		}
		prev = off
	}
	t.Logf("%d cell sống, %d chỗ offset KHÔNG giảm dần theo slot", n, inversions)
	if inversions == 0 {
		t.Fatal("không dựng được thế xấu -> bench dưới vô nghĩa")
	}
}

func BenchmarkCompactScrambled(b *testing.B) {
	tmpl := buildScrambled(b, 300)
	p := New(TypeHeap)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		copy(p, tmpl)
		b.StartTimer()
		p.Compact()
	}
	b.ReportMetric(float64(tmpl.NumLive()), "cell-sống")
}

// Verify là thứ fuzz gọi sau MỖI thao tác, nên giá của nó nhân với độ dài
// chương trình fuzz. Đo để biết vì sao input dài làm fuzzer đứng hình.
func BenchmarkVerifyFullPage(b *testing.B) {
	p := New(TypeHeap)
	n := 0
	for {
		if _, err := p.InsertNoCompact([]byte{1}); err != nil {
			break
		}
		n++
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := p.Verify(); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(n), "slot")
}

// verifyRef là bản Verify đầu tiên: gom cell sống rồi sort để tìm chồng lấn.
// Giữ lại làm bản tham chiếu, vì (a) nó kiểm tra chéo bản bitmap đang dùng và
// (b) nó làm tỉ số "nhanh hơn bao nhiêu lần" tái lập được, thay vì một con số
// đo một lần trên code đã xóa.
func verifyRef(p Page) error {
	n := p.NumSlots()
	cs := p.cellStart()
	type span struct{ lo, hi, id int }
	live := make([]span, 0, n)
	dead, sum := 0, 0
	for i := 0; i < n; i++ {
		off, ln := p.slot(i)
		if off == deadOffset {
			dead++
			continue
		}
		if off < cs || off+ln > PageSize {
			return fmt.Errorf("slot %d ngoài vùng cell", i)
		}
		live = append(live, span{off, off + ln, i})
		sum += ln
	}
	if dead != p.NumDead() {
		return fmt.Errorf("numDead lệch")
	}
	sort.Slice(live, func(a, b int) bool { return live[a].lo < live[b].lo })
	for i := 1; i < len(live); i++ {
		if live[i].lo < live[i-1].hi {
			return fmt.Errorf("cell slot %d chồng slot %d", live[i].id, live[i-1].id)
		}
	}
	if want := (PageSize - cs) - sum; want != p.Frag() {
		return fmt.Errorf("frag lệch: header=%d tính lại=%d", p.Frag(), want)
	}
	return nil
}

// Hai verifier phải luôn đồng ý — nếu không, cái nhanh đã mua tốc độ bằng
// cách bỏ sót lỗi.
func TestVerifyAgreesWithReference(t *testing.T) {
	r := rand.New(rand.NewSource(99))
	p := New(TypeHeap)
	var live []SlotID
	for i := 0; i < 30000; i++ {
		switch r.Intn(4) {
		case 0:
			if id, err := p.Insert(rec(1+r.Intn(300), byte(i))); err == nil {
				live = append(live, id)
			}
		case 1:
			if len(live) > 0 {
				k := r.Intn(len(live))
				_ = p.Delete(live[k])
				live = append(live[:k], live[k+1:]...)
			}
		case 2:
			p.Compact()
		default:
			if len(live) > 0 {
				_ = p.Update(live[r.Intn(len(live))], rec(1+r.Intn(300), byte(i)))
			}
		}
		gotFast, gotRef := p.Verify(), verifyRef(p)
		if (gotFast == nil) != (gotRef == nil) {
			t.Fatalf("op %d: Verify=%v nhưng verifyRef=%v", i, gotFast, gotRef)
		}
	}
}

func BenchmarkVerifyRefFullPage(b *testing.B) {
	p := New(TypeHeap)
	n := 0
	for {
		if _, err := p.InsertNoCompact([]byte{1}); err != nil {
			break
		}
		n++
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := verifyRef(p); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(n), "slot")
}
