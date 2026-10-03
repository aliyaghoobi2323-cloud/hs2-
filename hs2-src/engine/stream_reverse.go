package engine

import (
	"context"
	"net"

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
		from := conn.RemoteAddr().String()
		if h, _, err := net.SplitHostPort(from); err == nil {
			from = h
		}
		go srv.Handle(ctx, conn, func(car *tlscarrier.Carrier) {
			l, err := newEdgeLink(car, sampler)
			if err != nil {
				car.Close()
				return
			}
			lm.AddLink(l, from)
			// Hold the carrier open until the link's smux session ends, then take
			// it out of the pool at once (and log why) instead of waiting for the
			// next health tick.
			select {
			case <-ctx.Done():
				l.Close()
			case <-l.closed():
				lm.DropLink(l, from)
			}
		})
	}
}

// runKharejReverse runs on the kharej exit in reverse: it maintains a DYNAMIC
// pool of links dialed to the iran edge (exit_pool.go). The edge, which alone
// sees the users and the throughput, drives the count over the pool-control
// channel between RevMin and RevMax; RevLinks is only the size held until the
// edge first speaks (so an older edge with no pool-control still gets a working
// pool).
func runKharejReverse(ctx context.Context, cfg KharejConfig, l3 *l3Set, logf func(string, ...any)) error {
	min, max := cfg.RevMin, cfg.RevMax
	if min < 1 {
		min = 1
	}
	if max < min {
		max = min
	}
	initial := cfg.RevLinks
	if initial < min {
		initial = min
	}
	if initial > max {
		initial = max
	}
	dial := func() (dialedLink, error) { return cfg.RevDial() }
	pool := newExitPool(ctx, min, max, dial, logf)
	// serve captures the pool so a link's pool-control stream can resize it; set
	// before any slot starts, then start the pool at its initial size.
	pool.serve = func(ctx context.Context, car dialedLink) string {
		return serveReverseLink(ctx, car.(*tlscarrier.Carrier), cfg, l3, pool)
	}
	if cfg.OnStart != nil {
		cfg.OnStart(pool.stats)
	}
	pool.setTarget(initial)
	<-ctx.Done()
	return nil
}

// serveReverseLink runs the smux server over one reverse carrier and delivers
// its streams to the panel, returning when the link dies or ctx ends — with
// why it ended, in operator words (the first socket error if there was one).
// pool (may be nil) lets the link's pool-control stream resize the exit pool.
func serveReverseLink(ctx context.Context, car *tlscarrier.Carrier, cfg KharejConfig, l3 *l3Set, pool *exitPool) string {
	mtr := &linkMeter{}                                         // download-side counters, reported over kindStats
	sess, why, err := newSession(car.RawConn(), true, nil, mtr) // smux server
	if err != nil {
		return "session setup failed: " + describeNetErr(err)
	}
	// Close the session when ctx ends, so AcceptStream unblocks and this returns
	// instead of hanging on a link the edge keeps alive with keepalives.
	closed := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			sess.Close()
		case <-closed:
		}
	}()
	var downErr error
	for {
		st, err := sess.AcceptStream()
		if err != nil {
			downErr = err
			break
		}
		go serveStream(ctx, st, cfg, l3, car, pool, mtr)
	}
	close(closed)
	sess.Close()
	return sessionEndReason(why, downErr)
}
