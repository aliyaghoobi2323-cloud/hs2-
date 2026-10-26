package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Fixtures: documentation addresses (RFC 5737 / 3849) and a made-up key —
// never a real server, key or link.
const (
	tShared  = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	tKharej  = "192.0.2.10"    // the exit's public address (direct link)
	tIran    = "198.51.100.20" // the edge's public address (reverse link)
	tDomain  = "tunnel.example.com"
	tPanel   = "127.0.0.1:8443"
	tOwnPan  = "127.0.0.1:9443" // a reverse exit's own panel: never in the link
	tUnit    = "hs2-rt"
	tBase    = "10.77.4.0"
	tAlt     = "203.0.113.7:2096,[2001:db8::7]:2096"
	tTunMTU  = 1320
	tKeyNote = "link: this link contains the tunnel key; keep it private (if it leaks, reinstall the tunnel on both servers; hs2 rekey arrives in a later release)"
)

var tPin = func() string {
	s := sha256.Sum256([]byte("hs2 test pin"))
	return base64.RawURLEncoding.EncodeToString(s[:])
}()

// rtCase is one way the installer sets a tunnel up: the arguments show_link
// receives (install.sh kharej_listener / iran_listener).
type rtCase struct {
	transport string // tcp | tun | udp | auto
	carrier   string // mtcp | l3mtcp | tls | dgtun | udp | auto
	encap     string // tun only: tcp (over TLS) | udp | icmp | gre | ipip | ipx
	proto     string // ipx only
	udp       bool
	reverse   bool
	v3        bool // the extended hs2v3:// form (fields 14 and 15)
}

func (c rtCase) stream() bool {
	return c.carrier == "mtcp" || c.carrier == "l3mtcp" || c.carrier == "tls"
}
func (c rtCase) raw() bool {
	return c.carrier == "dgtun" && (c.encap == "icmp" || c.encap == "gre" || c.encap == "ipip" || c.encap == "ipx")
}
func (c rtCase) name() string {
	d := "direct"
	if c.reverse {
		d = "reverse"
	}
	s := fmt.Sprintf("%s/%s/%s", d, c.transport, c.carrier)
	if c.encap != "" {
		s += "/" + c.encap + c.proto
	}
	if c.stream() {
		s += fmt.Sprintf("/udp=%v", c.udp)
	}
	if c.v3 {
		s += "/v3"
	}
	return s
}

// rtCases: every combination §3.16 test (1) names — tcp with mtcp/l3mtcp/tls,
// tun over TLS with l3mtcp/tls, dgtun with udp/icmp/gre/ipip/ipx (with proto),
// udp and auto — direct and reverse, in both link formats.
func rtCases() []rtCase {
	var out []rtCase
	for _, rev := range []bool{false, true} {
		for _, v3 := range []bool{false, true} {
			add := func(c rtCase) { c.reverse, c.v3 = rev, v3; out = append(out, c) }
			for _, car := range []string{"mtcp", "l3mtcp", "tls"} {
				for _, u := range []bool{false, true} {
					add(rtCase{transport: "tcp", carrier: car, udp: u})
				}
			}
			for _, car := range []string{"l3mtcp", "tls"} {
				for _, u := range []bool{false, true} {
					add(rtCase{transport: "tun", carrier: car, encap: "tcp", udp: u})
				}
			}
			for _, e := range []string{"udp", "icmp", "gre", "ipip"} {
				add(rtCase{transport: "tun", carrier: "dgtun", encap: e})
			}
			for _, p := range []string{"253", "200"} {
				add(rtCase{transport: "tun", carrier: "dgtun", encap: "ipx", proto: p})
			}
			add(rtCase{transport: "udp", carrier: "udp"})
			add(rtCase{transport: "auto", carrier: "auto"})
		}
	}
	return out
}

// installFields: exactly what show_link puts in the link for this case.
func installFields(c rtCase) linkFields {
	pub := tKharej
	if c.reverse {
		pub = tIran
	}
	psuf := ":2096"
	if c.raw() {
		psuf = ""
	}
	f := linkFields{Endpoint: pub + psuf, Domain: "-", Shared: tShared, Panel: "-", Carrier: c.carrier, UDP: "false",
		Transport: c.transport, Direction: "direct", Unit: tUnit, TunBase: tBase}
	if c.reverse {
		f.Direction = "reverse"
	}
	if c.stream() {
		f.Domain = tDomain
		f.UDP = strconv.FormatBool(c.udp)
	}
	if !c.reverse && c.carrier != "udp" && c.carrier != "auto" {
		f.Panel = tPanel
	}
	switch {
	case c.transport == "tun" && c.stream():
		f.MTU, f.Encap = strconv.Itoa(tTunMTU), "tcp"
	case c.carrier == "dgtun":
		f.MTU, f.Encap = "1280", c.encap
		if c.encap == "ipx" {
			f.Proto = c.proto
		}
	}
	if c.v3 {
		f.Alt, f.Pin = tAlt, tPin
	}
	return f
}

// withParseDefaults mirrors install.sh parse_link's defaults for older links.
func (f linkFields) withParseDefaults() linkFields {
	if f.Carrier == "" {
		f.Carrier = "mtcp"
	}
	if f.UDP == "" {
		f.UDP = "false"
	}
	if f.Transport == "" {
		switch f.Carrier {
		case "udp":
			f.Transport = "udp"
		case "auto":
			f.Transport = "auto"
		default:
			f.Transport = "tcp"
		}
	}
	if f.Direction == "" {
		f.Direction = "direct"
	}
	if f.Encap == "" && f.Transport == "tun" {
		f.Encap = "tcp"
	}
	if n, err := strconv.Atoi(f.Proto); err != nil || n < 0 || n > 255 || strconv.Itoa(n) != f.Proto {
		f.Proto = ""
	}
	if f.MTU == "-" {
		f.MTU = ""
	}
	if f.Unit == "" {
		f.Unit = "hs2"
	}
	if f.TunBase == "" {
		f.TunBase = "10.77.0.0"
	}
	return f
}

