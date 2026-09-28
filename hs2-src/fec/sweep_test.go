package fec

import (
	"encoding/binary"
	"os"
	"sort"
	"testing"
	"time"
)

type timed interface{ set(nowNs int64) }

// runTimed is runChannel with a time-aware loss model, an adaptive loss
// estimate fed from the realised loss every 100 ms (as the carrier's feedback
// does), and a record of how late each rebuilt packet arrived.
func runTimed(cfg Config, lm lossModel, dur time.Duration, pps float64) (res result, lateP50, lateP99 time.Duration) {
	enc := NewEncoder(cfg)
	ac := AdapterConfig{}
	if defaultAdapterOverride != nil {
		ac = *defaultAdapterOverride
	}
	ad := NewAdapter(ac)
	dec := NewDecoder(time.Second, enc.Config().MaxPayload+lenPrefix)
	start := time.Unix(1000, 0)
	now := start
	sentAt := map[uint32]time.Time{}
	seen := map[uint32]bool{}
	var late []time.Duration
	var ivSent, ivLost int
	nextReport := now.Add(100 * time.Millisecond)
	onWire := func(pkt []byte) {
		res.wire++
		if IsParity(pkt) {
			res.parity++
		}
		ivSent++
		if tm, ok := lm.(timed); ok {
			tm.set(now.Sub(start).Nanoseconds())
		}
		if lm.drop() {
			ivLost++
			return
		}
		dec.Decode(append([]byte(nil), pkt...), now, func(p []byte) {
			id := binary.BigEndian.Uint32(p)
			if !seen[id] {
				seen[id] = true
				if d := now.Sub(sentAt[id]); d > 0 {
					late = append(late, d)
				}
			}
		})
	}
	payload := make([]byte, 1200)
	gap := time.Duration(float64(time.Second) / pps)
	n := int(dur.Seconds() * pps)
	var lostTotal int
	for i := 0; i < n; i++ {
		binary.BigEndian.PutUint32(payload, uint32(i))
		sentAt[uint32(i)] = now
		enc.Encode(payload, now, onWire)
		now = now.Add(gap)
		enc.Flush(now, onWire)
		if !now.Before(nextReport) {
			if ivSent > 0 {
				enc.SetLoss(ad.Observe(float64(ivLost)/float64(ivSent), now))
			}
			lostTotal += ivLost
			ivSent, ivLost = 0, 0
			nextReport = nextReport.Add(100 * time.Millisecond)
		}
	}
	now = now.Add(time.Second)
	enc.Flush(now, onWire)
	lostTotal += ivLost
	res.sent = n
	res.delivered = len(seen)
	res.recovered = dec.Stats().Recovered
	res.wireLoss = float64(lostTotal) / float64(res.wire)
	sort.Slice(late, func(i, j int) bool { return late[i] < late[j] })
	if len(late) > 0 {
		lateP50 = late[len(late)/2]
		lateP99 = late[len(late)*99/100]
	}
	return
}

// TestSweep explores the code parameters on the modelled target path. It is
// a tuning tool, not a pass/fail test: HS2_FEC_SWEEP=1 go test -run Sweep -v
func TestSweep(t *testing.T) {
	if os.Getenv("HS2_FEC_SWEEP") == "" {
		t.Skip("set HS2_FEC_SWEEP=1")
	}
	paths := []struct {
		name string
		mk   func() lossModel
	}{
		// ~26% mean: 20% background, 15 ms bursts of 85% every ~165 ms
		{"GE 26% bursts15ms", func() lossModel { return newTimeGE(11, 0.20, 0.85, 150, 15) }},
		// ~30% mean with longer bursts (40 ms)
		{"GE 30% bursts40ms", func() lossModel { return newTimeGE(12, 0.20, 0.85, 250, 40) }},
		{"iid 26%", func() lossModel { return newIID(13, 0.26) }},
	}
	for _, path := range paths {
		for _, pps := range []float64{1000, 8000} {
			for _, k := range []int{8, 16, 32} {
				for _, w := range []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond, 80 * time.Millisecond} {
					for _, tr := range []float64{0.01, 0.02} {
						r, p50, p99 := runTimed(Config{K: k, Window: w, TargetResidual: tr}, path.mk(), 20*time.Second, pps)
						t.Logf("%-18s pps=%5.0f k=%2d win=%3dms tgt=%.0f%% | wire %.1f%% resid %5.2f%% ovh %4.0f%% goodput %.2f | rebuilt late p50 %4.1fms p99 %4.1fms",
							path.name, pps, k, w.Milliseconds(), tr*100, r.wireLoss*100, r.residual()*100,
							r.overhead()*100, float64(r.delivered)/float64(r.wire),
							float64(p50.Microseconds())/1000, float64(p99.Microseconds())/1000)
					}
				}
			}
		}
	}
}

var defaultAdapterOverride *AdapterConfig

func TestSweepAdapter(t *testing.T) {
	if os.Getenv("HS2_FEC_SWEEP") == "" {
		t.Skip("set HS2_FEC_SWEEP=1")
	}
	def := DefaultAdapterConfig()
	for _, path := range []struct {
		name string
		mk   func() lossModel
	}{
		{"GE 26% bursts15ms", func() lossModel { return newTimeGE(11, 0.20, 0.85, 150, 15) }},
		{"GE 30% bursts40ms", func() lossModel { return newTimeGE(12, 0.20, 0.85, 250, 40) }},
		{"iid 26%", func() lossModel { return newIID(13, 0.26) }},
	} {
		for _, hf := range []float64{0.001, 0.5, 0.75} {
			for _, mx := range []float64{0.45, 0.55} {
				saved := defaultAdapterOverride
				defaultAdapterOverride = &AdapterConfig{HoldFrac: hf, Max: mx, RiseAlpha: def.RiseAlpha, FallAlpha: def.FallAlpha, Hold: def.Hold, Floor: def.Floor, Margin: def.Margin}
				r, p50, p99 := runTimed(Config{K: 32, Window: 30 * time.Millisecond}, path.mk(), 30*time.Second, 8000)
				defaultAdapterOverride = saved
				t.Logf("%-18s hold=%.2f max=%.2f | wire %.1f%% resid %5.2f%% ovh %4.0f%% goodput %.2f | late p50 %.1fms p99 %.1fms",
					path.name, hf, mx, r.wireLoss*100, r.residual()*100, r.overhead()*100,
					float64(r.delivered)/float64(r.wire), float64(p50.Microseconds())/1000, float64(p99.Microseconds())/1000)
			}
		}
	}
}
