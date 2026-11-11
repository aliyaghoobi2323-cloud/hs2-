package main

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

// The doctor's side check never goes to the network in tests.
func init() {
	frontTargetGeo = func(string) []ipGeo { return nil }
	targetRTT = func(string) time.Duration { return 0 }
}

// Which server listens: the carrier does not matter, the mode does.
func TestSideOfConfig(t *testing.T) {
	for _, c := range []struct {
		mode    string
		reverse bool
		carrier string
		listens bool
		iran    bool
	}{
		{"dial", false, "mtcp", false, false}, // direct, Iran edge dials kharej
		{"listen", false, "mtcp", true, false},
		{"dial", true, "mtcp", true, true}, // reverse, the Iran edge listens
		{"listen", true, "mtcp", false, true},
		{"dial", false, "l3mtcp", false, false}, {"listen", false, "l3mtcp", true, false},
		{"dial", true, "l3mtcp", true, true}, {"listen", true, "l3mtcp", false, true},
		{"dial", false, "tls", false, false}, {"listen", false, "tls", true, false},
		{"dial", true, "tls", true, true}, {"listen", true, "tls", false, true},
	} {
		fc := fileConfig{Mode: c.mode, Reverse: c.reverse, Carrier: c.carrier, Addr: "10.0.0.9:443"}
		s := sideOfConfig(fc)
		if !s.Known || s.ThisListens != c.listens || s.ListenerIran != c.iran {
			t.Errorf("%s reverse=%v %s: listens=%v iran=%v, want %v %v", c.mode, c.reverse, c.carrier, s.ThisListens, s.ListenerIran, c.listens, c.iran)
		}
		if c.listens && (len(s.ListenerIPs) != 1 || s.ListenerIPs[0] != "10.0.0.9") {
			t.Errorf("listener IPs of a listener: %v", s.ListenerIPs)
		}
		if !c.listens && (len(s.ListenerIPs) == 0 || s.ListenerIPs[0] != "10.0.0.9") {
			t.Errorf("listener IPs seen from the dialer: %v", s.ListenerIPs)
		}
	}
	if s, ok := sideFromFlag("iran"); !ok || !s.ListenerIran {
		t.Error("--side iran")
	}
	if s, ok := sideFromFlag("kharej"); !ok || s.ListenerIran || !s.Known {
		t.Error("--side kharej")
	}
	if _, ok := sideFromFlag("x"); ok {
		t.Error("--side x accepted")
	}
	if l := candidatesFor(realitySide{Known: true, ListenerIran: true}); l[0] != realityCandidatesIran[0] || len(l) != len(realityCandidatesIran) {
		t.Errorf("iran candidates: %v", l)
	}
	if l := candidatesFor(realitySide{Known: true}); l[0] != realityCandidatesForeign[0] {
		t.Errorf("foreign candidates: %v", l)
	}
	if l := candidatesFor(realitySide{}); len(l) != len(realityCandidatesIran)+len(realityCandidatesForeign) {
		t.Errorf("unknown side: both lists, got %d", len(l))
	}
}

