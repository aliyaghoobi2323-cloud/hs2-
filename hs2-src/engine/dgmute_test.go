package engine

import (
	"strings"
	"testing"
	"time"
)

// flowOn finds a flow hash that pickHash places on want.
func flowOn(t *testing.T, p *dgPool, want *dgLink) uint32 {
	t.Helper()
	for f := uint32(1); f < 100000; f++ {
		if p.pickHash(f) == want {
			return f
		}
	}
	t.Fatal("no flow hashes to the carrier")
	return 0
}

// A carrier whose own way through is cut (its echo id dropped) while the
// others still hear the other server: after dgMuteAfter new flows avoid it and
// its flows move at their next packet; at dgSilentDead it is closed — the
// other server told — instead of holding its users for its 15 s timeout.
func TestDgMuteCarrierAvoidedThenClosed(t *testing.T) {
	r := newDgRig(t, false)
	now := time.Now()
	mute, mx, me, _ := r.carrier(now)
	_, _, e2, _ := r.carrier(now)
	_, _, e3, _ := r.carrier(now)
	flow := flowOn(t, r.edge, mute)
	if r.edge.pick(flow, now) != mute {
		t.Fatal("setup: the flow is not on the carrier")
	}

	// 0.8 s of silence is not yet mute.
	me.rx.Store(now.Add(-800 * time.Millisecond).UnixNano())
	r.edge.muteTick(now)
	if mute.muted.Load() || r.edge.pick(flow, now) != mute {
		t.Fatal("muted before dgMuteAfter")
	}

	me.rx.Store(now.Add(-1500 * time.Millisecond).UnixNano())
	r.edge.muteTick(now)
	if !mute.muted.Load() {
		t.Fatal("a carrier silent 1.5 s while two others hear was not marked mute")
	}
	if l := r.edge.pick(flow, now); l == mute || l == nil {
		t.Fatalf("the flow stayed on the mute carrier (%v)", l)
	}
	for f := uint32(1); f < 2000; f++ {
		if r.edge.pickHash(f) == mute {
			t.Fatalf("new flow %d placed on the mute carrier", f)
		}
	}
	if lg := r.log(); !strings.Contains(lg, "has heard nothing from the other server for 1.5s while 2 other carrier(s) still do") {
		t.Fatalf("no mute line:\n%s", lg)
	}
	if !strings.Contains(r.edge.carrierLine(), "M") {
		t.Fatalf("carriers line has no M flag: %s", r.edge.carrierLine())
	}

	// Still silent at 3.2 s: closed, and the other side told.
	me.rx.Store(now.Add(-3200 * time.Millisecond).UnixNano())
	e2.rx.Store(now.UnixNano())
	e3.rx.Store(now.UnixNano())
	r.edge.muteTick(now)
	within(t, 2*time.Second, "the mute carrier is closed on both sides", func() bool {
		return !mute.alive() && !mx.alive()
	})
	if n := r.edge.muteClosed.Load(); n != 1 {
		t.Fatalf("muteClosed = %d, want 1", n)
	}
	if lg := r.log(); !strings.Contains(lg, "heard nothing for 3.2s — closed; a new carrier replaces it") {
		t.Fatalf("no close line:\n%s", lg)
	}
	var ps PoolStats
	r.edge.carrierStats(&ps)
	if ps.MuteClosed != 1 {
		t.Fatalf("PoolStats.MuteClosed = %d, want 1", ps.MuteClosed)
	}
}

// A mute carrier that hears again takes flows again; a carrier that cannot
// say when it last heard (zero) is never judged.
func TestDgMuteCarrierHearsAgain(t *testing.T) {
	r := newDgRig(t, false)
	now := time.Now()
	mute, _, me, _ := r.carrier(now)
	r.carrier(now)
	blind, _, _, _ := r.carrier() // LastRx zero: cannot say
	me.rx.Store(now.Add(-2 * time.Second).UnixNano())
	r.edge.muteTick(now)
	if !mute.muted.Load() || blind.muted.Load() {
		t.Fatalf("muted: silent %v, blind %v — want true, false", mute.muted.Load(), blind.muted.Load())
	}
	me.rx.Store(now.UnixNano())
	r.edge.muteTick(now)
	if mute.muted.Load() {
		t.Fatal("still mute after hearing again")
	}
	if !strings.Contains(r.log(), "hears the other server again") {
		t.Fatalf("no line:\n%s", r.log())
	}
	flow := flowOn(t, r.edge, mute)
	if r.edge.pick(flow, now) != mute {
		t.Fatal("a carrier that hears again took no flows")
	}
}

