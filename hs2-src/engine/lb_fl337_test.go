package engine

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// fl337Paths: 2 Kharej IPs × 3 Iran IPs, the dialing side's names (kharej>iran).
func fl337Paths() []LBPath {
	var ps []LBPath
	for _, k := range []string{"10.70.1.2", "10.70.2.2"} {
		for _, i := range []string{"10.60.1.1", "10.60.2.1", "10.60.3.1"} {
			ps = append(ps, LBPath{Local: k, Remote: i, Addr: i + ":1", Weight: 10, Primary: len(ps) == 0})
		}
	}
	return ps
}

// fl337LB: an auto2 dialing-side LB at fake time.
func fl337LB(t *testing.T, logs *[]string) (*LB, *time.Time) {
	withAuto2(t)
	b, err := NewLB(fl337Paths(), nil)
	if err != nil {
		t.Fatal(err)
	}
	b.SetAuto(true)
	now := time.Unix(1000, 0)
	b.now = func() time.Time { return now }
	var mu sync.Mutex
	b.log = func(f string, a ...any) {
		mu.Lock()
		*logs = append(*logs, fmt.Sprintf(f, a...))
		mu.Unlock()
	}
	return b, &now
}

// fl337Step: one 5 s reading. All paths read base loss (2000 segs); the bad
// IP's paths carry a small burst sample (60 segs at bad: no judge reading)
// and rto links in RTO backoff each.
func fl337Step(b *LB, now *time.Time, base float64, badIP string, bad float64, rto int) {
	*now = now.Add(5 * time.Second)
	rs := map[int]q2Reading{}
	b.mu.Lock()
	b.flCnt = map[string]flRead{}
	b.flRTO = nil
	for j, p := range b.paths {
		k := p.Name()
		if p.Remote == badIP {
			b.flCnt[k] = flRead{rt: uint64(bad * 60), segs: 60}
			for i := 0; i < rto; i++ {
				b.flRTO = append(b.flRTO, flStallEv{link: new(int), path: k})
			}
			continue
		}
		b.flCnt[k] = flRead{rt: uint64(base * 2000), segs: 2000}
		rs[j] = q2Reading{loss: base, segs: 2000}
	}
	b.mu.Unlock()
	b.adapt2(rs)
}

func fl337Keys(t *testing.T, fl, logOnly bool) {
	o1, o2, o3 := b335FastLower, flLogOnly, flStallOn
	b335FastLower, flLogOnly, flStallOn = fl, logOnly, true
	t.Cleanup(func() { b335FastLower, flLogOnly, flStallOn = o1, o2, o3 })
}

func TestB337KharejLowersFromRTO(t *testing.T) {
	fl337Keys(t, true, false)
	var logs []string
	b, now := fl337LB(t, &logs)
	for i := 0; i < 3; i++ {
		fl337Step(b, now, 0.003, "", 0, 0)
	}
	for j := range b.paths {
		if b.mult[j] < 0.99 {
			t.Fatalf("quiet: path %s factor %.2f", b.paths[j].Name(), b.mult[j])
		}
	}
	k0, _ := FastLowerB337Counters()
	_ = k0
	before := flKhLowers.Load()
	fl337Step(b, now, 0.003, "10.60.3.1", 0.10, 2) // 2 links per path, 4 on the IP
	for j, p := range b.paths {
		low := b.mult[j] <= lb2Floor+1e-9
		if (p.Remote == "10.60.3.1") != low {
			t.Fatalf("path %s factor %.2f after one stall-hot reading", p.Name(), b.mult[j])
		}
	}
	if flKhLowers.Load() != before+1 {
		t.Fatal("kharej lower not counted")
	}
	// the edge sees it through the report
	r := b.report()
	for _, pp := range r.Paths {
		if pp.Remote == "10.60.3.1" && pp.Factor > lb2Floor+1e-9 {
			t.Fatalf("report factor %.2f for %s", pp.Factor, pp.Remote)
		}
		if pp.Remote == "10.60.3.1" && !strings.Contains(pp.Reason, "fast lower") {
			t.Fatalf("report reason %q", pp.Reason)
		}
	}
	// held while the burst goes on (small over-bar sample: not calm)
	fl337Step(b, now, 0.003, "10.60.3.1", 0.10, 0)
	if b.mult[2] > lb2Floor+1e-9 {
		t.Fatalf("lifted mid-burst: %.2f", b.mult[2])
	}
	// calm: lifted after flCalm readings, then climbs back
	for i := 0; i < flCalm+1; i++ {
		fl337Step(b, now, 0.003, "", 0, 0)
	}
	if !b.fl.lowered("10.70.1.2>10.60.3.1") && b.mult[2] <= lb2Floor+1e-9 {
		t.Fatalf("not lifted after calm: factor %.2f", b.mult[2])
	}
	if b.fl.lowered("10.70.1.2>10.60.3.1") {
		t.Fatal("still lowered after calm")
	}
	found := false
	for _, l := range logs {
		if strings.Contains(l, "lb fastlower: IP 10.60.3.1 lowered for new connections at once") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no lower line: %q", logs)
	}
}

