package engine

import (
	"context"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// holdProxy forwards edge→exit and exit→edge; while hold is set, edge→exit
// bytes wait (in order) until it is cleared.
type holdProxy struct {
	mu   sync.Mutex
	cond *sync.Cond
	hold bool
}

func (h *holdProxy) setHold(v bool) {
	h.mu.Lock()
	h.hold = v
	h.cond.Broadcast()
	h.mu.Unlock()
}

func (h *holdProxy) wait() {
	h.mu.Lock()
	for h.hold {
		h.cond.Wait()
	}
	h.mu.Unlock()
}

// heldLink is realLink with a hold proxy between the edge and the exit.
func heldLink(t *testing.T, ctx context.Context, panel string) (*mtcpLink, *holdProxy) {
	t.Helper()
	exitLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { exitLn.Close() })
	go func() {
		c, err := exitLn.Accept()
		if err != nil {
			return
		}
		go func() { <-ctx.Done(); c.Close() }()
		mtr := &linkMeter{}
		srv, _, err := newSession(c, true, nil, mtr)
		if err != nil {
			return
		}
		go func() { <-ctx.Done(); srv.Close() }()
		for {
			st, err := srv.AcceptStream()
			if err != nil {
				return
			}
			go serveStream(ctx, st, KharejConfig{Panel: panel}, nil, nil, nil, mtr)
		}
	}()
	hp := &holdProxy{}
	hp.cond = sync.NewCond(&hp.mu)
	pLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pLn.Close() })
	go func() {
		a, err := pLn.Accept()
		if err != nil {
			return
		}
		b, err := net.Dial("tcp", exitLn.Addr().String())
		if err != nil {
			a.Close()
			return
		}
		go func() { <-ctx.Done(); a.Close(); b.Close() }()
		go func() { // edge→exit, held while hold
			buf := make([]byte, 64<<10)
			for {
				n, err := a.Read(buf)
				if n > 0 {
					hp.wait()
					if _, err := b.Write(buf[:n]); err != nil {
						return
					}
				}
				if err != nil {
					return
				}
			}
		}()
		io.Copy(a, b)
	}()
	c, err := net.Dial("tcp", pLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	mtr := &linkMeter{statsPoll: make(chan struct{}, 1)}
	sess, why, err := newSession(c, false, nil, mtr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close(); c.Close() })
	return &mtcpLink{sess: sess, mtr: mtr, why: why}, hp
}

// reqPanel counts the "REQ!" requests it is sent (executes them) and answers
// each with one byte.
func reqPanel(t *testing.T, ctx context.Context) (string, *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { <-ctx.Done(); ln.Close() }()
	var got atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				b := make([]byte, 4)
				if _, err := io.ReadFull(c, b); err != nil {
					return
				}
				got.Add(1)
				c.Write([]byte("k"))
				io.Copy(io.Discard, c)
			}()
		}
	}()
	return ln.Addr().String(), &got
}

// A request whose first segments reach the exit late on a live, idle link (a
// lost segment and its resend: TCP sends it again an RTO later, then two RTO
// later, about 1.5 s at a 300 ms round trip) looks just like one on a dead
// link to the rescue: unanswered, on a link that lags and has heard nothing.
// It must stay, so it runs once, when it arrives — resetting it so the app
// reconnects at once ran it twice. Production timings (rescueWaitMin).
func TestRescueLeavesLateRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	panel, got := reqPanel(t, ctx)
	l0, hp := heldLink(t, ctx, panel)
	l1 := realLink(t, ctx, false, panel)
	m := NewLinkManager(nil, 1, 32, 8, nil)
	m.links = []*managedLink{m.newManaged(l0, 0, time.Now()), m.newManaged(l1, 1, time.Now())}
	m.links[1].flowing = 10 // picks go to link 0 while it does not lag
	// link 0 worked a moment ago: one stream answered, then silence (an
	// idle link hears only the exit's keepalive every 4–8 s)
	if st, _, rel, ok := openStreamOn(ctx, m, false, 0); ok {
		waitUntil(t, "an answer on link 0", 2*time.Second, func() bool { return answered(st) })
		st.Close()
		rel()
	}
	time.Sleep(1200 * time.Millisecond)

	hp.setHold(true)
	app, edge := tcpPair(t)
	st, link, release, ok := openStreamOn(ctx, m, false, 0)
	if !ok || link != Link(l0) {
		t.Fatal("the user did not land on link 0")
	}
	start := time.Now()
	go func() { time.Sleep(1700 * time.Millisecond); hp.setHold(false) }()
	ended := make(chan struct{})
	go func() { relayUser(ctx, edge, m, 0, st, link, release); close(ended) }()
	app.Write([]byte("REQ!"))
	app.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := app.Read(make([]byte, 1)); err != nil {
		t.Fatalf("app read error %v after %s, want its answer", err, time.Since(start))
	}
	t.Logf("answer after %s", time.Since(start).Round(time.Millisecond))
	time.Sleep(500 * time.Millisecond)
	if n, mv := got.Load(), m.rescue.moves.Load(); n != 1 || mv != 0 {
		t.Fatalf("the panel ran the request %d times (moves %d), want once", n, mv)
	}
	app.Close()
	<-ended
}
