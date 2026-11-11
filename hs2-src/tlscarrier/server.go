package tlscarrier

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Server terminates real TLS (standard crypto/tls, real cert), then decides per
// connection: authorised tunnel, or probe/browser to be served real content.
type Server struct {
	SharedKey []byte
	Cert      tls.Certificate
	// GetCertificate, if set, supplies the certificate per handshake instead of
	// the static Cert. It lets the certificate be hot-swapped after a renewal
	// without restarting the tunnel (see cmd/hs2 certReloader): new connections
	// pick up the fresh cert, existing ones are undisturbed.
	GetCertificate func(*tls.ClientHelloInfo) (*tls.Certificate, error)
	BackendAddr    string // a real local web server; probes are proxied here
	// BackendFor, if set, picks the backend per connection from the local
	// address it arrived on and the SNI it asked for (a distinct cover site per
	// address/subdomain); "" falls back to BackendAddr.
	BackendFor func(local net.Addr, sni string) string
	// Logf, if set, receives rare diagnostic messages (e.g. an old client).
	Logf    func(string, ...any)
	replay  *replayMem
	once    sync.Once
	lastLog atomic.Int64
	// shared is the one tls.Config of every connection with Resume on, so
	// they share Go's (in-memory, auto-rotated) ticket keys and a ticket one
	// connection issued can resume another (resume.go).
	shared *tls.Config
}

func (s *Server) init() {
	s.once.Do(func() {
		s.replay = newReplayMem()
		if Resume {
			s.shared = s.newTLSConfig()
		}
	})
}

// newTLSConfig is a connection's TLS config: built per connection as before,
// or the one shared config with Resume on.
func (s *Server) newTLSConfig() *tls.Config {
	tcfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"http/1.1"},
	}
	if s.GetCertificate != nil {
		tcfg.GetCertificate = s.GetCertificate
	} else {
		tcfg.Certificates = []tls.Certificate{s.Cert}
	}
	return tcfg
}

// firstReadTimeout is how long a connection may stay silent after the TLS
// handshake. A real client sends its auth record at once; this only bounds idle
// probes. With HS2_COVER_NGINX it is nginx's client_header_timeout (60 s): a
// real nginx 1.24 closes a TLS connection that never sent a byte after 60 s
// with a close_notify (measured side by side in the lab), and so do we — an
// idle probe times us exactly like the nginx we present as (it was 30 s).
// Without the key (the Go-style cover) the old 30 s stays. Tunnel links never
// wait here: the auth record is their first write after the handshake.
func firstReadTimeout() time.Duration {
	if CoverNginx {
		return 60 * time.Second
	}
	return 30 * time.Second
}

// handshakeTimeout bounds the TLS handshake (from accept to a finished
// handshake), so the most a flood of half-open connections can hold a
// goroutine/fd is this value. A genuine client sends its ClientHello in one
// flight and completes 1-RTT TLS in well under this even on a lossy
// Iran↔foreign path. (A per-connection probe shield is deferred.)
const handshakeTimeout = 10 * time.Second

// httpToHTTPS400 is the exact response Go's own HTTPS server sends when it gets
// a plain-HTTP request on a TLS port. We reproduce it byte-for-byte (pinned by a
// test against net/http) so a plain-HTTP probe sees what a real Go HTTPS server
// sends, instead of the old silent close that stood out as a fingerprint. Our
// TLS stack IS Go's, so this is the honest, coherent answer — not an imitation.
const httpToHTTPS400 = "HTTP/1.0 400 Bad Request\r\n\r\nClient sent an HTTP request to an HTTPS server.\n"

// tlsRecordHeaderLooksLikeHTTP mirrors net/http's own detection verbatim: ONLY
// these exact 5-byte request starts are answered as plain HTTP. Every other
// start — including other HTTP methods like DELETE — is not; a real Go HTTPS
// server lets those fail at the TLS layer, and so do we. Matching Go's exact
// set is what keeps us coherent.
// plainHTTPReply is the answer to a plain-HTTP request on the TLS port: Go's
// (httpToHTTPS400), or with HS2_COVER_NGINX the reply nginx sends there, so
// the port answers like the nginx-style cover it fronts.
func plainHTTPReply(now time.Time) string {
	if !CoverNginx {
		return httpToHTTPS400
	}
	return NginxError(400, "The plain HTTP request was sent to HTTPS port", now, true)
}

