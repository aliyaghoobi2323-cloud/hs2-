package engine

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/core"
	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
	"github.com/xtaci/smux"
)

// tunWriter is the TUN device as the L3 path uses it.
type tunWriter interface {
	Write([]byte) (int, error)
	Read([]byte) (int, error)
	MTU() int
}

// pktConn is what an L3 link sends framed packets over: a whole TLS carrier,
// or (in stream mode) one smux stream of a shared link. Frames are TUN frames
// (tlscarrier.AppendTunFrame): each carries its packet's user tag and flow.
type pktConn interface {
	WriteRaw([]byte) error
	ReadTunFrame() (ftype byte, payload []byte, userTag, flow uint32, err error)
	SetReadDeadline(time.Time)
	Close() error
}

// Shared L3 link machinery for both the Iran (dialing) and kharej (accepting)
// pools.
//
// The data path is built so that one slow link can never stall the tunnel:
//
//   - The TUN reader never writes to a socket. It picks a link and hands the
//     packet to that link's bounded queue; if the queue is full the packet is
//     dropped (the inner TCP treats it as ordinary loss and backs off) instead
//     of blocking every other flow behind it.
//   - Each link has its own writer goroutine that drains its queue and
//     coalesces whatever is already waiting into a single TLS write, so a burst
//     costs one record/syscall instead of one per packet.
//   - The writer sends a keepalive when the link has been idle, so a link that
//     happens to carry no flows is not killed by the peer's read deadline.
//   - Links are chosen by rendezvous hashing, so when a link dies only the
//     flows that were on it move; every other flow stays on its link and in
//     order.
//
// With C2 (R1, angle A; HS2_TUNE_SCHED=0 keeps the above) a flow's first
// packet picks its link by that hash and the flow is pinned there (linkFor):
// links that come later take none of it. On a stream-mode link the FIFO and
// writeLoop give way to per-flow queues fed to the link's smux scheduler
// (l3sched.go).

const (
	// l3QueueLen bounds the packets waiting for one link. Kept short on
	// purpose: a long queue here is exactly the bufferbloat that shows up as
	// seconds of delay under load.
	l3QueueLen = 256
	// l3BatchBytes is the most payload one coalesced write carries (about one
	// full TLS record).
	l3BatchBytes = 16 << 10
	// l3MaxSojourn drops a packet that has waited this long in a link's queue.
	// A queue bounded only in packets is bounded in time only on a fast link:
	// on a slow or throttled one 256 packets are seconds of delay (the
	// multi-second pings under load). Bounding the wait instead keeps latency
	// low at any rate, and the inner TCP reads the drops as congestion.
	l3MaxSojourn = 60 * time.Millisecond
	// l3KeepaliveEvery / l3DeadAfter: an idle link sends a keepalive this
	// often, and a link that delivers no frame at all for l3DeadAfter is
	// dropped. Shorter than the single-carrier values because the pool has
	// spare links: a black-holed link should hand its flows over in seconds.
	l3KeepaliveEvery = 2 * time.Second
	l3DeadAfter      = 8 * time.Second

	// In the stream modes (l3mtcp) the TUN side channel rides a smux stream on
	// a link that already has its own liveness (smux keepalive, the edge's
	// suspect check, the wedge guard), so its reader allows l3StreamDeadAfter
	// and — once the other server says over kindInfo that it does the same
	// (capL3Quiet) — an idle stream sends a keepalive only every
	// l3QuietKeepalive (±20%) instead of every 2 s: at hundreds of mostly idle
	// links that is most of the idle traffic. An older peer keeps its 8 s
	// deadline, so towards it the keepalive stays at 2 s.
	l3StreamDeadAfter = 30 * time.Second
	l3QuietKeepalive  = 10 * time.Second
)

// l3SessionSilent: the stream side channel's 30 s grace is for a link whose
// session still talks (a wedge, a long idle); a session that has received
// nothing at all for this long — the other server's smux keepalive arrives
// every 4–8 s on a live link — is black-holed, and its TUN flows move to
// another link now instead of after 24–30 s. A variable for tests.
var l3SessionSilent = 12 * time.Second

