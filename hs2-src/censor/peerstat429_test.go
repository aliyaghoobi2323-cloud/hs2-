package censor

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// b429: the abort-report path counters move with the events the Kharej
// evaluator sees (report signal, held resets, a report claiming some, the
// rest released, an orphan with evidence, a credit claimed by a later
// reset), reach the status block and its rendering, and the periodic line
// comes at most every PeerLogEvery and only on change.
func TestPeerStatCounters(t *testing.T) {
	before := *peerStatusOrZero()
	s := newSim(t, SideKharej)
	s.quiet(3)
	rst := Death{Class: DeathRST, LifeMs: 120000, Bytes: 300 << 20, HaveTCP: true, LastSentMs: 3, MinRTTUs: 44000}
	s.ev(event{kind: evPeerAbort, remote: ipA}) // the edge reports
	for k := 0; k < 3; k++ {
		s.death(ipA, ipK1, rst) // held for a report
	}
	s.ev(event{kind: evPeerAbort, remote: ipA, n: 2}) // claims 2 held
	s.death(ipA, ipK1, rst)                           // held again
	s.now = s.now.Add(peerHold + time.Second)
	s.tick()                                          // releases the 2 unclaimed
	s.ev(event{kind: evPeerOrphan, remote: ipA})      // recent rise: long hold
	s.ev(event{kind: evPeerAbort, remote: ipA, n: 1}) // nothing held: a credit
	s.death(ipA, ipK1, rst)                           // claimed by the credit
	got := *peerStatusOrZero()
	want := PeerStatus{Reports: 2, RepAborts: 3, Credited: 3, Orphans: 1, OrphanHolds: 1, Held: 4, Released: 2, EdgeSigs: 1}
	d := PeerStatus{Reports: got.Reports - before.Reports, RepAborts: got.RepAborts - before.RepAborts,
		Credited: got.Credited - before.Credited, Orphans: got.Orphans - before.Orphans,
		OrphanHolds: got.OrphanHolds - before.OrphanHolds, Held: got.Held - before.Held,
		Released: got.Released - before.Released, EdgeSigs: got.EdgeSigs - before.EdgeSigs}
	if d != want {
		t.Fatalf("counter deltas %+v want %+v", d, want)
	}

	// Iran side counters (fed by the engine)
	NoteOwnAbort()
	NoteOwnAbortsReported(1)

	setStatus(s.e)
	var st Status
	if err := json.Unmarshal(StatusJSON(), &st); err != nil || st.Peer == nil {
		t.Fatalf("status peer block: %v %s", err, StatusJSON())
	}
	if st.Peer.Held < want.Held || st.Peer.OwnAborts == 0 || st.Peer.OwnReported == 0 {
		t.Fatalf("status peer block %+v", *st.Peer)
	}
	r := Render(StatusJSON())
	if !strings.Contains(r, "peer abort reports (kharej):") || !strings.Contains(r, "own aborts (iran):") {
		t.Fatalf("render:\n%s", r)
	}

	// periodic line
	was := active.Load()
	active.Store(true)
	defer active.Store(was)
	peerLog.at.Store(0)
	peerLog.last.Store(nil)
	t0 := time.Unix(5000, 0)
	l := PeerLogLine(t0)
	if !strings.HasPrefix(l, "censor351: peer kharej: reports=") || !strings.Contains(l, "own_reported=") {
		t.Fatalf("line %q", l)
	}
	if l := PeerLogLine(t0.Add(time.Minute)); l != "" {
		t.Fatalf("not due yet: %q", l)
	}
	if l := PeerLogLine(t0.Add(PeerLogEvery + time.Second)); l != "" {
		t.Fatalf("unchanged counters: %q", l)
	}
	NoteOwnAbort()
	if l := PeerLogLine(t0.Add(PeerLogEvery + 2*time.Second)); l == "" {
		t.Fatal("changed counters gave no line")
	}
}

func peerStatusOrZero() *PeerStatus {
	if p := peerStatus(); p != nil {
		return p
	}
	return &PeerStatus{}
}
