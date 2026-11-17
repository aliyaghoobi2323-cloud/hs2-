package engine

import (
	"fmt"
	"log"
	"os"
	"sync/atomic"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/envdef"
)

// dc254 (HS2_DC254, default on; "0" = the rel69i behaviour): darkcall's
// blind spot on a silent call whose links still look alive.
//
// st245/st246 SS2: a 2 Mbit/s dual call froze ~4.6 s in a storm (both
// directions, released at once) and darkcall counted nothing: dark 0,
// switched 0. A sub is DARK only when its link has bytes in flight with no
// ack progress for darkcallAfter, in RTO backoff, AND nothing received on
// the link for darkcallAfter. In a storm the link is shared by hundreds of
// conns and the reverse direction keeps delivering, so "nothing received"
// never holds on a link whose OUR->peer direction sits in RTO backoff: the
// call's sends freeze for the whole backoff chain (0.3+0.6+1.2+2.4 s) and
// the detector calls the link lossy-but-alive. Worse, each side's call
// copy decision (callRollLocked) wants both directions call-like in every
// window: once one direction stalls, the window's receive falls under
// mCallMinRate (or the send burst breaks mCallSteady), the side stops
// copying and the call rides only its primary, so one stalled link freezes
// the whole call (the SS2 logs show "call bytes copied" falling in exactly
// that window).
//
// Detector (counters and log lines only; runs whenever the darkcall timer
// runs, key on or off, so an A/B reads the same detector):
//   - detector: a call connection whose receive has not advanced for
//     dc254SilentAfter (seen at 800 ms: catches every 1 s silence) counts one "silent rx" episode (its length is
//     logged and the max kept), and a call whose primary has bytes in
//     flight with no ack progress for 1 s counts one "tx stall" episode.
//     Each side watches its own receive, so a silent direction is caught
//     by its receiver; the tx stall catches it at the sender too.
//
// With the key on:
//   - txDark: a sub whose link has bytes in flight, no ack progress for
//     darkcallAfter and RTO backoff (backoff >= 2 or >= 2 retransmits),
//     whatever the link still receives. A txDark primary is switched like
//     a dark one (same candidate rules; a candidate must not be txDark).
//   - copy hold: a call that was copying keeps copying while it was
//     call-like within callFreeTTL and the window stays under
//     mCallMaxRate, instead of dropping to one copy the moment a stall
//     makes one window look not call-like.
//
// dc254On is atomic: tests flip it while the global darkcall timer may still
// visit a previous test's connections.
var dc254On atomic.Bool

func init() { dc254On.Store(dc254Key()) }

// dc254Key: HS2_DC254 as set, default on; under HS2_DEFAULTS=legacy (the
// envdef table dormant) only an explicit 1 turns it on, as for every table
// key (b264 audit: the in-code default kept dc254 on under legacy, so the
// "all keys off" rollback did not give back rel69i's darkcall).
func dc254Key() bool {
	if envdef.Legacy() {
		return os.Getenv("HS2_DC254") == "1"
	}
	return envBoolDefault("HS2_DC254", true)
}

// dc254SilentAfter: the visits run every darkcallEvery (200 ms), so a
// silence of 1 s is seen at >= 1 s - darkcallEvery: count from 800 ms to
// catch every silence of 1 s or more.
const dc254SilentAfter = 800 * time.Millisecond

var dc254Stat struct {
	silentRx  atomic.Uint64 // silent-rx episodes (>= 1 s)
	silentMax atomic.Int64  // longest silent-rx episode, ms
	txStall   atomic.Uint64 // primary tx-stall episodes (>= 1 s)
	txDark    atomic.Uint64 // txDark subs (link alive, our sends in RTO)
	txDarkSw  atomic.Uint64 // switches off a txDark (not dark) primary
	copyHold  atomic.Uint64 // call windows kept copying by the hold
	// lateTicks: darkcall ticks >= dc254SilentAfter late (the whole process,
	// not a link, stalled: CPU starvation, a long GC pause); a call silent
	// for that long is invisible to the per-conn receive test (readers and
	// the timer resume together), so it is counted here
	lateTicks atomic.Uint64
	lateMax   atomic.Int64 // ms
	// silentGone: silent-rx episodes that ended because the connection
	// stopped being a call (a hang-up keeps "call" for callFreeTTL and its
	// receive stops: counted at onset in silentRx, never in silentMax);
	// silentRx - silentGone are the episodes the receive came back from
	silentGone atomic.Uint64
	// b271 Q3: silentRx split at the onset by the subs' link evidence:
	// silentLink, some live sub's link shows trouble (dark, txDark, bytes
	// in flight without ack progress, or nothing read on the link) for
	// dc254SilentAfter; silentApp, every live sub's link looks healthy, so
	// the silence is the application's (prod: ~88 % of the episodes)
	silentLink atomic.Uint64
	silentApp  atomic.Uint64
}

