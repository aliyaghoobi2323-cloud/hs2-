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
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/ed25519"
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
	mrand "math/rand/v2"
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
	DefaultFallbackMaxConns   = 256
	DefaultFallbackMaxPerNet  = 16
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

// realityCapPad (signal byte 1, bit 0): the dialer skips post-handshake pad
// records (NewSessionTicket mirroring, phase 4). Old listeners ignore bytes
// 1-3; a listener sends pads only to a dialer that set the bit.
const realityCapPad = 1

// realityDialCaps is what this dialer announces (tests: 0 = an old dialer).
var realityDialCaps byte = realityCapPad

func realityPlain(t time.Time, sid [8]byte) []byte {
	p := make([]byte, 16)
	p[0] = realityVersion
	p[1] = realityDialCaps
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
		return nil, &ConnectError{err}
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
	if err := readPadsThenProof(u, p.Key, nonce, ekm); err != nil {
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
	certKey  *ecdsa.PrivateKey  // the default (P-256) throw-away key
	issuer   ed25519.PrivateKey // signs the throw-away certs (fixed-size signature)
	padHint  sync.Map           // cert pad sizes that hit the wanted size
	// key: the throw-away cert's key in use (the target leaf's type/size once
	// measured, phase 4), with its cert base sizes.
	key atomic.Pointer[certKey]
	// floorWarned: the "target chain below our minimum" warning was logged.
	floorWarned atomic.Int64
	replay      *replayMem
	maxBytes    int64
	rateBps     int64
	maxDur      time.Duration
	// CertPad, if > 0, pads the throw-away certificate to about this many
	// bytes: size mirroring of the target's certificate chain, so the
	// encrypted Certificate message is about as long as the target's
	// (StartSizeMirror keeps it fresh).
	CertPad atomic.Int64
	baseLen int // DER length of an unpadded throw-away cert
	// mirror: the target's observed handshake choices (StartSizeMirror),
	// copied by the authenticated handshake so a passive observer sees the
	// target's ServerHello group and ALPN (phase 4).
	mirror atomic.Pointer[targetMirror]
	// DialTarget dials the target (tests override; nil = TCP 5 s).
	DialTarget func(ctx context.Context, addr string) (net.Conn, error)
	// Hybrid ("both", phase 3): a hello without a valid reality signal whose
	// SNI TLSName accepts goes to TLS (the tls front's Server.Handle over the
	// already-read bytes); anything else is cover (the target splice).
	TLS     *Server
	TLSName func(local net.Addr, sni string) bool
	// Counters of this listener (one per lb IP): reality / tls / cover.
	Counters FrontCounters
	// concurrent cover splices: this listener and per client /24 (/64)
	fbMu            sync.Mutex
	fbAll           int
	fbNet           map[string]int
	fbMax, fbMaxNet int
	// budget: free cover bytes per client net per minute (sec41)
	budget netBudget
	// pre: connections before their hello (sec41)
	pre preCount
}

func clientNet(a net.Addr) string {
	ta, ok := a.(*net.TCPAddr)
	if !ok {
		return ""
	}
	if v4 := ta.IP.To4(); v4 != nil {
		return v4.Mask(net.CIDRMask(24, 32)).String()
	}
	return ta.IP.Mask(net.CIDRMask(64, 128)).String()
}

// fbAcquire takes a cover-splice slot for a client; false when this listener
// or the client's /24 is at its cap.
func (r *RealityServer) fbAcquire(a net.Addr) (release func(), ok bool) {
	k := clientNet(a)
	r.fbMu.Lock()
	defer r.fbMu.Unlock()
	if r.fbNet == nil {
		r.fbNet = map[string]int{}
	}
	if r.fbMax > 0 && r.fbAll >= r.fbMax || r.fbMaxNet > 0 && r.fbNet[k] >= r.fbMaxNet {
		return nil, false
	}
	r.fbAll++
	r.fbNet[k]++
	return func() {
		r.fbMu.Lock()
		r.fbAll--
		if r.fbNet[k]--; r.fbNet[k] <= 0 {
			delete(r.fbNet, k)
		}
		r.fbMu.Unlock()
	}, true
}

// capClose ends a connection over the cover cap after its hello was read: a
// FIN (CloseWrite), then the client's remaining bytes are drained for up to
// 2 s so the close never becomes an RST (closing with unread data does) —
// what a busy server that accepts and then hangs up looks like.
func capClose(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	}
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	io.Copy(io.Discard, io.LimitReader(c, 64<<10))
	c.Close()
}

