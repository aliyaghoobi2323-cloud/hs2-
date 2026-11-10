package tlscarrier

// The "reality" front (phase 2, see docs/reality-architecture.md).
//
// Dial side: a uTLS Chrome ClientHello exactly like the tls front, but with
// the SNI of a real site (one of the profile's server_names) and a hidden
// signal in its 32-byte legacy session_id:
//
//	shared  = X25519(client ephemeral of the hello's own X25519 key_share, listener static)
//	authKey = HKDF-SHA256(shared, salt = client_random[:20], info = "hs2-reality-v1")
//	plain   = [ver=1][0 0 0][unix time u32][short_id 8]            (16 bytes)
//	sid     = AES-256-GCM(authKey).Seal(nonce = client_random[20:32], plain,
//	          aad = the ClientHello handshake message with a zeroed session_id)
//
// So the hello stays byte-plausible (Chrome sends a random 32-byte session_id
// too) and a modified byte anywhere in it breaks the AEAD. The listener
// (which alone holds the static private key) answers an authenticated hello
// with its OWN TLS 1.3 handshake and a throw-away ECDSA certificate that
// carries HMAC-SHA256(authKey, SPKI) in a private extension; the dialer
// checks that tag (no CA), and TLS 1.3's CertificateVerify proves the
// listener holds the certificate's key. Then the unchanged tunnel auth
// (tls-exporter bound, mutual; auth.go) runs on top.
//
// Anything else — no/invalid signal, wrong SNI, stale time, unknown short id,
// a replayed hello, not TLS at all — is spliced byte for byte (including the
// already-read bytes) to the real target, never decrypted or altered, with
// limits (after FallbackMaxBytes per direction the rate is capped at
// FallbackRateBps; FallbackMaxSeconds caps the duration).

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	utls "github.com/refraction-networking/utls"
)

const (
	realityVersion  = 1
	realitySkew     = 90 * time.Second
	realityInfo     = "hs2-reality-v1"
	realityMaxHello = 32 << 10

	// Fallback defaults (applied when the profile leaves them 0).
	DefaultFallbackMaxBytes   = 256 << 10
	DefaultFallbackRateBps    = 64 << 10
	DefaultFallbackMaxSeconds = 600
)

// realityCertOID is the private extension carrying the cert's tag.
var realityCertOID = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 55738, 1, 1}

// realityNow / realityDialNow are the listener's and the dialer's clocks
// (tests move them apart).
var realityNow, realityDialNow = time.Now, time.Now

// ---- keys ----

// RealityKeygen returns a fresh x25519 key pair, base64url (no padding).
func RealityKeygen() (priv, pub string, err error) {
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	e := base64.RawURLEncoding
	return e.EncodeToString(k.Bytes()), e.EncodeToString(k.PublicKey().Bytes()), nil
}

// RealityPublicOf derives the public key of a base64url private key.
func RealityPublicOf(priv string) (string, error) {
	b, err := parseKey32(priv)
	if err != nil {
		return "", err
	}
	k, err := ecdh.X25519().NewPrivateKey(b)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(k.PublicKey().Bytes()), nil
}

func parseKey32(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	for _, e := range []*base64.Encoding{base64.RawURLEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.StdEncoding} {
		if b, err := e.DecodeString(s); err == nil && len(b) == 32 {
			return b, nil
		}
	}
	if b, err := hex.DecodeString(s); err == nil && len(b) == 32 {
		return b, nil
	}
	return nil, errors.New("reality: key must be 32 bytes (base64url or hex)")
}

// parseShortID: hex, 0..16 digits, right-padded with zeros to 8 bytes (like Xray).
func parseShortID(s string) ([8]byte, error) {
	var id [8]byte
	s = strings.TrimSpace(s)
	if len(s) > 16 || len(s)%2 != 0 {
		return id, fmt.Errorf("reality: short_id %q: want 0..16 hex digits, even length", s)
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return id, fmt.Errorf("reality: short_id %q: %v", s, err)
	}
	copy(id[:], b)
	return id, nil
}

