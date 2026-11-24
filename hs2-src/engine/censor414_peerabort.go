package engine

import (
	"math/rand/v2"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/censor"
)

// b414 (censor351 F2, Kharej vantage): the Iran end tells the Kharej end
// how many of its links on an Iran IP it aborted itself, so a reset the
// Kharej detector sees on such a link is not taken for an injector. The
// detector stays detect-only: nothing here changes a link, a timing or a
// decision; it only feeds censor.NotePeerAborts.
//
// Wire: bits 56..61 of the control ping's sequence word (free in every
// release: the seq is bits 0..39, mt135's MTU bits 40..55, ctrlMtuBit 62,
// ctrlIdleBit 63; an older exit reads only those and echoes the rest, and
// the edge masks the echo with ctrlSeqMask) carry the Iran process's own
// ECONNABORTED link deaths (censor.DeathAbort: the socket was killed on the
// Iran host, so the kernel sent the Kharej end a reset) on the link's Iran
// IP, as a wrapping count in 1..63 (count mod 63, plus 1). 0 means "this
// edge does not report" (an older edge, or the detector off), so a newer
// Kharej knows from the first ping of a reporting edge that it reports and
// holds resets for a report from then on, the first abort burst included
// (b414 v2: without that, the first burst after a Kharej start came before
// any report and read as injected resets). Every link on an IP carries the
// same count; the Kharej end credits an IP's increase only as far as the
// link itself saw it rise (a link's first ping is a baseline), so a
// restarted Iran process, whose count starts over, gives no false credit.
// Only with the detector on (censor.Active) on each side.
//
// b435 extension (rel69r-rc10): 6 bits cannot tell a rise of 32 or more
// (one own burst of 32+ links on an IP between pings) from a restarted Iran
// process, so such bursts went uncredited. A reporting edge now also puts,
// in the ping's second word (its send time, which every exit only echoes
// and only the edge reads back), the flag bit 63 (never set by an older
// edge: a UnixNano below 2^63 until 2262), an 11-bit process instance id
// (bits 52..62, random, never 0) and the count mod 4096 (bits 40..51); the
// send time keeps bits 0..39 (18 min, a mod-2^40 round trip). The 6-bit
// field still goes out for older exits. A newer exit that sees the flag
// reads only the extension: the instance tells a restart (or the other
// process during a zero-downtime upgrade) apart for certain, and a rise up
// to 2047 is a rise. An older edge (no flag) is read as before.

const (
	ctrlAbortShift = 56
	ctrlAbortMask  = uint64(0x3F)
	// ctrlAbortMod: the count's wire values are 1..63 (0: no report).
	ctrlAbortMod = 63
	// ctrlAbortStale: a delta this large (of 63) is an older ping overtaken
	// by a newer one on another link; ignored, the newer count stands.
	ctrlAbortStale = 32

	// b435: the extension in the ping's second word.
	ctrlAbortExtFlag      = uint64(1) << 63
	ctrlAbortExtInstShift = 52
	ctrlAbortExtInstMask  = uint64(0x7FF)
	ctrlAbortExtShift     = 40
	ctrlAbortExtMask      = uint64(0xFFF)
	ctrlAbortExtMod       = 4096
	ctrlAbortExtStale     = 2048
	// ctrlTimeMask: the send time the edge keeps under the extension.
	ctrlTimeMask = uint64(1)<<ctrlAbortExtShift - 1
)

// censorInstance (b435): this process's abort-count instance id, 1..2047.
var censorInstance = uint16(1 + rand.IntN(int(ctrlAbortExtInstMask)))

// ctrlPingTime (b435) is the ping's second word: the send time, under the
// abort extension when ext (from censorAbortWords) is not 0.
func ctrlPingTime(nanos int64, ext uint64) uint64 {
	if ext == 0 {
		return uint64(nanos)
	}
	return ext | uint64(nanos)&ctrlTimeMask
}

// ctrlPongRTT (b435) is the round trip in ns from an echoed second word
// (either form) and the time now; <= 0 when unusable.
//
// b435 v2: the 40-bit difference is signed (sign-extended from bit 39), so
// a wall clock stepped back between ping and pong gives a negative round
// trip that the caller drops, as the plain form always did, instead of
// ~1100 s (2^40 ns) that would reach rttMicros, the stuck check's prompt
// links, the cs228 path quality and the RTT histograms. Real round trips
// stay far below the 2^39 ns (~550 s) bound.
func ctrlPongRTT(echo uint64, now int64) int64 {
	if echo&ctrlAbortExtFlag == 0 {
		return now - int64(echo)
	}
	return int64((uint64(now)-echo)<<(64-ctrlAbortExtShift)) >> (64 - ctrlAbortExtShift)
}

// censorAborts: Iran side, the per-Iran-IP count of own aborts.
var censorAborts sync.Map // netip.Addr -> *ownAborts

