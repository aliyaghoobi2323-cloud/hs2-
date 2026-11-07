package engine

import (
	"io"
	"net"
	"testing"
	"time"

	"github.com/xtaci/smux"
)

func setHeavyBacklog(t *testing.T, on bool) {
	t.Helper()
	old := heavyBacklog
	heavyBacklog = on
	t.Cleanup(func() { heavyBacklog = old })
}

// feed moves total bytes through cs in chunks of size (the last one shorter).
func feed(cs *countedStream, total, size int) {
	for total > 0 {
		n := min(size, total)
		cs.moved(n)
		total -= n
	}
}

// heavyFlows runs 10 s of samples over four streams beside each other:
//
//	call:  a steady 3 Mbit/s call on a link with room — never waits, so each
//	       read/write carries the ~1.2 KB the app sent since the last one;
//	dl:    a 5 Mbit/s download filling its link — full 16 KiB frames;
//	up:    a 1 Mbit/s upload filling its link — full 32 KiB writes;
//	small: a stream that waited once a sample but moves 8 KB/s.
func heavyFlows(t *testing.T) (l *mtcpLink, call, dl, up, small *countedStream, now time.Time) {
	t.Helper()
	l = &mtcpLink{flows: map[*countedStream]struct{}{}}
	call, dl, up, small = &countedStream{}, &countedStream{}, &countedStream{}, &countedStream{}
	for _, cs := range []*countedStream{call, dl, up, small} {
		l.flows[cs] = struct{}{}
	}
	now = time.Now()
	sec := int(healthTick / time.Second)
	for i := 0; i < 5; i++ {
		now = now.Add(healthTick)
		feed(call, 375<<10*sec, 1200)
		feed(dl, 625<<10*sec, SmuxFrameSize)
		feed(dl, 8<<10*sec, 100) // its acks the other way
		feed(up, 125<<10*sec, 32<<10)
		feed(small, 8<<10*sec, 16<<10)
		feed(small, 1<<10*sec, 200)
		l.flowStats(now, healthTick, time.Minute)
	}
	return
}

// With HS2_HEAVY_BACKLOG a stream is heavy while it builds a queue, not while
// it is fast: the steady call (heavy by rate before) is not, the download and
// the upload filling their links are, the small stream that waited is not.
func TestHeavyBacklog(t *testing.T) {
	setHeavyBacklog(t, true)
	l, call, dl, up, small, now := heavyFlows(t)
	if call.heavy(now) {
		t.Fatalf("a steady 3 Mbit/s call that never waits counts as heavy (rate %.0f B/s, backlog %.0f B/s)", call.ewma, call.backlogEwma)
	}
	if !dl.heavy(now) {
		t.Fatalf("a download filling its link is not heavy (rate %.0f B/s, backlog %.0f B/s)", dl.ewma, dl.backlogEwma)
	}
	if !up.heavy(now) {
		t.Fatalf("an upload filling its link is not heavy (rate %.0f B/s, backlog %.0f B/s)", up.ewma, up.backlogEwma)
	}
	if small.heavy(now) {
		t.Fatalf("a small stream that waited counts as heavy (rate %.0f B/s, backlog %.0f B/s)", small.ewma, small.backlogEwma)
	}
	if fs := l.flowStats(now.Add(healthTick), healthTick, time.Minute); fs.heavy != 2 {
		t.Fatalf("heavy %d, want 2 (the download and the upload)", fs.heavy)
	}
	// The download ends: it stops counting within flowRecent, as before.
	for i := 0; i < 4; i++ {
		now = now.Add(healthTick)
		feed(up, 250<<10, 32<<10)
		feed(call, 750<<10, 1200)
		l.flowStats(now, healthTick, time.Minute)
	}
	if dl.heavy(now) || !up.heavy(now) || call.heavy(now) {
		t.Fatalf("after the download ended: dl %v up %v call %v, want false true false", dl.heavy(now), up.heavy(now), call.heavy(now))
	}
}

