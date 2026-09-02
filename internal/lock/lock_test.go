package lock

import (
	"errors"
	"testing"
	"time"
)

// waitFor đợi tới khi cond đúng, tối đa d. Dùng thay time.Sleep ở mọi chỗ:
// một bài test đồng thời mà ngủ một khoảng đoán bừa thì hoặc chậm, hoặc flaky
// trên máy khác — và cả hai đều làm con số trong diary mất giá trị.
func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(200 * time.Microsecond)
	}
	t.Fatalf("quá %v mà chưa: %s", d, what)
}

// tryLock mô phỏng đúng cái mà người gọi thật (txn.Txn) làm: nhận
// ErrDeadlock thì ABORT, tức thả hết lock. Bỏ bước ấy đi thì nạn nhân chết mà
// vẫn ngồi giữ lock, chu trình không tan, và những kẻ còn lại chờ tới timeout
// — hai bài test dưới đây từng đỏ vì đúng lý do đó, và cái sai nằm ở test.
func tryLock(m *Manager, txn uint64, r Res, mode Mode) error {
	err := m.Acquire(txn, r, mode)
	if errors.Is(err, ErrDeadlock) {
		m.ReleaseAll(txn)
	}
	return err
}

func mgr(t *testing.T) *Manager {
	m := New()
	m.Timeout = 2 * time.Second
	return m
}

func TestOverlaps(t *testing.T) {
	k := func(s string) Res { return Key([]byte(s)) }
	sp := func(a, b string) Res {
		var hi []byte
		if b != "" {
			hi = []byte(b)
		}
		return Span([]byte(a), hi)
	}
	cases := []struct {
		a, b Res
		want bool
	}{
		{k("b"), k("b"), true},
		{k("b"), k("c"), false},
		{sp("a", "c"), k("b"), true},
		{sp("a", "c"), k("c"), false}, // nửa mở: hi không thuộc khoảng
		{sp("a", "c"), k("a"), true},
		{sp("a", "c"), sp("c", "e"), false},
		{sp("a", "c"), sp("b", "e"), true},
		{sp("a", ""), k("zzz"), true}, // mở tới vô cực
		{k("zzz"), sp("a", ""), true},
		{sp("m", ""), k("a"), false},
	}
	for _, c := range cases {
		if got := c.a.Overlaps(c.b); got != c.want {
			t.Errorf("%s.Overlaps(%s) = %v, muốn %v", c.a, c.b, got, c.want)
		}
		if got := c.b.Overlaps(c.a); got != c.want {
			t.Errorf("giao hoán vỡ: %s.Overlaps(%s) = %v, muốn %v", c.b, c.a, got, c.want)
		}
	}
}

func TestSharedDoesNotBlockShared(t *testing.T) {
	m := mgr(t)
	if err := m.Acquire(1, Key([]byte("k")), S); err != nil {
		t.Fatal(err)
	}
	if err := m.Acquire(2, Key([]byte("k")), S); err != nil {
		t.Fatalf("S-S phải hòa nhau: %v", err)
	}
	if m.Waiters() != 0 {
		t.Fatalf("không ai được chờ, có %d", m.Waiters())
	}
}

func TestExclusiveBlocksThenWakes(t *testing.T) {
	m := mgr(t)
	if err := m.Acquire(1, Key([]byte("k")), X); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- m.Acquire(2, Key([]byte("k")), S) }()
	waitFor(t, time.Second, "txn 2 chặn lại", func() bool { return m.Waiters() == 1 })
	m.ReleaseAll(1)
	if err := <-done; err != nil {
		t.Fatalf("thả lock rồi mà txn 2 vẫn không lấy được: %v", err)
	}
}

// TestSpanBlocksKeyInside là hạt nhân của việc chặn phantom: khóa "b" CHƯA
// TỒN TẠI vẫn bị chặn, vì cái bị lock là chỗ nó sẽ nằm.
func TestSpanBlocksKeyInside(t *testing.T) {
	m := mgr(t)
	if err := m.Acquire(1, Span([]byte("a"), []byte("c")), S); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- m.Acquire(2, Key([]byte("b")), X) }()
	waitFor(t, time.Second, "khóa trong khoảng bị chặn", func() bool { return m.Waiters() == 1 })

	// Ngoài khoảng thì không chặn.
	if err := m.Acquire(3, Key([]byte("d")), X); err != nil {
		t.Fatalf("khóa ngoài khoảng bị chặn oan: %v", err)
	}
	m.ReleaseAll(1)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestUpgradeAloneSucceeds(t *testing.T) {
	m := mgr(t)
	if err := m.Acquire(1, Key([]byte("k")), S); err != nil {
		t.Fatal(err)
	}
	if err := m.Acquire(1, Key([]byte("k")), X); err != nil {
		t.Fatalf("nâng cấp S->X một mình phải được: %v", err)
	}
	if got := m.Stats().Upgrades; got != 1 {
		t.Fatalf("Upgrades = %d, muốn 1", got)
	}
	// Xin lại X: đã giữ rồi, không được đếm thêm holder vô hạn.
	if err := m.Acquire(1, Key([]byte("k")), X); err != nil {
		t.Fatal(err)
	}
	if got := m.Held(1); got != 2 {
		t.Fatalf("giữ %d lock, muốn 2 (S và X), không được cộng dồn", got)
	}
}

