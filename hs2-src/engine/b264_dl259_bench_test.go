package engine

import (
	"fmt"
	"testing"
	"time"
)

// BenchmarkB264DL259Gate (b264 audit, 4x scale): one dl259 gate decision of
// a download with 4 stripe subs against 400 exit links on 10 paths. It runs
// every stripeQEvery (100 ms) per striping connection, under the global
// stripePathQ mutex.
func BenchmarkB264DL259Gate(b *testing.B) { benchDL259Gate(b, false, false) }

// ...Uncached: the pre-b264 code (every decision sorts every path).
func BenchmarkB264DL259GateUncached(b *testing.B) { benchDL259Gate(b, true, false) }

// ...Rebuild: the cache rebuilt on every decision (a new measurement each
// time: the worst case, one striping conn alone).
func BenchmarkB264DL259GateRebuild(b *testing.B) { benchDL259Gate(b, false, true) }

func benchDL259Gate(b *testing.B, uncached, rebuild bool) {
	p := &stripeQ{links: map[*linkMeter]*stripeLinkQ{}}
	var own []*linkMeter
	for i := 0; i < 400; i++ {
		mt := &linkMeter{path: fmt.Sprintf("k>%d", i%10)}
		p.links[mt] = &stripeLinkQ{busy: i%3 != 0, q: time.Duration(i%7) * time.Millisecond}
		if i < 5 {
			own = append(own, mt)
		}
	}
	old := dl259On.Load()
	dl259On.Store(true)
	defer dl259On.Store(old)
	dl259Uncached.Store(uncached)
	defer dl259Uncached.Store(false)
	pri := &msub{}
	m := &mConn{subs: []*msub{pri}}
	meters := map[*msub]*linkMeter{}
	for i := 1; i < 5; i++ {
		s := &msub{stripe: true}
		m.subs = append(m.subs, s)
		meters[s] = own[i]
	}
	dl259TestMeter = func(s *msub) *linkMeter { return meters[s] }
	defer func() { dl259TestMeter = nil }()
	now := time.Now()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if rebuild {
			p.at = now.Add(time.Duration(i) * time.Millisecond)
		}
		m.dl259GateLocked(p, own, pri, 0, 3*time.Millisecond, now.Add(time.Duration(i)*100*time.Millisecond))
	}
}
