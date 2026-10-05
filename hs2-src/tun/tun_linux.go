// Package tun owns the TUN device. It is opened ONCE for the life of the
// process and never torn down because a carrier or session failed: the
// interface, its address and every route the operator put on it survive any
// number of reconnects. (BackPack recreates its TUN on every engine
// generation, which silently deletes routes pointing at it. Not here.)
package tun

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	tunSetIff     = 0x400454ca // TUNSETIFF
	tunSetOffload = 0x400454d0 // TUNSETOFFLOAD
)

type Device struct {
	f    *os.File
	name string
	mtu  int

	// offload: opened with a virtio-net header and TCP offloads (see
	// offload.go). Read hands out the segments of one kernel read in turn
	// (rmu: one reader); writes carry the header.
	offload bool
	rmu     sync.Mutex
	rbuf    []byte
	segs    []byte // segments of the last read, back to back
	ends    []int  // each segment's end in segs
	segi    int    // next segment to hand out
	single  []byte // the last read when it was one packet (in rbuf)
	dropped atomic.Uint64

	// what the offloads saved, for the status line: kernel packets read and
	// the segments they gave; writes made and the packets they carried;
	// merged packets the kernel refused (their segments went in one by one).
	reads, segsOut, writes, pktsIn, refused atomic.Uint64
}

// OffloadCounts is what OffloadStats reports.
type OffloadCounts struct {
	Reads, Segs   uint64 // kernel packets read, and the IP packets they gave
	Writes, Pkts  uint64 // writes made, and the IP packets they carried
	Dropped       uint64 // offload packets from the kernel dropped as malformed
	RefusedMerges uint64 // merged packets the kernel refused (sent one by one instead)
}

// Options for OpenWith.
type Options struct {
	// Offload asks for TCP segmentation offload both ways (the kernel hands
	// over and takes 64 KB TCP packets); the device falls back to plain
	// packets when the kernel refuses it (Offloaded reports which).
	Offload bool
}

// Open creates (or attaches to) a TUN interface, assigns local/prefix with an
// explicit peer route, sets MTU and brings it up.
func Open(name, localCIDR, peerIP string, mtu int) (*Device, error) {
	return OpenWith(name, localCIDR, peerIP, mtu, Options{})
}

// openFD creates the interface and returns its fd, with the virtio-net header
// and TCP offloads when vnet; the kernel's name for it.
func openFD(name string, vnet bool) (int, string, error) {
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, "", fmt.Errorf("tun: open /dev/net/tun: %w (need root/CAP_NET_ADMIN, tun module)", err)
	}
	var ifr [unix.IFNAMSIZ + 64]byte
	copy(ifr[:unix.IFNAMSIZ-1], name)
	flags := uint16(unix.IFF_TUN | unix.IFF_NO_PI)
	if vnet {
		flags |= unix.IFF_VNET_HDR
	}
	*(*uint16)(unsafe.Pointer(&ifr[unix.IFNAMSIZ])) = flags
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), tunSetIff, uintptr(unsafe.Pointer(&ifr[0]))); e != 0 {
		unix.Close(fd)
		return -1, "", fmt.Errorf("tun: TUNSETIFF %s: %v", name, e)
	}
	if vnet {
		if _, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), tunSetOffload, uintptr(unix.TUN_F_CSUM|unix.TUN_F_TSO4)); e != 0 {
			unix.Close(fd)
			return -1, "", fmt.Errorf("tun: TUNSETOFFLOAD: %v", e)
		}
	}
	return fd, strings.TrimRight(string(ifr[:unix.IFNAMSIZ]), "\x00"), nil
}

// OpenWith is Open with options (offloads).
func OpenWith(name, localCIDR, peerIP string, mtu int, o Options) (*Device, error) {
	// Remove a stale interface of the same name left by a crashed/killed prior
	// instance, so restart never fails with "device or resource busy".
	exec.Command("ip", "link", "del", name).Run()
	fd, actual, err := -1, "", error(nil)
	offload := false
	if o.Offload {
		if fd, actual, err = openFD(name, true); err == nil {
			offload = true
		} else {
			exec.Command("ip", "link", "del", name).Run()
		}
	}
	if !offload {
		if fd, actual, err = openFD(name, false); err != nil {
			return nil, err
		}
	}
	d := &Device{f: os.NewFile(uintptr(fd), "/dev/net/tun"), name: actual, mtu: mtu, offload: offload}
	if offload {
		d.rbuf = make([]byte, vnetHdrLen+65535)
		d.segs = make([]byte, 0, 128<<10)
	}
	steps := [][]string{
		{"addr", "replace", localCIDR, "dev", actual},
		{"link", "set", "dev", actual, "mtu", strconv.Itoa(mtu)},
		{"link", "set", "dev", actual, "txqueuelen", "2000"},
		{"link", "set", "dev", actual, "up"},
	}
	if peerIP != "" {
		steps = append(steps, []string{"route", "replace", peerIP + "/32", "dev", actual})
	}
	for _, a := range steps {
		if out, err := exec.Command("ip", a...).CombinedOutput(); err != nil {
			d.Close()
			return nil, fmt.Errorf("tun: ip %s: %v: %s", strings.Join(a, " "), err, strings.TrimSpace(string(out)))
		}
	}
	return d, nil
}

func (d *Device) Name() string { return d.name }
func (d *Device) MTU() int     { return d.mtu }

