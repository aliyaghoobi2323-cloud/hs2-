package engine

import (
	"bytes"
	"testing"
	"time"
)

func setRescue(t *testing.T, on bool) {
	old := rescueOn.Load()
	rescueOn.Store(on)
	t.Cleanup(func() { rescueOn.Store(old) })
}

func TestRescueRingBounds(t *testing.T) {
	memBefore := rescueStat.mem.Load()
	var r rescueRing
	r.on = true
	var all []byte
	for i := 0; i < 1000; i++ {
		p := randBytes(1+i%3000, uint64(i))
		r.add(p)
		all = append(all, p...)
	}
	if len(r.buf) != rescueRingCap() || r.n != rescueRingCap() {
		t.Fatalf("ring holds %d in %d, want cap %d", r.n, len(r.buf), rescueRingCap())
	}
	end := uint64(len(all))
	got, ok := r.get(end-1000, end)
	if !ok || !bytes.Equal(got, all[len(all)-1000:]) {
		t.Fatal("last 1000 bytes differ")
	}
	if _, ok := r.get(end-uint64(rescueRingCap())-1, end); ok {
		t.Fatal("a gap older than the ring was covered")
	}
	if got, ok := r.get(end, end); !ok || len(got) != 0 {
		t.Fatal("empty gap not covered")
	}
	r.release()
	if rescueStat.mem.Load() != memBefore {
		t.Fatalf("ring memory not returned: %d", rescueStat.mem.Load()-memBefore)
	}
	// the global cap disables a ring instead of growing it
	old := rescueMemMax
	rescueMemMax = rescueStat.mem.Load() + 1024
	defer func() { rescueMemMax = old }()
	var r2 rescueRing
	r2.on = true
	r2.add(make([]byte, 100))
	if r2.on || r2.buf != nil {
		t.Fatal("ring grew past the global cap")
	}
}

// newRescuePair: a raw pair whose first sub is role 3 (both rings on).
func newRescuePair(t *testing.T) (*tNet, *mConn, *mConn) {
	t.Helper()
	setRescue(t, true)
	n, iran, exit := newRawPair(t)
	iran.rescueEnable()
	exit.rescueEnable()
	return n, iran, exit
}

func rescueNow(t *testing.T, m *mConn) {
	t.Helper()
	m.mu.Lock()
	ok := m.rescueEligibleLocked(time.Now())
	if ok {
		m.lightOpening = true
	}
	m.mu.Unlock()
	if !ok {
		t.Fatal("not eligible for a rescue")
	}
	m.rescueStart()
}

// The first sub blackholes (nothing written on it arrives, it never answers)
// with a request and a response in flight: the rescue delivers both, once,
// in order, over the new sub, and the connection goes on.
func TestRescueDeadSubNeverAnswers(t *testing.T) {
	n, iran, exit := newRescuePair(t)
	a, b := randBytes(3000, 1), randBytes(5000, 2)
	both(t,
		func() error { return xfer(iran, a, 1000, 0) },
		func() error { return readN(exit, a) },
		func() error { return xfer(exit, b, 1000, 0) },
		func() error { return readN(iran, b) },
	)
	l0 := n.link(0)
	l0.iran.drop.Store(true)
	l0.exit.drop.Store(true)
	req, resp := randBytes(7000, 3), randBytes(9000, 4)
	if _, err := iran.Write(req); err != nil { // lost in the blackhole
		t.Fatal(err)
	}
	if _, err := exit.Write(resp); err != nil {
		t.Fatal(err)
	}
	rescueNow(t, iran)
	more, more2 := randBytes(40000, 5), randBytes(30000, 6)
	seq := func(fs ...func() error) func() error {
		return func() error {
			for _, f := range fs {
				if err := f(); err != nil {
					return err
				}
			}
			return nil
		}
	}
	both(t,
		seq(func() error { return readN(exit, req) }, func() error { return readN(exit, more) }),
		seq(func() error { return readN(iran, resp) }, func() error { return readN(iran, more2) }),
		func() error { return xfer(iran, more, 4000, 0) },
		func() error { return xfer(exit, more2, 4000, 0) },
	)
	if iran.rawW.Load() || exit.rawW.Load() || iran.rawR.Load() || exit.rawR.Load() {
		t.Fatal("still raw after the rescue")
	}
	mWaitFor(t, "rings freed", time.Second, func() bool {
		iran.wmu.Lock()
		exit.wmu.Lock()
		defer iran.wmu.Unlock()
		defer exit.wmu.Unlock()
		return iran.ring.buf == nil && exit.ring.buf == nil
	})
	closeAndWait(t, iran, exit)
}

// A false trigger on a live path: everything already delivered raw is resent
// too, and the receiver drops it by offset (no double delivery).
func TestRescueLivePathNoDuplicate(t *testing.T) {
	_, iran, exit := newRescuePair(t)
	a, b := randBytes(20000, 7), randBytes(20000, 8)
	both(t,
		func() error { return xfer(iran, a, 3000, 0) },
		func() error { return readN(exit, a) },
		func() error { return xfer(exit, b, 3000, 0) },
		func() error { return readN(iran, b) },
	)
	rescueNow(t, iran)
	c, d := randBytes(10000, 9), randBytes(10000, 10)
	both(t,
		func() error { return xfer(iran, c, 2000, 0) },
		func() error { return readN(exit, c) },
		func() error { return xfer(exit, d, 2000, 0) },
		func() error { return readN(iran, d) },
	)
	// nothing more than c / d may be readable
	for _, m := range []*mConn{iran, exit} {
		m.r.mu.Lock()
		extra := m.r.readyBytes
		m.r.mu.Unlock()
		if extra != 0 {
			t.Fatalf("%d bytes delivered twice", extra)
		}
	}
	closeAndWait(t, iran, exit)
}

// Mixed: a connection opened as plain raw (peer without the bit) never
// rescues; the exit refuses RESCUE on such a connection.
func TestRescueMixedCapability(t *testing.T) {
	setRescue(t, true)
	_, iran, exit := newRawPair(t)
	iran.mu.Lock()
	ok := iran.rescueEligibleLocked(time.Now())
	iran.mu.Unlock()
	if ok {
		t.Fatal("a role 2 connection is eligible")
	}
	rescueSilentLinks(map[any]bool{0: true})
	if !iran.rawW.Load() {
		t.Fatal("a role 2 connection left raw")
	}
	if exit.onRescue(&msub{m: exit}, 0, 0) {
		t.Fatal("exit took RESCUE on a role 2 connection")
	}
	closeAndWait(t, iran, exit)
}
