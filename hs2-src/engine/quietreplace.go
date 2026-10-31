package engine

import (
	"fmt"
	"slices"
	"sync/atomic"
	"time"
)

// R1 round 5: a QUIET link that is alive but slow is replaced, make before
// break (the independent engine review's B2, docs/architecture/wip/r1/
// review/B-engine.fa.md; round5/REPORT.fa.md).
//
// The BULK pool replaces a soft-bad link (heal) and passes a poor or lagging
// one over for new users (quality.go); the QUIET set replaced a link only
// once it was dead. A DPI box that throttles or delays the one QUIET
// connection (its five-tuple) — or a connection stuck in resends that does
// not die — held every new connection of every user, the latency lane and
// the TUN's new flows on it for good (with max_links <= 5 there is one
// QUIET link); production's pool would have moved them to a fresh link.
//
// With QuietReplace the edge judges each QUIET link every pool tick against
// its BULK links, which share its path: slow when, for quietSlowTicks ticks
// in a row,
//
//   - its control round trip at best (rttMin, the last eight pongs) is
//     qualDelayAbs, and qualDelayRatio times theirs, over theirs (the lower
//     median of the serving BULK links): the BULK pool's own margins;
//   - or it resends (both ways: this end's TCP_INFO, the exit's count from
//     each pong) qualLossFrac of what it sends and qualLossRatio times what
//     they do, at least qualLossEvents segments, decayed over replLossTau:
//     a policer on its five-tuple drops its packets alone, while the path's
//     random loss hits every link alike;
//   - or it lags (meterLagging: a control ping unanswered, its writer stuck,
//     nothing read for lagQuiet) while a BULK link does not.
//
// A slow link is retired: no connection opens on it (pick passes retiring
// links over), the set wants one QUIET link more for it (wantNow: the
// reverse edge offers CapQuiet on the next link, and the pool asks the exit
// for one link more, ctlTarget; a direct edge dials it), the connections on
// it move to the new one once it is up (quietTick: with CapLight's moves,
// quietlight.go), and it is closed once empty for QuietFollowGraceMs, or
// replMaxHold after it was retired whatever is left on it. At most one link
// at a time, replGap apart, and replBudget in replWindow: a path whose
// every new QUIET link is slow (a DPI box that throttles what looks like
// one) is the path's, not a link's, and the set keeps what it has.

// QuietReplace (HS2_TUNE_QUIET_REPLACE; 0: off): replace a slow QUIET link.
var QuietReplace = newTuneInt(0)

const (
	quietSlowTicks = 3                // pool ticks in a row (healthTick each: 6 s)
	replGap        = time.Minute      // the least time between two replacements
	replWindow     = 10 * time.Minute // ... and at most replBudget of them in this long
	replBudget     = 3
	replMaxHold    = time.Minute // a retired link is closed this long after at the latest
	replLossTau    = 20 * time.Second
)

// replaceRoom is the links a reverse pool may hold past its count for a slow
// QUIET link's replacement (make before break): the exit dials it next to
// the slow one, and the edge's ceiling must allow it (stream_iran.go).
func replaceRoom() int {
	if QuietReplace.Get() != 0 {
		return 1
	}
	return 0
}

// replMinAge: a link younger than this is not judged (a var for the tests).
var replMinAge = 10 * time.Second

// replState is a QUIET link's record for the judge (the pool goroutine's).
type replState struct {
	prevRetrans uint64
	prevSegs    uint32
	prevPeer    peerLossRec
	resent      float64 // decayed over replLossTau, both ways
	segs        float64
	run         int       // ticks in a row judged slow
	why         string    // the latest verdict's reason
	at          time.Time // when it was retired (replaced)
}

// replBulk is the BULK links' record for the judge's reference.
type replBulk struct {
	prevRetrans uint64
	prevSegs    uint32
	prevPeer    peerLossRec
	resent      float64
	segs        float64
}

// replSet is the set's replacement state (the pool goroutine's, but for
// the counters).
type replSet struct {
	links    map[*quietLink]*replState
	bulk     map[*managedLink]*replBulk
	done     []time.Time // the replacements of the last replWindow
	held     time.Time   // when the budget ran out (logged once a window)
	replaced atomic.Int64
}

