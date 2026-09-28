package engine

import (
	"context"
	"net"
	"time"

	"github.com/xtaci/smux"
	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

// RunMTCPIran (Iran side): brings up the link pool and opens user ports. Each
// user connection picks the least-loaded link and rides one smux stream to the
// kharej panel. User connections are pinned to their link (no cross-link
// reorder). No TUN is used on this path — the pool carries user traffic
// directly, which is simpler and faster than routing through hs0.
func RunMTCPIran(ctx context.Context, dialer LinkDialer, ports []string, min, max, perLink int, logf func(string, ...any)) error {
	lm := NewLinkManager(dialer, min, max, perLink, logf)
	go lm.Run(ctx)
	// wait briefly for the first links
	time.Sleep(3 * time.Second)

	for _, p := range ports {
		ln, err := net.Listen("tcp", ":"+p)
		if err != nil {
			return err
		}
		go func(ln net.Listener, port string) {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				go handleUser(c, lm)
			}
		}(ln, p)
		if logf != nil {
			logf("mtcp: user port %s open, load-balanced across links", p)
		}
	}
	<-ctx.Done()
	return nil
}

func handleUser(user net.Conn, lm *LinkManager) {
	defer user.Close()
	link, release, ok := lm.Pick()
	if !ok {
		return // no link available; drop (user retries)
	}
	defer release()
	st, err := link.OpenStream()
	if err != nil {
		return
	}
	defer st.Close()
	// pin the user to this stream both ways; stream is not a net.Conn so use the
	// io-level splice.
	done := make(chan struct{}, 2)
	go func() { copyIO(st, user); done <- struct{}{} }()
	go func() { copyIO(user, st); done <- struct{}{} }()
	<-done
}

// RunMTCPKharej (Kharej side): accepts parallel TLS links, runs a smux server on
// each authenticated session, and relays every accepted stream to the panel.
func RunMTCPKharej(ctx context.Context, ln net.Listener, srv *tlscarrier.Server, panelAddr string, logf func(string, ...any)) error {
	go func() { <-ctx.Done(); ln.Close() }()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			continue
		}
		go srv.Handle(ctx, conn, func(car *tlscarrier.Carrier) {
			// authorised link: run smux server over the authed TLS conn
			sess, err := smux.Server(car.RawConn(), newSmuxConfig())
			if err != nil {
				car.Close()
				return
			}
			defer sess.Close()
			if logf != nil {
				logf("mtcp: link up from %s", conn.RemoteAddr())
			}
			for {
				st, err := sess.AcceptStream()
				if err != nil {
					return
				}
				go relayStreamToPanel(st, panelAddr)
			}
		})
	}
}

func relayStreamToPanel(st *smux.Stream, panelAddr string) {
	defer st.Close()
	up, err := net.DialTimeout("tcp", panelAddr, 5*time.Second)
	if err != nil {
		return
	}
	defer up.Close()
	spliceStream(st, up)
}

func spliceStream(a *smux.Stream, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { copyIO(a, b); done <- struct{}{} }()
	go func() { copyIO(b, a); done <- struct{}{} }()
	<-done
}
