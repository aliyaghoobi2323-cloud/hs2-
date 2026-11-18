package engine

import (
	"os"
	"sync/atomic"
	"time"
)

// ul268: data-dark rescue of a raw first sub (HS2_UL268_RESCUE, default on,
// =0 off; Iran acts, the trigger runs where rescueWatch runs).
//
// ab267: when a path goes dark for data in one direction (data segments
// dropped, ACKs and small keepalives pass) the er92 silence test never marks
// it (the path is still heard), so an upload whose bytes sit on a link of it
// waits for TCP's RTO backoff (~9 s at 0 Mbit after a 5 s blackout). And the
// raw ring held 128 KB while a fast upload had 2-4 MB in flight.
//
//   - Trigger (per link, TCP_INFO): unacked segments and bytes_acked frozen
//     for >= max(ul268MinFrozen, 8 x srtt), or backoff >= 2 and frozen >=
//     ul268MinFrozen, while a link on ANOTHER path acked within ul268AltFresh
//     (a live alternative: a whole-tunnel stall is not a dark path). The dark
//     links join the er92 silent set: the same rescue (rescueStart / rrStart)
//     runs, with every dark link avoided by the join.
//   - A link that is only data-dark (not er92-silent) rescues a raw conn
//     only when its ring covers what may be in flight (ul268Cover).
//   - Ring: an Iran raw ring of a fast writer (>= ul268FastBps, 2 Mbit/s) grows to half
//     a second of its rate, at most ul268ConnMax; the part above
//     rescueRingCap comes from a process budget (ul268MemCap: min(256 MB,
//     MemTotal/16)). Not there: the ring stays as it is. Slow/light conns keep
//     rescueRingCap.
//   - The receiver drops resent bytes by offset (er91 reassembly): no
//     duplicate delivery.

var ul268On atomic.Bool // loadEnvKeys: HS2_UL268_RESCUE (default on)

// ul268Iran: this process has made an Iran (edge) mConn. Only Iran acts on
// the dark set, so the exit skips the TCP_INFO poll (ab269 bench: 400 links
// cost ~0.86 ms and 400 getsockopt + 100 KB garbage per 250 ms tick).
var ul268Iran atomic.Bool

// ul268DarkOnly: the links of the current watch tick that are data-dark but
// not er92-silent (rescueSilentLinks asks ul268Cover for these).
var ul268DarkOnly atomic.Pointer[map[any]bool]

func ul268FromEnv() bool { return os.Getenv("HS2_UL268_RESCUE") != "0" }

var (
	ul268MinFrozen       = time.Duration(envIntDef("HS2_UL268_MIN_MS", 1000)) * time.Millisecond
	ul268AltFresh        = 2 * time.Second
	ul268FastBps         = envIntDef("HS2_UL268_FAST_KBPS", 256) << 10 // bytes/s
	ul268ConnMax         = envIntDef("HS2_UL268_CONN_MB", 8) << 20
	ul268MemMaxFix int64 = int64(envIntDef("HS2_UL268_MEM_MB", 256)) << 20
)

var ul268Stat struct {
	mem      atomic.Int64  // ring bytes above rescueRingCap (budget)
	noCov    atomic.Uint64 // dark-only rescues skipped: the ring does not cover
	callSkip atomic.Uint64 // dark-only rescues skipped: a call (darkcall's)
	rescued  atomic.Uint64 // raw rescues started from a dark-only link
	over     atomic.Uint64 // growth refused by the budget
	grown    atomic.Uint64 // rings grown past rescueRingCap
	darkEv   atomic.Uint64 // links that turned dark (one per episode)
	darkTick atomic.Uint64 // watch ticks with >= 1 dark link
	noAlt    atomic.Uint64 // dark-looking ticks skipped: no other path acked
}

func ul268LogLine() string {
	s := &ul268Stat
	return "ul268: dark " + u64s(s.darkEv.Load()) + " ticks " + u64s(s.darkTick.Load()) + " noalt " + u64s(s.noAlt.Load()) + " stripe " + u64s(ul268StripeN.Load()) + " rescued " + u64s(s.rescued.Load()) + " nocov " + u64s(s.noCov.Load()) + " callskip " + u64s(s.callSkip.Load()) +
		" · ring grown " + u64s(s.grown.Load()) + " over " + u64s(s.over.Load()) + " mem " + u64s(uint64(max(0, s.mem.Load())>>20)) +
		"/" + u64s(uint64(ul268MemCap()>>20)) + " MB"
}

// ul268MemCap: min(HS2_UL268_MEM_MB, MemTotal/16).
func ul268MemCap() int64 {
	c := ul268MemMaxFix
	if t := rescueMemTotal(); t > 0 && t/16 < c {
		c = t / 16
	}
	return c
}

