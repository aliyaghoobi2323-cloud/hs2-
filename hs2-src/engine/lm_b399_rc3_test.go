package engine

import (
	"context"
	"testing"
	"time"
)

// rel69r-rc3 (sk401): marks were ~90 % on recovering paths 5-45 s after their
// cut (2 zombie links + a 300 ms read gap on a quiet path), flapping every
// 0.1-1 s, and call subs bunched on the paths the marks left.

// (a) rule 2 counts only recently live links: leftover links of a past cut
// (last read long before the path's last read) are not evidence.
func TestB399ZombieLinksIgnored(t *testing.T) {
	ms := int64(time.Millisecond)
	now := int64(200_000) * ms
	x := 300 * ms
	quiet := now - 400*ms     // the live link's last read: a quiet, healthy path
	zombie := now - 30_000*ms // leftovers of the cut 30 s ago
	zpend := now - 25_000*ms
	fresh := now - 20*ms
	cases := []struct {
		name string
		obs  []b399Obs
		want bool
	}{
		{"sk401: 2 zombies + quiet live link", []b399Obs{{"A", zombie, zpend}, {"A", zombie, zpend}, {"A", quiet, 0}, {"B", fresh, 0}}, false},
		{"3 zombies + 1 live pending link: < 2 live", []b399Obs{{"A", zombie, zpend}, {"A", zombie, zpend}, {"A", zombie, zpend}, {"A", quiet, now - 350*ms}, {"B", fresh, 0}}, false},
		{"2 live pending links + zombies: a real stall", []b399Obs{{"A", zombie, zpend}, {"A", zombie, zpend}, {"A", quiet, now - 350*ms}, {"A", quiet - 500*ms, now - 350*ms}, {"B", fresh, 0}}, true},
		{"live link 1.9 s older than the path's last read counts", []b399Obs{{"A", quiet, now - 350*ms}, {"A", quiet - 1900*ms, now - 350*ms}, {"B", fresh, 0}}, true},
		{"link 2.1 s older does not", []b399Obs{{"A", quiet, now - 350*ms}, {"A", quiet - 2100*ms, now - 350*ms}, {"B", fresh, 0}}, false},
	}
	for _, c := range cases {
		st, _ := b399Judge(c.obs, now, x, nil, 0, nil)
		if _, got := st["A"]; got != c.want {
			t.Fatalf("%s: A stalled=%v, want %v", c.name, got, c.want)
		}
	}
	// a held mark of a long cut keeps: every link went silent together
	long := []b399Obs{{"A", now - 20_000*ms, now - 19_000*ms}, {"A", now - 20_500*ms, now - 19_000*ms}, {"B", fresh, 0}}
	if st, _ := b399Judge(long, now, x, map[string]time.Time{"A": {}}, 0, nil); len(st) != 1 {
		t.Fatalf("long cut lost its mark: %v", st)
	}
}

// (b, v3) re-mark hysteresis: a path that cleared is marked again only after
// the condition held >= b399ReHold (3 s since sk402) without a break.
func TestB399ReMarkHysteresis(t *testing.T) {
	ms := int64(time.Millisecond)
	if b399ReHold.Load() != int64(3*time.Second) {
		t.Fatalf("b399ReHold %v, want 3s (b399 v3)", time.Duration(b399ReHold.Load()))
	}
	h := newB399Hyst()
	cand := func() map[string]b399PathSt { return map[string]b399PathSt{"A": {links: 3, pend: 2, fresh: 2}} }
	t0 := int64(500_000) * ms
	// never cleared: marks at once (cut-detect latency unchanged)
	if st := h.filter(cand(), nil, t0); len(st) != 1 {
		t.Fatalf("first mark delayed: %v", st)
	}
	h.clear("A", t0+2000*ms)
	h.filter(nil, nil, t0+2000*ms)
	tc := t0 + 2100*ms
	for d := int64(0); d < 3000; d += 100 {
		if st := h.filter(cand(), nil, tc+d*ms); len(st) != 0 {
			t.Fatalf("re-marked %d ms after the condition came back", d)
		}
	}
	// a break restarts the hold
	h.filter(nil, nil, tc+3000*ms)
	if st := h.filter(cand(), nil, tc+3100*ms); len(st) != 0 {
		t.Fatal("hold not restarted after a break")
	}
	if st := h.filter(cand(), nil, tc+5600*ms); len(st) != 0 {
		t.Fatal("re-marked 2.5 s after a break")
	}
	if st := h.filter(cand(), nil, tc+6100*ms); len(st) != 1 {
		t.Fatal("not re-marked after 3 s of the full condition")
	}
	// a held mark passes; a clear older than b399ReMem is forgotten
	if st := h.filter(cand(), map[string]time.Time{"A": {}}, tc+6200*ms); len(st) != 1 {
		t.Fatal("held mark dropped")
	}
	h.clear("A", tc+7000*ms)
	h.filter(nil, nil, tc+7000*ms)
	if st := h.filter(cand(), nil, tc+7000*ms+b399ReMem+ms); len(st) != 1 {
		t.Fatal("a clear older than b399ReMem still delays")
	}
}