// TestDeadlockUpgradeCycle là chính xác hình dạng của write skew ở mức
// Serializable: hai transaction cùng đọc (S), rồi cùng muốn ghi (X). Không ai
// nhường được, và đồ thị wait-for phải thấy điều đó — chứ không phải để cả hai
// treo tới lúc timeout.
func TestDeadlockUpgradeCycle(t *testing.T) {
	m := mgr(t)
	k := Key([]byte("k"))
	if err := m.Acquire(1, k, S); err != nil {
		t.Fatal(err)
	}
	if err := m.Acquire(2, k, S); err != nil {
		t.Fatal(err)
	}
	e1 := make(chan error, 1)
	e2 := make(chan error, 1)
	go func() { e1 <- tryLock(m, 1, k, X) }()
	waitFor(t, time.Second, "txn 1 chờ nâng cấp", func() bool { return m.Waiters() == 1 })
	go func() { e2 <- tryLock(m, 2, k, X) }()

	err1, err2 := <-e1, <-e2
	dead := 0
	if errors.Is(err1, ErrDeadlock) {
		dead++
	}
	if errors.Is(err2, ErrDeadlock) {
		dead++
	}
	if dead != 1 {
		t.Fatalf("phải có ĐÚNG một nạn nhân: err1=%v err2=%v", err1, err2)
	}
	// Nạn nhân là kẻ trẻ nhất -> txn 2.
	if !errors.Is(err2, ErrDeadlock) {
		t.Fatalf("nạn nhân phải là txn TRẺ nhất (2), nhưng txn 1 chết: %v", err1)
	}
	if got := m.Stats().Timeouts; got != 0 {
		t.Fatalf("Timeouts = %d — deadlock phải bị đồ thị bắt, không phải bị timeout bắt", got)
	}
}

func TestDeadlockTwoTxnTwoKeys(t *testing.T) {
	m := mgr(t)
	a, b := Key([]byte("a")), Key([]byte("b"))
	if err := m.Acquire(1, a, X); err != nil {
		t.Fatal(err)
	}
	if err := m.Acquire(2, b, X); err != nil {
		t.Fatal(err)
	}
	e1 := make(chan error, 1)
	e2 := make(chan error, 1)
	go func() { e1 <- tryLock(m, 1, b, X) }()
	waitFor(t, time.Second, "txn 1 chờ b", func() bool { return m.Waiters() == 1 })
	go func() { e2 <- tryLock(m, 2, a, X) }()

	err1, err2 := <-e1, <-e2
	if errors.Is(err1, ErrDeadlock) == errors.Is(err2, ErrDeadlock) {
		t.Fatalf("phải đúng một kẻ chết: err1=%v err2=%v", err1, err2)
	}
	if s := m.Stats().Deadlocks; s == 0 {
		t.Fatal("Deadlocks = 0 dù vừa có deadlock")
	}
}

func TestDeadlockThreeCycle(t *testing.T) {
	m := mgr(t)
	keys := []Res{Key([]byte("a")), Key([]byte("b")), Key([]byte("c"))}
	for i := uint64(1); i <= 3; i++ {
		if err := m.Acquire(i, keys[i-1], X); err != nil {
			t.Fatal(err)
		}
	}
	errs := make(chan error, 3)
	// 1 -> b(2), 2 -> c(3), 3 -> a(1). Mỗi goroutine xin xong thì KẾT THÚC
	// transaction (ReleaseAll) — nếu không, kẻ vừa lấy được lock lại thành
	// chướng ngại mới và bài test đo ra timeout thay vì đo ra deadlock.
	run := func(txn uint64, r Res) {
		err := tryLock(m, txn, r, X)
		m.ReleaseAll(txn)
		errs <- err
	}
	go run(1, keys[1])
	waitFor(t, time.Second, "cạnh 1->2", func() bool { return m.Waiters() == 1 })
	go run(2, keys[2])
	waitFor(t, time.Second, "cạnh 2->3", func() bool { return m.Waiters() == 2 })
	go run(3, keys[0])

	// Nạn nhân là txn 3 (trẻ nhất). Nó chết -> thả lock -> hai kẻ kia đi tiếp.
	got := 0
	for i := 0; i < 3; i++ {
		err := <-errs
		if errors.Is(err, ErrDeadlock) {
			got++
			continue
		}
		if err != nil {
			t.Fatalf("lỗi không mong đợi: %v", err)
		}
	}
	if got != 1 {
		t.Fatalf("chu trình 3 đỉnh phải giết đúng 1, giết %d", got)
	}
}

func TestTimeoutIsTheSafetyNet(t *testing.T) {
	m := New()
	m.Timeout = 80 * time.Millisecond
	if err := m.Acquire(1, Key([]byte("k")), X); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err := m.Acquire(2, Key([]byte("k")), X)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("muốn ErrTimeout, được %v", err)
	}
	if d := time.Since(start); d < 80*time.Millisecond {
		t.Fatalf("về sớm hơn timeout (%v) — cái đợi không thật", d)
	}
	if s := m.Stats(); s.Timeouts != 1 || s.Deadlocks != 0 {
		t.Fatalf("chờ không có chu trình phải là timeout, không phải deadlock: %+v", s)
	}
}

func TestKilledFlagClearedByRelease(t *testing.T) {
	m := mgr(t)
	m.mu.Lock()
	m.kill[7] = true
	m.mu.Unlock()
	if !m.Killed(7) {
		t.Fatal("Killed(7) phải true")
	}
	m.ReleaseAll(7)
	if m.Killed(7) {
		t.Fatal("ReleaseAll phải xóa dấu nạn nhân, nếu không transaction SAU dùng lại id đó sẽ chết oan")
	}
}
