package engine

import (
	"errors"
	"math"
	"sync/atomic"
	"testing"
	"time"
)

// b443 (lab r443 op5): a path in its dial cooldown at a Kharej hand-off. The
// old judge dropped it (forget(keep)), so LBSTATE left it out and the new LB
// started it uncooled at factor 1 ("no reading yet"). Now it rides LBSTATE
// with its factor, cut state and the cooldown left, and the new LB cools it
// for the rest at the old factor.
func TestLBSeedCarriesCooledPath(t *testing.T) {
	withAuto2(t)
	t0 := time.Unix(7000, 0)
	tOld := t0
	old := seedTestLB(t, &tOld)
	seedFeed(old, &tOld, 3, 0.003, 0.003, 0.003)
	seedFeed(old, &tOld, 3, 0.003, 0.35, 0.003)
	if q := old.judge.p["k>b"]; q == nil || !q.cut || old.mult[1] > 0.11 {
		t.Fatalf("old judge: path b not cut (factor %.2f)", old.mult[1])
	}
	// b fails its dials and is cooled for 2 m; the judge then drops it.
	old.mu.Lock()
	old.links[1]++
	old.probing[1] = true // a failed probe dial cools it (as after a cut path's failed dials)
	old.resultLocked(1, true, nil, errors.New("i/o timeout"))
	cooled := !old.until[1].IsZero()
	old.mu.Unlock()
	if !cooled {
		t.Fatal("path b not cooled after its failed dials")
	}
	seedFeed(old, &tOld, 2, 0.003, -1, 0.003)
	if old.judge.p["k>b"] != nil {
		t.Fatal("the old judge kept the cooled path (test premise)")
	}
	left := old.until[1].Sub(tOld)
	wall := time.Now()
	doc := seedDoc(t, old, wall)

	tNew := tOld.Add(2 * time.Second)
	nw := seedTestLB(t, &tNew)
	nw.applySeed(doc, wall.Add(2*time.Second), false)
	nw.mu.Lock()
	until, mult := nw.until[1], nw.mult[1]
	nw.mu.Unlock()
	if until.IsZero() {
		t.Fatalf("new LB: path b not cooled (factor %.2f): it would be dialled at once at 'no reading yet'", mult)
	}
	if d := until.Sub(tNew); d < left-3*time.Second || d > left {
		t.Fatalf("new LB: cooled for %s more, old had %s left", d, left)
	}
	if mult > 0.11 {
		t.Fatalf("new LB: path b factor %.2f, want the old 0.10", mult)
	}
	if r := nw.report(); r.Paths[1].Factor > 0.11 {
		t.Fatalf("new LB report: path b factor %.2f (the edge would steer onto it)", r.Paths[1].Factor)
	}
	// The other paths seed as before.
	if q := nw.judge.p["k>a"]; q == nil || !q.seeded {
		t.Fatal("path a not seeded")
	}
	// A later hand-off from the new process passes the cooled path on.
	seedFeed(nw, &tNew, 1, 0.003, -1, 0.003)
	d2 := seedDoc(t, nw, wall.Add(10*time.Second))
	found := false
	for _, sp := range d2.L[0].P {
		if sp.N == "k>b" {
			found = sp.Cool > 0 && sp.Cut && sp.M < 0.11
		}
	}
	if !found {
		t.Fatalf("second hand-off: cooled path b not carried with its cut: %+v", d2.L[0].P)
	}
	// A dial the new process had in flight before the seed succeeds: the path
	// is back early, with the inherited cut (not judged afresh: 0.10 -> 1.00).
	{
		tE := tNew
		e := seedTestLB(t, &tE)
		e.applySeed(doc, wall.Add(2*time.Second), false)
		e.mu.Lock()
		e.links[1]++
		e.resultLocked(1, false, &fakeCarrier{}, nil)
		back := e.until[1].IsZero()
		e.mu.Unlock()
		if !back {
			t.Fatal("premise: the in-flight dial's success ends the cooldown")
		}
		seedFeed(e, &tE, 2, 0.003, -1, 0.003)
		if q := e.judge.p["k>b"]; q == nil || q.reason == "no reading yet" || e.mult[1] > 0.11 {
			t.Fatalf("back early: path b factor %.2f (inherited state not restored: judged afresh)", e.mult[1])
		}
	}
	// After the cooldown: back at the old factor, its next dial is the probe.
	tNew = until.Add(time.Second)
	nw.mu.Lock()
	j, probe := nw.acquireLocked()
	m := nw.mult[1]
	nw.mu.Unlock()
	if j != 1 || !probe || math.Abs(m-mult) > 1e-9 {
		t.Fatalf("after the cooldown: dial %d probe %v factor %.2f", j, probe, m)
	}
}

