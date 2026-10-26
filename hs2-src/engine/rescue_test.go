package engine

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// clientHello returns the first flight a Go TLS client sends: one
// ClientHello record.
func clientHello(t *testing.T) []byte {
	t.Helper()
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	go tls.Client(c1, &tls.Config{ServerName: "example.com", InsecureSkipVerify: true}).Handshake()
	c2.SetReadDeadline(time.Now().Add(5 * time.Second))
	hdr := make([]byte, 5)
	if _, err := io.ReadFull(c2, hdr); err != nil {
		t.Fatal(err)
	}
	body := make([]byte, int(hdr[3])<<8|int(hdr[4]))
	if _, err := io.ReadFull(c2, body); err != nil {
		t.Fatal(err)
	}
	return append(hdr, body...)
}

// splitRecords re-frames one handshake record's body into records of at
// most n bytes each.
func splitRecords(rec []byte, n int) []byte {
	var out []byte
	for body := rec[5:]; len(body) > 0; {
		k := min(n, len(body))
		out = append(out, rec[0], rec[1], rec[2], byte(k>>8), byte(k))
		out = append(out, body[:k]...)
		body = body[k:]
	}
	return out
}

func TestClientHelloPrefix(t *testing.T) {
	ch := clientHello(t)
	split := splitRecords(ch, 50)
	ccs := []byte{20, 3, 3, 0, 1, 1}
	cases := []struct {
		name string
		b    []byte
		want bool
	}{
		{"nothing", nil, true},
		{"hello", ch, true},
		{"hello split over records", split, true},
		{"start of a hello", ch[:100], true},
		{"start of a split hello", split[:200], true},
		{"hello and change cipher spec", append(append([]byte{}, ch...), ccs...), false},
		{"hello and a second handshake record", append(append([]byte{}, ch...), ch...), false},
		{"hello and application data", append(append([]byte{}, ch...), 23, 3, 3, 0, 1, 0), false},
		{"partial record header", ch[:3], false},
		{"plain request", []byte("REQ!"), false},
		{"http", []byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"), false},
		{"server hello", func() []byte { b := append([]byte{}, ch...); b[5] = 2; return b }(), false},
		{"ssl 3.0 record", func() []byte { b := append([]byte{}, ch...); b[2] = 0; return b }(), false},
		{"not tls inside", func() []byte { b := append([]byte{}, ch...); b[9] = 7; return b }(), false},
		{"empty record", []byte{22, 3, 1, 0, 0}, false},
	}
	for _, c := range cases {
		if got := replayable(c.b); got != c.want {
			t.Errorf("%s: replayable = %v, want %v", c.name, got, c.want)
		}
	}
}

// rsStream is a user stream whose answer, reads and end the test drives.
type rsStream struct {
	mu     sync.Mutex
	wrote  []byte
	in     chan []byte
	rest   []byte
	die    chan struct{}
	once   sync.Once
	answer atomic.Bool
}

func newRSStream() *rsStream {
	return &rsStream{in: make(chan []byte, 16), die: make(chan struct{})}
}

func (s *rsStream) Read(p []byte) (int, error) {
	if len(s.rest) == 0 {
		select {
		case b := <-s.in:
			s.rest = b
		case <-s.die:
			return 0, io.ErrClosedPipe
		}
	}
	n := copy(p, s.rest)
	s.rest = s.rest[n:]
	return n, nil
}

func (s *rsStream) Write(p []byte) (int, error) {
	select {
	case <-s.die:
		return 0, io.ErrClosedPipe
	default:
	}
	s.mu.Lock()
	s.wrote = append(s.wrote, p...)
	s.mu.Unlock()
	return len(p), nil
}

func (s *rsStream) Close() error              { s.once.Do(func() { close(s.die) }); return nil }
func (s *rsStream) GetDieCh() <-chan struct{} { return s.die }
func (s *rsStream) Answered() bool            { return s.answer.Load() }
func (s *rsStream) closed() bool {
	select {
	case <-s.die:
		return true
	default:
		return false
	}
}
func (s *rsStream) written() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte{}, s.wrote...)
}
func (s *rsStream) send(b []byte) { s.answer.Store(true); s.in <- b }

