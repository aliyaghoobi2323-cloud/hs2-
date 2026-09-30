//go:build linux

package encap

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// With nft or iptables available, an icmp listener must (1) keep the kernel from
// answering the tunnel's own echo requests, (2) leave ordinary ping working —
// the server's IP keeps answering, and icmp_echo_ignore_all is not touched — and
// (3) remove its rule when it closes. Each method is forced in turn.
func TestICMPEchoGuardKeepsHostPing(t *testing.T) {
	needRawNetns(t)
	if _, err := exec.LookPath("ping"); err != nil {
		t.Skip("ping not installed")
	}
	for _, method := range []string{"nft", "iptables"} {
		t.Run(method, func(t *testing.T) {
			if _, err := exec.LookPath(method); err != nil {
				t.Skipf("%s not installed", method)
			}
			t.Setenv("HS2_ICMP_SUPPRESS", method)
			os.WriteFile(echoIgnorePath, []byte("0\n"), 0o644)

			srv, err := Listen(KindICMP, "127.0.0.1", Options{Key: []byte("guard-" + method)})
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			magic := srv.(*rawPacketConn).f.rxMagic
			if got := echoGuardMethod(magic); got != method {
				srv.Close()
				t.Fatalf("suppression method %q, want %q", got, method)
			}
			if EchoIgnored() {
				srv.Close()
				t.Fatal("icmp_echo_ignore_all was turned on — the whole server would stop answering ping")
			}

			// (2) an ordinary ping is answered. -s 8 leaves no room for ping's
			// timestamp, so the payload is exactly the -p pattern; pick one that
			// is not the tunnel's magic.
			pat := "a55a"
			if fmt.Sprintf("%04x", magic) == pat {
				pat = "5aa5"
			}
			if out, err := exec.Command("ping", "-c", "2", "-W", "1", "-s", "8", "-p", pat, "127.0.0.1").CombinedOutput(); err != nil {
				srv.Close()
				t.Fatalf("an ordinary ping to the server went unanswered while the icmp listener was open: %v\n%s", err, out)
			}

			// (1) the tunnel's own echo requests get no kernel reply on the wire.
			sn := newICMPReplySniffer(t)
			cli, err := Dial(KindICMP, "127.0.0.1", Options{Key: []byte("guard-" + method)})
			if err != nil {
				srv.Close()
				t.Fatalf("dial: %v", err)
			}
			for i := 0; i < 10; i++ {
				cli.Write([]byte("req"))
			}
			for i := 0; i < 10; i++ {
				readFromT(t, srv, 2*time.Second)
			}
			time.Sleep(250 * time.Millisecond)
			cli.Close()
			if n := sn.replies(); n != 0 {
				srv.Close()
				t.Fatalf("kernel emitted %d echo replies to the tunnel's requests (%s rule not effective)", n, method)
			}

			// (3) closing removes the rule.
			srv.Close()
			if echoGuardMethod(magic) != "" {
				t.Fatal("suppression still held after the listener closed")
			}
			if left := guardRuleLeft(method, magic); left != "" {
				t.Fatalf("%s rule left behind after close: %s", method, left)
			}
		})
	}
}

// Two listeners with the same key share one rule; it stays until the last one
// closes (refcount), and a double close does not drop it early.
func TestICMPEchoGuardRefcount(t *testing.T) {
	needRawNetns(t)
	if _, err := exec.LookPath("nft"); err != nil {
		t.Skip("nft not installed")
	}
	t.Setenv("HS2_ICMP_SUPPRESS", "nft")
	opt := Options{Key: []byte("guard-refcount")}
	l1, err := Listen(KindICMP, "127.0.0.1", opt)
	if err != nil {
		t.Fatal(err)
	}
	l2, err := Listen(KindICMP, "127.0.0.2", opt)
	if err != nil {
		l1.Close()
		t.Fatal(err)
	}
	magic := l1.(*rawPacketConn).f.rxMagic
	l1.Close()
	l1.Close()
	if guardRuleLeft("nft", magic) == "" {
		l2.Close()
		t.Fatal("rule removed while a second listener still needs it")
	}
	l2.Close()
	if left := guardRuleLeft("nft", magic); left != "" {
		t.Fatalf("rule left after the last listener closed: %s", left)
	}
}

// guardRuleLeft returns the installed rule for magic ("" when none).
func guardRuleLeft(method string, magic uint16) string {
	switch method {
	case "nft":
		out, _ := exec.Command("nft", "list", "tables").CombinedOutput()
		for _, l := range strings.Split(string(out), "\n") {
			if strings.Contains(l, nftGuardTable(magic)) {
				return strings.TrimSpace(l)
			}
		}
	case "iptables":
		out, _ := exec.Command("iptables", "-w", "-S", "OUTPUT").CombinedOutput()
		for _, l := range strings.Split(string(out), "\n") {
			if strings.Contains(l, iptGuardTag(magic)) {
				return strings.TrimSpace(l)
			}
		}
	}
	return ""
}

