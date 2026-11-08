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
	Panel    string    // the default panel (expose): where a stream with no own target goes
	TUN      tunWriter // non-nil: accept hs0 side-channel streams

	// Routes (port_map) gives some Iran user ports their own target; a port
	// not in it goes to Panel, and with no Panel it is refused (routes.go).
	Routes map[int]string
	Log    func(string, ...any)

	// Reverse exit: dial the iran edge instead of listening. RevDial returns a
	// fresh authenticated TLS carrier to the edge. The link count is dynamic: the
	// edge drives it over the pool-control channel between RevMin and RevMax, and
	// RevLinks is only the count to hold until the edge first speaks.
	RevDial func() (*tlscarrier.Carrier, error)
	// RevDialScout, if set, is RevDial with a short TCP connect timeout, for
	// the one slot that keeps trying while no link is up: on a black-holed
	// path a long connect spaces its SYNs out to 4 s, and the first link
	// after the path returns waits for the next one.
	RevDialScout func() (*tlscarrier.Carrier, error)
	RevLinks     int
	RevMin       int
	RevMax       int
	// RevLB (reverse, optional): place the dialed links on several paths by
	// weight (lb.go). RevDialPath dials one path (scout: short timeout).
	RevLB       *LB
	RevDialPath func(p LBPath, scout bool) (*tlscarrier.Carrier, error)
	// LB advertises capLB to the edge (lb enabled on this side).
	LB bool
	// Advert (lb on): answers the edge's kindLBAdvert (lb_advert.go).
	Advert *LBAdvert

	// OnStart, if set, is called once with a function that returns a live
	// snapshot of the link pattern, for monitoring.
	OnStart func(StatsFn)

	// MaxLinks is this server's link-pool ceiling, told to the edge over
	// kindInfo so both servers can show the effective ceiling. A reverse exit
	// reports the max it applies (RevMax); a direct exit reports its configured
	// value although it does not apply it (the edge alone decides in direct).
	MaxLinks int

	// peers is the set of live links; each stores the ceiling its edge reported
	// (set up by RunKharej; nil in tests that call serveStream directly).
	peers *linkPeers
	// noRoute rate-limits the "no target for user port P" log line.
	noRoute *noRouteLog
	// traffic counts this exit's own user connections and throughput.
	traffic *exitTraffic
	// mconns is the exit's table of multi-link connections by id
	// (mconn_link.go; nil: the process-wide one).
	mconns *mconnTable
}

// routeTable is the exit's routing table: Routes, then Panel.
func (cfg KharejConfig) routeTable() RouteTable {
	return RouteTable{Default: cfg.Panel, Ports: cfg.Routes}
}

// info is what this exit tells the edge over kindInfo: its ceiling (myMax),
// that it routes port-tagged connections, whether it has a default panel, and
// the ports with their own target.
func (cfg KharejConfig) info(myMax int) peerInfo {
	t := cfg.routeTable()
	pi := peerInfo{MaxLinks: myMax, Caps: capPortTags | capL3Quiet | l3DupCap() | mconnCap() | quietCap(), Ext: infoExt(), Ports: t.MappedPorts()}
	if cfg.LB {
		pi.Caps |= capLB
	}
	if cfg.Advert != nil {
		pi.Caps |= capLBAdvert
	}
	if t.Default != "" {
		pi.Flags |= flagDefault
	}
	pi.Flags |= closeHSFlag()
	return pi
}

