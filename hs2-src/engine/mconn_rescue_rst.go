package engine

import (
	"encoding/binary"
	"fmt"
	"sync/atomic"
	"time"
)

// rr155: RESCUE of a records light connection whose sub died uncleanly (RST
// or EOF of its link) while another sub of it is still healthy
// (HS2_MCONN_RESCUE_RST, default off; both sides).
//
// Before: a sub that dies holding the only copy of bytes in flight
// (subGone: lost, or every live sub missed a copy lately) ends the whole
// connection with errMConnLost and an RST, even when a healthy sub on
// another IP is left (lab cd152: 3-4 of 30 calls died per RST/flap event).
// The records rescue (er97) never sees it: it starts only on path silence.
//
// Now, with the key on and the peer announcing capExtMConnRescueRst, the side
// that sees such a death (either side, both may) does not end. It sends
// RRST(B = its sendOff, R = its receive offset) on a live sub and waits at
// most rstGrace for the answer:
//
//   - the peer resends [R, Bk) from its records ring (er97's ring, sized to
//     3 s of the conn's write rate since cd152) as DATA records on the sub
//     the RRST came on, preceded by RRST-ACK(Bk, Rk), and a FIN it already
//     sent again; its ring short of R ends the connection there (RST: the
//     old outcome);
//   - on the ACK this side resends [Rk, its sendOff) from its own ring (and a
//     FIN already sent); short of Rk: the connection ends (the old outcome);
//   - no ACK within rstGrace: the connection ends (the old outcome, late by
//     at most rstGrace). A sub dying while the answer is awaited asks again
//     once the answer is in (its bytes may postdate the first ask); the
//     ask's own sub dying re-sends the ask on another live sub.
//
// Reassembly drops what it already holds (by offset): nothing is delivered
// twice. Both sides asking at once is harmless (each answers, each resends).
// A connection asks at most rstMaxRounds times. A peer without the bit (or
// with the key off) never receives 0x13/0x14 and nothing changes.
//
// rr156 (same key, same bit):
//
//   - EVERY sub died at once (one Iran IP reset): no live sub carries the
//     RRST. Iran opens a sub of role 5 (mconnRoleRstJoin) on a fresh link
//     (off the dead subs' links and, with JOINPICK, their paths) and sends
//     the RRST on it; the exit, with no sub left, holds the connection for
//     rstGrace instead of ending (the RRST arriving ends the hold). No link
//     to open, or no answer within rstGrace: the old end (errMConnSubsDied).
//     A conn opens at most rstMaxJoins such subs per round.
//   - the peer no longer has the connection: an exit reading role 5 for an
//     id it does not hold answers with an RST record (0x05, every version
//     reads it) and closes, so Iran ends at once instead of after rstGrace.
//     Iran opens that role-5 probe also when the RRST on a live sub is not
//     answered within rstProbe (the live sub may be a zombie whose exit end
//     is gone: rr155 lab B2, 49 timeouts, "exit join missed (no conn)").
//     Role 5 goes only to a link whose exit announces the bit.
//
// rr157 (same key): the exit's hold was mostly useless (rr156 lab: 951 holds,
// 744 expired): Iran's side of those conns had already ended with no live sub
// to carry its RST or FIN, so the hold only delayed the end by rstGrace. Now
// Iran, ending a conn uncleanly with no live sub, sends an END NOTICE: a
// role-6 sub (mconnRoleRstEnd, header only) on any live link whose exit
// announces ext bit12 (capExtMConnRstEnd); the exit ends that conn at once
// (held or not: its Iran side is gone) and closes the sub. An exit without
// bit12 never gets role 6 (it keeps rr156's hold); a notice for an id the
// exit does not hold is dropped. Notices are best effort (a bounded number in
// flight, at most three links tried): a lost one leaves the old 3 s hold.

