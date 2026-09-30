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

// setTarget sets the pool's target to n (clamped to [min,max]). Growing starts
// new dial slots at once. Shrinking only lowers the target: the exit cannot see
// which of its links carry users, so it never closes one itself. The edge, which
// can, closes an idle link, and the slot whose link that was retires instead of
// redialing (see runSlot). A link with users on it is therefore never cut to
// shrink the pool.
func (p *exitPool) setTarget(n int) {
	if n < p.min {
		n = p.min
	}
	if n > p.max {
		n = p.max
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	prev := p.want
	p.want = n
	for len(p.slots) < n { // grow
		p.startSlotLocked()
	}
	if n != prev {
		p.log("mtcp: exit pool target %d links (edge asked; was %d, %d up)", n, prev, p.live)
	}
}

// retireIfOver removes slot s when the pool holds more slots than the target,
// reporting whether it did. Called when a slot's link has ended.
func (p *exitPool) retireIfOver(s *exitSlot) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.slots) <= p.want {
		return false
	}
	for i, x := range p.slots {
		if x == s {
			p.slots = append(p.slots[:i], p.slots[i+1:]...)
			s.cancel()
			return true
		}
	}
	return false
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
		if ctx.Err() != nil {
			return
		}
		// The edge closed an idle link to shrink the pattern (or a link dropped
		// while the pool is above target): retire this slot instead of redialing.
		if p.retireIfOver(s) {
			p.log("mtcp: exit slot %d retired — pattern shrinking (now %d)", s.id, n)
			return
		}
		p.log("mtcp: exit link down (slot %d; now %d); redial", s.id, n)
	}
}

func (p *exitPool) incLive(d int) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.live += d
	return p.live
}

// stats reports the reverse exit pool for the live monitor. The exit follows the
// edge's target, so phase is always "following"; throughput and users are only
// visible on the edge, not here.
func (p *exitPool) stats() PoolStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return PoolStats{Links: p.live, Target: p.want, Min: p.min, Max: p.max, Phase: "following"}
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
	last, lastSent := -1, time.Time{}
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		// Send on every change (within a second) and refresh every
		// poolCtlInterval so a freshly started exit learns the target quickly.
		// Nothing is sent until the edge has a real target (> 0).
		if n := get(); n > 0 && (n != last || time.Since(lastSent) >= poolCtlInterval) {
			binary.BigEndian.PutUint16(buf, uint16(n))
			st.SetWriteDeadline(time.Now().Add(poolCtlInterval))
			if _, err := st.Write(buf); err != nil {
				return
			}
			last, lastSent = n, time.Now()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
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