// validateReality checks a reality profile's own fields. side: "dial",
// "listen" or "" (both / unknown).
func (p *FrontProfile) validateReality() error {
	if len(p.ServerNames) == 0 {
		return errors.New("reality: server_names is empty")
	}
	for _, n := range p.ServerNames {
		if n == "" || strings.ContainsAny(n, " /:") {
			return fmt.Errorf("reality: bad server name %q", n)
		}
	}
	if len(p.ShortIDs) == 0 {
		return errors.New("reality: short_ids is empty")
	}
	for _, s := range p.ShortIDs {
		if _, err := parseShortID(s); err != nil {
			return err
		}
	}
	if p.PrivateKey == "" && p.PublicKey == "" {
		return errors.New("reality: need private_key (listener) or public_key (dialer)")
	}
	if p.PrivateKey != "" {
		pub, err := RealityPublicOf(p.PrivateKey)
		if err != nil {
			return err
		}
		if p.PublicKey != "" {
			if b, err := parseKey32(p.PublicKey); err != nil || base64.RawURLEncoding.EncodeToString(b) != pub {
				return errors.New("reality: public_key does not match private_key")
			}
		}
	} else if _, err := parseKey32(p.PublicKey); err != nil {
		return err
	}
	if p.PrivateKey != "" { // the listener splices to the target
		if _, _, err := net.SplitHostPort(p.Target); err != nil {
			return fmt.Errorf("reality: target %q: want host:port", p.Target)
		}
	}
	if p.FallbackMaxBytes < 0 || p.FallbackRateBps < 0 || p.FallbackMaxSeconds < 0 {
		return errors.New("reality: negative fallback limit")
	}
	return nil
}

// ---- signal ----

func realityAuthKey(shared, random []byte) ([]byte, error) {
	return hkdf.Key(sha256.New, shared, random[:20], realityInfo, 32)
}

func realityAEAD(authKey []byte) cipher.AEAD {
	b, _ := aes.NewCipher(authKey)
	a, _ := cipher.NewGCM(b)
	return a
}

func realityPlain(t time.Time, sid [8]byte) []byte {
	p := make([]byte, 16)
	p[0] = realityVersion
	binary.BigEndian.PutUint32(p[4:], uint32(t.Unix()))
	copy(p[8:], sid[:])
	return p
}

// certTag binds a certificate's public key to this connection's authKey.
func certTag(authKey, spki []byte) []byte {
	m := hmac.New(sha256.New, authKey)
	m.Write([]byte("hs2-reality-cert"))
	m.Write(spki)
	return m.Sum(nil)
}

// ---- ClientHello parsing ----

type realityHello struct {
	msg      []byte // the handshake message (type..end), a copy
	random   []byte
	sidOff   int // offset of session_id within msg
	sid      []byte
	sni      string
	x25519   [][]byte // candidate x25519 public keys (0x001d share, then the hybrid's tail)
	complete bool
}

var errNotHello = errors.New("not a ClientHello")

