package engine

import (
	"fmt"
	"sort"
	"time"
)

// The loss rule: a link resending a large share of what it sends while it
// moves data (activeBytes) is degraded and drained like a stuck one. Two
// things made it misjudge busy links at high bandwidth, where hundreds of
// links each run into a per-connection throttle that drops what goes over
// its rate (the load test: 3 Mbit/s each, 150 downloads over 300 links —
// 28 to 40 links drained in 2.5 min, ~12 a minute, and their users cut, on
// this build and on the one before it alike):
//
//   - The download loss was the exit's retransmits from the last pong over
//     the bytes read in the 2 s health tick. Pongs come every 3 s, so one
//     tick in three had none (no loss, the streak reset) and the others
//     held 3 s — or 6 s — of resends over 2 s of bytes: with steady pongs a
//     link resending 30-90% was never drained, and with pongs jittering
//     behind the link's own queue, as on every busy link at peak, one
//     resending 9% was (16 of 20 in 5 min). It is now judged over the window
//     between two pongs, against what came in over that same window, each
//     direction with its own streak.
//   - The denominator was payload bytes / mss, while retransmits count TCP
//     segments: a link of small or padded frames read 1.1-10× lossier than
//     it was. It is now the socket's own segment count (TCP_INFO data
//     segments, Linux ≥ 4.6; bytes / mss where the kernel lacks it).
//
// And it judged each link alone, with no limit: a throttle every
// connection meets, or a lossy path, drained every busy link it touched,
// and their users came back onto links resending just as much. Now:
//
//   - when most busy links (lossPathMin or more judged in the tick) resend
//     more than lossFrac, it is the path or a throttle every connection
//     meets, not those links: none is drained (said once a minute);
//   - otherwise a link is drained only if it resends more than lossFrac and
//     more than lossRelative times what the busy links resend at the median
//     — a link worse than the ones its users would move to;
//   - a link that still moves at least lossKeepShare of what the pressed
//     links move (those whose sender waits for the path: what a link gets)
//     is kept — its resends are what the path's rate costs (a throttle
//     dropping what goes over it), not a fault, and its users would get no
//     more elsewhere. A link losing so much that it moves less is drained.
//     In the load test the exact fractions alone still drained 25 links in
//     2.5 min (resending 14-28% at the throttle's rate while the median
//     busy link resent 3-10%);
//   - at most drainHeadroom degraded links (lossy or stuck) drain at a time,
//     the lossiest first.
const (
	// lossRelative: how many times the busy links' median a link must
	// resend to be drained (when lossPathMin or more were judged).
	lossRelative = 2.0
	// lossPathMin: below this many links judged in a tick, neither the
	// path-wide nor the relative test applies — two or three busy links at
	// night say nothing about the path.
	lossPathMin = 4
	// lossWinMin: a download window shorter than this (two pongs read
	// together after a wait) stays open for the next pong — a few hundred
	// milliseconds of segments make a noisy fraction.
	lossWinMin = time.Second
	// lossKeepShare: a lossy link moving at least this share of the median
	// pressed link's rate is kept (when lossPathMin or more are pressed).
	lossKeepShare = 0.5
)

// lossDir is one direction's loss reading for a tick.
type lossDir struct {
	judged bool
	resent uint64  // segments resent
	segs   float64 // segments sent, resends included (or bytes / mss + resends)
	rate   float64 // what it moved meanwhile, bytes/s
}

func (d lossDir) frac() float64 {
	if !d.judged || d.segs <= 0 {
		return 0
	}
	return float64(d.resent) / d.segs
}

func (d lossDir) bad() bool { return d.judged && d.frac() > lossFrac }

func (d lossDir) String() string {
	if !d.judged {
		return "—"
	}
	return fmt.Sprintf("%.0f%% of %.0f segments", d.frac()*100, d.segs)
}

// judgeUpLoss reads this tick's upload: resends (local TCP_INFO) over the
// data segments sent, resends included (dSegsOut; 0 when the kernel does not
// count them: bytes / mss plus the resends instead). Judged only while it
// moves activeBytes.
func judgeUpLoss(tsOK bool, dWr, dUp uint64, dSegsOut uint32, secs float64) lossDir {
	if !tsOK || dWr < activeBytes {
		return lossDir{}
	}
	segs := float64(dSegsOut)
	if segs == 0 {
		segs = float64(dWr)/mss + float64(dUp)
	}
	return lossDir{judged: true, resent: dUp, segs: segs, rate: float64(dWr) / secs}
}

