package engine

import (
	"log"
	"os"
	"sync/atomic"
)

// b462 HS2_B462_DBG=1 (lab debug only, default off; log lines only, no
// behaviour change): one line per lite conn that ends with errMConnLost or
// errMConnPeerDone from a sub's death, and per hedge sub dropped by the
// HEDGE3 heavy rule, to name the F1e arm-B end errors (which sub died, who
// closed it, one-copy bytes on it, whether the user's transfer was over).
var b462Dbg = os.Getenv("HS2_B462_DBG") == "1"

var b462DbgN atomic.Int64

const b462DbgMax = 400

// b462Snap: what the dying sub held, taken before subGone drops its queue.
type b462Snap struct {
	q1, q1B   int // queued chunks (and bytes) whose only copy is on s
	qN        int // queued chunks
	cur1      bool
	sentN     int
	hedge     bool
	first     bool
	prim      int
	retired   bool
	closeHS   bool
	closeDone bool
}

func (m *mConn) b462SnapLocked(s *msub) (b b462Snap) {
	if !b462Dbg || !m.lite {
		return
	}
	for _, c := range s.q {
		if c.seq() {
			b.qN++
			if c.copies <= 1 {
				b.q1++
				b.q1B += c.n
			}
		}
	}
	if c := s.cur; c != nil && c.seq() && c.copies <= 1 {
		b.cur1 = true
	}
	b.sentN = len(s.sent)
	b.hedge = s == m.hedgeSub.Load()
	b.first = s == m.h4First
	b.prim = -1
	if p := m.primary.Load(); p != nil {
		b.prim = p.idx
	}
	b.retired, b.closeHS, b.closeDone = s.retired, s.closeHS, s.closeDone
	return
}

func (m *mConn) b462SubEndLocked(s *msub, sn b462Snap, cause, endErr error, lost, resent bool) {
	if !b462Dbg || !m.lite || (endErr != errMConnLost && endErr != errMConnPeerDone) {
		return
	}
	if b462DbgN.Add(1) > b462DbgMax {
		return
	}
	r := &m.r
	r.mu.Lock()
	rnext, rfin, rfinOff := r.next, r.fin, r.finOff
	r.mu.Unlock()
	log.Printf("b462 dbg end: iran=%v id=%x err=%q sub=%d cause=%v hedge=%v first=%v prim=%d h3=%v heavy=%v h4made=%v lost=%v resent=%v hole=%v q=%d q1=%d q1B=%d cur1=%v sent=%d retired=%v closeHS=%v/%v subs=%d wclosed=%v closing=%v peerFin=%v sendOff=%d rnext=%d rfin=%v/%d",
		m.iran, m.id&0xffffff, endErr.Error()[:20], s.idx, cause, sn.hedge, sn.first, sn.prim, m.h3, m.h3Heavy, m.h4Made,
		lost, resent, m.holeRecentLocked(), sn.qN, sn.q1, sn.q1B, sn.cur1, sn.sentN, sn.retired, sn.closeHS, sn.closeDone,
		len(m.subs), m.wclosed, m.closing, m.peerFin, m.sendOff, rnext, rfin, rfinOff)
}

// b462DropLocked (Iran, the HEDGE3 heavy rule closes the hedge sub).
func (m *mConn) b462DropLocked(s *msub, hs bool) {
	if !b462Dbg || b462DbgN.Add(1) > b462DbgMax {
		return
	}
	sn := m.b462SnapLocked(s)
	log.Printf("b462 dbg drop: id=%x sub=%d closehs=%v q=%d q1=%d sent=%d subs=%d sendOff=%d wclosed=%v peerFin=%v",
		m.id&0xffffff, s.idx, hs, sn.qN, sn.q1, sn.sentN, len(m.subs), m.sendOff, m.wclosed, m.peerFin)
}
