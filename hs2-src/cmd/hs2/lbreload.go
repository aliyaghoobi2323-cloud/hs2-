package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/engine"
)

// Multipath LB, stage 6: the listening side announces its own addresses
// (engine/lb_advert.go) and SIGHUP reloads the lb section without a restart.
//
//   - Listening side: listens on its own list and announces it; a reload adds
//     a listener for a new address, closes the one of a removed address (its
//     established links are not touched) and re-announces.
//   - Dialing side: its remote addresses come from the peer's announcement
//     when there is one (else its local list, stage 5); its source addresses
//     always from its local list. A reload (or a new announcement) replaces
//     the paths in place: no link is closed — the idle-link rebalance and the
//     normal growth/replacement move the shares.
//
// Only the lb section is taken from the file on a reload; turning lb on or
// off, or changing anything else, still needs a restart.

// lbRuntime is the running tunnel's lb state (one tunnel per process).
type lbRuntime struct {
	mu       sync.Mutex
	fc       fileConfig
	lb       *engine.LB         // dialing side
	ln       engine.ListenerSet // listening side
	adv      *engine.LBAdvert
	peer     []engine.LBAdvertIP // the peer's latest valid list (dialing side)
	loadedAt time.Time
	note     string // the local remote list differs from the announced one
	logf     func(string, ...any)
}

var (
	lbRunMu sync.Mutex
	lbRun   *lbRuntime
)

func curLB() *lbRuntime {
	lbRunMu.Lock()
	defer lbRunMu.Unlock()
	return lbRun
}

// lbCur is the lb config in effect now (the reloaded one), for lookups made
// at dial time such as a path's SNI.
func lbCur(fc fileConfig) fileConfig {
	if rt := curLB(); rt != nil {
		rt.mu.Lock()
		defer rt.mu.Unlock()
		return rt.fc
	}
	return fc
}

// lbOwnAdvert is the listening side's own list as announced: every address
// with its weight, the primary included. nil on the dialing side or when the
// primary is a wildcard listen (nothing exact to announce: the peer keeps its
// local list).
func lbOwnAdvert(fc fileConfig) []engine.LBAdvertIP {
	if !lbOn(fc) || dialing(fc) {
		return nil
	}
	local, _, lp, _ := lbLists(fc)
	if lp == "" {
		return nil
	}
	ips, w, err := lbSideWeights("local", local, lp, true)
	if err != nil {
		return nil
	}
	out := make([]engine.LBAdvertIP, 0, len(ips))
	for i, ip := range ips {
		out = append(out, engine.LBAdvertIP{IP: ip, Weight: w[i], Front: lbAnnouncedFront(local, ip)})
	}
	if engine.ValidateLBAdvert(out) != nil {
		return nil
	}
	return out
}

// lbPathsAdvert builds the dialing side's paths with the remote addresses
// from adv (nil: the local list). The remote primary (addr) always stays: if
// adv lacks it, it joins with an equal share.
func lbPathsAdvert(fc fileConfig, adv []engine.LBAdvertIP) ([]engine.LBPath, error) {
	if adv == nil {
		ps, err := lbPaths(fc)
		for i := range ps {
			ps[i].Source = "local"
		}
		return ps, err
	}
	if err := engine.ValidateLBAdvert(adv); err != nil {
		return nil, err
	}
	local, remote, lp, rp := lbLists(fc)
	_, port, err := net.SplitHostPort(fc.Addr)
	if err != nil {
		return nil, fmt.Errorf("addr %q: %v", fc.Addr, err)
	}
	lips, lw, err := lbSideWeights("local", local, lp, true)
	if err != nil {
		return nil, err
	}
	var rl []lbIP
	listed := false
	for _, a := range adv {
		e := lbIP{IP: a.IP, Weight: a.Weight, Front: a.Front}
		for _, x := range remote { // keep this side's per-address sni
			if ip := net.ParseIP(x.IP); ip != nil && ip.String() == a.IP {
				e.SNI = x.SNI
				// Precedence (phase 3): this side's explicit front > the
				// announced one > (later, pathFront) the tunnel's.
				if !x.Front.IsDefault() {
					e.Front = x.Front
				}
			}
		}
		if a.IP == rp {
			listed = true
		}
		rl = append(rl, e)
	}
	rips, rw, err := lbSideWeights("remote", rl, rp, listed)
	if err != nil {
		return nil, err
	}
	var out []engine.LBPath
	for i, l := range lips {
		for j, r := range rips {
			if r == "" || !lbSameFamily(l, r) {
				continue
			}
			src := "advert"
			if r == rp && !listed {
				src = "local"
			}
			out = append(out, engine.LBPath{Local: l, Remote: r, Addr: net.JoinHostPort(r, port),
				Weight: lw[i] * rw[j], Primary: l == lp && r == rp, Source: src, Front: lbFrontOf(rl, r)})
		}
	}
	return out, nil
}

