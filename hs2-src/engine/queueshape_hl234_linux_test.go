package engine

import (
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// hl234: a new peer set goes in under the other generation's prios before
// the old ones go (no tick without the class's filters), in one tc batch;
// stats reads the leaf's backlog.
func TestQShaperFilterSwapOnKernel(t *testing.T) {
	if os.Getenv(qNetnsEnv) != "1" {
		if os.Geteuid() != 0 {
			t.Skip("needs root (a private network namespace)")
		}
		unshare, err := exec.LookPath("unshare")
		if err != nil {
			t.Skip("no unshare")
		}
		cmd := exec.Command(unshare, "-n", "--", "sh", "-c",
			`ip link add d0 type dummy && ip addr add 10.9.0.1/24 dev d0 && ip link set d0 up && exec "$0" -test.run '^TestQShaperFilterSwapOnKernel$' -test.v`, os.Args[0])
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
	defer func(k bool, h time.Duration) { queueCtlKeep, qGraftHoldMax = k, h }(queueCtlKeep, qGraftHoldMax)
	queueCtlKeep, qGraftHoldMax = true, 0 // a cap before any park installs at once (audit246 L5 hold off)
	filters := func() string {
		out, _ := exec.Command("tc", "filter", "show", "dev", "d0").CombinedOutput()
		return string(out)
	}
	qLockDir = t.TempDir()
	var logs []string
	s := newQShaper(func(f string, a ...any) { logs = append(logs, f) })
	a := netip.MustParseAddrPort("10.9.0.2:443")
	b := netip.MustParseAddrPort("10.9.0.2:50123")
	c := netip.MustParseAddrPort("[fd00::2]:443")
	if !s.apply(12.5e6/qWire, qDsts(a)) {
		t.Fatalf("apply refused: %q", logs)
	}
	g1 := s.gen
	if !s.apply(12.5e6/qWire, qDsts(b, a, c)) || s.gen == g1 {
		t.Fatalf("second set: gen %d -> %d %q", g1, s.gen, logs)
	}
	f := filters()
	v4, _ := qFilterPriosGen(s.minor, s.gen)
	o4, o6 := qFilterPriosGen(s.minor, g1)
	if !strings.Contains(f, "pref "+v4+" ") || strings.Contains(f, "pref "+o4+" ") || strings.Contains(f, "pref "+o6+" ") {
		t.Fatalf("generations: want only %s: %s", v4, f)
	}
	// u32 shows the ports as hex: 443 = 0x01bb, 50123 = 0xc3cb
	if !strings.Contains(f, "01bb") || !strings.Contains(f, "c3cb") {
		t.Fatalf("ports missing: %s", f)
	}
	if d, bl := s.stats(); d != 0 || bl != 0 {
		t.Fatalf("stats on an idle leaf: drops %d backlog %d", d, bl)
	}
	s.remove()
	if f := filters(); strings.Contains(f, "flowid") {
		t.Fatalf("filters left behind: %s", f)
	}
}
