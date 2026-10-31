package engine

import (
	"context"
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
