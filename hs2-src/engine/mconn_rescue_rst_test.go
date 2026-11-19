package engine

import (
	"fmt"
	"testing"
	"time"
)

// rr155 (mconn_rescue_rst.go): an unclean death of the sub carrying the only
// copy of bytes in flight, with another sub left.

func setRst(t *testing.T, on, peer bool) {
	t.Helper()
	old, oldP := rescueRstOn.Load(), rstPeerOK
	rescueRstOn.Store(on)
	rstPeerOK = func(*msub) bool { return peer }
	t.Cleanup(func() { rescueRstOn.Store(old); rstPeerOK = oldP })
}

// newRstPair: a records pair (newRRPair) with two live light subs on each
// side, both directions single-copy on the primary (dupOff, as a call).
func newRstPair(t *testing.T) (*tNet, *mConn, *mConn) {
	t.Helper()
	n, iran, exit := newRRPair(t)
	for i := 0; i < 100 && (rstLive(iran) < 2 || rstLive(exit) < 2); i++ {
		if i == 0 && rstLive(iran) < 2 {
			go iran.openLight(nil)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if rstLive(iran) < 2 || rstLive(exit) < 2 {
		t.Fatalf("live subs iran %d exit %d, want 2: iran %s exit %s", rstLive(iran), rstLive(exit), rstDumpSubs(iran), rstDumpSubs(exit))
	}
	for _, m := range []*mConn{iran, exit} {
		m.mu.Lock()
		m.dupOff = true
		m.dupStart = time.Now()
		m.dupBytes = 1 << 20
		m.mu.Unlock()
	}
	a, b := randBytes(60000, 11), randBytes(60000, 12)
	both(t,
		func() error { return xfer(iran, a, 2000, 0) },
		func() error { return readN(exit, a) },
		func() error { return xfer(exit, b, 2000, 0) },
		func() error { return readN(iran, b) },
	)
	return n, iran, exit
}

// rstKillPrimary: Iran's primary link swallows what is written on it (both
// ends), req/resp are written (req on that link only), then the link resets.
func rstKillPrimary(t *testing.T, n *tNet, iran, exit *mConn, req, resp []byte) {
	t.Helper()
	p := iran.primary.Load()
	if p == nil {
		t.Fatal("no primary")
	}
	l := n.link(p.key.(int))
	l.iran.drop.Store(true)
	l.exit.drop.Store(true)
	if _, err := iran.Write(req); err != nil {
		t.Fatal(err)
	}
	if _, err := exit.Write(resp); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	l.iran.Close() // the link's RST: both ends see the stream die
	l.exit.Close()
}

func rstEnded(m *mConn) bool {
	select {
	case <-m.GetDieCh():
		return true
	default:
		return false
	}
}

// The sub holding the only copy dies by RST with a healthy sub left: both
// directions arrive byte-exact, once, and the conn goes on.
func TestRescueRstSurvives(t *testing.T) {
	// the test writes faster than the ring learns a rate: a ring of 3 s of
	// a call (cd152) is what production keeps
	setRRRingCap(t, 512<<10)
	setRst(t, true, true)
	n, iran, exit := newRstPair(t)
	asked0, served0, acked0 := rstStat.asked.Load(), rstStat.served.Load(), rstStat.acked.Load()
	req, resp := randBytes(20000, 21), randBytes(30000, 22)
	rstKillPrimary(t, n, iran, exit, req, resp)
	more, more2 := randBytes(40000, 23), randBytes(50000, 24)
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
		func() error { return xfer(iran, more, 2000, 0) },
		func() error { return xfer(exit, more2, 2000, 0) },
	)
	if rstEnded(iran) || rstEnded(exit) {
		t.Fatalf("conn ended: iran %v exit %v", iran.err, exit.err)
	}
	noExtra(t, iran, exit)
	if rstStat.asked.Load() == asked0 || rstStat.served.Load() == served0 || rstStat.acked.Load() == acked0 {
		t.Fatalf("no rescue ran: %s", rescueRstLogLine())
	}
	t.Log(rescueRstLogLine())
	closeAndWait(t, iran, exit)
}

// The lost bytes reach back past the ring: the conn ends (errMConnLost or
// the peer's RST), within the grace, on both sides.
func TestRescueRstBeyondRing(t *testing.T) {
	setB375(t, false) // b375's in-flight floor would hold these 40 KB (TestB375RstNowCovered)
	setRst(t, true, true)
	oldSecs := rrRateSecs
	rrRateSecs = 0
	t.Cleanup(func() { rrRateSecs = oldSecs })
	setRRRingCap(t, 8<<10)
	n, iran, exit := newRstPair(t)
	req, resp := randBytes(40000, 31), randBytes(40000, 32)
	t0 := time.Now()
	rstKillPrimary(t, n, iran, exit, req, resp)
	mWaitFor(t, "both ends", rstGrace+2*time.Second, func() bool { return rstEnded(iran) && rstEnded(exit) })
	t.Logf("ended after %v: iran %v exit %v | %s", time.Since(t0).Round(time.Millisecond), iran.err, exit.err, rescueRstLogLine())
}

// The peer does not announce the bit: the old outcome (errMConnLost at once),
// no RRST.
func TestRescueRstMixedVersion(t *testing.T) {
	setRst(t, true, false)
	n, iran, exit := newRstPair(t)
	asked0 := rstStat.asked.Load()
	req, resp := randBytes(20000, 41), randBytes(20000, 42)
	t0 := time.Now()
	rstKillPrimary(t, n, iran, exit, req, resp)
	mWaitFor(t, "both ends", 2*time.Second, func() bool { return rstEnded(iran) && rstEnded(exit) })
	if d := time.Since(t0); d > 1500*time.Millisecond {
		t.Fatalf("old path took %v (a grace ran?)", d)
	}
	if rstStat.asked.Load() != asked0 {
		t.Fatal("RRST sent toward a peer without the bit")
	}
}

// Key off (the default): the old outcome.
func TestRescueRstKeyOff(t *testing.T) {
	setRst(t, false, true)
	n, iran, exit := newRstPair(t)
	asked0 := rstStat.asked.Load()
	rstKillPrimary(t, n, iran, exit, randBytes(20000, 51), randBytes(20000, 52))
	mWaitFor(t, "both ends", 2*time.Second, func() bool { return rstEnded(iran) && rstEnded(exit) })
	if rstStat.asked.Load() != asked0 {
		t.Fatal("RRST sent with the key off")
	}
}

func rstDumpSubs(m *mConn) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := ""
	for _, s := range m.subs {
		out += fmt.Sprintf("[idx %d key %v dead %v ret %v bulk %v closeHS %v closeAfter %v gated %v] ", s.idx, s.key, s.dead, s.retired, s.bulk.Load(), s.closeHS, s.closeAfter, s.gated)
	}
	return out + fmt.Sprintf("raw1 %v rawW %v rawR %v lite %v", m.raw1 != nil, m.rawW.Load(), m.rawR.Load(), m.lite)
}

func rstLive(m *mConn) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.rrLiveSubsLocked())
}
