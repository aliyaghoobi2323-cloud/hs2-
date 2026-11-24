package engine

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"sync/atomic"
	"time"
)

// ZDU building block B5 (proposals/zdu350.md §7 S4): the generation-aware
// exit pool (Kharej), and on the Iran side the foreign-drain term.
//
// During an Iran handover the exit holds links to two Iran processes: the old
// one (draining its connections) and the new one (only it accepts links). Each
// link's own kindInfo from the edge carries the Iran instance (B2). With
// HS2_ZDU=1 on the exit:
//
//   - a slot whose link announced an instance older than the newest one among
//     the exit's live links (linkPeers.zduCur) is LEGACY: it does not count
//     toward want (setTarget, retireIfOver, addSpares, the honest-log over
//     test, the b331 spare cap), so the new Iran's target is met with new
//     links next to the old ones; and when its link ends it retires instead
//     of redialing (the redial could only land on the new process, which
//     already has its own slots for its target);
//   - servePoolCtl applies a target only from a link that is not of an older
//     instance: the old process's last word never resizes the pool for the
//     new one (B6 has the old process send none; this is the guard).
//
// A link whose edge sent no instance (no answer yet, a pre-ZDU Iran, the Iran
// with the key off) is never legacy and its poolctl is applied, as today. If
// the new Iran process dies in probation its links go, the newest instance
// falls back to the old one, and the old slots count again at once (the
// legacy test is made on every use, not stored).
//
// Iran side, the foreign-drain term: while the old process drains it reports
// its live link count (DRAINSTAT old_links, every 2 s, slot S5/S6 wires the
// channel). The new process adds it to ctlTarget so a PRE-ZDU exit, which
// counts the old process's links against want, still dials the new pool. It
// is dropped once the exit announced capExtZDU (that exit counts only the
// current generation, above), when the old process reports 0 or exits, and
// when no report came for zduForeignTTL (the old process died).
//
// HS2_ZDU off: no slot is ever bound to its link, no legacy count, every
// poolctl is applied, and the foreign term is never added (one atomic load on
// each path; TestZDUExitOffIdentical).

// zduExitStat: the exit's B5 counters (mem line, key on, once non-zero).
var zduExitStat struct {
	legacyRetired atomic.Int64 // zdu_exit_legacy_retired: legacy slots retired at link end (no redial)
	ctlOld        atomic.Int64 // zdu_poolctl_old: targets ignored from an older-instance link
}

// zduSlotKey carries the exit slot in the context its link is served with
// (key on only), so the serve path can bind the link's meter to its slot.
type zduSlotKey struct{}

// zduSlotCtx returns ctx carrying slot s when the key is on, ctx otherwise.
func zduSlotCtx(ctx context.Context, s *exitSlot) context.Context {
	if !zduOn.Load() {
		return ctx
	}
	return context.WithValue(ctx, zduSlotKey{}, s)
}

// zduExitBind binds the link meter m to the slot its link is served for
// (serveReverseLink). No-op with the key off or outside a pool slot.
func zduExitBind(ctx context.Context, m *linkMeter) {
	if !zduOn.Load() || m == nil {
		return
	}
	if s, _ := ctx.Value(zduSlotKey{}).(*exitSlot); s != nil {
		s.zmtr.Store(m)
	}
}

// zduCur is the newest Iran instance among the exit's live links (none when
// no live link announced one).
func (p *linkPeers) zduCur() (cur zduInst) {
	if p == nil {
		return
	}
	p.mu.Lock()
	for m := range p.m {
		if pi := m.peerInfo.Load(); pi.zdu() && pi.Inst.newer(cur) {
			cur = pi.Inst
		}
	}
	p.mu.Unlock()
	return
}

// zduOlder reports whether meter m's edge announced an instance older than cur.
func zduOlder(m *linkMeter, cur zduInst) bool {
	if m == nil || !cur.known() {
		return false
	}
	pi := m.peerInfo.Load()
	return pi.zdu() && cur.newer(pi.Inst)
}

// zduLegacyLocked counts the slots whose live link is of an older Iran
// instance. Caller holds p.mu (lock order: p.mu, then peers.mu).
func (p *exitPool) zduLegacyLocked() int {
	if !zduOn.Load() {
		return 0
	}
	n := 0
	var cur zduInst
	first := true
	for _, s := range p.slots {
		m := s.zmtr.Load()
		if m == nil {
			continue
		}
		if first { // the peer scan only when some slot is bound
			cur, first = p.peers.zduCur(), false
			if !cur.known() {
				return 0
			}
		}
		if zduOlder(m, cur) {
			n++
		}
	}
	return n
}

