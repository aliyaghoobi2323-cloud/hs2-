package engine

import (
	"testing"
	"time"
)

func setHedge3(t *testing.T, on bool) {
	o3, o2 := liteHedge3, liteHedge
	liteHedge3, liteHedge = on, on
	t.Cleanup(func() { liteHedge3, liteHedge = o3, o2 })
}

// The hedge open carries the mark (role 7 at the exit), and the open drops
// it from the avoid list.
func TestB459HedgeMark(t *testing.T) {
	setHedge3(t, true)
	n, iran, _ := newHedge2Pair(t, nil)
	mWaitFor(t, "two subs", time.Second, func() bool { return liveSubs(iran) == 2 })
	found := false
	for _, a := range n.link(1).avoid {
		if _, ok := a.(hedgeMark); ok {
			found = true
		}
	}
	if !found {
		t.Fatal("hedge open without the mark")
	}
	av, ok := takeHedgeMark([]any{1, hedgeMark{}, 2})
	if !ok || len(av) != 2 {
		t.Fatalf("takeHedgeMark %v %v", av, ok)
	}
	iran.mu.Lock()
	h3 := iran.h3
	iran.mu.Unlock()
	if !h3 {
		t.Fatal("iran conn not marked hedged")
	}
}

// Exit: whichever of first/hedge comes second joins the conn the other
// created; with the key off nothing joins.
func TestB459ExitFirstOrJoin(t *testing.T) {
	setHedge3(t, true)
	old := mconnJoinWait
	mconnJoinWait = 200 * time.Millisecond
	t.Cleanup(func() { mconnJoinWait = old })
	tbl := newMConnTable()
	mc := newMConn(41, false, nil)
	t.Cleanup(func() { mc.Close() })
	if !tbl.reserve(41, mc) {
		t.Fatal("reserve")
	}
	tbl.publish(41, mc)
	if got := exitHedge3Join(tbl, 41, mconnRoleFirst, nil); got != mc {
		t.Fatal("first did not join the hedge-created conn")
	}
	if got := exitHedge3Join(tbl, 41, mconnRoleHedge, nil); got != mc {
		t.Fatal("hedge did not join")
	}
	if got := exitHedge3Join(tbl, 41, mconnRoleFirstRaw, nil); got != nil {
		t.Fatal("raw first joined")
	}
	liteHedge3 = false
	if got := exitHedge3Join(tbl, 41, mconnRoleHedge, nil); got != nil {
		t.Fatal("joined with the key off")
	}
}

// Heavy: a download past hedge3Bytes goes one copy from the exit, and Iran
// drops the hedge sub and opens no new second sub.
func TestB459HeavyDropsHedge(t *testing.T) {
	setHedge3(t, true)
	_, iran, exit := newHedge2Pair(t, nil)
	mWaitFor(t, "two subs", time.Second, func() bool { return liveSubs(iran) == 2 })
	exit.exitHedge3Mark(false)
	drop0 := hedge3Stat.dropped.Load()
	resp := randBytes(4*int(hedge3Bytes), 12)
	both(t, func() error { return xfer(exit, resp, 4000, 0) }, func() error { return readN(iran, resp) })
	exit.mu.Lock()
	heavy := exit.h3Heavy
	exit.mu.Unlock()
	if !heavy {
		t.Fatal("exit not heavy past hedge3Bytes")
	}
	iran.kickMgr()
	mWaitFor(t, "hedge dropped", 3*time.Second, func() bool { return liveSubs(iran) == 1 })
	if hedge3Stat.dropped.Load() == drop0 {
		t.Fatal("drop not counted")
	}
	time.Sleep(200 * time.Millisecond)
	if liveSubs(iran) != 1 {
		t.Fatal("second sub reopened on a heavy hedged conn")
	}
	more := randBytes(3000, 13)
	both(t, func() error { return xfer(exit, more, 1000, 0) }, func() error { return readN(iran, more) })
}

// A light hedged exchange (under hedge3Bytes) stays doubled.
func TestB459LightStaysDual(t *testing.T) {
	setHedge3(t, true)
	_, iran, exit := newHedge2Pair(t, nil)
	mWaitFor(t, "two subs", time.Second, func() bool { return liveSubs(iran) == 2 })
	exit.exitHedge3Mark(false)
	d0 := hedge3Stat.dupB.Load()
	resp := randBytes(4000, 14)
	both(t, func() error { return xfer(exit, resp, 1000, 0) }, func() error { return readN(iran, resp) })
	if hedge3Stat.dupB.Load() == d0 {
		t.Fatal("light response not duplicated")
	}
	iran.mu.Lock()
	h := iran.h3Heavy
	iran.mu.Unlock()
	if h || liveSubs(iran) != 2 {
		t.Fatalf("light conn heavy=%v subs=%d", h, liveSubs(iran))
	}
}
