package engine

import "net"

// The send buffer of a BULK link's socket (R1 round 3, the judge).
//
// Linux grows a socket's send buffer from its first 130 KB (ten segments,
// BBR's three times) as its congestion window grows, but never while the
// host's TCP memory is past tcp_mem's first mark (1.07 GB on the 23 GB
// Kharej server), pressure or not. The panel's loopback sockets of 25 users
// of 16 connections each held about a gigabyte (the judge's smokes): a BULK
// link that started after that, when the connections moved to it, kept a
// send buffer of 130 KB for the run, 65-90 KB in flight at 72 ms, and the
// 400 connections on it got 9 Mbit/s of 100 in the runs of three smokes
// (one escaped to 88 when the link had started earlier); the link's writer
// waited on the full socket with no cap, gate or budget in force.
//
// So a BULK link's socket gets a fixed send buffer, TuneLinkSndbuf: no
// growth to wait for. It is a ceiling, not a cost: the in-flight budget and
// C3 keep what it holds to the link's own flight. A link at 1 Gbit/s and a
// 150 ms round trip would want more than the default (8 MiB: ~450 Mbit/s at
// 150 ms, ~900 at 72); 0 leaves Linux's autotuning, as before.

// TuneLinkSndbuf (HS2_TUNE_LINK_SNDBUF, bytes; 0: autotuning): the send
// buffer of a BULK link's socket.
var TuneLinkSndbuf = 8 << 20

// setLinkSndbuf sets c's send buffer to TuneLinkSndbuf (best effort).
func setLinkSndbuf(c *net.TCPConn) {
	if c == nil || TuneLinkSndbuf <= 0 {
		return
	}
	if rc, err := c.SyscallConn(); err == nil {
		rc.Control(func(fd uintptr) { setSndBuf(fd, TuneLinkSndbuf) })
	}
}
