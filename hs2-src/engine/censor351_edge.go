package engine

import (
	"sort"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/censor"
)

// censor351 slot 4: the reverse edge's (Iran listener's) own read-only path
// view for the detector.
//
// In the production layout (l3mtcp reverse) the Iran side holds no balancer:
// LB.measure runs only on the dialing Kharej side, so the Iran detector got
// no PathSample at all, and PeerLoss (O7) never reached either side (the
// Kharej readings have no peer figure). This pass reuses the TCP_INFO the
// health tick already reads for every accepted link (sampleHealth; no new
// syscall, no new per-packet work) and the peer's retransmits the control
// pong already carries (linkMeter.peerRetrans), groups the accepted links by
// (Iran IP = its local, Kharej IP = its remote) and hands one PathSample per
// pair to censor.NotePath at most every censorEdgeEvery.
//
// Chosen over carrying PeerLoss to Kharej in the pong because it changes no
// wire format, no peer and no timing: it is local, read-only, and runs only
// when censor.Active() (key off: the gate is one atomic load and m.cen stays
// nil, so nothing is allocated). Nothing reads what it computes except the
// detector, through the write-only hook.

const (
	censorEdgeEvery = 5 * time.Second  // one sample per pair per detector tick
	censorEdgeMax   = 30 * time.Second // a pair that never reaches the segment floor starts over
)

type censorEdge struct {
	last time.Time
	prev map[*managedLink]qPrev
	acc  map[censorPair]*censorEdgeAcc
}

type censorPair struct{ local, remote string }

type censorEdgeAcc struct {
	since             time.Time
	segs, retr, acked uint64
	busyWait          bool // some link had data in flight or backing off
	cp                censorPath
	delays            []float64
	links             int
	ticks             uint16
}

// censorEdgeObs is what the edge pass needs of one link this health tick.
type censorEdgeObs struct {
	ml *managedLink
	st tcpStat
}

// censorEdgeTick runs at the end of the health tick's socket reads (the
// reverse edge only, the detector on). obs are this tick's alive links with
// TCP_INFO.
func (m *LinkManager) censorEdgeTick(now time.Time, obs []censorEdgeObs) {
	if m.cen == nil {
		m.cen = &censorEdge{prev: map[*managedLink]qPrev{}, acc: map[censorPair]*censorEdgeAcc{}}
	}
	ce := m.cen
	dt := now.Sub(ce.last)
	if ce.last.IsZero() || dt <= 0 || dt > time.Minute {
		dt = healthTick
	}
	ce.last = now
	seen := make(map[*managedLink]bool, len(obs))
	for _, o := range obs {
		ml := o.ml
		if ml.mtr == nil {
			continue
		}
		lo, re := pathIPs(ml.mtr.path)
		if lo == "" || re == "" {
			continue
		}
		seen[ml] = true
		cur := qPrev{acked: o.st.bytesAcked, segsOut: uint64(o.st.segsOut), retrans: o.st.retrans}
		censorPrevFill(&cur, o.st, ml.mtr)
		prev, ok := ce.prev[ml]
		ce.prev[ml] = cur
		k := censorPair{lo, re}
		a := ce.acc[k]
		if a == nil {
			a = &censorEdgeAcc{since: now}
			ce.acc[k] = a
		}
		a.links++
		if o.st.unacked > 0 || o.st.backoff > 0 {
			a.busyWait = true
		}
		if !ok || cur.acked < prev.acked || cur.segsOut < prev.segsOut || cur.retrans < prev.retrans {
			continue
		}
		a.segs += cur.segsOut - prev.segsOut
		a.retr += cur.retrans - prev.retrans
		a.acked += cur.acked - prev.acked
		one := censorAcc{p: []censorPath{{}}}
		one.add(0, &cur, &prev, dt)
		p := one.p[0]
		a.cp.peerRt += p.peerRt
		a.cp.segsIn += p.segsIn
		a.cp.busy += p.busy
		a.cp.limited += p.limited
		a.cp.chronoN += p.chronoN
		if cur.acked-prev.acked >= qBusyBytes && o.st.minRttUs > 0 && o.st.rttUs >= o.st.minRttUs {
			a.delays = append(a.delays, float64(o.st.rttUs-o.st.minRttUs)/1000)
		}
	}
	for ml := range ce.prev {
		if !seen[ml] {
			delete(ce.prev, ml)
		}
	}
	for k, a := range ce.acc {
		if now.Sub(a.since) < censorEdgeEvery {
			continue
		}
		if a.links == 0 {
			delete(ce.acc, k) // no live link on the pair any more
			continue
		}
		p, ok := censorEdgeSample(k, a, now)
		if !ok {
			continue
		}
		censor.NotePath(p)
		*a = censorEdgeAcc{since: now}
	}
	for _, a := range ce.acc {
		a.links = 0
		if a.ticks < 1<<15 {
			a.ticks++
		}
	}
}

// censorEdgeSample turns one pair's accumulation into a sample. ok=false:
// not enough data yet (the accumulation goes on, up to censorEdgeMax).
func censorEdgeSample(k censorPair, a *censorEdgeAcc, now time.Time) (censor.PathSample, bool) {
	el := now.Sub(a.since)
	p := censor.PathSample{Local: censor.ParseAddr(k.local), Remote: censor.ParseAddr(k.remote), PeerLoss: -1, PathLimited: -1}
	if el > 0 {
		p.RateBps = float64(a.acked) / el.Seconds()
	}
	switch {
	case a.acked == 0 && a.busyWait && a.segs > 0:
		// Data waited and nothing was acked over the whole span: silent
		// (as the dialing side's lbSilent reading).
		p.Silent, p.Loss = true, 1
	case a.segs >= lbViewMinSegs: // as the status view's loss reading
		p.Loss = float64(a.retr) / float64(a.segs)
	default:
		if el >= censorEdgeMax {
			*a = censorEdgeAcc{since: now}
		}
		return p, false
	}
	if a.cp.segsIn >= censorMinSegs {
		p.PeerLoss = float64(a.cp.peerRt) / float64(a.cp.segsIn)
	}
	p.BusyLinks = a.cp.busy
	if a.ticks > 1 {
		p.BusyLinks = (a.cp.busy + a.ticks/2) / a.ticks // links busy per health tick, as one LB reading counts them
	}
	if a.cp.chronoN > 0 {
		p.PathLimited = a.cp.limited / float64(a.cp.chronoN)
	}
	if d := a.delays; len(d) > 0 {
		sort.Float64s(d)
		p.QueueMs = d[len(d)/2]
	}
	return p, true
}
