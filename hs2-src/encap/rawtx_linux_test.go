//go:build linux

package encap

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// captureStart records the IPv4 packets this netns sends until stop.
func captureStart(t *testing.T) (stop func() [][]byte) {
	t.Helper()
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_DGRAM, 0x0008) // ETH_P_IP, network order
	if err != nil {
		t.Skip("AF_PACKET:", err)
	}
	unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &unix.Timeval{Usec: 200000})
	var out [][]byte
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 65536)
		for {
			n, from, err := unix.Recvfrom(fd, buf, 0)
			if err != nil {
				return
			}
			if ll, ok := from.(*unix.SockaddrLinklayer); ok && ll.Pkttype == unix.PACKET_OUTGOING {
				continue // lo shows each packet twice
			}
			out = append(out, append([]byte(nil), buf[:n]...))
		}
	}()
	return func() [][]byte { <-done; unix.Close(fd); return out }
}

// withRawTx runs f with the send-only socket on or off for the sockets it opens.
func withRawTx(on bool, f func()) {
	old := rawNoTx
	rawNoTx = !on
	defer func() { rawNoTx = old }()
	f()
}

// The send-only socket puts the outer IPv4 header the kernel built for the
// receive socket on the wire: icmp both ways (requests DF with id 0, replies
// without DF and an id from the kernel), the same TTL, TOS, protocol and
// addresses, the destination filled in.
func TestRawTxHeaderMatchesKernel(t *testing.T) {
	needRawNetns(t)
	type hdrs struct{ req, rep []byte }
	run := func(on bool) hdrs {
		var h hdrs
		withRawTx(on, func() {
			opt := Options{Key: []byte(fmt.Sprintf("wire-%v", on))} // a mux of its own
			pc := listenT(t, KindICMP, "127.0.0.1", opt)
			c := dialT(t, KindICMP, "127.0.0.1", opt)
			if got := c.(*rawConn).mx.tx != nil; got != on {
				t.Fatalf("tx on=%v but open=%v", on, got)
			}
			if got := pc.(*rawPacketConn).tx != nil; got != on {
				t.Fatalf("tx on=%v but listener's open=%v", on, got)
			}
			stop := captureStart(t)
			c.Write(bytes.Repeat([]byte{1}, 1300))
			_, a := readFromT(t, pc, 2*time.Second)
			pc.WriteTo(bytes.Repeat([]byte{2}, 1300), a)
			readT(t, c, 2*time.Second)
			for _, p := range stop() {
				if len(p) < 28 || p[9] != 1 {
					continue
				}
				switch p[20] {
				case icmpEchoRequest:
					h.req = p[:20]
				case icmpEchoReply:
					h.rep = p[:20]
				}
			}
		})
		if h.req == nil || h.rep == nil {
			t.Fatalf("tx=%v: request %v reply %v not captured", on, h.req != nil, h.rep != nil)
		}
		return h
	}
	kern, tx := run(false), run(true)
	cmp := func(name string, k, x []byte) {
		// everything but the id (4:6) and the checksum (10:12) is equal
		if !bytes.Equal(k[:4], x[:4]) || !bytes.Equal(k[6:10], x[6:10]) || !bytes.Equal(k[12:20], x[12:20]) {
			t.Errorf("%s: headers differ\nkernel % x\ntx     % x", name, k, x)
		}
		kid, xid := binary.BigEndian.Uint16(k[4:6]), binary.BigEndian.Uint16(x[4:6])
		if df := k[6]&0x40 != 0; df && (kid != 0 || xid != 0) {
			t.Errorf("%s: DF set but id kernel=%d tx=%d (want 0, 0)", name, kid, xid)
		} else if !df && (kid == 0) != (xid == 0) {
			t.Errorf("%s: no DF, id kernel=%d tx=%d", name, kid, xid)
		}
	}
	if kern.req[6]&0x40 == 0 || kern.rep[6]&0x40 != 0 {
		t.Fatalf("the kernel's DF: request % x reply % x (want DF on requests only)", kern.req[6:8], kern.rep[6:8])
	}
	cmp("request", kern.req, tx.req)
	cmp("reply", kern.rep, tx.rep)
}

