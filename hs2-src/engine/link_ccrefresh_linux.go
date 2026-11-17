//go:build linux

package engine

import (
	"encoding/binary"
	"net"
	"unsafe"

	"golang.org/x/sys/unix"
)

// tcpCCInfo reads the socket's congestion control name and, for BBR,
// TCP_CC_INFO (struct tcp_bbr_info: bw_lo, bw_hi, min_rtt, pacing_gain,
// cwnd_gain, all u32).
func tcpCCInfo(tc *net.TCPConn) (ccInfo, bool) {
	rc, err := tc.SyscallConn()
	if err != nil {
		return ccInfo{}, false
	}
	var ci ccInfo
	ok := false
	rc.Control(func(fd uintptr) {
		name, e := unix.GetsockoptString(int(fd), unix.IPPROTO_TCP, unix.TCP_CONGESTION)
		if e != nil {
			return
		}
		ok = true
		if name != "bbr" {
			return
		}
		var b [20]byte
		l := uint32(len(b))
		_, _, en := unix.Syscall6(unix.SYS_GETSOCKOPT, fd, unix.IPPROTO_TCP, unix.TCP_CC_INFO,
			uintptr(unsafe.Pointer(&b[0])), uintptr(unsafe.Pointer(&l)), 0)
		if en != 0 || l < 20 {
			ok = false
			return
		}
		ci.bbr = true
		ci.bwBytes = uint64(binary.NativeEndian.Uint32(b[0:])) | uint64(binary.NativeEndian.Uint32(b[4:]))<<32
		ci.gain = binary.NativeEndian.Uint32(b[12:])
		ci.cwndGain = binary.NativeEndian.Uint32(b[16:])
	})
	return ci, ok
}

// tcpCCReinit re-initialises the socket's congestion control: the kernel
// does it only on a change of algorithm, so "reno" (always built in) first,
// then the socket's own algorithm back.
func tcpCCReinit(tc *net.TCPConn) bool {
	rc, err := tc.SyscallConn()
	if err != nil {
		return false
	}
	ok := false
	rc.Control(func(fd uintptr) {
		name, e := unix.GetsockoptString(int(fd), unix.IPPROTO_TCP, unix.TCP_CONGESTION)
		if e == nil && name == "reno" {
			// fx247: a socket a failed reinit left on reno goes back
			if want, had := ccBackTake(tc); had {
				if unix.SetsockoptString(int(fd), unix.IPPROTO_TCP, unix.TCP_CONGESTION, want) != nil {
					ccBackNote(tc, want) // retried next tick
					return
				}
				ccBackStat.back.Add(1)
				ok = true
				return
			}
		}
		if e != nil || name == "" || name == "reno" {
			return
		}
		if unix.SetsockoptString(int(fd), unix.IPPROTO_TCP, unix.TCP_CONGESTION, "reno") != nil {
			return
		}
		ok = unix.SetsockoptString(int(fd), unix.IPPROTO_TCP, unix.TCP_CONGESTION, name) == nil
		if !ok {
			// fx247 (audit246 L1): never left on reno: once more now, and
			// remembered (ccBack) so the next tick puts it back first
			ok = unix.SetsockoptString(int(fd), unix.IPPROTO_TCP, unix.TCP_CONGESTION, name) == nil
			if !ok {
				ccBackNote(tc, name)
			}
		}
	})
	return ok
}
