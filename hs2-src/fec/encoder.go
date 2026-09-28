package fec

import (
	"encoding/binary"
	"math"
	"sync"
	"time"
)

// Config shapes the code. Zero fields take DefaultConfig values.
type Config struct {
	// K is the number of data shards in a full group.
	K int
	// Window is the time a group's packets are spread over. Consecutive
	// payloads are dealt round-robin to as many open groups as it takes for
	// each group to span Window at the current packet rate (the
	// interleaving), and a group still open after Window is closed early.
	// So a burst of loss shorter than Window costs each group only part of
	// its packets, and a lost packet waits at most Window for its parity.
	Window time.Duration
	// Depth, if > 0, fixes the number of interleaved groups instead of
	// deriving it from the packet rate.
	Depth int
	// MaxDepth bounds the derived depth (memory and parity burstiness).
	MaxDepth int
	// MaxPayload is the largest payload one shard carries.
	MaxPayload int
	// TargetResidual is the data loss left after decoding that parity is
	// sized for, under the binomial model (see Residual).
	TargetResidual float64
	// CeilRatio caps parity at CeilRatio·k per group.
	CeilRatio float64
}

// DefaultConfig holds the values chosen by simulation (fec/sweep_test.go)
// and the lab (see BUILD.md).
//
// K=32: the binomial tail makes large groups far cheaper — at 26% loss a 1%
// residual needs r=8 for k=8 (100% overhead), r=12 for k=16 (75%) and ~r=21
// for k=32 (65%). At low rates the Window closes groups before they fill, so
// a large K costs nothing there.
// Window=30ms: 10 ms windows left 5–7% residual under 15 ms bursts, 30–40 ms
// windows 3.5–4.5%, and 80 ms barely more while delaying rebuilt packets up
// to 80 ms — long enough for the inner TCP to treat them as lost. 30 ms keeps
// the extra delay of a rebuilt packet under a quarter of the path RTT.
func DefaultConfig() Config {
	return Config{
		K:              32,
		Window:         30 * time.Millisecond,
		MaxDepth:       32,
		MaxPayload:     1400,
		TargetResidual: 0.01,
		CeilRatio:      1.5,
	}
}

func (c *Config) fill() {
	d := DefaultConfig()
	if c.K == 0 {
		c.K = d.K
	}
	if c.Window == 0 {
		c.Window = d.Window
	}
	if c.MaxDepth == 0 {
		c.MaxDepth = d.MaxDepth
	}
	if c.MaxPayload == 0 {
		c.MaxPayload = d.MaxPayload
	}
	if c.TargetResidual == 0 {
		c.TargetResidual = d.TargetResidual
	}
	if c.CeilRatio == 0 {
		c.CeilRatio = d.CeilRatio
	}
	if c.K > 128 {
		c.K = 128
	}
}

// Encoder produces shard packets. It is safe for concurrent use.
type Encoder struct {
	cfg    Config
	codecs codecs

	mu    sync.Mutex
	lanes []*txGroup // open groups; len is the current depth
	cur   int
	next  uint32
	loss  float64
	buf   []byte // scratch for the packet being emitted
	stats EncoderStats

	// input rate, for the derived depth
	rateAt time.Time
	rateN  int
	pps    float64
}

// EncoderStats counts what the encoder has produced.
type EncoderStats struct {
	Data, Parity uint64 // packets
	DataBytes    uint64 // payload bytes
	ParityBytes  uint64 // parity shard bytes
	Groups       uint64
}

type txGroup struct {
	id     uint32
	born   time.Time
	shards [][]byte // shard content [len:2][payload], unpadded
	max    int      // longest content
}

// NewEncoder builds an encoder; zero config fields take defaults.
func NewEncoder(cfg Config) *Encoder {
	cfg.fill()
	depth := cfg.Depth
	if depth <= 0 {
		depth = 1
	}
	return &Encoder{
		cfg:   cfg,
		lanes: make([]*txGroup, depth),
		buf:   make([]byte, HeaderLen+lenPrefix+cfg.MaxPayload),
		loss:  DefaultAdapterConfig().Floor,
	}
}

// Config returns the effective configuration.
func (e *Encoder) Config() Config { return e.cfg }

// SetLoss sets the loss estimate parity is sized for (from an Adapter).
func (e *Encoder) SetLoss(p float64) {
	e.mu.Lock()
	e.loss = p
	e.mu.Unlock()
}

// Loss returns the loss estimate parity is currently sized for.
func (e *Encoder) Loss() float64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.loss
}

// Stats returns a snapshot of the counters.
func (e *Encoder) Stats() EncoderStats {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.stats
}

// ParityRatio returns r/k for a full group at the current estimate.
func (e *Encoder) ParityRatio() float64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return float64(e.parityLocked(e.cfg.K)) / float64(e.cfg.K)
}

