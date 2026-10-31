package engine

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

// R1 round 5: a QUIET link's congestion control under the path's random
// loss (docs/architecture/wip/r1/round5/REPORT.fa.md).
//
// A QUIET link is one TCP connection that carries every light class at
// once: the calls (kept there by TwoWay), the games, each connection's
// start, the web's responses until they move, and the control. With cubic
// (round 2's choice: BBR's PROBE_RTT held 200 users' probes) it halves its
// window less 30% at each loss, and on a path that drops packets at random
// it never grows back: Mathis's bound at a 75 ms round trip is ~11 Mbit/s
// at 0.03%, ~6 at 0.1% and ~3.4 at 0.3%. The red team's second pass
// measured the light classes collapse from 0.03% with one QUIET link (calls
// +3.2 s, web pages failing), and the round-5 trace showed why: the QUIET
// socket's window at 36 to 42 segments, 2.7 and 5.5 Mbit/s acknowledged a
// direction, C3's gate shut 88% of the time, and the calls' steady lane
// lost (1 of 8 calls still steady).
//
// The final design: Illinois on every QUIET link at every link count (QuietCC
// below), the watcher QuietCCLoss a lab switch, off.
//
// QuietCC is now a list: the first congestion control the kernel takes.
// The round-5 measure picked Illinois (tcp_illinois, in Linux since 2.6.22):
// a loss-based window like cubic's that reads the round trip's queueing
// delay: with no queue it grows up to 10 segments a round trip and cuts by
// an eighth at a loss, with a queue as Reno does (1 segment, a half). So a
// QUIET link at its fq_codel flow queue, whose losses there are CoDel's
// (a queue), behaves as cubic did, and on a lossy path it keeps its window.
// Its kernel module loads on the first socket that asks for it (the server
// runs as root); a kernel without it takes the next (cubic) and the log says
// so once.
//
// QuietCCLoss (round 5's second approach, measured against the first): the
// socket runs QuietCC and switches, at each end on its own sending, to
// QuietCCLoss while it sees random loss — a share of retransmitted segments
// of at least QuietLossPpm over quietLossWin with the socket's own queue
// (srtt − min_rtt) under quietLossQueue: a loss with no queue is the path's,
// not the bottleneck's — and back once quietCalm went by without it.

// setCongestion is tlscarrier.SetCongestion (a var for the tests).
var setCongestion = tlscarrier.SetCongestion

// quietCCSeen: the congestion control the latest QUIET socket got, for the
// one log line a change makes.
var quietCCSeen struct {
	mu  sync.Mutex
	got string
}

// quietCCLogf logs the QUIET links' congestion control when it changes
// (a var for the tests).
var quietCCLogf = coreLogf

// setQuietCC gives a QUIET link's socket the first congestion control of
// list (comma-separated) the kernel takes; "" when none (the socket keeps
// the links' own, BBR).
func setQuietCC(tc *net.TCPConn, list string) string {
	got := ""
	for _, cc := range strings.Split(list, ",") {
		cc = strings.TrimSpace(cc)
		if cc == "" {
			continue
		}
		if setCongestion(tc, cc) {
			got = cc
			break
		}
	}
	if tc != nil && list != "" {
		quietCCSeen.mu.Lock()
		changed := quietCCSeen.got != got
		quietCCSeen.got = got
		quietCCSeen.mu.Unlock()
		if changed {
			switch {
			case got == "":
				quietCCLogf("quiet: the kernel took none of %q for the QUIET links; they keep the links' own congestion control", list)
			case !strings.HasPrefix(list, got):
				quietCCLogf("quiet: QUIET links run %s (%q: the ones before it are not in this kernel)", got, list)
			default:
				quietCCLogf("quiet: QUIET links run %s", got)
			}
		}
	}
	return got
}

// QuietCCLoss (HS2_TUNE_QUIET_CC_LOSS; "": off): the congestion control a
// QUIET socket switches to while it sees random loss (above).
var QuietCCLoss = ""

// QuietLossPpm (HS2_TUNE_QUIET_LOSS_PPM): the share of retransmitted
// segments, per million sent over quietLossWin, that is random loss (150:
// 0.015%, half the field's 0.03%).
var QuietLossPpm = 150

