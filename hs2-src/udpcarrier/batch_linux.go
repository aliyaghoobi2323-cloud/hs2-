//go:build linux

package udpcarrier

import (
	"net"
	"os"
	"sync"
	"unsafe"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/mmsg"
	"golang.org/x/sys/unix"
)

// Batched sends and receives on UDP sockets (sendmmsg/recvmmsg): the raw
// encapsulations batch inside encap; these cover the udp one. HS2_RAW_BATCH=0
// turns every batch off (one datagram per syscall, as before).

var noBatch = os.Getenv("HS2_RAW_BATCH") == "0"

const udpBatch = 32

// udpConnWriteBatch is a batch sender for a connected UDP socket (the dial
// side), nil when batches are off.
func udpConnWriteBatch(uc *net.UDPConn) func([][]byte) error {
	if noBatch {
		return nil
	}
	rc, err := uc.SyscallConn()
	if err != nil {
		return nil
	}
	b := mmsg.NewBatch(udpBatch) // the carrier's one pacer is its only user
	return func(ps [][]byte) error {
		for len(ps) > 0 {
			chunk := ps[:min(len(ps), udpBatch)]
			n, err := b.Send(rc, chunk, nil, nil)
			if err != nil {
				return err
			}
			ps = ps[n:]
		}
		return nil
	}
}

// udpConnReadBatch reads what is waiting on a connected UDP socket in one
// call, handing each datagram (a fresh copy) to fn; nil when batches are off.
func udpConnReadBatch(uc *net.UDPConn) func(fn func([]byte)) error {
	if noBatch {
		return nil
	}
	rc, err := uc.SyscallConn()
	if err != nil {
		return nil
	}
	b := mmsg.NewBatch(udpBatch)
	bufs := make([][]byte, udpBatch)
	for i := range bufs {
		bufs[i] = make([]byte, 2048)
	}
	sizes, trunc := make([]int, udpBatch), make([]bool, udpBatch)
	return func(fn func([]byte)) error {
		n, err := b.Recv(rc, bufs, sizes, trunc, false, nil, nil)
		if err != nil {
			return err
		}
		for i := 0; i < n; i++ {
			if !trunc[i] {
				fn(append([]byte(nil), bufs[i][:sizes[i]]...))
			}
		}
		return nil
	}
}

// udpListenBatch is the listener's batch reader and writer for its UDP
// socket: sources from the message names, the local address each peer
// targeted from IP_PKTINFO when it is on (pktinfo), and replies from it.
type udpListenBatch struct {
	rc interface {
		Read(func(uintptr) bool) error
	}
	uc      *net.UDPConn
	pktinfo bool
	rb      *mmsg.Batch
	bufs    [][]byte
	sizes   []int
	trunc   []bool
	oobs    [][]byte
	oobns   []int
}

func newUDPListenBatch(uc *net.UDPConn, pktinfo bool) *udpListenBatch {
	if noBatch {
		return nil
	}
	if _, err := uc.SyscallConn(); err != nil {
		return nil
	}
	l := &udpListenBatch{uc: uc, pktinfo: pktinfo, rb: mmsg.NewBatch(udpBatch),
		bufs: make([][]byte, udpBatch), sizes: make([]int, udpBatch), trunc: make([]bool, udpBatch)}
	for i := range l.bufs {
		l.bufs[i] = make([]byte, 2048)
	}
	if pktinfo {
		l.oobs, l.oobns = make([][]byte, udpBatch), make([]int, udpBatch)
		for i := range l.oobs {
			l.oobs[i] = make([]byte, pktinfoOOB)
		}
	}
	return l
}

// read takes the waiting datagrams in one call; fn gets each (its buffer is
// reused after fn returns), its source, and the local IP it was sent to (nil
// without pktinfo).
func (l *udpListenBatch) read(fn func(b []byte, src net.Addr, dst net.IP)) error {
	rc, err := l.uc.SyscallConn()
	if err != nil {
		return err
	}
	n, err := l.rb.Recv(rc, l.bufs, l.sizes, l.trunc, true, l.oobs, l.oobns)
	if err != nil {
		return err
	}
	for i := 0; i < n; i++ {
		if l.trunc[i] {
			continue
		}
		sa := l.rb.Name(i)
		if sa.Family != unix.AF_INET {
			continue
		}
		pb := (*[2]byte)(unsafe.Pointer(&sa.Port)) // network byte order in memory
		port := int(pb[0])<<8 | int(pb[1])
		src := &net.UDPAddr{IP: net.IPv4(sa.Addr[0], sa.Addr[1], sa.Addr[2], sa.Addr[3]), Port: port}
		var dst net.IP
		if l.pktinfo && l.oobns[i] > 0 {
			dst = pktinfoDst(l.oobs[i][:l.oobns[i]])
		}
		fn(l.bufs[i][:l.sizes[i]], src, dst)
	}
	return nil
}

// pktinfoDst is the IP_PKTINFO header destination in a control message.
func pktinfoDst(oob []byte) net.IP {
	msgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return nil
	}
	for _, m := range msgs {
		if m.Header.Level == unix.IPPROTO_IP && m.Header.Type == unix.IP_PKTINFO && len(m.Data) >= 12 {
			return net.IPv4(m.Data[8], m.Data[9], m.Data[10], m.Data[11])
		}
	}
	return nil
}

// udpWriteBatchTo sends several datagrams to one peer from src (nil: the
// kernel's choice) in one call.
func udpWriteBatchTo(uc *net.UDPConn, ps [][]byte, to *net.UDPAddr, src net.IP) error {
	rc, err := uc.SyscallConn()
	if err != nil {
		return err
	}
	ip4 := to.IP.To4()
	if ip4 == nil {
		for _, p := range ps {
			if err := writeFrom(uc, p, to, src); err != nil {
				return err
			}
		}
		return nil
	}
	var oob []byte
	if s4 := src.To4(); s4 != nil {
		info := &unix.Inet4Pktinfo{}
		copy(info.Spec_dst[:], s4)
		oob = unix.PktInfo4(info)
	}
	b := udpBatchPool.Get().(*mmsg.Batch)
	defer udpBatchPool.Put(b)
	sa := mmsg.Inet4(ip4, to.Port)
	for len(ps) > 0 {
		chunk := ps[:min(len(ps), udpBatch)]
		n, err := b.Send(rc, chunk, sa, oob)
		if err != nil {
			return err
		}
		ps = ps[n:]
	}
	return nil
}

var udpBatchPool = sync.Pool{New: func() any { return mmsg.NewBatch(udpBatch) }}
