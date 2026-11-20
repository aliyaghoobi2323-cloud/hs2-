package engine

import (
	"context"
	"testing"
	"time"
)

// rel69r-rc2: HS2_B399_STALLPICK x ZDU. STALLPICK only reorders links INSIDE
// a picker tier (pickKey.cutNew); the ZDU tiers (zduOld after the current
// generation) and the generation pin (avoid-list mark) stay above it. So:
//
//   - a stalled current-generation link still beats an old-generation link
//     (the old one is tier 2, the stalled one tier 0);
//   - a healthy current-generation link beats a stalled one;
//   - a connection pinned to the old generation whose path is stalled keeps
//     every sub on old links (the stalled ones) and never crosses.

const (
	b399zPathA = "10.0.0.1>10.1.0.1"
	b399zPathB = "10.0.0.2>10.1.0.1"
)

func b399zSilent(l *mcLink, path string) {
	now := ctrlNow()
	l.mtr.path = path
	l.mtr.lastRd.Store(now - int64(time.Second)) // v3 rule 7: a fresh onset
	l.mtr.pendSince.Store(now - int64(900*time.Millisecond))
}

func b399zFresh(l *mcLink, path string) {
	l.mtr.path = path
	l.mtr.lastRd.Store(ctrlNow() + int64(time.Hour))
}

func b399zPicks(t *testing.T, m *LinkManager) map[string]*managedLink {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.pickLocked(0)
	b, _ := m.pickStormFP(0)
	return map[string]*managedLink{"plain": a, "storm": b}
}

func TestB399ZDUTiersCompose(t *testing.T) {
	for _, zdu := range []bool{true, false} {
		setZDU(t, zdu)
		setB399(t, true)
		m, olds := zduPool(4, 0)
		for _, l := range olds {
			b399zFresh(l, b399zPathB) // the old generation on a healthy path
		}
		cur := []*mcLink{zduLink(zduInstNew), zduLink(zduInstNew), zduLink(zduInstNew)}
		for _, l := range cur {
			b399zSilent(l, b399zPathA) // the current generation on a stalled path
		}
		zduAddLinks(m, cur...)
		ctx, cancel := context.WithCancel(context.Background())
		go m.b399Loop(ctx)
		b399Wait(t, m, 3)
		for i := 0; i < 30; i++ {
			for kind, ml := range b399zPicks(t, m) {
				if ml == nil {
					t.Fatalf("zdu=%v %s: no pick", zdu, kind)
				}
				inst := linkZDUInst(ml.link)
				if zdu && (inst != zduInstNew || ml.zduOld) {
					t.Fatalf("zdu=%v %s: picked %+v: an old-generation link beat a stalled current one", zdu, kind, inst)
				}
				if !zdu && mlPath(ml) != b399zPathB {
					t.Fatalf("zdu=%v %s: picked the stalled path with a healthy one free", zdu, kind)
				}
			}
		}
		// a healthy current-generation link beats the stalled ones
		h := zduLink(zduInstNew)
		b399zFresh(h, b399zPathB)
		zduAddLinks(m, h)
		for i := 0; i < 30; i++ {
			for kind, ml := range b399zPicks(t, m) {
				if ml == nil || ml.link != Link(h) && zdu {
					t.Fatalf("zdu=%v %s: want the healthy current link, got %v", zdu, kind, ml)
				}
				if ml.b399Stall.Load() {
					t.Fatalf("zdu=%v %s: picked a stalled link", zdu, kind)
				}
			}
		}
		cancel()
	}
}

// A connection pinned to the old generation, whose path is stalled while the
// current generation's path is healthy: every sub stays on old links.
func TestB399ZDUPinNeverCrosses(t *testing.T) {
	setLite(t, false)
	setZDU(t, true)
	setB399(t, true)
	for _, nolock := range []bool{true, false} {
		setOpenNoLock(t, nolock)
		x0 := zduStat.crossgen.Load()
		m, olds := zduPool(3, 0)
		for _, l := range olds {
			b399zFresh(l, b399zPathA)
		}
		var pinned []*mconnEdge
		for i := 0; i < 4; i++ {
			e := zduNewEdge(m, uint64(300+i))
			if _, _, err := e.open(planeLight, nil); err != nil {
				t.Fatal(err)
			}
			pinned = append(pinned, e)
		}
		cur := []*mcLink{zduLink(zduInstNew), zduLink(zduInstNew), zduLink(zduInstNew)}
		for _, l := range cur {
			b399zFresh(l, b399zPathB)
		}
		zduAddLinks(m, cur...)
		for _, l := range olds {
			b399zSilent(l, b399zPathA) // the old generation's path goes silent
		}
		ctx, cancel := context.WithCancel(context.Background())
		go m.b399Loop(ctx)
		b399Wait(t, m, 3)
		for r := 0; r < 4; r++ {
			for _, p := range []mPlane{planeLight, planeBulk} {
				for _, e := range pinned {
					_, l, err := e.open(p, nil)
					if err != nil {
						t.Fatalf("nolock=%v pinned sub: %v", nolock, err)
					}
					if in := linkZDUInst(l.(Link)); in != zduInstOld {
						t.Fatalf("nolock=%v pinned old conn sub crossed to %+v", nolock, in)
					}
				}
			}
		}
		// new conns go to the current generation (healthy path)
		e := zduNewEdge(m, 399)
		if _, l, err := e.open(planeLight, nil); err != nil || linkZDUInst(l.(Link)) != zduInstNew {
			t.Fatalf("nolock=%v new conn: %v on %v", nolock, err, l)
		}
		if on, _ := zduStreamsOn(cur, zduInstNew); on != 1 {
			t.Fatalf("nolock=%v current links carry %d streams, want only the new conn's 1", nolock, on)
		}
		if x := zduStat.crossgen.Load() - x0; x != 0 {
			t.Fatalf("nolock=%v zdu_crossgen_reject +%d", nolock, x)
		}
		for _, e := range append(pinned, e) {
			e.end()
		}
		cancel()
	}
}
