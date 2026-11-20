package main

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/engine"
)

// b333: "auto6" in an lb address list.
//
// A Hetzner cloud VM gets a NEW global IPv6 at every boot (cloud-init), so a
// literal v6 entry in the lb list goes stale at the first reboot: every dial
// from it fails with "bind: cannot assign requested address" until someone
// edits the file. An entry {"ip": "auto6"} (or "auto6:eth0" to pin the
// interface) stands for this host's current global unicast IPv6 instead:
//
//   - candidates: IPv6 global unicast, not link-local, not ULA (fc00::/7),
//     not deprecated, temporary (privacy), tentative or DAD-failed;
//   - "auto6" takes an address ONLY on the interface of the kernel's IPv6
//     route to the internet (`ip -6 route get 2001:4860::`, else the main
//     table's default route), never one on a wg/tun/tap/ppp/docker/veth/warp
//     or other virtual or point-to-point interface (rel69r fix: xray's
//     Cloudflare WARP wg10 /128 was picked in production); the current
//     choice is kept while it is still there (no flapping), else a prefix
//     shorter than /128, a permanent one, the lowest address; no default
//     route: the current choice is kept while it exists (pickAuto6);
//   - re-resolved at once when the kernel adds or removes an IPv6 address
//     (rtnetlink, lbauto6_linux.go) or a dial fails with "cannot assign
//     requested address" (engine.LocalAddrErr), and every auto6Every anyway. On a change the
//     running tunnel swaps the path set in place (lbRuntime.rebuildLocked),
//     closes the links still bound to the old address (engine.LB.CloseLinksFrom)
//     and, on a listening side, listens on the new address and re-announces
//     its list — no restart;
//   - none found: the entry has no path (no dial at all), with a log line on
//     each change and at most once per auto6LogEvery while it stays absent.
//
// Only this side's own list is resolved; an auto6 entry in the other side's
// list is the other server's business and gives no path here. Literal IPs are
// untouched.

const auto6Word = "auto6"

var (
	auto6Every    = 10 * time.Second // two small /proc reads; also bounds the boot race (hs2 up before cloud-init's v6)
	auto6LogEvery = 10 * time.Minute
	auto6Loop     = true // tests: no background resolver (they call auto6Check)
)

// lbAutoSpec parses "auto6" / "auto6:IFACE".
func lbAutoSpec(s string) (iface string, ok bool) {
	t := strings.TrimSpace(s)
	if len(t) < len(auto6Word) || !strings.EqualFold(t[:len(auto6Word)], auto6Word) {
		return "", false
	}
	rest := t[len(auto6Word):]
	switch {
	case rest == "":
		return "", true
	case rest[0] == ':' && len(rest) > 1 && !strings.ContainsAny(rest[1:], " /:"):
		return rest[1:], true
	}
	return "", false
}

// lbEntryKey is an entry's identity in a list: the canonical IP, or the auto
// spec as written ("auto6", "auto6:eth0").
func lbEntryKey(s string) string {
	if ifc, ok := lbAutoSpec(s); ok {
		if ifc == "" {
			return auto6Word
		}
		return auto6Word + ":" + ifc
	}
	if ip := net.ParseIP(s); ip != nil {
		return ip.String()
	}
	return s
}

// if6Addr is one IPv6 address of the host as /proc/net/if_inet6 shows it.
// Plen is the prefix length (0 = unknown); Virt marks an interface that is
// point-to-point or not an Ethernet-like link (wireguard/tun/ppp/sit/..., from
// /sys, auto6IfaceVirt).
type if6Addr struct {
	Iface string
	IP    net.IP
	Flags uint32
	Plen  int
	Virt  bool
}

// IFA_F_* (linux/if_addr.h).
const (
	ifaFTemporary  = 0x01
	ifaFDadFailed  = 0x08
	ifaFDeprecated = 0x20
	ifaFTentative  = 0x40
	ifaFPermanent  = 0x80
)

// parseIfInet6 parses /proc/net/if_inet6:
// "addr(32 hex) ifindex prefixlen scope flags name".
func parseIfInet6(text string) []if6Addr {
	var out []if6Addr
	for _, ln := range strings.Split(text, "\n") {
		f := strings.Fields(ln)
		if len(f) < 6 || len(f[0]) != 32 {
			continue
		}
		b, err := hex.DecodeString(f[0])
		if err != nil {
			continue
		}
		fl, err := strconv.ParseUint(f[4], 16, 32)
		if err != nil {
			continue
		}
		pl, err := strconv.ParseUint(f[2], 16, 8)
		if err != nil {
			continue
		}
		out = append(out, if6Addr{Iface: f[5], IP: net.IP(b), Flags: uint32(fl), Plen: int(pl)})
	}
	return out
}

