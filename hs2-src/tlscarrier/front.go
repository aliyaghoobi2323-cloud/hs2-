package tlscarrier

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

// The FRONT is the outermost layer of a link: the TLS handshake / front door
// that a censor sees on the wire, below the tunnel auth (tls-exporter bound,
// mutual, auth.go) and the link pool above it.
//
//	users → link pool/LB → tunnel auth → FRONT → TCP
//
// Phase 1 (this file) introduces the seam with exactly one implementation,
// "tls" — today's own-cert/own-domain uTLS front with the nginx-identical
// cover — by wrapping DialFromTimeout and Server.Handle unchanged: same bytes
// on the wire, same timing, same behaviour. Later variants ("reality",
// "both") plug in here; see docs/reality-architecture.md.

// Front kinds.
const (
	FrontTLS = "tls" // today's front (also the meaning of "")
	// Reserved for later phases; not implemented yet.
	FrontReality = "reality"
	FrontBoth    = "both"
)

// FrontProfile is one path's (one listening IP's) front configuration. The
// zero value is the "tls" front, i.e. today's behaviour. Every field but Kind
// is reserved for the reality front and is ignored by "tls".
//
// It is carried in the lb section per IP ("front": {...}) and announced by the
// listening side to the dialer as an OPTIONAL extension of the lb advert
// (engine/lb_advert.go) that old peers ignore. PrivateKey never leaves this
// side (json "-"): the dialer only ever needs the public key.
type FrontProfile struct {
	Kind string `json:"kind,omitempty"` // "" | "tls" | (later) "reality" | "both"
	// reality (reserved): the real site whose ClientHello SNI is borrowed and
	// to which unauthenticated connections are spliced.
	Target      string   `json:"target,omitempty"`       // host:port of the real site
	ServerNames []string `json:"server_names,omitempty"` // SNIs accepted / sent
	PublicKey   string   `json:"public_key,omitempty"`   // x25519, base64url (dialer)
	PrivateKey  string   `json:"-"`                      // x25519, listener only: never announced
	ShortIDs    []string `json:"short_ids,omitempty"`    // hex
	// Fallback (splice to Target) limits for unauthenticated connections.
	FallbackMaxBytes int64 `json:"fallback_max_bytes,omitempty"` // 0: unlimited
	FallbackRateBps  int64 `json:"fallback_rate_bps,omitempty"`  // 0: unlimited
}

// NormKind is the profile's kind with "" meaning "tls".
func (p *FrontProfile) NormKind() string {
	if p == nil || p.Kind == "" {
		return FrontTLS
	}
	return strings.ToLower(p.Kind)
}

// IsDefault: the profile is today's tls front with nothing else set, so it is
// never announced and never changes a byte on the wire.
func (p *FrontProfile) IsDefault() bool {
	if p == nil {
		return true
	}
	return p.NormKind() == FrontTLS && p.Target == "" && len(p.ServerNames) == 0 &&
		p.PublicKey == "" && p.PrivateKey == "" && len(p.ShortIDs) == 0 &&
		p.FallbackMaxBytes == 0 && p.FallbackRateBps == 0
}

// Validate checks a profile. Phase 1 accepts only the tls kind.
func (p *FrontProfile) Validate() error {
	switch k := p.NormKind(); k {
	case FrontTLS:
		return nil
	case FrontReality, FrontBoth:
		return fmt.Errorf("front %q is not supported by this build yet", k)
	default:
		return fmt.Errorf("unknown front kind %q", k)
	}
}

// DialPath is what the dial side of a front needs for one link.
type DialPath struct {
	Addr    string        // host:port to dial
	SNI     string        // the SNI of this path (lb per-IP sni, else the top-level one)
	BindIP  string        // local source IP ("" = default route)
	Connect time.Duration // TCP connect timeout (0 = the default 8 s)
	Key     []byte        // the tunnel's shared key (tunnel auth above the front)
	Profile *FrontProfile // nil = tls
}

// Dialer is the dial side of a front: it returns an authenticated carrier.
type Dialer interface {
	DialFront(ctx context.Context, p DialPath) (*Carrier, error)
}

// Acceptor is the listen side of a front. AcceptFront takes a raw accepted TCP
// conn and either hands an authenticated tunnel carrier to onTunnel (as its
// last action) or handles the connection itself as cover (probe, browser,
// unauthenticated client) and returns without calling onTunnel.
type Acceptor interface {
	AcceptFront(ctx context.Context, raw net.Conn, onTunnel func(*Carrier))
}

// ErrFrontUnsupported: the profile names a front this build does not have.
var ErrFrontUnsupported = errors.New("tlscarrier: front not supported")

// TLSFront is the "tls" front's dial side: exactly DialFromTimeout.
type TLSFront struct{}

// DialFront dials p with today's uTLS front. ctx is not consulted, exactly as
// before (the connect and auth timeouts bound the dial).
func (TLSFront) DialFront(_ context.Context, p DialPath) (*Carrier, error) {
	c := p.Connect
	if c <= 0 {
		c = 8 * time.Second
	}
	return DialFromTimeout(p.Addr, p.SNI, p.Key, p.BindIP, c)
}

// AcceptFront is the "tls" front's listen side: exactly Handle.
func (s *Server) AcceptFront(ctx context.Context, raw net.Conn, onTunnel func(*Carrier)) {
	s.Handle(ctx, raw, onTunnel)
}

var (
	_ Dialer   = TLSFront{}
	_ Acceptor = (*Server)(nil)
)

// DialerFor picks the dial-side front for a profile.
func DialerFor(p *FrontProfile) (Dialer, error) {
	switch p.NormKind() {
	case FrontTLS:
		return TLSFront{}, nil
	}
	return nil, fmt.Errorf("%w: %q", ErrFrontUnsupported, p.NormKind())
}

// DialPathFront dials p with the front its profile names.
func DialPathFront(ctx context.Context, p DialPath) (*Carrier, error) {
	d, err := DialerFor(p.Profile)
	if err != nil {
		return nil, err
	}
	return d.DialFront(ctx, p)
}
