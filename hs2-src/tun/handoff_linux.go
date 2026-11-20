package tun

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ZDU (proposals/zdu350.md, B7): the TUN fd handed over by the old process.
//
// The old process sends Device.File() over the hand-off channel; the new
// process registers it with AdoptFD before it opens its TUN. OpenWith for
// that name then wraps the fd (OpenFromFD) and never runs `ip link del`,
// at either of its two sites: deleting the device would cut every packet in
// flight and drop the address and routes the operator put on it. The
// device lives while any fd to it is open (old and new process both hold
// one during the drain). A normal stop is unchanged: the last close of a
// non-persistent TUN removes the device, and the next cold start's
// `ip link del` clears a leftover as today.

const tunGetIff = 0x800454d2 // TUNGETIFF

// linkDel removes a stale interface (OpenWith). A variable for the tests.
var linkDel = func(name string) { exec.Command("ip", "link", "del", name).Run() }

var handed struct {
	mu sync.Mutex
	m  map[string]*os.File
}

// AdoptFD registers f, a TUN fd received from the old process, for name.
// It checks that f is a TUN device of that name first. OpenWith(name, ...)
// takes it (once).
func AdoptFD(name string, f *os.File) error {
	if err := CheckFD(name, f); err != nil {
		return err
	}
	handed.mu.Lock()
	defer handed.mu.Unlock()
	if handed.m == nil {
		handed.m = map[string]*os.File{}
	}
	if old := handed.m[name]; old != nil {
		old.Close()
	}
	handed.m[name] = f
	return nil
}

// CheckFD reports whether f is a TUN fd (IFF_TUN|IFF_NO_PI) of device name.
func CheckFD(name string, f *os.File) error {
	if f == nil {
		return fmt.Errorf("tun: handed-off fd for %q missing", name)
	}
	actual, _, err := fdInfo(f)
	if err != nil {
		return err
	}
	if actual != name {
		return fmt.Errorf("tun: handed-off fd is %q, want %q", actual, name)
	}
	return nil
}

// DropHanded closes every handed-off fd not opened yet.
func DropHanded() {
	handed.mu.Lock()
	defer handed.mu.Unlock()
	for k, f := range handed.m {
		f.Close()
		delete(handed.m, k)
	}
}

// Adopted reports whether a handed-off fd is waiting for name.
func Adopted(name string) bool {
	handed.mu.Lock()
	defer handed.mu.Unlock()
	return handed.m[name] != nil
}

func takeHanded(name string, mtu int) (*Device, bool, error) {
	handed.mu.Lock()
	f := handed.m[name]
	delete(handed.m, name)
	handed.mu.Unlock()
	if f == nil {
		return nil, false, nil
	}
	d, err := OpenFromFD(f, name, mtu)
	if err != nil {
		f.Close()
	} else {
		d.gate = readGate.c
	}
	return d, true, err
}

// fdInfo reads the device name and flags of a TUN fd (TUNGETIFF).
func fdInfo(f *os.File) (string, uint16, error) {
	var ifr [unix.IFNAMSIZ + 64]byte
	rc, err := f.SyscallConn()
	if err != nil {
		return "", 0, fmt.Errorf("tun: handed-off fd: %w", err)
	}
	var e unix.Errno
	if err := rc.Control(func(fd uintptr) {
		_, _, e = unix.Syscall(unix.SYS_IOCTL, fd, tunGetIff, uintptr(unsafe.Pointer(&ifr[0])))
	}); err != nil {
		return "", 0, fmt.Errorf("tun: handed-off fd: %w", err)
	}
	if e != 0 {
		return "", 0, fmt.Errorf("tun: handed-off fd: TUNGETIFF: %v", e)
	}
	flags := *(*uint16)(unsafe.Pointer(&ifr[unix.IFNAMSIZ]))
	if flags&unix.IFF_TUN == 0 || flags&unix.IFF_NO_PI == 0 {
		return "", 0, fmt.Errorf("tun: handed-off fd: flags %#x, want IFF_TUN|IFF_NO_PI", flags)
	}
	return strings.TrimRight(string(ifr[:unix.IFNAMSIZ]), "\x00"), flags, nil
}

// OpenFromFD wraps a live TUN fd: no `ip link del`, no address or route
// change. The offload mode follows the fd (IFF_VNET_HDR cannot change on a
// live fd, so the new process reads and writes the way the fd was opened).
// The MTU is set only when the config changed it.
func OpenFromFD(f *os.File, name string, mtu int) (*Device, error) {
	actual, flags, err := fdInfo(f)
	if err != nil {
		return nil, err
	}
	if actual != name {
		return nil, fmt.Errorf("tun: handed-off fd is %q, want %q", actual, name)
	}
	offload := flags&unix.IFF_VNET_HDR != 0
	d := &Device{f: f, name: actual, mtu: mtu, offload: offload}
	if offload {
		d.rbuf = make([]byte, vnetHdrLen+65535)
		d.segs = make([]byte, 0, 128<<10)
	}
	if ifc, err := net.InterfaceByName(actual); err == nil && mtu > 0 && ifc.MTU != mtu {
		if out, err := exec.Command("ip", "link", "set", "dev", actual, "mtu", strconv.Itoa(mtu)).CombinedOutput(); err != nil {
			return nil, fmt.Errorf("tun: mtu %d on %s: %v: %s", mtu, actual, err, strings.TrimSpace(string(out)))
		}
	}
	return d, nil
}

// File is the device fd, for the hand-off (sent as SCM_RIGHTS, which dups
// it; this Device keeps its own).
func (d *Device) File() *os.File { return d.f }

// readGate holds Read on handed-off devices until the hand-off's GO.
var readGate = struct {
	c    chan struct{}
	once sync.Once
}{c: make(chan struct{})}

// OpenReadGate lets handed-off devices read (the new process got GO).
func OpenReadGate() { readGate.once.Do(func() { close(readGate.c) }) }

// StopReads (ZDU S7, the old process once its handover passed probation):
// Read takes no more packets from the kernel queue, which the new process
// shares through the handed-over fd, so every packet the kernel queues from
// now on goes to the new process and none is lost when this process exits.
// Packets Read already took (a read in progress, offload segments already
// split) are still handed out and sent on this process's links, which are up
// for the drain. Writes are unchanged: replies arriving on the draining
// links are still written to the device. A read blocked in the kernel at
// the call takes one more packet at most, while this process still serves.
func (d *Device) StopReads() {
	d.offMu.Lock()
	if d.offCh == nil {
		d.offCh = make(chan struct{})
	}
	d.offMu.Unlock()
	d.readsOff.Store(true)
}

// ResumeReads undoes StopReads.
func (d *Device) ResumeReads() {
	d.offMu.Lock()
	d.readsOff.Store(false)
	if d.offCh != nil {
		close(d.offCh)
		d.offCh = nil
	}
	d.offMu.Unlock()
}

// ReadsStopped reports StopReads in effect.
func (d *Device) ReadsStopped() bool { return d.readsOff.Load() }

func (d *Device) waitReadsOn() {
	for d.readsOff.Load() {
		d.offMu.Lock()
		ch := d.offCh
		d.offMu.Unlock()
		if ch == nil {
			continue // ResumeReads in progress
		}
		<-ch
	}
}
