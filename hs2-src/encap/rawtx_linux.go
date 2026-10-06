//go:build linux

package encap

import (
	"errors"
	"log"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/mmsg"
	"golang.org/x/sys/unix"
)

// rawTx is a send-only IPPROTO_RAW socket beside an icmp receive socket (the
// dial side's shared mux socket, or the listener's). Every carrier of that
// socket sends through it with sendmmsg WITHOUT Go's per-socket write lock
// (mmsg SendNoLock), and the kernel's IP_HDRINCL send path takes no socket
// lock either. On the shared receive socket the carriers queued behind each
// other twice — Go's write lock, then the kernel's lock_sock on a raw socket
// that builds the IP header — and on a sender short of CPU one carrier's
// writer, descheduled while holding them, held every other carrier back. An
// IPPROTO_RAW socket receives nothing of the encapsulations' protocols, so it
// does not bring back the per-socket receive cost the shared socket removed.
//
// The outer IPv4 header is built here from the receive socket's own settings,
// so the packet on the wire is the one the kernel built before: TTL and TOS
// read from it, DF from its IP_MTU_DISCOVER (DO: DF and id 0, as the kernel
// leaves it; DONT: no DF, id 0 so the kernel picks the same per-destination id
// it did), the source left 0 for the kernel to fill unless the socket is bound
// or a wildcard listener answers from a given address; total length and
// checksum are always the kernel's. Only icmp: for gre, ipip and ipx the
// kernel's default (IP_PMTUDISC_WANT) sets DF with a hashed id that a built
// header cannot reproduce, so they keep the old path.
//
// A datagram the kernel refuses with a soft error is dropped and counted, as
// on the old path; one too big for the device without DF goes through the old
// socket, which fragments it as before. Any other failure turns the socket off
// for good (logged once): its senders go back to the old path.
type rawTx struct {
	ipc  *net.IPConn
	rc   syscall.RawConn
	tmpl [ipv4HdrLen]byte
	df   bool

	protoCmsg []byte      // IP_PROTOCOL: policy routing sees icmp, not IPPROTO_RAW
	noProto   atomic.Bool // the kernel refused IP_PROTOCOL: sent without it
	broken    atomic.Bool // failed: the old path serves from now on
}

const ipv4HdrLen = 20

// rawNoTx (HS2_RAW_TX=0, or HS2_RAW_BATCH=0) keeps every send on the receive
// socket, as before.
var rawNoTx = rawNoBatch || os.Getenv("HS2_RAW_TX") == "0"

// txFellBack counts datagrams the send-only socket handed to the old path (too
// big without DF, or after it failed), for tests and the start log.
var txFellBack atomic.Uint64

var txOpenLog sync.Once

// openRawTx opens the send socket for the receive socket from (bound to bind;
// nil for the wildcard). It returns nil — the old path — when it is turned
// off, for a kind other than icmp, when the receive socket's DF policy cannot
// be reproduced, or when the socket cannot be opened.
func openRawTx(f *framer, from *net.IPConn, bind net.IP) *rawTx {
	if rawNoTx || !mmsg.Supported || f.kind != KindICMP {
		return nil
	}
	frc, err := from.SyscallConn()
	if err != nil {
		return nil
	}
	var ttl, tos, pmtu int
	var gerr error
	if err := frc.Control(func(fd uintptr) {
		s := int(fd)
		if ttl, gerr = unix.GetsockoptInt(s, unix.IPPROTO_IP, unix.IP_TTL); gerr != nil {
			return
		}
		if tos, gerr = unix.GetsockoptInt(s, unix.IPPROTO_IP, unix.IP_TOS); gerr != nil {
			return
		}
		pmtu, gerr = unix.GetsockoptInt(s, unix.IPPROTO_IP, unix.IP_MTU_DISCOVER)
	}); err != nil || gerr != nil {
		return nil
	}
	t := &rawTx{}
	switch pmtu {
	case unix.IP_PMTUDISC_DO, unix.IP_PMTUDISC_PROBE:
		t.df = true
	case unix.IP_PMTUDISC_DONT:
	default:
		return nil // WANT: DF with a kernel-hashed id; not reproducible here
	}
	la := &net.IPAddr{IP: net.IPv4zero}
	if bind != nil {
		la = &net.IPAddr{IP: bind}
	}
	ipc, err := net.ListenIP("ip4:255", la)
	if err != nil {
		return nil
	}
	rc, err := ipc.SyscallConn()
	if err != nil {
		ipc.Close()
		return nil
	}
	drop := []unix.SockFilter{{Code: bpfRetK, K: 0}}
	if err := rc.Control(func(fd uintptr) {
		s := int(fd)
		forceSockBufs(s, sockBuf)
		unix.SetsockoptSockFprog(s, unix.SOL_SOCKET, unix.SO_ATTACH_FILTER, &unix.SockFprog{Len: 1, Filter: &drop[0]})
	}); err != nil {
		ipc.Close()
		return nil
	}
	t.ipc, t.rc = ipc, rc
	t.tmpl[0] = 0x45
	t.tmpl[1] = byte(tos)
	if t.df {
		t.tmpl[6] = 0x40
	}
	t.tmpl[8] = byte(ttl)
	t.tmpl[9] = byte(f.proto)
	if b4 := bind.To4(); b4 != nil {
		copy(t.tmpl[12:16], b4)
	}
	c := make([]byte, unix.CmsgSpace(4))
	h := (*unix.Cmsghdr)(unsafe.Pointer(&c[0]))
	h.Level, h.Type = unix.IPPROTO_IP, unix.IP_PROTOCOL
	h.SetLen(unix.CmsgLen(4))
	*(*int32)(unsafe.Pointer(&c[unix.CmsgLen(0)])) = int32(f.proto)
	t.protoCmsg = c
	txOpenLog.Do(func() {
		log.Printf("encap icmp: carriers send through a send-only raw socket, without the shared socket's lock (HS2_RAW_TX=0 turns it off)")
	})
	return t
}

