package udpcarrier

import (
	"encoding/binary"
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

	in   chan []byte // data shards
	pri  chan []byte // parity shards, drained first so they arrive in time
	pool sync.Pool

	done      chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup

	enqueued uint64
	dropped  uint64
	sent     uint64
	wireSeq  uint32 // pacer goroutine only
	writeErr atomic.Pointer[error]
}

func newPacer(rc *rateControl, write func([]byte) error, queueDepth int) *pacer {
	if queueDepth < 64 {
		queueDepth = 64
	}
	p := &pacer{
		rc:    rc,
		write: write,
		in:    make(chan []byte, queueDepth),
		pri:   make(chan []byte, queueDepth),
		done:  make(chan struct{}),
	}
	p.pool.New = func() any { b := make([]byte, 0, 2048); return &b }
	p.wg.Add(1)
	go p.loop()
	return p
}

// enqueue copies pkt (the encoder reuses its buffer after emit returns) and
// queues it, blocking while the queue is full so backpressure reaches the
// sender. It returns once queued, or when the carrier closes.
func (p *pacer) enqueue(pkt []byte) {
	bp := p.pool.Get().(*[]byte)
	// Reserve a 5-byte datagram header (tag + wire sequence) that the pacer
	// fills at write time; the copy the pacer already makes carries the payload.
	b := append((*bp)[:0], 0, 0, 0, 0, 0)
	b = append(b, pkt...)
	dst := p.in
	if fec.IsParity(pkt) {
		dst = p.pri // parity jumps the queue so it beats the group's ttl
	}
	select {
	case <-p.done:
		*bp = b
		p.pool.Put(bp)
		return
	case dst <- b:
		atomic.AddUint64(&p.enqueued, 1)
	}
}

func (p *pacer) loop() {
	defer p.wg.Done()
	var tokens float64
	last := time.Now()
	for {
		var b []byte
		// Drain parity ahead of data so a group's parity is not stuck behind a
		// backlog of data and miss the decoder's recovery window.
		select {
		case <-p.done:
			return
		case b = <-p.pri:
		default:
			select {
			case <-p.done:
				return
			case b = <-p.pri:
			case b = <-p.in:
			}
		}
		// Fill the reserved header in wire (send) order: tag + 4-byte wire
		// sequence, so the receiver measures loss on the actual wire order.
		b[0] = tagData
		binary.BigEndian.PutUint32(b[1:5], p.wireSeq)
		p.wireSeq++
		now := time.Now()
		rate := p.rc.pacingRate(now)
		tokens += now.Sub(last).Seconds() * rate
		// cap the burst the bucket can accumulate at ~2 datagrams so an idle
		// period cannot release a flood that spikes queueing latency.
		if maxBurst := 2 * float64(len(b)+64); tokens > maxBurst {
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
		if err := p.write(b); err != nil {
			e := err
			p.writeErr.Store(&e)
			p.recycle(b)
			return
		}
		atomic.AddUint64(&p.sent, 1)
		p.recycle(b)
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