// parseRealityHello reassembles the ClientHello from TLS records in b.
// complete=false with a nil error means more bytes are needed.
func parseRealityHello(b []byte) (*realityHello, error) {
	var msg []byte
	need := -1
	// Decide "not a hello" from the first bytes already, so garbage is spliced
	// at once (a real TLS server answers garbage without waiting for more).
	if len(b) >= 1 && b[0] != 22 || len(b) >= 2 && b[1] != 3 || len(b) >= 6 && b[5] != 1 {
		return nil, errNotHello
	}
	for off := 0; ; {
		if len(b)-off < 5 {
			return &realityHello{}, nil
		}
		if b[off] != 22 || b[off+1] != 3 {
			return nil, errNotHello
		}
		rl := int(b[off+3])<<8 | int(b[off+4])
		if rl == 0 || rl > 1<<14 {
			return nil, errNotHello
		}
		if len(b)-off-5 < rl {
			return &realityHello{}, nil
		}
		msg = append(msg, b[off+5:off+5+rl]...)
		off += 5 + rl
		if need < 0 && len(msg) >= 4 {
			if msg[0] != 1 {
				return nil, errNotHello
			}
			need = 4 + (int(msg[1])<<16 | int(msg[2])<<8 | int(msg[3]))
			if need > realityMaxHello {
				return nil, errNotHello
			}
		}
		if need >= 0 && len(msg) >= need {
			if len(msg) > need {
				return nil, errNotHello // trailing handshake data: not a plain hello
			}
			break
		}
	}
	h := &realityHello{msg: msg, complete: true}
	p := msg[4:]
	if len(p) < 2+32+1 {
		return nil, errNotHello
	}
	h.random = p[2:34]
	sl := int(p[34])
	if sl > 32 || len(p) < 35+sl {
		return nil, errNotHello
	}
	h.sidOff = 4 + 35
	h.sid = p[35 : 35+sl]
	p = p[35+sl:]
	if len(p) < 2 {
		return nil, errNotHello
	}
	cl := int(binary.BigEndian.Uint16(p))
	if len(p) < 2+cl+1 {
		return nil, errNotHello
	}
	p = p[2+cl:]
	ml := int(p[0])
	if len(p) < 1+ml {
		return nil, errNotHello
	}
	p = p[1+ml:]
	if len(p) < 2 {
		return h, nil // no extensions
	}
	el := int(binary.BigEndian.Uint16(p))
	p = p[2:]
	if len(p) < el {
		return nil, errNotHello
	}
	p = p[:el]
	var x, hyb [][]byte
	for len(p) >= 4 {
		t := binary.BigEndian.Uint16(p)
		l := int(binary.BigEndian.Uint16(p[2:]))
		if len(p) < 4+l {
			return nil, errNotHello
		}
		d := p[4 : 4+l]
		p = p[4+l:]
		switch t {
		case 0: // server_name
			if len(d) >= 5 && d[2] == 0 {
				nl := int(binary.BigEndian.Uint16(d[3:]))
				if len(d) >= 5+nl {
					h.sni = string(d[5 : 5+nl])
				}
			}
		case 51: // key_share
			if len(d) < 2 {
				break
			}
			d = d[2:]
			for len(d) >= 4 {
				g := binary.BigEndian.Uint16(d)
				kl := int(binary.BigEndian.Uint16(d[2:]))
				if len(d) < 4+kl {
					break
				}
				k := d[4 : 4+kl]
				d = d[4+kl:]
				switch {
				case g == 0x001d && kl == 32:
					x = append(x, k)
				case g == 0x11ec && kl == 1184+32: // X25519MLKEM768: mlkem || x25519
					hyb = append(hyb, k[1184:])
				}
			}
		}
	}
	h.x25519 = append(x, hyb...)
	return h, nil
}

// ---- stats ----

// RealityStats are process-wide counters of the reality listener(s).
type RealityStats struct {
	Authed        int64            `json:"authed"`
	FallbackConns int64            `json:"fallback_conns"`
	FallbackBytes int64            `json:"fallback_bytes"`
	Rejected      map[string]int64 `json:"rejected,omitempty"`
}

var realityStats struct {
	authed, fbConns, fbBytes atomic.Int64
	mu                       sync.Mutex
	rej                      map[string]int64
}

func realityReject(reason string) {
	realityStats.mu.Lock()
	if realityStats.rej == nil {
		realityStats.rej = map[string]int64{}
	}
	realityStats.rej[reason]++
	realityStats.mu.Unlock()
}

// RealityStatsNow snapshots the counters.
func RealityStatsNow() RealityStats {
	s := RealityStats{Authed: realityStats.authed.Load(), FallbackConns: realityStats.fbConns.Load(), FallbackBytes: realityStats.fbBytes.Load()}
	realityStats.mu.Lock()
	if len(realityStats.rej) > 0 {
		s.Rejected = make(map[string]int64, len(realityStats.rej))
		for k, v := range realityStats.rej {
			s.Rejected[k] = v
		}
	}
	realityStats.mu.Unlock()
	return s
}

