package engine

import (
	"testing"
	"time"
)

// cs221: the darkcall switch resends only what the target was not given
// (below its missEnd); with the key off it resends the whole unconsumed
// range as before. The connection still delivers every byte once, in order.
func TestCs221SwitchSkipsHeld(t *testing.T) {
	setDarkcall(t)
	oldK := cs221Darkcall.Load()
	t.Cleanup(func() { cs221Darkcall.Store(oldK) })
	cs221Darkcall.Store(true)
	_, iran, exit := darkswPairDC(t)
	f := newDcFake()
	darkcallReadSet(f.read)
	const unc = 60_000
	darkcallMarksSet(func(*msub) (uint32, uint32, bool) { return unc, 0, true })
	data := randBytes(150_000, 31)
	both(t,
		func() error { return xfer(exit, data, 2000, 0) },
		func() error { return readN(iran, data) },
	)
	// switch p -> to with a known missEnd on to
	exit.mu.Lock()
	p := exit.primary.Load()
	var to *msub
	for _, s := range exit.rrLiveSubsLocked() {
		if s != p {
			to = s
		}
	}
	if to == nil || exit.missLost {
		exit.mu.Unlock()
		t.Fatalf("setup: to=%v missLost=%v", to, exit.missLost)
	}
	switchRange := func(p *msub) (from, qStart uint64) {
		qStart = exit.sendOff
		for _, c := range p.q {
			if c.seq() {
				qStart = c.off
				break
			}
		}
		var cur uint64
		if c := p.cur; c != nil && c.seq() {
			cur += uint64(c.n)
		}
		for _, c := range p.curMore {
			if c.seq() {
				cur += uint64(c.n)
			}
		}
		return qStart - (unc + cur), qStart
	}
	from, qStart := switchRange(p)
	to.missEnd = from + 10_000
	to.wrLo, to.wrHi = 0, exit.sendOff // b262: to wrote every chunk it was given
	r0, h0, c0 := darkcallStat.resent.Load(), cs221Stat.held.Load(), darkcallStat.covered.Load()
	exit.dcSwitchLocked(p, to, time.Now(), 0)
	got := darkcallStat.resent.Load() - r0
	if got != 10_000 {
		exit.mu.Unlock()
		t.Fatalf("key on: resent %d, want 10000 (range [%d,%d), missEnd %d)", got, from, qStart, to.missEnd)
	}
	if cs221Stat.held.Load() == h0 || darkcallStat.covered.Load() == c0 {
		exit.mu.Unlock()
		t.Fatal("key on: held / covered not counted")
	}
	// a target that was given everything: nothing to resend
	p.dcLeft = false // cs224: each call here is a first switch off p
	to.missEnd = 0
	r0 = darkcallStat.resent.Load()
	exit.dcSwitchLocked(p, to, time.Now(), 0)
	if got := darkcallStat.resent.Load() - r0; got != 0 {
		exit.mu.Unlock()
		t.Fatalf("key on, target holds all: resent %d, want 0", got)
	}
	// key off: the whole range again
	cs221Darkcall.Store(false)
	p.dcLeft = false
	to.missEnd = from + 10_000
	from, qStart = switchRange(p)
	r0 = darkcallStat.resent.Load()
	exit.dcSwitchLocked(p, to, time.Now(), 0)
	if got := darkcallStat.resent.Load() - r0; got != qStart-from {
		exit.mu.Unlock()
		t.Fatalf("key off: resent %d, want %d", got, qStart-from)
	}
	exit.mu.Unlock()
	cs221Darkcall.Store(true)
	noExtra(t, iran, exit)
	more, up := randBytes(40_000, 32), randBytes(20_000, 33)
	both(t,
		func() error { return xfer(exit, more, 2000, 0) },
		func() error { return readN(iran, more) },
		func() error { return xfer(iran, up, 2000, 0) },
		func() error { return readN(exit, up) },
	)
	closeAndWait(t, iran, exit)
}

// cs221: missEnd follows the placement: a sub that joins late lacks what
// was sent before it; a full queue's miss moves it to the chunk's end.
func TestCs221MissEnd(t *testing.T) {
	s := &msub{}
	s.noteMiss(500)
	s.noteMiss(300)
	if s.missEnd != 500 {
		t.Fatalf("missEnd %d, want 500 (never moves back)", s.missEnd)
	}
	m := &mConn{}
	if m.cs221HeldFrom(s) != 500 {
		t.Fatal("held-from is not missEnd")
	}
	m.missLost = true
	if m.cs221HeldFrom(s) != ^uint64(0) {
		t.Fatal("after a bulk or stripe chunk the target must be taken as lacking all")
	}
	old := cs221Darkcall.Load()
	defer func() { cs221Darkcall.Store(old) }()
	cs221Darkcall.Store(false)
	m.missLost = false
	if m.cs221HeldFrom(s) != ^uint64(0) {
		t.Fatal("key off must give back the full resend")
	}
}