// ownAborts: one Iran IP's own-abort count, and (b429, status only) the
// count a control ping last carried, so each abort is counted reported once
// whichever link's ping carries it first.
type ownAborts struct {
	n, sent atomic.Uint32
}

// censorNoteOwnAbort counts one own abort on Iran IP ip (from censorDeath).
func censorNoteOwnAbort(ip netip.Addr) {
	if !ip.IsValid() {
		return
	}
	v, ok := censorAborts.Load(ip)
	if !ok {
		v, _ = censorAborts.LoadOrStore(ip, new(ownAborts))
	}
	v.(*ownAborts).n.Add(1)
	censor.NoteOwnAbort()
}

// censorAbortBits is the edge's ping bits for a link whose local (Iran) IP
// is ip.
func censorAbortBits(ip netip.Addr) uint64 {
	w0, _ := censorAbortWords(ip)
	return w0
}

// censorAbortWords (b435) is censorAbortBits plus the extension for the
// ping's second word (flag, instance, count mod 4096; the caller adds its
// send time with ctrlPingTime). 0, 0 for an invalid ip.
func censorAbortWords(ip netip.Addr) (w0, ext uint64) {
	if !ip.IsValid() {
		return 0, 0
	}
	v, ok := censorAborts.Load(ip)
	if !ok { // reporting from the first ping on: count 0 is wire value 1
		v, _ = censorAborts.LoadOrStore(ip, new(ownAborts))
	}
	a := v.(*ownAborts)
	n := a.n.Load()
	for { // b429: the aborts this ping carries first (CAS: sent never goes back)
		old := a.sent.Load()
		if n <= old {
			break
		}
		if a.sent.CompareAndSwap(old, n) {
			censor.NoteOwnAbortsReported(uint64(n - old))
			break
		}
	}
	w0 = (uint64(n%ctrlAbortMod) + 1) << ctrlAbortShift
	ext = ctrlAbortExtFlag | uint64(censorInstance)<<ctrlAbortExtInstShift |
		uint64(n%ctrlAbortExtMod)<<ctrlAbortExtShift
	return w0, ext
}

// censorLocalIP is a link's local IP (the Iran IP on the edge).
func censorLocalIP(tc *net.TCPConn) netip.Addr {
	if tc == nil {
		return netip.Addr{}
	}
	return censor.AddrOf(tc.LocalAddr())
}

// censorPeerAborts: Kharej side, per Iran IP: the count stream (instance;
// 0 for an older edge's 6-bit count) last seen and its count, when a ping
// last showed it, the live reporting links, and the links that ended since
// the count was last taken.
var censorPeerAborts struct {
	mu sync.Mutex
	ip map[netip.Addr]*peerAbortIP
}

type peerAbortIP struct {
	inst uint16 // b435: the count's Iran instance (0: the 6-bit count)
	last uint16
	at   time.Time // the last ping on the IP (any link)
	live int       // links whose pings carry the count, still up
	gone int       // links on the IP that ended since last was taken
}

// peerAbortPrev (b435): one link's last count and its instance.
type peerAbortPrev struct {
	n    uint16
	inst uint16
	set  bool
}

// ctrlAbortLastAge (b414 v4): how old the IP's last count may be for a new
// link's first ping to credit the rise since it. It is the evaluator's
// censor.PeerOrphanHold (kept equal; the engine only writes to the
// detector): a reset held longer than that has been counted
// anyway, so an older count could only credit later, unrelated resets.
const ctrlAbortLastAge = 2 * time.Minute

// censorPeerAbortDelta folds one ping's 6-bit count for Iran IP ir, seen on
// a link whose previous value is *prev (0: the link's first ping), into the
// last one seen for the IP. It returns the new aborts to credit, and sig:
// the IP's edge reports and this process sees that for the first time (the
// caller tells the detector, which holds resets from then on), or this
// ping is the first on an IP that had no live reporting link (the caller
// tells the detector, so resets held for that report are judged now). An
// older edge (0 bits) gives (0, false) forever.
//
// b414 v4: a link's first ping also credits the rise over the IP's last
// count when the links that saw the rise all died before a ping of theirs
// showed it (Iran's own aborts took the last links on the IP, the case a
// per-link rise never reaches). Against a restarted Iran, whose count
// starts over, three bounds: the rise is below the stale bound (a count
// that went backwards reads as a large rise on the ring), it is at most
// the links on the IP that ended since the count was taken (an abort can
// only explain a link that died), and the IP's count is at most
// ctrlAbortLastAge old. The 6-bit count carries no Iran process id; the
// b435 extension does (censorPeerAbortPing).
func censorPeerAbortDelta(ir netip.Addr, wire uint64, prev *uint8) (d uint64, sig bool) {
	return censorPeerAbortDeltaAt(ir, wire, prev, time.Now())
}

func censorPeerAbortDeltaAt(ir netip.Addr, wire uint64, prev *uint8, now time.Time) (d uint64, sig bool) {
	p := peerAbortPrev{n: uint16(*prev), set: *prev != 0}
	d, sig = censorPeerAbortPingAt(ir, wire, 0, &p, now)
	*prev = uint8(p.n)
	return d, sig
}

