package tlscarrier

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
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
	// pairs: the QUIET halves of pairs waiting for their BULK (pairWait)
	pairMu sync.Mutex
	pairs  map[[pairIDLen]byte]chan *Carrier
	// levelCap, when set, is the highest level this server speaks (tests:
	// an older server)
	levelCap byte
}

func (s *Server) init() { s.once.Do(func() { s.replay = newReplayMem() }) }

// pairTimeout: a pair's QUIET half waits this long for its BULK half (which
// its client dials as soon as it has QUIET's proof).
var pairTimeout = authTimeout

// pairWait registers the QUIET half of pair id: its BULK half is handed to
// the channel (pairGive).
func (s *Server) pairWait(id [pairIDLen]byte) chan *Carrier {
	s.pairMu.Lock()
	defer s.pairMu.Unlock()
	if s.pairs == nil {
		s.pairs = map[[pairIDLen]byte]chan *Carrier{}
	}
	if _, dup := s.pairs[id]; dup {
		return nil // (a random 128-bit id seen twice: refused)
	}
	w := make(chan *Carrier, 1)
	s.pairs[id] = w
	return w
}

// pairGive hands car, the BULK half of pair id, to its QUIET half (false:
// none waits for it).
func (s *Server) pairGive(id [pairIDLen]byte, car *Carrier) bool {
	s.pairMu.Lock()
	defer s.pairMu.Unlock()
	w := s.pairs[id]
	if w == nil {
		return false
	}
	delete(s.pairs, id)
	w <- car // (buffered: one giver)
	return true
}

// pairDrop: the QUIET half of pair id stops waiting; a BULK half handed to
// it meanwhile is closed.
func (s *Server) pairDrop(id [pairIDLen]byte, w chan *Carrier) {
	s.pairMu.Lock()
	defer s.pairMu.Unlock()
	if s.pairs[id] == w {
		delete(s.pairs, id)
	}
	select {
	case b := <-w:
		b.conn.Close()
	default:
	}
}

// firstReadTimeout is how long a connection may stay silent after the TLS
// handshake. A real client sends its auth record at once; this only bounds idle
// probes, roughly like a web server's header timeout. Kept modest on purpose:
// lengthening it toward nginx's 60s default would make a flood of idle probes
// hold goroutines longer (against the probe-load constraint) for only a soft
// timing-coherence gain, and the per-connection probe shield is not built yet.
const firstReadTimeout = 30 * time.Second

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
	s.init()
	tuneTCP(raw)

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
		// Probe handling is Go's OWN, not an imitation of it: TLS reads the
		// socket itself (so how much of a probe is consumed — and therefore
		// whether the close is a FIN or an RST — is exactly crypto/tls's), and a
		// first record that is not TLS surfaces as tls.RecordHeaderError, which
		// is precisely what net/http's HTTPS server inspects to answer a
		// plain-HTTP request. Same check, same bytes, same close as net/http.
		var re tls.RecordHeaderError
		if errors.As(err, &re) && re.Conn != nil && tlsRecordHeaderLooksLikeHTTP(re.RecordHeader) {
			io.WriteString(re.Conn, httpToHTTPS400)
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
	tconn.SetDeadline(time.Now().Add(firstReadTimeout))
	buf := make([]byte, 16<<10)
	n, err := tconn.Read(buf)
	first := buf[:n]
	if n == 0 {
		tconn.Close()
		return
	}
	nonce, hdr, more, v2, ok := parseClientAuth(s.SharedKey, ekm, first)
	if !ok {
		if v2 {
			// A v2 hs2 that knows the key: there is no v2 mode to serve it in.
			// Closed (it redials with its backoff), and said why.
			proto.refused(true)
			s.logPeer(raw.RemoteAddr(), ErrPeerV2.Error())
			tconn.Close()
			return
		}
		if isLegacyAuth(s.SharedKey, first) {
			s.logRare("tls: refused a pre-v2 client (no channel binding) from %s: upgrade the Iran server", raw.RemoteAddr())
		}
		tconn.SetDeadline(time.Time{})
		s.forward(ctx, tconn, first)
		return
	}
	tconn.SetDeadline(time.Now().Add(authTimeout))
	pad := first[authHeadLen:len(first):len(first)] // (its head may be a pair's hello)
	if more > 0 {
		rest := make([]byte, more)
		if _, err := io.ReadFull(tconn, rest); err != nil {
			tconn.Close()
			return
		}
		pad = append(pad, rest...)
	}
	if !s.replay.add(nonce) {
		tconn.Close()
		return
	}
	// The level and caps for this link. With no level in common the proof
	// says level 0 — the client then knows why — and the link closes.
	srvMax := levelMax
	if s.levelCap != 0 {
		srvMax = s.levelCap
	}
	level, caps, agreed := negotiate(hdr, levelMin, srvMax, localCaps())
	// A pair (level 2 and CapPair both ways): this connection is the half
	// its hello says. The QUIET half waits for its BULK half, registered
	// before its proof leaves (the client dials BULK once it has it); the
	// BULK half is handed to it and the QUIET half's handler runs the link.
	var hello pairHello
	pair := false
	if agreed && level >= LevelPair && caps&CapPair != 0 {
		hello, pair = parsePairHello(pad, s.SharedKey, nonce, ekm)
	}
	if !pair {
		caps &^= CapPair // (a pair-capable client always says which half it is)
	}
	var wait chan *Carrier
	if pair && hello.role == pairQuiet {
		if wait = s.pairWait(hello.id); wait == nil {
			tconn.Close()
			return
		}
	}
	if _, err := tconn.Write(makeServerProof(s.SharedKey, nonce, ekm, ProofHdr{Ver: protoVersion, Level: level, Caps: caps})); err != nil || !agreed {
		if wait != nil {
			s.pairDrop(hello.id, wait)
		}
		if !agreed {
			proto.refused(false)
			s.logPeer(raw.RemoteAddr(), fmt.Sprintf("peer speaks protocol v3 levels %d-%d, this server %d-%d; upgrade the older server",
				hdr.MinLevel, hdr.MaxLevel, levelMin, srvMax))
		}
		tconn.Close()
		return
	}
	tconn.SetDeadline(time.Time{})
	car := &Carrier{conn: tconn, level: level, caps: caps}
	if pair && hello.role == pairBulk {
		if !s.pairGive(hello.id, car) {
			tconn.Close() // no QUIET half waits for it
		}
		return
	}
	if wait != nil {
		t := time.NewTimer(pairTimeout)
		defer t.Stop()
		select {
		case b := <-wait:
			car.bulk = b
		case <-t.C:
			s.pairDrop(hello.id, wait)
			tconn.Close()
			return
		case <-ctx.Done():
			s.pairDrop(hello.id, wait)
			tconn.Close()
			return
		}
	}
	proto.linked(level, caps)
	proto.peerLevels(hdr.MinLevel, hdr.MaxLevel)
	onTunnel(car)
}

// logPeer logs line for peer at most once a minute (the refusals of §4.5: a
// peer that keeps redialing would fill the log).
func (s *Server) logPeer(peer net.Addr, line string) {
	if s.Logf == nil {
		return
	}
	host := ""
	if peer != nil {
		host = peer.String()
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
	}
	if proto.mayLog(host+" "+line, time.Now()) {
		s.Logf("%s", line)
	}
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
