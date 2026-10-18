//go:build linux

package engine

import (
	"net"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// SO_MAX_PACING_RATE as the net sets it: the cap reads back, and 0 lifts it
// (~0U).
func TestPaceNetSetsSocket(t *testing.T) {
	_, s := loopbackTCP(t)
	get := func() uint32 {
		var v int
		rc, _ := s.SyscallConn()
		rc.Control(func(fd uintptr) { v, _ = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_MAX_PACING_RATE) })
		return uint32(v)
	}
	if !setMaxPacing(s, 30e6) || get() != 30e6 {
		t.Fatalf("cap 30 MB/s: reads %d", get())
	}
	if !setMaxPacing(s, 0) || get() != ^uint32(0) {
		t.Fatalf("lifted: reads %d", get())
	}
}

// wnFakeInfo stands in a socket's TCP_INFO (the exit's, fd) with a sender
// that sends 20000 segments a second, resends share of them, delivers 30
// MB/s and paces at 3.9 times that; every other socket reads its own.
func wnFakeInfo(t *testing.T, c *net.TCPConn, share *float64, mu *sync.Mutex) {
	var ours int
	rc, _ := c.SyscallConn()
	rc.Control(func(fd uintptr) { ours = int(fd) })
	t0 := time.Now()
	f := func(fd int) (*unix.TCPInfo, int, error) {
		if fd != ours {
			return getsockoptTCPInfo(fd)
		}
		mu.Lock()
		sh := *share
		mu.Unlock()
		dt := time.Since(t0).Seconds()
		segs := 20000 * dt
		return &unix.TCPInfo{Data_segs_out: uint32(segs), Total_retrans: uint32(sh * segs), Bytes_acked: uint64(30e6 * dt),
			Delivery_rate: 30e6, Pacing_rate: 117e6, Rtt: 100000, Min_rtt: 100000}, unix.SizeofTCPInfo, nil
	}
	old := tcpInfoRead.Load()
	tcpInfoRead.Store(&f)
	t.Cleanup(func() { tcpInfoRead.Store(old) })
}

// The runner runs the net on every exit link socket (the exit's session has
// no core by default): it caps the pacing of one that resends at 3.9 times
// its delivery rate, at wcNetK times that rate; with PaceNet 0 the exit's
// session is not even registered and no socket's pacing is touched.
func TestPaceNetRunner(t *testing.T) {
	for _, on := range []bool{true, false} {
		old := PaceNet
		PaceNet = 0
		if on {
			PaceNet = 1
		}
		c, s := loopbackTCP(t)
		var mu sync.Mutex
		share := 0.06
		wnFakeInfo(t, s, &share, &mu)
		var caps []uint64
		set := func(tc *net.TCPConn, rate uint64) bool {
			if tc == s {
				mu.Lock()
				caps = append(caps, rate)
				mu.Unlock()
			}
			return true
		}
		oldSet := wcPaceSet.Load()
		wcPaceSet.Store(&set)
		mtr := &linkMeter{}
		srv, _, err := newSession(s, true, nil, mtr)
		if err != nil {
			t.Fatal(err)
		}
		cli, _, err := newSession(c, false, nil, &linkMeter{})
		if err != nil {
			t.Fatal(err)
		}
		if on != (mtr.wc != nil && mtr.wc.pnet) {
			t.Fatalf("PaceNet %d: the exit's session registered %v", PaceNet, mtr.wc != nil)
		}
		got := func() []uint64 {
			mu.Lock()
			defer mu.Unlock()
			return append([]uint64(nil), caps...)
		}
		if on {
			wcWaitFor(t, 5*time.Second, "no cap", func() bool { return len(got()) > 0 })
			if v := got()[0]; v != uint64(wcNetK*30e6) {
				t.Fatalf("capped at %d, want %d", v, uint64(wcNetK*30e6))
			}
			if mtr.wc.inStats.netHeld.Load() != 1 || wcNetNote(mtr) == "" {
				t.Fatalf("counted %d, note %q", mtr.wc.inStats.netHeld.Load(), wcNetNote(mtr))
			}
		} else {
			time.Sleep(1500 * time.Millisecond)
			if v := got(); len(v) != 0 {
				t.Fatalf("PaceNet 0: caps %v", v)
			}
		}
		srv.Close()
		cli.Close()
		wcPaceSet.Store(oldSet)
		PaceNet = old
	}
}

// Registration: an exit's link socket is registered for the net whatever its
// window config (no window pool: no core, still a net), an edge's never, and
// none with PaceNet 0 or the lab's always-on cap in its place.
func TestPaceNetRegistration(t *testing.T) {
	_, s := loopbackTCP(t)
	old, cap0 := PaceNet, WcorePaceCap
	t.Cleanup(func() { PaceNet, WcorePaceCap = old, cap0 })
	cfg := wcTestConfig()
	cfg.WindowPool = nil
	PaceNet = 1
	if l := newWcLink(nil, cfg, s, nil, true, nil); l == nil || !l.pnet || !l.off {
		t.Fatalf("an exit's socket without a window pool: %+v", l)
	}
	if l := newWcLink(nil, cfg, s, nil, false, nil); l != nil {
		t.Fatal("an edge's socket registered for the net")
	}
	if l := newWcLink(nil, cfg, nil, nil, true, nil); l != nil {
		t.Fatal("an exit's session with no socket registered for the net")
	}
	PaceNet = 0
	if l := newWcLink(nil, cfg, s, nil, true, nil); l != nil {
		t.Fatal("PaceNet 0: registered")
	}
	PaceNet, WcorePaceCap = 1, 1.25
	if l := newWcLink(nil, wcTestConfig(), s, nil, true, nil); l == nil || l.pnet {
		t.Fatalf("with the lab's always-on cap: %+v, want its registration, not the net's", l)
	}
}
