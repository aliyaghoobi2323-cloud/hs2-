package engine

import (
	"context"
	"net"
	"strconv"
	"sync"
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
// port's read loop is shared).
func pickWait(ctx context.Context, lm *LinkManager, hold bool) (Link, func(), bool) {
	for i := 0; i < 40; i++ {
		if !hold {
			if l, rel, ok := lm.Pick(); ok {
				return l, rel, true
			}
		} else if l, rel, ok, w := lm.pickHeld(); ok {
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

// openStream opens a user stream for a connection that came in on the user
// port `port` (0: unknown — never tagged).
func openStream(ctx context.Context, lm *LinkManager, udp bool, port int) (stream, func(), bool) {
	// A link can die between Pick and OpenStream; try another one.
	for try := 0; try < 3; try++ {
		link, release, ok := pickWait(ctx, lm, !udp)
		if !ok {
			return nil, nil, false
		}
		st, err := link.OpenStream()
		if err == nil {
			if _, err = st.Write(userStreamHeader(link, udp, port, lm.exitInfo.Load())); err == nil {
				return st, release, true
			}
			st.Close()
		}
		release()
	}
	return nil, nil, false
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

type udpFlow struct {
	st   stream
	last time.Time
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
					f.st.Close()
					delete(flows, k)
				}
			}
			mu.Unlock()
		}
	}()
	buf := make([]byte, maxDatagram)
	for {
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		key := addr.String()
		mu.Lock()
		f := flows[key]
		if f != nil {
			f.last = time.Now()
		}
		mu.Unlock()
		if f == nil {
			st, release, ok := openStream(ctx, lm, true, port)
			if !ok {
				continue
			}
			f = &udpFlow{st: st, last: time.Now()}
			mu.Lock()
			flows[key] = f
			mu.Unlock()
			go func() {
				defer release()
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
				st.Close()
				mu.Lock()
				if flows[key] == f {
					delete(flows, key)
				}
				mu.Unlock()
			}()
		}
		if writeDatagram(f.st, buf[:n]) != nil {
			f.st.Close()
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
	pl := newStreamL3Link(st, peerL3Quiet(linkMeterOf(l)))
	set.add(pl)
	go func() {
		set.serveLink(ctx, pl, dev)
		set.remove(pl)
	}()
}