func blockOf(f linkFields) map[string]any {
	return map[string]any{"endpoint": f.Endpoint, "domain": f.Domain, "transport": f.Transport, "udp": f.UDP == "true", "panel": f.Panel}
}

// listenerConfig mirrors the config the installer writes on the side that
// MAKES the link (kharej_listener in direct, iran_listener in reverse).
func listenerConfig(c rtCase, f linkFields, block bool) map[string]any {
	psuf := ":2096"
	if c.raw() {
		psuf = ""
	}
	m := map[string]any{"carrier": c.carrier, "reverse": c.reverse, "addr": "0.0.0.0" + psuf, "iface": "hs9",
		"mtu": 1280, "shared_key": tShared}
	edge := c.reverse
	if edge {
		m["mode"], m["local_cidr"], m["peer_ip"] = "dial", "10.77.4.1/30", "10.77.4.2"
	} else {
		m["mode"], m["local_cidr"], m["peer_ip"] = "listen", "10.77.4.2/30", "10.77.4.1"
	}
	switch {
	case c.stream():
		m["mtu"] = 1380
		if c.transport == "tun" {
			m["mtu"] = tTunMTU
		}
		m["backend_addr"], m["cover_seed"], m["cert_file"], m["key_file"] = "builtin", "00", "/x/c.pem", "/x/k.pem"
		if edge {
			m["udp"], m["forward_ports"], m["user_listen_ip"] = c.udp, "8443", ""
		} else {
			m["expose"] = tPanel
		}
	case c.carrier == "dgtun":
		m["encap"] = c.encap
		if c.encap == "ipx" && c.proto != "253" {
			m["proto"] = json.Number(c.proto)
		}
		if edge {
			m["forward_ports"] = "8443"
		} else {
			m["expose"] = tPanel
		}
	}
	if c.v3 {
		m["alt_addrs"] = []any{"203.0.113.7:2096", "[2001:db8::7]:2096"}
		m["pin_spki"] = tPin
	}
	if block {
		m["link"] = blockOf(f)
	}
	return reread(m)
}

// dialerConfig mirrors the config the installer writes on the side that
// PASTES the link (iran_dialer in direct, kharej_dialer in reverse), from the
// link as parse_link read it.
func dialerConfig(p linkFields, block bool) map[string]any {
	edge := p.Direction != "reverse"
	m := map[string]any{"carrier": p.Carrier, "reverse": !edge, "addr": p.Endpoint, "iface": "hs9", "mtu": 1280,
		"shared_key": p.Shared}
	if edge {
		m["mode"], m["local_cidr"], m["peer_ip"] = "dial", "10.77.4.1/30", "10.77.4.2"
	} else {
		m["mode"], m["local_cidr"], m["peer_ip"] = "listen", "10.77.4.2/30", "10.77.4.1"
	}
	streamSide := func() {
		m["sni"] = p.Domain
		if edge {
			m["udp"], m["forward_ports"], m["peer_panel"] = p.UDP == "true", "8443", p.Panel
		} else {
			m["expose"] = tOwnPan
		}
	}
	switch {
	case p.Transport == "tcp":
		streamSide()
		m["mtu"] = 1380
	case p.Transport == "tun" && p.Encap == "tcp":
		if p.Carrier != "tls" {
			m["carrier"] = "l3mtcp" // tun_tls_carrier
		}
		streamSide()
		n, _ := strconv.Atoi(p.MTU)
		m["mtu"] = n
	case p.Transport == "tun":
		m["carrier"], m["encap"] = "dgtun", p.Encap
		if p.Encap == "ipx" && p.Proto != "" && p.Proto != "253" && p.Proto != "0" {
			m["proto"] = json.Number(p.Proto)
		}
		if edge {
			m["forward_ports"] = "8443"
		} else {
			m["expose"] = tOwnPan
		}
	}
	if p.Alt != "" {
		var alt []any
		for _, a := range strings.Split(p.Alt, ",") {
			alt = append(alt, a)
		}
		m["alt_addrs"] = alt
	}
	if p.Pin != "" {
		m["pin_spki"] = p.Pin
	}
	if block {
		m["link"] = blockOf(p)
	}
	return reread(m)
}

// reread: the map as it comes back from the file (json.Number for numbers).
func reread(m map[string]any) map[string]any {
	b, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	out, err := decodeConfigMap(b)
	if err != nil {
		panic(err)
	}
	return out
}

func mustEncode(t *testing.T, f linkFields) string {
	t.Helper()
	l, err := encodeLink(f)
	if err != nil {
		t.Fatalf("encodeLink(%+v): %v", f, err)
	}
	return l
}

// §3.16 test (1): link → parse_link → config → hs2 link is byte-for-byte the
// same link, for every carrier/transport combination, direct and reverse, from
// both sides, in both formats.
func TestLinkRoundTripAll(t *testing.T) {
	n := 0
	for _, c := range rtCases() {
		c := c
		t.Run(c.name(), func(t *testing.T) {
			want := mustEncode(t, installFields(c))
			wantPrefix := "hs2://"
			if c.v3 {
				wantPrefix = "hs2v3://"
			}
			if !strings.HasPrefix(want, wantPrefix) {
				t.Fatalf("link %q does not start with %s", want, wantPrefix)
			}
			// the side that made it
			got, notes, missing, err := buildLink(listenerConfig(c, installFields(c), true), linkEnv{Unit: tUnit}, linkFields{})
			if err != nil || len(missing) > 0 || got != want || len(notes) > 0 {
				t.Fatalf("maker side:\n got %q\nwant %q\nmissing=%v notes=%v err=%v", got, want, missing, notes, err)
			}
			// the side that pasted it
			d, err := decodeLink(want)
			if err != nil {
				t.Fatalf("decodeLink: %v", err)
			}
			got, notes, missing, err = buildLink(dialerConfig(d.withParseDefaults(), true), linkEnv{Unit: tUnit}, linkFields{})
			if err != nil || len(missing) > 0 || got != want || len(notes) > 0 {
				t.Fatalf("paster side:\n got %q\nwant %q\nmissing=%v notes=%v err=%v", got, want, missing, notes, err)
			}
		})
		n++
	}
	if n < 60 {
		t.Fatalf("only %d combinations", n)
	}
}

