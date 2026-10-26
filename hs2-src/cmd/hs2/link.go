package main

// C15 (docs/architecture/REDESIGN.fa.md §3.16, §4.10): the hs2:// setup link,
// rebuilt from a tunnel's config.
//
//	hs2 link -c <config> [--endpoint IP:PORT] [--domain NAME] [--transport tcp|tun] [--udp true|false] [--unit NAME]
//	hs2 link --list
//	hs2 link --decode [LINK | -]        (no LINK, or "-": read it from stdin)
//
// The link is the installer's: "hs2://" + base64 (standard, padded, one line)
// of 13 fields joined by '|' with no escaping —
//
//	endpoint|domain|shared|panel|carrier|udp|transport|direction|mtu|encap|proto|unit|tun_base
//
// — and, when field 14 (alt, R9) or 15 (pin, R8) is set, "hs2v3://" + the same
// with 15 fields. A link without those is byte-for-byte the link the bash
// encoder (install.sh encode_link) made.
//
// The side that LISTENS makes the link and the side that DIALS pastes it. Every
// field is found in this order: the config's "link" block (written by the
// installer from R1 on, on both sides) → the working fields of the config →
// inference → else the command names the missing field and the flag to pass,
// rather than print a broken link. Where a working field disagrees with the
// link block (a hand edit), the working field wins with a note: the rebuilt
// link says what the tunnel runs with now. A field the pasting side never uses
// is written with parse_link's default and a note; a field it does use is
// never guessed.
//
// Exit status: 0 a complete link (stdout), 3 a field is missing (named on
// stderr), 2 an invalid config, link or flag, 4 no permission to read the
// config. Nothing is sent on the network and nothing is written anywhere: the
// link goes to stdout only; notes and refusals go to stderr and never hold it.

import (
	"bytes"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
)

const (
	linkScheme   = "hs2://"
	linkSchemeV3 = "hs2v3://"
	linkKeyNote  = "link: this link contains the tunnel key; keep it private (revoke with hs2 rekey)"
	linkUDPNote  = "link: udp is not recorded on this side; written as false, which the side that pastes this link does not use"
)

// linkFields are the link's fields, in order. Alt and Pin are the extended
// (hs2v3://) fields 14 and 15.
type linkFields struct {
	Endpoint, Domain, Shared, Panel, Carrier, UDP, Transport, Direction, MTU, Encap, Proto, Unit, TunBase string
	Alt, Pin                                                                                              string
}

var linkFieldNames = []string{"endpoint", "domain", "shared", "panel", "carrier", "udp", "transport", "direction",
	"mtu", "encap", "proto", "unit", "tun_base", "alt", "pin"}

func (f *linkFields) slots() []*string {
	return []*string{&f.Endpoint, &f.Domain, &f.Shared, &f.Panel, &f.Carrier, &f.UDP, &f.Transport, &f.Direction,
		&f.MTU, &f.Encap, &f.Proto, &f.Unit, &f.TunBase, &f.Alt, &f.Pin}
}

func (f linkFields) extended() bool { return f.Alt != "" || f.Pin != "" }

// encodeLink builds the link. A field that holds '|' or a line break cannot
// travel (the format has no escaping) and is refused.
func encodeLink(f linkFields) (string, error) {
	n, scheme := 13, linkScheme
	if f.extended() {
		n, scheme = 15, linkSchemeV3
	}
	vals := make([]string, n)
	for i, p := range f.slots()[:n] {
		if strings.ContainsAny(*p, "|\n\r\x00") {
			return "", fmt.Errorf("field %s holds '|' or a line break, which a link cannot carry", linkFieldNames[i])
		}
		vals[i] = *p
	}
	return scheme + base64.StdEncoding.EncodeToString([]byte(strings.Join(vals, "|"))), nil
}

