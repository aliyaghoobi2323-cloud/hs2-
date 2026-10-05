//go:build linux

package tun

import (
	"encoding/binary"
	"errors"
	"net"
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// As root the test binary re-executes itself in a private network namespace
// (unshare -n), where it may create TUN devices; otherwise these tests skip.
const netnsEnv = "HS2_TUN_NETNS"

func TestMain(m *testing.M) {
	if os.Geteuid() == 0 && os.Getenv(netnsEnv) == "" {
		ok := make(chan bool, 1)
		go func() { runtime.LockOSThread(); ok <- unix.Unshare(unix.CLONE_NEWNET) == nil }()
		if path, err := exec.LookPath("unshare"); err == nil && <-ok {
			cmd := exec.Command(path, append([]string{"-n", "--", os.Args[0]}, os.Args[1:]...)...)
			cmd.Env = append(os.Environ(), netnsEnv+"=1")
			cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
			err := cmd.Run()
			var ee *exec.ExitError
			switch {
			case err == nil:
				os.Exit(0)
			case errors.As(err, &ee):
				os.Exit(ee.ExitCode())
			}
		}
	}
	if os.Getenv(netnsEnv) == "1" {
		exec.Command("ip", "link", "set", "lo", "up").Run()
	}
	os.Exit(m.Run())
}

func needNetns(t *testing.T) {
	t.Helper()
	if os.Getenv(netnsEnv) != "1" {
		t.Skip("TUN tests need root and a private network namespace")
	}
}

// With offloads the kernel's packets come out complete (a partial UDP
// checksum finished), and packets written in (one by one and in a batch)
// reach a local socket — the kernel verifies their checksums.
func TestOffloadDeviceUDP(t *testing.T) {
	needNetns(t)
	d, err := OpenWith("hs2t0", "10.9.0.1/30", "10.9.0.2", 1280, Options{Offload: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if !d.Offloaded() {
		t.Fatal("the kernel refused the offloads (want them on this kernel)")
	}
	// kernel → tun
	c, err := net.DialUDP("udp4", &net.UDPAddr{IP: net.IPv4(10, 9, 0, 1), Port: 7001}, &net.UDPAddr{IP: net.IPv4(10, 9, 0, 2), Port: 7002})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("out through the tun"))
	buf := make([]byte, 2048)
	deadline := time.Now().Add(3 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("the UDP packet never came out of the tun")
		}
		n, err := d.Read(buf)
		if err != nil {
			t.Fatal(err)
		}
		p := buf[:n]
		if n < 28 || p[9] != 17 || binary.BigEndian.Uint16(p[22:]) != 7002 {
			continue // IPv6 router solicitations and the like
		}
		u := p[20:]
		if csumFold(csumAdd(pseudo4(p[12:16], p[16:20], 17, len(u)), u)) != 0xffff {
			t.Fatal("the packet's UDP checksum is not complete")
		}
		if string(u[8:]) != "out through the tun" {
			t.Fatalf("payload %q", u[8:])
		}
		break
	}
	// tun → kernel
	ln, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(10, 9, 0, 1), Port: 7003})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if _, err := d.Write(udpPkt([4]byte{10, 9, 0, 2}, [4]byte{10, 9, 0, 1}, 9, 7003, []byte("one"))); err != nil {
		t.Fatal(err)
	}
	// A packet the kernel refuses (not IP) costs only itself.
	if n, err := d.WriteBatch([][]byte{
		udpPkt([4]byte{10, 9, 0, 2}, [4]byte{10, 9, 0, 1}, 9, 7003, []byte("two")),
		make([]byte, 40),
		udpPkt([4]byte{10, 9, 0, 2}, [4]byte{10, 9, 0, 1}, 9, 7003, []byte("three")),
	}); n != 2 || err == nil {
		t.Fatalf("WriteBatch = %d, %v; want 2 and the refusal", n, err)
	}
	ln.SetReadDeadline(time.Now().Add(3 * time.Second))
	for _, want := range []string{"one", "two", "three"} {
		n, _, err := ln.ReadFromUDP(buf)
		if err != nil {
			t.Fatalf("waiting for %q: %v", want, err)
		}
		if string(buf[:n]) != want {
			t.Fatalf("got %q, want %q", buf[:n], want)
		}
	}
}
