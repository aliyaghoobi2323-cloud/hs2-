package engine

import (
	"bytes"
	"errors"
	"io"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Tests of the raw first sub (lite + capMConnRaw, mconn_raw.go).

// newRawPair: a connected Iran/exit pair whose first sub is raw.
func newRawPair(t *testing.T) (*tNet, *mConn, *mConn) {
	t.Helper()
	setLite(t, true)
	resetLiteBudget()
	n := &tNet{exit: newMConnExitRaw(5), raw: true}
	iran, err := newMConnIran(5, n.open)
	if err != nil {
		t.Fatal(err)
	}
	if iran.raw1 == nil || !iran.rawW.Load() || liveSubs(iran) != 1 {
		t.Fatal("the first sub is not raw")
	}
	mWaitFor(t, "exit raw sub", time.Second, func() bool { return liveSubs(n.exit) == 1 })
	return n, iran, n.exit
}

// rawDone: both sides read and write records on every sub (handshake over).
func rawDone(m *mConn) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return !m.rawW.Load() && !m.rawR.Load() && !m.handshaking && m.raw1 != nil && !m.raw1.gated
}

// forceUpgrade starts Iran's upgrade now (instead of the 1 s rate rule).
func forceUpgrade(m *mConn) {
	m.mu.Lock()
	if m.rawTimer != nil {
		m.rawTimer.Stop()
	}
	if m.lightOpening || !m.rawW.Load() {
		m.mu.Unlock()
		return
	}
	m.lightOpening = true
	avoid := []any{m.raw1.key}
	m.mu.Unlock()
	go m.upgradeRaw(avoid)
}

// xfer writes data in random pieces with a pause between them.
func xfer(m *mConn, data []byte, piece int, pause time.Duration) error {
	for len(data) > 0 {
		k := min(len(data), piece)
		if _, err := m.Write(data[:k]); err != nil {
			return err
		}
		data = data[k:]
		if pause > 0 {
			time.Sleep(pause)
		}
	}
	return nil
}

func readN(m *mConn, want []byte) error {
	got := make([]byte, len(want))
	if _, err := io.ReadFull(m, got); err != nil {
		return err
	}
	if !bytes.Equal(got, want) {
		i := 0
		for got[i] == want[i] {
			i++
		}
		return errors.New("data differs at byte " + itoa(i))
	}
	return nil
}

