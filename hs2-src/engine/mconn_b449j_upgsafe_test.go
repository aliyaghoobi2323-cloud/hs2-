package engine

import (
	"io"
	"math/rand"
	"runtime"
	"testing"
	"time"
)

// b449j (mconn_b449j_upgsafe.go): the sub carrying the raw upgrade's
// handshake dies (lab3 round 4: every heavy-download death, rc12 and b449).
// Was TestB449iUpgradeSubDeathRepro (3/3 FAIL on 4ed68a9a).

type upgCase struct {
	safe             bool   // HS2_B449J_UPGSAFE
	fail             string // "iran": Iran's UPGRADE write fails; "exit": the exit's reply write fails
	iranOld, exitOld bool   // that side plays rc12 (no upgsafe)
	drain            bool   // both sides read (a raw side notices the first sub's end only in its reader)
}

// hsOver: the handshake finished on m (both directions records, first sub open).
func hsOver(m *mConn) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return !m.ended && !m.rawW.Load() && !m.handshaking && m.raw1 != nil && !m.raw1.gated
}

// upgDeathRun: one handshake with its first join sub killed; it reports
// whether either side ended and whether both finished the handshake.
func upgDeathRun(t *testing.T, c upgCase) (iEnded, xEnded, done bool, iran, exit *mConn) {
	t.Helper()
	setUpgSafe(t, c.safe)
	// b449k: production runs HS2_MCONN_DEADLINE_FIX=1 on both sides
	// (envdef); the bare test binary has it off. Off, the exit's
	// setReadLimit wakes a raw reader that is not running yet (nobody reads
	// the exit before upgXfer): the past deadline stays on the first sub, the
	// late reader switches at once (Bi = 0) and readLoop dies "i/o timeout"
	// holding the only copy -> errMConnLost in upgXfer (lab3 b449k: 18/40
	// FAIL with a debug line on that end: exit, sub 0, raw1, after the
	// handshake). That is the pre-fix bug b312 documents, not upgsafe.
	setDeadlineFix(t, true)
	n, iran, exit := newRawPair(t)
	iran.mu.Lock()
	iran.upgOld = c.iranOld
	iran.mu.Unlock()
	exit.mu.Lock()
	exit.upgOld = c.exitOld
	exit.mu.Unlock()
	n.hook = func(l *tLink) {
		if l.key == 1 {
			if c.fail == "iran" {
				l.iran.failW.Store(true)
			} else {
				l.exit.failW.Store(true)
			}
		}
		if l.key >= 2 && c.fail == "iran" && c.exitOld && !c.iranOld {
			// b449l: an rc12 exit survives Iran's dead UPGRADE sub only
			// when it drops that sub before the repeat UPGRADE (fresh sub)
			// starts its step 2: a death in its handshake ends it (no
			// upgsafe there, as rc12 today). Under CPU load the dead pipe's
			// EOF reached the exit after the fresh sub's UPGRADE (lab3
			// b449l: 6 FAIL in 600 next to a CPU burner, 1 in each whole
			// engine run; exit err "every sub died", handshaking=true). That
			// order is the scenario of the next subtests, not this one:
			// hold the fresh sub until the exit saw the death.
			// (Iran's reopen goroutine: no t.Fatal here; a miss shows as done=false)
			for end := time.Now().Add(2 * time.Second); liveSubs(exit) != 1 && time.Now().Before(end); {
				time.Sleep(2 * time.Millisecond)
			}
		}
	}
	if c.drain {
		go io.Copy(io.Discard, iran)
		go io.Copy(io.Discard, exit)
	}
	forceUpgrade(iran)
	mWaitFor(t, "the upgrade sub opened", 2*time.Second, func() bool { return n.link(1) != nil })
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		iran.mu.Lock()
		iEnded = iran.ended
		iran.mu.Unlock()
		exit.mu.Lock()
		xEnded = exit.ended
		exit.mu.Unlock()
		done = hsOver(iran) && hsOver(exit)
		if c.drain {
			// the relay closes a connection whose Read failed (end leaves a
			// raw first sub to its reader)
			if iEnded {
				iran.Close()
			}
			if xEnded {
				exit.Close()
			}
		}
		if (iEnded && xEnded) || done {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Logf("%+v: iran ended=%v exit ended=%v done=%v", c, iEnded, xEnded, done)
	if !done {
		// b449l: why a side ended (its end error and raw state)
		t.Logf("iran %s | exit %s", rawStateOf(iran), rawStateOf(exit))
	}
	return
}

func upgXfer(t *testing.T, iran, exit *mConn) {
	t.Helper()
	up, down := make([]byte, 200<<10), make([]byte, 400<<10)
	rand.Read(up)
	rand.Read(down)
	both(t,
		func() error { return xfer(iran, up, 7000, 0) },
		func() error { return readN(exit, up) },
		func() error { return xfer(exit, down, 9000, 0) },
		func() error { return readN(iran, down) },
	)
}

func TestB449jUpgradeSubDeathOff(t *testing.T) {
	for _, f := range []string{"iran", "exit"} {
		t.Run(f, func(t *testing.T) {
			iE, xE, done, _, _ := upgDeathRun(t, upgCase{safe: false, fail: f, drain: true})
			if !iE || !xE || done {
				t.Fatalf("key off: expected today's end (iran ended=%v exit ended=%v done=%v)", iE, xE, done)
			}
		})
	}
}

func TestB449jUpgradeSubDeathOn(t *testing.T) {
	for _, f := range []string{"iran", "exit"} {
		t.Run(f, func(t *testing.T) {
			k0, r0 := b449jStat.iranKept.Load(), b449jStat.reply.Load()
			u0 := Observed().B449jUpg
			iE, xE, done, iran, exit := upgDeathRun(t, upgCase{safe: true, fail: f})
			if iE || xE || !done {
				t.Fatalf("key on: one dead handshake sub ended the connection (iran ended=%v exit ended=%v done=%v)", iE, xE, done)
			}
			// b449k: the status JSON's totals (Observed().B449jUpg) move
			u := Observed().B449jUpg
			if u.IranKept <= u0.IranKept || u.IranDone <= u0.IranDone {
				t.Fatalf("snapshot: iran kept/done did not move: %+v -> %+v", u0, u)
			}
			// b449l: the exit's kept/done move only when it handles its dead
			// sub 1 before UPACK; under CPU load its failed writer may report
			// that death after the handshake (a plain sub death then, the
			// connection goes on: lab3 b449l 7 FAIL in 600 next to a CPU
			// burner, done=true, only Reply moved). Either order is fine;
			// the reply to the repeat and kept == done are what must hold.
			if f == "exit" && (u.Reply <= u0.Reply || u.ExitKept-u0.ExitKept != u.ExitDone-u0.ExitDone) {
				t.Fatalf("snapshot: exit reply did not move or kept != done: %+v -> %+v", u0, u)
			}
			if b449jStat.iranKept.Load() == k0 {
				t.Fatal("iranKept not counted")
			}
			if f == "exit" && b449jStat.reply.Load() == r0 {
				t.Fatal("the exit did not answer the repeated UPGRADE")
			}
			upgXfer(t, iran, exit)
		})
	}
}

// Mixed versions: a new Iran with an rc12 exit, an rc12 Iran with a new exit.
func TestB449jUpgradeSubDeathMixed(t *testing.T) {
	t.Run("newIran-oldExit-iranFail", func(t *testing.T) {
		// the old exit never saw the UPGRADE: a normal step 2 on the repeat
		iE, xE, done, iran, exit := upgDeathRun(t, upgCase{safe: true, fail: "iran", exitOld: true})
		if iE || xE || !done {
			t.Fatalf("iran ended=%v exit ended=%v done=%v", iE, xE, done)
		}
		upgXfer(t, iran, exit)
	})
	for _, c := range []upgCase{
		{safe: true, fail: "exit", exitOld: true, drain: true}, // the old exit ends (today) and closes the first sub
		{safe: true, fail: "exit", iranOld: true, drain: true}, // the old Iran ends (today) and closes the first sub
		{safe: true, fail: "iran", iranOld: true, drain: true}, // the old Iran ends; the exit was still raw
	} {
		t.Run(c.fail+map[bool]string{true: "-oldIran", false: "-oldExit"}[c.iranOld], func(t *testing.T) {
			iE, xE, done, _, _ := upgDeathRun(t, c)
			if !iE || !xE || done {
				t.Fatalf("expected both ends as today, not a hang (iran ended=%v exit ended=%v done=%v)", iE, xE, done)
			}
		})
	}
}

func setUpgSafe(t *testing.T, on bool) {
	old := b449jUpgSafe.Load()
	b449jUpgSafe.Store(on)
	t.Cleanup(func() { b449jUpgSafe.Store(old) })
}

// TestMConnRawSecondSubDies with the key: the exit can never answer (its
// writes on every new link stall), so the kept handshake ends at the wait
// bound on both sides: no hang, no leak.
func TestB449jRawSecondSubDiesBound(t *testing.T) {
	setUpgSafe(t, true)
	before := runtime.NumGoroutine()
	n, iran, exit := newRawPair(t)
	go io.Copy(io.Discard, iran)
	go io.Copy(io.Discard, exit)
	n.hook = func(l *tLink) { l.exit.stall(time.Hour) }
	forceUpgrade(iran)
	mWaitFor(t, "second open", 2*time.Second, func() bool { return n.link(1) != nil })
	time.Sleep(50 * time.Millisecond)
	n.link(1).iran.Close()
	for _, m := range []*mConn{iran, exit} {
		select {
		case <-m.GetDieCh():
		case <-time.After(upgSafeWait + 3*time.Second):
			t.Fatal("connection did not end at the wait bound")
		}
	}
	closeAndWait(t, iran, exit)
	noLeak(t, before)
}

// TestStripeOverlapSubDiesInHandshake with the key: the connection stays,
// and the handshake finishes once the exit can answer.
func TestB449jStripeOverlapSubDies(t *testing.T) {
	setUpgSafe(t, true)
	setStripeOverlap(t, true, true)
	n, iran, exit := overlapStart(t, 2)
	mWaitFor(t, "3 stripe subs", 2*time.Second, func() bool { return liveStripe(iran) >= 3 })
	n.mu.Lock()
	l := n.links[len(n.links)-1]
	n.mu.Unlock()
	l.iran.Close()
	select {
	case <-iran.GetDieCh():
		t.Fatal("a stripe sub's death in the handshake ended the connection with the key on")
	case <-time.After(500 * time.Millisecond):
	}
	exit.wmu.Unlock()
	mWaitFor(t, "handshake over", 3*time.Second, func() bool { return hsOver(iran) && hsOver(exit) })
	iran.Close()
	exit.Close()
}