// decodeLink reads a link in either format. Like parse_link it also takes the
// bare base64 (no prefix) and an older link with fewer fields (the missing ones
// are empty); it refuses what parse_link would read wrongly.
func decodeLink(s string) (linkFields, error) {
	var f linkFields
	s = strings.TrimSpace(s)
	v3 := false
	switch {
	case strings.HasPrefix(s, linkSchemeV3):
		s, v3 = s[len(linkSchemeV3):], true
	case strings.HasPrefix(s, linkScheme):
		s = s[len(linkScheme):]
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return f, errors.New("invalid link: it is not one piece of base64 — copy the whole link again")
	}
	if bytes.ContainsAny(raw, "\n\r\x00") {
		return f, errors.New("invalid link: a field holds a line break")
	}
	parts := strings.Split(string(raw), "|")
	switch {
	case v3 && len(parts) > 15:
		return f, fmt.Errorf("invalid link: %d fields; this hs2 reads up to 15 — the link is from a newer installer, update this one", len(parts))
	case v3 && len(parts) < 14:
		return f, fmt.Errorf("invalid link: an hs2v3:// link has 14 or 15 fields, this one %d", len(parts))
	case !v3 && len(parts) > 13:
		return f, fmt.Errorf("invalid link: %d fields; an hs2:// link has at most 13", len(parts))
	}
	for i, p := range parts {
		*f.slots()[i] = p
	}
	if f.Endpoint == "" || f.Shared == "" {
		return linkFields{}, errors.New("link is missing fields")
	}
	if f.Pin != "" && !validPin(f.Pin) {
		return linkFields{}, errors.New("invalid link: the pin is not 43 base64url characters")
	}
	return f, nil
}

// validPin: base64url without padding of a SHA-256 (43 characters).
func validPin(p string) bool {
	if len(p) != 43 {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(p)
	return err == nil && len(b) == 32
}

// linkEnv is what the command layer read besides the config: the systemd unit
// that runs it, and the names in the listener's certificate.
type linkEnv struct {
	Unit         string   // the unit whose ExecStart runs this config ("" = none found)
	UnitInferred bool     // Unit comes from the config's file name, not a unit file
	CertNames    []string // the certificate's DNS/IP names (listener side), when read
	CertErr      string   // why the certificate could not be read
}

// linkBlock is the config's "link" object: the link fields the working config
// does not hold. nil = not set.
type linkBlock struct {
	Endpoint, Domain, Transport, Panel *string
	UDP                                *bool
}

func readLinkBlock(cfg map[string]any) (linkBlock, error) {
	var lb linkBlock
	v, ok := cfg["link"]
	if !ok || v == nil {
		return lb, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return lb, errors.New(`"link" must be an object`)
	}
	str := func(k string, dst **string) error {
		x, ok := m[k]
		if !ok || x == nil {
			return nil
		}
		s, ok := x.(string)
		if !ok {
			return fmt.Errorf(`"link.%s" must be a string`, k)
		}
		*dst = &s
		return nil
	}
	for k, dst := range map[string]**string{"endpoint": &lb.Endpoint, "domain": &lb.Domain, "transport": &lb.Transport, "panel": &lb.Panel} {
		if err := str(k, dst); err != nil {
			return lb, err
		}
	}
	if x, ok := m["udp"]; ok && x != nil {
		var b bool
		switch u := x.(type) {
		case bool:
			b = u
		case string:
			p, err := strconv.ParseBool(u)
			if err != nil || (u != "true" && u != "false") {
				return lb, errors.New(`"link.udp" must be true or false`)
			}
			b = p
		default:
			return lb, errors.New(`"link.udp" must be true or false`)
		}
		lb.UDP = &b
	}
	return lb, nil
}

// cfgString / cfgBool / cfgInt read one top-level key (present?, wrong type = error).
func cfgString(cfg map[string]any, k string) (string, bool, error) {
	v, ok := cfg[k]
	if !ok || v == nil {
		return "", false, nil
	}
	s, ok := v.(string)
	if !ok {
		return "", true, fmt.Errorf("%q must be a string", k)
	}
	return s, true, nil
}

func cfgBool(cfg map[string]any, k string) (bool, bool, error) {
	v, ok := cfg[k]
	if !ok || v == nil {
		return false, false, nil
	}
	b, ok := v.(bool)
	if !ok {
		return false, true, fmt.Errorf("%q must be true or false", k)
	}
	return b, true, nil
}

func cfgInt(cfg map[string]any, k string) (int64, bool, error) {
	v, ok := cfg[k]
	if !ok || v == nil {
		return 0, false, nil
	}
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0, true, fmt.Errorf("%q must be a whole number", k)
		}
		return i, true, nil
	case float64:
		if n != float64(int64(n)) {
			return 0, true, fmt.Errorf("%q must be a whole number", k)
		}
		return int64(n), true, nil
	}
	return 0, true, fmt.Errorf("%q must be a number", k)
}

