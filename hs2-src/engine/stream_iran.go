package engine

import (
	"context"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
	"github.com/xtaci/smux"
)

// IranConfig configures the Iran (edge) side of stream mode: the side users
// connect to. In the DIRECT direction it dials the link pool to the kharej; in
// the REVERSE direction it instead LISTENS and accepts links the kharej dials
// in (RevServer/RevListener set), while still originating the user streams.
type IranConfig struct {
	Dialer   LinkDialer
	Min, Max int // link pool bounds (tls mode: 1, 1)
	PerLink  int // concurrently active flows per link the pool sizes for
	ListenIP string
	Ports    []string
	UDP      bool      // also forward UDP on Ports
	TUN      tunWriter // non-nil: carry hs0 packets as a side channel
	Log      func(string, ...any)

	// Reverse edge: accept links from the kharej instead of dialing. When set,
	// Dialer/Min/Max are ignored and the pool grows/shrinks with what the peer
	// dials in.
	RevServer   *tlscarrier.Server
	RevListener net.Listener

	// DrainIdle: a connection on a retiring link that has moved nothing for
	// this long is closed so the link can finish. 0 = the default (310 s, above
	// xray's 300 s connIdle); negative = never.
	DrainIdle time.Duration

	// WarmLinks (> 0): come up at this many links instead of warmStartLinks —
	// the target the pool had before a restart (the caller keeps it across
	// restarts), so users reconnecting after a restart under load spread over
	// as many links as they had.
	WarmLinks int

	// OnStart, if set, is called once with a function that returns a live
	// snapshot of the link pattern, so the caller can publish it for monitoring.
	OnStart func(StatsFn)

	// ProxyPorts: the user ports (of Ports) behind a proxy that sends a PROXY
	// protocol header (proxyproto.go), believed only from ProxyFrom (nil:
	// loopback). Their users are told apart by the address in the header.
	ProxyPorts []int
	ProxyFrom  []netip.Prefix
}

// StatsFn returns a live snapshot of the link pattern.
type StatsFn func() PoolStats

