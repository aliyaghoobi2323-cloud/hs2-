package engine

import (
	"context"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/core"
	"github.com/hosseintaghipoursori-alt/hs2-tunnel/udpcarrier"
)

// This file plugs the udpcarrier package into the engine's Carrier seam, and
// adds the "auto" transport that chooses UDP or TCP per connection.
//
// UDP and TCP are kept completely independent: the auto dialer decides once, up
// front, with a lightweight probe on a SEPARATE socket, then commits to one
// carrier. If the UDP carrier later dies, the engine's reconnect loop calls the
// dialer again, which re-probes and — if UDP is now unreachable — silently
// returns a TCP carrier instead. The engine never tears down the TUN across
// that swap, so the user's session pauses but does not drop.

// maxRecoverableLoss is the wire-loss above which the FEC overhead is not worth
// it and the auto selector prefers TCP. The FEC adapter is capped at 50% loss;
// past ~45% both transports struggle, so TCP (no redundancy overhead) wins.
const maxRecoverableLoss = 0.45

// udpDialer dials a pure UDP (Noise+FEC) carrier.
type udpDialer struct {
	addr   string
	shared []byte
	mtu    int
}

func (d *udpDialer) Dial(ctx context.Context) (Carrier, error) {
	return udpcarrier.Dial(ctx, d.addr, d.shared, d.mtu)
}

// udpListener accepts UDP carriers.
type udpListener struct {
	ln *udpcarrier.Listener
}

func newUDPListener(addr string, shared []byte, mtu int) (*udpListener, error) {
	ln, err := udpcarrier.Listen(addr, shared, mtu)
	if err != nil {
		return nil, err
	}
	return &udpListener{ln: ln}, nil
}

func (l *udpListener) Accept(ctx context.Context) (Carrier, error) {
	return l.ln.Accept(ctx)
}

func (l *udpListener) Close() error { return l.ln.Close() }

// autoDialer probes the UDP path and returns a UDP carrier when it is reachable
// with FEC-recoverable loss, otherwise a TCP (noise) carrier. Both carriers are
// keyed from the one shared secret via core.StaticFromSeed, so no extra keys
// need distributing.
type autoDialer struct {
	addr   string
	shared []byte
	mtu    int
	log    func(string, ...any)
}

func (d *autoDialer) Dial(ctx context.Context) (Carrier, error) {
	res, err := udpcarrier.Probe(ctx, d.addr, d.shared, 16, 8*time.Millisecond, 300*time.Millisecond)
	if err == nil && res.Reachable && res.Loss <= maxRecoverableLoss {
		c, derr := udpcarrier.Dial(ctx, d.addr, d.shared, d.mtu)
		if derr == nil {
			d.logf("transport: UDP selected (probe loss %.0f%%, rtt %s)", res.Loss*100, res.RTTMedian)
			return c, nil
		}
		d.logf("transport: UDP probe ok but dial failed (%v); falling back to TCP", derr)
	} else if err == nil {
		d.logf("transport: UDP unusable (reachable=%v loss %.0f%%); using TCP", res.Reachable, res.Loss*100)
	} else {
		d.logf("transport: UDP probe error (%v); using TCP", err)
	}
	return d.tcpFallback().Dial(ctx)
}

func (d *autoDialer) tcpFallback() CarrierDialer {
	local, _ := core.StaticFromSeed(d.shared, "hs2-udp-initiator")
	server, _ := core.StaticFromSeed(d.shared, "hs2-udp-responder")
	return &noiseDialer{addr: d.addr, local: local, remoteStatic: server.Public, psk: d.shared}
}

func (d *autoDialer) logf(f string, a ...any) {
	if d.log != nil {
		d.log(f, a...)
	}
}

// autoListener serves BOTH a UDP and a TCP carrier on the same address and
// yields whichever the client actually establishes. The two listeners are
// independent: a failure on one never touches the other.
type autoListener struct {
	udp  *udpListener
	tcp  *noiseListener
	out  chan acceptResult
	ctx  context.Context
	stop context.CancelFunc
}

type acceptResult struct {
	c   Carrier
	err error
}

func newAutoListener(addr string, shared []byte, mtu int) (*autoListener, error) {
	local, err := core.StaticFromSeed(shared, "hs2-udp-responder")
	if err != nil {
		return nil, err
	}
	ul, err := newUDPListener(addr, shared, mtu)
	if err != nil {
		return nil, err
	}
	tl, err := newNoiseListener(addr, local, shared)
	if err != nil {
		ul.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	al := &autoListener{udp: ul, tcp: tl, out: make(chan acceptResult, 8), ctx: ctx, stop: cancel}
	go al.pump(ul)
	go al.pump(tl)
	return al, nil
}

func (l *autoListener) pump(ln CarrierListener) {
	for {
		c, err := ln.Accept(l.ctx)
		if err != nil {
			select {
			case <-l.ctx.Done():
				return
			default:
			}
			// One listener erroring must not kill the other, but must not spin
			// either: back off briefly before retrying.
			select {
			case <-l.ctx.Done():
				return
			case <-time.After(50 * time.Millisecond):
			}
			continue
		}
		select {
		case l.out <- acceptResult{c: c}:
		case <-l.ctx.Done():
			c.Close()
			return
		}
	}
}

func (l *autoListener) Accept(ctx context.Context) (Carrier, error) {
	select {
	case r := <-l.out:
		return r.c, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-l.ctx.Done():
		return nil, context.Canceled
	}
}

func (l *autoListener) Close() error {
	l.stop()
	l.udp.Close()
	l.tcp.Close()
	return nil
}