// rsLink opens rsStreams (gate, if set, holds each open until it closes).
// It has a meter, so it can lag and go quiet.
type rsLink struct {
	mu      sync.Mutex
	streams []*rsStream
	gate    chan struct{}
	alive   atomic.Bool
	mtr     *linkMeter
}

func newRSLink() *rsLink { l := &rsLink{mtr: &linkMeter{}}; l.alive.Store(true); return l }

func (l *rsLink) meter() *linkMeter         { return l.mtr }
func (l *rsLink) tcpStats() (tcpStat, bool) { return tcpStat{}, false }

func (l *rsLink) OpenStream() (stream, error) {
	if l.gate != nil {
		<-l.gate
	}
	s := newRSStream()
	l.mu.Lock()
	l.streams = append(l.streams, s)
	l.mu.Unlock()
	return s, nil
}
func (l *rsLink) Active() int32 { return 0 }
func (l *rsLink) Alive() bool   { return l.alive.Load() }
func (l *rsLink) Close() error  { return nil }
func (l *rsLink) opened() []*rsStream {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]*rsStream{}, l.streams...)
}

// rsPool is a pool of rsLinks, each with a meter so it can lag.
func rsPool(t *testing.T, n int) (*LinkManager, []*rsLink) {
	t.Helper()
	m := NewLinkManager(nil, 1, 32, 8, nil)
	var ls []*rsLink
	for i := 0; i < n; i++ {
		l := newRSLink()
		ls = append(ls, l)
		m.links = append(m.links, m.newManaged(l, i, time.Now()))
	}
	return m, ls
}

// lag makes link i of m lag (as a new stream left unanswered on it would).
func lag(m *LinkManager, i int) {
	m.mu.Lock()
	m.links[i].openSlowTill = ctrlNow() + int64(time.Minute)
	m.mu.Unlock()
}

func withRescueTimes(t *testing.T, wait, recheck time.Duration) {
	ow, or, og := rescueWaitMin, rescueRecheck, relayDieGrace.Load()
	rescueWaitMin, rescueRecheck = wait, recheck
	relayDieGrace.Store(int64(50 * time.Millisecond)) // a relay that took the old stream's end for its own would end soon
	t.Cleanup(func() {
		rescueWaitMin, rescueRecheck = ow, or
		relayDieGrace.Store(og)
	})
}

// tcpPair returns both ends of a loopback TCP connection: the app's and the
// edge's.
func tcpPair(t *testing.T) (app, edge *net.TCPConn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ch := make(chan net.Conn, 1)
	go func() { c, _ := ln.Accept(); ch <- c }()
	a, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	e := <-ch
	t.Cleanup(func() { a.Close(); e.Close() })
	return a.(*net.TCPConn), e.(*net.TCPConn)
}

// startRelay opens the app's stream on the pool's first pick and relays it
// as serveUserTCP does; ended closes when the relay returns.
func startRelay(t *testing.T, m *LinkManager, edge net.Conn) (first *rsStream, link Link, ended chan struct{}) {
	t.Helper()
	st, link, release, ok := openStreamOn(context.Background(), m, false, 0)
	if !ok {
		t.Fatal("no stream")
	}
	ended = make(chan struct{})
	go func() {
		relayUser(context.Background(), edge, m, 0, st, link, release)
		close(ended)
	}()
	return st.(*rsStream), link, ended
}