// lbAdvertNote compares this side's local remote list with the announced one;
// "" when the local list is empty or the same.
func lbAdvertNote(fc fileConfig, adv []engine.LBAdvertIP) string {
	_, remote, _, rp := lbLists(fc)
	if len(remote) == 0 || adv == nil {
		return ""
	}
	ips, w, err := lbSideWeights("remote", remote, rp, false)
	if err != nil {
		return ""
	}
	a := map[string]int{}
	for i, ip := range ips {
		if ip != "" {
			a[ip] = w[i]
		}
	}
	b := map[string]int{}
	for _, e := range adv {
		b[e.IP] = e.Weight
	}
	if reflect.DeepEqual(a, b) {
		return ""
	}
	fmtl := func(m map[string]int) string {
		var s []string
		for k, v := range m {
			s = append(s, fmt.Sprintf("%s %d", k, v))
		}
		sort.Strings(s)
		return strings.Join(s, ", ")
	}
	return fmt.Sprintf("the other server announces its addresses as [%s]; this config lists [%s] — the announcement is used (this side's list for the other server is ignored)", fmtl(b), fmtl(a))
}

// setupLB registers the running tunnel's lb parts and returns its advert.
// lb: the dialing side's balancer (nil on the listening side); ln: the
// listening side's listener (nil on the dialing side).
func setupLB(fc fileConfig, lb *engine.LB, ln net.Listener, logf func(string, ...any)) *engine.LBAdvert {
	if !lbOn(fc) {
		return nil
	}
	rt := &lbRuntime{fc: fc, lb: lb, loadedAt: time.Now(), logf: logf}
	if s, ok := ln.(engine.ListenerSet); ok {
		rt.ln = s
	}
	var onPeer func([]engine.LBAdvertIP)
	if lb != nil {
		onPeer = rt.applyAdvert
	}
	rt.adv = engine.NewLBAdvert(lbOwnAdvert(fc), onPeer, logf)
	lbRunMu.Lock()
	lbRun = rt
	lbRunMu.Unlock()
	return rt.adv
}

// applyAdvert takes the peer's new list (dialing side).
func (rt *lbRuntime) applyAdvert(adv []engine.LBAdvertIP) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.peer = append([]engine.LBAdvertIP(nil), adv...)
	rt.rebuildLocked("the other server's announcement")
}

// rebuildLocked recomputes the dialing side's paths from the current config
// and announcement, and swaps them in without touching a link.
func (rt *lbRuntime) rebuildLocked(why string) {
	if rt.lb == nil {
		return
	}
	paths, err := lbPathsAdvert(rt.fc, rt.peer)
	if err != nil && rt.peer != nil {
		rt.logf("lb: the other server's address list does not fit this config (%v) — using the local list", err)
		paths, err = lbPathsAdvert(rt.fc, nil)
	}
	if err != nil {
		rt.logf("lb: %s: %v — keeping the current paths", why, err)
		return
	}
	if err := rt.lb.SetPaths(paths); err != nil {
		rt.logf("lb: %s: %v — keeping the current paths", why, err)
		return
	}
	rt.lb.SetAuto(lbModeName(rt.fc) == "auto")
	if n := lbAdvertNote(rt.fc, rt.peer); n != rt.note {
		rt.note = n
		if n != "" {
			rt.logf("lb: warning: %s", n)
		}
	}
	rt.logf("lb: paths updated (%s), no link closed: %s", why, lbSummary(paths))
}

