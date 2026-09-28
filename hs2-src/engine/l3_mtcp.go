package engine

import (
	"context"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

// L3-over-mtcp: the persistent TUN's IP packets are distributed across N
// parallel TLS links by FLOW HASH, so every packet of one flow (same src/dst
// IP+port+proto) rides the same link and arrives in order — no cross-link
// reordering — while different flows spread across links to beat per-connection
// throttling.
//
// Each link carries IP-packet frames using the tlscarrier frame format (one
// real TLS session, Chrome fingerprint). The engine owns the TUN; the shared
// l3Set (l3_link.go) queues each outbound packet on its flow's link, and every
// link's reader injects received packets back into the TUN.

// L3Pool manages N links for L3 mode and routes packets by flow hash.
type L3Pool struct {
	dialer  *l3Dialer
	min     int
	max     int
	perFlow int // approx flows per link before scaling
	log     func(string, ...any)

	set l3Set
	dev tunWriter
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

// Run brings up the pool, starts the TUN read pump (TUN->links) and per-link
// pumps, and maintains the pool.
func (p *L3Pool) Run(ctx context.Context, dev tunWriter) {
	p.dev = dev
	for i := 0; i < p.min; i++ {
		p.addLink(ctx)
	}
	go p.set.pumpTun(ctx, dev)
	go p.set.logDrops(ctx, p.log)
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			p.set.closeAll()
			return
		case <-tick.C:
			p.reap(ctx)
		}
	}
}

func (p *L3Pool) addLink(ctx context.Context) {
	if p.set.count() >= p.max {
		return
	}
	car, err := p.dialer.dial()
	if err != nil {
		p.log("l3: link dial failed: %v", err)
		return
	}
	l := newL3Link(car)
	p.set.add(l)
	go p.set.serveLink(ctx, l, p.dev)
}

func (p *L3Pool) reap(ctx context.Context) {
	dead := p.set.removeDead()
	// Always top the pool back up to at least min. This runs every tick, so if
	// addLink fails while the peer is down, the next tick retries — recovery is
	// automatic once the peer returns. Not gated by the dead count.
	added := 0
	for p.set.count() < p.min {
		before := p.set.count()
		p.addLink(ctx)
		if p.set.count() == before {
			break // dial failed; try again next tick
		}
		added++
	}
	if dead > 0 || added > 0 {
		p.log("l3: reap: %d died, %d rebuilt, now %d", dead, added, p.set.count())
	}
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

// NewL3PoolFromCfg builds an L3Pool with a dialer from plain params (for cmd).
func NewL3PoolFromCfg(addr, sni string, key []byte, bindIP string, min, max, perFlow int, logf func(string, ...any)) *L3Pool {
	d := &l3Dialer{addr: addr, sni: sni, sharedKey: key, bindIP: bindIP}
	return NewL3Pool(d, min, max, perFlow, logf)
}
