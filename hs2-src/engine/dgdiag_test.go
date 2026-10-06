package engine

import (
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/udpcarrier"
)

// The status's send stage is the change between two samples, summed over the
// live carriers: the bytes sent over each carrier's span (a rate), the held
// share as the mean per carrier over the time each was sampled, the
// fair-queue wait and the write time per packet and per call; a carrier's
// first sample only sets its base.
func TestSendDiag(t *testing.T) {
	a, b := &dgLink{}, &dgLink{}
	t0 := time.Now()
	st := func(bytes float64, held, write time.Duration, writes, sent uint64) udpcarrier.Stats {
		return udpcarrier.Stats{PacerSentBytes: uint64(bytes), PacerHeld: held, PacerWrite: write, PacerWrites: writes, PacerSent: sent}
	}
	var d sendDiag
	a.fqSojNs.Store(int64(time.Second))
	a.fqPops.Store(1000)
	d.add(a, st(1e9, time.Hour, time.Hour, 1e6, 1e6), t0)
	var ps PoolStats
	d.fill(&ps)
	if ps.SendMbit != 0 || ps.SendHeldPct != 0 || ps.FQWaitMs != 0 || ps.WriteUs != 0 || ps.PerWrite != 0 {
		t.Fatalf("first sample: %+v", ps)
	}

	d = sendDiag{}
	a.fqSojNs.Add(int64(250 * time.Millisecond))
	a.fqPops.Add(50)
	d.add(a, st(1e9+4e6, time.Hour+time.Second, time.Hour+20*time.Millisecond, 1e6+100, 1e6+350), t0.Add(2*time.Second)) // 4 MB in 2 s
	d.add(b, st(5e5, 0, 0, 0, 0), t0.Add(2*time.Second))                                                                 // b's first sample
	ps = PoolStats{}
	d.fill(&ps)
	if ps.SendMbit != 16 || ps.SendHeldPct != 50 || ps.FQWaitMs != 5 || ps.WriteUs != 200 || ps.PerWrite != 3.5 {
		t.Fatalf("second sample: %+v", ps)
	}

	d = sendDiag{} // both sampled over 2 s; only a held its writer, all of it
	d.add(a, st(1e9+4e6, time.Hour+3*time.Second, time.Hour+20*time.Millisecond, 1e6+100, 1e6+350), t0.Add(4*time.Second))
	d.add(b, st(5e5+1e6, 0, 0, 0, 0), t0.Add(4*time.Second)) // b: 1 MB in 2 s
	ps = PoolStats{}
	d.fill(&ps)
	if ps.SendHeldPct != 50 || ps.WriteUs != 0 || ps.FQWaitMs != 0 || ps.SendMbit != 4 {
		t.Fatalf("mean over carriers: %+v", ps)
	}
}
