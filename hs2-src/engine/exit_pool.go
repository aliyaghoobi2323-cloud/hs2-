package engine

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
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

// Slot redial pacing. A failed dial backs off with jitter, doubling from
// slotBackoffMin to slotBackoffMax; a link that ended waits a jittered pause
// before its slot redials, at the backoff floor if it had been up for
// slotStableAfter and doubling otherwise (a path that kills links right after
// the handshake must not become a dial loop). Every dial also takes a turn
// from the process's dial gate (dialgate.go).
const (
	slotBackoffMin  = 500 * time.Millisecond
	slotBackoffMax  = 8 * time.Second
	slotStableAfter = 30 * time.Second
	slotFailLogGap  = 30 * time.Second
)

// jitterDur returns a duration in [d/2, d).
func jitterDur(d time.Duration) time.Duration {
	if d <= 1 {
		return d
	}
	return d/2 + time.Duration(rand.Int64N(int64(d/2)))
}

// exitPool keeps a dynamic set of reverse dial slots.
type exitPool struct {
	dial func() (dialedLink, error)
	// serve runs the smux-server loop for one link and returns, in operator
	// words, why the link ended (see sessionEndReason).
	serve func(ctx context.Context, car dialedLink) string
	min   int
	max   int
	log   func(string, ...any)
	gate  *dialGate

	mu      sync.Mutex
	slots   []*exitSlot
	seq     int
	want    int // last target requested by the edge (clamped)
	initial int // the count held until the edge speaks (first setTarget)
	live    int // slots whose link is currently up (for the log)
	parent  context.Context

	// Outage: while no link is up and dials fail, only the scout slot dials
	// (with its backoff); the others wait on upCh until a link is up again,
	// then come back through the gate — not every slot hammering a dead edge.
	outage bool
	scout  *exitSlot
	upCh   chan struct{}

	// failures since the last failure log line, and when that was
	failN   int
	failErr error
	failLog time.Time

	upLog, downLog *burstLog

	// peers is the exit's live links; each carries the ceiling its edge
	// reported over kindInfo (nil when the pool runs without one).
	peers *linkPeers
	// traffic: this exit's own count of user connections and throughput.
	traffic *exitTraffic
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
	if logf == nil {
		logf = func(string, ...any) {}
	}
	p := &exitPool{dial: dial, min: min, max: max, log: logf, parent: ctx, gate: linkGate,
		upCh: make(chan struct{})}
	logp := func(f string, a ...any) { p.log(f, a...) } // p.log may be swapped (tests)
	p.upLog = newBurstLog("mtcp: ", "exit links up", logp)
	p.downLog = newBurstLog("mtcp: ", "exit link ends", logp)
	return p
}

// bounds is the pool's [min,max], lowered to the edge's own ceiling when the
// edge has reported one (kindInfo): an edge never uses more links than its
// max, so dialing past it only makes links it has to hold as spares.
func (p *exitPool) bounds() (lo, hi int) {
	lo, hi = p.min, p.max
	if pm := p.peers.max(); pm > 0 && pm < hi {
		hi = pm
		if lo > hi {
			lo = hi
		}
	}
	return lo, hi
}

