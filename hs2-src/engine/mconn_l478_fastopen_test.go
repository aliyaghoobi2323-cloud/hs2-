package engine

import (
	"testing"
	"time"
)

// l478: with the key on, a dark primary whose tail goes to the live hedge
// hands the primary to it (both sides); key off: the primary stays.
func TestL478DarkPrimaryMoves(t *testing.T) {
	silent, rd := l477Setup(t)
	o := l478FastOpen.Load()
	t.Cleanup(func() { l478FastOpen.Store(o) })
	for _, on := range []bool{false, true} {
		l478FastOpen.Store(on)
		for _, iran := range []bool{true, false} {
			m, a, b := l477Conn(iran)
			silent[a], rd[a] = time.Second, 7
			m0 := l478Stat.moves.Load()
			m.l477TailResLocked(nil, time.Now(), 0)
			if len(b.q) == 0 {
				t.Fatal("no tail resent")
			}
			got := m.primary.Load()
			if on && (got != b || l478Stat.moves.Load() != m0+1) {
				t.Fatalf("iran=%v: dark primary not moved to the live hedge", iran)
			}
			if !on && got != a {
				t.Fatalf("iran=%v key off: primary moved", iran)
			}
		}
	}
}

// l478: a hedge that is not the primary going dark never moves the primary.
func TestL478HedgeDarkNoMove(t *testing.T) {
	silent, rd := l477Setup(t)
	o := l478FastOpen.Load()
	t.Cleanup(func() { l478FastOpen.Store(o) })
	l478FastOpen.Store(true)
	m, a, b := l477Conn(true)
	b.sent, a.sent = a.sent, nil
	silent[b], rd[b] = time.Second, 7
	m.l477TailResLocked(nil, time.Now(), 0)
	if m.primary.Load() != a {
		t.Fatal("primary changed for a dark hedge")
	}
}

// l478OpenOK: the global bound and the ZDU drain refuse opens; a slot comes
// back with l478OpenDone.
func TestL478OpenBound(t *testing.T) {
	og := l478Global
	t.Cleanup(func() { l478Global = og; l478Inflight.Store(0); zduDraining.Store(false) })
	l478Global = 2
	l478Inflight.Store(0)
	if !l478OpenOK() || !l478OpenOK() {
		t.Fatal("refused under the bound")
	}
	c0 := l478Stat.capped.Load()
	if l478OpenOK() || l478Stat.capped.Load() != c0+1 {
		t.Fatal("over the bound")
	}
	l478OpenDone()
	if !l478OpenOK() {
		t.Fatal("slot not returned")
	}
	l478OpenDone()
	zduDraining.Store(true)
	if l478OpenOK() {
		t.Fatal("open during a ZDU drain")
	}
	if l478Inflight.Load() != 1 {
		t.Fatalf("inflight %d", l478Inflight.Load())
	}
}
