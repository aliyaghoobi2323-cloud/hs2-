package tlscarrier

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Server terminates real TLS (standard crypto/tls, real cert), then decides per
// connection: authorised tunnel, or probe/browser to be served real content.
type Server struct {
	SharedKey   []byte
	Cert        tls.Certificate
	BackendAddr string // a real local web server; probes are proxied here
	// Logf, if set, receives rare diagnostic messages (e.g. an old client).
	Logf    func(string, ...any)
	replay  *replayMem
	once    sync.Once
	lastLog atomic.Int64
}

func (s *Server) init() { s.once.Do(func() { s.replay = newReplayMem() }) }

// firstReadTimeout is how long a connection may stay silent after the TLS
// handshake. A real client sends its auth record at once; this only bounds
// idle probes, roughly like a web server's header timeout.
const firstReadTimeout = 30 * time.Second

// Handle takes a raw accepted TCP conn, completes TLS, and routes it. On an
// authorised client it returns a ready carrier via onTunnel; otherwise it
// proxies the decrypted stream to the backend so the peer gets real content.
func (s *Server) Handle(ctx context.Context, raw net.Conn, onTunnel func(*Carrier)) {
	s.init()
	tuneTCP(raw)
	tconn := tls.Server(raw, &tls.Config{
		Certificates: []tls.Certificate{s.Cert},
		MinVersion:   tls.VersionTLS12,
		NextProtos:   []string{"http/1.1"},
	})
	tconn.SetDeadline(time.Now().Add(authTimeout))
	if err := tconn.Handshake(); err != nil {
		raw.Close()
		return
	}
	cs := tconn.ConnectionState()
	ekm, err := exportEKM(&cs)
	if err != nil {
		raw.Close()
		return
	}

	// The first TLS record is either our auth record (always sent in one
	// write) or whatever a browser/probe says. Reading one record — not a
	// fixed byte count — means a short probe is answered by the backend at
	// once instead of hanging on a read no web server would make.
	tconn.SetDeadline(time.Now().Add(firstReadTimeout))
	buf := make([]byte, 16<<10)
	n, err := tconn.Read(buf)
	first := buf[:n]
	if n == 0 {
		tconn.Close()
		return
	}
	nonce, more, ok := parseClientAuth(s.SharedKey, ekm, first)
	if !ok {
		if isLegacyAuth(s.SharedKey, first) {
			s.logRare("tls: refused a pre-v2 client (no channel binding) from %s: upgrade the Iran server", raw.RemoteAddr())
		}
		tconn.SetDeadline(time.Time{})
		s.forward(ctx, tconn, first)
		return
	}
	tconn.SetDeadline(time.Now().Add(authTimeout))
	if more > 0 {
		if _, err := io.CopyN(io.Discard, tconn, int64(more)); err != nil {
			tconn.Close()
			return
		}
	}
	if !s.replay.add(nonce) {
		tconn.Close()
		return
	}
	if _, err := tconn.Write(makeServerProof(s.SharedKey, nonce, ekm)); err != nil {
		tconn.Close()
		return
	}
	tconn.SetDeadline(time.Time{})
	onTunnel(&Carrier{conn: tconn})
}

// logRare logs at most once every 30 seconds.
func (s *Server) logRare(f string, a ...any) {
	if s.Logf == nil {
		return
	}
	now := time.Now().Unix()
	if last := s.lastLog.Load(); now-last < 30 || !s.lastLog.CompareAndSwap(last, now) {
		return
	}
	s.Logf(f, a...)
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