const (
	mRecRstRsc    = 0x13 // either side: kind + u64 B (sendOff) + u64 R (receive offset)
	mRecRstRscAck = 0x14 // the answer: kind + u64 Bk + u64 Rk

	// capExtMConnRescueRst: ext bit11 of kindInfo: this server answers RRST
	// and keeps records rings (HS2_MCONN_RESCUE_RST with HS2_MCONN_RESCUE).
	capExtMConnRescueRst = 1 << 11

	rstMaxRounds = 8

	// mconnRoleRstJoin (rr156): a light join carrying an RRST; an exit without
	// the connection answers it with an RST record. Sent only to an exit
	// announcing capExtMConnRescueRst.
	mconnRoleRstJoin byte = 5

	rstMaxJoins = 3 // role-5 subs per round

	// mconnRoleRstEnd (rr157): Iran's end notice for a conn it ended with no
	// live sub. Sent only to an exit announcing capExtMConnRstEnd.
	mconnRoleRstEnd byte = 6

	// capExtMConnRstEnd: ext bit12 of kindInfo: this server reads role 6.
	capExtMConnRstEnd = 1 << 12

	rstEndMaxFlight = 64 // end notices in flight at once (more: dropped)
)

var rstEndFlight = make(chan struct{}, rstEndMaxFlight)

var rstProbe = time.Duration(envIntDef("HS2_MCONN_RESCUE_RST_PROBE_MS", 600)) * time.Millisecond

var rescueRstOn atomic.Bool // loadEnvKeys: HS2_MCONN_RESCUE_RST

var rstGrace = time.Duration(envIntDef("HS2_MCONN_RESCUE_RST_MS", 3000)) * time.Millisecond

var rstStat struct {
	asked    atomic.Uint64 // RRST sent (rounds)
	served   atomic.Uint64 // RRST answered with an ACK and a resend
	acked    atomic.Uint64 // ACK read, own resend queued
	saved    atomic.Uint64 // unclean sub deaths that did not end the conn (a rescue started or joined)
	noCover  atomic.Uint64 // a ring short of the gap (either side): conn ended
	timeout  atomic.Uint64 // no ACK within rstGrace: conn ended
	refused  atomic.Uint64 // RRST read with the key off (protocol error)
	noRing   atomic.Uint64 // unclean death with a live sub, but no complete ring here: old path
	noPeer   atomic.Uint64 // ... the peer does not announce the bit: old path
	maxRound atomic.Uint64 // ... rstMaxRounds used: old path
	reask    atomic.Uint64 // the ask's sub died: asked again on another
	resent   atomic.Uint64 // bytes resent (both sides)
	// rr156
	allDied  atomic.Uint64 // every sub died uncleanly: a rescue on a fresh link started (Iran) or the conn held (exit)
	hold     atomic.Uint64 // exit: held with no sub for Iran's role-5 join
	probe    atomic.Uint64 // Iran: no ACK within rstProbe: a role-5 probe join opened
	join     atomic.Uint64 // Iran: role-5 sub opened, RRST sent on it
	joinFail atomic.Uint64 // Iran: no link for the role-5 sub
	joinIn   atomic.Uint64 // exit: role-5 sub joined its conn
	nakSent  atomic.Uint64 // exit: role-5 for a conn it does not hold: RST answered
	nakRead  atomic.Uint64 // Iran: an RST read while a rescue-rst waited: ended at once
	// rr157
	holdExp   atomic.Uint64 // exit: a hold ran out (no join, no notice): ended rstGrace late
	holdRsc   atomic.Uint64 // exit: a hold ended by Iran's RRST (rescued)
	holdEnd   atomic.Uint64 // exit: a hold ended by Iran's end notice
	endDue    atomic.Uint64 // Iran: an unclean end with no live sub: a notice is due
	endSent   atomic.Uint64 // Iran: end notice sent
	endNoLink atomic.Uint64 // Iran: no link with bit12 (or the open failed): not sent
	endDrop   atomic.Uint64 // Iran: rstEndMaxFlight in flight: not sent
	endIn     atomic.Uint64 // exit: end notice for a live conn: ended it
	endNoConn atomic.Uint64 // exit: end notice for a conn it does not hold
	// rr158
	endSkip atomic.Uint64 // Iran: an unclean end with no live sub, but the exit cannot be holding: no notice
}

// rstState: per connection (embedded in rescueState, under mu).
type rstState struct {
	rstPhase  int // 0 none, 1 an RRST waits for its ACK
	rstSub    *msub
	rstAgain  bool // a sub died while waiting: ask again on the ACK
	rstRounds int
	rstGen    int
	rstTimer  *time.Timer
	rstT0     time.Time
	// rr156
	rstHold    bool  // exit: no sub left, waiting for Iran's role-5 join
	rstJoining bool  // Iran: a role-5 open is under way
	rstJoins   int   // role-5 opens this round
	rstLeft    []any // the links the round's dead subs were on (avoid)
	rstProbeT  *time.Timer
	// rr157
	rstEndFn func() // Iran: sends the end notice (serveUserMConn; tests replace it)
}

