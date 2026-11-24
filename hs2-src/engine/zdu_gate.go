package engine

import (
	"context"
	"sync/atomic"
	"time"
)

// ZDU review issue 4 (proposals/zdu350.md Phase 1 steps 3-5): nothing user-
// facing in the new process before GO, and the reverse-mode link bootstrap.
//
// New process (HS2_HANDOVER_FD, ZDUHoldUsers right after the fds are adopted,
// before the engine opens anything):
//   - every user listener (zduListen) and UDP socket (zduListenPacket) is
//     created paused: Accept / ReadFrom wait on the cond and never touch the
//     shared kernel queue, so the old process, still serving, takes every
//     connection and datagram until GO (S9: the new Iran's pre-GO accepts and
//     reads cost ~700 crowd fails + ~1070 UDP datagrams per reverted
//     handover);
//   - link listeners (ListenLink: the reverse edge's, the direct exit's) are
//     NOT held: the new process gets its own links before READY;
//   - the exit (Kharej) has no user listener: its users arrive as streams on
//     its links. zduWaitGO holds such a stream until GO (Iran routes new conns
//     to a new exit only once it has min(old/2, warm) links, zdu_phase.go, and
//     the new exit's READY waits for about the same count, so the wait is the
//     READY->GO round trip in practice);
//   - ZDUOpenUsers at GO releases all of them. A new process that never gets
//     GO exits without having served anyone.
//
// Old process, handover start (before the spawn):
//   - ZDUPauseLinkAccept: it stops accepting on its link listeners, so every
//     link dialed from then on lands on the new process (the fd is shared:
//     only one of the two calls accept on it). Its existing links stay.
//     Resumed when the handover fails or reverts.
//   - ZDUBoost (Iran pools): ctlTarget adds the pool's own target, so the exit
//     dials that many more links, which can only land on the new process
//     (step 4). A ZDU exit then counts the old links as legacy at the first
//     new link and dials the new pool in full (zdu_exit.go). An exit already
//     at its ceiling cannot grow: then up to zduBootClose idle links (no user)
//     are closed so their slots redial into the new process.
//
// HS2_ZDU off: nothing here is reachable (ListenLink is ListenReuse, zduHold
// is never set, the boost term is 0).

var (
	zduHold   atomic.Bool                   // a new process before GO
	zduHoldCh atomic.Pointer[chan struct{}] // closed at GO
	// zduExitPoolRef is the exit's link pool (RunKharej, key on): the warm need.
	zduExitPoolRef atomic.Pointer[exitPool]
)

// zduBootClose: idle links an old Iran closes at handover start when the exit
// pool is at its ceiling (the boost cannot grow it).
const zduBootClose = 2

// ZDUHoldUsers marks this process as a hand-off child before GO.
func ZDUHoldUsers() {
	if !zduOn.Load() {
		return
	}
	ch := make(chan struct{})
	zduHoldCh.Store(&ch)
	zduHold.Store(true)
}

// ZDUUsersHeld reports whether users are still held (tests, logs).
func ZDUUsersHeld() bool { return zduHold.Load() }

// ZDUOpenUsers releases the held user listeners, UDP sockets and exit
// streams (new process, at GO). Returns the listeners + sockets released.
func ZDUOpenUsers() int {
	zduLns.mu.Lock()
	zduPCs.mu.Lock()
	was := zduHold.Swap(false)
	ls := make([]*zduLn, 0, len(zduLns.m))
	for _, z := range zduLns.m {
		if !z.link {
			ls = append(ls, z)
		}
	}
	ps := make([]*zduPC, 0, len(zduPCs.m))
	for _, z := range zduPCs.m {
		ps = append(ps, z)
	}
	zduPCs.mu.Unlock()
	zduLns.mu.Unlock()
	if !was {
		return 0
	}
	for _, z := range ls {
		z.setPaused(false)
	}
	for _, z := range ps {
		z.setPaused(false)
	}
	if p := zduHoldCh.Load(); p != nil {
		close(*p)
	}
	return len(ls) + len(ps)
}

// zduWaitGO holds an exit user stream until GO; false = ctx ended first.
// Not held: one atomic load.
func zduWaitGO(ctx context.Context) bool {
	if !zduHold.Load() {
		return true
	}
	p := zduHoldCh.Load()
	if p == nil {
		return true
	}
	select {
	case <-*p:
		return true
	case <-ctx.Done():
		return false
	}
}

// ZDUPauseLinkAccept stops accept on the link listeners only (old process,
// handover start). Returns how many.
func ZDUPauseLinkAccept() int { return zduSetLinkPaused(true) }

// ZDUResumeLinkAccept re-opens accept on the link listeners (failed handover).
func ZDUResumeLinkAccept() int { return zduSetLinkPaused(false) }

func zduSetLinkPaused(p bool) (n int) {
	zduLinkPaused.Store(p) // review M2: a link listener made meanwhile starts paused
	for _, z := range zduAll() {
		if z.link {
			z.setPaused(p)
			n++
		}
	}
	return n
}