// lossNote decays a record by dt and adds the resends and segments since its
// previous reading (both ways); it returns the share and the resends.
func lossNote(resent, segs *float64, prevRt *uint64, prevSegs *uint32, prevPeer *peerLossRec, st tcpStat, stOK bool, peer *peerLossRec, dt time.Duration) {
	k := 1.0
	if dt > 0 {
		k = float64(replLossTau) / float64(replLossTau+dt)
	}
	*resent *= k
	*segs *= k
	if stOK {
		if *prevSegs != 0 || *prevRt != 0 {
			*resent += float64(st.retrans - min(st.retrans, *prevRt))
			*segs += float64(uint32(st.segsOut - *prevSegs))
		}
		*prevRt, *prevSegs = st.retrans, st.segsOut
	}
	if peer != nil && peer.n > prevPeer.n {
		if prevPeer.n > 0 && peer.segsIn != 0 && prevPeer.segsIn != 0 {
			*resent += float64(peer.rt - min(peer.rt, prevPeer.rt))
			*segs += float64(uint32(peer.segsIn - prevPeer.segsIn))
		}
		*prevPeer = *peer
	}
}

// replaceTick judges the QUIET links (each pool tick), retires a slow one
// and closes the retired ones that are done (QuietReplace).
func (q *quietSet) replaceTick(now time.Time) {
	if q == nil || QuietReplace.Get() == 0 || !q.active() {
		return
	}
	r := &q.repl
	if r.links == nil {
		r.links = map[*quietLink]*replState{}
		r.bulk = map[*managedLink]*replBulk{}
	}
	dt := healthTick
	// the reference: the serving BULK links' round trips at best, and
	// their resends
	ctrl := ctrlNow()
	var floors []time.Duration
	var bResent, bSegs float64
	bulkOK := false
	q.m.mu.RLock()
	live := map[*managedLink]bool{}
	for _, ml := range q.m.links {
		if !ml.serving() {
			continue
		}
		live[ml] = true
		mtr := linkMeterOf(ml.link)
		if mtr == nil {
			continue
		}
		if us := mtr.rttMin.Load(); us > 0 {
			floors = append(floors, time.Duration(us)*time.Microsecond)
		}
		if !ml.lagging(ctrl) {
			bulkOK = true
		}
		b := r.bulk[ml]
		if b == nil {
			b = &replBulk{}
			r.bulk[ml] = b
		}
		st, ok := linkTCPStatsOf(ml.link)
		lossNote(&b.resent, &b.segs, &b.prevRetrans, &b.prevSegs, &b.prevPeer, st, ok, mtr.peerLoss.Load(), dt)
		bResent += b.resent
		bSegs += b.segs
	}
	q.m.mu.RUnlock()
	for ml := range r.bulk {
		if !live[ml] {
			delete(r.bulk, ml)
		}
	}
	var bFloor time.Duration
	if len(floors) > 0 {
		bFloor = lowerMedian(floors)
	}
	bLoss := -1.0
	if bSegs >= qualLossSegsMin {
		bLoss = bResent / bSegs
	}

	q.mu.Lock()
	links := slices.Clone(q.links)
	up := 0 // QUIET links up that are not retiring: a replacement there
	for _, ql := range links {
		if ql.link.Alive() && !ql.retiring.Load() {
			up++
		}
	}
	q.mu.Unlock()
	seen := map[*quietLink]bool{}
	var slow *quietLink
	var closing, kept []*quietLink
	for _, ql := range links {
		seen[ql] = true
		s := r.links[ql]
		if s == nil {
			s = &replState{}
			r.links[ql] = s
		}
		if ql.replaced.Load() {
			switch {
			case !ql.link.Alive():
			case up == 0 && now.Sub(s.at) >= replMaxHold:
				kept = append(kept, ql) // no replacement came: it stays
			case up == 0:
				// make before break: it serves until one is up
			case followCloseNow(ql, ql.users.Load(), ctrl) || now.Sub(s.at) >= replMaxHold:
				closing = append(closing, ql)
			}
			continue
		}
		mtr := ql.link.mtr
		if mtr == nil || !ql.link.Alive() || ql.retiring.Load() {
			s.run = 0
			continue
		}
		st, ok := ql.link.tcpStats()
		lossNote(&s.resent, &s.segs, &s.prevRetrans, &s.prevSegs, &s.prevPeer, st, ok, mtr.peerLoss.Load(), dt)
		why := ""
		qFloor := time.Duration(mtr.rttMin.Load()) * time.Microsecond
		switch {
		case now.Sub(ql.born) < replMinAge || !bulkOK:
		case bFloor > 0 && qFloor > 0 && qFloor-bFloor >= max(qualDelayAbs, time.Duration(qualDelayRatio*float64(bFloor))):
			why = fmt.Sprintf("round trip %dms at best where the BULK links take %dms", qFloor.Milliseconds(), bFloor.Milliseconds())
		case bLoss >= 0 && s.segs >= qualLossSegsMin && s.resent >= qualLossEvents && s.resent/s.segs > max(qualLossFrac, qualLossRatio*bLoss):
			why = fmt.Sprintf("resends %.1f%% of what it sends where the BULK links resend %.1f%%", 100*s.resent/s.segs, 100*bLoss)
		case meterLagging(mtr, ctrl):
			why = "lags (a control ping unanswered, its writer stuck or nothing read) while the BULK links do not"
		}
		if why == "" {
			s.run = 0
			continue
		}
		s.run++
		s.why = why
		if s.run >= quietSlowTicks && slow == nil {
			slow = ql
		}
	}
	for ql := range r.links {
		if !seen[ql] {
			delete(r.links, ql)
		}
	}
	for _, ql := range closing {
		q.m.log("mtcp: QUIET link q%d closed: replaced for being slow (%d connections left on it)", ql.id, ql.users.Load())
		ql.link.Close()
	}
	for _, ql := range kept {
		q.mu.Lock()
		ql.retiring.Store(false)
		q.replacedGoneLocked(ql)
		q.mu.Unlock()
		ql.link.l3avoid.Store(ql.light.Load())
		if cl := coreLinks.of(ql.link.sess); cl != nil {
			if t := cl.tun.Load(); t != nil {
				t.avoid.Store(ql.light.Load())
			}
		}
		q.m.log("mtcp: QUIET link q%d stays: no QUIET link came up to replace it within %s", ql.id, replMaxHold)
	}
	if slow == nil || q.replacing.Load() > 0 {
		return
	}
	r.done = slices.DeleteFunc(r.done, func(t time.Time) bool { return now.Sub(t) > replWindow })
	if n := len(r.done); n > 0 && now.Sub(r.done[n-1]) < replGap {
		return
	}
	if len(r.done) >= replBudget {
		if r.held.IsZero() || now.Sub(r.held) > replWindow {
			r.held = now
			q.m.log("mtcp: QUIET link q%d is slow (%s), but %d QUIET links were replaced in the last %s: the path, not a link; it stays", slow.id, r.links[slow].why, len(r.done), replWindow)
		}
		return
	}
	s := r.links[slow]
	s.at, s.run = now, 0
	r.done = append(r.done, now)
	r.replaced.Add(1)
	slow.replaced.Store(true)
	slow.retiring.Store(true)
	slow.link.l3avoid.Store(true) // the TUN's flows leave it now (l3Link.avoid)
	if cl := coreLinks.of(slow.link.sess); cl != nil {
		if t := cl.tun.Load(); t != nil {
			t.avoid.Store(true)
		}
	}
	q.replacing.Add(1)
	q.m.log("mtcp: QUIET link q%d is slow next to the BULK links (%s): a new QUIET link comes up, its connections move there, then it goes", slow.id, s.why)
	q.m.notifyCtl() // the exit dials one link more now (ctlTarget)
}

// replacedGone takes a replaced link that ended out of the count it holds
// (reap, drop; q.mu held).
func (q *quietSet) replacedGoneLocked(ql *quietLink) {
	if ql.replaced.CompareAndSwap(true, false) {
		q.replacing.Add(-1)
	}
}