// Without the key nothing changes: heavy is the rate rule (the call and the
// download are heavy, the 1 Mbit/s upload and the small stream are not).
func TestHeavyBacklogOff(t *testing.T) {
	setHeavyBacklog(t, false)
	l, call, dl, up, small, now := heavyFlows(t)
	for _, c := range []struct {
		name string
		cs   *countedStream
	}{{"call", call}, {"dl", dl}, {"up", up}, {"small", small}} {
		want := float64(c.cs.ewma) >= heavyRate
		if got := c.cs.heavy(now); got != want {
			t.Fatalf("%s: heavy %v, the rate rule says %v", c.name, got, want)
		}
		if c.cs.backlogEwma != 0 {
			t.Fatalf("%s: backlog EWMA %v kept with the key off", c.name, c.cs.backlogEwma)
		}
	}
	if !call.heavy(now) || !dl.heavy(now) || up.heavy(now) || small.heavy(now) {
		t.Fatal("without the key: want the call and the download heavy, as before")
	}
	if fs := l.flowStats(now.Add(healthTick), healthTick, time.Minute); fs.heavy != 2 {
		t.Fatalf("heavy %d, want 2", fs.heavy)
	}
}

// slowConn caps a link's send rate (the exit's side of a link slower than its
// source).
type slowConn struct {
	net.Conn
	rate float64 // bytes/s
}

func (c slowConn) Write(p []byte) (int, error) {
	time.Sleep(time.Duration(float64(len(p)) / c.rate * float64(time.Second)))
	return c.Conn.Write(p)
}

// The signal end to end over a real smux session: the exit relays a source
// socket into a stream (relayStream, 32 KiB reads), the edge reads it through
// a countedStream. A source with more than the link can carry ends up in
// backlog chunks; a paced call far below the link's rate does not.
func TestHeavyBacklogChunksOverSmux(t *testing.T) {
	run := func(t *testing.T, src func(c net.Conn)) *countedStream {
		ca, cb := net.Pipe()
		exit, err := smux.Server(slowConn{ca, 20 << 20}, newSmuxConfig())
		if err != nil {
			t.Fatal(err)
		}
		defer exit.Close()
		edge, err := smux.Client(cb, newSmuxConfig())
		if err != nil {
			t.Fatal(err)
		}
		defer edge.Close()
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		go func() {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			src(c)
			c.Close()
		}()
		go func() {
			st, err := exit.AcceptStream()
			if err != nil {
				return
			}
			up, err := net.Dial("tcp", ln.Addr().String())
			if err != nil {
				st.Close()
				return
			}
			relayStream(up, st, nil)
		}()
		s, err := edge.OpenStream()
		if err != nil {
			t.Fatal(err)
		}
		cs := &countedStream{Stream: s, link: &mtcpLink{flows: map[*countedStream]struct{}{}}}
		buf := make([]byte, 32<<10)
		cs.SetReadDeadline(time.Now().Add(10 * time.Second))
		for {
			if _, err := cs.Read(buf); err != nil {
				if err != io.EOF {
					t.Fatalf("read: %v", err)
				}
				break
			}
		}
		return cs
	}
	share := func(cs *countedStream) float64 { return float64(cs.backlog.Load()) / float64(cs.bytes.Load()) }

	dl := run(t, func(c net.Conn) {
		b := make([]byte, 64<<10)
		for i := 0; i < 48; i++ { // 3 MiB, as fast as the socket takes it
			if _, err := c.Write(b); err != nil {
				return
			}
		}
	})
	if dl.bytes.Load() != 3<<20 || share(dl) < 0.8 {
		t.Fatalf("download: %d bytes, %.2f in backlog chunks — want 3 MiB, at least 0.8", dl.bytes.Load(), share(dl))
	}
	call := run(t, func(c net.Conn) {
		b := make([]byte, 1200)
		for i := 0; i < 150; i++ { // 600 KB/s, 1/35 of the link
			if _, err := c.Write(b); err != nil {
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	})
	if call.bytes.Load() != 150*1200 || share(call) >= backlogShare {
		t.Fatalf("call: %d bytes, %.2f in backlog chunks — want %d, below %.1f", call.bytes.Load(), share(call), 150*1200, backlogShare)
	}
	t.Logf("backlog share: download %.2f, call %.2f", share(dl), share(call))
}