func waitUntil(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// A connection that sent one ClientHello on a link that went quiet moves:
// its stream opens on another link with the header and the hello, the old
// stream is closed and its slot given back, and the app goes on with the
// new stream as if nothing happened.
func TestRescueMovesUnansweredHello(t *testing.T) {
	withRescueTimes(t, 100*time.Millisecond, 20*time.Millisecond)
	m, ls := rsPool(t, 2)
	m.links[1].flowing = 10 // the first pick goes to link 0
	app, edge := tcpPair(t)
	first, link, ended := startRelay(t, m, edge)
	if link != ls[0] {
		t.Fatal("the first pick was not link 0")
	}
	hello := clientHello(t)
	app.Write(hello)
	waitUntil(t, "the hello on the first stream", time.Second, func() bool { return bytes.HasSuffix(first.written(), hello) })
	lag(m, 0) // it goes quiet
	waitUntil(t, "a stream on link 1", 2*time.Second, func() bool { return len(ls[1].opened()) == 1 })
	second := ls[1].opened()[0]
	waitUntil(t, "the hello sent again", time.Second, func() bool { return bytes.Equal(second.written(), append([]byte{kindTCPPort, 0, 0}, hello...)) })
	waitUntil(t, "the old stream closed", time.Second, first.closed)
	if n := m.links[0].users.Load(); n != 0 {
		t.Fatalf("link 0 keeps %d users after the move", n)
	}
	if n := m.links[1].users.Load(); n != 1 {
		t.Fatalf("link 1 has %d users, want 1", n)
	}
	time.Sleep(150 * time.Millisecond) // past relayDieGrace: the relay must not have taken the old stream's end for its own
	app.Write([]byte("more"))
	waitUntil(t, "the app's next bytes on the new stream", time.Second, func() bool { return bytes.HasSuffix(second.written(), []byte("more")) })
	second.send([]byte("server hello"))
	app.SetReadDeadline(time.Now().Add(2 * time.Second))
	got := make([]byte, len("server hello"))
	if _, err := io.ReadFull(app, got); err != nil || string(got) != "server hello" {
		t.Fatalf("app read %q, %v", got, err)
	}
	waitUntil(t, "the move counted", time.Second, func() bool { return m.rescue.moves.Load() == 1 })
	app.Close()
	<-ended
	if n := m.links[1].users.Load(); n != 0 || m.users.Load() != 0 {
		t.Fatalf("slots not given back: link 1 %d, pool %d", n, m.users.Load())
	}
	if !second.closed() {
		t.Fatal("the new stream was not closed at the end")
	}
}

// An app that has sent nothing yet moves too: there is nothing to repeat.
func TestRescueMovesSilentApp(t *testing.T) {
	withRescueTimes(t, 100*time.Millisecond, 20*time.Millisecond)
	m, ls := rsPool(t, 2)
	m.links[1].flowing = 10
	app, edge := tcpPair(t)
	first, _, ended := startRelay(t, m, edge)
	lag(m, 0)
	waitUntil(t, "a stream on link 1", 2*time.Second, func() bool { return len(ls[1].opened()) == 1 })
	second := ls[1].opened()[0]
	waitUntil(t, "the old stream closed", time.Second, first.closed)
	if w := second.written(); !bytes.Equal(w, []byte{kindTCPPort, 0, 0}) {
		t.Fatalf("new stream got %q, want the header only", w)
	}
	app.Write([]byte("SSH-2.0-x\r\n"))
	waitUntil(t, "the app's bytes on the new stream", time.Second, func() bool { return bytes.HasSuffix(second.written(), []byte("SSH-2.0-x\r\n")) })
	app.Close()
	<-ended
}

// Anything else stays where it is: it may already have done something at the
// panel, or still do (a segment lost on a live link arrives later), so it is
// neither sent again nor reset; the app gets its answer if one comes.
func TestRescueLeavesWhatCannotBeSentAgain(t *testing.T) {
	withRescueTimes(t, 100*time.Millisecond, 20*time.Millisecond)
	big := make([]byte, rescueKeep+1)
	big[0] = 22
	for _, c := range []struct {
		name string
		b    []byte
	}{
		{"request", []byte("REQ!")},
		{"hello and more", append(clientHello(t), 23, 3, 3, 0, 1, 0)},
		{"too much", big},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, ls := rsPool(t, 2)
			m.links[1].flowing = 10
			app, edge := tcpPair(t)
			first, _, ended := startRelay(t, m, edge)
			app.Write(c.b)
			waitUntil(t, "the bytes on the first stream", time.Second, func() bool { return len(first.written()) == len(userStreamHeader(false, 0))+len(c.b) }) // the header, then the bytes
			lag(m, 0)
			time.Sleep(400 * time.Millisecond)
			if n := len(ls[1].opened()); n != 0 || first.closed() || m.rescue.moves.Load() != 0 {
				t.Fatalf("%d stream(s) opened on link 1, first closed %v, moves %d: what the app sent could have run twice", n, first.closed(), m.rescue.moves.Load())
			}
			first.send([]byte("late answer"))
			app.SetReadDeadline(time.Now().Add(2 * time.Second))
			got := make([]byte, len("late answer"))
			if _, err := io.ReadFull(app, got); err != nil || string(got) != "late answer" {
				t.Fatalf("app read %q, %v", got, err)
			}
			app.Close()
			<-ended
			if m.users.Load() != 0 {
				t.Fatalf("pool keeps %d users", m.users.Load())
			}
		})
	}
}

