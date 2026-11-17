package engine

import (
	"runtime"
	"testing"
	"time"
)

func st244Reset(rate int64) func() {
	old := st244Rate.Load()
	st244Rate.Store(rate)
	st244Bucket.mu.Lock()
	st244Bucket.tokens, st244Bucket.last = 0, 0
	st244Bucket.mu.Unlock()
	return func() {
		st244Rate.Store(old)
		st244Bucket.mu.Lock()
		st244Bucket.tokens, st244Bucket.last = 0, 0
		st244Bucket.mu.Unlock()
	}
}

func TestST244OffAlwaysPasses(t *testing.T) {
	defer st244Reset(0)()
	now := time.Now()
	for i := 0; i < 10000; i++ {
		if !st244UpgradeToken(now) {
			t.Fatal("key off: an upgrade was deferred")
		}
	}
}

func TestST244BucketPacesStorm(t *testing.T) {
	defer st244Reset(1000)()
	t0 := time.Unix(1000, 0)
	// a storm: 6000 conns ask in the same instant -> only the burst (100) passes
	n := 0
	for i := 0; i < 6000; i++ {
		if st244UpgradeToken(t0) {
			n++
		}
	}
	if n != 100 {
		t.Fatalf("burst passed %d, want 100", n)
	}
	// each conn re-asks once a second: over 6 s about 1000/s pass, never more
	passed := n
	for s := 1; s <= 6; s++ {
		now := t0.Add(time.Duration(s) * time.Second)
		k := 0
		for i := 0; i < 6000-passed; i++ {
			if st244UpgradeToken(now) {
				k++
			}
		}
		if k > 100 { // tokens cap at the burst between asks in the same instant
			t.Fatalf("second %d passed %d > burst", s, k)
		}
		passed += k
	}
	// spread over time (asks every 1 ms) the rate holds: ~1000 in 1 s
	defer st244Reset(1000)()
	k := 0
	for i := 0; i < 1000; i++ {
		if st244UpgradeToken(t0.Add(time.Duration(i) * time.Millisecond)) {
			k++
		}
	}
	if k < 990 || k > 1001 {
		t.Fatalf("1000 asks 1 ms apart at 1000/s: passed %d", k)
	}
}

func TestST244SteadyChurnNeverWaits(t *testing.T) {
	defer st244Reset(1000)()
	t0 := time.Unix(2000, 0)
	// 300 upgrades a second, evenly spread: none deferred
	for i := 0; i < 3000; i++ {
		if !st244UpgradeToken(t0.Add(time.Duration(i) * time.Second / 300)) {
			t.Fatalf("steady churn: ask %d deferred", i)
		}
	}
}

func TestST246InteractiveSkipsPacing(t *testing.T) {
	defer st244Reset(500)()
	old := st246InteractiveBps.Load()
	defer st246InteractiveBps.Store(old)
	st246InteractiveBps.Store(1000)
	if !st246Interactive(4000) { // voice: 32 kbit/s both ways
		t.Fatal("a 4000 B/s two-way flow was not interactive")
	}
	if st246Interactive(200) { // a request/response connection
		t.Fatal("a 200 B/s flow was called interactive")
	}
	if st246Interactive(0) { // a storm connection, quiet after its request
		t.Fatal("a quiet flow was called interactive")
	}
	st246InteractiveBps.Store(0)
	if st246Interactive(1e6) {
		t.Fatal("floor 0 (off) still exempted")
	}
	st246InteractiveBps.Store(1000)
	st244Rate.Store(0)
	if st246Interactive(1e6) {
		t.Fatal("pacing off: the exemption should not count")
	}
}

func TestST246LiteSampleSetsUpLo(t *testing.T) {
	m := &mConn{}
	t0 := time.Now()
	m.liteSample = t0
	m.sendOff = 6000                                        // 3000 B/s up over 2 s
	if !m.liteWantDualLocked(t0.Add(2*time.Second), 2000) { // small: lite upgrades it
		t.Fatal("a small two-way connection did not ask for its upgrade")
	}
	if m.upLo < 999 || m.upLo > 1001 {
		t.Fatalf("upLo %v, want 1000 (the quieter direction, 2000 B over 2 s)", m.upLo)
	}
}

// rel69h: HS2_ST244_UPG_RATE parsing — "auto" scales with the cores (never
// under 500), a number is taken as is, 0 / junk / negative = off.
func TestST244RateFrom(t *testing.T) {
	auto := int64(max(500, 100*runtime.GOMAXPROCS(0)))
	for v, want := range map[string]int64{"auto": auto, " AUTO ": auto, "": 0, "0": 0, "750": 750, "-5": 0, "x": 0} {
		if got := st244RateFrom(v); got != want {
			t.Errorf("st244RateFrom(%q) = %d, want %d", v, got, want)
		}
	}
	old := runtime.GOMAXPROCS(12)
	defer runtime.GOMAXPROCS(old)
	if got := st244RateFrom("auto"); got != 1200 {
		t.Errorf("auto at 12 procs = %d, want 1200", got)
	}
	runtime.GOMAXPROCS(2)
	if got := st244RateFrom("auto"); got != 500 {
		t.Errorf("auto at 2 procs = %d, want 500", got)
	}
}