// ok reports whether sends go through t (nil, or failed: the old path).
func (t *rawTx) ok() bool { return t != nil && !t.broken.Load() }

func (t *rawTx) close() {
	if t != nil {
		t.ipc.Close()
	}
}

// frame builds into *bp the whole IP packet for one transport payload: the
// header from the template (src, when set, is the source) and the framer's
// transport header and payload after it.
func (t *rawTx) frame(bp *[]byte, f *framer, id, seq uint16, payload []byte, dst, src net.IP) []byte {
	n := ipv4HdrLen + f.hdr + len(payload)
	if cap(*bp) < n {
		*bp = make([]byte, 0, n)
	}
	b := (*bp)[:n]
	f.build(b[ipv4HdrLen:ipv4HdrLen], id, seq, payload) // in place: the room is there
	copy(b[:ipv4HdrLen], t.tmpl[:])
	if s4 := src.To4(); s4 != nil {
		copy(b[12:16], s4)
	}
	// The kernel routes by the sockaddr but sends this header as it is: the
	// destination must be in it.
	copy(b[16:20], dst.To4())
	return b
}

var txBatchPool = sync.Pool{New: func() any { return mmsg.NewBatch(rawBatch) }}

// oob is the control data for a send: IP_PKTINFO (the source a wildcard
// listener answers from) and IP_PROTOCOL.
func (t *rawTx) oob(local net.IP) []byte {
	proto := t.protoCmsg
	if t.noProto.Load() {
		proto = nil
	}
	l4 := local.To4()
	if l4 == nil {
		return proto
	}
	info := &unix.Inet4Pktinfo{}
	copy(info.Spec_dst[:], l4)
	return append(unix.PktInfo4(info), proto...)
}

// sendAll sends whole IP packets to dst (see rawTx for what a failure does).
// legacy sends one transport packet (pkt[ipv4HdrLen:]) on the old socket.
func (t *rawTx) sendAll(pkts [][]byte, dst, local net.IP, legacy func(transport []byte) error) error {
	b := txBatchPool.Get().(*mmsg.Batch)
	defer txBatchPool.Put(b)
	sa := mmsg.Inet4(dst.To4(), 0)
	oob := t.oob(local)
	for len(pkts) > 0 {
		if t.broken.Load() {
			for _, p := range pkts {
				txFellBack.Add(1)
				if err := legacy(p[ipv4HdrLen:]); err != nil {
					return err
				}
			}
			return nil
		}
		n, err := b.SendNoLock(t.rc, pkts[:min(len(pkts), rawBatch)], sa, oob)
		pkts = pkts[n:]
		if err == nil {
			continue
		}
		switch {
		case errors.Is(err, net.ErrClosed):
			return err // the mux or listener is closing
		case errors.Is(err, unix.EINVAL) && !t.noProto.Load():
			t.noProto.Store(true)
			oob = t.oob(local)
			continue // the same datagram again, without IP_PROTOCOL
		case errors.Is(err, unix.EMSGSIZE) && !t.df:
			txFellBack.Add(1)
			if lerr := legacy(pkts[0][ipv4HdrLen:]); lerr != nil {
				return lerr
			}
		case softErr(err):
			sendRefused.Add(1) // lost, as on the old path
		default:
			if t.broken.CompareAndSwap(false, true) {
				log.Printf("encap icmp: the send-only raw socket failed (%v) — its carriers send on the shared socket from now on", err)
			}
			continue // this datagram and the rest: the old path
		}
		pkts = pkts[1:]
	}
	return nil
}
