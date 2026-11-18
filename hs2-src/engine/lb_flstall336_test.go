package engine

import (
	"sync"
	"testing"
	"time"
)

// fl336Stalls: n stall notes on path k, connections from c0, one link each.
func fl336Stalls(now time.Time, k string, c0 uint64, n int) []flStallEv {
	var ev []flStallEv
	for i := 0; i < n; i++ {
		ev = append(ev, flStallEv{at: now, conn: c0 + uint64(i), link: new(int), path: k})
	}
	return ev
}

// fl336Win: every path at base loss with segs segments, the bad IP's paths at loss.
func fl336Win(alive map[string]bool, base float64, bad string, loss float64, segs uint64) map[string]flRead {
	c := map[string]flRead{}
	for k := range alive {
		l, n := base, uint64(1000)
		if ir, _ := pathIPs(k); ir == bad {
			l, n = loss, segs
		}
		c[k] = flRead{rt: uint64(l * float64(n)), segs: n}
	}
	return c
}

const fl336A, fl336B = "10.60.3.1>10.70.1.2", "10.60.3.1>10.70.2.2"

func TestFlStall336LowersAtOnce(t *testing.T) {
	a := fl335Paths()
	d := newFastLowerDet(t.Logf)
	now := time.Unix(1e9, 0)
	d.stepStall(now, fl336Win(a, 0.003, "", 0, 0), a, nil)
	now = now.Add(2 * time.Second)
	// first window of the burst: a small sample (40 segs) at 10 % — too
	// small for the resent-only rule (flMinSegs), and one window anyway
	st := append(fl336Stalls(now, fl336A, 100, 3), fl336Stalls(now, fl336B, 200, 2)...)
	d.stepStall(now, fl336Win(a, 0.003, "10.60.3.1", 0.10, 40), a, st)
	if !d.lowered(fl336A) || !d.lowered(fl336B) {
		t.Fatal("stall-hot IP not lowered at its first window")
	}
	for _, k := range []string{"10.60.2.1>10.70.1.2", "10.60.4.1>10.70.2.2", "10.60.1.1>10.70.1.2"} {
		if d.lowered(k) {
			t.Fatalf("%s lowered (another Iran IP or a Kharej IP)", k)
		}
	}
	if flStallLowers.Load() == 0 {
		t.Fatal("stall lower not counted")
	}
}

func TestFlStall336NeedsSeveral(t *testing.T) {
	a := fl335Paths()
	now := time.Unix(1e9, 0)
	w := fl336Win(a, 0.003, "10.60.3.1", 0.10, 40)
	cases := map[string][]flStallEv{
		// one user: many notes, one connection, both links
		"one connection": {{now, 7, new(int), fl336A}, {now, 7, new(int), fl336B}, {now, 7, new(int), fl336A}},
		// two users only
		"two connections": append(fl336Stalls(now, fl336A, 1, 1), fl336Stalls(now, fl336B, 2, 1)...),
		// three users, one link
		"one link": func() []flStallEv {
			l := new(int)
			return []flStallEv{{now, 1, l, fl336A}, {now, 2, l, fl336A}, {now, 3, l, fl336A}}
		}(),
		// several users, links on one path only
		"one path": fl336Stalls(now, fl336A, 1, 5),
	}
	for name, st := range cases {
		d := newFastLowerDet(t.Logf)
		d.stepStall(now, w, a, st)
		if d.lowered(fl336A) {
			t.Fatalf("%s: lowered", name)
		}
	}
}

func TestFlStall336NeedsCorroboration(t *testing.T) {
	a := fl335Paths()
	now := time.Unix(1e9, 0)
	st := append(fl336Stalls(now, fl336A, 1, 4), fl336Stalls(now, fl336B, 10, 4)...)
	for name, w := range map[string]map[string]flRead{
		"no resends":       fl336Win(a, 0.003, "10.60.3.1", 0.003, 1000),
		"tiny sample":      fl336Win(a, 0.003, "10.60.3.1", 0.5, 10),
		"one path of four": fl336Win(a, 0.003, "", 0, 0),
		"every IP lossy":   fl336Win(a, 0.08, "10.60.3.1", 0.10, 1000),
	} {
		if name == "one path of four" {
			w[fl336A] = flRead{rt: 10, segs: 100}
			w[fl336B] = flRead{rt: 0, segs: 100}
			w["10.60.3.1>10.70.3.2"] = flRead{rt: 0, segs: 100}
			w["10.60.3.1>10.70.4.2"] = flRead{rt: 0, segs: 100}
			a2 := map[string]bool{}
			for k := range a {
				a2[k] = true
			}
			a2["10.60.3.1>10.70.3.2"], a2["10.60.3.1>10.70.4.2"] = true, true
			d := newFastLowerDet(t.Logf)
			d.stepStall(now, w, a2, st)
			if d.lowered(fl336A) {
				t.Fatalf("%s: lowered", name)
			}
			continue
		}
		d := newFastLowerDet(t.Logf)
		d.stepStall(now, w, a, st)
		if d.lowered(fl336A) {
			t.Fatalf("%s: lowered", name)
		}
	}
}