// A tunnel installed before R1 has no link block: the rebuilt link is either
// the install link, or refused with the missing field's name, or differs only
// in a field the pasting side does not use (with a note). Given the missing
// values as flags, it is the install link again.
func TestLinkPreR1(t *testing.T) {
	for _, c := range rtCases() {
		if c.v3 {
			continue
		}
		c := c
		t.Run(c.name(), func(t *testing.T) {
			inst := installFields(c)
			want := mustEncode(t, inst)
			ambiguous := c.carrier == "l3mtcp" || c.carrier == "tls"

			// maker side
			lc := listenerConfig(c, inst, false)
			env := linkEnv{Unit: tUnit, CertNames: []string{tDomain}}
			_, _, missing, err := buildLink(lc, env, linkFields{})
			wantMissing := []string{"endpoint"}
			if ambiguous {
				wantMissing = append(wantMissing, "transport")
			}
			if c.stream() && !c.reverse {
				wantMissing = append(wantMissing, "udp")
			}
			if err != nil || !sameSet(missing, wantMissing) {
				t.Fatalf("maker side: missing=%v want %v err=%v", missing, wantMissing, err)
			}
			ov := linkFields{Endpoint: inst.Endpoint}
			if ambiguous {
				ov.Transport = c.transport
			}
			if c.stream() && !c.reverse {
				ov.UDP = inst.UDP
			}
			got, _, missing, err := buildLink(lc, env, ov)
			if err != nil || len(missing) > 0 || got != want {
				t.Fatalf("maker side with flags:\n got %q\nwant %q\nmissing=%v err=%v", got, want, missing, err)
			}

			// paster side
			d, _ := decodeLink(want)
			dc := dialerConfig(d.withParseDefaults(), false)
			_, _, missing, err = buildLink(dc, linkEnv{Unit: tUnit}, linkFields{})
			wantMissing = nil
			if ambiguous {
				wantMissing = []string{"transport"}
			}
			if err != nil || !sameSet(missing, wantMissing) {
				t.Fatalf("paster side: missing=%v want %v err=%v", missing, wantMissing, err)
			}
			ov = linkFields{}
			if ambiguous {
				ov.Transport = c.transport
			}
			got, notes, missing, err := buildLink(dc, linkEnv{Unit: tUnit}, ov)
			exp := inst
			var wantNote string
			switch {
			case !c.reverse && c.carrier == "dgtun": // the Iran edge never stores the panel
				exp.Panel, wantNote = "-", "panel"
			case c.reverse && c.stream(): // the reverse exit never stores udp
				exp.UDP, wantNote = "false", "link: udp is not recorded on this side; written as false, which the side that pastes this link does not use"
			}
			if err != nil || len(missing) > 0 || got != mustEncode(t, exp) {
				t.Fatalf("paster side:\n got %q\nwant %q\nmissing=%v err=%v", got, mustEncode(t, exp), missing, err)
			}
			if wantNote != "" && !strings.Contains(strings.Join(notes, "\n"), wantNote) {
				t.Fatalf("paster side: notes %v do not say %q", notes, wantNote)
			}
			if wantNote == "" && len(notes) > 0 {
				t.Fatalf("paster side: unexpected notes %v", notes)
			}
		})
	}
}

func sameSet(a, b []string) bool {
	a, b = append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(a)
	sort.Strings(b)
	return strings.Join(a, ",") == strings.Join(b, ",")
}

// §3.16 test (2): the Go encoder is byte-for-byte the installer's encode_link
// (extracted from install.sh and run by bash) for the same fields, and the Go
// decoder reads back what decode_link + parse_link's `IFS='|' read` reads.
func TestLinkGoldenBash(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	src, err := os.ReadFile("../../install/install.sh")
	if err != nil {
		t.Fatal(err)
	}
	var enc, dec string
	for _, l := range strings.Split(string(src), "\n") {
		switch {
		case strings.HasPrefix(l, "encode_link(){"):
			enc = l
		case strings.HasPrefix(l, "decode_link(){"):
			dec = l
		}
	}
	if enc == "" || dec == "" {
		t.Fatal("encode_link/decode_link not found in install.sh")
	}
	sets := []linkFields{}
	for _, c := range rtCases() {
		if !c.v3 {
			sets = append(sets, installFields(c))
		}
	}
	sets = append(sets,
		linkFields{Endpoint: "[2001:db8::1]:443", Domain: "a-b.example", Shared: tShared, Panel: "-", Carrier: "mtcp",
			UDP: "false", Transport: "tcp", Direction: "direct", Unit: "hs2", TunBase: "10.77.0.0"},
		// anything but '|' and a line break travels as is: spaces, UTF-8, long fields
		linkFields{Endpoint: "x y:1", Domain: "دامنه.example", Shared: strings.Repeat("f", 200), Panel: "p q",
			Carrier: "mtcp", Unit: "hs2-ünï", TunBase: "10.77.255.252"},
		linkFields{Endpoint: "e:1", Shared: "s"}, // trailing empty fields
	)
	script := enc + "\n" + dec + `
printf 'hs2://%s\n' "$(encode_link "$1")"
DEC=$(decode_link "${2#hs2://}")
IFS='|' read -r A B C D E F G H I J K L M <<< "$DEC"
printf '%s\n' "$A" "$B" "$C" "$D" "$E" "$F" "$G" "$H" "$I" "$J" "$K" "$L" "$M"
`
	for i, f := range sets {
		want := mustEncode(t, f)
		raw := strings.Join([]string{f.Endpoint, f.Domain, f.Shared, f.Panel, f.Carrier, f.UDP, f.Transport,
			f.Direction, f.MTU, f.Encap, f.Proto, f.Unit, f.TunBase}, "|")
		out, err := exec.Command(bash, "-c", script, "golden", raw, want).Output()
		if err != nil {
			t.Fatalf("set %d: bash: %v", i, err)
		}
		lines := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
		if len(lines) != 14 {
			t.Fatalf("set %d: bash printed %d lines", i, len(lines))
		}
		if lines[0] != want {
			t.Fatalf("set %d: encode_link\n bash %q\n   go %q", i, lines[0], want)
		}
		d, err := decodeLink(want)
		if err != nil {
			t.Fatalf("set %d: decodeLink: %v", i, err)
		}
		goFields := []string{d.Endpoint, d.Domain, d.Shared, d.Panel, d.Carrier, d.UDP, d.Transport, d.Direction,
			d.MTU, d.Encap, d.Proto, d.Unit, d.TunBase}
		if strings.Join(lines[1:], "\n") != strings.Join(goFields, "\n") {
			t.Fatalf("set %d: decode\n bash %q\n   go %q", i, lines[1:], goFields)
		}
	}
}

