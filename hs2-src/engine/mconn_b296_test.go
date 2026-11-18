package engine

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// b296: exitTraffic.sample copies the set under t.mu and computes outside
// it; counts stay right and add/done never wait for the scan (-race).
func TestB296ExitSampleCopy(t *testing.T) {
	tr := newExitTraffic(nil)
	mtr := &linkMeter{}
	var cs []*exitConn
	for i := 0; i < 100; i++ {
		c := tr.open(mtr)
		if i >= 60 {
			tr.done(c)
			c = tr.openSub(mtr) // a sub: heavy only, not a user conn
		}
		cs = append(cs, c)
	}
	now := time.Now()
	tr.sample(now)
	for i, c := range cs {
		if i%2 == 0 {
			c.bytes.Add(8 << 20) // 4 MB/s over 2 s: flowing and heavy
		}
	}
	tr.sample(now.Add(healthTick))
	s := tr.snap.Load()
	if s.open != 60 || s.flowing != 30 {
		t.Fatalf("open %d flowing %d, want 60 30", s.open, s.flowing)
	}
	if h := mtr.heavyN.Load(); h != 50 {
		t.Fatalf("heavyN %d, want 50", h)
	}
	if len(tr.scan) != 0 {
		t.Fatalf("scan copy kept %d pointers", len(tr.scan))
	}

	// concurrent opens/closes while sampling
	var wg sync.WaitGroup
	var stop atomic.Bool
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				c := tr.open(mtr)
				c.bytes.Add(100)
				tr.done(c)
			}
		}()
	}
	at := now.Add(2 * healthTick)
	for i := 0; i < 50; i++ {
		at = at.Add(healthTick)
		tr.sample(at)
	}
	stop.Store(true)
	wg.Wait()
	tr.sample(at.Add(healthTick))
	if s := tr.snap.Load(); s.open != 60 {
		t.Fatalf("after churn: open %d, want 60", s.open)
	}
	if u := mtr.users.Load(); u != 100 {
		t.Fatalf("users %d, want 100", u)
	}
}

// b296: darkcall idle proof — a visit of an idle, non-call conn with nothing
// in flight lets the timer skip it until a write (dcWr) or a sub; in-flight
// bytes, a call or the backstop tick visit it.
func TestB296DarkcallIdleSkip(t *testing.T) {
	setDarkcall(t)
	withCs226(t, true)
	withCs228(t, false, true)
	b296Force.Store(true)
	t.Cleanup(func() { b296Force.Store(false) })
	_, _, exit := darkswPairDC(t)
	var inflight atomic.Bool
	darkcallMarksSet(func(s *msub) (uint32, uint32, bool) {
		if inflight.Load() {
			return 1000, 10, true
		}
		return 1000, 1000, true
	})
	withCs226Silent(t, func(*msub, int64) bool { return true })
	noCall := func() {
		exit.mu.Lock()
		exit.callLikeAt, exit.callOn = time.Time{}, false
		exit.mu.Unlock()
	}
	// prove: visits until a visit saw no write meanwhile (the pair's own
	// setup traffic bumps dcWr for a while)
	prove := func() bool {
		for i := 0; i < 200; i++ {
			exit.darkcallVisit(time.Now(), ctrlNow())
			if exit.b296Skip(false) {
				return true
			}
			time.Sleep(10 * time.Millisecond)
		}
		return false
	}
	noCall()
	if !prove() {
		t.Fatal("idle conn: no proof after a visit")
	}
	if exit.b296Skip(true) {
		t.Fatal("backstop tick skipped")
	}
	// a write ends the proof
	exit.dcWrBump()
	if exit.b296Skip(false) {
		t.Fatal("skip after a write")
	}
	// in flight: a lite visit, no proof
	inflight.Store(true)
	exit.darkcallVisit(time.Now(), ctrlNow())
	if exit.b296Skip(false) {
		t.Fatal("proof with bytes in flight")
	}
	inflight.Store(false)
	if !prove() {
		t.Fatal("no proof once consumed")
	}
	// a call: never skipped
	exit.dcWrBump()
	exit.mu.Lock()
	exit.callLikeAt = time.Now()
	exit.mu.Unlock()
	exit.darkcallVisit(time.Now(), ctrlNow())
	if exit.b296Skip(false) {
		t.Fatal("call conn proved idle")
	}
	noCall()
	if !prove() {
		t.Fatal("no proof after the call ended")
	}
	// the timer skips it (no lite walk) while the proof holds
	exit.mu.Lock()
	w0 := exit.dc.liteWalks
	exit.mu.Unlock()
	sk0 := b296Stat.skipped.Load()
	b296Tick.Store(1)
	darkcallTick(time.Now(), ctrlNow())
	exit.mu.Lock()
	w1 := exit.dc.liteWalks
	exit.mu.Unlock()
	if b296Stat.skipped.Load() == sk0 {
		t.Fatal("timer skipped nothing")
	}
	if w1 != w0 && w1-w0 > 1 { // the global timer may race one visit in
		t.Fatalf("skipped conn walked %d times", w1-w0)
	}
	// key off: no skip
	b296DCSkip.Store(false)
	defer b296DCSkip.Store(true)
	if exit.b296Skip(false) {
		t.Fatal("skip with HS2_B296_DCSKIP=0")
	}
}

