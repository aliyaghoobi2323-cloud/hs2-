package engine

import (
	"fmt"
	"sync/atomic"
)

// ZDU building block B3 (proposals/zdu350.md §7 S3b): the Iran pool counts
// only the current peer generation.
//
// During a Kharej handover the pool holds the old process's links (they carry
// the connections that process still drains, zdu_pin.go) next to the new
// process's. A link whose own kindInfo announced an instance older than the
// current one (zduCurrent: the newest among the live links) is peerOld:
//
//   - it is not serving() and not in the plain picker's first tier: no new
//     connection lands on it (mconn conns are already sent to the current
//     instance by the pin mark; mconn pickers that carry the mark test up(),
//     so an old conn still places its joins and stripes on its own links);
//   - it is not in aliveLocked / countsLocked, the autopilot's sample, the
//     slots dialRoom counts, or the replaced-drain term of ctlTarget: the pool
//     sizes for the new generation only;
//   - it does not count against the reverse accept cap (acceptReverseLinks
//     tests lm.alive()), so the new exit can dial its full pool next to the
//     old links: with prod's ~80 links at the hourly storm the gate sees the
//     new pool only, under 2·max+8, instead of ~133 links against it.
//
// The links stay up for their draining connections; reclaimIdle / the exit
// close them as today. The marks are recomputed under m.mu whenever an
// instance announcement arrives (zduNoteInfo), a reverse link goes
// (DropLink), and on every health tick (sampleHealth) — so if the new
// process dies in probation, the current instance falls back to the old one
// and its links count again on the next of those.
//
// HS2_ZDU off, or no link announced capExtZDU: zduRefreshLocked returns at
// once, no link is ever marked, every count is exactly as before.

// zduGauge: the mem-line gauges, summed over the pools of this process.
var zduGauge struct {
	peerOld  atomic.Int64 // zdu_peerold: live links of an older peer instance
	oldConns atomic.Int64 // zdu_old_conns: user connections on those links
}

// zduRefreshLocked re-marks the peerOld links and updates the gauges. Caller
// holds m.mu for writing.
func (m *LinkManager) zduRefreshLocked() {
	if !zduOn.Load() || m.zdu.gen.Load() == 0 {
		return
	}
	cur := m.zduCurrentLocked()
	mx := m.zduMixNow() // review issue 3: both generations serve (zdu_phase.go)
	mixing := mx != nil
	var old, conns int64
	changed := false
	for _, ml := range m.links {
		o := false
		if !mixing && cur.known() && ml.link.Alive() {
			if in := linkZDUInst(ml.link); in.known() && in != cur {
				o = true
				old++
				conns += int64(ml.users.Load())
			}
		}
		// zdu review 5 (M): while mixing no link carries the routing mark,
		// but the older generation stays outside reconcile (zdu_recon.go):
		// the generation flag is separate from the mark.
		mo := false
		if mixing && ml.link.Alive() {
			if in := linkZDUInst(ml.link); in.known() && in != mx.newI {
				mo = true
			}
		}
		ml.zduMixOld = mo
		if o != ml.zduOld {
			ml.zduOld, changed = o, true
			if mtr := linkMeterOf(ml.link); mtr != nil { // the L3 picker's view (zdu_l3.go)
				mtr.zduOld.Store(o)
			}
			zduL3Gen.Add(1)
		}
	}
	if changed {
		m.b327Bump() // the cached pick list (lm_b327_picklist.go) holds serving links only
	}
	if old > 0 {
		zduSeen.Store(1)
	}
	zduGauge.peerOld.Add(old - m.zdu.peerOld)
	zduGauge.oldConns.Add(conns - m.zdu.oldConns)
	m.zdu.peerOld, m.zdu.oldConns = old, conns
}

// zduForget takes this pool's share out of the package gauges when the pool
// is done (Run returned). A process has one edge pool for its life, so in
// production the share only goes at exit; a pool dropped while it still held
// marks would otherwise leave zdu_peerold above 0 for good. A later refresh
// (a link dropped after Run) counts the pool again from 0, so the sum stays
// exact. Key off: both shares are 0.
func (m *LinkManager) zduForget() {
	m.mu.Lock()
	zduGauge.peerOld.Add(-m.zdu.peerOld)
	zduGauge.oldConns.Add(-m.zdu.oldConns)
	m.zdu.peerOld, m.zdu.oldConns = 0, 0
	m.mu.Unlock()
}

// zduPeerOld is the number of peerOld links in this pool (tests, status).
func (m *LinkManager) zduPeerOld() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return int(m.zdu.peerOld)
}

// zduCountsLine: " zdu_peerold= zdu_old_conns=" on the mem line once a
// generation change was seen (after the pin's " zdu: ..." part).
func zduCountsLine() string {
	if !zduOn.Load() {
		return ""
	}
	s := ""
	if zduSeen.Load() != 0 || zduGauge.peerOld.Load() != 0 {
		s = fmt.Sprintf(" zdu_peerold=%d zdu_old_conns=%d", zduGauge.peerOld.Load(), zduGauge.oldConns.Load())
	}
	if _, cur, old := zduExitPeers.Load().zduGens(); old > 0 || zduExitSeen.Load() {
		zduExitSeen.Store(true)
		s += fmt.Sprintf(" zdu_exit_cur=%d zdu_exit_old=%d", cur, old)
	}
	if n := zduUDPMoved.Load(); n > 0 { // zdu_udpmove.go
		s += fmt.Sprintf(" zdu_udp_moved=%d", n)
	}
	return s + zduExitLine()
}

// Exit side (Kharej): the live links by Iran instance, as each link's own
// kindInfo from the edge announced it. Counting only: S4 (B5) builds the
// legacy slots (no want, no redial) and the poolctl filter on it.
var (
	zduExitPeers atomic.Pointer[linkPeers] // the exit's live links (RunKharej)
	zduExitSeen  atomic.Bool               // an old-instance link was seen: keep the gauges on the line
)

// zduGens returns the newest Iran instance among the live links, the links
// of it and the links of an older one. Links whose edge sent no instance
// (no answer yet, pre-ZDU edge) are in neither count.
func (p *linkPeers) zduGens() (cur zduInst, curN, oldN int) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for m := range p.m {
		if pi := m.peerInfo.Load(); pi.zdu() && pi.Inst.newer(cur) {
			cur = pi.Inst
		}
	}
	if !cur.known() {
		return
	}
	for m := range p.m {
		if pi := m.peerInfo.Load(); pi.zdu() {
			if pi.Inst == cur {
				curN++
			} else {
				oldN++
			}
		}
	}
	return
}

// zduSeen: some pool marked a peerOld link at least once (the gauges then
// stay on the mem line, also at 0, so the drain's end is visible).
var zduSeen atomic.Int64