// §3.16 test (3): a listener on 0.0.0.0 without a link block is refused with
// the field's name and the doc's message — never a broken link.
func TestLinkRefuseEndpoint(t *testing.T) {
	c := rtCase{transport: "tcp", carrier: "mtcp", reverse: true} // the Iran edge of a reverse tunnel
	lc := listenerConfig(c, installFields(c), false)
	link, notes, missing, err := buildLink(lc, linkEnv{Unit: tUnit, CertNames: []string{tDomain}}, linkFields{})
	if err != nil || link != "" || strings.Join(missing, ",") != "endpoint" {
		t.Fatalf("link=%q missing=%v err=%v", link, missing, err)
	}
	const msg = "link: this config lacks endpoint (the server listens on all addresses and its public address was never stored); pass --endpoint <public-ip:port>"
	if !hasLine(notes, msg) {
		t.Fatalf("notes %v lack %q", notes, msg)
	}
	// through the command: exit 3, the message and the name on stderr, nothing on stdout
	cfg := writeCfg(t, lc)
	code, out, errOut := runLink(t, "", "-c", cfg, "--unit", tUnit)
	if code != 3 || out != "" || !strings.Contains(errOut, msg) || !strings.Contains(errOut, "link: missing: endpoint") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	// and with the flag it is the install link
	code, out, _ = runLink(t, "", "-c", cfg, "--unit", tUnit, "--endpoint", tIran+":2096", "--domain", tDomain)
	if code != 0 || out != mustEncode(t, installFields(c))+"\n" {
		t.Fatalf("with --endpoint: code=%d out=%q", code, out)
	}
}

// l3mtcp is tcp or tun: without link.transport the field is named, not guessed.
func TestLinkRefuseTransport(t *testing.T) {
	for _, tr := range []string{"tcp", "tun"} {
		c := rtCase{transport: tr, carrier: "l3mtcp"}
		if tr == "tun" {
			c.encap = "tcp"
		}
		d, _ := decodeLink(mustEncode(t, installFields(c)))
		dc := dialerConfig(d.withParseDefaults(), false) // the Iran edge, pre-R1
		link, notes, missing, err := buildLink(dc, linkEnv{Unit: tUnit}, linkFields{})
		if err != nil || link != "" || strings.Join(missing, ",") != "transport" {
			t.Fatalf("%s: link=%q missing=%v err=%v", tr, link, missing, err)
		}
		const msg = "link: transport is ambiguous for carrier l3mtcp (tcp or tun); pass --transport tcp|tun"
		if !hasLine(notes, msg) {
			t.Fatalf("%s: notes %v lack %q", tr, notes, msg)
		}
		got, _, _, err := buildLink(dc, linkEnv{Unit: tUnit}, linkFields{Transport: tr})
		if err != nil || got != mustEncode(t, installFields(c)) {
			t.Fatalf("%s with --transport: %q err=%v", tr, got, err)
		}
	}
	// a transport that cannot be this carrier's is an error, not a link
	d, _ := decodeLink(mustEncode(t, installFields(rtCase{transport: "tcp", carrier: "mtcp"})))
	if _, _, _, err := buildLink(dialerConfig(d.withParseDefaults(), false), linkEnv{Unit: tUnit}, linkFields{Transport: "tun"}); err == nil {
		t.Fatal("mtcp with transport tun accepted")
	}
}

// A field holding '|' (or a line break) cannot travel in a link: refused.
func TestLinkRejectPipe(t *testing.T) {
	base := installFields(rtCase{transport: "tcp", carrier: "mtcp"})
	for _, bad := range []string{"a|b", "a\nb", "a\rb"} {
		f := base
		f.Domain = bad
		if l, err := encodeLink(f); err == nil {
			t.Fatalf("domain %q encoded as %q", bad, l)
		}
		f = base
		f.Pin = "x|y"
		if _, err := encodeLink(f); err == nil {
			t.Fatal("pin with | encoded")
		}
	}
	// from a config: an sni with '|' is an invalid config (exit 2)
	d, _ := decodeLink(mustEncode(t, base))
	dc := dialerConfig(d.withParseDefaults(), true)
	dc["sni"] = "a|b.example"
	if _, _, _, err := buildLink(dc, linkEnv{Unit: tUnit}, linkFields{}); err == nil {
		t.Fatal("sni with | built a link")
	}
	code, out, _ := runLink(t, "", "-c", writeCfg(t, dc), "--unit", tUnit)
	if code != 2 || out != "" {
		t.Fatalf("code=%d out=%q", code, out)
	}
	// and a flag with '|'
	code, _, _ = runLink(t, "", "-c", writeCfg(t, dc), "--unit", tUnit, "--domain", "a|b")
	if code != 2 {
		t.Fatalf("--domain a|b: code=%d", code)
	}
}