func rescueRstLogLine() string {
	s := &rstStat
	return fmt.Sprintf("mconn rescue-rst: asked %d served %d acked %d saved %d · nocover %d timeout %d refused %d · noring %d nopeer %d maxround %d reask %d · resent %d KB · alldied %d hold %d probe %d join %d joinfail %d joinin %d naksent %d nakread %d · holdexp %d holdrsc %d holdend %d · enddue %d endsent %d endnolink %d enddrop %d endskip %d endin %d endnoconn %d",
		s.asked.Load(), s.served.Load(), s.acked.Load(), s.saved.Load(), s.noCover.Load(), s.timeout.Load(), s.refused.Load(),
		s.noRing.Load(), s.noPeer.Load(), s.maxRound.Load(), s.reask.Load(), s.resent.Load()>>10,
		s.allDied.Load(), s.hold.Load(), s.probe.Load(), s.join.Load(), s.joinFail.Load(), s.joinIn.Load(), s.nakSent.Load(), s.nakRead.Load(),
		s.holdExp.Load(), s.holdRsc.Load(), s.holdEnd.Load(), s.endDue.Load(), s.endSent.Load(), s.endNoLink.Load(), s.endDrop.Load(), s.endSkip.Load(), s.endIn.Load(), s.endNoConn.Load())
}

// rstPeerOK: s's link's peer announced capExtMConnRescueRst (tests replace it).
var rstPeerOK = rstPeerOKLink

func rstPeerOKLink(s *msub) bool {
	var mtr *linkMeter
	if l, ok := s.key.(Link); ok {
		mtr = linkMeterOf(l)
	} else if e, ok := s.rw.(exitCountedStream); ok && e.c != nil {
		mtr = e.c.mtr // the exit: subs arrive without a key
	}
	if mtr == nil {
		return false
	}
	pi := mtr.peerInfo.Load()
	return pi != nil && pi.Ext&capExtMConnRescueRst != 0
}

// rstRingOKLocked (under mu): the records ring holds every byte up to sendOff
// (whether it reaches back far enough is known only from the peer's R).
func (m *mConn) rstRingOKLocked() bool {
	m.rkMu.Lock()
	defer m.rkMu.Unlock()
	return m.rk.on && m.rkEnd == m.sendOff
}

// rstTryLocked (under mu, subGone): dead died holding bytes no live sub has.
// True: a rescue runs (or was started) and the connection must not end now.
func (m *mConn) rstTryLocked(dead *msub) bool {
	if !m.rstBaseOKLocked() {
		return false
	}
	live := m.rrLiveSubsLocked()
	if len(live) == 0 {
		return m.rstAllDiedLocked(dead)
	}
	if !rstPeerOK(live[0]) {
		rstStat.noPeer.Add(1)
		return false
	}
	if m.rstPhase == 1 {
		m.rstNoteLeftLocked(dead)
		if dead == m.rstSub {
			rstStat.reask.Add(1)
			m.rstAskLocked(live) // the ask went down with its sub
		} else {
			m.rstAgain = true
		}
		rstStat.saved.Add(1)
		return true
	}
	if !m.rstBeginLocked() {
		return false
	}
	m.rstNoteLeftLocked(dead)
	m.rstAskLocked(live)
	if m.iran && rstProbe > 0 && rstProbe < rstGrace {
		gen := m.rstGen
		if m.rstProbeT != nil {
			m.rstProbeT.Stop()
		}
		m.rstProbeT = time.AfterFunc(rstProbe, func() { m.rstProbeFire(gen) })
	}
	rstStat.saved.Add(1)
	return true
}

// rstBaseOKLocked (under mu): the key and the connection allow a rescue-rst.
func (m *mConn) rstBaseOKLocked() bool {
	return rescueRstOn.Load() && rescueOn.Load() && rescueRecOn.Load() && m.rscCap && m.mode == planeLight && m.stN == 0 &&
		!m.ended && !m.closing && !m.handshaking && !m.rawW.Load()
}