// Stalls everywhere (an Iran-wide hiccup, or one busy Kharej IP): not one IP's.
func TestFlStall336Spread(t *testing.T) {
	a := fl335Paths()
	now := time.Unix(1e9, 0)
	var st []flStallEv
	c := uint64(1)
	for k := range a {
		st = append(st, fl336Stalls(now, k, c, 2)...)
		c += 10
	}
	d := newFastLowerDet(t.Logf)
	d.stepStall(now, fl336Win(a, 0.003, "10.60.3.1", 0.10, 200), a, st)
	if d.lowered(fl336A) {
		t.Fatal("lowered on stalls spread over every IP")
	}
	// Kharej IP 10.70.1.2: stalls on its paths to three Iran IPs, its
	// path to 10.60.3.1 lossy — the Iran IP may go, the Kharej IP not
	st = nil
	for i, ir := range []string{"10.60.3.1", "10.60.2.1", "10.60.5.1"} {
		st = append(st, fl336Stalls(now, ir+">10.70.1.2", uint64(100*i+1), 3)...)
	}
	d = newFastLowerDet(t.Logf)
	w := fl336Win(a, 0.003, "", 0, 0)
	w[fl336A] = flRead{rt: 100, segs: 1000}
	d.stepStall(now, w, a, st)
	if d.lowered("10.60.1.1>10.70.1.2") {
		t.Fatal("Kharej IP lowered on one lossy path")
	}
}

func TestFlStall336FlapGuard(t *testing.T) {
	a := fl335Paths()
	d := newFastLowerDet(t.Logf)
	now := time.Unix(1e9, 0)
	hot := func() {
		now = now.Add(2 * time.Second)
		st := append(fl336Stalls(now, fl336A, 1, 3), fl336Stalls(now, fl336B, 10, 3)...)
		d.stepStall(now, fl336Win(a, 0.003, "10.60.3.1", 0.10, 200), a, st)
	}
	calm := func(n int) {
		for i := 0; i < n; i++ {
			now = now.Add(2 * time.Second)
			d.stepStall(now, fl336Win(a, 0.003, "", 0, 0), a, nil)
		}
	}
	for r := 0; r < 2; r++ {
		hot()
		if !d.lowered(fl336A) {
			t.Fatalf("lower %d: not lowered at once", r+1)
		}
		calm(6)
		if d.lowered(fl336A) {
			t.Fatalf("lower %d: not lifted", r+1)
		}
	}
	// third time within lb2FlapWin: flNeedFlap windows
	for i := 1; i <= flNeedFlap; i++ {
		hot()
		if got := d.lowered(fl336A); got != (i == flNeedFlap) {
			t.Fatalf("flap guard: window %d lowered=%v", i, got)
		}
	}
}

func TestFlStall336Registry(t *testing.T) {
	old, oldOn := b335FastLower, flStallOn
	b335FastLower, flStallOn = true, true
	defer func() { b335FastLower, flStallOn = old, oldOn; flStallTake(time.Now()) }()
	flStallTake(time.Now())
	now := time.Now()
	flStallNote(flStallEv{at: now.Add(-10 * time.Second), conn: 1, path: fl336A})
	flStallNote(flStallEv{at: now, conn: 2, path: fl336A})
	flStallNote(flStallEv{at: now, conn: 3, path: ""})
	if ev := flStallTake(now); len(ev) != 1 || ev[0].conn != 2 {
		t.Fatalf("take: %+v", ev)
	}
	if ev := flStallTake(now); len(ev) != 0 {
		t.Fatalf("second take not empty: %+v", ev)
	}
	b335FastLower = false
	flStallNote(flStallEv{at: now, conn: 4, path: fl336A})
	if ev := flStallTake(now); len(ev) != 0 {
		t.Fatal("noted with the key off")
	}
	b335FastLower = true
	// concurrent notes and takes (-race), and the cap
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 1000; i++ {
				flStallNote(flStallEv{at: time.Now(), conn: uint64(g*1000 + i), path: fl336A})
			}
		}(g)
	}
	n := 0
	for i := 0; i < 50; i++ {
		n += len(flStallTake(time.Now()))
	}
	wg.Wait()
	n += len(flStallTake(time.Now()))
	if int64(n)+flStallDrop.Load() < 8000 {
		t.Fatalf("notes lost: %d taken, %d dropped", n, flStallDrop.Load())
	}
}

