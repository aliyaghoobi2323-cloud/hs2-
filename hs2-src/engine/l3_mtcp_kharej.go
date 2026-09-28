package engine

import (
	"context"
	"net"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

// Kharej L3 side: accept N parallel TLS links, and bind them all to ONE shared
// TUN. Packets arriving on any link are written to the TUN (the kernel then
// delivers/routes them, e.g. DNAT to the panel or to 10.77.0.2). Packets the
// TUN emits are queued on a link chosen by the same flow hash, so replies of a
// flow return on a consistent link.
//
// Unlike the Iran side, the kharej side does not dial — it accepts links from
// the tls dispatcher after auth, and registers each into the pool.

type L3KharejPool struct {
	set l3Set
	dev tunWriter
	log func(string, ...any)
}

func NewL3KharejPool(dev tunWriter, logf func(string, ...any)) *L3KharejPool {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &L3KharejPool{dev: dev, log: logf}
}

// Serve accepts authenticated carriers and adds each as a link, and runs the
// single TUN->links pump.
func (p *L3KharejPool) Serve(ctx context.Context, ln net.Listener, srv *tlscarrier.Server) error {
	go func() {
		<-ctx.Done()
		ln.Close()
		p.set.closeAll()
	}()
	go p.set.pumpTun(ctx, p.dev)
	go p.set.logDrops(ctx, p.log)
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			continue
		}
		go srv.Handle(ctx, conn, func(car *tlscarrier.Carrier) {
			l := newL3Link(car)
			p.set.add(l)
			p.log("l3: link up from %s (now %d)", conn.RemoteAddr(), p.set.count())
			p.set.serveLink(ctx, l, p.dev) // blocks until link dies
			p.set.remove(l)
			p.log("l3: link down (now %d)", p.set.count())
		})
	}
}
