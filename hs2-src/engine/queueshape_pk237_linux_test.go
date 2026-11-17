package engine

import (
	"net"
	"net/netip"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// pk237 (HS2_QUEUE_CTL_SHAPER=ifb): the interface's root qdisc is never
// touched; parked = no redirect filters; a cap steers the links' packets
// (and only theirs) to the ifb's class; parking again drops the steering
// after the leaf drained; a second process shares the ifb; removal takes the
// ifb, leaves the clsact.
func TestQShaperIfbOnKernel(t *testing.T) {
	if os.Getenv(qNetnsEnv) != "1" {
		if os.Geteuid() != 0 {
			t.Skip("needs root (a private network namespace)")
		}
		unshare, err := exec.LookPath("unshare")
		if err != nil {
			t.Skip("no unshare")
		}
		cmd := exec.Command(unshare, "-n", "--", "sh", "-c",
			`ip link add d0 type dummy && ip addr add 10.9.0.1/24 dev d0 && ip link set d0 up && exec "$0" -test.run '^TestQShaperIfbOnKernel$' -test.v`, os.Args[0])
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
	defer func(k bool, m string) { queueCtlKeep, qShaperMode = k, m }(queueCtlKeep, qShaperMode)
	queueCtlKeep, qShaperMode = true, "ifb"
	qLockDir = t.TempDir()
	tcs := func(a ...string) string {
		out, _ := exec.Command("tc", a...).CombinedOutput()
		return string(out)
	}
	root0 := tcs("qdisc", "show", "dev", "d0", "root")
	ifb := qIfbName("d0")
	var logs []string
	lf := func(f string, a ...any) { logs = append(logs, f) }
	s := newQShaper(lf)
	peer := netip.MustParseAddrPort("10.9.0.2:443")
	if !s.park(qDsts(peer)) || !s.ifb || s.qdev != ifb {
		t.Fatalf("park: ifb %v qdev %q %q", s.ifb, s.qdev, logs)
	}
	egress := func() string { return tcs("filter", "show", "dev", "d0", "egress") }
	if r := tcs("qdisc", "show", "dev", "d0", "root"); r != root0 {
		t.Fatalf("root qdisc touched: %q -> %q", root0, r)
	}
	if q := tcs("qdisc", "show", "dev", "d0"); !strings.Contains(q, "clsact") {
		t.Fatalf("no clsact: %s", q)
	}
	if q := tcs("qdisc", "show", "dev", ifb); !strings.Contains(q, "htb "+qHandle+":") {
		t.Fatalf("no htb on %s: %s", ifb, q)
	}
	if f := egress(); strings.Contains(f, "mirred") {
		t.Fatalf("parked but steering: %s", f)
	}
	// a cap: the links' packets go to the ifb class
	if !s.apply(12.5e6/qWire, qDsts(peer)) {
		t.Fatalf("apply: %q", logs)
	}
	if f := egress(); !strings.Contains(f, "mirred") || !strings.Contains(f, ifb) || !strings.Contains(f, "000001bb/0000ffff") {
		t.Fatalf("capped, no steering: %s", f)
	}
	if c := tcs("class", "show", "dev", ifb); !strings.Contains(c, "100Mbit") {
		t.Fatalf("cap not on the ifb class: %s", c)
	}
	sent := func(port int) uint64 {
		u, err := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(netip.AddrPortFrom(peer.Addr(), uint16(port))))
		if err != nil {
			t.Fatal(err)
		}
		before := qClassSent(tcs("-s", "class", "show", "dev", ifb), s.minor)
		for i := 0; i < 20; i++ {
			u.Write(make([]byte, 100))
		}
		u.Close()
		return qClassSent(tcs("-s", "class", "show", "dev", ifb), s.minor) - before
	}
	if n := sent(443); n < 20 {
		t.Fatalf("the link's packets did not reach the class: %d", n)
	}
	if n := sent(444); n != 0 {
		t.Fatalf("other traffic went through the class: %d", n)
	}
	if r := tcs("qdisc", "show", "dev", "d0", "root"); r != root0 {
		t.Fatalf("root qdisc touched under a cap: %q -> %q", root0, r)
	}
	// a second process on the same interface
	s2 := newQShaper(lf)
	peer2 := netip.MustParseAddrPort("10.9.0.2:8443")
	if !s2.apply(3e6/qWire, qDsts(peer2)) || s2.minor == s.minor || s2.qdev != ifb {
		t.Fatalf("second: minor %d/%d qdev %q %q", s.minor, s2.minor, s2.qdev, logs)
	}
	if f := egress(); !strings.Contains(f, "000020fb/0000ffff") || !strings.Contains(f, "000001bb/0000ffff") {
		t.Fatalf("both sets of filters wanted: %s", f)
	}
	// park: the first park tick keeps the redirect (drain), the next drops it
	if !s.park(nil) || !s.steer {
		t.Fatalf("first park tick dropped the steering at once")
	}
	if !s.park(nil) || s.steer || strings.Contains(egress(), "000001bb/0000ffff") {
		t.Fatalf("second park tick kept the steering: %s", egress())
	}
	if !strings.Contains(egress(), "000020fb/0000ffff") {
		t.Fatalf("the other process's filters went: %s", egress())
	}
	if n := sent(443); n != 0 {
		t.Fatalf("parked, yet the link's packets went through the ifb: %d", n)
	}
	// a cap again steers again, with the peers it learned while parked
	if !s.apply(12.5e6/qWire, qDsts(peer)) || !strings.Contains(egress(), "000001bb/0000ffff") {
		t.Fatalf("re-cap did not steer: %s", egress())
	}
	s.remove()
	if f := egress(); strings.Contains(f, "000001bb/0000ffff") {
		t.Fatalf("filters left behind: %s", f)
	}
	if exec.Command("ip", "link", "show", "dev", ifb).Run() != nil {
		t.Fatalf("the first's removal took the shared ifb")
	}
	s2.remove()
	if exec.Command("ip", "link", "show", "dev", ifb).Run() == nil {
		t.Fatalf("ifb left after the last class")
	}
	if q := tcs("qdisc", "show", "dev", "d0"); !strings.Contains(q, "clsact") || tcs("qdisc", "show", "dev", "d0", "root") != root0 {
		t.Fatalf("after removal: want clsact kept and the root as it was: %s", q)
	}
	if f := egress(); strings.Contains(f, "mirred") {
		t.Fatalf("filters left: %s", f)
	}
	// a restart: no graft on d0 (clsact already there), the ifb comes back
	s3 := newQShaper(lf)
	if !s3.park(qDsts(peer)) || !s3.ifb {
		t.Fatalf("restart: %q", logs)
	}
	s3.remove()
}

// qClassSent: the packets class 1fe1:<minor> sent (tc -s class show).
func qClassSent(out string, minor int) uint64 {
	re := regexp.MustCompile(`class htb ` + qClassID(minor) + ` [^\n]*\n\s*Sent \d+ bytes (\d+) pkt`)
	m := re.FindStringSubmatch(out)
	if m == nil {
		return 0
	}
	n, _ := strconv.ParseUint(m[1], 10, 64)
	return n
}
