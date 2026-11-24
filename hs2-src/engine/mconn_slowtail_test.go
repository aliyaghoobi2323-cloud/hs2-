package engine

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

// fx168 (audit rel66 H2): fn164 waited only mCloseWaitEnd (1 s) for the
// queued tail and the FIN before closing every sub. On a slow link a tail
// that needs longer was still cut: the exit saw its subs die without a FIN
// (errMConnSubsDied, rst=false) and the target got a truncated upload with a
// clean FIN. The tunnel path here is throttled (Iran→exit only); the user
// writes an upload and closes at once.
//   - an upload that drains within mCloseLinger: every byte and a clean EOF;
//   - one that cannot: every byte, or an abnormal end (RST) at the target —
//     never a clean EOF on truncated data.

// slowPauseUntil: throttleProxy forwards nothing client→dst until then
// (unix ns): a dark path for the moment.
var slowPauseUntil atomic.Int64

// throttleProxy forwards to dst; the client→dst direction of each connection
// is limited to rate bytes/s (dst→client is not).
func throttleProxy(t *testing.T, dst string, rate int) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.(*net.TCPConn).SetReadBuffer(16 << 10)
			u, err := net.Dial("tcp", dst)
			if err != nil {
				c.Close()
				continue
			}
			go func() { io.Copy(c, u); c.Close() }()
			go func() {
				defer u.Close()
				const step = 4 << 10
				b := make([]byte, step)
				t0 := time.Now()
				sent := 0
				for {
					n, err := c.Read(b)
					if n > 0 {
						for time.Now().UnixNano() < slowPauseUntil.Load() {
							time.Sleep(5 * time.Millisecond)
						}
						if _, werr := u.Write(b[:n]); werr != nil {
							return
						}
						sent += n
						if d := time.Duration(float64(sent)/float64(rate)*float64(time.Second)) - time.Since(t0); d > 0 {
							time.Sleep(d)
						}
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	return ln.Addr().String()
}

// startSlowTunnel is startTunnelTarget with the Iran→exit path throttled.
func startSlowTunnel(t *testing.T, links, rate int, target string) string {
	key := bytes.Repeat([]byte{0x5a}, 32)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	raw, _ := net.Listen("tcp", "127.0.0.1:0")
	srv := &tlscarrier.Server{SharedKey: key, Cert: testCert(t), BackendAddr: "127.0.0.1:1"}
	go RunKharej(ctx, KharejConfig{Listener: raw, Server: srv, Panel: target})
	px := throttleProxy(t, raw.Addr().String(), rate)
	port := freePort(t)
	go RunIran(ctx, IranConfig{
		Dialer: NewMTCPDialer(px, "lab.example.com", key, ""),
		Min:    links, Max: links, PerLink: 50,
		ListenIP: "127.0.0.1", Ports: []string{port}, UDP: true,
	})
	addr := "127.0.0.1:" + port
	for i := 0; i < 100; i++ {
		if c, err := net.Dial("tcp", addr); err == nil {
			c.Close()
			return addr
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("user port never opened")
	return ""
}

// sendTailN is sendTail with a long write deadline; it returns the bytes of
// payload the user got written before closing.
func sendTailN(addr string, id uint64, size int) (uint64, error) {
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return 0, err
	}
	b := make([]byte, 16+size)
	binary.BigEndian.PutUint64(b, id)
	binary.BigEndian.PutUint64(b[8:], uint64(size))
	for i := 0; i < size; i++ {
		b[16+i] = byte(uint64(i)*7 + id)
	}
	c.SetWriteDeadline(time.Now().Add(40 * time.Second))
	n, err := c.Write(b)
	c.Close()
	if n < 16 {
		return 0, err
	}
	return uint64(n - 16), err
}

// waitTail returns the tail of connection id, skipping (and logging) any
// other connection's end that reaches the target meanwhile. b449m: the
// warm-up uploads (id 1<<40, 1 KB) count the mconn as used when it opens,
// not when the target has read them; under CPU load one of them can end at
// the target after the 300 ms drain and was then taken for the upload under
// test ("SILENT TRUNCATION: target got 1024 of 1048576 ... after 3ms").
func waitTail(t *testing.T, got <-chan tailGot, id uint64, d time.Duration) (tailGot, bool) {
	dl := time.After(d)
	for {
		select {
		case g := <-got:
			if g.id == id {
				return g, true
			}
			t.Logf("skipped another connection's end at the target: id %d got %d of %d (pattern ok %v) err %v", g.id, g.got, g.want, g.sum, g.err)
		case <-dl:
			return tailGot{}, false
		}
	}
}

func TestMConnCloseSlowTail(t *testing.T) {
	if testing.Short() {
		t.Skip("slow link")
	}
	const rate = 125000 // 1 Mbit/s
	for _, lite := range []bool{false, true} {
		name := "full"
		if lite {
			name = "lite"
		}
		t.Run(name, func(t *testing.T) {
			withMConn(t, true)
			setLite(t, lite)
			got := make(chan tailGot, 64)
			addr := startSlowTunnel(t, 1, rate, tailSink(t, got))
			before := mconnStat.total.Load()
			for i := 0; mconnStat.total.Load() == before; i++ {
				if i == 50 {
					t.Fatal("no connection ever used an mconn")
				}
				sendTailN(addr, 1<<40, 1024)
				time.Sleep(50 * time.Millisecond)
			}
			time.Sleep(300 * time.Millisecond)
			for len(got) > 0 {
				<-got
			}
			// 400 KB: ~3.3 s at the rate, under mCloseLinger (5 s); 2 MB:
			// ~17 s, over it.
			for i, sz := range []int{400 << 10, 2 << 20} {
				id := uint64(i + 1)
				t0 := time.Now()
				wrote, werr := sendTailN(addr, id, sz)
				tw := time.Since(t0)
				g, ok := waitTail(t, got, id, 45*time.Second)
				if !ok {
					t.Fatalf("size %d: the target never saw the end", sz)
				}
				t.Logf("size %d: user wrote %d in %v (err %v); target got %d (pattern ok %v) end err %v after %v",
					sz, wrote, tw.Round(time.Millisecond), werr, g.got, g.sum, g.err, time.Since(t0).Round(time.Millisecond))
				if !g.sum {
					t.Errorf("size %d: corrupt bytes at the target", sz)
				}
				if g.err == nil && g.got != wrote {
					t.Errorf("size %d: SILENT TRUNCATION: target got %d of %d bytes and a clean EOF", sz, g.got, wrote)
				}
				if i == 0 && (g.got != wrote || g.err != nil) {
					t.Errorf("size %d (drains within the linger): target got %d of %d, end err %v", sz, g.got, wrote, g.err)
				}
				time.Sleep(time.Second)
				for len(got) > 0 {
					<-got
				}
			}
		})
	}
}

// An upload whose tail cannot drain within mCloseLinger (the path goes dark
// right after the user's Close, for longer than the linger): the target must
// get every byte or an abnormal end (an RST, counted in mconnTargetRst) —
// never a clean EOF on fewer bytes than the user wrote.
func TestMConnCloseSlowTailOverLinger(t *testing.T) {
	if testing.Short() {
		t.Skip("slow link")
	}
	withMConn(t, true)
	const rate = 125000 // 1 Mbit/s
	got := make(chan tailGot, 64)
	addr := startSlowTunnel(t, 1, rate, tailSink(t, got))
	before := mconnStat.total.Load()
	for i := 0; mconnStat.total.Load() == before; i++ {
		if i == 50 {
			t.Fatal("no connection ever used an mconn")
		}
		sendTailN(addr, 1<<40, 1024)
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	for len(got) > 0 {
		<-got
	}
	rst0 := mconnTargetRst.Load()
	// the path goes dark the moment the edge closes the mConn, for longer
	// than the linger
	hook := func() {
		slowPauseUntil.Store(time.Now().Add(mCloseLinger + 3*time.Second).UnixNano())
	}
	testHookUserMConnClosed.Store(&hook)
	t.Cleanup(func() { testHookUserMConnClosed.Store(nil); slowPauseUntil.Store(0) })
	// a warm-up's end that reaches the target late must be skipped, not
	// taken for this upload: one such stale end is queued here on purpose
	// (deterministic regression for the b449m flake).
	got <- tailGot{id: 1 << 40, want: 1024, got: 1024, sum: true}
	const sz = 1 << 20
	t0 := time.Now()
	wrote, werr := sendTailN(addr, 1, sz)
	g, ok := waitTail(t, got, 1, 60*time.Second)
	if !ok {
		t.Fatal("the target never saw the end")
	}
	rsts := mconnTargetRst.Load() - rst0
	t.Logf("user wrote %d (err %v); target got %d (pattern ok %v) end err %v after %v; target resets %d",
		wrote, werr, g.got, g.sum, g.err, time.Since(t0).Round(time.Millisecond), rsts)
	if !g.sum {
		t.Error("corrupt bytes at the target")
	}
	if g.err == nil && g.got != wrote {
		t.Errorf("SILENT TRUNCATION: target got %d of %d bytes and a clean EOF", g.got, wrote)
	}
	if g.got != wrote && rsts == 0 {
		t.Errorf("truncated (%d of %d) but no target reset counted", g.got, wrote)
	}
}
