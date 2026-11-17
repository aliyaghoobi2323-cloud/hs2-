package engine

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// b262: the darkcall switch's coverage hole (lab1 one-direction cut, b261).
//
// In every remaining multi-second call stall on b261 (an upload cut on a big
// call, 376-603 KB unconsumed on the cut sub), both sides' switches logged
// "resent 0 covered true", and the receive stopped once the new sub had
// delivered what it held, until the old link's TCP left RTO after the uncut:
// some bytes only the cut sub held were never resent.
//
// HS2_B262_COVER (default on; =0 gives back b261):
//   - each sub keeps the newest contiguous DATA range its writer actually
//     handed to its stream (wrLo, wrHi), not what was queued on it;
//   - cs221's "to holds every chunk from missEnd on" is trusted only for the
//     part of [from, qStart) that to provably has: its written range plus the
//     chunks it is writing or still has queued, contiguous from there up to
//     qStart. Below that the switch resends from the ring;
//   - "covered" is true only when the ring (or to) reaches back to from: a
//     ring that restarted empty (memory budget) or does not reach from is
//     reported short, not covered.
//
// The receiver already drops overlaps (mrecv: end <= r.next, or a range an
// out-of-order entry holds).
// b262CoverOn is atomic: darkcall visits (a global ticker over all conns) read it
// via b262DarkLocked while tests flip it (b272 race fix, ported in b276).
var b262CoverOn atomic.Bool

func init() { b262CoverOn.Store(envBoolDefault("HS2_B262_COVER", true)) }

var b262Stat struct {
	claimCut  atomic.Uint64    // switches where cs221's claim was cut to what to provably holds
	claimB    atomic.Uint64    // bytes the claim no longer covers (resent from the ring if there)
	ringShort atomic.Uint64    // switches whose ring misses bytes the peer provably did not consume
	ringLag   atomic.Uint64    // switches whose ring misses only bytes inside the consumed-report lag
	ringLagKB atomic.Uint64    // ... the missed bytes (KB), an upper bound, not a loss
	darkWait  atomic.Uint64    // sends that waited: only a dark sub had room
	darkPair  atomic.Uint64    // call pairs (3+ subs) whose picks were all dark: a live sub added
	dwN       atomic.Uint64    // darkwait episodes (sends that waited at least once)
	dwNs      atomic.Uint64    // their total wait
	dwMaxNs   atomic.Uint64    // the longest (since start)
	dw100     atomic.Uint64    // episodes >= 100 ms
	dwGive    atomic.Uint64    // episodes cut by HS2_B262_WAIT_MS: the chunk went on as b261 did
	dwHist    [5]atomic.Uint64 // episode lengths: <10 ms, <100 ms, <500 ms, <1 s, >= 1 s
	dwLong    [3]atomic.Uint64 // episodes >= 500 ms by class: call, lite (non-call), other
	dwLogged  atomic.Int64     // "b262 darkwait long" lines written (b262DwLogMax)
}

// b262DwLogMax bounds the "b262 darkwait long" diagnostic lines per process.
const b262DwLogMax = 300

// b262WaitMax (HS2_B262_WAIT_MS, default 500; 0: no bound): a darkwait lasts
// at most this long, then the chunk goes out as b261 sent it (dark sub too).
// b266 (lab1 quiet 3-arm ABAB): unbounded, Kharej held sends up to 4.6-5.0 s
// in loaded runs. The dark flags it waits on are refreshed only by darkcall
// visits (calls, cs226 lite visits) and a flag that clears wakes no waiter,
// so an unbounded wait can outlive the darkness; 500 ms keeps the short
// waits (most end < 100 ms) and gives up only a few per run.
const b262WaitDefaultMs = 500

var b262WaitMax = time.Duration(b262WaitEnv(os.Getenv("HS2_B262_WAIT_MS"))) * time.Millisecond

// b262WaitEnv: HS2_B262_WAIT_MS in ms; unset or not a number: the default;
// 0 (no bound) is a value here, unlike envIntDefault's "> 0 or default".
func b262WaitEnv(v string) int {
	if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 {
		return n
	}
	return b262WaitDefaultMs
}