// String: "authed=.. fallback=.. (bytes) rejected[a=1 b=2]".
func (s RealityStats) String() string {
	var ks []string
	for k := range s.Rejected {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	var b strings.Builder
	fmt.Fprintf(&b, "authed=%d fallback_conns=%d fallback_bytes=%d", s.Authed, s.FallbackConns, s.FallbackBytes)
	if len(ks) > 0 {
		b.WriteString(" rejected[")
		for i, k := range ks {
			if i > 0 {
				b.WriteByte(' ')
			}
			fmt.Fprintf(&b, "%s=%d", k, s.Rejected[k])
		}
		b.WriteByte(']')
	}
	return b.String()
}

// ---- dial side ----

// RealityFront is the reality front's dial side.
type RealityFront struct{}

var _ Dialer = RealityFront{}

// realitySNI: the path's SNI when it is one of the server names, else the first.
func realitySNI(p *FrontProfile, sni string) string {
	for _, n := range p.ServerNames {
		if strings.EqualFold(n, sni) {
			return n
		}
	}
	return p.ServerNames[0]
}

func dialRawTCP(p DialPath) (net.Conn, error) {
	c := p.Connect
	if c <= 0 {
		c = 8 * time.Second
	}
	d := net.Dialer{Timeout: c}
	if p.BindIP != "" {
		ip := net.ParseIP(p.BindIP)
		if ip == nil {
			return nil, fmt.Errorf("tlscarrier: invalid bind_local_ip %q", p.BindIP)
		}
		d.LocalAddr = &net.TCPAddr{IP: ip}
	}
	raw, err := d.Dial("tcp", p.Addr)
	if err != nil {
		return nil, err
	}
	if tc, ok := raw.(*net.TCPConn); ok {
		tc.SetKeepAlive(true)
		tc.SetKeepAlivePeriod(dialKeepAlive())
	}
	tuneTCP(raw)
	return raw, nil
}

// ErrRealityCert: the listener's certificate did not carry our tag (not our
// listener — the real target or an interceptor answered).
var ErrRealityCert = errors.New("tlscarrier: reality listener did not prove the key (wrong public_key/short_id, clock skew > 90 s, or the target answered)")

// DialFront dials p with the reality front.
func (RealityFront) DialFront(_ context.Context, p DialPath) (*Carrier, error) {
	prof := p.Profile
	if prof == nil || prof.PublicKey == "" || len(prof.ServerNames) == 0 || len(prof.ShortIDs) == 0 {
		return nil, errors.New("tlscarrier: reality front needs public_key, server_names, short_ids")
	}
	pk, err := parseKey32(prof.PublicKey)
	if err != nil {
		return nil, err
	}
	spub, err := ecdh.X25519().NewPublicKey(pk)
	if err != nil {
		return nil, err
	}
	sid8, err := parseShortID(prof.ShortIDs[0])
	if err != nil {
		return nil, err
	}
	raw, err := dialRawTCP(p)
	if err != nil {
		return nil, err
	}
	var authKey []byte
	var verified bool
	cfg := &utls.Config{ServerName: realitySNI(prof, p.SNI), InsecureSkipVerify: true}
	cfg.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 || authKey == nil {
			return ErrRealityCert
		}
		c, err := x509.ParseCertificate(rawCerts[0])
		if err != nil {
			return ErrRealityCert
		}
		want := certTag(authKey, c.RawSubjectPublicKeyInfo)
		for _, e := range c.Extensions {
			if e.Id.Equal(realityCertOID) && len(e.Value) >= 2+32 &&
				subtle.ConstantTimeCompare(e.Value[2:2+32], want) == 1 {
				verified = true
				return nil
			}
		}
		return ErrRealityCert
	}
	u := utls.UClient(wrapBatch(raw), cfg, clientHello)
	fail := func(err error) (*Carrier, error) { raw.Close(); return nil, err }
	if err := u.BuildHandshakeState(); err != nil {
		return fail(err)
	}
	hello := u.HandshakeState.Hello
	if len(hello.Raw) < 39+32 || hello.Raw[38] != 32 || len(hello.Random) != 32 {
		return fail(errors.New("tlscarrier: reality: preset has no 32-byte session_id"))
	}
	ks := u.HandshakeState.State13.KeyShareKeys
	var eph *ecdh.PrivateKey
	if ks != nil {
		eph = ks.Ecdhe
		if eph == nil {
			eph = ks.MlkemEcdhe
		}
	}
	if eph == nil {
		return fail(errors.New("tlscarrier: reality: preset has no x25519 key share"))
	}
	shared, err := eph.ECDH(spub)
	if err != nil {
		return fail(err)
	}
	if authKey, err = realityAuthKey(shared, hello.Random); err != nil {
		return fail(err)
	}
	for i := 39; i < 39+32; i++ {
		hello.Raw[i] = 0
	}
	sid := realityAEAD(authKey).Seal(nil, hello.Random[20:32], realityPlain(realityDialNow(), sid8), hello.Raw)
	hello.SessionId = sid
	copy(hello.Raw[39:], sid)
	raw.SetDeadline(time.Now().Add(authTimeout))
	if err := u.Handshake(); err != nil {
		return fail(err)
	}
	if !verified {
		return fail(ErrRealityCert)
	}
	ekm, err := clientEKM(u, cfg)
	if err != nil {
		return fail(err)
	}
	rec, nonce := makeClientAuth(p.Key, ekm)
	if _, err := u.Write(rec); err != nil {
		return fail(err)
	}
	if err := readServerProof(u, p.Key, nonce, ekm); err != nil {
		return fail(err)
	}
	raw.SetDeadline(time.Time{})
	return &Carrier{conn: u}, nil
}

