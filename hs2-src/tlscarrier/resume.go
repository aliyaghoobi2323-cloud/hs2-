package tlscarrier

import (
	"net"
	"sync"

	utls "github.com/refraction-networking/utls"
)

// TLS session resumption (HS2_TLS_RESUME=1).
//
// Without it every link is a full handshake with the same ClientHello, link
// after link to one server — a browser resumes instead: after its first
// connection it offers a ticket (pre_shared_key) and the server skips the
// certificate. With the key on, the dialer keeps the tickets the server sends
// and the server can resume them (one shared tls.Config, server.go).
//
// The auth stays bound to THIS connection. Chrome — and the preset — offers
// only psk_dhe_ke, so a resumed handshake still runs a fresh (EC)DHE exchange,
// and the exporter secret the auth tag is bound to (auth.go) is derived from
// the whole handshake transcript, both randoms and that new key share
// included (RFC 8446 §7.1, §7.5). Every connection — resumed or not — has its
// own EKM, so an auth record recorded on one connection is worthless on any
// other: the server checks the tag against its own connection's EKM and sends
// the replayer to the cover site (TestResumeReplayFails). A ticket only lets
// a client skip the certificate; it proves nothing to the tunnel, and a
// stolen one gains no key: the shared key is never in it. No 0-RTT data is
// sent or accepted (Go's server refuses early data), so nothing can be
// replayed before the handshake ends either.
//
// Tickets are used once, newest first, like Chrome's TLS 1.3 tickets: links
// are dialed in parallel, and one ticket offered on several connections would
// put the same identity on the wire several times — a mark no browser makes.
// The server issues a fresh ticket on every connection (resumed ones too), so
// the cache refills as links are built. A ticket the server can no longer
// open (a restart: Go's ticket keys live in memory) just yields a full
// handshake, as in a browser.
//
// One cache per (address, SNI): uTLS keys sessions by SNI alone, and the
// tunnel may dial several addresses (multipath lb) under one name, each a
// separate server whose tickets the others cannot open.

// ticketCacheMax bounds the tickets kept per server (a pool of links).
const ticketCacheMax = 64

// ticketCache is a single-use, newest-first utls.ClientSessionCache.
type ticketCache struct {
	mu sync.Mutex
	s  []*utls.ClientSessionState
}

// Get hands out the newest ticket and forgets it (single use).
func (c *ticketCache) Get(string) (*utls.ClientSessionState, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(c.s)
	if n == 0 {
		return nil, false
	}
	cs := c.s[n-1]
	c.s[n-1] = nil
	c.s = c.s[:n-1]
	return cs, true
}

// Put stores a ticket; nil (uTLS dropping an unusable session) clears the
// cache, since the tickets before it came from the same server state.
func (c *ticketCache) Put(_ string, cs *utls.ClientSessionState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cs == nil {
		clear(c.s)
		c.s = c.s[:0]
		return
	}
	if len(c.s) >= ticketCacheMax {
		copy(c.s, c.s[1:])
		c.s = c.s[:len(c.s)-1]
	}
	c.s = append(c.s, cs)
}

func (c *ticketCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.s)
}

var (
	ticketMu     sync.Mutex
	ticketCaches = map[string]*ticketCache{}
)

// ticketCacheFor is the cache of one server (address and SNI).
func ticketCacheFor(addr, sni string) *ticketCache {
	k := addr + "\x00" + sni
	ticketMu.Lock()
	defer ticketMu.Unlock()
	c := ticketCaches[k]
	if c == nil {
		c = &ticketCache{}
		ticketCaches[k] = c
	}
	return c
}

// newClientConn builds the link's uTLS client. With Resume off it is exactly
// today's UClient. With it on, the same preset spec gets a pre_shared_key
// extension at the end (where Chrome puts it; uTLS leaves it out entirely
// while there is no ticket, so a first hello is byte-for-byte the preset's)
// and the server's ticket cache.
func newClientConn(raw net.Conn, cfg *utls.Config, addr string, id utls.ClientHelloID) (*utls.UConn, error) {
	if !Resume {
		return utls.UClient(raw, cfg, id), nil
	}
	cfg.ClientSessionCache = ticketCacheFor(addr, cfg.ServerName)
	// No ticket yet: leave the extension out (uTLS refuses an empty one
	// otherwise), so the hello is the preset's.
	cfg.OmitEmptyPsk = true
	spec, err := utls.UTLSIdToSpec(id)
	if err != nil {
		return nil, err
	}
	if !hasPSK(spec.Extensions) {
		spec.Extensions = append(spec.Extensions, &utls.UtlsPreSharedKeyExtension{})
	}
	u := utls.UClient(raw, cfg, utls.HelloCustom)
	if err := u.ApplyPreset(&spec); err != nil {
		return nil, err
	}
	return u, nil
}

func hasPSK(exts []utls.TLSExtension) bool {
	for _, e := range exts {
		if _, ok := e.(utls.PreSharedKeyExtension); ok {
			return true
		}
	}
	return false
}