// transportFits: the transports the installer pairs with each carrier.
func transportFits(carrier, tr string) bool {
	switch carrier {
	case "mtcp":
		return tr == "tcp"
	case "l3mtcp", "l3", "tls":
		return tr == "tcp" || tr == "tun"
	case "dgtun":
		return tr == "tun"
	case "udp":
		return tr == "udp"
	case "auto":
		return tr == "auto"
	}
	return false
}

// tunBase is install.sh block_of: the /30 base of local_cidr's address.
func tunBase(localCIDR string) (string, bool) {
	ip := strings.SplitN(localCIDR, "/", 2)[0]
	p := net.ParseIP(ip)
	if p == nil || p.To4() == nil {
		return "", false
	}
	v4 := p.To4()
	return fmt.Sprintf("%d.%d.%d.%d", v4[0], v4[1], v4[2], v4[3]/4*4), true
}

// buildLink rebuilds the link from a config (decoded with UseNumber), the unit
// and certificate the command layer found, and the operator's flags. notes are
// for stderr (including one line per missing field); missing names the fields
// that could not be found; err is an invalid config. Pure: no I/O.
func buildLink(cfg map[string]any, env linkEnv, ov linkFields) (link string, notes, missing []string, err error) {
	f, notes, missing, err := resolveLink(cfg, env, ov)
	if err != nil || len(missing) > 0 {
		return "", notes, missing, err
	}
	link, err = encodeLink(f)
	if err != nil {
		return "", notes, nil, err
	}
	return link, notes, nil, nil
}

