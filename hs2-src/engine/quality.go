package engine

import (
	"fmt"
	"math"
	"slices"
	"time"
)

// A poor link: one that works, but clearly worse than the pool's other
// links — a DPI box holding one flow's packets a few hundred milliseconds,
// a bad route, a deep queue somewhere on its path, delay spikes, or a few
// percent loss on a link that carries too little for the loss rule
// (activeBytes). None of the other rules sees it: it answers within lagWait,
// it is not stuck, and it is not degraded. Picked by load alone, it took its
// share of new users, and each of them paid its extra delay on every round
// trip (on the rig: a fifth of the new users on a link 800 ms slower than
// its three siblings, their first byte 0.9-1.7 s late instead of 0.07) or a
// stall for every lost segment (a short connection on a quiet link waits
// out a retransmit timeout).
//
// Every health tick the sampler notes, per link, what its socket and its
// control channel already say, at no cost on the wire:
//
//   - its round trip: the kernel's smoothed round trip of the link socket
//     (TCP_INFO), and the control channel's last round trip when a pong
//     came since the last tick (an idle link's smoothed round trip follows
//     a change slowly). Over the last qualTicks ticks the least of them is
//     the link's floor — its delay without its own queue, near enough: a
//     link carrying traffic still answers fast now and then, and BBR keeps
//     at most a round trip queued, so a busy link's floor stays within
//     twice its path's. Its high mark is the second largest of what a
//     segment may wait on it: the smoothed round trip plus four times its
//     mean deviation (the kernel's own retransmit timeout, without its
//     200 ms floor) in a tick that sent qualTimedSegs or more, or a
//     control round trip if longer — the delay spikes and the deep queues
//     that come and go;
//   - its resends, both ways (TCP_INFO for what this side sends, the
//     exit's count from each pong for the rest), against the data segments
//     sent, decayed over qualLossTau, so that the few segments of a quiet
//     link add up.
//
// Its own load is not held against it. The floor is what the path gives
// with its queue empty, near enough, busy or not. The high mark and the
// resends leave out the ticks in which the link moved activeBytes and more
// than qualHeavy times what the median serving link moved: a link busy
// with downloads of its own next to links that are not has its own queue,
// and resends what its rate costs (loss.go) — at the start of a download
// most of all, before its sender is seen pressed — and that says nothing
// about its path. A link with a deep queue or a lossy path moves no more
// than the others (it moves less), so it is not left out.
//
// A serving link is judged against the lower median of the serving links
// (lower, so one bad link of two is still told apart) — whatever most
// links meet is the path, not a link. It is poor when its floor is
// qualDelayAbs, and qualDelayRatio times the pool's floor, over the pool's
// floor; when its high mark is qualSpreadAbs, and qualSpreadRatio times the
// pool's floor, over the pool's high mark; or when it resent
// qualLossEvents segments or more, and more than qualLossFrac and
// qualLossRatio times the pool's share. It turns poor after qualOn ticks of
// that, and back once it has been poor for qualHold and stayed clear of
// half those margins for qualOff ticks — judged on its resends of the last
// qualCleanTau when it sent qualCleanSegs or more in them (resends decayed
// over a minute kept a link that had stopped resending poor for 2-3
// minutes with clean traffic on it). A poor link too quiet lately to say
// keeps qualQuietSegs of its older segments at most, so that they age out
// within about three minutes however busy it was: a link that had resent
// its own downloads' share into a shallow queue kept its verdict for over
// four minutes after they ended on the rig (seven on the model). Back, it
// forgets all but its recent resends: a few left over turned it poor again
// on its first new users' clean traffic.
//
// A poor link takes new users only when no serving link that is not poor
// (nor lagging) can, or when more than half the serving links were poor or
// lagging at the last tick (pickLocked). It keeps the users it has and
// stays in service.
const (
	qualTicks       = 10 // ticks the floor and high marks are taken over (20 s)
	qualSamplesMin  = 3  // fewer ticks are not judged, nor counted for the pool
	qualDelayAbs    = 150 * time.Millisecond
	qualDelayRatio  = 1.5
	qualSpreadAbs   = 400 * time.Millisecond
	qualSpreadRatio = 3.0
	qualLossTau     = time.Minute
	qualLossEvents  = 4
	qualLossFrac    = 0.01
	qualLossRatio   = 3.0
	qualLossSegsMin = 200 // segments sent to be judged, or counted for the pool
	qualHeavy       = 2.0 // a tick moving this many times the median link's is its own load
	qualTimedSegs   = 8   // data segments sent in a tick for its round trip deviation to count
	qualOn          = 2
	qualOff         = 3
	qualHold        = 20 * time.Second
	qualCleanTau    = 10 * time.Second              // what a poor link resent lately ...
	qualCleanSegs   = qualLossEvents / qualLossFrac // ... judged on this many segments or more
	qualQuietSegs   = 1500                          // a poor link too quiet for that keeps no more (2 min to age out)
)

