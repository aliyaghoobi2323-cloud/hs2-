package engine

import (
	"context"
	"sync"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
	"github.com/xtaci/smux"
)

// R1 round 5, the second approach (docs/architecture/wip/r1/round5/
// REPORT.fa.md): a light QUIET link next to the general one.
//
// A QUIET link is one TCP connection, so one lost segment of it holds every
// message behind it for a round trip and a reordering window (~94 ms at a
// 75 ms round trip): a game's message waits behind a web page's lost segment
// on the same connection. The share of time a stream sits behind such a
// hole is the link's losses a second times that hold, and that grows with
// what the link carries: at N=1 in the home mix the QUIET link carries ~45
// Mbit/s down (web responses, calls, every connection's start) once its
// window holds under loss (Illinois), ~3900 segments a second, so at 0.03%
// random loss a game's messages sat behind a hole 11% of the time (p99 +99
// ms down against +72 for fin2, whose cubic QUIET carried 12 Mbit/s).
//
// With QuietLight a pool of up to quietLightMax links keeps one QUIET link
// more, the light one: no connection opens on it; a connection whose two
// ways both stay light and steady (lightRule: a game, a probe — both ways
// active in lightTicks of the 20 ticks of the last 2 s, neither past
// lightBytes) moves to it, and one there that stops being light (a call
// that opened there while the general link was down, a game that grew)
// moves back to a general one, at most each lightGap. A bulk one moves on
// to a BULK link as before (the exit's hint, the edge's rule): a group
// moves up to lightMoves times (smux Config.GroupMoves), which both ends
// allow with tlscarrier.CapLight. The light link so carries a few hundred
// small records a second: holes on it are rare, and a game's message never
// waits behind a web page's.
//
// The roles are this end's alone (the edge's): the exit dials its links as
// before, one more QUIET (QuietCount), and serves a group's move stream on
// any link. Both ends set QuietLight alike (QuietFollow keeps the smaller
// count; with one QUIET link the edge has no light one).
//
// QuietWaitMs (the red team's R2-12): with no QUIET link up but one up
// within quietWaitRecent, a new connection waits up to QuietWaitMs for one
// (a reset QUIET link comes back in ~1 s) instead of opening on a BULK link
// for good; on the light link at once if that one is up (it moves to a
// general one once one is back).

// QuietLight (HS2_TUNE_QUIET_LIGHT; 0: off): 1, a pool of up to
// quietLightMax links keeps one QUIET link more, the light one; 2, besides,
// a larger pool makes one of its QuietMany QUIET links the light one (no
// link more: past quietLightMax the pool has more connections than the DPI
// model's population already, and four QUIET flows at the bottleneck).
var QuietLight = newTuneInt(0)

// QuietWaitMs (HS2_TUNE_QUIET_WAIT, ms; 0: none): how long a new connection
// waits for a QUIET link that was up within quietWaitRecent.
var QuietWaitMs = newTuneInt(0)

const (
	// quietLightMax: the most links of a pool that keeps a light QUIET link
	// more: max_links + 2 connections stay within the population of
	// dpi_eval2's model (1 to 6, decision T19), so 4; a pool of 5 keeps its
	// one QUIET link, and past quietOneMax one of the QuietMany is the light
	// one (QuietLight 2)
	quietLightMax = quietOneMax - 1
	// lightTicks: the ticks of moveSlots in which each way sent at least a
	// byte, for a light verdict (a probe every 200 ms: 10; a game: 20)
	lightTicks = 8
	// lightBytes: the most bytes of each way over the last 2 s (192 kbit/s:
	// a game's 64 messages of 60 to 120 B a second up, 30 of 100 to 600 B
	// down; a call's 1.5 Mbit/s is 375 KB)
	lightBytes = 48 << 10
	// lightGap: the least time between two moves of one group between the
	// QUIET links
	lightGap = 5 * time.Second
	// lightOff: how long a connection on the light link is not light
	// before it moves back to a general one
	lightOff = 2 * time.Second
	// lightMoves: the moves a group may make with CapLight (to the light
	// link, back, on to BULK, and one spare)
	lightMoves = 4
	// quietWaitRecent: a QUIET link up within this long is expected back
	quietWaitRecent = 10 * time.Second
	// quietWaitPoll: the wait's look at the QUIET links
	quietWaitPoll = 20 * time.Millisecond
)

