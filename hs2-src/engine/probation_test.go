package engine

import (
	"context"
	"testing"
)

// A link degraded by a loss burst that then comes right stops draining —
// it retires, keeping its users — with HS2_LINK_PROBATION; without it, or if
// it stays lossy, it drains as before.
func TestProbationRecoversDegradedLink(t *testing.T) {
	run := func(probation, staysLossy bool) *managedLink {
		old := linkProbation
		linkProbation = probation
		defer func() { linkProbation = old }()
		m := NewLinkManager(&fakeDialer{}, 4, 8, 50, nil)
		m.gate = instantGate()
		good := []*meteredFakeLink{newMeteredFake(), newMeteredFake(), newMeteredFake()}
		bad := newMeteredFake()
		for _, g := range good {
			ml := &managedLink{link: g, mtr: g.m}
			ml.users.Store(5)
			m.links = append(m.links, ml)
		}
		badML := &managedLink{link: bad, mtr: bad.m}
		badML.users.Store(5)
		m.links = append(m.links, badML)
		step := func(badRetrans uint64) {
			for _, g := range good {
				g.active(200<<10, 0)
			}
			bad.active(200<<10, badRetrans)
			m.sampleHealth()
		}
		step(30) // seed
		for i := 0; i < degradeStreak; i++ {
			step(30) // ~20% loss
		}
		if !badML.degraded {
			t.Fatal("the lossy link was not degraded")
		}
		m.heal(context.Background())
		settle(m)
		if !badML.draining {
			t.Fatal("the degraded link is not draining")
		}
		after := uint64(0) // the burst is over
		if staysLossy {
			after = 30
		}
		for i := 0; i < probationTicks+1; i++ {
			step(after)
		}
		return badML
	}
	if ml := run(true, false); ml.degraded || ml.draining || !ml.retiring || ml.users.Load() != 5 {
		t.Fatalf("probation, recovered: degraded=%v draining=%v retiring=%v users=%d; want it retiring with its 5 users",
			ml.degraded, ml.draining, ml.retiring, ml.users.Load())
	}
	if ml := run(true, true); !ml.degraded || !ml.draining {
		t.Fatal("probation: a link still losing 20% stopped draining")
	}
	if ml := run(false, false); !ml.degraded || !ml.draining {
		t.Fatal("without the key a degraded link must drain as before")
	}
}