// parseDefault6Iface parses /proc/net/ipv6_route and returns the interface of
// the IPv6 default route with the lowest metric ("" = none). Reject routes,
// lo and virtual interface names (auto6VirtName) are skipped. It is the last
// fallback of auto6DefaultIface (this file has no routing-table column).
func parseDefault6Iface(text string) string {
	best, bestM := "", uint64(1<<63)
	for _, ln := range strings.Split(text, "\n") {
		f := strings.Fields(ln)
		if len(f) < 10 || f[0] != strings.Repeat("0", 32) || f[1] != "00" {
			continue
		}
		m, err1 := strconv.ParseUint(f[5], 16, 64)
		fl, err2 := strconv.ParseUint(f[8], 16, 32)
		if err1 != nil || err2 != nil || fl&0x0200 != 0 || f[9] == "lo" || auto6VirtName(f[9]) { // RTF_REJECT
			continue
		}
		if m < bestM {
			best, bestM = f[9], m
		}
	}
	return best
}

// auto6VirtName: interface names that never carry this server's own uplink
// address for auto6 — VPN, tunnel and container links. The 2026-11-19
// production bug: xray's Cloudflare WARP interface wg10 has a global /128
// (2001:db8:f:...) that rotates with every xray restart, and auto6 picked
// it instead of eth0's address.
func auto6VirtName(name string) bool {
	n := strings.ToLower(name)
	if n == "lo" || strings.Contains(n, "warp") {
		return true
	}
	for _, p := range []string{"wg", "tun", "tap", "ppp", "docker", "veth", "br-", "virbr", "vxlan",
		"gre", "ip6gre", "ip6tnl", "sit", "teredo", "tailscale", "zt", "nordlynx", "utun", "ipsec",
		"vti", "xfrm", "he-ipv6", "6to4", "cni", "flannel", "cali", "kube", "lxc", "vmnet", "vboxnet"} {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	return false
}

// auto6Candidate: a usable global address for a tunnel source.
func auto6Candidate(a if6Addr) bool {
	ip := a.IP
	if len(ip) != net.IPv6len || ip.To4() != nil || !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return false
	}
	return a.Flags&(ifaFTemporary|ifaFDadFailed|ifaFDeprecated|ifaFTentative) == 0
}

// auto6Uplinkish: an address that may be kept while no default route is
// known: not on a virtual or point-to-point interface, not a /128.
func auto6Uplinkish(a if6Addr) bool {
	return !a.Virt && !auto6VirtName(a.Iface) && a.Plen < 128
}

// pickAuto6 chooses the address for one spec. prev is the current choice
// ("" = none); defIface is the interface of the kernel's IPv6 route to the
// internet (auto6DefaultIface, "" = none).
//
//   - "auto6:IFACE": a global on IFACE (the operator chose the interface);
//   - "auto6": ONLY an address on the default-route interface — never one on
//     a wg/tun/warp/... or point-to-point interface, whatever its scope;
//   - the current choice is kept while it is still a candidate there (no
//     flapping); else a prefix shorter than /128 first, then a permanent
//     (static) one, then the lowest address;
//   - no usable default route: the current choice is kept while it still
//     exists on a non-virtual interface and is not a /128; else none.
//
// It returns the address ("" = none), its interface and words for the log.
func pickAuto6(addrs []if6Addr, defIface, wantIface, prev string) (ip, iface, note string) {
	if wantIface == "" && auto6VirtName(defIface) {
		defIface = "" // defensive: auto6DefaultIface never returns one
	}
	if wantIface == "" && defIface == "" {
		for _, a := range addrs {
			if prev != "" && a.IP.String() == prev && auto6Candidate(a) && auto6Uplinkish(a) {
				return prev, a.Iface, "no default IPv6 route on this server: kept " + prev + " on " + a.Iface
			}
		}
		return "", "", "no default IPv6 route on this server (or only via a VPN/tunnel interface): no address picked (\"auto6:IFACE\" pins an interface)"
	}
	on, how := wantIface, "pinned interface "+wantIface
	if on == "" {
		on, how = defIface, "default IPv6 route dev "+defIface
	}
	var c []if6Addr
	for _, a := range addrs {
		if a.Iface != on || !auto6Candidate(a) {
			continue
		}
		if prev != "" && a.IP.String() == prev {
			return prev, on, how // same words as a fresh pick: a note change means a real change
		}
		c = append(c, a)
	}
	if len(c) == 0 {
		return "", "", how + ": no stable global IPv6 on " + on
	}
	rank := func(a if6Addr) int {
		r := 0
		if a.Plen >= 128 {
			r += 2
		}
		if a.Flags&ifaFPermanent == 0 {
			r++
		}
		return r
	}
	sort.SliceStable(c, func(i, j int) bool {
		if ri, rj := rank(c[i]), rank(c[j]); ri != rj {
			return ri < rj
		}
		return bytes.Compare(c[i].IP, c[j].IP) < 0
	})
	return c[0].IP.String(), on, how
}

