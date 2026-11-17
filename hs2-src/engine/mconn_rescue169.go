package engine

import (
	"os"
	"slices"
	"sync/atomic"
	"time"
)

// rs169: the rel66 audit's rescue findings under HS2_MCONN_RESCUE_RETRY
// (docs/audit-rel66.md H1, M1, M3, M5). One key, on unless set to 0, both
// sides; no wire change (an older peer sees the same records as before):
//
//   - H1 exit: a RESCUE repeated by Iran's retry (same Bi, Ri) on a newer
//     join of a connection already served here gets the same answer again
//     (with the resent [Ri, Bk) kept rescueSrvHold, and the records written
//     since Bk from the records ring), instead of a refusal (reset). When the
//     ring covers them the old join is abandoned and the new one leads.
//     An older join's RESCUE overtaken by the retry is ignored.
//   - H1 Iran: the answer of a raw rescue join a retry replaced counts (it
//     answers the same RESCUE); a second answer after the first is a no-op.
//     Records rescue: an answer on a replaced join is ignored (the retry's
//     RREC has its own answer).
//   - M1 Iran: the replaced raw join is retired (off the primary): later
//     writes go to the new join, not to the silent one.
//   - M3 Iran: no retry when no records ring covers [Bi, sendOff) (the retry
//     could only reset); a retry that cannot go on keeps a live join.
//   - H1 exit, fresh: the er94 fresh RESCUE (the join opened the conn) is
//     kept too, and the conn's records ring turned on, so its repeat is
//     answered (the rs169 lab: every retried raw RESCUE was fresh); Iran's
//     retry of a fresh RESCUE goes as role 4 as well.
//   - M5 both: a peer that read past this side's Bi/Bk (a cut raw write sent
//     whole, counted short) needs nothing resent, instead of a reset.
//
// HS2_MCONN_RESCUE_IDEM=0 restores rel66.

var rs169Fix atomic.Bool

func init() { rs169Fix.Store(os.Getenv("HS2_MCONN_RESCUE_IDEM") != "0") }

const rescueSrvHold = 60 * time.Second

var rs169Stat struct {
	repeat   atomic.Uint64 // Kharej: a repeated RESCUE answered again
	repeatSw atomic.Uint64 // of those, the old join abandoned (ring covered)
	stale    atomic.Uint64 // Kharej: an overtaken RESCUE ignored
	lateAns  atomic.Uint64 // Iran: answer of a replaced raw join taken
	dupAns   atomic.Uint64 // Iran: a second raw answer ignored
	lateRR   atomic.Uint64 // Iran: answer of a replaced records join ignored
	noRing   atomic.Uint64 // Iran: raw retry skipped, no ring covers
	overRead atomic.Uint64 // the peer read past this side's raw count (M5)
	held     atomic.Int64  // Kharej: bytes kept for a repeated RESCUE
	fresh    atomic.Uint64 // Kharej: a fresh (role 4) RESCUE's answer kept
}

func rescue169LogLine() string {
	s := &rs169Stat
	return "mconn rescue169: repeat " + u64s(s.repeat.Load()) + " repeatsw " + u64s(s.repeatSw.Load()) + " stale " + u64s(s.stale.Load()) +
		" · lateans " + u64s(s.lateAns.Load()) + " dupans " + u64s(s.dupAns.Load()) + " laterr " + u64s(s.lateRR.Load()) +
		" · fresh " + u64s(s.fresh.Load()) + " noring " + u64s(s.noRing.Load()) + " overread " + u64s(s.overRead.Load()) + " · held " + u64s(uint64(max(s.held.Load(), 0))>>10) + " KB"
}

// rescueServed (Kharej): the answer given to RESCUE(bi, ri).
type rescueServed struct {
	set     bool
	expired bool // data dropped after rescueSrvHold
	bi, ri  uint64
	bk, rk  uint64
	kind    byte
	data    []byte // [ri, bk) resent with the answer
}

// rescueServedLocked (Kharej, under mu): remember the answer.
func (m *mConn) rescueServedLocked(bi, ri, bk, rk uint64, kind byte, data []byte) {
	if !rs169Fix.Load() {
		return
	}
	m.rscSrv = rescueServed{set: true, bi: bi, ri: ri, bk: bk, rk: rk, kind: kind, data: data}
	if n := int64(len(data)); n > 0 {
		rs169Stat.held.Add(n)
		time.AfterFunc(rescueSrvHold, func() {
			m.mu.Lock()
			m.rscSrv.data = nil
			m.rscSrv.expired = true
			m.mu.Unlock()
			rs169Stat.held.Add(-n)
		})
	}
}