// reloadLB re-reads the lb section of the config file (SIGHUP).
func reloadLB(why string) {
	rt := curLB()
	if rt == nil || configPath == "" {
		return
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		rt.logf("lb: reload (%s): %v", why, err)
		return
	}
	var nf fileConfig
	if err := json.Unmarshal(raw, &nf); err != nil {
		rt.logf("lb: reload (%s): config: %v — keeping the current lb settings", why, err)
		return
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if !lbOn(nf) {
		rt.logf("lb: reload (%s): lb is off in the file — turning it off needs a restart; keeping the current paths", why)
		return
	}
	next := rt.fc
	next.LB = nf.LB
	if err := validateLB(next); err != nil {
		rt.logf("lb: reload (%s): %v — keeping the current lb settings", why, err)
		return
	}
	if reflect.DeepEqual(next.LB, rt.fc.LB) {
		rt.logf("lb: reload (%s): lb section unchanged", why)
		rt.loadedAt = time.Now()
		return
	}
	rt.fc = next
	rt.loadedAt = time.Now()
	rt.rebuildLocked("reload, " + why)
	if rt.ln != nil {
		rt.syncListenersLocked()
	}
	if m := curFrontMux.Load(); m != nil { // per-IP fronts: new links only
		m.rebuild(next)
	}
	if rt.adv != nil {
		rt.adv.SetMine(lbOwnAdvert(next))
	}
}

// syncListenersLocked opens a listener for every address added to this
// side's list and closes the one of every address removed (never addr's).
func (rt *lbRuntime) syncListenersLocked() {
	want := map[string]bool{}
	for _, ip := range lbListenIPs(rt.fc) {
		want[ip] = true
	}
	host, port, _ := net.SplitHostPort(rt.fc.Addr)
	prim := ""
	if ip := net.ParseIP(host); ip != nil {
		prim = ip.String()
	}
	have := map[string]bool{}
	for _, ip := range rt.ln.ListenIPs() {
		have[ip] = true
		if ip != prim && !want[ip] {
			rt.ln.RemoveListener(ip)
			rt.logf("lb: stopped listening on %s (removed from the list; its links end with their users)", ip)
		}
	}
	for ip := range want {
		if have[ip] {
			continue
		}
		x, err := engine.ListenReuse(net.JoinHostPort(ip, port))
		if err != nil {
			rt.logf("lb: listen on %s: %v", ip, err)
			continue
		}
		rt.ln.AddListener(x)
		rt.logf("lb: now also listening on %s (port %s)", ip, port)
	}
}

// lbStatus is the lb part of the status file.
type lbStatus struct {
	Reload     bool   `json:"lb_reload,omitempty"`      // this hs2 reloads lb on SIGHUP
	Source     string `json:"lb_source,omitempty"`      // advert | local | announces
	LoadedAt   int64  `json:"lb_loaded_at,omitempty"`   // last (re)load, unix seconds
	Note       string `json:"lb_note,omitempty"`        // announced list differs from the local one
	PeerAdvert *bool  `json:"lb_peer_advert,omitempty"` // the peer runs stage 6 (nil: not known yet)
	// Live advert state: the last exchange with the other server (unix s), this
	// side's announced list and the other side's (the effective lists).
	PeerAt   int64               `json:"lb_peer_at,omitempty"`
	Mine     []engine.LBAdvertIP `json:"lb_mine,omitempty"`
	PeerList []engine.LBAdvertIP `json:"lb_peer_list,omitempty"`
	Rejected string              `json:"lb_rejected,omitempty"`
}

func lbStatusNow() lbStatus {
	rt := curLB()
	if rt == nil {
		return lbStatus{}
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	st := rt.adv.State()
	s := lbStatus{Reload: true, LoadedAt: rt.loadedAt.Unix(), Note: rt.note, Rejected: st.Rejected, PeerList: st.Peer}
	if !st.LastAt.IsZero() {
		s.PeerAt = st.LastAt.Unix()
	}
	if rt.adv != nil {
		s.Mine = rt.adv.Mine()
	}
	switch {
	case rt.lb == nil:
		s.Source = "announces"
	case rt.peer != nil:
		s.Source = "advert"
	default:
		s.Source = "local"
	}
	if st.PeerHas {
		t := true
		s.PeerAdvert = &t
	}
	return s
}

// lbStatusLine is the status text line about the lb source.
func lbStatusLine(s lbStatus) string {
	if !s.Reload {
		return ""
	}
	src := map[string]string{
		"advert":    "the other server's announcement",
		"local":     "this config (the other server announces nothing)",
		"announces": "this config — announced to the other server",
	}[s.Source]
	line := "addresses from " + src
	if s.LoadedAt > 0 {
		line += " · loaded " + time.Unix(s.LoadedAt, 0).Format("2006-01-02 15:04:05")
	}
	if s.PeerAdvert == nil {
		line += " · other server: not known yet"
	} else {
		line += " · other server reads announcements"
	}
	return line
}

// checkLBAdvert (doctor): the running tunnel's announced list differs from
// this config's list for the other server, or an announcement was rejected.
func checkLBAdvert(d *doctorReport, cfgPath string) {
	b, err := os.ReadFile(statusPath(cfgPath))
	if err != nil {
		return
	}
	var ls liveStatus
	if json.Unmarshal(b, &ls) != nil || time.Now().Unix()-ls.Updated > 6 {
		return
	}
	if ls.lbStatus.Note != "" {
		d.warn("lb addresses", ls.lbStatus.Note)
	}
	if ls.lbStatus.Rejected != "" {
		d.warn("lb addresses", "the other server's announcement was rejected: "+ls.lbStatus.Rejected)
	}
}
