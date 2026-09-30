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

// A full-size tunnel packet leaves as exactly innerMTU+CarrierOverhead bytes of
// UDP payload, both directions and at every FEC loss level, so InnerMTUFor's
// arithmetic keeps a full packet within the path MTU.
func TestCarrierOverhead(t *testing.T) {
	for _, mtu := range []int{1280, 1400, InnerMTUFor("udp", PathMTU)} {
		shared := testShared()
		l, err := Listen("127.0.0.1:0", shared, mtu)
		if err != nil {
			t.Fatal(err)
		}
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
		// Force heavy parity so parity shards are exercised too.
		cli.enc.SetLoss(0.4)
		srv.enc.SetLoss(0.4)
		full := make([]byte, mtu)
		for i := 0; i < 200; i++ {
			cli.SendFrame(core.TypeData, full)
			srv.SendFrame(core.TypeData, full)
		}
		time.Sleep(500 * time.Millisecond)
		for name, got := range map[string]int64{"c2s": r.maxC.Load(), "s2c": r.maxS.Load()} {
			if want := int64(mtu + CarrierOverhead); got != want {
				t.Errorf("mtu %d %s: largest datagram %d, want innerMTU+CarrierOverhead = %d", mtu, name, got, want)
			}
		}
		cli.Close()
		srv.Close()
		l.Close()
	}
}

func TestInnerMTUFor(t *testing.T) {
	want := map[string]int{"udp": 1416, "icmp": 1414, "gre": 1416, "ipip": 1420, "ipx": 1420}
	for k, w := range want {
		if got := InnerMTUFor(k, 1500); got != w {
			t.Errorf("%s: %d, want %d", k, got, w)
		}
		if got := InnerMTUFor(k, 1500) + CarrierOverhead + encap.Overhead(k) + 20; got != 1500 {
			t.Errorf("%s: sum %d", k, got)
		}
		if DefaultInnerMTU > InnerMTUFor(k, 1400) {
			t.Errorf("%s: default MTU does not fit a 1400-byte path", k)
		}
	}
}
