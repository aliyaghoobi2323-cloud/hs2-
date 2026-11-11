package main

// `hs2 config validate`: the full `hs2 check` plus host cross-checks that only
// make sense on the server the config is about to run on.

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// validateHost is the injectable view of the host (tests replace it).
type validateHost struct {
	localIP   func(net.IP) bool
	listening func() []sockListen
	otherCfgs func(self string) map[string][]byte // path -> raw config of the other tunnels
}

var realHost = validateHost{
	localIP:   isLocalIP,
	listening: listeningSockets,
	otherCfgs: func(self string) map[string][]byte {
		out := map[string][]byte{}
		ms, _ := filepath.Glob(filepath.Join(hs2CfgDir, "*.json"))
		for _, m := range ms {
			if sameFile(m, self) {
				continue
			}
			if b, err := os.ReadFile(m); err == nil {
				out[m] = b
			}
		}
		return out
	},
}

func sameFile(a, b string) bool {
	if a == b {
		return true
	}
	sa, ea := os.Stat(a)
	sb, eb := os.Stat(b)
	return ea == nil && eb == nil && os.SameFile(sa, sb)
}

// hostPorts is every TCP port this config makes hs2 LISTEN on.
func hostPorts(fc fileConfig) []int {
	var ps []int
	listens := (fc.Mode == "listen") != fc.Reverse // reverse flips who listens
	if listens {
		if _, p, err := net.SplitHostPort(fc.Addr); err == nil {
			if n, err := strconv.Atoi(p); err == nil && n > 0 {
				ps = append(ps, n)
			}
		}
	}
	if isEdge(fc) {
		ps = append(ps, edgePorts(fc)...)
	}
	return ps
}

// isEdge: the Iran side (user ports, the edge). It is mode "dial" in both
// directions — reverse only flips who opens the tunnel connection (the edge
// then listens on addr and the exit dials it), as runCmd/applyDefaults and
// lbPrimaries read it. (Until M4 this was "dial XOR reverse", which took the
// kharej side of a reverse tunnel for the edge: its own lb list was the Iran
// list, so `hs2 tunnel join --update` with lb failed there.)
func isEdge(fc fileConfig) bool { return fc.Mode == "dial" }

// crossCheck runs the host checks; cur is the unit's current config (nil when
// new): ports it already holds are expected to be in use by itself.
func crossCheck(raw, cur []byte, self string, h validateHost) (errs, warns []string) {
	var fc fileConfig
	if err := json.Unmarshal(raw, &fc); err != nil {
		return []string{"not valid JSON: " + err.Error()}, nil
	}
	var curFC fileConfig
	if cur != nil {
		_ = json.Unmarshal(cur, &curFC)
	}
	mine := hostPorts(fc)

	// 1. port conflicts with the other tunnels on this host
	for path, b := range h.otherCfgs(self) {
		var o fileConfig
		if json.Unmarshal(b, &o) != nil {
			continue
		}
		for _, p := range hostPorts(o) {
			if contains(mine, p) {
				errs = append(errs, fmt.Sprintf("port %d is also used by the tunnel in %s", p, path))
			}
		}
	}
	// 2. ports NEW in this config that something already listens on
	if ls := h.listening(); ls != nil {
		held := hostPorts(curFC)
		for _, p := range mine {
			if contains(held, p) {
				continue
			}
			for _, s := range ls {
				if !s.udp && s.port == p {
					errs = append(errs, fmt.Sprintf("port %d is already in use on this server (another service listens on it)", p))
					break
				}
			}
		}
	}
	// 3. IPs that must exist on this server's interfaces
	need := map[string]string{"bind_local_ip": fc.BindLocalIP, "user_listen_ip": fc.UserListenIP}
	if (fc.Mode == "listen") != fc.Reverse {
		if host, _, err := net.SplitHostPort(fc.Addr); err == nil {
			need["addr"] = host
		}
	}
	if lbOn(fc) {
		own := fc.LB.Kharej
		if isEdge(fc) {
			own = fc.LB.Iran
		}
		for i, e := range own {
			need[fmt.Sprintf("lb.%d", i)] = e.IP
		}
	}
	for k, v := range need {
		if v == "" {
			continue
		}
		ip := net.ParseIP(v)
		if ip == nil || ip.IsUnspecified() {
			continue
		}
		if !h.localIP(ip) {
			errs = append(errs, fmt.Sprintf("%s: %s is not an address of this server", k, v))
		}
	}
	// 4. cert/key files readable
	for k, v := range map[string]string{"cert_file": fc.CertFile, "key_file": fc.KeyFile} {
		if v == "" {
			continue
		}
		f, err := os.Open(v)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: cannot read %s: %v", k, v, err))
			continue
		}
		f.Close()
	}
	// 5. lb weights: each listed side must sum to 100 when weights are given
	if lbOn(fc) {
		for name, list := range map[string][]lbIP{"iran": fc.LB.Iran, "kharej": fc.LB.Kharej} {
			sum, any := 0, false
			for _, e := range list {
				if e.Weight < 0 {
					errs = append(errs, fmt.Sprintf("lb.%s: %s has a negative weight", name, e.IP))
				}
				if e.Weight > 0 {
					any = true
				}
				sum += e.Weight
			}
			if any && sum != 100 {
				warns = append(warns, fmt.Sprintf("lb.%s: weights sum to %d, not 100", name, sum))
			}
		}
	}
	return errs, warns
}

// validateFull = hs2 check + crossCheck.
func validateFull(raw, cur []byte, self string, h validateHost) (errs, warns []string) {
	errs, warns = checkConfig(raw, h.localIP, time.Now())
	if len(errs) > 0 {
		return errs, warns
	}
	e2, w2 := crossCheck(raw, cur, self, h)
	return append(errs, e2...), append(warns, w2...)
}

func printValidation(errs, warns []string) {
	for _, e := range errs {
		fmt.Println("ERROR: " + e)
	}
	for _, w := range warns {
		fmt.Println("WARN:  " + w)
	}
	if len(errs) == 0 {
		fmt.Println("config OK")
	}
}