// setTarget sets the pool's target to n (clamped to bounds). Growing starts
// new dial slots at once; each dials when the gate gives it a turn. Shrinking
// only lowers the target: the exit cannot see which of its links carry users,
// so it never closes one itself. The edge, which can, closes an idle link, and
// the slot whose link that was retires instead of redialing (see runSlot). A
// link with users on it is therefore never cut to shrink the pool.
func (p *exitPool) setTarget(n int) {
	lo, hi := p.bounds()
	if n < lo {
		n = lo
	}
	if n > hi {
		n = hi
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	prev := p.want
	p.want = n
	if p.initial == 0 {
		p.initial = n
	}
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
// reporting whether it did. Called when a slot's link has ended, and before a
// slot without a link dials.
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
			if p.scout == s { // let a waiting slot take over as scout
				p.scout = nil
				p.wakeLocked()
			}
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

// wakeLocked releases every slot waiting out an outage. Caller holds p.mu.
func (p *exitPool) wakeLocked() {
	close(p.upCh)
	p.upCh = make(chan struct{})
}

// waitTurn holds slot s while another slot scouts a dead edge; false if ctx
// ended.
func (p *exitPool) waitTurn(ctx context.Context, s *exitSlot) bool {
	for {
		p.mu.Lock()
		if !p.outage || p.scout == nil || p.scout == s {
			p.mu.Unlock()
			return true
		}
		ch := p.upCh
		p.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return false
		}
	}
}

// dialFailed records a failed dial: with no link up the pool is in an outage
// and s becomes its scout if there is none. Failure lines are folded into one
// every slotFailLogGap.
func (p *exitPool) dialFailed(s *exitSlot, err error, next time.Duration) {
	p.mu.Lock()
	if p.live == 0 {
		p.outage = true
		if p.scout == nil {
			p.scout = s
		}
	}
	p.failN++
	p.failErr = err
	now := time.Now()
	var line string
	if now.Sub(p.failLog) >= slotFailLogGap {
		if p.failN == 1 {
			line = fmt.Sprintf("mtcp: exit slot %d dial to edge failed: %v (retry in %s)", s.id, err, fmtDur(next))
		} else {
			line = fmt.Sprintf("mtcp: %d dials to the edge failed in the last %s, latest: %v", p.failN, fmtDur(now.Sub(p.failLog)), err)
		}
		if p.outage {
			line += fmt.Sprintf(" — no link up: one slot keeps trying (every ≤%s), the other %d wait for it", fmtDur(slotBackoffMax), len(p.slots)-1)
		}
		p.failN, p.failLog = 0, now
	}
	p.mu.Unlock()
	if line != "" {
		p.log("%s", line)
	}
}

// dialed records a successful dial: an outage is over, and every waiting slot
// comes back (through the gate).
func (p *exitPool) dialed() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.outage {
		p.outage, p.scout = false, nil
		p.wakeLocked()
	}
	p.live++
	return p.live
}

// runSlot keeps one link dialed until its slot is cancelled.
func (p *exitPool) runSlot(ctx context.Context, s *exitSlot) {
	backoff := slotBackoffMin
	for ctx.Err() == nil {
		// A slot without a link above the target retires instead of dialing
		// (the target fell while it waited for its turn or backed off).
		if p.retireIfOver(s) {
			return
		}
		if !p.waitTurn(ctx, s) {
			return
		}
		// A slot that is over the target retires instead of taking a turn:
		// checked when a place is free and before the start spacing is
		// reserved, so after an outage the slots that must retire never hold
		// up the ones that must dial.
		retired := false
		release, ok := p.gate.acquireIf(ctx, func() bool {
			retired = p.retireIfOver(s)
			return !retired
		})
		if retired || !ok {
			return
		}
		if p.retireIfOver(s) { // the target fell during the spacing wait
			release()
			return
		}
		car, err := p.dial()
		release()
		if err != nil {
			wait := jitterDur(backoff)
			p.dialFailed(s, err, wait)
			if !sleepCtx(ctx, wait) {
				return
			}
			if backoff < slotBackoffMax {
				backoff *= 2
			}
			continue
		}
		n := p.dialed()
		up := time.Now()
		p.upLog.log("mtcp: exit link up to edge (slot %d; now %d)", s.id, n)
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
				p.downLog.log("mtcp: exit slot %d retired — closed by the edge while above its target (pattern shrinking) (now %d)", s.id, n)
			case why == "": // reason unknown: claim neither
				p.downLog.log("mtcp: exit slot %d retired — pool above target (now %d)", s.id, n)
			default:
				p.downLog.log("mtcp: exit slot %d lost (%s) — not redialed, pool above target (now %d)", s.id, why, n)
			}
			return
		}
		if why == "" {
			why = "reason unknown"
		}
		p.downLog.log("mtcp: exit link down (slot %d: %s; now %d); redial", s.id, why, n)
		// A link that lived is redialed after a short jittered pause; one that
		// died right after coming up backs off like a failed dial.
		if time.Since(up) >= slotStableAfter {
			backoff = slotBackoffMin
		} else if backoff < slotBackoffMax {
			backoff *= 2
		}
		if !sleepCtx(ctx, jitterDur(backoff)) {
			return
		}
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
	if p.live == 0 && d < 0 && p.initial > 0 && p.want > p.initial {
		// Every link is gone: the edge restarted, or the path dropped. Hold the
		// initial count until the edge speaks again (its target arrives on the
		// first link back): a restarted edge that wants 8 must not be handed
		// hundreds of links it can only hold as spares and close one by one.
		p.want = p.initial
	}
	return p.live
}

