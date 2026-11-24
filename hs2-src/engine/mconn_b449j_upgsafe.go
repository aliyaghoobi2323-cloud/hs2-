package engine

import (
	"log"
	"sync/atomic"
	"time"
)

// b449j upgrade-safe (HS2_B449J_UPGSAFE, default on; no wire change): lab3
// round 4 found every heavy-download death (rc12 and b449) at second 5-6, in
// the raw -> records handshake (mconn_raw.go): the UPGRADE record and the
// exit's reply ride only on the new sub, and subGone ended the whole
// connection when any sub died while handshaking.
//
// With the key a non-raw sub dying in the handshake keeps the connection when
// the raw first sub lives and nothing was lost (no chunk left without a copy,
// or the copies were resent):
//   - Iran: UPGRADE(Bi) again, the SAME boundary, on another live sub or a
//     fresh one (at most upgSafeTries times); the first sub stays gated, as in
//     step 1, so the peer reads the same raw bytes either way.
//   - Kharej: waits for UPACK (Iran pushes it on every sub, the first one
//     too) or for Iran's new UPGRADE(Bi), which it answers again with the
//     same Bk on that sub (or with its first reply if that never went out).
//
// Either side ends as before (errMConnSubsDied, no RST) if the handshake is
// not over upgSafeWait after the first kept death. Mixed versions: an rc12
// exit that saw the UPGRADE ends on its own sub's death (today), closing the
// first sub, so a new Iran ends too; one that never saw it does a normal step
// 2 on the new UPGRADE. An rc12 Iran ends on its sub's death and closes the
// first sub, so a new exit ends too. An rc12 exit that ignored a second
// UPGRADE leaves a new Iran at the wait bound: today's end, later.
//
// The raw first sub dying before the boundary still ends the connection
// (readRaw): its raw bytes in flight have no copy, so no offset can be
// trusted.
var b449jUpgSafe atomic.Bool

func init() { b449jUpgSafe.Store(envBoolDefault("HS2_B449J_UPGSAFE", true)) }

const (
	upgSafeTries = 3
	upgSafeWait  = 8 * time.Second
)

var b449jStat struct {
	iranKept atomic.Uint64 // Iran: a handshake sub death kept, UPGRADE sent again
	exitKept atomic.Uint64 // Kharej: a handshake sub death kept
	reply    atomic.Uint64 // Kharej: answered a repeated UPGRADE
	ended    atomic.Uint64 // the wait bound ended a kept handshake
	refused  atomic.Uint64 // a handshake sub death not kept (lost data, raw sub dead, tries used up)
	iranDone atomic.Uint64 // Iran: a kept handshake finished
	exitDone atomic.Uint64 // Kharej: a kept handshake finished
	reopenNo atomic.Uint64 // Iran: the fresh sub for the repeated UPGRADE did not open (the conn ended)
}

// B449jUpgSnap: HS2_B449J_UPGSAFE totals since start (status JSON
// "b449j_upgsafe", b449k; before only in the log lines).
type B449jUpgSnap struct {
	IranKept, IranDone, ExitKept, ExitDone, Reply uint64
	BoundEnded, ReopenFail, Refused               uint64
}

func b449jUpgSnap() B449jUpgSnap {
	return B449jUpgSnap{IranKept: b449jStat.iranKept.Load(), IranDone: b449jStat.iranDone.Load(),
		ExitKept: b449jStat.exitKept.Load(), ExitDone: b449jStat.exitDone.Load(), Reply: b449jStat.reply.Load(),
		BoundEnded: b449jStat.ended.Load(), ReopenFail: b449jStat.reopenNo.Load(), Refused: b449jStat.refused.Load()}
}

// upgSafeOn: the key, unless a test plays an old peer on this side.
func (m *mConn) upgSafeOn() bool { return b449jUpgSafe.Load() && !m.upgOld }

