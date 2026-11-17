package engine

import (
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// cs193: HS2_MCONN_DARKCALL (both sides, each its own sending direction; no
// wire change; default on since rel68b via envdef).
//
// The remaining long-call deaths: a path goes dark (IP outage, flap), the
// call's receive stops at the onset and the call dies 10-40 s later. st167
// and cs189 found why the existing machinery misses it: the records rescue
// wants EVERY live sub silent (the exit sends one copy on its primary only,
// so a healthy second sub blocks it), the exit runs MBs into the dead link
// (smux window + kernel buffer, past what any ring holds), and cs189's
// switch looked only from the one-copy send path (an idle sender never
// looked) on weak evidence (bytes_acked frozen 1 s: true on lossy links).
//
// With the key on, a timer (darkcallEvery) visits every records connection
// that is call-classified (copied as a call, or call-like within callFreeTTL):
//   - per live sub it reads the link's TCP_INFO and the link's last receive.
//     A sub is DARK when ALL hold: bytes in flight with no ack progress for
//     darkcallAfter; repeated RTO (backoff >= 2, or >= 2 retransmits since the
//     last ack progress); and nothing at all received on the link for
//     darkcallAfter (keepalives included). A lossy-but-alive link fails the
//     last test: something arrives every RTT. darkcallAfter is max(1 s, 4x
//     the call's packet interval); call-like means >= mCallMinRate both
//     ways, so the interval term never exceeds 1 s and the floor rules.
//   - our primary DARK: it moves to a live sub on another socket AND another
//     path that is not dark, received within darkcallAfter and shows ack
//     progress (or nothing in flight); what the dark sub wrote that the peer
//     has not consumed (smux written - consumed) is resent from the records
//     ring and its queue is moved. The peer sees the same link dark (an
//     outage is both ways) and moves its own primary by the same rule.
//   - no such sub (one-sub call): Iran starts a records rescue (er97 join +
//     RREC, both sides resend from their rings) without the path-level
//     silent set; a done rescue re-arms after darkcallGap.
//   - Iran also starts it when any live sub is dark and the receive has not
//     advanced for darkcallAfter (the exit's primary may be the dark one and
//     have no candidate).
//   - in-flight cap: a call sub with more than max(darkcallCapMin, 3/4 of the
//     ring) written but not consumed by the peer takes no more chunks until
//     the next visit (Write waits): the ring then always covers the resend.
//   - at most one action per darkcallGap per connection; every action is
//     counted and darkcallCheck later its outcome (receive or ack progress).

var (
	darkcallOn    atomic.Bool // loadEnvKeys: HS2_MCONN_DARKCALL
	darkcallOnce  sync.Once
	darkcallEvery = time.Duration(envIntDef("HS2_MCONN_DARKCALL_TICK_MS", 200)) * time.Millisecond
	darkcallCheck = 2 * time.Second
	// darkcallCapMin: the in-flight cap's floor (bytes)
	darkcallCapMin = envIntDef("HS2_MCONN_DARKCALL_CAPKB", 256) << 10
	darkcallLogMax = int64(3000)
	// fx212 (an210 Q2): HS2_FX212_DARKCALL (default on) gates the in-flight
	// cap by link evidence. smux written - consumed includes bytes the peer
	// read but has not reported (AutoWindow sends an update only every
	// win/div = 256 KiB at base), so the old cap fired on healthy links,
	// capped both subs of a call at once and stalled it 1.4-3 s. With it:
	//   - a sub is capped only if its link also shows bytes in flight with
	//     no TCP ack progress for >= darkcallAfter/2;
	//   - never every live sub of a call (2+ subs): the one with the latest
	//     ack progress stays open;
	//   - a darkcall switch clears the target's cap;
	//   - the cap is at most 3/4 of the records ring (so the switch's resend
	//     from the ring covers what the capped sub left unconsumed).
	fx212Darkcall = envBoolDefault("HS2_FX212_DARKCALL", true)
)

// darkcallTrace (HS2_MCONN_DARKCALL_TRACE, cs194; loadEnvKeys): the timer
// runs even with the key off (observe only: no cap, no action) and logs one
// line per call connection per second: receive progress, primary and every
// live sub's link evidence, so both arms of an A/B read the same way.
var darkcallTrace atomic.Bool

func darkcallAfterMs(ms int) time.Duration {
	return time.Duration(min(max(ms, 300), 5000)) * time.Millisecond
}

// darkcallRunning: the timer has work (key on, or trace observe-only).
func darkcallRunning() bool { return darkcallOn.Load() || darkcallTrace.Load() }

var darkcallStat struct {
	visits   atomic.Uint64 // call conns visited (per tick)
	looks    atomic.Uint64 // sub links read
	noSock   atomic.Uint64
	dark     atomic.Uint64 // dark episodes (sub links)
	darkPrim atomic.Uint64 // ... on this side's primary
	switched atomic.Uint64
	covered  atomic.Uint64
	short    atomic.Uint64
	noCand   atomic.Uint64 // dark primary, no candidate (per action try)
	rrec     atomic.Uint64 // Iran records rescues started
	rrRx     atomic.Uint64 // ... of them by "dark sub + receive stalled"
	rrNo     atomic.Uint64 // ... not eligible
	rearm    atomic.Uint64
	okAfter  atomic.Uint64 // outcome: progress within darkcallCheck
	badAfter atomic.Uint64 // outcome: none
	capped   atomic.Uint64 // in-flight cap engaged (episodes)
	resent   atomic.Uint64
	moved    atomic.Uint64
	again    atomic.Uint64 // cs194: switched again inside the gap (target went dark)
	uncapAll atomic.Uint64 // fx212: every live sub capped: one released
	logged   atomic.Int64
}

func darkcallLogLine() string {
	s := &darkcallStat
	return fmt.Sprintf("mconn darkcall: on %v · visits %d looks %d nosock %d · dark %d prim %d · switched %d covered %d short %d nocand %d · rrec %d (rx %d) notelig %d rearm %d · ok %d bad %d · capped %d · resent %d KB moved %d KB · again %d uncapall %d · cs221 held %d (%d KB) skipq %d rxlive %d samepair %d",
		darkcallOn.Load(), s.visits.Load(), s.looks.Load(), s.noSock.Load(), s.dark.Load(), s.darkPrim.Load(),
		s.switched.Load(), s.covered.Load(), s.short.Load(), s.noCand.Load(), s.rrec.Load(), s.rrRx.Load(), s.rrNo.Load(), s.rearm.Load(),
		s.okAfter.Load(), s.badAfter.Load(), s.capped.Load(), s.resent.Load()>>10, s.moved.Load()>>10, s.again.Load(), s.uncapAll.Load(),
		cs221Stat.held.Load(), cs221Stat.heldB.Load()>>10, cs221Stat.skipQ.Load(), cs221Stat.rxLive.Load(), cs221Stat.samePair.Load()) +
		cs223LogPart() + cs224LogPart() + cs226LogPart() + dc254LogPart() + xs261LogPart() + b262LogPart() + b262RepLogPart() + b270LogPart() + b274LogPart()
}

// dcSub: a sub's link evidence (under m.mu).
type dcSub struct {
	ok      bool
	tc      *net.TCPConn
	path    string
	lastRd  int64 // ctrlNow of the link's last read (0: unknown)
	acked   uint64
	unacked uint32
	backoff uint8
	retrans uint32
	ackAt   time.Time // last ack progress (or nothing in flight)
	rtAck   uint32    // total retransmits at ackAt
	isDark  bool
	txDark  bool  // dc254: sends frozen in RTO backoff, receive ignored
	full    bool  // in-flight cap: takes no chunk (fits)
	fullAt  int64 // monoNano when full was set
}

// fullFresh: a cap older than 1 s is ignored (the timer stopped visiting).
func (d *dcSub) fullFresh() bool { return monoNano(time.Now())-d.fullAt < int64(time.Second) }

// dcConn: per connection (under m.mu).
type dcConn struct {
	last     time.Time // last action
	lastTo   *msub     // the sub the last switch made primary (nil: rescue)
	trAt     time.Time // last trace line
	trNext   uint64    // r.next at trAt
	rxNext   uint64    // r.next at rxAt
	rxAt     time.Time // last receive progress seen by the timer
	checkAt  time.Time // outcome check due (zero: none)
	chkNext  uint64
	chkTo    *msub
	chkAcked uint64
	// cs221Held: the rx rescue is held back (counted once per hold)
	cs221Held bool
	liteWalks uint32 // cs226LiteVisitLocked walks that got past its gates (audit246 L3 test)
	// dc254: silent-rx / tx-stall episode state
	silent    bool
	silentFor time.Duration
	txStall   bool
	xs        xsConn // xs261: the cross-side switch (mconn_xs261.go)
}

// darkcallRead: the link evidence of s (tests replace it).
func darkcallReadOS(s *msub) (tc *net.TCPConn, path string, lastRd int64, acked uint64, unacked uint32, backoff uint8, retrans uint32, ok bool) {
	tc = subTCP(s)
	if tc == nil {
		return
	}
	if mtr := dcSubMeter(s); mtr != nil {
		path, lastRd = mtr.path, mtr.lastRd.Load()
	}
	acked, unacked, backoff, retrans, ok = tcpDarkInfo(tc)
	return
}

// darkcallMarks: s's smux written / consumed counts (tests replace it).
func darkcallMarksOS(s *msub) (written, consumed uint32, ok bool) { return ackMarksOf(s.rw) }

// dcSubMeter: the meter of the link s rides (both sides) (nil: unknown).
func dcSubMeter(s *msub) *linkMeter {
	switch r := s.rw.(type) {
	case *mconnSub:
		if cs, ok := r.stream.(*countedStream); ok && cs.link != nil {
			return cs.link.mtr
		}
	case exitCountedStream:
		if r.c != nil {
			return r.c.mtr
		}
	}
	return nil
}

func darkcallLoop() {
	t := time.NewTicker(darkcallEvery)
	defer t.Stop()
	var last time.Time
	for range t.C {
		if !darkcallRunning() {
			last = time.Time{}
			continue
		}
		now := time.Now()
		dc254TickLate(last, now) // dc254: a late tick = the process (not a link) stalled
		last = now
		darkcallTick(now, ctrlNow())
	}
}

func darkcallTick(now time.Time, nowc int64) {
	rrConns.Range(func(k, _ any) bool {
		m := k.(*mConn)
		rr := m.darkcallVisit(now, nowc) // cs228: a lite visit's TCP_INFO reads run unlocked
		if rr {
			go m.rrStart()
		}
		return true
	})
}

func (m *mConn) dcIsCallLocked(now time.Time) bool {
	return m.callOn || !m.callLikeAt.IsZero() && now.Sub(m.callLikeAt) < callFreeTTL
}

// darkcallLocked (under m.mu): one visit; true: start a records rescue
// (lightOpening was set).
func (m *mConn) darkcallLocked(now time.Time, nowc int64) bool {
	isCall := m.dcIsCallLocked(now)
	lite := !isCall && !m.ended && !m.closing && m.cs226LiteVisitLocked(nowc) // cs226 (mconn_cs226.go)
	return m.darkcallLiteLocked(now, nowc, lite)
}

// darkcallLiteLocked: darkcallLocked with the cs226 lite verdict already
// computed under this same hold of m.mu (cs228PreLocked).
func (m *mConn) darkcallLiteLocked(now time.Time, nowc int64, lite bool) bool {
	isCall := m.dcIsCallLocked(now)
	if m.ended || m.closing || m.mode != planeLight || m.stN > 0 || !isCall && !lite {
		m.dc254LeaveLocked() // b264: a hang-up's silence is not a stall
		for _, s := range m.subs {
			if s.dc.full {
				s.dc.full = false
				m.sendCond.Broadcast()
			}
		}
		return false
	}
	if lite {
		cs226Stat.visits.Add(1)
	} else {
		darkcallStat.visits.Add(1)
	}
	d := &m.dc
	r := &m.r
	r.mu.Lock()
	next := r.next
	r.mu.Unlock()
	if next != d.rxNext || d.rxAt.IsZero() {
		d.rxNext, d.rxAt = next, now
	}
	// outcome of the last action
	if !d.checkAt.IsZero() && !now.Before(d.checkAt) {
		ok := next != d.chkNext
		if to := d.chkTo; !ok && to != nil && to.dc.ok && to.dc.acked != d.chkAcked {
			ok = true
		}
		if ok {
			darkcallStat.okAfter.Add(1)
		} else {
			darkcallStat.badAfter.Add(1)
		}
		m.dcLogf("%x darkcall outcome iran=%v progress=%v", m.id, m.iran, ok)
		d.checkAt, d.chkTo = time.Time{}, nil
	}
	live := m.rrLiveSubsLocked()
	capB := darkcallCapMin
	m.rkMu.Lock()
	if fx212Darkcall {
		capB = dcCapFor(darkcallCapMin, m.rk.on, len(m.rk.buf))
	} else if n := m.rk.n * 3 / 4; m.rk.on && n > capB {
		capB = n
	}
	m.rkMu.Unlock()
	anyDark := false
	act := darkcallOn.Load()
	for _, s := range live {
		m.dcLookLocked(s, now, nowc)
		m.dc254LookLocked(s, now)
		if !lite {
			m.b270TxResendLocked(s, now, nowc) // b270: copies stranded on a half-dead second sub
		}
		if s.dc.isDark {
			anyDark = true
		}
		if !act || lite {
			continue // cs226: the cap stays calls-only
		}
		// in-flight cap
		full := false
		if w, c, ok := darkcallMarks(s); ok && int(w-c) > capB {
			full = !fx212Darkcall || s.dc.stalled(now)
		}
		if full {
			if !s.dc.full {
				darkcallStat.capped.Add(1)
			}
			s.dc.full, s.dc.fullAt = true, monoNano(now)
		} else if s.dc.full {
			s.dc.full = false
			m.sendCond.Broadcast()
		}
	}
	if act && fx212Darkcall && !lite {
		m.dcNotAllCappedLocked(live)
	}
	if darkcallTrace.Load() && now.Sub(d.trAt) >= time.Second {
		m.dcTraceLocked(live, now, nowc, next, lite)
	}
	p := m.primary.Load()
	if !lite {
		m.dc254SilenceLocked(p, live, now, nowc, next)
		m.xsRxSilentLocked(live, now, nowc)     // xs261
		m.b262ReplaceLocked(p, live, now, nowc) // b262 replace (mconn_b262rep.go)
	} else {
		m.dc254LeaveLocked() // b264: a lite (non-call) visit
	}
	// cs194: the gap holds unless the sub the last switch went to is itself
	// dark now (a fresh dark proof on it, >= darkcallAfter after the switch):
	// ip2x's staggered outage kills the candidate 2 s after the switch and
	// the 3 s gap then added a full extra second of silence.
	again := p != nil && p == d.lastTo && p.dc.isDark
	if !act || now.Sub(d.last) < darkcallGap() && !again {
		return false
	}
	if again {
		// one bypass per switch (a no-candidate try must not lift the
		// gap every tick); counted only when it did bypass the gap
		d.lastTo = nil
		if now.Sub(d.last) < darkcallGap() {
			darkcallStat.again.Add(1)
		}
	}
	// b264 M3: dc254's txDark switch (and its no-candidate records rescue)
	// is for call visits only; a cs226 lite visit keeps rel69i's DARK-only
	// rule (a txDark primary of a non-call conn stays where it is).
	if p != nil && (p.dc.isDark || p.dc.txDark && !lite) && !p.dead && !p.retired {
		var to *msub
		for _, s := range live {
			if s == p || !s.dc.ok || s.dc.isDark || s.dc.txDark || s.dc.tc == p.dc.tc {
				continue
			}
			if s.dc.path != "" && s.dc.path == p.dc.path {
				continue // same path: likely as dark
			}
			if lite && rescueLowKey(s.key) {
				continue // cs226: a lite conn leaves a lowered path for a sound one only
			}
			if s.dc.lastRd == 0 || time.Duration(nowc-s.dc.lastRd) >= darkcallAfter() {
				continue // not heard from: no proof it lives
			}
			if s.dc.unacked > 0 && now.Sub(s.dc.ackAt) >= darkcallAfter() {
				continue
			}
			// cs194: prefer the sub sharing fewer endpoint IPs with the dark
			// primary (an outage takes an IP down: lab3 r1B moved a call
			// 10.60.4.1>10.70.1.2 -> 10.60.4.1>10.70.2.2, same Iran IP,
			// dark 0.4 s later), then the freshest receive
			if dv, tv := dcPathDiv(p.dc.path, s.dc.path), dcPathDiv(p.dc.path, toPathOf(to)); to == nil || dv > tv || dv == tv && s.dc.lastRd > to.dc.lastRd {
				to = s
			}
		}
		if to != nil {
			if lite {
				cs226Stat.sw.Add(1)
			}
			if !p.dc.isDark {
				dc254Stat.txDarkSw.Add(1)
			}
			m.dcSwitchLocked(p, to, now, next)
			if isCall {
				m.xsSendLocked(to, now, false) // xs261: the peer leaves p too
			}
			return false
		}
		darkcallStat.noCand.Add(1)
		if lite {
			cs226Stat.noCand.Add(1)
		}
		rr := m.dcRescueLocked(now, next, false)
		if rr && lite {
			cs226Stat.rrec.Add(1)
		}
		return rr
	}
	if m.iran && anyDark && now.Sub(d.rxAt) >= darkcallAfter() {
		if m.cs221LiveReadLocked(live, now) {
			if !d.cs221Held {
				d.cs221Held = true
				cs221Stat.rxLive.Add(1)
			}
			return false
		}
		d.cs221Held = false
		rr := m.dcRescueLocked(now, next, true)
		if rr && lite {
			cs226Stat.rrec.Add(1)
		}
		return rr
	}
	return false
}

func toPathOf(s *msub) string {
	if s == nil {
		return ""
	}
	return s.dc.path
}

// dcPathDiv: how many endpoint IPs of path b ("local>remote") differ from
// path a's (0-2; unknown: 0).
func dcPathDiv(a, b string) int {
	al, ar, ok1 := strings.Cut(a, ">")
	bl, br, ok2 := strings.Cut(b, ">")
	if !ok1 || !ok2 {
		return 0
	}
	n := 0
	if al != bl {
		n++
	}
	if ar != br {
		n++
	}
	return n
}

// dcLookLocked reads s's link and updates its dark state.
func (m *mConn) dcLookLocked(s *msub, now time.Time, nowc int64) {
	darkcallStat.looks.Add(1)
	tc, path, lastRd, acked, unacked, backoff, retrans, ok := m.dcReadLocked(s)
	d := &s.dc
	if !ok {
		darkcallStat.noSock.Add(1)
		*d = dcSub{full: d.full, fullAt: d.fullAt}
		return
	}
	if !d.ok || tc != d.tc || unacked == 0 || acked != d.acked || retrans < d.rtAck {
		d.ackAt, d.rtAck = now, retrans
	}
	d.ok, d.tc, d.path, d.lastRd, d.acked, d.unacked, d.backoff, d.retrans = true, tc, path, lastRd, acked, unacked, backoff, retrans
	dark := unacked > 0 && now.Sub(d.ackAt) >= darkcallAfter() &&
		(backoff >= 2 || retrans-d.rtAck >= 2) &&
		lastRd != 0 && time.Duration(nowc-lastRd) >= darkcallAfter()
	if dark && !d.isDark {
		darkcallStat.dark.Add(1)
		if s == m.primary.Load() {
			darkcallStat.darkPrim.Add(1)
		}
		m.dcLogf("%x darkcall dark iran=%v sub %d path %s noack %v backoff %d rt+%d rxidle %v primary=%v",
			m.id, m.iran, s.idx, path, now.Sub(d.ackAt).Round(time.Millisecond), backoff, retrans-d.rtAck,
			time.Duration(nowc-lastRd).Round(time.Millisecond), s == m.primary.Load())
		if s != m.primary.Load() {
			d.isDark = dark
			m.cs223DarkResendLocked(s) // cs223: its chunks the others lack ride a live sub now
		}
	}
	if !dark && d.isDark {
		s.dcLeft = false // cs224: lit again; a later darkness has fresh marks
	}
	d.isDark = dark
}

// dcTraceLocked: one trace line (HS2_MCONN_DARKCALL_TRACE): receive progress
// since the last line, the primary, and per live sub
// idx:path:unacked KB:backoff:retransmits since ack progress:ms since ack
// progress:ms since the link's last receive, flags D (dark) C (capped)
// P (primary).
func (m *mConn) dcTraceLocked(live []*msub, now time.Time, nowc int64, next uint64, lite bool) {
	d := &m.dc
	rx := next - d.trNext
	if d.trAt.IsZero() {
		rx = 0
	}
	d.trAt, d.trNext = now, next
	p := m.primary.Load()
	pi := -1
	if p != nil {
		pi = p.idx
	}
	b := make([]byte, 0, 64+48*len(live))
	for _, s := range live {
		x := &s.dc
		fl := ""
		if x.isDark {
			fl += "D"
		}
		if x.full {
			fl += "C"
		}
		if x.txDark {
			fl += "T" // dc254
		}
		if s == p {
			fl += "P"
		}
		rxIdle := int64(-1)
		if x.lastRd != 0 {
			rxIdle = (nowc - x.lastRd) / int64(time.Millisecond)
		}
		b = fmt.Appendf(b, " %d:%s:%d:%d:%d:%d:%d%s", s.idx, x.path, x.unacked>>10, x.backoff,
			x.retrans-x.rtAck, now.Sub(x.ackAt).Milliseconds(), rxIdle, fl)
	}
	tag := "trace"
	if lite {
		tag = "ltrace" // cs226: a lite (non-call) connection on a silent/marked link
	}
	b = m.b262TraceTail(b, live) // b262: send queues and receive reassembly
	log.Printf("%x darkcall %s iran=%v rxKB %d prim %d subs %d stalled %d ms |%s", m.id, tag, m.iran, rx>>10,
		pi, len(live), now.Sub(d.rxAt).Milliseconds(), b)
}

func (m *mConn) dcLogf(format string, a ...any) {
	if darkcallStat.logged.Add(1) > darkcallLogMax {
		return
	}
	log.Printf(format, a...)
}

func (m *mConn) dcArmCheck(now time.Time, next uint64, to *msub) {
	d := &m.dc
	d.last, d.checkAt, d.chkNext, d.chkTo, d.lastTo = now, now.Add(darkcallCheck), next, to, to
	if to != nil {
		d.chkAcked = to.dc.acked
	}
}

// dcRescueLocked (Iran): a records rescue on a dark link; rx: started by
// "a live sub dark and the receive stalled".
func (m *mConn) dcRescueLocked(now time.Time, next uint64, rx bool) bool {
	if !m.iran {
		return false
	}
	if m.rrPhase == 2 && now.Sub(m.rrT0) >= darkcallGap() {
		m.rrPhase, m.rrRetried, m.rrDead = 0, false, nil
		darkcallStat.rearm.Add(1)
	}
	set := map[any]bool{}
	for _, s := range m.rrLiveSubsLocked() {
		set[s.key] = true
	}
	if !m.rrEligibleLocked(now, set) {
		darkcallStat.rrNo.Add(1)
		m.dc.last = now // do not retry every tick
		return false
	}
	if !rescueAllowNow(now) {
		darkcallStat.rrNo.Add(1)
		return false
	}
	m.lightOpening = true
	darkcallStat.rrec.Add(1)
	if rx {
		darkcallStat.rrRx.Add(1)
	}
	m.dcArmCheck(now, next, nil)
	m.dcLogf("%x darkcall rrec iran=%v rx=%v", m.id, m.iran, rx)
	return true
}

// dcSwitchLocked makes to the primary and puts on it what p holds that the
// peer may lack (cs189's switch).
func (m *mConn) dcSwitchLocked(p, to *msub, now time.Time, next uint64) {
	var cur uint64
	if c := p.cur; c != nil && c.seq() {
		cur += uint64(c.n)
	}
	for _, c := range p.curMore {
		if c.seq() {
			cur += uint64(c.n)
		}
	}
	qStart := m.sendOff
	for _, c := range p.q {
		if c.seq() {
			qStart = c.off
			break
		}
	}
	var unc uint64
	if w, c, ok := darkcallMarks(p); ok {
		unc = uint64(w - c) // uint32 wrap
	}
	from := uint64(0)
	if back := unc + cur; back < qStart {
		from = qStart - back
	}
	// cs221: to holds every DATA chunk from held on; resend only below it
	hi := qStart
	held0 := m.cs221HeldFrom(to)
	held := held0
	if b262CoverOn.Load() && held < qStart && from < qStart {
		// b262: trust the claim only as far as to provably has the bytes
		if h := b262HoldsFrom(to, qStart); h > max(held, from) {
			b262Stat.claimCut.Add(1)
			b262Stat.claimB.Add(min(h, qStart) - max(held, from))
			held = h
		}
	}
	if held < hi {
		hi = max(held, from)
		cs221Stat.held.Add(1)
		cs221Stat.heldB.Add(qStart - hi)
	}
	m.prefPrimary = to
	m.setPrimaryLocked()
	covered := false
	var resent int
	stale := cs224StaleSwitch(p) // cs224: p's marks are those of a switch already made
	if stale {
		hi = from
	}
	p.dcLeft = true
	if xs261On.Load() {
		// b258's fix (0f6cf83f): to is chosen as primary on live evidence;
		// its marks count again. cs224's "left" mark used to stay for the
		// sub's life, so a later switch off it (lab1 NN1 1d3: 1->2, 2->1,
		// 1->2) was taken as stale and resent nothing (unc 621 KB, call
		// stalled 5.4 s).
		to.dcLeft = false
	}
	var rgOn bool
	var rgStart, rgEnd uint64
	if m.rscCap && !stale {
		m.rkMu.Lock()
		rgOn, rgEnd, rgStart = m.rk.on, m.rkEnd, m.rkEnd-uint64(m.rk.n)
		if m.rk.on && m.rkEnd >= qStart {
			start := m.rkEnd - uint64(m.rk.n)
			lo := max(from, start)
			if lo < hi {
				if data, ok := m.rrRangeLocked(lo, hi); ok {
					pushResendLocked(to, lo, data)
					resent = len(data)
					to.rsLo, to.rsHi = lo, lo+uint64(len(data)) // cs223
					covered = lo == from
				}
			} else if b262CoverOn.Load() && from < hi {
				// b262: the ring starts at or past hi: it holds none of
				// [from, hi) (e.g. it restarted empty): not covered
				b262RingMissLocked(from, start, hi)
			} else {
				covered = true
			}
		}
		m.rkMu.Unlock()
	} else if from >= hi && !stale {
		covered = true
	}
	moved := 0
	heldQ := held0
	if b262CoverOn.Load() && heldQ < m.sendOff {
		heldQ = max(heldQ, b262HoldsFrom(to, m.sendOff))
	}
	for _, c := range p.q {
		if c.seq() {
			if c.off >= heldQ {
				cs221Stat.skipQ.Add(1)
				continue // cs221: to was given this chunk too
			}
			to.push(c)
			moved += c.n
		}
	}
	if covered {
		to.lastHole = time.Time{}
		darkcallStat.covered.Add(1)
	} else {
		darkcallStat.short.Add(1)
	}
	darkcallStat.switched.Add(1)
	darkcallStat.resent.Add(uint64(resent))
	darkcallStat.moved.Add(uint64(moved))
	obs.mconnDupBytes.Add(uint64(resent + moved))
	m.pStuckSince, m.qHighSince = time.Time{}, time.Time{}
	p.dc.full = false
	if fx212Darkcall {
		to.dc.full = false // fx212: the new primary must take the call now
	}
	m.dcArmCheck(now, next, to)
	m.tr.ev("darkcall: primary %d -> %d: link dark, unconsumed %d, resent [%d,%d) %d covered=%v, moved %d",
		p.idx, to.idx, unc, from, qStart, resent, covered, moved)
	m.dcLogf("%x darkcall switch iran=%v %d(%s) -> %d(%s) unc %d resent %d covered %v moved %d"+
		" · b262 need [%d,%d) hi %d miss %d held %d heldQ %d ring %v [%d,%d) to wr [%d,%d) txEnd %d sendOff %d subs %d call %v dupOff %v lite %v stale %v",
		m.id, m.iran, p.idx, p.dc.path, to.idx, to.dc.path, unc, resent, covered, moved,
		from, qStart, hi, to.missEnd, held, heldQ, rgOn, rgStart, rgEnd, to.wrLo, to.wrHi, to.txEnd, m.sendOff, len(m.subs), m.callOn, m.dupOff, m.lite, stale)
	m.sendCond.Broadcast()
}

// stalled (fx212): the link shows bytes in flight and no TCP ack progress
// for at least darkcallAfter/2 (fresh evidence from dcLookLocked).
func (d *dcSub) stalled(now time.Time) bool {
	return d.ok && d.unacked > 0 && now.Sub(d.ackAt) >= darkcallAfter()/2
}

// dcCapFor (fx212): the in-flight cap: capMin, but at most 3/4 of the
// records ring (its capacity, at least rrRingCap) while the ring is on, so
// the switch's resend from the ring covers what a capped sub holds.
func dcCapFor(capMin int, ringOn bool, ringBuf int) int {
	if !ringOn {
		return capMin
	}
	return min(capMin, max(ringBuf, int(rrRingCapDC.Load()))*3/4) // cs228: the timer reads a mirror (tests set rrRingCap)
}

// dcNotAllCappedLocked (fx212): a call with 2+ live subs never has all of
// them capped: the sub with the latest ack progress is released.
func (m *mConn) dcNotAllCappedLocked(live []*msub) {
	if len(live) < 2 {
		return
	}
	var best *msub
	for _, s := range live {
		if !s.dc.full {
			return
		}
		if best == nil || s.dc.ackAt.After(best.dc.ackAt) {
			best = s
		}
	}
	best.dc.full = false
	darkcallStat.uncapAll.Add(1)
	m.sendCond.Broadcast()
}
