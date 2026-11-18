package engine

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xtaci/smux"
)

// b322 tests: HS2_B322_EXITNOWATCH (exit subs without a watcher goroutine).

// b332: the key is on by default; envdef lists it as "1" (exit).
func TestB322ExitNoWatchDefaultOn(t *testing.T) {
	if !b322ExitNoWatchDefault {
		t.Fatal("HS2_B322_EXITNOWATCH must default to on")
	}
}

func b322Key(t *testing.T, on bool) {
	old := b322ExitNoWatch.Load()
	b322ExitNoWatch.Store(on)
	t.Cleanup(func() { b322ExitNoWatch.Store(old) })
}

// b322Streams opens n client streams and returns the client ends and the
// accepted server ends (in order).
func b322Streams(t *testing.T, cs, ss *smux.Session, n int) ([]*smux.Stream, []*smux.Stream) {
	var cl, sv []*smux.Stream
	for i := 0; i < n; i++ {
		c, err := cs.OpenStream()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Write([]byte{1}); err != nil { // the server learns the stream
			t.Fatal(err)
		}
		s, err := ss.AcceptStream()
		if err != nil {
			t.Fatal(err)
		}
		var b [1]byte
		if _, err := s.Read(b[:]); err != nil {
			t.Fatal(err)
		}
		cl, sv = append(cl, c), append(sv, s)
	}
	return cl, sv
}

func b322Conns(tr *exitTraffic) int {
	n := 0
	for i := range tr.sh {
		tr.sh[i].mu.Lock()
		n += len(tr.sh[i].conns)
		tr.sh[i].mu.Unlock()
	}
	return n
}

func b322WaitFor(t *testing.T, what string, f func() bool) {
	t.Helper()
	dl := time.Now().Add(5 * time.Second)
	for !f() {
		if time.Now().After(dl) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func streamDead(s *smux.Stream) bool {
	select {
	case <-s.GetDieCh():
		return true
	default:
		return false
	}
}

// Key on and off: subs closed by the peer and the rest ended with the
// connection (abort: rst, the subs' own close is delayed, so the die path
// closes them) leave no traffic entry and the gauge back where it was; with
// the key on every sub went through exactly one of the two paths.
func TestB322ExitSubsBookkeeping(t *testing.T) {
	for _, on := range []bool{false, true} {
		b322Key(t, on)
		cs, ss := smuxPair(t)
		tr := newExitTraffic(nil)
		g0 := mconnStat.exitSubs.Load()
		a0, c0, d0 := b322Stat.attached.Load(), b322Stat.byClose.Load(), b322Stat.byDie.Load()
		mc := newMConnExit(1)
		cl, sv := b322Streams(t, cs, ss, 6)
		for _, s := range sv {
			attachExitSub(s, mc, mconnRoleJoin, tr, nil)
		}
		if b322Conns(tr) != 6 || mconnStat.exitSubs.Load()-g0 != 6 {
			t.Fatalf("on=%v: %d traffic entries, gauge +%d, want 6", on, b322Conns(tr), mconnStat.exitSubs.Load()-g0)
		}
		for _, c := range cl[:3] { // the peer closes half
			c.Close()
		}
		b322WaitFor(t, "peer-closed subs counted down", func() bool { return b322Conns(tr) == 3 })
		mc.abort()
		b322WaitFor(t, "every sub counted down", func() bool { return b322Conns(tr) == 0 && mconnStat.exitSubs.Load() == g0 })
		for i, s := range sv {
			if !streamDead(s) {
				b322WaitFor(t, "stream closed", func() bool { return streamDead(s) })
			}
			_ = i
		}
		if on {
			a, c, d := b322Stat.attached.Load()-a0, b322Stat.byClose.Load()-c0, b322Stat.byDie.Load()-d0
			if a != 6 || c+d != 6 || c < 3 {
				t.Fatalf("attached %d close %d die %d, want 6 = close(>=3) + die", a, c, d)
			}
			if b322Line() == "" {
				t.Fatal("no mem line")
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A sub attached after its connection died is closed and counted down at
// once (the watcher's mc-died branch).
func TestB322AttachAfterDie(t *testing.T) {
	b322Key(t, true)
	cs, ss := smuxPair(t)
	tr := newExitTraffic(nil)
	g0 := mconnStat.exitSubs.Load()
	mc := newMConnExit(2)
	mc.abort()
	<-mc.GetDieCh()
	_, sv := b322Streams(t, cs, ss, 2)
	for _, s := range sv {
		attachExitSub(s, mc, mconnRoleJoin, tr, nil)
	}
	if b322Conns(tr) != 0 || mconnStat.exitSubs.Load() != g0 {
		t.Fatalf("%d entries, gauge %+d after attach to a dead conn", b322Conns(tr), mconnStat.exitSubs.Load()-g0)
	}
	for _, s := range sv {
		if !streamDead(s) {
			t.Fatal("stream of a dead conn left open")
		}
	}
}

// A sub moved off m.subs without being closed (what a rescue does) is
// closed when the connection ends, its bookkeeping run once.
func TestB322AbandonedSubClosedAtDie(t *testing.T) {
	b322Key(t, true)
	cs, ss := smuxPair(t)
	tr := newExitTraffic(nil)
	mc := newMConnExit(3)
	_, sv := b322Streams(t, cs, ss, 2)
	for _, s := range sv {
		attachExitSub(s, mc, mconnRoleJoin, tr, nil)
	}
	mc.mu.Lock()
	x := mc.subs[0]
	x.retired = true
	mc.removeSubLocked(x)
	mc.mu.Unlock()
	d0 := b322Stat.byDie.Load()
	mc.end(errMConnAborted, false) // not rst: end closes what is in m.subs now
	b322WaitFor(t, "abandoned sub closed", func() bool { return b322Conns(tr) == 0 && streamDead(sv[0]) && streamDead(sv[1]) })
	if b322Stat.byDie.Load()-d0 < 1 {
		t.Fatal("the abandoned sub was not fired by the die path")
	}
}

// Many attaches racing the end: every sub's bookkeeping runs exactly once
// (the traffic entry and the gauge come back exactly; a double run would
// drive the gauge below its start).
func TestB322AttachRacesEnd(t *testing.T) {
	b322Key(t, true)
	for it := 0; it < 40; it++ {
		cs, ss := smuxPair(t)
		tr := newExitTraffic(nil)
		g0 := mconnStat.exitSubs.Load()
		mc := newMConnExit(uint64(100 + it))
		_, sv := b322Streams(t, cs, ss, 8)
		var fired atomic.Int64
		var wg sync.WaitGroup
		for _, s := range sv {
			wg.Add(1)
			go func(s *smux.Stream) {
				defer wg.Done()
				c := tr.openSub(nil)
				mconnStat.exitSubs.Add(1)
				attachExitNoWatch(s, exitCountedStream{s, c}, mc, mconnRoleJoin, func() {
					fired.Add(1)
					tr.done(c)
					mconnStat.exitSubs.Add(-1)
				})
			}(s)
		}
		go mc.abort()
		wg.Wait()
		b322WaitFor(t, "all fired", func() bool { return fired.Load() == 8 })
		time.Sleep(5 * time.Millisecond)
		if fired.Load() != 8 || b322Conns(tr) != 0 || mconnStat.exitSubs.Load() != g0 {
			t.Fatalf("it %d: fired %d entries %d gauge %+d", it, fired.Load(), b322Conns(tr), mconnStat.exitSubs.Load()-g0)
		}
		cs.Close()
		ss.Close()
	}
}