// The sweep removes rules whose owner is gone — a daemon SIGKILLed or crashed
// before its listener closed — and nothing else: a rule of a live hs2 (another
// tunnel) stays, and a PID-less rule from an older binary stays unless the
// caller says no older binary can own it (legacy).
func TestSweepStaleEchoGuards(t *testing.T) {
	needRawNetns(t)
	for _, tool := range []string{"nft", "iptables"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	// a PID that is certainly dead: a child that has exited and been reaped
	c := exec.Command("true")
	if err := c.Run(); err != nil {
		t.Fatal(err)
	}
	dead := c.Process.Pid
	self := os.Getpid()
	nftAdd := func(name string) {
		if out, err := exec.Command("nft", "add", "table", "inet", name).CombinedOutput(); err != nil {
			t.Fatalf("nft add %s: %v %s", name, err, out)
		}
	}
	iptAdd := func(tag string, magic uint16) {
		if out, err := exec.Command("iptables", iptGuardArgsTag("-I", magic, tag)...).CombinedOutput(); err != nil {
			t.Fatalf("iptables add %s: %v %s", tag, err, out)
		}
	}
	nftAdd(fmt.Sprintf("hs2_icmp_aa01_%d", dead))
	nftAdd(fmt.Sprintf("hs2_icmp_aa02_%d", self))
	nftAdd("hs2_icmp_aa03")
	nftAdd("unrelated_table")
	iptAdd(fmt.Sprintf("hs2-icmp-bb01-%d", dead), 0xbb01)
	iptAdd(fmt.Sprintf("hs2-icmp-bb02-%d", self), 0xbb02)
	iptAdd("hs2-icmp-bb03", 0xbb03)
	defer func() {
		for _, n := range []string{"hs2_icmp_aa02_" + fmt.Sprint(self), "hs2_icmp_aa03", "unrelated_table"} {
			exec.Command("nft", "delete", "table", "inet", n).Run()
		}
		exec.Command("iptables", iptGuardArgsTag("-D", 0xbb02, fmt.Sprintf("hs2-icmp-bb02-%d", self))...).Run()
		exec.Command("iptables", iptGuardArgsTag("-D", 0xbb03, "hs2-icmp-bb03")...).Run()
	}()
	has := func() string {
		a, _ := exec.Command("nft", "list", "tables").CombinedOutput()
		b, _ := exec.Command("iptables", "-w", "-S", "OUTPUT").CombinedOutput()
		return string(a) + string(b)
	}

	got := SweepStaleEchoGuards(false)
	h := has()
	if len(got) != 2 || strings.Contains(h, fmt.Sprintf("aa01_%d", dead)) || strings.Contains(h, fmt.Sprintf("bb01-%d", dead)) {
		t.Fatalf("dead owner's rules not both removed: removed=%v\n%s", got, h)
	}
	for _, keep := range []string{fmt.Sprintf("aa02_%d", self), "hs2_icmp_aa03", "unrelated_table", fmt.Sprintf("bb02-%d", self), "hs2-icmp-bb03"} {
		if !strings.Contains(h, keep) {
			t.Fatalf("sweep removed %s, which it must keep\n%s", keep, h)
		}
	}
	got = SweepStaleEchoGuards(true)
	h = has()
	if len(got) != 2 || strings.Contains(h, "hs2_icmp_aa03") || strings.Contains(h, "hs2-icmp-bb03") {
		t.Fatalf("legacy sweep did not remove the PID-less rules: removed=%v\n%s", got, h)
	}
	if !strings.Contains(h, fmt.Sprintf("aa02_%d", self)) || !strings.Contains(h, "unrelated_table") {
		t.Fatalf("legacy sweep removed a live or foreign rule\n%s", h)
	}
}

// A daemon that died with the global fallback on (icmp_echo_ignore_all=1)
// leaves a marker; the next sweep in the same namespace turns ping replies
// back on — unless a live daemon still holds the setting.
func TestSweepRestoresGlobalEchoIgnore(t *testing.T) {
	needRawNetns(t)
	dir := t.TempDir()
	defer func(d string) { echoMarkDir = d }(echoMarkDir)
	echoMarkDir = dir
	c := exec.Command("true")
	if err := c.Run(); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(echoIgnorePath, []byte("1\n"), 0o644)
	defer os.WriteFile(echoIgnorePath, []byte("0\n"), 0o644)
	os.WriteFile(fmt.Sprintf("%s/%d.%s", dir, os.Getpid(), netnsID()), nil, 0o644) // live (us)
	os.WriteFile(fmt.Sprintf("%s/%d.%s", dir, c.Process.Pid, netnsID()), nil, 0o644)
	SweepStaleEchoGuards(false)
	if !EchoIgnored() {
		t.Fatal("ping replies turned on while a live daemon still holds the setting")
	}
	os.Remove(fmt.Sprintf("%s/%d.%s", dir, os.Getpid(), netnsID()))
	os.WriteFile(fmt.Sprintf("%s/%d.%s", dir, c.Process.Pid, netnsID()), nil, 0o644)
	SweepStaleEchoGuards(false)
	if EchoIgnored() {
		t.Fatal("a dead daemon's icmp_echo_ignore_all=1 was not undone")
	}
}