// rstBeginLocked (under mu): a new round (phase 1, the grace timer).
func (m *mConn) rstBeginLocked() bool {
	if m.rstRounds >= rstMaxRounds {
		rstStat.maxRound.Add(1)
		return false
	}
	if !m.rstRingOKLocked() {
		rstStat.noRing.Add(1)
		return false
	}
	m.rstRounds++
	m.rstPhase, m.rstAgain, m.rstT0 = 1, false, time.Now()
	m.rstHold, m.rstJoins, m.rstLeft = false, 0, nil
	m.rstGen++
	gen := m.rstGen
	if m.rstTimer != nil {
		m.rstTimer.Stop()
	}
	m.rstTimer = time.AfterFunc(rstGrace, func() { m.rstExpire(gen) })
	m.rrWait.Store(true) // the sweeper keeps the ring
	return true
}

func (m *mConn) rstNoteLeftLocked(dead *msub) {
	if dead != nil && dead.key != nil && len(m.rstLeft) < 16 {
		m.rstLeft = append(m.rstLeft, dead.key)
	}
}

// rstAllDiedLocked (under mu, subGone): dead was the last sub (rr156).
// Iran: a role-5 sub on a fresh link carries the RRST; the exit holds.
func (m *mConn) rstAllDiedLocked(dead *msub) bool {
	if dead == nil || !m.rstBaseOKLocked() {
		return false
	}
	if !rstPeerOK(dead) {
		rstStat.noPeer.Add(1)
		return false
	}
	if m.rstPhase != 1 && !m.rstBeginLocked() {
		return false
	}
	m.rstNoteLeftLocked(dead)
	m.rstSub = nil
	rstStat.allDied.Add(1)
	if m.iran {
		if !m.rstJoinStartLocked() {
			return false // rstMaxJoins used: the old end
		}
	} else if !m.rstHold {
		m.rstHold = true
		rstStat.hold.Add(1)
		m.tr.ev("rescue-rst: every sub died, held %v for a join", rstGrace)
		rtrace("%x rst every sub died: held iran=%v", m.id, m.iran)
	}
	rstStat.saved.Add(1)
	return true
}

// rstProbeFire: the RRST on a live sub is not answered within rstProbe.
func (m *mConn) rstProbeFire(gen int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ended || m.rstPhase != 1 || m.rstGen != gen || m.rstJoining {
		return
	}
	if m.rstJoinStartLocked() {
		rstStat.probe.Add(1)
	}
}

// rstJoinStartLocked (Iran, under mu): opens a role-5 sub (rstJoin).
func (m *mConn) rstJoinStartLocked() bool {
	if m.rstJoining {
		return true
	}
	if m.rstJoins >= rstMaxJoins {
		return false
	}
	m.rstJoins++
	m.rstJoining = true
	var avoid []any
	for _, k := range m.rstLeft {
		avoid = append(avoid, avoidKey(k))
	}
	avoid = rescueJoinAvoid(avoid, m.rstLeft, nil) // rs145 JOINPICK: a fresh link off those paths
	avoid = append(avoid, rstJoinMark{})
	rtrace("%x rst role-5 join %d (left %d)", m.id, m.rstJoins, len(m.rstLeft))
	go m.rstJoin(m.rstGen, avoid)
	return true
}

// rstJoinMark in an avoid list asks mconnEdge.open for role 5.
type rstJoinMark struct{}

func takeRstJoinMark(avoid []any) ([]any, bool) {
	for i, a := range avoid {
		if _, ok := a.(rstJoinMark); ok {
			return append(avoid[:i:i], avoid[i+1:]...), true
		}
	}
	return avoid, false
}

// rstJoin (Iran): the role-5 sub; the RRST goes on it.
func (m *mConn) rstJoin(gen int, avoid []any) {
	sub, key, err := m.open(planeLight, avoid)
	m.mu.Lock()
	m.rstJoining = false
	if err != nil {
		rstStat.joinFail.Add(1)
		rtrace("%x rst role-5 open failed err=%v", m.id, err)
		end := !m.ended && m.rstPhase == 1 && m.rstGen == gen && len(m.rrLiveSubsLocked()) == 0
		m.mu.Unlock()
		if end {
			m.end(errMConnSubsDied, false) // no link: the old outcome, now
		}
		return
	}
	if m.ended || m.closing || m.mode != planeLight || m.rstPhase != 1 || m.rstGen != gen {
		m.mu.Unlock()
		sub.Close()
		return
	}
	s := m.addSubLocked(sub, key, false)
	m.rstAskLocked([]*msub{s})
	rstStat.join.Add(1)
	m.mu.Unlock()
	m.startSub(s)
}

