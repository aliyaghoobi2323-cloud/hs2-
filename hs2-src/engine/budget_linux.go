//go:build linux

package engine

import (
	"net"
	"unsafe"

	"golang.org/x/sys/unix"
)

// newOutQ is the in-flight budget's measure of a link socket (budget.go):
// with BudgetInflight (the default), what it has in flight as Linux counts
// it for its window — segments sent, not acknowledged, not selectively
// acknowledged and not lost, plus those resent (tcp_packets_in_flight:
// unacked − sacked − lost + retrans), times its segment — and its unsent
// bytes, from one TCP_INFO; else (or on a kernel whose TCP_INFO has no
// tcpi_notsent_bytes, before 4.6) SIOCOUTQ, everything not acknowledged.
// SIOCOUTQ counts the bytes past a hole that the peer has: a loss held the
// gate shut for the round trip of its recovery (the rig's S2, s5 at 25
// Mbit/s with 0.1% loss: 18.5 Mbit/s against 21.8 for the base binary).
// nil: no socket to read, or a kernel without either call.
func newOutQ(tc *net.TCPConn) func() (int, error) {
	if tc == nil {
		return nil
	}
	raw, err := tc.SyscallConn()
	if err != nil {
		return nil
	}
	outq := func() (int, error) {
		var v int
		var err error
		if e := raw.Control(func(fd uintptr) { v, err = unix.IoctlGetInt(int(fd), unix.SIOCOUTQ) }); e != nil {
			return 0, e
		}
		return v, err
	}
	info := func() (int, error) {
		var v int
		var err error
		short := false
		if e := raw.Control(func(fd uintptr) {
			ti, n, e := (*tcpInfoRead.Load())(int(fd))
			if e != nil || ti == nil {
				err = e
				return
			}
			if !tcpInfoHasNotsent(n) {
				short = true
				return
			}
			v = inflightOf(ti)
		}); e != nil {
			return 0, e
		}
		if short {
			return outq()
		}
		return v, err
	}
	if BudgetInflight != 0 {
		if _, err := info(); err == nil {
			return info
		}
	}
	if _, err := outq(); err != nil && hostqUnsupported(err) {
		return nil
	}
	return outq
}

// tcpInfoHasNotsent says whether a TCP_INFO of n bytes has
// tcpi_notsent_bytes in it (Linux 4.6 on; -1: a length not known).
func tcpInfoHasNotsent(n int) bool {
	return n >= int(unsafe.Offsetof(unix.TCPInfo{}.Notsent_bytes)+unsafe.Sizeof(unix.TCPInfo{}.Notsent_bytes))
}

// inflightOf is a TCP_INFO's bytes in flight (Linux's count for its window)
// and unsent.
func inflightOf(ti *unix.TCPInfo) int {
	segs := int64(ti.Unacked) - int64(ti.Sacked) - int64(ti.Lost) + int64(ti.Retrans)
	if segs < 0 {
		segs = 0
	}
	return int(segs*int64(ti.Snd_mss)) + int(ti.Notsent_bytes)
}