// RunIran brings up the link pool and the user-facing listeners and serves
// until ctx ends. Listeners never depend on a link being up: a user who
// arrives during a reconnect waits briefly for one.
func RunIran(ctx context.Context, cfg IranConfig) error {
	logf := cfg.Log
	if logf == nil {
		logf = func(string, ...any) {}
	}
	setGuardLog(cfg.Log)
	setWcoreLog(cfg.Log)
	go logWire(ctx, logf)
	reverse := cfg.RevServer != nil
	var lm *LinkManager
	if reverse {
		// The reverse edge cannot dial, but it still runs the full autopilot over
		// the same [min,max] envelope: it decides the link count from the users
		// and throughput it sees and sends it to the exit (which dials) over the
		// pool-control channel.
		lm = NewLinkManager(nil, cfg.Min, cfg.Max, cfg.PerLink, logf)
		lm.accept = true
	} else {
		lm = NewLinkManager(cfg.Dialer, cfg.Min, cfg.Max, cfg.PerLink, logf)
	}
	notePoolMax(lm.max) // the QUIET links' congestion control by the pool's size (quietcc.go)
	if cfg.DrainIdle != 0 {
		lm.SetDrainIdle(cfg.DrainIdle)
	}
	// β2: QUIET links next to the pool's (quiet.go); they count toward
	// max_links, so the ceiling the exit is told holds them too
	quiet := lm.enableQuiet()
	lm.SetWarm(cfg.WarmLinks)
	// The status shows what the exit reports over kindInfo about its routing
	// (routes.go); user connections do not wait for it (userStreamHeader).
	lm.showRoutes = true
	mine := peerInfo{MaxLinks: lm.max, Caps: capPortTags | capL3Quiet}
	if quiet != nil {
		mine.MaxLinks = QuietTotal(lm.max)
		if reverse {
			cfg.RevServer.OfferCaps = quiet.offer
		}
	}
	for _, p := range cfg.Ports {
		if n, err := strconv.Atoi(p); err == nil && n > 0 && n <= 65535 {
			mine.Ports = append(mine.Ports, n)
		}
	}
	if cfg.UDP {
		mine.Flags |= flagUDP
	}
	var l3 *l3Set
	if cfg.TUN != nil {
		l3 = &l3Set{}
		go l3.pumpTun(ctx, cfg.TUN)
		go l3.logDrops(ctx, logf)
	}
	// Every new link gets a control channel (health feedback) and a stats
	// channel (the exit's download-side pressure), each in its own goroutine,
	// and, in TUN mode, its L3 side-channel stream. On the reverse edge each
	// link also carries the pool-control stream that tells the exit the desired
	// link count.
	lm.OnLink = func(l Link) {
		go openControl(ctx, l, logf)
		go openOWD(ctx, l)         // a BULK link: α's one-way delays (owd.go)
		go openHint(ctx, l, quiet) // a QUIET link: the exit's move hints (hint.go)
		go openStats(ctx, l, logf)
		go func() { // tell the exit our ceiling and user ports, learn its own
			mtr := linkMeterOf(l)
			for i := 0; ; i++ {
				if i > 0 && !sleepCtx(ctx, infoSlowRetry) {
					return
				}
				openInfo(ctx, l, mine)
				if mtr == nil || mtr.infoRefused.Load() || !l.Alive() || ctx.Err() != nil {
					return
				}
				if mtr.peerInfo.Load() != nil {
					return
				}
				// No answer in all of openInfo's tries (a congested link): keep
				// asking, slowly, so the status never lacks it for the link's
				// whole life.
			}
		}()
		if reverse {
			go openPoolCtl(ctx, l, lm, logf, func() { lm.markPoolRefused(l) })
		}
		if l3 != nil {
			openL3(ctx, l, l3, cfg.TUN, logf)
		}
	}
	if cfg.OnStart != nil {
		cfg.OnStart(lm.Stats)
	}
	go lm.Run(ctx)
	if reverse {
		go acceptReverseLinks(ctx, cfg.RevListener, cfg.RevServer, lm, logf)
	}

	var refusals proxyRefusals // what the proxy ports close, logged once a minute each
	for _, p := range cfg.Ports {
		bind := net.JoinHostPort(cfg.ListenIP, p)
		port, _ := strconv.Atoi(p) // the tag the exit routes by (0: none)
		proxied := port > 0 && slices.Contains(cfg.ProxyPorts, port)
		ln, err := ListenReuse(bind)
		if err != nil {
			return err
		}
		go func() { <-ctx.Done(); ln.Close() }()
		go func() {
			// A user port must not die on a transient accept error (out of
			// file descriptors under a connection flood): back off and retry.
			var bo acceptBackoff
			for {
				c, err := ln.Accept()
				if err != nil {
					if !bo.wait(ctx, err, logf, "user port "+p) {
						return
					}
					continue
				}
				bo.ok()
				if proxied {
					go serveProxiedTCP(ctx, c, lm, port, cfg.ProxyFrom, &refusals, logf)
				} else {
					go serveUserTCP(ctx, c, lm, port)
				}
			}
		}()
		msg := "tcp"
		if cfg.UDP {
			if pc, err := net.ListenPacket("udp", bind); err == nil {
				go func() { <-ctx.Done(); pc.Close() }()
				go serveUserUDP(ctx, pc, lm, port, logf)
				msg = "tcp+udp"
			} else {
				// UDP on the user ports is an addition: a port whose UDP
				// another program holds must not take the tunnel down
				// (hs2 run exits on RunIran's error, and systemd restarted
				// it every 3 s with the TCP port never serving).
				logf("user port %s: UDP is NOT forwarded: %v — another program holds UDP %s on this server (ss -lunp shows which); TCP on it works, and UDP is forwarded once the port is free", p, err, p)
				go retryUserUDP(ctx, bind, p, lm, port, logf)
				msg = "tcp; udp waits for the port"
			}
		}
		if proxied {
			msg += "; tcp behind a proxy: a PROXY protocol header first"
		}
		logf("user port %s open (%s), carried over the link pool", p, msg)
	}
	<-ctx.Done()
	return nil
}

// userUDPRetry is how long the edge waits before it tries a user port's UDP
// socket again (retryUserUDP), doubling up to userUDPRetryMax.
var (
	userUDPRetry    = time.Second
	userUDPRetryMax = 30 * time.Second
)

// retryUserUDP opens the user port's UDP socket once another program lets it
// go, and serves it from then on.
func retryUserUDP(ctx context.Context, bind, p string, lm *LinkManager, port int, logf func(string, ...any)) {
	wait := userUDPRetry
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		pc, err := net.ListenPacket("udp", bind)
		if err != nil {
			wait = min(2*wait, userUDPRetryMax)
			continue
		}
		go func() { <-ctx.Done(); pc.Close() }()
		logf("user port %s: UDP forwarded now (the port is free)", p)
		serveUserUDP(ctx, pc, lm, port, logf)
		return
	}
}

