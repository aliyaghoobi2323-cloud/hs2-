package engine

import (
	"context"
	"encoding/binary"
	"io"
	"sync"
	"time"
)

// The reverse exit pool makes the kharej side's link count DYNAMIC.
//
// In reverse, only the exit (kharej) dials; the edge (iran) accepts. So the edge
// — which alone sees the users and the throughput — cannot create links itself.
// Instead the edge's autopilot decides how many links the pattern should have
// and sends that number down a pool-control stream (kindPool); the exit runs
// this pool, which keeps exactly that many dial "slots" alive, clamped to its
// own [min,max]. Each slot dials one carrier to the edge, serves its streams,
// and redials if it drops — so the whole pool is self-healing, and growing or
// shrinking is just starting or stopping slots.
//
// Before the edge has spoken (or against an older edge with no pool-control
// channel) the pool holds a safe default so the tunnel still works; the moment a
// target arrives, the edge is in charge.
//
// Wire (kindPool stream, after the one kind byte): a stream of 2-byte
// big-endian desired counts, one every poolCtlInterval and one on every change.
const (
	poolCtlInterval = 3 * time.Second
	poolCtlLen      = 2
)

// dialedLink is what a reverse dial slot holds — a live carrier to the edge.
// It is an interface only so the pool can be exercised with a fake in tests;
// production always uses *tlscarrier.Carrier.
type dialedLink interface{ Close() error }

// exitPool keeps a dynamic set of reverse dial slots.
type exitPool struct {
	dial  func() (dialedLink, error)
	serve func(ctx context.Context, car dialedLink) // smux-server loop for one link
	min   int
	max   int
	log   func(string, ...any)

	mu     sync.Mutex
	slots  []*exitSlot
	seq    int
	want   int // last target requested by the edge (clamped)
	live   int // slots whose link is currently up (for the log)
	parent context.Context
}

type exitSlot struct {
	id     int
	cancel context.CancelFunc
}

// newExitPool builds a pool but starts no slots; the caller sets p.serve and
// then calls setTarget to bring it up (so the serve closure can capture the
// pool without a data race on it).
func newExitPool(ctx context.Context, min, max int, dial func() (dialedLink, error),
	logf func(string, ...any)) *exitPool {
	if min < 1 {
		min = 1
	}
	if max < min {
		max = min
	}
	return &exitPool{dial: dial, min: min, max: max, log: logf, parent: ctx}
}

// setTarget grows or shrinks the pool toward n (clamped to [min,max]). It is
// called from the edge's pool-control messages and at startup.
func (p *exitPool) setTarget(n int) {
	if n < p.min {
		n = p.min
	}
	if n > p.max {
		n = p.max
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if n == p.want && len(p.slots) == n {
		return
	}
	prev := p.want
	p.want = n
	for len(p.slots) < n { // grow
		p.startSlotLocked()
	}
	for len(p.slots) > n { // shrink
		s := p.slots[len(p.slots)-1]
		p.slots = p.slots[:len(p.slots)-1]
		s.cancel() // drops its link and stops redialing
	}
	if n != prev {
		p.log("mtcp: exit pool target %d links (edge asked; was %d)", n, prev)
	}
}

// startSlotLocked launches one dial slot. Caller holds p.mu.
func (p *exitPool) startSlotLocked() {
	ctx, cancel := context.WithCancel(p.parent)
	s := &exitSlot{id: p.seq, cancel: cancel}
	p.seq++
	p.slots = append(p.slots, s)
	go p.runSlot(ctx, s)
}

// runSlot keeps one link dialed until its slot is cancelled.
func (p *exitPool) runSlot(ctx context.Context, s *exitSlot) {
	backoff := 500 * time.Millisecond
	for ctx.Err() == nil {
		car, err := p.dial()
		if err != nil {
			p.log("mtcp: exit slot %d dial to edge failed: %v (retry in %s)", s.id, err, backoff)
			if !sleepCtx(ctx, backoff) {
				return
			}
			if backoff < 8*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = 500 * time.Millisecond
		n := p.incLive(1)
		p.log("mtcp: exit link up to edge (slot %d; now %d)", s.id, n)
		p.serve(ctx, car) // returns when the link dies or ctx ends
		car.Close()
		n = p.incLive(-1)
		if ctx.Err() == nil {
			p.log("mtcp: exit link down (slot %d; now %d); redial", s.id, n)
		}
	}
}

func (p *exitPool) incLive(d int) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.live += d
	return p.live
}

// openPoolCtl runs the EDGE side of pool-control for one link: it opens a
// kindPool stream and sends the autopilot's desired link count periodically and
// whenever it changes, until the link or ctx ends. get returns the current
// target. Against an old exit the stream is refused (its serveStream has no
// kindPool case) and this simply returns — the exit keeps its default count.
func openPoolCtl(ctx context.Context, l Link, get func() int, logf func(string, ...any)) {
	ro, ok := l.(rawStreamOpener)
	if !ok {
		return
	}
	st, err := ro.OpenRawStream()
	if err != nil {
		return
	}
	defer st.Close()
	if _, err := st.Write([]byte{kindPool}); err != nil {
		return
	}
	buf := make([]byte, poolCtlLen)
	last := -1
	t := time.NewTicker(poolCtlInterval)
	defer t.Stop()
	send := func() bool {
		n := get()
		binary.BigEndian.PutUint16(buf, uint16(n))
		st.SetWriteDeadline(time.Now().Add(poolCtlInterval))
		if _, err := st.Write(buf); err != nil {
			return false
		}
		last = n
		return true
	}
	if !send() {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if get() != last { // send on change, plus a periodic refresh below
				if !send() {
					return
				}
				continue
			}
			// periodic keep-alive of the target so a fresh exit slot learns it soon
			if !send() {
				return
			}
		}
	}
}

// servePoolCtl runs the EXIT side: it reads desired counts from the edge and
// applies them to the pool until the stream ends.
func servePoolCtl(ctx context.Context, st io.ReadWriteCloser, pool *exitPool) {
	defer st.Close()
	if pool == nil {
		io.Copy(io.Discard, st) // no pool here (direct exit): just drain
		return
	}
	buf := make([]byte, poolCtlLen)
	for ctx.Err() == nil {
		if _, err := io.ReadFull(st, buf); err != nil {
			return
		}
		pool.setTarget(int(binary.BigEndian.Uint16(buf)))
	}
}