// b296: the reordered cs226 lite gate (link tests before the ack marks)
// reaches rel69l's verdict for every sub state: random silent/pending/low/
// in-flight/dark/dead mixes, old order evaluated inline as the reference.
func TestB296LiteOrderSame(t *testing.T) {
	setDarkcall(t)
	withCs226(t, true)
	withCs228(t, false, true)
	type st struct{ silent, pend, low, infl, marksOK, dark, dead bool }
	var states map[*msub]st
	var calls atomic.Int64
	withCs226Silent(t, func(s *msub, _ int64) bool { calls.Add(1); return states[s].silent })
	withCs228Pend(t, func(s *msub, _ int64) bool { return states[s].pend })
	darkcallMarksSet(func(s *msub) (uint32, uint32, bool) {
		x := states[s]
		if x.infl {
			return 20, 10, x.marksOK
		}
		return 10, 10, x.marksOK
	})
	ref := func(m *mConn) bool { // rel69l order (5674c6bb)
		if !m.dc.checkAt.IsZero() {
			return true
		}
		for _, s := range m.subs {
			if s.dead || s.retired {
				continue
			}
			if s.dc.isDark {
				return true
			}
			if !cs228InFlight(s) {
				continue
			}
			if rescueLowKey(s.key) && cs228Pending(s, 0) || cs226Silent(s, 0) {
				return true
			}
		}
		return false
	}
	seed := uint64(0x9e3779b97f4a7c15)
	rnd := func() bool { seed ^= seed << 13; seed ^= seed >> 7; seed ^= seed << 17; return seed&1 == 1 }
	diff, trues := 0, 0
	for it := 0; it < 20000; it++ {
		m := &mConn{mode: planeLight}
		states = map[*msub]st{}
		n := 1 + it%4
		for i := 0; i < n; i++ {
			key := new(int)
			s := &msub{m: m, key: key}
			x := st{rnd(), rnd(), rnd(), rnd(), !rnd() || rnd(), rnd() && rnd() && rnd(), rnd() && rnd()}
			s.dc.isDark, s.dead = x.dark, x.dead
			if x.low {
				rescueLow.Store(key, true)
			}
			states[s] = x
			m.subs = append(m.subs, s)
		}
		a, b := ref(m), m.cs226LiteVisitLocked(0)
		if a != b {
			diff++
		}
		if a {
			trues++
		}
		for _, s := range m.subs {
			rescueLow.Delete(s.key)
		}
	}
	if diff != 0 {
		t.Fatalf("%d of 20000 verdicts differ from rel69l's order", diff)
	}
	if trues < 2000 || trues > 18000 {
		t.Fatalf("degenerate mix: %d true verdicts", trues)
	}
}

// b296: the cached link meter is the one dcSubMeter finds (subs never change
// rw; a link's meter is set at creation), also for a sub without a meter.
func TestB296MeterCacheSame(t *testing.T) {
	setDarkcall(t)
	_, iran, exit := darkswPairDC(t)
	for _, m := range []*mConn{iran, exit} {
		m.mu.Lock()
		for _, s := range m.subs {
			if dcMeterOf(s) != dcSubMeter(s) {
				t.Fatalf("sub %d: cached meter differs", s.idx)
			}
		}
		m.mu.Unlock()
	}
	mtr := &linkMeter{}
	l := &mtcpLink{mtr: mtr}
	s := &msub{rw: &mconnSub{stream: &countedStream{link: l}}}
	s.dcMtr = dcSubMeter(s) // as addSubLocked does
	if s.dcMtr != mtr || dcMeterOf(s) != mtr {
		t.Fatal("counted stream: wrong cached meter")
	}
	s2 := &msub{rw: &mconnSub{stream: &countedStream{}}}
	s2.dcMtr = dcSubMeter(s2)
	if s2.dcMtr != nil || dcMeterOf(s2) != nil {
		t.Fatal("no link: meter should be nil")
	}
}