// pickWait returns a link, waiting up to a few seconds for one to come up.
// hold: the connection may also wait in the refill hold (refill.go) for a link
// with room while the pool refills after a start or a total loss — at most
// refillHoldMax, and the hold never refuses it. UDP flows pass false (their
// port's read loop is shared). tried: links the connection was tried on
// already (openStream), not picked again.
func pickWait(ctx context.Context, lm *LinkManager, hold bool, tried ...Link) (Link, func(), bool) {
	for i := 0; i < 40; i++ {
		if !hold {
			if l, rel, ok := lm.pickExcept(tried, false); ok {
				return l, rel, true
			}
		} else if l, rel, ok, w := lm.pickHeld(tried...); ok {
			return l, rel, true
		} else if w != nil {
			select {
			case ml := <-w.ch:
				if ml != nil {
					return ml.link, lm.releaseFor(ml), true
				}
				continue // the hold ended with no link up: the ordinary wait
			case <-ctx.Done():
				lm.cancelHeld(w)
				return nil, nil, false
			}
		}
		select {
		case <-ctx.Done():
			return nil, nil, false
		case <-time.After(150 * time.Millisecond):
		}
	}
	return nil, nil, false
}

// userStreamHeader is what opens a user stream: the kind tagged with the user
// port (0: none known), so the exit routes it by the table it has now — 2
// bytes once per stream, inside TLS. Wire v3 has only this form (§4.2): the
// one-byte kindTCP / kindUDP of v2 is gone with the v2 peers that needed it.
func userStreamHeader(udp bool, port int) [3]byte {
	k := kindTCPPort
	if udp {
		k = kindUDPPort
	}
	if port < 0 || port > 65535 {
		port = 0
	}
	return [3]byte{k, byte(port >> 8), byte(port)}
}

// userTagKey carries a user connection's tag (UserTag) in its context down to
// the SYN of every stream opened for it: the first, and one a rescue opens.
type userTagKey struct{}

// withUserTag is ctx carrying the user tag of the client at addr.
func withUserTag(ctx context.Context, addr net.Addr) context.Context {
	return context.WithValue(ctx, userTagKey{}, connUserTag(addr))
}

// userTagOf is the user tag ctx carries (0: none).
func userTagOf(ctx context.Context) uint32 {
	tag, _ := ctx.Value(userTagKey{}).(uint32)
	return tag
}

// connUserTag is the user tag of the client at addr (0: not an IP address).
func connUserTag(addr net.Addr) uint32 {
	var ip netip.Addr
	switch a := addr.(type) {
	case *net.TCPAddr:
		ip, _ = netip.AddrFromSlice(a.IP)
	case *net.UDPAddr:
		ip, _ = netip.AddrFromSlice(a.IP)
	case nil:
	default:
		ap, _ := netip.ParseAddrPort(a.String())
		ip = ap.Addr()
	}
	if !ip.IsValid() {
		return 0
	}
	return UserTag(ip, classV6Prefix)
}

// synOpener is a link that opens a user stream with the SYN payload it is
// given (mtcpLink); any other (a test fake) opens it with tag 0.
type synOpener interface {
	OpenStreamSyn(p [smux.SynPayloadLen]byte) (stream, error)
}

// openUserStream opens a user stream on link with the user's tag in its SYN.
// The class hint is unknown: the edge has no guess before the first byte.
// On a QUIET link it is a group stream (quiet.go), which may move once to a
// BULK link; rel is its slot on link.
func openUserStream(lm *LinkManager, link Link, tag uint32, rel func()) (stream, error) {
	if ml, ok := link.(*mtcpLink); ok && ml.quiet && ml.sess.HasSched() {
		var q *quietSet
		if lm != nil {
			q = lm.quiet
		}
		return openGroupStream(q, ml, tag, rel)
	}
	if so, ok := link.(synOpener); ok {
		return so.OpenStreamSyn(smux.EncodeSyn(tag, 0))
	}
	return link.OpenStream()
}

