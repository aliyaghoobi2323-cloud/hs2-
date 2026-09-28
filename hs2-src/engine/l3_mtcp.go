package engine

import (
	"context"
	"encoding/binary"
	"hash/fnv"
	"sync"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/core"
	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

// L3-over-mtcp: the persistent TUN's IP packets are distributed across N
// parallel TLS links by FLOW HASH, so every packet of one flow (same src/dst
// IP+port+proto) rides the same link and arrives in order — no cross-link
// reordering — while different flows spread across links to beat per-connection
// throttling.
//
// Each link carries IP-packet frames using the tlscarrier frame format (one
// real TLS session, Chrome fingerprint, length-padding). The engine owns the
// TUN; a flowRouter maps each outbound packet to a link and writes it there,
// and every link's reader injects received packets back into the TUN.

// l3Link is one link in L3 mode: a TLS carrier used as a framed packet pipe.
type l3Link struct {
	car    *tlscarrier.Carrier
	dead   bool
	mu     sync.Mutex
	active int32 // flows currently hashed here (approximate load)
}

func (l *l3Link) Alive() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return !l.dead
}

func (l *l3Link) markDead() {
	l.mu.Lock()
	l.dead = true
	l.mu.Unlock()
}

// flowKey extracts a 5-tuple-ish key from an IP packet for hashing. Supports
// IPv4 TCP/UDP; falls back to src+dst+proto for others, so every packet still
// maps deterministically to a link.
func flowHash(pkt []byte) uint32 {
	h := fnv.New32a()
	if len(pkt) >= 20 && pkt[0]>>4 == 4 { // IPv4
		ihl := int(pkt[0]&0x0f) * 4
		proto := pkt[9]
		h.Write(pkt[12:20]) // src+dst IP
		h.Write([]byte{proto})
		if (proto == 6 || proto == 17) && len(pkt) >= ihl+4 {
			h.Write(pkt[ihl : ihl+4]) // src+dst ports
		}
	} else if len(pkt) >= 40 && pkt[0]>>4 == 6 { // IPv6
		h.Write(pkt[8:40])
	} else {
		h.Write(pkt)
	}
	return h.Sum32()
}

// L3Pool manages N links for L3 mode and routes packets by flow hash.
type L3Pool struct {
	dialer  *l3Dialer
	min     int
	max     int
	perFlow int // approx flows per link before scaling
	log     func(string, ...any)

	mu    sync.RWMutex
	links []*l3Link
	dev   tunWriter
	flows atomic32Map // flowHash -> link index sticky cache
}

type tunWriter interface {
	Write([]byte) (int, error)
	Read([]byte) (int, error)
	MTU() int
}

func NewL3Pool(dialer *l3Dialer, min, max, perFlow int, logf func(string, ...any)) *L3Pool {
	if min < 1 {
		min = 1
	}
	if max < min {
		max = min
	}
	if perFlow < 1 {
		perFlow = 256
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &L3Pool{dialer: dialer, min: min, max: max, perFlow: perFlow, log: logf}
}

// pickLink maps a packet to a link: sticky by flow hash so a flow stays put,
// modulo the current link count. When the link count changes, existing flows
// may move — acceptable because L3 flows tolerate a brief reorder far better
// than being split mid-flight, and TCP recovers.
func (p *L3Pool) pickLink(pkt []byte) *l3Link {
	p.mu.RLock()
	defer p.mu.RUnlock()
	n := len(p.links)
	if n == 0 {
		return nil
	}
	// find alive links; hash selects among them
	alive := p.links[:0:0]
	for _, l := range p.links {
		if l.Alive() {
			alive = append(alive, l)
		}
	}
	if len(alive) == 0 {
		return nil
	}
	idx := flowHash(pkt) % uint32(len(alive))
	return alive[idx]
}

// Run brings up the pool, starts the TUN read pump (TUN->link) and per-link
// read pumps (link->TUN), and maintains the pool.
func (p *L3Pool) Run(ctx context.Context, dev tunWriter) {
	p.dev = dev
	for i := 0; i < p.min; i++ {
		p.addLink(ctx)
	}
	go p.tunToLinks(ctx)
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			p.closeAll()
			return
		case <-tick.C:
			p.reap(ctx)
		}
	}
}

// tunToLinks reads IP packets from the TUN and writes each to its flow's link.
func (p *L3Pool) tunToLinks(ctx context.Context) {
	buf := make([]byte, p.dev.MTU()+128)
	for ctx.Err() == nil {
		n, err := p.dev.Read(buf)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			continue
		}
		l := p.pickLink(buf[:n])
		if l == nil {
			continue // no link; drop, TCP resends
		}
		if err := l.car.SendFrame(core.TypeData, buf[:n]); err != nil {
			l.markDead()
		}
	}
}

// linkToTun pumps received IP packets from one link into the TUN.
func (p *L3Pool) linkToTun(ctx context.Context, l *l3Link) {
	for ctx.Err() == nil {
		l.car.SetReadDeadline(time.Now().Add(deadAfter))
		ft, payload, err := l.car.ReadFrame()
		if err != nil {
			l.markDead()
			return
		}
		if ft == core.TypeData {
			p.dev.Write(payload)
		}
	}
}

func (p *L3Pool) addLink(ctx context.Context) {
	if p.count() >= p.max {
		return
	}
	car, err := p.dialer.dial()
	if err != nil {
		p.log("l3: link dial failed: %v", err)
		return
	}
	l := &l3Link{car: car}
	p.mu.Lock()
	p.links = append(p.links, l)
	p.mu.Unlock()
	go p.linkToTun(ctx, l)
}

func (p *L3Pool) reap(ctx context.Context) {
	p.mu.Lock()
	alive := p.links[:0]
	dead := 0
	for _, l := range p.links {
		if l.Alive() {
			alive = append(alive, l)
		} else {
			l.car.Close()
			dead++
		}
	}
	p.links = alive
	p.mu.Unlock()
	// Always top the pool back up to at least min. This runs every tick, so if
	// addLink fails while the peer is down, the next tick retries — recovery is
	// automatic once the peer returns. Not gated by the dead count.
	added := 0
	for p.count() < p.min {
		before := p.count()
		p.addLink(ctx)
		if p.count() == before {
			break // dial failed; try again next tick
		}
		added++
	}
	if dead > 0 || added > 0 {
		p.log("l3: reap: %d died, %d rebuilt, now %d", dead, added, p.count())
	}
}

func (p *L3Pool) count() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.links)
}

func (p *L3Pool) closeAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, l := range p.links {
		l.car.Close()
	}
	p.links = nil
}

// l3Dialer dials one TLS carrier for L3 mode.
type l3Dialer struct {
	addr, sni string
	sharedKey []byte
	bindIP    string // optional local source IP
}

func (d *l3Dialer) dial() (*tlscarrier.Carrier, error) {
	return tlscarrier.DialFrom(d.addr, d.sni, d.sharedKey, d.bindIP)
}

// atomic32Map is a tiny concurrent map placeholder (unused sticky cache hook).
type atomic32Map struct{}

var _ = binary.BigEndian

// NewL3PoolFromCfg builds an L3Pool with a dialer from plain params (for cmd).
func NewL3PoolFromCfg(addr, sni string, key []byte, bindIP string, min, max, perFlow int, logf func(string, ...any)) *L3Pool {
	d := &l3Dialer{addr: addr, sni: sni, sharedKey: key, bindIP: bindIP}
	return NewL3Pool(d, min, max, perFlow, logf)
}
