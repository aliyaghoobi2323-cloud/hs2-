package tlscarrier

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"sync"
	"time"
)

// Server terminates real TLS (standard crypto/tls, real cert), then decides per
// connection: authorised tunnel, or probe/browser to be served real content.
type Server struct {
	SharedKey   []byte
	Cert        tls.Certificate
	BackendAddr string // a real local web server; probes are proxied here
	replay      *replayMem
	once        sync.Once
}

func (s *Server) init() { s.once.Do(func() { s.replay = newReplayMem() }) }

// Handle takes a raw accepted TCP conn, completes TLS, and routes it. On an
// authorised client it returns a ready carrier via onTunnel; otherwise it
// proxies the decrypted stream to the backend so the peer gets real content.
func (s *Server) Handle(ctx context.Context, raw net.Conn, onTunnel func(*Carrier)) {
	s.init()
	tconn := tls.Server(raw, &tls.Config{
		Certificates: []tls.Certificate{s.Cert},
		MinVersion:   tls.VersionTLS12,
	})
	tconn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := tconn.Handshake(); err != nil {
		raw.Close()
		return
	}
	tconn.SetDeadline(time.Time{})

	// Peek the first in-TLS bytes: an auth record from a real client, or an
	// HTTP request from a probe/browser.
	rec, err := readAuthRecord(tconn)
	if err != nil {
		s.forward(ctx, tconn, rec)
		return
	}
	nonce, ok := verifyAuthRecord(s.SharedKey, rec)
	if !ok || !s.replay.add(nonce) {
		// Invalid or replayed: treat as a probe. Its decrypted bytes (rec) plus
		// the rest go to the real backend, which answers like a website.
		s.forward(ctx, tconn, rec)
		return
	}
	onTunnel(&Carrier{conn: tconn})
}

// forward proxies the TLS-decrypted stream to the real backend, replaying the
// bytes already read, so a probe receives a genuine response from a real site.
func (s *Server) forward(ctx context.Context, tconn *tls.Conn, already []byte) {
	defer tconn.Close()
	up, err := net.DialTimeout("tcp", s.BackendAddr, 8*time.Second)
	if err != nil {
		return
	}
	defer up.Close()
	if len(already) > 0 {
		if _, err := up.Write(already); err != nil {
			return
		}
	}
	done := make(chan struct{}, 2)
	go func() { io.Copy(up, tconn); done <- struct{}{} }()
	go func() { io.Copy(tconn, up); done <- struct{}{} }()
	select {
	case <-ctx.Done():
	case <-done:
	}
}
