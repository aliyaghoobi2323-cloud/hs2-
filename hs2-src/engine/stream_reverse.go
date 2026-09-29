package engine

import (
	"context"
	"net"
	"sync/atomic"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/obfs"
	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

// Reverse direction for the stream carriers (mtcp/tls).
//
// The data path and the smux roles are unchanged from direct: the iran EDGE is
// the smux client that originates one stream per user connection, and the
// kharej EXIT is the smux server that delivers streams to the panel. Only WHO
// DIALS the TLS carrier flips: in reverse the kharej dials the iran edge, so the
// first SYN originates abroad. This is what helps when outbound-from-iran is
// filtered but inbound-to-iran is not, or when the kharej is behind NAT.
//
//	direct : iran = TLS client (dials) + smux client ; kharej = TLS server + smux server
//	reverse: iran = TLS server (listens) + smux client ; kharej = TLS client (dials) + smux server

// acceptReverseLinks runs on the iran edge in reverse: it accepts the TLS
// carriers the kharej dials in, wraps each as an edge (smux-client) link, and
// feeds it to the pool. Each accepted link is held open for its lifetime so the
// user-stream pool keeps it.
func acceptReverseLinks(ctx context.Context, ln net.Listener, srv *tlscarrier.Server, lm *LinkManager, logf func(string, ...any)) {
	sampler := obfs.NewHTTPSLengthSampler()
	go func() { <-ctx.Done(); ln.Close() }()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			continue
		}
		go srv.Handle(ctx, conn, func(car *tlscarrier.Carrier) {
			l, err := newEdgeLink(car, sampler)
			if err != nil {
				car.Close()
				return
			}
			lm.AddLink(l)
			// Hold the carrier open until the link's smux session ends; the pool
			// reaps it after that.
			select {
			case <-ctx.Done():
			case <-l.closed():
			}
			l.Close()
		})
	}
}

// runKharejReverse runs on the kharej exit in reverse: it keeps RevLinks
// parallel links dialed to the iran edge, running the smux server on each and
// delivering streams to the panel. A fixed number of links is used because the
// exit cannot see the user load the edge sees.
func runKharejReverse(ctx context.Context, cfg KharejConfig, l3 *l3Set, logf func(string, ...any)) error {
	n := cfg.RevLinks
	if n < 1 {
		n = 1
	}
	var links atomic.Int32
	for i := 0; i < n; i++ {
		go maintainExitLink(ctx, cfg, l3, &links, logf)
	}
	<-ctx.Done()
	return nil
}

func maintainExitLink(ctx context.Context, cfg KharejConfig, l3 *l3Set, links *atomic.Int32, logf func(string, ...any)) {
	backoff := 500 * time.Millisecond
	for ctx.Err() == nil {
		car, err := cfg.RevDial()
		if err != nil {
			logf("reverse dial to edge failed: %v (retry in %s)", err, backoff)
			if !sleepCtx(ctx, backoff) {
				return
			}
			if backoff < 8*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = 500 * time.Millisecond
		sess, err := newSession(car.RawConn(), true, nil) // smux server
		if err != nil {
			car.Close()
			continue
		}
		// Close the session when ctx ends, so AcceptStream unblocks and this
		// goroutine exits instead of hanging on a link the edge keeps alive with
		// keepalives (mirrors the direct exit path in stream_kharej.go).
		closed := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				sess.Close()
			case <-closed:
			}
		}()
		logf("reverse link up to edge (now %d)", links.Add(1))
		for {
			st, err := sess.AcceptStream()
			if err != nil {
				break
			}
			go serveStream(ctx, st, cfg, l3)
		}
		close(closed)
		sess.Close()
		car.Close()
		logf("reverse link down (now %d); redial", links.Add(-1))
	}
}
