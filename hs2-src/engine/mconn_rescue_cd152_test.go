package engine

import (
	"bytes"
	"testing"
	"time"
)

// cd152: a records ring of a conn writing at call rate holds rrRateSecs
// seconds of it (a fixed 32 KB never covered a 2 Mbit/s call's rescue).

func TestCD152RecRingCapScalesWithRate(t *testing.T) {
	if got := rrCapFor(0); got != rrRingCap {
		t.Fatalf("idle cap %d, want %d", got, rrRingCap)
	}
	if got := rrCapFor(1000); got != rrRingCap {
		t.Fatalf("slow conn cap %d, want the floor %d", got, rrRingCap)
	}
	if got, want := rrCapFor(250_000), min(250_000*rrRateSecs, rrRingMax); got != want || got <= rrRingCap {
		t.Fatalf("2 Mbit/s cap %d, want %d (> %d)", got, want, rrRingCap)
	}
	if got := rrCapFor(100 << 20); got != rrRingMax {
		t.Fatalf("fast conn cap %d, want the ceiling %d", got, rrRingMax)
	}
}

func TestCD152RecRingRate(t *testing.T) {
	var r rescueRing
	t0 := int64(1) << 40
	r.noteRate(t0, 0)
	r.noteRate(t0+int64(500*time.Millisecond), 125_000)
	r.noteRate(t0+int64(time.Second), 125_000)
	if r.rate != 125_000 {
		t.Fatalf("rate %d after 125 KB in the first second (the window closes before its own bytes)", r.rate)
	}
	r.noteRate(t0+int64(2*time.Second), 0)
	if r.rate != 125_000 {
		t.Fatalf("rate %d, want 125000", r.rate)
	}
	r.noteRate(t0+int64(5*time.Second), 10)
	if r.rate != 0 {
		t.Fatalf("rate %d after a 3 s pause, want 0", r.rate)
	}
}

// The ring of a 2 Mbit/s conn covers [end-600 KB, end) and returns the right
// bytes; with the old fixed cap it would not.
func TestCD152RecRingHoldsCallSeconds(t *testing.T) {
	oldMax := rrMemMax
	rrMemMax = 64 << 20
	defer func() { rrMemMax = oldMax }()
	r := rescueRing{on: true, rec: true}
	defer r.release()
	r.rate = 250_000
	r.winT0 = monoNano(time.Now()) // the window stays open during the test
	var all []byte
	p := make([]byte, 2000)
	for i := 0; i < 400; i++ { // 800 KB
		for k := range p {
			p[k] = byte(i*7 + k)
		}
		r.add(p)
		all = append(all, p...)
	}
	if !r.on {
		t.Fatal("ring went off")
	}
	end := uint64(len(all))
	got, ok := r.get(end-600_000, end)
	if !ok {
		t.Fatalf("ring (len %d, n %d) does not cover the last 600 KB", len(r.buf), r.n)
	}
	if !bytes.Equal(got, all[len(all)-600_000:]) {
		t.Fatal("ring bytes differ")
	}
	if len(r.buf) > rrCapFor(250_000) {
		t.Fatalf("ring %d over its cap %d", len(r.buf), rrCapFor(250_000))
	}
}

// The growth past rrRingCap comes from rrBigMax: a records budget pinned by
// the crowd's rings (rrMemCap) does not hold a call's ring at 32 KB; release
// gives both budgets back.
func TestCD152RecRingGrowthOwnBudget(t *testing.T) {
	oldMax := rrMemMax
	defer func() { rrMemMax = oldMax }()
	mem0, big0 := rrStat.mem.Load(), rrStat.big.Load()
	rrMemMax = mem0 + int64(rrRingCap) + 1024 // the shared budget: one 32 KB ring left
	r := rescueRing{on: true, rec: true}
	r.rate = 250_000
	r.winT0 = monoNano(time.Now())
	p := make([]byte, 2000)
	for i := 0; i < 400; i++ {
		r.add(p)
	}
	if !r.on || len(r.buf) != rrCapFor(250_000) {
		t.Fatalf("on=%v len=%d, want %d", r.on, len(r.buf), rrCapFor(250_000))
	}
	if got := rrStat.mem.Load() - mem0; got != int64(rrRingCap) {
		t.Fatalf("shared budget took %d, want %d", got, rrRingCap)
	}
	if got := rrStat.big.Load() - big0; got != int64(len(r.buf)-rrRingCap) {
		t.Fatalf("big budget took %d, want %d", got, len(r.buf)-rrRingCap)
	}
	r.release()
	if rrStat.mem.Load() != mem0 || rrStat.big.Load() != big0 {
		t.Fatalf("release: mem %d (want %d) big %d (want %d)", rrStat.mem.Load(), mem0, rrStat.big.Load(), big0)
	}
}

// Growth that does not fit rrBigMax keeps the ring (rrRingCap, still on)
// instead of dropping it.
func TestCD152RecRingGrowthOverBudgetKeepsRing(t *testing.T) {
	setRRBigMax(t, rrStat.big.Load()+1024)
	r := rescueRing{on: true, rec: true}
	defer r.release()
	p := make([]byte, 2000)
	for i := 0; i < 40; i++ { // 80 KB at no known rate: the 32 KB floor
		r.add(p)
	}
	if !r.on || len(r.buf) != rrRingCap {
		t.Fatalf("on=%v len=%d, want a %d ring", r.on, len(r.buf), rrRingCap)
	}
	r.rate = 250_000
	r.winT0 = monoNano(time.Now())
	for i := 0; i < 200; i++ {
		r.add(p)
	}
	if !r.on || len(r.buf) != rrRingCap || r.n != rrRingCap || r.big != 0 {
		t.Fatalf("on=%v len=%d n=%d big=%d: the over-budget growth must keep the %d ring", r.on, len(r.buf), r.n, r.big, rrRingCap)
	}
}
