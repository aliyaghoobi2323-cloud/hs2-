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
	SharedKey []byte
	Cert      tls.Certificate
	// GetCertificate, if set, supplies the certificate per handshake instead of
	// the static Cert. It lets the certificate be hot-swapped after a renewal
	// without restarting the tunnel (see cmd/hs2 certReloader): new connections
	// pick up the fresh cert, existing ones are undisturbed.
	GetCertificate func(*tls.ClientHelloInfo) (*tls.Certificate, error)
	BackendAddr    string // a real local web server; probes are proxied here
	// Logf, if set, receives rare diagnostic messages (e.g. an old client).
	Logf    func(string, ...any)
	replay  *replayMem
	once    sync.Once
	lastLog atomic.Int64
}

func (s *Server) init() { s.once.Do(func() { s.replay = newReplayMem() }) }

// firstReadTimeout is how long a connection may stay silent after the TLS
// handshake. A real client sends its auth record at once; this only bounds idle
// probes, roughly like a web server's header timeout. Kept modest on purpose:
// lengthening it toward nginx's 60s default would make a flood of idle probes
// hold goroutines longer (against the probe-load constraint) for only a soft
// timing-coherence gain, and the per-connection probe shield is not built yet.
const firstReadTimeout = 30 * time.Second

// handshakeTimeout bounds the TLS handshake and the 5-byte record-header peek
// before it. A real tunnel client completes the handshake in a few round trips
// even on a lossy Iran↔foreign path, so 15s is comfortably enough for a genuine
// client while keeping the hold a flood of half-open handshakes can impose close
// to the old 10s — again favouring the probe-load constraint over chasing
// nginx's longer default.
const handshakeTimeout = 15 * time.Second

// httpToHTTPS400 is the exact response Go's own HTTPS server sends when it gets
// a plain-HTTP request on a TLS port. We reproduce it byte-for-byte (pinned by a
// test against net/http) so a plain-HTTP probe sees what a real Go HTTPS server
// sends, instead of the old silent close that stood out as a fingerprint. Our
// TLS stack IS Go's, so this is the honest, coherent answer — not an imitation.
const httpToHTTPS400 = "HTTP/1.0 400 Bad Request\r\n\r\nClient sent an HTTP request to an HTTPS server.\n"

// tlsRecordHeaderLooksLikeHTTP mirrors net/http's own detection verbatim: ONLY
// these exact 5-byte request starts are answered as plain HTTP. Every other
// start — including other HTTP methods like DELETE — is not; a real Go HTTPS
// server lets those fail at the TLS layer, and so do we (by handing the bytes to
// tls.Server unchanged). Matching Go's exact set is what keeps us coherent.
func tlsRecordHeaderLooksLikeHTTP(h [5]byte) bool {
	switch string(h[:]) {
	case "GET /", "HEAD ", "POST ", "PUT /", "OPTIO":
		return true
	}
	return false
}

// prefixConn re-serves bytes already read from the underlying conn (the peeked
// record header) and then becomes a transparent passthrough: once the prefix is
// drained, Read delegates straight to the underlying conn with no copy, so the
// authenticated data path carries zero steady-state overhead from the peek.
type prefixConn struct {
	net.Conn
	prefix []byte
}

func (p *prefixConn) Read(b []byte) (int, error) {
	if len(p.prefix) > 0 {
		n := copy(b, p.prefix)
		p.prefix = p.prefix[n:]
		return n, nil
	}
	return p.Conn.Read(b)
}

// Handle takes a raw accepted TCP conn, completes TLS, and routes it. On an
// authorised client it returns a ready carrier via onTunnel; otherwise it
// proxies the decrypted stream to the backend so the peer gets real content.
func (s *Server) Handle(ctx context.Context, raw net.Conn, onTunnel func(*Carrier)) {
	s.init()
	tuneTCP(raw)

	// Classify the connection by its first 5 bytes (a TLS record header is 5
	// bytes, and so is the shortest HTTP request start we recognise) BEFORE
	// handing it to TLS. A plain-HTTP probe is then answered exactly as a Go
	// HTTPS server answers it, instead of the old silent close that was a
	// fingerprint. Everything that is not one of net/http's recognised HTTP
	// starts — a real TLS client, or genuine garbage — flows into tls.Server
	// unchanged via prefixConn, so its behaviour stays byte-identical to a real
	// Go TLS server. The authenticated data path is untouched.
	raw.SetReadDeadline(time.Now().Add(handshakeTimeout))
	var hdr [5]byte
	nh, herr := io.ReadFull(raw, hdr[:])
	raw.SetReadDeadline(time.Time{})
	if nh == 0 || (herr != nil && herr != io.ErrUnexpectedEOF) {
		// Nothing arrived, or a read error/timeout before a usable header: close
		// like a server that never received a request.
		raw.Close()
		return
	}
	if nh == 5 && tlsRecordHeaderLooksLikeHTTP(hdr) {
		raw.SetWriteDeadline(time.Now().Add(10 * time.Second))
		io.WriteString(raw, httpToHTTPS400)
		raw.Close()
		return
	}
	// Replay the peeked bytes to TLS. A short read (nh < 5) is a malformed start;
	// tls.Server rejects it exactly as it would without the peek.
	raw = &prefixConn{Conn: raw, prefix: append([]byte(nil), hdr[:nh]...)}

	tcfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"http/1.1"},
	}
	if s.GetCertificate != nil {
		tcfg.GetCertificate = s.GetCertificate
	} else {
		tcfg.Certificates = []tls.Certificate{s.Cert}
	}
	tconn := tls.Server(raw, tcfg)
	tconn.SetDeadline(time.Now().Add(handshakeTimeout))
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