// ZDUDropLinkListeners closes this process's copy of every link listener
// (old process, at the commit: it never accepts a link again). Review M2: a
// link listener the new process did not bind (its config dropped the IP, or
// an LB reload added it here during the warm wait) then refuses dials instead
// of holding them in a paused backlog through the whole drain; one the new
// process holds stays open there. Returns how many.
func ZDUDropLinkListeners() (n int) {
	for _, z := range zduAll() {
		if z.link {
			z.drop()
			n++
		}
	}
	return n
}

// ZDUBoost raises (on) or clears every Iran pool's poolctl target by the
// pool's own target (old process, handover start / failure). It returns the
// total boost and the idle links closed because the exit is at its ceiling.
func ZDUBoost(on bool) (boost, closed int) {
	zduLMs.mu.Lock()
	ls := append([]*LinkManager(nil), zduLMs.l...)
	zduLMs.mu.Unlock()
	for _, m := range ls {
		b := 0
		if on {
			b = max(m.Target(), 0)
		}
		if int(m.zdu.boost.Swap(int32(b))) != b {
			m.notifyCtl()
		}
		boost += b
		if on && b > 0 {
			closed += m.zduBootClose()
		}
	}
	return boost, closed
}

// zduBoostTerm is what ctlTarget adds while the old process boots the new.
func (m *LinkManager) zduBoostTerm() int {
	if !zduOn.Load() {
		return 0
	}
	return int(m.zdu.boost.Load())
}

// zduBootClose closes up to zduBootClose idle live links when the exit's
// pool is at its ceiling (it reports its clamp as MaxLinks), so their slots
// redial into the new process. Returns the links closed.
func (m *LinkManager) zduBootClose() int {
	pi := m.exitInfo.Load()
	if pi == nil || pi.MaxLinks <= 0 || m.alive() < pi.MaxLinks {
		return 0
	}
	now := ctrlNow()
	m.mu.RLock()
	var idle []Link
	for _, ml := range m.links {
		if len(idle) >= zduBootClose {
			break
		}
		if ml.link.Alive() && !ml.zduOld && ml.users.Load() == 0 && !zduL3Busy(ml.mtr, now) {
			idle = append(idle, ml.link)
		}
	}
	m.mu.RUnlock()
	for _, l := range idle {
		l.Close()
	}
	return len(idle)
}

// zduBootL3Recent: a link that carried a TUN packet this recently is busy
// for zduBootClose (review M3: L3 lanes are not users; closing such a link
// loses the packets in flight and re-pins its lane flows).
const zduBootL3Recent = 30 * time.Second

// zduMarkL3 stamps a link's L3 activity (both directions). Key off: one
// atomic load.
func zduMarkL3(at *atomic.Int64) {
	if zduOn.Load() && at != nil {
		at.Store(ctrlNow())
	}
}

// zduL3Busy: the link carried TUN traffic within zduBootL3Recent, or its
// activity is unknown (no meter): never closed by zduBootClose.
func zduL3Busy(mtr *linkMeter, now int64) bool {
	if mtr == nil {
		return true
	}
	at := mtr.zduL3At.Load()
	return at != 0 && time.Duration(now-at) < zduBootL3Recent
}

// ZDUWarmNeed is the link count a new process waits for before READY:
// min(old_serving/2, warm) per pool (Phase 1 step 5), as zduNeedOf (ceil(t/2),
// never 1 for a multi-link pool) of each pool's own target (an Iran pool
// starts at the old's target; the exit pool at the old pool size). Review H1:
// the same rule for every pool kind; a direct (forward) exit has no target
// here, so it needs zduNeedFloor (its handover is refused in direct mode
// anyway, cmd/hs2 zduOld.direct).
func ZDUWarmNeed() int {
	zduLMs.mu.Lock()
	ls := append([]*LinkManager(nil), zduLMs.l...)
	zduLMs.mu.Unlock()
	need := 0
	for _, m := range ls {
		need += zduNeedOf(m.Target())
	}
	if p := zduExitPoolRef.Load(); p != nil {
		need += zduNeedOf(p.target())
	} else if zduExitPeers.Load() != nil {
		need += zduNeedFloor
	}
	return need
}

// zduNeedFloor: the least warm need of a multi-link pool (review H1: one
// link is never enough for every new user).
const zduNeedFloor = 2

// zduNeedOf is one pool's warm need for target t: ceil(t/2), at least
// zduNeedFloor, at most t (a single-link pool needs its one link).
func zduNeedOf(t int) int {
	if t <= 1 {
		return 1
	}
	return min(max((t+1)/2, zduNeedFloor), t)
}

// zduExitBoot: spare dials a ZDU exit starts when the edge asks above the
// pool's ceiling (the old Iran's boot boost while the pool is at its max).
const zduExitBoot = 2

// zduBootJump: the rise of the asked target over the last ask (prev) that
// starts a new boot episode. The handover boost doubles the ask (+ the old
// Iran's target); a cut-drain replacement adds a few links.
func zduBootJump(prev int) int { return max(prev/2, zduExitBoot) }

