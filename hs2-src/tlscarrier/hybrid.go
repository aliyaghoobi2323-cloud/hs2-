package tlscarrier

import (
	"context"
	"errors"
	"io"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// The "both" front (reality phase 3): one listening port that serves the
// reality front AND the tls front, and a dialer that prefers reality and
// falls back to tls when reality keeps failing on a path.
//
// Listen side (RealityServer with TLS/TLSName set): every ClientHello is
// classified once it is complete:
//
//	valid reality signal            -> reality (our TLS 1.3 + tunnel auth)
//	SNI is one of our own tls names -> tls (Server.Handle over the read bytes;
//	                                   its nginx-identical cover included)
//	anything else                   -> cover: spliced to the reality target
//
// Dial side (BothFront): reality first. A reality failure after the TCP
// connect worked (wrong key, target down on the listener, a DPI that kills
// the reality hello) is retried AT ONCE with tls on the same path, so a front
// problem never shows as a path loss to the LB; only a path where both fronts
// fail returns an error (and is cut as usual). After N reality failures in a
// row the path dials tls only for a cool-down, then tries reality again.
//
// rel50: only a failure that our listener or the key explains counts toward
// the cool-down (RealityRefusal): the certificate did not prove the key
// (ErrRealityCert: wrong key/short id, clock skew, the target answered) or our
// listener refused the hello with a clean close (EOF: target down or slow on
// the listener). A reset, a timeout or any other network error during the
// reality handshake is a PATH problem: it is returned to the LB as a failed
// dial (no tls retry, no count), so a censor that resets reality handshakes
// cannot push a path onto tls for the cool-down. A tls dial that the reality
// target answers (ErrTargetAnswered: the listener is pure reality now) ends
// the cool-down at once.

// FrontCounters are one listener's (one lb IP's) classification counters.
type FrontCounters struct {
	Reality  atomic.Int64 // authenticated reality tunnels
	TLS      atomic.Int64 // hellos handed to the tls front ("both")
	Cover    atomic.Int64 // spliced to the target
	Rejected atomic.Int64 // failed reality checks (incl. those spliced)
}

// FrontCountersSnap is a JSON snapshot of FrontCounters.
type FrontCountersSnap struct {
	Reality  int64 `json:"reality"`
	TLS      int64 `json:"tls"`
	Cover    int64 `json:"cover"`
	Rejected int64 `json:"rejected"`
}

// Snap snapshots c.
func (c *FrontCounters) Snap() FrontCountersSnap {
	return FrontCountersSnap{Reality: c.Reality.Load(), TLS: c.TLS.Load(), Cover: c.Cover.Load(), Rejected: c.Rejected.Load()}
}

// ConnectError: the TCP connect of a front dial failed (a path problem, not
// a front problem).
type ConnectError struct{ Err error }

func (e *ConnectError) Error() string { return e.Err.Error() }
func (e *ConnectError) Unwrap() error { return e.Err }

// BothPolicy is the per-path reality -> tls fallback state machine.
//
//	reality --fail x N in a row--> tls (until now + Cool) --expiry--> reality
//	any reality success resets the count.
type BothPolicy struct {
	N    int           // consecutive reality failures that start a cool-down (0 = 3)
	Cool time.Duration // cool-down on tls (0 = 10 min)
	Now  func() time.Time
	Logf func(string, ...any)

	mu sync.Mutex
	m  map[string]*bothPath
}

type bothPath struct {
	fails     int
	coolUntil time.Time
	st        BothPathStats
}

// BothPathStats is one path's dial-side state for status.
type BothPathStats struct {
	Path        string `json:"path"`
	Mode        string `json:"mode"` // reality | tls (cool-down)
	CoolUntil   int64  `json:"cool_until,omitempty"`
	ConsecFails int    `json:"consec_fails,omitempty"`
	RealityOK   int64  `json:"reality_ok"`
	RealityFail int64  `json:"reality_fail"`
	TLSOK       int64  `json:"tls_ok"`
	TLSFail     int64  `json:"tls_fail"`
	Fallbacks   int64  `json:"fallbacks"` // cool-downs entered
	ConnectFail int64  `json:"connect_fail"`
	// PathFail: reality handshakes that failed with a reset/timeout/network
	// error (a path problem: returned to the LB, no tls retry, not counted).
	PathFail int64 `json:"path_fail,omitempty"`
}

// RealityRefusal: err is a reality failure that our listener or the key
// explains (counts toward the tls cool-down), not a path problem.
func RealityRefusal(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrRealityCert) || errors.Is(err, ErrTargetAnswered) {
		return true
	}
	// our listener refused the hello with a clean close (target down/slow)
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// realityOn ends k's cool-down (the listener is pure reality now).
func (p *BothPolicy) realityOn(k string) {
	p.mu.Lock()
	b := p.path(k)
	was := !b.coolUntil.IsZero()
	b.coolUntil, b.fails = time.Time{}, 0
	p.mu.Unlock()
	if was && p.Logf != nil {
		p.Logf("front both: %s: the tls dial reached the reality target: back to reality", k)
	}
}

// DefaultBothPolicy is the process-wide policy of "both" paths.
var DefaultBothPolicy = &BothPolicy{}

func (p *BothPolicy) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *BothPolicy) path(k string) *bothPath {
	if p.m == nil {
		p.m = map[string]*bothPath{}
	}
	b := p.m[k]
	if b == nil {
		b = &bothPath{st: BothPathStats{Path: k}}
		p.m[k] = b
	}
	return b
}

