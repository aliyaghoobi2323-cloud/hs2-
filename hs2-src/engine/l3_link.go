package engine

import (
	"context"
	"encoding/binary"
	"math/rand/v2"
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
// or (in stream mode) one smux stream of a shared link.
type pktConn interface {
	WriteRaw([]byte) error
	ReadFrameReuse() (byte, []byte, error)
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
	q   chan qpkt     // packets waiting for the writer
	ka  time.Duration // idle time before a keepalive is sent
	// kaFn, when set, decides ka per keepalive (stream mode: by what the peer
	// supports); deadAfter, when set, replaces l3DeadAfter for the reader.
	kaFn      func() time.Duration
	deadAfter time.Duration
	// heavy (nil: unknown, counted light) is the link's count of user streams
	// at a download's rate, as its sampler last stored it (linkMeter.heavyN);
	// the lane (HS2_L3_LANE) keeps interactive flows off links where it is > 0.
	heavy *atomic.Int32
	// stripe (nil: unknown) counts the stripe subs (exit) or stripe holds
	// (Iran) on the link (linkMeter.stripeN, HS2_MCONN_STRIPE): the lane
	// moves an interactive flow off such a link at once.
	stripe *atomic.Int32
	// lane (nil: not tracked) is linkMeter.laneAt: stamped when the lane
	// sends an interactive packet here (HS2_MCONN_STRIPE_SHARE reads it).
	lane *atomic.Int64
	// dupOK (nil: no) reports whether the other server reads TypeDataDup
	// frames (capL3Dup); HS2_L3_DUP sends them only over such links.
	dupOK func() bool
	// path is the key of the path the link's TCP connection rides ("" not
	// known); HS2_DUP_PATHS puts a second copy on another path (dup_paths.go).
	path string
	// qs is the link's HS2_QUIET_IDLE state (nil: off; quiet.go).
	qs   *quietState
	dead atomic.Bool
	once sync.Once
	done chan struct{}
}

// qpkt is a queued packet and when it was queued.
type qpkt struct {
	b   *[]byte
	t   time.Time
	id  uint32 // dup: the TypeDataDup id (l3_dup.go)
	dup bool   // send as TypeDataDup, not TypeData
}

func newL3Link(car pktConn) *l3Link {
	return &l3Link{
		car:  car,
		id:   rand.Uint32(),
		q:    make(chan qpkt, l3QueueLen),
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
	return newStreamL3LinkQ(st, quiet, sessReads, nil)
}

// newStreamL3LinkQ is newStreamL3Link on a link with HS2_QUIET_IDLE state qs
// (nil: without the key, exactly newStreamL3Link): the link's TUN packets
// keep it busy, an idle link sends its TUN keepalive every 45–65 s, and its
// silence limits loosen while it is idle (quiet.go).
func newStreamL3LinkQ(st *smux.Stream, quiet func() bool, sessReads func() uint64, qs *quietState) *l3Link {
	l := newL3Link(newStreamPkt(st))
	l.qs = qs
	if sessReads != nil {
		go l.watchSession(sessReads)
	}
	l.deadAfter = l3StreamDeadAfter
	l.kaFn = func() time.Duration {
		if qs.sendQuiet() {
			return between(quietL3Min, quietL3Max)
		}
		if quiet() {
			return jitterAround(l3QuietKeepalive)
		}
		return l3KeepaliveEvery
	}
	return l
}

// readDeadAfter is the reader's limit without a frame: with a peer that may
// go quiet (HS2_QUIET_IDLE both sides) it allows the quiet TUN keepalive.
func (l *l3Link) readDeadAfter() time.Duration {
	if l.qs != nil && l.qs.peerQuiet() {
		return quietL3ReadDead
	}
	if l.deadAfter > 0 {
		return l.deadAfter
	}
	return l3DeadAfter
}

// watchSession drops the link once its session has received nothing for
// l3SessionSilent (it runs beside the reader, so a frame is never cut by a
// read deadline). With HS2_QUIET_IDLE, quietSilent while the link is idle.
func (l *l3Link) watchSession(reads func() uint64) {
	every := min(2*time.Second, l3SessionSilent/4)
	t := time.NewTicker(every)
	defer t.Stop()
	last, since := reads(), time.Now()
	var qw quietWatch
	for {
		select {
		case <-l.done:
			return
		case now := <-t.C:
			limit, ref := l3SessionSilent, since
			if l.qs != nil {
				lax := l.qs.lenient()
				ref = qw.ref(lax, since, now)
				if lax {
					limit = quietSilent
				}
			}
			if n := reads(); n != last {
				last, since = n, now
			} else if now.Sub(ref) >= limit {
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

// enqueue hands a packet to the link without ever blocking.
func (l *l3Link) enqueue(b *[]byte) bool {
	select {
	case l.q <- qpkt{b: b, t: time.Now()}:
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
	add := func(p qpkt, now time.Time) {
		if now.Sub(p.t) > l3MaxSojourn {
			drops.Add(1)
		} else if p.dup {
			batch = appendDupFrame(batch, p.id, *p.b)
		} else {
			batch = tlscarrier.AppendFrame(batch, core.TypeData, *p.b)
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
			l.qs.markL3()
		case <-idle.C:
			batch = tlscarrier.AppendFrame(batch, core.TypePing, make([]byte, core.KeepalivePad()))
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
	lane  *l3Lane       // HS2_L3_LANE state (nil: off); see l3_lane.go
	// HS2_L3_DUP (l3_dup.go): dup sends interactive packets over two links,
	// dupSeq is the next TypeDataDup id (pumpTun only), rx drops the second
	// copy of what the other server sends twice.
	dup    bool
	dupSeq uint32
	rx     l3Dedup
	// HS2_L3_FEC (l3_fec.go): fec is the sending side (nil: off), fecRx the
	// receiving side (nil until the other server sent a parity packet).
	fec   *l3Fec
	fecRx atomic.Pointer[l3FecRx]
}

func (s *l3Set) add(l *l3Link) {
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
//
// With the lane on (HS2_L3_LANE, l3_lane.go) an interactive flow goes by
// rendezvous over the links carrying no download instead. pumpTun is pick's
// only caller: the lane's flow table is not locked.
func (s *l3Set) pick(pkt []byte) *l3Link {
	l, _ := s.pickDup(pkt)
	return l
}

// pickDup is pick plus, with HS2_L3_DUP on and an interactive flow whose
// link's peer reads TypeDataDup, the second link to send it over (nil: send
// once).
func (s *l3Set) pickDup(pkt []byte) (first, second *l3Link) {
	first, second, _, _ = s.pickAll(pkt)
	return first, second
}

// pickAll is pickDup plus, with HS2_L3_FEC on, the striping plan of a flow
// the FEC takes (fec true: send by fp, l3_fec.go).
func (s *l3Set) pickAll(pkt []byte) (first, second *l3Link, fp fecPlan, fec bool) {
	h := flowHash(pkt)
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.lane != nil {
		if l, ok := s.lane.pick(s.links, pkt, h); ok {
			if s.dup && l != nil && l.canDup() {
				second = dupSecond(s.links, h, l)
				if dupPathsOn && second != nil && l.path != "" {
					countDupPath(second.path != l.path, true)
				}
			}
			return l, second, fp, false
		}
		if s.fec != nil {
			e := &s.lane.flows[h&(l3LaneSlots-1)]
			if fp, ok := s.fecPlan(e, s.links, pkt, h, s.lane.now()); ok {
				return nil, nil, fp, true
			}
		}
	}
	return rendezvous(s.links, h, false), nil, fp, false
}

// rendezvous is the live link (lightOnly: and carrying no download) with the
// highest weight for flow hash h; nil if there is none.
func rendezvous(links []*l3Link, h uint32, lightOnly bool) *l3Link {
	var best *l3Link
	var bestW uint32
	for _, l := range links {
		if !l.Alive() || lightOnly && l.isHeavy() {
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
	go l.writeLoop(&s.pool, &s.drops)
	s.linkToTun(ctx, l, dev)
}

// pumpTun reads IP packets from the TUN and queues each on its flow's link.
// It never blocks on a link: no link or a full queue means drop.
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
		l, l2, fp, fec := s.pickAll(*bp)
		if fec {
			s.sendFec(bp, fp)
			continue
		}
		if l2 != nil {
			s.sendDup(bp, l, l2)
			continue
		}
		if l == nil || !l.enqueue(bp) {
			s.pool.Put(bp)
			s.drops.Add(1)
		}
	}
}

// linkToTun pumps received IP packets from one link into the TUN. Any frame,
// keepalives included, proves the link is alive and extends the deadline.
func (s *l3Set) linkToTun(ctx context.Context, l *l3Link, dev tunWriter) {
	defer l.markDead()
	for ctx.Err() == nil {
		l.car.SetReadDeadline(time.Now().Add(l.readDeadAfter()))
		ft, payload, err := l.car.ReadFrameReuse()
		if err != nil {
			return
		}
		switch ft {
		case core.TypeData:
			l.qs.markL3()
			dev.Write(payload)
		case core.TypeDataDup:
			l.qs.markL3()
			if p := s.rx.accept(payload); p != nil {
				// a parity packet, or one the FEC may need (l3_fec.go)
				s.fecRecv(binary.BigEndian.Uint32(payload)&l3DupIDMask, p, dev)
			}
		}
	}
}

// logDrops reports packets dropped for lack of a link or queue space.
func (s *l3Set) logDrops(ctx context.Context, logf func(string, ...any)) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	var fc fecCounts
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n := s.drops.Swap(0); n > 0 {
				logf("l3: dropped %d packets in 30s on the tun side channel (queue limit or no link) — in the TLS modes the tun is for ping and light traffic; the user ports carry the bulk, unaffected", n)
			}
			if c := s.fecCounts(); c != fc {
				logf("l3 fec (HS2_L3_FEC), 30s: %d packets striped (%d KB) · %d parity (%d KB) · %d groups over budget · %d rebuilt here · %d parity not needed",
					c.striped-fc.striped, (c.dataB-fc.dataB)>>10, c.parity-fc.parity, (c.parB-fc.parB)>>10, c.capped-fc.capped, c.recovered-fc.recovered, c.late-fc.late)
				fc = c
			}
		}
	}
}

// flowHash extracts a 5-tuple-ish key from an IP packet and hashes it with
// FNV-1a (inline, so the per-packet path does not allocate). Supports IPv4
// TCP/UDP; falls back to src+dst+proto for others, so every packet still maps
// deterministically to a link.
func flowHash(pkt []byte) uint32 {
	h := uint32(fnvOffset)
	if len(pkt) >= 20 && pkt[0]>>4 == 4 { // IPv4
		ihl := int(pkt[0]&0x0f) * 4
		proto := pkt[9]
		h = fnvAdd(h, pkt[12:20]) // src+dst IP
		h = fnvAdd(h, []byte{proto})
		if (proto == 6 || proto == 17) && len(pkt) >= ihl+4 {
			h = fnvAdd(h, pkt[ihl:ihl+4]) // src+dst ports
		}
	} else if len(pkt) >= 40 && pkt[0]>>4 == 6 { // IPv6
		h = fnvAdd(h, pkt[8:40])
	} else {
		h = fnvAdd(h, pkt)
	}
	return h
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