// openTimeout: how long one try at opening a user stream waits for its link —
// the stream's SYN, then its header, each queued behind the link's writer —
// before the link counts as slow (noteOpenSlow) and, if the SYN itself has not
// gone out, the connection is tried on another link as well. openGiveUp bounds
// the whole open, as smux's own open timeout (30 s) bounded each try before.
// (Variables only so the tests can shorten them.)
var (
	openTimeout = 3 * time.Second
	openGiveUp  = 30 * time.Second
)

// openTries: the links one connection is tried on at most.
const openTries = 3

// openStream opens a user stream for a connection that came in on the user
// port `port` (0: unknown — the exit's default panel), with the user tag ctx
// carries (withUserTag) in its SYN.
//
// A link can die between Pick and OpenStream, or be dying already: one
// black-holed under load holds a new stream's SYN behind a writer that never
// moves (smux gives up after 30 s), and the sampler takes it out of use only
// seconds later. So a try that fails is retried on another link, and one whose
// SYN has not gone out within openTimeout is left running while the
// connection is tried on another link too: the first to open takes it, the
// others are closed as they open — a slow but working path keeps its user (on
// a throttled pool every link may take that long). A try whose SYN went out
// but whose header still waits (a long upload queue on a link that moves) is
// not doubled: the exit would dial the panel for each copy whose header goes
// out (in a throttled-pool test, 9 per 100 new users). Either way the link
// that kept it waiting is lagging for a while (noteOpenSlow), so the
// connections after it go elsewhere at once.
func openStream(ctx context.Context, lm *LinkManager, udp bool, port int) (stream, func(), bool) {
	st, _, release, ok := openStreamOn(ctx, lm, udp, port)
	return st, release, ok
}

// openStreamOn is openStream that also returns the link the stream opened on,
// and never tries the links in avoid (a connection moving off its link,
// rescue.go).
func openStreamOn(ctx context.Context, lm *LinkManager, udp bool, port int, avoid ...Link) (stream, Link, func(), bool) {
	giveUp := time.NewTimer(openGiveUp)
	defer giveUp.Stop()
	done := make(chan *openTry, openTries) // never blocks a try
	tried := slices.Clone(avoid)
	maxTried := len(avoid) + openTries
	var live []*openTry
	defer func() {
		for _, t := range live {
			t.abandon()
		}
	}()
	hdr, tag := userStreamHeader(udp, port), userTagOf(ctx)
	start := func(link Link, release func()) {
		tried = append(tried, link)
		live = append(live, startOpen(lm, link, release, hdr[:], tag, done))
	}
	for {
		if len(live) == 0 {
			if len(tried) >= maxTried {
				return nil, nil, nil, false
			}
			link, release, ok := pickOpen(ctx, lm, !udp, tried...)
			if !ok {
				return nil, nil, nil, false
			}
			start(link, release)
		}
		wait := openTimeout // the newest try was found slow: look for a link again
		if newest := live[len(live)-1]; !newest.slow {
			wait -= time.Since(newest.at)
		}
		slow := time.NewTimer(wait)
		select {
		case t := <-done:
			live = slices.DeleteFunc(live, func(x *openTry) bool { return x == t })
			if t.err == nil {
				slow.Stop()
				return t.st, t.link, t.release, true
			}
			t.release() // failed (the link died): the next try goes elsewhere
			if len(live) > 0 && len(tried) < maxTried && !anyOpened(live) {
				// an older try still waits for its SYN: the next link now,
				// not a whole openTimeout later
				if link, release, ok := pickOpenExcept(lm, tried, !udp, tag); ok {
					start(link, release)
				}
			}
		case <-slow.C:
			for _, t := range live {
				if d := time.Since(t.at); !t.slow && d >= openTimeout {
					t.slow = true
					lm.noteOpenSlow(t.link, d)
				}
			}
			if len(tried) < maxTried && !anyOpened(live) {
				if link, release, ok := pickOpenExcept(lm, tried, !udp, tag); ok {
					start(link, release)
				}
			}
		case <-giveUp.C:
			slow.Stop()
			return nil, nil, nil, false
		case <-ctx.Done():
			slow.Stop()
			return nil, nil, nil, false
		}
		slow.Stop()
	}
}

// anyOpened reports whether one of the tries has its stream open (its SYN went
// out): its header is on the way, and a try on one more link would have the
// exit dial the panel twice.
func anyOpened(live []*openTry) bool {
	return slices.ContainsFunc(live, func(t *openTry) bool { return t.opened.Load() })
}