// l3Link is one link in L3 mode: a TLS carrier used as a framed packet pipe.
type l3Link struct {
	car pktConn
	id  uint32        // rendezvous hash seed
	q   chan l3pkt    // packets waiting for the writer
	ka  time.Duration // idle time before a keepalive is sent
	// kaFn, when set, decides ka per keepalive (stream mode: by what the peer
	// supports); deadAfter, when set, replaces l3DeadAfter for the reader.
	kaFn      func() time.Duration
	deadAfter time.Duration
	dead      atomic.Bool
	once      sync.Once
	done      chan struct{}
	// sq: the per-flow queues that feed the link's smux scheduler (C2,
	// l3sched.go) in place of q and writeLoop (nil: those)
	sq *tunSched
}

// qpkt is a queued packet and when it was queued.
type qpkt struct {
	b *[]byte
	t time.Time
}

// l3pkt is a packet queued on an L3 link with what its TUN frame carries:
// its user's tag and its flow (flowKey).
type l3pkt struct {
	qpkt
	tag, flow uint32
}

func newL3Link(car pktConn) *l3Link {
	return &l3Link{
		car:  car,
		id:   rand.Uint32(),
		q:    make(chan l3pkt, l3QueueLen),
		ka:   l3KeepaliveEvery,
		done: make(chan struct{}),
	}
}

// newStreamL3Link is the TUN side channel of a stream-mode link (one smux
// stream); quiet reports whether the other server allows quiet keepalives.
// sessReads (nil: unknown) counts the session's completed socket reads: while
// it does not move for l3SessionSilent the whole link is silent and the side
// channel is dropped (watchSession).
func newStreamL3Link(st *smux.Stream, quiet func() bool, sessReads func() uint64) *l3Link {
	l := newL3Link(newStreamPkt(st))
	if st != nil && st.Session().HasSched() {
		sess := st.Session()
		l.sq = newTunSched(st)
		if cl := coreLinks.of(sess); cl != nil {
			cl.tun.Store(l)
		}
	}
	if sessReads != nil {
		go l.watchSession(sessReads)
	}
	l.deadAfter = l3StreamDeadAfter
	l.kaFn = func() time.Duration {
		if quiet() {
			return jitterAround(l3QuietKeepalive)
		}
		return l3KeepaliveEvery
	}
	return l
}

// watchSession drops the link once its session has received nothing for
// l3SessionSilent (it runs beside the reader, so a frame is never cut by a
// read deadline).
func (l *l3Link) watchSession(reads func() uint64) {
	every := min(2*time.Second, l3SessionSilent/4)
	t := time.NewTicker(every)
	defer t.Stop()
	last, since := reads(), time.Now()
	for {
		select {
		case <-l.done:
			return
		case now := <-t.C:
			if n := reads(); n != last {
				last, since = n, now
			} else if now.Sub(since) >= l3SessionSilent {
				l.markDead()
				return
			}
		}
	}
}

// sessReadsOf is the session read counter of a metered link (nil: unknown).
func sessReadsOf(m *linkMeter) func() uint64 {
	if m == nil || m.guard == nil || m.guard.w == nil {
		return nil
	}
	return m.guard.w.rdCalls.Load // moves with every read the session completes
}

// keepalive is the idle time before the next keepalive.
func (l *l3Link) keepalive() time.Duration {
	if l.kaFn != nil {
		return l.kaFn()
	}
	return l.ka
}

func (l *l3Link) Alive() bool { return !l.dead.Load() }

// markDead retires the link and closes its carrier right away, so the peer's
// reader fails at once instead of waiting out its read deadline.
func (l *l3Link) markDead() {
	l.once.Do(func() {
		l.dead.Store(true)
		close(l.done)
		l.car.Close()
	})
}

// enqueue hands a packet to the link without ever blocking (echo: an ICMP
// echo). With the per-flow queues (sq) it is always taken: what they drop
// they count.
func (l *l3Link) enqueue(b *[]byte, tag, flow uint32, echo bool) bool {
	if l.sq != nil {
		l.sq.push(b, tag, flow, echo)
		return true
	}
	select {
	case l.q <- l3pkt{qpkt{b, time.Now()}, tag, flow}:
		return true
	default:
		return false
	}
}

