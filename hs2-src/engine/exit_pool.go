package engine

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"sync"
	"sync/atomic"
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
	dial func() (dialedLink, error)
	// serve runs the smux-server loop for one link and returns, in operator
	// words, why the link ended (see sessionEndReason).
	serve func(ctx context.Context, car dialedLink) string
	min   int
	max   int
	log   func(string, ...any)

	mu     sync.Mutex
	slots  []*exitSlot
	seq    int
	want   int // last target requested by the edge (clamped)
	live   int // slots whose link is currently up (for the log)
	parent context.Context

	// peerMax is the edge's ceiling as the edge reported it over kindInfo
	// (shared with the serving links; nil when the pool runs without one).
	peerMax *atomic.Int32
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
	switch {
	case prev == 0:
		p.log("mtcp: exit pool starting %d links (the edge sets the count once it is connected)", n)
	case n != prev:
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
		why := p.serve(ctx, car) // returns when the link dies or ctx ends
		car.Close()
		n = p.incLive(-1)
		if ctx.Err() != nil {
			return
		}
		// Above the edge's target, a slot whose link ended retires instead of
		// redialing. Two different events end up here, and the log must not
		// conflate them: the edge closing the link cleanly — normally the
		// autopilot retiring an idle link to shrink the pattern — and a link LOST
		// to a reset, a stalled path or a timeout while the pool happened to be
		// above target (a fault worth seeing). Only the behaviour is the same:
		// either way it is not redialed. (A clean close is reported as exactly
		// that: crypto/tls cannot tell the edge's close_notify from a bare FIN,
		// e.g. a crashed edge process, so the words do not claim more.)
		if p.retireIfOver(s) {
			switch {
			case edgeClosed(why):
				p.log("mtcp: exit slot %d retired — closed by the edge while above its target (pattern shrinking) (now %d)", s.id, n)
			case why == "": // reason unknown: claim neither
				p.log("mtcp: exit slot %d retired — pool above target (now %d)", s.id, n)
			default:
				p.log("mtcp: exit slot %d lost (%s) — not redialed, pool above target (now %d)", s.id, why, n)
			}
			return
		}
		if why == "" {
			why = "reason unknown"
		}
		p.log("mtcp: exit link down (slot %d: %s; now %d); redial", s.id, why, n)
	}
}

// edgeClosed reports whether a link ended by the edge closing it cleanly — the
// first socket error was EOF on read (after its TLS close_notify, or a bare FIN)
// — as opposed to a reset, a stalled path or a keepalive loss.
func edgeClosed(why string) bool { return why == "read: "+reasonPeerClosed }

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
	st := PoolStats{Links: p.live, Target: p.want, Min: p.min, Max: p.max, Phase: "following"}
	if p.peerMax != nil && p.live > 0 { // only while a link is up: never a stale edge value
		st.PeerMax = int(p.peerMax.Load())
	}
	return st
}

// openPoolCtl runs the EDGE side of pool-control for one link: it opens a
// kindPool stream and sends the autopilot's desired serving-link count
// periodically and whenever it changes, until the link or ctx ends. get returns
// the current target. An exit never writes on this stream, so a reader watches
// for the one thing it can say: an exit older than pool control closes the
// stream at once (its serveStream has no kindPool case). If that happens while
// the link is still up, refused is called — the exit keeps its own fixed count,
// so the edge must not close links to shrink it (they would be redialed).
func openPoolCtl(ctx context.Context, l Link, get func() int, logf func(string, ...any), refused func()) {
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
	done := make(chan struct{})
	go func() {
		defer close(done)
		var b [1]byte
		for {
			if _, err := st.Read(b[:]); err != nil {
				// io.EOF is the exit's FIN; a local close or a dying session
				// reads differently, and a link lost at the same moment is
				// told apart by whether it is still up a moment later.
				if errors.Is(err, io.EOF) && ctx.Err() == nil && refused != nil {
					time.Sleep(200 * time.Millisecond)
					if l.Alive() {
						refused()
					}
				}
				return
			}
		}
	}()
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
		case <-done:
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
