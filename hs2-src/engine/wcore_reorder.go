package engine

import (
	"math"
	"time"
)

// The link's reordering profile and the undo of a verdict of loss on holes
// alone (WINDOW_CORE §5.5, field round of step 1).
//
// Rcv_ooopack counts every packet that arrived behind a hole: a loss's,
// which stays open a round trip and more, or one a path that reorders opens
// for a few ms. lossHole tells them apart by how long the holes of a few
// wakes kept packets waiting together (T_base/4, RACK's own reordering
// window), which held for the red team's reordering of single packets. It
// does not hold for the real path: on the edge's 72 ms path to Iran, at 180
// to 1000 Mbit/s, the core drained 4 to 11 times per 90–120 s download, nearly
// all "packets arrived behind holes on a clean path" (and "... as the windows
// grew"), each a cut to the 2 MiB floor with a cap left for 10 minutes, while
// the exit's link sockets resent 0.000–0.002% (one download 180–338 Mbit/s
// with the core, 102 without). Direct TCP on the same path showed why: the
// receiver's Rcv_ooopack/data_segs_in per 0.5 s was 2% at 300 Mbit/s with
// bursts to 38% (5 of 29 half-seconds had any), 6% at 600 (to 35%), 15% at
// 1.7 Gbit/s (p50 20%, to 49%), 0–0.5% at a few Mbit/s, and the sender
// resent next to nothing: Linux's RACK does not resend a packet reordered by
// less than its window. Reordering there comes in bursts and grows with the
// rate, which is what "holes past their level as B grew" read as loss: a
// burst of 38% keeps packets behind holes 76 ms of two wakes, past T_base/4
// (18 ms) and past the round trip itself.
//
// What only the receiver sees cannot tell such a burst from a loss on a path
// whose round trip is under the runner's wake (100 ms): a loss's hole of
// ~1.25 round trips holds about as many packets as the burst. What tells
// them is what the sender did: the exit resends every packet it lost, and a
// reordered one it does not. On the edge every ~2 s record of the exit says
// how many segments it resent. So the edge learns, per link, how long its
// path's reordering keeps packets behind holes at a given rate, from windows
// of wakes the records showed held no loss (wcReo), and a hole is a loss's
// only past that (holeThr); and a cut or a drain whose only sign of loss was
// the holes is undone when the records that cover its hole and its resend
// show nothing resent (wcUndo), as TCP undoes a window it cut for a loss its
// DSACKs showed spurious. A record that did resend keeps both as they were:
// the policer's loss, any loss of ours, and a sender that resends reordered
// packets are judged as before, and with no record (the exit's side, an old
// exit, records that stopped or carry no TCP_INFO) nothing is learned or
// undone.
var (
	// wcReoMargin: a window's holes are a loss's past this many times what
	// the profile allows at its rate (one burst's share over two wakes is
	// no bound of the next one's).
	wcReoMargin = 1.5
	// wcReoCap: the profile raises lossHole's threshold to at most this share
	// of T_base (and 0.9 of the window's length): a loss's hole keeps the
	// packets that arrive behind it waiting a round trip at least (its resend
	// comes no sooner), so a window whose holes held less is no loss's once
	// the path is known to reorder, and one past it still is.
	wcReoCap = 0.75
	// wcReoTTL: a learned point of the profile holds this long (a route
	// change may take the reordering away).
	wcReoTTL = 3 * time.Minute
	// wcReoLearn: a window is kept for the profile only if its holes held
	// this share of the base threshold (T_base/4) or more: the profile only
	// matters near it.
	wcReoLearn = 0.5
)

// wcReoPts, wcReoWait: how many learned points the profile keeps, and how
// many windows wait for a record (64 wakes: ~6 s while the core is on).
const (
	wcReoPts  = 12
	wcReoWait = 64
)

// wcReoPt is a window of runner wakes (lossHole's: this wake and holeSpan
// before it): when it ended, how many segments a second arrived over it, and
// what share of its time its packets waited behind holes.
type wcReoPt struct {
	at    time.Time
	rate  float64
	share float64
}

// wcReo is the link's reordering profile: the windows the exit's records
// showed held no loss (pts), and the windows that wait for a record's
// verdict (wait).
type wcReo struct {
	pts   []wcReoPt
	wait  []wcReoPt
	dirty time.Time // windows that end before this may hold the hole of a loss a record resent
}

// share is the share of a window's time reordering keeps packets behind
// holes at rate r, by the profile: the largest learned share, scaled by r
// over the rate it was learned at where r is faster (the field: 2% at 300,
// 6% at 600, 15% at 1700 Mbit/s), at most 1.
func (p *wcReo) share(now time.Time, r float64) float64 {
	s := 0.0
	for _, x := range p.pts {
		if now.Sub(x.at) >= wcReoTTL {
			continue
		}
		v := x.share
		if x.rate > 0 && r > x.rate {
			v *= r / x.rate
		}
		s = math.Max(s, v)
	}
	return math.Min(s, 1)
}