// openTry is one try at opening a user stream on one link (openStream).
type openTry struct {
	link    Link
	release func() // the link's slot for this connection
	at      time.Time
	slow    bool        // it passed openTimeout (openStream's own)
	opened  atomic.Bool // its stream is open (the SYN went out); the header may wait
	st      stream
	err     error
	// claim: 0 running; 1 done, its result sent to openStream; 2 abandoned —
	// the connection opened elsewhere or gave up, and the try closes what it
	// opens.
	claim atomic.Int32
}

// startOpen opens a stream on link, with the user tag in its SYN, and writes
// its header in a goroutine of its own; the result comes on done unless the
// try is abandoned first.
func startOpen(lm *LinkManager, link Link, release func(), hdr []byte, tag uint32, done chan<- *openTry) *openTry {
	t := &openTry{link: link, release: release, at: time.Now()}
	limit := t.at.Add(openGiveUp)
	go func() {
		st, err := openUserStream(lm, link, tag, release)
		if err == nil {
			t.opened.Store(true)
			if t.claim.Load() == 2 {
				st.Close() // abandoned before it opened: no header, no panel dial
				return
			}
			// The header waits for the link's writer like any frame, and
			// without a deadline smux waits for as long as the session lives.
			wd, _ := st.(interface{ SetWriteDeadline(time.Time) error })
			if wd != nil {
				wd.SetWriteDeadline(limit)
			}
			sent := ctrlNow() // before the write: an answer can beat the stamp after it
			if _, err = st.Write(hdr); err == nil && wd != nil {
				wd.SetWriteDeadline(time.Time{})
			}
			if mtr := linkMeterOf(link); err == nil && mtr != nil {
				// Its answer is due (lagging): unless an older open still
				// waits for one, this one is the oldest.
				for {
					w := mtr.openWait.Load()
					if w != 0 && mtr.rxAt.Load() < w || mtr.openWait.CompareAndSwap(w, sent) {
						break
					}
				}
			}
		}
		var failed stream
		if err != nil && st != nil {
			st, failed = nil, st
		}
		t.st, t.err = st, err
		if t.claim.CompareAndSwap(0, 1) {
			done <- t
		} else if st != nil {
			st.Close() // abandoned: the connection is on another link
		}
		if failed != nil {
			failed.Close()
		}
	}()
	return t
}

// abandon gives up on a try openStream no longer waits for: its link's slot is
// released now, and the stream it opens — or opened, its result unread — is
// closed (in the background: a FIN waits for the link's writer too).
func (t *openTry) abandon() {
	if !t.claim.CompareAndSwap(0, 2) && t.st != nil {
		go t.st.Close()
	}
	t.release()
}

// serveProxiedTCP is serveUserTCP on a proxy port: the connection is the
// proxy's, and the user is the one its PROXY header names (proxyproto.go).
// Without a valid header, or from outside allow, it is closed.
func serveProxiedTCP(ctx context.Context, c net.Conn, lm *LinkManager, port int, allow []netip.Prefix,
	refusals *proxyRefusals, logf func(string, ...any)) {
	user, ok, why := acceptProxied(c, allow)
	if !ok {
		refusals.note(port, why, logf)
		c.Close()
		return
	}
	serveUserTCP(ctx, user, lm, port)
}

func serveUserTCP(ctx context.Context, user net.Conn, lm *LinkManager, port int) {
	ctx = withUserTag(ctx, user.RemoteAddr())
	st, link, release, ok := openStreamOn(ctx, lm, false, port)
	if !ok {
		user.Close()
		return
	}
	relayUser(ctx, user, lm, port, st, link, release)
}

// relayUser relays a user connection over the stream st just opened for it
// on link (release: the connection's slot there), moving it to another link
// if its link dies before the exit answers and what its app sent can be sent
// again (rescue.go).
func relayUser(ctx context.Context, user net.Conn, lm *LinkManager, port int, st stream, link Link, release func()) {
	rs := newRescuable(ctx, lm, port, st, link, release)
	defer rs.release()
	relayStream(user, rs, guardOf(rs))
}