func (e *Encoder) parityLocked(k int) int {
	maxR := int(math.Ceil(float64(k) * e.cfg.CeilRatio))
	return ParityFor(k, e.loss, e.cfg.TargetResidual, maxR)
}

// Encode emits the data packet for payload, then the parity of any group the
// payload completes. emit runs synchronously and must not keep the slice.
func (e *Encoder) Encode(payload []byte, now time.Time, emit func(pkt []byte)) error {
	if len(payload) > e.cfg.MaxPayload {
		return errTooBig
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.trackRate(now, emit)
	lane := e.cur % len(e.lanes)
	e.cur = (lane + 1) % len(e.lanes)
	g := e.lanes[lane]
	if g == nil {
		g = &txGroup{id: e.next, born: now}
		e.next++
		e.lanes[lane] = g
	}
	idx := len(g.shards)
	content := make([]byte, lenPrefix+len(payload))
	binary.BigEndian.PutUint16(content, uint16(len(payload)))
	copy(content[lenPrefix:], payload)
	g.shards = append(g.shards, content)
	if len(content) > g.max {
		g.max = len(content)
	}

	pkt := e.buf[:HeaderLen+len(content)]
	putHeader(pkt, header{group: g.id, idx: idx})
	copy(pkt[HeaderLen:], content)
	e.stats.Data++
	e.stats.DataBytes += uint64(len(payload))
	emit(pkt)

	if len(g.shards) >= e.cfg.K {
		e.lanes[lane] = nil
		return e.closeLocked(g, emit)
	}
	return nil
}

// rateEvery is how often the input rate (and so the depth) is re-derived.
const rateEvery = 50 * time.Millisecond

// trackRate measures the payload rate and resizes the set of open groups so
// that one group spans about Window: depth = rate·Window/K.
func (e *Encoder) trackRate(now time.Time, emit func([]byte)) {
	e.rateN++
	if e.rateAt.IsZero() {
		e.rateAt = now
		return
	}
	el := now.Sub(e.rateAt)
	if el < rateEvery {
		return
	}
	inst := float64(e.rateN) / el.Seconds()
	if e.pps == 0 {
		e.pps = inst
	} else {
		e.pps += 0.3 * (inst - e.pps)
	}
	e.rateAt, e.rateN = now, 0
	if e.cfg.Depth > 0 {
		return
	}
	want := int(e.pps*e.cfg.Window.Seconds()/float64(e.cfg.K) + 0.5)
	if want < 1 {
		want = 1
	}
	if want > e.cfg.MaxDepth {
		want = e.cfg.MaxDepth
	}
	switch {
	case want > len(e.lanes):
		e.lanes = append(e.lanes, make([]*txGroup, want-len(e.lanes))...)
	case want < len(e.lanes):
		for _, g := range e.lanes[want:] {
			if g != nil {
				e.closeLocked(g, emit)
			}
		}
		e.lanes = e.lanes[:want]
	}
}

// Depth returns the number of groups currently open for interleaving.
func (e *Encoder) Depth() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.lanes)
}

// Flush closes every group open for Window or longer and emits its
// parity. Call it from a timer (NextDeadline says when).
func (e *Encoder) Flush(now time.Time, emit func(pkt []byte)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, g := range e.lanes {
		if g != nil && now.Sub(g.born) >= e.cfg.Window {
			e.lanes[i] = nil
			e.closeLocked(g, emit)
		}
	}
}

// NextDeadline returns when the oldest open group must be flushed, or the
// zero time if no group is open.
func (e *Encoder) NextDeadline() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	var d time.Time
	for _, g := range e.lanes {
		if g == nil {
			continue
		}
		if t := g.born.Add(e.cfg.Window); d.IsZero() || t.Before(d) {
			d = t
		}
	}
	return d
}

func (e *Encoder) closeLocked(g *txGroup, emit func(pkt []byte)) error {
	k := len(g.shards)
	r := e.parityLocked(k)
	e.stats.Groups++
	if r == 0 {
		return nil
	}
	enc, err := e.codecs.get(k, r)
	if err != nil {
		return err
	}
	size := g.max
	all := make([][]byte, k+r)
	backing := make([]byte, (k+r)*size)
	for i := range all {
		all[i] = backing[i*size : (i+1)*size]
		if i < k {
			copy(all[i], g.shards[i]) // zero-extended
		}
	}
	if err := enc.Encode(all); err != nil {
		return err
	}
	pkt := make([]byte, HeaderLen+size)
	for j := 0; j < r; j++ {
		putHeader(pkt, header{group: g.id, idx: k + j, k: k, r: r, size: size})
		copy(pkt[HeaderLen:], all[k+j])
		e.stats.Parity++
		e.stats.ParityBytes += uint64(size)
		emit(pkt)
	}
	return nil
}