// curSlotsLocked: the slots that count toward want (all but the legacy ones).
// Caller holds p.mu.
func (p *exitPool) curSlotsLocked() int {
	return len(p.slots) - p.zduLegacyLocked()
}

// zduRetireLegacy, after slot s's link ended: when that link was of an older
// Iran instance than the newest still up, s leaves the pool (no redial) and
// true is returned. The binding is cleared either way.
func (p *exitPool) zduRetireLegacy(s *exitSlot) bool {
	m := s.zmtr.Swap(nil)
	if m == nil || !zduOn.Load() {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !zduOlder(m, p.peers.zduCur()) {
		return false
	}
	for i, x := range p.slots {
		if x == s {
			p.slots = append(p.slots[:i], p.slots[i+1:]...)
			s.cancel()
			if p.scout == s {
				p.scout = nil
				p.wakeLocked()
			}
			zduExitStat.legacyRetired.Add(1)
			return true
		}
	}
	return false
}

// zduCtlFromOld reports whether a poolctl target read on the link of meter m
// must be ignored: the key is on and m's edge is an older Iran instance than
// the newest live one.
func (p *exitPool) zduCtlFromOld(m *linkMeter) bool {
	if !zduOn.Load() || m == nil {
		return false
	}
	if pi := m.peerInfo.Load(); !pi.zdu() {
		return false // no instance on this link: applied as today
	}
	return zduOlder(m, p.peers.zduCur())
}

// servePoolCtlFrom is servePoolCtl for the stream of the link with meter m:
// with the key on, a target from an older-instance link is ignored.
func servePoolCtlFrom(ctx context.Context, st io.ReadWriteCloser, pool *exitPool, m *linkMeter) {
	defer st.Close()
	if pool == nil {
		io.Copy(io.Discard, st) // no pool here (direct exit): just drain
		return
	}
	buf := make([]byte, poolCtlLen)
	for ctx.Err() == nil {
		if _, err := io.ReadFull(st, buf); err != nil {
			return
		}
		if pool.zduCtlFromOld(m) {
			zduExitStat.ctlOld.Add(1)
			continue
		}
		pool.setTarget(int(binary.BigEndian.Uint16(buf)))
	}
}

// zduExitLine: " zdu_exit_legacy=N zdu_exit_legacy_retired= zdu_poolctl_old="
// for the mem line (key on, once something happened).
func zduExitLine() string {
	r, c := zduExitStat.legacyRetired.Load(), zduExitStat.ctlOld.Load()
	if r == 0 && c == 0 && zduExitBootN.Load() == 0 {
		return ""
	}
	return fmt.Sprintf(" zdu_exit_legacy_retired=%d zdu_poolctl_old=%d zdu_exit_boot=%d zdu_exit_boot_retry=%d", r, c, zduExitBootN.Load(), zduExitBootRetryN.Load())
}

// ---- Iran: the foreign-drain term -------------------------------------------

// zduForeignTTL: a foreign-drain report older than this is dropped (the old
// process reports every 2 s; a dead one stops).
const zduForeignTTL = 10 * time.Second

// zduForeign is the old process's last DRAINSTAT link count, with its time.
type zduForeign struct {
	links int
	at    time.Time
}

// SetZDUForeignDrain records the draining old process's live link count
// (DRAINSTAT old_links); 0 when it exited. Key off: ignored.
func (m *LinkManager) SetZDUForeignDrain(links int) {
	if !zduOn.Load() {
		return
	}
	var f *zduForeign
	if links > 0 {
		f = &zduForeign{links: links, at: time.Now()}
	}
	prev := m.zdu.foreign.Swap(f)
	if (prev == nil) != (f == nil) || (prev != nil && prev.links != f.links) {
		m.notifyCtl()
	}
}

// zduForeignTerm is what ctlTarget adds for the draining old process: its
// link count while fresh, the key on and the exit not ZDU-aware.
func (m *LinkManager) zduForeignTerm(now func() time.Time) int {
	f := m.zdu.foreign.Load()
	if f == nil || !zduOn.Load() {
		return 0
	}
	if m.exitInfo.Load().zdu() { // the exit counts per generation (B5)
		return 0
	}
	if now().Sub(f.at) > zduForeignTTL {
		return 0
	}
	return f.links
}
