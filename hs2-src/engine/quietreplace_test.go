package engine

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

// R1 round 5, the engine review's B2 (quietreplace.go): a QUIET link slow
// next to the BULK links is replaced, make before break.

// replRig is a direct edge's pool (reviewBDialer counts its dials) of one
// QUIET link old enough to be judged and one serving BULK link, each with
// its control round trip at best (µs).
func replRig(t *testing.T, quietRTT, bulkRTT uint64) (*LinkManager, *quietSet, *classPair, *classPair, *reviewBDialer) {
	t.Helper()
	if QuietOff() {
		t.Skip("HS2_TUNE_QUIET=0")
	}
	withTune(t, QuietReplace, 1)
	withTune(t, QuietFollowGraceMs, 0)
	d := &reviewBDialer{}
	lm := NewLinkManager(d, 1, 4, 8, t.Logf)
	q := lm.enableQuiet()
	mk := func(class uint32) *classPair {
		up, dn := newAngleWire(angleRateBps, time.Millisecond), newAngleWire(angleRateBps, time.Millisecond)
		t.Cleanup(func() { up.close(); dn.close() })
		return newClassPair(t, up, dn, classCaps(class))
	}
	qp, bp := mk(tlscarrier.CapQuiet), mk(tlscarrier.CapBulk)
	if !q.place(qp.edge, "") {
		t.Fatal("QUIET link not placed")
	}
	q.quietLinks()[0].born = time.Now().Add(-time.Minute)
	addManaged(lm, bp.edge)
	qp.edge.mtr.rttMin.Store(quietRTT)
	bp.edge.mtr.rttMin.Store(bulkRTT)
	return lm, q, qp, bp, d
}

// replTicks runs n pool ticks of the set, each after feed (nil: none).
func replTicks(lm *LinkManager, q *quietSet, n int, feed func()) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for range n {
		if feed != nil {
			feed()
		}
		q.tick(ctx)
		lm.dialWG.Wait()
	}
}

// TestQuietReplaceSlow: a QUIET link whose round trip at best is 400 ms
// where the BULK link's is 75 (a DPI box delaying its five-tuple) is
// retired after quietSlowTicks ticks — not before — and one QUIET link more
// is wanted (dialed here: a direct edge); a new connection no longer opens
// on it. Once its replacement is up and it is empty it closes, and the set
// wants its count again. With the switch off it is kept, as before.
func TestQuietReplaceSlow(t *testing.T) {
	lm, q, qp, _, d := replRig(t, 400_000, 75_000)
	replTicks(lm, q, quietSlowTicks-1, nil)
	if q.replacing.Load() != 0 || d.quiet.Load() != 0 {
		t.Fatalf("replaced after %d ticks (replacing %d, dials %d)", quietSlowTicks-1, q.replacing.Load(), d.quiet.Load())
	}
	replTicks(lm, q, 1, nil)
	ql := q.quietLinks()[0]
	if !ql.replaced.Load() || !ql.retiring.Load() || q.replacing.Load() != 1 || q.wantNow() != 2 {
		t.Fatalf("not replaced: replaced %v retiring %v replacing %d want %d", ql.replaced.Load(), ql.retiring.Load(), q.replacing.Load(), q.wantNow())
	}
	replTicks(lm, q, 1, nil)
	if d.quiet.Load() == 0 {
		t.Fatal("no QUIET dial for the replacement")
	}
	if _, rel, ok := q.pick(nil, 1); ok {
		rel()
		t.Fatal("a new connection picked the slow link")
	}
	// the replacement comes up (as the reverse edge's next link would)
	up, dn := newAngleWire(angleRateBps, time.Millisecond), newAngleWire(angleRateBps, time.Millisecond)
	t.Cleanup(func() { up.close(); dn.close() })
	np := newClassPair(t, up, dn, classCaps(tlscarrier.CapQuiet))
	q.place(np.edge, "")
	if ql2, rel, ok := q.pick(nil, 1); !ok || ql2.link != np.edge {
		t.Fatalf("a new connection does not open on the replacement (%v)", ok)
	} else {
		rel()
	}
	replTicks(lm, q, 2, nil) // empty: closed (grace 0), then reaped
	if qp.edge.Alive() || q.replacing.Load() != 0 || q.wantNow() != 1 || q.count() != 1 {
		t.Fatalf("after the replacement: slow alive %v, replacing %d, want %d, count %d", qp.edge.Alive(), q.replacing.Load(), q.wantNow(), q.count())
	}
	if q.repl.replaced.Load() != 1 {
		t.Fatalf("replaced %d", q.repl.replaced.Load())
	}

	// off: kept
	withTune(t, QuietReplace, 0)
	lm2, q2, _, _, d2 := replRig(t, 400_000, 75_000)
	withTune(t, QuietReplace, 0)
	replTicks(lm2, q2, 2*quietSlowTicks, nil)
	if q2.replacing.Load() != 0 || d2.quiet.Load() != 0 {
		t.Fatal("replaced with the switch off")
	}
}

