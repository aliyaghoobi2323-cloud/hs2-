//go:build linux

package engine

import (
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"testing"
)

const qNetnsEnv = "HS2_QSHAPE_NETNS"

// The shaper on a real kernel, in a private network namespace (as root;
// re-executed under unshare -n): it installs on the route's interface, sets
// and changes the cap, filters the edge's IP, comes off leaving the default
// root qdisc — and never replaces a root qdisc set up by hand.
func TestQShaperOnKernel(t *testing.T) {
	if os.Getenv(qNetnsEnv) != "1" {
		if os.Geteuid() != 0 {
			t.Skip("needs root (a private network namespace)")
		}
		unshare, err := exec.LookPath("unshare")
		if err != nil {
			t.Skip("no unshare")
		}
		cmd := exec.Command(unshare, "-n", "--", "sh", "-c",
			`ip link add d0 type dummy && ip addr add 10.9.0.1/24 dev d0 && ip link set d0 up && exec "$0" -test.run '^TestQShaperOnKernel$' -test.v`, os.Args[0])
		cmd.Env = append(os.Environ(), qNetnsEnv+"=1")
		out, err := cmd.CombinedOutput()
		t.Logf("in a private netns:\n%s", out)
		if err != nil {
			t.Fatalf("the test in its own netns failed: %v", err)
		}
		return
	}
	if _, err := exec.LookPath("tc"); err != nil {
		t.Skip("no tc")
	}
	show := func(what ...string) string {
		out, _ := exec.Command("tc", append(what, "show", "dev", "d0")...).CombinedOutput()
		return string(out)
	}
	qLockDir = t.TempDir()
	var logs []string
	s := newQShaper(func(f string, a ...any) { logs = append(logs, f) })
	peer := netip.MustParseAddrPort("10.9.0.2:443")
	if !s.apply(12.5e6/qWire, []netip.AddrPort{peer}) { // 100 Mbit/s on the wire
		t.Fatalf("apply refused: %q", logs)
	}
	q, c, f := show("qdisc"), show("class"), show("filter")
	if !strings.Contains(q, "htb "+qHandle+":") || !strings.Contains(c, "100Mbit") || !strings.Contains(f, "flowid "+qHandle+":2") || !strings.Contains(f, "000001bb/0000ffff") {
		t.Fatalf("shaper not in place:\nqdisc %s\nclass %s\nfilter %s", q, c, f)
	}
	if !strings.Contains(q, "cake") && !strings.Contains(q, "fq_codel") {
		t.Fatalf("no flow-queueing leaf: %s", q)
	}
	s.apply(6.25e6/qWire, []netip.AddrPort{peer})
	if c := show("class"); !strings.Contains(c, "50Mbit") {
		t.Fatalf("cap not changed to 50 Mbit: %s", c)
	}

	// A second hs2 on the same interface (hs2 and hs2-out): a class of its
	// own on the same root, its own filters; either can go first.
	s2 := newQShaper(func(f string, a ...any) { logs = append(logs, f) })
	peer2 := netip.MustParseAddrPort("10.9.0.2:8443")
	if !s2.apply(3e6/qWire, []netip.AddrPort{peer2}) {
		t.Fatalf("the second shaper was refused: %q", logs)
	}
	if s2.minor == s.minor {
		t.Fatalf("both shapers on class %d", s.minor)
	}
	if f := show("filter"); !strings.Contains(f, "flowid "+qClassID(s2.minor)) || !strings.Contains(f, "000020fb/0000ffff") {
		t.Fatalf("the second shaper's filter is missing: %s", f)
	}
	s.remove()
	if q, c := show("qdisc"), show("class"); !strings.Contains(q, "htb "+qHandle+":") || !strings.Contains(c, qClassID(s2.minor)) || strings.Contains(c, qClassID(s.minor)+" ") {
		t.Fatalf("the first's removal took the second's shaper: qdisc %s class %s", q, c)
	}
	if f := show("filter"); strings.Contains(f, "flowid "+qClassID(s.minor)) {
		t.Fatalf("the first's filters left behind: %s", f)
	}
	s.apply(6.25e6/qWire, []netip.AddrPort{peer}) // back, beside the second
	if c := show("class"); !strings.Contains(c, qClassID(s.minor)) {
		t.Fatalf("not back: %s", c)
	}
	s2.remove()
	s.remove()
	if q := show("qdisc"); strings.Contains(q, "htb") {
		t.Fatalf("shaper left behind: %s", q)
	}

	// A class left by a process that is gone (its lock free) is cleaned up.
	if !s.apply(6.25e6/qWire, []netip.AddrPort{peer}) {
		t.Fatal("apply refused")
	}
	stale := s.minor
	s.slot.Close() // as if the process died: the class stays, its lock goes
	s.slot, s.installed = nil, false
	s3 := newQShaper(func(string, ...any) {})
	if !s3.apply(6.25e6/qWire, []netip.AddrPort{peer2}) {
		t.Fatal("apply refused beside a stale class")
	}
	if c := show("class"); strings.Count(c, "class htb "+qHandle+":") != 2 || s3.minor != stale {
		t.Fatalf("the stale class %d was not cleaned up (took %d): %s", stale, s3.minor, c)
	}
	s3.remove()
	if q := show("qdisc"); strings.Contains(q, "htb") {
		t.Fatalf("shaper left behind: %s", q)
	}

	// An operator's own root qdisc is left alone.
	if out, err := exec.Command("tc", "qdisc", "add", "dev", "d0", "root", "tbf", "rate", "10mbit", "burst", "32k", "latency", "50ms").CombinedOutput(); err != nil {
		t.Skipf("tbf: %s", out)
	}
	s4 := newQShaper(func(string, ...any) {})
	if s4.apply(12.5e6, []netip.AddrPort{peer}) {
		t.Fatal("apply replaced a root qdisc set up by hand")
	}
	if q := show("qdisc"); !strings.Contains(q, "tbf") {
		t.Fatalf("the operator's qdisc is gone: %s", q)
	}
}
