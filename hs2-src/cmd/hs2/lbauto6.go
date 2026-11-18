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
//   - the address on the default-route interface first, then by interface
//     name and address bytes (deterministic); the current choice is kept while
//     it stays a candidate of the best tier (no flapping between two globals);
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
type if6Addr struct {
	Iface string
	IP    net.IP
	Flags uint32
}

// IFA_F_* (linux/if_addr.h).
const (
	ifaFTemporary  = 0x01
	ifaFDadFailed  = 0x08
	ifaFDeprecated = 0x20
	ifaFTentative  = 0x40
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
		out = append(out, if6Addr{Iface: f[5], IP: net.IP(b), Flags: uint32(fl)})
	}
	return out
}

// parseDefault6Iface parses /proc/net/ipv6_route and returns the interface of
// the IPv6 default route with the lowest metric ("" = none). Reject routes
// and lo are skipped.
func parseDefault6Iface(text string) string {
	best, bestM := "", uint64(1<<63)
	for _, ln := range strings.Split(text, "\n") {
		f := strings.Fields(ln)
		if len(f) < 10 || f[0] != strings.Repeat("0", 32) || f[1] != "00" {
			continue
		}
		m, err1 := strconv.ParseUint(f[5], 16, 64)
		fl, err2 := strconv.ParseUint(f[8], 16, 32)
		if err1 != nil || err2 != nil || fl&0x0200 != 0 || f[9] == "lo" { // RTF_REJECT
			continue
		}
		if m < bestM {
			best, bestM = f[9], m
		}
	}
	return best
}

// auto6Candidate: a usable global address for a tunnel source.
func auto6Candidate(a if6Addr) bool {
	ip := a.IP
	if len(ip) != net.IPv6len || ip.To4() != nil || !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return false
	}
	return a.Flags&(ifaFTemporary|ifaFDadFailed|ifaFDeprecated|ifaFTentative) == 0
}

// pickAuto6 chooses the address for one spec (see the file comment). prev is
// the current choice ("" = none).
func pickAuto6(addrs []if6Addr, defIface, wantIface, prev string) string {
	var c []if6Addr
	for _, a := range addrs {
		if auto6Candidate(a) && (wantIface == "" || a.Iface == wantIface) {
			c = append(c, a)
		}
	}
	if len(c) == 0 {
		return ""
	}
	tier := func(a if6Addr) int {
		if a.Iface == defIface {
			return 0
		}
		return 1
	}
	sort.SliceStable(c, func(i, j int) bool {
		if ti, tj := tier(c[i]), tier(c[j]); ti != tj {
			return ti < tj
		}
		if c[i].Iface != c[j].Iface {
			return c[i].Iface < c[j].Iface
		}
		return bytes.Compare(c[i].IP, c[j].IP) < 0
	})
	for _, a := range c {
		if tier(a) != tier(c[0]) {
			break
		}
		if a.IP.String() == prev {
			return prev
		}
	}
	return c[0].IP.String()
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
	r, _ := os.ReadFile("/proc/net/ipv6_route")
	return parseIfInet6(string(a)), parseDefault6Iface(string(r)), nil
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
	specs := lbLocalAutos(raw)
	if len(specs) == 0 {
		return nil, nil
	}
	addrs, def, err := auto6Source()
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, s := range specs {
		ifc, _ := lbAutoSpec(s)
		ip := pickAuto6(addrs, def, ifc, prev[s])
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
	return out, nil
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
	lbAutoMu       sync.Mutex
	lbAutoStartRaw *lbConfig
	lbAutoStartRes map[string]string
)

// lbResolveStart resolves the auto specs once at start (before the paths and
// listeners are built) and returns the config to run with.
func lbResolveStart(fc fileConfig, logf func(string, ...any)) fileConfig {
	if len(lbLocalAutos(fc)) == 0 {
		return fc
	}
	res, err := resolveAutos(fc, nil)
	if err != nil {
		logf("lb: auto6: cannot read this server's IPv6 addresses: %v — the auto6 path is off until they can be read", err)
	} else {
		logf("lb: auto6: %s", auto6Words(res))
	}
	lbAutoMu.Lock()
	lbAutoStartRaw, lbAutoStartRes = fc.LB, res
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
	res, err := resolveAutos(raw, rt.auto)
	if err != nil {
		if now.Sub(rt.autoLogAt) >= auto6LogEvery {
			rt.logf("lb: auto6: cannot read this server's IPv6 addresses: %v", err)
			rt.autoLogAt = now
		}
		return false
	}
	if mapsEqualStr(res, rt.auto) {
		if allEmpty(res) && now.Sub(rt.autoLogAt) >= auto6LogEvery {
			rt.logf("lb: auto6: still no global IPv6 on this server — the v6 path stays off")
			rt.autoLogAt = now
		}
		return false
	}
	old := rt.auto
	rt.auto = res
	rt.autoLogAt = now
	next := applyAutos(raw, res)
	if allEmpty(res) {
		rt.logf("lb: auto6: this server's IPv6 is gone: %s -> %s — the v6 path is off until one appears (checked every %s, no restart)", fmtAutos(old), fmtAutos(res), fmtDurShort(auto6Every))
	} else {
		rt.logf("lb: auto6: this server's IPv6 changed: %s -> %s — moving the path, no restart", fmtAutos(old), fmtAutos(res))
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
	ip := pickAuto6(addrs, def, ifc, "")
	if ip == "" {
		where := "this server"
		if ifc != "" {
			where = "interface " + ifc
		}
		return fmt.Sprintf("%s: no global IPv6 on %s now — the v6 path stays off until one appears (re-checked every %s while running)", spec, where, fmtDurShort(auto6Every))
	}
	return fmt.Sprintf("%s: resolves to %s now (re-resolved every %s while running; a new address moves the path without a restart)", spec, ip, fmtDurShort(auto6Every))
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