// writeLoop is the only goroutine that writes to the link's carrier.
func (l *l3Link) writeLoop(pool *sync.Pool, drops *atomic.Uint64) {
	batch := make([]byte, 0, l3BatchBytes+4096)
	idle := time.NewTimer(l.keepalive())
	defer idle.Stop()
	add := func(p l3pkt, now time.Time) {
		if now.Sub(p.t) > l3MaxSojourn {
			drops.Add(1)
		} else {
			batch = tlscarrier.AppendTunFrame(batch, core.TypeData, p.tag, p.flow, *p.b)
		}
		pool.Put(p.b)
	}
	for {
		batch = batch[:0]
		select {
		case <-l.done:
			return
		case p := <-l.q:
			now := time.Now()
			add(p, now)
		drain:
			for len(batch) < l3BatchBytes {
				select {
				case p := <-l.q:
					add(p, now)
				default:
					break drain
				}
			}
			if len(batch) == 0 {
				continue // everything was stale
			}
		case <-idle.C:
			batch = tlscarrier.AppendTunFrame(batch, core.TypePing, 0, 0, make([]byte, core.KeepalivePad()))
		}
		if err := l.car.WriteRaw(batch); err != nil {
			l.markDead()
			return
		}
		idle.Reset(l.keepalive())
	}
}

// l3Set is a set of live links plus the pumps between them and the TUN. The
// zero value is ready to use.
type l3Set struct {
	mu    sync.RWMutex
	links []*l3Link
	pool  sync.Pool     // *[]byte packet buffers
	drops atomic.Uint64 // no link, queue full, or waited too long
	// toUser: the TUN's packets go to the users (the exit's side); their
	// user tag is that of their destination, else of their source (§3.1).
	toUser bool
	// pins: each flow's link, pinned by its first packet (C2; pumpTun's
	// alone), and when the idle ones were last swept
	pins    map[uint32]*l3Pin
	pinSwAt int64
	// pinMode: 1 pinned, 2 by the hash per packet (HS2_TUNE_SCHED=0), as
	// the first packet found the switch (0: none yet)
	pinMode int8
}

// l3Pin is a flow's link and when the flow last had a packet.
type l3Pin struct {
	l  *l3Link
	at int64
}

const (
	// l3PinIdle: a flow with no packet this long loses its pin (its next
	// one is pinned anew); l3PinMax: the most flows pinned (past it, a new
	// flow goes by the hash alone); l3PinSweep: the idle pins are dropped
	// this often.
	l3PinIdle  = 2 * time.Minute
	l3PinMax   = 1 << 16
	l3PinSweep = 10 * time.Second
)

// linkFor is the link of a packet of flow h: with C2 the flow's pinned link
// (§5.1: its first packet picks one by the rendezvous hash, and it stays
// there while that link lives, whatever links come and go), else the hash's
// pick per packet as before.
func (s *l3Set) linkFor(h uint32) *l3Link {
	if s.pinMode == 0 {
		s.pinMode = 1
		if SchedOff() {
			s.pinMode = 2
		}
	}
	if s.pinMode == 2 {
		return s.pickFlow(h)
	}
	now := ctrlNow()
	if now-s.pinSwAt > int64(l3PinSweep) {
		s.sweepPins(now, l3PinIdle)
	}
	p := s.pins[h]
	if p != nil && p.l.Alive() {
		p.at = now
		return p.l
	}
	l := s.pickFlow(h)
	if l == nil {
		return nil
	}
	switch {
	case p != nil:
		p.l, p.at = l, now
	case len(s.pins) < l3PinMax:
		if s.pins == nil {
			s.pins = map[uint32]*l3Pin{}
		}
		s.pins[h] = &l3Pin{l, now}
	}
	return l
}

// sweepPins drops the pins idle for idle, and those of dead links.
func (s *l3Set) sweepPins(now int64, idle time.Duration) {
	s.pinSwAt = now
	for h, p := range s.pins {
		if now-p.at > int64(idle) || !p.l.Alive() {
			delete(s.pins, h)
		}
	}
}

func (s *l3Set) add(l *l3Link) {
	if l.sq != nil {
		l.sq.attach(&s.pool, &s.drops)
	}
	s.mu.Lock()
	s.links = append(s.links, l)
	s.mu.Unlock()
}

