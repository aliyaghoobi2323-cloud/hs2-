package engine

import (
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestQReleaseAtLeast(t *testing.T) {
	for _, c := range []struct {
		rel  string
		want bool
	}{{"6.18.2-x64v3-xanmod1", true}, {"6.8.0-31-generic", true}, {"5.18.0", true}, {"5.17.15", false}, {"5.4.0-200", false}, {"4.19", false}, {"", false}} {
		if g := qReleaseAtLeast(c.rel, 5, 18); g != c.want {
			t.Errorf("%q: %v", c.rel, g)
		}
	}
	if m := qMqRootRe.FindStringSubmatch("qdisc mq 0: root"); m == nil || m[1] != "0" {
		t.Errorf("default mq root: %v", m)
	}
	if m := qMqRootRe.FindStringSubmatch("qdisc htb 1fe1: root refcnt 21 r2q 10 default 0x1"); m != nil {
		t.Errorf("htb root matched as mq")
	}
}

// pk239 (HS2_QUEUE_CTL_SHAPER=mqchild): the mq root is never touched; the
// tree goes on the last mq child once; parked = no filters; a cap moves the
// links' packets (and only theirs) onto that queue's class; parking drops
// the filters after the leaf drained; a second process shares the tree; a
// restart grafts nothing; a 1-queue device falls back to root mode.
func TestQShaperMqChildOnKernel(t *testing.T) {
	if os.Getenv(qNetnsEnv) != "1" {
		if os.Geteuid() != 0 {
			t.Skip("needs root (a private network namespace)")
		}
		unshare, err := exec.LookPath("unshare")
		if err != nil {
			t.Skip("no unshare")
		}
		cmd := exec.Command(unshare, "-n", "--", "sh", "-c",
			`ip link add v0 numtxqueues 4 numrxqueues 4 type veth peer name v1 numtxqueues 4 numrxqueues 4 && ip addr add 10.9.0.1/24 dev v0 && ip link set v1 up && ip link set v0 up && ip neigh replace 10.9.0.2 lladdr 02:00:00:00:00:02 dev v0 && tc qdisc replace dev v0 root handle 1: mq && ip link add d0 type dummy && ip addr add 10.8.0.1/24 dev d0 && ip link set d0 up && exec "$0" -test.run '^TestQShaperMqChildOnKernel$' -test.v`, os.Args[0])
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
	if !qKernelAtLeast(5, 18) {
		t.Skip("kernel < 5.18")
	}
	defer func(k bool, m string) { queueCtlKeep, qShaperMode = k, m }(queueCtlKeep, qShaperMode)
	queueCtlKeep, qShaperMode = true, "mqchild"
	qLockDir = t.TempDir()
	tcs := func(a ...string) string {
		out, _ := exec.Command("tc", a...).CombinedOutput()
		return string(out)
	}
	root0 := tcs("qdisc", "show", "dev", "v0", "root")
	var logs []string
	lf := func(f string, a ...any) { logs = append(logs, f) }
	s := newQShaper(lf)
	peer := netip.MustParseAddrPort("10.9.0.2:443")
	if !s.park(qDsts(peer)) || !s.mqc || s.mqParent != "1:4" || s.mqQueue != 3 || s.qdev != "v0" {
		t.Fatalf("park: mqc %v parent %q queue %d qdev %q %q", s.mqc, s.mqParent, s.mqQueue, s.qdev, logs)
	}
	egress := func() string { return tcs("filter", "show", "dev", "v0", "egress") }
	if r := tcs("qdisc", "show", "dev", "v0", "root"); r != root0 {
		t.Fatalf("root qdisc touched: %q -> %q", root0, r)
	}
	if q := tcs("qdisc", "show", "dev", "v0"); !strings.Contains(q, "clsact") || !strings.Contains(q, "qdisc htb 1fe1: parent 1:4") {
		t.Fatalf("want clsact and htb on 1:4: %s", q)
	}
	if f := egress(); strings.Contains(f, "skbedit") {
		t.Fatalf("parked but steering: %s", f)
	}
	if !s.apply(12.5e6/qWire, qDsts(peer)) {
		t.Fatalf("apply: %q", logs)
	}
	if f := egress(); !strings.Contains(f, "queue_mapping 3") || strings.Contains(f, "mirred") || !strings.Contains(f, "000001bb/0000ffff") {
		t.Fatalf("capped, no steering: %s", f)
	}
	if c := tcs("class", "show", "dev", "v0"); !strings.Contains(c, "100Mbit") {
		t.Fatalf("cap not on the class: %s", c)
	}
	sent := func(port int) uint64 {
		u, err := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(netip.AddrPortFrom(peer.Addr(), uint16(port))))
		if err != nil {
			t.Fatal(err)
		}
		before := qClassSent(tcs("-s", "class", "show", "dev", "v0"), s.minor)
		for i := 0; i < 20; i++ {
			u.Write(make([]byte, 100))
		}
		u.Close()
		return qClassSent(tcs("-s", "class", "show", "dev", "v0"), s.minor) - before
	}
	if n := sent(443); n < 20 {
		t.Fatalf("the link's packets did not reach the class: %d", n)
	}
	if n := sent(444); n != 0 {
		t.Fatalf("other traffic went through the class: %d", n)
	}
	if r := tcs("qdisc", "show", "dev", "v0", "root"); r != root0 {
		t.Fatalf("root qdisc touched under a cap: %q -> %q", root0, r)
	}
	s2 := newQShaper(lf)
	peer2 := netip.MustParseAddrPort("10.9.0.2:8443")
	if !s2.apply(3e6/qWire, qDsts(peer2)) || s2.minor == s.minor || !s2.mqc {
		t.Fatalf("second: minor %d/%d %q", s.minor, s2.minor, logs)
	}
	if f := egress(); !strings.Contains(f, "000020fb/0000ffff") || !strings.Contains(f, "000001bb/0000ffff") {
		t.Fatalf("both sets of filters wanted: %s", f)
	}
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
		t.Fatalf("parked, yet the link's packets went through the class: %d", n)
	}
	if !s.apply(12.5e6/qWire, qDsts(peer)) || !strings.Contains(egress(), "000001bb/0000ffff") {
		t.Fatalf("re-cap did not steer: %s", egress())
	}
	s.remove()
	s2.remove()
	if f := egress(); strings.Contains(f, "skbedit") {
		t.Fatalf("filters left: %s", f)
	}
	q := tcs("qdisc", "show", "dev", "v0")
	if !strings.Contains(q, "clsact") || !strings.Contains(q, "qdisc htb 1fe1: parent 1:4") || tcs("qdisc", "show", "dev", "v0", "root") != root0 {
		t.Fatalf("after removal: want clsact and the child tree kept, the root as it was: %s", q)
	}
	// a restart: reuses the tree (no graft: the htb's handle and the
	// default class's counters stay)
	before := tcs("qdisc", "show", "dev", "v0", "parent", qHandle+":1")
	s3 := newQShaper(lf)
	if !s3.park(qDsts(peer)) || !s3.mqc {
		t.Fatalf("restart: %q", logs)
	}
	if after := tcs("qdisc", "show", "dev", "v0", "parent", qHandle+":1"); before == "" || after != before {
		t.Fatalf("restart re-grafted (the default leaf changed): %q -> %q", before, after)
	}
	// rel69h: a stop that skipped remove (signal) leaves our class and its
	// cake; the next holder of the slot keeps that leaf (same handle) when
	// its parameters are ours
	leaf := tcs("qdisc", "show", "dev", "v0", "parent", qClassID(s3.minor))
	if !strings.Contains(leaf, "qdisc cake ") {
		t.Fatalf("restart: want a cake leaf under %s: %q", qClassID(s3.minor), leaf)
	}
	m3 := s3.minor
	s3.slot.Close() // the process is gone, its tree stays
	s5 := newQShaper(lf)
	if !s5.park(qDsts(peer)) || s5.minor != m3 {
		t.Fatalf("restart 2: minor %d want %d %q", s5.minor, m3, logs)
	}
	if after := tcs("qdisc", "show", "dev", "v0", "parent", qClassID(m3)); after != leaf {
		t.Fatalf("restart re-created an identical cake leaf: %q -> %q", leaf, after)
	}
	s5.remove()
	// a 1-queue device: root mode
	s4 := newQShaperOn("d0", lf)
	if !s4.park(qDsts(netip.MustParseAddrPort("10.8.0.2:443"))) || s4.mqc {
		t.Fatalf("1-queue dev: want root mode: mqc %v %q", s4.mqc, logs)
	}
	if r := tcs("qdisc", "show", "dev", "d0", "root"); !strings.Contains(r, "htb "+qHandle+":") {
		t.Fatalf("1-queue dev: root tree wanted: %s", r)
	}
	s4.remove()
}

