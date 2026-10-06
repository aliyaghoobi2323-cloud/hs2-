package fec

import (
	"encoding/binary"
	"math"
	"sync"
	"sync/atomic"
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
//
// Encode and Flush hold mu while emit runs, and emit may block (a carrier's
// pacer applies backpressure there). So what other goroutines need while a
// writer waits — the loss estimate the feedback sets, the counters and the
// parity ratio the status reads — is kept outside mu: a report or a status
// sample never waits behind a writer held back by its pacer.
type Encoder struct {
	cfg    Config
	codecs codecs

	mu    sync.Mutex
	lanes []*txGroup // open groups; len is the current depth
	cur   int
	next  uint32
	loss  atomic.Uint64 // float64 bits: the loss estimate parity is sized for
	stats encCounters

	rmu    sync.Mutex     // guards rcache (taken under mu, or alone)
	rcache map[[2]int]int // (k, loss step) -> parity count

	// Buffer reuse. A shard buffer has HeaderLen bytes of headroom in front
	// of the shard content, so the data packet is emitted from the same
	// buffer the group keeps for parity (no second copy). Parity buffers and
	// the shard table are scratch reused by every group.
	bufs   bufFree
	parity [][]byte
	all    [][]byte
	spare  []*txGroup // closed groups, reused with their shard table

	// input rate, for the derived depth
	rateAt time.Time
	rateN  int
	pps    float64
}

// encCounters are EncoderStats, read without mu (see Encoder).
type encCounters struct {
	data, parity, dataBytes, parityBytes, groups atomic.Uint64
}

// EncoderStats counts what the encoder has produced.
type EncoderStats struct {
	Data, Parity uint64 // packets
	DataBytes    uint64 // payload bytes
	ParityBytes  uint64 // parity shard bytes
	Groups       uint64
}

type txGroup struct {
	id   uint32
	born time.Time
	bufs [][]byte // per data shard: header headroom + content [len:2][payload]
	max  int      // longest content
}

// NewEncoder builds an encoder; zero config fields take defaults.
func NewEncoder(cfg Config) *Encoder {
	cfg.fill()
	depth := cfg.Depth
	if depth <= 0 {
		depth = 1
	}
	e := &Encoder{
		cfg:   cfg,
		lanes: make([]*txGroup, depth),
		bufs: bufFree{
			size: HeaderLen + lenPrefix + cfg.MaxPayload,
			max:  cfg.K * cfg.MaxDepth,
		},
	}
	e.SetLoss(DefaultAdapterConfig().Floor)
	return e
}

// Config returns the effective configuration.
func (e *Encoder) Config() Config { return e.cfg }

// SetLoss sets the loss estimate parity is sized for (from an Adapter). It
// never waits for an Encode in progress (see Encoder); a group closing at the
// same moment is sized for one estimate or the other.
func (e *Encoder) SetLoss(p float64) { e.loss.Store(math.Float64bits(p)) }

// Loss returns the loss estimate parity is currently sized for.
func (e *Encoder) Loss() float64 { return math.Float64frombits(e.loss.Load()) }

// Stats returns a snapshot of the counters (each read on its own: a group
// closing meanwhile may show in one and not yet in another).
func (e *Encoder) Stats() EncoderStats {
	return EncoderStats{
		Data: e.stats.data.Load(), Parity: e.stats.parity.Load(),
		DataBytes: e.stats.dataBytes.Load(), ParityBytes: e.stats.parityBytes.Load(),
		Groups: e.stats.groups.Load(),
	}
}

// ParityRatio returns r/k for a full group at the current estimate.
func (e *Encoder) ParityRatio() float64 {
	return float64(e.parityFor(e.cfg.K)) / float64(e.cfg.K)
}

// parityFor is the parity count for a group of k data shards at the current
// estimate.
func (e *Encoder) parityFor(k int) int {
	if k < 1 {
		return 0
	}
	key := [2]int{k, lossStep(e.Loss())}
	e.rmu.Lock()
	defer e.rmu.Unlock()
	if r, ok := e.rcache[key]; ok {
		return r
	}
	if e.rcache == nil {
		e.rcache = make(map[[2]int]int)
	}
	maxR := int(math.Ceil(float64(k) * e.cfg.CeilRatio))
	r := parityForStep(k, key[1], e.cfg.TargetResidual, maxR)
	e.rcache[key] = r
	return r
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
		if n := len(e.spare); n > 0 {
			g = e.spare[n-1]
			e.spare = e.spare[:n-1]
		} else {
			g = &txGroup{}
		}
		g.id, g.born, g.max = e.next, now, 0
		e.next++
		e.lanes[lane] = g
	}
	idx := len(g.bufs)
	pkt := e.bufs.get()[:HeaderLen+lenPrefix+len(payload)]
	putHeader(pkt, header{group: g.id, idx: idx})
	binary.BigEndian.PutUint16(pkt[HeaderLen:], uint16(len(payload)))
	copy(pkt[HeaderLen+lenPrefix:], payload)
	g.bufs = append(g.bufs, pkt)
	if n := len(pkt) - HeaderLen; n > g.max {
		g.max = n
	}
	e.stats.data.Add(1)
	e.stats.dataBytes.Add(uint64(len(payload)))
	emit(pkt)

	if len(g.bufs) >= e.cfg.K {
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
	defer e.release(g)
	k := len(g.bufs)
	r := e.parityFor(k)
	e.stats.groups.Add(1)
	if r == 0 {
		return nil
	}
	enc, err := e.codecs.get(k, r)
	if err != nil {
		return err
	}
	size := g.max
	for len(e.parity) < r {
		e.parity = append(e.parity, make([]byte, e.bufs.size))
	}
	if cap(e.all) < k+r {
		e.all = make([][]byte, k+r)
	}
	all := e.all[:k+r]
	for i, b := range g.bufs {
		// Zero-extend the content to the coded size in place: the buffer
		// has room (size never exceeds lenPrefix+MaxPayload).
		s := b[HeaderLen:]
		n := len(s)
		s = s[:size]
		clear(s[n:])
		all[i] = s
	}
	for j := 0; j < r; j++ {
		all[k+j] = e.parity[j][HeaderLen : HeaderLen+size]
	}
	if err := enc.Encode(all); err != nil {
		return err
	}
	for j := 0; j < r; j++ {
		pkt := e.parity[j][:HeaderLen+size]
		putHeader(pkt, header{group: g.id, idx: k + j, k: k, r: r, size: size})
		e.stats.parity.Add(1)
		e.stats.parityBytes.Add(uint64(size))
		emit(pkt)
	}
	return nil
}

// release returns a closed group's shard buffers to the free list and the
// group itself to the spare list.
func (e *Encoder) release(g *txGroup) {
	for i, b := range g.bufs {
		e.bufs.put(b)
		g.bufs[i] = nil
	}
	g.bufs = g.bufs[:0]
	if len(e.spare) < e.cfg.MaxDepth {
		e.spare = append(e.spare, g)
	}
}
