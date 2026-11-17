package engine

import (
	"fmt"
	"maps"
	"math/rand"
	"testing"
	"time"
)

// b264 L4: the per-measurement per-path cache gives the same per-path queues
// and the same gate decisions (width queue and cut set) as the pre-b264
// uncached code, over random links, paths, own sets, measurements, link
// tracks/untracks and clocks.
func TestB264DL259CacheSame(t *testing.T) {
	old := dl259On.Load()
	dl259On.Store(true)
	t.Cleanup(func() { dl259On.Store(old); dl259TestMeter = nil; dl259Uncached.Store(false) })
	rng := rand.New(rand.NewSource(259))
	for round := 0; round < 200; round++ {
		p := &stripeQ{links: map[*linkMeter]*stripeLinkQ{}}
		npath := 1 + rng.Intn(6)
		var all []*linkMeter
		add := func() {
			path := fmt.Sprintf("k>%d", rng.Intn(npath))
			if rng.Intn(15) == 0 {
				path = "" // unknown path: left out
			}
			mt := &linkMeter{path: path}
			p.links[mt] = &stripeLinkQ{}
			p.gen++
			all = append(all, mt)
		}
		measure := func(now time.Time) {
			p.at = now
			p.gen++ // as delay(): every measurement counts (same-time measurements too)
			for _, l := range p.links {
				l.busy = rng.Intn(3) != 0
				l.q = time.Duration(rng.Intn(8000)) * time.Microsecond // ties likely
			}
		}
		for i, n := 0, 1+rng.Intn(60); i < n; i++ {
			add()
		}
		pri := &msub{}
		mc, mu := &mConn{subs: []*msub{pri}}, &mConn{subs: []*msub{pri}}
		meters := map[*msub]*linkMeter{}
		var own []*linkMeter
		for i, n := 0, 1+rng.Intn(5); i < n; i++ {
			s := &msub{stripe: true, dead: rng.Intn(8) == 0}
			mt := all[rng.Intn(len(all))]
			meters[s] = mt
			own = append(own, mt)
			mc.subs = append(mc.subs, s)
			mu.subs = append(mu.subs, s)
		}
		dl259TestMeter = func(s *msub) *linkMeter { return meters[s] }
		now := time.Unix(1_900_000_000, 0)
		measure(now)
		for step := 0; step < 80; step++ {
			now = now.Add(time.Duration(rng.Intn(200)) * time.Millisecond)
			switch rng.Intn(6) {
			case 0:
				measure(now)
			case 1:
				add()
			case 2:
				if len(all) > 0 {
					i := rng.Intn(len(all))
					delete(p.links, all[i])
					p.gen++
					all = append(all[:i], all[i+1:]...)
				}
			}
			o := own
			if rng.Intn(4) == 0 {
				o = nil
			}
			base := time.Duration(rng.Intn(2000)) * time.Microsecond
			target := time.Duration(1+rng.Intn(5)) * time.Millisecond
			dl259Uncached.Store(false)
			qc := p.pathQ(o)
			gc := mc.dl259GateLocked(p, o, pri, base, target, now)
			dl259Uncached.Store(true)
			qu := p.pathQ(o)
			gu := mu.dl259GateLocked(p, o, pri, base, target, now)
			if !maps.Equal(qc, qu) {
				t.Fatalf("round %d step %d: pathQ cached %v uncached %v", round, step, qc, qu)
			}
			if gc != gu || !maps.Equal(mc.dl259Cut, mu.dl259Cut) {
				t.Fatalf("round %d step %d: gate cached %v %v uncached %v %v", round, step, gc, mc.dl259Cut, gu, mu.dl259Cut)
			}
		}
	}
}
