package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// procLine renders one /proc/net/if_inet6 line.
func procLine(ip, iface string, flags uint32) string {
	b := net.ParseIP(ip).To16()
	return fmt.Sprintf("%x 02 40 00 %02x %s", []byte(b), flags, iface)
}

func TestLBAutoSpec(t *testing.T) {
	for in, want := range map[string]string{"auto6": "", "AUTO6": "", " auto6 ": "", "auto6:eth0": "eth0", "auto6:ens3": "ens3"} {
		if ifc, ok := lbAutoSpec(in); !ok || ifc != want {
			t.Errorf("%q -> %q %v", in, ifc, ok)
		}
	}
	for _, in := range []string{"auto", "auto6:", "auto66", "auto4", "2001:db8::1", "auto6:a b", ""} {
		if _, ok := lbAutoSpec(in); ok {
			t.Errorf("%q accepted", in)
		}
	}
}

func TestParseIfInet6AndDefault(t *testing.T) {
	text := strings.Join([]string{
		procLine("fe80::1", "eth0", 0x80),
		procLine("2001:db8:a:c010:3724::1", "eth0", 0x80),
		procLine("::1", "lo", 0x80),
		"garbage line",
	}, "\n")
	a := parseIfInet6(text)
	if len(a) != 3 || a[1].IP.String() != "2001:db8:a:c010:3724::1" || a[1].Iface != "eth0" || a[1].Flags != 0x80 || a[1].Plen != 64 {
		t.Fatalf("%+v", a)
	}
	z := strings.Repeat("0", 32)
	route := strings.Join([]string{
		z + " 00 " + z + " 00 " + z + " ffffffff 00000001 00000000 00200200       lo", // unreachable
		z + " 00 " + z + " 00 fe800000000000000000000000000001 00000400 00000001 00000000 00000003     eth1",
		z + " 00 " + z + " 00 fe800000000000000000000000000001 00000400 00000001 00000000 00000003",
		z + " 00 " + z + " 00 fe800000000000000000000000000001 00000100 00000001 00000000 00000003     eth0",
		"20010db8000000000000000000000000 40 " + z + " 00 " + z + " 00000100 00000001 00000000 00000001     eth2",
	}, "\n")
	if d := parseDefault6Iface(route); d != "eth0" {
		t.Fatalf("default iface %q", d)
	}
	if d := parseDefault6Iface(""); d != "" {
		t.Fatalf("no routes: %q", d)
	}
}

func a6(ip, iface string, flags uint32) if6Addr {
	return if6Addr{Iface: iface, IP: net.ParseIP(ip), Flags: flags}
}

