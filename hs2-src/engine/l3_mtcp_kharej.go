package engine

import (
	"context"
	"net"
	"sync"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/core"
	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

// Kharej L3 side: accept N parallel TLS links, and bind them all to ONE shared
// TUN. Packets arriving on any link are written to the TUN (the kernel then
// delivers/routes them, e.g. DNAT to the panel or to 10.77.0.2). Packets the
// TUN emits are sent back on a link chosen by the same flow hash, so replies of
// a flow return on a consistent link.
//
// Unlike the Iran side, the kharej side does not dial — it accepts links from
// the reality/tls dispatcher after auth, and registers each into the pool.

type L3KharejPool struct {
	mu    sync.RWMutex
	links []*l3Link
	dev   tunWriter
	log   func(string, ...any)
}

func NewL3KharejPool(dev tunWriter, logf func(string, ...any)) *L3KharejPool {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &L3KharejPool{dev: dev, log: logf}
}

// Serve accepts authenticated carriers and adds each as a link. It also starts
// the single TUN->links pump once, on first call.
func (p *L3KharejPool) Serve(ctx context.Context, ln net.Listener, srv *tlscarrier.Server) error {
	var once sync.Once
	go func() {
		<-ctx.Done()
		ln.Close()
		p.mu.Lock()
		for _, l := range p.links {
			l.car.Close()
		}
		p.mu.Unlock()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			continue
		}
		go srv.Handle(ctx, conn, func(car *tlscarrier.Carrier) {
			l := &l3Link{car: car}
			p.mu.Lock()
			p.links = append(p.links, l)
			p.mu.Unlock()
			once.Do(func() { go p.tunToLinks(ctx) })
			p.log("l3: link up from %s (now %d)", conn.RemoteAddr(), p.count())
			p.linkToTun(ctx, l) // blocks until link dies
			l.car.Close()
			p.remove(l)
			p.log("l3: link down (now %d)", p.count())
		})
	}
}

func (p *L3KharejPool) tunToLinks(ctx context.Context) {
	buf := make([]byte, p.dev.MTU()+128)
	for ctx.Err() == nil {
		n, err := p.dev.Read(buf)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			continue
		}
		l := p.pick(buf[:n])
		if l == nil {
			continue
		}
		if err := l.car.SendFrame(core.TypeData, buf[:n]); err != nil {
			l.markDead()
		}
	}
}

func (p *L3KharejPool) linkToTun(ctx context.Context, l *l3Link) {
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

func (p *L3KharejPool) pick(pkt []byte) *l3Link {
	p.mu.RLock()
	defer p.mu.RUnlock()
	alive := make([]*l3Link, 0, len(p.links))
	for _, l := range p.links {
		if l.Alive() {
			alive = append(alive, l)
		}
	}
	if len(alive) == 0 {
		return nil
	}
	return alive[flowHash(pkt)%uint32(len(alive))]
}

func (p *L3KharejPool) remove(dead *l3Link) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.links[:0]
	for _, l := range p.links {
		if l != dead {
			out = append(out, l)
		}
	}
	p.links = out
}

func (p *L3KharejPool) count() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.links)
}