// Offloaded reports whether the kernel took the TCP offloads.
func (d *Device) Offloaded() bool { return d.offload }

// OffloadStats is what the offloads did since the device opened.
func (d *Device) OffloadStats() OffloadCounts {
	return OffloadCounts{Reads: d.reads.Load(), Segs: d.segsOut.Load(), Writes: d.writes.Load(), Pkts: d.pktsIn.Load(),
		Dropped: d.dropped.Load(), RefusedMerges: d.refused.Load()}
}

// Read returns one IP packet. With offloads one kernel read may give several
// (a TCP packet of up to 64 KB is cut into MTU-sized segments): they are
// handed out in turn, with no syscall until they are gone.
func (d *Device) Read(p []byte) (int, error) {
	if !d.offload {
		return d.f.Read(p)
	}
	d.rmu.Lock()
	defer d.rmu.Unlock()
	for {
		if d.single != nil {
			s := d.single
			d.single = nil
			if len(s) > len(p) {
				d.dropped.Add(1)
				continue
			}
			return copy(p, s), nil
		}
		if d.segi < len(d.ends) {
			start := 0
			if d.segi > 0 {
				start = d.ends[d.segi-1]
			}
			s := d.segs[start:d.ends[d.segi]]
			d.segi++
			if len(s) > len(p) {
				d.dropped.Add(1)
				continue
			}
			return copy(p, s), nil
		}
		n, err := d.f.Read(d.rbuf)
		if err != nil {
			return 0, err
		}
		d.reads.Add(1)
		if err := d.unpack(d.rbuf[:n]); err != nil {
			d.dropped.Add(1)
		}
	}
}

// unpack turns one kernel read into the packets Read hands out.
func (d *Device) unpack(b []byte) error {
	var h vnetHdr
	if err := h.decode(b); err != nil {
		return err
	}
	pkt := b[vnetHdrLen:]
	d.segs, d.ends, d.segi = d.segs[:0], d.ends[:0], 0
	switch h.gsoType &^ gsoECN {
	case gsoNone:
		if h.flags&vnetNeedsCsum != 0 {
			if err := completeCsum(pkt, int(h.csumStart), int(h.csumOffset)); err != nil {
				return err
			}
		}
		d.single = pkt
		d.segsOut.Add(1)
		return nil
	case gsoTCPv4:
		var err error
		d.segs, d.ends, err = splitTCP(pkt, int(h.gsoSize), d.segs, d.ends)
		if err != nil {
			d.ends = d.ends[:0]
			return err
		}
		d.segsOut.Add(uint64(len(d.ends)))
		return nil
	}
	return errUnsuppGS
}

// Write injects one IP packet.
func (d *Device) Write(p []byte) (int, error) {
	if !d.offload {
		return d.f.Write(p)
	}
	bp := wbufPool.Get().(*[]byte)
	b := append((*bp)[:0], make([]byte, vnetHdrLen)...) // a zero header: a plain packet
	b = append(b, p...)
	_, err := d.f.Write(b)
	*bp = b[:0]
	wbufPool.Put(bp)
	if err != nil {
		return 0, err
	}
	d.writes.Add(1)
	d.pktsIn.Add(1)
	return len(p), nil
}

// WriteBatch injects several IP packets: with offloads, consecutive segments
// of one TCP connection go in as one (see coalesce); each other packet as it
// is. Without offloads it writes them one by one. A packet the kernel refuses
// is skipped, not the rest of the batch: n counts the packets that went in,
// err is the first refusal.
func (d *Device) WriteBatch(pkts [][]byte) (n int, err error) {
	keep := func(e error) {
		if err == nil {
			err = e
		}
	}
	if !d.offload || len(pkts) == 1 {
		for _, p := range pkts {
			if _, e := d.Write(p); e != nil {
				keep(e)
				continue
			}
			n++
		}
		return n, err
	}
	gs := groPool.Get().(*groScratch)
	defer groPool.Put(gs)
	gs.items = coalesce(pkts, gs.items[:0], gs.open)
	for i := range gs.items {
		it := &gs.items[i]
		if len(it.segs) == 1 {
			if _, e := d.Write(pkts[it.segs[0]]); e != nil {
				keep(e)
				continue
			}
			n++
			continue
		}
		gs.buf = buildMerged(gs.buf, pkts, it)
		if _, e := d.f.Write(gs.buf); e != nil {
			// The kernel refused the merged packet: offer its segments one
			// by one, so one bad merge cannot lose a whole run.
			d.refused.Add(1)
			for _, si := range it.segs {
				if _, e2 := d.Write(pkts[si]); e2 != nil {
					keep(e)
					continue
				}
				n++
			}
			continue
		}
		d.writes.Add(1)
		d.pktsIn.Add(uint64(len(it.segs)))
		n += len(it.segs)
	}
	return n, err
}

// groScratch is one WriteBatch's working space (carriers write concurrently).
type groScratch struct {
	items []groItem
	open  map[[12]byte]int
	buf   []byte
}

var (
	groPool  = sync.Pool{New: func() any { return &groScratch{open: map[[12]byte]int{}, buf: make([]byte, 0, vnetHdrLen+65535)} }}
	wbufPool = sync.Pool{New: func() any { b := make([]byte, 0, 2048); return &b }}
)

func (d *Device) Close() error { return d.f.Close() }
