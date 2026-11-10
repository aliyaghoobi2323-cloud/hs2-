package main

import (
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

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
	f := tunnelFront(fc)
	if f.NormKind() != tlscarrier.FrontReality {
		return srv
	}
	r, err := tlscarrier.NewRealityServer(f, srv)
	must(err)
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

// realityCmd: hs2 reality keygen | check-target host:443 [sni]
func realityCmd(args []string) {
	if len(args) == 0 {
		fmt.Println("usage: hs2 reality keygen | hs2 reality check-target host:443 [server_name]")
		os.Exit(2)
	}
	switch args[0] {
	case "keygen":
		priv, pub, err := tlscarrier.RealityKeygen()
		must(err)
		sid := make([]byte, 8)
		rand.Read(sid)
		fmt.Printf("private_key: %s   (listening side only, never share)\npublic_key:  %s   (dialing side)\nshort_id:    %s\n", priv, pub, hex.EncodeToString(sid))
	case "check-target":
		if len(args) < 2 {
			fmt.Println("usage: hs2 reality check-target host:443 [server_name]")
			os.Exit(2)
		}
		target := args[1]
		if !strings.Contains(target, ":") {
			target += ":443"
		}
		host, _, _ := net.SplitHostPort(target)
		sni := host
		if len(args) > 2 {
			sni = args[2]
		}
		os.Exit(checkTarget(target, sni))
	default:
		fmt.Println("usage: hs2 reality keygen | hs2 reality check-target host:443 [server_name]")
		os.Exit(2)
	}
}

// checkTarget reports what matters for a reality target: TLS 1.3, X25519,
// ALPN h2, the certificate's names, a redirect, handshake time, and whether
// it sits on a multi-tenant CDN.
func checkTarget(target, sni string) int {
	bad := 0
	if cdn := multiTenantCDN(sni); cdn != "" {
		fmt.Printf("BAD   %s looks like %s (multi-tenant CDN)\n", sni, cdn)
		bad++
	}
	t0 := time.Now()
	d := &net.Dialer{Timeout: 8 * time.Second}
	c, err := tls.DialWithDialer(d, "tcp", target, &tls.Config{ServerName: sni, NextProtos: []string{"h2", "http/1.1"},
		CurvePreferences: []tls.CurveID{tls.X25519}, MinVersion: tls.VersionTLS12})
	if err != nil {
		fmt.Printf("BAD   TLS to %s (sni %s): %v\n", target, sni, err)
		return 1
	}
	defer c.Close()
	hs := time.Since(t0)
	cs := c.ConnectionState()
	if cs.Version == tls.VersionTLS13 {
		fmt.Println("OK    TLS 1.3")
	} else {
		fmt.Printf("BAD   TLS version %x (need 1.3)\n", cs.Version)
		bad++
	}
	fmt.Println("OK    X25519 accepted (offered alone)")
	fmt.Printf("INFO  ALPN %q\n", cs.NegotiatedProtocol)
	if len(cs.PeerCertificates) > 0 {
		crt := cs.PeerCertificates[0]
		fmt.Printf("INFO  cert SANs %v, issuer %q, chain %d certs\n", crt.DNSNames, crt.Issuer.CommonName, len(cs.PeerCertificates))
		if err := crt.VerifyHostname(sni); err != nil {
			fmt.Printf("BAD   certificate does not cover %s\n", sni)
			bad++
		} else {
			fmt.Printf("OK    certificate covers %s (verified chain: %v)\n", sni, len(cs.VerifiedChains) > 0)
		}
	}
	fmt.Printf("INFO  handshake %v\n", hs.Round(time.Millisecond))
	c.SetDeadline(time.Now().Add(8 * time.Second))
	if cs.NegotiatedProtocol != "h2" {
		fmt.Fprintf(c, "HEAD / HTTP/1.1\r\nHost: %s\r\nUser-Agent: Mozilla/5.0\r\nConnection: close\r\n\r\n", sni)
		b := make([]byte, 2048)
		n, _ := c.Read(b)
		head := string(b[:n])
		if i := strings.Index(head, "\r\n"); i > 0 {
			fmt.Printf("INFO  HTTP %s\n", head[:i])
		}
		for _, l := range strings.Split(head, "\r\n") {
			if strings.HasPrefix(strings.ToLower(l), "location:") {
				fmt.Printf("INFO  redirect %s\n", strings.TrimSpace(l[9:]))
			}
		}
	}
	if bad > 0 {
		return 1
	}
	return 0
}