// quietLightOn says whether the pool keeps a light QUIET link.
func (q *quietSet) quietLightOn() bool {
	return q != nil && !QuietOff() && (QuietLight.Get() == 1 && q.m.max <= quietLightMax || QuietLight.Get() >= 2)
}

// lightCount is the light QUIET links QuietCount adds to a pool of maxLinks.
func lightCount(maxLinks int) int {
	if QuietLight.Get() > 0 && maxLinks <= quietLightMax {
		return 1
	}
	return 0
}

// roleLocked gives a QUIET link that came up its role (q.mu held): light
// when the pool keeps one, has none up and has a general one up besides.
func (q *quietSet) roleLocked(ql *quietLink) {
	if !q.quietLightOn() || q.wantNow() < 2 || ql.link.caps&tlscarrier.CapLight == 0 {
		return
	}
	general, light := 0, 0
	for _, o := range q.links {
		if o == ql || !o.link.Alive() || o.retiring.Load() {
			continue
		}
		if o.light.Load() {
			light++
		} else {
			general++
		}
	}
	if general >= 1 && light == 0 {
		ql.light.Store(true)
	}
}

// generalUp says whether a general QUIET link is up (q.mu held), and notes
// the time for the wait (QuietWaitMs).
func (q *quietSet) generalUpLocked(now time.Time) bool {
	for _, ql := range q.links {
		if ql.link.Alive() && !ql.light.Load() && !ql.retiring.Load() {
			q.generalAt = now
			return true
		}
	}
	return false
}

// pickLight is the light QUIET link a connection moves to (or opens on when
// no general one is up): the one with the fewest connections.
func (q *quietSet) pickLight(avoid []Link) (*quietLink, func(), bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	var best *quietLink
	for _, ql := range q.links {
		if !ql.light.Load() || !ql.link.Alive() || ql.retiring.Load() {
			continue
		}
		skip := false
		for _, a := range avoid {
			if a == Link(ql.link) {
				skip = true
			}
		}
		if skip {
			continue
		}
		if best == nil || ql.users.Load() < best.users.Load() {
			best = ql
		}
	}
	if best == nil {
		return nil, nil, false
	}
	return best, q.slotLocked(best), true
}

// slotLocked takes a connection's slot on ql (q.mu held); the func gives it
// back, once.
func (q *quietSet) slotLocked(ql *quietLink) func() {
	ql.users.Add(1)
	q.m.users.Add(1)
	var once sync.Once
	return func() {
		once.Do(func() {
			ql.users.Add(-1)
			q.m.users.Add(-1)
		})
	}
}

// expectQuiet says whether a QUIET link is expected back soon: none up
// now, one up within quietWaitRecent.
func (q *quietSet) expectQuiet(now time.Time) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.generalUpLocked(now) {
		return false
	}
	for _, ql := range q.links {
		if !ql.light.Load() && !ql.link.Alive() {
			return true // dead and not reaped yet: it just ended
		}
	}
	return !q.generalAt.IsZero() && now.Sub(q.generalAt) < quietWaitRecent
}

// openWaitQuiet is pickOpen's wait for a general QUIET link (QuietWaitMs): the
// light link at once if it is up, else the general one when it comes up
// within the wait; false: none (the caller opens on a BULK link).
func openWaitQuiet(ctx context.Context, lm *LinkManager, tag uint32, tried []Link) (Link, func(), bool) {
	q := lm.quiet
	if ql, rel, ok := q.pickLight(tried); ok {
		q.lightOpened.Add(1)
		return ql.link, rel, true
	}
	w := time.Duration(QuietWaitMs.Get()) * time.Millisecond
	if w <= 0 || !q.expectQuiet(time.Now()) {
		return nil, nil, false
	}
	q.waited.Add(1)
	t := time.NewTicker(quietWaitPoll)
	defer t.Stop()
	end := time.Now().Add(w)
	for time.Now().Before(end) {
		select {
		case <-ctx.Done():
			return nil, nil, false
		case <-t.C:
		}
		if ql, rel, ok := q.pick(tried, tag); ok {
			q.waitedOK.Add(1)
			return ql.link, rel, true
		}
		if ql, rel, ok := q.pickLight(tried); ok {
			q.waitedOK.Add(1)
			q.lightOpened.Add(1)
			return ql.link, rel, true
		}
	}
	return nil, nil, false
}

