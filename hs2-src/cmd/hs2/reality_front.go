package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync/atomic"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/engine"
	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

// The tunnel's link front (reality phase 2): the per-tunnel "front" block
//
//	"front": {"kind":"reality", "target":"www.site.ir:443", "server_names":["www.site.ir"],
//	          "private_key":"…" (listener) / "public_key":"…" (dialer), "short_ids":["a1b2…"],
//	          "fallback_max_bytes":262144, "fallback_rate_bps":65536, "fallback_max_seconds":600}
//
// applies to every link of the tunnel (direct and reverse; mtcp, l3mtcp and
// tls carriers) unless an lb path names its own (phase 3). Absent = tls.

// tunnelFront is the tunnel's front profile (nil = tls).
func tunnelFront(fc fileConfig) *tlscarrier.FrontProfile {
	if fc.Front.IsDefault() {
		return nil
	}
	return fc.Front
}

// pathFront: an lb path's own front, else the tunnel's.
func pathFront(fc fileConfig, p *tlscarrier.FrontProfile) *tlscarrier.FrontProfile {
	if p != nil && !p.IsDefault() {
		if p.NormKind() != tlscarrier.FrontTLS && p.PublicKey == "" && p.PrivateKey != "" {
			return publicFront(p) // a dialer config copied from the listener's
		}
		return p
	}
	return tunnelFront(fc)
}

// tunnelListens: this side is the TLS listener of the links.
func tunnelListens(fc fileConfig) bool {
	iran := fc.Mode == "dial"
	return iran == fc.Reverse
}

// validateFront checks the tunnel front at config load.
func validateFront(fc fileConfig) error {
	f := fc.Front
	if f.IsDefault() {
		return nil
	}
	if err := f.Validate(); err != nil {
		return fmt.Errorf(`"front": %v`, err)
	}
	if f.NormKind() == tlscarrier.FrontBoth {
		return errors.New(`"front": kind "both" is per lb IP ("lb.<side>[].front"); the tunnel's front is "tls" or "reality"`)
	}
	if f.NormKind() != tlscarrier.FrontReality {
		return nil
	}
	switch fc.Carrier {
	case "mtcp", "l3mtcp", "l3", "tls":
	default:
		return fmt.Errorf(`"front": reality needs carrier mtcp, l3mtcp or tls (not %q)`, fc.Carrier)
	}
	if tunnelListens(fc) {
		if f.PrivateKey == "" {
			return errors.New(`"front": the listening side needs "private_key" (hs2 reality keygen)`)
		}
		return validateRealityTarget(f)
	}
	if f.PublicKey == "" {
		return errors.New(`"front": the dialing side needs "public_key" (the listener's, from hs2 reality keygen)`)
	}
	return nil
}

// validateRealityTarget refuses a target on a multi-tenant CDN (every
// stranger could use our listener as a clean front for it) unless forced,
// and server names that are not the target's own host.
func validateRealityTarget(f *tlscarrier.FrontProfile) error {
	host, _, err := net.SplitHostPort(f.Target)
	if err != nil {
		return fmt.Errorf(`"front": target %q: want host:port`, f.Target)
	}
	if !f.ForceTarget {
		for _, n := range append([]string{host}, f.ServerNames...) {
			if cdn := multiTenantCDN(n); cdn != "" {
				return fmt.Errorf(`"front": target/server name %s is on %s, a multi-tenant CDN: pick a single-tenant site (or set "force_target": true)`, n, cdn)
			}
		}
	}
	if net.ParseIP(host) == nil {
		ok := false
		for _, n := range f.ServerNames {
			if strings.EqualFold(n, host) || strings.HasSuffix(strings.ToLower(n), "."+strings.ToLower(host)) ||
				strings.HasSuffix(strings.ToLower(host), "."+strings.ToLower(n)) {
				ok = true
			}
		}
		if !ok && !f.ForceTarget {
			return fmt.Errorf(`"front": server_names %v are not names of the target %s (check with: hs2 reality check-target %s)`, f.ServerNames, host, f.Target)
		}
	}
	return nil
}

// realityAcceptor wraps the tls front's Server as the listen side of the
// tunnel's front.
func realityAcceptor(fc fileConfig, srv *tlscarrier.Server, logf func(string, ...any)) tlscarrier.Acceptor {
	syncPeerExempt(fc, logf)
	def := tunnelAcceptor(fc, srv, logf)
	if lbOn(fc) { // per-IP fronts (phase 3), swapped on SIGHUP
		return newFrontMux(fc, def, srv, logf)
	}
	return def
}

func tunnelAcceptor(fc fileConfig, srv *tlscarrier.Server, logf func(string, ...any)) tlscarrier.Acceptor {
	f := tunnelFront(fc)
	if f.NormKind() != tlscarrier.FrontReality {
		return srv
	}
	r, err := tlscarrier.NewRealityServer(f, srv)
	must(err)
	r.StartSizeMirror(context.Background(), 0, logf)
	logf("front: reality, server_names %v, target %s, fallback free %d B then %d B/s (0 = defaults 256 KB, 64 KB/s)", f.ServerNames, f.Target, f.FallbackMaxBytes, f.FallbackRateBps)
	return r
}