// ---- listen side ----

// RealityServer is the reality front's listen side. Inner supplies the
// tunnel's shared key, replay memory and logging (its cert/cover are unused).
type RealityServer struct {
	prof     FrontProfile
	inner    *Server
	priv     *ecdh.PrivateKey
	shortIDs map[[8]byte]bool
	names    map[string]bool
	certKey  *ecdsa.PrivateKey
	replay   *replayMem
	maxBytes int64
	rateBps  int64
	maxDur   time.Duration
	// CertPad, if > 0, pads the throw-away certificate to about this many
	// bytes (size mirroring of the target's chain; phase 3 fills it).
	CertPad atomic.Int64
	// DialTarget dials the target (tests override; nil = TCP 5 s).
	DialTarget func(ctx context.Context, addr string) (net.Conn, error)
}

var _ Acceptor = (*RealityServer)(nil)

// NewRealityServer builds the listener of a validated reality profile.
func NewRealityServer(p *FrontProfile, inner *Server) (*RealityServer, error) {
	if p == nil || p.NormKind() != FrontReality {
		return nil, errors.New("reality: not a reality profile")
	}
	if err := p.validateReality(); err != nil {
		return nil, err
	}
	if p.PrivateKey == "" {
		return nil, errors.New("reality: the listener needs private_key")
	}
	kb, _ := parseKey32(p.PrivateKey)
	priv, err := ecdh.X25519().NewPrivateKey(kb)
	if err != nil {
		return nil, err
	}
	ck, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	r := &RealityServer{prof: *p, inner: inner, priv: priv, certKey: ck, replay: newReplayMem(),
		shortIDs: map[[8]byte]bool{}, names: map[string]bool{}}
	for _, s := range p.ShortIDs {
		id, _ := parseShortID(s)
		r.shortIDs[id] = true
	}
	for _, n := range p.ServerNames {
		r.names[strings.ToLower(n)] = true
	}
	r.maxBytes, r.rateBps, r.maxDur = p.FallbackMaxBytes, p.FallbackRateBps, time.Duration(p.FallbackMaxSeconds)*time.Second
	if r.maxBytes == 0 {
		r.maxBytes = DefaultFallbackMaxBytes
	}
	if r.rateBps == 0 {
		r.rateBps = DefaultFallbackRateBps
	}
	if r.maxDur == 0 {
		r.maxDur = DefaultFallbackMaxSeconds * time.Second
	}
	if inner != nil {
		inner.init()
	}
	return r, nil
}

// check opens the signal of a complete hello. It returns the authKey or the
// reject reason.
func (r *RealityServer) check(h *realityHello) ([]byte, string) {
	if !r.names[strings.ToLower(h.sni)] {
		return nil, "sni"
	}
	if len(h.sid) != 32 {
		return nil, "no_session_id"
	}
	if len(h.x25519) == 0 {
		return nil, "no_x25519"
	}
	aad := append([]byte(nil), h.msg...)
	for i := h.sidOff; i < h.sidOff+32; i++ {
		aad[i] = 0
	}
	for _, kpub := range h.x25519 {
		pub, err := ecdh.X25519().NewPublicKey(kpub)
		if err != nil {
			continue
		}
		shared, err := r.priv.ECDH(pub)
		if err != nil {
			continue
		}
		ak, err := realityAuthKey(shared, h.random)
		if err != nil {
			continue
		}
		plain, err := realityAEAD(ak).Open(nil, h.random[20:32], h.sid, aad)
		if err != nil || len(plain) != 16 {
			continue
		}
		if plain[0] != realityVersion {
			return nil, "version"
		}
		t := time.Unix(int64(binary.BigEndian.Uint32(plain[4:8])), 0)
		if d := realityNow().Sub(t); d > realitySkew || d < -realitySkew {
			return nil, "time"
		}
		var id [8]byte
		copy(id[:], plain[8:16])
		if !r.shortIDs[id] {
			return nil, "short_id"
		}
		if !r.replay.add(h.sid[:16]) {
			return nil, "replay"
		}
		return ak, ""
	}
	return nil, "signal"
}