// The offline table: Iranian CDNs, Iranian ASNs registered elsewhere,
// Hetzner / OVH, Cloudflare; unknown stays unknown.
func TestGeoOffline(t *testing.T) {
	for _, c := range []struct {
		ip, cc string
		asn    int
		cdn    string
	}{
		{"185.143.234.122", "IR", 205585, "ArvanCloud"},
		{"185.166.104.7", "IR", 202319, "Sotoon CDN"},
		{"5.145.118.1", "IR", 60423, "Derak"},
		{"5.160.218.165", "IR", 42337, ""}, // Respina: registry says AE
		{"185.188.106.12", "IR", 43211, ""},
		{"2.144.1.1", "IR", 44244, ""},
		{"91.107.130.1", "DE", 24940, ""},
		{"213.133.116.44", "DE", 24940, ""},
		{"51.89.1.1", "FR", 16276, ""},
		{"104.16.1.1", "", 13335, "Cloudflare"},
	} {
		g, ok := geoOffline(net.ParseIP(c.ip))
		if !ok || g.Country != c.cc || g.ASN != c.asn || g.CDN != c.cdn {
			t.Errorf("%s: %+v, want %s AS%d cdn %q", c.ip, g, c.cc, c.asn, c.cdn)
		}
	}
	if g, ok := geoOffline(net.ParseIP("8.8.8.8")); ok || g.ASN != 0 {
		t.Errorf("8.8.8.8 placed: %+v", g)
	}
	g, ok := parseCymruOrigin("42337 | 5.160.0.0/17 | AE | ripencc | 2012-07-25")
	if !ok || g.ASN != 42337 || g.Country != "AE" {
		t.Errorf("cymru parse: %+v", g)
	}
	if _, ok := parseCymruOrigin("garbage"); ok {
		t.Error("cymru garbage parsed")
	}
	// online: a known ASN overrides the registry country and sets the CDN
	old := geoOnline
	t.Cleanup(func() { geoOnline = old })
	geoOnline = func(ip net.IP, _ time.Duration) (ipGeo, bool) {
		return ipGeo{IP: ip.String(), ASN: 42337, Country: "AE", Src: "cymru"}, true
	}
	if g := geoOf(net.ParseIP("203.0.113.77"), true); g.Country != "IR" || g.Src != "cymru" {
		t.Errorf("online geo: %+v", g)
	}
	if g := geoOf(net.ParseIP("203.0.113.78"), false); g.ASN != 0 {
		t.Errorf("offline must not ask online: %+v", g)
	}
}

func TestApplySide(t *testing.T) {
	iran := realitySide{Known: true, ListenerIran: true, Reverse: true, ThisListens: true}
	kharej := realitySide{Known: true, ThisListens: true}
	ir := []ipGeo{{IP: "185.188.106.12", Country: "IR", ASN: 43211}}
	de := []ipGeo{{IP: "213.133.116.44", Country: "DE", ASN: 24940, Org: "Hetzner"}}
	hz := []ipGeo{{IP: "91.107.130.1", Country: "DE", ASN: 24940}}
	ov := []ipGeo{{IP: "51.89.1.1", Country: "FR", ASN: 16276}}

	r := targetCheck{}
	applySide(&r, iran, ir, nil)
	if r.SideMatch != sideMatch || len(r.Bad) != 0 {
		t.Errorf("iran listener + iranian site: %+v", r)
	}
	r = targetCheck{}
	applySide(&r, iran, de, nil)
	if r.SideMatch != sideMismatch || len(r.Bad) == 0 || !strings.Contains(r.Bad[0], "MISMATCH") {
		t.Errorf("iran listener + foreign site: %+v", r)
	}
	r = targetCheck{}
	applySide(&r, kharej, ir, hz)
	if r.SideMatch != sideMismatch || len(r.Bad) == 0 {
		t.Errorf("kharej listener + iranian site: %+v", r)
	}
	r = targetCheck{}
	applySide(&r, kharej, de, hz)
	if r.SideMatch != sideMatch || len(r.Bad) != 0 || !strings.Contains(strings.Join(r.Info, ";"), "same provider") {
		t.Errorf("kharej on Hetzner + Hetzner site: %+v", r)
	}
	r = targetCheck{}
	applySide(&r, kharej, de, ov)
	if r.SideMatch != sideMatch || !strings.Contains(strings.Join(r.Info, ";"), "other provider") {
		t.Errorf("kharej on OVH + Hetzner site: %+v", r)
	}
	// a CDN found by IP rejects through finishCheck
	r = targetCheck{}
	applySide(&r, iran, []ipGeo{{IP: "185.143.234.122", Country: "IR", ASN: 205585, CDN: "ArvanCloud"}}, nil)
	if r = finishCheck(r); r.OK || r.CDN != "ArvanCloud" {
		t.Errorf("Arvan CDN by IP: %+v", r)
	}
	if r2 := finishCheck(r); len(r2.Bad) != len(r.Bad) {
		t.Errorf("finishCheck twice duplicated the CDN reason: %v", r2.Bad)
	}
	// unknown geo: latency decides (listener only), a guess only warns
	r = targetCheck{ConnectMs: 0.4}
	applySide(&r, iran, []ipGeo{{IP: "198.51.100.1"}}, nil)
	if r.SideMatch != sideMatch || len(r.Bad) != 0 {
		t.Errorf("near by latency: %+v", r)
	}
	r = targetCheck{ConnectMs: 80}
	applySide(&r, iran, []ipGeo{{IP: "198.51.100.1"}}, nil)
	if r.SideMatch != sideMismatch || len(r.Bad) != 0 || len(r.Warn) == 0 {
		t.Errorf("far by latency must warn only: %+v", r)
	}
	r = targetCheck{ConnectMs: 80}
	applySide(&r, realitySide{Known: true, ListenerIran: true}, []ipGeo{{IP: "198.51.100.1"}}, nil)
	if r.SideMatch != sideUnknown {
		t.Errorf("latency from the dialer must not decide: %+v", r)
	}
	r = targetCheck{}
	applySide(&r, realitySide{}, ir, nil)
	if r.SideMatch != sideUnknown || len(r.Bad) != 0 {
		t.Errorf("unknown side: %+v", r)
	}
}

