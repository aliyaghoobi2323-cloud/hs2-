package udpcarrier

import (
	"encoding/binary"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/fec"
)

// pacer drains shard packets to the socket at the rate the rateControl models.
// The FEC encoder emits packets synchronously while holding its own lock, so
// emit copies the packet into a pooled buffer and hands it to the pacer
// goroutine, which owns every socket write and spaces them with a token bucket
// at the paced rate.
//
// The queue is bounded near the bandwidth-delay product. When it fills, enqueue
// BLOCKS the caller (with a close escape) rather than dropping: a full queue
// means the sender is offering data faster than the path drains, so blocking
// pushes that backpressure up through SendFrame to the TUN read loop — exactly
// what a real NIC's send queue does — instead of silently losing tunnel data.
// The bound keeps the queue, and so the added latency, small.
type pacer struct {
	rc    *rateControl
	write func([]byte) error

	in  chan []byte // data shards
	pri chan []byte // parity shards, drained first so they arrive in time
	// fast: data shards of an interactive flow's packet (SendUrgent), sent
	// after parity and before the data queue, and never held back by the
	// queue's time bound: a ping or a game packet does not wait behind up to
	// pacerQueueTime of a download. The pool marks only flows that had
	// nothing queued, and never one whose earlier packets may still be in
	// the data queue (so a flow's packets keep their order).
	fast chan []byte
	// inQueued / inLeft count the data shards put in `in` and taken out of it
	// (sent, or dropped on close): the pool lets a flow use the fast lane only
	// once inLeft has reached what inQueued was after the flow's last
	// ordinary packet — so its fast packet can never overtake it, however
	// slowly the data queue drains (the carrier's floor rate, a policer cap).
	inQueued atomic.Uint64
	inLeft   atomic.Uint64
	// writeBatch, when the socket can (sendmmsg), sends several datagrams in
	// one syscall: the pacer gathers the ones already waiting that the bucket
	// can pay for (pacerBatch at most).
	writeBatch atomic.Pointer[func([][]byte) error]
	pool       sync.Pool

	done      chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup

	enqueued uint64
	dropped  uint64
	sent     uint64
	wireSeq  uint32 // pacer goroutine only
	writeErr atomic.Pointer[error]
	stamps   *atomic.Bool              // the peer takes stamped datagrams (tagDataTS)
	gov      *atomic.Pointer[Governor] // the pool's shared cap (nil / empty outside a capped pool)

	// Time bound on the data queue: queued counts the bytes waiting (data
	// and parity), and enqueue of DATA waits while it exceeds what the
	// current rate sends in pacerQueueTime. A fixed packet count is a time
	// bound only at one rate: 64 datagrams are 5 ms at 100 Mbit/s but 300 ms
	// at 2 Mbit/s — self-inflicted latency the path never asked for. Parity
	// is never held back (it must beat its group's ttl).
	queued atomic.Int64
	room   chan struct{} // signalled when queued drops
}

// pacerQueueTime / pacerQueueMin bound the send queue in time (see pacer).
const (
	pacerQueueTime = 20 * time.Millisecond
	pacerQueueMin  = 3 * 1500 // at the 0.26 Mbit/s floor still 140 ms; 8 datagrams were 375 ms
)

// pacerBatch is the most datagrams one batched send carries.
const pacerBatch = 16

// pacerQuantum is the pacing burst the bucket may hold: two timer wake-ups'
// worth of the current rate (Go sleeps in ~1 ms steps). At 100 Mbit/s that is
// 25 KB — 2 ms of line rate, far below any buffer that would add latency.
const pacerQuantum = 2 * time.Millisecond

func newPacer(rc *rateControl, write func([]byte) error, queueDepth int, stamps *atomic.Bool) *pacer {
	if queueDepth < 64 {
		queueDepth = 64
	}
	p := &pacer{
		rc:    rc,
		write: write,
		in:    make(chan []byte, queueDepth),
		pri:   make(chan []byte, queueDepth),
		fast:  make(chan []byte, queueDepth),
		done:  make(chan struct{}),
		room:  make(chan struct{}, 1),
	}
	if stamps == nil {
		stamps = new(atomic.Bool)
	}
	p.stamps = stamps
	p.pool.New = func() any { b := make([]byte, 0, 2048); return &b }
	p.wg.Add(1)
	go p.loop()
	return p
}

