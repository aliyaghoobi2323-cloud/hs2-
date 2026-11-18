package engine

import (
	"context"
	"testing"
	"time"
)

// b339 hysteresis: a cut path's links held while the whole path waits stay
// held when part of it answers again (the backoff stragglers are not judged
// at 8 s), within stuckPathWait of the first stalled sample; a link
// throttled alone on another path is still judged; with the key off the
// stragglers are judged (the ab337 X5 gap).
func TestB339PathHysteresis(t *testing.T) {
	for _, on := range []bool{true, false} {
		oldS, oldH := b337PathStall.Load(), b339PathHyst.Load()
		b337PathStall.Store(true)
		b339PathHyst.Store(on)
		r := newStuckRig(t, 30)
		for _, g := range r.good {
			g.m.path = "10.0.0.2>10.0.1.1"
		}
		var fs []*meteredFakeLink
		var mls []*managedLink
		for i := 0; i < 6; i++ {
			f, ml := r.add()
			f.m.path = "10.0.0.2>10.0.9.1"
			fs, mls = append(fs, f), append(mls, ml)
		}
		lone, loneML := r.add()
		lone.m.path = "10.0.0.2>10.0.1.1"
		r.step(nil, 0)
		// The whole path waits: held by b337 either way.
		for i := 0; i < stuckStreak; i++ {
			for _, f := range fs {
				waiting(f, 8*time.Second)
			}
			r.step(nil, 0)
		}
		for k, ml := range mls {
			if ml.degraded {
				t.Fatalf("on=%v: link %d of a whole-path stall judged", on, k)
			}
		}
		// 4 of 6 answer again; 2 stragglers still wait (RTO backoff).
		for i := 0; i < stuckStreak; i++ {
			for _, f := range fs[:4] {
				prompt(f, 200<<10)
			}
			for _, f := range fs[4:] {
				waiting(f, 8*time.Second+time.Duration(i)*healthTick)
			}
			waiting(lone, 9*time.Second)
			r.step(nil, 0)
		}
		if !loneML.degraded {
			t.Fatalf("on=%v: a link stuck alone on a working path was not judged", on)
		}
		judged := 0
		for _, ml := range mls[4:] {
			if ml.degraded {
				judged++
			}
		}
		if on && judged != 0 {
			t.Fatalf("on: %d stragglers of a recovering cut path judged within %s of its stall", judged, stuckPathWait)
		}
		if !on && judged != 2 {
			t.Fatalf("off: %d of 2 stragglers judged (want the b337 rule: both)", judged)
		}
		if on {
			if b339Stat.hystHeld.Load() == 0 {
				t.Fatal("no hysteresis hold counted")
			}
			// Past stuckPathWait from the first stalled sample, a straggler
			// still waiting is judged.
			for i := 0; i < 8; i++ {
				for _, f := range fs[:4] {
					prompt(f, 200<<10)
				}
				for _, f := range fs[4:] {
					waiting(f, stuckPathWait+time.Duration(i)*healthTick)
				}
				r.step(nil, 0)
			}
			for k, ml := range mls[4:] {
				if !ml.degraded {
					t.Fatalf("straggler %d still waiting past %s not judged", k, stuckPathWait)
				}
			}
		}
		b337PathStall.Store(oldS)
		b339PathHyst.Store(oldH)
	}
}

// The hysteresis state: held for stuckPathWait from the first stalled sample
// even when no longer stalled, not re-armed while the path keeps stalling
// (a path stalled for good is judged), re-armed after a quiet stuckPathWait.
func TestB339HoldPaths(t *testing.T) {
	old := b339PathHyst.Load()
	b339PathHyst.Store(true)
	defer b339PathHyst.Store(old)
	m := &LinkManager{}
	t0 := time.Unix(1000, 0)
	st := map[string]bool{"p": true}
	if h := m.b339HoldPathsLocked(st, t0); !h["p"] {
		t.Fatal("not held at the first stalled sample")
	}
	if h := m.b339HoldPathsLocked(nil, t0.Add(10*time.Second)); !h["p"] {
		t.Fatal("not held 10 s on, the path partly answering")
	}
	if h := m.b339HoldPathsLocked(st, t0.Add(stuckPathWait)); h["p"] {
		t.Fatal("still held by the hysteresis at stuckPathWait while stalled for good")
	}
	if h := m.b339HoldPathsLocked(st, t0.Add(30*time.Second)); h["p"] {
		t.Fatal("re-armed while the path keeps stalling")
	}
	if h := m.b339HoldPathsLocked(nil, t0.Add(30*time.Second+stuckPathWait)); h["p"] || len(m.b339Hold) != 0 {
		t.Fatalf("hold state left after a quiet %s: %v", stuckPathWait, m.b339Hold)
	}
	if h := m.b339HoldPathsLocked(st, t0.Add(100*time.Second)); !h["p"] {
		t.Fatal("not re-armed by a new stall after a quiet spell")
	}
	if h := m.b339HoldPathsLocked(map[string]bool{"q": true}, t0.Add(101*time.Second)); h["q"] != true || h["p"] != true {
		t.Fatalf("two paths: %v", h)
	}
}

