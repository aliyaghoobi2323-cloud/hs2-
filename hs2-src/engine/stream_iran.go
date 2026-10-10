package engine

import (
	"context"
	"net"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
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
	if cfg.DrainIdle != 0 {
		lm.SetDrainIdle(cfg.DrainIdle)
	}
	lm.SetWarm(cfg.WarmLinks)
	// A link carries user connections only once its kindInfo exchange is over,
	// so each one knows whether the exit routes port tags (routes.go).
	lm.gateInfo = true
	mine := peerInfo{MaxLinks: lm.max, Caps: capPortTags | capL3Quiet}
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
				if v := mtr.peerInfo.Load(); v != nil {
					lm.exitInfo.Store(v) // what links whose own answer is late go by
					return
				}
				// No answer in all of openInfo's tries (a congested link): the
				// link goes by the pool's answer meanwhile; keep asking, slowly,
				// so it never stays without one for its whole life.
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

	for _, p := range cfg.Ports {
		bind := net.JoinHostPort(cfg.ListenIP, p)
		port, _ := strconv.Atoi(p) // the tag the exit routes by (0: none)
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
				go serveUserTCP(ctx, c, lm, port)
			}
		}()
		msg := "tcp"
		if cfg.UDP {
			pc, err := net.ListenPacket("udp", bind)
			if err != nil {
				return err
			}
			go func() { <-ctx.Done(); pc.Close() }()
			go serveUserUDP(ctx, pc, lm, port, logf)
			msg = "tcp+udp"
		}
		logf("user port %s open (%s), carried over the link pool", p, msg)
	}
	<-ctx.Done()
	return nil
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

// userStreamHeader is what opens a user stream on link l: the kind tagged with
// the user port whenever the exit routes tags — it then decides with the table
// it has now (2 bytes once per stream, inside TLS) — else the untagged kind
// every exit understands. What the exit said on THIS link decides; a link
// whose exchange has not been answered yet (slow, still retrying) goes by what
// the pool's other links learned from the exit (pool, may be nil). An exit
// that refused the exchange is never tagged.
func userStreamHeader(l Link, udp bool, port int, pool *peerInfo) []byte {
	if mtr := linkMeterOf(l); mtr != nil && port > 0 && port <= 65535 {
		pi := mtr.peerInfo.Load()
		if pi == nil && !mtr.infoRefused.Load() {
			pi = pool
		}
		if pi.tags() {
			k := kindTCPPort
			if udp {
				k = kindUDPPort
			}
			return []byte{k, byte(port >> 8), byte(port)}
		}
	}
	if udp {
		return []byte{kindUDP}
	}
	return []byte{kindTCP}
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
// port `port` (0: unknown — never tagged).
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
	giveUp := time.NewTimer(openGiveUp)
	defer giveUp.Stop()
	done := make(chan *openTry, openTries) // never blocks a try
	var tried []Link
	var live []*openTry
	defer func() {
		for _, t := range live {
			t.abandon()
		}
	}()
	start := func(link Link, release func()) {
		tried = append(tried, link)
		live = append(live, startOpen(link, release, userStreamHeader(link, udp, port, lm.exitInfo.Load()), done))
	}
	for {
		if len(live) == 0 {
			if len(tried) >= openTries {
				return nil, nil, false
			}
			link, release, ok := pickWait(ctx, lm, !udp, tried...)
			if !ok {
				return nil, nil, false
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
				return t.st, t.release, true
			}
			t.release() // failed (the link died): the next try goes elsewhere
			if len(live) > 0 && len(tried) < openTries && !anyOpened(live) {
				// an older try still waits for its SYN: the next link now,
				// not a whole openTimeout later
				if link, release, ok := lm.pickExcept(tried, !udp); ok {
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
			if len(tried) < openTries && !anyOpened(live) {
				if link, release, ok := lm.pickExcept(tried, !udp); ok {
					start(link, release)
				}
			}
		case <-giveUp.C:
			slow.Stop()
			return nil, nil, false
		case <-ctx.Done():
			slow.Stop()
			return nil, nil, false
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

// startOpen opens a stream on link and writes its header in a goroutine of
// its own; the result comes on done unless the try is abandoned first.
func startOpen(link Link, release func(), hdr []byte, done chan<- *openTry) *openTry {
	t := &openTry{link: link, release: release, at: time.Now()}
	limit := t.at.Add(openGiveUp)
	go func() {
		st, err := link.OpenStream()
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

func serveUserTCP(ctx context.Context, user net.Conn, lm *LinkManager, port int) {
	st, release, ok := openStream(ctx, lm, false, port)
	if !ok {
		user.Close()
		return
	}
	defer release()
	relayStream(user, st, guardOf(st))
}

// udpFlow is one client address on a UDP user port: its datagrams wait in q
// for the flow's own writer, so the port's one read loop never waits on a
// link — before, it wrote each datagram itself, and a user whose link was
// throttled or busy (a DPI-slowed link, or an upload faster than the link)
// held up every other user of the port: in a test one user uploading 4.8
// Mbit/s over a 2 Mbit/s link left the others 34% of their datagrams, 430 ms
// late. A full queue drops the datagram, as a full UDP socket would.
type udpFlow struct {
	q     chan []byte
	qb    atomic.Int64 // bytes waiting in q
	last  time.Time    // under the port's mu
	st    stream       // nil until the flow's stream is open (under mu)
	stop  chan struct{}
	ended bool // stop is closed (under mu)
}

const (
	udpFlowQueue = 256       // datagrams waiting for a flow's link
	udpFlowBytes = 512 << 10 // ... and their bytes at most
)

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
					delete(flows, k)
				}
			}
			mu.Unlock()
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
				delete(flows, key)
			}
			mu.Unlock()
		}
		st, release, ok := openStream(ctx, lm, true, port)
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
				pc.WriteTo(p, addr)
				mu.Lock()
				f.last = time.Now()
				mu.Unlock()
			}
			gone()
		}()
		for {
			select {
			case p := <-f.q:
				f.qb.Add(-int64(len(p)))
				if writeDatagram(st, p) != nil {
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
	buf := make([]byte, maxDatagram)
	for {
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			mu.Lock()
			for k, f := range flows {
				f.end()
				delete(flows, k)
			}
			mu.Unlock()
			return
		}
		key := addr.String()
		mu.Lock()
		f := flows[key]
		if f == nil {
			f = &udpFlow{q: make(chan []byte, udpFlowQueue), stop: make(chan struct{})}
			flows[key] = f
			go run(key, addr, f)
		}
		f.last = time.Now()
		mu.Unlock()
		if f.qb.Load()+int64(n) > udpFlowBytes {
			continue // the flow's link is behind: drop
		}
		p := append([]byte(nil), buf[:n]...)
		select {
		case f.q <- p:
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
	set.add(pl)
	go func() {
		set.serveLink(ctx, pl, dev)
		set.remove(pl)
	}()
}
