package engine

import (
	"context"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/core"
	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
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
)

// l3Link is one link in L3 mode: a TLS carrier used as a framed packet pipe.
type l3Link struct {
	car  pktConn
	id   uint32        // rendezvous hash seed
	q    chan qpkt     // packets waiting for the writer
	ka   time.Duration // idle time before a keepalive is sent
	dead atomic.Bool
	once sync.Once
	done chan struct{}
}

// qpkt is a queued packet and when it was queued.
type qpkt struct {
	b *[]byte
	t time.Time
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
	case l.q <- qpkt{b, time.Now()}:
		return true
	default:
		return false
	}
}

// writeLoop is the only goroutine that writes to the link's carrier.
func (l *l3Link) writeLoop(pool *sync.Pool, drops *atomic.Uint64) {
	batch := make([]byte, 0, l3BatchBytes+4096)
	idle := time.NewTimer(l.ka)
	defer idle.Stop()
	add := func(p qpkt, now time.Time) {
		if now.Sub(p.t) > l3MaxSojourn {
			drops.Add(1)
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
		case <-idle.C:
			batch = tlscarrier.AppendFrame(batch, core.TypePing, make([]byte, core.KeepalivePad()))
		}
		if err := l.car.WriteRaw(batch); err != nil {
			l.markDead()
			return
		}
		idle.Reset(l.ka)
	}
}

// l3Set is a set of live links plus the pumps between them and the TUN. The
// zero value is ready to use.
type l3Set struct {
	mu    sync.RWMutex
	links []*l3Link
	pool  sync.Pool     // *[]byte packet buffers
	drops atomic.Uint64 // no link, queue full, or waited too long
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
func (s *l3Set) pick(pkt []byte) *l3Link {
	h := flowHash(pkt)
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
		if l := s.pick(*bp); l == nil || !l.enqueue(bp) {
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
		l.car.SetReadDeadline(time.Now().Add(l3DeadAfter))
		ft, payload, err := l.car.ReadFrameReuse()
		if err != nil {
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
