package engine

import (
	"bytes"
	"testing"
	"time"
)

// mb174: the records rings' big budget — shrink after a low rate, tiers.

func bigRingFill(t *testing.T, r *rescueRing, rate, n int) []byte {
	t.Helper()
	r.on, r.rec = true, true
	r.rate = rate
	r.winT0 = monoNano(time.Now())
	var all []byte
	p := make([]byte, 2000)
	for i := 0; i < n/len(p); i++ {
		for k := range p {
			p[k] = byte(i*13 + k)
		}
		r.add(p)
		all = append(all, p...)
	}
	return all
}

// A burst grows the ring; once the rate wants half or less for rrRateSecs
// the sweeper shrinks it back (newest bytes kept, big returned).
func TestBigRingShrinksAfterLowRate(t *testing.T) {
	big0 := rrStat.big.Load()
	var r rescueRing
	defer r.release()
	all := bigRingFill(t, &r, 300_000, 900_000)
	if len(r.buf) <= rrRingCap || r.big == 0 {
		t.Fatalf("ring did not grow: len %d big %d", len(r.buf), r.big)
	}
	// the conn goes quiet: a trickle of 2 KB/s
	now := r.winT0
	for s := 1; s <= 10; s++ {
		now += int64(time.Second)
		r.winT0, r.winN, r.rate = now-int64(500*time.Millisecond), 1000, 2000
		r.shrinkIdle(now)
	}
	if len(r.buf) != rrRingCap || r.big != 0 || rrStat.big.Load() != big0 {
		t.Fatalf("not shrunk: len %d big %d budget +%d", len(r.buf), r.big, rrStat.big.Load()-big0)
	}
	end := uint64(len(all))
	got, ok := r.get(end-uint64(rrRingCap), end)
	if !ok || !bytes.Equal(got, all[len(all)-rrRingCap:]) {
		t.Fatal("shrunk ring lost its newest bytes")
	}
}

// A steady call keeps its ring (no churn), and a short dip does not shrink it.
func TestBigRingCallKeepsRing(t *testing.T) {
	var r rescueRing
	defer r.release()
	bigRingFill(t, &r, 250_000, 800_000)
	size := len(r.buf)
	now := r.winT0
	for s := 1; s <= 20; s++ {
		now += int64(time.Second)
		rate := 250_000
		if s%7 == 0 {
			rate = 20_000 // a 1-2 s dip (voice activity)
		}
		r.winT0, r.winN, r.rate = now-int64(200*time.Millisecond), rate/5, rate
		r.shrinkIdle(now)
	}
	if len(r.buf) != size {
		t.Fatalf("call ring changed %d -> %d", size, len(r.buf))
	}
}

// Tiers: bursts stop at rrBigBurstPct of the budget; a call still grows.
// Before mb174 the bursts took all of it and the call stayed at 32 KB.
func TestBigRingTiersKeepRoomForCalls(t *testing.T) {
	big0 := rrStat.big.Load()
	setRRBigMax(t, big0+8<<20)
	var bursts []*rescueRing
	defer func() {
		for _, r := range bursts {
			r.release()
		}
	}()
	for i := 0; i < 16; i++ {
		r := &rescueRing{}
		bursts = append(bursts, r)
		bigRingFill(t, r, 400_000, 1_100_000)
	}
	used := rrStat.big.Load() - big0
	if used > 8<<20*rrBigBurstPct/100+int64(rrRingMax) {
		t.Fatalf("bursts took %d KB of the 8 MB budget (tier %d%%)", used>>10, rrBigBurstPct)
	}
	call := &rescueRing{call: true}
	defer call.release()
	bigRingFill(t, call, 250_000, 800_000)
	if len(call.buf) != rrCapFor(250_000) {
		t.Fatalf("call ring %d, want %d (budget used %d KB)", len(call.buf), rrCapFor(250_000), (rrStat.big.Load()-big0)>>10)
	}
	if rrStat.bigCall.Load() < int64(call.big) {
		t.Fatalf("bigCall %d < call's big %d", rrStat.bigCall.Load(), call.big)
	}
}

// Sustained: rrSusWins full windows in a row needing a big ring.
func TestBigRingSustained(t *testing.T) {
	var r rescueRing
	t0 := int64(1) << 40
	r.noteRate(t0, 0)
	for s := 1; s <= rrSusWins; s++ {
		r.noteRate(t0+int64(s)*int64(time.Second), 200_000)
	}
	if r.susN < rrSusWins-1 {
		t.Fatalf("susN %d after %d busy windows", r.susN, rrSusWins)
	}
	r.noteRate(t0+int64(rrSusWins+5)*int64(time.Second), 10) // a pause
	if r.susN != 0 {
		t.Fatalf("susN %d after a pause", r.susN)
	}
}