// auto6Source reads the host's IPv6 addresses and default-route interface
// (var for tests).
var auto6Source = func() ([]if6Addr, string, error) {
	a, err := os.ReadFile("/proc/net/if_inet6")
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", nil // no IPv6 at all
		}
		return nil, "", err
	}
	addrs := parseIfInet6(string(a))
	for i := range addrs {
		addrs[i].Virt = auto6IfaceVirt(addrs[i].Iface)
	}
	return addrs, auto6DefaultIface(), nil
}

// auto6IfaceVirt: /sys says the interface is point-to-point (IFF_POINTOPOINT)
// or not an Ethernet/InfiniBand link (wireguard and tun are ARPHRD_NONE, ppp
// is 512, sit/ip6tnl/gre are 768+). Unknown: false.
func auto6IfaceVirt(name string) bool {
	if name == "" || name == "." || name == ".." || strings.Contains(name, "/") {
		return true
	}
	if b, err := os.ReadFile("/sys/class/net/" + name + "/flags"); err == nil {
		if f, err := strconv.ParseUint(strings.TrimPrefix(strings.TrimSpace(string(b)), "0x"), 16, 32); err == nil && f&0x10 != 0 {
			return true
		}
	}
	if b, err := os.ReadFile("/sys/class/net/" + name + "/type"); err == nil {
		if t, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && t != 1 && t != 32 {
			return true
		}
	}
	return false
}

// auto6DefaultIface is the interface of the kernel's IPv6 route to the
// internet: `ip -6 route get 2001:4860::` (rtnetlink, lbauto6_linux.go), else
// the lowest-metric default route of the main table (`ip -6 route show
// default`), else /proc/net/ipv6_route. A route via a virtual or
// point-to-point interface does not count. "" = none.
func auto6DefaultIface() string {
	ok := func(n string) bool { return n != "" && !auto6VirtName(n) && !auto6IfaceVirt(n) }
	if n := auto6RouteGetIface(); ok(n) {
		return n
	}
	if n := auto6MainDefaultIface(); ok(n) {
		return n
	}
	r, _ := os.ReadFile("/proc/net/ipv6_route")
	if n := parseDefault6Iface(string(r)); ok(n) {
		return n
	}
	return ""
}

// lbLocalAutos lists the auto specs in this side's own list.
func lbLocalAutos(fc fileConfig) []string {
	if !lbOn(fc) {
		return nil
	}
	local, _, _, _ := lbLists(fc)
	var out []string
	for _, e := range local {
		if _, ok := lbAutoSpec(e.IP); ok {
			out = append(out, lbEntryKey(e.IP))
		}
	}
	return out
}

// resolveAutos resolves every spec of raw's own list; prev holds the current
// choices. A spec with no address maps to "".
func resolveAutos(raw fileConfig, prev map[string]string) (map[string]string, error) {
	res, _, err := resolveAutosNote(raw, prev)
	return res, err
}

