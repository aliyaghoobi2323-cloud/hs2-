//go:build linux

// Package mmsg sends and receives several datagrams per system call
// (sendmmsg/recvmmsg) on a socket the Go runtime polls. One syscall per
// datagram was ~30% of the sending server's CPU at 60 Mbit/s over icmp (a
// profile of the tun pool); a batch costs one.
package mmsg

import (
	"errors"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Supported reports whether batches work on this platform.
const Supported = true

// mmsghdr is struct mmsghdr: a msghdr and the length the kernel reports
// (Go's padding matches C's on every Linux ABI).
type mmsghdr struct {
	hdr unix.Msghdr
	n   uint32
}

// Batch holds the kernel-facing arrays for up to its size datagrams, reused
// call after call. Not safe for concurrent use.
type Batch struct {
	hdrs  []mmsghdr
	iovs  []unix.Iovec
	names []unix.RawSockaddrInet4
}

// NewBatch returns a batch for up to n datagrams per call.
func NewBatch(n int) *Batch {
	return &Batch{hdrs: make([]mmsghdr, n), iovs: make([]unix.Iovec, n), names: make([]unix.RawSockaddrInet4, n)}
}

// Size is the most datagrams one call moves.
func (b *Batch) Size() int { return len(b.hdrs) }

// Name is datagram i's source after Recv with names (IPv4).
func (b *Batch) Name(i int) *unix.RawSockaddrInet4 { return &b.names[i] }

// Inet4 builds the destination sockaddr for Send.
func Inet4(ip []byte, port int) *unix.RawSockaddrInet4 {
	sa := &unix.RawSockaddrInet4{Family: unix.AF_INET}
	copy(sa.Addr[:], ip)
	p := (*[2]byte)(unsafe.Pointer(&sa.Port))
	p[0], p[1] = byte(port>>8), byte(port)
	return sa
}

// Send hands bufs (one datagram each, at most Size) to the kernel in as few
// calls as it takes: to is the destination (nil on a connected socket), oob
// the control data every datagram carries (nil: none). It returns how many
// were sent before the first error (which it returns); the caller may drop
// the failing datagram and send the rest.
func (b *Batch) Send(rc syscall.RawConn, bufs [][]byte, to *unix.RawSockaddrInet4, oob []byte) (int, error) {
	n := b.fill(bufs, to, oob)
	sent := 0
	var serr error
	for sent < n && serr == nil {
		var k int
		err := rc.Write(func(fd uintptr) bool {
			r, _, e := unix.Syscall6(unix.SYS_SENDMMSG, fd, uintptr(unsafe.Pointer(&b.hdrs[sent])), uintptr(n-sent), 0, 0, 0)
			if e == unix.EAGAIN {
				return false // wait until the socket can take more
			}
			if e != 0 {
				serr = e
				return true
			}
			k = int(r)
			return true
		})
		if err != nil {
			return sent, err
		}
		sent += k
		if k == 0 && serr == nil {
			return sent, errors.New("mmsg: sendmmsg sent nothing")
		}
	}
	return sent, serr
}

// SendNoLock is Send without Go's per-socket write lock: each call runs inside
// rc.Control, which only holds a reference on the descriptor, so several
// goroutines reach the kernel on one socket at once. Only for a socket whose
// kernel send path does not serialise on the socket itself (UDP that is not
// corked, a raw IP_HDRINCL socket); on a plain raw socket the kernel's
// lock_sock would take the place of Go's lock, and sleeping on it costs more.
// EAGAIN (the send buffer is full) falls back to rc.Write, which waits.
func (b *Batch) SendNoLock(rc syscall.RawConn, bufs [][]byte, to *unix.RawSockaddrInet4, oob []byte) (int, error) {
	n := b.fill(bufs, to, oob)
	sent := 0
	for sent < n {
		var k int
		var e syscall.Errno
		call := func(fd uintptr) bool {
			r, _, en := unix.Syscall6(unix.SYS_SENDMMSG, fd, uintptr(unsafe.Pointer(&b.hdrs[sent])), uintptr(n-sent), 0, 0, 0)
			e, k = en, int(r)
			return en != unix.EAGAIN
		}
		if err := rc.Control(func(fd uintptr) { call(fd) }); err != nil {
			return sent, err
		}
		if e == unix.EAGAIN {
			if err := rc.Write(call); err != nil {
				return sent, err
			}
		}
		if e != 0 {
			return sent, e
		}
		if k == 0 {
			return sent, errors.New("mmsg: sendmmsg sent nothing")
		}
		sent += k
	}
	return sent, nil
}

// fill prepares the message headers for bufs (at most Size) and returns how
// many it took.
func (b *Batch) fill(bufs [][]byte, to *unix.RawSockaddrInet4, oob []byte) int {
	n := min(len(bufs), len(b.hdrs))
	for i := 0; i < n; i++ {
		h := &b.hdrs[i]
		*h = mmsghdr{}
		if len(bufs[i]) > 0 {
			b.iovs[i].Base = &bufs[i][0]
		} else {
			b.iovs[i].Base = nil
		}
		b.iovs[i].SetLen(len(bufs[i]))
		h.hdr.Iov = &b.iovs[i]
		h.hdr.SetIovlen(1)
		if to != nil {
			h.hdr.Name = (*byte)(unsafe.Pointer(to))
			h.hdr.Namelen = unix.SizeofSockaddrInet4
		}
		if len(oob) > 0 {
			h.hdr.Control = &oob[0]
			h.hdr.SetControllen(len(oob))
		}
	}
	return n
}

// Recv reads up to len(bufs) datagrams already waiting (at least one: it
// waits for the first), each into its buffer: sizes[i] is its length and
// trunc[i] whether it was cut short. With names, Name(i) is its source.
// oobs, when set, receive each datagram's control data (oobns[i] its length).
func (b *Batch) Recv(rc syscall.RawConn, bufs [][]byte, sizes []int, trunc []bool, names bool, oobs [][]byte, oobns []int) (int, error) {
	n := min(len(bufs), len(b.hdrs))
	for i := 0; i < n; i++ {
		h := &b.hdrs[i]
		*h = mmsghdr{}
		b.iovs[i].Base = &bufs[i][0]
		b.iovs[i].SetLen(len(bufs[i]))
		h.hdr.Iov = &b.iovs[i]
		h.hdr.SetIovlen(1)
		if names {
			h.hdr.Name = (*byte)(unsafe.Pointer(&b.names[i]))
			h.hdr.Namelen = unix.SizeofSockaddrInet4
		}
		if oobs != nil && len(oobs[i]) > 0 {
			h.hdr.Control = &oobs[i][0]
			h.hdr.SetControllen(len(oobs[i]))
		}
	}
	var got int
	var rerr error
	err := rc.Read(func(fd uintptr) bool {
		r, _, e := unix.Syscall6(unix.SYS_RECVMMSG, fd, uintptr(unsafe.Pointer(&b.hdrs[0])), uintptr(n), 0, 0, 0)
		if e == unix.EAGAIN {
			return false
		}
		if e != 0 {
			rerr = e
			return true
		}
		got = int(r)
		return true
	})
	if err != nil {
		return 0, err
	}
	if rerr != nil {
		return 0, rerr
	}
	for i := 0; i < got; i++ {
		sizes[i] = int(b.hdrs[i].n)
		if trunc != nil {
			trunc[i] = b.hdrs[i].hdr.Flags&unix.MSG_TRUNC != 0
		}
		if oobns != nil {
			oobns[i] = int(b.hdrs[i].hdr.Controllen)
		}
	}
	return got, nil
}
