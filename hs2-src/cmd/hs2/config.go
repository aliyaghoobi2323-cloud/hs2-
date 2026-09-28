package main

import (
	"encoding/hex"
	"fmt"
	"net"
	"strconv"
	"strings"
)

// minKeyBytes is the shortest shared key accepted. The installer generates 32
// random bytes; anything much shorter is a truncated paste or a typo.
const minKeyBytes = 16

// validate rejects a config that would otherwise start in a broken or unsafe
// state. Every problem is reported at once, with the field name, so a single
// look at the journal says what to fix.
func validate(fc fileConfig) error {
	var errs []string
	bad := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }

	switch fc.Mode {
	case "dial", "listen":
	default:
		bad(`mode must be "dial" (Iran) or "listen" (kharej), got %q`, fc.Mode)
	}
	tlsCarrier := false
	switch fc.Carrier {
	case "mtcp", "l3mtcp", "l3", "tls":
		tlsCarrier = true
	case "reality", "noise", "":
	default:
		bad("unknown carrier %q (use mtcp, l3mtcp or tls)", fc.Carrier)
	}

	if tlsCarrier || fc.Carrier == "reality" {
		// A key that does not decode used to become an EMPTY key silently,
		// which anyone can compute. Refuse it instead.
		k, err := hex.DecodeString(strings.TrimSpace(fc.SharedKey))
		switch {
		case fc.SharedKey == "":
			bad("shared_key is missing")
		case err != nil:
			bad("shared_key is not valid hex (%v); copy it again from the kharej config or setup link", err)
		case len(k) < minKeyBytes:
			bad("shared_key is only %d bytes; at least %d are required", len(k), minKeyBytes)
		}
	}

	if err := checkHostPort(fc.Addr, fc.Mode == "listen"); err != nil {
		bad("addr %q: %v", fc.Addr, err)
	}

	if tlsCarrier && fc.Mode == "listen" {
		if err := checkHostPort(fc.Expose, false); err != nil {
			bad("expose (panel address) %q: %v", fc.Expose, err)
		}
		if fc.CertFile == "" || fc.KeyFile == "" {
			bad("cert_file and key_file are required on the kharej server")
		}
	}

	if tlsCarrier && fc.Mode == "dial" {
		if fc.SNI == "" {
			bad("sni (the kharej domain) is missing")
		}
		ports := splitComma(fc.ForwardPorts)
		if len(ports) == 0 {
			bad("forward_ports is empty; list at least one user port")
		}
		seen := map[string]bool{}
		for _, p := range ports {
			if !validPort(p) {
				bad("forward_ports: %q is not a port number (1-65535)", p)
			} else if seen[p] {
				bad("forward_ports: port %s is listed twice", p)
			}
			seen[p] = true
		}
		if fc.BindLocalIP != "" && net.ParseIP(fc.BindLocalIP) == nil {
			bad("bind_local_ip %q is not an IP address (leave it empty for automatic)", fc.BindLocalIP)
		}
		if fc.UserListenIP != "" && net.ParseIP(fc.UserListenIP) == nil {
			bad("user_listen_ip %q is not an IP address (leave it empty for all addresses)", fc.UserListenIP)
		}
		if fc.MinLinks < 0 || fc.MaxLinks < 0 || fc.PerLink < 0 {
			bad("min_links, max_links and per_link must not be negative")
		}
		if fc.MaxLinks > 0 && fc.MinLinks > fc.MaxLinks {
			bad("min_links (%d) is larger than max_links (%d)", fc.MinLinks, fc.MaxLinks)
		}
		if fc.MaxLinks > 64 {
			bad("max_links %d is unreasonable (at most 64)", fc.MaxLinks)
		}
	}

	if fc.MTU != 0 && (fc.MTU < 576 || fc.MTU > 9000) {
		bad("mtu %d is outside 576-9000", fc.MTU)
	}
	if len(errs) > 0 {
		return fmt.Errorf("invalid config:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

func validPort(s string) bool {
	n, err := strconv.Atoi(s)
	return err == nil && n >= 1 && n <= 65535 && strconv.Itoa(n) == s
}

// checkHostPort accepts "host:port" or "[v6]:port". The host may be empty
// (all addresses) only when allowEmptyHost is set, i.e. for a listen address.
func checkHostPort(s string, allowEmptyHost bool) error {
	if s == "" {
		return fmt.Errorf("missing")
	}
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return fmt.Errorf("want host:port (IPv6 as [addr]:port): %v", err)
	}
	if !validPort(port) {
		return fmt.Errorf("port %q is not 1-65535", port)
	}
	if host == "" {
		if allowEmptyHost {
			return nil
		}
		return fmt.Errorf("host is missing")
	}
	if net.ParseIP(host) != nil {
		return nil
	}
	if !validHostname(host) {
		return fmt.Errorf("%q is neither an IP address nor a valid host name", host)
	}
	return nil
}

func validHostname(h string) bool {
	if len(h) == 0 || len(h) > 253 {
		return false
	}
	labels := strings.Split(strings.TrimSuffix(h, "."), ".")
	// A name ending in an all-digit label is a mistyped IP (1.2.3.4.5), not a
	// host name: top-level domains are never numeric (RFC 3696 §2).
	if strings.Trim(labels[len(labels)-1], "0123456789") == "" {
		return false
	}
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}
