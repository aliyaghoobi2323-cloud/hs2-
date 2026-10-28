package engine

import (
	"testing"
	"time"
)

// R1 round 4, XShare (xshare.go): a user's floor on a link weighed by the
// links it is active on.

// TestXShareNote: the links a user is active on, counted per span (this one
// or the one before, whichever more), the old ones forgotten, and a swept
// entry told to its holder.
func TestXShareNote(t *testing.T) {
	var s xshareSet
	ms := int64(time.Millisecond)
	e := s.ent(7, 0)
	if k := e.note(1, 0); k != 1 {
		t.Fatalf("one link: %d", k)
	}
	for l := 2; l <= 8; l++ {
		e.note(l, 10*ms)
	}
	if k := e.note(1, 50*ms); k != 8 {
		t.Fatalf("eight links in a span: %d", k)
	}
	// the next span: only link 1 ticked so far, the span before counts
	if k := e.note(1, 210*ms); k != 8 {
		t.Fatalf("the span before still counts: %d", k)
	}
	// two spans later with link 1 alone
	e.note(1, 410*ms)
	if k := e.note(1, 420*ms); k != 1 {
		t.Fatalf("seven links gone for two spans: %d", k)
	}
	if len(e.seen) > 2 {
		t.Fatalf("the links gone are still kept: %v", e.seen)
	}
	// idle for xshareIdle: the next ent of another tag sweeps it
	later := 420*ms + int64(xshareIdle) + int64(time.Second)
	s.ent(8, later)
	if k := e.note(1, later); k != 0 {
		t.Fatalf("a swept entry answered %d, not 0", k)
	}
	if e2 := s.ent(7, later); e2 == e || e2.note(1, later) != 1 {
		t.Fatalf("the tag's new entry")
	}
}

// xshareSim: link 1's scheduler of a sim with XShare on (its own set), and
// a hook that marks user `heavy` active on links 2..links at each tick.
func xshareSim(t *testing.T, on bool, heavy uint32, links int) *schedSim {
	m := newSchedSim(t, 100, 3)
	if on {
		set := &xshareSet{}
		m.s.setXShare(set, 1)
		m.onTick = func(now time.Duration) {
			for l := 2; l <= links; l++ {
				set.ent(heavy, int64(now)).note(l, int64(now))
			}
		}
	}
	return m
}

// TestXShareViewer: on a link, a viewer (one flow) next to a heavy user
// that has two flows here and is active on 7 other links too. Each link
// alone gives the viewer C/2 and a third of the surplus; with XShare its
// floor is C·1/(1 + 1/8) and it takes 0.85 of the link at least, the heavy
// user keeping its floor here (C/9) and the link full.
func TestXShareViewer(t *testing.T) {
	for _, on := range []bool{false, true} {
		m := xshareSim(t, on, 2, 8)
		m.add(simFlow{user: 1, kind: simBacklog, lane: LaneBulk})
		for range 2 {
			m.add(simFlow{user: 2, kind: simBacklog, lane: LaneBulk})
		}
		m.run(10 * time.Second)
		from, to := time.Second, 10*time.Second
		rv, rh := m.rate(from, to, ofUser(1)), m.rate(from, to, ofUser(2))
		t.Logf("XShare %v: viewer %.1f%%, heavy %.1f%% of the link", on, 100*rv/m.r, 100*rh/m.r)
		if rv+rh < 0.97*m.r {
			t.Fatalf("XShare %v: the link %.1f%% full", on, 100*(rv+rh)/m.r)
		}
		switch {
		case !on && (rv < 0.45*m.r || rv > 0.6*m.r):
			t.Fatalf("XShare off: the viewer got %.1f%%, C/2 and a third of the surplus is due", 100*rv/m.r)
		case on && rv < 0.85*m.r:
			t.Fatalf("XShare on: the viewer got %.1f%%, its floor is 88.9%%", 100*rv/m.r)
		case on && rh < 0.9*m.r/9:
			t.Fatalf("XShare on: the heavy user got %.1f%%, its floor is 11.1%%", 100*rh/m.r)
		}
	}
}

// TestXShareOneLinkEach: users each active on this link alone weigh 1 each:
// the shares of TestSchedLowDemandFreesShare (C/3 floors, 0.1 R asked and
// taken, the surplus by flows) with XShare on.
func TestXShareOneLinkEach(t *testing.T) {
	m := xshareSim(t, true, 99, 1)
	m.add(simFlow{user: 1, kind: simPaced, lane: LaneBulk, rate: 0.1 * m.r})
	m.add(simFlow{user: 2, kind: simBacklog, lane: LaneBulk})
	for range 4 {
		m.add(simFlow{user: 3, kind: simBacklog, lane: LaneBulk})
	}
	m.run(10 * time.Second)
	from, to := time.Second, 10*time.Second
	ra, rb, rc := m.rate(from, to, ofUser(1)), m.rate(from, to, ofUser(2)), m.rate(from, to, ofUser(3))
	t.Logf("low demand %.1f%%, 1 flow %.1f%%, 4 flows %.1f%% of the link", 100*ra/m.r, 100*rb/m.r, 100*rc/m.r)
	if ra < 0.09*m.r || ra > 0.11*m.r || rb < 0.9*m.r/3 || rc < 0.9*m.r/3 || ra+rb+rc < 0.97*m.r {
		t.Fatalf("XShare with one link each changed C2's shares")
	}
}

// TestXShareLightUnchanged: XShare moves only the bulk floors: a light
// user's frames still wait at most a bulk frame and the frame in progress
// (TestSchedLightWait's bound), the heavy user active on 8 links.
func TestXShareLightUnchanged(t *testing.T) {
	m := xshareSim(t, true, 2, 8)
	m.add(simFlow{user: 1, kind: simBacklog, lane: LaneBulk})
	for range 4 {
		m.add(simFlow{user: 2, kind: simBacklog, lane: LaneBulk})
	}
	for u := uint32(10); u < 15; u++ {
		m.add(simFlow{user: u, kind: simRandom, lane: LaneLatency, size: 200, gap: 50 * time.Millisecond})
	}
	m.run(5 * time.Second)
	p99, worst := m.waits("XShare")
	t.Logf("light wait p99 %v max %v", p99, worst)
}