// When no carrier hears the other server it is the path (or the other
// server), not one carrier: nothing is marked, nothing closed — the scout's
// job — and earlier marks are lifted, so placement is the plain hash.
func TestDgMuteNotJudgedWhenEveryCarrierIsSilent(t *testing.T) {
	r := newDgRig(t, false)
	now := time.Now()
	a, _, ea, _ := r.carrier(now)
	b, _, eb, _ := r.carrier(now)
	ea.rx.Store(now.Add(-1500 * time.Millisecond).UnixNano())
	r.edge.muteTick(now)
	if !a.muted.Load() {
		t.Fatal("setup: a not mute")
	}
	ea.rx.Store(now.Add(-10 * time.Second).UnixNano())
	eb.rx.Store(now.Add(-10 * time.Second).UnixNano())
	r.edge.muteTick(now)
	if a.muted.Load() || b.muted.Load() {
		t.Fatal("marks kept while every carrier is silent")
	}
	time.Sleep(50 * time.Millisecond)
	if !a.alive() || !b.alive() || r.edge.muteClosed.Load() != 0 {
		t.Fatal("a carrier was closed while every carrier was silent")
	}
}

// Placement order: serving over retiring over mute; with only mute carriers
// left a flow still gets one (better than dropping).
func TestDgPickHashTiers(t *testing.T) {
	r := newDgRig(t, false)
	now := time.Now()
	s, _, _, _ := r.carrier(now)
	ret, _, _, _ := r.carrier(now)
	ret.retiring = true
	for f := uint32(1); f < 500; f++ {
		if r.edge.pickHash(f) != s {
			t.Fatal("a flow went to the retiring carrier while a serving one was up")
		}
	}
	s.muted.Store(true)
	for f := uint32(1); f < 500; f++ {
		if r.edge.pickHash(f) != ret {
			t.Fatal("a flow went to the mute carrier while a retiring one hears")
		}
	}
	ret.muted.Store(true)
	for f := uint32(1); f < 500; f++ {
		if r.edge.pickHash(f) != s {
			t.Fatal("with every carrier mute the serving one is the choice")
		}
	}
}

// A tick that comes late (the process or its VM was stopped) is not judged,
// nor are the ticks within the hold after it; on-time ticks are.
func TestStallGate(t *testing.T) {
	var g stallGate
	t0 := time.Unix(1000, 0)
	ev, hold := 250*time.Millisecond, 500*time.Millisecond
	if !g.ok(t0, ev, hold) || !g.ok(t0.Add(ev), ev, hold) {
		t.Fatal("on-time ticks not judged")
	}
	late := t0.Add(ev + 3*time.Second) // stopped for 3 s
	if g.ok(late, ev, hold) {
		t.Fatal("the late tick was judged")
	}
	if g.ok(late.Add(ev), ev, hold) {
		t.Fatal("judged inside the hold")
	}
	if !g.ok(late.Add(2*ev), ev, hold) {
		t.Fatal("not judged after the hold")
	}
}