// lightRule says whether the connection's last 2 s were light and steady
// both ways (a game, a probe; QuietLight).
func (r *moveRule) lightRule() bool {
	if r.seen < moveSlots || r.sumTx > lightBytes || r.sumRx > lightBytes || r.busyRun > 0 {
		return false
	}
	tx, rx := 0, 0
	for i := 0; i < moveSlots; i++ {
		if r.tx[i] > 0 {
			tx++
		}
		if r.rx[i] > 0 {
			rx++
		}
	}
	return tx >= lightTicks && rx >= lightTicks
}

// lightTick is the light QUIET link's part of the mover's look at gs (not
// on a BULK link, not due to move there): to the light link when it is
// light, back to a general one when it is on the light link and not; true
// when it moved.
func (q *quietSet) lightTick(gs *groupStream, now int64) bool {
	gs.mu.Lock()
	at, ql := gs.lightAt, gs.ql
	gs.mu.Unlock()
	if ql == nil || ql.caps&tlscarrier.CapLight == 0 {
		return false
	}
	onLight, replaced := q.roleOf(ql)
	if replaced {
		// its link is slow and being replaced (QuietReplace,
		// quietreplace.go): to a QUIET link of its role that is not, once
		// one is up
		toLight := onLight && gs.rule.lightRule()
		if toLight {
			if ql, rel, ok := q.pickLight(nil); ok {
				rel()
				_ = ql
			} else {
				toLight = false // no light one up (yet): a general one
			}
		}
		return q.moveQuiet(gs, toLight, now)
	}
	if !q.quietLightOn() {
		return false
	}
	light := gs.rule.lightRule()
	switch {
	case light || !onLight || gs.rule.seen < moveSlots:
		gs.offLight = 0
	case gs.offLight == 0:
		gs.offLight = now
	}
	if at != 0 && time.Duration(now-at) < lightGap {
		return false
	}
	switch {
	case !onLight && light:
		return q.moveQuiet(gs, true, now)
	case onLight && gs.offLight != 0 && time.Duration(now-gs.offLight) >= lightOff:
		// not light for lightOff, and not bulk (that one moves on to a BULK
		// link within a few hundred ms: the hint, the edge's rule)
		return q.moveQuiet(gs, false, now)
	}
	return false
}

// roleOf says whether l is a light QUIET link of the set, and whether it is
// being replaced for being slow (QuietReplace).
func (q *quietSet) roleOf(l *mtcpLink) (light, replaced bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, ql := range q.links {
		if ql.link == l {
			return ql.light.Load(), ql.replaced.Load()
		}
	}
	return false, false
}

// moveQuiet moves gs between the QUIET links: to the light one (toLight),
// or back to a general one. False: no link took it now, or its group may
// not move yet (the stream before not let go; tried at a later tick).
func (q *quietSet) moveQuiet(gs *groupStream, toLight bool, now int64) bool {
	if gs.g.Moves() > 0 && !gs.g.Retired() {
		return false
	}
	var ql *quietLink
	var rel func()
	var ok bool
	if toLight {
		ql, rel, ok = q.pickLight(nil)
	} else {
		ql, rel, ok = q.pick(nil, gs.tag)
	}
	if !ok {
		return false
	}
	if ql.link.caps&tlscarrier.CapLight == 0 {
		rel()
		return false
	}
	s1, err := gs.g.Move(ql.link.sess, gs.tag)
	if err != nil {
		rel()
		if err == smux.ErrNoMove {
			gs.mu.Lock()
			gs.lightAt = now // its group refused (moved its most): not again soon
			gs.mu.Unlock()
		}
		return false
	}
	gs.mu.Lock()
	oldCS, oldRel := gs.qcs, gs.qrel
	gs.qcs, gs.qrel, gs.qown, gs.ql = ql.link.track(s1), rel, true, ql.link
	gs.lightAt = now
	gs.offLight = 0
	closed := gs.closed
	qcs := gs.qcs
	gs.mu.Unlock()
	if closed {
		qcs.untrack()
		rel()
	}
	oldCS.untrack()
	if oldRel != nil {
		oldRel()
	}
	if toLight {
		q.movedLight.Add(1)
	} else {
		q.movedGeneral.Add(1)
	}
	return true
}