func ul268Take(n int) bool {
	if ul268Stat.mem.Add(int64(n)) > ul268MemCap() {
		ul268Stat.mem.Add(-int64(n))
		ul268Stat.over.Add(1)
		return false
	}
	return true
}

// ul268Note (raw ring, Iran): the write rate, from the last full second or
// the window running now (>= 250 ms old), whichever is higher.
func (r *rescueRing) ul268Note(now int64, n int) {
	d := now - r.uT0
	switch {
	case r.uT0 == 0:
		r.uT0, r.uN = now, 0
	case d >= int64(time.Second):
		if d >= 2*int64(time.Second) {
			r.uRate = 0
		} else {
			r.uRate = int(int64(r.uN) * int64(time.Second) / d)
		}
		r.uT0, r.uN = now, 0
	}
	r.uN += n
	if d = now - r.uT0; d >= int64(250*time.Millisecond) {
		if est := int(int64(r.uN) * int64(time.Second) / d); est > r.uRate {
			r.uRate = est
		}
	}
}

// ul268Cover (under wmu, after an add): a data-dark rescue of this ring's
// conn may start only when the ring holds every byte written, or is sized to
// its rate (half a second of it) and no budget refusal cut it short. A
// rescue the ring cannot cover ends the conn (noCover): worse than the stall.
func (r *rescueRing) ul268Cover() {
	r.uCov.Store(r.on && r.n > 0 && (int64(r.n) >= r.uTot || !r.uOver && len(r.buf) >= ul268RawCap(r.uRate)))
}

// ul268RawCap: the raw ring's cap for a writer of rate bytes/s: half a
// second of it, rounded UP to rescueRingCap x 2^k (at most ul268ConnMax).
//
// ul268i: with the exact rate/2 the cap crept up with every new rate
// estimate and each step re-made (and copied) a multi-MB ring: lab2 pprof
// 752 MB/min of raw ring allocations vs 23 MB/min in bin264, GC cycles
// 24 -> 43 per run (GC pauses were in 2.7 % of the time and held ~1/3 of
// the icmp tail). Power-of-two steps: at most log2(ConnMax/RingCap)
// growths per conn, <= 2 x the final size allocated in all.
func ul268RawCap(rate int) int {
	if rate < ul268FastBps {
		return rescueRingCap()
	}
	c := rescueRingCap()
	for c < rate/2 && c < ul268ConnMax {
		c <<= 1
	}
	return min(c, ul268ConnMax)
}

// ul268Link: the trigger's per-link state (rescueWatch goroutine only).
type ul268Link struct {
	acked  uint64
	prog   int64 // ns: bytes_acked last moved, or nothing was outstanding
	ackAt  int64 // ns: bytes_acked last moved (0: never seen moving)
	dark   bool
	darkN  int // consecutive ticks judged dark (with a live alternative)
	primed bool
	seen   int64 // ns: the last sample (a gap > ul268Gap re-primes)
}

// ul268Gap: samples further apart than this say nothing about a stall.
const ul268Gap = time.Second

// ul268Judge: one TCP_INFO sample of a link; true when it is data-dark.
func ul268Judge(st *ul268Link, ts tcpStat, now int64) bool {
	gap := st.seen != 0 && time.Duration(now-st.seen) > ul268Gap
	st.seen = now
	if !st.primed || gap {
		st.primed, st.acked, st.prog = true, ts.bytesAcked, now
		return false
	}
	if ts.bytesAcked != st.acked {
		st.acked, st.prog, st.ackAt = ts.bytesAcked, now, now
	}
	if ts.unacked == 0 {
		st.prog = now // nothing outstanding: not stalled
		return false
	}
	frozen := time.Duration(now - st.prog)
	need := max(ul268MinFrozen, 8*time.Duration(ts.rttUs)*time.Microsecond)
	return frozen >= need || ts.backoff >= 2 && frozen >= ul268MinFrozen
}

// ul268Sample: one link as the dark-set pass sees it.
type ul268Sample struct {
	key  any
	path string
	st   *ul268Link
	ts   tcpStat
	ok   bool
}

// ul268DarkSet: the dark links of samples (nil: none). A link is dark only
// while some link on another path acked within ul268AltFresh.
func ul268DarkSet(samples []ul268Sample, now int64) map[any]bool {
	type cand struct {
		key  any
		path string
		st   *ul268Link
	}
	var dark []cand
	for i := range samples {
		s := &samples[i]
		if !s.ok {
			continue
		}
		if ul268Judge(s.st, s.ts, now) {
			dark = append(dark, cand{s.key, s.path, s.st})
		} else {
			s.st.dark, s.st.darkN = false, 0
		}
	}
	if len(dark) == 0 {
		return nil
	}
	var set map[any]bool
	for _, d := range dark {
		alt := false
		for i := range samples {
			o := &samples[i]
			if o.ok && o.path != d.path && o.path != "" && o.st.ackAt != 0 && time.Duration(now-o.st.ackAt) <= ul268AltFresh {
				alt = true
				break
			}
		}
		if !alt {
			ul268Stat.noAlt.Add(1)
			d.st.darkN = 0
			continue
		}
		// review-rel69k #1: dark on ul268DarkTicks consecutive ticks
		if d.st.darkN++; d.st.darkN < ul268DarkTicks {
			continue
		}
		if !d.st.dark {
			d.st.dark = true
			ul268Stat.darkEv.Add(1)
		}
		if set == nil {
			set = map[any]bool{}
		}
		set[d.key] = true
	}
	if set != nil {
		ul268Stat.darkTick.Add(1)
	}
	return set
}