// rel69e: a root-mode htb left on an mq device by a process that is gone
// (a stop by signal skips the queue loop's remove) is replaced by mq 1fe0:
// once, and the tree goes on its last child; while a root-mode process still
// holds a class there, mqchild shares the root tree instead (root mode).
func TestQShaperMqChildTakesOverRootLeftover(t *testing.T) {
	if os.Getenv(qNetnsEnv) != "1" {
		if os.Geteuid() != 0 {
			t.Skip("needs root (a private network namespace)")
		}
		unshare, err := exec.LookPath("unshare")
		if err != nil {
			t.Skip("no unshare")
		}
		cmd := exec.Command(unshare, "-n", "--", "sh", "-c",
			`ip link add v0 numtxqueues 4 numrxqueues 4 type veth peer name v1 numtxqueues 4 numrxqueues 4 && ip addr add 10.9.0.1/24 dev v0 && ip link set v1 up && ip link set v0 up && ip neigh replace 10.9.0.2 lladdr 02:00:00:00:00:02 dev v0 && exec "$0" -test.run '^TestQShaperMqChildTakesOverRootLeftover$' -test.v`, os.Args[0])
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
	if !qKernelAtLeast(5, 18) {
		t.Skip("kernel < 5.18")
	}
	defer func(k bool, m string) { queueCtlKeep, qShaperMode = k, m }(queueCtlKeep, qShaperMode)
	queueCtlKeep = true
	qLockDir = t.TempDir()
	tcs := func(a ...string) string {
		out, _ := exec.Command("tc", a...).CombinedOutput()
		return string(out)
	}
	var logs []string
	lf := func(f string, a ...any) { logs = append(logs, f) }
	peer := netip.MustParseAddrPort("10.9.0.2:443")
	qShaperMode = "root"
	old := newQShaper(lf) // rel69d: root mode
	if !old.park(qDsts(peer)) || !strings.Contains(tcs("qdisc", "show", "dev", "v0", "root"), "htb 1fe1:") {
		t.Fatalf("root mode: %s %q", tcs("qdisc", "show", "dev", "v0"), logs)
	}
	qShaperMode = "mqchild"
	live := newQShaper(lf) // the root-mode process is still up: share its tree
	if !live.park(qDsts(peer)) || live.mqc || !strings.Contains(tcs("qdisc", "show", "dev", "v0", "root"), "htb 1fe1:") {
		t.Fatalf("live root-mode tree taken over: mqc %v %s", live.mqc, tcs("qdisc", "show", "dev", "v0"))
	}
	live.remove()
	old.slot.Close() // the root-mode process is gone, its htb root is not
	if !strings.Contains(tcs("qdisc", "show", "dev", "v0", "root"), "htb 1fe1:") {
		t.Fatalf("want the leftover htb root: %s", tcs("qdisc", "show", "dev", "v0"))
	}
	s := newQShaper(lf)
	if !s.park(qDsts(peer)) || !s.mqc || s.mqParent != "1fe0:4" || s.mqQueue != 3 {
		t.Fatalf("leftover not taken over: mqc %v parent %q %s %q", s.mqc, s.mqParent, tcs("qdisc", "show", "dev", "v0"), logs)
	}
	root1 := tcs("qdisc", "show", "dev", "v0", "root")
	if !strings.Contains(root1, "qdisc mq 1fe0: root") || !strings.Contains(tcs("qdisc", "show", "dev", "v0"), "qdisc htb 1fe1: parent 1fe0:4") {
		t.Fatalf("want mq 1fe0: root and htb on 1fe0:4: %s", tcs("qdisc", "show", "dev", "v0"))
	}
	if c := tcs("class", "show", "dev", "v0"); strings.Contains(c, "1fe1:2 ") && s.minor != 2 {
		t.Fatalf("the gone process's class came along: %s", c)
	}
	s.slot.Close() // stop by signal: no remove
	s2 := newQShaper(lf)
	if !s2.park(qDsts(peer)) || !s2.mqc || s2.mqParent != "1fe0:4" {
		t.Fatalf("restart: mqc %v parent %q %q", s2.mqc, s2.mqParent, logs)
	}
	if r := tcs("qdisc", "show", "dev", "v0", "root"); r != root1 {
		t.Fatalf("restart touched the root: %q -> %q", root1, r)
	}
	s2.remove()
}
