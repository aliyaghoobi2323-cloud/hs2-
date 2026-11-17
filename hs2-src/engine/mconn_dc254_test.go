package engine

import (
	"strings"
	"testing"
	"time"
)

func setDc254(t *testing.T, on bool) {
	t.Helper()
	old := dc254On.Load()
	dc254On.Store(on)
	t.Cleanup(func() { dc254On.Store(old) })
}

// A primary whose sends sit in RTO backoff while its link still receives
// (the reverse direction of a storm-shared link): rel69i never called it
// dark; dc254 marks it txDark and moves the call off it.
func TestDc254TxDarkSwitch(t *testing.T) {
	setDarkcall(t)
	setDc254(t, true)
	_, iran, exit := darkswPairDC(t)
	f := newDcFake()
	darkcallReadSet(f.read)
	darkcallMarksSet(func(*msub) (uint32, uint32, bool) { return 0, 0, true })
	exit.mu.Lock()
	p := exit.primary.Load()
	exit.mu.Unlock()
	f.set(p, "lossy") // no ack, backoff 3, rx fresh
	sw0, td0, tds0, d0 := darkcallStat.switched.Load(), dc254Stat.txDark.Load(), dc254Stat.txDarkSw.Load(), darkcallStat.dark.Load()
	dcVisit(exit, 3)
	exit.mu.Lock()
	np := exit.primary.Load()
	exit.mu.Unlock()
	if darkcallStat.switched.Load() == sw0 || np == p {
		t.Fatal("no switch off a txDark primary")
	}
	if dc254Stat.txDark.Load() == td0 || dc254Stat.txDarkSw.Load() == tds0 {
		t.Fatal("txdark not counted")
	}
	if darkcallStat.dark.Load() != d0 {
		t.Fatal("a link that receives was counted dark")
	}
	closeAndWait(t, iran, exit)
}

// The key off: a lossy primary stays (the rel69i rule).
func TestDc254Off(t *testing.T) {
	setDarkcall(t)
	setDc254(t, false)
	_, iran, exit := darkswPairDC(t)
	f := newDcFake()
	darkcallReadSet(f.read)
	darkcallMarksSet(func(*msub) (uint32, uint32, bool) { return 0, 0, true })
	exit.mu.Lock()
	p := exit.primary.Load()
	exit.mu.Unlock()
	f.set(p, "lossy")
	sw0, td0 := darkcallStat.switched.Load(), dc254Stat.txDarkSw.Load()
	dcVisit(exit, 3)
	if darkcallStat.switched.Load() != sw0 || dc254Stat.txDarkSw.Load() != td0 {
		t.Fatal("key off acted")
	}
	closeAndWait(t, iran, exit)
}

// Silent rx: a call whose receive stops for >= 1 s counts one episode, and
// its length lands in the max when the receive moves again.
func TestDc254SilentRx(t *testing.T) {
	setDc254(t, true)
	m := &mConn{}
	now := time.Now()
	m.dc.rxAt = now.Add(-1500 * time.Millisecond)
	s0 := dc254Stat.silentRx.Load()
	m.dc254SilenceLocked(nil, nil, now, 0, 0)
	m.dc254SilenceLocked(nil, nil, now.Add(200*time.Millisecond), 0, 0)
	if got := dc254Stat.silentRx.Load() - s0; got != 1 {
		t.Fatalf("silent episodes %d, want 1", got)
	}
	m.dc.rxAt = now.Add(300 * time.Millisecond)
	m.dc254SilenceLocked(nil, nil, now.Add(300*time.Millisecond), 0, 0)
	if m.dc.silent || dc254Stat.silentMax.Load() < 1500 {
		t.Fatalf("episode not closed: silent %v max %d", m.dc.silent, dc254Stat.silentMax.Load())
	}
	// short gaps are not counted
	m.dc.rxAt = now.Add(1000 * time.Millisecond)
	m.dc254SilenceLocked(nil, nil, now.Add(1700*time.Millisecond), 0, 0)
	if dc254Stat.silentRx.Load()-s0 != 1 {
		t.Fatal("a 0.7 s gap was counted")
	}
	// tx stall of the primary
	p := &msub{}
	p.dc = dcSub{ok: true, unacked: 100, ackAt: now.Add(-1200 * time.Millisecond)}
	t0 := dc254Stat.txStall.Load()
	m.dc254SilenceLocked(p, nil, now, 0, 0)
	m.dc254SilenceLocked(p, nil, now.Add(100*time.Millisecond), 0, 0)
	if dc254Stat.txStall.Load()-t0 != 1 {
		t.Fatal("tx stall not counted once")
	}
}