// The decoder: both prefixes, a bare body (parse_link strips hs2:// itself),
// older links with fewer fields, and clear refusals.
func TestLinkDecode(t *testing.T) {
	f := installFields(rtCase{transport: "tcp", carrier: "mtcp"})
	l := mustEncode(t, f)
	for _, s := range []string{l, "  " + l + "\n", strings.TrimPrefix(l, "hs2://")} {
		d, err := decodeLink(s)
		if err != nil || d != f {
			t.Fatalf("decode %q: %+v %v", s, d, err)
		}
	}
	v := f
	v.Alt, v.Pin = tAlt, tPin
	lv := mustEncode(t, v)
	if !strings.HasPrefix(lv, "hs2v3://") {
		t.Fatalf("extended link %q", lv)
	}
	if d, err := decodeLink(lv); err != nil || d != v {
		t.Fatalf("v3 decode: %+v %v", d, err)
	}
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	old := "hs2://" + b64("192.0.2.1:2096|-|"+tShared+"|127.0.0.1:8443|dgtun|false|tun|direct|1280|udp|")
	if d, err := decodeLink(old); err != nil || d.Unit != "" || d.Encap != "udp" || d.Proto != "" {
		t.Fatalf("older 11-field link: %+v %v", d, err)
	}
	bad := map[string]string{
		"not base64":            "hs2://%%%",
		"no padding":            "hs2://" + strings.TrimRight(b64("a|b|c"), "="),
		"empty":                 "",
		"no endpoint":           "hs2://" + b64("|-|"+tShared),
		"no key":                "hs2://" + b64("192.0.2.1:1|-|"),
		"14 fields in hs2://":   "hs2://" + b64(strings.Repeat("x|", 13)+"x"),
		"line break":            "hs2://" + b64("192.0.2.1:1|-|k\n|x"),
		"16 fields in hs2v3://": "hs2v3://" + b64(strings.Repeat("x|", 15)+"x"),
		"13 fields in hs2v3://": "hs2v3://" + b64(strings.Repeat("x|", 12)+"x"),
		"bad pin":               "hs2v3://" + b64(strings.Repeat("x|", 14)+"short"),
	}
	for name, s := range bad {
		if d, err := decodeLink(s); err == nil {
			t.Errorf("%s: %q decoded to %+v", name, s, d)
		}
	}
}

// Hand-edited configs: the rebuilt link says what the tunnel runs with now,
// and a note says where it differs from the stored link block.
func TestLinkWorkingFieldWins(t *testing.T) {
	c := rtCase{transport: "tcp", carrier: "mtcp", udp: true}
	inst := installFields(c)
	d, _ := decodeLink(mustEncode(t, inst))
	dc := dialerConfig(d.withParseDefaults(), true)
	dc["addr"] = "192.0.2.99:2096"
	dc["udp"] = false
	got, notes, _, err := buildLink(dc, linkEnv{Unit: tUnit}, linkFields{})
	exp := inst
	exp.Endpoint, exp.UDP = "192.0.2.99:2096", "false"
	if err != nil || got != mustEncode(t, exp) || len(notes) != 2 {
		t.Fatalf("paster: %q notes=%v err=%v", got, notes, err)
	}
	// the maker: its listen port moved
	lc := listenerConfig(c, inst, true)
	lc["addr"] = "0.0.0.0:3000"
	got, notes, _, err = buildLink(lc, linkEnv{Unit: tUnit}, linkFields{})
	exp = inst
	exp.Endpoint = tKharej + ":3000"
	if err != nil || got != mustEncode(t, exp) || len(notes) != 1 || !strings.Contains(notes[0], "3000") {
		t.Fatalf("maker: %q notes=%v err=%v", got, notes, err)
	}
	// a flag beats everything
	got, _, _, _ = buildLink(lc, linkEnv{Unit: tUnit}, linkFields{Endpoint: "192.0.2.5:4000"})
	exp.Endpoint = "192.0.2.5:4000"
	if got != mustEncode(t, exp) {
		t.Fatalf("flag: %q", got)
	}
}

// The listener's domain comes from its certificate only when that names
// exactly one domain; otherwise it is asked for.
func TestLinkDomainFromCert(t *testing.T) {
	c := rtCase{transport: "tcp", carrier: "mtcp", reverse: true}
	lc := listenerConfig(c, installFields(c), false)
	ov := linkFields{Endpoint: tIran + ":2096"}
	for _, names := range [][]string{nil, {"a.example", "b.example"}, {"*.example.com"}} {
		_, notes, missing, err := buildLink(lc, linkEnv{Unit: tUnit, CertNames: names}, ov)
		if err != nil || strings.Join(missing, ",") != "domain" || !strings.Contains(strings.Join(notes, "\n"), "--domain") {
			t.Fatalf("names %v: missing=%v notes=%v err=%v", names, missing, notes, err)
		}
	}
	// through the command, with a real certificate file
	cert := writeSANCert(t, tDomain)
	lc["cert_file"] = cert
	code, out, errOut := runLink(t, "", "-c", writeCfg(t, lc), "--unit", tUnit, "--endpoint", tIran+":2096")
	if code != 0 || out != mustEncode(t, installFields(c))+"\n" {
		t.Fatalf("code=%d out=%q err=%q", code, out, errOut)
	}
}

// The unit name: from the systemd unit that runs this config (ExecStart -c),
// else from the installer's file naming (with a note), else asked for.
func TestLinkUnit(t *testing.T) {
	c := rtCase{transport: "udp", carrier: "udp"}
	inst := installFields(c)
	d, _ := decodeLink(mustEncode(t, inst))
	dc := dialerConfig(d.withParseDefaults(), true)

	units := t.TempDir()
	old := linkUnitDir
	linkUnitDir = units
	defer func() { linkUnitDir = old }()

	dir := t.TempDir()
	cfg := filepath.Join(dir, "tunnel-a.json")
	writeCfgAt(t, cfg, dc)
	os.WriteFile(filepath.Join(units, "hs2-de1.service"), []byte("[Service]\nExecStartPre=-/sbin/modprobe tun\nExecStart=/usr/local/bin/hs2 run -c "+cfg+"\n"), 0644)
	os.WriteFile(filepath.Join(units, "hs2-other.service"), []byte("[Service]\nExecStart=/usr/local/bin/hs2 run -c /nowhere.json\n"), 0644)
	code, out, _ := runLink(t, "", "-c", cfg)
	exp := inst
	exp.Unit = "hs2-de1"
	if code != 0 || out != mustEncode(t, exp)+"\n" {
		t.Fatalf("from the unit file: code=%d out=%q", code, out)
	}
	// the installer's naming: config.json is hs2, hs2-<name>.json is hs2-<name>
	for file, unit := range map[string]string{"config.json": "hs2", "hs2-de2.json": "hs2-de2"} {
		p := filepath.Join(dir, file)
		writeCfgAt(t, p, dc)
		code, out, errOut := runLink(t, "", "-c", p)
		exp.Unit = unit
		if code != 0 || out != mustEncode(t, exp)+"\n" || !strings.Contains(errOut, "file name") {
			t.Fatalf("%s: code=%d out=%q err=%q", file, code, out, errOut)
		}
	}
	// anything else: named, and --unit gives it
	p := filepath.Join(dir, "x.json")
	writeCfgAt(t, p, dc)
	code, _, errOut := runLink(t, "", "-c", p)
	if code != 3 || !strings.Contains(errOut, "link: missing: unit") || !strings.Contains(errOut, "--unit") {
		t.Fatalf("unknown unit: code=%d err=%q", code, errOut)
	}
	if code, _, _ := runLink(t, "", "-c", p, "--unit", "Bad Name"); code != 2 {
		t.Fatalf("bad --unit accepted: %d", code)
	}
}

