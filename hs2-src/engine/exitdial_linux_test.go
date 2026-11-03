//go:build linux

package engine

import (
	"net"
	"os"
	"os/exec"
	"testing"
)

// The port-exhaustion test needs a network namespace of its own: it narrows
// ip_local_port_range (a per-namespace sysctl) to a handful of ports, which on
// the host would break everything else. As root the test re-executes just
// itself under `unshare -n`, with loopback up; otherwise it skips.
const exitNetnsEnv = "HS2_EXITDIAL_NETNS"

func TestExitDialRotatesWhenPortsRunOut(t *testing.T) {
	if os.Getenv(exitNetnsEnv) != "1" {
		if os.Geteuid() != 0 {
			t.Skip("needs root (a private network namespace)")
		}
		unshare, err := exec.LookPath("unshare")
		if err != nil {
			t.Skip("no unshare")
		}
		cmd := exec.Command(unshare, "-n", "--", "sh", "-c", `ip link set lo up && exec "$0" -test.run '^TestExitDialRotatesWhenPortsRunOut$' -test.v`, os.Args[0])
		cmd.Env = append(os.Environ(), exitNetnsEnv+"=1")
		out, err := cmd.CombinedOutput()
		t.Logf("in a private netns:\n%s", out)
		if err != nil {
			t.Fatalf("the test in its own netns failed: %v", err)
		}
		return
	}

	const ports = 8
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_local_port_range", []byte("40000 40007"), 0o644); err != nil {
		t.Fatalf("narrow the port range: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:39999") // outside the narrowed range, which is all for the dials
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	target := ln.Addr().String()
	var held []net.Conn
	defer func() {
		for _, c := range held {
			c.Close()
		}
	}()
	for i := 0; i < ports; i++ {
		c, err := dialExitTCPWith(target, false)
		if err != nil {
			t.Fatalf("dial %d of %d: %v", i+1, ports, err)
		}
		held = append(held, c)
	}

	before := Observed()
	if c, err := dialExitTCPWith(target, false); err == nil {
		c.Close()
		t.Fatal("with every port in use a dial got through: the test does not exhaust the range")
	}
	if d := Observed().Sub(before); d.ExitNoAddr != 1 {
		t.Fatalf("the failure was not counted as EADDRNOTAVAIL: %+v", d)
	}

	before = Observed()
	c, err := dialExitTCPWith(target, true)
	if err != nil {
		t.Fatalf("with rotation the dial still failed: %v", err)
	}
	defer c.Close()
	src := c.LocalAddr().(*net.TCPAddr).IP
	if src.Equal(net.IPv4(127, 0, 0, 1)) || !src.IsLoopback() {
		t.Fatalf("rotated dial came from %v, want another 127.0.0.x", src)
	}
	if d := Observed().Sub(before); d.ExitRotated != 1 || d.ExitNoAddr != 0 {
		t.Fatalf("counters after a rotated dial: rotated %d, noaddr %d", d.ExitRotated, d.ExitNoAddr)
	}
}

// Rotation never applies to a target that is not on this host.
func TestExitDialLoopbackOnly(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:443": true, "127.9.9.9:1": true, "[::1]:443": false,
		"10.0.0.1:443": false, "example.com:443": false, "bad": false,
	} {
		if got := loopback4(addr); got != want {
			t.Errorf("loopback4(%q) = %v, want %v", addr, got, want)
		}
	}
	seen := map[string]bool{}
	for i := 0; i < 600; i++ {
		ip := exitSrc()
		if ip.Equal(net.IPv4(127, 0, 0, 1)) || ip[15] < 2 || ip[15] > 254 {
			t.Fatalf("source %v outside 127.0.0.2–254", ip)
		}
		seen[ip.String()] = true
	}
	if len(seen) != 253 {
		t.Fatalf("rotation used %d sources, want all 253", len(seen))
	}
}
