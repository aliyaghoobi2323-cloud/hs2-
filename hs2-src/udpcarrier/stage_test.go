package udpcarrier

import (
	"math"
	"testing"
	"time"
)

// stageDrive feeds a rateControl one report per feedbackEvery: the pacer
// sends what the send stage lets out (cpu bytes/s, 0: no limit) of what the
// rate allows, all of it arrives, the one-way queue is q, and when the
// application offered more than went out (backlog) the pool's send queue
// dropped (noteStageDrop).
type stageDrive struct {
	r   *rateControl
	now time.Time
	rx  float64
	i   int64
}

func newStageDrive(fair bool) *stageDrive {
	r := newRateControl()
	r.fair = fair
	return &stageDrive{r: r, now: time.Unix(1_700_000_000, 0)}
}

// report runs one interval; it returns what was sent (bytes/s).
func (d *stageDrive) report(cpu float64, backlog bool, q time.Duration) float64 {
	d.now = d.now.Add(feedbackEvery)
	d.i++
	allow := d.r.pacingRate(d.now)
	sent := allow
	if cpu > 0 && cpu < sent {
		sent = cpu
	}
	b := sent * feedbackEvery.Seconds()
	d.r.onSent(int(b))
	d.rx += b
	if backlog && (cpu > 0 && cpu < allow) {
		d.r.noteStageDrop()
	}
	d.r.onFeedback(d.now, uint64(d.rx), 0.038+q.Seconds(), 0, d.i, uint32(q/stampTick), true)
	return sent
}

// A CPU-bound sender (its pacer sends far less than it is allowed while its
// send queue drops) leaves startup, and its allowance stays within
// stageHeadroom of what it gets out — not the 2.9-18x of before (W8). With
// the pool rules off (HS2_FAIR_SHARE=0) nothing changes.
func TestRateControlStageLimitedLeavesStartup(t *testing.T) {
	const cpu = 30e6 / 8
	for _, fair := range []bool{true, false} {
		d := newStageDrive(fair)
		exitAt := -1
		worst := 0.0
		for i := 0; i < 100; i++ {
			d.report(cpu, true, 0)
			if exitAt < 0 && !d.r.startup {
				exitAt = i
			}
			if i >= 40 {
				worst = math.Max(worst, d.r.rateSnapshot()/cpu)
			}
		}
		switch {
		case fair && (exitAt < 0 || exitAt > 30):
			t.Fatalf("rules on: left startup at report %d, want within 30", exitAt)
		case fair && worst > 2.25:
			t.Fatalf("rules on: allowance %.1fx what the CPU lets out, want <= 2.25", worst)
		case !fair && exitAt >= 0:
			t.Fatalf("rules off: left startup at report %d; the stage rule must be off", exitAt)
		}
		t.Logf("fair=%v: left startup at report %d, allowance up to %.1fx what got out", fair, exitAt, worst)
	}
}

// After startup the allowance follows the send stage down (another process
// takes the CPU), and regrows fast once the stage limit lifts — 25% per RTT
// while no queue stands, the ordinary 4% probe again once a queue has stood.
func TestRateControlStageCapAndRegrow(t *testing.T) {
	const cpu = 40e6 / 8
	d := newStageDrive(true)
	for i := 0; i < 60; i++ {
		d.report(cpu, true, 0)
	}
	if d.r.startup {
		t.Fatal("still in startup")
	}
	for i := 0; i < 30; i++ {
		d.report(cpu/4, true, 0)
	}
	if got := d.r.rateSnapshot() / (cpu / 4); got > 2.25 {
		t.Fatalf("CPU share fell 4x: allowance %.1fx what got out, want <= 2.25", got)
	}
	// The CPU frees: everything allowed goes out, no queue yet.
	r0 := d.r.rateSnapshot()
	var traj []float64
	for i := 0; i < 15; i++ {
		d.report(0, false, 0)
		traj = append(traj, math.Round(d.r.rateSnapshot()/r0*10)/10)
	}
	t.Logf("allowance after the stage limit lifted, x before, per report: %v", traj)
	if g := d.r.rateSnapshot() / r0; g < 3 {
		t.Fatalf("after the stage limit lifted the allowance grew %.1fx in 15 reports (one base probe among them), want >= 3 (25%%/RTT)", g)
	}
	// A queue stands: back to ordinary probing.
	d.report(0, false, 20*time.Millisecond)
	if d.r.stageRate != 0 {
		t.Fatal("a standing queue must end the fast regrowth")
	}
	for i := 0; i < 3; i++ {
		d.report(0, false, 0)
	}
	c0 := d.r.capEst
	d.report(0, false, 0)
	if g := d.r.capEst / c0; g > probeGrow+1e-9 {
		t.Fatalf("after a queue stood capacity grew %.3fx per report, want <= %.2f", g, probeGrow)
	}
}

