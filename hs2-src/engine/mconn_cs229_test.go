package engine

import "testing"

// Older tests pin cs225/cs228 placement: MINPLACE is off unless a test turns
// it on (withCs229).
func init() { cs229MinPlaceKey.Store(false) }

func withCs229(t *testing.T, on bool) {
	t.Helper()
	old := cs229MinPlaceKey.Load()
	cs229MinPlaceKey.Store(on)
	t.Cleanup(func() { cs229MinPlaceKey.Store(old) })
}

// Lite second sub: the original pick stays when it is off the primary's
// path; on it, the re-pick lands on the other path.
func TestCs229MinPlaceLite(t *testing.T) {
	withDupPaths(t, false)
	withCs226(t, true)
	withCs228(t, false, true)
	withCs229(t, true)

	m, ls := pathLinks("a>x", "a>x", "a>x", "b>y", "b>y")
	k0, r0 := cs229Stat.kept.Load(), cs229Stat.repicked.Load()
	if got := pickSecond(t, m, ls, 0, 60); got[1]+got[2] != 0 || got[3]+got[4] != 60 {
		t.Fatalf("second sub on the primary's path: %v", got)
	}
	k, r := cs229Stat.kept.Load()-k0, cs229Stat.repicked.Load()-r0
	if k+r != 60 || r == 0 {
		t.Fatalf("kept %d repicked %d, want sum 60 and some repicks", k, r)
	}
	// slower other path: no candidate, the original pick stays
	m, ls = pathLinks("a>x", "a>x", "a>x", "b>y", "b>y")
	ls[3].mtr.rttMicros.Store(80_000)
	ls[4].mtr.rttMicros.Store(80_000)
	n0 := cs229Stat.nocand.Load()
	if got := pickSecond(t, m, ls, 0, 100); got[1]+got[2] == 0 {
		t.Fatalf("slower path forced: %v", got)
	}
	if cs229Stat.nocand.Load() == n0 {
		t.Fatal("no nocand counted")
	}
	// single path: original pick, nocand
	m, ls = pathLinks("a>x", "a>x", "a>x")
	if got := pickSecond(t, m, ls, 0, 20); got[1]+got[2] != 20 {
		t.Fatalf("single path: %v", got)
	}
	// a primary on b: a pick on a is kept as is
	m, ls = pathLinks("a>x", "a>x", "a>x", "b>y", "b>y")
	if got := pickSecond(t, m, ls, 3, 40); got[4] != 0 {
		t.Fatalf("second sub beside b: %v", got)
	}
}

// The original's no-download property holds on the re-pick: the other
// path's only link carries a download, so a same-path original stays (the
// plain pick may still choose that link itself: kept, not re-picked).
func TestCs229MinPlaceNoDownload(t *testing.T) {
	withDupPaths(t, false)
	withCs226(t, true)
	withCs228(t, false, true)
	withCs229(t, true)
	m, ls := pathLinks("a>x", "a>x", "b>y")
	m.links[2].heavy = 1
	r0, n0, k0 := cs229Stat.repicked.Load(), cs229Stat.nocand.Load(), cs229Stat.kept.Load()
	got := pickSecond(t, m, ls, 0, 30)
	if cs229Stat.repicked.Load() != r0 {
		t.Fatalf("copy re-picked behind a download: %v", got)
	}
	if n, k := cs229Stat.nocand.Load()-n0, cs229Stat.kept.Load()-k0; n != uint64(got[1]) || k != uint64(got[2]) {
		t.Fatalf("nocand %d kept %d, picks %v", n, k, got)
	}
}

// Master off: cs225 behaviour for lite (no path rule), MINPLACE untouched.
func TestCs229MasterOff(t *testing.T) {
	withDupPaths(t, false)
	withCs226(t, false)
	withCs229(t, true)
	m, ls := pathLinks("a>x", "a>x", "a>x", "b>y", "b>y")
	k0 := cs229Stat.kept.Load() + cs229Stat.repicked.Load() + cs229Stat.nocand.Load()
	pickSecond(t, m, ls, 0, 20)
	if cs229Stat.kept.Load()+cs229Stat.repicked.Load()+cs229Stat.nocand.Load() != k0 {
		t.Fatal("cs229 lite rule ran with the master off")
	}
}

// Call subs: MINPLACE replaces cs225 (also with HS2_CS225_CALLPATH=0).
func TestCs229MinPlaceCall(t *testing.T) {
	withDupPaths(t, false)
	withCs229(t, true)
	withCs225(t, false)
	m, ls := pathLinks("a>x", "a>x", "a>x", "b>y", "b>y")
	r0 := cs229Stat.callRep.Load()
	got := pickCallSecond(t, m, ls, 0, 40)
	if got[0]+got[1]+got[2] != 0 {
		t.Fatalf("call copy on the primary's path: %v", got)
	}
	if cs229Stat.callRep.Load() == r0 {
		t.Fatal("no call repick counted")
	}
}