// (v3 rule 7) a new mark (or rule 6's hold) needs >= 2 pending links that
// read within b399Live of NOW; a running hold or a held mark continues on
// rules 1-5 (a real cut ages past 2 s inside the 3 s hold).
func TestB399FreshOnset(t *testing.T) {
	ms := int64(time.Millisecond)
	now := int64(300_000) * ms
	x := 300 * ms
	fresh := now - 20*ms
	// sk402: a quiet path whose 2 pending links last read 2.5 s ago (the
	// path's own last read, too): rules 1-5 pass, rule 7 does not
	stale := []b399Obs{{"A", now - 2500*ms, now - 2400*ms}, {"A", now - 2500*ms, now - 2400*ms}, {"B", fresh, 0}}
	st, _ := b399Judge(stale, now, x, nil, 0, nil)
	if _, ok := st["A"]; !ok {
		t.Fatal("setup: rules 1-5 should pass")
	}
	if st[("A")].fresh != 0 {
		t.Fatalf("fresh %d, want 0", st["A"].fresh)
	}
	h := newB399Hyst()
	if got := h.filter(st, nil, now); len(got) != 0 {
		t.Fatalf("marked with no fresh link: %v", got)
	}
	// 1.9 s old reads: fresh, marked at once (never cleared)
	ok := []b399Obs{{"A", now - 1900*ms, now - 1800*ms}, {"A", now - 1950*ms, now - 1800*ms}, {"B", fresh, 0}}
	st, _ = b399Judge(ok, now, x, nil, 0, nil)
	if st["A"].fresh != 2 {
		t.Fatalf("fresh %d, want 2", st["A"].fresh)
	}
	if got := newB399Hyst().filter(st, nil, now); len(got) != 1 {
		t.Fatal("fresh onset not marked")
	}
	// a held mark keeps with stale links
	st, _ = b399Judge(stale, now, x, map[string]time.Time{"A": {}}, 0, nil)
	if got := newB399Hyst().filter(st, map[string]time.Time{"A": {}}, now); len(got) != 1 {
		t.Fatal("held mark dropped by rule 7")
	}
	// a recut of a cleared path: onset fresh, the hold runs on while the
	// links age past 2 s, marked at 3 s
	h = newB399Hyst()
	h.clear("A", now-5000*ms)
	t0 := now
	for d := int64(0); d <= 3000; d += 100 {
		at := t0 + d*ms
		obs := []b399Obs{{"A", t0 - 400*ms, t0 - 350*ms}, {"A", t0 - 450*ms, t0 - 350*ms}, {"B", at - 20*ms, 0}}
		st, _ := b399Judge(obs, at, x, nil, 0, nil)
		got := h.filter(st, nil, at)
		if d < 3000 && len(got) != 0 {
			t.Fatalf("recut marked at +%d ms (hold 3 s)", d)
		}
		if d == 3000 && len(got) != 1 {
			t.Fatal("recut not marked after the 3 s hold")
		}
	}
	// a recovering path (sk402): links silent 1.4-2 s, then a read: never marked
	h = newB399Hyst()
	h.clear("A", now-20_000*ms)
	for d := int64(0); d <= 4000; d += 100 {
		at := now + d*ms
		rd := at - (d%2000)*ms - 300*ms // reads every ~2 s
		obs := []b399Obs{{"A", rd, rd + 50*ms}, {"A", rd, rd + 50*ms}, {"B", at - 20*ms, 0}}
		st, _ := b399Judge(obs, at, x, nil, 0, nil)
		if got := h.filter(st, nil, at); len(got) != 0 {
			t.Fatalf("recovering path marked at +%d ms", d)
		}
	}
}

// (b) in the loop: a path that clears and stalls again stays unmarked for
// ~3 s (sk401's 0.1-1 s flaps no longer re-mark).
func TestB399LoopNoFlap(t *testing.T) {
	setB399(t, true)
	m, ls := b399Pool(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.b399Loop(ctx)
	b399Wait(t, m, 4)
	now := ctrlNow()
	for _, l := range ls[:4] {
		l.mtr.lastRd.Store(now) // a read on the path: clear
	}
	b399Wait(t, m, 0)
	now = ctrlNow()
	for _, l := range ls[:4] { // stalled again 300 ms after its last read
		l.mtr.lastRd.Store(now - int64(400*time.Millisecond))
		l.mtr.pendSince.Store(now - int64(350*time.Millisecond))
	}
	start := time.Now()
	for time.Since(start) < 2600*time.Millisecond {
		m.mu.RLock()
		for _, ml := range m.links[:4] {
			if ml.b399Stall.Load() {
				m.mu.RUnlock()
				t.Fatalf("re-marked %v after the clear (flap)", time.Since(start))
			}
		}
		m.mu.RUnlock()
		time.Sleep(20 * time.Millisecond)
	}
	b399Wait(t, m, 4) // held >= 3 s: marked again
}

// (v3) part (c) reverted: with path A marked, call subs (call avoid/upgrade,
// call rescue, callmove to another path, call stuck/late) follow the stall
// rank like user opens. sk402: a call starting at a cut onset put its second
// sub on the freshly cut path with (c) (16-22 s stalls).
func TestB399CallSubsFollowStall(t *testing.T) {
	setB399(t, true)
	for _, fast := range []bool{false, true} {
		setFastPick(t, fast)
		m, ls := b399Pool(t)
		for _, ml := range m.links[:4] {
			ml.b399Stall.Store(true) // path A marked
		}
		count := func(avoid func() []any) (onA int) {
			var rels []func()
			for i := 0; i < 40; i++ {
				l, rel, ok := m.pickPlaneCall(planeLight, avoid())
				if !ok {
					t.Fatal("no pick")
				}
				rels = append(rels, rel)
				if l == Link(ls[0]) || l == Link(ls[1]) || l == Link(ls[2]) || l == Link(ls[3]) {
					onA++
				}
			}
			for _, r := range rels {
				r()
			}
			return onA
		}
		for _, mk := range []any{nil, callRescueMark{}, callAvoidMark{}, callStuckMark{path: "zz"}} {
			mk := mk
			if n := count(func() []any {
				if mk == nil {
					return nil
				}
				return []any{mk}
			}); n != 0 {
				t.Fatalf("fast=%v %T: picks on the marked path %d/40, want 0 (calls follow the stall rank)", fast, mk, n)
			}
		}
	}
}
