//go:build linux

package engine

import (
	"fmt"
	"math"
	"net/netip"
	"os/exec"
	"slices"
	"strings"
)

// qShaper holds the tunnel's egress to the edge at the controller's cap with
// tc, on the interface the route to the edge leaves by:
//
//	root 1fe1: htb, default class 1fe1:1 (everything else: unlimited, fq_codel)
//	class 1fe1:2 at the cap → cake (flow queueing: a game's packets do not wait
//	behind a download's), filtered by the edge's IPs
//
// Installed when a cap begins, removed (tc qdisc del root: the kernel's default
// comes back) when it ends and when hs2 stops. A root qdisc an operator set up
// (anything but the kernel's defaults, or this one left by a crash) is never
// replaced: the controller is told it cannot shape and stops trying.
type qShaper struct {
	logf      func(string, ...any)
	dev       string
	peers     []netip.Addr
	rate      float64
	installed bool
	refused   bool
}

func newQShaper(logf func(string, ...any)) *qShaper { return &qShaper{logf: logf} }

const qHandle = "1fe1"

// qDefaultRoots are the root qdiscs a kernel or a distribution sets up.
var qDefaultRoots = []string{"noqueue", "pfifo_fast", "fq_codel", "fq", "mq", "pfifo", "sfq"}

func qTC(args ...string) (string, error) {
	out, err := exec.Command("tc", args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("tc %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func qRouteDev(a netip.Addr) string {
	out, err := exec.Command("ip", "route", "get", a.String()).Output()
	if err != nil {
		return ""
	}
	f := strings.Fields(string(out))
	for i := 0; i+1 < len(f); i++ {
		if f[i] == "dev" {
			return f[i+1]
		}
	}
	return ""
}

// qWire: the cap is in the payload the links deliver (TCP_INFO bytes_acked);
// tc counts whole packets, TCP/IP and Ethernet headers included — ~4.5% more
// on full-size segments. Without it every cap held the links ~4.5% under its
// own number.
const qWire = 1.045

func qBits(rate float64) string { return fmt.Sprintf("%dbit", int64(math.Round(rate*qWire*8))) }

// apply installs the shaper (once) and sets the cap and the edge's IPs; false
// when this server cannot be shaped (no tc, an operator's root qdisc).
func (s *qShaper) apply(rate float64, peers []netip.Addr) bool {
	if s.refused {
		return false
	}
	if !s.installed {
		dev := qRouteDev(peers[0])
		if dev == "" {
			return s.refuse("no route to %s found", peers[0])
		}
		out, err := qTC("qdisc", "show", "dev", dev, "root")
		if err != nil {
			return s.refuse("%v", err)
		}
		kind := ""
		if f := strings.Fields(out); len(f) > 1 {
			kind = f[1]
		}
		if !slices.Contains(qDefaultRoots, kind) && !strings.Contains(out, qHandle+":") {
			return s.refuse("%s has a root qdisc set up by hand (%s) — left as it is", dev, kind)
		}
		leaf := []string{"cake", "besteffort", "flows"}
		if _, err := qTC("qdisc", "replace", "dev", dev, "root", "handle", qHandle+":", "htb", "default", "1"); err != nil {
			return s.refuse("%v", err)
		}
		s.dev, s.installed = dev, true
		steps := [][]string{
			{"class", "replace", "dev", dev, "parent", qHandle + ":", "classid", qHandle + ":1", "htb", "rate", "40gbit"},
			{"qdisc", "replace", "dev", dev, "parent", qHandle + ":1", "fq_codel"},
			{"class", "replace", "dev", dev, "parent", qHandle + ":", "classid", qHandle + ":2", "htb", "rate", qBits(rate), "ceil", qBits(rate)},
		}
		for _, a := range steps {
			if _, err := qTC(a...); err != nil {
				s.remove()
				return s.refuse("%v", err)
			}
		}
		if _, err := qTC(append([]string{"qdisc", "replace", "dev", dev, "parent", qHandle + ":2"}, leaf...)...); err != nil {
			if _, err := qTC("qdisc", "replace", "dev", dev, "parent", qHandle+":2", "fq_codel"); err != nil { // no cake module
				s.remove()
				return s.refuse("%v", err)
			}
		}
		s.rate, s.peers = rate, nil
		s.logf("queue: shaper on %s (tc htb + cake), tunnel egress to the edge at %.1f Mbit/s", dev, mbitps(rate))
	}
	if !slices.Equal(peers, s.peers) {
		qTC("filter", "del", "dev", s.dev, "parent", qHandle+":")
		for _, p := range peers {
			args := []string{"filter", "add", "dev", s.dev, "parent", qHandle + ":", "protocol", "ip", "prio", "1", "u32", "match", "ip", "dst", p.String() + "/32", "flowid", qHandle + ":2"}
			if p.Is6() {
				args = []string{"filter", "add", "dev", s.dev, "parent", qHandle + ":", "protocol", "ipv6", "prio", "2", "u32", "match", "ip6", "dst", p.String() + "/128", "flowid", qHandle + ":2"}
			}
			if _, err := qTC(args...); err != nil {
				s.logf("queue: %v", err)
			}
		}
		s.peers = slices.Clone(peers)
	}
	if rate != s.rate {
		if _, err := qTC("class", "change", "dev", s.dev, "parent", qHandle+":", "classid", qHandle+":2", "htb", "rate", qBits(rate), "ceil", qBits(rate)); err != nil {
			s.logf("queue: %v", err)
		}
		s.rate = rate
	}
	return true
}

func (s *qShaper) refuse(f string, a ...any) bool {
	s.refused = true
	s.logf("queue: cannot shape the tunnel's egress here: "+f+" — the path's queue is left as it is", a...)
	return false
}

// remove takes the shaper away, restoring the kernel's default root qdisc.
func (s *qShaper) remove() {
	if !s.installed {
		return
	}
	if _, err := qTC("qdisc", "del", "dev", s.dev, "root"); err != nil {
		s.logf("queue: %v", err)
	} else {
		s.logf("queue: shaper on %s removed", s.dev)
	}
	s.installed, s.peers, s.rate = false, nil, 0
}