// Iranian multi-tenant CDNs: ArvanCloud, Derak, Sotoon (headers, names).
func TestIranianCDNDetection(t *testing.T) {
	for _, c := range []struct{ k, v, want string }{
		{"server", "ArvanCloud", "ArvanCloud"}, {"ar-request-id", "abc", "ArvanCloud"}, {"ar-poweredby", "Arvan Cloud (arvancloud.ir)", "ArvanCloud"},
		{"server", "DERAK.CLOUD", "Derak"}, {"derak-cache", "EXPIRED", "Derak"}, {"derak-umbrage", "x", "Derak"},
		{"server", "Sotoon CDN", "Sotoon CDN"}, {"x-zrk-cs", "HIT", "Sotoon CDN"},
		{"server", "DigiCDN Edge", ""}, {"server", "roadrunner", ""}, {"x-cache", "HIT", ""},
	} {
		if g := cdnHeader(c.k, c.v); g != c.want {
			t.Errorf("cdnHeader(%s: %s) = %q, want %q", c.k, c.v, g, c.want)
		}
	}
	for _, h := range []string{"x.arvancdn.ir", "www.arvancloud.ir", "derak.cloud", "cdn.sotoon.ir"} {
		if multiTenantCDN(h) == "" {
			t.Errorf("multiTenantCDN(%s) empty", h)
		}
	}
	// a full probe of a target behind ArvanCloud headers
	arv := tlsTarget(t, map[string]string{"Server": "ArvanCloud", "AR-Request-ID": "x1"})
	der := tlsTarget(t, map[string]string{"Server": "DERAK.CLOUD", "Derak-Cache": "HIT"})
	old, oldRes := targetDialer, targetResolve
	t.Cleanup(func() { targetDialer, targetResolve = old, oldRes })
	targetResolve = func(string) []net.IP { return nil }
	route := map[string]string{"arvan.example.com:443": arv, "derak.example.com:443": der}
	targetDialer = func(ctx context.Context, a string) (net.Conn, error) {
		if r, ok := route[a]; ok {
			return (&net.Dialer{}).DialContext(ctx, "tcp", r)
		}
		return nil, errors.New("connection refused")
	}
	if r := probeTarget("arvan.example.com", "example.com", nil); r.OK || r.CDN != "ArvanCloud" {
		t.Errorf("Arvan target: %+v", r)
	}
	if r := probeTarget("derak.example.com", "example.com", nil); r.OK || r.CDN != "Derak" {
		t.Errorf("Derak target: %+v", r)
	}
}

