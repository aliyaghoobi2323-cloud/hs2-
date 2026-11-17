package engine

import (
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// hc183: half-close through a real tunnel (halfclose.go), on every plane:
// the one-stream path (mconn off), the full mConn (records) and lite (a raw
// first sub).

func hcPlanes(t *testing.T, f func(t *testing.T)) {
	for _, p := range []string{"stream", "full", "lite"} {
		t.Run(p, func(t *testing.T) {
			withMConn(t, p != "stream")
			setLite(t, p == "lite")
			f(t)
		})
	}
}

func hcPattern(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*13) + seed
	}
	return b
}

// hcTarget runs serve on each accepted target connection.
func hcTarget(t *testing.T, serve func(c *net.TCPConn)) string {
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
			go func() {
				defer c.Close()
				serve(c.(*net.TCPConn))
			}()
		}
	}()
	return ln.Addr().String()
}

// hcDial dials the tunnel until a connection reaches the target (the probe
// connections of startTunnelTarget also reach it: serve sees them as empty).
func hcDial(t *testing.T, addr string) *net.TCPConn {
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return c.(*net.TCPConn)
}

func readAllT(c net.Conn, d time.Duration) ([]byte, error) {
	c.SetReadDeadline(time.Now().Add(d))
	return io.ReadAll(c)
}

// waitHC waits until the edge knows the exit announced half-close.
func waitHC(t *testing.T, addr string) {
	t.Helper()
	time.Sleep(300 * time.Millisecond) // kindInfo is exchanged before links carry users
}

const hcReq = 100 << 10

// (1) the client sends a request, SHUT_WR, then reads a large response.
func TestHalfCloseClientShutWr(t *testing.T) {
	hcPlanes(t, func(t *testing.T) {
		resp := hcPattern(4<<20, 3)
		addr := startTunnelTarget(t, 3, 3, hcTarget(t, func(c *net.TCPConn) {
			req, err := readAllT(c, 20*time.Second)
			if err != nil || len(req) == 0 {
				return // a probe
			}
			if len(req) != hcReq {
				c.Write([]byte("BAD"))
				return
			}
			c.Write(resp)
		}))
		waitHC(t, addr)
		for i := 0; i < 3; i++ {
			c := hcDial(t, addr)
			if _, err := c.Write(hcPattern(hcReq, 1)); err != nil {
				t.Fatal(err)
			}
			c.CloseWrite()
			got, err := readAllT(c, 30*time.Second)
			c.Close()
			if err != nil || len(got) != len(resp) || string(got) != string(resp) {
				t.Fatalf("round %d: got %d of %d bytes, err %v (half-closes %d)", i, len(got), len(resp), err, halfCloses.Load())
			}
		}
	})
}

// (2) the target sends its response, SHUT_WR, and keeps reading the upload.
func TestHalfCloseTargetShutWr(t *testing.T) {
	hcPlanes(t, func(t *testing.T) {
		up := hcPattern(3<<20, 5)
		got := make(chan int, 16)
		addr := startTunnelTarget(t, 3, 3, hcTarget(t, func(c *net.TCPConn) {
			c.Write([]byte("HELLO"))
			c.CloseWrite()
			b, _ := readAllT(c, 30*time.Second)
			if len(b) == 0 {
				return // a probe
			}
			if string(b) != string(up) {
				got <- -len(b)
				return
			}
			got <- len(b)
		}))
		waitHC(t, addr)
		for i := 0; i < 3; i++ {
			c := hcDial(t, addr)
			hello, err := readAllT(c, 10*time.Second) // until the target's FIN
			if err != nil || string(hello) != "HELLO" {
				t.Fatalf("round %d: response %q err %v", i, hello, err)
			}
			c.SetWriteDeadline(time.Now().Add(30 * time.Second))
			if _, err := c.Write(up); err != nil {
				t.Fatalf("round %d: upload after the target's FIN: %v", i, err)
			}
			c.CloseWrite()
			select {
			case n := <-got:
				if n != len(up) {
					t.Fatalf("round %d: target got %d of %d upload bytes (negative: wrong bytes)", i, n, len(up))
				}
			case <-time.After(30 * time.Second):
				t.Fatalf("round %d: the target never got the whole upload", i)
			}
			// both directions over: the client sees the end
			c.SetReadDeadline(time.Now().Add(10 * time.Second))
			if n, err := c.Read(make([]byte, 1)); err != io.EOF {
				t.Fatalf("round %d: end read %d %v, want EOF", i, n, err)
			}
			c.Close()
		}
	})
}

