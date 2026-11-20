//go:build linux

package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/handoff"
)

const takeFailEnv = "HS2_ZDU_TAKEFAIL_CHILD"

func openFDs() []string {
	ents, _ := os.ReadDir("/proc/self/fd")
	out := make([]string, 0, len(ents))
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

// TestZDUTakeFailChild is the re-exec'd hand-off child (not a test on its own).
func TestZDUTakeFailChild(t *testing.T) {
	if os.Getenv(takeFailEnv) == "" {
		t.Skip("helper")
	}
	// Init the netpoller (epoll + eventfd) before counting fds.
	if l, err := net.Listen("tcp", "127.0.0.1:0"); err == nil {
		l.Close()
	}
	start := len(openFDs()) // includes the readdir fd itself and fd 3 when passed
	has3 := os.Getenv("HS2_HANDOVER_FD") == "3"
	zduExit = func(code int) {
		n := len(openFDs())
		want := start
		if has3 {
			want-- // fd 3 (the channel) must be gone
		}
		fmt.Printf("EXIT %d fds %d want %d\n", code, n, want)
		os.Exit(code)
	}
	zduTakeover(func(f string, a ...any) { fmt.Printf("LOG "+f+"\n", a...) })
	fmt.Println("COLD-START") // the engine would now bind and run `ip link del`
	os.Exit(0)
}

func runTakeFail(t *testing.T, env []string, extra *os.File, parent func(*handoff.Conn)) string {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestZDUTakeFailChild$", "-test.count=1")
	cmd.Env = append(append(os.Environ(), takeFailEnv+"=1"), env...)
	if extra != nil {
		cmd.ExtraFiles = []*os.File{extra}
	}
	out, err := cmd.CombinedOutput()
	s := string(out)
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 1 {
		t.Fatalf("child exit %v, want 1:\n%s", err, s)
	}
	if strings.Contains(s, "COLD-START") {
		t.Fatalf("child cold-started:\n%s", s)
	}
	var code, n, want int
	for _, ln := range strings.Split(s, "\n") {
		if strings.HasPrefix(ln, "EXIT ") {
			fmt.Sscanf(ln, "EXIT %d fds %d want %d", &code, &n, &want)
		}
	}
	if code != 1 || n != want {
		t.Fatalf("fd leak or no exit line: exit %d fds %d want %d:\n%s", code, n, want, s)
	}
	return s
}

// childEnd makes a socketpair; the parent end runs parent, the other end is
// the child's fd 3.
func childEnd(t *testing.T, parent func(*handoff.Conn)) (*os.File, func()) {
	t.Helper()
	a, b, err := handoff.Pair()
	if err != nil {
		t.Fatal(err)
	}
	bf, err := b.File()
	b.Close()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if parent != nil {
			parent(a)
		}
	}()
	return bf, func() {
		bf.Close()
		a.Close()
		<-done
	}
}

// Review F3: a child started with HS2_HANDOVER_FD exits 1 on every takeover
// failure, closes every received fd, and never reaches the cold start (the
// engine's binds and the TUN's `ip link del` come after zduTakeover).
func TestZDUTakeFailExits(t *testing.T) {
	hello := handoff.Hello{Proto: handoff.Proto, InstStart: 1}
	give := func(fds []handoff.FD) func(*handoff.Conn) {
		return func(c *handoff.Conn) {
			c.Give(hello, fds, 5*time.Second, 5*time.Second)
		}
	}
	t.Run("badfd", func(t *testing.T) {
		runTakeFail(t, []string{"HS2_ZDU=1", "HS2_HANDOVER_FD=x"}, nil, nil)
	})
	t.Run("keyoff", func(t *testing.T) {
		f, stop := childEnd(t, nil)
		defer stop()
		runTakeFail(t, []string{"HS2_ZDU=", "HS2_HANDOVER_FD=3"}, f, nil)
	})
	t.Run("notsocket", func(t *testing.T) {
		r, w, _ := os.Pipe()
		defer w.Close()
		defer r.Close()
		runTakeFail(t, []string{"HS2_ZDU=1", "HS2_HANDOVER_FD=3"}, r, nil)
	})
	t.Run("abort", func(t *testing.T) {
		f, stop := childEnd(t, func(c *handoff.Conn) {
			c.Give(handoff.Hello{Proto: handoff.Proto + 1}, nil, 5*time.Second, time.Second)
		})
		defer stop()
		runTakeFail(t, []string{"HS2_ZDU=1", "HS2_HANDOVER_FD=3"}, f, nil)
	})
	mkFDs := func(t *testing.T, tunName string) []handoff.FD {
		ln, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ln.Close() })
		lf, _ := ln.File()
		uc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { uc.Close() })
		uf, _ := uc.File()
		r, w, _ := os.Pipe()
		t.Cleanup(func() { r.Close(); w.Close(); lf.Close(); uf.Close() })
		return []handoff.FD{{Name: "tcp:" + ln.Addr().String(), File: lf}, {Name: "udp:" + uc.LocalAddr().String(), File: uf}, {Name: tunName, File: r}}
	}
	t.Run("badtun", func(t *testing.T) {
		f, stop := childEnd(t, give(mkFDs(t, "tun:hs2zdutest")))
		defer stop()
		s := runTakeFail(t, []string{"HS2_ZDU=1", "HS2_HANDOVER_FD=3"}, f, nil)
		if !strings.Contains(s, "tun fd refused") {
			t.Fatalf("no tun refusal:\n%s", s)
		}
	})
	t.Run("unknown", func(t *testing.T) {
		f, stop := childEnd(t, give(mkFDs(t, "foo:x")))
		defer stop()
		runTakeFail(t, []string{"HS2_ZDU=1", "HS2_HANDOVER_FD=3"}, f, nil)
	})
}