var _ Acceptor = (*RealityServer)(nil)

// NewRealityServer builds the listener of a validated reality (or "both")
// profile.
func NewRealityServer(p *FrontProfile, inner *Server) (*RealityServer, error) {
	if p == nil || (p.NormKind() != FrontReality && p.NormKind() != FrontBoth) {
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
	_, iss, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	r := &RealityServer{prof: *p, inner: inner, priv: priv, certKey: ck, issuer: iss, replay: newReplayMem(),
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
	r.fbMax, r.fbMaxNet = p.FallbackMaxConns, p.FallbackMaxPerNet
	if r.fbMax <= 0 {
		r.fbMax = DefaultFallbackMaxConns
	}
	if r.fbMaxNet <= 0 {
		r.fbMaxNet = DefaultFallbackMaxPerNet
	}
	r.fbNet = map[string]int{}
	if inner != nil {
		inner.init()
	}
	k, err := r.newCertKey(ck, "ecdsa", 256)
	if err != nil {
		return nil, err
	}
	r.key.Store(k)
	r.baseLen = k.base
	return r, nil
}

// newCertKey measures the cert base sizes (normal and minimal form) of a key.
func (r *RealityServer) newCertKey(s crypto.Signer, kind string, bits int) (*certKey, error) {
	k := &certKey{signer: s, kind: kind, bits: bits} // signed by the Ed25519 issuer: fixed size
	for _, minimal := range []bool{false, true} {
		der, err := r.buildCert(k, make([]byte, 32), r.prof.ServerNames[0], 0, minimal)
		if err != nil {
			return nil, err
		}
		if minimal {
			k.minLen = len(der)
		} else {
			k.base = len(der)
		}
	}
	return k, nil
}

// useLeafKey switches the throw-away cert to the target leaf's key type and
// size (cached per process; generated outside the handshake path).
func (r *RealityServer) useLeafKey(kind string, bits int) error {
	if cur := r.key.Load(); cur != nil && cur.kind == kind && cur.bits == bits {
		return nil
	}
	var s crypto.Signer
	if kind == "ecdsa" && bits == 256 {
		s = r.certKey
	} else {
		var err error
		if s, err = cachedKey(kind, bits); err != nil {
			return err
		}
	}
	k, err := r.newCertKey(s, kind, bits)
	if err != nil {
		return err
	}
	r.key.Store(k)
	return nil
}

// targetMirror is what the target chose for a Chrome-like offer (X25519MLKEM768
// + X25519, ALPN h2/http1.1): the key-share group and ALPN.
type targetMirror struct {
	Curve tls.CurveID
	ALPN  string
	// leaf key (CertificateVerify size) and the record lengths of the
	// target's post-handshake NewSessionTickets (phase 4)
	KeyKind string
	KeyBits int
	NST     []int // one probe's ticket records (the count)
	// NSTSizes: every ticket record length seen over nstProbes probes (an
	// OpenSSL ticket is 266 or 282 B at random): a pad picks one of these.
	NSTSizes []int
	// OCSP / SCTs: what the target staples in its Certificate message (the
	// authenticated handshake staples the same, so the record is as long)
	OCSP []byte
	SCTs [][]byte
}

// nstProbes: handshakes per mirror refresh that sample the ticket sizes.
const nstProbes = 4

// sampleNST adds nstProbes-1 more ticket size samples to the mirror (after
// the cert size is installed: the first connections are not kept waiting).
func (r *RealityServer) sampleNST(ctx context.Context) {
	m := r.mirror.Load()
	if m == nil || len(m.NST) == 0 {
		return
	}
	sizes := append([]int(nil), m.NST...)
	for i := 1; i < nstProbes; i++ {
		sizes = append(sizes, r.probeNST(ctx)...)
	}
	m2 := *m
	m2.NSTSizes = sizes
	r.mirror.CompareAndSwap(m, &m2)
}

// drainFor reads (and drops) whatever comes for d: a ticket is consumed
// inside Read, but a server that sends application data would end Read early.
func drainFor(c *tls.Conn, d time.Duration) {
	c.SetReadDeadline(time.Now().Add(d))
	b := make([]byte, 4096)
	for {
		if _, err := c.Read(b); err != nil {
			return
		}
	}
}

// probeNST does one handshake with the target and returns its ticket record
// lengths.
func (r *RealityServer) probeNST(ctx context.Context) []int {
	dctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	var raw net.Conn
	var err error
	if r.DialTarget != nil {
		raw, err = r.DialTarget(dctx, r.prof.Target)
	} else {
		var d net.Dialer
		raw, err = d.DialContext(dctx, "tcp", r.prof.Target)
	}
	if err != nil {
		return nil
	}
	defer raw.Close()
	raw.SetDeadline(time.Now().Add(8 * time.Second))
	tap := &recordTap{Conn: raw}
	c := tls.Client(tap, &tls.Config{ServerName: r.prof.ServerNames[0], InsecureSkipVerify: true, NextProtos: []string{"h2", "http/1.1"}})
	if c.Handshake() != nil {
		return nil
	}
	i0 := tap.count()
	drainFor(c, nstWait)
	return tap.since(i0)
}

// Mirror returns the target's observed group and ALPN (nil before the first
// measurement).
func (r *RealityServer) Mirror() *targetMirror { return r.mirror.Load() }

// targetChainSize is what the target's Certificate message carries: the
// chain's DER bytes plus the per-entry overhead beyond the first entry (our
// message has one entry).
func (r *RealityServer) targetChainSize(ctx context.Context) (int64, error) {
	var raw net.Conn
	var err error
	dctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if r.DialTarget != nil {
		raw, err = r.DialTarget(dctx, r.prof.Target)
	} else {
		var d net.Dialer
		raw, err = d.DialContext(dctx, "tcp", r.prof.Target)
	}
	if err != nil {
		return 0, err
	}
	defer raw.Close()
	raw.SetDeadline(time.Now().Add(8 * time.Second))
	tap := &recordTap{Conn: raw}
	c := tls.Client(tap, &tls.Config{ServerName: r.prof.ServerNames[0], InsecureSkipVerify: true, NextProtos: []string{"h2", "http/1.1"}})
	if err := c.Handshake(); err != nil {
		return 0, err
	}
	// The tickets come after our Finished (which Handshake just sent): every
	// record from here on within nstWait is one.
	i0 := tap.count()
	drainFor(c, nstWait)
	var n int64
	cs := c.ConnectionState()
	m := &targetMirror{Curve: cs.CurveID, ALPN: cs.NegotiatedProtocol, NST: tap.since(i0), OCSP: cs.OCSPResponse, SCTs: cs.SignedCertificateTimestamps}
	if len(cs.PeerCertificates) > 0 {
		m.KeyKind, m.KeyBits = leafKey(cs.PeerCertificates[0])
	}
	if m.KeyKind != "" {
		if err := r.useLeafKey(m.KeyKind, m.KeyBits); err != nil {
			m.KeyKind = "" // keep the default key
		}
	}
	m.NSTSizes = append([]int(nil), m.NST...)
	r.mirror.Store(m)
	for i, crt := range cs.PeerCertificates {
		n += int64(len(crt.Raw))
		if i > 0 {
			n += 5
		}
	}
	return n, nil
}

// StartSizeMirror measures the target's certificate chain now and every
// `every` (0 = 3 h) until ctx ends, and pads the throw-away certificate to it.
func (r *RealityServer) StartSizeMirror(ctx context.Context, every time.Duration, logf func(string, ...any)) {
	if every <= 0 {
		every = 3 * time.Hour
	}
	go func() {
		for {
			if n, err := r.targetChainSize(ctx); err == nil {
				if old := r.CertPad.Swap(n); old != n && logf != nil {
					m, k := r.Mirror(), r.key.Load()
					logf("front: reality: target certificate chain %d B, leaf %s %d, tickets %v; mirroring them", n, m.KeyKind, m.KeyBits, m.NST)
					if k != nil && n < int64(k.minLen) && r.floorWarned.Swap(n) != n {
						logf("front: reality: WARNING the target's certificate chain (%d B) is smaller than our smallest throw-away certificate (%d B): an authenticated handshake's Certificate record is %d B longer than the target's (a passive observer can tell); pick a target with a larger chain", n, k.minLen, int64(k.minLen)-n)
					}
				}
				r.sampleNST(ctx)
			} else if logf != nil {
				logf("front: reality: could not measure the target's certificate chain: %v", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(every):
			}
		}
	}()
}

// check opens the signal of a complete hello. It returns the authKey or the
// reject reason.
func (r *RealityServer) check(h *realityHello) ([]byte, string) {
	ak, _, reason := r.checkCaps(h)
	return ak, reason
}

// checkCaps is check plus the dialer's capability bits (signal byte 1).
func (r *RealityServer) checkCaps(h *realityHello) ([]byte, byte, string) {
	if !r.names[strings.ToLower(h.sni)] {
		return nil, 0, "sni"
	}
	if len(h.sid) != 32 {
		return nil, 0, "no_session_id"
	}
	if len(h.x25519) == 0 {
		return nil, 0, "no_x25519"
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
			return nil, 0, "version"
		}
		t := time.Unix(int64(binary.BigEndian.Uint32(plain[4:8])), 0)
		if d := realityNow().Sub(t); d > realitySkew || d < -realitySkew {
			return nil, 0, "time"
		}
		var id [8]byte
		copy(id[:], plain[8:16])
		if !r.shortIDs[id] {
			return nil, 0, "short_id"
		}
		if !r.replay.add(h.sid[:16]) {
			return nil, 0, "replay"
		}
		return ak, plain[1], ""
	}
	return nil, 0, "signal"
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

// certTries counts throw-away cert signings (benchmarks).
var certTries atomic.Int64

// lazyPreDial: a "both" connection without a complete hello after this long
// dials the target anyway (see AcceptFront).
var lazyPreDial = 10 * time.Millisecond

// dialCoverHTTP is the cover backend of the tls front on a "both" IP: a TLS
// connection to the target (its own name, http/1.1 as our tls front offers),
// so the decrypted request of a client of OUR certificate is answered by the
// target's HTTP server.
func (r *RealityServer) dialCoverHTTP(ctx context.Context) (net.Conn, error) {
	dctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	var raw net.Conn
	var err error
	if r.DialTarget != nil {
		raw, err = r.DialTarget(dctx, r.prof.Target)
	} else {
		var d net.Dialer
		raw, err = d.DialContext(dctx, "tcp", r.prof.Target)
	}
	if err != nil {
		return nil, err
	}
	c := tls.Client(raw, &tls.Config{ServerName: r.prof.ServerNames[0], InsecureSkipVerify: true, NextProtos: []string{"http/1.1"}})
	if err := c.HandshakeContext(dctx); err != nil {
		raw.Close()
		return nil, err
	}
	return c, nil
}

func (r *RealityServer) dialTarget(ctx context.Context, tch chan<- struct {
	c   net.Conn
	err error
}) {
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var c net.Conn
	var err error
	if r.DialTarget != nil {
		c, err = r.DialTarget(dctx, r.prof.Target)
	} else {
		var d net.Dialer
		c, err = d.DialContext(dctx, "tcp", r.prof.Target)
	}
	tch <- struct {
		c   net.Conn
		err error
	}{c, err}
}

// AcceptFront: read the ClientHello (bounded, with a deadline) while
// forwarding it to the target as it arrives; an authenticated hello gets our
// own TLS 1.3 and the tunnel auth, anything else the target, byte for byte.
func (r *RealityServer) AcceptFront(ctx context.Context, raw net.Conn, onTunnel func(*Carrier)) {
	// sec41: every accepted connection dials the target before its hello is
	// read; without a cap a stranger's idle connections were mirrored 1:1 to
	// the target (lab: 200 idle sockets from one /24 -> 200 target
	// connections held 10 s each).
	preRelease, ok := r.preAcquire(raw.RemoteAddr())
	if !ok {
		realityReject("prehello_cap")
		capClose(raw)
		return
	}
	preDone := false
	endPre := func() {
		if !preDone {
			preDone = true
			preRelease()
		}
	}
	defer endPre()
	tuneTCP(raw)
	if QuietIdle {
		if tc, ok := raw.(*net.TCPConn); ok {
			tc.SetKeepAlive(true)
			tc.SetKeepAlivePeriod(quietKeepAlive)
		}
	}
	type dres = struct {
		c   net.Conn
		err error
	}
	tch := make(chan dres, 1)
	// "both" (phase 4): the target is dialed lazily — only once the hello is
	// not ours (or is not a complete hello in the first read: a prober's
	// partial bytes still reach the target as they arrive). Our own tls
	// dials never touch the target. "reality" dials at accept (an
	// authenticated hello needs the target up, see below).
	lazy := r.TLS != nil && r.TLSName != nil
	var dialOnce sync.Once
	var startedA atomic.Bool
	startDial := func() {
		dialOnce.Do(func() {
			startedA.Store(true)
			go r.dialTarget(ctx, tch)
		})
	}
	// isStarted is exact even while the pre-dial timer's startDial runs (Do
	// returns only after the first Do finished).
	isStarted := func() bool { dialOnce.Do(func() {}); return startedA.Load() }
	var preDial *time.Timer
	if !lazy {
		startDial()
	} else {
		// Timing parity (phase 4): a client that connects and stays silent
		// (a prober sending its hello later) would otherwise wait one
		// listener->target connect more for its ServerHello than with an
		// eager dial. Real clients — our own dialer included — send the
		// hello right after the connect, well inside lazyPreDial.
		preDial = time.AfterFunc(lazyPreDial, startDial)
	}
	var target net.Conn
	targetErr := false
	getTarget := func(block bool) {
		if target != nil || targetErr {
			return
		}
		if !startedA.Load() {
			if !block {
				return
			}
			startDial()
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
	_ = dres{}
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
		startDial() // a partial hello: the target sees the bytes as they come
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
	endPre()
	if preDial != nil {
		preDial.Stop()
	}
	var authKey []byte
	var caps byte
	if hello != nil {
		authKey, caps, reason = r.checkCaps(hello)
	}
	if authKey == nil && hello != nil && r.TLS != nil && r.TLSName != nil && r.TLSName(raw.LocalAddr(), hello.sni) {
		// "both": our own tls name -> the tls front. Its cover is the target
		// too (phase 4): an unauthenticated request after our handshake is
		// proxied to the target over TLS, so plain HTTP, other SNIs and our
		// SNI all show one origin on this IP.
		if isStarted() {
			go func() {
				getTarget(true)
				if target != nil {
					target.Close()
				}
			}()
		}
		r.Counters.TLS.Add(1)
		r.TLS.handle(ctx, &prefixConn{Conn: raw, pre: buf}, onTunnel, r.dialCoverHTTP)
		return
	}
	if authKey == nil {
		realityReject(reason)
		r.Counters.Rejected.Add(1)
		release, ok := r.fbAcquire(raw.RemoteAddr())
		if !ok {
			realityReject("fallback_cap")
			if isStarted() {
				go func() {
					getTarget(true)
					if target != nil {
						target.Close()
					}
				}()
			}
			capClose(raw)
			return
		}
		defer release()
		free, rate := r.spliceLimits(hello)
		getTarget(true)
		r.fallback(ctx, raw, target, buf[sent:], free, rate)
		return
	}
	// Authenticated. The listener only answers as the target would: when the
	// target cannot be reached neither can we (no reality without a reachable
	// target; a "both" dialer then falls back to tls). The dial started at
	// accept, so this rarely waits.
	getTarget(true)
	if target == nil {
		realityReject("target_down")
		r.Counters.Rejected.Add(1)
		raw.Close()
		return
	}
	// Timing and choice parity (sec41): the target gets this very hello and
	// our ServerHello leaves only once the target's has arrived (a spliced
	// connection's timing), with the target's suite and group. The cert is
	// made meanwhile.
	type certRes struct {
		c   *tls.Certificate
		err error
	}
	cch := make(chan certRes, 1)
	go func() { c, err := r.makeCert(authKey, hello.sni); cch <- certRes{c, err} }()
	var th targetHello
	thOK := false
	if _, err := target.Write(buf[sent:]); err == nil {
		th, thOK = readTargetHello(target, handshakeTimeout)
	}
	target.Close()
	cr := <-cch
	if cr.err != nil {
		realityReject("cert")
		raw.Close()
		return
	}
	r.serveTunnel(ctx, raw, buf, hello, cr.c, th, thOK, caps, onTunnel)
}

func (r *RealityServer) makeCert(authKey []byte, sni string) (*tls.Certificate, error) {
	k := r.key.Load()
	want := r.CertPad.Load()
	// The minimal form (no subject, no key usages) only when the target's
	// chain is smaller than our normal unpadded cert (phase 4: the lab chain
	// of 387 B was below the old ~412 B floor).
	minimal := want > 0 && want < int64(k.base)
	der, err := r.buildCert(k, authKey, sni, want, minimal)
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: k.signer}, nil
}

// buildCert makes the throw-away cert with key k, exactly want bytes long
// when reachable (want 0: unpadded). Exact size, no jitter: an ECDSA
// signature's DER varies by a byte or two, so re-sign until it has the modal
// length; then nudge the padding until the DER is exactly the target's chain
// size (a passive observer saw 441-443 B vs the target's fixed 417 B before).
func (r *RealityServer) buildCert(k *certKey, authKey []byte, sni string, want int64, minimal bool) ([]byte, error) {
	spki, err := x509.MarshalPKIXPublicKey(k.signer.Public())
	if err != nil {
		return nil, err
	}
	tag := certTag(authKey, spki)
	base := k.base
	if minimal {
		base = k.minLen
	}
	// Two pads: up to tagSlack bytes ride in the tag's OCTET STRING after the
	// 32-byte tag (the dialer reads Value[2:34]; the length stays one byte),
	// a larger gap gets its own extension (~24 B of overhead).
	const tagSlack = 90
	gap := want - int64(base)
	var extra, pad int64
	switch {
	case base == 0 || want == 0 || gap <= 0:
	case gap <= tagSlack:
		extra = gap
	default:
		pad = gap - 24
	}
	// the pad sizes that hit want last time (DER length bytes make the first
	// estimate miss by a byte or two): one signing per cert
	hk := fmt.Sprintf("%p/%d/%v/%d", k, want, minimal, len(sni))
	if v, ok := r.padHint.Load(hk); ok && want > 0 && gap > 0 {
		h := v.([2]int64)
		extra, pad = h[0], h[1]
	}
	var der []byte
	var err2 error
	for try := 0; try < 64; try++ {
		val := append([]byte{0x04, byte(32 + extra)}, tag...) // OCTET STRING
		if extra > 0 {
			x := make([]byte, extra)
			rand.Read(x)
			val = append(val, x...)
		}
		exts := []pkix.Extension{{Id: realityCertOID, Value: val}}
		if pad > 0 {
			p := make([]byte, pad)
			rand.Read(p)
			v, _ := asn1.Marshal(p)
			exts = append(exts, pkix.Extension{Id: asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 55738, 1, 2}, Value: v})
		}
		now := time.Now()
		var tmpl *x509.Certificate
		if minimal {
			sn, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 64))
			sn.SetBit(sn, 63, 1)
			tmpl = &x509.Certificate{SerialNumber: sn, DNSNames: []string{sni},
				NotBefore: now.Add(-time.Hour), NotAfter: now.Add(90 * 24 * time.Hour), ExtraExtensions: exts}
		} else {
			sn, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
			sn.SetBit(sn, 119, 1) // fixed serial length
			tmpl = &x509.Certificate{SerialNumber: sn, Subject: pkix.Name{CommonName: sni}, DNSNames: []string{sni},
				NotBefore: now.Add(-time.Hour), NotAfter: now.Add(90 * 24 * time.Hour), ExtraExtensions: exts,
				KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
		}
		// Always signed by an Ed25519 issuer key: the cert's own signature is
		// seen by nobody but our dialer (which checks only the tag), only its
		// SIZE shows and the padding fixes that. Ed25519's fixed 64-byte
		// signature makes the size exact in one signing (ECDSA's 70-72 B DER
		// took ~4 signings, ~1 ms per cert on the handshake path; an RSA
		// self-signature ~1.5 ms each). The SPKI is the mirrored key, so
		// CertificateVerify is the target's size.
		der, err2 = x509.CreateCertificate(rand.Reader, tmpl, tmpl, k.signer.Public(), r.issuer)
		certTries.Add(1)
		if err2 != nil {
			return nil, err2
		}
		if k.sigLen > 0 {
			if c, perr := x509.ParseCertificate(der); perr != nil || len(c.Signature) != k.sigLen {
				continue
			}
		}
		d := want - int64(len(der))
		if d == 0 && (extra > 0 || pad > 0) {
			r.padHint.Store(hk, [2]int64{extra, pad})
		}
		if extra == 0 && pad == 0 || d == 0 {
			break
		}
		if pad > 0 {
			if pad += d; pad < 1 {
				pad = 1
			}
		} else if extra += d; extra < 0 {
			extra = 0
		} else if extra > tagSlack {
			extra = tagSlack
		}
	}
	return der, nil
}

func (r *RealityServer) tunnelTLSConfig(cert *tls.Certificate) *tls.Config {
	tcfg := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{*cert},
		NextProtos: []string{"h2", "http/1.1"}, SessionTicketsDisabled: true}
	if m := r.mirror.Load(); m != nil {
		if m.Curve != 0 {
			tcfg.CurvePreferences = []tls.CurveID{m.Curve}
		}
		tcfg.NextProtos = nil
		if m.ALPN != "" {
			tcfg.NextProtos = []string{m.ALPN}
		}
	}
	return tcfg
}

func (r *RealityServer) serveTunnel(ctx context.Context, raw net.Conn, pre []byte, h *realityHello, cert *tls.Certificate, th targetHello, thOK bool, caps byte, onTunnel func(*Carrier)) {
	ucfg := r.toUTLSConfig(r.tunnelTLSConfig(cert), th, thOK)
	var suite uint16
	if thOK {
		suite = th.suite
	}
	release := lockSuite(suite)
	tconn := utls.Server(&firstWriteConn{Conn: &prefixConn{Conn: wrapBatch(raw), pre: pre}, f: release}, ucfg)
	tconn.SetDeadline(time.Now().Add(handshakeTimeout))
	err := tconn.Handshake()
	release()
	if err != nil {
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
	// NewSessionTicket parity (phase 4): the target (OpenSSL) sends its
	// tickets right after the client's Finished — which Handshake just read —
	// as records of fixed sizes; send pads of those sizes now, to a dialer
	// that skips them. Go's own ticket (one 139 B record inside the first
	// flight, before the client's Finished) stays off: a Go-only pattern.
	if m := r.mirror.Load(); m != nil && caps&realityCapPad != 0 && len(m.NST) > 0 {
		for range m.NST {
			l := m.NSTSizes[mrand.IntN(len(m.NSTSizes))]
			if p := padRecord(ekm, l); p != nil {
				if _, err := tconn.Write(p); err != nil {
					raw.Close()
					return
				}
			}
		}
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
	r.Counters.Reality.Add(1)
	onTunnel(&Carrier{conn: tconn})
}

// fallback splices client <-> target byte for byte (pending = client bytes
// read but not yet sent to the target), with the limits.
func (r *RealityServer) fallback(ctx context.Context, client, target net.Conn, pending []byte, free, rate int64) {
	netKey := clientNet(client.RemoteAddr())
	defer client.Close()
	if target == nil {
		realityReject("target_dial")
		return // like an unreachable site: the connection just closes
	}
	defer target.Close()
	realityStats.fbConns.Add(1)
	r.Counters.Cover.Add(1)
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
		r.limitedCopy(dst, src, free, rate, netKey)
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
func (r *RealityServer) limitedCopy(dst, src net.Conn, maxBytes, rateBps int64, netKey string) {
	b := make([]byte, 32<<10)
	var total int64
	var start time.Time
	var after int64
	capped := false
	for {
		chunk := b
		if capped && len(chunk) > 16<<10 {
			chunk = b[:16<<10]
		}
		n, err := src.Read(chunk)
		if n > 0 {
			if _, werr := dst.Write(b[:n]); werr != nil {
				return
			}
			total += int64(n)
			realityStats.fbBytes.Add(int64(n))
			if !capped {
				// free while this connection is under maxBytes AND its client
				// net has budget left this minute (charged as used, sec41)
				inFree := min(int64(n), maxBytes-(total-int64(n)))
				var got int64
				if inFree > 0 {
					got = r.budget.take(netKey, inFree)
				}
				if got < int64(n) {
					capped, start, after = true, time.Now(), int64(n)-got
				}
			} else {
				after += int64(n)
			}
			if capped {
				want := time.Duration(float64(after) / float64(rateBps) * float64(time.Second))
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