func tlsRecordHeaderLooksLikeHTTP(h [5]byte) bool {
	switch string(h[:]) {
	case "GET /", "HEAD ", "POST ", "PUT /", "OPTIO":
		return true
	}
	return false
}

// Handle takes a raw accepted TCP conn, completes TLS, and routes it. On an
// authorised client it returns a ready carrier via onTunnel; otherwise it
// proxies the decrypted stream to the backend so the peer gets real content.
func (s *Server) Handle(ctx context.Context, raw net.Conn, onTunnel func(*Carrier)) {
	s.handle(ctx, raw, onTunnel, nil)
}

// handle is Handle with an optional cover dialer: non-nil replaces the
// backend of an unauthenticated connection (a "both" IP proxies it to the
// reality target, phase 4).
func (s *Server) handle(ctx context.Context, raw net.Conn, onTunnel func(*Carrier), cover func(context.Context) (net.Conn, error)) {
	s.init()
	tuneTCP(raw)
	if QuietIdle { // Chrome's 45 s instead of the listener's 15 s (dpi.go)
		if tc, ok := raw.(*net.TCPConn); ok {
			tc.SetKeepAlive(true)
			tc.SetKeepAlivePeriod(quietKeepAlive)
		}
	}

	tcfg := s.shared
	if tcfg == nil {
		tcfg = s.newTLSConfig()
	}
	tconn := tls.Server(wrapBatch(raw), tcfg)
	tconn.SetDeadline(time.Now().Add(handshakeTimeout))
	if err := tconn.Handshake(); err != nil {
		// Probe handling is Go's OWN, not an imitation of it: TLS reads the
		// socket itself (so how much of a probe is consumed — and therefore
		// whether the close is a FIN or an RST — is exactly crypto/tls's), and a
		// first record that is not TLS surfaces as tls.RecordHeaderError, which
		// is precisely what net/http's HTTPS server inspects to answer a
		// plain-HTTP request. Same check, same bytes, same close as net/http.
		var re tls.RecordHeaderError
		if errors.As(err, &re) && re.Conn != nil && tlsRecordHeaderLooksLikeHTTP(re.RecordHeader) {
			io.WriteString(re.Conn, plainHTTPReply(time.Now()))
			re.Conn.Close()
			return
		}
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
	tconn.SetDeadline(time.Now().Add(firstReadTimeout()))
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
		backend := s.BackendAddr
		if s.BackendFor != nil {
			if b := s.BackendFor(raw.LocalAddr(), cs.ServerName); b != "" {
				backend = b
			}
		}
		s.forward(ctx, tconn, first, backend, cover)
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
func (s *Server) forward(ctx context.Context, tconn *tls.Conn, already []byte, backend string, cover func(context.Context) (net.Conn, error)) {
	defer tconn.Close()
	var up net.Conn
	var err error
	if cover != nil {
		up, err = cover(ctx)
	} else {
		up, err = net.DialTimeout("tcp", backend, 8*time.Second)
	}
	if err != nil {
		return
	}
	defer up.Close()
	if len(already) > 0 {
		if _, err := up.Write(already); err != nil {
			return
		}
	}
	fromClient := make(chan struct{}, 1)
	fromBackend := make(chan int64, 1)
	go func() { io.Copy(up, tconn); fromClient <- struct{}{} }()
	go func() { n, _ := io.Copy(tconn, up); fromBackend <- n }()
	select {
	case <-ctx.Done():
	case <-fromClient:
	case n := <-fromBackend:
		if !CoverNginx {
			return
		}
		if n == 0 {
			// The cover closed without a word (its header timeout on a
			// stalled or partial request): nginx 1.24 closes quietly there, with
			// no close_notify (measured side by side in the lab).
			tconn.NetConn().Close()
			return
		}
		// After its response the cover lingers (cover_head.go nginxLinger):
		// let what the client still sends drain through to it instead of
		// resetting the connection under the response, then close.
		tconn.CloseWrite()
		select {
		case <-ctx.Done():
		case <-fromClient:
		case <-time.After(30 * time.Second):
		}
	}
}
