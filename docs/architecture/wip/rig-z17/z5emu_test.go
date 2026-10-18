package engine

// Gap 4 judge rig, part 1 (scratch, not part of the product; from z5-depth): a path emulator in
// the kernel's own packet path, so the two ends of a link are real kernel TCP
// sockets (BBR, TCP_NOTSENT_LOWAT, real cwnd and pacing) that see a real
// round trip and a real bottleneck queue — unlike a userspace TCP relay,
// which ends each TCP connection at the relay and hides the sender's own
// queue in the relay's receive buffer.
//
// One TUN device (IFF_TUN|IFF_NO_PI) carries 10.214.0.1/16. The edge dials a
// fake exit address (z5ExitIP) routed out the TUN; the emulator rewrites
// each packet (src -> z5EdgeIP, dst -> the local address) and writes it back
// in, where the exit's listener takes it; the exit's replies to z5EdgeIP
// leave the TUN and come back as from z5ExitIP. Each direction has its own
// bottleneck (rate, drop-tail queue in bytes) and one-way delay. ACKs share
// the bottleneck of their direction with data, as on a real path.

import (
	"fmt"
	"math/rand/v2"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

var (
	z5Local  = [4]byte{10, 214, 0, 1}
	z5ExitIP = [4]byte{10, 214, 1, 1} // what the edge dials
	z5EdgeIP = [4]byte{10, 214, 2, 1} // where the exit's packets go
)

type z5Path struct {
	Rate  float64       // bottleneck bytes/s (0: none)
	Delay time.Duration // one-way delay
	Queue int           // bottleneck queue, bytes (drop-tail)
	Loss  float64       // random loss, share of packets (0: none)
}

type z5Pkt struct {
	at int64 // ns since emu start: when it leaves the path
	b  []byte
}

type z5Dir struct {
	p        z5Path
	nextFree int64 // ns since emu start when the bottleneck is idle again (reader only)
	out      chan z5Pkt
	bytes    atomic.Int64 // IP bytes delivered
	pkts     atomic.Int64
	drops    atomic.Int64
	maxQ     atomic.Int64 // largest queue seen, bytes
	nf       atomic.Int64 // delay rig: a copy of nextFree for the tracer
	chg      atomic.Pointer[z5Path] // speed-change rig: the path's rate and queue now (nil: p)
	// z17: a token-bucket policer in front of the bottleneck (nil: none):
	// what finds the bucket short is dropped (polDrops), with no queue
	pol      atomic.Pointer[z5Pol]
	polTok   float64 // bytes (reader only)
	polAt    int64   // ns of the last refill
	polDrops atomic.Int64
}

// z5Pol is a policer: rate bytes/s, a bucket of burst bytes.
type z5Pol struct {
	rate, burst float64
}

type z5Emu struct {
	fd     int
	name   string
	t0     time.Time
	up, dn z5Dir // up: edge -> exit, dn: exit -> edge
}

func (e *z5Emu) now() int64 { return int64(time.Since(e.t0)) }

// delay rig: the emulator's goroutines each keep an OS thread of their own,
// so their CPU (with the kernel's packet work done in their context: what
// they write into the TUN is received there) can be told from the rest
// (cpuS): emuCPU.
var z5EmuTids sync.Map

func (e *z5Emu) lockThread() {
	runtime.LockOSThread()
	z5EmuTids.Store(unix.Gettid(), true)
}

// z5EmuCPU is the CPU seconds the emulator's threads used so far.
func z5EmuCPU() float64 {
	var s float64
	z5EmuTids.Range(func(k, _ any) bool {
		b, err := os.ReadFile(fmt.Sprintf("/proc/self/task/%d/stat", k.(int)))
		if err != nil {
			return true
		}
		f := strings.Fields(string(b[strings.LastIndexByte(string(b), ')')+2:]))
		// fields from state (3rd): utime is the 14th, stime the 15th
		if len(f) > 12 {
			u, _ := strconv.ParseFloat(f[11], 64)
			st, _ := strconv.ParseFloat(f[12], 64)
			s += (u + st) / 100
		}
		return true
	})
	return s
}

// qms is the bottleneck queue of d now, in ms (delay rig tracer).
func (e *z5Emu) qms(d *z5Dir) float64 {
	q := d.nf.Load() - e.now()
	if q < 0 {
		return 0
	}
	return float64(q) / 1e6
}

// newZ5Emu opens the TUN device and starts the emulator; the device goes
// away with the process.
func newZ5Emu(up, dn z5Path, mtu int) (*z5Emu, error) {
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	name := fmt.Sprintf("z5d%d", os.Getpid()%100000)
	var ifr [unix.IFNAMSIZ + 64]byte
	copy(ifr[:unix.IFNAMSIZ-1], name)
	*(*uint16)(unsafe.Pointer(&ifr[unix.IFNAMSIZ])) = uint16(unix.IFF_TUN | unix.IFF_NO_PI)
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(unix.TUNSETIFF), uintptr(unsafe.Pointer(&ifr[0]))); e != 0 {
		unix.Close(fd)
		return nil, fmt.Errorf("TUNSETIFF: %v", e)
	}
	s, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(s)
	set := func(req uint, f func(*unix.Ifreq)) error {
		r, err := unix.NewIfreq(name)
		if err != nil {
			return err
		}
		f(r)
		return unix.IoctlIfreq(s, req, r)
	}
	if err := set(unix.SIOCSIFADDR, func(r *unix.Ifreq) { r.SetInet4Addr(z5Local[:]) }); err != nil {
		return nil, fmt.Errorf("SIOCSIFADDR: %v", err)
	}
	if err := set(unix.SIOCSIFNETMASK, func(r *unix.Ifreq) { r.SetInet4Addr([]byte{255, 255, 0, 0}) }); err != nil {
		return nil, fmt.Errorf("SIOCSIFNETMASK: %v", err)
	}
	if err := set(unix.SIOCSIFMTU, func(r *unix.Ifreq) { r.SetUint32(uint32(mtu)) }); err != nil {
		return nil, fmt.Errorf("SIOCSIFMTU: %v", err)
	}
	// z11: Z5_TXQ packets of TUN transmit queue (the kernel's side, before
	// the emulator reads a packet; default 500): a sender paced at 2.89 times
	// a 1 Gbit/s path's rate overflows 500 packets, and those drops are no
	// drops of the emulated bottleneck.
	if q := z5Env("Z5_TXQ", 0); q > 0 {
		if err := set(unix.SIOCSIFTXQLEN, func(r *unix.Ifreq) { r.SetUint32(uint32(q)) }); err != nil {
			return nil, fmt.Errorf("SIOCSIFTXQLEN: %v", err)
		}
	}
	if err := set(unix.SIOCSIFFLAGS, func(r *unix.Ifreq) { r.SetUint16(unix.IFF_UP | unix.IFF_RUNNING) }); err != nil {
		return nil, fmt.Errorf("SIOCSIFFLAGS: %v", err)
	}
	e := &z5Emu{fd: fd, name: name, t0: time.Now()}
	e.up.p, e.dn.p = up, dn
	e.up.out = make(chan z5Pkt, 1<<17)
	e.dn.out = make(chan z5Pkt, 1<<17)
	go func() { e.lockThread(); e.readLoop(mtu) }()
	go func() { e.lockThread(); e.deliver(&e.up) }()
	go func() { e.lockThread(); e.deliver(&e.dn) }()
	return e, nil
}

