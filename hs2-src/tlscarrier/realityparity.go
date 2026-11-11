package tlscarrier

// sec41: the authenticated reality handshake answers exactly as the target
// answered THIS client hello, and the cover splice is no free relay.
//
// Before, a passive observer saw on every authenticated link (lab 2026-11-11,
// target www.microsoft.com, uTLS Chrome hello): cipher 0x1301 (Go's fixed
// server preference) vs the target's 0x1302, a Finished of 53 B vs 69 B (the
// SHA-256 vs SHA-384 suite), a Certificate record 2349 B shorter (the target
// staples OCSP and SCTs), and a ServerHello one listener->target round trip
// EARLIER than the target's own (the listener answered without waiting for
// the target). Now the listener forwards the hello to the target, waits for
// the target's ServerHello (the timing of a spliced connection), takes its
// cipher suite and key-share group, and serves that with the uTLS server
// (the suite per connection, onlySuite13: no global state, no lock),
// stapling the target's OCSP response and SCTs.

import (
	"crypto/tls"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	utls "github.com/refraction-networking/utls"
)

// onlySuite13 makes s the only TLS 1.3 suite the uTLS server sees in this
// client's offer, so it must pick s (rel44). It rewrites, in place, the
// parsed hello's suite list that GetConfigForClient's ClientHelloInfo shares
// with the handshake (utls handshake_server.go: CipherSuites:
// clientHello.cipherSuites); the transcript hashes the hello's original bytes
// (clientHelloMsg.marshal returns m.original), so nothing on the wire or in
// the Finished changes. Per connection: no global state, no lock. A no-op
// when the client did not offer s. TestRealityTargetSuiteParity guards the
// uTLS detail it relies on.
func onlySuite13(l []uint16, s uint16) {
	offered := false
	for _, x := range l {
		if x == s {
			offered = true
		}
	}
	if !offered {
		return
	}
	for i, x := range l {
		if x>>8 == 0x13 && x != s {
			l[i] = s
		}
	}
}

// targetHello is what the target answered to the client's own hello.
type targetHello struct {
	suite, group uint16
}

// readTargetHello reads the target's first handshake record(s) up to its
// ServerHello and returns the chosen suite and key-share group (ok=false:
// no ServerHello in time, an alert, a HelloRetryRequest).
func readTargetHello(c net.Conn, d time.Duration) (targetHello, bool) {
	c.SetReadDeadline(time.Now().Add(d))
	defer c.SetReadDeadline(time.Time{})
	var buf []byte
	tmp := make([]byte, 4096)
	for len(buf) < 32<<10 {
		if len(buf) >= 5 {
			if buf[0] != 22 {
				return targetHello{}, false
			}
			l := int(binary.BigEndian.Uint16(buf[3:5]))
			if len(buf) >= 5+l {
				return parseServerHello(buf[5 : 5+l])
			}
		}
		n, err := c.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil && n == 0 {
			return targetHello{}, false
		}
	}
	return targetHello{}, false
}

// hrrRandom marks a HelloRetryRequest (RFC 8446 4.1.3).
var hrrRandom = []byte{0xCF, 0x21, 0xAD, 0x74, 0xE5, 0x9A, 0x61, 0x11, 0xBE, 0x1D, 0x8C, 0x02, 0x1E, 0x65, 0xB8, 0x91,
	0xC2, 0xA2, 0x11, 0x16, 0x7A, 0xBB, 0x8C, 0x5E, 0x07, 0x9E, 0x09, 0xE2, 0xC8, 0xA8, 0x33, 0x9C}

func parseServerHello(m []byte) (targetHello, bool) {
	var th targetHello
	if len(m) < 4+2+32+1 || m[0] != 2 {
		return th, false
	}
	p := m[4:]
	if string(p[2:34]) == string(hrrRandom) {
		return th, false
	}
	sl := int(p[34])
	p = p[35:]
	if len(p) < sl+2+1+2 {
		return th, false
	}
	p = p[sl:]
	th.suite = binary.BigEndian.Uint16(p)
	el := int(binary.BigEndian.Uint16(p[3:]))
	e := p[5:]
	if len(e) < el {
		return th, false
	}
	e = e[:el]
	tls13 := false
	for len(e) >= 4 {
		id, l := binary.BigEndian.Uint16(e), int(binary.BigEndian.Uint16(e[2:]))
		if len(e) < 4+l {
			return th, false
		}
		d := e[4 : 4+l]
		switch id {
		case 43:
			tls13 = l == 2 && binary.BigEndian.Uint16(d) == 0x0304
		case 51:
			if l >= 2 {
				th.group = binary.BigEndian.Uint16(d)
			}
		}
		e = e[4+l:]
	}
	if !tls13 || th.suite>>8 != 0x13 {
		return th, false
	}
	return th, true
}

