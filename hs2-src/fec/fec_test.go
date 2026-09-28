package fec

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"testing"
	"time"
)

// channel runs payloads through encoder -> lossy path -> decoder, with a
// virtual clock advancing gap per packet on the wire, and reports how many
// distinct payloads arrived.
type result struct {
	sent, delivered, dups int
	wire, parity          int
	recovered             uint64
	wireLoss              float64
}

func (r result) residual() float64 { return 1 - float64(r.delivered)/float64(r.sent) }
func (r result) overhead() float64 { return float64(r.parity) / float64(r.sent) }

func runChannel(t testing.TB, cfg Config, loss float64, lm lossModel, n int, gap time.Duration) result {
	enc := NewEncoder(cfg)
	enc.SetLoss(loss)
	dec := NewDecoder(time.Second, enc.Config().MaxPayload+lenPrefix)
	seen := map[uint32]bool{}
	var res result
	var lost int
	now := time.Unix(1000, 0)
	onWire := func(pkt []byte) {
		res.wire++
		if IsParity(pkt) {
			res.parity++
		}
		now = now.Add(gap)
		if lm.drop() {
			lost++
			return
		}
		cp := append([]byte(nil), pkt...)
		if err := dec.Decode(cp, now, func(p []byte) {
			id := binary.BigEndian.Uint32(p)
			if seen[id] {
				res.dups++
			}
			seen[id] = true
		}); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	payload := make([]byte, 1200)
	for i := 0; i < n; i++ {
		binary.BigEndian.PutUint32(payload, uint32(i))
		if err := enc.Encode(payload, now, onWire); err != nil {
			t.Fatal(err)
		}
		enc.Flush(now, onWire)
	}
	now = now.Add(time.Second)
	enc.Flush(now, onWire)
	res.sent = n
	res.delivered = len(seen)
	res.recovered = dec.Stats().Recovered
	res.wireLoss = float64(lost) / float64(res.wire)
	return res
}

func TestNoLossDeliversEverythingOnce(t *testing.T) {
	r := runChannel(t, Config{}, 0.03, newIID(1, 0), 5000, 100*time.Microsecond)
	if r.delivered != r.sent || r.dups != 0 || r.recovered != 0 {
		t.Fatalf("%+v", r)
	}
	// floor: some parity even on a clean path
	if r.overhead() < 0.05 || r.overhead() > 0.2 {
		t.Fatalf("floor overhead %.2f", r.overhead())
	}
}

// TestRecoversAtPathLoss: at the measured base loss (26% iid, 31% iid) with
// parity sized for it, residual loss is near the 1% target.
func TestRecoversAtPathLoss(t *testing.T) {
	for _, p := range []float64{0.26, 0.31} {
		r := runChannel(t, Config{}, p+0.02, newIID(2, p), 40000, 100*time.Microsecond)
		t.Logf("iid %.0f%%: wire loss %.1f%%, residual %.2f%%, overhead %.0f%%, recovered %d",
			p*100, r.wireLoss*100, r.residual()*100, r.overhead()*100, r.recovered)
		if r.residual() > 0.02 {
			t.Fatalf("residual %.2f%% at %.0f%% loss", r.residual()*100, p*100)
		}
		if r.dups != 0 {
			t.Fatalf("%d duplicate deliveries", r.dups)
		}
	}
}

// TestInterleavingBeatsBursts: under bursty loss at the same mean rate, the
// interleaved code (Depth 4) must leave far less residual loss than the same
// code sent group after group (Depth 1).
func TestInterleavingBeatsBursts(t *testing.T) {
	// Gilbert-Elliott: ~22% loss in the good state, 90% in bursts averaging
	// 6 packets, entered often enough for a ~28% mean.
	mk := func() lossModel { return newGE(3, 0.22, 0.9, 0.02, 0.17) }
	flat := runChannel(t, Config{Depth: 1}, 0.30, mk(), 40000, 100*time.Microsecond)
	inter := runChannel(t, Config{Depth: 4}, 0.30, mk(), 40000, 100*time.Microsecond)
	t.Logf("bursty, depth 1: wire loss %.1f%% residual %.2f%%", flat.wireLoss*100, flat.residual()*100)
	t.Logf("bursty, depth 4: wire loss %.1f%% residual %.2f%%", inter.wireLoss*100, inter.residual()*100)
	if inter.residual() >= flat.residual()*0.6 {
		t.Fatalf("interleaving did not help: %.2f%% vs %.2f%%", inter.residual()*100, flat.residual()*100)
	}
}

// TestNetemCorrelatedLoss runs the exact kernel generator for
// `tc netem loss 26% 25%` and reports the loss it really produces.
func TestNetemCorrelatedLoss(t *testing.T) {
	r := runChannel(t, Config{}, 0.28, newNetem(4, 0.26, 0.25), 40000, 100*time.Microsecond)
	t.Logf("netem 26%%/25%%: realised wire loss %.1f%%, residual %.2f%%, overhead %.0f%%",
		r.wireLoss*100, r.residual()*100, r.overhead()*100)
	if r.residual() > 0.02 {
		t.Fatalf("residual %.2f%%", r.residual()*100)
	}
}

// TestFlushBoundsWait: at a trickle (one packet every 50 ms, like a ping) a
// group never fills, so parity must come from the flush timer and a lost
// packet is rebuilt within FlushAfter.
func TestFlushBoundsWait(t *testing.T) {
	enc := NewEncoder(Config{})
	enc.SetLoss(0.3)
	dec := NewDecoder(time.Second, 1500)
	now := time.Unix(0, 0)
	var got []uint32
	var gotAt []time.Time
	drop := true // lose the first data packet
	emit := func(pkt []byte) {
		if !IsParity(pkt) && drop {
			drop = false
			return
		}
		dec.Decode(append([]byte(nil), pkt...), now, func(p []byte) {
			got = append(got, binary.BigEndian.Uint32(p))
			gotAt = append(gotAt, now)
		})
	}
	sentAt := now
	enc.Encode([]byte{0, 0, 0, 7}, now, emit)
	for i := 0; i < 10; i++ {
		now = now.Add(5 * time.Millisecond)
		enc.Flush(now, emit)
	}
	if len(got) != 1 || got[0] != 7 {
		t.Fatalf("lost packet not rebuilt: %v", got)
	}
	if w := gotAt[0].Sub(sentAt); w > enc.Config().Window+5*time.Millisecond {
		t.Fatalf("rebuilt after %v", w)
	}
}

// TestAdapterTracksSteps: loss steps 5% -> 50% -> 5%. The estimate must reach
// most of the step up within two reports, must not flap on single noisy
// reports, and must come down over seconds.
func TestAdapterTracksSteps(t *testing.T) {
	a := NewAdapter(AdapterConfig{})
	now := time.Unix(0, 0)
	rng := rand.New(rand.NewPCG(9, 9))
	noisy := func(p float64) float64 { // one 100 ms report of ~200 packets
		lost := 0
		for i := 0; i < 200; i++ {
			if rng.Float64() < p {
				lost++
			}
		}
		return float64(lost) / 200
	}
	var e float64
	for i := 0; i < 50; i++ { // 5 s at 5%
		now = now.Add(100 * time.Millisecond)
		e = a.Observe(noisy(0.05), now)
	}
	if e < 0.05 || e > 0.14 {
		t.Fatalf("steady 5%%: estimate %.3f", e)
	}
	low := e
	for i := 0; i < 2; i++ {
		now = now.Add(100 * time.Millisecond)
		e = a.Observe(noisy(0.5), now)
	}
	if e < 0.38 {
		t.Fatalf("two reports into 50%% loss the estimate is only %.3f", e)
	}
	for i := 0; i < 48; i++ {
		now = now.Add(100 * time.Millisecond)
		e = a.Observe(noisy(0.5), now)
	}
	high := e
	// Back to 5%: after 0.5 s protection is still high (hold), after 8 s it
	// is back near the low level.
	var after05, after8 float64
	for i := 1; i <= 80; i++ {
		now = now.Add(100 * time.Millisecond)
		e = a.Observe(noisy(0.05), now)
		if i == 5 {
			after05 = e
		}
	}
	after8 = e
	t.Logf("estimate: steady5=%.3f high=%.3f +0.5s=%.3f +8s=%.3f", low, high, after05, after8)
	if after05 < 0.6*high {
		t.Fatalf("estimate dropped too fast: %.3f -> %.3f", high, after05)
	}
	if after8 > low+0.05 {
		t.Fatalf("estimate did not come back down: %.3f", after8)
	}
}

// TestAdaptiveOverheadFollowsLoss: parity per group follows the estimate,
// between the floor and the ceiling.
func TestAdaptiveOverheadFollowsLoss(t *testing.T) {
	enc := NewEncoder(Config{})
	var prev float64
	for _, p := range []float64{0.03, 0.07, 0.15, 0.28, 0.4, 0.55} {
		enc.SetLoss(p)
		r := enc.ParityRatio()
		t.Logf("loss estimate %.0f%% -> parity %.2f per data shard", p*100, r)
		if r < prev {
			t.Fatalf("parity fell as loss rose")
		}
		prev = r
	}
	enc.SetLoss(0.9)
	if r := enc.ParityRatio(); r > enc.Config().CeilRatio {
		t.Fatalf("ceiling exceeded: %.2f", r)
	}
}

func TestMalformedDoesNotPanic(t *testing.T) {
	dec := NewDecoder(time.Second, 1500)
	rng := rand.New(rand.NewPCG(5, 5))
	now := time.Unix(0, 0)
	for i := 0; i < 20000; i++ {
		b := make([]byte, rng.IntN(64))
		for j := range b {
			b[j] = byte(rng.Uint32())
		}
		if len(b) > HeaderLen && rng.IntN(2) == 0 {
			b[5] = byte(rng.IntN(4)) // small k to reach the decode path
			b[6] = byte(rng.IntN(4))
			binary.BigEndian.PutUint16(b[7:], uint16(len(b)-HeaderLen))
		}
		dec.Decode(b, now, func([]byte) {})
	}
	dec.Expire(now.Add(time.Hour))
}

func TestSmallGroupsAndOrder(t *testing.T) {
	// Parity may arrive before data and data out of order.
	enc := NewEncoder(Config{K: 4, Depth: 1})
	enc.SetLoss(0.3)
	var pkts [][]byte
	now := time.Unix(0, 0)
	for i := 0; i < 4; i++ {
		enc.Encode([]byte(fmt.Sprintf("payload-%d", i)), now, func(p []byte) { pkts = append(pkts, append([]byte(nil), p...)) })
	}
	// drop data 1 and 2, reverse the rest
	var keep [][]byte
	for i, p := range pkts {
		if i == 1 || i == 2 {
			continue
		}
		keep = append([][]byte{p}, keep...)
	}
	dec := NewDecoder(time.Second, 1500)
	var got [][]byte
	for _, p := range keep {
		dec.Decode(p, now, func(b []byte) { got = append(got, append([]byte(nil), b...)) })
	}
	want := map[string]bool{"payload-0": true, "payload-1": true, "payload-2": true, "payload-3": true}
	for _, g := range got {
		delete(want, string(g))
	}
	if len(want) != 0 || len(got) != 4 {
		t.Fatalf("missing %v, got %q", want, got)
	}
	if !bytes.Equal(got[len(got)-1][:8], []byte("payload-")) {
		t.Fatal("bad payload")
	}
}

func BenchmarkEncodeFullGroup(b *testing.B) {
	enc := NewEncoder(Config{})
	enc.SetLoss(0.28)
	p := make([]byte, 1300)
	now := time.Now()
	b.SetBytes(int64(len(p)))
	for i := 0; i < b.N; i++ {
		enc.Encode(p, now, func([]byte) {})
	}
}