// A stream the exit answered stays, however its link looks; so does one
// whose link does not lag, and one on a pool where most links lag (the
// path, not the link).
func TestRescueLeavesStream(t *testing.T) {
	withRescueTimes(t, 50*time.Millisecond, 10*time.Millisecond)
	for _, c := range []struct {
		name string
		set  func(m *LinkManager, first *rsStream)
	}{
		{"answered", func(m *LinkManager, first *rsStream) { first.answer.Store(true); lag(m, 0) }},
		{"link fine", func(m *LinkManager, first *rsStream) {}},
		{"most links lag", func(m *LinkManager, first *rsStream) { lag(m, 0); lag(m, 1) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, ls := rsPool(t, 3)
			m.links[1].flowing, m.links[2].flowing = 10, 10
			app, edge := tcpPair(t)
			first, _, ended := startRelay(t, m, edge)
			app.Write([]byte("REQ!"))
			c.set(m, first)
			time.Sleep(300 * time.Millisecond)
			if len(ls[1].opened())+len(ls[2].opened()) != 0 || first.closed() {
				t.Fatal("the connection was moved")
			}
			if mv := m.rescue.moves.Load(); mv != 0 {
				t.Fatalf("moves %d", mv)
			}
			app.Close()
			<-ended
		})
	}
}

// An answer that comes while the move's new stream opens wins: the
// connection stays, the app gets the old stream's data, and the new stream
// is closed with its slot given back.
func TestRescueAnswerDuringMove(t *testing.T) {
	withRescueTimes(t, 50*time.Millisecond, 10*time.Millisecond)
	m, ls := rsPool(t, 2)
	m.links[1].flowing = 10
	app, edge := tcpPair(t)
	first, _, ended := startRelay(t, m, edge)
	ls[1].gate = make(chan struct{})
	lag(m, 0)
	time.Sleep(200 * time.Millisecond) // the move waits for its open on link 1
	first.send([]byte("late answer"))
	app.SetReadDeadline(time.Now().Add(2 * time.Second))
	got := make([]byte, len("late answer"))
	if _, err := io.ReadFull(app, got); err != nil || string(got) != "late answer" {
		t.Fatalf("app read %q, %v", got, err)
	}
	close(ls[1].gate)
	waitUntil(t, "the move's stream", time.Second, func() bool { return len(ls[1].opened()) == 1 })
	second := ls[1].opened()[0]
	waitUntil(t, "the move's stream closed", time.Second, second.closed)
	waitUntil(t, "link 1's slot back", time.Second, func() bool { return m.links[1].users.Load() == 0 })
	if first.closed() {
		t.Fatal("the answered stream was closed")
	}
	if n := m.links[0].users.Load(); n != 1 {
		t.Fatalf("link 0 has %d users, want 1", n)
	}
	app.Close()
	<-ended
}

