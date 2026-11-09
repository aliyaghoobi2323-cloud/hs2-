//go:build linux

package engine

import (
	"fmt"
	"math"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

// qShaper holds the tunnel's egress at the controller's cap with tc, on the
// interface its links leave by:
//
//	root 1fe1: htb, default class 1fe1:1 (everything else: unlimited, fq_codel)
//	class 1fe1:N at the cap → cake (flow queueing: a game's packets do not wait
//	behind a download's), filtered by the peers' address AND port: only this
//	tunnel's own links, never another tunnel's or anything else to that IP
//
// Several hs2 processes on one server (hs2 and hs2-out, 2026-11-09) share the
// root: each takes a class of its own, N, held by an flock on
// <qLockDir>/hs2-qshape.<dev>.<N> while it is in place (a class whose lock
// nobody holds was left by a process that is gone, and is cleaned up), and
// every change to the tree is made under <qLockDir>/hs2-qshape.lock. The root
// goes (tc qdisc del root: the kernel's default comes back) with the last
// class. Before, the second process tried to replace the first's root
// ("Change operation not supported"), and the first's filter, by the exit's
// IP alone, held the second's traffic to that IP under a cap measured on the
// first's links only.
//
// A root qdisc an operator set up (anything but the kernel's defaults or this
// tree) is never replaced: the controller is told it cannot shape, once.
type qShaper struct {
	logf      func(string, ...any)
	want      string // the interface to shape; "": the one the route to the first peer leaves by
	dev       string
	minor     int      // this process's class, 1fe1:<minor>
	slot      *os.File // the flock that holds it
	peers     []netip.AddrPort
	rate      float64
	drop      uint64 // the leaf's drop counter at the last drops()
	installed bool
	refused   bool
}

func newQShaper(logf func(string, ...any)) *qShaper { return &qShaper{logf: logf} }

// newQShaperOn shapes dev ("": the route's interface, as newQShaper).
func newQShaperOn(dev string, logf func(string, ...any)) *qShaper {
	return &qShaper{logf: logf, want: dev}
}

const qHandle = "1fe1"

// qMaxMinor bounds the classes (hs2 processes) on one interface.
const qMaxMinor = 0xff

// qLockDir holds the shapers' locks (a var: tests point it elsewhere).
var qLockDir = "/run"

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

// qTreeLock serialises changes to the tc tree across hs2 processes; the
// returned func lets go.
func qTreeLock() func() {
	f, err := os.OpenFile(filepath.Join(qLockDir, "hs2-qshape.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return func() {}
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
	return func() { f.Close() } // closing drops the flock
}

// qTrySlot takes class minor's lock on dev if no live process holds it.
func qTrySlot(dev string, minor int) *os.File {
	f, err := os.OpenFile(filepath.Join(qLockDir, fmt.Sprintf("hs2-qshape.%s.%x", dev, minor)), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil
	}
	if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		f.Close()
		return nil
	}
	return f
}

var qClassRe = regexp.MustCompile(`class htb ` + qHandle + `:([0-9a-f]+) `)

// qClasses lists the minors of the classes under 1fe1: on dev (1 included).
func qClasses(dev string) []int {
	out, _ := qTC("class", "show", "dev", dev)
	var ms []int
	for _, m := range qClassRe.FindAllStringSubmatch(out, -1) {
		if v, err := strconv.ParseUint(m[1], 16, 16); err == nil {
			ms = append(ms, int(v))
		}
	}
	return ms
}

// qFilterPrios: a class's filters, IPv4 and IPv6 (one protocol per prio).
func qFilterPrios(minor int) (v4, v6 string) {
	return strconv.Itoa(minor), strconv.Itoa(0x1000 + minor)
}

func qClassID(minor int) string { return fmt.Sprintf("%s:%x", qHandle, minor) }

// qDropClass takes class minor's filters and class (its leaf with it) off dev.
func qDropClass(dev string, minor int) {
	v4, v6 := qFilterPrios(minor)
	qTC("filter", "del", "dev", dev, "parent", qHandle+":", "prio", v4)
	qTC("filter", "del", "dev", dev, "parent", qHandle+":", "prio", v6)
	qTC("class", "del", "dev", dev, "classid", qClassID(minor))
}

// apply installs the shaper (once) and sets the cap and the peers; false
// when this server cannot be shaped (no tc, an operator's root qdisc).
func (s *qShaper) apply(rate float64, peers []netip.AddrPort) bool {
	if s.refused {
		return false
	}
	if !s.installed {
		dev := s.want
		if dev == "" {
			dev = qRouteDev(peers[0].Addr())
		}
		if dev == "" {
			return s.refuse("no route to %s found", peers[0].Addr())
		}
		if !s.install(dev, rate) {
			return false
		}
		s.logf("queue: shaper on %s (tc htb + cake), tunnel egress to the edge at %.1f Mbit/s", dev, mbitps(rate))
	}
	if !slices.Equal(peers, s.peers) {
		unlock := qTreeLock()
		v4, v6 := qFilterPrios(s.minor)
		flow := qClassID(s.minor)
		qTC("filter", "del", "dev", s.dev, "parent", qHandle+":", "prio", v4)
		qTC("filter", "del", "dev", s.dev, "parent", qHandle+":", "prio", v6)
		for _, p := range peers {
			port := strconv.Itoa(int(p.Port()))
			args := []string{"filter", "add", "dev", s.dev, "parent", qHandle + ":", "protocol", "ip", "prio", v4, "u32",
				"match", "ip", "dst", p.Addr().String() + "/32", "match", "ip", "dport", port, "0xffff", "flowid", flow}
			if p.Addr().Is6() {
				args = []string{"filter", "add", "dev", s.dev, "parent", qHandle + ":", "protocol", "ipv6", "prio", v6, "u32",
					"match", "ip6", "dst", p.Addr().String() + "/128", "match", "ip6", "dport", port, "0xffff", "flowid", flow}
			}
			if _, err := qTC(args...); err != nil {
				s.logf("queue: %v", err)
			}
		}
		unlock()
		s.peers = slices.Clone(peers)
	}
	if rate != s.rate {
		if _, err := qTC("class", "change", "dev", s.dev, "parent", qHandle+":", "classid", qClassID(s.minor), "htb", "rate", qBits(rate), "ceil", qBits(rate)); err != nil {
			s.logf("queue: %v", err)
		}
		s.rate = rate
	}
	return true
}

// install puts this process's class (and the shared root, when it is not
// there yet) on dev, under the tree lock.
func (s *qShaper) install(dev string, rate float64) bool {
	unlock := qTreeLock()
	defer unlock()
	out, err := qTC("qdisc", "show", "dev", dev, "root")
	if err != nil {
		return s.refuse("%v", err)
	}
	f := strings.Fields(out)
	kind := ""
	if len(f) > 1 {
		kind = f[1]
	}
	ours := kind == "htb" && len(f) > 2 && f[2] == qHandle+":"
	if !ours && !slices.Contains(qDefaultRoots, kind) {
		return s.refuse("%s has a root qdisc set up by hand (%s) — left as it is", dev, kind)
	}
	if ours {
		// Classes whose process is gone (nobody holds their lock): cleaned up.
		for _, m := range qClasses(dev) {
			if m < 2 {
				continue
			}
			if l := qTrySlot(dev, m); l != nil {
				qDropClass(dev, m)
				l.Close()
			}
		}
	} else {
		if _, err := qTC("qdisc", "replace", "dev", dev, "root", "handle", qHandle+":", "htb", "default", "1"); err != nil {
			return s.refuse("%v", err)
		}
		if _, err := qTC("class", "replace", "dev", dev, "parent", qHandle+":", "classid", qHandle+":1", "htb", "rate", "40gbit"); err == nil {
			qTC("qdisc", "replace", "dev", dev, "parent", qHandle+":1", "fq_codel")
		}
	}
	for m := 2; m <= qMaxMinor && s.slot == nil; m++ {
		if l := qTrySlot(dev, m); l != nil {
			s.slot, s.minor = l, m
		}
	}
	s.dev = dev
	if s.slot == nil {
		s.dropRootIfLast()
		return s.refuse("no free class on %s", dev)
	}
	s.installed = true
	cls := qClassID(s.minor)
	if _, err := qTC("class", "replace", "dev", dev, "parent", qHandle+":", "classid", cls, "htb", "rate", qBits(rate), "ceil", qBits(rate)); err != nil {
		s.removeLocked()
		return s.refuse("%v", err)
	}
	if _, err := qTC("qdisc", "replace", "dev", dev, "parent", cls, "cake", "besteffort", "flows"); err != nil {
		if _, err := qTC("qdisc", "replace", "dev", dev, "parent", cls, "fq_codel"); err != nil { // no cake module
			s.removeLocked()
			return s.refuse("%v", err)
		}
	}
	s.rate, s.peers, s.drop = rate, nil, 0
	return true
}

func (s *qShaper) refuse(f string, a ...any) bool {
	s.refused = true
	s.logf("queue: cannot shape the tunnel's egress here: "+f+" — the path's queue is left as it is", a...)
	return false
}

var qDroppedRe = regexp.MustCompile(`\(dropped (\d+),`)

// drops is how many packets the shaper's own leaf dropped since the last call
// (0 when it is not in place): each comes back as a retransmit that the cap,
// not the path, made.
func (s *qShaper) drops() uint64 {
	if !s.installed {
		return 0
	}
	out, err := qTC("-s", "qdisc", "show", "dev", s.dev, "parent", qClassID(s.minor))
	if err != nil {
		return 0
	}
	m := qDroppedRe.FindStringSubmatch(out)
	if m == nil {
		return 0
	}
	n, _ := strconv.ParseUint(m[1], 10, 64)
	d := uint64(0)
	if n >= s.drop {
		d = n - s.drop
	}
	s.drop = n
	return d
}

// remove takes this process's class away, and the root with the last class
// (the kernel's default root qdisc comes back).
func (s *qShaper) remove() {
	if !s.installed {
		return
	}
	unlock := qTreeLock()
	defer unlock()
	s.removeLocked()
	s.logf("queue: shaper on %s removed", s.dev)
}

func (s *qShaper) removeLocked() {
	qDropClass(s.dev, s.minor)
	s.dropRootIfLast()
	if s.slot != nil {
		s.slot.Close()
	}
	s.slot, s.installed, s.peers, s.rate, s.drop = nil, false, nil, 0, 0
}

// dropRootIfLast takes the shared root off when no hs2 class is left on it.
func (s *qShaper) dropRootIfLast() {
	for _, m := range qClasses(s.dev) {
		if m >= 2 && (m != s.minor || !s.installed) {
			return
		}
	}
	if out, _ := qTC("qdisc", "show", "dev", s.dev, "root"); !strings.Contains(out, "htb "+qHandle+":") {
		return
	}
	if _, err := qTC("qdisc", "del", "dev", s.dev, "root"); err != nil {
		s.logf("queue: %v", err)
	}
}

// qLinkDev is the interface a link's packets leave by: the route to its peer
// from its own local address (an edge's lb addresses can each have a route of
// their own); "" when not known.
func qLinkDev(tc *net.TCPConn) string {
	if tc == nil {
		return ""
	}
	r, err1 := netip.ParseAddrPort(tc.RemoteAddr().String())
	l, err2 := netip.ParseAddrPort(tc.LocalAddr().String())
	if err1 != nil || err2 != nil {
		return ""
	}
	out, err := exec.Command("ip", "route", "get", r.Addr().Unmap().String(), "from", l.Addr().Unmap().String()).Output()
	if err != nil {
		return qRouteDev(r.Addr().Unmap())
	}
	f := strings.Fields(string(out))
	for i := 0; i+1 < len(f); i++ {
		if f[i] == "dev" {
			return f[i+1]
		}
	}
	return ""
}