// rescueFreshServedLocked (Kharej, under mu): the er94 fresh RESCUE(bi, 0)
// was answered ACK(sendOff, 0) on m.rscSub (the join that opened the conn).
// rs169 lab rt: every retried raw RESCUE was a fresh one, and its repeat on
// a newer join was refused (reset) because this answer was not kept. Keep
// it, and keep what is written from here on in the records ring (rscCap) so
// a repeat can resend what rode the lost join.
func (m *mConn) rescueFreshServedLocked(bi uint64) {
	if !rs169Fix.Load() {
		return
	}
	m.rescueServedLocked(bi, 0, m.sendOff, 0, mRecRescueAck, nil)
	m.rscCap = true
	rs169Stat.fresh.Add(1)
}

// rescueRepeatLocked (Kharej, under mu, unlocks): RESCUE again on s for the
// request already served on m.rscSub.
func (m *mConn) rescueRepeatLocked(s *msub) bool {
	srv := &m.rscSrv
	old := m.rscSub
	if m.ended {
		m.mu.Unlock()
		return true
	}
	if s.idx < old.idx { // the first RESCUE, overtaken by its retry
		m.mu.Unlock()
		rs169Stat.stale.Add(1)
		rtrace("%x exit RESCUE(%d,%d) again on an older join: ignored", m.id, srv.bi, srv.ri)
		return true
	}
	if srv.kind == mRecRescueAck && srv.expired && srv.ri < srv.bk {
		m.mu.Unlock()
		rescueStat.noCover.Add(1)
		rtrace("%x exit RESCUE(%d,%d) again after the hold: no cover", m.id, srv.bi, srv.ri)
		m.end(errMConnLost, true)
		return true
	}
	s.push(rescueRec(srv.kind, srv.bk, srv.rk))
	pushResendLocked(s, srv.ri, srv.data)
	m.rkMu.Lock()
	recs, ok := m.rrRangeLocked(srv.bk, m.sendOff)
	m.rkMu.Unlock()
	var gone []*msub
	if ok {
		pushResendLocked(s, srv.bk, recs)
		if m.wclosed {
			c := getChunk(mRecFin)
			c.setOff(m.sendOff)
			s.push(c)
		}
		for _, x := range append([]*msub(nil), m.subs...) {
			if x != s && !x.bulk.Load() {
				x.retired = true
				x.rrAband = true
				m.removeSubLocked(x)
				m.abandonSentLocked(x) // fx212 (H2)
				gone = append(gone, x)
			}
		}
	}
	m.rscSub = s
	m.prefPrimary = s
	m.setPrimaryLocked()
	m.tr.ev("rescue: RESCUE(%d,%d) again on sub %d, answer (%d,%d) resent %d+%d switched=%v", srv.bi, srv.ri, s.idx, srv.bk, srv.rk, len(srv.data), len(recs), ok)
	m.sendCond.Broadcast()
	m.mu.Unlock()
	rs169Stat.repeat.Add(1)
	if ok {
		rs169Stat.repeatSw.Add(1)
	}
	rtrace("%x exit RESCUE(%d,%d) again: answered, resent %d+%d switched=%v", m.id, srv.bi, srv.ri, len(srv.data), len(recs), ok)
	for _, x := range gone {
		go x.close()
	}
	return true
}

// rescueAnswerFromLocked (Iran, under wmu and mu): an ACK/NAK read on s.
// late: s is a join a retry replaced, its answer counts; handled: nothing
// to do (an answer after the first).
func (m *mConn) rescueAnswerFromLocked(s *msub) (late, handled bool) {
	if !m.iran || !rs169Fix.Load() {
		return false, false
	}
	aband := slices.Contains(m.rscAband, s)
	if m.rscPhase == 2 && (s == m.rscSub || aband) {
		rs169Stat.dupAns.Add(1)
		rtrace("%x iran rescue answer again on sub %d: ignored", m.id, s.idx)
		return false, true
	}
	if m.rscPhase == 1 && s != m.rscSub && aband {
		rs169Stat.lateAns.Add(1)
		rtrace("%x iran rescue answer on a replaced join: taken", m.id)
		return true, false
	}
	return false, false
}

