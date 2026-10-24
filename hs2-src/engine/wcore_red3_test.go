package engine

import (
	"math"
	"testing"
	"time"
)

// TestWcoreRed3GrownSwing (red team, round 3, finding 3): the autopilot
// sizes the pool by each pressed link's capacity for ordinary connections,
// what windows within NeedStreamBuffer carry. The core's own swings (a probe
// tick, a drain tick) moved the link's rate and its grown part together:
// the least rate of the ticks less the most grown of them read a link of a
// few Mbit/s where the windows of need carried ~168 (2 MiB a 100 ms round
// trip), and the pool kept 4 links serving where base kept 2 for the same
// load (6 against 2 in skmany). The capacity is each tick's rate less its own
// grown, at its least over the ticks; a tick in which the core read no grown
// at all (the sender's PROBE_RTT) still does not take the whole rate
// (TestWcoreRedGrownSus, TestWcoreRedGrownDip).
func TestWcoreRed3GrownSwing(t *testing.T) {
	const mbit = 1e6 / 8
	m := NewLinkManager(nil, 2, 2, 50, nil)
	now := time.Unix(1e9, 0)
	m.clock = func() time.Time { return now }
	f := newMeteredFake()
	f.m.wc = wcLogLink("link 1", wcEdge)
	ml := &managedLink{link: f, mtr: f.m}
	m.links = []*managedLink{ml}
	for _, x := range []struct{ rate, grown float64 }{
		{400, 232},                        // (the first sample sets the counters' base)
		{400, 232}, {250, 82}, {380, 212}, // hold, a drain, a probe: 168 each of need
	} {
		now = now.Add(healthTick)
		f.m.wc.inStats.grown.Store(math.Float64bits(x.grown * mbit))
		f.m.rdBytes.Add(uint64(x.rate * mbit * healthTick.Seconds()))
		m.sampleHealth()
	}
	got := (ml.sustained - ml.grownSus) / mbit
	t.Logf("sustained %.0f, grownSus %.0f: capacity for ordinary connections %.0f Mbit/s", ml.sustained/mbit, ml.grownSus/mbit, got)
	if got < 150 || got > 180 {
		t.Errorf("capacity for ordinary connections %.0f Mbit/s, want ~168 (the windows of need)", got)
	}
	// and the autopilot takes it as the link's capacity
	a := newAutopilot(2, 64, 8)
	s := apSample{now: now, growable: true}
	for i := 0; i < 8; i++ { // (capMinSamples pressed links)
		s.links = append(s.links, apLink{id: i, serving: true, pressed: true, rate: 380 * mbit, sustained: ml.sustained,
			grown: 212 * mbit, grownSus: ml.grownSus, flowing: 1, open: 1})
	}
	a.decide(s)
	if c := a.capEstimate(now) / mbit; c < 150 || c > 180 {
		t.Errorf("the autopilot's capacity per link %.0f Mbit/s, want ~168", c)
	}
}