// b262WaitOver (sendLocked, under m.mu, the only copy would ride a dark
// sub): starts the episode on the first call; true once it has lasted
// b262WaitMax (bounded mode), so the send goes on as b261's.
func b262WaitOver(w *b262Wait, m *mConn, n int) bool {
	now := time.Now()
	if w.at.IsZero() {
		w.at = now
		w.snap = m.b262WaitSnapLocked(n)
		w.class = m.b262WaitClassLocked(now)
		b262Stat.dwN.Add(1)
		if b262WaitMax > 0 { // wake the wait at the bound
			time.AfterFunc(b262WaitMax, func() {
				m.mu.Lock()
				m.sendCond.Broadcast()
				m.mu.Unlock()
			})
		}
		return false
	}
	if b262WaitMax > 0 && now.Sub(w.at) >= b262WaitMax {
		b262Stat.dwGive.Add(1)
		w.gave = true
		return true
	}
	return false
}

// b262Wait is one send's darkwait episode (sendLocked's local).
type b262Wait struct {
	at    time.Time // the episode began (zero: not waiting)
	snap  string    // the subs when it began (logged if it lasts >= 500 ms)
	class int       // 0 call, 1 lite (non-call), 2 other
	gave  bool      // ended by HS2_B262_WAIT_MS
}

// b262WaitClassLocked: 0 a call (dcIsCallLocked), 1 a lite non-call, 2 other.
func (m *mConn) b262WaitClassLocked(now time.Time) int {
	switch {
	case m.dcIsCallLocked(now):
		return 0
	case m.lite:
		return 1
	}
	return 2
}

// b262WaitSnapLocked: per sub dark flags, queue and room for an n-byte chunk
// (P primary, R retired, X dead, L dcLeft, T txDark, D isDark, C in-flight
// cap, H close handshake, A closeAfter, G gated, B bulk, f fits).
func (m *mConn) b262WaitSnapLocked(n int) string {
	b := fmt.Appendf(nil, "n %d lite %v dupOff %v mode %d subs %d", n, m.lite, m.dupOff, m.mode, len(m.subs))
	p := m.primary.Load()
	for _, s := range m.subs {
		fl := ""
		for _, f := range [...]struct {
			on bool
			c  string
		}{{s == p, "P"}, {s.retired, "R"}, {s.dead, "X"}, {s.dcLeft, "L"}, {s.dc.txDark, "T"}, {s.dc.isDark, "D"},
			{s.dc.full, "C"}, {s.closeHS, "H"}, {s.closeAfter, "A"}, {s.gated, "G"}, {s.bulk.Load(), "B"}, {s.fits(n), "f"}} {
			if f.on {
				fl += f.c
			}
		}
		cur := 0
		if s.cur != nil {
			cur = 1
		}
		b = fmt.Appendf(b, " | %d:%s:q%d/%d:w%d:%s", s.idx, s.dc.path, s.qBytes>>10, len(s.q), cur, fl)
	}
	return string(b)
}

// b262WaitDone ends a darkwait episode (no-op when none).
func (m *mConn) b262WaitDone(w *b262Wait) {
	if w.at.IsZero() {
		return
	}
	dd := time.Since(w.at)
	d := uint64(dd)
	b262Stat.dwNs.Add(d)
	if d >= uint64(100*time.Millisecond) {
		b262Stat.dw100.Add(1)
	}
	switch {
	case dd < 10*time.Millisecond:
		b262Stat.dwHist[0].Add(1)
	case dd < 100*time.Millisecond:
		b262Stat.dwHist[1].Add(1)
	case dd < 500*time.Millisecond:
		b262Stat.dwHist[2].Add(1)
	case dd < time.Second:
		b262Stat.dwHist[3].Add(1)
	default:
		b262Stat.dwHist[4].Add(1)
	}
	if dd >= 500*time.Millisecond {
		b262Stat.dwLong[w.class].Add(1)
		if b262Stat.dwLogged.Add(1) <= b262DwLogMax {
			log.Printf("%x b262 darkwait long iran=%v %d ms class %s gave %v | at start: %s | at end: %s", m.id, m.iran,
				dd.Milliseconds(), [3]string{"call", "lite", "other"}[w.class], w.gave, w.snap, m.b262WaitSnapLocked(0))
		}
	}
	*w = b262Wait{}
	for {
		old := b262Stat.dwMaxNs.Load()
		if d <= old || b262Stat.dwMaxNs.CompareAndSwap(old, d) {
			return
		}
	}
}

