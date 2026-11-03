package engine

import (
	"errors"
	"net"
	"os"
	"sync/atomic"
	"syscall"
	"time"
)

// The exit dials one TCP connection to its target (the panel) per user
// stream. To a target on this server — 127.0.0.1:443, say — every one of
// those connections has the same source IP, destination IP and port, so only
// the source port tells them apart: the kernel's ephemeral range (32768–60999
// by default, ~28k ports) is a hard cap on the user connections one panel port
// can hold at once, and past it connect() fails with EADDRNOTAVAIL while the
// tunnel itself looks healthy.
//
// All of 127.0.0.0/8 is this host, so a connection from 127.0.0.2 is a new
// 4-tuple with a fresh ~28k ports. With HS2_EXIT_SRC_ROTATE=1, a dial to a
// loopback IPv4 target that fails for want of a local port is retried from
// the next source in 127.0.0.2–127.0.0.254, and only then. While ports last
// nothing changes: the panel sees 127.0.0.1 as before. Off by default: a
// panel that limits the IPs per user would count each extra source as one
// more IP (`hs2 status` counts the EADDRNOTAVAIL failures — measured: line —
// so the need shows before it is switched on).

var exitSrcRotate = os.Getenv("HS2_EXIT_SRC_ROTATE") == "1"

const (
	exitDialTimeout = 5 * time.Second
	// exitSrcTries: the other sources one failed dial tries. A source whose
	// ports are used up fails at once (no packet leaves), so this costs
	// microseconds, not timeouts.
	exitSrcTries = 8
)

// exitSrcNext picks the rotation's next source, 127.0.0.2..254.
var exitSrcNext atomic.Uint32

func exitSrc() net.IP {
	return net.IPv4(127, 0, 0, byte(2+exitSrcNext.Add(1)%253))
}

// dialExitTCP dials the exit's target. See exitSrcRotate.
func dialExitTCP(target string) (net.Conn, error) {
	return dialExitTCPWith(target, exitSrcRotate)
}

func dialExitTCPWith(target string, rotate bool) (net.Conn, error) {
	c, err := net.DialTimeout("tcp", target, exitDialTimeout)
	if err == nil || !rotate || !errors.Is(err, syscall.EADDRNOTAVAIL) || !loopback4(target) {
		noteExitDial(err)
		return c, err
	}
	for i := 0; i < exitSrcTries; i++ {
		d := net.Dialer{Timeout: exitDialTimeout, LocalAddr: &net.TCPAddr{IP: exitSrc()}}
		c, e := d.Dial("tcp", target)
		if e == nil {
			obs.exitRotated.Add(1)
			noteExitDial(nil)
			return c, nil
		}
		if !errors.Is(e, syscall.EADDRNOTAVAIL) {
			noteExitDial(e)
			return nil, e
		}
	}
	noteExitDial(err)
	return nil, err
}

// loopback4 reports whether target (host:port) is an IPv4 loopback address.
func loopback4(target string) bool {
	host, _, err := net.SplitHostPort(target)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.To4() != nil && ip.IsLoopback()
}
