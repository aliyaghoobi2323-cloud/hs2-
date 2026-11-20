package engine

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"
)

func setB399(t *testing.T, on bool) {
	t.Helper()
	o, st := b399On.Load(), b399Settle.Load()
	b399On.Store(on)
	b399Settle.Store(0) // loop tests: pool and paths count as settled at once
	t.Cleanup(func() { b399On.Store(o); b399Settle.Store(st) })
}

func TestB399Defaults(t *testing.T) {
	if _, set := os.LookupEnv("HS2_B399_STALLPICK"); set {
		t.Skip("HS2_B399_STALLPICK set in the environment")
	}
	if envDefault("HS2_B399_STALL_MS") == "" && b399X.Load() != int64(300*time.Millisecond) {
		t.Fatalf("default X %v, want 300ms", time.Duration(b399X.Load()))
	}
	if !b399On.Load() { // the table value ("1") is envdef_test's
		t.Fatal("HS2_B399_STALLPICK default must be on (rel69r-rc5)")
	}
}

// Rule 5 (stricter start-up gate): sk399b P2 still marked 3 paths at el
// -0.4..2.0 s, each with a handshake read < 10 s old (rule 4 passed). A new
// mark now needs the pool AND the path up >= 10 s; a mark already held keeps.
func TestB399StartupGateP2(t *testing.T) {
	ms := int64(time.Millisecond)
	x := 300 * ms
	pool := int64(50_000) * ms // pool (loop) start
	for _, el := range []int64{-400, 0, 500, 1000, 2000} {
		now := pool + 1000*ms + el*ms // el from the harness start, ~1 s after the pool
		hs := pool + 100*ms           // handshake reads right after start
		pend := now - x - 50*ms
		obs := []b399Obs{{"A", hs, pend}, {"A", hs, pend}, {"A", hs, 0}, {"B", now - 20*ms, 0}}
		pathUp := map[string]int64{"A": pool, "B": pool}
		if now-hs < x {
			continue
		}
		if st, _ := b399Judge(obs, now, x, nil, 0, nil); len(st) != 1 {
			t.Fatalf("el %d ms: rules 1-4 alone should mark (the P2 case): %v", el, st)
		}
		if st, _ := b399Judge(obs, now, x, nil, pool, nil); len(st) != 0 {
			t.Fatalf("el %d ms: pool-age gate missed: %v", el, st)
		}
		if st, _ := b399Judge(obs, now, x, nil, pool, pathUp); len(st) != 0 {
			t.Fatalf("el %d ms: start-up mark: %v", el, st)
		}
	}
	// settled pool, freshly (re)opened path: no new mark for 10 s
	now := pool + 60_000*ms
	obs := []b399Obs{{"A", now - 900*ms, now - 800*ms}, {"A", now - 900*ms, now - 800*ms}, {"B", now - 20*ms, 0}}
	if st, _ := b399Judge(obs, now, x, nil, pool, map[string]int64{"A": now - 9_000*ms, "B": pool}); len(st) != 0 {
		t.Fatalf("young path marked: %v", st)
	}
	if st, _ := b399Judge(obs, now, x, nil, pool, map[string]int64{"A": now - 10_000*ms, "B": pool}); len(st) != 1 {
		t.Fatalf("settled path not marked: %v", st)
	}
	// an existing mark is kept even if the path counter was reset
	if st, _ := b399Judge(obs, now, x, map[string]time.Time{"A": {}}, now-ms, map[string]int64{"A": now - ms}); len(st) != 1 {
		t.Fatalf("held mark dropped: %v", st)
	}
	// the loop: a fresh pool marks nothing while the gate is closed
	setB399(t, true)
	b399Settle.Store(int64(10 * time.Second))
	m, _ := b399Pool(t)
	ctx, cancel := context.WithCancel(context.Background())
	go m.b399Loop(ctx)
	time.Sleep(400 * time.Millisecond)
	cancel()
	for _, ml := range m.links {
		if ml.b399Stall.Load() {
			t.Fatalf("link %d marked during pool start-up", ml.id)
		}
	}
}