// linkQual is the sampler's quality record of one link (under m.mu).
type linkQual struct {
	lo, hi  [qualTicks]uint32 // per tick, the least round trip seen and the longest wait (µs; 0: none)
	own     [qualTicks]bool   // ... and whether it was the link's own load (qualHeavy)
	n       int               // ticks noted
	moved   float64           // what it moved in the last tick, both ways (bytes per healthTick)
	pongs   uint64            // the control round trips read when last noted (peerLossRec.n)
	peer    peerLossRec       // the pong the download resends were counted up to
	resent  float64           // resends, both ways, decayed over qualLossTau
	segs    float64           // ... and the data segments sent
	rResent float64           // the same two decayed over qualCleanTau: what
	rSegs   float64           // ... a poor link sent lately (turning back)
	on, off int               // ticks its verdict held, or stayed clear, in a row
	since   time.Time         // when it turned poor
}

// noteQual records one tick of a sampled link that moved moved bytes per
// healthTick, its own load if over heavy too (qualHeavy): its round trips,
// and unless its own load, its resends (dUp of dSegsOut segments sent, 0:
// bytes / mss instead) and the exit's since the last pong. Caller holds
// m.mu.
func (ml *managedLink) noteQual(o *linkObs, dt time.Duration, dWr, dUp uint64, dSegsOut uint32, moved, heavy float64) {
	if ml.q == nil {
		ml.q = &linkQual{}
	}
	q := ml.q
	var lo, hi uint32
	if o.tsOK && o.ts.rtt > 0 {
		lo = o.ts.rtt
		// The deviation is current only if this side sent enough to be
		// timed this tick: on a link left idle, a round trip that just got
		// shorter takes the smoothed one a minute of keepalives to follow.
		if dSegsOut >= qualTimedSegs || o.ts.segsOut == 0 && dWr >= ctrlBusyBytes {
			hi = o.ts.rtt + 4*o.ts.rttVar
		}
	}
	fresh := o.peerLoss != nil && o.peerLoss.n != q.pongs
	if fresh {
		q.pongs = o.peerLoss.n
		if us := ml.mtr.rttMicros.Load(); us > 0 && us < math.MaxUint32 {
			if lo == 0 || uint32(us) < lo {
				lo = uint32(us)
			}
			hi = max(hi, uint32(us))
		}
	}
	own := moved >= activeBytes && moved > heavy
	i := q.n % qualTicks
	q.lo[i], q.hi[i], q.own[i] = lo, hi, own
	q.n++
	q.moved = moved

	decay := math.Exp(-dt.Seconds() / qualLossTau.Seconds())
	q.resent *= decay
	q.segs *= decay
	decay = math.Exp(-dt.Seconds() / qualCleanTau.Seconds())
	q.rResent *= decay
	q.rSegs *= decay
	if o.tsOK && !own {
		segs := float64(dSegsOut)
		if segs == 0 {
			segs = float64(dWr)/mss + float64(dUp)
		}
		q.add(float64(dUp), segs)
	}
	if fresh {
		r, p := o.peerLoss, q.peer
		q.peer = *r
		if p.n != 0 && r.rt >= p.rt && r.rd >= p.rd && !own {
			dR := r.rt - p.rt
			segs := float64(r.segsIn - p.segsIn)
			if r.segsIn == 0 || p.segsIn == 0 {
				segs = float64(r.rd-p.rd) / mss
			}
			q.add(float64(dR), segs+float64(dR))
		}
	}
}

// add counts resent of segs data segments sent, in both windows.
func (q *linkQual) add(resent, segs float64) {
	q.resent += resent
	q.segs += segs
	q.rResent += resent
	q.rSegs += segs
}

// lossShare is the resent share, -1 if too few segments to say.
func (q *linkQual) lossShare() float64 {
	if q.segs < qualLossSegsMin {
		return -1
	}
	return q.resent / q.segs
}

// qualView is one link's quality reading in this tick.
type qualView struct {
	ml        *managedLink
	floor, hi time.Duration // 0: too few ticks
	loss      float64       // the resent share, -1: too few segments
	resent    float64       // ... out of this many resends
}

// view is the link's floor and high mark over the last qualTicks ticks (0:
// too few of them; the high mark leaves out its own load).
func (q *linkQual) view() (floor, hi time.Duration) {
	var lo, top, next uint32 // the least lo, the largest hi and the one after it
	nlo, nhi := 0, 0
	for i := range min(q.n, qualTicks) {
		if q.lo[i] == 0 {
			continue
		}
		if nlo++; lo == 0 || q.lo[i] < lo {
			lo = q.lo[i]
		}
		if h := q.hi[i]; !q.own[i] && h > 0 {
			nhi++
			if h > top {
				top, next = h, top
			} else if h > next {
				next = h
			}
		}
	}
	if nlo < qualSamplesMin {
		return 0, 0
	}
	if nhi >= qualSamplesMin {
		hi = time.Duration(next) * time.Microsecond
	}
	return time.Duration(lo) * time.Microsecond, hi
}

