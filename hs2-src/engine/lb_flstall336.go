package engine

import (
	"fmt"
	"log"
	"math"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// b336: stall evidence for the fast lower (HS2_B335_FASTLOWER=1; "0" in
// HS2_B336_FL_STALL turns this part off).
//
// Production (tunnel 1, 2026-11-18 12:56): the first sign of a loss burst
// on one Iran IP was a run of "darkcall silent ... rx stalled" lines on
// many connections 0–1 s after it began; the LB's first action came 11 s
// later. A stalled call connection notes the path of its primary link here
// (dc254SilenceLocked, at the stall's onset). Every health tick the edge's
// fast-lower detector takes the window's notes and reads an IP as
// stall-hot when ALL of:
//   - at least flStallConns distinct connections and flStallLinks distinct
//     links stalled on its paths in the window (one stalled user never
//     counts, nor one user's links);
//   - they are concentrated on it: at least flStallConc× the stalled
//     connections of any other IP on the same side (a tunnel-wide stall,
//     an Iran-wide hiccup or a busy Kharej IP spreads them evenly);
//   - its own resent counts corroborate it: some path of it with at least
//     flStallSegs segments resends over lb2CutLoss and over 2× the best
//     other IP's path + 1 point, while that best path is under lb2CutLoss.
//
// One stall-hot window lowers the IP at once (the resent-only rule needs
// flNeed); the flap guard is unchanged: an IP lowered flFlapN times within
// lb2FlapWin waits for flNeedFlap hot windows in a row, stall or not.
var flStallOn = os.Getenv("HS2_B336_FL_STALL") != "0"

var (
	flStallConns  = 3
	flStallRTO    = 3         // links in RTO backoff or resending (instead of the connections)
	flStallRtLink = uint64(2) // segments a link resent in one tick to count
	flStallLinks  = 2
	flStallConc   = 2.0
	flStallSegs   = uint64(20)
	flStallCorr   = 0.015           // corroborating resent share (half lb2CutLoss: the stalls are the main evidence)
	flStallAge    = 3 * time.Second // notes older than this at a take are dropped
	flStallCap    = 4096
)

// Counters (lab and status).
var (
	flStallNotes  atomic.Int64 // stall notes taken
	flStallWins   atomic.Int64 // stall-hot windows (per IP)
	flStallLowers atomic.Int64 // lowers on a stall-hot window
	flStallDrop   atomic.Int64 // notes dropped (cap)
)

// FastLowerStallCounters is notes, stall-hot windows, stall lowers, drops.
func FastLowerStallCounters() (int64, int64, int64, int64) {
	return flStallNotes.Load(), flStallWins.Load(), flStallLowers.Load(), flStallDrop.Load()
}

// flStallEv is one stall onset: the connection, its link and the link's path.
type flStallEv struct {
	at   time.Time
	conn uint64
	link any
	path string
}

var flStallReg struct {
	mu sync.Mutex
	ev []flStallEv
}

// flStallNote records a stall onset (any goroutine).
func flStallNote(e flStallEv) {
	if !flOn() || !flStallOn || e.path == "" {
		return
	}
	flStallReg.mu.Lock()
	if len(flStallReg.ev) >= flStallCap {
		flStallDrop.Add(1)
	} else {
		flStallReg.ev = append(flStallReg.ev, e)
	}
	flStallReg.mu.Unlock()
}

// flStallTake returns the notes since the last take (younger than flStallAge).
func flStallTake(now time.Time) []flStallEv {
	flStallReg.mu.Lock()
	ev := flStallReg.ev
	flStallReg.ev = nil
	flStallReg.mu.Unlock()
	out := ev[:0]
	for _, e := range ev {
		if now.Sub(e.at) <= flStallAge {
			out = append(out, e)
		}
	}
	flStallNotes.Add(int64(len(out)))
	return out
}

// flStallNoteLocked (dc254SilenceLocked, at a silent-rx onset): the stalled
// connection's primary link, else its first live link with a path.
func (m *mConn) flStallNoteLocked(p *msub, live []*msub, now time.Time) {
	if !flOn() || !flStallOn {
		return
	}
	s := p
	if s == nil || s.dc.path == "" {
		s = nil
		for _, x := range live {
			if x != nil && !x.dead && !x.retired && x.dc.path != "" {
				s = x
				break
			}
		}
	}
	if s == nil {
		return
	}
	flStallNote(flStallEv{at: now, conn: m.id, link: s, path: s.dc.path})
}

// flStallHot: the IPs this window's stalls (with counts' corroboration) mark
// hot, with the reason for the log.
func flStallHot(counts map[string]flRead, alive map[string]bool, stalls []flStallEv) map[string]string {
	if len(stalls) == 0 {
		return nil
	}
	flStallDbg("window: %d notes", len(stalls))
	type agg struct {
		conns map[uint64]bool
		rto   map[any]bool // links in RTO backoff (conn 0: the edge tick's TCP_INFO)
		links map[any]bool
		paths map[string]bool
		side  int
	}
	ips := map[string]*agg{}
	for _, e := range stalls {
		if !alive[e.path] {
			continue
		}
		l, r := pathIPs(e.path)
		for s, ip := range [2]string{l, r} {
			if ip == "" {
				continue
			}
			a := ips[ip]
			if a == nil {
				a = &agg{conns: map[uint64]bool{}, rto: map[any]bool{}, links: map[any]bool{}, paths: map[string]bool{}, side: s}
				ips[ip] = a
			}
			if e.conn == 0 {
				a.rto[e.link] = true
			} else {
				a.conns[e.conn] = true
			}
			a.links[e.link] = true
			a.paths[e.path] = true
		}
	}
	share := func(c flRead) float64 { return float64(c.rt) / float64(c.segs) }
	var out map[string]string
	for ip, a := range ips {
		n, nr := len(a.conns), len(a.rto)
		if n < flStallConns && nr < flStallRTO || len(a.links) < flStallLinks || len(a.paths) < 2 {
			if n+nr >= 2 {
				flStallDbg("IP %s conns %d rto %d links %d paths %d: too few", ip, n, nr, len(a.links), len(a.paths))
			}
			continue
		}
		other := 0
		for ip2, b := range ips {
			if ip2 != ip && b.side == a.side {
				other = max(other, len(b.conns)+len(b.rto))
			}
		}
		if float64(n+nr) < flStallConc*float64(other) {
			flStallDbg("IP %s conns %d rto %d links %d: not concentrated (next IP %d)", ip, n, nr, len(a.links), other)
			continue
		}
		// corroboration: this IP's own resent counts
		best := math.Inf(1)
		for k, c := range counts {
			if !alive[k] || c.segs < flMinSegs {
				continue
			}
			l, r := pathIPs(k)
			if l != ip && r != ip {
				best = math.Min(best, share(c))
			}
		}
		if math.IsInf(best, 1) || best > lb2CutLoss {
			flStallDbg("IP %s conns %d rto %d: no clean other IP (best %.1f%%)", ip, n, nr, 100*best)
			continue
		}
		worst, wk, read, bad := -1.0, "", 0, 0
		for k, c := range counts {
			if !alive[k] || c.segs < flStallSegs {
				continue
			}
			if l, r := pathIPs(k); l != ip && r != ip {
				continue
			}
			read++
			if sh := share(c); sh > flStallCorr && sh > 3*best+0.005 {
				bad++
				if sh > worst {
					worst, wk = sh, k
				}
			}
		}
		if wk == "" || float64(bad) < 0.5*float64(read) {
			flStallDbg("IP %s conns %d rto %d links %d paths %d other %d: corroboration %d of %d paths (worst %.1f%%, best other %.1f%%)", ip, n, nr, len(a.links), len(a.paths), other, bad, read, 100*worst, 100*best)
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		out[ip] = fmt.Sprintf("%d connections stalled, %d links in RTO backoff, %d links (next IP %d), path %s resends %.1f%% (best other %.1f%%)", n, nr, len(a.links), other, wk, 100*worst, 100*best)
	}
	return out
}

// b336CalmSmall: a lowered IP's window with a small sample (flStallSegs..
// flMinSegs) over lb2CutLoss is not calm (HS2_B336_FL_STALL governs it too).
var b336CalmSmall = flStallOn

// HS2_B336_FL_DEBUG=1 (lab): log every IP with stall evidence and why it is
// not stall-hot.
var flStallDebug = os.Getenv("HS2_B336_FL_DEBUG") == "1"

func flStallDbg(f string, a ...any) {
	if flStallDebug {
		log.Printf("lb flstall: "+f, a...)
	}
}