// Drops while the carrier uses its allowance (its pacer is the limit — the
// ordinary paced case) or under the pool's policer cap say nothing about the
// send stage: the controller behaves exactly as without them.
func TestRateControlStageIgnoredWhenLimitedOrCapped(t *testing.T) {
	run := func(drops, capped bool, cpu float64) []float64 {
		d := newStageDrive(true)
		d.r.govCapped.Store(capped)
		var rates []float64
		for i := 0; i < 80; i++ {
			q := time.Duration(0)
			if i > 30 {
				q = 15 * time.Millisecond
			}
			d.report(cpu, false, q)
			if drops {
				d.r.noteStageDrop()
			}
			rates = append(rates, d.r.rateSnapshot())
		}
		return rates
	}
	same := func(a, b []float64) bool {
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}
	if !same(run(false, false, 0), run(true, false, 0)) {
		t.Error("drops while the pacer is the limit changed the controller")
	}
	if !same(run(false, true, 30e6/8), run(true, true, 30e6/8)) {
		t.Error("drops under the pool's policer cap changed the controller")
	}
}

// The other sign of a sender short of CPU: no queue drop (the flows inside
// adapted to the pacer's pace) but the writer waiting for pacer room for much
// of each report while the carrier sends well under its allowance. It
// leaves startup the same way, flagged as held back by its send stage.
func TestRateControlStageFromPacerWait(t *testing.T) {
	const cpu = 30e6 / 8
	d := newStageDrive(true)
	exitAt := -1
	for i := 0; i < 60; i++ {
		d.r.noteStageHeld(feedbackEvery / 2) // the writer waited half the report
		d.report(cpu, false, 0)
		if exitAt < 0 && !d.r.startup {
			exitAt = i
		}
	}
	if exitAt < 0 || exitAt > 20 {
		t.Fatalf("left startup at report %d, want within 20", exitAt)
	}
	if !d.r.stageLimited() {
		t.Fatal("not flagged as held back by its send stage")
	}
	if a := d.r.rateSnapshot() / cpu; a > stageHeadroom*1.15 {
		t.Fatalf("allowance %.1fx what got out", a)
	}
	// A wait under stageHeldMin of the report is no sign.
	d2 := newStageDrive(true)
	for i := 0; i < 60; i++ {
		d2.r.noteStageHeld(time.Duration(float64(feedbackEvery) * stageHeldMin * 0.5))
		d2.report(cpu, false, 0)
	}
	if !d2.r.startup || d2.r.stageLimited() {
		t.Fatal("a short wait for pacer room read as a send stage limit")
	}
}

// A path-bound carrier — using its allowance, a queue standing — waits for
// pacer room all the time, and in each base probe it sends under its
// allowance on purpose. That must not read as a send stage limit: no flag,
// and the controller exactly as without the waits (no fast regrowth after
// every probe).
func TestRateControlStageNotInBaseProbe(t *testing.T) {
	run := func(held bool) ([]float64, bool) {
		d := newStageDrive(true)
		var rates []float64
		flagged := false
		for i := 0; i < 400; i++ { // 40 s: ten base probes
			if held {
				d.r.noteStageHeld(feedbackEvery * 9 / 10)
			}
			q := time.Duration(0)
			if i > 20 {
				q = 10 * time.Millisecond
			}
			d.report(0, false, q)
			rates = append(rates, d.r.rateSnapshot())
			flagged = flagged || d.r.stageLimited()
		}
		return rates, flagged
	}
	base, _ := run(false)
	got, flagged := run(true)
	if flagged {
		t.Fatal("a path-bound carrier was flagged as held back by its send stage")
	}
	for i := range base {
		if base[i] != got[i] {
			t.Fatalf("report %d: rate %.0f with the writer waiting, %.0f without", i, got[i], base[i])
		}
	}
}
