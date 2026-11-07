package main

import (
	"crypto/tls"
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/engine"
)

// ---- ACME HTTP-01 on the user ports ----------------------------------------

// acmeDefaultWebroot is where the installer points certbot --webroot.
const acmeDefaultWebroot = "/var/lib/hs2/acme"

// acmeWebrootOf resolves acme_webroot: "" off; "on"/"true"/"default" the
// installer's directory; otherwise the given absolute path.
func acmeWebrootOf(fc fileConfig) string {
	switch v := strings.TrimSpace(fc.ACMEWebroot); strings.ToLower(v) {
	case "", "off", "false", "no":
		return ""
	case "on", "true", "yes", "default":
		return acmeDefaultWebroot
	default:
		return filepath.Clean(v)
	}
}

// acmePortsOf: the user ports that answer challenges (acme_ports, default 80).
func acmePortsOf(fc fileConfig) (map[int]bool, error) {
	spec := strings.TrimSpace(fc.ACMEPorts)
	if spec == "" {
		spec = "80"
	}
	out := map[int]bool{}
	for _, p := range splitComma(spec) {
		n, err := strconv.Atoi(p)
		if err != nil || n <= 0 || n > 65535 {
			return nil, fmt.Errorf(`"acme_ports": %q is not a port`, p)
		}
		out[n] = true
	}
	return out, nil
}

// acmeResponder builds the user-port challenge responder; nil when off (then
// every user connection is forwarded exactly as before).
func acmeResponder(fc fileConfig, logf func(string, ...any)) *engine.ACMEResponder {
	root := acmeWebrootOf(fc)
	if root == "" || fc.Mode != "dial" {
		return nil
	}
	ports, err := acmePortsOf(fc)
	must(err)
	var on []string
	for _, p := range splitComma(fc.ForwardPorts) {
		if n, err := strconv.Atoi(p); err == nil && ports[n] {
			on = append(on, p)
		}
	}
	if len(on) > 0 {
		logf("acme: answering HTTP-01 challenges on user port(s) %s from %s", strings.Join(on, ","), root)
	}
	return &engine.ACMEResponder{Webroot: root, Ports: ports, Log: logf}
}

// ---- per-address certificate / SNI -----------------------------------------

// certSelector picks the server certificate per connection: by the local IP
// the connection arrived on (lb.<this side>[].cert_file), then by the SNI the
// client asked for, else the top-level cert. Every reloader is registered, so
// SIGHUP and the mtime poll (cert.go) reload renewed per-address certs too.
type certSelector struct {
	def   *certReloader
	byIP  map[string]*certReloader
	bySNI map[string]*certReloader
}

// lbLocalList: this side's lb list when lb is on (the addresses it serves on).
func lbLocalList(fc fileConfig) []lbIP {
	if !lbOn(fc) {
		return nil
	}
	local, _, _, _ := lbLists(fc)
	return local
}

func newCertSelector(fc fileConfig, logf func(string, ...any)) (*certSelector, error) {
	def, err := newCertReloader(fc.CertFile, fc.KeyFile)
	if err != nil {
		return nil, err
	}
	s := &certSelector{def: def, byIP: map[string]*certReloader{}, bySNI: map[string]*certReloader{}}
	files := map[[2]string]*certReloader{{fc.CertFile, fc.KeyFile}: def}
	for _, e := range lbLocalList(fc) {
		ip := net.ParseIP(e.IP)
		if ip == nil || e.CertFile == "" || e.KeyFile == "" {
			continue
		}
		k := [2]string{e.CertFile, e.KeyFile}
		r := files[k]
		if r == nil {
			if r, err = newCertReloader(e.CertFile, e.KeyFile); err != nil {
				return nil, fmt.Errorf("lb: certificate of %s: %v", e.IP, err)
			}
			files[k] = r
		}
		s.byIP[ip.String()] = r
		if e.SNI != "" {
			s.bySNI[strings.ToLower(e.SNI)] = r
		}
		if logf != nil {
			logf("lb: %s serves its own certificate %s", ip, e.CertFile)
		}
	}
	return s, nil
}

func (s *certSelector) pick(local net.Addr, sni string) *certReloader {
	if ta, ok := local.(*net.TCPAddr); ok && len(s.byIP) > 0 {
		ip := ta.IP
		if v4 := ip.To4(); v4 != nil {
			ip = v4
		}
		if r := s.byIP[ip.String()]; r != nil {
			return r
		}
	}
	if r := s.bySNI[strings.ToLower(sni)]; r != nil {
		return r
	}
	return s.def
}

// getCertificate is the tls.Config.GetCertificate callback.
func (s *certSelector) getCertificate(h *tls.ClientHelloInfo) (*tls.Certificate, error) {
	var local net.Addr
	if h != nil && h.Conn != nil {
		local = h.Conn.LocalAddr()
	}
	sni := ""
	if h != nil {
		sni = h.ServerName
	}
	return s.pick(local, sni).cur.Load(), nil
}

// lbSNIFor is the SNI the dialing side sends on a path to remote IP: that
// address's own "sni" in the other side's lb list, else the top-level sni.
func lbSNIFor(fc fileConfig, remote string) string {
	if lbOn(fc) {
		_, rl, _, _ := lbLists(fc)
		for _, e := range rl {
			if ip := net.ParseIP(e.IP); ip != nil && ip.String() == remote && e.SNI != "" {
				return e.SNI
			}
		}
	}
	return fc.SNI
}

// ---- per-address cover site ------------------------------------------------

// coverSeedFor derives a distinct cover seed per address/subdomain. label ""
// (the primary address) keeps the install's seed, so its page is unchanged; an
// empty seed (legacy fixed page) stays empty — a seed derived from a public
// name alone would be computable by anyone.
func coverSeedFor(seed, label string) string {
	if seed == "" || label == "" {
		return seed
	}
	return seed + "\x00" + strings.ToLower(label)
}

// coverBackends starts one built-in cover site per extra lb address of this
// side (labelled by its sni, else its IP) and returns the per-connection
// chooser; nil when not applicable (custom backend_addr, lb off).
func coverBackends(fc fileConfig, logf func(string, ...any)) func(net.Addr, string) string {
	if fc.BackendAddr != "" && fc.BackendAddr != "builtin" {
		return nil
	}
	list := lbLocalList(fc)
	if len(list) == 0 || fc.CoverSeed == "" {
		return nil
	}
	_, _, prim, _ := lbLists(fc)
	byIP := map[string]string{}
	bySNI := map[string]string{}
	for _, e := range list {
		ip := net.ParseIP(e.IP)
		if ip == nil || ip.String() == prim {
			continue
		}
		label := e.SNI
		if label == "" {
			label = ip.String()
		}
		addr, err := startBuiltinBackendLabel(fc.CoverSeed, label)
		if err != nil {
			if logf != nil {
				logf("lb: cover site for %s: %v (the main one is served there)", ip, err)
			}
			continue
		}
		byIP[ip.String()] = addr
		if e.SNI != "" {
			bySNI[strings.ToLower(e.SNI)] = addr
		}
	}
	if len(byIP) == 0 {
		return nil
	}
	return func(local net.Addr, sni string) string {
		if ta, ok := local.(*net.TCPAddr); ok {
			ip := ta.IP
			if v4 := ip.To4(); v4 != nil {
				ip = v4
			}
			if a := byIP[ip.String()]; a != "" {
				return a
			}
		}
		return bySNI[strings.ToLower(sni)]
	}
}