// resolveAutosNote is resolveAutos plus, for the log, which interface each
// spec used and why ("auto6: default IPv6 route dev eth0, kept 2a01:...").
func resolveAutosNote(raw fileConfig, prev map[string]string) (map[string]string, string, error) {
	specs := lbLocalAutos(raw)
	if len(specs) == 0 {
		return nil, "", nil
	}
	addrs, def, err := auto6Source()
	if err != nil {
		return nil, "", err
	}
	out := map[string]string{}
	var notes []string
	for _, s := range specs {
		ifc, _ := lbAutoSpec(s)
		ip, _, note := pickAuto6(addrs, def, ifc, prev[s])
		notes = append(notes, s+": "+note)
		// a literal entry of the list already names it: keep the spec empty
		// (the address keeps its literal path; a duplicate would be refused)
		local, _, _, _ := lbLists(raw)
		for _, e := range local {
			if lbEntryKey(e.IP) == ip {
				ip = ""
			}
		}
		for _, v := range out {
			if v == ip {
				ip = "" // two specs on one address: the first one has it
			}
		}
		out[s] = ip
	}
	return out, strings.Join(notes, "; "), nil
}

// applyAutos returns raw with its own list's specs replaced by their
// addresses (a spec with "" stays as written: it gives no path).
func applyAutos(raw fileConfig, res map[string]string) fileConfig {
	if !lbOn(raw) || len(res) == 0 {
		return raw
	}
	lb := *raw.LB
	own := &lb.Kharej
	if raw.Mode == "dial" {
		own = &lb.Iran
	}
	l := append([]lbIP(nil), (*own)...)
	for i, e := range l {
		if _, ok := lbAutoSpec(e.IP); ok {
			if ip := res[lbEntryKey(e.IP)]; ip != "" {
				l[i].IP = ip
			}
		}
	}
	*own = l
	raw.LB = &lb
	return raw
}

// lbAutoUnresolved: an entry still holding a spec (no address found, or the
// other side's list).
func lbAutoUnresolved(e lbIP) bool {
	_, ok := lbAutoSpec(e.IP)
	return ok
}

func fmtAutos(res map[string]string) string {
	var s []string
	for k, v := range res {
		if v == "" {
			v = "none"
		}
		s = append(s, k+"="+v)
	}
	sort.Strings(s)
	return strings.Join(s, ", ")
}

// lbAutoStart is the raw (unresolved) lb section of this run and its first
// resolution, handed from lbResolveStart to setupLB.
var (
	lbAutoMu        sync.Mutex
	lbAutoStartRaw  *lbConfig
	lbAutoStartRes  map[string]string
	lbAutoStartNote string
)

// lbResolveStart resolves the auto specs once at start (before the paths and
// listeners are built) and returns the config to run with.
func lbResolveStart(fc fileConfig, logf func(string, ...any)) fileConfig {
	if len(lbLocalAutos(fc)) == 0 {
		return fc
	}
	res, note, err := resolveAutosNote(fc, nil)
	if err != nil {
		logf("lb: auto6: cannot read this server's IPv6 addresses: %v — the auto6 path is off until they can be read", err)
	} else {
		logf("lb: auto6: %s (%s)", auto6Words(res), note)
	}
	lbAutoMu.Lock()
	lbAutoStartRaw, lbAutoStartRes, lbAutoStartNote = fc.LB, res, note
	lbAutoMu.Unlock()
	return applyAutos(fc, res)
}

func auto6Words(res map[string]string) string {
	none := true
	for _, v := range res {
		if v != "" {
			none = false
		}
	}
	if none {
		return fmtAutos(res) + " — no global IPv6 on this server: the v6 path is off (checked every " + fmtDurShort(auto6Every) + ")"
	}
	return "resolved " + fmtAutos(res)
}

func fmtDurShort(d time.Duration) string { return d.String() }

// --- runtime -------------------------------------------------------------

// auto6Kick asks the running resolver for an immediate check (a dial failed
// because its source address is gone). Never blocks.
var auto6Kick = make(chan struct{}, 1)

func lbAutoKick() {
	select {
	case auto6Kick <- struct{}{}:
	default:
	}
}

// lbDialFailed is called with every lb path dial error.
func lbDialFailed(err error) {
	if err != nil && engine.LocalAddrErr(err) {
		lbAutoKick()
	}
}

// runAuto6 re-resolves the specs every auto6Every (and on a kick) and applies
// a change to the running tunnel. It returns when stop is closed.
func (rt *lbRuntime) runAuto6(stop <-chan struct{}) {
	go auto6Watch(stop) // address add/remove notifications (lbauto6_linux.go)
	t := time.NewTicker(auto6Every)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		case <-auto6Kick:
			// coalesce a burst (an address swap is a del + an add; failed
			// dials come in groups): one check after it settles
			time.Sleep(300 * time.Millisecond)
			select {
			case <-auto6Kick:
			default:
			}
		}
		rt.auto6Check(time.Now())
	}
}

