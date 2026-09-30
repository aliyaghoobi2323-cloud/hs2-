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
			if strings.Contains(l, fmt.Sprintf("hs2-icmp-%04x", magic)) {
				return strings.TrimSpace(l)
			}
		}
	}
	return ""
}
