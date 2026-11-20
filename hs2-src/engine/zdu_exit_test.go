package engine

import (
	"context"
	"encoding/binary"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// zduExitPool is a test exit pool whose links report the Iran instance that
// accepts them at dial time (iran), the way serveReverseLink binds a link's
// meter to its slot.
type zduExitPool struct {
	*testPool
	iran   atomic.Pointer[zduInst] // the Iran process now accepting links (nil: pre-ZDU Iran)
	peers  *linkPeers
	mu     sync.Mutex
	byInst map[*fakeCarrier]*linkMeter
}

func zduMeter(in zduInst) *linkMeter {
	m := &linkMeter{}
	pi := peerInfo{V2: true}
	if in.known() {
		pi.Ext, pi.Inst = capExtZDU, in
	}
	m.peerInfo.Store(&pi)
	return m
}

func newZDUExitPool(t *testing.T, min, max int) *zduExitPool {
	zp := &zduExitPool{testPool: newTestPool(t, min, max), peers: &linkPeers{},
		byInst: map[*fakeCarrier]*linkMeter{}}
	zp.exitPool.peers = zp.peers
	inner := zp.testPool.serve
	zp.testPool.serve = func(ctx context.Context, c dialedLink) string {
		var in zduInst
		if p := zp.iran.Load(); p != nil {
			in = *p
		}
		m := zduMeter(in)
		zp.peers.add(m)
		zduExitBind(ctx, m)
		zp.mu.Lock()
		zp.byInst[c.(*fakeCarrier)] = m
		zp.mu.Unlock()
		defer zp.peers.remove(m)
		return inner(ctx, c)
	}
	return zp
}

func (zp *zduExitPool) setIran(in zduInst) { zp.iran.Store(&in) }

// liveOf counts (or with end, ends) the live links of instance in.
func (zp *zduExitPool) liveOf(in zduInst, end bool) int {
	zp.testPool.mu.Lock()
	live := append([]*fakeCarrier(nil), zp.testPool.live...)
	zp.testPool.mu.Unlock()
	n := 0
	for _, c := range live {
		zp.mu.Lock()
		m := zp.byInst[c]
		zp.mu.Unlock()
		if m != nil && m.peerInfo.Load().Inst == in {
			n++
			if end {
				c.end()
			}
		}
	}
	return n
}

// meterOf returns the meter of one live link of instance in.
func (zp *zduExitPool) meterOf(in zduInst) *linkMeter {
	zp.mu.Lock()
	defer zp.mu.Unlock()
	for _, m := range zp.byInst {
		if m.peerInfo.Load().Inst == in {
			return m
		}
	}
	return nil
}

func (zp *zduExitPool) legacy() int {
	zp.exitPool.mu.Lock()
	defer zp.exitPool.mu.Unlock()
	return zp.exitPool.zduLegacyLocked()
}

func (zp *zduExitPool) want() int {
	zp.exitPool.mu.Lock()
	defer zp.exitPool.mu.Unlock()
	return zp.exitPool.want
}

func zduResetExitStat(t *testing.T) {
	r, c := zduExitStat.legacyRetired.Load(), zduExitStat.ctlOld.Load()
	zduExitStat.legacyRetired.Store(0)
	zduExitStat.ctlOld.Store(0)
	t.Cleanup(func() { zduExitStat.legacyRetired.Store(r); zduExitStat.ctlOld.Store(c) })
}

// sendCtl writes target n on a poolctl stream served for the link of meter m.
func sendCtl(t *testing.T, pool *exitPool, m *linkMeter, n int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		servePoolCtlFrom(ctx, struct {
			io.Reader
			io.WriteCloser
		}{pr, pw}, pool, m)
		close(done)
	}()
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], uint16(n))
	pw.Write(b[:])
	pw.Close()
	<-done
}