// b262DarkLocked (under m.mu): s must not carry a chunk's only copy: a
// darkcall switch left it (dcLeft) or its link sends nothing (txDark,
// isDark). Lab1 YT1 0u3 (b262t trace): after Iran's switch 1 -> 0 the new
// primary's queue was over its bound with the resend, the dark old primary
// (its cap cleared by the switch) had room, and a 22-byte call chunk went
// out on it alone: Kharej's receive sat on that 22-byte gap with 563 KB
// queued behind it until the old link's TCP left RTO (3.8 s).
func b262DarkLocked(s *msub) bool {
	return b262CoverOn.Load() && (s.dcLeft || s.dc.txDark || s.dc.isDark)
}

// b262 ringshort anomaly (lab1 ZC1, b262v3): Iran counted 239-670 ringshort
// per run (~every Iran switch), Kharej 0. All were lite conns (call false):
// the switch's need [from, qStart) comes from the sub's smux marks, written
// minus the peer's last REPORTED consumed count, and the peer reports it
// only on its first read and then every MaxStreamBuffer/2 (1 MB; autowin:
// win/div) consumed. A lite conn moving a few KB/s almost never reports, so
// "unc" is nearly every byte since its first read (need [0,814) on an 814-
// byte conn; 124 KB on a 4 KB/s conn whose peer, per the Kharej trace, had
// read everything up to the switch) and its 32 KB ring cannot reach from.
// The bytes were not missing: the same run's rescue-rec (the peer's real
// receive offset) resent 4 KB in all over 1271 rescues, nocover 0. So the
// counter, not the cover, was wrong: a miss counts as ringshort only when
// the ring misses bytes at or past from + the report lag (the peer would
// have reported consuming them: provably unconsumed); a miss inside the lag
// is ringlag (unproven; covered stays false as before). Behaviour unchanged.
var b262ReportLag = uint64(SmuxStreamBuffer / 2)

// b262RingMissLocked counts a switch whose ring (starting at start >= hi)
// holds none of [from, hi).
func b262RingMissLocked(from, start, hi uint64) {
	if b262RingMissProven(from, start, hi) {
		b262Stat.ringShort.Add(1)
		return
	}
	b262Stat.ringLag.Add(1)
	b262Stat.ringLagKB.Add((min(start, hi) - from) >> 10)
}

// b262RingMissProven: the missed range [from, min(start, hi)) reaches past
// from + the consumed-report lag.
func b262RingMissProven(from, start, hi uint64) bool {
	return min(start, hi) > from+b262ReportLag
}

func b262LogPart() string {
	return fmt.Sprintf(" · b262 on %v claimcut %d (%d KB) ringshort %d ringlag %d (%d KB) darkwait %d darkpair %d dw ep %d ms %d max %d >=100ms %d give %d hist %d/%d/%d/%d/%d long call %d lite %d other %d", b262CoverOn.Load(),
		b262Stat.claimCut.Load(), b262Stat.claimB.Load()>>10, b262Stat.ringShort.Load(), b262Stat.ringLag.Load(), b262Stat.ringLagKB.Load(), b262Stat.darkWait.Load(),
		b262Stat.darkPair.Load(), b262Stat.dwN.Load(), b262Stat.dwNs.Load()/1e6, b262Stat.dwMaxNs.Load()/1e6,
		b262Stat.dw100.Load(), b262Stat.dwGive.Load(), b262Stat.dwHist[0].Load(), b262Stat.dwHist[1].Load(), b262Stat.dwHist[2].Load(),
		b262Stat.dwHist[3].Load(), b262Stat.dwHist[4].Load(), b262Stat.dwLong[0].Load(), b262Stat.dwLong[1].Load(), b262Stat.dwLong[2].Load())
}