// qualPool is what the serving links make together: the lower median of
// their floors, high marks and resent shares, and how many links each
// comes from.
type qualPool struct {
	floor, hi          time.Duration
	loss               float64
	nFloor, nHi, nLoss int
}

// lowerMedian is the lower median of v (sorted in place), 0 if empty.
func lowerMedian[T ~int64 | ~float64](v []T) T {
	if len(v) == 0 {
		return 0
	}
	slices.Sort(v)
	return v[(len(v)-1)/2]
}

// judgeQuality turns serving links poor or back (see the top of this file)
// against the pool's other serving links — none poor while the path is
// slow for most links or getting over it (recovering, stuck.go): links back
// off unevenly through a squeeze, and what each does then is not its own —
// and notes what the median one moved (the next tick's qualHeavy). Caller
// holds m.mu; it returns the log lines.
func (m *LinkManager) judgeQuality(now time.Time, recovering bool) []string {
	n := len(m.links)
	views := make([]qualView, 0, n)
	floors, his := make([]time.Duration, 0, n), make([]time.Duration, 0, n)
	losses, moved := make([]float64, 0, n), make([]float64, 0, n)
	for _, ml := range m.links {
		if !ml.serving() || ml.q == nil {
			continue
		}
		v := qualView{ml: ml, loss: ml.q.lossShare(), resent: ml.q.resent}
		v.floor, v.hi = ml.q.view()
		if v.floor > 0 {
			floors = append(floors, v.floor)
		}
		if v.hi > 0 {
			his = append(his, v.hi)
		}
		if v.loss >= 0 {
			losses = append(losses, v.loss)
		}
		moved = append(moved, ml.q.moved)
		views = append(views, v)
	}
	m.qualMoved = 0
	if len(moved) > 0 {
		slices.Sort(moved)
		m.qualMoved = moved[len(moved)/2]
	}
	p := qualPool{floor: lowerMedian(floors), hi: lowerMedian(his), loss: lowerMedian(losses),
		nFloor: len(floors), nHi: len(his), nLoss: len(losses)}
	var logs []string
	for _, v := range views {
		why := p.why(v, 1)
		q := v.ml.q
		if !v.ml.poor {
			if why == "" || recovering {
				q.on = 0
				continue
			}
			if q.on++; q.on >= qualOn {
				v.ml.poor, q.since, q.off = true, now, 0
				logs = append(logs, fmt.Sprintf("link %d: %s — new connections go to the other links while they have room", v.ml.id, why))
			}
			continue
		}
		recent := q.rSegs >= qualCleanSegs
		if recent {
			v.loss, v.resent = q.rResent/q.rSegs, q.rResent
		} else if q.segs > qualQuietSegs {
			keep := qualQuietSegs / q.segs
			q.resent, q.segs = keep*q.resent, qualQuietSegs
			v.resent = q.resent
		}
		if p.why(v, 0.5) != "" {
			q.off = 0
			continue
		}
		if q.off++; q.off >= qualOff && now.Sub(q.since) >= qualHold {
			v.ml.poor, q.on = false, 0
			q.resent, q.segs = q.rResent, q.rSegs // the older resends are past
			logs = append(logs, fmt.Sprintf("link %d: on par with the other links again after %s (round trip %dms at best)",
				v.ml.id, fmtDur(now.Sub(q.since)), v.floor.Milliseconds()))
		}
	}
	// What pickLocked goes by until the next tick: the links it passes
	// over, poor or lagging, of the serving ones.
	m.qualPoor, m.qualLag, m.qualServing = 0, 0, len(views)
	ctrl := ctrlNow()
	for _, v := range views {
		if v.ml.poor {
			m.qualPoor++
		} else if v.ml.lagging(ctrl) {
			m.qualLag++
		}
	}
	return logs
}

// why says why link v is poor against the pool, with the margins scaled
// by k; "" if it is not.
func (p qualPool) why(v qualView, k float64) string {
	margin := func(abs time.Duration, ratio float64) time.Duration {
		return time.Duration(k * float64(max(abs, time.Duration(ratio*float64(p.floor)))))
	}
	switch {
	case p.nFloor >= 2 && v.floor > 0 && v.floor-p.floor >= margin(qualDelayAbs, qualDelayRatio):
		return fmt.Sprintf("round trip %dms at best where the other links take %dms", v.floor.Milliseconds(), p.floor.Milliseconds())
	case p.nFloor >= 2 && p.nHi >= 2 && v.hi > 0 && v.hi-p.hi >= margin(qualSpreadAbs, qualSpreadRatio):
		return fmt.Sprintf("round trip up to %dms where the other links stay under %dms", v.hi.Milliseconds(), p.hi.Milliseconds())
	case p.nLoss >= 2 && v.loss >= 0 && v.resent >= k*qualLossEvents && v.loss > k*max(qualLossFrac, qualLossRatio*p.loss):
		return fmt.Sprintf("resends %.1f%% of what it sends where the other links resend %.1f%%", 100*v.loss, 100*p.loss)
	}
	return ""
}