// auto6Check resolves once and applies a change (exported to tests via rt).
func (rt *lbRuntime) auto6Check(now time.Time) bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.raw == nil {
		return false
	}
	raw := rt.fc
	raw.LB = rt.raw
	res, note, err := resolveAutosNote(raw, rt.auto)
	if err != nil {
		if now.Sub(rt.autoLogAt) >= auto6LogEvery {
			rt.logf("lb: auto6: cannot read this server's IPv6 addresses: %v", err)
			rt.autoLogAt = now
		}
		return false
	}
	if mapsEqualStr(res, rt.auto) {
		switch {
		case allEmpty(res) && now.Sub(rt.autoLogAt) >= auto6LogEvery:
			rt.logf("lb: auto6: still no global IPv6 on this server — the v6 path stays off (%s)", note)
			rt.autoLogAt, rt.autoNote, rt.autoNoteAt = now, note, now
		case note != rt.autoNote && now.Sub(rt.autoNoteAt) >= time.Minute:
			// same address, other reason (e.g. the default route went away
			// and the address is kept): once per change, at most 1/min
			rt.logf("lb: auto6: %s (address unchanged)", note)
			rt.autoNote, rt.autoNoteAt = note, now
		}
		return false
	}
	old := rt.auto
	rt.auto = res
	rt.autoLogAt, rt.autoNote, rt.autoNoteAt = now, note, now
	next := applyAutos(raw, res)
	if allEmpty(res) {
		rt.logf("lb: auto6: this server's IPv6 is gone: %s -> %s — the v6 path is off until one appears (checked every %s, no restart; %s)", fmtAutos(old), fmtAutos(res), fmtDurShort(auto6Every), note)
	} else {
		rt.logf("lb: auto6: this server's IPv6 changed: %s -> %s — moving the path, no restart (%s)", fmtAutos(old), fmtAutos(res), note)
	}
	rt.fc = next
	rt.rebuildLocked("auto6 address change")
	if rt.ln != nil {
		rt.syncListenersLocked()
		if m := curFrontMux.Load(); m != nil {
			m.rebuild(next)
		}
	}
	if rt.adv != nil && rt.lb == nil {
		rt.adv.SetMine(lbOwnAdvert(next))
	}
	if rt.lb != nil {
		for k, ip := range old {
			if ip != "" && res[k] != ip {
				if n := rt.lb.CloseLinksFrom(ip); n > 0 {
					rt.logf("lb: auto6: closed %d link(s) still bound to the old address %s (their slots redial on the current paths)", n, ip)
				}
			}
		}
	}
	return true
}

func mapsEqualStr(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

func allEmpty(m map[string]string) bool {
	for _, v := range m {
		if v != "" {
			return false
		}
	}
	return true
}

// checkAuto6Line is `hs2 check`'s line for an auto spec.
func checkAuto6Line(spec string) string {
	ifc, _ := lbAutoSpec(spec)
	addrs, def, err := auto6Source()
	if err != nil {
		return fmt.Sprintf("%s: cannot read this server's IPv6 addresses (%v)", spec, err)
	}
	ip, on, note := pickAuto6(addrs, def, ifc, "")
	if ip == "" {
		where := "this server"
		if ifc != "" {
			where = "interface " + ifc
		}
		return fmt.Sprintf("%s: no global IPv6 on %s now (%s) — the v6 path stays off until one appears (re-checked every %s while running)", spec, where, note, fmtDurShort(auto6Every))
	}
	return fmt.Sprintf("%s: resolves to %s on %s now (%s; re-resolved every %s while running; a new address moves the path without a restart)", spec, ip, on, note, fmtDurShort(auto6Every))
}

// lbCheckResolved is fc with its auto specs resolved now (hs2 check's path
// summary); fc itself when there are none or they cannot be read.
func lbCheckResolved(fc fileConfig) fileConfig {
	if len(lbLocalAutos(fc)) == 0 {
		return fc
	}
	res, err := resolveAutos(fc, nil)
	if err != nil {
		return fc
	}
	return applyAutos(fc, res)
}