// A one-way cut (only this direction's packets dropped): the side that hears
// nothing says so, and the other side moves its flows off the carrier at once
// instead of sending into the void until the carrier is closed.
func TestDgMuteToldToTheOtherSide(t *testing.T) {
	r := newDgRig(t, false)
	now := time.Now()
	mute, mx, me, _ := r.carrier(now)
	r.carrier(now)
	r.carrier(now)
	flow := flowOn(t, r.exit, mx)
	if r.exit.pick(flow, time.Now()) != mx {
		t.Fatal("setup: the exit's flow is not on the carrier")
	}
	me.rx.Store(now.Add(-1200 * time.Millisecond).UnixNano())
	r.edge.muteTick(now)
	within(t, 2*time.Second, "the exit learns the carrier is mute", func() bool { return mx.avoid(time.Now()) })
	if mx.muted.Load() {
		t.Fatal("the exit marked it mute itself (it hears the edge fine)")
	}
	if l := r.exit.pick(flow, time.Now()); l == mx || l == nil {
		t.Fatal("the exit's flow stayed on the carrier the edge hears nothing on")
	}
	for f := uint32(1); f < 2000; f++ {
		if r.exit.pickHash(f) == mx {
			t.Fatal("the exit placed a new flow on it")
		}
	}
	if !strings.Contains(r.log(), "the other server hears nothing on it") {
		t.Fatalf("no line:\n%s", r.log())
	}
	// The edge hears again: the exit uses the carrier again.
	me.rx.Store(time.Now().UnixNano())
	r.edge.muteTick(time.Now())
	within(t, 2*time.Second, "the exit learns the carrier hears again", func() bool { return !mx.avoid(time.Now()) })
	if !strings.Contains(r.log(), "the other server hears it again") {
		t.Fatalf("no line:\n%s", r.log())
	}
	_ = mute
}

// A lost "hears again" does not keep the carrier empty: the other server's
// mute word expires after dgPeerMuteFor.
func TestDgPeerMuteExpires(t *testing.T) {
	r := newDgRig(t, false)
	t0 := time.Unix(5000, 0)
	clk := t0
	r.exit.clock = func() time.Time { return clk }
	_, mx, _, _ := r.carrier(time.Now())
	r.exit.onCloseFrame(mx, []byte{closeMute})
	if !mx.avoid(clk) {
		t.Fatal("not avoided after closeMute")
	}
	clk = t0.Add(dgPeerMuteFor - time.Millisecond)
	if !mx.avoid(clk) {
		t.Fatal("expired early")
	}
	clk = t0.Add(dgPeerMuteFor)
	if mx.avoid(clk) {
		t.Fatal("not expired after dgPeerMuteFor")
	}
}

// A carrier that fails (a send error) says so in the log; one closed on
// purpose, by either side, does not.
func TestDgLostCarrierLogged(t *testing.T) {
	r := newDgRig(t, false)
	el, xl, _, _ := r.carrier(time.Now())
	el.lose(errDgFakeClosed)
	within(t, 2*time.Second, "the failure line", func() bool {
		return strings.Contains(r.log(), "failed (context canceled) — its flows move to live carriers")
	})
	_ = xl
	r2 := newDgRig(t, false)
	a, _, _, _ := r2.carrier(time.Now())
	r2.edge.closeLink(a)
	time.Sleep(100 * time.Millisecond)
	if strings.Contains(r2.log(), "failed") {
		t.Fatalf("a carrier closed on purpose was logged as failed:\n%s", r2.log())
	}
}

// A mute carrier is closed once, however many ticks see it before the close
// lands (a slow send of the goodbye).
func TestDgMuteClosedOnce(t *testing.T) {
	r := newDgRig(t, false)
	now := time.Now()
	mute, _, me, _ := r.carrier(now)
	r.carrier(now)
	me.rx.Store(now.Add(-1500 * time.Millisecond).UnixNano())
	r.edge.muteTick(now)
	me.rx.Store(now.Add(-3500 * time.Millisecond).UnixNano())
	r.edge.muteTick(now)
	r.edge.muteTick(now) // before the close goroutine ran, or after
	r.edge.muteTick(now)
	within(t, 2*time.Second, "closed", func() bool { return !mute.alive() })
	if n := r.edge.muteClosed.Load(); n != 1 {
		t.Fatalf("muteClosed = %d, want 1", n)
	}
	if c := strings.Count(r.log(), "— closed; a new carrier replaces it"); c != 1 {
		t.Fatalf("%d close lines:\n%s", c, r.log())
	}
}
