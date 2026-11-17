package engine

import (
	"errors"
	"testing"
	"time"
)

// rr157 (mconn_rescue_rst.go): Iran's end notice ends the exit's hold at once.

// Every sub dies and no link can be opened: Iran ends at once and its end
// notice (role 6, delivered here directly) ends the exit's hold well before
// rstGrace.
func TestRescueRstEndNoticeEndsHold(t *testing.T) {
	rstBigRing(t)
	setRst(t, true, true)
	n, iran, exit := newRstPair(t)
	n.fail.Store(true)
	sent := make(chan struct{}, 4)
	iran.mu.Lock()
	iran.rstEndFn = func() {
		sent <- struct{}{}
		exit.rstPeerEnded()
	}
	iran.mu.Unlock()
	in0, due0 := rstStat.endIn.Load(), rstStat.endDue.Load()
	t0 := time.Now()
	rstKillAll(t, n, iran, exit, randBytes(20000, 91), randBytes(20000, 92))
	mWaitFor(t, "iran end", time.Second, func() bool { return rstEnded(iran) })
	mWaitFor(t, "exit end", rstGrace, func() bool { return rstEnded(exit) })
	de := time.Since(t0)
	if de > rstGrace/2 {
		t.Fatalf("exit ended after %v (grace %v): the notice did not end it", de, rstGrace)
	}
	if len(sent) != 1 || rstStat.endIn.Load() == in0 || rstStat.endDue.Load() == due0 {
		t.Fatalf("notices %d: %s", len(sent), rescueRstLogLine())
	}
	if !errors.Is(exit.err, errMConnSubsDied) {
		t.Fatalf("exit err %v", exit.err)
	}
	t.Logf("exit ended after %v | %s", de.Round(time.Millisecond), rescueRstLogLine())
}

// No notice while a live sub carries the RST, on the exit's own reset or a
// clean close, with the key off, or on the exit side.
func TestRescueRstEndNoticeOnlyWhenDue(t *testing.T) {
	rstBigRing(t)
	setRst(t, true, true)
	_, iran, exit := newRstPair(t)
	fn := func() {}
	iran.mu.Lock()
	iran.rstEndFn = fn
	live := iran.rstEndNoticeLocked(errMConnLost)
	reset := iran.rstEndNoticeLocked(errMConnReset)
	// every sub dead: due, except for the clean or peer-made ends
	var saved []bool
	for _, s := range iran.subs {
		saved = append(saved, s.dead)
		s.dead = true
	}
	due := iran.rstEndNoticeLocked(errMConnLost)
	subsDied := iran.rstEndNoticeLocked(errMConnSubsDied)
	closed := iran.rstEndNoticeLocked(errMConnClosed)
	peerDone := iran.rstEndNoticeLocked(errMConnPeerDone)
	rescueRstOn.Store(false)
	off := iran.rstEndNoticeLocked(errMConnLost)
	rescueRstOn.Store(true)
	for i, s := range iran.subs {
		s.dead = saved[i]
	}
	iran.mu.Unlock()
	exit.mu.Lock()
	exit.rstEndFn = fn
	ex := exit.rstEndNoticeLocked(errMConnLost)
	exit.mu.Unlock()
	if live != nil || reset != nil || closed != nil || peerDone != nil || off != nil || ex != nil {
		t.Fatalf("notice not due but given: live %v reset %v closed %v peerDone %v off %v exit %v",
			live != nil, reset != nil, closed != nil, peerDone != nil, off != nil, ex != nil)
	}
	if due == nil || subsDied == nil {
		t.Fatalf("notice due but not given: lost %v subsDied %v", due != nil, subsDied != nil)
	}
	closeAndWait(t, iran, exit)
}

