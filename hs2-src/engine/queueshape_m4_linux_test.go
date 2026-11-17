//go:build linux

package engine

import (
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// m4f251 (audit246 M4) on a real kernel: an accepted-links key set (peer IP
// + our listen port, TCP) shapes exactly the packets from our listen port to
// that peer — never another port's (xray, a second tunnel) to the same IP, a
// UDP socket on the same port number, our port to another IP, or a dial TO
// that port — and 600 accepted links make one filter per peer address.
func TestQShaperM4GroupedOnKernel(t *testing.T) {
	if os.Getenv(qNetnsEnv) != "1" {
		if os.Geteuid() != 0 {
			t.Skip("needs root (a private network namespace)")
		}
		unshare, err := exec.LookPath("unshare")
		if err != nil {
			t.Skip("no unshare")
		}
		cmd := exec.Command(unshare, "-n", "--", "sh", "-c",
			`ip link add d0 type dummy && ip addr add 10.9.0.1/24 dev d0 && ip addr add fd00::1/64 dev d0 nodad && ip link set d0 up && ip link set lo up && exec "$0" -test.run '^TestQShaperM4GroupedOnKernel$' -test.v`, os.Args[0])
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
	qLockDir = t.TempDir()
	var logs []string
	s := newQShaperOn("d0", func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) })
	const listen = 18443
	// 600 accepted links: 2 peer v4 addresses + 1 v6, each its own ephemeral port
	var ks qKeySet
	for i := 0; i < 600; i++ {
		var peer netip.AddrPort
		switch i % 3 {
		case 0:
			peer = netip.AddrPortFrom(netip.MustParseAddr("10.9.0.2"), uint16(30000+i))
		case 1:
			peer = netip.AddrPortFrom(netip.MustParseAddr("10.9.0.4"), uint16(30000+i))
		default:
			peer = netip.AddrPortFrom(netip.MustParseAddr("fd00::2"), uint16(30000+i))
		}
		ks.add(netip.AddrPortFrom(netip.MustParseAddr("10.9.0.1"), listen), peer, true)
	}
	if len(ks.keys) != 3 {
		t.Fatalf("600 accepted links: %d keys, want 3", len(ks.keys))
	}
	if !s.apply(12.5e6/qWire, ks.keys) {
		t.Fatalf("apply refused: %q", logs)
	}
	defer s.remove()
	out, _ := exec.Command("tc", "filter", "show", "dev", "d0").CombinedOutput()
	if n := strings.Count(string(out), "flowid "+qClassID(s.minor)); n != 3 {
		t.Fatalf("filters for the class: %d, want 3:\n%s", n, out)
	}
	pkts := func() int {
		out, _ := exec.Command("tc", "-s", "class", "show", "dev", "d0", "classid", qClassID(s.minor)).CombinedOutput()
		m := regexp.MustCompile(`Sent \d+ bytes (\d+) pkt`).FindStringSubmatch(string(out))
		if m == nil {
			t.Fatalf("class stats: %s", out)
		}
		n, _ := strconv.Atoi(m[1])
		return n
	}
	// send one SYN from local port lport to dst (the dummy drops it: no answer)
	syn := func(lport int, dst string) {
		d := net.Dialer{Timeout: 150 * time.Millisecond, Control: func(_, _ string, c syscall.RawConn) error {
			return c.Control(func(fd uintptr) { syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1) })
		}}
		if lport != 0 {
			ip := "10.9.0.1"
			if strings.Contains(dst, "fd00") {
				ip = "fd00::1"
			}
			d.LocalAddr = &net.TCPAddr{IP: net.ParseIP(ip), Port: lport}
		}
		if c, err := d.Dial("tcp", dst); err == nil {
			c.Close()
		}
	}
	udp := func(lport int, dst string) {
		c, err := net.DialUDP("udp", &net.UDPAddr{IP: net.ParseIP("10.9.0.1"), Port: lport}, net.UDPAddrFromAddrPort(netip.MustParseAddrPort(dst)))
		if err != nil {
			t.Fatal(err)
		}
		c.Write([]byte("x"))
		c.Close()
	}
	cases := []struct {
		name   string
		send   func()
		shaped bool
	}{
		{"link v4 peer A", func() { syn(listen, "10.9.0.2:40001") }, true},
		{"link v4 peer B", func() { syn(listen, "10.9.0.4:40002") }, true},
		{"link v6", func() { syn(listen, "[fd00::2]:40003") }, true},
		{"other port to the same IP (xray)", func() { syn(18444, "10.9.0.2:40001") }, false},
		{"our port to another IP", func() { syn(listen, "10.9.0.3:40001") }, false},
		{"a dial TO our port number", func() { syn(0, "10.9.0.2:18443") }, false},
		{"UDP from our port number", func() { udp(listen, "10.9.0.2:40001") }, false},
		{"v6 other port", func() { syn(18444, "[fd00::2]:40003") }, false},
	}
	for _, c := range cases {
		before := pkts()
		c.send()
		time.Sleep(50 * time.Millisecond)
		got := pkts() - before
		if (got > 0) != c.shaped {
			t.Errorf("%s: %d packets through the cap class, shaped want %v", c.name, got, c.shaped)
		} else {
			t.Logf("%s: %d packets through the cap class (ok)", c.name, got)
		}
	}
	// qLinkSport: an accepted link's wire source port; a DNATed (REDIRECTed) one's is
	// the port the peer dialed (the reply is translated back before egress)
	accept := func(ln net.Listener, dial string) (*net.TCPConn, func()) {
		c, err := net.DialTimeout("tcp", dial, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		a, err := ln.Accept()
		if err != nil {
			t.Fatal(err)
		}
		return a.(*net.TCPConn), func() { a.Close(); c.Close() }
	}
	ln, err := net.Listen("tcp", "10.9.0.1:18445")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	a, done := accept(ln, "10.9.0.1:18445")
	if p := qLinkSport(a); p != 18445 {
		t.Errorf("plain accepted link: sport %d, want 18445", p)
	}
	done()
	if err := exec.Command("iptables", "-t", "nat", "-A", "OUTPUT", "-p", "tcp", "-d", "10.9.0.1", "--dport", "443", "-j", "DNAT", "--to-destination", "10.9.0.1:18445").Run(); err != nil {
		t.Logf("no iptables nat (%v): the DNAT case not checked", err)
	} else {
		a, done = accept(ln, "10.9.0.1:443")
		if p := qLinkSport(a); p != 443 {
			t.Errorf("DNATed accepted link: sport %d, want 443 (the dialed port)", p)
		} else {
			t.Logf("DNATed accepted link: sport 443 (local port %s)", a.LocalAddr())
		}
		done()
	}
	// churn: new ephemeral ports, same peers: no filter swap
	g := s.gen
	var ks2 qKeySet
	for i := 0; i < 600; i++ {
		ks2.add(netip.AddrPortFrom(netip.MustParseAddr("10.9.0.1"), listen), netip.AddrPortFrom(netip.MustParseAddr([]string{"10.9.0.2", "10.9.0.4", "fd00::2"}[i%3]), uint16(50000+i)), true)
	}
	if !s.apply(12.5e6/qWire, ks2.keys) || s.gen != g {
		t.Fatalf("churn swapped the filters: gen %d -> %d", g, s.gen)
	}
}