func TestB337KharejNoLowerQuietNoise(t *testing.T) {
	fl337Keys(t, true, false)
	var logs []string
	b, now := fl337LB(t, &logs)
	// busy healthy links resend everywhere: 3–7 per IP, spread evenly, 0.3 %
	for i := 0; i < 20; i++ {
		*now = now.Add(5 * time.Second)
		rs := map[int]q2Reading{}
		b.mu.Lock()
		b.flCnt = map[string]flRead{}
		b.flRTO = nil
		for j, p := range b.paths {
			k := p.Name()
			b.flCnt[k] = flRead{rt: 6, segs: 2000}
			rs[j] = q2Reading{loss: 0.003, segs: 2000}
			for x := 0; x < 2+i%3; x++ {
				b.flRTO = append(b.flRTO, flStallEv{link: new(int), path: k})
			}
		}
		b.mu.Unlock()
		b.adapt2(rs)
	}
	for j := range b.paths {
		if b.mult[j] < 0.99 {
			t.Fatalf("spread noise lowered %s: %.2f", b.paths[j].Name(), b.mult[j])
		}
	}
	// concentrated RTO links but no corroboration (the IP's counts are clean)
	for i := 0; i < 5; i++ {
		fl337Step(b, now, 0.003, "10.60.3.1", 0.003, 6)
	}
	for j := range b.paths {
		if b.mult[j] < 0.99 {
			t.Fatalf("uncorroborated lower %s: %.2f", b.paths[j].Name(), b.mult[j])
		}
	}
}

func TestB337KharejNeedsSwitch(t *testing.T) {
	fl337Keys(t, false, false)
	var logs []string
	b, now := fl337LB(t, &logs)
	for i := 0; i < 2; i++ {
		fl337Step(b, now, 0.003, "", 0, 0)
	}
	fl337Step(b, now, 0.003, "10.60.3.1", 0.10, 3)
	if b.fl != nil || b.mult[2] < 0.99 {
		t.Fatalf("acted with the switches off: fl %v factor %.2f", b.fl != nil, b.mult[2])
	}
}

func TestB337LogOnlyKharej(t *testing.T) {
	fl337Keys(t, false, true) // LOGONLY alone runs the detector
	var logs []string
	b, now := fl337LB(t, &logs)
	for i := 0; i < 2; i++ {
		fl337Step(b, now, 0.003, "", 0, 0)
	}
	w0 := flWould.Load()
	fl337Step(b, now, 0.003, "10.60.3.1", 0.10, 3)
	for j := range b.paths {
		if b.mult[j] < 0.99 {
			t.Fatalf("log only acted: %s %.2f", b.paths[j].Name(), b.mult[j])
		}
	}
	if flWould.Load() != w0+1 {
		t.Fatalf("would-be lowers %d, want 1", flWould.Load()-w0)
	}
	n := 0
	for _, l := range logs {
		if strings.Contains(l, "IP 10.60.3.1 WOULD be lowered (log only) at once") && strings.Contains(l, "links in RTO backoff") && strings.Contains(l, "resends") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d would-be lines: %q", n, logs)
	}
	// one line per would-be lower: the burst going on adds none
	fl337Step(b, now, 0.003, "10.60.3.1", 0.10, 3)
	if flWould.Load() != w0+1 {
		t.Fatal("a second line for the same burst")
	}
	// FASTLOWER=1 with LOGONLY=1: still log only
	fl337Keys(t, true, true)
	logs = nil
	b2, now2 := fl337LB(t, &logs)
	for i := 0; i < 2; i++ {
		fl337Step(b2, now2, 0.003, "", 0, 0)
	}
	fl337Step(b2, now2, 0.003, "10.60.3.1", 0.10, 3)
	if b2.mult[2] < 0.99 {
		t.Fatal("LOGONLY did not win over FASTLOWER")
	}
}

