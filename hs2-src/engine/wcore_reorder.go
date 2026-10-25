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
// Three things tell them apart here, each where it can:
//
//   - The hole itself (wcBacklog, the receiver's own counters): a loss's hole
//     stays open until its resend comes, a round trip and RACK's window
//     after it opened; one that reordering opens is gone within RACK's
//     window (else the sender resends it). The hole still open at a wake's end
//     shows in what passed it (tcpi_bytes_received against Data_segs_in, the
//     same TCP_INFO): open past T_base/4, by an age the estimate trusts, it is
//     a loss's whatever else says (lossHole). It sees a loss's hole at a wake's
//     end only if one falls in it: always where the round trip is a wake and
//     T_base/4 or longer, at 72 ms in about 7 of 10 (more with a queue in the
//     round trip, as a loss of ours has).
//   - The link's reordering profile (wcReo): the share of a window's time its
//     holes kept packets waiting, at the window's rate, in windows shown to
//     hold no loss; lossHole's sum of holes is a loss's only past wcReoMargin
//     times what the profile allows at the rate (holeThr). The proof is the
//     exit's records on the edge: the exit resends every packet it lost and a
//     reordered one it does not, and its ~2 s record says how many it resent
//     (a window is learned once the records that would hold its loss's resend
//     came with none). With no record (the exit's side) the proof is the
//     receiver's own where it is one: from two wakes of round trip a loss's
//     hole is always seen open (reoSelf). Where wcBacklog judges the
//     holes, the profile may take the sum to a window's length (its episodes
//     of the model of the field keep half the packets behind holes for 120
//     to 200 ms: two wakes' sum of a loss's at 72 ms); where it does not, to
//     wcReoCap of T_base, under what a loss's hole keeps.
//   - The undo (wcUndo): a cut or a drain whose only sign of loss was the
//     holes is undone when the records that cover its hole and its resend
//     show nothing resent, as TCP undoes a window it cut for a loss its DSACKs
//     showed spurious, and the profile learns its windows.
//
// A record that did resend keeps everything as it was: the policer's loss,
// any loss of ours, and a sender that resends reordered packets (Linux before
// its first DSACK) are judged as before. A path that does not reorder learns
// nothing and its holes are judged as before (the model's table is the same,
// byte for byte).
//
// On the reproduction of the field (wcore_s2_reorder_test.go, 1000/72 behind
// 50 ms with its reordering and a RACK sender, three seeds) the core of
// before kept 106 of its 624 Mbit/s gain over base, with 8 drains for loss
// and 5 cuts, the exit resending 0.000%; this one keeps 482 (the cuts come
// before the profile has learned the path, in its first ~15 s, and are undone).
var (
	// wcReoMargin: a window's holes are a loss's past this many times what
	// the profile allows at its rate (one burst's share over two wakes is
	// no bound of the next one's).
	wcReoMargin = 1.5
	// wcReoCap: with no wcBacklog to judge the holes, the profile raises
	// lossHole's threshold to at most this share of T_base (and 0.9 of the
	// window's length): a loss's hole keeps the packets that arrive behind it
	// waiting a round trip at least (its resend comes no sooner), so a window
	// whose holes held less is no loss's once the path is known to reorder,
	// and one past it still is.
	wcReoCap = 0.75
	// wcReoTTL: a learned point of the profile holds this long, as a loss
	// point does (wcQcapTTL): reordering is the path's, and its episodes
	// renew the profile while it lasts (the model of the field's: one every
	// 0.5 to 3 s at 300 to 1000 Mbit/s); a route change may take it away, and
	// then a loss's hole is told by wcBacklog and the exit's records until the
	// profile ages out.
	wcReoTTL = 10 * time.Minute
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

// wcReo is the link's reordering profile: the windows shown to hold no loss
// (pts), the windows that wait for a record's verdict (wait), and those that
// wait for the receiver's own (self: reoSelf).
type wcReo struct {
	pts   []wcReoPt
	wait  []wcReoPt
	self  []wcReoCand
	dirty time.Time // windows that end before this may hold the hole of a loss a record resent
	// selfBad: the latest wake the receiver's own counters could not judge,
	// or that found a loss's hole (no window with it in it is learned)
	selfBad time.Time
}

// wcReoCand is a window that waits for the receiver's own verdict until
// until.
type wcReoCand struct {
	wcReoPt
	until time.Time
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
	lim := time.Duration(0.9 * float64(span))
	if !c.bk.ok {
		// no hole of the receiver's own to tell a loss by: no more than a
		// loss's hole keeps packets waiting (a round trip, less what the
		// sender held back)
		lim = min(lim, time.Duration(wcReoCap*float64(c.tBase)))
	}
	return min(max(thr, time.Duration(wcReoMargin*s*float64(span))), max(thr, lim))
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

// reoSelf learns the profile from the receiver's own counters, with no
// record of the sender (the exit's side; the edge too, sooner than its
// records): a window whose holes summed past T_base/4 with no hole open past
// it at its end is reordering if, in the wakes after it, none stays open
// past T_base/4 either. A loss's hole stays open a round trip at the least
// (its resend comes no sooner), so where the round trip is two wakes or more
// the hole spans a whole wake, in which nothing passes it: wcBacklog's age
// grows by the wake's length there, whatever the pace the packets came at
// (openAny). An age it measures within a wake takes the packets as evenly
// spread over it; a window-limited sender's bursts, and a policer that drops
// their tails, leave a hole of a round trip with few packets behind it, an
// age of a few ms at the wake's end: with three wakes to a round trip
// (0.75·T_base past a wake, the first rule) such holes went for reordering,
// the profile took the sum past them, and the red team's policer cells at
// 150 ms lost up to 28% (1000/150 q50 pol90>pol40: 269 → 195 Mbit/s, resent
// 0.97 → 1.50%). Under two wakes (the edge's 72 ms path to Iran) only the
// exit's records tell.
func (c *wcore) reoSelf(now time.Time, w wcHoleWin, dt time.Duration) {
	sure := c.bk.ok && c.tBase >= 2*dt
	if !sure || c.bk.openAny(c.tBase) {
		// no proof (or a loss's hole): none of the windows waiting, nor one
		// with this wake in it, is learned
		c.reo.self, c.reo.selfBad = c.reo.self[:0], now
		return
	}
	k := 0
	for _, x := range c.reo.self {
		if !now.Before(x.until) {
			c.reo.learn(x.wcReoPt)
			continue
		}
		c.reo.self[k] = x
		k++
	}
	c.reo.self = c.reo.self[:k]
	thr := time.Duration(wcOooHole * float64(c.tBase))
	if w.span > 0 && w.segs > 0 && w.hole >= thr && w.ooo >= uint64(wcOooMinSegs) && len(c.reo.self) < wcReoWait &&
		now.Sub(c.reo.selfBad) > w.span {
		// (a hole that opened as the window ended is past T_base/4 at the
		// end of the wake after T_base/4, if it lasts: a loss's does)
		c.reo.self = append(c.reo.self, wcReoCand{wcReoPt{at: now, rate: float64(w.segs) / w.span.Seconds(),
			share: math.Min(float64(w.hole)/float64(w.span), 1)}, now.Add(thr + 2*dt)})
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
		c.undo.until = until
		c.undo.drains = -1 // set after the change (undoNoted)
		return
	}
	c.undo = wcUndo{on: true, at: now, until: until, drains: -1, state: c.state, B: c.B, from: c.probeFrom,
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
	// (a cap or Q_cap set since the first verdict is theirs, or the test of
	// a jump's (lossTestDone: back at its level at the floor, as a path whose
	// reordering grows with the rate reads there): any other loss of ours
	// since came with a drain, which voids the undo, or with resends, which
	// drop it)
	ours := func(t time.Time) bool { return !t.Before(u.at) }
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

// wcBacklog is the receiver's own view of the oldest hole at the latest
// runner wake: how many segments arrived behind it and wait (bk), and how
// long it has been open (age). A loss's hole stays open until its resend
// comes, a round trip and RACK's reordering window (T_base/4) after the
// packets that showed it at the least; a hole a path that reorders opens is
// gone once the late packet comes, within RACK's window (else the sender
// resends it). Rcv_ooopack alone sums every hole of a wake, many short ones
// as one long one; the hole still open at the wake's end shows in what went
// in order: of the segments that came (Data_segs_in) the payload that passed
// every hole (tcpi_bytes_received, in the same TCP_INFO) is short by what
// waits behind the oldest one.
//
// Bytes and segments meet through segB, the payload of a segment, learned
// from wakes with no hole at either end. Its error adds up over wakes in
// which a hole stays open and something passes (the late packets of one hole
// come, another opens: dense reordering), so the age is trusted only when it
// was measured from a wake that ended with no hole open, through wakes in
// which nothing passed (the same hole, a wake older each: exact).
type wcBacklog struct {
	segB    float64       // payload bytes per segment
	clean   bool          // the previous wake had no segment behind a hole
	ok      bool          // segB is known and the wake had both counters
	bk      float64       // segments waiting behind the oldest hole (0: none open)
	age     time.Duration // how long it has been open
	trusted bool          // ... measured from a wake that ended with none
}

// wcBkAlpha: segB's smoothing over wakes with no hole.
const wcBkAlpha = 0.1

// wake takes a runner wake dt long.
func (b *wcBacklog) wake(in wcIn, dt time.Duration) {
	if !in.oooOK || !in.inOrderOK {
		*b = wcBacklog{segB: b.segB}
		return
	}
	a, o, d := float64(in.segsIn), float64(in.ooo), float64(in.inOrder)
	if o == 0 && b.clean && a >= float64(2*wcOooMinSegs) {
		// no hole at either end of it: what came went in order
		if r := d / a; b.segB == 0 {
			b.segB = r
		} else {
			b.segB += wcBkAlpha * (r - b.segB)
		}
	}
	if a > 0 {
		b.clean = o == 0
	}
	if b.segB <= 0 {
		b.ok, b.bk, b.age, b.trusted = false, 0, 0, false
		return
	}
	b.ok = true
	prev, prevAge := b.bk, b.age
	if o == 0 && a > 0 {
		// all that came went in order: no hole open at the wake's end
		b.bk, b.age, b.trusted = 0, 0, true
		return
	}
	if prev > 0 && d < 4*b.segB {
		// nothing passed the hole open at the wake before: the same one, a
		// wake older
		b.bk, b.age = prev+a, prevAge+dt
		return
	}
	bk := prev + a - d/b.segB
	if bk < math.Max(4, 0.02*a) {
		// what waits is within segB's noise: no hole open at the wake's end
		b.bk, b.age, b.trusted = 0, 0, true
		return
	}
	// what waited at the wake before passed (or none did): the segments
	// behind the hole open now came since it opened, at the wake's pace,
	// and behind a hole (no more than the wake's Rcv_ooopack: segB's error
	// adds up to no more than that)
	b.trusted = prev == 0 && b.trusted
	bk = math.Min(bk, o)
	b.bk, b.age = bk, time.Duration(math.Min(bk/a, 1)*float64(dt))
}

// open says whether a hole has been open past RACK's reordering window
// (T_base/4, wcOooHole), wcOooMinSegs segments or more behind it, by an age
// the estimate trusts: a loss's, by the receiver's own signs.
func (b *wcBacklog) open(tBase time.Duration) bool {
	return b.trusted && b.openAny(tBase)
}

// openAny is open by any age the estimate has, trusted or not: what may be a
// loss's hole (no window with it is learned as reordering, reoSelf).
func (b *wcBacklog) openAny(tBase time.Duration) bool {
	return b.ok && b.bk >= float64(wcOooMinSegs) && b.age >= time.Duration(wcOooHole*float64(tBase))
}