// ---- cover splice abuse limits ----

// A hello whose SNI is not one of our server names (or no SNI, or not TLS at
// all) is nobody's visit to the cover site: on a target behind a shared CDN
// it is a stranger using our IP as a clean front for ANOTHER site (lab
// 2026-11-11: target www.npmjs.com, SNI www.cloudflare.com -> 843 KB of
// cloudflare.com through the listener). Such splices get a small free
// allowance and a low rate: a probe's handshake and error page still pass
// as they would, a relay is useless.
const (
	foreignFreeBytes = 32 << 10
	foreignRateBps   = 8 << 10
	// netFreeBudget: free (full-speed) cover bytes per client /24 (/64) per
	// minute across ALL its connections and both directions, charged as
	// used; past it every splice from that net runs at its rate cap
	// (reconnecting no longer resets the allowance).
	netFreeBudget = 8 << 20
)

type netBudget struct {
	mu    sync.Mutex
	epoch int64
	used  map[string]int64
}

// take reserves up to want free bytes for net k this minute.
func (b *netBudget) take(k string, want int64) int64 {
	now := time.Now().Unix() / 60
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used == nil || b.epoch != now || len(b.used) > 1<<16 {
		b.used, b.epoch = map[string]int64{}, now
	}
	left := netFreeBudget - b.used[k]
	if left <= 0 {
		return 0
	}
	if want > left {
		want = left
	}
	b.used[k] += want
	return want
}

// spliceLimits are one cover splice's free bytes (per direction) and rate.
func (r *RealityServer) spliceLimits(h *realityHello) (free, rate int64) {
	free, rate = r.maxBytes, r.rateBps
	if h == nil || !r.names[strings.ToLower(h.sni)] {
		if free > foreignFreeBytes {
			free = foreignFreeBytes
		}
		if rate > foreignRateBps {
			rate = foreignRateBps
		}
	}
	return free, rate
}

// realityKeyLog: tests only (TestRealityEEParity decrypts our EE); nil in
// production.
var realityKeyLog atomic.Pointer[io.Writer]

// toUTLSConfig converts the authenticated handshake's config (built as a
// crypto/tls one, tunnelTLSConfig) for the uTLS server, staples the target's
// OCSP response / SCTs and pins the key-share group the target chose.
func (r *RealityServer) toUTLSConfig(c *tls.Config, th targetHello, ok bool) *utls.Config {
	u := &utls.Config{MinVersion: utls.VersionTLS13, NextProtos: c.NextProtos, SessionTicketsDisabled: true}
	if w := realityKeyLog.Load(); w != nil {
		u.KeyLogWriter = *w
	}
	if m := r.mirror.Load(); m != nil {
		u.ServerNameAck = m.SNIAck // EE as long as the target's (§11.8.3)
	}
	for _, cv := range c.CurvePreferences {
		u.CurvePreferences = append(u.CurvePreferences, utls.CurveID(cv))
	}
	if ok && th.group != 0 {
		u.CurvePreferences = []utls.CurveID{utls.CurveID(th.group)}
	}
	for _, crt := range c.Certificates {
		uc := utls.Certificate{Certificate: crt.Certificate, PrivateKey: crt.PrivateKey}
		if m := r.mirror.Load(); m != nil {
			uc.OCSPStaple, uc.SignedCertificateTimestamps = m.OCSP, m.SCTs
		}
		u.Certificates = append(u.Certificates, uc)
	}
	return u
}

// Connections still before their hello (each holds a target connection):
// per client /24 (/64) and per listener.
const (
	preMaxNet = 64
	preMax    = 4096
)

type preCount struct {
	mu  sync.Mutex
	all int
	net map[string]int
}

func (r *RealityServer) preAcquire(a net.Addr) (release func(), ok bool) {
	k := clientNet(a)
	p := &r.pre
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.net == nil {
		p.net = map[string]int{}
	}
	if p.all >= preMax || p.net[k] >= preMaxNet {
		return nil, false
	}
	p.all++
	p.net[k]++
	return func() {
		p.mu.Lock()
		p.all--
		if p.net[k]--; p.net[k] <= 0 {
			delete(p.net, k)
		}
		p.mu.Unlock()
	}, true
}