const (
	// quietLossWin: the readings (one a second) the share is taken over:
	// a QUIET link of 1500 segments a second at the field's 0.03% resends
	// 4.5 in 10 s
	quietLossWin = 10
	// quietLossMin: the least retransmits in the window that count (round 5's
	// screen S3: with 3, a QUIET link of ~1000 segments a second at 0.03%
	// switched 2 to 24 s after its start, and a call that fell behind
	// meanwhile lost its realtime lane for good)
	quietLossMin = 2
	// quietLossQueue: the socket's mean srtt − min_rtt under which a loss
	// is the path's (CoDel drops a flow queue past 5 ms of sojourn)
	quietLossQueue = 4 * time.Millisecond
	// quietCalm: how long without random loss before the switch back (a
	// path that loses at random now and then keeps the switch: each switch
	// costs the calls a few seconds behind a cubic window)
	quietCalm = 2 * time.Minute
	// quietCCEvery: the watcher reads its socket each this many core ticks
	quietCCEvery = 10
)

// ccRead is one reading of a QUIET socket for the watcher.
type ccRead struct {
	retrans uint64
	segs    uint32
	q       time.Duration
}

// quietCCWatch is one QUIET socket's switch between its congestion control
// and QuietCCLoss.
type quietCCWatch struct {
	tc         *net.TCPConn
	base, loss string
	on         bool // running loss
	lossyAt    time.Duration
	hist       []ccRead
	switches   int
	tick       int
	// the latest window's share (ppm) and mean queue, for the status line
	ppm    uint64
	queue  time.Duration
	failed bool // the kernel refused loss
}

func newQuietCCWatch(tc *net.TCPConn, base, loss string) *quietCCWatch {
	return &quietCCWatch{tc: tc, base: base, loss: loss}
}

// due says whether this core tick reads the socket.
func (w *quietCCWatch) due() bool {
	w.tick++
	return w.tick%quietCCEvery == 0
}

// step takes one reading (each second) and switches when the window says.
func (w *quietCCWatch) step(now time.Duration, st tcpStat) {
	q := time.Duration(0)
	if st.rtt > st.minRTT {
		q = time.Duration(st.rtt-st.minRTT) * time.Microsecond
	}
	w.hist = append(w.hist, ccRead{retrans: st.retrans, segs: st.segsOut, q: q})
	if len(w.hist) > quietLossWin+1 {
		w.hist = w.hist[len(w.hist)-quietLossWin-1:]
	}
	if len(w.hist) < 2 {
		return
	}
	a, b := w.hist[0], w.hist[len(w.hist)-1]
	dr := b.retrans - a.retrans
	if b.retrans < a.retrans {
		dr = 0
	}
	ds := uint64(b.segs - a.segs) // wraps with the kernel's u32
	// the queue of the seconds that resent: a burst's CoDel drops come
	// with its queue, the path's random ones at any queue
	var qs time.Duration
	n := 0
	for i := 1; i < len(w.hist); i++ {
		if w.hist[i].retrans > w.hist[i-1].retrans {
			qs += w.hist[i].q
			n++
		}
	}
	w.queue = 0
	if n > 0 {
		w.queue = qs / time.Duration(n)
	}
	w.ppm = 0
	if ds > 0 {
		w.ppm = dr * 1_000_000 / ds
	}
	lossy := dr >= quietLossMin && ds > 0 && w.ppm >= uint64(QuietLossPpm)
	switch {
	case !w.on && lossy && w.queue < quietLossQueue && !w.failed:
		if setCongestion(w.tc, w.loss) {
			w.on, w.lossyAt = true, now
			w.switches++
			quietCCLogf("quiet: a QUIET link sees random loss (%.3f%% resent at a %.1f ms queue): %s until %s without it", float64(w.ppm)/1e4, msOf(w.queue), w.loss, quietCalm)
		} else {
			w.failed = true
			quietCCLogf("quiet: the kernel refused %q for a lossy QUIET link; it keeps %s", w.loss, w.base)
		}
	case w.on && dr >= 1 && w.ppm >= uint64(QuietLossPpm)/2:
		w.lossyAt = now
	case w.on && now-w.lossyAt >= quietCalm:
		if w.base == "" || setCongestion(w.tc, w.base) {
			w.on = false
			w.switches++
			quietCCLogf("quiet: a QUIET link has seen no random loss for %s: back to %s", quietCalm, w.base)
		}
	}
}

// line is the watcher's status line for link id.
func (w *quietCCWatch) line(id int) string {
	cc := w.base
	if w.on {
		cc = w.loss
	}
	return fmt.Sprintf("quiet cc link %d: %s (loss switch to %s; resent %.3f%% at a %.1f ms queue over %ds; switches %d)",
		id, cc, w.loss, float64(w.ppm)/1e4, msOf(w.queue), quietLossWin, w.switches)
}
