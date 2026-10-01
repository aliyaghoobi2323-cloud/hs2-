package udpcarrier

import (
	"sync/atomic"
	"testing"
	"time"
)

// The pacer records "demand" (held) whenever a data enqueue had to wait for
// queue room: the sender was offering faster than the rate drains (pacing-
// limited). A sender that offers only now and then (application-limited) never
// fills the queue above budget and so never sets it. tookDemand clears it.
func TestPacerTookDemand(t *testing.T) {
	rc := newRateControl() // default ~1 Mbit/s: a shallow queue budget
	write := func(b []byte) error { return nil }
	p := newPacer(rc, write, 256, new(atomic.Bool))
	defer p.close()

	if p.tookDemand() {
		t.Fatal("demand set before any send")
	}

	// A burst far larger than the shallow queue budget: enqueue must wait for
	// room, so demand is recorded. Draining at ~1 Mbit/s keeps the queue full
	// well past the first overrun.
	pkt := make([]byte, 1200)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 200; i++ {
			p.enqueue(append([]byte(nil), pkt...))
		}
		close(done)
	}()
	// Give the burst time to overrun the budget and block.
	deadline := time.After(3 * time.Second)
	for {
		if p.tookDemand() { // observed demand: put it back is not needed, test done
			break
		}
		select {
		case <-done:
			if !p.tookDemand() {
				t.Fatal("a burst that overran the queue budget did not record demand")
			}
			return
		case <-deadline:
			t.Fatal("burst did not record demand within the deadline")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// The core of the after-idle fix: a carrier whose averaged send rate dips below
// the limited-share (a bursty inner flow that drains the shallow queue between
// wake-ups) must still count as "using its allowance" when the pacer reports
// demand — so the rate can grow — whereas a genuinely application-limited
// carrier (no demand) must not, so an idle tunnel does not inflate its rate.
func TestRateDemandCountsAsLimited(t *testing.T) {
	feed := func(demand bool) (pushing bool, grew bool) {
		rc := newRateControl()
		start := rc.rateSnapshot()
		base := time.Unix(1_700_000_000, 0)
		var rx uint64
		// Prime timing/rx.
		rc.onFeedback(base, rx, 0.05, 0, 0, 0, false, false)
		for i := 1; i <= 40; i++ { // 4 s of reports
			now := base.Add(time.Duration(i) * 100 * time.Millisecond)
			// The path delivers healthily (clean, no loss), but we record NO
			// onSent, so the averaged send rate reads ~0 — below the share. Only
			// `demand` can mark this as a real capacity sample.
			rx += uint64(5e6 / 8 * 0.1) // ~5 Mbit/s delivered in 100 ms
			rc.onFeedback(now, rx, 0.05, 0, 0, 0, false, demand)
		}
		return rc.pushing.Load(), rc.rateSnapshot() > start*1.5
	}

	pushD, grewD := feed(true)
	if !pushD || !grewD {
		t.Fatalf("with demand: pushing=%v grew=%v, want both true (the rate must unfreeze)", pushD, grewD)
	}
	pushN, grewN := feed(false)
	if pushN {
		t.Fatalf("without demand: a zero-send-rate (idle) carrier was marked pushing")
	}
	if grewN {
		t.Fatalf("without demand: an application-limited carrier inflated its rate")
	}
}
