// kprobe: the kernel interfaces hs2's R1 cores use, probed on the running
// kernel (standard library only, so `go run` builds it anywhere and the
// binary runs on any Linux of its architecture). Each line: the interface,
// the kernel that has it, what the code does without it, and whether this
// kernel has it. Exit 1 if one the cores need is missing.
package main

import (
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
	"unsafe"
)

const (
	tcpThinLinearTimeouts = 16
	tcpUserTimeout        = 18
	tcpNotsentLowat       = 25
	soMaxPacingRate       = 47
	siocOutq              = 0x5411 // TIOCOUTQ
	siocOutqNSD           = 0x894B
	// tcp_info: the code reads up to tcpi_rcv_ooopack (offset 224, 4
	// bytes: Linux 5.4); tcpi_delivery_rate is at 160 (4.9) and the
	// app-limited bit is bit 0 of byte 7 (4.9).
	tcpInfoNeed = 228
)

type row struct {
	name, since, without string
	ok                   bool
	note                 string
}

func main() {
	var rows []row
	add := func(name, since, without string, ok bool, note string) {
		rows = append(rows, row{name, since, without, ok, note})
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Println("listen:", err)
		os.Exit(2)
	}
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			defer c.Close()
			buf := make([]byte, 4096)
			for {
				if _, err := c.Read(buf); err != nil {
					return
				}
			}
		}
	}()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		fmt.Println("dial:", err)
		os.Exit(2)
	}
	defer c.Close()
	c.Write(make([]byte, 64<<10))
	raw, _ := c.(*net.TCPConn).SyscallConn()
	raw.Control(func(fd uintptr) {
		s := int(fd)
		setget := func(level, opt, v int) (int, error) {
			if err := syscall.SetsockoptInt(s, level, opt, v); err != nil {
				return 0, err
			}
			return syscall.GetsockoptInt(s, level, opt)
		}
		v, err := setget(syscall.IPPROTO_TCP, tcpNotsentLowat, 11584)
		add("TCP_NOTSENT_LOWAT", "3.12", "C3 falls back to the fixed mark and the old send gate (TestHostQNoSyscalls)", err == nil && v == 11584, fmt.Sprint(v, err))
		var n int32
		_, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, siocOutqNSD, uintptr(unsafe.Pointer(&n)))
		add("SIOCOUTQNSD", "3.x", "C3 falls back as above", e == 0, fmt.Sprint(n, " ", e))
		_, _, e = syscall.Syscall(syscall.SYS_IOCTL, fd, siocOutq, uintptr(unsafe.Pointer(&n)))
		add("SIOCOUTQ", "2.x", "the BULK in-flight budget is off (newLinkBudget nil)", e == 0, fmt.Sprint(n, " ", e))
		info := make([]byte, 512)
		l := uint32(len(info))
		_, _, e = syscall.Syscall6(syscall.SYS_GETSOCKOPT, fd, syscall.IPPROTO_TCP, syscall.TCP_INFO, uintptr(unsafe.Pointer(&info[0])), uintptr(unsafe.Pointer(&l)), 0)
		add("TCP_INFO up to tcpi_rcv_ooopack", "5.4", "fields past the kernel's length read 0 (C4-lite: no delivery rate below 4.9, TestSendCapNoDeliveryRate)", e == 0 && l >= tcpInfoNeed, fmt.Sprintf("length %d, need %d", l, tcpInfoNeed))
		err = syscall.SetsockoptInt(s, syscall.SOL_SOCKET, soMaxPacingRate, 1<<20) // (u32 is taken on every kernel that has it)
		add("SO_MAX_PACING_RATE", "3.13", "no pacing cap (C4-lite, the BULK budget's pace)", err == nil, fmt.Sprint(err))
		_, err = setget(syscall.IPPROTO_TCP, tcpThinLinearTimeouts, 1)
		add("TCP_THIN_LINEAR_TIMEOUTS", "2.6.34", "a QUIET link keeps the exponential RTO back-off (best effort)", err == nil, fmt.Sprint(err))
		_, err = setget(syscall.IPPROTO_TCP, tcpUserTimeout, 30000)
		add("TCP_USER_TIMEOUT", "2.6.37", "link death found later", err == nil, fmt.Sprint(err))
		err = syscall.SetsockoptString(s, syscall.IPPROTO_TCP, syscall.TCP_CONGESTION, "cubic")
		add("TCP_CONGESTION cubic (QUIET links)", "2.6.13", "a QUIET link keeps the default", err == nil, fmt.Sprint(err))
		_, err = setget(syscall.IPPROTO_TCP, syscall.TCP_MAXSEG, 1460)
		add("TCP_MAXSEG (panel socket)", "2.x", "the route's segment", err == nil, fmt.Sprint(err))
		// (no getter for the FORCE option: SO_SNDBUF reads it back, doubled)
		err = syscall.SetsockoptInt(s, syscall.SOL_SOCKET, syscall.SO_SNDBUFFORCE, 8<<20)
		sb, _ := syscall.GetsockoptInt(s, syscall.SOL_SOCKET, syscall.SO_SNDBUF)
		add("SO_SNDBUFFORCE (BULK send buffer)", "2.6.14", "SO_SNDBUF within wmem_max", err == nil && sb >= 8<<20, fmt.Sprint("SO_SNDBUF ", sb, " ", err))
	})
	cc, _ := os.ReadFile("/proc/sys/net/ipv4/tcp_available_congestion_control")
	add("BBR available", "4.9", "links use the kernel's default", strings.Contains(string(cc), "bbr"), strings.TrimSpace(string(cc))+" (bbr is loaded on first use: see tcp_allowed_congestion_control)")
	q, _ := os.ReadFile("/proc/sys/net/core/default_qdisc")
	add("default qdisc", "-", "-", true, strings.TrimSpace(string(q)))
	var u syscall.Utsname
	syscall.Uname(&u)
	rel := make([]byte, 0, 65)
	for _, b := range u.Release {
		if b == 0 {
			break
		}
		rel = append(rel, byte(b))
	}
	fmt.Printf("kernel %s\n", rel)
	missing := 0
	for _, r := range rows {
		st := "ok"
		if !r.ok {
			st = "MISSING"
			if r.name != "BBR available" {
				missing++
			}
		}
		fmt.Printf("%-7s %-36s since %-7s | %s | without: %s\n", st, r.name, r.since, r.note, r.without)
	}
	if missing > 0 {
		fmt.Printf("%d interface(s) the cores use are missing: their fallback runs\n", missing)
		os.Exit(1)
	}
}
