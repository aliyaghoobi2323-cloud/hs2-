package engine

import (
	"context"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/udpcarrier"
)

// Datagram-carrier dialers and listeners for the datagram tun pool. Each
// carrier is a udpcarrier.Conn over a chosen encapsulation (udp/icmp/gre/ipip/
// ipx); the pool (dgpool.go) treats it through the engine's Carrier interface.

// EncapConfig re-exports the carrier's encapsulation selector so cmd can build
// a datagram pool over any encapsulation without importing udpcarrier.
type EncapConfig = udpcarrier.EncapConfig

// dgUDPDialer dials one datagram carrier over the configured encapsulation.
type dgUDPDialer struct {
	addr   string
	ec     EncapConfig
	shared []byte
	mtu    int
}

func (d *dgUDPDialer) Dial(ctx context.Context) (Carrier, error) {
	return udpcarrier.DialCfg(ctx, d.addr, d.ec, d.shared, d.mtu)
}

// NewDgDialer builds a datagram-carrier dialer over the encapsulation ec.
func NewDgDialer(addr string, ec EncapConfig, shared []byte, mtu int) DgDialer {
	return &dgUDPDialer{addr: addr, ec: ec, shared: shared, mtu: mtu}
}

// dgUDPListener accepts datagram carriers over the configured encapsulation.
type dgUDPListener struct {
	ln *udpcarrier.Listener
}

func (l *dgUDPListener) Accept(ctx context.Context) (Carrier, error) {
	return l.ln.Accept(ctx)
}

func (l *dgUDPListener) Close() error { return l.ln.Close() }

// NewDgListener binds a datagram-carrier listener over the encapsulation ec.
func NewDgListener(addr string, ec EncapConfig, shared []byte, mtu int) (DgListener, error) {
	ln, err := udpcarrier.ListenCfg(addr, ec, shared, mtu)
	if err != nil {
		return nil, err
	}
	return &dgUDPListener{ln: ln}, nil
}
