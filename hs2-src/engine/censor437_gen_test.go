package engine

import (
	"io"
	"testing"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/censor"
)

// b437 Q2: the real ZDU signals reach the detector. A pool's newest peer
// instance (zduRefreshLocked, from the links' own kindInfo announcements)
// rising is a peer generation change; a death on a link its pool marked
// peerOld is that generation ending, not a death.
func censor437Seams(t *testing.T) (gens, olds, deaths *int) {
	t.Helper()
	var g, o, d int
	a, nd, no, ng := censorActive, censorNoteDeath, censorNotePeerOld, censorNotePeerGen
	censorPeerGen.mu.Lock()
	cur := censorPeerGen.cur
	censorPeerGen.cur = zduInst{}
	censorPeerGen.mu.Unlock()
	censorActive = func() bool { return true }
	censorNoteDeath = func(censor.Death) { d++ }
	censorNotePeerOld = func(censor.Death) { o++ }
	censorNotePeerGen = func() { g++ }
	t.Cleanup(func() {
		censorActive, censorNoteDeath, censorNotePeerOld, censorNotePeerGen = a, nd, no, ng
		censorPeerGen.mu.Lock()
		censorPeerGen.cur = cur
		censorPeerGen.mu.Unlock()
	})
	return &g, &o, &d
}

func TestCensor437PeerGenerationSignals(t *testing.T) {
	setZDU(t, true)
	zduResetGauge(t)
	gens, olds, deaths := censor437Seams(t)
	// before the handover: only the old Kharej process's links
	m0, _ := zduPool(3, 0)
	zduRefresh(m0)
	if *gens != 0 {
		t.Fatalf("the first instance seen is a baseline, got %d generation events", *gens)
	}
	// the new process's links announce a newer instance
	m, ls := zduPool(3, 2)
	zduRefresh(m)
	zduRefresh(m) // again: the same instance is no new change
	if *gens != 1 {
		t.Fatalf("generation events %d, want 1", *gens)
	}
	// the old process's drain end: its links die
	for _, l := range ls {
		censorDeath(&watchConn{meter: l.mtr}, io.EOF)
	}
	if *olds != 3 || *deaths != 2 {
		t.Fatalf("peerOld ends %d deaths %d, want 3 and 2", *olds, *deaths)
	}
	// an older instance (a link of the old process seen late) is no change
	censorPeerInst(zduInstOld)
	if *gens != 1 {
		t.Fatalf("an older instance counted as a change (%d)", *gens)
	}
}

// Key off: no marks, every death is a plain death, no generation event.
func TestCensor437ZDUOffPlainDeaths(t *testing.T) {
	setZDU(t, false)
	zduResetGauge(t)
	gens, olds, deaths := censor437Seams(t)
	m, ls := zduPool(3, 2)
	zduRefresh(m)
	for _, l := range ls {
		censorDeath(&watchConn{meter: l.mtr}, io.EOF)
	}
	if *gens != 0 || *olds != 0 || *deaths != 5 {
		t.Fatalf("gens %d olds %d deaths %d, want 0 0 5", *gens, *olds, *deaths)
	}
}