// Exit codes: 0 link, 3 a field missing, 2 an invalid config or flag, 4 no
// read access (the config is root's, 0600) — with a clear message, never a raw error.
func TestLinkExitCodes(t *testing.T) {
	c := rtCase{transport: "tcp", carrier: "mtcp"}
	d, _ := decodeLink(mustEncode(t, installFields(c)))
	good := writeCfg(t, dialerConfig(d.withParseDefaults(), true))

	code, out, errOut := runLink(t, "", "-c", good, "--unit", tUnit)
	if code != 0 || out != mustEncode(t, installFields(c))+"\n" || !strings.Contains(errOut, tKeyNote) {
		t.Fatalf("ok: code=%d out=%q err=%q", code, out, errOut)
	}
	if strings.Contains(errOut, strings.TrimSpace(out)) || strings.Contains(errOut, tShared) {
		t.Fatal("the link or the key reached stderr")
	}

	bad := filepath.Join(t.TempDir(), "bad.json")
	os.WriteFile(bad, []byte(`{"mode": "dial",`), 0600)
	for _, args := range [][]string{
		{"-c", bad},
		{"-c", filepath.Join(t.TempDir(), "none.json")},
		{"-c", writeCfg(t, map[string]any{"mode": "dial", "carrier": "noise", "addr": "192.0.2.1:1", "shared_key": tShared})},
		{"-c", writeCfg(t, map[string]any{"mode": "dial", "carrier": "mtcp", "addr": "192.0.2.1:1"})}, // no key
		{"-c", good, "--udp", "yes"},
		{"-c", good, "--transport", "quic"},
		{"-c", good, "--endpoint", "no-port"},
		{},
	} {
		if code, out, _ := runLink(t, "", args...); code != 2 || out != "" {
			t.Errorf("%v: code=%d out=%q, want 2 and nothing on stdout", args, code, out)
		}
	}

	old := linkReadFile
	linkReadFile = func(string) ([]byte, error) { return nil, &fs.PathError{Op: "open", Path: good, Err: fs.ErrPermission} }
	defer func() { linkReadFile = old }()
	code, out, errOut = runLink(t, "", "-c", good)
	if code != 4 || out != "" || !strings.Contains(errOut, "root") {
		t.Fatalf("no access: code=%d out=%q err=%q", code, out, errOut)
	}
}

// hs2 link --decode reads a link from its argument or stdin (so the installer
// never puts the key on a command line) and prints name=value lines.
func TestLinkDecodeCommand(t *testing.T) {
	f := installFields(rtCase{transport: "tun", carrier: "dgtun", encap: "ipx", proto: "200"})
	l := mustEncode(t, f)
	want := "format=hs2\nendpoint=192.0.2.10\ndomain=-\nshared=" + tShared + "\npanel=127.0.0.1:8443\ncarrier=dgtun\n" +
		"udp=false\ntransport=tun\ndirection=direct\nmtu=1280\nencap=ipx\nproto=200\nunit=hs2-rt\ntun_base=10.77.4.0\n"
	for _, in := range []struct {
		stdin string
		args  []string
	}{{"", []string{"--decode", l}}, {l + "\n", []string{"--decode"}}, {l, []string{"--decode", "-"}}} {
		code, out, _ := runLink(t, in.stdin, in.args...)
		if code != 0 || out != want {
			t.Fatalf("%v: code=%d out=%q", in.args, code, out)
		}
	}
	v := f
	v.Alt, v.Pin = tAlt, tPin
	code, out, _ := runLink(t, "", "--decode", mustEncode(t, v))
	if code != 0 || !strings.HasPrefix(out, "format=hs2v3\n") || !strings.HasSuffix(out, "alt="+tAlt+"\npin="+tPin+"\n") {
		t.Fatalf("v3: code=%d out=%q", code, out)
	}
	code, out, errOut := runLink(t, "", "--decode", "hs2://%%%")
	if code != 2 || out != "" || !strings.Contains(errOut, "invalid link") {
		t.Fatalf("bad link: code=%d out=%q err=%q", code, out, errOut)
	}
}

