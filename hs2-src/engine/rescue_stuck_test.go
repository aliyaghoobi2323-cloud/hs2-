package engine

import (
	"context"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// echoHelloPanel answers each connection's first TLS record with "server
// hello", then echoes everything.
func echoHelloPanel(t *testing.T, ctx context.Context) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { <-ctx.Done(); ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				hdr := make([]byte, 5)
				if _, err := io.ReadFull(c, hdr); err != nil {
					return
				}
				if _, err := io.ReadFull(c, make([]byte, int(hdr[3])<<8|int(hdr[4]))); err != nil {
					return
				}
				c.Write([]byte("server hello"))
				io.Copy(c, c)
			}()
		}
	}()
	return ln.Addr().String()
}

// stuckLink is an edge link over a real smux session on a pipe whose exit
// side reads until black is set, then nothing: the link's writer gets stuck,
// as on a path that died with data queued.
func stuckLink(t *testing.T, ctx context.Context) (*mtcpLink, *atomic.Bool) {
	t.Helper()
	a, b := net.Pipe()
	black := new(atomic.Bool)
	go func() {
		buf := make([]byte, 64<<10)
		for !black.Load() {
			b.SetReadDeadline(time.Now().Add(10 * time.Millisecond))
			b.Read(buf)
		}
		<-ctx.Done()
	}()
	mtr := &linkMeter{statsPoll: make(chan struct{}, 1)}
	sess, why, err := newSession(a, false, nil, mtr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close(); a.Close(); b.Close() })
	return &mtcpLink{sess: sess, mtr: mtr, why: why}, black
}

// A write the app made on the old stream that waits for the dead link's
// writer (closing the old stream does not end it) must not hold the app's
// next bytes once the connection moved: they go on the new stream at once.
func TestRescueMoveFreesWriteStuckOnOldLink(t *testing.T) {
	withRescueTimes(t, 100*time.Millisecond, 20*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	panel := echoHelloPanel(t, ctx)

	l0, black := stuckLink(t, ctx)
	l1 := realLink(t, ctx, false, panel)

	m := NewLinkManager(nil, 1, 32, 8, nil)
	m.links = []*managedLink{m.newManaged(l0, 0, time.Now()), m.newManaged(l1, 1, time.Now())}
	m.links[1].flowing = 10
	st, link, release, ok := openStreamOn(ctx, m, false, 0)
	if !ok || link != Link(l0) {
		t.Fatal("not on link 0")
	}
	time.Sleep(50 * time.Millisecond)
	black.Store(true)
	app, edge := tcpPair(t)
	ended := make(chan struct{})
	go func() { relayUser(ctx, edge, m, 0, st, link, release); close(ended) }()
	app.Write(clientHello(t))
	app.SetReadDeadline(time.Now().Add(5 * time.Second))
	reply := make([]byte, len("server hello"))
	if _, err := io.ReadFull(app, reply); err != nil || string(reply) != "server hello" {
		t.Fatalf("app read %q, %v (moves %d)", reply, err, m.rescue.moves.Load())
	}
	app.Write([]byte("ping"))
	app.SetReadDeadline(time.Now().Add(3 * time.Second))
	got := make([]byte, 4)
	if _, err := io.ReadFull(app, got); err != nil || string(got) != "ping" {
		t.Fatalf("after the move, the app's next bytes were not echoed: %q, %v", got, err)
	}
	app.Close()
	cancel()
}

// gatedLink opens streams whose second write (the move's copy of what the
// app sent) waits for gate.
type gatedLink struct {
	*rsLink
	gate    chan struct{}
	blocked chan struct{}
}

type gatedStream struct {
	*rsStream
	l *gatedLink
	n atomic.Int32
}

func (s *gatedStream) Write(p []byte) (int, error) {
	if s.n.Add(1) == 2 {
		close(s.l.blocked)
		<-s.l.gate
	}
	return s.rsStream.Write(p)
}

func (l *gatedLink) OpenStream() (stream, error) {
	st, _ := l.rsLink.OpenStream()
	return &gatedStream{rsStream: st.(*rsStream), l: l}, nil
}

// The old stream ends while the move is sending the app's hello on the new
// one: the relay must follow the move, not take the old stream's end for the
// connection's and close it relayDieGrace later.
func TestRescueOldStreamEndsDuringMove(t *testing.T) {
	withRescueTimes(t, 100*time.Millisecond, 20*time.Millisecond)
	m := NewLinkManager(nil, 1, 32, 8, nil)
	l0 := newRSLink()
	l1 := &gatedLink{rsLink: newRSLink(), gate: make(chan struct{}), blocked: make(chan struct{})}
	m.links = []*managedLink{m.newManaged(l0, 0, time.Now()), m.newManaged(l1, 1, time.Now())}
	m.links[1].flowing = 10
	app, edge := tcpPair(t)
	first, _, ended := startRelay(t, m, edge)
	hello := clientHello(t)
	app.Write(hello)
	waitUntil(t, "hello on first", time.Second, func() bool { return len(first.written()) == len(userStreamHeader(false, 0))+len(hello) })
	lag(m, 0)
	select {
	case <-l1.blocked:
	case <-time.After(3 * time.Second):
		t.Fatal("no move")
	}
	first.Close() // the old link's session dies now
	time.Sleep(20 * time.Millisecond)
	close(l1.gate)
	waitUntil(t, "the move", time.Second, func() bool { return m.rescue.moves.Load() == 1 })
	time.Sleep(300 * time.Millisecond) // past relayDieGrace (50 ms)
	select {
	case <-ended:
		t.Fatalf("the relay ended although the connection moved to link 1")
	default:
	}
	app.Close()
	<-ended
}

// A ClientHello the app sends in pieces (as clients that fight censorship
// do): the link's writer stalls after the first piece went out, the second
// waits in the old stream's Write, the third is still in the app's socket.
// The connection moves, and the whole hello reaches the panel.
func TestRescueMovesFragmentedHelloOffStuckLink(t *testing.T) {
	withRescueTimes(t, 100*time.Millisecond, 20*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	panel := echoHelloPanel(t, ctx)

	l0, black := stuckLink(t, ctx)
	l1 := realLink(t, ctx, false, panel)

	m := NewLinkManager(nil, 1, 32, 8, nil)
	m.links = []*managedLink{m.newManaged(l0, 0, time.Now()), m.newManaged(l1, 1, time.Now())}
	m.links[1].flowing = 10
	st, link, release, ok := openStreamOn(ctx, m, false, 0)
	if !ok || link != Link(l0) {
		t.Fatal("not on link 0")
	}
	app, edge := tcpPair(t)
	go relayUser(ctx, edge, m, 0, st, link, release)
	hello := clientHello(t)
	app.Write(hello[:100])
	time.Sleep(50 * time.Millisecond)
	black.Store(true)
	time.Sleep(30 * time.Millisecond)
	app.Write(hello[100:200])
	time.Sleep(30 * time.Millisecond)
	app.Write(hello[200:])
	app.SetReadDeadline(time.Now().Add(4 * time.Second))
	reply := make([]byte, len("server hello"))
	if _, err := io.ReadFull(app, reply); err != nil || string(reply) != "server hello" {
		t.Fatalf("app read %q, %v (moves %d)", reply, err, m.rescue.moves.Load())
	}
}