// The picker: side verdicts, matches first, a CDN IP rejected.
func TestCheckTargetsSideAware(t *testing.T) {
	good := tlsTarget(t, nil)
	old, oldRes, oldOn := targetDialer, targetResolve, geoOnline
	t.Cleanup(func() { targetDialer, targetResolve, geoOnline = old, oldRes, oldOn })
	geoOnline = func(net.IP, time.Duration) (ipGeo, bool) { return ipGeo{}, false }
	ips := map[string]string{"ir.example.com": "185.188.106.12", "de.example.com": "213.133.116.44", "arv.example.com": "185.143.234.122"}
	targetResolve = func(h string) []net.IP { return []net.IP{net.ParseIP(ips[h])} }
	targetDialer = func(ctx context.Context, a string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", good)
	}
	// names are checked with sni example.com (httptest's cert)
	s := realitySide{Known: true, ListenerIran: true, Reverse: true, ThisListens: true}
	var rs []targetCheck
	for _, h := range []string{"de.example.com", "arv.example.com", "ir.example.com"} {
		rs = append(rs, checkSide(h, "example.com", s, nil, false))
	}
	if !rs[2].OK || rs[2].SideMatch != sideMatch {
		t.Errorf("iranian target on an Iran listener: %+v", rs[2])
	}
	if rs[0].OK || rs[0].SideMatch != sideMismatch {
		t.Errorf("foreign target on an Iran listener accepted: %+v", rs[0])
	}
	if rs[1].OK || rs[1].CDN != "ArvanCloud" {
		t.Errorf("Arvan CDN IP accepted: %+v", rs[1])
	}
	var b strings.Builder
	renderTargets(&b, rs)
	if !strings.Contains(b.String(), "mismatch DE AS24940") || !strings.Contains(b.String(), "match IR AS43211") {
		t.Errorf("render:\n%s", b.String())
	}
	if !strings.Contains(describeSide(s, nil), "IRAN server") || !strings.Contains(describeSide(realitySide{Known: true}, nil), "KHAREJ server") {
		t.Error("describeSide")
	}
	if !strings.Contains(noneAcceptedAdvice(s), "of your own") {
		t.Error("advice")
	}
}

// Doctor: a configured target off the listener's side warns.
func TestDoctorFrontSide(t *testing.T) {
	old := frontTargetGeo
	t.Cleanup(func() { frontTargetGeo = old })
	geo := map[string][]ipGeo{
		"www.digikala.com": {{IP: "185.188.106.12", Country: "IR", ASN: 43211}},
		"www.hetzner.com":  {{IP: "213.133.116.44", Country: "DE", ASN: 24940}},
	}
	frontTargetGeo = func(h string) []ipGeo { return geo[h] }
	rev := fileConfig{Mode: "dial", Reverse: true, Carrier: "mtcp", Addr: "0.0.0.0:443"}
	dir := fileConfig{Mode: "listen", Reverse: false, Carrier: "mtcp", Addr: "0.0.0.0:443"}
	if f := frontSideFinding(rev, "www.hetzner.com:443", "front x"); len(f) != 1 || f[0].Sev != "WARN" {
		t.Errorf("reverse + foreign target: %+v", f)
	}
	if f := frontSideFinding(rev, "www.digikala.com:443", "front x"); len(f) != 1 || f[0].Sev != "OK" {
		t.Errorf("reverse + iranian target: %+v", f)
	}
	if f := frontSideFinding(dir, "www.digikala.com:443", "front x"); len(f) != 1 || f[0].Sev != "WARN" {
		t.Errorf("direct + iranian target: %+v", f)
	}
	if f := frontSideFinding(dir, "unknown.example:443", "front x"); len(f) != 0 {
		t.Errorf("unplaced target: %+v", f)
	}
}