// rescueAnsRangeLocked (Iran, under wmu and mu): the ring's [rk, Bi).
func (m *mConn) rescueAnsRangeLocked(rk uint64) ([]byte, bool) {
	if rk > m.rscBi && rs169Fix.Load() { // M5: the exit read past Bi
		rs169Stat.overRead.Add(1)
		return nil, true
	}
	return m.ring.get(rk, m.rscBi)
}

// rescueLateResendLocked (Iran, under mu): a late answer read on a replaced
// join: the resend goes on the current join too (s's path is suspect).
func (m *mConn) rescueLateResendLocked(s *msub, late bool, rk uint64, data []byte) {
	if late && m.rscSub != nil && m.rscSub != s && !m.rscSub.dead && len(data) > 0 {
		pushResendLocked(m.rscSub, rk, data)
	}
}

// rrLateAnsLocked (Iran, under mu, unlocks): the answer (bk, rk) to an
// RREC a retry replaced, read on s (the join was only slow). The exit had
// [0, rk) then; while the retry waits, the snapshot taken for it covers
// [rk, Bi) too: resend it now (the retry's own answer then has nothing to
// do). Not covered, or no longer waiting: ignored.
func (m *mConn) rrLateAnsLocked(s *msub, nak bool, bk, rk uint64) bool {
	if m.rrPhase != 1 || m.rrSub == nil || rk < m.rrBi && rk < m.rrSnap0 {
		m.mu.Unlock()
		rs169Stat.lateRR.Add(1)
		rtrace("%x iran RREC late answer nak=%v (%d,%d) on a replaced join: ignored", m.id, nak, bk, rk)
		return true
	}
	var data []byte
	if rk < m.rrBi {
		data = m.rrSnap[rk-m.rrSnap0:]
	}
	for _, x := range []*msub{m.rrSub, s} {
		if x.dead {
			continue
		}
		if len(data) > 0 {
			pushResendLocked(x, rk, data)
		}
		if m.wclosed {
			c := getChunk(mRecFin)
			c.setOff(m.sendOff)
			x.push(c)
		}
	}
	rrStat.resent.Add(uint64(len(data)))
	m.rrPhase = 2
	m.rrLate = true
	m.rrWait.Store(false)
	m.rrSnap = nil
	m.tr.ev("rescue-rec: late answer nak=%v (%d,%d) on sub %d, resent %d", nak, bk, rk, s.idx, len(data))
	m.mu.Unlock()
	rs169Stat.lateAns.Add(1)
	rtrace("%x iran RREC late answer nak=%v (%d,%d) on a replaced join: resent %d", m.id, nak, bk, rk, len(data))
	return true
}

// rrCoversLocked (under mu): the records ring holds [from, to).
func (m *mConn) rrCoversLocked(from, to uint64) bool {
	if from >= to {
		return true
	}
	m.rkMu.Lock()
	defer m.rkMu.Unlock()
	return m.rk.on && to <= m.rkEnd && m.rkEnd-from <= uint64(m.rk.n)
}

// rrMergeSnapLocked (Iran, under mu, rrStart of a retry with Bi bi): the
// replaced RREC's snapshot extends the new one downwards when they meet
// (the writes since rolled the ring past the exit's Rk: rel66 lab and unit
// test TestRS169RecLateAnswer, a sure reset).
func (m *mConn) rrMergeSnapLocked(bi uint64) {
	p, p0 := m.rrSnapP, m.rrSnapP0
	m.rrSnapP = nil
	if p == nil || !rs169Fix.Load() || p0 == ^uint64(0) {
		return
	}
	end := p0 + uint64(len(p))
	switch {
	case m.rrSnap0 == ^uint64(0):
		if bi == end { // nothing written since: the old snapshot is the new one
			m.rrSnap, m.rrSnap0 = p, p0
		}
	case p0 < m.rrSnap0 && m.rrSnap0 <= end:
		k := m.rrSnap0 - p0
		m.rrSnap = append(p[:k:k], m.rrSnap...)
		m.rrSnap0 = p0
	}
}
