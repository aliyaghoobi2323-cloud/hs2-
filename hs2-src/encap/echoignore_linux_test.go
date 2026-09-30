//go:build linux

package encap

import (
	"os"
	"strings"
	"testing"
)

// These tests cover the LAST-RESORT path of the icmp listener's echo-reply
// suppression (echoguard_linux.go): the global net.ipv4.icmp_echo_ignore_all,
// used only when neither nft nor iptables works. They force it with
// HS2_ICMP_SUPPRESS=global. They toggle the sysctl, so they must run in the
// package's private network namespace (TestMain re-execs into one). They reach
// the package-internal refcount directly to start from a known state, since it
// is process-global and other tests may have used ICMP listeners before them.

func readEchoIgnore(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(echoIgnorePath)
	if err != nil {
		t.Fatalf("read %s: %v", echoIgnorePath, err)
	}
	return strings.TrimSpace(string(b))
}

func setEchoIgnore(t *testing.T, v string) {
	t.Helper()
	if err := os.WriteFile(echoIgnorePath, []byte(v+"\n"), 0o644); err != nil {
		t.Skipf("cannot write %s (need root/netns): %v", echoIgnorePath, err)
	}
}

func resetEchoRefs() {
	echoMu.Lock()
	echoRefs, echoWeSet = 0, false
	echoMu.Unlock()
}

// An ICMP listener turns the kernel's echo replies off while it is open and
// turns them back on when it closes — so stopping the tunnel does not leave the
// host silent to real pings. With two listeners the setting is held until the
// last one closes (refcounted).
func TestICMPEchoRestoredOnClose(t *testing.T) {
	needRawNetns(t)
	t.Setenv("HS2_ICMP_SUPPRESS", "global") // the sysctl fallback (no nft/iptables)
	resetEchoRefs()
	setEchoIgnore(t, "0") // host answers pings to begin with
	defer resetEchoRefs()

	l1, err := Listen(KindICMP, "127.0.0.1", Options{Key: []byte("echo-a")})
	if err != nil {
		t.Fatalf("listen 1: %v", err)
	}
	if got := readEchoIgnore(t); got != "1" {
		l1.Close()
		t.Fatalf("after opening an ICMP listener icmp_echo_ignore_all=%s, want 1", got)
	}
	l2, err := Listen(KindICMP, "127.0.0.2", Options{Key: []byte("echo-b")})
	if err != nil {
		l1.Close()
		t.Fatalf("listen 2: %v", err)
	}

	if err := l1.Close(); err != nil {
		t.Fatalf("close 1: %v", err)
	}
	if got := readEchoIgnore(t); got != "1" {
		l2.Close()
		t.Fatalf("with one ICMP listener still open icmp_echo_ignore_all=%s, want 1", got)
	}
	// Closing twice must not double-release the refcount.
	l1.Close()
	if got := readEchoIgnore(t); got != "1" {
		l2.Close()
		t.Fatalf("a double close dropped the hold: icmp_echo_ignore_all=%s, want 1", got)
	}

	if err := l2.Close(); err != nil {
		t.Fatalf("close 2: %v", err)
	}
	if got := readEchoIgnore(t); got != "0" {
		t.Fatalf("after the last ICMP listener closed icmp_echo_ignore_all=%s, want 0 (restored)", got)
	}
}

// A host where echo replies were already off keeps that setting after the
// tunnel stops: we only put back a value we ourselves changed.
func TestICMPEchoLeftAloneWhenPreSet(t *testing.T) {
	needRawNetns(t)
	t.Setenv("HS2_ICMP_SUPPRESS", "global") // the sysctl fallback (no nft/iptables)
	resetEchoRefs()
	setEchoIgnore(t, "1") // operator has disabled echo replies globally
	defer resetEchoRefs()

	l, err := Listen(KindICMP, "127.0.0.1", Options{Key: []byte("preset")})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := readEchoIgnore(t); got != "1" {
		t.Fatalf("we reset an operator-set value we did not change: icmp_echo_ignore_all=%s, want 1", got)
	}
}
