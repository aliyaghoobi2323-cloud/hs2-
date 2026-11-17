package engine

import (
	"errors"
	"net"
	"testing"
	"time"
)

// rr156 (mconn_rescue_rst.go): every sub died at once (a fresh link carries
// the RRST, the exit holds), and the explicit refusal of a peer without the
// conn.

// rstKillAll: every link swallows what is written on it, req/resp are
// written, then every link resets.
func rstKillAll(t *testing.T, n *tNet, iran, exit *mConn, req, resp []byte) {
	t.Helper()
	n.mu.Lock()
	ls := append([]*tLink(nil), n.links...)
	n.mu.Unlock()
	for _, l := range ls {
		l.iran.drop.Store(true)
		l.exit.drop.Store(true)
	}
	if _, err := iran.Write(req); err != nil {
		t.Fatal(err)
	}
	if _, err := exit.Write(resp); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	for _, l := range ls {
		l.iran.Close()
		l.exit.Close()
	}
}

func rstBigRing(t *testing.T) {
	setRRRingCap(t, 512<<10)
}

func rstShortGrace(t *testing.T, d time.Duration) {
	old := rstGrace
	rstGrace = d
	t.Cleanup(func() { rstGrace = old })
}

// Every sub dies by RST with bytes in flight both ways; a fresh link is
// there: Iran's role-5 sub carries the RRST, the exit held the conn, both
// directions arrive byte-exact and the conn goes on.
func TestRescueRstAllDiedSurvives(t *testing.T) {
	rstBigRing(t)
	setRst(t, true, true)
	n, iran, exit := newRstPair(t)
	all0, join0, acked0 := rstStat.allDied.Load(), rstStat.join.Load(), rstStat.acked.Load()
	req, resp := randBytes(20000, 61), randBytes(30000, 62)
	rstKillAll(t, n, iran, exit, req, resp)
	more, more2 := randBytes(40000, 63), randBytes(50000, 64)
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
	// the exit holds only when Iran's role-5 sub comes after its last sub's
	// death (else it reasks on that sub): hold is not required
	if rstStat.allDied.Load() == all0 || rstStat.join.Load() == join0 || rstStat.acked.Load() == acked0 {
		t.Fatalf("no all-died rescue ran: %s", rescueRstLogLine())
	}
	t.Log(rescueRstLogLine())
	closeAndWait(t, iran, exit)
}

// Every sub dies and no link can be opened: Iran ends at once, the exit's
// hold ends within the grace; nothing hangs.
func TestRescueRstAllDiedNoLink(t *testing.T) {
	rstBigRing(t)
	rstShortGrace(t, time.Second)
	setRst(t, true, true)
	n, iran, exit := newRstPair(t)
	n.fail.Store(true)
	t0 := time.Now()
	rstKillAll(t, n, iran, exit, randBytes(20000, 71), randBytes(20000, 72))
	mWaitFor(t, "iran end", time.Second, func() bool { return rstEnded(iran) })
	di := time.Since(t0)
	mWaitFor(t, "exit end", rstGrace+time.Second, func() bool { return rstEnded(exit) })
	de := time.Since(t0)
	if de < rstGrace-100*time.Millisecond {
		t.Fatalf("exit ended after %v, before its hold (%v): %v", de, rstGrace, exit.err)
	}
	if !errors.Is(iran.err, errMConnSubsDied) || !errors.Is(exit.err, errMConnSubsDied) {
		t.Fatalf("errors: iran %v exit %v", iran.err, exit.err)
	}
	t.Logf("iran ended after %v, exit after %v | %s", di.Round(time.Millisecond), de.Round(time.Millisecond), rescueRstLogLine())
}

// The live sub left is a zombie (its exit end is gone: what it carries
// vanishes) and the exit no longer has the conn: the role-5 probe after
// rstProbe is refused with an RST and Iran ends at once, not after rstGrace.
func TestRescueRstRefusedProbe(t *testing.T) {
	rstBigRing(t)
	setRst(t, true, true)
	n, iran, exit := newRstPair(t)
	nak0, probe0 := rstStat.nakRead.Load(), rstStat.probe.Load()
	refused := make(chan struct{}, 4)
	iran.mu.Lock()
	inner := iran.open
	iran.open = func(p mPlane, avoid []any) (mSub, any, error) {
		if _, rj := takeRstJoinMark(append([]any(nil), avoid...)); rj {
			// the exit without the conn (serveMConnSub, role 5)
			a, b := net.Pipe()
			go func() {
				b.Write([]byte{mRecRst})
				b.Close()
			}()
			refused <- struct{}{}
			return newTSub(a), "refused", nil
		}
		return inner(p, avoid)
	}
	iran.mu.Unlock()
	p := iran.primary.Load()
	for _, l := range n.links {
		if l.key != p.key.(int) {
			l.iran.drop.Store(true) // the zombie: the RRST vanishes
			l.exit.drop.Store(true)
		}
	}
	t0 := time.Now()
	rstKillPrimary(t, n, iran, exit, randBytes(20000, 81), randBytes(20000, 82))
	mWaitFor(t, "iran end", rstGrace, func() bool { return rstEnded(iran) })
	d := time.Since(t0)
	if d > rstProbe+time.Second {
		t.Fatalf("iran ended after %v (probe %v, grace %v)", d, rstProbe, rstGrace)
	}
	if !errors.Is(iran.err, errMConnReset) {
		t.Fatalf("iran err %v, want the refusal's reset", iran.err)
	}
	if len(refused) == 0 || rstStat.nakRead.Load() == nak0 || rstStat.probe.Load() == probe0 {
		t.Fatalf("no refused probe: %s", rescueRstLogLine())
	}
	t.Logf("iran ended after %v: %v | %s", d.Round(time.Millisecond), iran.err, rescueRstLogLine())
	mWaitFor(t, "exit end", rstGrace+2*time.Second, func() bool { return rstEnded(exit) })
}