// TestQuietReplaceLoss: random loss that hits every link alike (the path's,
// 0.3% each way) replaces nothing; a QUIET link that alone resends 10% of
// what it receives (a policer on its five-tuple) is replaced; and a path
// whose every new QUIET link is slow stops after replBudget replacements.
func TestQuietReplaceLoss(t *testing.T) {
	lm, q, qp, bp, _ := replRig(t, 75_000, 75_000)
	var n uint64
	var qrt, brt uint64
	var segs uint32
	feed := func(qLoss, bLoss uint64) func() {
		return func() {
			n++
			segs += 1000
			qrt += qLoss
			brt += bLoss
			qp.edge.mtr.peerLoss.Store(&peerLossRec{n: n, rt: qrt, segsIn: segs})
			bp.edge.mtr.peerLoss.Store(&peerLossRec{n: n, rt: brt, segsIn: segs})
		}
	}
	replTicks(lm, q, 4*quietSlowTicks, feed(3, 3))
	if q.replacing.Load() != 0 {
		t.Fatal("the path's random loss replaced the QUIET link")
	}
	replTicks(lm, q, quietSlowTicks+2, feed(100, 3))
	if q.replacing.Load() != 1 {
		t.Fatalf("a QUIET link resending 10%% alone was not replaced (%s)", q.repl.links[q.quietLinks()[0]].why)
	}
	// the budget: replBudget in replWindow, replGap apart
	q.repl.done = []time.Time{time.Now().Add(-5 * time.Minute), time.Now().Add(-4 * time.Minute), time.Now().Add(-3 * time.Minute)}
	q.replacing.Store(0)
	ql := q.quietLinks()[0]
	ql.replaced.Store(false)
	ql.retiring.Store(false)
	replTicks(lm, q, quietSlowTicks+2, feed(100, 3))
	if q.replacing.Load() != 0 || q.repl.held.IsZero() {
		t.Fatalf("past the budget: replacing %d, held %v", q.replacing.Load(), q.repl.held)
	}
}

// TestQuietReplaceRoom: with the switch on a reverse exit may hold one link
// past its count for a replacement, even with its own count above
// quietPeerMax's room (5 QUIET links past 5 with the light one); what it
// reports to the edge does not change.
func TestQuietReplaceRoom(t *testing.T) {
	defer func(x int) { QuietExtra = x }(QuietExtra)
	QuietExtra = 1
	for _, c := range []struct {
		replace, max, own int
		follow            bool
		report, hi        int
	}{
		{0, 8, 5, true, 13, 13},
		{1, 8, 5, true, 13, 14},
		{1, 1, 2, true, 3, 5},
		{1, 8, 4, false, 12, 13},
		{0, 8, 4, false, 12, 12},
	} {
		withTune(t, QuietReplace, c.replace)
		if r, h := exitRevCeilings(c.max, c.own, c.follow); r != c.report || h != c.hi {
			t.Errorf("replace %d, exitRevCeilings(%d, %d, %v) = %d %d, want %d %d", c.replace, c.max, c.own, c.follow, r, h, c.report, c.hi)
		}
	}
}