// (3) both directions half-close at once.
func TestHalfCloseBoth(t *testing.T) {
	hcPlanes(t, func(t *testing.T) {
		a, b := hcPattern(1<<20, 7), hcPattern(2<<20, 9)
		got := make(chan int, 16)
		addr := startTunnelTarget(t, 3, 3, hcTarget(t, func(c *net.TCPConn) {
			var wg sync.WaitGroup
			wg.Add(1)
			go func() { defer wg.Done(); c.Write(b); c.CloseWrite() }()
			in, _ := readAllT(c, 30*time.Second)
			wg.Wait()
			if len(in) == 0 {
				return
			}
			if string(in) != string(a) {
				got <- -len(in)
				return
			}
			got <- len(in)
		}))
		waitHC(t, addr)
		for i := 0; i < 3; i++ {
			c := hcDial(t, addr)
			go func() { c.Write(a); c.CloseWrite() }()
			in, err := readAllT(c, 30*time.Second)
			if err != nil || string(in) != string(b) {
				t.Fatalf("round %d: client got %d of %d bytes, err %v", i, len(in), len(b), err)
			}
			select {
			case n := <-got:
				if n != len(a) {
					t.Fatalf("round %d: target got %d of %d", i, n, len(a))
				}
			case <-time.After(30 * time.Second):
				t.Fatalf("round %d: target never finished", i)
			}
			c.Close()
		}
	})
}

// (4) an abnormal end while half-closed resets the waiting side: the target
// resets in the middle of its response, or never answers (halfCloseIdle).
func TestHalfCloseAbnormal(t *testing.T) {
	oldIdle, oldPoll := halfCloseIdle.Load(), halfClosePoll.Load()
	halfCloseIdle.Store(int64(2 * time.Second))
	halfClosePoll.Store(int64(200 * time.Millisecond))
	t.Cleanup(func() { halfCloseIdle.Store(oldIdle); halfClosePoll.Store(oldPoll) })
	hcPlanes(t, func(t *testing.T) {
		addr := startTunnelTarget(t, 3, 3, hcTarget(t, func(c *net.TCPConn) {
			req, _ := readAllT(c, 20*time.Second)
			switch string(req) {
			case "RST":
				c.Write(hcPattern(256<<10, 1))
				time.Sleep(200 * time.Millisecond)
				c.SetLinger(0) // the response is cut
			case "IDLE":
				time.Sleep(8 * time.Second) // never answers, never closes
			}
		}))
		waitHC(t, addr)
		modes := []string{"RST", "IDLE"}
		if !mconnOn.Load() || mconnLite.Load() {
			// the one-stream path and a raw first sub have no reset on the
			// wire (smux has no RST frame): a target reset reaches the user
			// as a FIN, as before hc183 (fx170 covers records only)
			modes = modes[1:]
		}
		for _, mode := range modes {
			c := hcDial(t, addr)
			c.Write([]byte(mode))
			c.CloseWrite()
			t0 := time.Now()
			got, err := readAllT(c, 15*time.Second)
			c.Close()
			var ne net.Error
			if err == nil || errors.As(err, &ne) && ne.Timeout() {
				t.Fatalf("%s: read %d bytes, end %v after %v; want a reset", mode, len(got), err, time.Since(t0))
			}
			t.Logf("%s: %d bytes then %v after %v", mode, len(got), err, time.Since(t0).Round(time.Millisecond))
		}
	})
}

// (5) a peer without capExtHalfClose (here: the key off) gets the old full
// close: the client's SHUT_WR ends the whole connection.
func TestHalfCloseOff(t *testing.T) {
	old := halfCloseOn.Load()
	halfCloseOn.Store(false)
	t.Cleanup(func() { halfCloseOn.Store(old) })
	if (&peerInfo{V2: true, Ext: capExtHalfClose}).halfClose() {
		t.Fatal("halfClose with the key off")
	}
	if infoExt()&capExtHalfClose != 0 {
		t.Fatal("bit announced with the key off")
	}
	hcPlanes(t, func(t *testing.T) {
		addr := startTunnelTarget(t, 3, 3, hcTarget(t, func(c *net.TCPConn) {
			req, _ := readAllT(c, 20*time.Second)
			if len(req) > 0 {
				time.Sleep(300 * time.Millisecond)
				c.Write([]byte("LATE"))
			}
		}))
		waitHC(t, addr)
		c := hcDial(t, addr)
		c.Write([]byte("x"))
		c.CloseWrite()
		got, _ := readAllT(c, 10*time.Second)
		c.Close()
		if len(got) != 0 {
			t.Fatalf("old behaviour: got %q after SHUT_WR", got)
		}
	})
	halfCloseOn.Store(true)
	if !(&peerInfo{V2: true, Ext: capExtHalfClose}).halfClose() || (&peerInfo{V2: true}).halfClose() {
		t.Fatal("peerInfo.halfClose")
	}
}