// tunnelDialer is the direct edge's link dialer over the tunnel's front.
func tunnelDialer(fc fileConfig, key []byte) engine.LinkDialer {
	f := tunnelFront(fc)
	if f == nil {
		return engine.NewMTCPDialer(fc.Addr, fc.SNI, key, fc.BindLocalIP)
	}
	d, err := engine.NewMTCPDialerFront(fc.Addr, fc.SNI, key, fc.BindLocalIP, f)
	if err != nil {
		return engine.FailingDialer(err)
	}
	return d
}

const realityUsage = "usage: hs2 reality keygen | check-target host[:443] [server_name] [-u U] | targets [-u U | --side iran|kharej] [--add h,h] | status -u U | front -u U --ip IP tls|reality|both [--target host:443] | rotate-key -u U (--ip IP|--all) | rotate-sid -u U (--ip IP|--all) | pin-code -u U (listener) | pin -u U CODE (dialer)"

// realityCmd: hs2 reality keygen | check-target | targets | status | front | rotate-key
func realityCmd(args []string) {
	if len(args) == 0 {
		fmt.Println(realityUsage)
		os.Exit(2)
	}
	switch args[0] {
	case "front", "rotate-key", "rotate-sid":
		os.Exit(realityFrontCmd(args[0], args[1:]))
	case "status":
		os.Exit(realityStatusCmd(args[1:]))
	case "pin":
		os.Exit(realityPinCmd(args[1:]))
	case "pin-code":
		os.Exit(realityPinCodeCmd(args[1:]))
	case "targets":
		os.Exit(realityTargetsCmd(args[1:]))
	case "--supported":
		fmt.Println("front rotate-key rotate-sid status targets check-target keygen pin pin-code")
	case "keygen":
		priv, pub, err := tlscarrier.RealityKeygen()
		must(err)
		sid := make([]byte, 8)
		rand.Read(sid)
		fmt.Printf("private_key: %s   (listening side only, never share)\npublic_key:  %s   (dialing side)\nshort_id:    %s\n", priv, pub, hex.EncodeToString(sid))
	case "check-target":
		os.Exit(realityCheckTargetCmd(args[1:]))
	default:
		fmt.Println(realityUsage)
		os.Exit(2)
	}
}

// syncPeerExempt (b388, sec353 1a) loads the reality pre-hello exemption's
// static part from the config: the other side's lb IPs (the addresses our own
// links come from) and the per-IP bound 2*max_links+8. Called at start and on
// every reload (frontMux.rebuild); IPs that authenticate a link are learned at
// run time (tlscarrier.PeerAllow, 24 h).
func syncPeerExempt(fc fileConfig, logf func(string, ...any)) {
	var ips []string
	unres := 0
	if lbOn(fc) {
		_, remote, _, _ := lbLists(fc)
		for _, e := range remote {
			if net.ParseIP(e.IP) == nil {
				unres++ // b411: "auto6" (resolved on the other side only)
				continue
			}
			ips = append(ips, e.IP)
		}
	}
	_, max, _ := linkEnvelope(fc)
	tlscarrier.DefaultPeers.SetStatic(ips)
	tlscarrier.DefaultPeers.SetBound(max)
	// b411 (prod t1: static 0): the listening side's config usually does not
	// hold the dialer's own source IPs (its primary is never listed there, and
	// an "auto6" entry only resolves on the other side), so the IPs that
	// authenticated a link are also saved next to the config and loaded into
	// the static part at the next start.
	loaded := -1
	if peerStoreOnce.CompareAndSwap(false, true) && tlscarrier.PeerExemptOn.Load() {
		n, err := tlscarrier.DefaultPeers.SetStore(peerIPsPath(configPath))
		loaded = n
		if err != nil && logf != nil {
			logf("front: reality pre-hello cap: cannot read the saved peer IPs: %v", err)
		}
	}
	if logf != nil && tlscarrier.PeerExemptOn.Load() && tunnelOrIPReality(fc) {
		s := tlscarrier.DefaultPeers.Snap()
		logf("front: reality pre-hello cap: %d peer IPs exempt (%d configured, %d unresolved auto6 skipped, %d saved from earlier links; + IPs that authenticated a link in 24 h, IPv6 by /64), %d pre-hello conns per peer IP", s.Static, len(ips), unres, max0(loaded), s.Bound)
	}
}

// peerStoreOnce: the save file is opened once per process (reloads only
// replace the configured part).
var peerStoreOnce atomic.Bool

func max0(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

// peerIPsPath (b411): <config>.peer-ips next to the config ("" = no saving).
func peerIPsPath(cfgPath string) string {
	if cfgPath == "" {
		return ""
	}
	return strings.TrimSuffix(cfgPath, ".json") + ".peer-ips"
}

// tunnelOrIPReality: some listening IP of fc serves a reality or "both" front.
func tunnelOrIPReality(fc fileConfig) bool {
	if k := tunnelFront(fc).NormKind(); k == tlscarrier.FrontReality || k == tlscarrier.FrontBoth {
		return true
	}
	for _, e := range lbLocalList(fc) {
		if e.Front == nil {
			continue
		}
		if k := e.Front.NormKind(); k == tlscarrier.FrontReality || k == tlscarrier.FrontBoth {
			return true
		}
	}
	return false
}