// CUTGRACE: a cut-drained link's connections are left alone for cutGrace
// into its path's cut, not when the cut flaps, not with the key off; a cut
// that ends inside the grace is counted short.
func TestB339CutGrace(t *testing.T) {
	old := b339CutGrace.Load()
	defer b339CutGrace.Store(old)
	m := &LinkManager{}
	ml := &managedLink{cutDrain: true, retiring: true}
	t0 := time.Unix(5000, 0)
	cut := map[string]int{"1.1.1.1>2.2.2.2": 2, "1.1.1.1>*": 2}
	ml.cutKey = cutReportKey(cut, "1.1.1.1>2.2.2.2")
	if ml.cutKey != "1.1.1.1>2.2.2.2" || cutReportKey(map[string]int{"1.1.1.1>*": 2}, "1.1.1.1>3.3.3.3") != "1.1.1.1>*" || cutReportKey(cut, "9.9.9.9>2.2.2.2") != "" {
		t.Fatal("cutReportKey")
	}
	b339CutGrace.Store(false)
	m.b339CutTrackLocked(cut, t0)
	if m.b339GraceLocked(ml, t0) {
		t.Fatal("grace with the key off")
	}
	b339CutGrace.Store(true)
	if !m.b339GraceLocked(ml, t0.Add(5*time.Second)) {
		t.Fatal("no grace 5 s into a cut")
	}
	m.b339CutTrackLocked(cut, t0.Add(29*time.Second))
	if !m.b339GraceLocked(ml, t0.Add(29*time.Second)) {
		t.Fatal("no grace 29 s into a cut")
	}
	if m.b339GraceLocked(ml, t0.Add(cutGrace)) {
		t.Fatalf("grace still at %s: a long cut moves its users", cutGrace)
	}
	short0 := b339Stat.graceEnded.Load()
	// A 5 s cut, ended, then cut again 20 s later: a flap, no grace.
	m2 := &LinkManager{}
	m2.b339CutTrackLocked(cut, t0)
	m2.b339CutTrackLocked(nil, t0.Add(5*time.Second))
	if b339Stat.graceEnded.Load() != short0+2 { // both keys of the report ended short
		t.Fatalf("short cuts %d, want %d", b339Stat.graceEnded.Load(), short0+2)
	}
	if m2.b339GraceLocked(ml, t0.Add(6*time.Second)) {
		t.Fatal("grace on a path no longer cut")
	}
	m2.b339CutTrackLocked(cut, t0.Add(25*time.Second))
	if m2.b339GraceLocked(ml, t0.Add(26*time.Second)) {
		t.Fatal("grace on a flapping path")
	}
	// Cut again after the flap window: grace again.
	m2.b339CutTrackLocked(nil, t0.Add(30*time.Second))
	m2.b339CutTrackLocked(nil, t0.Add(30*time.Second+cutFlapWindow))
	m2.b339CutTrackLocked(cut, t0.Add(31*time.Second+cutFlapWindow))
	if !m2.b339GraceLocked(ml, t0.Add(35*time.Second+cutFlapWindow)) {
		t.Fatal("no grace on a new cut after the flap window")
	}
	// Not a cut-drained link: no grace.
	if m2.b339GraceLocked(&managedLink{cutKey: ml.cutKey}, t0.Add(35*time.Second+cutFlapWindow)) {
		t.Fatal("grace on a link not cut-drained")
	}
}