// setMTU sets an interface's MTU (ip link set <if> mtu n) without iproute2.
func setMTU(t *testing.T, name string, mtu int) {
	t.Helper()
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	ifr, err := unix.NewIfreq(name)
	if err != nil {
		t.Fatal(err)
	}
	ifr.SetUint32(uint32(mtu))
	if err := unix.IoctlIfreq(fd, unix.SIOCSIFMTU, ifr); err != nil {
		t.Fatalf("set %s mtu %d: %v", name, mtu, err)
	}
}

// A reply larger than the device without DF goes out as before — the old
// socket fragments it — and arrives; a request that size has DF and is
// refused by the kernel on either path, counted and dropped.
func TestRawTxOversize(t *testing.T) {
	needRawNetns(t)
	setMTU(t, "lo", 1500)
	t.Cleanup(func() { setMTU(t, "lo", 65536) })
	withRawTx(true, func() {
		opt := Options{Key: []byte("oversize")}
		pc := listenT(t, KindICMP, "127.0.0.1", opt)
		c := dialT(t, KindICMP, "127.0.0.1", opt)
		c.Write([]byte("hello"))
		_, a := readFromT(t, pc, 2*time.Second)

		fb := txFellBack.Load()
		big := bytes.Repeat([]byte{7}, 1600)
		if _, err := pc.WriteTo(big, a); err != nil {
			t.Fatalf("an oversized reply: %v", err)
		}
		if got := readT(t, c, 2*time.Second); !bytes.Equal(got, big) {
			t.Fatalf("the oversized reply arrived as %d bytes", len(got))
		}
		if txFellBack.Load() != fb+1 {
			t.Fatalf("the oversized reply did not go through the old socket (fell back %d)", txFellBack.Load()-fb)
		}

		ref := sendRefused.Load()
		if _, err := c.Write(big); err != nil {
			t.Fatalf("an oversized request must be dropped, not fail the link: %v", err)
		}
		if sendRefused.Load() != ref+1 {
			t.Fatalf("the refused request was not counted")
		}
		c.Write([]byte("after"))
		if got, _ := readFromT(t, pc, 2*time.Second); string(got) != "after" {
			t.Fatalf("after the refused request: %q", got)
		}
	})
}

// The send-only socket receives nothing: after traffic its receive queue is
// empty, and there is still one icmp socket per side.
func TestRawTxReceivesNothing(t *testing.T) {
	needRawNetns(t)
	withRawTx(true, func() {
		opt := Options{Key: []byte("rx-nothing")}
		pc := listenT(t, KindICMP, "127.0.0.1", opt)
		c := dialT(t, KindICMP, "127.0.0.1", opt)
		for i := 0; i < 50; i++ {
			c.Write([]byte("ping"))
			_, a := readFromT(t, pc, 2*time.Second)
			pc.WriteTo([]byte("pong"), a)
			readT(t, c, 2*time.Second)
		}
		b, err := os.ReadFile("/proc/net/raw")
		if err != nil {
			t.Skip(err)
		}
		icmp, raw := 0, 0
		for _, l := range strings.Split(string(b), "\n")[1:] {
			f := strings.Fields(l)
			if len(f) < 5 {
				continue
			}
			// local address is ip:proto in hex; tx_queue:rx_queue
			_, proto, _ := strings.Cut(f[1], ":")
			_, rxq, _ := strings.Cut(f[4], ":")
			switch proto {
			case "0001":
				icmp++
			case "00FF":
				raw++
				if strings.Trim(rxq, "0") != "" {
					t.Errorf("a send-only socket has %s bytes queued to read", rxq)
				}
			}
		}
		if icmp != 2 || raw != 2 {
			t.Fatalf("%d icmp and %d IPPROTO_RAW sockets, want 2 and 2 (one each per side)", icmp, raw)
		}
	})
}

// failRC is a RawConn whose calls fail as a broken socket would.
type failRC struct{}

func (failRC) Control(func(uintptr)) error   { return errors.New("injected failure") }
func (failRC) Read(func(uintptr) bool) error  { return errors.New("injected failure") }
func (failRC) Write(func(uintptr) bool) error { return errors.New("injected failure") }

var _ syscall.RawConn = failRC{}

