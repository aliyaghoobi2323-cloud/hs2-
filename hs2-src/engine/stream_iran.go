package engine

import (
	"context"
	"net"
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
	PerLink  int // user connections per link before the pool grows
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
}

// RunIran brings up the link pool and the user-facing listeners and serves
// until ctx ends. Listeners never depend on a link being up: a user who
// arrives during a reconnect waits briefly for one.
func RunIran(ctx context.Context, cfg IranConfig) error {
	logf := cfg.Log
	if logf == nil {
		logf = func(string, ...any) {}
	}
	reverse := cfg.RevServer != nil
	var lm *LinkManager
	if reverse {
		lm = NewLinkManager(nil, 1, 1, cfg.PerLink, logf)
		lm.accept = true
	} else {
		lm = NewLinkManager(cfg.Dialer, cfg.Min, cfg.Max, cfg.PerLink, logf)
	}
	var l3 *l3Set
	if cfg.TUN != nil {
		l3 = &l3Set{}
		go l3.pumpTun(ctx, cfg.TUN)
		go l3.logDrops(ctx, logf)
	}
	// Every new link gets a control channel (health feedback) in its own
	// goroutine, and, in TUN mode, its L3 side-channel stream.
	lm.OnLink = func(l Link) {
		go openControl(ctx, l, logf)
		if l3 != nil {
			openL3(ctx, l, l3, cfg.TUN, logf)
		}
	}
	go lm.Run(ctx)
	if reverse {
		go acceptReverseLinks(ctx, cfg.RevListener, cfg.RevServer, lm, logf)
	}

	for _, p := range cfg.Ports {
		bind := net.JoinHostPort(cfg.ListenIP, p)
		ln, err := ListenReuse(bind)
		if err != nil {
			return err
		}
		go func() { <-ctx.Done(); ln.Close() }()
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				go serveUserTCP(ctx, c, lm)
			}
		}()
		msg := "tcp"
		if cfg.UDP {
			pc, err := net.ListenPacket("udp", bind)
			if err != nil {
				return err
			}
			go func() { <-ctx.Done(); pc.Close() }()
			go serveUserUDP(ctx, pc, lm, logf)
			msg = "tcp+udp"
		}
		logf("user port %s open (%s), carried over the link pool", p, msg)
	}
	<-ctx.Done()
	return nil
}

// pickWait returns a link, waiting up to a few seconds for one to come up.
func pickWait(ctx context.Context, lm *LinkManager) (Link, func(), bool) {
	for i := 0; i < 40; i++ {
		if l, rel, ok := lm.Pick(); ok {
			return l, rel, true
		}
		select {
		case <-ctx.Done():
			return nil, nil, false
		case <-time.After(150 * time.Millisecond):
		}
	}
	return nil, nil, false
}

func openStream(ctx context.Context, lm *LinkManager, kind byte) (stream, func(), bool) {
	// A link can die between Pick and OpenStream; try another one.
	for try := 0; try < 3; try++ {
		link, release, ok := pickWait(ctx, lm)
		if !ok {
			return nil, nil, false
		}
		st, err := link.OpenStream()
		if err == nil {
			if _, err = st.Write([]byte{kind}); err == nil {
				return st, release, true
			}
			st.Close()
		}
		release()
	}
	return nil, nil, false
}

func serveUserTCP(ctx context.Context, user net.Conn, lm *LinkManager) {
	st, release, ok := openStream(ctx, lm, kindTCP)
	if !ok {
		user.Close()
		return
	}
	defer release()
	relay(user, st)
}

type udpFlow struct {
	st   stream
	last time.Time
}

// serveUserUDP gives each client address its own stream (so one flow's loss
// or backlog never stalls another) and closes flows after udpIdle.
func serveUserUDP(ctx context.Context, pc net.PacketConn, lm *LinkManager, logf func(string, ...any)) {
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
			st, release, ok := openStream(ctx, lm, kindUDP)
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
	pl := newL3Link(newStreamPkt(st))
	set.add(pl)
	go func() {
		set.serveLink(ctx, pl, dev)
		set.remove(pl)
	}()
}