// censorPeerAbortPing (b435) reads one ping (w0: the sequence word, w1: the
// time word) for Iran IP ir: the extension when the edge sends one, else
// the 6-bit count.
func censorPeerAbortPing(ir netip.Addr, w0, w1 uint64, prev *peerAbortPrev) (d uint64, sig bool) {
	return censorPeerAbortPingAt(ir, w0, w1, prev, time.Now())
}

func censorPeerAbortPingAt(ir netip.Addr, w0, w1 uint64, prev *peerAbortPrev, now time.Time) (d uint64, sig bool) {
	if !ir.IsValid() {
		return 0, false
	}
	if w1&ctrlAbortExtFlag != 0 {
		inst := uint16(w1 >> ctrlAbortExtInstShift & ctrlAbortExtInstMask)
		if inst == 0 { // never sent; not a count
			return 0, false
		}
		n := uint16(w1 >> ctrlAbortExtShift & ctrlAbortExtMask)
		return censorPeerAbortFold(ir, inst, n, ctrlAbortExtMod, ctrlAbortExtStale, prev, now)
	}
	n := uint16(w0 >> ctrlAbortShift & ctrlAbortMask)
	if n == 0 {
		return 0, false
	}
	return censorPeerAbortFold(ir, 0, n, ctrlAbortMod, ctrlAbortStale, prev, now)
}

// censorPeerAbortFold is the per-IP fold for one count stream (inst, a
// ring of size mod, rises of stale or more are not rises). A ping of
// another instance than the IP's last (an Iran restart, or the other
// process of an upgrade) starts the IP over from its count and credits
// nothing; a link whose own instance changed is a new baseline.
func censorPeerAbortFold(ir netip.Addr, inst, n uint16, mod, stale int, prev *peerAbortPrev, now time.Time) (d uint64, sig bool) {
	lp := *prev
	*prev = peerAbortPrev{n: n, inst: inst, set: true}
	p := &censorPeerAborts
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ip == nil {
		p.ip = map[netip.Addr]*peerAbortIP{}
	}
	st := p.ip[ir]
	if st == nil {
		p.ip[ir] = &peerAbortIP{inst: inst, last: n, at: now, live: 1}
		return 0, true
	}
	diff := func(a, b uint16) int { return (int(a) - int(b) + 2*mod) % mod }
	old, age := st.last, now.Sub(st.at)
	st.at = now
	same := st.inst == inst
	if !lp.set {
		st.live++
		up := st.live == 1 // the IP had no reporting link: a report was due
		if !same {
			st.inst, st.last, st.gone = inst, n, 0
			return 0, up
		}
		id := diff(n, old)
		if id == 0 || id >= stale || st.gone == 0 || age > ctrlAbortLastAge {
			return 0, up // the link's baseline
		}
		c := min(id, st.gone)
		st.last, st.gone = n, 0
		return uint64(c), up
	}
	if lp.inst != inst { // this link's count stream changed: a new baseline
		if !same {
			st.inst, st.last, st.gone = inst, n, 0
		}
		return 0, false
	}
	ld := diff(n, lp.n) // the rise this link saw itself
	if ld == 0 || ld >= stale {
		return 0, false
	}
	if !same {
		st.inst, st.last, st.gone = inst, n, 0
		return 0, false
	}
	id := diff(n, old)
	if id == 0 {
		return 0, false
	}
	st.last, st.gone = n, 0
	if id >= stale {
		// further from the IP's count than an overtaken ping can be: a
		// restarted Iran process counting from 1 again; follow it
		return 0, false
	}
	return uint64(min(ld, id)), false
}

// censorPeerLinkEnd (b414 v4) is a reporting link on Iran IP ir ending
// (prev: its last count, 0: it never carried one). It returns true when it
// was the IP's last live reporting link: the caller tells the detector,
// which then holds the IP's resets until the next link's first ping (or
// censor.PeerOrphanHold), since no report can come before one.
func censorPeerLinkEnd(ir netip.Addr, prev uint8) bool {
	return censorPeerLinkEndP(ir, peerAbortPrev{n: uint16(prev), set: prev != 0})
}

// censorPeerLinkEndP is censorPeerLinkEnd for a link's peerAbortPrev.
func censorPeerLinkEndP(ir netip.Addr, prev peerAbortPrev) bool {
	if !ir.IsValid() || !prev.set {
		return false
	}
	p := &censorPeerAborts
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.ip[ir]
	if st == nil || st.live <= 0 {
		return false
	}
	st.live--
	st.gone++
	return st.live == 0
}

// censorAbortDiff is a - b on the 1..63 wire count ring.
func censorAbortDiff(a, b uint8) uint8 {
	return uint8((int(a) - int(b) + 2*ctrlAbortMod) % ctrlAbortMod)
}
