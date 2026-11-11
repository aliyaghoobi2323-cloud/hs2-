package engine

import (
	"testing"
	"time"
)

func withRescueCallFree(t *testing.T) {
	t.Helper()
	withRescue(t, true)
	lbRescueCallFree = true
	t.Cleanup(func() {
		lbRescueCallFree = false
		rescueCallLinks.mu.Lock()
		rescueCallLinks.at = nil
		rescueCallLinks.mu.Unlock()
	})
}

// A rescue copy (the one sub on a lowered link) carries the mark; a call's
// own upgrade, a sub on a good link, or the key off do not.
func TestRescueAvoidMark(t *testing.T) {
	m := &mConn{}
	rescueMark(nil, []any{1})
	if got := m.rescueAvoidLocked([]any{1}, 1); len(got) != 1 {
		t.Fatal("mark with the key off")
	}
	withRescueCallFree(t)
	rescueMark(nil, []any{1})
	got := m.rescueAvoidLocked([]any{1}, 1)
	rest, ok := takeRescueMark(got)
	if !ok || len(rest) != 1 || rest[0] != 1 {
		t.Fatalf("mark: %v -> %v %v", got, rest, ok)
	}
	if got := m.rescueAvoidLocked(nil, 2); len(got) != 0 {
		t.Fatal("mark for a sub on a good link")
	}
	m.callUp = true
	if got := m.rescueAvoidLocked(nil, 1); len(got) != 0 {
		t.Fatal("mark for a call's own upgrade")
	}
	if _, ok := takeRescueMark([]any{1, callAvoidMark{}}); ok {
		t.Fatal("found a mark that is not there")
	}
}

// Iran marks the links of a call-like connection; the mark expires.
func TestRescueCallMark(t *testing.T) {
	withRescueCallFree(t)
	a, b := newMCLink(0), newMCLink(0)
	m := &mConn{iran: true}
	m.subs = []*msub{{key: Link(a)}, {key: Link(b), retired: true}}
	now := time.Now()
	m.rescueCallMarkLocked(now, false)
	if rescueCallLinkOn(a.mtr, now) {
		t.Fatal("marked a connection that is not call-like")
	}
	m.rescueCallMarkLocked(now, true)
	if !rescueCallLinkOn(a.mtr, now) || rescueCallLinkOn(b.mtr, now) {
		t.Fatal("call link not marked, or a retired sub's link marked")
	}
	if rescueCallLinkOn(a.mtr, now.Add(callFreeTTL+time.Second)) {
		t.Fatal("mark did not expire")
	}
	x := &mConn{iran: false, callOn: true}
	x.subs = []*msub{{key: Link(b)}}
	x.rescueCallMarkLocked(now, true)
	if rescueCallLinkOn(b.mtr, now) {
		t.Fatal("exit side marked")
	}
}

// A rescue copy goes to a light link with no call mark and not lowered; with
// none such, the usual pick (never an avoided link).
func TestRescuePickOffCallLinks(t *testing.T) {
	withRescueCallFree(t)
	m, ls := pickLinks([]int{0, 0, 0, 0}, []bool{false, false, false, false})
	rescueMark(nil, []any{Link(ls[0])}) // the copy's own link: lowered
	call := &mConn{iran: true, subs: []*msub{{key: Link(ls[1])}, {key: Link(ls[2])}}}
	call.rescueCallMarkLocked(time.Now(), true)
	for i := 0; i < 3; i++ {
		l, rel, ok := m.pickPlaneCall(planeLight, []any{Link(ls[0]), rescueAvoidMark{}})
		if !ok || linkIndex(ls, l) != 3 {
			t.Fatalf("rescue copy on link %d, want 3", linkIndex(ls, l))
		}
		rel()
	}
	// link 3 avoided too: the usual pick, still avoiding 0 and 3
	l, rel, ok := m.pickPlaneCall(planeLight, []any{Link(ls[0]), Link(ls[3]), rescueAvoidMark{}})
	if !ok || (linkIndex(ls, l) != 1 && linkIndex(ls, l) != 2) {
		t.Fatalf("fallback on link %d", linkIndex(ls, l))
	}
	rel()
}