func TestB399Judge(t *testing.T) {
	ms := int64(time.Millisecond)
	now := int64(100_000) * ms
	x := 500 * ms
	fresh := now - 50*ms
	old := now - 2000*ms
	pend := now - 900*ms // unanswered for 900 ms
	cases := []struct {
		name    string
		obs     []b399Obs
		want    []string
		wantHld bool
	}{
		{"cut path, 2 pending links", []b399Obs{{"A", old, pend}, {"A", old, pend}, {"A", old, 0}, {"B", fresh, 0}}, []string{"A"}, false},
		{"one pending link is not evidence (b383)", []b399Obs{{"A", old, pend}, {"A", old, 0}, {"B", fresh, 0}}, nil, false},
		{"single-link path never", []b399Obs{{"A", old, pend}, {"B", fresh, 0}}, nil, false},
		{"one read on the path clears it", []b399Obs{{"A", old, pend}, {"A", old, pend}, {"A", fresh, 0}, {"B", fresh, 0}}, nil, false},
		{"pending too young", []b399Obs{{"A", old, now - 200*ms}, {"A", old, now - 200*ms}, {"B", fresh, 0}}, nil, false},
		{"answered writes (pend before read)", []b399Obs{{"A", old, old - ms}, {"A", old, old - ms}, {"B", fresh, 0}}, nil, false},
		{"every path stalled: fall back", []b399Obs{{"A", old, pend}, {"A", old, pend}, {"B", old, pend}, {"B", old, pend}}, nil, true},
		{"stamps off (rescue key off): inert", []b399Obs{{"A", 0, 0}, {"A", 0, 0}, {"B", 0, 0}}, nil, false},
		{"two paths stalled, one healthy", []b399Obs{{"A", old, pend}, {"A", old, pend}, {"C", old, pend}, {"C", old, pend}, {"B", fresh, 0}}, []string{"A", "C"}, false},
	}
	for _, c := range cases {
		st, held := b399Judge(c.obs, now, x, nil, 0, nil)
		if held != c.wantHld || len(st) != len(c.want) {
			t.Fatalf("%s: got %v held=%v, want %v held=%v", c.name, st, held, c.want, c.wantHld)
		}
		for _, p := range c.want {
			if _, ok := st[p]; !ok {
				t.Fatalf("%s: %s not stalled: %v", c.name, p, st)
			}
		}
	}
}

// Rule 4 (start-up guard): a path with no read in the last 10 s (or never)
// is not newly marked; one already marked keeps its mark past 10 s.
func TestB399StartupGuard(t *testing.T) {
	ms := int64(time.Millisecond)
	now := int64(100_000) * ms
	x := 500 * ms
	fresh := now - 50*ms
	pend := now - 900*ms
	stale := now - 11_000*ms
	recent := now - 9_000*ms
	cases := []struct {
		name string
		obs  []b399Obs
		prev map[string]time.Time
		want bool
	}{
		{"never read (start-up)", []b399Obs{{"A", 0, pend}, {"A", 0, pend}, {"B", fresh, 0}}, nil, false},
		{"last read 11 s ago", []b399Obs{{"A", stale, pend}, {"A", stale, pend}, {"B", fresh, 0}}, nil, false},
		{"last read 9 s ago", []b399Obs{{"A", recent, pend}, {"A", recent, pend}, {"B", fresh, 0}}, nil, true},
		{"already marked, 11 s silent: kept", []b399Obs{{"A", stale, pend}, {"A", stale, pend}, {"B", fresh, 0}}, map[string]time.Time{"A": {}}, true},
		{"already marked, never read: kept", []b399Obs{{"A", 0, pend}, {"A", 0, pend}, {"B", fresh, 0}}, map[string]time.Time{"A": {}}, true},
		{"marked but now answered: cleared", []b399Obs{{"A", fresh, 0}, {"A", stale, pend}, {"B", fresh, 0}}, map[string]time.Time{"A": {}}, false},
	}
	for _, c := range cases {
		st, _ := b399Judge(c.obs, now, x, c.prev, 0, nil)
		if _, got := st["A"]; got != c.want {
			t.Fatalf("%s: A stalled=%v, want %v (%v)", c.name, got, c.want, st)
		}
	}
}