// udpFlow is one client address on a UDP user port: its datagrams wait in q
// for the flow's own writer, so the port's one read loop never waits on a
// link — before, it wrote each datagram itself, and a user whose link was
// throttled or busy (a DPI-slowed link, or an upload faster than the link)
// held up every other user of the port: in a test one user uploading 4.8
// Mbit/s over a 2 Mbit/s link left the others 34% of their datagrams, 430 ms
// late. A full queue drops the datagram, as a full UDP socket would.
type udpFlow struct {
	q     chan udpDgram
	qb    atomic.Int64 // bytes waiting in q
	last  time.Time    // under the port's mu
	st    stream       // nil until the flow's stream is open (under mu)
	stop  chan struct{}
	ended bool // stop is closed (under mu)
	// dst: the address the user sent to, on a port bound to every address
	// (replyPin): its replies leave from it (under mu)
	dst netip.Addr
	// tag: its user's (connUserTag), for UDPFlowsPerUser
	tag uint32
}

// UDPFlowsMax (HS2_TUNE_UDP_FLOWS; 0: none) is the most UDP flows a user
// port keeps at once, UDPFlowsPerUser (HS2_TUNE_UDP_USER_FLOWS; 0: none)
// the most of one user (its tag: the IPv4 address, or the IPv6 /64). A
// datagram that would start a flow past either is dropped. A flow is a
// client address and port: each new source port of one client made a
// flow, two goroutines and a 64 KiB buffer here, a stream on a link, and a
// UDP socket and two more 64 KiB buffers at the exit, each kept for
// udpIdle (2 min) — any client of an open UDP port could fill both
// servers' memory with datagrams from changing source ports, no spoofing
// needed (the R1 red team's second pass, item 7). A game or a call uses
// one to a few flows; a home behind one address a few dozen.
var (
	UDPFlowsMax     = newTuneInt(4096)
	UDPFlowsPerUser = newTuneInt(256)
)

// udpCapDrops counts the datagrams dropped for the flow caps.
var udpCapDrops atomic.Int64

const (
	udpFlowQueue = 256       // datagrams waiting for a flow's link
	udpFlowBytes = 512 << 10 // ... and their bytes at most
)

// udpDgram is a user's datagram waiting in its flow's queue, with when it
// came (ctrlNow).
type udpDgram struct {
	p  []byte
	at int64
}

// UDPLateMs (HS2_TUNE_UDP_LATE, ms; 0: none, as before): a user's datagram
// that waited longer than this in its flow's queue at the edge is dropped,
// not sent (R1 round 3, decision T20 and the UDP report's item 3). A game
// or a call takes a late datagram as lost anyway, and it would hold the
// fresh ones behind it on the flow's ordered stream: the queue holds up to
// 256 datagrams, 4 s of a 64 Hz game, with no age limit before.
var UDPLateMs = 150

// udpLateDrops counts the datagrams dropped for their age.
var udpLateDrops atomic.Int64

// udpLate says whether a datagram that came at at is too old at now.
func udpLate(at, now int64) bool {
	return UDPLateMs > 0 && now-at > int64(UDPLateMs)*int64(time.Millisecond)
}

// end stops the flow (idle, or its stream failed). Caller holds the port's mu.
func (f *udpFlow) end() {
	if !f.ended {
		f.ended = true
		close(f.stop)
		if f.st != nil {
			f.st.Close()
		}
	}
}

