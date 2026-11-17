package engine

import (
	"net/netip"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"
)

// audit246 L5: with parking on, a first install under a cap holds its root
// graft (for qGraftHoldMax); a light rate, parking off, or the hold expired
// let it through; a lost tree holds as before (fx247).
func TestQNoRootGraftFirstInstall(t *testing.T) {
	defer func(k bool, h time.Duration) { queueCtlKeep, qGraftHoldMax = k, h }(queueCtlKeep, qGraftHoldMax)
	queueCtlKeep, qGraftHoldMax = true, time.Minute
	s := &qShaper{}
	if s.noRootGraft(qParkRate) {
		t.Fatal("parked (light) install held")
	}
	if !s.noRootGraft(1e6) || !s.noRootGraft(1e6) {
		t.Fatal("first install under a cap not held")
	}
	s.graftHeldAt = time.Now().Add(-2 * time.Minute)
	if s.noRootGraft(1e6) {
		t.Fatal("held past qGraftHoldMax")
	}
	s.lost = true
	if !s.noRootGraft(1e6) {
		t.Fatal("lost tree under a cap not held (fx247)")
	}
	queueCtlKeep = false
	if s2 := (&qShaper{}); s2.noRootGraft(1e6) {
		t.Fatal("parking off: first install held (it would never come)")
	}
}

// audit246 L5 on a kernel: a stale root-mode htb (its process gone) needs a
// root graft to become mq again. Under a cap the first install holds it
// (root untouched, mqchild still wanted — not a root-mode htb on the next
// tick); the first light tick (park) does the graft and goes in as mq child.
func TestQShaperL5HeldGraftOnKernel(t *testing.T) {
	if os.Getenv(qNetnsEnv) != "1" {
		if os.Geteuid() != 0 {
			t.Skip("needs root (a private network namespace)")
		}
		unshare, err := exec.LookPath("unshare")
		if err != nil {
			t.Skip("no unshare")
		}
		cmd := exec.Command(unshare, "-n", "--", "sh", "-c",
			`ip link add v0 numtxqueues 4 numrxqueues 4 type veth peer name v1 numtxqueues 4 numrxqueues 4 && ip addr add 10.9.0.1/24 dev v0 && ip link set v1 up && ip link set v0 up && ip neigh replace 10.9.0.2 lladdr 02:00:00:00:00:02 dev v0 && tc qdisc replace dev v0 root handle 1fe1: htb default 1 && exec "$0" -test.run '^TestQShaperL5HeldGraftOnKernel$' -test.v`, os.Args[0])
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
	// the htb line carries direct_packets_stat, which a stray packet on the
	// veth (IPv6 ND) bumps 0 -> 1: compare the qdisc without that counter
	dps := regexp.MustCompile(`direct_packets_stat \d+`)
	root := func() string {
		return dps.ReplaceAllString(tcs("qdisc", "show", "dev", "v0", "root"), "direct_packets_stat N")
	}
	root0 := root()
	if !strings.HasPrefix(root0, "qdisc htb 1fe1:") {
		t.Fatalf("setup: root %q", root0)
	}
	var logs []string
	lf := func(f string, a ...any) { logs = append(logs, f) }
	s := newQShaper(lf)
	peer := netip.MustParseAddrPort("10.9.0.2:443")
	def := qStat.deferred.Load()
	for i := 0; i < 2; i++ { // two capped ticks
		if !s.apply(12.5e6/qWire, qDsts(peer)) {
			t.Fatalf("lane off under a held graft: %q", logs)
		}
		if s.installed || !s.mqc || root() != root0 {
			t.Fatalf("tick %d: graft under a cap: installed %v mqc %v root %q", i, s.installed, s.mqc, tcs("qdisc", "show", "dev", "v0", "root"))
		}
	}
	if qStat.deferred.Load() == def {
		t.Fatal("held graft not counted")
	}
	if !s.park(qDsts(peer)) || !s.installed || !s.mqc {
		t.Fatalf("light tick: installed %v mqc %v %q", s.installed, s.mqc, logs)
	}
	if r := tcs("qdisc", "show", "dev", "v0", "root"); !strings.HasPrefix(r, "qdisc mq "+qMqHandle+":") {
		t.Fatalf("root after the light tick: %q", r)
	}
	s.remove()
}
