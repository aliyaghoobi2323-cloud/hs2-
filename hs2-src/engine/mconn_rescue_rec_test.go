package engine

import (
	"testing"
	"time"
)

// newRRPair: a role-3 raw pair upgraded to records, both rings keeping,
// Iran told the exit takes RREC.
func newRRPair(t *testing.T) (*tNet, *mConn, *mConn) {
	t.Helper()
	old := rescueRecOn.Load()
	rescueRecOn.Store(true)
	t.Cleanup(func() { rescueRecOn.Store(old) })
	oldDF := mconnDeadlineFix.Load()
	mconnDeadlineFix.Store(true) // production default (envdef); see TestRescueVsUpgradeRace
	t.Cleanup(func() { mconnDeadlineFix.Store(oldDF) })
	n, iran, exit := newRescuePair(t)
	iran.mu.Lock()
	iran.rrPeer = true
	iran.mu.Unlock()
	a, b := randBytes(3000, 1), randBytes(4000, 2)
	both(t,
		func() error { return xfer(iran, a, 1000, 0) },
		func() error { return readN(exit, a) },
		func() error { return xfer(exit, b, 1000, 0) },
		func() error { return readN(iran, b) },
	)
	forceUpgrade(iran)
	for i := 0; i < 50 && !(rawDone(iran) && rawDone(exit)); i++ {
		c, d := randBytes(2000, uint64(100+i)), randBytes(2000, uint64(200+i))
		both(t,
			func() error { return xfer(iran, c, 1000, 0) },
			func() error { return readN(exit, c) },
			func() error { return xfer(exit, d, 1000, 0) },
			func() error { return readN(iran, d) },
		)
		time.Sleep(20 * time.Millisecond)
	}
	mWaitFor(t, "upgrade", time.Second, func() bool { return rawDone(iran) && rawDone(exit) })
	c, d := randBytes(5000, 3), randBytes(6000, 4)
	both(t,
		func() error { return xfer(iran, c, 1000, 0) },
		func() error { return readN(exit, c) },
		func() error { return xfer(exit, d, 1000, 0) },
		func() error { return readN(iran, d) },
	)
	return n, iran, exit
}

func rrLiveKeys(m *mConn) map[any]bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	set := map[any]bool{}
	for _, s := range m.rrLiveSubsLocked() {
		set[s.key] = true
	}
	return set
}

func rrNow(t *testing.T, m *mConn, set map[any]bool) {
	t.Helper()
	m.mu.Lock()
	ok := m.rrEligibleLocked(time.Now(), set)
	if ok {
		m.lightOpening = true
	}
	m.mu.Unlock()
	if !ok {
		t.Fatal("not eligible for a records rescue")
	}
	m.rrStart()
}

func blackhole(n *tNet, set map[any]bool) {
	for k := range set {
		l := n.link(k.(int))
		l.iran.drop.Store(true)
		l.exit.drop.Store(true)
	}
}

func noExtra(t *testing.T, ms ...*mConn) {
	t.Helper()
	for _, m := range ms {
		m.r.mu.Lock()
		extra := m.r.readyBytes
		m.r.mu.Unlock()
		if extra != 0 {
			t.Fatalf("%d bytes delivered twice", extra)
		}
	}
}

// Every live sub of a records conn blackholes with a request and a response
// in flight: the rescue delivers both, once, in order, and the conn goes on.
func TestRescueRecLostOnlySub(t *testing.T) {
	n, iran, exit := newRRPair(t)
	t.Cleanup(func() {
		if t.Failed() {
			t.Log(rescueRecLogLine())
			iran.mu.Lock()
			t.Logf("iran sendOff %d rrBi %d", iran.sendOff, iran.rrBi)
			iran.mu.Unlock()
			iran.rkMu.Lock()
			t.Logf("iran ring end %d n %d on %v", iran.rkEnd, iran.rk.n, iran.rk.on)
			iran.rkMu.Unlock()
		}
	})
	served0, acked0 := rrStat.served.Load(), rrStat.acked.Load()
	set := rrLiveKeys(iran)
	if len(set) == 0 {
		t.Fatal("no live sub")
	}
	blackhole(n, set)
	req, resp := randBytes(7000, 5), randBytes(9000, 6)
	if _, err := iran.Write(req); err != nil {
		t.Fatal(err)
	}
	if _, err := exit.Write(resp); err != nil {
		t.Fatal(err)
	}
	rrNow(t, iran, set)
	more, more2 := randBytes(30000, 7), randBytes(20000, 8)
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
	noExtra(t, iran, exit)
	if rrStat.served.Load() == served0 || rrStat.acked.Load() == acked0 {
		t.Fatal("no ACK served / read")
	}
	for k := range set {
		if rrLiveKeys(iran)[k] {
			t.Fatal("an abandoned sub still carries the conn")
		}
	}
	closeAndWait(t, iran, exit)
	mWaitFor(t, "rings freed", time.Second, func() bool {
		for _, m := range []*mConn{iran, exit} {
			m.rkMu.Lock()
			held := m.rk.buf != nil
			m.rkMu.Unlock()
			if held {
				return false
			}
			if _, ok := rrConns.Load(m); ok {
				return false
			}
		}
		return true
	})
}

