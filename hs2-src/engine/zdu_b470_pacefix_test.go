package engine

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// b470: when the close work adds to every sleep (rc14i 24k: ~8 ms of kills
// per 13-user batch at 5 ms steps, ~1000/s), the pace still ends at
// users/rate: close work comes off the next sleep and the batches grow to
// catch up. Key off: the old open-loop pace runs ~1.7x long.
func TestB470PaceKeepsRate(t *testing.T) {
	for _, c := range []struct {
		env      string
		lo, hi   float64 // virtual seconds
		maxBatch int
	}{{"", 4.40, 4.75, 400}, {"0", 7, 1e9, 14}} {
		t.Setenv("HS2_B470_PACEFIX", c.env)
		t.Setenv("HS2_B470_PAR", "1")
		var mu sync.Mutex
		clock := time.Unix(1e9, 0)
		prev := zduPaceNow
		zduPaceNow = func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
		adv := func(d time.Duration) { mu.Lock(); clock = clock.Add(d); mu.Unlock() }
		const n = 12034
		ls := make([]zduPaceLink, n)
		cur, big := 0, 0
		for i := range ls {
			ls[i] = zduPaceLink{users: 1, close: func() { cur++; adv(250 * time.Microsecond) }}
		}
		start := zduPaceNow()
		sleep := func(d time.Duration) {
			big = max(big, cur)
			cur = 0
			adv(d + 100*time.Microsecond) // timer oversleep
		}
		_, users, _ := zduPaceClose(ls, 2667, 5*time.Millisecond, zduPaceMax, sleep)
		big = max(big, cur)
		took := zduPaceNow().Sub(start).Seconds()
		zduPaceNow = prev
		// n/2667 = 4.51 s; 0.25 ms per close (3 s of work) on top of every
		// full sleep runs ~7.5 s open-loop; the fix takes the work off the sleeps.
		if users != n || took < c.lo || took > c.hi {
			t.Fatalf("fix %q: users %d took %.2fs, want [%.2f,%.2f]", c.env, users, took, c.lo, c.hi)
		}
		if c.env == "" && big > c.maxBatch {
			t.Fatalf("fix on: a catch-up batch of %d (> %d): a burst", big, c.maxBatch)
		}
	}
}

// b470: a batch of 8+ entries closes on b470Par goroutines, every entry
// exactly once; smaller batches stay sequential.
func TestB470CloseAllParallel(t *testing.T) {
	t.Setenv("HS2_B470_PAR", "4")
	var n, live, peak atomic.Int32
	mk := func(k int) []zduPaceLink {
		ls := make([]zduPaceLink, k)
		for i := range ls {
			ls[i] = zduPaceLink{users: 1, close: func() {
				v := live.Add(1)
				for {
					p := peak.Load()
					if v <= p || peak.CompareAndSwap(p, v) {
						break
					}
				}
				time.Sleep(2 * time.Millisecond)
				live.Add(-1)
				n.Add(1)
			}}
		}
		return ls
	}
	b470CloseAll(mk(40), true)
	if n.Load() != 40 || peak.Load() < 2 || peak.Load() > 4 {
		t.Fatalf("closed %d peak %d, want 40 on 2..4 goroutines", n.Load(), peak.Load())
	}
	n.Store(0)
	peak.Store(0)
	b470CloseAll(mk(7), true)
	b470CloseAll(mk(40), false)
	if n.Load() != 47 || peak.Load() != 1 {
		t.Fatalf("closed %d peak %d, want 47 sequential", n.Load(), peak.Load())
	}
}

// b470: the user pace's rate fits the lead the drain loop started it with
// (rc14i 24k: 12034 users over a 9.0 s lead from 24068 conns -> 1338/s);
// never above the rate; key off or no lead: the rate.
func TestB470RateFitsLead(t *testing.T) {
	prev := b470Lead.Load()
	defer b470Lead.Store(prev)
	ZDUDrainEndPaceLead(24068)
	if r := b470Rate(12034); r < 1330 || r > 1340 {
		t.Fatalf("rate %d, want ~1337", r)
	}
	if r := b470Rate(30000); r != zduPaceRate {
		t.Fatalf("rate %d, want the cap %d", r, zduPaceRate)
	}
	t.Setenv("HS2_B470_PACEFIX", "0")
	if r := b470Rate(12034); r != zduPaceRate {
		t.Fatalf("key off: rate %d", r)
	}
	t.Setenv("HS2_B470_PACEFIX", "")
	b470Lead.Store(0)
	if r := b470Rate(12034); r != zduPaceRate {
		t.Fatalf("no lead: rate %d", r)
	}
}