// b443 (lab r443 op4): the boot spares must not go to the path just back from
// its cooldown (probe first) or the one with the fewest links (two per path):
// a boot spare dials a proven path.
func TestZDUBootSparePicksProvenPath(t *testing.T) {
	withAuto2(t)
	now := time.Unix(9000, 0)
	b := seedTestLB(t, &now)
	b.mu.Lock()
	b.links[0], b.links[1], b.links[2] = 60, 60, 0
	b.mult[2] = 1                      // "no reading yet" after its cooldown
	b.until[2] = now.Add(-time.Second) // cooldown over: its probe is due
	b.mult[1] = 0.3                    // a weak path: not for a boot spare
	j, probe := b.acquireLocked()      // the usual pick
	b.links[j]--
	b.dials[j]--
	if probe {
		b.probing[j] = false
	}
	b.mu.Unlock()
	if j != 2 {
		t.Fatalf("premise: the usual pick is the probe path 2, got %d", j)
	}
	var got []string
	for i := 0; i < 4; i++ {
		_, err := b.dialViaPick(func(p LBPath) (any, error) {
			got = append(got, p.Name())
			return &fakeCarrier{}, nil
		}, true)
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range got {
		if n != "k>a" {
			t.Fatalf("boot spares dialled %v, want only the proven path k>a", got)
		}
	}
}

// b443: when the round's boot spares hang (the lossy path; 19 s TLS
// handshakes in r443 op4) and no link of a new Iran instance comes, the next
// poolctl refresh after zduBootRetry starts one more round; the spares of a
// round that reach an Iran process (no handover) start none.
func TestZDUExitBootRetriesUntilNewInstance(t *testing.T) {
	setZDU(t, true)
	zduResetExitStat(t)
	const hi = 6
	zp := newZDUExitPool(t, 1, hi)
	var hang atomic.Int32
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	inner := zp.exitPool.dial
	zp.exitPool.dial = func() (dialedLink, error) {
		if hang.Load() > 0 && hang.Add(-1) >= 0 {
			select {
			case <-stop:
			case <-time.After(20 * time.Second):
			}
			return nil, errors.New("i/o timeout")
		}
		return inner()
	}
	zp.setIran(zduInstOld)
	zp.setTarget(hi)
	eventually(t, "old pool at its ceiling", func() bool { return zp.liveOf(zduInstOld, false) == hi })

	// No handover: an overshoot's spares reach the running Iran: no retry.
	r0 := zduExitBootRetryN.Load()
	zp.setTarget(hi + 1)
	eventually(t, "overshoot spares up", func() bool { return zp.liveOf(zduInstOld, false) == hi+zduExitBoot })
	time.Sleep(zduBootRetry + 200*time.Millisecond)
	zp.setTarget(hi + 1)
	zp.setTarget(hi)
	if n := zduExitBootRetryN.Load() - r0; n != 0 {
		t.Fatalf("no handover: %d extra boot round(s) (churn)", n)
	}

	// The handover: the first round's spares hang.
	zp.setIran(zduInstNew)
	hang.Store(zduExitBoot)
	zp.setTarget(hi + hi) // the old Iran's boost
	time.Sleep(zduBootRetry + 200*time.Millisecond)
	if zp.liveOf(zduInstNew, false) != 0 {
		t.Fatal("premise: the first round's spares hang")
	}
	zp.setTarget(hi + hi) // a poolctl refresh
	eventually(t, "a second round reaches the new Iran", func() bool { return zp.liveOf(zduInstNew, false) >= 1 })
	if n := zduExitBootRetryN.Load() - r0; n != 1 {
		t.Fatalf("extra rounds %d, want 1", n)
	}
	sendCtl(t, zp.exitPool, zp.meterOf(zduInstNew), hi)
	// (the first round's slots still hang: they count until their dial fails)
	eventually(t, "new pool grows", func() bool { return zp.liveOf(zduInstNew, false) >= hi-zduExitBoot })
	// The new instance is up: no more rounds.
	time.Sleep(zduBootRetry + 200*time.Millisecond)
	zp.setTarget(hi + hi)
	if n := zduExitBootRetryN.Load() - r0; n != 1 {
		t.Fatalf("rounds after the new instance came: %d", n)
	}
}

// b448 (lab r448 op4): the path is cooled less than one adapt2 round before
// the LBSTATE export, so coolStashLocked has not stashed its judge state yet.
// The entry must still carry the cut (from the live judge entry), and the new
// LB, its in-flight dial back early, keeps the inherited cut instead of
// ramping the path 0.10 -> 1.00 as "cooled".
func TestLBSeedCooledJustBeforeExportKeepsCut(t *testing.T) {
	withAuto2(t)
	tOld := time.Unix(7500, 0)
	old := seedTestLB(t, &tOld)
	seedFeed(old, &tOld, 3, 0.003, 0.003, 0.003)
	seedFeed(old, &tOld, 3, 0.003, 0.35, 0.003)
	if q := old.judge.p["k>b"]; q == nil || !q.cut || old.mult[1] > 0.11 {
		t.Fatalf("old judge: path b not cut (factor %.2f)", old.mult[1])
	}
	old.mu.Lock()
	old.links[1]++
	old.probing[1] = true
	old.resultLocked(1, true, nil, errors.New("i/o timeout"))
	cooled := !old.until[1].IsZero()
	_, stashed := old.judgeCool["k>b"]
	old.mu.Unlock()
	if !cooled || stashed {
		t.Fatalf("premise: path b cooled (%v) and not stashed yet (%v)", cooled, stashed)
	}
	// The export right away: no adapt2 round in between.
	tOld = tOld.Add(time.Second)
	wall := time.Now()
	doc := seedDoc(t, old, wall)
	var sp *lbSeedPath
	for i := range doc.L[0].P {
		if doc.L[0].P[i].N == "k>b" {
			sp = &doc.L[0].P[i]
		}
	}
	if sp == nil || sp.Cool <= 0 {
		t.Fatalf("export: cooled path b missing: %+v", doc.L[0].P)
	}
	if !sp.Cut || sp.R == "cooled" || sp.T > 0.11 {
		t.Fatalf("export: path b went out as a bare cooled entry (cut %v reason %q target %.2f): the new LB would ramp it", sp.Cut, sp.R, sp.T)
	}
	// The new LB: its in-flight dial succeeds before the inherited cooldown
	// ends; the path keeps the inherited cut, not judged afresh.
	tNew := tOld.Add(2 * time.Second)
	nw := seedTestLB(t, &tNew)
	nw.applySeed(doc, wall.Add(2*time.Second), false)
	nw.mu.Lock()
	nw.links[1]++
	nw.resultLocked(1, false, &fakeCarrier{}, nil)
	back := nw.until[1].IsZero()
	nw.mu.Unlock()
	if !back {
		t.Fatal("premise: the in-flight dial's success ends the cooldown")
	}
	// The old judge's own clocks: last read when it was cooled, its hold. The
	// path stays cut (and is not reset to "no reading yet" at the seed TTL)
	// until the no-reading probe rule of the old judge would have lifted it.
	seedFeed(nw, &tNew, 1, 0.003, -1, 0.003) // the round that restores it
	sq := nw.judge.p["k>b"]
	if sq == nil || sq.lastRead.IsZero() || sq.hold < lb2Hold {
		t.Fatalf("back early: restored state has no recovery clocks: %+v", sq)
	}
	lift := sq.lastRead.Add(lb2Quiet)
	if c := sq.cutAt.Add(recoverHold(sq)); c.After(lift) {
		lift = c
	}
	for i := 0; tNew.Add(5 * time.Second).Before(lift); i++ {
		seedFeed(nw, &tNew, 1, 0.003, -1, 0.003)
		if q := nw.judge.p["k>b"]; q == nil || !q.cut || nw.mult[1] > 0.11 {
			t.Fatalf("round %d back early (%s before the old judge's probe): path b factor %.2f cut %v (inherited cut lost)",
				i, lift.Sub(tNew), nw.mult[1], q != nil && q.cut)
		}
	}
	// Then it comes back as in the old process: probing, step by step.
	for i := 0; i < 4; i++ {
		seedFeed(nw, &tNew, 1, 0.003, -1, 0.003)
	}
	if q := nw.judge.p["k>b"]; q == nil || q.cut {
		t.Fatal("the inherited cut is never lifted by the no-reading probe rule")
	}
}

// b449: an older sender's cooled entry (no "h", "ra" 0): the restored cut is
// still held at least lb2Hold from the snapshot.
func TestLBSeedCooledOldSenderHold(t *testing.T) {
	withAuto2(t)
	now := time.Unix(8000, 0)
	b := seedTestLB(t, &now)
	seedFeed(b, &now, 1, 0.003, -1, 0.003) // the judge exists (path b unread)
	at := now
	sp := lbSeedPath{N: "k>b", M: 0.1, T: 0.1, R: "cut: loss 35%", E: 0.35, Cut: true, CutAgo: 20000, OkAgo: -1, UncAgo: -1, Cool: 60000, Fails: 3}
	b.mu.Lock()
	ok := b.coolSeedLocked(1, sp, now, at)
	st := b.judgeCool["k>b"]
	b.mu.Unlock()
	if !ok || st == nil {
		t.Fatal("cooled entry not seeded")
	}
	if st.hold < lb2Hold || !st.lastRead.Equal(at) || !st.cut {
		t.Fatalf("old sender: hold %s lastRead %v cut %v", st.hold, st.lastRead, st.cut)
	}
}
