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
	"unsafe"

	"golang.org/x/sys/unix"
)

const tunSetIff = 0x400454ca // TUNSETIFF

type Device struct {
	f    *os.File
	name string
	mtu  int
}

// Open creates (or attaches to) a TUN interface, assigns local/prefix with an
// explicit peer route, sets MTU and brings it up.
func Open(name, localCIDR, peerIP string, mtu int) (*Device, error) {
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("tun: open /dev/net/tun: %w (need root/CAP_NET_ADMIN, tun module)", err)
	}
	// Remove a stale interface of the same name left by a crashed/killed prior
	// instance, so restart never fails with "device or resource busy".
	exec.Command("ip", "link", "del", name).Run()
	var ifr [unix.IFNAMSIZ + 64]byte
	copy(ifr[:unix.IFNAMSIZ-1], name)
	*(*uint16)(unsafe.Pointer(&ifr[unix.IFNAMSIZ])) = unix.IFF_TUN | unix.IFF_NO_PI
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), tunSetIff, uintptr(unsafe.Pointer(&ifr[0]))); e != 0 {
		unix.Close(fd)
		return nil, fmt.Errorf("tun: TUNSETIFF %s: %v", name, e)
	}
	actual := strings.TrimRight(string(ifr[:unix.IFNAMSIZ]), "\x00")
	d := &Device{f: os.NewFile(uintptr(fd), "/dev/net/tun"), name: actual, mtu: mtu}
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

// Read returns one IP packet.
func (d *Device) Read(p []byte) (int, error) { return d.f.Read(p) }

// Write injects one IP packet.
func (d *Device) Write(p []byte) (int, error) { return d.f.Write(p) }

func (d *Device) Close() error { return d.f.Close() }