// rstNoteReset (readLoop): an RST record arrived; counted when it answers a
// waiting rescue-rst (the peer no longer has the conn).
func (m *mConn) rstNoteReset() {
	m.mu.Lock()
	w := m.rstPhase == 1
	m.mu.Unlock()
	if w {
		rstStat.nakRead.Add(1)
		rtrace("%x rst RST read while waiting: peer has no conn iran=%v", m.id, m.iran)
	}
}

// rstLinkOK: l's exit announces capExtMConnRescueRst (role 5 may go there).
func rstLinkOK(l Link) bool {
	mtr := linkMeterOf(l)
	if mtr == nil {
		return false
	}
	pi := mtr.peerInfo.Load()
	return pi != nil && pi.Ext&capExtMConnRescueRst != 0
}

// rstAskLocked (under mu): RRST on the live sub heard from last.
func (m *mConn) rstAskLocked(live []*msub) {
	s := live[0]
	for _, x := range live[1:] {
		if x.lastRecv.Load() > s.lastRecv.Load() {
			s = x
		}
	}
	m.r.mu.Lock()
	r := m.r.next
	m.r.mu.Unlock()
	s.push(rescueRec(mRecRstRsc, m.sendOff, r))
	m.rstSub = s
	m.sendCond.Broadcast()
	m.tr.ev("rescue-rst: RRST(%d,%d) on sub %d round %d", m.sendOff, r, s.idx, m.rstRounds)
	rstStat.asked.Add(1)
	rtrace("%x rst RRST(%d,%d) iran=%v round %d", m.id, m.sendOff, r, m.iran, m.rstRounds)
}

func (m *mConn) rstExpire(gen int) {
	m.mu.Lock()
	if m.ended || m.rstPhase != 1 || m.rstGen != gen {
		m.mu.Unlock()
		return
	}
	m.rstPhase = 0
	none := len(m.rrLiveSubsLocked()) == 0
	if m.rstHold {
		rstStat.holdExp.Add(1)
	}
	m.tr.ev("rescue-rst: no ACK in %v (no sub %v)", rstGrace, none)
	m.mu.Unlock()
	rstStat.timeout.Add(1)
	rtrace("%x rst no ACK in %v iran=%v nosub=%v", m.id, rstGrace, m.iran, none)
	if none {
		m.end(errMConnSubsDied, false) // rr156: every sub died and none came back
		return
	}
	m.end(errMConnLost, true)
}

// rstResendLocked (under mu): [from, sendOff) from the ring on s (and a FIN
// already sent). False: the ring does not hold it.
func (m *mConn) rstResendLocked(s *msub, from uint64) bool {
	to := m.sendOff
	if from > to {
		return false
	}
	m.rkMu.Lock()
	data, ok := m.rrRangeLocked(from, to)
	m.rkMu.Unlock()
	if !ok {
		return false
	}
	pushResendLocked(s, from, data)
	rstStat.resent.Add(uint64(len(data)))
	if m.wclosed {
		c := getChunk(mRecFin)
		c.setOff(m.sendOff)
		s.push(c)
	}
	m.sendCond.Broadcast()
	return true
}

// onRstRsc: the peer's RRST(b, r) on s: ACK(Bk, Rk) and resend [r, Bk).
func (m *mConn) onRstRsc(s *msub, b, r uint64) bool {
	if !rescueRstOn.Load() {
		rstStat.refused.Add(1)
		return false
	}
	m.mu.Lock()
	if m.ended {
		m.mu.Unlock()
		return true
	}
	if m.rstHold { // rr156: Iran's join came; its round serves both directions
		m.rstHold, m.rstPhase = false, 0
		rstStat.holdRsc.Add(1)
		if m.rstTimer != nil {
			m.rstTimer.Stop()
		}
		if m.rrPhase != 1 {
			m.rrWait.Store(false)
		}
		m.tr.ev("rescue-rst: hold ended by RRST after %v", time.Since(m.rstT0).Round(time.Millisecond))
	}
	m.r.mu.Lock()
	rk := m.r.next
	m.r.mu.Unlock()
	bk := m.sendOff
	s.push(rescueRec(mRecRstRscAck, bk, rk))
	if !m.rstResendLocked(s, r) {
		m.tr.ev("rescue-rst: RRST(%d,%d) not covered (sent %d)", b, r, bk)
		m.mu.Unlock()
		rstStat.noCover.Add(1)
		rtrace("%x rst RRST(%d,%d) nocover bk %d iran=%v ring %s", m.id, b, r, bk, m.iran, m.rstRingDesc())
		m.end(errMConnLost, true)
		return true
	}
	m.tr.ev("rescue-rst: served RRST(%d,%d), ACK(%d,%d), resent %d", b, r, bk, rk, bk-r)
	m.mu.Unlock()
	rstStat.served.Add(1)
	rtrace("%x rst served RRST(%d,%d) ACK(%d,%d) iran=%v", m.id, b, r, bk, rk, m.iran)
	return true
}

