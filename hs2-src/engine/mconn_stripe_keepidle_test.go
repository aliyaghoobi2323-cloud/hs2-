package engine

import (
	"testing"
	"time"
)

// mb174 (audit M2): idle connections' kept chunks fill keepStallGlobal, then
// an active connection's dark-path hole is dropped.

// keepIdleConn: a striped conn whose 4 subs wrote perSub bytes each with
// lag bytes in flight, then went quiet; the peer then acks everything.
func keepIdleConn(fm fakeMarks, id uint64, t0 time.Time, perSub, lag int) (*mConn, time.Duration) {
	m := newMConn(id, false, nil)
	for i := 0; i < 4; i++ {
		m.subs = append(m.subs, &msub{m: m, idx: i})
	}
	m.stN = 3
	m.mu.Lock()
	off := uint64(0)
	for j := 0; j < perSub>>14; j++ {
		for _, s := range m.subs {
			fm.write(m, s, t0, keepChunk(off, 16<<10), uint32(lag))
			off += 16 << 10
		}
	}
	_, w := m.ackStallTickLocked(t0) // the manager's tick at the end (nothing suspect)
	m.mu.Unlock()
	fmMu.Lock()
	for _, s := range m.subs {
		if p := fm[s]; p != nil {
			p[1] = p[0] // the peer read it all (its update came after the tick)
		}
	}
	fmMu.Unlock()
	return m, w
}

// simIdle: the manager of m for d from t0: a tick only when the previous one
// asked for it (d > 0), as manage() does; nothing else kicks a quiet conn.
func simIdle(m *mConn, t0 time.Time, w, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := t0
	for w > 0 && now.Sub(t0) < d {
		now = now.Add(w)
		_, w = m.ackStallTickLocked(now)
	}
}

func keepIdleScenario(t *testing.T, idleOn bool) (pinned int64, holeKept bool) {
	fm := setKeepStall(t, true)
	keepStallLazy.Store(true) // production: lazy poll
	oldG, oldI := keepStallGlobal, keepIdleOn
	keepStallGlobal = 24 << 20
	keepIdleOn = idleOn
	t.Cleanup(func() { keepStallGlobal, keepIdleOn = oldG, oldI })
	base := keepStallStat.kept.Load()
	t0 := time.Now()
	var idle []*mConn
	var ws []time.Duration
	for i := 0; i < 10; i++ { // 10 x 4 subs x ~1 MiB in flight
		m, w := keepIdleConn(fm, uint64(1740+i), t0, 2<<20, 1<<20)
		idle, ws = append(idle, m), append(ws, w)
	}
	for i, m := range idle {
		simIdle(m, t0, ws[i], 3*time.Second)
	}
	pinned = keepStallStat.kept.Load() - base
	// an active heavy conn: sub x's chunks went into a dark path (the hole),
	// sub y writes on (8 MiB, each chunk acked at once)
	a := newMConn(1750, false, nil)
	x, y := &msub{m: a, idx: 1}, &msub{m: a, idx: 2}
	a.subs = []*msub{x, y}
	a.stN = 3
	a.mu.Lock()
	t1 := t0.Add(3 * time.Second)
	// (the dark path swallowed 17 MiB: over stripeKeepBytes, where the
	// global cap applies)
	off := uint64(7000)
	for i := 0; i < 17<<6; i++ {
		fm.write(a, x, t1, keepChunk(off, 16<<10), 1<<30)
		off += 16 << 10
	}
	for i := 0; i < 512; i++ {
		fm.write(a, y, t1.Add(time.Duration(i)*100*time.Microsecond), keepChunk(off, 16<<10), 0)
		off += 16 << 10
	}
	holeKept = len(x.sent) > 0 && x.sent[0].c.off == 7000
	a.releaseSentLocked()
	a.mu.Unlock()
	for _, m := range idle {
		m.mu.Lock()
		m.releaseSentLocked()
		m.mu.Unlock()
	}
	return pinned, holeKept
}

// The bug (HS2_MCONN_STRIPE_KEEPSTALL_IDLE=0): acked bytes stay pinned on
// quiet conns and the active conn's hole goes.
func TestKeepIdleM2Repro(t *testing.T) {
	pinned, hole := keepIdleScenario(t, false)
	t.Logf("old: pinned %d KiB by quiet conns, hole kept %v", pinned>>10, hole)
	if pinned < 20<<20 || hole { // (other tests may leave a few kept bytes in the global count)
		t.Fatalf("no longer reproduces M2: pinned %d hole %v", pinned, hole)
	}
}

// The fix: the slow poll frees the acked bytes within ~1 s; the hole stays.
func TestKeepIdleReleasesAcked(t *testing.T) {
	pinned, hole := keepIdleScenario(t, true)
	t.Logf("new: pinned %d KiB, hole kept %v", pinned>>10, hole)
	if pinned != 0 {
		t.Fatalf("quiet conns still pin %d bytes", pinned)
	}
	if !hole {
		t.Fatal("the active conn's hole was dropped")
	}
}

// The unacked tail (under smux's update threshold) of a quiet conn goes after
// keepIdleDrop, not before; an active conn's (bytes queued) never.
func TestKeepIdleDropsTail(t *testing.T) {
	fm := setKeepStall(t, true)
	keepStallLazy.Store(true)
	t0 := time.Now()
	m := newMConn(1760, false, nil)
	s := &msub{m: m, idx: 1}
	m.subs = []*msub{s}
	m.stN = 3
	m.mu.Lock()
	defer m.mu.Unlock()
	fm.write(m, s, t0, keepChunk(0, 16<<10), 1<<30) // never acked
	if !m.ksIdle {
		t.Fatal("write did not arm the slow poll")
	}
	if _, w := m.ackStallTickLocked(t0.Add(5 * time.Second)); w == 0 || m.keptBytes == 0 {
		t.Fatalf("tail dropped too early (w=%v kept=%d)", w, m.keptBytes)
	}
	s.qBytes = 1 // active: never dropped
	m.ackStallTickLocked(t0.Add(keepIdleDrop + time.Second))
	if m.keptBytes == 0 {
		t.Fatal("an active conn's chunk dropped")
	}
	s.qBytes = 0
	if _, w := m.ackStallTickLocked(t0.Add(keepIdleDrop + time.Second)); w != 0 || m.keptBytes != 0 || m.ksIdle {
		t.Fatalf("quiet tail kept after %v (w=%v kept=%d)", keepIdleDrop, w, m.keptBytes)
	}
}