// A connection moves once: if its new link goes quiet too, it stays there.
func TestRescueMovesOnce(t *testing.T) {
	withRescueTimes(t, 100*time.Millisecond, 20*time.Millisecond)
	m, ls := rsPool(t, 4)
	m.links[1].flowing, m.links[2].flowing, m.links[3].flowing = 10, 20, 20
	app, edge := tcpPair(t)
	_, _, ended := startRelay(t, m, edge)
	lag(m, 0)
	waitUntil(t, "the move to link 1", 2*time.Second, func() bool { return len(ls[1].opened()) == 1 })
	second := ls[1].opened()[0]
	lag(m, 1)
	time.Sleep(400 * time.Millisecond)
	if n := len(ls[2].opened()) + len(ls[3].opened()); n != 0 || second.closed() {
		t.Fatalf("moved a second time (%d opens, the second stream closed %v)", n, second.closed())
	}
	second.send([]byte("answer"))
	app.SetReadDeadline(time.Now().Add(2 * time.Second))
	got := make([]byte, len("answer"))
	if _, err := io.ReadFull(app, got); err != nil || string(got) != "answer" {
		t.Fatalf("app read %q, %v", got, err)
	}
	app.Close()
	<-ended
	if m.users.Load() != 0 {
		t.Fatalf("pool keeps %d users", m.users.Load())
	}
}

// A connection that ends before its watch fires leaves nothing behind: no
// timer goroutine, no slot.
func TestRescueNoLeak(t *testing.T) {
	withRescueTimes(t, 50*time.Millisecond, 10*time.Millisecond)
	m, _ := rsPool(t, 2)
	base := runtime.NumGoroutine()
	for i := 0; i < 50; i++ {
		app, edge := tcpPair(t)
		first, _, ended := startRelay(t, m, edge)
		if i%2 == 0 {
			first.answer.Store(true)
		}
		app.Write([]byte("x"))
		app.Close()
		<-ended
	}
	time.Sleep(200 * time.Millisecond) // past every watch
	waitUntil(t, "goroutines back", 2*time.Second, func() bool { return runtime.NumGoroutine() <= base+2 })
	if m.users.Load() != 0 {
		t.Fatalf("pool keeps %d users", m.users.Load())
	}
}

func TestRescueRateLimit(t *testing.T) {
	var s rescueState
	now := time.Now()
	for i := 0; i < rescueBurst; i++ {
		if !s.take(now) {
			t.Fatalf("refused rescue %d of the burst", i)
		}
	}
	if s.take(now) {
		t.Fatal("a rescue past the burst")
	}
	now = now.Add(time.Second)
	n := 0
	for s.take(now) {
		n++
	}
	if n != rescueRate {
		t.Fatalf("%d moves a second later, want %d", n, rescueRate)
	}
}

// stranded: a dead or lagging link is left for one that does not lag; when
// most serving links lag, or none other is fine, nothing moves.
func TestStranded(t *testing.T) {
	cases := []struct {
		name string
		set  func(m *LinkManager, ls []*rsLink)
		want bool
	}{
		{"fine", func(m *LinkManager, ls []*rsLink) {}, false},
		{"lags", func(m *LinkManager, ls []*rsLink) { lag(m, 0) }, true},
		{"dead", func(m *LinkManager, ls []*rsLink) { ls[0].alive.Store(false) }, true},
		{"suspect", func(m *LinkManager, ls []*rsLink) { m.links[0].suspect = true }, true},
		{"retiring", func(m *LinkManager, ls []*rsLink) { m.links[0].retiring = true }, false},
		{"most lag", func(m *LinkManager, ls []*rsLink) { lag(m, 0); lag(m, 1) }, false},
		{"the others are down", func(m *LinkManager, ls []*rsLink) {
			lag(m, 0)
			ls[1].alive.Store(false)
			ls[2].alive.Store(false)
		}, false},
		{"gone from the pool", func(m *LinkManager, ls []*rsLink) { m.links = m.links[1:] }, true},
	}
	for _, c := range cases {
		m, ls := rsPool(t, 3)
		c.set(m, ls)
		if got := m.stranded(ls[0]); got != c.want {
			t.Errorf("%s: stranded = %v, want %v", c.name, got, c.want)
		}
	}
}

// realLink is an edge link over a real smux session on the loopback. Its exit
// serves user streams with the real serveStream in front of panel — or, when
// black, reads everything and answers nothing (a path that died on the way
// back).
func realLink(t *testing.T, ctx context.Context, black bool, panel string) *mtcpLink {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() { <-ctx.Done(); c.Close() }()
		if black {
			io.Copy(io.Discard, c)
			return
		}
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
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	mtr := &linkMeter{statsPoll: make(chan struct{}, 1)}
	sess, why, err := newSession(c, false, nil, mtr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close(); c.Close() })
	return &mtcpLink{sess: sess, mtr: mtr, why: why}
}

