package main

import (
	"context"
	"net"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/engine"
	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

// Link fronts per lb IP (reality phases 1+3): the per-IP "front" of the lb
// lists, what the listening side announces, and the listening side's
// per-IP acceptor (frontMux). A nil / default profile is exactly today's tls
// front (see tlscarrier/front.go).

// lbFrontOf is the front profile of ip in list (nil: none set = tls).
func lbFrontOf(list []lbIP, ip string) *tlscarrier.FrontProfile {
	for _, e := range list {
		if x := net.ParseIP(e.IP); x != nil && x.String() == ip {
			return e.Front
		}
	}
	return nil
}

// publicFront is the part of a profile a dialer needs and may see: kind,
// server names, short ids and the public key (derived from the private key
// when only that is set). Never the private key, the target or the fallback
// limits (listener-only). nil for the default tls front.
func publicFront(f *tlscarrier.FrontProfile) *tlscarrier.FrontProfile {
	if f.IsDefault() {
		return nil
	}
	k := f.NormKind()
	if k == tlscarrier.FrontTLS {
		return nil
	}
	pub := f.PublicKey
	if pub == "" && f.PrivateKey != "" {
		pub, _ = tlscarrier.RealityPublicOf(f.PrivateKey)
	}
	return &tlscarrier.FrontProfile{Kind: k, ServerNames: append([]string(nil), f.ServerNames...),
		PublicKey: pub, ShortIDs: append([]string(nil), f.ShortIDs...)}
}

// lbAnnouncedFront is what the listening side announces for its own ip:
// nothing (nil) for the default tls front, so the advert keeps its stage-6
// bytes; else publicFront.
func lbAnnouncedFront(own []lbIP, ip string) *tlscarrier.FrontProfile {
	return publicFront(lbFrontOf(own, ip))
}

// lbPathDialer is the edge's link dialer for one lb path over its front. A
// path whose front this build lacks gets a dialer that always fails (the path
// cools like a dead one) instead of silently dialing the wrong front.
func lbPathDialer(p engine.LBPath, sni string, key []byte) engine.LinkDialer {
	d, err := engine.NewMTCPDialerFront(p.Addr, sni, key, p.Local, p.Front)
	if err != nil {
		return engine.FailingDialer(err)
	}
	return d
}

// ---- listening side: one acceptor per local IP ------------------------------

// frontMux is the listen side of the links' fronts: by the local IP a
// connection arrived on, that IP's front (lb.<this side>[].front), else the
// tunnel's. A reload (SIGHUP) swaps the table atomically: new connections get
// the new front, established links are untouched (zero-downtime switch).
type frontMux struct {
	def  tlscarrier.Acceptor // the tunnel's front
	srv  *tlscarrier.Server  // the tls front (also the "both" tls half)
	fc   fileConfig
	logf func(string, ...any)

	mu   sync.Mutex // serialises rebuilds
	byIP atomic.Pointer[map[string]*frontEntry]
}

type frontEntry struct {
	ip     string
	prof   tlscarrier.FrontProfile // with the private key (never leaves)
	kind   string
	acc    tlscarrier.Acceptor
	rs     *tlscarrier.RealityServer
	cancel context.CancelFunc // the size mirror
}

var curFrontMux atomic.Pointer[frontMux]

func newFrontMux(fc fileConfig, def tlscarrier.Acceptor, srv *tlscarrier.Server, logf func(string, ...any)) *frontMux {
	m := &frontMux{def: def, srv: srv, fc: fc, logf: logf}
	empty := map[string]*frontEntry{}
	m.byIP.Store(&empty)
	m.rebuild(fc)
	curFrontMux.Store(m)
	return m
}

// tlsNames: the SNIs our own tls front answers to (top-level sni and every
// per-IP sni of this side): a "both" hello with one of them goes to tls.
func tlsNames(fc fileConfig) map[string]bool {
	n := map[string]bool{}
	if fc.SNI != "" {
		n[strings.ToLower(fc.SNI)] = true
	}
	for _, e := range lbLocalList(fc) {
		if e.SNI != "" {
			n[strings.ToLower(e.SNI)] = true
		}
	}
	return n
}

// rebuild installs fc's per-IP fronts, reusing the listener of an unchanged
// profile (keeps its replay memory and counters).
func (m *frontMux) rebuild(fc fileConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()
	old := *m.byIP.Load()
	next := map[string]*frontEntry{}
	names := tlsNames(fc)
	for _, e := range lbLocalList(fc) {
		ip := net.ParseIP(e.IP)
		if ip == nil || e.Front.IsDefault() {
			continue
		}
		k := ip.String()
		kind := e.Front.NormKind()
		if o := old[k]; o != nil && reflect.DeepEqual(o.prof, *e.Front) {
			next[k] = o
			continue
		}
		ne := &frontEntry{ip: k, prof: *e.Front, kind: kind}
		switch kind {
		case tlscarrier.FrontTLS:
			ne.acc = m.srv
		case tlscarrier.FrontReality, tlscarrier.FrontBoth:
			rs, err := tlscarrier.NewRealityServer(e.Front, m.srv)
			if err != nil {
				m.logf("front: %s: %v — this IP keeps its previous front", k, err)
				if o := old[k]; o != nil {
					next[k] = o
				}
				continue
			}
			if kind == tlscarrier.FrontBoth {
				rs.TLS = m.srv
				rs.TLSName = func(_ net.Addr, sni string) bool { return names[strings.ToLower(sni)] }
			}
			ctx, cancel := context.WithCancel(context.Background())
			rs.StartSizeMirror(ctx, 0, m.logf)
			ne.acc, ne.rs, ne.cancel = rs, rs, cancel
		default:
			continue
		}
		m.logf("front: %s now serves %s (server_names %v, target %s); established links keep running", k, kind, e.Front.ServerNames, e.Front.Target)
		next[k] = ne
	}
	for k, o := range old {
		if next[k] != o && o.cancel != nil {
			o.cancel()
		}
		if next[k] == nil {
			m.logf("front: %s back to the tunnel's front; established links keep running", k)
		}
	}
	m.fc = fc
	m.byIP.Store(&next)
	warnSNIResolve(fc, m.logf)
}

// AcceptFront dispatches by the local IP.
func (m *frontMux) AcceptFront(ctx context.Context, raw net.Conn, onTunnel func(*tlscarrier.Carrier)) {
	if ta, ok := raw.LocalAddr().(*net.TCPAddr); ok {
		ip := ta.IP
		if v4 := ip.To4(); v4 != nil {
			ip = v4
		}
		if e := (*m.byIP.Load())[ip.String()]; e != nil {
			e.acc.AcceptFront(ctx, raw, onTunnel)
			return
		}
	}
	m.def.AcceptFront(ctx, raw, onTunnel)
}

// frontIPStatus is one listening IP's front in the status file.
type frontIPStatus struct {
	IP       string                        `json:"ip"`
	Kind     string                        `json:"kind"`
	Counters *tlscarrier.FrontCountersSnap `json:"counters,omitempty"`
}

// frontStatusNow: the listening side's per-IP fronts (nil when none).
func frontStatusNow() []frontIPStatus {
	m := curFrontMux.Load()
	if m == nil {
		return nil
	}
	var out []frontIPStatus
	for _, e := range lbLocalList(lbCur(m.fc)) {
		ip := net.ParseIP(e.IP)
		if ip == nil {
			continue
		}
		s := frontIPStatus{IP: ip.String(), Kind: tlscarrier.FrontTLS}
		if fe := (*m.byIP.Load())[ip.String()]; fe != nil {
			s.Kind = fe.kind
			if fe.rs != nil {
				c := fe.rs.Counters.Snap()
				s.Counters = &c
			}
		} else if k := tunnelFront(m.fc).NormKind(); k != "" {
			s.Kind = k
		}
		out = append(out, s)
	}
	return out
}

// frontPathStatus is one dialing path's front in the status file.
type frontPathStatus struct {
	Path string                    `json:"path"` // local>remote
	Kind string                    `json:"kind"` // tls | reality | both
	Both *tlscarrier.BothPathStats `json:"both,omitempty"`
}

// frontPathsNow: the dialing side's per-path fronts (nil when every path is
// the default tls front).
func frontPathsNow(fc fileConfig) []frontPathStatus {
	rt := curLB()
	if rt == nil || rt.lb == nil {
		return nil
	}
	both := map[string]tlscarrier.BothPathStats{}
	for _, s := range tlscarrier.DefaultBothPolicy.Stats() {
		both[s.Path] = s
	}
	var out []frontPathStatus
	any := false
	for _, p := range rt.lb.Paths() {
		f := pathFront(fc, p.Front)
		s := frontPathStatus{Path: p.Name(), Kind: f.NormKind()}
		if s.Kind != tlscarrier.FrontTLS {
			any = true
		}
		if b, ok := both[p.Local+">"+p.Addr]; ok {
			s.Both = &b
		}
		out = append(out, s)
	}
	if !any {
		return nil
	}
	return out
}
