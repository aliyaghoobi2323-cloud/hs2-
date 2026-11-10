package main

import (
	"fmt"
	"net"
	"strings"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/engine"
	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

// Multipath load balancing config (stage 1, fixed weights). See lb-spec.md.
//
//	"lb": {"enabled": true, "mode": "fixed",
//	       "iran":   [{"ip": "A", "weight": 70}, {"ip": "B", "weight": 30}],
//	       "kharej": [{"ip": "K", "weight": 100}]}
//
// Absent or enabled=false: today's behaviour and wire bytes exactly.

type lbIP struct {
	IP     string `json:"ip"`
	Weight int    `json:"weight"`
	// stage 3 (per-IP cert/SNI, certsel.go): the listening side serves
	// cert_file/key_file on this IP (and for this sni); the dialing side sends
	// sni on paths to this IP. Unset: the top-level sni/cert.
	SNI      string `json:"sni,omitempty"`
	CertFile string `json:"cert_file,omitempty"`
	KeyFile  string `json:"key_file,omitempty"`
	// front (reality phase 1, tlscarrier/front.go): the front this IP runs.
	// Absent or {"kind":"tls"}: today's front, nothing announced.
	Front *tlscarrier.FrontProfile `json:"front,omitempty"`
}

type lbConfig struct {
	Enabled bool   `json:"enabled"`
	Mode    string `json:"mode"` // "fixed" (default) | "auto" (stage 2)
	Iran    []lbIP `json:"iran"`
	Kharej  []lbIP `json:"kharej"`
}

func lbOn(fc fileConfig) bool { return fc.LB != nil && fc.LB.Enabled }

// lbSideWeights resolves one side's list with its primary IP: returns ip ->
// weight in order (primary first). prim "" = this side's primary is not a
// fixed IP (the default route / a wildcard listen): it is then an unlisted
// member "" with an equal share. strict: the weights must sum to 100.
func lbSideWeights(name string, list []lbIP, prim string, strict bool) (ips []string, w []int, err error) {
	seen := map[string]bool{}
	sum := 0
	primListed := false
	for _, e := range list {
		ip := net.ParseIP(e.IP)
		if ip == nil || ip.IsUnspecified() {
			return nil, nil, fmt.Errorf(`"lb.%s": %q is not a valid IP address`, name, e.IP)
		}
		k := ip.String()
		if seen[k] {
			return nil, nil, fmt.Errorf(`"lb.%s": %s is listed twice`, name, k)
		}
		seen[k] = true
		if e.Weight < 0 || e.Weight > 100 {
			return nil, nil, fmt.Errorf(`"lb.%s": weight %d of %s must be 0..100`, name, e.Weight, k)
		}
		if err := e.Front.Validate(); err != nil {
			return nil, nil, fmt.Errorf(`"lb.%s": %s: %v`, name, k, err)
		}
		if e.Front.NormKind() != tlscarrier.FrontTLS { // per-IP fronts: phase 3; use the tunnel's "front"
			return nil, nil, fmt.Errorf(`"lb.%s": %s: front %q per lb IP is not supported by this build yet (set it on the tunnel: "front")`, name, k, e.Front.NormKind())
		}
		sum += e.Weight
		if k == prim {
			primListed = true
		}
	}
	if !primListed {
		share := 100 / (len(list) + 1)
		ips, w = append(ips, prim), append(w, share)
		if strict && sum+share != 100 {
			p := prim
			if p == "" {
				p = "the default IP"
			}
			return nil, nil, fmt.Errorf(`"lb.%s": weights must sum to 100 with the primary (%s is not listed, so it joins with an equal share of %d): the listed weights sum to %d, need %d — or list the primary with its own weight`, name, p, share, sum, 100-share)
		}
	} else if strict && sum != 100 {
		return nil, nil, fmt.Errorf(`"lb.%s": weights sum to %d, must be 100`, name, sum)
	}
	if !strict && sum > 100 {
		return nil, nil, fmt.Errorf(`"lb.%s": weights sum to %d, more than 100`, name, sum)
	}
	for _, e := range list {
		k := net.ParseIP(e.IP).String()
		if k == prim {
			ips, w = append([]string{k}, ips...), append([]int{e.Weight}, w...)
		} else {
			ips, w = append(ips, k), append(w, e.Weight)
		}
	}
	return ips, w, nil
}

// lbPrimaries: this config's primary iran and kharej IPs ("" = not a fixed IP
// or not known on this side), and whether this side dials.
func lbPrimaries(fc fileConfig) (iran, kharej string) {
	host, _, _ := net.SplitHostPort(fc.Addr)
	if ip := net.ParseIP(host); ip != nil && !ip.IsUnspecified() {
		host = ip.String()
	} else {
		host = ""
	}
	bind := ""
	if ip := net.ParseIP(fc.BindLocalIP); ip != nil {
		bind = ip.String()
	}
	edge := fc.Mode == "dial"
	switch {
	case edge && fc.Reverse: // iran listens on addr
		return host, ""
	case edge: // iran dials addr (kharej) from bind_local_ip
		return bind, host
	case fc.Reverse: // kharej dials addr (iran) from bind_local_ip
		return host, bind
	default: // kharej listens on addr
		return "", host
	}
}

// validateLB checks the lb section; nil when absent or disabled.
func validateLB(fc fileConfig) error {
	if !lbOn(fc) {
		return nil
	}
	switch fc.LB.Mode {
	case "", "fixed":
	case "auto":
	default:
		return fmt.Errorf(`"lb.mode" %q: must be "fixed" or "auto"`, fc.LB.Mode)
	}
	if fc.Carrier != "mtcp" && fc.Carrier != "l3mtcp" && fc.Carrier != "l3" {
		return fmt.Errorf(`"lb" works with the mtcp/l3mtcp carriers only (this config: %q)`, fc.Carrier)
	}
	ir, kh := lbPrimaries(fc)
	edge := fc.Mode == "dial"
	dials := dialing(fc)
	iranLocal := edge
	// The dialing side uses both lists and must have them exact; the listening
	// side uses only its own list (its IPs to listen on).
	if _, _, err := lbSideWeights("iran", fc.LB.Iran, ir, dials || (iranLocal && ir != "")); err != nil {
		return err
	}
	if _, _, err := lbSideWeights("kharej", fc.LB.Kharej, kh, dials || (!iranLocal && kh != "")); err != nil {
		return err
	}
	return nil
}

// lbLists returns this side's (local) and the other side's (remote) lists.
func lbLists(fc fileConfig) (local, remote []lbIP, localPrim, remotePrim string) {
	ir, kh := lbPrimaries(fc)
	if fc.Mode == "dial" {
		return fc.LB.Iran, fc.LB.Kharej, ir, kh
	}
	return fc.LB.Kharej, fc.LB.Iran, kh, ir
}

// lbPaths builds the dialing side's paths: every local × remote pair, weight =
// local% × remote%, primary = (bind_local_ip, addr).
func lbPaths(fc fileConfig) ([]engine.LBPath, error) {
	if err := validateLB(fc); err != nil {
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
	rips, rw, err := lbSideWeights("remote", remote, rp, true)
	if err != nil {
		return nil, err
	}
	var out []engine.LBPath
	for i, l := range lips {
		for j, r := range rips {
			if r == "" || !lbSameFamily(l, r) { // r "": cannot happen on the dialing side (addr is an IP)
				continue
			}
			out = append(out, engine.LBPath{Local: l, Remote: r, Addr: net.JoinHostPort(r, port),
				Weight: lw[i] * rw[j], Primary: l == lp && r == rp, Front: lbFrontOf(remote, r)})
		}
	}
	return out, nil
}

// lbListenIPs: the extra IPs the listening side listens on (its own list
// minus addr's IP); none when addr is a wildcard that covers them. An IPv4
// wildcard (0.0.0.0) does not cover IPv6 addresses: those are listened on
// one by one ([ip]:port).
func lbListenIPs(fc fileConfig) []string {
	if !lbOn(fc) {
		return nil
	}
	host, _, _ := net.SplitHostPort(fc.Addr)
	hip := net.ParseIP(host)
	if hip == nil && host != "" {
		return nil
	}
	v4wild := host == "" || hip.IsUnspecified() && hip.To4() != nil
	if host == "" || hip.IsUnspecified() && !v4wild {
		return nil // "" or [::]: every address, both families
	}
	local, _, _, _ := lbLists(fc)
	var out []string
	for _, e := range local {
		ip := net.ParseIP(e.IP)
		if ip == nil || ip.Equal(hip) || v4wild && ip.To4() != nil {
			continue
		}
		out = append(out, ip.String())
	}
	return out
}

// lbSummary is the one-line description for logs and doctor.
func lbSummary(paths []engine.LBPath) string {
	var b strings.Builder
	tot := 0
	for _, p := range paths {
		tot += p.Weight
	}
	for i, p := range paths {
		if i > 0 {
			b.WriteString(", ")
		}
		pct := 0.0
		if tot > 0 {
			pct = 100 * float64(p.Weight) / float64(tot)
		}
		fmt.Fprintf(&b, "%s %.0f%%", p.Name(), pct)
		if p.Primary {
			b.WriteString(" (primary)")
		}
	}
	return b.String()
}

// lbModeName is the lb mode for messages ("fixed" when unset).
func lbModeName(fc fileConfig) string {
	if fc.LB != nil && fc.LB.Mode == "auto" {
		return "auto"
	}
	return "fixed"
}

// lbSameFamily: a path pairs a local and a remote IP of the same family
// (IPv4 with IPv4, IPv6 with IPv6). Local "" (the default route) pairs with
// either: the kernel picks a source of the destination's family.
func lbSameFamily(local, remote string) bool {
	if local == "" {
		return true
	}
	l, r := net.ParseIP(local), net.ParseIP(remote)
	if l == nil || r == nil {
		return false
	}
	return (l.To4() != nil) == (r.To4() != nil)
}
