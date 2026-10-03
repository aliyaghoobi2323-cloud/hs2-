package engine

import (
	"context"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
	"github.com/xtaci/smux"
)

// KharejConfig configures the kharej (exit) side of stream mode: the side with
// the panel. In the DIRECT direction it LISTENS for links the iran edge dials;
// in the REVERSE direction it DIALS RevLinks parallel links to the iran edge
// (RevDial set) and runs the smux server on each.
type KharejConfig struct {
	Listener net.Listener
	Server   *tlscarrier.Server
	Panel    string    // where TCP and UDP streams are delivered
	TUN      tunWriter // non-nil: accept hs0 side-channel streams
	Log      func(string, ...any)

	// Reverse exit: dial the iran edge instead of listening. RevDial returns a
	// fresh authenticated TLS carrier to the edge. The link count is dynamic: the
	// edge drives it over the pool-control channel between RevMin and RevMax, and
	// RevLinks is only the count to hold until the edge first speaks.
	RevDial  func() (*tlscarrier.Carrier, error)
	RevLinks int
	RevMin   int
	RevMax   int

	// OnStart, if set, is called once with a function that returns a live
	// snapshot of the link pattern, for monitoring.
	OnStart func(StatsFn)
}

// RunKharej accepts links and serves their streams until ctx ends.
func RunKharej(ctx context.Context, cfg KharejConfig) error {
	logf := cfg.Log
	if logf == nil {
		logf = func(string, ...any) {}
	}
	var l3 *l3Set
	if cfg.TUN != nil {
		l3 = &l3Set{}
		go l3.pumpTun(ctx, cfg.TUN)
		go l3.logDrops(ctx, logf)
	}
	if cfg.RevDial != nil {
		return runKharejReverse(ctx, cfg, l3, logf)
	}
	var links atomic.Int32
	if cfg.OnStart != nil {
		cfg.OnStart(func() PoolStats { return PoolStats{Links: int(links.Load()), Phase: "listening"} })
	}
	go func() { <-ctx.Done(); cfg.Listener.Close() }()
	for {
		conn, err := cfg.Listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			continue
		}
		go cfg.Server.Handle(ctx, conn, func(car *tlscarrier.Carrier) {
			mtr := &linkMeter{} // download-side counters, reported over kindStats
			sess, why, err := newSession(car.RawConn(), true, nil, mtr)
			if err != nil {
				car.Close()
				return
			}
			go func() {
				select {
				case <-ctx.Done():
					sess.Close()
				case <-sess.CloseChan():
				}
			}()
			logf("link up from %s (now %d)", conn.RemoteAddr(), links.Add(1))
			var downErr error
			for {
				st, err := sess.AcceptStream()
				if err != nil {
					downErr = err
					break
				}
				go serveStream(ctx, st, cfg, l3, car, nil, mtr)
			}
			sess.Close()
			car.Close()
			logf("link down from %s: %s (now %d)", conn.RemoteAddr(), sessionEndReason(why, downErr), links.Add(-1))
		})
	}
}

// sessionEndReason explains why an exit-side smux session ended: the socket
// error that killed it if there was one, else smux's own reason.
func sessionEndReason(why func() string, sessErr error) string {
	if why != nil {
		if r := why(); r != "" {
			return r
		}
	}
	if sessErr == nil || errors.Is(sessErr, io.ErrClosedPipe) {
		return "session ended (keepalive timeout or closed by the other server)"
	}
	return describeNetErr(sessErr)
}

func serveStream(ctx context.Context, st *smux.Stream, cfg KharejConfig, l3 *l3Set, car *tlscarrier.Carrier, pool *exitPool, mtr *linkMeter) {
	var kind [1]byte
	st.SetReadDeadline(time.Now().Add(kindTimeout))
	if _, err := io.ReadFull(st, kind[:]); err != nil {
		st.Close()
		return
	}
	st.SetReadDeadline(time.Time{})
	switch kind[0] {
	case kindCtrl:
		serveControl(ctx, st, car)
	case kindPool:
		servePoolCtl(ctx, st, pool)
	case kindStats:
		// The reverse exit has its own [min,max] pool; a direct exit does not
		// (it just accepts links the edge dials, so its max never binds). Report
		// the pool's max when there is one, 0 otherwise.
		exitMax := 0
		if pool != nil {
			exitMax = pool.max
		}
		serveStats(ctx, st, car, mtr, exitMax)
	case kindTCP:
		up, err := net.DialTimeout("tcp", cfg.Panel, 5*time.Second)
		if err != nil {
			st.Close()
			return
		}
		relay(st, up)
	case kindUDP:
		up, err := net.Dial("udp", cfg.Panel)
		if err != nil {
			st.Close()
			return
		}
		relayUDPConn(st, up)
	case kindL3:
		if l3 == nil {
			st.Close() // this side runs without a TUN
			return
		}
		pl := newL3Link(newStreamPkt(st))
		l3.add(pl)
		l3.serveLink(ctx, pl, cfg.TUN)
		l3.remove(pl)
	default:
		st.Close()
	}
}
