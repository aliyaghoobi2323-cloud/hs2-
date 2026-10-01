package udpcarrier

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/core"
	"github.com/hosseintaghipoursori-alt/hs2-tunnel/encap"
)

// udpRelay forwards datagrams between one client and a server, recording the
// largest datagram it saw in each direction.
type udpRelay struct {
	pc         *net.UDPConn
	srv        *net.UDPAddr
	mu         sync.Mutex
	cli        *net.UDPAddr
	maxC, maxS atomic.Int64
}

func newUDPRelay(t *testing.T, srv string) *udpRelay {
	pc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	sa, _ := net.ResolveUDPAddr("udp4", srv)
	r := &udpRelay{pc: pc, srv: sa}
	go func() {
		buf := make([]byte, 65536)
		for {
			n, from, err := pc.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if from.Port == sa.Port && from.IP.Equal(sa.IP) {
				if int64(n) > r.maxS.Load() {
					r.maxS.Store(int64(n))
				}
				r.mu.Lock()
				c := r.cli
				r.mu.Unlock()
				if c != nil {
					pc.WriteToUDP(buf[:n], c)
				}
				continue
			}
			r.mu.Lock()
			r.cli = from
			r.mu.Unlock()
			if int64(n) > r.maxC.Load() {
				r.maxC.Store(int64(n))
			}
			pc.WriteToUDP(buf[:n], sa)
		}
	}()
	t.Cleanup(func() { pc.Close() })
	return r
}

// measureFullUDPPayload brings up a udp carrier at the given inner MTU through a
// recording relay, sends 200 full-size heavy-parity frames each way, and returns
// the largest UDP PAYLOAD seen in each direction (the on-wire IPv4 datagram is
// that + 8 for the UDP header + 20 for the IPv4 header).
func measureFullUDPPayload(t *testing.T, mtu int) (c2s, s2c int64) {
	t.Helper()
	shared := testShared()
	l, err := Listen("127.0.0.1:0", shared, mtu)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	r := newUDPRelay(t, l.LocalAddr().String())
	accCh := make(chan *Conn, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c, _ := l.Accept(ctx)
		accCh <- c
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	cli, err := Dial(ctx, r.pc.LocalAddr().String(), shared, mtu)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	srv := <-accCh
	if srv == nil {
		t.Fatal("no accept")
	}
	defer cli.Close()
	defer srv.Close()
	// Force heavy parity so parity shards are exercised too.
	cli.enc.SetLoss(0.4)
	srv.enc.SetLoss(0.4)
	full := make([]byte, mtu)
	for i := 0; i < 200; i++ {
		cli.SendFrame(core.TypeData, full)
		srv.SendFrame(core.TypeData, full)
	}
	time.Sleep(500 * time.Millisecond)
	return r.maxC.Load(), r.maxS.Load()
}

// A full-size tunnel packet leaves as exactly innerMTU+CarrierOverhead bytes of
// UDP payload, both directions and at every FEC loss level, so InnerMTUFor's
// arithmetic keeps a full packet within the path MTU.
func TestCarrierOverhead(t *testing.T) {
	for _, mtu := range []int{1280, 1400, InnerMTUFor("udp", PathMTU)} {
		c2s, s2c := measureFullUDPPayload(t, mtu)
		for name, got := range map[string]int64{"c2s": c2s, "s2c": s2c} {
			if want := int64(mtu + CarrierOverhead); got != want {
				t.Errorf("mtu %d %s: largest datagram %d, want innerMTU+CarrierOverhead = %d", mtu, name, got, want)
			}
		}
	}
}

func TestInnerMTUFor(t *testing.T) {
	want := map[string]int{"udp": 1416, "icmp": 1408, "gre": 1416, "ipip": 1420, "ipx": 1420}
	for k, w := range want {
		if got := InnerMTUFor(k, 1500); got != w {
			t.Errorf("%s: %d, want %d", k, got, w)
		}
		if DefaultInnerMTU > InnerMTUFor(k, 1400) {
			t.Errorf("%s: default MTU does not fit a 1400-byte path", k)
		}
	}
	// A REAL measurement, not a restatement of the formula: over udp, a full
	// InnerMTUFor("udp",1500)-sized tunnel payload must leave as an IPv4 datagram
	// of no more than 1500 bytes, and of EXACTLY 20 (IPv4) + Overhead (the UDP
	// header) + CarrierOverhead + innerMTU — so the arithmetic genuinely keeps a
	// full packet inside a 1500-byte path unfragmented. The relay records the UDP
	// payload; the on-wire IPv4 datagram is that + 8 (UDP header) + 20 (IPv4).
	// (A raw-encapsulation on-wire capture is in encap's netns test suite.)
	innerMTU := InnerMTUFor("udp", 1500)
	c2s, s2c := measureFullUDPPayload(t, innerMTU)
	for name, udpPayload := range map[string]int64{"c2s": c2s, "s2c": s2c} {
		onWire := int(udpPayload) + encap.Overhead("udp") + ipv4Header
		if onWire > 1500 {
			t.Errorf("udp %s: on-wire IPv4 datagram %d exceeds the 1500-byte path (would fragment)", name, onWire)
		}
		if wantLen := ipv4Header + encap.Overhead("udp") + CarrierOverhead + innerMTU; onWire != wantLen {
			t.Errorf("udp %s: on-wire IPv4 datagram %d, want 20+Overhead+CarrierOverhead+innerMTU = %d", name, onWire, wantLen)
		}
	}
}
