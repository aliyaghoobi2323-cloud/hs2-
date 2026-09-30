package udpcarrier

import (
	"math/rand/v2"
	"testing"
	"time"
)

// govSim drives a Governor with fake carriers and a fake clock: each tick every
// carrier "sends" rate bytes/s (capped by the governor when it is capping) and
// reports the loss the scenario assigns it.
type govSim struct {
	g     *Governor
	cs    []*Conn
	clock time.Time
	logs  []string
}

func newGovSim(n int) *govSim {
	s := &govSim{clock: time.Unix(1_000_000, 0)}
	s.g = NewGovernor(func(f string, a ...any) { s.logs = append(s.logs, f) })
	s.g.now = func() time.Time { return s.clock }
	for i := 0; i < n; i++ {
		c := &Conn{rc: newRateControl()}
		s.cs = append(s.cs, c)
		s.g.attach(c)
	}
	return s
}

// step advances one governor tick. want is the pool's offered rate in
// bytes/s (split evenly); loss(i, sent) gives carrier i's reported loss for
// the pool's actual total sent; queue is every carrier's standing queue.
func (s *govSim) step(want float64, loss func(i int, total float64) float64, queue float64) float64 {
	s.clock = s.clock.Add(govTickEvery)
	total := want
	if c := s.g.CapBytes(); c > 0 && total > c {
		total = c
	}
	per := total / float64(len(s.cs))
	for i, c := range s.cs {
		c.rc.sent.Add(uint64(per * govTickEvery.Seconds()))
		for r := 0; r < 5; r++ { // five 100 ms reports per tick
			s.g.report(c, loss(i, total), queue)
		}
	}
	s.g.tick()
	return total
}

const mb = 1e6 / 8 // bytes/s per Mbit/s

// A per-IP policer at 60 Mbit/s; the pool offers 150. Above the limit every
// few seconds an episode drops ~40% on all carriers at once, no queue. The
// governor must cap the pool, and settle it near (under) the policer.
func policerLoss(limit float64, tick *int) func(int, float64) float64 {
	return func(_ int, total float64) float64 {
		if total > limit && *tick%12 == 0 { // an episode every 6 s while above
			return 0.4
		}
		return 0.001
	}
}

func TestGovernorCapsAPolicer(t *testing.T) {
	s := newGovSim(8)
	tick := 0
	var sent []float64
	for tick = 1; tick <= 400; tick++ { // 200 s
		sent = append(sent, s.step(150*mb, policerLoss(60*mb, &tick), 0.001))
	}
	if !s.g.Capped() {
		t.Fatalf("a per-IP policer was never detected; logs: %v", s.logs)
	}
	// settled: mostly under the policer, and not far below it
	var sum float64
	for _, v := range sent[len(sent)-100:] {
		sum += v
	}
	avg := sum / 100
	if avg > 64*mb || avg < 40*mb {
		t.Fatalf("pool settles at %.1f Mbit/s against a 60 Mbit/s policer (want ~45-60)", avg/mb)
	}
	t.Logf("settled at %.1f Mbit/s (policer 60); cap now %.1f", avg/mb, s.g.CapBytes()/mb)
}

// Random loss spread over carriers and time (the Iran UDP path's ~10%): never
// simultaneous on most carriers, so never a policer — no cap.
func TestGovernorIgnoresRandomLoss(t *testing.T) {
	s := newGovSim(8)
	r := rand.New(rand.NewPCG(1, 2))
	for i := 0; i < 400; i++ {
		s.step(150*mb, func(int, float64) float64 {
			if r.Float64() < 0.25 {
				return 0.10
			}
			return 0.0
		}, 0.001)
	}
	if s.g.Capped() {
		t.Fatalf("random, non-simultaneous loss capped the pool: %v", s.logs)
	}
}

// Loss with a standing queue is congestion at a buffer — the carriers' own
// delay control handles that; the governor must not cap.
func TestGovernorIgnoresCongestionLoss(t *testing.T) {
	s := newGovSim(8)
	tick := 0
	for tick = 1; tick <= 200; tick++ {
		s.step(150*mb, policerLoss(60*mb, &tick), 0.030) // 30 ms queue standing
	}
	if s.g.Capped() {
		t.Fatalf("congestion (loss with a queue) capped the pool: %v", s.logs)
	}
}

// Simultaneous loss episodes that do NOT depend on the rate (a flapping path)
// continue even at the lowest cap: the governor must give up and lift it.
func TestGovernorLiftsCapWhenLossIsNotRateDependent(t *testing.T) {
	s := newGovSim(8)
	tick := 0
	lifted := false
	for tick = 1; tick <= 600; tick++ {
		s.step(150*mb, func(int, float64) float64 {
			if tick%12 == 0 {
				return 0.4 // every 6 s whatever we send
			}
			return 0.001
		}, 0.001)
		if tick > 40 && !s.g.Capped() && s.g.restUntil.After(s.clock) {
			lifted = true
			break
		}
	}
	if !lifted {
		t.Fatalf("rate-independent episodes kept the pool capped (cap %.1f Mbit/s): %v", s.g.CapBytes()/mb, s.logs)
	}
}

// The shared bucket holds the pool to its cap whatever the number of
// carriers: 8 pacers reserving as fast as they may for 2 s send ~cap x 2 s.
func TestGovernorBucketHoldsThePoolToItsCap(t *testing.T) {
	g := NewGovernor(nil)
	clock := time.Unix(1_000_000, 0)
	g.now = func() time.Time { return clock }
	g.setCap(10 * mb) // 10 Mbit/s
	var sent int
	end := clock.Add(2 * time.Second)
	next := make([]time.Time, 8)
	for i := range next {
		next[i] = clock
	}
	for clock.Before(end) {
		// the carrier that may send next
		k := 0
		for i := range next {
			if next[i].Before(next[k]) {
				k = i
			}
		}
		clock = next[k]
		if !clock.Before(end) {
			break
		}
		wait := g.reserve(1300)
		sent += 1300
		next[k] = clock.Add(wait)
	}
	got := float64(sent) / 2 / mb
	if got < 9 || got > 11.5 {
		t.Fatalf("8 carriers under a 10 Mbit/s cap sent %.1f Mbit/s", got)
	}
}