// learn adds a window the records showed held no loss. A point it covers at
// every rate (as large a share, and as large a share per rate) goes; past
// wcReoPts the oldest does.
func (p *wcReo) learn(x wcReoPt) {
	k := 0
	for _, o := range p.pts {
		if x.at.Sub(o.at) >= wcReoTTL || x.share >= o.share && x.share*o.rate >= o.share*x.rate {
			continue
		}
		p.pts[k] = o
		k++
	}
	p.pts = append(p.pts[:k], x)
	if n := len(p.pts) - wcReoPts; n > 0 {
		p.pts = p.pts[:copy(p.pts, p.pts[n:])]
	}
}

// holeThr is how long the holes of a window of wakes (span long, segs
// segments in it) must keep packets waiting to be a loss's: T_base/4 (RACK's
// reordering window, wcOooHole), or, where the link's profile shows the path
// reorders, wcReoMargin times what it allows at the window's rate, up to
// wcReoCap of T_base and 0.9 of the window.
func (c *wcore) holeThr(now time.Time, segs uint64, span time.Duration) time.Duration {
	thr := time.Duration(wcOooHole * float64(c.tBase))
	if span <= 0 || segs == 0 {
		return thr
	}
	s := c.reo.share(now, float64(segs)/span.Seconds())
	if s <= 0 {
		return thr
	}
	lim := max(thr, min(time.Duration(wcReoCap*float64(c.tBase)), time.Duration(0.9*float64(span))))
	return min(max(thr, time.Duration(wcReoMargin*s*float64(span))), lim)
}

// reoLag is how long after a hole the exit's record that would hold its
// resend comes at the latest: the exit's RACK resends a lost packet a round
// trip and its reordering window (up to a round trip when DSACKs raised it)
// after it sent it, and the record leaves after that; two round trips of the
// exit's socket (its srtt has the queue in it) and a wake.
func (c *wcore) reoLag() time.Duration {
	return 2*max(c.recSrtt, c.tBase) + wcRampTick
}

// reoWindow keeps the wake's window for the profile, to be learned once the
// records that cover it show nothing resent (edge only: the exit's side has
// no record of the sender).
func (c *wcore) reoWindow(now time.Time, h time.Duration, segs uint64, span time.Duration) {
	if c.cfg.side != wcEdge || !c.sndSeen || span <= 0 || segs == 0 || now.Before(c.reo.dirty) ||
		h < time.Duration(wcReoLearn*wcOooHole*float64(c.tBase)) {
		return
	}
	c.reo.wait = append(c.reo.wait, wcReoPt{at: now, rate: float64(segs) / span.Seconds(), share: math.Min(float64(h)/float64(span), 1)})
	if n := len(c.reo.wait) - wcReoWait; n > 0 {
		c.reo.wait = c.reo.wait[:copy(c.reo.wait, c.reo.wait[n:])]
	}
}

// reoRecord takes a record of the exit that came in this wake (edge). One
// with resends: every window before it, and those of the next window's
// length, may hold the hole of a loss it resent (none of them is learned),
// and a verdict of loss on holes waiting for the records stands. One with
// none: the windows that ended reoLag or more before it held no loss (a
// resend of theirs would be in it or in one before it), and so did a
// verdict's hole that waits for it (undoDue). A record with no reading (an
// old exit, no TCP_INFO at the exit, nothing sent) says nothing.
func (c *wcore) reoRecord(in wcIn, span time.Duration) {
	s := in.snd
	if c.cfg.side != wcEdge || !s.fresh || s.srtt == 0 || s.minRTT == 0 || s.segsOut == 0 {
		return
	}
	now := in.now
	c.recSrtt = s.srtt
	if s.retrans > 0 {
		c.recRtxAt = now
		c.reo.dirty = now.Add(max(span, wcRampTick))
		c.reo.wait = c.reo.wait[:0]
		c.undo = wcUndo{}
		return
	}
	lag := c.reoLag()
	k := 0
	for _, w := range c.reo.wait {
		if now.Sub(w.at) >= lag {
			c.reo.learn(w)
			continue
		}
		c.reo.wait[k] = w
		k++
	}
	c.reo.wait = c.reo.wait[:k]
	if c.undo.on && !now.Before(c.undo.until) {
		c.undoDue = true
	}
}