func resolveLink(cfg map[string]any, env linkEnv, ov linkFields) (f linkFields, notes, missing []string, err error) {
	note := func(s string, a ...any) { notes = append(notes, fmt.Sprintf(s, a...)) }
	need := func(name, s string, a ...any) { missing = append(missing, name); note(s, a...) }

	mode, _, err := cfgString(cfg, "mode")
	if err != nil {
		return f, nil, nil, err
	}
	if mode != "dial" && mode != "listen" {
		return f, nil, nil, fmt.Errorf(`"mode" must be "dial" or "listen", got %q`, mode)
	}
	reverse, _, err := cfgBool(cfg, "reverse")
	if err != nil {
		return f, nil, nil, err
	}
	carrier, _, err := cfgString(cfg, "carrier")
	if err != nil {
		return f, nil, nil, err
	}
	switch carrier {
	case "mtcp", "l3mtcp", "l3", "tls", "dgtun", "udp", "auto":
	default:
		return f, nil, nil, fmt.Errorf("carrier %q has no hs2:// link (the installer makes links for mtcp, l3mtcp, tls, dgtun, udp and auto)", carrier)
	}
	stream := carrier == "mtcp" || carrier == "l3mtcp" || carrier == "l3" || carrier == "tls"
	dialer := (mode == "dial") != reverse // this side pastes the link
	lb, err := readLinkBlock(cfg)
	if err != nil {
		return f, nil, nil, err
	}
	addr, _, err := cfgString(cfg, "addr")
	if err != nil {
		return f, nil, nil, err
	}
	shared, _, err := cfgString(cfg, "shared_key")
	if err != nil {
		return f, nil, nil, err
	}
	if shared == "" {
		return f, nil, nil, errors.New(`"shared_key" is missing`)
	}
	f.Shared, f.Carrier, f.Direction = shared, carrier, "direct"
	if reverse {
		f.Direction = "reverse"
	}

	// 7 transport: tcp, tun, udp or auto — only l3mtcp and tls are both.
	switch {
	case ov.Transport != "":
		f.Transport = ov.Transport
	case lb.Transport != nil && *lb.Transport != "":
		f.Transport = *lb.Transport
	default:
		switch carrier {
		case "mtcp":
			f.Transport = "tcp"
		case "dgtun":
			f.Transport = "tun"
		case "udp", "auto":
			f.Transport = carrier
		default:
			need("transport", "link: transport is ambiguous for carrier %s (tcp or tun); pass --transport tcp|tun", carrier)
		}
	}
	if f.Transport != "" && !transportFits(carrier, f.Transport) {
		return f, notes, nil, fmt.Errorf("transport %q cannot be carrier %q's (mtcp is tcp; l3mtcp and tls are tcp or tun; dgtun is tun; udp and auto are themselves)", f.Transport, carrier)
	}

	// 1 endpoint: what the pasting side dials.
	switch {
	case ov.Endpoint != "":
		f.Endpoint = ov.Endpoint
	case dialer:
		if addr == "" {
			return f, notes, nil, errors.New(`"addr" is missing`)
		}
		f.Endpoint = addr
		if lb.Endpoint != nil && *lb.Endpoint != "" && *lb.Endpoint != addr {
			note("link: the link block's endpoint %s differs from addr %s; the link uses addr, which this tunnel dials", *lb.Endpoint, addr)
		}
	case lb.Endpoint != nil && *lb.Endpoint != "":
		f.Endpoint = *lb.Endpoint
		// The listener's public port is its listen port (the installer used one
		// for both): if the listen port moved, the link follows it.
		if _, lp, e := net.SplitHostPort(addr); e == nil {
			if h, bp, e := net.SplitHostPort(f.Endpoint); e == nil && bp != lp {
				f.Endpoint = net.JoinHostPort(h, lp)
				note("link: this side now listens on port %s, not %s as the link block says; the link uses port %s (pass --endpoint to give another)", lp, bp, lp)
			}
		}
	default:
		host, _, e := net.SplitHostPort(addr)
		if e != nil {
			host = addr
		}
		if ip := net.ParseIP(host); host == "" || ip != nil && ip.IsUnspecified() {
			need("endpoint", "link: this config lacks endpoint (the server listens on all addresses and its public address was never stored); pass --endpoint <public-ip:port>")
		} else {
			need("endpoint", "link: this config lacks endpoint (the server listens on %s and its public address was never stored); pass --endpoint <public-ip:port>", host)
		}
	}

	// 2 domain: the TLS name the pasting side checks; "-" without TLS.
	switch {
	case ov.Domain != "":
		f.Domain = ov.Domain
	case !stream:
		f.Domain = "-"
		if lb.Domain != nil {
			f.Domain = *lb.Domain
		}
	case dialer:
		sni, _, e := cfgString(cfg, "sni")
		if e != nil {
			return f, notes, nil, e
		}
		switch {
		case sni != "":
			f.Domain = sni
			if lb.Domain != nil && *lb.Domain != sni {
				note("link: the link block's domain %s differs from sni %s; the link uses sni", *lb.Domain, sni)
			}
		case lb.Domain != nil && *lb.Domain != "":
			f.Domain = *lb.Domain
		default:
			need("domain", `link: this config lacks domain ("sni" is empty); pass --domain <name>`)
		}
	case lb.Domain != nil && *lb.Domain != "":
		f.Domain = *lb.Domain
	default:
		var names []string
		for _, n := range env.CertNames {
			if !strings.HasPrefix(n, "*") {
				names = append(names, n)
			}
		}
		switch {
		case env.CertErr != "":
			need("domain", "link: this config lacks domain (the certificate could not be read: %s); pass --domain <name>", env.CertErr)
		case len(names) == 1 && len(env.CertNames) == 1:
			f.Domain = names[0]
		case len(env.CertNames) == 0:
			need("domain", "link: this config lacks domain (the certificate names none); pass --domain <name>")
		default:
			need("domain", "link: this config lacks domain (the certificate names %d: %s); pass --domain <name>", len(env.CertNames), strings.Join(env.CertNames, ", "))
		}
	}

	// 4 panel: informational on the pasting side; "-" for none.
	blockPanel := func(def string) string {
		if lb.Panel != nil {
			return *lb.Panel
		}
		return def
	}
	switch {
	case ov.Panel != "":
		f.Panel = ov.Panel
	case reverse || carrier == "udp" || carrier == "auto":
		f.Panel = blockPanel("-")
	case !dialer: // the exit of a direct tunnel: its default panel
		exp, has, e := cfgString(cfg, "expose")
		if e != nil {
			return f, notes, nil, e
		}
		switch {
		case has:
			if exp == "" {
				exp = "-"
			}
			f.Panel = exp
			if lb.Panel != nil && *lb.Panel != exp {
				note("link: the link block's panel %s differs from expose %s; the link uses expose", *lb.Panel, exp)
			}
		case lb.Panel != nil:
			f.Panel = *lb.Panel
		default:
			f.Panel = "-"
			note("link: panel is not recorded on this side; written as -, which the side that pastes this link only shows")
		}
	default: // the edge of a direct tunnel: the panel as the exit told it
		pp, has, e := cfgString(cfg, "peer_panel")
		if e != nil {
			return f, notes, nil, e
		}
		switch {
		case has:
			f.Panel = pp
			if lb.Panel != nil && *lb.Panel != pp {
				note("link: the link block's panel %s differs from peer_panel %s; the link uses peer_panel", *lb.Panel, pp)
			}
		case lb.Panel != nil:
			f.Panel = *lb.Panel
		default:
			f.Panel = "-"
			note("link: panel is not recorded on this side; written as -, which the side that pastes this link does not use")
		}
	}

	// 6 udp: whether the edge also forwards UDP on the user ports.
	u, hasU, err := cfgBool(cfg, "udp")
	if err != nil {
		return f, notes, nil, err
	}
	switch {
	case ov.UDP != "":
		f.UDP = ov.UDP
	case hasU:
		f.UDP = strconv.FormatBool(u)
		if lb.UDP != nil && *lb.UDP != u {
			note("link: the link block's udp %v differs from udp %v; the link uses udp", *lb.UDP, u)
		}
	case lb.UDP != nil:
		f.UDP = strconv.FormatBool(*lb.UDP)
	case !stream:
		f.UDP = "false" // the installer always writes false for these
	case !reverse: // the Iran edge pastes it and writes it into its config
		need("udp", "link: this config lacks udp (whether the Iran server also forwards UDP on the user ports was never stored here); pass --udp true|false")
	default:
		f.UDP = "false"
		note(linkUDPNote)
	}

	// 9 mtu, 10 encap, 11 proto follow the transport.
	switch {
	case f.Transport == "tun" && stream:
		mtu, _, e := cfgInt(cfg, "mtu")
		if e != nil {
			return f, notes, nil, e
		}
		if mtu <= 0 {
			need("mtu", "link: this config lacks mtu (a tun over TLS link carries the tunnel MTU); set \"mtu\" in the config")
		} else {
			f.MTU = strconv.FormatInt(mtu, 10)
		}
		f.Encap = "tcp"
	case carrier == "dgtun":
		f.MTU = "1280" // fixed for the datagram tun, on both sides
		enc, _, e := cfgString(cfg, "encap")
		if e != nil {
			return f, notes, nil, e
		}
		if enc == "" {
			enc = "udp"
		}
		f.Encap = enc
		if enc == "ipx" {
			p, _, e := cfgInt(cfg, "proto")
			if e != nil {
				return f, notes, nil, e
			}
			if p <= 0 {
				p = 253 // the installer's default, left out of the config
			}
			f.Proto = strconv.FormatInt(p, 10)
		}
	}

	// 12 unit, 13 tun_base: the name and the /30 the other side reuses.
	switch {
	case ov.Unit != "":
		f.Unit = ov.Unit
	case env.Unit != "":
		f.Unit = env.Unit
		if env.UnitInferred {
			note("link: unit %s comes from the config's file name (no systemd unit here runs this config)", env.Unit)
		}
	default:
		need("unit", "link: this config lacks unit (no systemd unit runs it); pass --unit <name>")
	}
	lc, _, err := cfgString(cfg, "local_cidr")
	if err != nil {
		return f, notes, nil, err
	}
	if b, ok := tunBase(lc); ok {
		f.TunBase = b
	} else {
		need("tun_base", "link: this config lacks local_cidr (the tunnel's /30), so the link cannot name the tunnel subnet")
	}

	// 14 alt, 15 pin: the extended form (R9, R8), only when the config has them.
	switch a := cfg["alt_addrs"].(type) {
	case string:
		f.Alt = a
	case []any:
		var s []string
		for _, x := range a {
			v, ok := x.(string)
			if !ok {
				return f, notes, nil, errors.New(`"alt_addrs" must be a list of addresses`)
			}
			s = append(s, v)
		}
		f.Alt = strings.Join(s, ",")
	}
	if f.Pin, _, err = cfgString(cfg, "pin_spki"); err != nil {
		return f, notes, nil, err
	}
	if f.Pin != "" && !validPin(f.Pin) {
		return f, notes, nil, errors.New(`"pin_spki" is not 43 base64url characters`)
	}
	return f, notes, missing, nil
}