// An Iran handover through a ZDU exit: the old process's bump dials the new
// pool, its slots then become legacy (out of want), poolctl from the old
// links is ignored, and the old links retire without a redial when they end.
func TestZDUExitLegacySlots(t *testing.T) {
	setZDU(t, true)
	zduResetExitStat(t)
	zp := newZDUExitPool(t, 1, 64)
	zp.setIran(zduInstOld)
	zp.setTarget(5)
	eventually(t, "5 old links", func() bool { return zp.liveOf(zduInstOld, false) == 5 })
	if l := zp.legacy(); l != 0 {
		t.Fatalf("one generation: legacy %d", l)
	}
	// handover: only the new process accepts; the old one bumps by its warm 5
	zp.setIran(zduInstNew)
	zp.setTarget(10)
	eventually(t, "5 new links", func() bool { return zp.liveOf(zduInstNew, false) == 5 })
	if l := zp.legacy(); l != 5 {
		t.Fatalf("legacy %d, want the 5 old slots", l)
	}
	// the new process's own target: 5 = its 5 links, nothing more
	sendCtl(t, zp.exitPool, zp.meterOf(zduInstNew), 5)
	if w := zp.want(); w != 5 {
		t.Fatalf("want %d after the new target 5", w)
	}
	// the old process's target (a late refresh) is ignored
	sendCtl(t, zp.exitPool, zp.meterOf(zduInstOld), 12)
	if w := zp.want(); w != 5 || zduExitStat.ctlOld.Load() != 1 {
		t.Fatalf("old poolctl applied: want %d ctlOld %d", w, zduExitStat.ctlOld.Load())
	}
	// the new pool grows by one: one dial, legacy slots do not fill it
	d0 := zp.dials.Load()
	sendCtl(t, zp.exitPool, zp.meterOf(zduInstNew), 6)
	eventually(t, "6 new links", func() bool { return zp.liveOf(zduInstNew, false) == 6 })
	if d := zp.dials.Load() - d0; d != 1 {
		t.Fatalf("%d dials for target 5->6, want 1", d)
	}
	// the old process drained: its links end, the slots retire, no redial
	d0 = zp.dials.Load()
	zp.liveOf(zduInstOld, true)
	eventually(t, "old slots retired", func() bool { return zduExitStat.legacyRetired.Load() == 5 })
	eventually(t, "6 slots", func() bool { return zp.slots() == 6 })
	time.Sleep(50 * time.Millisecond)
	if d := zp.dials.Load() - d0; d != 0 {
		t.Fatalf("%d redials of legacy slots", d)
	}
	if n := zp.liveCount(); n != 6 {
		t.Fatalf("live %d, want 6", n)
	}
	if s := zduCountsLine(); !strings.Contains(s, "zdu_exit_legacy_retired=5 zdu_poolctl_old=1") {
		t.Fatalf("mem line %q", s)
	}
}

// The new Iran process dies in probation: its links go, the newest instance
// falls back to the old one and the old slots count again (no retire).
func TestZDUExitProbationFallback(t *testing.T) {
	setZDU(t, true)
	zduResetExitStat(t)
	zp := newZDUExitPool(t, 1, 64)
	zp.setIran(zduInstOld)
	zp.setTarget(4)
	eventually(t, "4 old", func() bool { return zp.liveOf(zduInstOld, false) == 4 })
	zp.setIran(zduInstNew)
	zp.setTarget(8)
	eventually(t, "4 new", func() bool { return zp.liveOf(zduInstNew, false) == 4 })
	if l := zp.legacy(); l != 4 {
		t.Fatalf("legacy %d", l)
	}
	zp.setIran(zduInstOld) // the old process re-opens accept
	sendCtl(t, zp.exitPool, zp.meterOf(zduInstOld), 4)
	if zduExitStat.ctlOld.Load() != 1 { // the new links are still up: still ignored
		t.Fatalf("ctlOld %d", zduExitStat.ctlOld.Load())
	}
	zp.liveOf(zduInstNew, true) // the new process died
	eventually(t, "new links gone", func() bool { return zp.liveOf(zduInstNew, false) == 0 })
	eventually(t, "legacy 0", func() bool { return zp.legacy() == 0 })
	sendCtl(t, zp.exitPool, zp.meterOf(zduInstOld), 4)
	if w := zp.want(); w != 4 {
		t.Fatalf("want %d: the old process's target applies again", w)
	}
	if r := zduExitStat.legacyRetired.Load(); r != 0 {
		t.Fatalf("legacy retired %d: the new links were current, not legacy", r)
	}
	if n := zp.liveOf(zduInstOld, false); n < 4 {
		t.Fatalf("old links %d, want >= 4 (none retired)", n)
	}
}

// Mixed versions: a pre-ZDU Iran (no instance) with the exit key on, and a
// ZDU Iran with the exit key off, behave as before: a link that ends is
// redialed, every poolctl applies, nothing is bound or counted.
func TestZDUExitMixedVersions(t *testing.T) {
	for _, c := range []struct {
		name  string
		key   bool
		insts [2]zduInst
	}{
		{"pre-ZDU Iran, exit key on", true, [2]zduInst{{}, {}}},
		{"ZDU Iran, exit key off", false, [2]zduInst{zduInstOld, zduInstNew}},
	} {
		t.Run(c.name, func(t *testing.T) {
			setZDU(t, c.key)
			zduResetExitStat(t)
			zp := newZDUExitPool(t, 1, 64)
			zp.setIran(c.insts[0])
			zp.setTarget(3)
			eventually(t, "3 links", func() bool { return zp.liveCount() == 3 })
			zp.setIran(c.insts[1])
			zp.setTarget(5)
			eventually(t, "5 links", func() bool { return zp.liveCount() == 5 })
			if l := zp.legacy(); l != 0 {
				t.Fatalf("legacy %d", l)
			}
			sendCtl(t, zp.exitPool, zp.meterOf(c.insts[0]), 5)
			if zduExitStat.ctlOld.Load() != 0 {
				t.Fatal("poolctl ignored")
			}
			d0 := zp.dials.Load()
			zp.endOne()
			eventually(t, "redial", func() bool { return zp.dials.Load() == d0+1 && zp.liveCount() == 5 })
			if zduExitStat.legacyRetired.Load() != 0 {
				t.Fatal("legacy retire")
			}
			if !c.key {
				zp.exitPool.mu.Lock()
				for _, s := range zp.exitPool.slots {
					if s.zmtr.Load() != nil {
						t.Error("key off: slot bound to its link")
					}
				}
				zp.exitPool.mu.Unlock()
				if s := zduCountsLine(); s != "" {
					t.Fatalf("key off line %q", s)
				}
			}
		})
	}
}

