//go:build linux

package engine

import (
	"net"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// C4-lite on every link socket at both ends (§3.4): over a loopback link
// whose two sockets read as delivering 30 MB/s, not app-limited and without
// resends, the edge's socket and the exit's are both capped at 1.25 × that,
// by the cores' runner; the exit's pacing net hands its cap to the exit's
// C4-lite (one writer of the cap). With HS2_TUNE_SENDCAP=0 neither socket
// is capped: the net alone, which caps only for resends.
func TestAngleSendCapBothEnds(t *testing.T) {
	if SendCapOff() {
		t.Skip("HS2_TUNE_SENDCAP=0 is set")
	}
	for _, on := range []bool{true, false} {
		func() {
			defer func(v int) { TuneSendCap = v }(TuneSendCap)
			if !on {
				TuneSendCap = 0
			}
			c, s := loopbackTCP(t)
			fds := map[int]bool{}
			for _, x := range []*net.TCPConn{c, s} {
				rc, _ := x.SyscallConn()
				rc.Control(func(fd uintptr) { fds[int(fd)] = true })
			}
			t0 := time.Now()
			f := func(fd int) (*unix.TCPInfo, int, error) {
				if !fds[fd] {
					return getsockoptTCPInfo(fd)
				}
				dt := time.Since(t0).Seconds()
				return &unix.TCPInfo{Data_segs_out: uint32(20000 * dt), Bytes_acked: uint64(30e6 * dt),
					Delivery_rate: 30e6, Pacing_rate: 40e6, Rtt: 20000, Min_rtt: 20000, Snd_mss: 1448}, unix.SizeofTCPInfo, nil
			}
			old := tcpInfoRead.Load()
			tcpInfoRead.Store(&f)
			defer tcpInfoRead.Store(old)
			var mu sync.Mutex
			caps := map[*net.TCPConn]uint64{}
			set := func(tc *net.TCPConn, rate uint64) bool {
				if tc == c || tc == s {
					mu.Lock()
					caps[tc] = rate
					mu.Unlock()
				}
				return true
			}
			oldSet := wcPaceSet.Load()
			wcPaceSet.Store(&set)
			defer wcPaceSet.Store(oldSet)
			mtr := &linkMeter{}
			srv, _, err := newSession(s, true, nil, mtr)
			if err != nil {
				t.Fatal(err)
			}
			defer srv.Close()
			cli, _, err := newSession(c, false, nil, &linkMeter{statsPoll: make(chan struct{}, 1)})
			if err != nil {
				t.Fatal(err)
			}
			defer cli.Close()
			got := func(tc *net.TCPConn) uint64 {
				mu.Lock()
				defer mu.Unlock()
				return caps[tc]
			}
			want := uint64(wcNetKOf() * 30e6)
			if on {
				wcWaitFor(t, 3*time.Second, "both sockets capped", func() bool { return got(c) == want && got(s) == want })
				cl := coreLinks.of(srv)
				if cl == nil || cl.sc == nil || mtr.wc == nil || mtr.wc.sc.Load() != cl.sc {
					t.Fatal("the exit's pacing net does not hand its cap to the socket's C4-lite")
				}
				return
			}
			time.Sleep(time.Second)
			if got(c) != 0 || got(s) != 0 {
				t.Fatalf("HS2_TUNE_SENDCAP=0: capped %d / %d", got(c), got(s))
			}
			if mtr.wc != nil && mtr.wc.sc.Load() != nil {
				t.Fatal("HS2_TUNE_SENDCAP=0: the net hands its cap to C4-lite")
			}
		}()
	}
}