func TestB337LogOnlyEdge(t *testing.T) {
	fl337Keys(t, true, true)
	a := fl335Paths()
	d := newFastLowerDet(t.Logf)
	now := time.Unix(1e9, 0)
	d.stepStall(now, fl336Win(a, 0.003, "", 0, 0), a, nil)
	now = now.Add(2 * time.Second)
	st := append(fl336Stalls(now, fl336A, 100, 3), fl336Stalls(now, fl336B, 200, 2)...)
	w0 := flWould.Load()
	d.stepStall(now, fl336Win(a, 0.003, "10.60.3.1", 0.10, 40), a, st)
	if d.lowered(fl336A) || d.lowered(fl336B) {
		t.Fatal("log only lowered on the edge")
	}
	if flWould.Load() != w0+1 {
		t.Fatal("would-be lower not counted")
	}
	stt := &lbSteerState{fl: d}
	if stt.fastLowered(fl336A) {
		t.Fatal("fastLowered under log only")
	}
}

func TestB337TakeForKeepsOthers(t *testing.T) {
	fl337Keys(t, true, false)
	flStallTake(time.Now()) // empty the registry
	now := time.Now()
	flStallNote(flStallEv{at: now, conn: 1, link: new(int), path: "a>b"})
	flStallNote(flStallEv{at: now, conn: 2, link: new(int), path: "c>d"})
	flStallNote(flStallEv{at: now.Add(-time.Minute), conn: 3, link: new(int), path: "a>b"})
	got := flStallTakeFor(now, flStallAgeK, map[string]bool{"a>b": true})
	if len(got) != 1 || got[0].conn != 1 {
		t.Fatalf("took %+v", got)
	}
	rest := flStallTake(now)
	if len(rest) != 1 || rest[0].path != "c>d" {
		t.Fatalf("left %+v", rest)
	}
	// concurrency (-race): notes from many goroutines while two LBs take
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				p := "a>b"
				if i%2 == 1 {
					p = "c>d"
				}
				flStallNote(flStallEv{at: time.Now(), conn: uint64(g*1000 + i), link: g, path: p})
			}
		}(g)
	}
	for g := 0; g < 2; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			keep := map[string]bool{"a>b": g == 0, "c>d": g == 1}
			for i := 0; i < 200; i++ {
				flStallTakeFor(time.Now(), flStallAgeK, keep)
			}
		}(g)
	}
	wg.Wait()
	flStallTake(time.Now())
}

