//go:build linux

package tun

import (
	"net"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

// countDel swaps linkDel for a counter that still deletes (so a normal open
// behaves as in prod).
func countDel(t *testing.T) *int {
	t.Helper()
	n := new(int)
	orig := linkDel
	linkDel = func(name string) { *n++; orig(name) }
	t.Cleanup(func() { linkDel = orig })
	return n
}

func dupFile(t *testing.T, f *os.File) *os.File {
	t.Helper()
	fd, err := unix.Dup(int(f.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	unix.CloseOnExec(fd)
	return os.NewFile(uintptr(fd), "tun-dup")
}

// S1 correction / B7: a handed-off TUN fd is used as is; neither `ip link
// del` site of OpenWith runs, the device (same ifindex, address kept) lives
// on after the old process closes its fd, and a normal stop (last close)
// still removes it; the next normal open still clears a stale name.
func TestZDUTunHandoffNoDelete(t *testing.T) {
	needNetns(t)
	for _, off := range []bool{false, true} {
		dels := countDel(t)
		const name = "zdu5"
		old, err := OpenWith(name, "10.231.0.1/30", "", 1400, Options{Offload: off})
		if err != nil {
			t.Fatal(err)
		}
		if *dels == 0 {
			t.Fatal("a normal open must clear a stale device first")
		}
		ifc0, err := net.InterfaceByName(name)
		if err != nil {
			t.Fatal(err)
		}
		*dels = 0
		if err := AdoptFD(name, dupFile(t, old.File())); err != nil {
			t.Fatal(err)
		}
		if !Adopted(name) {
			t.Fatal("not adopted")
		}
		nw, err := OpenWith(name, "10.231.0.1/30", "", 1400, Options{Offload: !off}) // option ignored: the fd decides
		if err != nil {
			t.Fatal(err)
		}
		if *dels != 0 {
			t.Fatalf("offload=%v: ip link del ran %d times on a handed-off fd", off, *dels)
		}
		if nw.Offloaded() != old.Offloaded() {
			t.Fatalf("offload %v vs %v: must follow the fd", nw.Offloaded(), old.Offloaded())
		}
		if Adopted(name) {
			t.Fatal("handed fd taken twice")
		}
		old.Close() // the old process exits
		ifc1, err := net.InterfaceByName(name)
		if err != nil || ifc1.Index != ifc0.Index {
			t.Fatalf("device gone or recreated after the old close: %v %v/%v", err, ifc1, ifc0)
		}
		addrs, _ := ifc1.Addrs()
		if len(addrs) == 0 {
			t.Fatal("address lost")
		}
		nw.Close() // a normal stop of the new process
		if _, err := net.InterfaceByName(name); err == nil {
			t.Fatal("device still there after a normal stop")
		}
	}
}

// A fd that is not a TUN of the right name is refused (the caller then
// opens cold).
func TestZDUTunAdoptRejects(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if err := AdoptFD("zdu5", r); err == nil {
		t.Fatal("a pipe adopted as TUN")
	}
	if Adopted("zdu5") {
		t.Fatal("adopted after error")
	}
	needNetns(t)
	d, err := Open("zdu6", "10.231.1.1/30", "", 1400)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	f := dupFile(t, d.File())
	defer f.Close()
	if err := AdoptFD("zdu7", f); err == nil {
		t.Fatal("wrong name adopted")
	}
}