// upgSafeKeepLocked (subGone, m.handshaking, s already removed): true when
// the connection survives s's death. Caller holds m.mu.
func (m *mConn) upgSafeKeepLocked(s *msub, lost, resent bool) bool {
	if !m.upgSafeOn() {
		return false
	}
	if m.raw1 == nil || s == m.raw1 || m.raw1.dead || m.closing || (lost && !resent) {
		b449jStat.refused.Add(1)
		return false
	}
	if m.iran {
		if m.hsTries >= upgSafeTries {
			b449jStat.refused.Add(1)
			return false
		}
		m.hsTries++
		b449jStat.iranKept.Add(1)
		m.upgSafeArmLocked()
		for _, t := range m.subs {
			if t != m.raw1 && !t.dead && !t.retired {
				c := getChunk(mRecUpgrade)
				c.setOff(m.hsBi)
				t.push(c)
				m.tr.ev("b449j: upgrade sub %d died, UPGRADE(%d) again on sub %d", s.idx, m.hsBi, t.idx)
				log.Printf("b449j upgsafe %x: iran kept (sub %d died), UPGRADE(%d) again on live sub %d try %d", m.id&0xffffff, s.idx, m.hsBi, t.idx, m.hsTries)
				return true
			}
		}
		log.Printf("b449j upgsafe %x: iran kept (sub %d died), UPGRADE(%d) again on a fresh sub try %d", m.id&0xffffff, s.idx, m.hsBi, m.hsTries)
		go m.upgSafeReopen()
		return true
	}
	b449jStat.exitKept.Add(1)
	m.upgSafeArmLocked()
	m.tr.ev("b449j: sub %d died in the handshake, waiting for UPACK", s.idx)
	log.Printf("b449j upgsafe %x: exit kept (sub %d died), waiting for UPACK or UPGRADE(%d)", m.id&0xffffff, s.idx, m.hsBi)
	return true
}

// upgSafeArmLocked: the wait bound of a kept handshake (armed once).
func (m *mConn) upgSafeArmLocked() {
	if m.hsKept {
		return
	}
	m.hsKept = true
	time.AfterFunc(upgSafeWait, func() {
		m.mu.Lock()
		stuck := m.handshaking && !m.ended
		m.mu.Unlock()
		if stuck {
			b449jStat.ended.Add(1)
			log.Printf("b449j upgsafe %x: handshake not over after %v, ending", m.id&0xffffff, upgSafeWait)
			m.end(errMConnSubsDied, false)
		}
	})
}

// upgSafeDoneLocked: a handshake ended normally (step 3 / 4).
func (m *mConn) upgSafeDoneLocked() {
	if m.hsKept {
		if m.iran {
			b449jStat.iranDone.Add(1)
		} else {
			b449jStat.exitDone.Add(1)
		}
		log.Printf("b449j upgsafe %x: kept handshake done (iran=%v tries=%d)", m.id&0xffffff, m.iran, m.hsTries)
	}
}

// upgSafeReopen (Iran): a fresh sub carrying UPGRADE(Bi) again.
func (m *mConn) upgSafeReopen() {
	m.mu.Lock()
	if m.ended || m.closing || !m.handshaking {
		m.mu.Unlock()
		return
	}
	p := m.hsPlane
	avoid := append([]any{avoidKey(m.raw1.key)}, m.deadAvoidLocked()...)
	m.mu.Unlock()
	sub, key, err := m.open(p, avoid)
	m.mu.Lock()
	if err != nil {
		hs := m.handshaking && !m.ended
		m.mu.Unlock()
		if hs {
			b449jStat.reopenNo.Add(1)
			log.Printf("b449j upgsafe %x: reopen failed (%v), ending", m.id&0xffffff, err)
			m.end(errMConnSubsDied, false)
		}
		return
	}
	if m.ended || m.closing || !m.handshaking {
		m.mu.Unlock()
		sub.Close()
		return
	}
	s := m.addSubLocked(sub, key, false)
	c := getChunk(mRecUpgrade)
	c.setOff(m.hsBi)
	s.push(c)
	if p == planeStripe {
		m.stripeJoinLocked(s)
	}
	m.tr.ev("b449j: UPGRADE(%d) again on fresh sub %d", m.hsBi, s.idx)
	m.mu.Unlock()
	m.startSub(s)
}

// upgSafeRepeatLocked (Kharej, onUpgrade with m.upgraded): Iran's repeated
// UPGRADE(off) on s. true: the caller must run upgradeSendExit (the first
// reply never went out). Caller holds m.mu.
func (m *mConn) upgSafeRepeatLocked(s *msub, off uint64) bool {
	if !m.upgSafeOn() || !m.handshaking || off != m.hsBi || s.dead {
		return false
	}
	b449jStat.reply.Add(1)
	if m.rawW.Load() {
		log.Printf("b449j upgsafe %x: exit got UPGRADE(%d) again on sub %d, first reply", m.id&0xffffff, off, s.idx)
		return true
	}
	c := getChunk(mRecUpgrade)
	c.setOff(m.hsBk)
	s.push(c)
	m.tr.ev("b449j: UPGRADE(%d) again, reply UPGRADE(%d) on sub %d", off, m.hsBk, s.idx)
	log.Printf("b449j upgsafe %x: exit got UPGRADE(%d) again on sub %d, reply UPGRADE(%d)", m.id&0xffffff, off, s.idx, m.hsBk)
	return false
}