// Key off: the hot paths allocate nothing and give the pre-ZDU answers; key
// on with bound slots, the legacy count and the poolctl test allocate nothing.
func TestZDUExitAllocs(t *testing.T) {
	for _, key := range []bool{false, true} {
		setZDU(t, key)
		zp := newZDUExitPool(t, 1, 64)
		zp.setIran(zduInstOld)
		zp.setTarget(3)
		eventually(t, "3", func() bool { return zp.liveCount() == 3 })
		zp.setIran(zduInstNew)
		zp.setTarget(5)
		eventually(t, "5", func() bool { return zp.liveCount() == 5 })
		mo := zp.meterOf(zduInstOld)
		p := zp.exitPool
		var leg int // the slots that count toward want
		var old bool
		a := testing.AllocsPerRun(200, func() {
			p.mu.Lock()
			leg = p.curSlotsLocked()
			p.mu.Unlock()
			old = p.zduCtlFromOld(mo)
			if !key {
				p.setTarget(5) // unchanged target: no slot, no line
			}
		})
		if a != 0 {
			t.Fatalf("key %v: %v allocs", key, a)
		}
		if key && (leg != 2 || !old) {
			t.Fatalf("key on: cur slots %d old %v, want 2 true", leg, old)
		}
		if !key && (leg != 5 || old) {
			t.Fatalf("key off: cur slots %d old %v, want 5 false", leg, old)
		}
		if !key {
			if ctx := zduSlotCtx(context.Background(), &exitSlot{}); ctx != context.Background() {
				t.Fatal("key off: slot context wrapped")
			}
		}
	}
}

// The foreign-drain term: added to ctlTarget while the old process reports,
// not with a ZDU exit, not when stale, not with the key off.
func TestZDUForeignDrainTerm(t *testing.T) {
	setZDU(t, true)
	m, _ := zduPool(0, 0)
	m.max = 40
	m.target.Store(5)
	if got := m.ctlTarget(); got != 5 {
		t.Fatalf("base %d", got)
	}
	ch := m.targetChanged()
	m.SetZDUForeignDrain(7)
	select {
	case <-ch:
	default:
		t.Fatal("no target change signalled")
	}
	if got := m.ctlTarget(); got != 12 {
		t.Fatalf("with 7 old links: %d, want 12", got)
	}
	// a pre-ZDU exit answered: the term stays
	m.exitInfo.Store(&peerInfo{V2: true})
	if got := m.ctlTarget(); got != 12 {
		t.Fatalf("pre-ZDU exit: %d, want 12", got)
	}
	// a ZDU exit counts per generation: the term is dropped
	m.exitInfo.Store(&peerInfo{V2: true, Ext: capExtZDU, Inst: zduInstNew})
	if got := m.ctlTarget(); got != 5 {
		t.Fatalf("ZDU exit: %d, want 5", got)
	}
	m.exitInfo.Store(nil)
	if got := m.zduForeignTerm(func() time.Time { return time.Now().Add(zduForeignTTL + time.Second) }); got != 0 {
		t.Fatalf("stale report: %d", got)
	}
	var a float64
	a = testing.AllocsPerRun(200, func() { _ = m.ctlTarget() })
	if a != 0 {
		t.Fatalf("ctlTarget allocs %v", a)
	}
	m.SetZDUForeignDrain(0) // the old process exited
	if got := m.ctlTarget(); got != 5 {
		t.Fatalf("after exit: %d", got)
	}
	setZDU(t, false)
	m.SetZDUForeignDrain(9)
	if got := m.ctlTarget(); got != 5 {
		t.Fatalf("key off: %d", got)
	}
	a = testing.AllocsPerRun(200, func() { _ = m.ctlTarget() })
	if a != 0 {
		t.Fatalf("key off ctlTarget allocs %v", a)
	}
}
