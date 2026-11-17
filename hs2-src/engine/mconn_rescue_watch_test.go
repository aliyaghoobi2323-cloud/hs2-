package engine

import (
	"testing"
	"time"
)

// TestRescueLinkState: the er92 per-link silence test of the RESCUE watcher.
func TestRescueLinkState(t *testing.T) {
	ms := func(d time.Duration) int64 { return int64(d) }
	now := ms(100 * time.Second)
	cases := []struct {
		name         string
		rd, wr, ps   int64
		silent, idle bool
	}{
		{"never read", 0, now, now, false, true},
		{"heard recently", now - ms(200*time.Millisecond), now, now - ms(100*time.Millisecond), false, false},
		{"quiet, nothing pending (alive)", now - ms(5*time.Second), now - ms(6*time.Second), now - ms(7*time.Second), false, false},
		{"quiet, ping just sent (alive, answer due)", now - ms(2900*time.Millisecond), now - ms(50*time.Millisecond), now - ms(50*time.Millisecond), false, false},
		{"lossy: pending 1.2 s (RTO backoff)", now - ms(2*time.Second), now - ms(100*time.Millisecond), now - ms(1200*time.Millisecond), false, false},
		{"dead: pending past rescuePend", now - ms(3*time.Second), now - ms(100*time.Millisecond), now - ms(2900*time.Millisecond), true, false},
	}
	for _, c := range cases {
		var mtr linkMeter
		mtr.lastRd.Store(c.rd)
		mtr.lastWr.Store(c.wr)
		mtr.pendSince.Store(c.ps)
		s, i := rescueLinkState(&mtr, now)
		if s != c.silent || i != c.idle {
			t.Errorf("%s: silent %v idle %v, want %v %v", c.name, s, i, c.silent, c.idle)
		}
	}
}

// TestRescueSilentSet: a blackholed path (all links pending, silent) is
// picked while another path hears; a lossy path whose links answer within a
// round trip is not; one silent link on an otherwise alive path is not.
func TestRescueSilentSet(t *testing.T) {
	now := int64(100 * time.Second)
	mk := func(path string, silent bool) *managedLink {
		mtr := &linkMeter{path: path}
		if silent {
			mtr.lastRd.Store(now - int64(3*time.Second))
			mtr.pendSince.Store(now - int64(2500*time.Millisecond))
			mtr.lastWr.Store(now - int64(10*time.Millisecond))
		} else {
			mtr.lastRd.Store(now - int64(100*time.Millisecond))
			mtr.pendSince.Store(now - int64(150*time.Millisecond))
			mtr.lastWr.Store(now - int64(10*time.Millisecond))
		}
		return &managedLink{link: &fakeLink{alive: true}, mtr: mtr}
	}
	m := &LinkManager{}
	for i := 0; i < 4; i++ {
		m.links = append(m.links, mk("a", true), mk("b", false), mk("c", i == 0))
	}
	set := m.rescueSilentSetLocked(now)
	n := 0
	for _, ml := range m.links {
		if set[ml.link] {
			n++
			if ml.mtr.path != "a" {
				t.Fatalf("picked a link of path %s", ml.mtr.path)
			}
		}
	}
	if n != 4 {
		t.Fatalf("picked %d links of the silent path, want 4", n)
	}
}

// er92: after a rescue (dead first sub) the connection stripes: the manage
// and stripe paths never use the abandoned raw1 again, every byte arrives
// once and in order, and the old sub is out of m.subs.
func TestRescueThenStripe(t *testing.T) {
	setStripeRate(t, 200<<10)
	n, iran, exit := newRescuePair(t)
	a := randBytes(4000, 61)
	both(t,
		func() error { return xfer(iran, a, 1000, 0) },
		func() error { return readN(exit, a) },
	)
	l0 := n.link(0)
	l0.iran.drop.Store(true)
	l0.exit.drop.Store(true)
	rescueNow(t, iran)
	n.hook = func(l *tLink) { l.exit.rate.Store(1 << 20) }
	iran.setStripe(3, nil)
	down, up := randBytes(4<<20, 62), randBytes(256<<10, 63)
	both(t,
		func() error { return xfer(exit, down, 32<<10, 0) },
		func() error { return readN(iran, down) },
		func() error { return xfer(iran, up, 4<<10, 5*time.Millisecond) },
		func() error { return readN(exit, up) },
	)
	for _, m := range []*mConn{iran, exit} {
		m.mu.Lock()
		old, in := m.rscOld, false
		for _, s := range m.subs {
			if s == old || s == m.raw1 && m.raw1 == old {
				in = true
			}
		}
		m.mu.Unlock()
		if old == nil || in {
			t.Fatalf("raw1 after rescue: old %v still in subs %v", old != nil, in)
		}
	}
	closeAndWait(t, iran, exit)
}

// er92: the rescue trigger racing Iran's own upgrade: whichever starts first
// wins, the other backs off (no protocol error), and the connection carries
// every byte once.
func TestRescueVsUpgradeRace(t *testing.T) {
	// er94: the -race flake (1 in 20) was the plain upgrade's stale read
	// deadline (raw1 died "read pipe: i/o timeout" right after "reader switched
	// at 0" with no rescue in that round): HS2_MCONN_DEADLINE_FIX, on in
	// production (envdef), off in the test binary.
	oldFix := mconnDeadlineFix.Load()
	mconnDeadlineFix.Store(true)
	t.Cleanup(func() { mconnDeadlineFix.Store(oldFix) })
	for i := 0; i < 20; i++ {
		_, iran, exit := newRescuePair(t)
		t.Cleanup(func() { dumpMTrace(t, iran, exit) })
		a := randBytes(3000, uint64(70+i))
		both(t,
			func() error { return xfer(iran, a, 1000, 0) },
			func() error { return readN(exit, a) },
		)
		f0 := rescueStat.failed.Load()
		go forceUpgrade(iran)
		iran.mu.Lock()
		ok := iran.rescueEligibleLocked(time.Now())
		if ok {
			iran.lightOpening = true
		}
		iran.mu.Unlock()
		if ok {
			iran.rescueStart()
		}
		c, d := randBytes(20000, uint64(100+i)), randBytes(20000, uint64(200+i))
		both(t,
			func() error { return xfer(iran, c, 2000, 0) },
			func() error { return readN(exit, c) },
			func() error { return xfer(exit, d, 2000, 0) },
			func() error { return readN(iran, d) },
		)
		for _, m := range []*mConn{iran, exit} {
			m.r.mu.Lock()
			extra := m.r.readyBytes
			m.r.mu.Unlock()
			if extra != 0 {
				t.Fatalf("run %d: %d bytes delivered twice", i, extra)
			}
		}
		_ = f0
		closeAndWait(t, iran, exit)
	}
}

// dumpMTrace logs the buffered mtrace events of ms when the test failed
// (HS2_MCONN_TRACE=1).
func dumpMTrace(t *testing.T, ms ...*mConn) {
	if !t.Failed() {
		return
	}
	for _, m := range ms {
		if m.tr == nil {
			continue
		}
		m.tr.mu.Lock()
		for _, l := range m.tr.buf {
			t.Log(l)
		}
		m.tr.mu.Unlock()
	}
}