// TestL3Avoid: no flow of the TUN lands on a QUIET link marked avoid (the
// light one, a slow one), and a flow pinned on one that gets the mark moves
// at its next packet.
func TestL3Avoid(t *testing.T) {
	q1, q2 := &l3Link{id: 1, class: l3Quiet}, &l3Link{id: 2, class: l3Quiet}
	var edge l3Set
	edge.add(q1)
	edge.add(q2)
	q1.avoid.Store(true)
	for h := uint32(1); h < 2000; h++ {
		if l := edge.linkForPkt(h, 100, false); l != q2 {
			t.Fatalf("flow %d on the avoided link", h)
		}
	}
	q1.avoid.Store(false)
	q2.avoid.Store(true)
	for h := uint32(1); h < 2000; h++ {
		if l := edge.linkForPkt(h, 100, false); l != q1 {
			t.Fatalf("flow %d stayed on the avoided link", h)
		}
	}
}

// TestQuietReplaceReverse: the reverse pool over real TLS on the loopback, as
// the field runs it. The one QUIET link lags (its control ping waits, as on
// a connection a DPI box holds): the edge asks the exit for one link more,
// that link comes up QUIET next to the slow one, the connection on the slow
// one moves there and goes on, in order, and the slow one closes; the pool
// settles at one QUIET and one BULK again, the BULK untouched.
func TestQuietReplaceReverse(t *testing.T) {
	if testing.Short() {
		t.Skip("real links, ~15 s")
	}
	withTune(t, QuietReplace, 1)
	withTune(t, QuietFollowGraceMs, 200)
	prevAge := replMinAge
	replMinAge = time.Second
	t.Cleanup(func() { replMinAge = prevAge })
	p := startQuietPoolMix(t, false, 1, quietMix{})
	p.waitFor("one QUIET and one BULK", 30*time.Second, func() bool { return p.q.count() == 1 && p.serving() == 1 })
	c, err := net.Dial("tcp", p.userAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	echo := func(off int) int {
		t.Helper()
		b := make([]byte, 2000)
		for i := range b {
			b[i] = byte((off + i) * 7)
		}
		if _, err := c.Write(b); err != nil {
			t.Fatal(err)
		}
		g := make([]byte, len(b))
		c.SetReadDeadline(time.Now().Add(20 * time.Second))
		if _, err := io.ReadFull(c, g); err != nil {
			t.Fatalf("echo at %d: %v", off, err)
		}
		for i := range g {
			if g[i] != b[i] {
				t.Fatalf("echo byte %d out of order", off+i)
			}
		}
		return off + len(b)
	}
	off := echo(0)
	slow := p.q.quietLinks()[0]
	stop := make(chan struct{})
	defer close(stop)
	go func() { // its oldest control ping has waited 30 s
		for {
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
				slow.link.mtr.ctrlWait.Store(ctrlNow() - int64(30*time.Second))
			}
		}
	}()
	p.waitFor("a QUIET link replaced", 30*time.Second, func() bool { return p.q.repl.replaced.Load() == 1 })
	p.waitFor("the new QUIET link up, the connection on it, the slow one closed", 30*time.Second, func() bool {
		off = echo(off)
		return !slow.link.Alive() && p.q.count() == 1 && p.q.movedGeneral.Load() == 1
	})
	if nl := p.q.quietLinks()[0]; nl == slow || nl.link == slow.link {
		t.Fatal("the QUIET link left is the slow one")
	}
	p.waitFor("the count back", 10*time.Second, func() bool { return p.q.replacing.Load() == 0 && p.q.wantNow() == 1 && p.serving() == 1 })
	off = echo(off)
	p.echoBulk(1 << 20) // a new connection opens on the new QUIET link and moves
	t.Logf("%d echoed in order across the replacement: %s", off, p.q.line())
}