// cs221: Iran's darkcall rx rescue (a live sub dark, the receive stalled)
// waits while a live sub that is not dark reads records; once none has read
// for darkcallAfter it goes ahead as before.
func TestCs221RxRescueHeldWhileLiveReads(t *testing.T) {
	setDarkcall(t)
	oldK := cs221Darkcall.Load()
	t.Cleanup(func() { cs221Darkcall.Store(oldK) })
	cs221Darkcall.Store(true)
	_, iran, exit := darkswPairDC(t)
	f := newDcFake()
	darkcallReadSet(f.read)
	darkcallMarksSet(func(*msub) (uint32, uint32, bool) { return 0, 0, true })
	data := randBytes(20_000, 34)
	both(t,
		func() error { return xfer(iran, data, 2000, 0) },
		func() error { return readN(exit, data) },
	)
	iran.mu.Lock()
	p := iran.primary.Load()
	var o *msub
	for _, s := range iran.rrLiveSubsLocked() {
		if s != p {
			o = s
		}
	}
	iran.mu.Unlock()
	if p == nil || o == nil {
		t.Fatal("setup: two subs")
	}
	f.set(o, "dark")
	visit := func(fresh bool) bool {
		iran.mu.Lock()
		defer iran.mu.Unlock()
		now := time.Now()
		if fresh {
			p.lastRecv.Store(monoNano(now))
		} else {
			p.lastRecv.Store(monoNano(now) - int64(10*time.Second))
		}
		iran.callLikeAt = now
		return iran.darkcallLocked(now, ctrlNow())
	}
	// the process timer (darkcallLoop) visits too: keep the live sub
	// reading all through the hold
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
				p.lastRecv.Store(monoNano(time.Now()))
			}
		}
	}()
	h0, rr0, rn0 := cs221Stat.rxLive.Load(), darkcallStat.rrec.Load(), darkcallStat.rrNo.Load()
	for i := 0; i < 4; i++ {
		if visit(true) {
			close(stop)
			<-done
			t.Fatal("rx rescue started while the live sub reads")
		}
		time.Sleep(darkcallAfter() * 3 / 2)
	}
	close(stop)
	<-done
	if cs221Stat.rxLive.Load() != h0+1 {
		t.Fatalf("rxlive %d, want 1 per hold", cs221Stat.rxLive.Load()-h0)
	}
	if darkcallStat.rrec.Load() != rr0 || darkcallStat.rrNo.Load() != rn0 {
		t.Fatal("rescue path reached during the hold")
	}
	// nothing read on the live sub for darkcallAfter: the old rule acts
	rr := visit(false)
	if darkcallStat.rrec.Load() == rr0 && darkcallStat.rrNo.Load() == rn0 {
		t.Fatal("rx rescue not tried once no live sub reads")
	}
	if rr {
		iran.rrStart()
	}
	// key off: no hold
	cs221Darkcall.Store(false)
	iran.mu.Lock()
	iran.dc.last = time.Time{}
	iran.mu.Unlock()
	h1 := cs221Stat.rxLive.Load()
	visit(true)
	if cs221Stat.rxLive.Load() != h1 {
		t.Fatal("key off still holds")
	}
	closeAndWait(t, iran, exit)
}

// cs221: a call pair never takes two subs of one path while a sub on another
// path has room; with one path only it pairs as before; key off: old pairs.
func TestCs221PairOffSamePath(t *testing.T) {
	old, oldP := cs221Darkcall.Load(), cs221PathOf
	t.Cleanup(func() { cs221PathOf = oldP; cs221Darkcall.Store(old) })
	m := &mConn{lite: true}
	a, b, c := &msub{m: m}, &msub{m: m}, &msub{m: m}
	path := map[*msub]string{a: "p0", b: "p0", c: "p1"}
	cs221PathOf = func(s *msub) string { return path[s] }
	cand := []*msub{a, b, c}
	cs221Darkcall.Store(true)
	for at := 0; at < 6; at++ {
		pk, k := callPairPick(cand, 3, at, 1000)
		if k != 2 {
			t.Fatalf("at %d: %d picks", at, k)
		}
		if path[pk[0]] == path[pk[1]] {
			t.Fatalf("at %d: both copies on %s", at, path[pk[0]])
		}
	}
	// c full: only one path has room, the pair stays there
	c.qBytes = mSubQueueLite
	s0 := cs221Stat.samePair.Load()
	if _, k := callPairPick(cand, 3, 0, 1000); k != 2 {
		t.Fatalf("one path left: %d picks, want 2", k)
	}
	if cs221Stat.samePair.Load() != s0+1 {
		t.Fatal("samepair not counted")
	}
	c.qBytes = 0
	cs221Darkcall.Store(false)
	if pk, _ := callPairPick(cand, 3, 0, 1000); pk[0] != a || pk[1] != b {
		t.Fatal("key off: not the old round-robin pair")
	}
}