// dc254Log (b264 audit): dc254's own log budget. Its lines used the darkcall
// lifetime cap (darkcallLogMax 3000, shared): txdark onsets alone ran
// 455-681 per side per 7 min on the lab2 bs201 soak, so in production the
// cap was gone within the first hour and every later darkcall line (switch,
// outcome, rrec) was dropped for the process lifetime. Now at most
// dc254Log.per lines a minute; the counters keep the totals.
var dc254Log = logBudget{per: 10}

// dc254TxLog (b271 Q3): the txdark onset lines' own budget; they were the
// most frequent dc254 line and ate dc254Log, leaving no room for the
// silent/txstall lines.
var dc254TxLog = logBudget{per: 10}

// logBudget: at most per lines a minute (b264 audit); drop counts the rest.
type logBudget struct {
	per  int64
	min  atomic.Int64 // unix minute of the current budget
	n    atomic.Int64 // lines in it
	drop atomic.Uint64
}

func (b *logBudget) ok(now time.Time) bool {
	w := now.Unix() / 60
	if o := b.min.Load(); o != w && b.min.CompareAndSwap(o, w) {
		b.n.Store(0)
	}
	if b.n.Add(1) > b.per {
		b.drop.Add(1)
		return false
	}
	return true
}

func dc254LogOK(now time.Time) bool { return dc254Log.ok(now) }

func dc254Logf(now time.Time, format string, a ...any) {
	if dc254LogOK(now) {
		log.Printf(format, a...)
	}
}

// dc254TickLate: counts a darkcall tick that came >= dc254SilentAfter later
// than its period (last: the previous tick; zero: none).
func dc254TickLate(last, now time.Time) {
	if last.IsZero() {
		return
	}
	late := now.Sub(last) - darkcallEvery
	if late < dc254SilentAfter {
		return
	}
	dc254Stat.lateTicks.Add(1)
	ms := late.Milliseconds()
	for {
		o := dc254Stat.lateMax.Load()
		if ms <= o || dc254Stat.lateMax.CompareAndSwap(o, ms) {
			break
		}
	}
	log.Printf("mconn darkcall late tick: %v late (process stall)", late.Round(time.Millisecond))
}

func dc254LogPart() string {
	s := &dc254Stat
	return fmt.Sprintf(" · dc254 on %v silent rx %d (link %d app %d, max %d ms) txstall %d · txdark %d sw %d · copyhold %d · late ticks %d (max %d ms) · silent gone %d log drop %d txdark drop %d",
		dc254On.Load(), s.silentRx.Load(), s.silentLink.Load(), s.silentApp.Load(), s.silentMax.Load(), s.txStall.Load(), s.txDark.Load(), s.txDarkSw.Load(), s.copyHold.Load(),
		s.lateTicks.Load(), s.lateMax.Load(), s.silentGone.Load(), dc254Log.drop.Load(), dc254TxLog.drop.Load())
}

// dc254TxDark: the link's sends are frozen in RTO backoff (dcLookLocked's
// first two tests, without the receive test).
func dc254TxDark(d *dcSub, now time.Time) bool {
	return dc254On.Load() && d.ok && d.unacked > 0 && now.Sub(d.ackAt) >= darkcallAfter() &&
		(d.backoff >= 2 || d.retrans-d.rtAck >= 2)
}

// dc254LookLocked (after dcLookLocked): updates s's txDark flag.
func (m *mConn) dc254LookLocked(s *msub, now time.Time) {
	d := &s.dc
	td := dc254TxDark(d, now)
	if td && !d.txDark {
		dc254Stat.txDark.Add(1)
		dc254TxLogf(now, "%x darkcall txdark iran=%v sub %d path %s noack %v backoff %d rt+%d primary=%v",
			m.id, m.iran, s.idx, d.path, now.Sub(d.ackAt).Round(time.Millisecond), d.backoff, d.retrans-d.rtAck,
			s == m.primary.Load())
	}
	d.txDark = td
}