// RunKharej accepts links and serves their streams until ctx ends.
func RunKharej(ctx context.Context, cfg KharejConfig) error {
	logf := cfg.Log
	if logf == nil {
		logf = func(string, ...any) {}
	}
	setGuardLog(cfg.Log)
	var l3 *l3Set
	if cfg.TUN != nil {
		l3 = newL3Set()
		go l3.pumpTun(ctx, cfg.TUN)
		go l3.logDrops(ctx, logf)
	}
	if cfg.peers == nil {
		cfg.peers = &linkPeers{}
	}
	if cfg.noRoute == nil {
		cfg.noRoute = &noRouteLog{}
	}
	if cfg.traffic == nil {
		cfg.traffic = newExitTraffic(cfg.peers)
		go cfg.traffic.run(ctx.Done())
	}
	if queueCtlOn {
		go exitQueue.run(ctx, logf)
	}
	if cfg.RevDial != nil || cfg.RevLB != nil {
		return runKharejReverse(ctx, cfg, l3, logf)
	}
	var links atomic.Int32
	var view *pathView
	if cfg.LB {
		view = newPathView(true)
	}
	if cfg.OnStart != nil {
		cfg.OnStart(func() PoolStats {
			// the edge's ceiling and user ports as reported by the links up NOW
			st := PoolStats{Links: int(links.Load()), Phase: "listening", PeerMax: cfg.peers.max(), Routes: cfg.peers.edgeRoutes()}
			cfg.traffic.fill(&st) // this exit's own user connections and throughput
			if view != nil {      // lb on: a row per address the edge reaches us on
				ms := cfg.peers.meters()
				var mine []LBAdvertIP
				if cfg.Advert != nil {
					mine = cfg.Advert.Mine()
				}
				st.Paths, st.PathTotal = view.build(listenDefs(mine, len(ms)), meterLinks(ms, func(m *linkMeter) string { return listenKey(m.path) }))
				st.PathSide = "listen"
			}
			return st
		})
	}
	go func() { <-ctx.Done(); cfg.Listener.Close() }()
	var bo acceptBackoff
	for {
		conn, err := cfg.Listener.Accept()
		if err != nil {
			if !bo.wait(ctx, err, logf, "link listener") {
				return nil
			}
			continue
		}
		bo.ok()
		go cfg.Server.Handle(ctx, conn, func(car *tlscarrier.Carrier) {
			mtr := &linkMeter{tcp: tcpConnOf(car)} // download-side counters, reported over kindStats
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
			cfg.peers.add(mtr)
			defer exitQueue.track(car)()
			defer cfg.peers.remove(mtr)
			defer stripeTrack(mtr)() // HS2_MCONN_STRIPE's gate (mconn_stripe_scav.go)
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
	case kindCtrl, kindPool, kindStats, kindInfo, kindLBAdvert:
		st.SetAutoWindowBackground() // no light neighbour (HS2_SMUX_AUTOWIN, smux_autowin.go)
	}
	switch kind[0] {
	case kindCtrl:
		serveControl(ctx, st, car, mtr)
	case kindPool:
		servePoolCtl(ctx, st, pool)
	case kindStats:
		serveStats(ctx, st, car, mtr)
	case kindInfo:
		myMax := cfg.MaxLinks
		if pool != nil {
			myMax = pool.max // what the reverse exit actually clamps to
		}
		serveInfo(st, cfg.info(myMax), func(pi peerInfo) {
			if mtr != nil { // per link: read back only while this link is up
				mtr.peerMax.Store(uint32(pi.MaxLinks))
				mtr.peerInfo.Store(&pi)
			}
		})
	case kindLBAdvert:
		if cfg.Advert == nil {
			st.Close()
			return
		}
		cfg.Advert.serve(st)
	case kindTCP, kindTCPPort, kindUDP, kindUDPPort:
		udp := kind[0] == kindUDP || kind[0] == kindUDPPort
		port := 0
		if kind[0] == kindTCPPort || kind[0] == kindUDPPort {
			var b [2]byte
			st.SetReadDeadline(time.Now().Add(kindTimeout))
			if _, err := io.ReadFull(st, b[:]); err != nil {
				st.Close()
				return
			}
			st.SetReadDeadline(time.Time{})
			port = int(b[0])<<8 | int(b[1])
		}
		target, ok := cfg.routeTable().Target(port)
		if !ok {
			proto := "tcp"
			if udp {
				proto = "udp"
			}
			cfg.noRoute.note(port, proto, cfg.Log)
			st.Close()
			return
		}
		if udp {
			up, err := net.Dial("udp", target)
			if err != nil {
				st.Close()
				return
			}
			c := cfg.traffic.open(mtr)
			defer cfg.traffic.done(c)
			relayUDPConn(exitCountedStream{st, c}, up)
			return
		}
		up, err := dialExitTCP(target)
		if err != nil {
			st.Close()
			return
		}
		var g *sessGuard
		if mtr != nil {
			g = mtr.guard
		}
		c := cfg.traffic.open(mtr)
		defer cfg.traffic.done(c)
		relayStream(up, exitCountedStream{st, c}, g)
	case kindMConn:
		serveMConnSub(ctx, st, cfg, mtr)
	case kindL3:
		if l3 == nil {
			st.Close() // this side runs without a TUN
			return
		}
		if mtr != nil {
			// Not a user: the TUN rides every link, so counting it kept
			// every reverse link busy and the LB rebalance never moved one.
			mtr.l3s.Add(1)
			defer mtr.l3s.Add(-1)
		}
		pl := newStreamL3LinkQ(st, peerL3Quiet(mtr), sessReadsOf(mtr), quietOf(mtr))
		pl.heavy = heavyOf(mtr)
		pl.stripe = stripeNOf(mtr) // HS2_MCONN_STRIPE (l3_lane.go)
		pl.dupOK = peerL3Dup(mtr)
		if mtr != nil {
			pl.path = mtr.path
		}
		l3.add(pl)
		l3.serveLink(ctx, pl, cfg.TUN)
		l3.remove(pl)
	default:
		st.Close()
	}
}