// onRstAck: ACK(bk, rk): resend [rk, sendOff).
func (m *mConn) onRstAck(s *msub, bk, rk uint64) bool {
	m.mu.Lock()
	if m.ended || m.rstPhase != 1 {
		m.mu.Unlock()
		return true // a late or second answer: nothing to do
	}
	m.rstPhase = 0
	if m.rstTimer != nil {
		m.rstTimer.Stop()
	}
	if m.rstProbeT != nil {
		m.rstProbeT.Stop()
	}
	if m.rrPhase != 1 {
		m.rrWait.Store(false)
	}
	if !m.rstResendLocked(s, rk) {
		m.tr.ev("rescue-rst: ACK(%d,%d) not covered (sent %d)", bk, rk, m.sendOff)
		m.mu.Unlock()
		rstStat.noCover.Add(1)
		rtrace("%x rst ACK(%d,%d) nocover iran=%v ring %s", m.id, bk, rk, m.iran, m.rstRingDesc())
		m.end(errMConnLost, true)
		return true
	}
	m.tr.ev("rescue-rst: ACK(%d,%d) after %v, resent %d", bk, rk, time.Since(m.rstT0).Round(time.Millisecond), m.sendOff-rk)
	rstStat.acked.Add(1)
	rtrace("%x rst ACK(%d,%d) after %v iran=%v", m.id, bk, rk, time.Since(m.rstT0).Round(time.Millisecond), m.iran)
	again := m.rstAgain
	m.rstAgain = false
	ask := false
	if again {
		m.rstSub = nil
		ask = m.rstTryLocked(nil)
	}
	end := again && !ask
	m.mu.Unlock()
	if end {
		// a sub died during the wait and no second ask is possible
		m.end(errMConnLost, true)
	}
	return true
}

// rstEndLocked (under mu, end).
func (m *mConn) rstEndLocked() {
	if m.rstTimer != nil {
		m.rstTimer.Stop()
		m.rstTimer = nil
	}
	if m.rstProbeT != nil {
		m.rstProbeT.Stop()
		m.rstProbeT = nil
	}
	m.rstPhase = 0
	m.rstSub = nil
	m.rstHold = false
	m.rstLeft = nil
}

// rstReadRec (readLoop): 0x13 / 0x14 with their 16 bytes in b.
func (m *mConn) rstReadRec(s *msub, kind byte, b []byte) bool {
	a, z := binary.BigEndian.Uint64(b[0:8]), binary.BigEndian.Uint64(b[8:16])
	if kind == mRecRstRsc {
		return m.onRstRsc(s, a, z)
	}
	return m.onRstAck(s, a, z)
}

// rstRingDesc: the records ring for the trace.
func (m *mConn) rstRingDesc() string {
	m.rkMu.Lock()
	defer m.rkMu.Unlock()
	return fmt.Sprintf("on=%v init=%v end=%d n=%d cap=%d", m.rk.on, m.rkInit, m.rkEnd, m.rk.n, len(m.rk.buf))
}

// ---- rr157: Iran's end notice ----------------------------------------------

