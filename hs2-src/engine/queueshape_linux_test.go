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
	var logs []string
	s := newQShaper(func(f string, a ...any) { logs = append(logs, f) })
	peer := netip.MustParseAddr("10.9.0.2")
	if !s.apply(12.5e6/qWire, []netip.Addr{peer}) { // 100 Mbit/s on the wire
		t.Fatalf("apply refused: %q", logs)
	}
	q, c, f := show("qdisc"), show("class"), show("filter")
	if !strings.Contains(q, "htb "+qHandle+":") || !strings.Contains(c, "100Mbit") || !strings.Contains(f, "flowid "+qHandle+":2") {
		t.Fatalf("shaper not in place:\nqdisc %s\nclass %s\nfilter %s", q, c, f)
	}
	if !strings.Contains(q, "cake") && !strings.Contains(q, "fq_codel") {
		t.Fatalf("no flow-queueing leaf: %s", q)
	}
	s.apply(6.25e6/qWire, []netip.Addr{peer})
	if c := show("class"); !strings.Contains(c, "50Mbit") {
		t.Fatalf("cap not changed to 50 Mbit: %s", c)
	}
	s.remove()
	if q := show("qdisc"); strings.Contains(q, "htb") {
		t.Fatalf("shaper left behind: %s", q)
	}

	// An operator's own root qdisc is left alone.
	if out, err := exec.Command("tc", "qdisc", "add", "dev", "d0", "root", "tbf", "rate", "10mbit", "burst", "32k", "latency", "50ms").CombinedOutput(); err != nil {
		t.Skipf("tbf: %s", out)
	}
	s2 := newQShaper(func(string, ...any) {})
	if s2.apply(12.5e6, []netip.Addr{peer}) {
		t.Fatal("apply replaced a root qdisc set up by hand")
	}
	if q := show("qdisc"); !strings.Contains(q, "tbf") {
		t.Fatalf("the operator's qdisc is gone: %s", q)
	}
}
