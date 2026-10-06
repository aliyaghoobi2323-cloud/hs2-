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
	return s.stepPush(want, loss, queue, true)
}

// stepPush is step with control over whether the carriers are using their
// allowance (pushing). A saturating sender pushes; a lightly loaded pool does
// not, and its loss must not be read as a rate cap.
func (s *govSim) stepPush(want float64, loss func(i int, total float64) float64, queue float64, pushing bool) float64 {
	s.clock = s.clock.Add(govTickEvery)
	total := want
	if c := s.g.CapBytes(); c > 0 && total > c {
		total = c
	}
	per := total / float64(len(s.cs))
	for i, c := range s.cs {
		c.rc.sent.Add(uint64(per * govTickEvery.Seconds()))
		c.rc.pushing.Store(pushing)
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
	if !s.g.Capped() || !s.g.Confirmed() {
		t.Fatalf("a per-IP policer was not detected and confirmed (capped=%v confirmed=%v); logs: %v", s.g.Capped(), s.g.Confirmed(), s.logs)
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

// Steady random loss on every carrier at once (the Iran UDP path: ~10% on
// everything, all the time) is simultaneous and queue-free too — but it is
// the pool's usual loss, not an episode above it, so it must never cap. (It
// did, in the lab: goodput 29 -> 23 Mbit/s.)
func TestGovernorIgnoresSteadyRandomLossOnAllCarriers(t *testing.T) {
	s := newGovSim(4)
	r := rand.New(rand.NewPCG(3, 4))
	for i := 0; i < 400; i++ {
		// one value per tick: over a few hundred packets the measured loss
		// swings widely around its mean (here 8%), dipping below 5% and back
		cur := 0.02 + 0.12*r.Float64()
		s.step(50*mb, func(int, float64) float64 { return cur }, 0.001)
	}
	if s.g.Capped() || len(s.logs) > 0 {
		t.Fatalf("steady 2-14%% random loss on every carrier capped the pool: %v", s.logs)
	}
}

// A lightly loaded pool (a reconnect: carriers sending well under their
// allowance) that sees loss must never be read as a policer. The field hit
// exactly this — the governor capped at 90% of ~27 Mbit/s of reconnect
// traffic, not of a real limit.
func TestGovernorIgnoresLossWhenNotPushing(t *testing.T) {
	s := newGovSim(8)
	tick := 0
	for tick = 1; tick <= 400; tick++ {
		// the same episodic loss as the policer test, but the carriers are NOT
		// using their allowance
		s.stepPush(150*mb, policerLoss(60*mb, &tick), 0.001, false)
	}
	if s.g.Capped() {
		t.Fatalf("a lightly loaded pool was capped on loss it did not cause by its rate: %v", s.logs)
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
// keep their rhythm under the cap: the test must end quickly with the cap
// lifted, parity must never be held meanwhile (those bursts are real loss FEC
// has to repair), and each further lift rests twice as long before re-testing.
func TestGovernorLiftsCapWhenLossIsNotRateDependent(t *testing.T) {
	s := newGovSim(8)
	var cappedAt, liftedAt int
	var rests []time.Duration
	held := false
	for tick := 1; tick <= 3000; tick++ { // 25 min
		s.step(150*mb, func(int, float64) float64 {
			if tick%8 == 0 {
				return 0.4 // every 4 s whatever we send
			}
			return 0.001
		}, 0.001)
		if s.g.Confirmed() {
			held = true
		}
		if s.g.Capped() && cappedAt == 0 {
			cappedAt = tick
		}
		if cappedAt != 0 && liftedAt == 0 && !s.g.Capped() {
			liftedAt = tick
		}
		if s.g.restUntil.After(s.clock) && (len(rests) == 0 || s.g.lifts > len(rests)) {
			rests = append(rests, s.g.restUntil.Sub(s.clock))
		}
	}
	if held {
		t.Fatal("parity was held for a flapping path (it treated rate-independent bursts as a policer's)")
	}
	if cappedAt == 0 || liftedAt == 0 {
		t.Fatalf("not tested and lifted (capped at tick %d, lifted at %d): %v", cappedAt, liftedAt, s.logs)
	}
	if d := time.Duration(liftedAt-cappedAt) * govTickEvery; d > 40*time.Second {
		t.Fatalf("the test of a flapping path took %s (want <= 40 s)", d)
	}
	if len(rests) < 2 || rests[1] < rests[0]*3/2 {
		t.Fatalf("the rest after a repeated lift did not grow: %v", rests)
	}
	t.Logf("tested for %s; rests %v", time.Duration(liftedAt-cappedAt)*govTickEvery, rests)
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

// BusyQueue is the median queue of the carriers using their allowance; it
// and Share hold through govShareHold intervals without busy carriers (a
// base probe slows them all at once), then clear.
func TestGovernorBusyQueueAndShareHold(t *testing.T) {
	s := newGovSim(3)
	none := func(int, float64) float64 { return 0 }
	s.step(30*mb, none, 0.2)
	if q := s.g.BusyQueue(); q != 0.2 {
		t.Fatalf("busy queue %.3f s, want 0.2", q)
	}
	share := s.g.Share()
	for i := 0; i < govShareHold; i++ {
		s.stepPush(30*mb, none, 0.2, false)
		if q, sh := s.g.BusyQueue(), s.g.Share(); q != 0.2 || sh != share {
			t.Fatalf("%d intervals without busy carriers: busy queue %.3f s, share %.0f (want 0.2, %.0f)", i+1, q, sh, share)
		}
	}
	s.stepPush(30*mb, none, 0.2, false)
	if q, sh := s.g.BusyQueue(), s.g.Share(); q != 0 || sh != 0 {
		t.Fatalf("past the hold: busy queue %.3f s, share %.0f, want both 0", q, sh)
	}
	s.step(30*mb, none, 0.01)
	if q := s.g.BusyQueue(); q != 0.01 {
		t.Fatalf("busy queue %.3f s, want 0.01", q)
	}
}