// The hook: a stalled call connection notes its primary link's path.
func TestFlStall336NoteHook(t *testing.T) {
	old, oldOn := b335FastLower, flStallOn
	b335FastLower, flStallOn = true, true
	defer func() { b335FastLower, flStallOn = old, oldOn; flStallTake(time.Now()) }()
	flStallTake(time.Now())
	m := &mConn{id: 42}
	p := &msub{}
	p.dc.path = fl336A
	q := &msub{}
	q.dc.path = fl336B
	now := time.Now()
	m.flStallNoteLocked(p, []*msub{q, p}, now)
	m.flStallNoteLocked(nil, []*msub{q, p}, now)
	m.flStallNoteLocked(nil, nil, now)
	ev := flStallTake(now)
	if len(ev) != 2 || ev[0].path != fl336A || ev[1].path != fl336B || ev[0].conn != 42 {
		t.Fatalf("notes: %+v", ev)
	}
}

// Links in RTO backoff (the edge tick's TCP_INFO, conn 0) count like stalled
// connections: three of them on the IP's two paths, concentrated, corroborated.
func TestFlStall336RTOLinks(t *testing.T) {
	a := fl335Paths()
	now := time.Unix(1e9, 0)
	rto := func(k string, n int) []flStallEv {
		var ev []flStallEv
		for i := 0; i < n; i++ {
			ev = append(ev, flStallEv{link: new(int), path: k})
		}
		return ev
	}
	w := fl336Win(a, 0.003, "10.60.3.1", 0.025, 100)
	d := newFastLowerDet(t.Logf)
	d.stepStall(now, w, a, append(rto(fl336A, 2), rto(fl336B, 1)...))
	if !d.lowered(fl336A) {
		t.Fatal("3 RTO links on the IP's 2 paths, resends 2.5 %: not lowered")
	}
	d = newFastLowerDet(t.Logf)
	d.stepStall(now, w, a, append(rto(fl336A, 1), rto(fl336B, 1)...))
	if d.lowered(fl336A) {
		t.Fatal("lowered on 2 RTO links")
	}
	// quiet noise: RTO links on every IP, 0.4 % resends everywhere
	var ev []flStallEv
	for k := range a {
		ev = append(ev, rto(k, 1)...)
	}
	d = newFastLowerDet(t.Logf)
	d.stepStall(now, fl336Win(a, 0.004, "", 0, 0), a, ev)
	for k := range a {
		if d.lowered(k) {
			t.Fatalf("quiet: %s lowered", k)
		}
	}
}

// A lowered IP that still resends over the bar on a small sample is not
// calm: it is not lifted while the burst lasts.
func TestFlStall336NoLiftMidBurst(t *testing.T) {
	a := fl335Paths()
	d := newFastLowerDet(t.Logf)
	now := time.Unix(1e9, 0)
	st := append(fl336Stalls(now, fl336A, 1, 3), fl336Stalls(now, fl336B, 10, 3)...)
	d.stepStall(now, fl336Win(a, 0.003, "10.60.3.1", 0.10, 200), a, st)
	if !d.lowered(fl336A) {
		t.Fatal("not lowered")
	}
	for i := 0; i < 10; i++ { // 20 s of a still-lossy, lightly used IP
		now = now.Add(2 * time.Second)
		d.stepStall(now, fl336Win(a, 0.003, "10.60.3.1", 0.10, 40), a, nil)
	}
	if !d.lowered(fl336A) {
		t.Fatal("lifted mid-burst on small samples over the bar")
	}
	for i := 0; i < 6; i++ {
		now = now.Add(2 * time.Second)
		d.stepStall(now, fl336Win(a, 0.003, "10.60.3.1", 0, 40), a, nil)
	}
	if d.lowered(fl336A) {
		t.Fatal("not lifted after the burst")
	}
}
