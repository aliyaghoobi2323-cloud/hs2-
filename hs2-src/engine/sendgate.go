package engine

import (
	"net"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// sendGate makes a slow link's socket keep to TCP_NOTSENT_LOWAT. Whatever the
// socket holds unsent, everything the link sends next — a window update, a
// reply, a control ping — waits behind, and the cap does not bound it: the
// kernel checks the cap only when a write needs a new segmentation-offload
// buffer and appends to the last one, up to 64 KiB, without looking. Woken
// once the socket holds less than half the cap, a writer that keeps writing
// fills it to about 64 KiB again: 100 ms at 5 Mbit/s, half a second at
// 1 Mbit/s. So on a link that takes less than SendGateRate while written to
// (meteredConn measures it), records go out gateBurst at a time, each burst
// only once the socket polls writable, that is holds less than half the cap
// (Linux 5.10 and later; older kernels poll writable below the whole cap):
// the socket holds no more than that and a burst — about 20 KB on average
// and 24-26 KB at most on the lab's 6.x kernel, about 40 KB at most on an
// older one, against 64 KB without the gate — and the rest waits in smux,
// where a stream with little to send goes next. The socket is still topped
// up from the same low mark as without the gate, so the link is no more
// often left with nothing to send. The wait is part of the write, as a full
// socket's was: the link's meter sees the writer blocked on the path as
// before (meteredConn, managedLink.lagging), and closing the link ends it as
// it ended a write blocked inside the TLS conn (Close).
type sendGate struct {
	net.Conn                 // the link's TLS conn
	tcp      *net.TCPConn    // its TCP socket, nil: none found
	raw      syscall.RawConn // the socket's, for the poll
	on       bool            // gate the records
	credit   int             // bytes that may go out before the socket is polled again
	// waiting: a write waits at the gate; closing: Close has begun (Close)
	waiting, closing atomic.Bool
	// the rate tally (took): bytes written and time spent writing since
	// the last sample
	n int64
	d time.Duration
	r int64 // bytes/s, the last samples averaged; 0: none yet
}

const (
	// gateBurst is how much goes out on one poll that found the socket
	// writable: a socket polled before each record, and woken for each,
	// costs a poll and a wakeup per record; one polled per gateBurst holds
	// that much more at most.
	gateBurst = 8 << 10
	// gateSample: the rate is sampled each time this much writing is tallied
	gateSample = 20 * time.Millisecond
)

// SendGateRate: links taking this many bytes a second or more while written
// to (about 25 Mbit/s) are not gated: what they hold unsent is gone in 20 ms
// or less, and the polls would cost CPU for little. 0 turns the gate off.
var SendGateRate = 3 << 20

func newSendGate(conn net.Conn) *sendGate {
	g := &sendGate{Conn: conn}
	if tc := tcpUnder(conn); tc != nil {
		g.tcp = tc
		g.raw, _ = tc.SyscallConn()
	}
	return g
}

// took tallies a write of n bytes by the link's writer that took d, the
// wait for the socket included (meteredConn, the writer's goroutine only),
// and each gateSample of writing turns the gate on or off by what the
// socket took per second: the path's rate while the socket is full, far
// more while it is not.
func (g *sendGate) took(n int, d time.Duration) {
	g.n += int64(n)
	g.d += d
	if g.d < gateSample {
		return
	}
	r := int64(float64(g.n) / g.d.Seconds())
	if g.r > 0 {
		r = (g.r + r) / 2
	}
	g.r, g.n, g.d = r, 0, 0
	g.on = g.raw != nil && r < int64(SendGateRate)
}

func (g *sendGate) Write(p []byte) (int, error) {
	if g.on {
		if g.credit <= 0 {
			// an error here (a closed socket) is the write's to report
			g.waiting.Store(true)
			if !g.closing.Load() {
				g.raw.Write(pollWritable)
			}
			g.waiting.Store(false)
			g.credit = gateBurst
		}
		g.credit -= len(p)
	}
	return g.Conn.Write(p)
}

// Close closes the link's conn. A write waiting at the gate is not inside the
// TLS conn, whose Close would then send close_notify behind the full socket:
// a write of its own, waiting for the socket's lock (held by the poll) until
// its 5 s deadline — the health loop that drops a black-holed link stood
// still that long. So Close ends that wait as tls.Conn.Close ends a write in
// flight: it closes the socket under it.
func (g *sendGate) Close() error {
	g.closing.Store(true)
	if g.waiting.Load() {
		g.tcp.Close()
	}
	return g.Conn.Close()
}

// pollWritable reports whether a socket polls writable (or in error); one
// that does not is then flagged to wake its poller once it is (tcp_poll).
func pollWritable(fd uintptr) bool {
	p := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLOUT}}
	n, err := unix.Poll(p, 0)
	return err != nil || n > 0
}

// tcpUnder is the TCP socket under c (c itself, or under its TLS), nil if
// there is none.
func tcpUnder(c net.Conn) *net.TCPConn {
	for range 3 {
		switch v := c.(type) {
		case *net.TCPConn:
			return v
		case interface{ NetConn() net.Conn }:
			c = v.NetConn()
		default:
			return nil
		}
	}
	return nil
}
