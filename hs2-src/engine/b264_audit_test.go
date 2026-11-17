package engine

import (
	"testing"
	"time"
)

// b264 audit: HS2_DEFAULTS=legacy (the "all keys off" rollback) turns dc254
// off unless HS2_DC254=1; otherwise the key keeps its in-code default (on).
func TestB264Dc254Legacy(t *testing.T) {
	t.Setenv("HS2_DEFAULTS", "legacy")
	t.Setenv("HS2_DC254", "")
	if dc254Key() {
		t.Fatal("legacy: dc254 on")
	}
	t.Setenv("HS2_DC254", "1")
	if !dc254Key() {
		t.Fatal("legacy + HS2_DC254=1: dc254 off")
	}
	t.Setenv("HS2_DEFAULTS", "")
	t.Setenv("HS2_DC254", "")
	if !dc254Key() {
		t.Fatal("default: dc254 off")
	}
	t.Setenv("HS2_DC254", "0")
	if dc254Key() {
		t.Fatal("HS2_DC254=0: dc254 on")
	}
}

// b264 audit: dc254's lines have their own per-minute budget and never eat
// the darkcall lifetime cap.
func TestB264Dc254LogBudget(t *testing.T) {
	now := time.Unix(1_900_000_020, 0)
	dc254Log.min.Store(0)
	dc254Log.n.Store(0)
	l0, d0 := darkcallStat.logged.Load(), dc254Log.drop.Load()
	for i := int64(0); i < dc254Log.per; i++ {
		if !dc254LogOK(now) {
			t.Fatalf("line %d refused", i)
		}
	}
	if dc254LogOK(now.Add(10*time.Second)) || dc254Log.drop.Load()-d0 != 1 {
		t.Fatal("over the budget: not refused/counted")
	}
	if !dc254LogOK(now.Add(time.Minute)) {
		t.Fatal("next minute: refused")
	}
	if darkcallStat.logged.Load() != l0 {
		t.Fatal("dc254 used the darkcall log cap")
	}
}

// b264 audit: a silent-rx episode that ends because the connection stops
// being a call (a hang-up) is counted as gone, never as a stall length.
func TestB264Dc254SilentGone(t *testing.T) {
	setDc254(t, true)
	dc254Log.drop.Store(0)
	m := &mConn{}
	now := time.Now()
	m.dc.rxAt = now.Add(-4 * time.Second)
	m.dc254SilenceLocked(nil, nil, now, 0, 0)
	if !m.dc.silent {
		t.Fatal("no episode")
	}
	g0, mx0 := dc254Stat.silentGone.Load(), dc254Stat.silentMax.Load()
	m.dc254LeaveLocked()
	if m.dc.silent || dc254Stat.silentGone.Load()-g0 != 1 {
		t.Fatal("leave did not close the episode as gone")
	}
	// the call comes back with a fresh receive: nothing more is counted
	m.dc.rxAt = now.Add(time.Second)
	m.dc254SilenceLocked(nil, nil, now.Add(time.Second), 0, 0)
	if dc254Stat.silentMax.Load() != mx0 || dc254Stat.silentGone.Load()-g0 != 1 {
		t.Fatal("a gone episode reached the max")
	}
}

// b264 audit: the first full ul257 sample counts only the writer's blocked
// time inside its own window.
func TestB264UL257SampleStart(t *testing.T) {
	m := &mConn{}
	m.rawW.Store(true)
	m.rawWBlk.Store(int64(5 * time.Second)) // blocked before the first sample
	now := time.Now()
	m.ul257SampleStartLocked(now)
	if d := m.rawBlockedLocked(now); d != 0 {
		t.Fatalf("blocked before the window counted: %v", d)
	}
}

// b264 M3: dc254's txDark switch and its no-candidate records rescue run on
// call visits only; a cs226 lite visit keeps rel69i's DARK-only rule (a
// txDark primary of a non-call conn is neither switched nor rescued), while a
// call visit on the same conn still moves it.
func TestB264Dc254LiteNoTxDark(t *testing.T) {
	setDarkcall(t)
	setDc254(t, true)
	for _, side := range []string{"exit", "iran"} {
		t.Run(side, func(t *testing.T) {
			_, iran, exit := darkswPairDC(t)
			m := exit
			if side == "iran" {
				m = iran
			}
			f := newDcFake()
			darkcallReadSet(f.read)
			darkcallMarksSet(func(*msub) (uint32, uint32, bool) { return 0, 0, true })
			m.mu.Lock()
			p := m.primary.Load()
			m.mu.Unlock()
			f.set(p, "lossy") // no ack, backoff 3, rx fresh: txDark, not dark
			sw0, tds0 := darkcallStat.switched.Load(), dc254Stat.txDarkSw.Load()
			lsw0, lrr0, lnc0, nc0 := cs226Stat.sw.Load(), cs226Stat.rrec.Load(), cs226Stat.noCand.Load(), darkcallStat.noCand.Load()
			sawTx := false
			for i := 0; i < 3; i++ {
				m.mu.Lock()
				m.callLikeAt, m.callOn = time.Time{}, false
				rr := m.darkcallLiteLocked(time.Now(), ctrlNow(), true)
				sawTx = sawTx || p.dc.txDark
				m.mu.Unlock()
				if rr {
					t.Fatal("lite visit started a records rescue on a txDark primary")
				}
				time.Sleep(darkcallAfter() * 3 / 2)
			}
			if !sawTx {
				t.Fatal("primary never txDark (test setup)")
			}
			m.mu.Lock()
			np := m.primary.Load()
			m.mu.Unlock()
			if np != p || darkcallStat.switched.Load() != sw0 || dc254Stat.txDarkSw.Load() != tds0 ||
				cs226Stat.sw.Load() != lsw0 || cs226Stat.rrec.Load() != lrr0 || cs226Stat.noCand.Load() != lnc0 ||
				darkcallStat.noCand.Load() != nc0 {
				t.Fatal("lite visit acted on a txDark primary")
			}
			// the same conn as a call: dc254 still moves it
			dcVisit(m, 3)
			m.mu.Lock()
			np = m.primary.Load()
			m.mu.Unlock()
			if np == p || dc254Stat.txDarkSw.Load() == tds0 {
				t.Fatal("call visit did not switch off the txDark primary")
			}
			closeAndWait(t, iran, exit)
		})
	}
}