// serveUserUDP gives each client address its own stream (so one flow's loss
// or backlog never stalls another) and closes flows after udpIdle.
func serveUserUDP(ctx context.Context, pc net.PacketConn, lm *LinkManager, port int, logf func(string, ...any)) {
	var mu sync.Mutex
	flows := map[string]*udpFlow{}
	perUser := map[uint32]int{} // flows by user tag (UDPFlowsPerUser)
	capped := int64(0)          // datagrams dropped for the caps (under mu), since the line before
	// forget drops flow k from flows (caller holds mu)
	forget := func(k string, f *udpFlow) {
		delete(flows, k)
		if perUser[f.tag]--; perUser[f.tag] <= 0 {
			delete(perUser, f.tag)
		}
	}
	pin := pinReplies(pc) // a port on every address: answer from the address used
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			mu.Lock()
			for k, f := range flows {
				if time.Since(f.last) > udpIdle {
					f.end()
					forget(k, f)
				}
			}
			n, nf := capped, len(flows)
			capped = 0
			mu.Unlock()
			if n > 0 {
				logf("udp port %d: %d datagrams that would start a flow dropped in the last 30 s: %d flows (at most %d, %d of one user: HS2_TUNE_UDP_FLOWS, HS2_TUNE_UDP_USER_FLOWS)",
					port, n, nf, UDPFlowsMax.Get(), UDPFlowsPerUser.Get())
			}
		}
	}()
	// run is one flow: it opens the flow's stream (the read loop does not
	// wait for a link), then writes what the client sends while a reader
	// returns what comes back.
	run := func(key string, addr net.Addr, f *udpFlow) {
		gone := func() {
			mu.Lock()
			f.end()
			if flows[key] == f {
				forget(key, f)
			}
			mu.Unlock()
		}
		st, release, ok := openStream(withUserTag(ctx, addr), lm, true, port)
		if !ok {
			gone()
			return
		}
		defer release()
		mu.Lock()
		if f.ended {
			mu.Unlock()
			st.Close()
			return
		}
		f.st = st
		mu.Unlock()
		done := make(chan struct{})
		go func() {
			defer close(done)
			rb := make([]byte, maxDatagram)
			for {
				p, err := readDatagram(st, rb)
				if err != nil {
					break
				}
				if ua, ok := addr.(*net.UDPAddr); pin != nil && ok {
					mu.Lock()
					src := f.dst
					mu.Unlock()
					pin.writeTo(p, ua, src)
				} else {
					pc.WriteTo(p, addr)
				}
				mu.Lock()
				f.last = time.Now()
				mu.Unlock()
			}
			gone()
		}()
		for {
			select {
			case d := <-f.q:
				f.qb.Add(-int64(len(d.p)))
				if udpLate(d.at, ctrlNow()) {
					udpLateDrops.Add(1) // stale: the game takes it as lost anyway
					continue
				}
				if writeDatagram(st, d.p) != nil {
					gone()
				}
				continue
			case <-f.stop:
			case <-ctx.Done():
				gone()
			}
			break
		}
		<-done // the stream is closed: the reader ends, then the slot is released
	}
	buf, oob := make([]byte, maxDatagram), make([]byte, replyPinOOB)
	for {
		var (
			n    int
			addr net.Addr
			dst  netip.Addr
			err  error
		)
		if pin != nil {
			var ua *net.UDPAddr
			n, ua, dst, err = pin.readFrom(buf, oob)
			addr = ua
		} else {
			n, addr, err = pc.ReadFrom(buf)
		}
		if err != nil {
			mu.Lock()
			for k, f := range flows {
				f.end()
				forget(k, f)
			}
			mu.Unlock()
			return
		}
		key := addr.String()
		mu.Lock()
		f := flows[key]
		if f == nil {
			tag := connUserTag(addr)
			if m := UDPFlowsMax.Get(); m > 0 && len(flows) >= m {
				capped++
				udpCapDrops.Add(1)
				mu.Unlock()
				continue
			}
			if m := UDPFlowsPerUser.Get(); m > 0 && perUser[tag] >= m {
				capped++
				udpCapDrops.Add(1)
				mu.Unlock()
				continue
			}
			f = &udpFlow{q: make(chan udpDgram, udpFlowQueue), stop: make(chan struct{}), tag: tag}
			flows[key] = f
			perUser[tag]++
			go run(key, addr, f)
		}
		f.last = time.Now()
		if dst.IsValid() {
			f.dst = dst
		}
		mu.Unlock()
		if f.qb.Load()+int64(n) > udpFlowBytes {
			continue // the flow's link is behind: drop
		}
		p := append([]byte(nil), buf[:n]...)
		select {
		case f.q <- udpDgram{p: p, at: ctrlNow()}:
			f.qb.Add(int64(n))
		default: // drop
		}
	}
}

// openL3 opens the TUN side-channel stream on a fresh link.
func openL3(ctx context.Context, l Link, set *l3Set, dev tunWriter, logf func(string, ...any)) {
	ro, ok := l.(rawStreamOpener)
	if !ok {
		return
	}
	st, err := ro.OpenRawStream()
	if err != nil {
		return
	}
	if _, err := st.Write([]byte{kindL3}); err != nil {
		st.Close()
		return
	}
	pl := newStreamL3Link(st, peerL3Quiet(linkMeterOf(l)), sessReadsOf(linkMeterOf(l)))
	if ml, ok := l.(*mtcpLink); ok {
		pl.class = l3ClassOf(ml.quiet, ml.bulk) // β2: new flows on QUIET
	}
	set.add(pl)
	go func() {
		set.serveLink(ctx, pl, dev)
		set.remove(pl)
	}()
}