// both runs the four halves of a two-way transfer and fails on any error.
func both(t *testing.T, fs ...func() error) {
	t.Helper()
	errs := make(chan error, len(fs))
	for _, f := range fs {
		go func() { errs <- f() }()
	}
	for range fs {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}

// Without an upgrade the bytes go raw: exactly the payload on the wire, no
// record, no second sub, no goroutine of the connection.
func TestMConnRawNoUpgrade(t *testing.T) {
	before := runtime.NumGoroutine()
	n, iran, exit := newRawPair(t)
	if g := runtime.NumGoroutine(); g > before {
		t.Fatalf("raw connection runs %d goroutines", g-before)
	}
	up, down := randBytes(3<<20, 21), randBytes(5<<20, 22)
	both(t,
		func() error { return xfer(iran, up, 32<<10, 0) },
		func() error { return xfer(exit, down, 32<<10, 0) },
		func() error { return readN(exit, up) },
		func() error { return readN(iran, down) },
	)
	l := n.link(0)
	if l.iran.wrote.Load() != int64(len(up)) || l.exit.wrote.Load() != int64(len(down)) {
		t.Fatalf("wire: %d/%d bytes, want exactly %d/%d", l.iran.wrote.Load(), l.exit.wrote.Load(), len(up), len(down))
	}
	if n.link(1) != nil || !iran.rawW.Load() {
		t.Fatal("a fast raw connection was upgraded")
	}
	closeAndWait(t, iran, exit)
	noLeak(t, before)
}

// A light connection upgrades by itself after ~1 s in the middle of a
// two-way transfer; every byte arrives in order both ways, and after the
// upgrade bigger bursts still go through.
func TestMConnRawUpgradeMidTransfer(t *testing.T) {
	before := runtime.NumGoroutine()
	n, iran, exit := newRawPair(t)
	up := append(randBytes(80<<10, 31), randBytes(2<<20, 32)...)
	down := append(randBytes(80<<10, 33), randBytes(3<<20, 34)...)
	slowThenFast := func(m *mConn, data []byte) func() error {
		return func() error {
			if err := xfer(m, data[:80<<10], 600, 20*time.Millisecond); err != nil { // ~30 KB/s for ~2.7 s
				return err
			}
			return xfer(m, data[80<<10:], 16<<10, 0)
		}
	}
	both(t,
		slowThenFast(iran, up), slowThenFast(exit, down),
		func() error { return readN(exit, up) },
		func() error { return readN(iran, down) },
	)
	if n.link(1) == nil {
		t.Fatal("no upgrade")
	}
	mWaitFor(t, "handshake over", 2*time.Second, func() bool { return rawDone(iran) && rawDone(exit) })
	// both subs carried records after the upgrade
	if n.link(1).iran.wrote.Load() == 0 || n.link(1).exit.wrote.Load() == 0 {
		t.Fatal("second sub unused")
	}
	closeAndWait(t, iran, exit)
	noLeak(t, before)
}

// Pings after the upgrade go on both subs: with either one stalled for
// 500 ms they still arrive in time.
func TestMConnRawPingsAfterUpgrade(t *testing.T) {
	before := runtime.NumGoroutine()
	n, iran, exit := newRawPair(t)
	var w1, l1, w2, l2 atomic.Int64
	var wg sync.WaitGroup
	pingPong(t, iran, exit, 200, &w1, &l1, &wg) // 4 s each way
	pingPong(t, exit, iran, 200, &w2, &l2, &wg)
	mWaitFor(t, "upgrade", mLiteAfter+2*time.Second, func() bool { return rawDone(iran) && rawDone(exit) })
	time.Sleep(200 * time.Millisecond)
	// stall the first (formerly raw) sub one way, the second one the other
	l1.Store(0)
	l2.Store(0)
	n.link(0).iran.stall(500 * time.Millisecond)
	n.link(1).exit.stall(500 * time.Millisecond)
	time.Sleep(700 * time.Millisecond)
	n.link(1).iran.stall(500 * time.Millisecond)
	n.link(0).exit.stall(500 * time.Millisecond)
	wg.Wait()
	t.Logf("delay max up %v down %v", time.Duration(l1.Load()), time.Duration(l2.Load()))
	if l1.Load() > int64(60*time.Millisecond) || l2.Load() > int64(60*time.Millisecond) {
		t.Fatal("a ping waited behind a stalled sub")
	}
	closeAndWait(t, iran, exit)
	noLeak(t, before)
}

// Close on either side at the moment of the upgrade: no hang, no leak, and
// the bytes written before the close all arrive.
func TestMConnRawUpgradeWithClose(t *testing.T) {
	before := runtime.NumGoroutine()
	upgraded := 0
	for i := 0; i < 24; i++ {
		_, iran, exit := newRawPair(t)
		closer, other := exit, iran
		if i%2 == 1 {
			closer, other = iran, exit
		}
		data := randBytes(20<<10+i*997, uint64(40+i))
		got := make(chan error, 1)
		go func() {
			b, err := io.ReadAll(other)
			if len(b) < len(data) || !bytes.Equal(b[:len(data)], data) {
				got <- errors.New("short or wrong: " + itoa(len(b)) + " of " + itoa(len(data)) + " (" + errString(err) + ")")
				return
			}
			got <- nil
		}()
		go func() { // the far side keeps reading what the closer gets
			io.Copy(io.Discard, closer)
		}()
		if err := xfer(closer, data, 4<<10, 0); err != nil {
			t.Fatal(err)
		}
		forceUpgrade(iran)
		time.Sleep(time.Duration(i%8) * 150 * time.Microsecond) // before, during, after the handshake
		if rawDone(iran) {
			upgraded++
		}
		closer.Close()
		select {
		case err := <-got:
			if err != nil {
				t.Fatalf("run %d: %v", i, err)
			}
		case <-time.After(mCloseLinger + 3*time.Second):
			t.Fatalf("run %d: reader hung", i)
		}
		other.Close()
		closeAndWait(t, iran, exit)
	}
	t.Logf("%d of 24 closed after the handshake", upgraded)
	noLeak(t, before)
}

func errString(err error) string {
	if err == nil {
		return "nil"
	}
	return err.Error()
}

// The second sub dies before the handshake is over: both sides end (no hang,
// no leak). A failed open of the second sub leaves the connection raw.
func TestMConnRawSecondSubDies(t *testing.T) {
	before := runtime.NumGoroutine()
	n, iran, exit := newRawPair(t)
	n.fail.Store(true)
	forceUpgrade(iran)
	time.Sleep(100 * time.Millisecond)
	if !iran.rawW.Load() || liveSubs(iran) != 1 {
		t.Fatal("a failed open changed the connection")
	}
	data := randBytes(100<<10, 51)
	both(t, func() error { return xfer(iran, data, 8<<10, 0) }, func() error { return readN(exit, data) })
	closeAndWait(t, iran, exit)

	n, iran, exit = newRawPair(t)
	go io.Copy(io.Discard, iran)
	go io.Copy(io.Discard, exit)
	// the exit cannot answer on the second sub; it dies mid-handshake
	n.fail.Store(false)
	n.hook = func(l *tLink) { l.exit.stall(time.Hour) }
	forceUpgrade(iran)
	mWaitFor(t, "second open", 2*time.Second, func() bool { return n.link(1) != nil })
	time.Sleep(50 * time.Millisecond)
	n.link(1).iran.Close()
	for _, m := range []*mConn{iran, exit} {
		select {
		case <-m.GetDieCh():
		case <-time.After(3 * time.Second):
			t.Fatal("connection did not end after the second sub died mid-handshake")
		}
	}
	closeAndWait(t, iran, exit)
	noLeak(t, before)
}

// An exit without capMConnRaw: the opener does not mark the first sub raw
// and the connection is today's lite (records from the first byte).
func TestMConnRawNotWithoutCap(t *testing.T) {
	pi := &peerInfo{V2: true, Caps: capMConn}
	if pi.mconnRaw() {
		t.Fatal("capMConn alone reads as raw-capable")
	}
	pi.Caps |= capMConnRaw
	if !pi.mconnRaw() {
		t.Fatal("capMConnRaw not seen")
	}
	if mconnOn.Load() && mconnCap()&capMConnRaw == 0 {
		t.Fatal("capMConnRaw not announced")
	}
	before := runtime.NumGoroutine()
	n, iran, exit := newLitePair(t)
	if iran.raw1 != nil || exit.raw1 != nil {
		t.Fatal("raw without the cap")
	}
	data := randBytes(64<<10, 61)
	both(t, func() error { return xfer(iran, data, 8<<10, 0) }, func() error { return readN(exit, data) })
	mWaitFor(t, "records on the wire", time.Second, func() bool { return n.link(0).iran.wrote.Load() > int64(len(data)) })
	closeAndWait(t, iran, exit)
	noLeak(t, before)
}

// liteBudTotals sums the budget's live buckets now.
func liteBudTotals() (all, dup uint64) {
	now := time.Now().Unix()
	for i := range liteBud.slots {
		s := &liteBud.slots[i]
		if sec := s.sec.Load(); sec > now-int64(mLiteCapWindow) && sec <= now {
			all += s.all.Load()
			dup += s.dup.Load()
		}
	}
	return
}

// The duplication budget counts both directions (received bytes too, raw
// bytes too): a big download with a few duplicated light connections is
// not capped; duplicates over 5% of both directions are.
func TestMConnLiteBudgetBothWays(t *testing.T) {
	before := runtime.NumGoroutine()
	// records: the receiver adds what it gets (the sender its own sends)
	_, iran, exit := newLitePair(t)
	down := randBytes(2<<20, 71)
	both(t, func() error { return xfer(exit, down, 32<<10, 0) }, func() error { return readN(iran, down) })
	if all, _ := liteBudTotals(); all < uint64(len(down))*19/10 {
		t.Fatalf("records: budget counts %d bytes for a %d-byte download (sender+receiver)", all, len(down))
	}
	closeAndWait(t, iran, exit)

	// raw: Iran's sample counts its received raw bytes
	_, iran, exit = newRawPair(t)
	both(t, func() error { return xfer(exit, down, 32<<10, 0) }, func() error { return readN(iran, down) })
	iran.rawBudget(time.Now())
	all, _ := liteBudTotals()
	if all < uint64(len(down)) {
		t.Fatalf("raw: budget counts %d bytes for a %d-byte download", all, len(down))
	}
	now := time.Now().Unix()
	liteBud.add(now, 0, int(all)*4/100) // a few light connections duplicated
	if liteBud.capped(now) {
		t.Fatal("capped at 4% duplicates")
	}
	liteBud.add(now, 0, int(all)*3/100)
	if !liteBud.capped(now) {
		t.Fatal("not capped at 7% duplicates")
	}
	closeAndWait(t, iran, exit)
	resetLiteBudget()
	noLeak(t, before)
}