func TestPickAuto6(t *testing.T) {
	ll := a6("fe80::1", "eth0", 0)
	ula := a6("fd00::5", "eth0", 0)
	dep := a6("2001:db8:a::d", "eth0", ifaFDeprecated)
	tmp := a6("2001:db8:a::7", "eth0", ifaFTemporary)
	tent := a6("2001:db8:a::9", "eth0", ifaFTentative)
	v4m := if6Addr{Iface: "eth0", IP: net.ParseIP("10.0.0.1")}
	g1 := a6("2001:db8:a:c010:3724::1", "eth0", 0x80)
	g2 := a6("2001:db8:a:c010:2bf0::1", "eth0", 0x80)
	g3 := a6("2001:db8:5::1", "eth1", 0)
	lo := a6("::1", "lo", 0)
	cases := []struct {
		name           string
		addrs          []if6Addr
		def, ifc, prev string
		want           string
	}{
		{"none", nil, "eth0", "", "", ""},
		{"only link-local, ULA, loopback", []if6Addr{ll, ula, lo}, "eth0", "", "", ""},
		{"deprecated, temporary, tentative skipped", []if6Addr{dep, tmp, tent, v4m}, "eth0", "", "", ""},
		{"one global among junk", []if6Addr{ll, ula, dep, tmp, g1}, "eth0", "", "", "2001:db8:a:c010:3724::1"},
		{"two globals: lowest first", []if6Addr{g1, g2}, "eth0", "", "", "2001:db8:a:c010:2bf0::1"},
		{"two globals: order of input does not matter", []if6Addr{g2, g1}, "eth0", "", "", "2001:db8:a:c010:2bf0::1"},
		{"two globals: current one kept", []if6Addr{g1, g2}, "eth0", "", "2001:db8:a:c010:3724::1", "2001:db8:a:c010:3724::1"},
		{"current one gone", []if6Addr{g2}, "eth0", "", "2001:db8:a:c010:3724::1", "2001:db8:a:c010:2bf0::1"},
		{"default-route interface first", []if6Addr{g3, g1}, "eth0", "", "", "2001:db8:a:c010:3724::1"},
		{"default-route interface beats a kept off-route one", []if6Addr{g3, g1}, "eth0", "", "2001:db8:5::1", "2001:db8:a:c010:3724::1"},
		{"no default route, nothing current: none", []if6Addr{g3, g1}, "", "", "", ""},
		{"no default route: current kept", []if6Addr{g3, g1}, "", "", "2001:db8:a:c010:3724::1", "2001:db8:a:c010:3724::1"},
		{"only off-route globals: none", []if6Addr{g3}, "eth0", "", "", ""},
		{"pinned interface", []if6Addr{g3, g1}, "eth0", "eth1", "", "2001:db8:5::1"},
		{"pinned interface without a global", []if6Addr{g1, ll}, "eth0", "eth1", "", ""},
	}
	for _, c := range cases {
		if got, _, _ := pickAuto6(c.addrs, c.def, c.ifc, c.prev); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

// The 2026-11-19 production bug: xray's Cloudflare WARP interface wg10 has a
// global /128 (2001:db8:f:...) that rotates with every xray restart;
// auto6 must stay on eth0's address.
func TestPickAuto6WarpAndRoute(t *testing.T) {
	eth := if6Addr{Iface: "eth0", IP: net.ParseIP("2001:db8:a:c010:2bf0::1"), Flags: ifaFPermanent, Plen: 64}
	eth2 := if6Addr{Iface: "eth0", IP: net.ParseIP("2001:db8:a:c010:77::1"), Flags: ifaFPermanent, Plen: 64}
	wg := if6Addr{Iface: "wg10", IP: net.ParseIP("2001:db8:f:8020::5"), Flags: ifaFPermanent, Plen: 128, Virt: true}
	wgLow := if6Addr{Iface: "wg10", IP: net.ParseIP("2001:db8::5"), Flags: ifaFPermanent, Plen: 128, Virt: true}
	warp := if6Addr{Iface: "CloudflareWARP", IP: net.ParseIP("2001:db8:f:1::9"), Flags: ifaFPermanent, Plen: 128}
	tun := if6Addr{Iface: "tun0", IP: net.ParseIP("2001:db8:7::1"), Flags: ifaFPermanent, Plen: 64}
	dummy := if6Addr{Iface: "hs2t6", IP: net.ParseIP("2001:db8:9::1"), Flags: ifaFPermanent, Plen: 128}
	eth128 := if6Addr{Iface: "eth0", IP: net.ParseIP("2001:db8:a:c010:1::1"), Flags: ifaFPermanent, Plen: 128}
	dyn := if6Addr{Iface: "eth0", IP: net.ParseIP("2001:db8:a:c010:1::2"), Plen: 64}
	cases := []struct {
		name           string
		addrs          []if6Addr
		def, ifc, prev string
		want, wantIf   string
		noteHas        string
	}{
		{"eth0 /64 + wg10 /128 -> eth0", []if6Addr{wg, eth}, "eth0", "", "", eth.IP.String(), "eth0", "default IPv6 route dev eth0"},
		{"wg10 address lower than eth0's: still eth0", []if6Addr{wgLow, eth, warp, tun, dummy}, "eth0", "", "", eth.IP.String(), "eth0", "dev eth0"},
		{"current = the WARP address: moves to eth0", []if6Addr{wg, eth}, "eth0", "", wg.IP.String(), eth.IP.String(), "eth0", ""},
		{"default route via eth0 among many", []if6Addr{tun, warp, dummy, eth}, "eth0", "", "", eth.IP.String(), "eth0", ""},
		{"eth0 address changed -> the new one", []if6Addr{wg, eth2}, "eth0", "", eth.IP.String(), eth2.IP.String(), "eth0", ""},
		{"eth0 current kept beside a new one (no flapping)", []if6Addr{eth2, eth}, "eth0", "", eth.IP.String(), eth.IP.String(), "eth0", "dev eth0"},
		{"no default v6 route: current kept, logged", []if6Addr{wg, eth}, "", "", eth.IP.String(), eth.IP.String(), "eth0", "no default IPv6 route on this server: kept"},
		{"no default v6 route, current on wg10: not kept", []if6Addr{wg, eth}, "", "", wg.IP.String(), "", "", "no default IPv6 route"},
		{"no default v6 route, nothing current: none", []if6Addr{wg, eth}, "", "", "", "", "", "no default IPv6 route"},
		{"default route via wg10 (defensive): none", []if6Addr{wg, eth}, "wg10", "", "", "", "", "no default IPv6 route"},
		{"/64 before /128 on eth0", []if6Addr{eth128, eth}, "eth0", "", "", eth.IP.String(), "eth0", ""},
		{"/128 on the default-route interface itself is fine", []if6Addr{eth128, wg}, "eth0", "", "", eth128.IP.String(), "eth0", ""},
		{"permanent before dynamic", []if6Addr{dyn, eth}, "eth0", "", "", eth.IP.String(), "eth0", ""},
		{"pinned wg10: the operator's choice", []if6Addr{wg, eth}, "eth0", "wg10", "", wg.IP.String(), "wg10", "pinned interface wg10"},
	}
	for _, c := range cases {
		ip, ifc, note := pickAuto6(c.addrs, c.def, c.ifc, c.prev)
		if ip != c.want || ifc != c.wantIf || !strings.Contains(note, c.noteHas) {
			t.Errorf("%s: got %q on %q (%s), want %q on %q (%q)", c.name, ip, ifc, note, c.want, c.wantIf, c.noteHas)
		}
	}
	for _, n := range []string{"wg10", "wg0", "tun0", "tap1", "ppp0", "docker0", "veth12ab", "warp0", "CloudflareWARP", "lo", "tailscale0"} {
		if !auto6VirtName(n) {
			t.Errorf("%s not virtual", n)
		}
	}
	for _, n := range []string{"eth0", "ens3", "enp1s0", "eno1", "bond0", "hs2t6"} {
		if auto6VirtName(n) {
			t.Errorf("%s virtual", n)
		}
	}
	z := strings.Repeat("0", 32)
	route := strings.Join([]string{
		z + " 00 " + z + " 00 " + z + " 00000001 00000001 00000000 00000001     wg10",
		z + " 00 " + z + " 00 fe800000000000000000000000000001 00000400 00000001 00000000 00000003     eth0",
	}, "\n")
	if d := parseDefault6Iface(route); d != "eth0" {
		t.Fatalf("default iface via /proc with a wg10 default: %q", d)
	}
}

// HS2_AUTO6_REALHOST=1 (lab only, Linux): the real source. With
// HS2_AUTO6_IGNORE=IFACE (a dummy with a global /128 made for the test) the
// pick must not be on IFACE and must be on the default-route interface.
func TestAuto6RealHost(t *testing.T) {
	if os.Getenv("HS2_AUTO6_REALHOST") != "1" {
		t.Skip("HS2_AUTO6_REALHOST=1 only")
	}
	addrs, def, err := auto6Source()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range addrs {
		t.Logf("addr %s on %s /%d flags %#x virt=%v", a.IP, a.Iface, a.Plen, a.Flags, a.Virt)
	}
	ip, ifc, note := pickAuto6(addrs, def, "", "")
	t.Logf("route get dev %q, main default dev %q, chosen default %q -> %q on %q (%s)", auto6RouteGetIface(), auto6MainDefaultIface(), def, ip, ifc, note)
	t.Logf("check line: %s", checkAuto6Line("auto6"))
	if ign := os.Getenv("HS2_AUTO6_IGNORE"); ign != "" {
		seen := false
		for _, a := range addrs {
			seen = seen || a.Iface == ign
		}
		if !seen {
			t.Fatalf("test interface %s has no IPv6 address here", ign)
		}
		if ifc == ign || def == ign {
			t.Fatalf("auto6 picked the test interface %s", ign)
		}
	}
	if ip != "" && ifc != def {
		t.Fatalf("picked %s on %s, default route dev %s", ip, ifc, def)
	}
}

// fakeAuto6 swaps the host's address source for the test.
type fakeAuto6 struct {
	mu    sync.Mutex
	addrs []if6Addr
	def   string
}

func (f *fakeAuto6) set(def string, a ...if6Addr) {
	f.mu.Lock()
	f.addrs, f.def = a, def
	f.mu.Unlock()
}

func installFakeAuto6(t *testing.T) *fakeAuto6 {
	f := &fakeAuto6{}
	old, oldLoop := auto6Source, auto6Loop
	auto6Source = func() ([]if6Addr, string, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		return append([]if6Addr(nil), f.addrs...), f.def, nil
	}
	auto6Loop = false
	t.Cleanup(func() {
		auto6Source, auto6Loop = old, oldLoop
		lbAutoMu.Lock()
		lbAutoStartRaw, lbAutoStartRes, lbAutoStartNote = nil, nil, ""
		lbAutoMu.Unlock()
	})
	return f
}

// Reverse kharej (dials): v4 primary + auto6; iran announces v4 and v6.
const lbKharejRevAuto6 = `{"mode":"listen","reverse":true,"carrier":"mtcp","addr":"10.1.0.1:443","bind_local_ip":"10.0.0.1",
 "lb":{"enabled":true,"iran":[{"ip":"10.1.0.1","weight":50},{"ip":"2001:db8:1::1","weight":50}],
       "kharej":[{"ip":"10.0.0.1","weight":50},{"ip":"auto6","weight":50}]}}`

func lbNames(t *testing.T, fc fileConfig) []string {
	t.Helper()
	ps, err := lbPaths(fc)
	if err != nil {
		t.Fatal(err)
	}
	var n []string
	for _, p := range ps {
		n = append(n, p.Name())
	}
	sort.Strings(n)
	return n
}

func TestLBAuto6ConfigAndPaths(t *testing.T) {
	f := installFakeAuto6(t)
	fc := lbFC(t, lbKharejRevAuto6)
	if err := validateLB(fc); err != nil {
		t.Fatalf("auto6 refused: %v", err)
	}
	// twice is refused like a literal listed twice
	bad := strings.Replace(lbKharejRevAuto6, `{"ip":"10.0.0.1","weight":50},{"ip":"auto6","weight":50}`, `{"ip":"10.0.0.1","weight":50},{"ip":"auto6","weight":25},{"ip":"AUTO6","weight":25}`, 1)
	if err := validateLB(lbFC(t, bad)); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("auto6 twice: %v", err)
	}
	// unresolved (no global v6): no v6 path at all, the v4 path stays
	if got := strings.Join(lbNames(t, fc), " "); got != "10.0.0.1>10.1.0.1" {
		t.Fatalf("unresolved paths: %s", got)
	}
	f.set("eth0", a6("fe80::1", "eth0", 0), a6("2001:db8:a:c010:3724::1", "eth0", 0))
	r := lbResolveStart(fc, t.Logf)
	if got := strings.Join(lbNames(t, r), " "); got != "10.0.0.1>10.1.0.1 2001:db8:a:c010:3724::1>2001:db8:1::1" {
		t.Fatalf("resolved paths: %s", got)
	}
	if fc.LB.Kharej[1].IP != "auto6" {
		t.Fatal("the raw config was modified")
	}
	// hs2 check's line
	if l := checkAuto6Line("auto6"); !strings.Contains(l, "2001:db8:a:c010:3724::1") {
		t.Fatalf("check line: %s", l)
	}
	f.set("eth0")
	if l := checkAuto6Line("auto6:eth0"); !strings.Contains(l, "no global IPv6 on interface eth0") {
		t.Fatalf("check line: %s", l)
	}
	// literal entries are untouched
	lit := lbFC(t, lbKharejRev)
	if r := lbResolveStart(lit, t.Logf); r.LB != lit.LB {
		t.Fatal("literal config changed")
	}
}

// Listening side: an auto6 without an address is not announced (the rest
// is scaled to 100); with one, it is announced and listened on.
func TestLBAuto6ListenSideAdvert(t *testing.T) {
	f := installFakeAuto6(t)
	js := `{"mode":"listen","carrier":"mtcp","addr":"10.1.0.1:443",
 "lb":{"enabled":true,"kharej":[{"ip":"10.1.0.1","weight":60},{"ip":"auto6","weight":40}]}}`
	fc := lbFC(t, js)
	a := lbOwnAdvert(applyAutos(fc, map[string]string{"auto6": ""}))
	if len(a) != 1 || a[0].IP != "10.1.0.1" || a[0].Weight != 100 {
		t.Fatalf("unresolved advert %+v", a)
	}
	f.set("eth0", a6("2001:db8:9::9", "eth0", 0))
	r := lbResolveStart(fc, t.Logf)
	a = lbOwnAdvert(r)
	if len(a) != 2 || a[1].IP != "2001:db8:9::9" || a[1].Weight != 40 {
		t.Fatalf("resolved advert %+v", a)
	}
	if l := lbListenIPs(r); len(l) != 1 || l[0] != "2001:db8:9::9" {
		t.Fatalf("listen ips %v", l)
	}
}

// The change-over: the running dialing side moves its v6 path to the new
// address in place (no restart), and an unchanged address changes nothing.
func TestLBAuto6ChangeOver(t *testing.T) {
	f := installFakeAuto6(t)
	f.set("eth0", a6("2001:db8:a:c010:3724::1", "eth0", 0))
	cfg := filepath.Join(t.TempDir(), "c.json")
	writeCfg(t, cfg, lbKharejRevAuto6)
	resetLBRun(t, cfg)
	var mu sync.Mutex
	var lines []string
	logf := func(f string, a ...any) {
		mu.Lock()
		lines = append(lines, fmt.Sprintf(f, a...))
		mu.Unlock()
	}
	fc := lbResolveStart(lbFC(t, lbKharejRevAuto6), logf)
	lb := newLB(fc, logf)
	setupLB(fc, lb, nil, logf)
	rt := curLB()
	names := func() string {
		var n []string
		for _, p := range lb.Paths() {
			n = append(n, p.Name())
		}
		sort.Strings(n)
		return strings.Join(n, " ")
	}
	if got := names(); got != "10.0.0.1>10.1.0.1 2001:db8:a:c010:3724::1>2001:db8:1::1" {
		t.Fatalf("start paths %s", got)
	}
	if rt.auto6Check(time.Now()) {
		t.Fatal("no change reported as a change")
	}
	// reboot-like change: A gone, B present
	f.set("eth0", a6("fe80::1", "eth0", 0), a6("2001:db8:a:c010:2bf0::1", "eth0", 0))
	if !rt.auto6Check(time.Now()) {
		t.Fatal("change not seen")
	}
	if got := names(); got != "10.0.0.1>10.1.0.1 2001:db8:a:c010:2bf0::1>2001:db8:1::1" {
		t.Fatalf("paths after the change %s", got)
	}
	// v6 gone completely: the v6 path disappears, v4 stays
	f.set("eth0", a6("fe80::1", "eth0", 0))
	if !rt.auto6Check(time.Now()) {
		t.Fatal("loss not seen")
	}
	if got := names(); got != "10.0.0.1>10.1.0.1" {
		t.Fatalf("paths without v6 %s", got)
	}
	// still none: no change, and the "still none" line is rate limited
	before := len(lines)
	for i := 0; i < 5; i++ {
		rt.auto6Check(time.Now())
	}
	if len(lines) != before {
		t.Fatalf("log not rate limited: %v", lines[before:])
	}
	// SIGHUP keeps the spec (the raw section) and resolves it again
	f.set("eth0", a6("2001:db8:a:c010:aaaa::1", "eth0", 0))
	writeCfg(t, cfg, strings.Replace(lbKharejRevAuto6, `"weight":50}],`, `"weight":50}], `, 1))
	reloadLB("test")
	rt.auto6Check(time.Now())
	if got := names(); got != "10.0.0.1>10.1.0.1 2001:db8:a:c010:aaaa::1>2001:db8:1::1" {
		t.Fatalf("paths after reload %s", got)
	}
	mu.Lock()
	joined := strings.Join(lines, "\n")
	mu.Unlock()
	if !strings.Contains(joined, "lb: auto6: this server's IPv6 changed: auto6=2001:db8:a:c010:3724::1 -> auto6=2001:db8:a:c010:2bf0::1") {
		t.Fatalf("no change line:\n%s", joined)
	}
	// b333 fix: the default v6 route goes away (WARP's wg10 /128 still up):
	// the current address is kept and the reason logged once; the WARP
	// address is never taken
	wg := if6Addr{Iface: "wg10", IP: net.ParseIP("2001:db8:f:8020::5"), Flags: ifaFPermanent, Plen: 128, Virt: true}
	f.set("", wg, a6("2001:db8:a:c010:aaaa::1", "eth0", 0x80))
	later := time.Now().Add(2 * time.Minute)
	if rt.auto6Check(later) {
		t.Fatal("route loss changed the address")
	}
	if rt.auto6Check(later.Add(time.Second)) {
		t.Fatal("second check changed the address")
	}
	mu.Lock()
	joined = strings.Join(lines, "\n")
	n := strings.Count(joined, "no default IPv6 route on this server: kept 2001:db8:a:c010:aaaa::1 on eth0 (address unchanged)")
	mu.Unlock()
	if n != 1 {
		t.Fatalf("no-route line logged %d times:\n%s", n, joined)
	}
	// the default route comes back via eth0 with WARP still up: still eth0
	f.set("eth0", wg, a6("2001:db8:a:c010:aaaa::1", "eth0", 0x80))
	rt.auto6Check(later.Add(2 * time.Minute))
	if got := names(); got != "10.0.0.1>10.1.0.1 2001:db8:a:c010:aaaa::1>2001:db8:1::1" {
		t.Fatalf("paths with WARP up %s", got)
	}
}
