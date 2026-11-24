package engine

import (
	"sync"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/censor"
)

// b437 Q2: ZDU generation changes for the censor detector (package censor).
// A ZDU is not a restart: the other server's new process announces a newer
// instance on its links (peer generation start), and its old process later
// closes the links it drained (peer generation end, censorDeath on a
// peerOld link). This process's own handover is told by the takeover
// (CensorOwnHandover). With the detector off every call is one atomic load.

// Test seams (b437): the detector's hooks.
var (
	censorActive      = censor.Active
	censorNoteDeath   = censor.NoteDeath
	censorNotePeerOld = censor.NotePeerOldDeath
	censorNotePeerGen = censor.NotePeerGeneration
)

// censorPeerGen: the newest peer instance any pool of this process saw.
var censorPeerGen struct {
	mu  sync.Mutex
	cur zduInst
}

// censorPeerInst records the newest peer instance a pool computed (Iran:
// zduRefreshLocked; Kharej exit: zduExitMark). The first one known is the
// baseline; a newer one after it is a peer generation change.
func censorPeerInst(cur zduInst) {
	if !censorActive() || !cur.known() {
		return
	}
	g := &censorPeerGen
	g.mu.Lock()
	change := g.cur.known() && cur.newer(g.cur)
	if !g.cur.known() || cur.newer(g.cur) {
		g.cur = cur
	}
	g.mu.Unlock()
	if change {
		censorNotePeerGen()
	}
}

// CensorOwnHandover tells the detector about this process's own ZDU
// takeover (done false: adopted from the old process; true: the old
// process drains, so our probation passed).
func CensorOwnHandover(done bool) { censor.NoteOwnHandover(done) }