// Copy hold: a copying call keeps copying through a window that did not
// look call-like (the receive stalled), not past callFreeTTL, never above
// mCallMaxRate, never when it was not copying.
func TestDc254CopyHold(t *testing.T) {
	setDc254(t, true)
	m := &mConn{}
	now := time.Now()
	m.callLikeAt = now.Add(-2 * time.Second)
	if !m.dc254CopyHoldLocked(now, time.Second, 250_000, true) {
		t.Fatal("no hold for a stalled copying call")
	}
	if m.dc254CopyHoldLocked(now, time.Second, 250_000, false) {
		t.Fatal("hold for a call that was not copying")
	}
	if m.dc254CopyHoldLocked(now, time.Second, mCallMaxRate+1, true) {
		t.Fatal("hold above mCallMaxRate")
	}
	m.callLikeAt = now.Add(-callFreeTTL)
	if m.dc254CopyHoldLocked(now, time.Second, 250_000, true) {
		t.Fatal("hold past callFreeTTL")
	}
	dc254On.Store(false)
	m.callLikeAt = now
	if m.dc254CopyHoldLocked(now, time.Second, 250_000, true) {
		t.Fatal("hold with the key off")
	}
}

// A darkcall tick >= 800 ms late counts as a process stall; an on-time one
// does not.
func TestDc254TickLate(t *testing.T) {
	n0 := dc254Stat.lateTicks.Load()
	now := time.Now()
	dc254TickLate(time.Time{}, now)
	dc254TickLate(now.Add(-darkcallEvery-100*time.Millisecond), now)
	if dc254Stat.lateTicks.Load() != n0 {
		t.Fatal("on-time tick counted")
	}
	dc254TickLate(now.Add(-darkcallEvery-1200*time.Millisecond), now)
	if dc254Stat.lateTicks.Load() != n0+1 || dc254Stat.lateMax.Load() < 1200 {
		t.Fatalf("late tick: n +%d max %d", dc254Stat.lateTicks.Load()-n0, dc254Stat.lateMax.Load())
	}
}

// b271 Q3: a silent-rx onset is split by the subs' link evidence: healthy
// links -> app silence; a sub with bytes in flight and no ack progress (or
// dark / txDark / nothing read) -> link evidence. txdark lines have their
// own budget.
func TestDc254SilentSplit(t *testing.T) {
	setDc254(t, true)
	now := time.Now()
	nowc := int64(1_000_000_000_000)
	ok := &msub{idx: 1}
	ok.dc = dcSub{ok: true, ackAt: now, lastRd: nowc}
	l0, a0 := dc254Stat.silentLink.Load(), dc254Stat.silentApp.Load()

	m := &mConn{}
	m.dc.rxAt = now.Add(-time.Second)
	m.dc254SilenceLocked(nil, []*msub{ok}, now, nowc, 0)
	if dc254Stat.silentApp.Load()-a0 != 1 || dc254Stat.silentLink.Load() != l0 {
		t.Fatal("healthy links: not counted as app silence")
	}

	cases := []dcSub{
		{ok: true, unacked: 10, ackAt: now.Add(-time.Second), lastRd: nowc},
		{ok: true, ackAt: now, lastRd: nowc - int64(time.Second)},
		{ok: true, ackAt: now, lastRd: nowc, isDark: true},
		{ok: true, ackAt: now, lastRd: nowc, txDark: true},
	}
	for i, d := range cases {
		bad := &msub{idx: 2}
		bad.dc = d
		m := &mConn{}
		m.dc.rxAt = now.Add(-time.Second)
		l := dc254Stat.silentLink.Load()
		m.dc254SilenceLocked(nil, []*msub{ok, bad}, now, nowc, 0)
		if dc254Stat.silentLink.Load()-l != 1 {
			t.Fatalf("case %d: link evidence not counted (%q)", i, dc254LinkEvidence([]*msub{ok, bad}, now, nowc))
		}
	}
	// a dead or not-looked-at sub gives no evidence
	dead := &msub{dead: true}
	dead.dc = cases[0]
	if ev := dc254LinkEvidence([]*msub{dead, {}}, now, nowc); ev != "" {
		t.Fatalf("dead/unlooked sub gave evidence %q", ev)
	}
	if !strings.Contains(dc254LogPart(), "(link ") {
		t.Fatal("stats line lacks the split")
	}
	// own budget: exhausting dc254TxLog leaves dc254Log untouched
	min := now.Unix() / 60
	dc254Log.min.Store(min)
	dc254Log.n.Store(0)
	for i := 0; i < 50; i++ {
		dc254TxLogf(now, "")
	}
	if dc254Log.n.Load() != 0 || !dc254LogOK(now) {
		t.Fatal("txdark lines ate dc254Log's budget")
	}
}