// zduExitBootN counts them (zdu_exit_boot on the mem line).
var zduExitBootN atomic.Int64

// zduBootLocked (exit, key on): a target above the ceiling is the old Iran's
// handover boost. The pool cannot grow past its ceiling, so it starts up to
// zduExitBoot spare dials per boost episode; they can only land on the new
// Iran (the old stopped accepting links), and at that first new-instance link
// the old links turn legacy and the new Iran's own poolctl grows its pool in
// full (zdu_exit.go). A target at or below the ceiling ends the episode.
//
// b441 (l437 finding 6: an Iran handover got "0 links after 20s, need 93"):
// the edge also asks above the ceiling without any handover, for the links
// its lb cut-drain replaces (ctlTarget's ctlDrain term). Such an overshoot
// opened an episode and spent the spares on the old Iran (zdu_exit_boot=8 on
// the new Kharej in the minute before); while it lasted, the real boost that
// followed was the same episode: no spare dialed, and the old Iran's
// zduBootClose links were retired as over target instead of redialing. The
// boost adds the old Iran's whole target, so an ask that rises by at least
// zduBootJump over the last one starts a new episode even while the ask was
// already above the ceiling. Any Iran version (the wire is unchanged).
// Caller holds p.mu.
func (p *exitPool) zduBootLocked(asked, hi int) {
	prev := p.zduAsk
	p.zduAsk = asked
	if !zduOn.Load() || asked <= hi || p.want == 0 {
		p.zduBoot, p.zduBootRound = 0, 0
		return
	}
	if prev > 0 && asked-prev >= zduBootJump(prev) {
		p.zduBoot = 0 // a boost on top of an overshoot: a new episode
		p.zduBootRound = 0
	}
	now := time.Now()
	if p.zduBoot >= zduExitBoot && p.zduBootRound < zduBootRounds && now.Sub(p.zduBootAt) >= zduBootRetry &&
		p.zduBootUp == p.zduBootUp0 && !p.peers.zduCur().newer(p.zduBootInst) {
		p.zduBoot = 0 // b443: no spare of the last round reached any Iran process and no new instance yet: one more round
		p.zduBootRound++
		zduExitBootRetryN.Add(1)
	}
	if p.zduBoot < zduExitBoot && !p.outage {
		p.zduBootAt, p.zduBootInst, p.zduBootUp0 = now, p.peers.zduCur(), p.zduBootUp
	}
	for ; p.zduBoot < zduExitBoot && !p.outage; p.zduBoot++ {
		p.startSlotLocked()
		s := p.slots[len(p.slots)-1]
		s.spare = true // never retired before its one dial
		s.boot = true  // b443: dialed on a proven path
		zduExitBootN.Add(1)
	}
}

// b443 (lab r443 op4: an Iran handover 37 s after the previous Iran drain got
// "3 links after 20s, need 96"): the boot spares did start (zdu_exit_boot
// +2) but the LB put both on the lossy path (35 % loss), just back from its
// dial cooldown: a cooled path's probe comes first and the "two per path"
// rule sends the next dial to the path with the fewest links. Their TLS
// handshakes took ~19 s, the new Iran had no link to send its own poolctl on,
// and the old Iran's boost could not grow a pool at its ceiling. Now:
//
//   - a boot spare dials a proven path (zduBootPathLocked: eligible, factor
//     >= zduBootMinMult, at least two links held there), by weight among them;
//   - the episode is tied to the new Iran instance, not only to the ask: when
//     no spare of the last round reached any Iran process (no link up) within
//     zduBootRetry and no newer Iran instance has appeared on a link since the
//     round began, one more round starts (at most zduBootRounds). Without a
//     handover the spares land on the running Iran at once (zduBootUp moves),
//     so no extra round and no churn; once a link of the new instance is up,
//     the new Iran's own poolctl grows its pool (zdu_exit.go).
//
// Exit-side only, wire unchanged, any Iran version.
const (
	zduBootRetry   = 5 * time.Second
	zduBootRounds  = 3
	zduBootMinMult = 0.5
)

// zduExitBootRetryN counts the extra rounds (zdu_exit_boot_retry on the mem line).
var zduExitBootRetryN atomic.Int64

// zduBootPathLocked is the path for a boot spare: among the eligible paths
// with factor >= zduBootMinMult and at least two links (live or dialing), the
// one furthest under its weighted share; -1 when none (the usual pick then).
// Caller holds b.mu.
func (b *LB) zduBootPathLocked() int {
	now := b.now()
	ok := func(j int) bool {
		return b.eligibleLocked(j, now) && j < len(b.mult) && b.mult[j] >= zduBootMinMult && b.links[j] >= 2 &&
			(j >= len(b.probing) || !b.probing[j])
	}
	found := false
	for j := range b.paths {
		if ok(j) {
			found = true
			break
		}
	}
	if !found {
		return -1
	}
	return pickLargestRemainder(b.effWeightsLocked(), b.links, ok, -1)
}