// ---- the command --------------------------------------------------------------

var (
	linkUnitDir  = "/etc/systemd/system" // where the tunnels' units are (tests point it elsewhere)
	linkReadFile = os.ReadFile           // tests replace it to simulate no permission
)

func decodeConfigMap(b []byte) (map[string]any, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var m map[string]any
	if err := d.Decode(&m); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, errors.New("the config is not a JSON object")
	}
	return m, nil
}

var (
	reUnitName = regexp.MustCompile(`^hs2(-[a-z0-9]([a-z0-9-]{0,18}[a-z0-9])?)?$`)
	reDomain   = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$`)
	reExecC    = regexp.MustCompile(`(?m)^ExecStart=.* -c ([^ \n]*)`)
)

// validEndpoint: IPv4:port, [IPv6]:port, name:port — or a bare IP (the raw
// datagram encapsulations have no port).
func validEndpoint(s string) bool {
	if strings.ContainsAny(s, "|\n\r\x00 ") || s == "" {
		return false
	}
	if net.ParseIP(s) != nil {
		return true
	}
	h, p, err := net.SplitHostPort(s)
	if err != nil || h == "" {
		return false
	}
	n, err := strconv.Atoi(p)
	return err == nil && n >= 1 && n <= 65535 && strconv.Itoa(n) == p
}

func checkLinkFlags(ov linkFields) error {
	switch {
	case ov.Endpoint != "" && !validEndpoint(ov.Endpoint):
		return fmt.Errorf("--endpoint %q: give the public address as IP:PORT ([IPv6]:PORT), or a bare IP for icmp/gre/ipip/ipx", ov.Endpoint)
	case ov.Domain != "" && ov.Domain != "-" && (!reDomain.MatchString(ov.Domain) || strings.Contains(ov.Domain, "..") || len(ov.Domain) > 253):
		return fmt.Errorf("--domain %q: give a domain name (letters, digits, '.', '-')", ov.Domain)
	case ov.Transport != "" && ov.Transport != "tcp" && ov.Transport != "tun" && ov.Transport != "udp" && ov.Transport != "auto":
		return fmt.Errorf("--transport %q: tcp, tun, udp or auto", ov.Transport)
	case ov.UDP != "" && ov.UDP != "true" && ov.UDP != "false":
		return fmt.Errorf("--udp %q: true or false", ov.UDP)
	case ov.Unit != "" && (!reUnitName.MatchString(ov.Unit) || ov.Unit == "hs2-menu"):
		return fmt.Errorf("--unit %q: hs2 or hs2-<name> (a-z, 0-9, '-', up to 20)", ov.Unit)
	}
	return nil
}

// hs2UnitFiles: hs2.service first, then hs2-*.service by name (install.sh tm_units).
func hs2UnitFiles(dir string) []string {
	var out []string
	if _, err := os.Lstat(filepath.Join(dir, "hs2.service")); err == nil {
		out = append(out, filepath.Join(dir, "hs2.service"))
	}
	more, _ := filepath.Glob(filepath.Join(dir, "hs2-*.service"))
	sort.Strings(more)
	return append(out, more...)
}

// unitConfig: the config a unit file runs (ExecStart … -c PATH, install.sh tm_cfg).
func unitConfig(unitFile string) string {
	b, err := os.ReadFile(unitFile)
	if err != nil {
		return ""
	}
	if m := reExecC.FindSubmatch(b); m != nil {
		return string(m[1])
	}
	return ""
}

func absClean(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return filepath.Clean(p)
}

// linkEnvFor finds the unit that runs cfgPath and, on the listener side, the
// names in its certificate.
func linkEnvFor(cfg map[string]any, cfgPath string) linkEnv {
	var env linkEnv
	want := absClean(cfgPath)
	for _, u := range hs2UnitFiles(linkUnitDir) {
		if c := unitConfig(u); c != "" && absClean(c) == want {
			env.Unit = strings.TrimSuffix(filepath.Base(u), ".service")
			break
		}
	}
	if env.Unit == "" {
		switch base := filepath.Base(cfgPath); {
		case base == "config.json":
			env.Unit, env.UnitInferred = "hs2", true
		case strings.HasSuffix(base, ".json") && reUnitName.MatchString(strings.TrimSuffix(base, ".json")):
			env.Unit, env.UnitInferred = strings.TrimSuffix(base, ".json"), true
		}
	}
	mode, _ := cfg["mode"].(string)
	reverse, _ := cfg["reverse"].(bool)
	if cf, _ := cfg["cert_file"].(string); cf != "" && (mode == "dial") == reverse {
		env.CertNames, env.CertErr = certNames(cf)
	}
	return env
}

// certNames: the DNS and IP names of the first certificate in a PEM file.
func certNames(path string) ([]string, string) {
	b, err := linkReadFile(path)
	if err != nil {
		return nil, err.Error()
	}
	for {
		var blk *pem.Block
		blk, b = pem.Decode(b)
		if blk == nil {
			return nil, "no certificate in " + path
		}
		if blk.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(blk.Bytes)
		if err != nil {
			return nil, err.Error()
		}
		names := append([]string(nil), c.DNSNames...)
		for _, ip := range c.IPAddresses {
			names = append(names, ip.String())
		}
		return names, ""
	}
}

// linkCmd is `hs2 link`; it returns the exit status.
func linkCmd(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("link", flag.ContinueOnError)
	fl.SetOutput(stderr)
	cfgPath := fl.String("c", "", "the tunnel's config (JSON)")
	list := fl.Bool("list", false, "list this server's tunnels and whether each link can be rebuilt")
	decode := fl.Bool("decode", false, "print a link's fields (the link as the argument, or on stdin)")
	var ov linkFields
	fl.StringVar(&ov.Endpoint, "endpoint", "", "this listener's public address IP:PORT (when the config does not have it)")
	fl.StringVar(&ov.Domain, "domain", "", "the TLS domain (when the config does not have it)")
	fl.StringVar(&ov.Transport, "transport", "", "tcp or tun (for l3mtcp/tls when the config does not say)")
	fl.StringVar(&ov.UDP, "udp", "", "true or false: the Iran server also forwards UDP on the user ports")
	fl.StringVar(&ov.Unit, "unit", "", "the tunnel's service name (when no systemd unit runs this config)")
	usage := func() int {
		fmt.Fprintln(stderr, "usage: hs2 link -c config.json [--endpoint IP:PORT] [--domain NAME] [--transport tcp|tun] [--udp true|false] [--unit NAME]")
		fmt.Fprintln(stderr, "       hs2 link --list")
		fmt.Fprintln(stderr, "       hs2 link --decode [LINK | -]")
		return 2
	}
	if err := fl.Parse(args); err != nil {
		return usage()
	}
	switch {
	case *decode:
		return linkDecodeCmd(fl.Args(), stdin, stdout, stderr)
	case *list:
		return linkListCmd(stdout, stderr)
	case *cfgPath == "" || fl.NArg() > 0:
		return usage()
	}
	if err := checkLinkFlags(ov); err != nil {
		fmt.Fprintln(stderr, "link: "+err.Error())
		return 2
	}
	cfg, code := readLinkConfig(*cfgPath, stderr)
	if code != 0 {
		return code
	}
	link, notes, missing, err := buildLink(cfg, linkEnvFor(cfg, *cfgPath), ov)
	for _, n := range notes {
		fmt.Fprintln(stderr, n)
	}
	switch {
	case err != nil:
		fmt.Fprintf(stderr, "link: %s is not a valid tunnel config: %v\n", *cfgPath, err)
		return 2
	case len(missing) > 0:
		fmt.Fprintf(stderr, "link: missing: %s (run: hs2 link -c %s%s)\n", strings.Join(missing, " "), *cfgPath, flagHint(missing))
		return 3
	}
	fmt.Fprintln(stdout, link)
	fmt.Fprintln(stderr, linkKeyNote)
	return 0
}

func flagHint(missing []string) string {
	var b strings.Builder
	for _, m := range missing {
		switch m {
		case "endpoint":
			b.WriteString(" --endpoint <public-ip:port>")
		case "domain":
			b.WriteString(" --domain <name>")
		case "transport":
			b.WriteString(" --transport tcp|tun")
		case "udp":
			b.WriteString(" --udp true|false")
		case "unit":
			b.WriteString(" --unit <name>")
		}
	}
	return b.String()
}

// readLinkConfig reads and decodes a config: exit status 4 without permission
// (it holds the tunnel key: root's, 0600), 2 when it is missing or not JSON.
func readLinkConfig(path string, stderr io.Writer) (map[string]any, int) {
	raw, err := linkReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			fmt.Fprintf(stderr, "link: cannot read %s: permission denied — a tunnel's config holds its key and only root can read it; run this as root (sudo)\n", path)
			return nil, 4
		}
		fmt.Fprintf(stderr, "link: cannot read %s: %v\n", path, err)
		return nil, 2
	}
	cfg, err := decodeConfigMap(raw)
	if err != nil {
		fmt.Fprintf(stderr, "link: %s is not valid JSON: %v\n", path, err)
		return nil, 2
	}
	return cfg, 0
}

// linkDecodeCmd prints the fields of a link as name=value lines (format first),
// the link given as the argument or on stdin.
func linkDecodeCmd(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	var s string
	switch {
	case len(args) > 1:
		fmt.Fprintln(stderr, "usage: hs2 link --decode [LINK | -]")
		return 2
	case len(args) == 1 && args[0] != "-":
		s = args[0]
	default:
		b, err := io.ReadAll(io.LimitReader(stdin, 1<<16))
		if err != nil {
			fmt.Fprintf(stderr, "link: %v\n", err)
			return 2
		}
		s = string(b)
	}
	f, err := decodeLink(s)
	if err != nil {
		fmt.Fprintln(stderr, "link: "+err.Error())
		return 2
	}
	var b strings.Builder
	n := 13
	if strings.HasPrefix(strings.TrimSpace(s), linkSchemeV3) {
		b.WriteString("format=hs2v3\n")
		n = 15
	} else {
		b.WriteString("format=hs2\n")
	}
	for i, p := range f.slots()[:n] {
		fmt.Fprintf(&b, "%s=%s\n", linkFieldNames[i], *p)
	}
	io.WriteString(stdout, b.String())
	return 0
}

// linkListCmd lists this server's tunnels: unit, config, side, carrier,
// direction, whether this side makes or pastes the link, and whether the link
// can be rebuilt from the config alone (never the link itself).
func linkListCmd(stdout, stderr io.Writer) int {
	units := hs2UnitFiles(linkUnitDir)
	if len(units) == 0 {
		fmt.Fprintf(stderr, "link: no hs2 tunnel on this server (no hs2*.service in %s)\n", linkUnitDir)
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "UNIT\tCONFIG\tSIDE\tCARRIER\tDIRECTION\tLINK\tSTATUS")
	code := 0
	for _, uf := range units {
		u := strings.TrimSuffix(filepath.Base(uf), ".service")
		p := unitConfig(uf)
		if p == "" {
			fmt.Fprintf(tw, "%s\t?\t?\t?\t?\t?\tno config in the unit (ExecStart … -c)\n", u)
			continue
		}
		raw, err := linkReadFile(p)
		if err != nil {
			st := "cannot read the config: " + err.Error()
			if errors.Is(err, fs.ErrPermission) {
				st, code = "cannot read the config (run as root)", 4
			}
			fmt.Fprintf(tw, "%s\t%s\t?\t?\t?\t?\t%s\n", u, p, st)
			continue
		}
		cfg, err := decodeConfigMap(raw)
		if err != nil {
			fmt.Fprintf(tw, "%s\t%s\t?\t?\t?\t?\tnot valid JSON\n", u, p)
			continue
		}
		mode, _ := cfg["mode"].(string)
		reverse, _ := cfg["reverse"].(bool)
		carrier, _ := cfg["carrier"].(string)
		side, dir, role := "exit (Kharej)", "direct", "makes"
		if mode == "dial" {
			side = "edge (Iran)"
		}
		if reverse {
			dir = "reverse"
		}
		if (mode == "dial") != reverse {
			role = "pastes"
		}
		env := linkEnvFor(cfg, p)
		if env.Unit == "" {
			env.Unit = u
		}
		_, _, missing, err := buildLink(cfg, env, linkFields{})
		st := "rebuildable"
		switch {
		case err != nil:
			st = "invalid: " + err.Error()
		case len(missing) > 0:
			st = "needs: " + strings.Join(missing, ", ")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", u, p, side, carrier, dir, role, st)
	}
	tw.Flush()
	return code
}

// linkStatusLine is §3.16's status line for a config, without the link; "" if
// the config cannot be read or has no link at all.
func linkStatusLine(cfgPath string) string {
	raw, err := linkReadFile(cfgPath)
	if err != nil {
		return ""
	}
	cfg, err := decodeConfigMap(raw)
	if err != nil {
		return ""
	}
	_, _, missing, err := buildLink(cfg, linkEnvFor(cfg, cfgPath), linkFields{})
	switch {
	case err != nil:
		return ""
	case len(missing) > 0:
		return fmt.Sprintf("link: needs %s (run: hs2 link -c %s)", strings.Join(missing, ", "), cfgPath)
	}
	return "link: rebuildable from this config"
}