// judgeDownLoss reads the download over the window since the pong that
// opened it, once a later pong has closed it (fresh): the exit's resends
// over the data segments this side received meanwhile plus those resends
// (what the exit sent, near enough: a lost segment is resent and the resend
// comes in), or bytes / mss where the kernel does not count segments.
// Judged only if the window brought activeBytes a tick. Caller holds m.mu.
func (ml *managedLink) judgeDownLoss(r *peerLossRec) (d lossDir, fresh bool) {
	if r == nil || r.n == ml.prevPeer.n {
		return lossDir{}, false
	}
	p := ml.prevPeer
	if p.n == 0 || r.n < p.n || r.rt < p.rt || r.rd < p.rd || r.at <= p.at {
		ml.prevPeer = *r // the first pong, or out of step: a window starts here
		return lossDir{}, false
	}
	win := time.Duration(r.at - p.at)
	if win < lossWinMin {
		return lossDir{}, false // stays open for the next pong
	}
	ml.prevPeer = *r
	dR, dRd := r.rt-p.rt, r.rd-p.rd
	if float64(dRd) < float64(activeBytes)*win.Seconds()/healthTick.Seconds() {
		return lossDir{}, true // closed, but too little came in to judge
	}
	segs := float64(r.segsIn - p.segsIn)
	if r.segsIn == 0 || p.segsIn == 0 {
		segs = float64(dRd) / mss
	}
	return lossDir{judged: true, resent: dR, segs: segs + float64(dR), rate: float64(dRd) / win.Seconds()}, true
}

// lossObs is a link the loss rule found bad degradeStreak samples running,
// judged after the loop: not if this very tick turns out slow.
type lossObs struct {
	ml     *managedLink
	up, dn lossDir
}

func (c lossObs) frac() float64 { return max(c.up.frac(), c.dn.frac()) }

// rate is what the link moved in its lossy direction(s), bytes/s.
func (c lossObs) rate() float64 {
	switch {
	case c.up.bad() && c.dn.bad():
		return min(c.up.rate, c.dn.rate)
	case c.up.bad():
		return c.up.rate
	}
	return c.dn.rate
}

// lossVerdicts degrades the loss candidates of this tick that resend well
// above the busy links judged with them (judged: every judged link's loss
// fraction) and move less than lossKeepShare of what the pressed links move
// (pressed: their rates), at most drainHeadroom degraded links at a time
// (degradedNow: those already draining), and says why when it drains none
// because most busy links resend that much. Caller holds m.mu; it returns
// the log lines.
func (m *LinkManager) lossVerdicts(now time.Time, cand []lossObs, judged, pressed []float64, degradedNow int) []string {
	if len(cand) == 0 {
		return nil
	}
	bar, med := lossFrac, 0.0
	if len(judged) >= lossPathMin {
		lossy := 0
		for _, f := range judged {
			if f > lossFrac {
				lossy++
			}
		}
		if lossy*2 > len(judged) {
			if now.Sub(m.lossPathLogAt) >= time.Minute {
				m.lossPathLogAt = now
				return []string{fmt.Sprintf("%d of %d busy links resend more than %.0f%% — the path (or a throttle every connection meets) is lossy, not those links: none is drained",
					lossy, len(judged), lossFrac*100)}
			}
			return nil
		}
		s := append([]float64(nil), judged...)
		sort.Float64s(s)
		med = s[len(s)/2]
		bar = max(bar, lossRelative*med)
	}
	keep := 0.0 // a link moving this much is at its path's rate (bytes/s)
	if len(pressed) >= lossPathMin {
		s := append([]float64(nil), pressed...)
		sort.Float64s(s)
		keep = lossKeepShare * s[len(s)/2]
	}
	sort.Slice(cand, func(i, j int) bool { return cand[i].frac() > cand[j].frac() })
	room := drainHeadroom(m.max) - degradedNow
	var logs []string
	for _, c := range cand {
		if c.frac() <= bar || keep > 0 && c.rate() >= keep || room <= 0 {
			continue
		}
		room--
		c.ml.degraded, c.ml.pressed = true, false
		logs = append(logs, fmt.Sprintf("link %d degraded (up-loss %v, down-loss %v, rtt %dms; busy links resend %.0f%% at the median) — draining",
			c.ml.id, c.up, c.dn, c.ml.mtr.rttMicros.Load()/1000, med*100))
	}
	return logs
}