// relay (dgforward, dgports): TCP to TCP half-close, both ways, and the idle
// bound.
func TestHalfCloseRelayTCP(t *testing.T) {
	oldIdle, oldPoll := halfCloseIdle.Load(), halfClosePoll.Load()
	halfCloseIdle.Store(int64(time.Second))
	halfClosePoll.Store(int64(100 * time.Millisecond))
	t.Cleanup(func() { halfCloseIdle.Store(oldIdle); halfClosePoll.Store(oldPoll) })
	pair := func() (*net.TCPConn, *net.TCPConn) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		s, err := ln.Accept()
		if err != nil {
			t.Fatal(err)
		}
		return c.(*net.TCPConn), s.(*net.TCPConn)
	}
	for _, mode := range []string{"client", "target", "idle"} {
		cli, userSide := pair()
		upSide, tgt := pair()
		ended := make(chan struct{})
		go func() { relay(userSide, upSide); close(ended) }()
		big := hcPattern(2<<20, 4)
		switch mode {
		case "client": // request, SHUT_WR, then the response
			cli.Write([]byte("req"))
			cli.CloseWrite()
			req, err := readAllT(tgt, 5*time.Second)
			if err != nil || string(req) != "req" {
				t.Fatalf("%s: req %q %v", mode, req, err)
			}
			go func() { tgt.Write(big); tgt.Close() }()
			got, err := readAllT(cli, 10*time.Second)
			if err != nil || string(got) != string(big) {
				t.Fatalf("%s: got %d of %d, %v", mode, len(got), len(big), err)
			}
		case "target": // response, SHUT_WR, then the upload
			tgt.Write([]byte("hi"))
			tgt.CloseWrite()
			hi, err := readAllT(cli, 5*time.Second)
			if err != nil || string(hi) != "hi" {
				t.Fatalf("%s: %q %v", mode, hi, err)
			}
			go func() { cli.Write(big); cli.Close() }()
			got, err := readAllT(tgt, 10*time.Second)
			if err != nil || string(got) != string(big) {
				t.Fatalf("%s: target got %d of %d, %v", mode, len(got), len(big), err)
			}
		case "idle": // SHUT_WR, and the target never answers: both reset
			cli.CloseWrite()
			_, err := readAllT(cli, 5*time.Second)
			var ne net.Error
			if err == nil || errors.As(err, &ne) && ne.Timeout() {
				t.Fatalf("%s: end %v, want a reset", mode, err)
			}
		}
		select {
		case <-ended:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: relay did not end", mode)
		}
		cli.Close()
		tgt.Close()
	}
}

// hc183 bound: with halfCloseMax relays already waiting half-closed, a new
// one closes at once (the pre-hc183 full close, counted in halfCloseOver);
// below the cap it half-closes as usual.
func TestHalfCloseCap(t *testing.T) {
	oldMax := halfCloseMax.Load()
	t.Cleanup(func() { halfCloseMax.Store(oldMax) })
	halfCloseMax.Store(2)
	before := halfOpenNow.Load()
	if before != 0 {
		t.Skipf("half-open relays left by other tests: %d", before)
	}
	if !halfOpenAdmit() || !halfOpenAdmit() {
		t.Fatal("admit below the cap")
	}
	over := halfCloseOver.Load()
	if halfOpenAdmit() || halfCloseOver.Load() != over+1 {
		t.Fatal("admit over the cap")
	}
	halfCloseMax.Store(0) // 0 = no cap
	if !halfOpenAdmit() {
		t.Fatal("admit with no cap")
	}
	halfOpenNow.Add(-3)

	// through a real tunnel: the cap held by fake occupants, the client's
	// SHUT_WR ends the connection at once (the target's late reply is not
	// relayed); once they leave, the reply comes through again
	hcPlanes(t, func(t *testing.T) {
		addr := startTunnelTarget(t, 3, 3, hcTarget(t, func(c *net.TCPConn) {
			req, _ := readAllT(c, 20*time.Second)
			if len(req) > 0 {
				time.Sleep(300 * time.Millisecond)
				c.Write([]byte("LATE"))
			}
		}))
		waitHC(t, addr)
		try := func() []byte {
			c := hcDial(t, addr)
			c.Write([]byte("x"))
			c.CloseWrite()
			got, _ := readAllT(c, 10*time.Second)
			c.Close()
			return got
		}
		halfCloseMax.Store(1)
		halfOpenNow.Add(1)
		over := halfCloseOver.Load()
		got := try()
		halfOpenNow.Add(-1)
		if len(got) != 0 || halfCloseOver.Load() == over {
			t.Fatalf("over the cap: got %q, over %d→%d", got, over, halfCloseOver.Load())
		}
		halfCloseMax.Store(oldMax)
		if got := try(); string(got) != "LATE" {
			t.Fatalf("below the cap: got %q", got)
		}
	})
}