// stats reports the reverse exit pool for the live monitor. The exit follows the
// edge's target, so phase is always "following" (the edge decides the size);
// the user connections and throughput are this exit's own count of what it
// relays (exitstats.go).
func (p *exitPool) stats() PoolStats {
	p.mu.Lock()
	st := PoolStats{Links: p.live, Target: p.want, Min: p.min, Max: p.max, Phase: "following"}
	p.mu.Unlock()
	st.PeerMax = p.peers.max() // reported by the links up NOW (never a gone link's value)
	st.Routes = p.peers.edgeRoutes()
	p.traffic.fill(&st)
	return st
}

// poolCtlSlow: how often a link that is not one of the two refreshing links
// re-sends the target (every link sends at once on a change).
const poolCtlSlow = 30 * time.Second

// poolCtlSource is what openPoolCtl needs from the edge's pool.
type poolCtlSource interface {
	Target() int
	targetChanged() <-chan struct{}
	poolCtlFast(l Link) bool
	// poolCtlLive records whether l's pool-control loop runs and its last
	// write went out, so the fast refreshers are links that can deliver.
	poolCtlLive(l Link, ok bool)
}

// poolCtlSpread: a changed target goes out on the two fast links at once and
// on every other link after a random delay up to this, so a change is not a
// burst of one small TLS record on every one of up to 300 links in the same
// instant (the periodic refresh used to spread it over ~1 s).
const poolCtlSpread = 1500 * time.Millisecond

// openPoolCtl runs the EDGE side of pool-control for one link: it opens a
// kindPool stream and sends the autopilot's desired serving-link count at
// once, on every change, and as a periodic refresh — every poolCtlInterval on
// the pool's two oldest links (a freshly started exit, or one that applied a
// late value from a congested link, is corrected within seconds) and every
// poolCtlSlow on the others, so at hundreds of links the refresh is a few
// messages a second, not one per link per interval. An exit never writes on
// this stream, so a reader watches for the one thing it can say: an exit older
// than pool control closes the stream at once (its serveStream has no kindPool
// case). If that happens while the link is still up, refused is called — the
// exit keeps its own fixed count, so the edge must not close links to shrink
// it (they would be redialed).
func openPoolCtl(ctx context.Context, l Link, src poolCtlSource, logf func(string, ...any), refused func()) {
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
	defer src.poolCtlLive(l, false)
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
	last, force := -1, true
	for {
		// Take the change channel before reading the target, so a change
		// made right after the read still wakes this loop.
		changed := src.targetChanged()
		// Nothing is sent until the edge has a real target (> 0).
		if n := src.Target(); n > 0 && (force || n != last) {
			binary.BigEndian.PutUint16(buf, uint16(min(n, 0xffff)))
			st.SetWriteDeadline(time.Now().Add(poolCtlInterval))
			if _, err := st.Write(buf); err != nil {
				return
			}
			last = n
		}
		src.poolCtlLive(l, true)
		force = false
		fast := src.poolCtlFast(l)
		every := poolCtlSlow
		if fast {
			every = poolCtlInterval
		}
		t := time.NewTimer(jitterAround(every))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-done:
			t.Stop()
			return
		case <-changed:
			if !fast { // the fast links carry it at once; the rest follow spread out
				t.Reset(time.Duration(rand.Int64N(int64(poolCtlSpread))))
				select {
				case <-ctx.Done():
					t.Stop()
					return
				case <-done:
					t.Stop()
					return
				case <-t.C:
				}
			}
		case <-t.C:
			force = true
		}
		t.Stop()
	}
}

// jitterAround returns d ±20%, so periodic per-link messages do not beat in
// step across the pool.
func jitterAround(d time.Duration) time.Duration {
	return d*4/5 + time.Duration(rand.Int64N(int64(d*2/5)+1))
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