func TestB337UncutDebug(t *testing.T) {
	old := lbUncutDebug
	lbUncutDebug = true
	defer func() { lbUncutDebug = old }()
	var logs []string
	b, now := fl337LB(t, &logs)
	for i := 0; i < 4; i++ {
		*now = now.Add(5 * time.Second)
		rs := map[int]q2Reading{0: {loss: 0.003, segs: 2000}, 1: {loss: 0.002, segs: 1800}, 2: {loss: 0.045, segs: 900}, 3: {loss: 0.003, segs: 1500}}
		if i == 1 {
			delete(rs, 0) // a path without a reading this time
		}
		b.adapt2(rs)
	}
	var dbg []string
	for _, l := range logs {
		if strings.HasPrefix(l, "lb uncutdbg:") {
			dbg = append(dbg, l)
		}
	}
	if len(dbg) < 2 {
		t.Fatalf("uncut lines: %q", logs)
	}
	first := dbg[0]
	for _, want := range []string{"10.70.1.2>10.60.1.1 segs 2000 raw 0.30%", "under-bar", "10.70.1.2>10.60.3.1 segs 900 raw 4.50%", "bad-1/2"} {
		if !strings.Contains(first, want) {
			t.Fatalf("missing %q in %s", want, first)
		}
	}
	if !strings.Contains(dbg[1], "10.70.1.2>10.60.1.1 ewma 0.30% t 1.00 no-reading") {
		t.Fatalf("no-reading path: %s", dbg[1])
	}
	// the cut path leaves the line
	last := dbg[len(dbg)-1]
	if strings.Contains(last, "10.70.1.2>10.60.3.1") {
		t.Fatalf("cut path in the uncut line: %s", last)
	}
	// rate limit: two readings 1 s apart give one line
	j := newQ2Judge(lb2Floor, "k", "i", nil)
	n := 0
	j.log = func(string, ...any) { n++ }
	t0 := time.Unix(5000, 0)
	j.update(t0, time.Second, map[string]q2Reading{"a>b": {loss: 0.001}, "c>d": {loss: 0.001}})
	j.update(t0.Add(time.Second), time.Second, map[string]q2Reading{"a>b": {loss: 0.001}, "c>d": {loss: 0.001}})
	if n != 1 {
		t.Fatalf("%d lines in 1 s", n)
	}
}

// ---- cost -------------------------------------------------------------------

// fl337Big: a 4× production window on the edge: 6 Kharej × 8 Iran IPs, busy
// links resending 3–7 per IP per tick (~250), 20 stall notes.
func fl337Big() (map[string]flRead, map[string]bool, []flStallEv) {
	c, a := map[string]flRead{}, map[string]bool{}
	var ev []flStallEv
	now := time.Unix(1e9, 0)
	for k := 0; k < 6; k++ {
		for i := 0; i < 8; i++ {
			p := fmt.Sprintf("10.60.%d.1>10.70.%d.2", i, k)
			a[p] = true
			c[p] = flRead{rt: 12, segs: 4000}
			for x := 0; x < 5; x++ {
				ev = append(ev, flStallEv{link: new(int), path: p})
			}
		}
	}
	for x := 0; x < 20; x++ {
		ev = append(ev, flStallEv{at: now, conn: uint64(x + 1), link: new(int), path: fmt.Sprintf("10.60.%d.1>10.70.%d.2", x%8, x%6)})
	}
	return c, a, ev
}

func BenchmarkB337StallWindow(b *testing.B) {
	o := flStallDebug
	flStallDebug = false
	defer func() { flStallDebug = o }()
	c, a, ev := fl337Big()
	d := newFastLowerDet(nil)
	now := time.Unix(1e9, 0)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		now = now.Add(2 * time.Second)
		d.stepStall(now, c, a, ev)
	}
}

func BenchmarkB337UncutLine(b *testing.B) {
	old := lbUncutDebug
	lbUncutDebug = true
	defer func() { lbUncutDebug = old }()
	j := newQ2Judge(lb2Floor, "k", "i", func(f string, a ...any) { _ = fmt.Sprintf(f, a...) })
	rs := map[string]q2Reading{}
	for k := 0; k < 6; k++ {
		for i := 0; i < 8; i++ {
			rs[fmt.Sprintf("10.70.%d.2>10.60.%d.1", k, i)] = q2Reading{loss: 0.003, segs: 3000}
		}
	}
	j.update(time.Unix(1e9, 0), 5*time.Second, rs)
	now := time.Unix(1e9, 0)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		now = now.Add(5 * time.Second)
		j.uncutAt = time.Time{}
		j.uncutDbg(now, rs, false)
	}
}

func TestB337LinkLossy(t *testing.T) {
	for _, c := range []struct {
		rt, segs uint64
		want     bool
	}{{3, 1000, false}, {6, 2000, false}, {20, 1000, true}, {2, 0, true}, {4, 100, true}} {
		if got := flLinkLossy(c.rt, c.segs); got != c.want {
			t.Fatalf("%d of %d: %v", c.rt, c.segs, got)
		}
	}
}
