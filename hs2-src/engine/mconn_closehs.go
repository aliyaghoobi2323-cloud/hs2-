package engine

import (
	"os"
	"sync/atomic"
)

// Close handshake for an idle lite second sub (HS2_MCONN_CLOSE_HS=1 on both
// sides, off by default).
//
// Without it Iran closes a second sub idle both ways for mLiteIdle on its own.
// The peer decides alone what it puts on that sub, and its view of "idle" is
// a delay away from Iran's: a chunk Kharej queued there as the only copy
// (its first sub full) while Iran's close was in flight died with the sub,
// and the connection was reset (a reel stalled). 5126e11 keeps the only copy
// off a sub idle for mLiteIdle/2, which narrows the window but cannot close
// it: a copy delayed in the link's queue longer than the margin still crosses
// the close.
//
//  1. Iran (idle rule): CLOSE-REQ on the sub instead of closing it, and puts
//     nothing more on it (retired).
//  2. Kharej, reading CLOSE-REQ: puts nothing more on the sub (retired) and
//     queues CLOSE-ACK(end) after everything already queued there; end is
//     the end offset of the last byte it placed on that sub.
//  3. Iran, reading CLOSE-ACK: the sub is ordered, so every record Kharej put
//     there has been read (checked: the bytes received on it reach end).
//     Iran queues CLOSE-DONE and closes the sub once it is written. Nothing
//     is sent again.
//  4. Kharej, reading CLOSE-DONE: everything it put on the sub was received;
//     it closes its end. Its death before CLOSE-DONE is an ordinary death
//     (the only copies it held may be lost: the usual reset rule), so Kharej
//     never takes the sub as closed while its bytes may be in flight. A
//     third record is needed because the stream's EOF cannot tell Iran's
//     close from a link that went down (smux reports io.EOF for both).
//
// Iran never closes the sub without CLOSE-ACK: it stays open (and out of
// use) until the ACK, the link's death, or the end of the connection.
//
// Wire: Iran sends CLOSE-REQ only on a sub to an exit that announced
// flagMConnCloseHS in its kindInfo (the exit announces it only with the key
// on). The caps byte is full (bits 0-6 here, bit 7 taken by another branch),
// so the flag rides in the exit's flags byte, where an older edge ignores it.
// An older exit never gets the new records; an older edge never sends them.
var mconnCloseHS atomic.Bool

func init() { mconnCloseHS.Store(os.Getenv("HS2_MCONN_CLOSE_HS") == "1") }

const (
	// record types 0x08/0x09 are left to other branches (stripe, FEC)
	mRecCloseReq  = 0x0a // Iran → Kharej, 1 byte
	mRecCloseAck  = 0x0b // Kharej → Iran, kind + u64 end offset
	mRecCloseDone = 0x0c // Iran → Kharej, 1 byte

	// flagMConnCloseHS (exit, kindInfo flags): reads CLOSE-REQ.
	flagMConnCloseHS = 1 << 2
)

// mconnCloseHSSub is a sub that knows whether its exit reads CLOSE-REQ.
type mconnCloseHSSub interface{ mconnCloseHS() bool }

func subCloseHS(rw mSub) bool {
	c, ok := rw.(mconnCloseHSSub)
	return ok && c.mconnCloseHS()
}

// closeHSFlag is the flag the exit announces (0 with either key off).
func closeHSFlag() uint8 {
	if mconnOn.Load() && mconnCloseHS.Load() {
		return flagMConnCloseHS
	}
	return 0
}

// peerCloseHS: the exit on l's link announced flagMConnCloseHS.
func peerCloseHS(l Link) bool {
	mtr := linkMeterOf(l)
	if mtr == nil {
		return false
	}
	pi := mtr.peerInfo.Load()
	return pi != nil && pi.V2 && pi.Flags&flagMConnCloseHS != 0
}

// closeReqLocked (Iran, manager, under m.mu): step 1 on an idle sub s.
func (m *mConn) closeReqLocked(s *msub) {
	s.retired = true
	s.closeHS = true
	s.push(getChunk(mRecCloseReq))
	m.tr.ev("lite: CLOSE-REQ on idle second sub %d", s.idx)
}

// onCloseReq (Kharej): step 2. false: a protocol error.
func (m *mConn) onCloseReq(s *msub) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.iran {
		return false
	}
	if s.closeHS || m.ended {
		return true
	}
	s.closeHS = true
	s.retired = true
	c := getChunk(mRecCloseAck)
	c.setOff(s.txEnd)
	s.push(c)
	m.setPrimaryLocked()
	m.refreshLiteDualLocked()
	m.sendCond.Broadcast()
	m.tr.ev("lite: CLOSE-REQ on sub %d, CLOSE-ACK(%d) queued", s.idx, s.txEnd)
	return true
}

// onCloseAck (Iran): step 3. false: a protocol error.
func (m *mConn) onCloseAck(s *msub, end uint64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.iran || !s.closeHS || s.rxEnd < end {
		return false
	}
	if s.closeDone || m.ended || s.dead {
		return true
	}
	s.closeDone = true
	s.closeAfter = true // the writer closes the sub once CLOSE-DONE is out
	s.push(getChunk(mRecCloseDone))
	m.tr.ev("lite: CLOSE-ACK(%d) on sub %d, closing", end, s.idx)
	return true
}

// onCloseDone (Kharej): step 4. false: a protocol error.
func (m *mConn) onCloseDone(s *msub) bool {
	m.mu.Lock()
	if m.iran || !s.closeHS {
		m.mu.Unlock()
		return false
	}
	s.closeDone = true
	m.tr.ev("lite: CLOSE-DONE on sub %d", s.idx)
	m.mu.Unlock()
	s.close()
	return true
}

// closedCleanLocked: s is retired and its death costs nothing — except a
// Kharej sub in the close handshake before CLOSE-DONE (step 4).
func (m *mConn) closedCleanLocked(s *msub) bool {
	return s.retired && (m.iran || !s.closeHS || s.closeDone)
}

// onlyClosingLocked: every sub left is in the close handshake (no sub to
// send on): the connection has no live sub.
func (m *mConn) onlyClosingLocked() bool {
	if len(m.subs) == 0 {
		return false
	}
	for _, s := range m.subs {
		if !s.closeHS {
			return false
		}
	}
	return true
}