func (e *z5Emu) readLoop(mtu int) {
	buf := make([]byte, mtu+64)
	for {
		n, err := unix.Read(e.fd, buf)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			return
		}
		if n < 20 || buf[0]>>4 != 4 {
			continue
		}
		var dst [4]byte
		copy(dst[:], buf[16:20])
		var d *z5Dir
		var nsrc [4]byte
		switch dst {
		case z5ExitIP:
			d, nsrc = &e.up, z5EdgeIP
		case z5EdgeIP:
			d, nsrc = &e.dn, z5ExitIP
		default:
			continue
		}
		if d.p.Loss > 0 && rand.Float64() < d.p.Loss {
			d.drops.Add(1)
			continue
		}
		now := e.now()
		if pl := d.pol.Load(); pl != nil {
			if d.polAt == 0 {
				d.polTok = pl.burst
			} else {
				d.polTok = min(pl.burst, d.polTok+float64(now-d.polAt)*pl.rate/1e9)
			}
			d.polAt = now
			if d.polTok < float64(n) {
				d.drops.Add(1)
				d.polDrops.Add(1)
				continue
			}
			d.polTok -= float64(n)
		}
		at := now
		p := d.p
		if c := d.chg.Load(); c != nil {
			p = *c
		}
		if p.Rate > 0 {
			if d.nextFree < now {
				d.nextFree = now
			}
			q := int64(float64(d.nextFree-now) * p.Rate / 1e9)
			if int(q)+n > p.Queue {
				d.drops.Add(1)
				continue
			}
			if q+int64(n) > d.maxQ.Load() {
				d.maxQ.Store(q + int64(n))
			}
			d.nextFree += int64(float64(n) / p.Rate * 1e9)
			d.nf.Store(d.nextFree)
			at = d.nextFree
		}
		at += int64(d.p.Delay)
		b := make([]byte, n)
		copy(b, buf[:n])
		z5Rewrite(b, nsrc, z5Local)
		d.out <- z5Pkt{at: at, b: b}
	}
}

func (e *z5Emu) deliver(d *z5Dir) {
	for pk := range d.out {
		if w := pk.at - e.now(); w > 0 {
			time.Sleep(time.Duration(w))
		}
		unix.Write(e.fd, pk.b)
		d.bytes.Add(int64(len(pk.b)))
		d.pkts.Add(1)
	}
}

// z5Rewrite sets an IPv4 packet's addresses, adjusting the IP and TCP
// checksums incrementally (RFC 1624).
func z5Rewrite(b []byte, src, dst [4]byte) {
	var osrc, odst [4]byte
	copy(osrc[:], b[12:16])
	copy(odst[:], b[16:20])
	copy(b[12:16], src[:])
	copy(b[16:20], dst[:])
	ihl := int(b[0]&0x0f) * 4
	z5Adjust(b[10:12], osrc, src, odst, dst)
	if b[9] == unix.IPPROTO_TCP && len(b) >= ihl+18 {
		z5Adjust(b[ihl+16:ihl+18], osrc, src, odst, dst)
	}
}

func z5Adjust(sum []byte, o1, n1, o2, n2 [4]byte) {
	s := uint32(^(uint16(sum[0])<<8 | uint16(sum[1])))
	for _, p := range [][2][4]byte{{o1, n1}, {o2, n2}} {
		for i := 0; i < 4; i += 2 {
			o := uint16(p[0][i])<<8 | uint16(p[0][i+1])
			n := uint16(p[1][i])<<8 | uint16(p[1][i+1])
			s += uint32(^o) + uint32(n)
		}
	}
	for s>>16 != 0 {
		s = s&0xffff + s>>16
	}
	c := ^uint16(s)
	sum[0], sum[1] = byte(c>>8), byte(c)
}