func (s *l3Set) remove(dead *l3Link) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.links[:0]
	for _, l := range s.links {
		if l != dead {
			out = append(out, l)
		}
	}
	s.links = out
}

// removeDead drops every dead link and reports how many were removed.
func (s *l3Set) removeDead() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.links[:0]
	for _, l := range s.links {
		if l.Alive() {
			out = append(out, l)
		}
	}
	n := len(s.links) - len(out)
	for i := len(out); i < len(s.links); i++ {
		s.links[i] = nil
	}
	s.links = out
	return n
}

func (s *l3Set) count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.links)
}

func (s *l3Set) closeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range s.links {
		l.markDead()
	}
	s.links = nil
}

// pick maps a packet to a live link by rendezvous (highest-random-weight)
// hashing on its flow: a flow keeps its link for as long as that link lives,
// and a dying link only moves its own flows.
func (s *l3Set) pick(pkt []byte) *l3Link { return s.pickFlow(flowHash(pkt)) }

// pickFlow is pick for a packet whose flow hash is h.
func (s *l3Set) pickFlow(h uint32) *l3Link {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var best *l3Link
	var bestW uint32
	for _, l := range s.links {
		if !l.Alive() {
			continue
		}
		if w := mix32(h ^ l.id); best == nil || w > bestW {
			best, bestW = l, w
		}
	}
	return best
}

// serveLink runs a link's writer and reader. The reader runs on the calling
// goroutine and returns when the link dies.
func (s *l3Set) serveLink(ctx context.Context, l *l3Link, dev tunWriter) {
	if l.sq != nil {
		go l.sq.run(l)
	} else {
		go l.writeLoop(&s.pool, &s.drops)
	}
	s.linkToTun(ctx, l, dev)
}

// pumpTun reads IP packets from the TUN and queues each on its flow's link,
// with its user's tag and its flow for its frame (flowKey is parsed once). It
// never blocks on a link: no link or a full queue means drop.
func (s *l3Set) pumpTun(ctx context.Context, dev tunWriter) {
	size := dev.MTU() + 128
	for ctx.Err() == nil {
		bp, _ := s.pool.Get().(*[]byte)
		if bp == nil || cap(*bp) < size {
			b := make([]byte, size)
			bp = &b
		}
		b := (*bp)[:size]
		n, err := dev.Read(b)
		if err != nil {
			s.pool.Put(bp)
			if ctx.Err() != nil {
				return
			}
			continue
		}
		*bp = b[:n]
		flow, user, ok := flowKey(*bp, s.toUser)
		var tag uint32
		if ok {
			tag = UserTag(user, classV6Prefix)
		}
		if l := s.linkFor(flow); l == nil || !l.enqueue(bp, tag, flow, ok && isEcho(*bp)) {
			s.pool.Put(bp)
			s.drops.Add(1)
		}
	}
}

// linkToTun pumps received IP packets from one link into the TUN. Any frame,
// keepalives included, proves the link is alive and extends the deadline. A
// frame without the v3 descriptor ends the link's side channel: the stream is
// out of step (counted, logged by logWire).
func (s *l3Set) linkToTun(ctx context.Context, l *l3Link, dev tunWriter) {
	defer l.markDead()
	for ctx.Err() == nil {
		dead := l3DeadAfter
		if l.deadAfter > 0 {
			dead = l.deadAfter
		}
		l.car.SetReadDeadline(time.Now().Add(dead))
		ft, payload, _, _, err := l.car.ReadTunFrame()
		if err != nil {
			if errors.Is(err, tlscarrier.ErrTunDesc) {
				badTunFrames.Add(1)
			}
			return
		}
		if ft == core.TypeData {
			dev.Write(payload)
		}
	}
}

// logDrops reports packets dropped for lack of a link or queue space.
func (s *l3Set) logDrops(ctx context.Context, logf func(string, ...any)) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n := s.drops.Swap(0); n > 0 {
				logf("l3: dropped %d packets in 30s on the tun side channel (queue limit or no link) — in the TLS modes the tun is for ping and light traffic; the user ports carry the bulk, unaffected", n)
			}
		}
	}
}

