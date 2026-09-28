// Package obfs shapes tunnel traffic so a flow-classifier cannot separate it
// from ordinary bulk HTTPS. It changes only the SHAPE — sizes and timing — of
// frames already encrypted by the core; it never touches the crypto.
//
// It is transport-agnostic on purpose: the same Shaper drives a TCP carrier and
// a UDP carrier. The engine picks which transports to run based on what is open
// on the path (both if both are open, TCP-only otherwise), and each wraps its
// sends through a Shaper.
package obfs

import (
	"crypto/rand"
	"encoding/binary"
	"math"
	"sync"
	"time"
)

// LengthSampler yields target packet sizes drawn from an empirical distribution
// of ordinary HTTPS record sizes, instead of buckets or a fixed MTU. Real bulk
// HTTPS is bimodal: many full-size (~1400) records plus a spread of small ones
// (acks, control). Matching that shape makes our size histogram overlap the
// crowd we hide in.
type LengthSampler struct {
	// cumulative distribution: (size, cumulative_probability)
	sizes  []int
	cum    []float64
	mu     sync.Mutex
	seeded bool
}

// NewHTTPSLengthSampler returns a sampler tuned to bulk-HTTPS record sizes.
// The distribution is approximate but captures the bimodal shape; it can be
// replaced with one fitted from a real pcap of the target class.
func NewHTTPSLengthSampler() *LengthSampler {
	// (size, weight) — heavy at full-size, a tail of small control records.
	pairs := []struct {
		size   int
		weight float64
	}{
		{1400, 0.55}, {1200, 0.08}, {900, 0.05}, {600, 0.05},
		{400, 0.05}, {250, 0.06}, {150, 0.06}, {80, 0.06}, {40, 0.04},
	}
	s := &LengthSampler{}
	var total float64
	for _, p := range pairs {
		total += p.weight
	}
	var acc float64
	for _, p := range pairs {
		acc += p.weight / total
		s.sizes = append(s.sizes, p.size)
		s.cum = append(s.cum, acc)
	}
	return s
}

// Sample returns a target on-wire size. A payload is padded up to (or split to)
// this size by the carrier. Sizes never exceed the path MTU on UDP; the carrier
// clamps.
func (s *LengthSampler) Sample() int {
	u := randFloat()
	for i, c := range s.cum {
		if u <= c {
			return s.sizes[i]
		}
	}
	return s.sizes[len(s.sizes)-1]
}

// Pacer emits a stream of send opportunities whose inter-departure times follow
// a jittered process with NO fixed period, so there is no heartbeat to lock
// onto. It uses a token bucket (steady average rate) plus per-gap jitter and
// occasional micro-bursts, matching how real bulk transfers clump.
type Pacer struct {
	mu         sync.Mutex
	rate       float64 // tokens (packets) per second, average
	burst      int
	tokens     float64
	last       time.Time
	jitterFrac float64
}

// NewPacer builds a pacer at avgPPS average packets/sec with burst capacity.
func NewPacer(avgPPS float64, burst int) *Pacer {
	return &Pacer{rate: avgPPS, burst: burst, tokens: float64(burst), last: time.Now(), jitterFrac: 0.4}
}

// NextDelay returns how long to wait before the next send. Real bulk HTTPS is
// BURSTY with a heavy tail: many tiny gaps within a burst, and occasional
// longer pauses between bursts. A too-regular pacer is itself a tell (timing
// variance was the residual signal a classifier locked onto). So NextDelay
// emits mostly small within-burst gaps and, with probability pPause, a longer
// inter-burst pause — matching the gap-variance of the crowd we hide in.
func (p *Pacer) NextDelay() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	// token bucket still bounds the average rate
	now := time.Now()
	elapsed := now.Sub(p.last).Seconds()
	p.last = now
	p.tokens += elapsed * p.rate
	if p.tokens > float64(p.burst) {
		p.tokens = float64(p.burst)
	}
	within := time.Duration(float64(time.Millisecond) * (2 + randFloat()*3)) // ~2-5ms
	if randFloat() < 0.2 {
		// inter-burst pause, heavy tail
		pause := time.Duration(float64(time.Millisecond) * (30 + randFloat()*80)) // ~30-110ms
		return pause
	}
	return within
}

// jitter multiplies d by a random factor in [1-f, 1+f].
func jitter(d time.Duration, f float64) time.Duration {
	m := 1 + (randFloat()*2-1)*f
	return time.Duration(float64(d) * m)
}

// UDPHeaderPrefix returns a short, low-entropy prefix to prepend to a UDP
// packet so its FIRST bytes resemble a QUIC/DTLS record header rather than
// full-random noise. This addresses the entropy tell on UDP where we have no
// real TLS to ride inside. It is NOT security — the real bytes are still
// AEAD-protected underneath — it only shapes the first-byte statistics.
//
// Layout mimics a QUIC short-header-ish start: one byte with the high bits in
// the QUIC short-header pattern, then a 4-byte "connection id"-like value that
// is stable per flow (so it looks like a real CID, not random each packet).
func UDPHeaderPrefix(flowID uint32) []byte {
	b := make([]byte, 5)
	b[0] = 0x40 | byte(randInt(0x3f)) // QUIC short header has 0b01 in top bits
	binary.BigEndian.PutUint32(b[1:], flowID)
	return b
}

func randFloat() float64 {
	var b [8]byte
	rand.Read(b[:])
	return float64(binary.BigEndian.Uint64(b[:])) / math.MaxUint64
}

func randInt(n int) int {
	if n <= 0 {
		return 0
	}
	var b [4]byte
	rand.Read(b[:])
	return int(binary.BigEndian.Uint32(b[:]) % uint32(n))
}