// hs2 link --list: every hs2 unit with its config, side, carrier, direction,
// whether it makes or pastes the link, and rebuildable / what it needs.
func TestLinkList(t *testing.T) {
	units := t.TempDir()
	old := linkUnitDir
	linkUnitDir = units
	defer func() { linkUnitDir = old }()
	dir := t.TempDir()

	maker := rtCase{transport: "tun", carrier: "dgtun", encap: "udp"}
	mc := filepath.Join(dir, "config.json")
	writeCfgAt(t, mc, listenerConfig(maker, installFields(maker), false)) // pre-R1: no endpoint
	paster := rtCase{transport: "tcp", carrier: "mtcp", reverse: true}
	d, _ := decodeLink(mustEncode(t, installFields(paster)))
	pc := filepath.Join(dir, "hs2-de1.json")
	writeCfgAt(t, pc, dialerConfig(d.withParseDefaults(), true))
	unit := func(name, cfg string) {
		os.WriteFile(filepath.Join(units, name+".service"), []byte("[Service]\nExecStart=/usr/local/bin/hs2 run -c "+cfg+"\n"), 0644)
	}
	unit("hs2", mc)
	unit("hs2-de1", pc)
	os.WriteFile(filepath.Join(units, "ssh.service"), []byte("[Service]\nExecStart=/usr/sbin/sshd -D\n"), 0644)
	// hs2-* units that are not tunnels: a monitor, another program given -c,
	// a binary only named like hs2, and hs2 run on a file that is no tunnel config
	os.WriteFile(filepath.Join(units, "hs2-mon.service"), []byte("[Service]\nExecStart=/usr/local/sbin/hs2-mon\n"), 0644)
	os.WriteFile(filepath.Join(units, "hs2-py.service"), []byte("[Service]\nExecStart=/usr/bin/python3 /opt/x.py -c "+pc+"\n"), 0644)
	os.WriteFile(filepath.Join(units, "hs2-my.service"), []byte("[Service]\nExecStart=/opt/myhs2 run -c "+pc+"\n"), 0644)
	junk := filepath.Join(dir, "junk.json")
	os.WriteFile(junk, []byte(`{"foo": 1}`), 0600)
	unit("hs2-junk", junk)
	broken := filepath.Join(dir, "broken.json")
	os.WriteFile(broken, []byte(`not json`), 0600)
	unit("hs2-broken", broken)

	code, out, errOut := runLink(t, "", "--list")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if code != 0 || len(lines) != 3 || errOut != "" {
		t.Fatalf("code=%d out=%q err=%q", code, out, errOut)
	}
	for _, u := range []string{"hs2-mon", "hs2-py", "hs2-my", "hs2-junk", "hs2-broken"} {
		if strings.Contains(out, u) {
			t.Errorf("%s is not a tunnel, but listed", u)
		}
	}
	for _, w := range []string{"hs2 ", mc, "exit", "dgtun", "direct", "makes", "needs: endpoint"} {
		if !strings.Contains(lines[1], w) {
			t.Errorf("line %q lacks %q", lines[1], w)
		}
	}
	for _, w := range []string{"hs2-de1", pc, "exit", "mtcp", "reverse", "pastes", "rebuildable"} {
		if !strings.Contains(lines[2], w) {
			t.Errorf("line %q lacks %q", lines[2], w)
		}
	}
	if strings.Contains(out, "hs2://") || strings.Contains(out, tShared) {
		t.Fatal("--list printed a link or a key")
	}

	// a tunnel whose config this user cannot read: not shown, said so, exit 4
	old2 := linkReadFile
	linkReadFile = func(p string) ([]byte, error) {
		if p == pc {
			return nil, &fs.PathError{Op: "open", Path: p, Err: fs.ErrPermission}
		}
		return old2(p)
	}
	defer func() { linkReadFile = old2 }()
	code, out, errOut = runLink(t, "", "--list")
	if code != 4 || strings.Contains(out, "hs2-de1") || !strings.Contains(out, mc) || !strings.Contains(errOut, "root") {
		t.Fatalf("unreadable: code=%d out=%q err=%q", code, out, errOut)
	}
}

