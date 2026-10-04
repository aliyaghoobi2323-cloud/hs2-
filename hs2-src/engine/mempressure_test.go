package engine

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/xtaci/smux"
)

// underPressure sets this server's kernel TCP memory pressure for one test.
func underPressure(t *testing.T, local, peer bool) {
	t.Helper()
	tcpMemPressure.Store(local)
	peerMemPressure.Store(peer)
	t.Cleanup(func() { tcpMemPressure.Store(false); peerMemPressure.Store(false) })
}

// One paused reader per link never fills a link's buffer, so the guard left
// it — but twenty of them held enough kernel buffers to squeeze every socket
// on a 2 GB server. Under kernel TCP memory pressure the guard ends it, and a
// reader that still reads stays.
func TestWedgeGuardEndsPausedReaderUnderMemoryPressure(t *testing.T) {
	underPressure(t, true, false)
	r := newWedgeRig(t, true)
	paused := r.open(nil)
	reading := r.open(readAt(0))
	killed := 0
	for i := 0; i < 12 && killed == 0; i++ {
		killed += r.lookFast()
	}
	if killed != 1 {
		t.Fatalf("guard ended %d relays under memory pressure, want the paused one", killed)
	}
	select {
	case <-paused.ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the paused reader's relay did not end")
	}
	if ended(reading) || r.edge.IsClosed() {
		t.Fatal("the reading user or the link was closed")
	}
	if n := r.g.squeezed.Load(); n != 1 {
		t.Fatalf("%d relays counted as ended for memory pressure, want 1", n)
	}
}

// The other server's pressure (from its stats records) counts the same.
func TestWedgeGuardEndsPausedReaderUnderPeerMemoryPressure(t *testing.T) {
	underPressure(t, false, true)
	r := newWedgeRig(t, true)
	paused := r.open(nil)
	killed := 0
	for i := 0; i < 12 && killed == 0; i++ {
		killed += r.lookFast()
	}
	if killed != 1 || !func() bool { <-paused.ended; return true }() {
		t.Fatalf("guard ended %d relays under the other server's memory pressure, want 1", killed)
	}
}

// While a server's kernel TCP memory is above its pressure mark no link is
// judged — neither a lossy one nor a stuck one — and the log says why; once
// it is over (and the recovery window after), they are.
func TestNoVerdictsUnderMemoryPressure(t *testing.T) {
	underPressure(t, true, false)
	r := newStuckRig(t, 4)
	lf, lml := r.add()
	sf, sml := r.add()
	step := func() {
		lf.download(200<<10, 60) // ~30% resent
		waiting(sf, stuckWait+2*time.Second)
		r.step(sf, 2<<10)
	}
	for i := 0; i < 10; i++ {
		step()
		if lml.degraded || sml.degraded {
			t.Fatalf("judged under memory pressure (tick %d): lossy %v stuck %v\n%s", i+1, lml.degraded, sml.degraded, r.lg)
		}
	}
	if r.lg.count("kernel TCP memory on this server is above its pressure mark") != 1 {
		t.Fatalf("no pressure line:\n%s", r.lg)
	}
	tcpMemPressure.Store(false)
	for el := time.Duration(0); el < stuckRecover-healthTick; el += healthTick {
		step()
		if lml.degraded || sml.degraded {
			t.Fatalf("judged %s after the pressure:\n%s", el, r.lg)
		}
	}
	for i := 0; i < degradeStreak+2; i++ {
		step()
	}
	if !lml.degraded || !sml.degraded {
		t.Fatalf("not judged after the pressure and its recovery: lossy %v stuck %v\n%s", lml.degraded, sml.degraded, r.lg)
	}
}

// The edge learns the other server's pressure from its links' stats records.
func TestPeerMemoryPressureFromStatsRecords(t *testing.T) {
	underPressure(t, false, false)
	r := newStuckRig(t, 3)
	f, _ := r.add()
	f.m.peer.Store(&statsRec{seq: 1, flags: statsFlagMemPressure, at: r.clk.Now().Add(healthTick)})
	r.step(nil, 0)
	if !peerMemPressure.Load() {
		t.Fatal("a fresh record with the pressure flag did not set the other server's pressure")
	}
	if r.lg.count("kernel TCP memory on the other server") != 1 {
		t.Fatalf("no line naming the other server:\n%s", r.lg)
	}
	for i := 0; i < 4; i++ { // the record goes stale
		r.step(nil, 0)
	}
	if peerMemPressure.Load() {
		t.Fatal("a stale record kept the other server's pressure set")
	}
}

// The exit says in its stats records when its kernel TCP memory is above the
// pressure mark; the edge decodes the flag (an older edge ignores it).
func TestStatsRecordCarriesMemoryPressure(t *testing.T) {
	underPressure(t, true, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := newStatsPair(t, ctx, nil, func(ctx context.Context, srv *smux.Session, st *smux.Stream, mtr *linkMeter) {
		var kind [1]byte
		if _, err := io.ReadFull(st, kind[:]); err != nil || kind[0] != kindStats {
			st.Close()
			return
		}
		serveStats(ctx, st, nil, mtr)
	})
	p.startStats(t, ctx, nil)
	waitStatsState(t, p.edge.m, statsOK, 5*time.Second)
	if r := pollRecord(t, p.edge.m, 1); r.flags&statsFlagMemPressure == 0 {
		t.Fatalf("record under pressure without the flag: %+v", r)
	}
	tcpMemPressure.Store(false)
	if r := pollRecord(t, p.edge.m, 2); r.flags&statsFlagMemPressure != 0 {
		t.Fatalf("record after the pressure still flagged: %+v", r)
	}
}