// rstEndNoticeLocked (Iran, under mu, end): the conn ends uncleanly, no live
// sub carries its RST or FIN, and the exit may be holding it for rstGrace
// (rr158: rstExitMayHoldLocked): the end notice is due (the returned func,
// run off mu). err is end's error.
func (m *mConn) rstEndNoticeLocked(err error) func() {
	if !m.iran || m.rstEndFn == nil || !rescueRstOn.Load() {
		return nil
	}
	switch err {
	case errMConnReset, errMConnPeerDone, errMConnClosed:
		return nil // the exit's side ended first, or both FINs went through
	}
	for _, s := range m.subs {
		if !s.dead && !s.gated && !s.retired {
			return nil // a live sub carries the RST (or its EOF tells)
		}
	}
	if !m.rstExitMayHoldLocked() {
		rstStat.endSkip.Add(1)
		return nil
	}
	rstStat.endDue.Add(1)
	return m.rstEndFn
}

// rstExitMayHoldLocked (Iran, under mu) - rr158: the exit holds a conn only
// when its own rstAllDiedLocked passes (rstBaseOKLocked), and that needs what
// both sides share: the rescue keys, the peer's bit (rscCap), a light,
// unstriped records conn past the handshake, the exit not writing raw (its
// rawW = our rawR; our rawW is no help: rawClose clears it on a pure raw
// close, rr158 soak: 588 of 796 notices in 2 min found no conn) and the exit
// not closing (no FIN of the exit's read here). rr157 lab: 1651 of 3503 notices found no conn at
// the exit: a bulk-switched, striped or raw conn, or one past the exit's FIN,
// ends there at once, without a hold, so its notice only costs a stream.
func (m *mConn) rstExitMayHoldLocked() bool {
	return rescueOn.Load() && rescueRecOn.Load() && m.rscCap && m.mode == planeLight && m.stN == 0 &&
		!m.handshaking && !m.rawR.Load() && !m.peerFin
}

// rstPeerEnded (exit): Iran's end notice for this conn: its Iran side is gone.
func (m *mConn) rstPeerEnded() {
	m.mu.Lock()
	if m.ended {
		m.mu.Unlock()
		return
	}
	held := m.rstHold
	if held {
		rstStat.holdEnd.Add(1)
		m.tr.ev("rescue-rst: hold ended by Iran's end notice after %v", time.Since(m.rstT0).Round(time.Millisecond))
	} else {
		m.tr.ev("rescue-rst: Iran's end notice")
	}
	m.mu.Unlock()
	rstStat.endIn.Add(1)
	// no rtrace: thousands per RST event would use up the trace budget (rescueTraceMax)
	m.end(errMConnSubsDied, true)
}

// rstEndNoticeIn (exit, serveMConnSub role 6): the notice for conn id.
func rstEndNoticeIn(tbl *mconnTable, id uint64) {
	if !rescueRstOn.Load() {
		return // never sent to a server not announcing it
	}
	if mc := tbl.peek(id); mc != nil {
		mc.rstPeerEnded()
		return
	}
	rstStat.endNoConn.Add(1)
}

// rstEndLinkOK: l's exit announces capExtMConnRstEnd (role 6 may go there).
var rstEndLinkOK = func(l Link) bool {
	mtr := linkMeterOf(l)
	if mtr == nil {
		return false
	}
	pi := mtr.peerInfo.Load()
	return pi != nil && pi.Ext&capExtMConnRstEnd != 0
}

// sendEndNotice (Iran, its own goroutine): a role-6 header on a live link
// whose exit reads it; best effort.
func (e *mconnEdge) sendEndNotice() {
	select {
	case rstEndFlight <- struct{}{}:
	default:
		rstStat.endDrop.Add(1)
		return
	}
	defer func() { <-rstEndFlight }()
	avoid := e.zduAvoid(nil) // ZDU: the end notice goes to the conn's own instance (zdu_pin.go)
	for tries := 0; tries < 3; tries++ {
		l, rel, ok := e.lm.pickPlaneCall(planeLight, avoid)
		if !ok {
			break
		}
		avoid = append(avoid, avoidKey(l))
		if !rstEndLinkOK(l) {
			rel()
			continue
		}
		st, err := l.OpenStream()
		if err == nil {
			if d, ok := st.(interface{ SetWriteDeadline(time.Time) error }); ok {
				d.SetWriteDeadline(time.Now().Add(kindTimeout))
			}
			_, err = st.Write(mconnHeader(e.port, e.id, mconnRoleRstEnd))
			st.Close()
		}
		rel()
		if err == nil {
			rstStat.endSent.Add(1)
			return
		}
	}
	rstStat.endNoLink.Add(1)
}