// UseTLS: path k is in its tls cool-down.
func (p *BothPolicy) UseTLS(k string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	b := p.path(k)
	if b.coolUntil.IsZero() {
		return false
	}
	if p.now().Before(b.coolUntil) {
		return true
	}
	b.coolUntil, b.fails = time.Time{}, 0
	if p.Logf != nil {
		p.Logf("front both: %s: cool-down over, trying reality again", k)
	}
	return false
}

// RealityOK records a reality success on k.
func (p *BothPolicy) RealityOK(k string) {
	p.mu.Lock()
	b := p.path(k)
	b.fails = 0
	b.st.RealityOK++
	p.mu.Unlock()
}

// RealityFail records a reality failure on k; true when it starts a cool-down.
func (p *BothPolicy) RealityFail(k string, err error) bool {
	n, cool := p.N, p.Cool
	if n <= 0 {
		n = 3
	}
	if cool <= 0 {
		cool = 10 * time.Minute
	}
	p.mu.Lock()
	b := p.path(k)
	b.fails++
	b.st.RealityFail++
	start := b.fails >= n && b.coolUntil.IsZero()
	if start {
		b.coolUntil = p.now().Add(cool)
		b.st.Fallbacks++
	}
	f := b.fails
	p.mu.Unlock()
	if start && p.Logf != nil {
		p.Logf("front both: %s: reality failed %d times in a row (%v): dialing tls for %v", k, f, err, cool)
	}
	return start
}

func (p *BothPolicy) count(k string, f func(*BothPathStats)) {
	p.mu.Lock()
	f(&p.path(k).st)
	p.mu.Unlock()
}

// Stats lists every path's state, sorted by path.
func (p *BothPolicy) Stats() []BothPathStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	out := make([]BothPathStats, 0, len(p.m))
	for _, b := range p.m {
		s := b.st
		s.Mode, s.ConsecFails = FrontReality, b.fails
		if !b.coolUntil.IsZero() && now.Before(b.coolUntil) {
			s.Mode, s.CoolUntil = FrontTLS, b.coolUntil.Unix()
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// BothFront is the "both" front's dial side.
type BothFront struct {
	Policy *BothPolicy
	// dial hooks (tests); nil = RealityFront / TLSFront.
	reality, tls Dialer
}

var _ Dialer = BothFront{}

// BothPathKey names a path for the policy: "local>remote:port".
func BothPathKey(p DialPath) string { return p.BindIP + ">" + p.Addr }

// DialFront: reality unless the path is cooling down; a reality failure past
// the TCP connect is retried with tls on the spot.
func (b BothFront) DialFront(ctx context.Context, p DialPath) (*Carrier, error) {
	pol := b.Policy
	if pol == nil {
		pol = DefaultBothPolicy
	}
	rd, td := b.reality, b.tls
	if rd == nil {
		rd = RealityFront{}
	}
	if td == nil {
		td = TLSFront{}
	}
	if pr := p.Profile; pr == nil || pr.PublicKey == "" || len(pr.ServerNames) == 0 || len(pr.ShortIDs) == 0 {
		return nil, errors.New("tlscarrier: both front needs public_key, server_names, short_ids")
	}
	k := BothPathKey(p)
	if !pol.UseTLS(k) {
		c, err := rd.DialFront(ctx, p)
		if err == nil {
			pol.RealityOK(k)
			return c, nil
		}
		var ce *ConnectError
		if errors.As(err, &ce) { // the path itself is down: no point in tls
			pol.count(k, func(s *BothPathStats) { s.ConnectFail++ })
			return nil, err
		}
		if !RealityRefusal(err) { // reset/timeout: the path's, for the LB
			pol.count(k, func(s *BothPathStats) { s.PathFail++ })
			return nil, err
		}
		pol.RealityFail(k, err)
	}
	c, err := td.DialFront(ctx, p)
	if errors.Is(err, ErrTargetAnswered) {
		pol.realityOn(k)
	}
	pol.count(k, func(s *BothPathStats) {
		if err == nil {
			s.TLSOK++
		} else {
			s.TLSFail++
		}
	})
	return c, err
}