// The status line of §3.16, without the link.
func TestLinkStatusLine(t *testing.T) {
	c := rtCase{transport: "tcp", carrier: "mtcp"}
	inst := installFields(c)
	lc := listenerConfig(c, inst, true)
	lc["cert_file"] = writeSANCert(t, tDomain)
	p := writeCfg(t, lc)
	old := linkUnitDir
	linkUnitDir = t.TempDir()
	defer func() { linkUnitDir = old }()
	os.WriteFile(filepath.Join(linkUnitDir, "hs2-rt.service"), []byte("ExecStart=/usr/local/bin/hs2 run -c "+p+"\n"), 0644)
	if got := linkStatusLine(p); got != "link: rebuildable from this config" {
		t.Fatalf("got %q", got)
	}
	delete(lc, "link")
	p2 := writeCfg(t, lc)
	os.WriteFile(filepath.Join(linkUnitDir, "hs2-rt2.service"), []byte("ExecStart=/usr/local/bin/hs2 run -c "+p2+"\n"), 0644)
	if got, want := linkStatusLine(p2), "link: needs endpoint, udp (run: hs2 link -c "+p2+")"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

// hs2 check knows the link block (no "unknown key" warning) and warns about a
// malformed one; the daemon never reads it.
func TestCheckLinkBlock(t *testing.T) {
	c := rtCase{transport: "udp", carrier: "udp"}
	d, _ := decodeLink(mustEncode(t, installFields(c)))
	m := dialerConfig(d.withParseDefaults(), true)
	raw, _ := json.Marshal(m)
	errs, warns := checkConfig(raw, localIs(), time.Now())
	if len(errs) > 0 || len(warns) > 0 {
		t.Fatalf("errs=%v warns=%v", errs, warns)
	}
	m["link"] = map[string]any{"endpoint": 5, "udp": "yes", "transport": "quic", "panle": "x"}
	raw, _ = json.Marshal(m)
	errs, warns = checkConfig(raw, localIs(), time.Now())
	w := strings.Join(warns, "|")
	if len(errs) > 0 || !strings.Contains(w, "link.endpoint") || !strings.Contains(w, "link.udp") ||
		!strings.Contains(w, "link.transport") || !strings.Contains(w, "link.panle") {
		t.Fatalf("errs=%v warns=%v", errs, warns)
	}
	var fc fileConfig
	if err := json.Unmarshal(raw, &fc); err != nil {
		t.Fatalf("the daemon's config read fails on a link block: %v", err)
	}
}

// hs2 config can set the link block's fields (the installer's menu saves the
// asked fields this way), type-checked.
func TestConfigSetLinkKeys(t *testing.T) {
	m := map[string]any{}
	for k, v := range map[string]string{"link.endpoint": "192.0.2.1:2096", "link.domain": tDomain, "link.transport": "tun", "link.udp": "true", "link.panel": "-"} {
		ck, ok := configKeys[k]
		if !ok {
			t.Fatalf("%s is not editable", k)
		}
		if err := setKey(m, ck, v); err != nil {
			t.Fatalf("%s: %v", k, err)
		}
	}
	lb, _ := m["link"].(map[string]any)
	if lb["endpoint"] != "192.0.2.1:2096" || lb["udp"] != true || lb["transport"] != "tun" || lb["panel"] != "-" {
		t.Fatalf("%v", lb)
	}
	for k, v := range map[string]string{"link.udp": "maybe", "link.transport": "quic", "link.endpoint": "a|b"} {
		if err := setKey(m, configKeys[k], v); err == nil {
			t.Errorf("%s=%q accepted", k, v)
		}
	}
}

// §3.16 test (6): no network (it works in an empty network namespace), no
// file and no log line holds the link, file modes unchanged.
func TestLinkNoNetworkNoLog(t *testing.T) {
	if os.Getenv("HS2_LINK_HELPER") == "1" {
		os.Exit(linkCmd(strings.Split(os.Getenv("HS2_LINK_ARGS"), "\n"), os.Stdin, os.Stdout, os.Stderr))
	}
	if os.Geteuid() != 0 {
		t.Skip("needs root for an empty network namespace (unshare -n)")
	}
	unshare, err := exec.LookPath("unshare")
	if err != nil {
		t.Skip("no unshare")
	}
	c := rtCase{transport: "tcp", carrier: "mtcp", reverse: true}
	inst := installFields(c)
	dir := t.TempDir()
	cert := writeSANCert(t, tDomain)
	lc := listenerConfig(c, inst, true)
	lc["cert_file"] = cert
	cfg := filepath.Join(dir, "hs2-rt.json")
	writeCfgAt(t, cfg, lc)
	// The command's own temp dir, home and working dir are fresh, so anything it
	// wrote would show up there; /run/hs2 (live status files of real tunnels,
	// which change on their own) is only searched for the link.
	tmp, home, wd := t.TempDir(), t.TempDir(), t.TempDir()
	watch := []string{dir, filepath.Dir(cert), tmp, home, wd}
	before := snapshot(watch)

	cmd := exec.Command(unshare, "-n", "--", os.Args[0], "-test.run=^TestLinkNoNetworkNoLog$")
	cmd.Env = append(os.Environ(), "HS2_LINK_HELPER=1", "HS2_LINK_ARGS=-c\n"+cfg+"\n--unit\n"+tUnit,
		"TMPDIR="+tmp, "HOME="+home)
	cmd.Dir = wd
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	if err := cmd.Run(); err != nil {
		t.Fatalf("in an empty network namespace: %v, stderr=%q", err, se.String())
	}
	link := mustEncode(t, inst)
	if so.String() != link+"\n" {
		t.Fatalf("stdout %q, want the link", so.String())
	}
	if strings.Contains(se.String(), link) || strings.Contains(se.String(), tShared) || !strings.Contains(se.String(), tKeyNote) {
		t.Fatalf("stderr %q", se.String())
	}
	filepath.WalkDir("/run/hs2", func(p string, e fs.DirEntry, err error) error {
		if err == nil && e.Type().IsRegular() {
			if b, err := os.ReadFile(p); err == nil && bytes.Contains(b, []byte(link)) {
				t.Errorf("%s holds the link", p)
			}
		}
		return nil
	})
	after := snapshot(watch)
	for p, st := range after {
		b, ok := before[p]
		if !ok {
			t.Errorf("new file %s", p)
			continue
		}
		if b != st {
			t.Errorf("%s changed: %s -> %s", p, b, st)
		}
	}
	for p := range before {
		if _, ok := after[p]; !ok {
			t.Errorf("file %s disappeared", p)
		}
	}
	if fi, _ := os.Stat(cfg); fi.Mode().Perm() != 0o600 {
		t.Fatalf("config mode %v", fi.Mode().Perm())
	}
}

// snapshot: path -> mode/size/mtime, and whether the file holds the key.
func snapshot(dirs []string) map[string]string {
	out := map[string]string{}
	for _, d := range dirs {
		filepath.WalkDir(d, func(p string, e fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if e.IsDir() && p != d && strings.Count(strings.TrimPrefix(p, d), "/") > 3 {
				return filepath.SkipDir
			}
			fi, err := e.Info()
			if err != nil {
				return nil
			}
			key := ""
			if fi.Mode().IsRegular() && fi.Size() < 1<<20 {
				if b, err := os.ReadFile(p); err == nil && bytes.Contains(b, []byte("hs2://")) {
					key = " holds-a-link"
				}
			}
			out[p] = fmt.Sprintf("%v %d %d%s", fi.Mode(), fi.Size(), fi.ModTime().UnixNano(), key)
			return nil
		})
	}
	return out
}

// §3.16 test (5): the decoder never panics, and whatever it accepts encodes
// again and decodes to the same fields.
func FuzzDecodeLink(f *testing.F) {
	for _, c := range rtCases() {
		l, _ := encodeLink(installFields(c))
		f.Add(l)
	}
	f.Add("")
	f.Add("hs2://")
	f.Add("hs2v3://")
	f.Add("hs2://" + base64.StdEncoding.EncodeToString([]byte("a|b|c|||||||||||||")))
	f.Fuzz(func(t *testing.T, s string) {
		d, err := decodeLink(s)
		if err != nil {
			return
		}
		l, err := encodeLink(d)
		if err != nil {
			t.Fatalf("decoded %q to %+v, which does not encode: %v", s, d, err)
		}
		d2, err := decodeLink(l)
		if err != nil || d2 != d {
			t.Fatalf("%q: %+v -> %q -> %+v %v", s, d, l, d2, err)
		}
	})
}

// ---- helpers ----------------------------------------------------------------

func hasLine(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func runLink(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var o, e bytes.Buffer
	code := linkCmd(args, strings.NewReader(stdin), &o, &e)
	return code, o.String(), e.String()
}

func writeCfg(t *testing.T, m map[string]any) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "cfg.json")
	writeCfgAt(t, p, m)
	return p
}

func writeCfgAt(t *testing.T, p string, m map[string]any) {
	t.Helper()
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeSANCert writes a self-signed certificate for exactly these names.
func writeSANCert(t *testing.T, names ...string) string {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: names[0]}, DNSNames: names,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "c.pem")
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}