// A send-only socket that fails turns itself off, once, and its carriers
// carry on through the old socket — every datagram still arrives.
func TestRawTxBrokenFallsBack(t *testing.T) {
	needRawNetns(t)
	withRawTx(true, func() {
		opt := Options{Key: []byte("broken")}
		pc := listenT(t, KindICMP, "127.0.0.1", opt)
		c := dialT(t, KindICMP, "127.0.0.1", opt)
		c.Write([]byte("first"))
		_, a := readFromT(t, pc, 2*time.Second)
		ctx, ptx := c.(*rawConn).mx.tx, pc.(*rawPacketConn).tx
		ctx.rc, ptx.rc = failRC{}, failRC{}
		fb := txFellBack.Load()
		if _, err := c.Write([]byte("one")); err != nil {
			t.Fatal(err)
		}
		if err := c.(*rawConn).WriteBatch([][]byte{[]byte("two"), []byte("three")}); err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"one", "two", "three"} {
			if got, _ := readFromT(t, pc, 2*time.Second); string(got) != want {
				t.Fatalf("got %q, want %q", got, want)
			}
		}
		if _, err := pc.WriteTo([]byte("back"), a); err != nil {
			t.Fatal(err)
		}
		if got := readT(t, c, 2*time.Second); string(got) != "back" {
			t.Fatalf("reply %q", got)
		}
		if ctx.ok() || ptx.ok() {
			t.Fatal("a failed send-only socket is still in use")
		}
		if txFellBack.Load() != fb+2 {
			// only the first datagram after each socket's failure goes through
			// sendAll's fallback; after that ok() sends straight on the old one
			t.Fatalf("fell back %d times, want 2 (one per side)", txFellBack.Load()-fb)
		}
	})
}

// Several carriers on one mux send at once, through the send-only socket and
// without a shared lock, with a send buffer far too small: a full buffer
// waits (EAGAIN falls back to a blocking write) instead of failing, and
// every datagram arrives intact on its own link.
func TestRawTxConcurrentFullBuffer(t *testing.T) {
	needRawNetns(t)
	withRawTx(true, func() {
		opt := Options{Key: []byte("concurrent")}
		pc := listenT(t, KindICMP, "127.0.0.1", opt)
		const links, per = 4, 300
		var cs []*rawConn
		for i := 0; i < links; i++ {
			cs = append(cs, dialT(t, KindICMP, "127.0.0.1", opt).(*rawConn))
		}
		tx := cs[0].mx.tx
		for _, c := range cs[1:] {
			if c.mx.tx != tx {
				t.Fatal("the links do not share one mux")
			}
		}
		tx.rc.Control(func(fd uintptr) { unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_SNDBUF, 4096) })
		got := map[string]int{}
		var mu sync.Mutex
		done := make(chan struct{})
		go func() {
			defer close(done)
			buf := make([]byte, 2048)
			pc.SetReadDeadline(time.Now().Add(10 * time.Second))
			for n := 0; n < links*per; n++ {
				k, _, err := pc.ReadFrom(buf)
				if err != nil {
					return
				}
				mu.Lock()
				got[string(buf[:k])]++
				mu.Unlock()
			}
		}()
		var wg sync.WaitGroup
		for i, c := range cs {
			wg.Add(1)
			go func(i int, c *rawConn) {
				defer wg.Done()
				var batch [][]byte
				for j := 0; j < per; j++ {
					batch = append(batch, []byte(fmt.Sprintf("l%d-%04d-%s", i, j, strings.Repeat("x", 1000))))
					if len(batch) == 8 || j == per-1 {
						if err := c.WriteBatch(batch); err != nil {
							t.Errorf("link %d: %v", i, err)
							return
						}
						batch = nil
					}
				}
			}(i, c)
		}
		wg.Wait()
		<-done
		mu.Lock()
		defer mu.Unlock()
		if len(got) < links*per*9/10 {
			t.Fatalf("%d of %d datagrams arrived", len(got), links*per)
		}
		for k, n := range got {
			if n != 1 || !strings.HasSuffix(k, strings.Repeat("x", 1000)) {
				t.Fatalf("datagram %.12q arrived %d times or damaged", k, n)
			}
		}
	})
}

var _ net.Conn = (*rawConn)(nil)