// prefixConn replays already-read bytes before the socket's.
type prefixConn struct {
	net.Conn
	pre []byte
}

func (c *prefixConn) Read(b []byte) (int, error) {
	if len(c.pre) > 0 {
		n := copy(b, c.pre)
		c.pre = c.pre[n:]
		return n, nil
	}
	return c.Conn.Read(b)
}

// NetConn exposes the socket (Carrier.TCPConn walks wrappers with it).
func (c *prefixConn) NetConn() net.Conn { return c.Conn }

// AcceptFront: read the ClientHello (bounded, with a deadline) while
// forwarding it to the target as it arrives; an authenticated hello gets our
// own TLS 1.3 and the tunnel auth, anything else the target, byte for byte.
func (r *RealityServer) AcceptFront(ctx context.Context, raw net.Conn, onTunnel func(*Carrier)) {
	tuneTCP(raw)
	if QuietIdle {
		if tc, ok := raw.(*net.TCPConn); ok {
			tc.SetKeepAlive(true)
			tc.SetKeepAlivePeriod(quietKeepAlive)
		}
	}
	type dres struct {
		c   net.Conn
		err error
	}
	tch := make(chan dres, 1)
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	go func() {
		defer cancel()
		var c net.Conn
		var err error
		if r.DialTarget != nil {
			c, err = r.DialTarget(dctx, r.prof.Target)
		} else {
			var d net.Dialer
			c, err = d.DialContext(dctx, "tcp", r.prof.Target)
		}
		tch <- dres{c, err}
	}()
	var target net.Conn
	targetErr := false
	getTarget := func(block bool) {
		if target != nil || targetErr {
			return
		}
		if block {
			x := <-tch
			target, targetErr = x.c, x.err != nil
			return
		}
		select {
		case x := <-tch:
			target, targetErr = x.c, x.err != nil
		default:
		}
	}
	buf := make([]byte, 0, 4096)
	sent := 0
	tmp := make([]byte, 4096)
	raw.SetReadDeadline(time.Now().Add(handshakeTimeout))
	reason := ""
	var hello *realityHello
	for {
		n, err := raw.Read(tmp)
		buf = append(buf, tmp[:n]...)
		getTarget(false)
		if target != nil && sent < len(buf) {
			if _, werr := target.Write(buf[sent:]); werr == nil {
				sent = len(buf)
			}
		}
		h, perr := parseRealityHello(buf)
		if perr != nil {
			reason = "not_hello"
			break
		}
		if h.complete {
			hello = h
			break
		}
		if err != nil {
			reason = "read"
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				reason = "timeout"
			}
			break
		}
		if len(buf) > realityMaxHello+1024 {
			reason = "not_hello"
			break
		}
	}
	raw.SetReadDeadline(time.Time{})
	var authKey []byte
	if hello != nil {
		authKey, reason = r.check(hello)
	}
	if authKey == nil {
		realityReject(reason)
		getTarget(true)
		r.fallback(ctx, raw, target, buf[sent:])
		return
	}
	// Authenticated: drop the target and serve TLS 1.3 ourselves.
	go func() {
		getTarget(true)
		if target != nil {
			target.Close()
		}
	}()
	r.serveTunnel(ctx, raw, buf, hello, authKey, onTunnel)
}

func (r *RealityServer) makeCert(authKey []byte, sni string) (*tls.Certificate, error) {
	spki, err := x509.MarshalPKIXPublicKey(&r.certKey.PublicKey)
	if err != nil {
		return nil, err
	}
	val := append([]byte{0x04, 32}, certTag(authKey, spki)...) // OCTET STRING
	exts := []pkix.Extension{{Id: realityCertOID, Value: val}}
	if pad := r.CertPad.Load(); pad > 600 {
		p := make([]byte, pad-600)
		rand.Read(p)
		v, _ := asn1.Marshal(p)
		exts = append(exts, pkix.Extension{Id: asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 55738, 1, 2}, Value: v})
	}
	sn, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	now := time.Now()
	tmpl := &x509.Certificate{SerialNumber: sn, Subject: pkix.Name{CommonName: sni}, DNSNames: []string{sni},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(90 * 24 * time.Hour), ExtraExtensions: exts,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &r.certKey.PublicKey, r.certKey)
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: r.certKey}, nil
}