// wcUndo is what a verdict of loss on holes alone changed (a cut between
// evaluations, or a drain whose only sign of loss was Rcv_ooopack), kept
// until the exit's records that cover its hole and its resend come.
type wcUndo struct {
	on     bool
	at     time.Time // the verdict
	last   time.Time // ... and the latest one since that waits with it
	until  time.Time // a record that came after this would hold the resend of its loss
	drains int       // the core's drains after the verdict: another one since voids it
	state  wcState
	B      uint64
	from   uint64 // PROBE: the budget the probe began at
	bCap   uint64
	bCapAt time.Time
	qcap   time.Duration
	qcapAt time.Time
	qcapOK bool
	back   int
	full   bool
	// STARTUP's comparison as it was
	fullR, prevR float64
	prevB        uint64
	flat, gear   int
}

// wcUndoMax: an undo still waiting this long after its verdict is dropped
// (the records stopped coming).
var wcUndoMax = 3 * wcSndQuiet

func (c *wcore) drainsN() int { return c.cnt.drainQ + c.cnt.drainLoss + c.cnt.drainRate }

// markUndo keeps what a verdict of loss on holes alone is about to change,
// on the edge with the exit's records coming (none to tell: nothing is kept).
// A record with resends that came within the round trip and a wake before it
// may hold its loss's resend already: nothing either. A second verdict while
// the first waits keeps the first's state and waits for its own records too.
func (c *wcore) markUndo(now time.Time) {
	if c.cfg.side != wcEdge || !c.sndSeen || c.sndQuiet(now) || c.state == wcOff {
		return
	}
	if !c.recRtxAt.IsZero() && now.Sub(c.recRtxAt) <= max(c.recSrtt, c.tBase)+wcRampTick {
		return
	}
	until := now.Add(c.reoLag())
	if c.undo.on {
		c.undo.last, c.undo.until = now, until
		c.undo.drains = -1 // set after the change (undoNoted)
		return
	}
	c.undo = wcUndo{on: true, at: now, last: now, until: until, drains: -1, state: c.state, B: c.B, from: c.probeFrom,
		bCap: c.bCap, bCapAt: c.bCapAt, qcap: c.qcap, qcapAt: c.qcapAt, qcapOK: c.qcapOK, back: c.backoff, full: c.pathFull,
		fullR: c.fullR, prevR: c.prevR, prevB: c.prevB, flat: c.flat, gear: c.gear}
	if c.state == wcProbe {
		c.undo.B = c.probeFrom
	}
}

// undoNoted takes the core's drains as the verdict left them: one more since
// voids the undo.
func (c *wcore) undoNoted() {
	if c.undo.on && c.undo.drains < 0 {
		c.undo.drains = c.drainsN()
	}
}

// undoCheck drops an undo whose verdict was followed by another drain (a
// queue, the path's rate, a loss the records showed), the core left for OFF,
// or the records stopped coming.
func (c *wcore) undoCheck(now time.Time) {
	u := &c.undo
	if !u.on {
		return
	}
	if c.state == wcOff || u.drains >= 0 && c.drainsN() != u.drains || now.Sub(u.at) > wcUndoMax {
		c.undo, c.undoDue = wcUndo{}, false
	}
}

// applyUndo undoes a verdict of loss on holes alone that the records showed
// was none: the loss point (bCap) and Q_cap it learned, if nothing renewed
// them since, the backoff and the full path, the loss drain, and the budget
// and state it took (STARTUP with its comparison as it was; a probe back to
// HOLD at the budget it began at). The published budget ramps back up.
func (c *wcore) applyUndo(in wcIn, m wcMeas, ev *[]wcEvent) {
	u := c.undo
	c.undo, c.undoDue = wcUndo{}, false
	if !u.on || c.state == wcOff {
		return
	}
	// (a cap or Q_cap set from the first verdict to the last is theirs: any
	// other loss of ours since came with a drain, which voids the undo)
	ours := func(t time.Time) bool { return !t.Before(u.at) && !t.After(u.last) }
	if ours(c.bCapAt) {
		c.bCap, c.bCapAt = u.bCap, u.bCapAt
	}
	if ours(c.qcapAt) {
		c.qcap, c.qcapAt, c.qcapOK = u.qcap, u.qcapAt, u.qcapOK
	}
	c.backoff, c.pathFull = u.back, u.full
	c.lossDrain, c.lossDrainN = false, 0
	if c.lossTest == wcSrcOoo {
		c.lossTest = wcSrcNone
	}
	from := c.B
	c.cnt.undo++
	if u.state == wcStartup {
		c.state = wcStartup
		c.refreshing = false
		c.fullR, c.prevR, c.prevB, c.flat, c.gear = u.fullR, u.prevR, u.prevB, u.flat, u.gear
		c.B = 0
		c.setB(in, m, u.B)
	} else {
		c.toHold(in, m)
		c.B = 0
		c.setB(in, m, u.B)
	}
	c.settling = true
	*ev = append(*ev, wcEvent{kind: wcEvUndo, from: from, B: c.B, n: len(m.f), T: c.tBase})
}