// rr158: with every sub dead and an unclean end, a notice goes only when the
// exit can be holding the conn (rstExitMayHoldLocked): not after the exit's
// FIN, nor for a bulk-switched, striped, raw, handshaking or rscCap-less
// conn, nor with the records rescue off. Each skip is counted (endskip).
func TestRescueRstEndNoticeOnlyWhenExitMayHold(t *testing.T) {
	rstBigRing(t)
	setRst(t, true, true)
	_, iran, exit := newRstPair(t)
	iran.mu.Lock()
	iran.rstEndFn = func() {}
	var saved []bool
	for _, s := range iran.subs {
		saved = append(saved, s.dead)
		s.dead = true
	}
	skip0, due0 := rstStat.endSkip.Load(), rstStat.endDue.Load()
	if iran.rstEndNoticeLocked(errMConnSubsDied) == nil {
		iran.mu.Unlock()
		t.Fatal("base case: notice due but not given")
	}
	cases := []struct {
		name       string
		set, reset func()
	}{
		{"peerFin", func() { iran.peerFin = true }, func() { iran.peerFin = false }},
		{"bulk", func() { iran.mode = planeBulk }, func() { iran.mode = planeLight }},
		{"striped", func() { iran.stN = 2 }, func() { iran.stN = 0 }},
		// raw: the exit writes raw (its rawW); rawClosing: rawClose cleared rawW
		{"raw", func() { iran.rawR.Store(true) }, func() { iran.rawR.Store(false) }},
		{"rawClosing", func() { iran.rawR.Store(true); iran.closing = true }, func() { iran.rawR.Store(false); iran.closing = false }},
		{"handshaking", func() { iran.handshaking = true }, func() { iran.handshaking = false }},
		{"noRscCap", func() { iran.rscCap = false }, func() { iran.rscCap = true }},
		{"recOff", func() { rescueRecOn.Store(false) }, func() { rescueRecOn.Store(true) }},
		{"rescueOff", func() { rescueOn.Store(false) }, func() { rescueOn.Store(true) }},
	}
	var given []string
	for _, c := range cases {
		c.set()
		if iran.rstEndNoticeLocked(errMConnSubsDied) != nil || iran.rstEndNoticeLocked(errMConnAborted) != nil {
			given = append(given, c.name)
		}
		c.reset()
	}
	again := iran.rstEndNoticeLocked(errMConnAborted) // all restored: due again
	for i, s := range iran.subs {
		s.dead = saved[i]
	}
	iran.mu.Unlock()
	if len(given) > 0 {
		t.Fatalf("notice given where the exit cannot hold: %v", given)
	}
	if again == nil {
		t.Fatal("restored state: notice due but not given")
	}
	if d := rstStat.endSkip.Load() - skip0; d != uint64(2*len(cases)) {
		t.Fatalf("endskip +%d, want +%d | %s", d, 2*len(cases), rescueRstLogLine())
	}
	if d := rstStat.endDue.Load() - due0; d != 2 {
		t.Fatalf("enddue +%d, want +2 | %s", d, rescueRstLogLine())
	}
	closeAndWait(t, iran, exit)
}

// The exit's role-6 handling: a held id ends, an unknown id is counted and
// leaves no slot behind, the key off ignores it.
func TestRescueRstEndNoticeTable(t *testing.T) {
	setRst(t, true, true)
	tbl := newMConnTable()
	const id = 0x157157
	mc := newMConnExit(id)
	if !tbl.create(id, mc) {
		t.Fatal("create")
	}
	rescueRstOn.Store(false)
	rstEndNoticeIn(tbl, id)
	if rstEnded(mc) {
		t.Fatal("ended with the key off")
	}
	rescueRstOn.Store(true)
	nc0, in0 := rstStat.endNoConn.Load(), rstStat.endIn.Load()
	rstEndNoticeIn(tbl, id+1)
	if rstStat.endNoConn.Load() != nc0+1 {
		t.Fatalf("unknown id not counted: %s", rescueRstLogLine())
	}
	tbl.mu.Lock()
	_, left := tbl.m[id+1]
	tbl.mu.Unlock()
	if left {
		t.Fatal("a slot was left for the unknown id")
	}
	rstEndNoticeIn(tbl, id)
	if !rstEnded(mc) || !errors.Is(mc.err, errMConnSubsDied) || rstStat.endIn.Load() != in0+1 {
		t.Fatalf("held id not ended: %v | %s", mc.err, rescueRstLogLine())
	}
	tbl.remove(id, mc)
	mc.Close()
}

// Bit12 goes with bit11 (same key) and never without it.
func TestRescueRstEndNoticeBit(t *testing.T) {
	o1, o2, o3, o4, o5 := mconnOn.Load(), mconnLite.Load(), rescueOn.Load(), rescueRecOn.Load(), rescueRstOn.Load()
	t.Cleanup(func() {
		mconnOn.Store(o1)
		mconnLite.Store(o2)
		rescueOn.Store(o3)
		rescueRecOn.Store(o4)
		rescueRstOn.Store(o5)
	})
	mconnOn.Store(true)
	mconnLite.Store(true)
	rescueOn.Store(true)
	rescueRecOn.Store(true)
	for _, on := range []bool{false, true} {
		rescueRstOn.Store(on)
		e := infoExt()
		if (e&capExtMConnRstEnd != 0) != on || (e&capExtMConnRescueRst != 0) != on {
			t.Fatalf("key %v: ext %#x", on, e)
		}
	}
	if capExtMConnRstEnd&(capExtMConnStripe|capExtMConnStripeUp|capExtMConnResend|capExtMConnOverlap|capExtMConnRescue|capExtMConnRescueRec|capExtMConnKeepStall|capExtMConnRescueRst) != 0 {
		t.Fatal("bit12 taken")
	}
}