func (r *RealityServer) serveTunnel(ctx context.Context, raw net.Conn, pre []byte, h *realityHello, authKey []byte, onTunnel func(*Carrier)) {
	cert, err := r.makeCert(authKey, h.sni)
	if err != nil {
		realityReject("cert")
		raw.Close()
		return
	}
	tcfg := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{*cert},
		NextProtos: []string{"h2", "http/1.1"}, SessionTicketsDisabled: true}
	tconn := tls.Server(&prefixConn{Conn: wrapBatch(raw), pre: pre}, tcfg)
	tconn.SetDeadline(time.Now().Add(handshakeTimeout))
	if err := tconn.Handshake(); err != nil {
		realityReject("handshake")
		raw.Close()
		return
	}
	cs := tconn.ConnectionState()
	ekm, err := exportEKM(&cs)
	if err != nil {
		raw.Close()
		return
	}
	tconn.SetDeadline(time.Now().Add(authTimeout))
	b := make([]byte, 16<<10)
	n, _ := tconn.Read(b)
	var key []byte
	if r.inner != nil {
		key = r.inner.SharedKey
	}
	nonce, more, ok := parseClientAuth(key, ekm, b[:n])
	if !ok {
		// The signal proves the reality key, not the tunnel key: a peer of a
		// different tunnel sharing the reality key. Nothing to cover here.
		realityReject("tunnel_auth")
		tconn.Close()
		return
	}
	if more > 0 {
		if _, err := io.CopyN(io.Discard, tconn, int64(more)); err != nil {
			tconn.Close()
			return
		}
	}
	if r.inner != nil && !r.inner.replay.add(nonce) {
		realityReject("tunnel_replay")
		tconn.Close()
		return
	}
	if _, err := tconn.Write(makeServerProof(key, nonce, ekm)); err != nil {
		tconn.Close()
		return
	}
	tconn.SetDeadline(time.Time{})
	realityStats.authed.Add(1)
	onTunnel(&Carrier{conn: tconn})
}

// fallback splices client <-> target byte for byte (pending = client bytes
// read but not yet sent to the target), with the limits.
func (r *RealityServer) fallback(ctx context.Context, client, target net.Conn, pending []byte) {
	defer client.Close()
	if target == nil {
		realityReject("target_dial")
		return // like an unreachable site: the connection just closes
	}
	defer target.Close()
	realityStats.fbConns.Add(1)
	if len(pending) > 0 {
		if _, err := target.Write(pending); err != nil {
			return
		}
	}
	dl := time.Now().Add(r.maxDur)
	client.SetDeadline(dl)
	target.SetDeadline(dl)
	var wg sync.WaitGroup
	wg.Add(2)
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		r.limitedCopy(dst, src)
		if c, ok := dst.(interface{ CloseWrite() error }); ok {
			c.CloseWrite()
		} else {
			dst.Close()
		}
	}
	go cp(target, client)
	go cp(client, target)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-ctx.Done():
		client.Close()
		target.Close()
		<-done
	case <-done:
	}
}

// limitedCopy: free up to maxBytes, then at most rateBps.
func (r *RealityServer) limitedCopy(dst, src net.Conn) {
	b := make([]byte, 32<<10)
	var total int64
	var start time.Time
	var after int64
	for {
		chunk := b
		if total >= r.maxBytes && len(chunk) > 16<<10 {
			chunk = b[:16<<10]
		}
		n, err := src.Read(chunk)
		if n > 0 {
			if _, werr := dst.Write(b[:n]); werr != nil {
				return
			}
			total += int64(n)
			realityStats.fbBytes.Add(int64(n))
			if total > r.maxBytes {
				if start.IsZero() {
					start = time.Now()
					after = total - r.maxBytes
				} else {
					after += int64(n)
				}
				want := time.Duration(float64(after) / float64(r.rateBps) * float64(time.Second))
				if d := want - time.Since(start); d > 0 {
					time.Sleep(d)
				}
			}
		}
		if err != nil {
			return
		}
	}
}