func ul268DarkOnlyHas(k any) bool {
	p := ul268DarkOnly.Load()
	return p != nil && (*p)[k]
}

// ---- striped uploads ---------------------------------------------------------
//
// A striped upload whose sub sits on a data-dark link (the current tick's
// dark set, silent or not) closes that sub as evac56 does (subDied: the
// stripe RESEND puts what it held on the live subs; the receiver drops what
// it already has; the growth rule opens a replacement, the join avoid list
// keeps it off the dark links). Uploads only (Iran is the sender and holds
// the copies), only with another live sub on a non-dark link, at most one
// sub per conn per stripeEvacGap, within stripeEvacAllow's per-second bound.

// ul268DarkAll: every dark link of the current watch tick (nil: none).
var ul268DarkAll atomic.Pointer[map[any]bool]

var ul268StripeN atomic.Uint64 // stripe subs closed off a dark link

func ul268DarkNow(k any) bool {
	p := ul268DarkAll.Load()
	return p != nil && (*p)[k]
}

// ul268StripeEvacLocked (Iran, manager tick, under m.mu): the sub to close
// now, or nil. The caller closes it outside the lock (subDied).
func (m *mConn) ul268StripeEvacLocked(now time.Time) *msub {
	if !ul268On.Load() || ul268DarkAll.Load() == nil || !m.iran || m.stN == 0 || !stripeResend.Load() || m.ended || m.closing ||
		m.handshaking || m.mode != planeLight || m.rawW.Load() || now.Before(m.stEvacAt) || !m.stUpNow || m.dcIsCallLocked(now) {
		return nil
	}
	p := m.primary.Load()
	var bad *msub
	good := 0
	for _, s := range m.subs {
		if s.retired || s.dead || s.closeHS || (!s.stripe && s != p) {
			continue
		}
		if ul268DarkNow(s.key) {
			if bad == nil || bad == p {
				bad = s
			}
			continue
		}
		good++
	}
	if bad == nil || good == 0 || !stripeEvacPeerOK(bad) || !stripeEvacAllow(now) {
		return nil
	}
	m.stEvacAt = now.Add(stripeEvacGap)
	ul268StripeN.Add(1)
	m.tr.ev("ul268: stripe sub %d off a data-dark link (primary=%v)", bad.idx, bad == p)
	return bad
}

// ---- calls -------------------------------------------------------------------
//
// ab268 lab: in 2 of 4 runs (Ma, Pb) the 2 Mbit call probe stalled ~9 s at
// the first blackout while ul268's dark-only links were in the rescue set.
// The first exclusion (856182cf) looked at m.callOn only, which the stall
// itself drops (a window with nothing received is not call-like), and left
// rs146's join retry and the stripe evac open. Now a conn that darkcall
// treats as a call (dcIsCallLocked: callOn, or call-like within callFreeTTL)
// sees only the er92 set: ul268 never acts on it; darkcall (dc254) keeps it.

// ul268CallSet: set without the current tick's data-dark-only links (set
// itself when there are none; nil when nothing is left).
func ul268CallSet(set map[any]bool) map[any]bool {
	p := ul268DarkOnly.Load()
	if p == nil || len(*p) == 0 {
		return set
	}
	var out map[any]bool
	for k := range set {
		if !(*p)[k] {
			if out == nil {
				out = make(map[any]bool, len(set))
			}
			out[k] = true
		}
	}
	return out
}

// ul268SetForLocked (Iran, under mu): the rescue set this conn sees: callSet
// for a call (counted in callskip when a live sub of it is on a dark-only
// link), set for everything else.
func (m *mConn) ul268SetForLocked(now time.Time, set, callSet map[any]bool) map[any]bool {
	p := ul268DarkOnly.Load()
	if p == nil || len(*p) == 0 || !m.dcIsCallLocked(now) {
		return set
	}
	for _, s := range m.subs {
		if !s.retired && !s.dead && (*p)[s.key] {
			ul268Stat.callSkip.Add(1)
			m.tr.ev("ul268: call sub %d on a data-dark link: left to darkcall", s.idx)
			break
		}
	}
	return callSet
}