// flowHash is a packet's flow (flowKey): what picks its link, and what its TUN
// frame carries.
func flowHash(pkt []byte) uint32 {
	flow, _, _ := flowKey(pkt, false)
	return flow
}

// The bounds of the IPv6 extension-header walk (§3.1): past them the flow is
// the 3-tuple.
const (
	l3MaxExtHdrs  = 8
	l3MaxExtBytes = 256
)

// flowKey extracts a packet's flow key and hashes it with FNV-1a (inline, so
// the per-packet path does not allocate): the 5-tuple — addresses, protocol,
// ports — of TCP and UDP, over IPv4 and IPv6; the 3-tuple of anything else,
// and of an IPv6 packet whose extension headers pass the walk's bounds or
// that is a fragment. user is the inner address on the user's side (§3.1): the
// source of a packet from the user, the destination of one going to the user
// (toUser). ok is false for anything but an IPv4 or IPv6 packet; its flow is
// then a hash of its bytes, so every packet still maps deterministically to a
// link.
func flowKey(pkt []byte, toUser bool) (flow uint32, user netip.Addr, ok bool) {
	h := uint32(fnvOffset)
	switch {
	case len(pkt) >= 20 && pkt[0]>>4 == 4: // IPv4
		ihl := int(pkt[0]&0x0f) * 4
		proto := pkt[9]
		h = fnvAdd(h, pkt[12:20]) // src+dst IP
		h = fnvAdd(h, []byte{proto})
		if (proto == 6 || proto == 17) && len(pkt) >= ihl+4 {
			h = fnvAdd(h, pkt[ihl:ihl+4]) // src+dst ports
		}
		at := 12
		if toUser {
			at = 16
		}
		return h, netip.AddrFrom4([4]byte(pkt[at : at+4])), true
	case len(pkt) >= 40 && pkt[0]>>4 == 6: // IPv6
		h = fnvAdd(h, pkt[8:40]) // src+dst IP
		proto, l4 := ipv6Transport(pkt)
		h = fnvAdd(h, []byte{proto})
		if (proto == 6 || proto == 17) && l4 > 0 && len(pkt) >= l4+4 {
			h = fnvAdd(h, pkt[l4:l4+4]) // src+dst ports
		}
		at := 8
		if toUser {
			at = 24
		}
		return h, netip.AddrFrom16([16]byte(pkt[at : at+16])), true
	}
	return fnvAdd(h, pkt), netip.Addr{}, false
}

// ipv6Transport walks an IPv6 packet's extension headers to its transport
// header: its protocol and offset. Past l3MaxExtHdrs headers or l3MaxExtBytes
// bytes of them, at a fragment header (the ports are in the first piece only:
// all of them keep one flow), or where the packet ends, it gives up: the
// first next header and offset 0, which keys the packet by its 3-tuple.
func ipv6Transport(pkt []byte) (proto byte, off int) {
	next, off := pkt[6], 40
	for i := 0; ; i++ {
		var size int
		switch next {
		case 0, 43, 60, 135, 139, 140: // hop-by-hop, routing, destination, mobility, HIP, shim6
			if len(pkt) < off+2 {
				return pkt[6], 0
			}
			size = (int(pkt[off+1]) + 1) * 8
		case 51: // authentication header: in 4-byte words, less two
			if len(pkt) < off+2 {
				return pkt[6], 0
			}
			size = (int(pkt[off+1]) + 2) * 4
		case 44: // fragment
			return pkt[6], 0
		default:
			return next, off
		}
		if i == l3MaxExtHdrs || off-40+size > l3MaxExtBytes || len(pkt) < off+size {
			return pkt[6], 0
		}
		next, off = pkt[off], off+size
	}
}

const (
	fnvOffset = 2166136261
	fnvPrime  = 16777619
)

func fnvAdd(h uint32, b []byte) uint32 {
	for _, c := range b {
		h ^= uint32(c)
		h *= fnvPrime
	}
	return h
}

// mix32 is the murmur3 finaliser: a cheap, well-spread 32-bit mix.
func mix32(x uint32) uint32 {
	x ^= x >> 16
	x *= 0x85ebca6b
	x ^= x >> 13
	x *= 0xc2b2ae35
	x ^= x >> 16
	return x
}