// helloPanel answers each connection's first TLS record with "server hello"
// and counts the records it got.
func helloPanel(t *testing.T, ctx context.Context) (string, *atomic.Int32) {
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
				hdr := make([]byte, 5)
				if _, err := io.ReadFull(c, hdr); err != nil {
					return
				}
				if _, err := io.ReadFull(c, make([]byte, int(hdr[3])<<8|int(hdr[4]))); err != nil {
					return
				}
				got.Add(1)
				c.Write([]byte("server hello"))
				io.Copy(io.Discard, c)
			}()
		}
	}()
	return ln.Addr().String(), &got
}

// End to end over real smux sessions: a ClientHello sent on a link whose way
// back died reaches the panel through another link, and the app gets the
// answer; a plain request there stays, and is sent nowhere else.
func TestRescueRealSessions(t *testing.T) {
	withRescueTimes(t, 100*time.Millisecond, 20*time.Millisecond)
	for _, hello := range []bool{true, false} {
		ctx, cancel := context.WithCancel(context.Background())
		panel, got := helloPanel(t, ctx)
		black := realLink(t, ctx, true, panel)
		good := realLink(t, ctx, false, panel)
		m := NewLinkManager(nil, 1, 32, 8, nil)
		m.links = []*managedLink{m.newManaged(black, 0, time.Now()), m.newManaged(good, 1, time.Now())}
		m.links[1].flowing = 10 // the first pick goes to the black link
		app, edge := tcpPair(t)
		st, link, release, ok := openStreamOn(ctx, m, false, 0)
		if !ok || link != Link(black) {
			t.Fatalf("hello %v: the first open did not land on the black link", hello)
		}
		ended := make(chan struct{})
		go func() { relayUser(ctx, edge, m, 0, st, link, release); close(ended) }()
		start := time.Now()
		app.SetReadDeadline(start.Add(5 * time.Second))
		if hello {
			app.Write(clientHello(t))
			reply := make([]byte, len("server hello"))
			if _, err := io.ReadFull(app, reply); err != nil || string(reply) != "server hello" {
				t.Fatalf("app read %q, %v", reply, err)
			}
			if n := got.Load(); n != 1 {
				t.Fatalf("panel got %d hellos, want 1", n)
			}
			waitUntil(t, "the move counted", time.Second, func() bool { return m.rescue.moves.Load() == 1 })
			// lagging needs the open unanswered for lagOpenWait (1 s)
			if d := time.Since(start); d < lagOpenWait || d > lagOpenWait+time.Second {
				t.Fatalf("rescued after %s", d)
			}
		} else {
			app.Write([]byte("REQ!"))
			app.SetReadDeadline(start.Add(lagOpenWait + time.Second))
			if _, err := app.Read(make([]byte, 1)); !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatalf("app read error %v, want its deadline", err)
			}
			if good.Active() != 0 || m.rescue.moves.Load() != 0 || got.Load() != 0 {
				t.Fatalf("a request was moved (streams on the good link %d, moves %d, panel got %d)", good.Active(), m.rescue.moves.Load(), got.Load())
			}
		}
		app.Close()
		<-ended
		if m.users.Load() != 0 {
			t.Fatalf("pool keeps %d users", m.users.Load())
		}
		cancel()
	}
}