// b399Pool: 2 paths x 4 links, path A silent with unanswered writes.
func b399Pool(t *testing.T) (*LinkManager, []*mcLink) {
	t.Helper()
	ls := make([]*mcLink, 8)
	for i := range ls {
		ls[i] = newMCLink(capPortTags | capMConn)
		ls[i].mtr.path = "10.0.0.1>10.1.0.1" // A
		if i >= 4 {
			ls[i].mtr.path = "10.0.0.2>10.1.0.1" // B
		}
	}
	m := mcManager(ls...)
	m.perLink = 1000
	now := ctrlNow()
	for i, l := range ls {
		if i < 4 {
			// v3 rule 7: a fresh onset (reads within 2 s of now); the mark then holds
			l.mtr.lastRd.Store(now - int64(time.Second))
			l.mtr.pendSince.Store(now - int64(900*time.Millisecond))
		} else {
			l.mtr.lastRd.Store(now + int64(time.Hour)) // stays fresh for the whole test
		}
	}
	return m, ls
}

func b399Wait(t *testing.T, m *LinkManager, want int) {
	t.Helper()
	for i := 0; i < 100; i++ {
		n := 0
		m.mu.RLock()
		for _, ml := range m.links {
			if ml.b399Stall.Load() {
				n++
			}
		}
		m.mu.RUnlock()
		if n == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("stalled links never reached %d", want)
}

func TestB399PickAvoidsStalledPath(t *testing.T) {
	setB399(t, true)
	m, _ := b399Pool(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.b399Loop(ctx)
	b399Wait(t, m, 4)
	for i := 0; i < 40; i++ {
		_, rel, ok := m.Pick()
		if !ok {
			t.Fatal("no pick")
		}
		defer rel()
	}
	m.mu.RLock()
	for _, ml := range m.links {
		if mlPath(ml) == "10.0.0.1>10.1.0.1" && ml.users.Load() != 0 {
			t.Fatalf("stalled-path link %d got %d users", ml.id, ml.users.Load())
		}
	}
	m.mu.RUnlock()
}

func TestB399KeyOffInert(t *testing.T) {
	setB399(t, false)
	m, _ := b399Pool(t)
	for _, ml := range m.links {
		ml.b399Stall.Store(true) // even a stale mark is ignored with the key off
	}
	seen := map[string]bool{}
	for i := 0; i < 40; i++ {
		ml := func() *managedLink {
			m.mu.Lock()
			defer m.mu.Unlock()
			x := m.pickLocked(0)
			m.placeLocked(x)
			return x
		}()
		seen[mlPath(ml)] = true
	}
	if !seen["10.0.0.1>10.1.0.1"] || !seen["10.0.0.2>10.1.0.1"] {
		t.Fatalf("key off: want both paths used, got %v", seen)
	}
}

func TestB399AllStalledFallsBack(t *testing.T) {
	setB399(t, true)
	m, ls := b399Pool(t)
	now := ctrlNow()
	for _, l := range ls[4:] {
		l.mtr.lastRd.Store(now - int64(3*time.Second))
		l.mtr.pendSince.Store(now - int64(2*time.Second))
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h0 := b399Stat.held.Load()
	go m.b399Loop(ctx)
	time.Sleep(350 * time.Millisecond)
	b399Wait(t, m, 0)
	if b399Stat.held.Load() == h0 {
		t.Fatal("no held tick")
	}
	if _, rel, ok := m.Pick(); !ok {
		t.Fatal("no pick")
	} else {
		rel()
	}
}

// -race: the loop marks/unmarks while many goroutines pick (fast path too).
func TestB399ConcurrentRace(t *testing.T) {
	setB399(t, true)
	setFastPick(t, true)
	m, ls := b399Pool(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.b399Loop(ctx)
	stop := make(chan struct{})
	var tg sync.WaitGroup
	tg.Add(1)
	go func() { // flip path A between silent and answering
		defer tg.Done()
		for k := 0; ; k++ {
			select {
			case <-stop:
				return
			case <-time.After(30 * time.Millisecond):
			}
			now := ctrlNow()
			for _, l := range ls[:4] {
				if k%4 == 0 {
					l.mtr.lastRd.Store(now)
				} else {
					l.mtr.pendSince.Store(now - int64(2*time.Second))
				}
			}
		}
	}()
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 300; i++ {
				_, rel, ok, _ := m.pickHeld()
				if !ok {
					t.Error("no pick")
					return
				}
				rel()
			}
		}()
	}
	wg.Wait()
	close(stop)
	tg.Wait()
	if m.users.Load() != 0 {
		t.Fatalf("pool users %d", m.users.Load())
	}
}