// enqueue copies pkt (the encoder reuses its buffer after emit returns) and
// queues it, blocking while the queue is full so backpressure reaches the
// sender. It returns once queued, or when the carrier closes.
func (p *pacer) enqueue(pkt []byte) { p.enqueueLane(pkt, false) }

// enqueueLane is enqueue; urgent puts a data shard in the fast lane.
func (p *pacer) enqueueLane(pkt []byte, urgent bool) {
	bp := p.pool.Get().(*[]byte)
	// Reserve a datagram header the pacer fills at write time — tag, wire
	// sequence and (stamped form) send time: 9 bytes, of which the unstamped
	// form uses the last 5. The copy the pacer already makes carries the payload.
	b := append((*bp)[:0], 0, 0, 0, 0, 0, 0, 0, 0, 0)
	b = append(b, pkt...)
	dst := p.in
	switch {
	case fec.IsParity(pkt):
		dst = p.pri // parity jumps the queue so it beats the group's ttl
	case urgent:
		dst = p.fast // an interactive packet: not behind the data queue
	default:
		for p.queued.Load() > p.budget() {
			select {
			case <-p.done:
				*bp = b
				p.pool.Put(bp)
				return
			case <-p.room:
			}
		}
	}
	p.queued.Add(int64(len(b)))
	select {
	case <-p.done:
		*bp = b
		p.pool.Put(bp)
		return
	case dst <- b:
		atomic.AddUint64(&p.enqueued, 1)
		if dst == p.in {
			p.inQueued.Add(1)
		}
	}
}

// budget is the most data bytes the queue may hold before enqueue waits:
// pacerQueueTime at the current rate, never below a few datagrams.
func (p *pacer) budget() int64 {
	b := int64(p.rc.pacingRate(time.Now()) * pacerQueueTime.Seconds())
	if b < pacerQueueMin {
		b = pacerQueueMin
	}
	return b
}

// dequeued releases n bytes of queue budget and wakes a waiting enqueue.
func (p *pacer) dequeued(n int) {
	p.queued.Add(-int64(n))
	select {
	case p.room <- struct{}{}:
	default:
	}
}