// Dual: one live sub on a good link: no rescue.
func TestRescueRecDualOneDeadNoRescue(t *testing.T) {
	n, iran, exit := newRRPair(t)
	set := rrLiveKeys(iran)
	if len(set) < 2 {
		// open a second light sub (lite keeps one after the upgrade)
		sub, key, err := n.open(planeLight, nil)
		if err != nil {
			t.Fatal(err)
		}
		iran.mu.Lock()
		s := iran.addSubLocked(sub, key, false)
		iran.mu.Unlock()
		iran.startSub(s)
		set = rrLiveKeys(iran)
	}
	one := map[any]bool{}
	for k := range set {
		one[k] = true
		break
	}
	iran.mu.Lock()
	ok := iran.rrEligibleLocked(time.Now(), one)
	iran.mu.Unlock()
	if ok {
		t.Fatal("eligible with a live sub on a good link")
	}
	closeAndWait(t, iran, exit)
}

// NAK: the exit's ring is short (stopped) while bytes it wrote were lost;
// it answers NAK and keeps its subs; Iran resends its part; the conn is not
// ended and its upload goes on with no double delivery.
func TestRescueRecNakKeepsConn(t *testing.T) {
	n, iran, exit := newRRPair(t)
	nak0, nakS0 := rrStat.nakd.Load(), rrStat.nak.Load()
	set := rrLiveKeys(iran)
	for k := range set {
		n.link(k.(int)).exit.drop.Store(true) // exit → Iran lost
	}
	if _, err := exit.Write(randBytes(3000, 13)); err != nil {
		t.Fatal(err)
	}
	exit.rkMu.Lock()
	exit.rrStopLocked()
	exit.rkMu.Unlock()
	rrNow(t, iran, set)
	mWaitFor(t, "NAK", 2*time.Second, func() bool { return rrStat.nakd.Load() > nak0 && rrStat.nak.Load() > nakS0 })
	c := randBytes(10000, 9)
	both(t,
		func() error { return xfer(iran, c, 2000, 0) },
		func() error { return readN(exit, c) },
	)
	noExtra(t, exit)
	select {
	case <-iran.GetDieCh():
		t.Fatal("NAK ended the conn")
	default:
	}
	closeAndWait(t, iran, exit)
}

// A false trigger on a live path with an ACK: resent bytes are dropped by
// offset (no double delivery).
func TestRescueRecLiveNoDuplicate(t *testing.T) {
	_, iran, exit := newRRPair(t)
	rrNow(t, iran, rrLiveKeys(iran))
	c, d := randBytes(10000, 11), randBytes(10000, 12)
	both(t,
		func() error { return xfer(iran, c, 2000, 0) },
		func() error { return readN(exit, c) },
		func() error { return xfer(exit, d, 2000, 0) },
		func() error { return readN(iran, d) },
	)
	noExtra(t, iran, exit)
	closeAndWait(t, iran, exit)
}

// Mixed: an exit without the bit (rrPeer false) is never sent RREC; an exit
// with the key off refuses one.
func TestRescueRecMixedCapability(t *testing.T) {
	_, iran, exit := newRRPair(t)
	iran.mu.Lock()
	iran.rrPeer = false
	ok := iran.rrEligibleLocked(time.Now(), rrLiveKeys0(iran))
	iran.mu.Unlock()
	if ok {
		t.Fatal("eligible toward an exit without the bit")
	}
	rescueRecOn.Store(false)
	if exit.onRRescue(&msub{m: exit}, 0, 0) {
		t.Fatal("exit took RREC with the key off")
	}
	rescueRecOn.Store(true)
	closeAndWait(t, iran, exit)
}

// rrLiveKeys0: rrLiveKeys under a held mu.
func rrLiveKeys0(m *mConn) map[any]bool {
	set := map[any]bool{}
	for _, s := range m.rrLiveSubsLocked() {
		set[s.key] = true
	}
	return set
}