// A link that lags but still receives is slow, not gone: a hello does not
// leave it until it falls silent (a request never does).
func TestRescueLeavesSlowLink(t *testing.T) {
	withRescueTimes(t, 50*time.Millisecond, 10*time.Millisecond)
	for _, hello := range []bool{true, false} {
		m, ls := rsPool(t, 2)
		m.links[1].flowing = 10
		stop := make(chan struct{})
		go func() { // another stream's data trickles in
			for {
				m.links[0].mtr.rxAt.Store(ctrlNow())
				select {
				case <-stop:
					return
				case <-time.After(20 * time.Millisecond):
				}
			}
		}()
		app, edge := tcpPair(t)
		first, _, ended := startRelay(t, m, edge)
		if hello {
			app.Write(clientHello(t))
		} else {
			app.Write([]byte("REQ!"))
		}
		lag(m, 0)
		time.Sleep(400 * time.Millisecond)
		if mv := m.rescue.moves.Load(); mv != 0 || len(ls[1].opened()) != 0 || first.closed() {
			t.Fatalf("hello %v: moves %d, opens on link 1 %d, closed %v", hello, mv, len(ls[1].opened()), first.closed())
		}
		// once it falls silent it is gone: a hello moves
		close(stop)
		if hello {
			waitUntil(t, "the move", 3*time.Second, func() bool { return m.rescue.moves.Load() == 1 })
		} else {
			time.Sleep(400 * time.Millisecond)
			if mv := m.rescue.moves.Load(); mv != 0 || len(ls[1].opened()) != 0 || first.closed() {
				t.Fatalf("a request moved: moves %d, opens on link 1 %d, closed %v", mv, len(ls[1].opened()), first.closed())
			}
		}
		app.Close()
		<-ended
	}
}

// nopStream is a stream whose reads and writes cost nothing, answered.
type nopStream struct{}

func (nopStream) Read(p []byte) (int, error)  { return len(p), nil }
func (nopStream) Write(p []byte) (int, error) { return len(p), nil }
func (nopStream) Close() error                { return nil }
func (nopStream) Answered() bool              { return true }

// The wrapper's cost on an answered connection's reads and writes (the
// relay's 32 KiB buffer), against the bare stream.
func BenchmarkRescuableSettled(b *testing.B) {
	m, ls := rsPool(&testing.T{}, 2)
	buf := make([]byte, 32<<10)
	for _, c := range []struct {
		name string
		st   stream
	}{
		{"bare", nopStream{}},
		{"wrapped", func() stream {
			r := newRescuable(context.Background(), m, 0, nopStream{}, ls[0], func() {})
			r.Read(buf) // data from the exit: settled
			return r
		}()},
	} {
		b.Run(c.name, func(b *testing.B) {
			b.SetBytes(int64(2 * len(buf)))
			for i := 0; i < b.N; i++ {
				c.st.Write(buf)
				c.st.Read(buf)
			}
		})
	}
}

// What watching a new connection costs until its answer: the wrapper, its
// timer and the copy of its first write (a 517-byte hello), up to the first
// reply (the timer stopped).
func BenchmarkRescuableOpenToAnswer(b *testing.B) {
	m, ls := rsPool(&testing.T{}, 2)
	hello := make([]byte, 517)
	buf := make([]byte, 32<<10)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		s := newRSStream()
		r := newRescuable(context.Background(), m, 0, s, ls[0], func() {})
		r.Write(hello)
		s.send(hello)
		r.Read(buf)
		r.Close()
	}
}

// A link is quiet when it is down, or when nothing came for openAnswerWait
// while its reader waits on the socket; a reader parked on a full receive
// buffer reads nothing on a live path, so its link is not quiet.
func TestLinkQuiet(t *testing.T) {
	now := ctrlNow()
	cases := []struct {
		name   string
		alive  bool
		rx     int64 // 0: never received
		guard  bool
		inRead bool
		want   bool
	}{
		{"down", false, now, false, false, true},
		{"just received", true, now, false, false, false},
		{"silent", true, now - int64(2*lagOpenWait), false, false, true},
		{"never received", true, 0, false, false, true},
		{"silent, reader waiting", true, now - int64(2*lagOpenWait), true, true, true},
		{"silent, reader parked", true, now - int64(2*lagOpenWait), true, false, false},
	}
	for _, c := range cases {
		l := newRSLink()
		l.alive.Store(c.alive)
		l.mtr.rxAt.Store(c.rx)
		if c.guard {
			l.mtr.guard = &sessGuard{w: &watchConn{}}
			l.mtr.guard.w.inRead.Store(c.inRead)
		}
		if got := linkQuiet(l); got != c.want {
			t.Errorf("%s: linkQuiet = %v, want %v", c.name, got, c.want)
		}
	}
}
