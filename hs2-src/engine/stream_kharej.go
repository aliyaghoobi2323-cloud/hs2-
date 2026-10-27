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
	pi := peerInfo{MaxLinks: myMax, Caps: capPortTags | capL3Quiet, Ports: t.MappedPorts()}
	if t.Default != "" {
		pi.Flags |= flagDefault
	}
	return pi
}

// RunKharej accepts links and serves their streams until ctx ends.
func RunKharej(ctx context.Context, cfg KharejConfig) error {
	logf := cfg.Log
	if logf == nil {
		logf = func(string, ...any) {}
	}
	setGuardLog(cfg.Log)
	setWcoreLog(cfg.Log)
	go logWire(ctx, logf)
	var l3 *l3Set
	if cfg.TUN != nil {
		l3 = &l3Set{toUser: true} // its TUN's packets go to the users
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
	if cfg.RevDial != nil {
		return runKharejReverse(ctx, cfg, l3, logf)
	}
	var links atomic.Int32
	if cfg.OnStart != nil {
		cfg.OnStart(func() PoolStats {
			// the edge's ceiling and user ports as reported by the links up NOW
			st := PoolStats{Links: int(links.Load()), Phase: "listening", PeerMax: cfg.peers.max(), Routes: cfg.peers.edgeRoutes()}
			cfg.traffic.fill(&st) // this exit's own user connections and throughput
			wcores.fillStats(&st)
			return st
		})
	}
	go func() { <-ctx.Done(); cfg.Listener.Close() }()
	if cfg.Server != nil && cfg.Server.OfferCaps == nil && !QuietOff() {
		// β2: the edge chooses each link's class; the exit takes it on
		// every link (quiet.go)
		cfg.Server.OfferCaps = func() uint32 { return tlscarrier.RoleCaps }
	}
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
			mtr := &linkMeter{} // download-side counters, reported over kindStats
			sess, why, err := newSessionCaps(car.RawConn(), true, nil, mtr, car.Caps())
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
			defer cfg.peers.remove(mtr)
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
			logf("link down from %s: %s (now %d)%s", conn.RemoteAddr(), sessionEndReason(why, downErr), links.Add(-1), wcoreEndNote(mtr))
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
	// conn: the user connection, the stream or, when its SYN names a group
	// (β2, quiet_exit.go), the group; a group's move stream joins its group
	var conn exitStream = st
	var eg *exitGroup
	if id, role, _ := st.GroupInfo(); id != 0 {
		if role == smux.RoleMove {
			if !exitGroups.attach(st, mtr) {
				st.Close() // no live group: refused (§4.5)
			}
			return
		}
		if eg = exitGroups.add(st, mtr); eg == nil {
			st.Close()
			return
		}
		defer exitGroups.remove(eg)
		conn = eg
	}
	rd, _ := conn.(interface{ SetReadDeadline(time.Time) error })
	var kind [1]byte
	rd.SetReadDeadline(time.Now().Add(kindTimeout))
	if _, err := io.ReadFull(conn, kind[:]); err != nil {
		conn.Close()
		return
	}
	rd.SetReadDeadline(time.Time{})
	if eg != nil && kind[0] != kindTCPPort && kind[0] != kindUDPPort {
		conn.Close() // only user connections are groups
		return
	}
	switch kind[0] {
	case kindCtrl:
		serveControl(ctx, st, car)
	case kindOWD:
		if mtr == nil {
			st.Close()
			break
		}
		if TuneOWDQuiet != 0 && car != nil {
			if q, _ := classOf(car.Caps()); q {
				quietOWD.Store(&mtr.owd) // version 5: the BULK links read it
			}
		}
		owdLoop(ctx, st, &mtr.owd) // α's one-way delays (owd.go)
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
	case kindTCPPort, kindUDPPort: // wire v3: the one-byte kindTCP / kindUDP are refused (default)
		udp := kind[0] == kindUDPPort
		var b [2]byte
		rd.SetReadDeadline(time.Now().Add(kindTimeout))
		if _, err := io.ReadFull(conn, b[:]); err != nil {
			conn.Close()
			return
		}
		rd.SetReadDeadline(time.Time{})
		port := int(b[0])<<8 | int(b[1])
		// The user's tag comes from the SYN the edge opened the stream with
		// (§3.1): the exit never hashes an address of its own.
		tag, _ := st.Syn()
		target, ok := cfg.routeTable().Target(port)
		if !ok {
			proto := "tcp"
			if udp {
				proto = "udp"
			}
			cfg.noRoute.note(port, proto, cfg.Log)
			conn.Close()
			return
		}
		if udp {
			up, err := net.Dial("udp", target)
			if err != nil {
				conn.Close()
				return
			}
			c := cfg.traffic.open(tag)
			defer cfg.traffic.done(c)
			relayUDPConn(exitCountedStream{conn, c}, up)
			return
		}
		up, err := net.DialTimeout("tcp", target, 5*time.Second)
		if err != nil {
			conn.Close()
			return
		}
		var g *sessGuard
		if mtr != nil {
			g = mtr.guard
		}
		c := cfg.traffic.open(tag)
		defer cfg.traffic.done(c)
		relayStream(up, exitCountedStream{conn, c}, g)
	case kindL3:
		if l3 == nil {
			st.Close() // this side runs without a TUN
			return
		}
		pl := newStreamL3Link(st, peerL3Quiet(mtr), sessReadsOf(mtr))
		if car != nil {
			pl.class = l3ClassOf(classOf(car.Caps())) // β2: replies by arrival
		}
		l3.add(pl)
		l3.serveLink(ctx, pl, cfg.TUN)
		l3.remove(pl)
	default:
		st.Close()
	}
}