func (p *pacer) loop() {
	defer p.wg.Done()
	var tokens float64
	last := time.Now()
	type item struct {
		b      []byte
		fromIn bool
	}
	var carry *item // taken from a lane but not sendable yet: first next time
	var batch []item
	var outs [][]byte
	for {
		var b []byte
		fromIn := false
		if carry != nil {
			b, fromIn, carry = carry.b, carry.fromIn, nil
		} else {
			// Drain parity ahead of data so a group's parity is not stuck
			// behind a backlog of data and miss the decoder's recovery
			// window; then the fast lane (interactive packets), then the
			// data queue.
			select {
			case <-p.done:
				return
			case b = <-p.pri:
			default:
				select {
				case <-p.done:
					return
				case b = <-p.pri:
				case b = <-p.fast:
				default:
					select {
					case <-p.done:
						return
					case b = <-p.pri:
					case b = <-p.fast:
					case b = <-p.in:
						fromIn = true
					}
				}
			}
		}
		now := time.Now()
		rate := p.rc.pacingRate(now)
		tokens += now.Sub(last).Seconds() * rate
		// Cap the burst the bucket can accumulate so an idle period cannot
		// release a flood that spikes queueing latency — but never below what
		// the rate earns in one timer wake-up: Go's timers sleep at least
		// ~1 ms, so a 2-datagram cap would limit ANY carrier to ~2 datagrams
		// per millisecond (~20 Mbit/s) whatever the path could take.
		if maxBurst := math.Max(2*float64(len(b)+64), rate*pacerQuantum.Seconds()); tokens > maxBurst {
			tokens = maxBurst
		}
		last = now
		need := float64(len(b))
		if tokens < need {
			wait := time.Duration((need - tokens) / rate * float64(time.Second))
			// cap a single wait so a bad estimate cannot stall liveness
			if wait > 50*time.Millisecond {
				wait = 50 * time.Millisecond
			}
			if wait > 0 {
				t := time.NewTimer(wait)
				select {
				case <-p.done:
					t.Stop()
					p.dequeued(len(b))
					p.recycle(b)
					return
				case <-t.C:
				}
			}
			now = time.Now()
			tokens += now.Sub(last).Seconds() * rate
			last = now
		}
		tokens -= need
		// Under a policer cap the whole pool shares one budget: every datagram,
		// data or parity, waits for its share of it.
		var g *Governor
		if p.gov != nil {
			g = p.gov.Load()
		}
		if g != nil {
			if d := g.reserve(len(b)); d > 0 {
				t := time.NewTimer(d)
				select {
				case <-p.done:
					t.Stop()
					p.dequeued(len(b))
					p.recycle(b)
					return
				case <-t.C:
				}
				// the bucket above refills while we wait for the pool's
				last = time.Now()
			}
		}
		batch = append(batch[:0], item{b, fromIn})
		// More datagrams already waiting that the bucket can pay for now go
		// in the same syscall (sendmmsg), in the same lane order. Not under a
		// policer cap: each of those waits for the pool's budget alone.
		wb := p.writeBatch.Load()
		if wb != nil && (g == nil || !g.Capped()) {
			for len(batch) < pacerBatch {
				var nb []byte
				nIn := false
				select {
				case nb = <-p.pri:
				default:
					select {
					case nb = <-p.fast:
					default:
						select {
						case nb = <-p.in:
							nIn = true
						default:
						}
					}
				}
				if nb == nil {
					break
				}
				if tokens < float64(len(nb)) {
					carry = &item{nb, nIn}
					break
				}
				tokens -= float64(len(nb))
				batch = append(batch, item{nb, nIn})
			}
		}
		// Fill the reserved header in wire (send) order, so the receiver
		// measures loss on the actual wire order and the stamp is the moment
		// the datagram leaves.
		outs = outs[:0]
		stamp := p.stamps.Load()
		for i := range batch {
			b := batch[i].b
			out := b
			if stamp {
				b[0] = tagDataTS
				binary.BigEndian.PutUint32(b[1:5], p.wireSeq)
				binary.BigEndian.PutUint32(b[5:9], stampOf(time.Now()))
			} else {
				out = b[4:]
				out[0] = tagData
				binary.BigEndian.PutUint32(out[1:5], p.wireSeq)
			}
			p.wireSeq++
			outs = append(outs, out)
		}
		var err error
		if len(outs) == 1 {
			err = p.write(outs[0])
		} else {
			err = (*wb)(outs)
		}
		if err != nil {
			e := err
			p.writeErr.Store(&e)
			for _, it := range batch {
				p.dequeued(len(it.b))
				p.recycle(it.b)
			}
			return
		}
		for i, it := range batch {
			atomic.AddUint64(&p.sent, 1)
			if it.fromIn {
				p.inLeft.Add(1)
			}
			p.rc.onSent(len(outs[i]))
			p.dequeued(len(it.b))
			p.recycle(it.b)
		}
	}
}

func (p *pacer) recycle(b []byte) {
	p.pool.Put(&b)
}

func (p *pacer) err() error {
	if e := p.writeErr.Load(); e != nil {
		return *e
	}
	return nil
}

func (p *pacer) stats() (enqueued, dropped, sent uint64) {
	return atomic.LoadUint64(&p.enqueued), atomic.LoadUint64(&p.dropped), atomic.LoadUint64(&p.sent)
}

func (p *pacer) close() {
	p.closeOnce.Do(func() { close(p.done) })
	p.wg.Wait()
}