// ab339 60 s cut: a truly dead path must not keep its links. With a fake
// clock, a path's links wait from a cut on and every sample is replayed for
// 60 s: stalled whole the whole time, partly answering (4 of 6 back, 2
// stragglers), and flapping between the two every sample. Every waiting
// link is judged, by its wait reaching stuckPathWait when the path is
// stalled whole and by 2*stuckPathWait at most otherwise.
func TestB339DeadPathJudged(t *testing.T) {
	oldS, oldH := b337PathStall.Load(), b339PathHyst.Load()
	defer func() { b337PathStall.Store(oldS); b339PathHyst.Store(oldH) }()
	b337PathStall.Store(true)
	b339PathHyst.Store(true)
	for _, mode := range []string{"whole", "partial", "flap"} {
		r := newStuckRig(t, 30)
		for _, g := range r.good {
			g.m.path = "10.0.0.2>10.0.1.1"
		}
		var fs []*meteredFakeLink
		var mls []*managedLink
		for i := 0; i < 6; i++ {
			f, ml := r.add()
			f.m.path = "10.0.0.2>10.0.9.1"
			fs, mls = append(fs, f), append(mls, ml)
		}
		r.step(nil, 0)
		judgedAt := map[int]time.Duration{}
		const start = 6 * time.Second // the first samples that see the cut
		for i := 0; ; i++ {
			wait := start + time.Duration(i)*healthTick
			if wait > 60*time.Second {
				break
			}
			whole := mode == "whole" || mode == "flap" && i%2 == 0
			for k, f := range fs {
				if !whole && k < 4 {
					prompt(f, 200<<10)
					continue
				}
				waiting(f, wait)
			}
			r.step(nil, 0)
			for k, ml := range mls {
				if _, ok := judgedAt[k]; !ok && ml.degraded {
					judgedAt[k] = wait
				}
			}
		}
		limit := 2 * stuckPathWait
		if mode == "whole" {
			limit = stuckPathWait
		}
		from := 0
		if mode != "whole" {
			from = 4
		}
		for k := from; k < 6; k++ {
			w, ok := judgedAt[k]
			if !ok && mode == "whole" && len(judgedAt) >= drainHeadroom(r.m.max) {
				continue // at most drainHeadroom stuck at a time (rig max 32: 4); prod max 300: 38
			}
			if !ok {
				t.Fatalf("%s: link %d of a dead path never judged in 60 s (judged %v)\n%s", mode, k, judgedAt, r.lg)
			}
			if w > limit {
				t.Fatalf("%s: link %d judged at wait %s, want <= %s", mode, k, w, limit)
			}
			if w < stuckWait {
				t.Fatalf("%s: link %d judged at wait %s, under stuckWait", mode, k, w)
			}
		}
		t.Logf("%s: judged at waits %v", mode, judgedAt)
	}
}

// CUTGRACE re-mark: links left retiring from a first cut (their mark cleared
// when it ended) are marked cut-drained again when the path is cut again, so
// a long second cut moves their users after the grace; their slots are not
// replaced and they do not come back into service while the path is cut.
// Off: not re-marked (the ab339 60 s cut moved nothing).
func TestB339CutRemark(t *testing.T) {
	for _, on := range []bool{true, false} {
		old := b339CutGrace.Load()
		b339CutGrace.Store(on)
		withCutDrain(t)
		m, clk, _ := newV2Manager(nil, 1, 64, true)
		cut := map[string]int{"i1>k1": 2}
		m.cutPaths = func() map[string]int { return cut }
		var bad []*managedLink
		for i := 0; i < 12; i++ {
			bad = append(bad, cutLink(m, "i1>k1", 20+i))
		}
		for i := 0; i < 12; i++ {
			cutLink(m, "i2>k1", 5)
		}
		ctx := context.Background()
		clk.Advance(time.Second)
		m.reconcile(ctx, 24)
		if S, R := poolCounts(m); S != 14 || R != 10 {
			t.Fatalf("on=%v first cut: %d serving, %d retiring", on, S, R)
		}
		// The cut ends; the pattern shrank meanwhile: the 10 stay retiring, unmarked.
		cut = nil
		clk.Advance(5 * time.Second)
		m.reconcile(ctx, 14)
		m.mu.Lock()
		for _, ml := range bad[2:] {
			if !ml.retiring || ml.cutDrain {
				t.Fatalf("on=%v after the cut: link %d retiring %v cutDrain %v", on, ml.id, ml.retiring, ml.cutDrain)
			}
		}
		m.mu.Unlock()
		// A second, long cut.
		cut = map[string]int{"i1>k1": 2}
		clk.Advance(2 * time.Minute)
		t2 := m.now()
		m.reconcile(ctx, 24)
		m.mu.Lock()
		marked := 0
		for _, ml := range bad[2:] {
			if ml.cutDrain {
				marked++
				if !ml.cutLate || ml.cutKey != "i1>k1" || !ml.retireSince.Equal(t2) {
					t.Fatalf("re-marked link %d: late %v key %q since %v", ml.id, ml.cutLate, ml.cutKey, ml.retireSince)
				}
				if !m.b339GraceLocked(ml, t2.Add(10*time.Second)) || m.b339GraceLocked(ml, t2.Add(cutGrace)) {
					t.Fatalf("re-marked link %d: grace wrong", ml.id)
				}
			}
		}
		rep := m.replacingLocked()
		m.mu.Unlock()
		if on && marked != 10 || !on && marked != 0 {
			t.Fatalf("on=%v: %d of 10 retiring links re-marked", on, marked)
		}
		if rep != 0 {
			t.Fatalf("on=%v: %d replacements asked for links that were not serving", on, rep)
		}
		if on {
			if S, R := poolCounts(m); R != 10 {
				t.Fatalf("re-marked links came back while cut: %d serving, %d retiring", S, R)
			}
		}
		// The cut ends: marks cleared again.
		cut = nil
		m.reconcile(ctx, 14)
		m.mu.Lock()
		for _, ml := range bad {
			if ml.cutDrain || ml.cutLate {
				t.Fatalf("on=%v: link %d still marked after the cut", on, ml.id)
			}
		}
		m.mu.Unlock()
		b339CutGrace.Store(old)
	}
}
