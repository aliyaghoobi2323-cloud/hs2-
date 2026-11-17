package engine

import (
	"testing"
	"time"
)

func withRetireKeep(t *testing.T, on bool, idleFor time.Duration) {
	t.Helper()
	o, f := retireKeepIdle, retireKeepIdleFor
	retireKeepIdle, retireKeepIdleFor = on, idleFor
	t.Cleanup(func() { retireKeepIdle, retireKeepIdleFor = o, f })
}

// Key off (rel61): an idle connection on a retiring link is closed after
// drain_idle (310 s). Key on: it survives 30 minutes of ticks, the link stays
// in the pool held by it, and it is reclaimed once idle retireKeepIdleFor.
func TestRetireKeepIdleSparesIdleConn(t *testing.T) {
	for _, on := range []bool{false, true} {
		withRetireKeep(t, on, 40*time.Minute)
		m, clk, lg := newV2Manager(nil, 1, 8, false)
		pl := newV2PipeLink(t)
		idle := pl.open(t)
		ml := addManaged(m, pl)
		ml.retiring, ml.retireSince = true, clk.Now()
		m.sampleHealth()
		runDrainTicks(t, m, clk, ml, 30*30, nil) // 30 minutes
		ended, _ := pl.exitEnded(idle, 200*time.Millisecond)
		if !on {
			if !ended {
				t.Fatal("key off: the idle connection was not closed after drain_idle")
			}
			continue
		}
		if ended || pl.Active() != 1 {
			t.Fatal("key on: an idle connection was closed within 30 min")
		}
		if !inPool(m, pl) {
			t.Fatal("key on: the held link left the pool")
		}
		if lg.count("retire keep: 1 retiring link(s) kept for 1 open connection(s)") == 0 {
			t.Fatalf("key on: no retire keep log:\n%s", lg)
		}
		runDrainTicks(t, m, clk, ml, 15*30, nil) // to 45 minutes
		if ended, _ := pl.exitEnded(idle, 2*time.Second); !ended {
			t.Fatal("key on: a connection idle past retireKeepIdleFor was not reclaimed")
		}
	}
}

// Key on: the retireForce (20 min) close of trickling connections is skipped.
func TestRetireKeepIdleNoTrickleForce(t *testing.T) {
	withRetireKeep(t, true, 60*time.Minute)
	m, clk, _ := newV2Manager(nil, 1, 8, false)
	pl := newV2PipeLink(t)
	trickle := pl.open(t)
	ml := addManaged(m, pl)
	ml.retiring, ml.retireSince = true, clk.Now()
	m.sampleHealth()
	runDrainTicks(t, m, clk, ml, 30*30, func(tick int) {
		if tick%60 == 0 { // a keepalive every 2 min
			if _, err := trickle.Write([]byte("k")); err != nil {
				t.Fatalf("tick %d: %v", tick, err)
			}
		}
	})
	if ended, _ := pl.exitEnded(trickle, 200*time.Millisecond); ended {
		t.Fatal("a trickling connection was force-closed with the key on")
	}
	if m.forced.Load() != 0 {
		t.Fatalf("forced=%d, want 0", m.forced.Load())
	}
}

// Above max_links the old rules apply to the up-max retiring links with the
// fewest open connections.
func TestRetireStrictAboveMax(t *testing.T) {
	m, _, _ := newV2Manager(nil, 1, 4, false)
	a, b, c := &managedLink{open: 5}, &managedLink{open: 1}, &managedLink{open: 3}
	if s := m.retireStrictLocked([]*managedLink{a, b, c}, 4); s != nil {
		t.Fatalf("at max: strict %v, want none", s)
	}
	s := m.retireStrictLocked([]*managedLink{a, b, c}, 6)
	if len(s) != 2 || !s[b] || !s[c] || s[a] {
		t.Fatalf("up 6 max 4: strict %v, want the links with 1 and 3 open", s)
	}
	if s := m.retireStrictLocked([]*managedLink{a}, 9); len(s) != 1 || !s[a] {
		t.Fatalf("more surplus than candidates: %v", s)
	}
	if got := retireKeepThreshold(0); got != 0 {
		t.Fatalf("drain_idle 0: threshold %v, want 0 (reclaim off)", got)
	}
	withRetireKeep(t, true, time.Minute)
	if got := retireKeepThreshold(drainIdleDefault); got != drainIdleDefault {
		t.Fatalf("threshold %v, want never below drain_idle", got)
	}
}