// dc254SilenceLocked (darkcall visit of a call connection, after the subs
// were looked at): the silent-rx and tx-stall episodes.
func (m *mConn) dc254SilenceLocked(p *msub, live []*msub, now time.Time, nowc int64, next uint64) {
	// the counters run with the key off too (observe only), so both arms
	// of an A/B read the same detector
	d := &m.dc
	if sil := now.Sub(d.rxAt); sil >= dc254SilentAfter {
		if !d.silent {
			d.silent = true
			dc254Stat.silentRx.Add(1)
			ev := dc254LinkEvidence(live, now, nowc)
			if ev != "" {
				dc254Stat.silentLink.Add(1)
			} else {
				dc254Stat.silentApp.Add(1)
				ev = "none"
			}
			if dc254LogOK(now) {
				log.Printf("%x darkcall silent iran=%v rx stalled %v link evidence %s", m.id, m.iran, sil.Round(time.Millisecond), ev)
			}
			if live != nil && darkcallStat.logged.Load() < darkcallLogMax && dc254LogOK(now) {
				m.dcTraceLocked(live, now, nowc, next, false) // the subs' link evidence at the onset
			}
		}
		d.silentFor = sil
	} else if d.silent {
		d.silent = false
		ms := d.silentFor.Milliseconds()
		for {
			o := dc254Stat.silentMax.Load()
			if ms <= o || dc254Stat.silentMax.CompareAndSwap(o, ms) {
				break
			}
		}
		dc254Logf(now, "%x darkcall silent iran=%v ended after >= %v", m.id, m.iran, d.silentFor.Round(time.Millisecond))
	}
	st := p != nil && p.dc.ok && p.dc.unacked > 0 && now.Sub(p.dc.ackAt) >= dc254SilentAfter
	if st && !d.txStall {
		dc254Stat.txStall.Add(1)
		dc254Logf(now, "%x darkcall txstall iran=%v sub %d path %s noack %v backoff %d", m.id, m.iran, p.idx, p.dc.path,
			now.Sub(p.dc.ackAt).Round(time.Millisecond), p.dc.backoff)
	}
	d.txStall = st
}

// dc254TxLogf: a txdark line under its own budget (b271 Q3).
func dc254TxLogf(now time.Time, format string, a ...any) {
	if dc254TxLog.ok(now) {
		log.Printf(format, a...)
	}
}

// dc254LinkEvidence (b271 Q3): "" when every live sub's link looks healthy
// at a silent-rx onset, else the first sub's trouble: D dark, T txDark, A
// bytes in flight with no ack progress for dc254SilentAfter, R nothing read
// on the link for dc254SilentAfter. Subs not yet looked at (!dc.ok) give no
// evidence either way.
func dc254LinkEvidence(live []*msub, now time.Time, nowc int64) string {
	for _, s := range live {
		if s == nil || s.dead || s.retired || !s.dc.ok {
			continue
		}
		d := &s.dc
		fl := ""
		if d.isDark {
			fl += "D"
		}
		if d.txDark {
			fl += "T"
		}
		if d.unacked > 0 && now.Sub(d.ackAt) >= dc254SilentAfter {
			fl += "A"
		}
		if d.lastRd != 0 && time.Duration(nowc-d.lastRd) >= dc254SilentAfter {
			fl += "R"
		}
		if fl != "" {
			return fmt.Sprintf("sub %d %s", s.idx, fl)
		}
	}
	return ""
}

// dc254LeaveLocked (a darkcall visit of a connection that is not a call, or
// not visited as one): an open silent-rx episode ends without a length (it
// is the call's end, not a stall), and the tx-stall edge re-arms (b264).
func (m *mConn) dc254LeaveLocked() {
	d := &m.dc
	if d.silent {
		d.silent, d.silentFor = false, 0
		dc254Stat.silentGone.Add(1)
	}
	d.txStall = false
}

// dc254CopyHoldLocked (callRollLocked): true when a copying call keeps
// copying although this window did not look call-like.
func (m *mConn) dc254CopyHoldLocked(now time.Time, w time.Duration, sent int, was bool) bool {
	if !dc254On.Load() || !was || m.callLikeAt.IsZero() || now.Sub(m.callLikeAt) >= callFreeTTL {
		return false
	}
	if w <= 0 || float64(sent)/w.Seconds() > mCallMaxRate {
		return false
	}
	dc254Stat.copyHold.Add(1)
	return true
}
