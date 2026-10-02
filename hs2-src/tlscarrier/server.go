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

// handshakeTimeout is ONE wall-clock budget shared by the 5-byte record-header
// peek AND the TLS handshake together (see Handle) — they do not stack. So the
// most a flood of half-open connections can hold a goroutine/fd is this value,
// matching the pre-peek behaviour (the old code bounded the handshake alone at
// authTimeout = 10s). A genuine client sends its ClientHello header+body in one
// flight and completes 1-RTT TLS in well under this even on a lossy
// Iran↔foreign path, so 10s is ample for real clients while giving mass probing
// no cheaper a hold than before. (A per-connection probe shield is deferred.)
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
// record header) before delegating to it. It stays wrapped around the conn for
// the connection's whole life, so the authenticated data path keeps reading
// through it; once the prefix is drained each Read is just one len-check and a
// pass-through call — no allocation and no copy, negligible beside the per-record
// AEAD — not literally removed from the path.
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

// NetConn exposes the wrapped connection so a caller unwrapping to the kernel
// socket (Carrier.TCPConn, for TCP_INFO stats) can see past this peek wrapper.
// Mirrors *tls.Conn.NetConn(), which is how the TLS layer above us is unwrapped.
func (p *prefixConn) NetConn() net.Conn { return p.Conn }

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
	// ONE wall-clock budget covers the peek AND the handshake below, so the two
	// do not stack: a stalling probe cannot spend handshakeTimeout on the peek
	// and then another handshakeTimeout on the handshake. Total half-open hold is
	// bounded by this single deadline — the same bound the handshake had before
	// the peek existed.
	deadline := time.Now().Add(handshakeTimeout)
	raw.SetReadDeadline(deadline)
	var hdr [5]byte
	nh, herr := io.ReadFull(raw, hdr[:])
	if nh == 0 || (herr != nil && herr != io.ErrUnexpectedEOF) {
		// Nothing arrived, or a read error/timeout before a usable header: close
		// like a server that never received a request.
		raw.Close()
		return
	}
	if nh == 5 && tlsRecordHeaderLooksLikeHTTP(hdr) {
		raw.SetWriteDeadline(time.Now().Add(5 * time.Second))
		io.WriteString(raw, httpToHTTPS400)
		// Consume the rest of the (already-buffered) request line/headers before
		// closing, so the close is a clean FIN like a real Go HTTPS server rather
		// than an RST triggered by unread data in the socket buffer — the
		// RST-vs-FIN difference is observable. One bounded read, ≤1s, is enough:
		// a plain-HTTP request arrives in one flight and is tiny.
		raw.SetReadDeadline(time.Now().Add(1 * time.Second))
		var discard [2048]byte
		raw.Read(discard[:])
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
	tconn.SetDeadline(deadline) // SAME budget as the peek — the two do not stack
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