// b262PairLocked (callPairLocked, under m.mu): the call pair's picks must not
// all be dark while a live sub exists. With 3+ subs callPairPick takes subs
// in turn, so a lone fitting pick (or both) can be the sub a switch just
// left: the YT1 0u3 hole again. A fitting live sub replaces the second pick
// (or joins a lone one); with no live sub fitting, k 0: the write waits.
// Key off, or no live sub at all: unchanged.
func b262PairLocked(pick [2]*msub, k int, all []*msub, size int) ([2]*msub, int) {
	if !b262CoverOn.Load() || k == 0 {
		return pick, k
	}
	for _, s := range pick[:k] {
		if !b262DarkLocked(s) {
			return pick, k
		}
	}
	live := false
	for _, s := range all {
		if s.retired || s.dead || b262DarkLocked(s) {
			continue
		}
		live = true
		if s.fits(size) {
			b262Stat.darkPair.Add(1)
			pick[1], k = s, 2
			return pick, k
		}
	}
	if live {
		b262Stat.darkWait.Add(1)
		return pick, 0
	}
	return pick, k
}

// noteWrittenLocked (writer, under m.mu, after a successful write of c and
// more): extend s's written DATA range.
func (s *msub) noteWrittenLocked(c *mchunk, more []*mchunk) {
	s.noteWritten1(c)
	for _, x := range more {
		s.noteWritten1(x)
	}
}

func (s *msub) noteWritten1(x *mchunk) {
	if x == nil || !x.seq() || x.n == 0 {
		return
	}
	lo, hi := x.off, x.off+uint64(x.n)
	switch {
	case s.wrHi == 0 && s.wrLo == 0:
		s.wrLo, s.wrHi = lo, hi
	case lo <= s.wrHi && hi >= s.wrLo: // overlaps or touches the run
		s.wrLo, s.wrHi = min(s.wrLo, lo), max(s.wrHi, hi)
	case lo > s.wrHi: // a gap: a new run
		s.wrLo, s.wrHi = lo, hi
	}
}

// b262HoldsFrom (under m.mu): the lowest offset H such that to provably has
// every DATA byte of [H, qStart): written to its stream, being written, or
// queued on it, contiguously. qStart when it has nothing of the range.
func b262HoldsFrom(to *msub, qStart uint64) uint64 {
	// the chunks it will still write, in queue order (cur first)
	type rng struct{ lo, hi uint64 }
	var rs []rng
	if to.wrHi > to.wrLo {
		rs = append(rs, rng{to.wrLo, to.wrHi})
	}
	add := func(c *mchunk) {
		if c != nil && c.seq() && c.n > 0 {
			rs = append(rs, rng{c.off, c.off + uint64(c.n)})
		}
	}
	add(to.cur)
	for _, c := range to.curMore {
		add(c)
	}
	for _, c := range to.q {
		add(c)
	}
	// walk down from qStart: the contiguous union that ends at/after qStart
	h := qStart
	for changed := true; changed; {
		changed = false
		for _, r := range rs {
			if r.lo < h && r.hi >= h {
				h = r.lo
				changed = true
			}
		}
	}
	return h
}

// b262TraceTail (under m.mu, darkcall trace): per live sub its queue (KB,
// chunks), whether its writer is inside a write, and how far its written
// range trails sendOff (KB); then the receive side: next, the out-of-order
// entries and the first one's distance from next (r.mu taken only if free).
func (m *mConn) b262TraceTail(b []byte, live []*msub) []byte {
	b = fmt.Appendf(b, " | snd off %d", m.sendOff)
	for _, s := range live {
		cur := 0
		if s.cur != nil {
			cur = 1
		}
		lag := int64(-1)
		if s.wrHi > 0 {
			lag = int64(m.sendOff-s.wrHi) >> 10
		}
		b = fmt.Appendf(b, " %d:q%d/%d:w%d:lag%d:miss%d", s.idx, s.qBytes>>10, len(s.q), cur, lag, int64(m.sendOff-s.missEnd)>>10)
	}
	r := &m.r
	if !r.mu.TryLock() {
		return append(b, " | rcv busy"...)
	}
	b = fmt.Appendf(b, " | rcv next %d ready %dKB ooo %d %dKB", r.next, r.readyBytes>>10, len(r.ooo), r.oooBytes>>10)
	if len(r.ooo) > 0 {
		b = fmt.Appendf(b, " gap %d", r.ooo[0].off-r.next)
	}
	r.mu.Unlock()
	return b
}
