package engine

import (
	"io"
	"net"
	"testing"
	"time"

	"github.com/xtaci/smux"
)

// The writer-blocked counter reads ~0 when the other side takes data at once,
// and most of the elapsed time when the other side is slow — the property the
// saturation signal relies on.
func TestMeteredConnBlockedTime(t *testing.T) {
	run := func(slow bool) float64 {
		a, b := net.Pipe()
		defer a.Close()
		defer b.Close()
		go func() {
			buf := make([]byte, 4096)
			for {
				if slow {
					time.Sleep(5 * time.Millisecond)
				}
				if _, err := b.Read(buf); err != nil {
					return
				}
			}
		}()
		m := &linkMeter{}
		c := &meteredConn{Conn: a, m: m}
		frame := make([]byte, 16<<10)
		start := time.Now()
		for time.Since(start) < 300*time.Millisecond {
			if _, err := c.Write(frame); err != nil {
				break
			}
		}
		return float64(m.wrBlocked.Load()) / float64(time.Since(start))
	}
	if fast := run(false); fast > 0.2 {
		t.Fatalf("unconstrained writer reads as blocked: %.0f%%", fast*100)
	}
	if slow := run(true); slow < 0.6 {
		t.Fatalf("constrained writer not detected as blocked: %.0f%%", slow*100)
	}
}

// flowActivity counts only streams that moved a byte within the window, and
// forgets closed ones.
func TestFlowActivityCountsOnlyActiveStreams(t *testing.T) {
	ca, cb := net.Pipe()
	cli, err := smux.Client(ca, newSmuxConfig())
	if err != nil {
		t.Fatal(err)
	}
	srv, err := smux.Server(cb, newSmuxConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	defer srv.Close()
	go func() { // exit side: echo every stream
		for {
			st, err := srv.AcceptStream()
			if err != nil {
				return
			}
			go io.Copy(st, st)
		}
	}()
	l := &mtcpLink{sess: cli}
	var ss []stream
	for i := 0; i < 3; i++ {
		s, err := l.OpenStream()
		if err != nil {
			t.Fatal(err)
		}
		ss = append(ss, s)
	}
	t0 := time.Now()
	if a, tot, _ := l.flowActivity(t0, 20*time.Second); a != 3 || tot != 3 {
		t.Fatalf("fresh streams: active=%d total=%d, want 3/3", a, tot)
	}
	// Move data on stream 0 only.
	ss[0].Write([]byte("hello"))
	buf := make([]byte, 5)
	io.ReadFull(ss[0], buf)
	if a, tot, _ := l.flowActivity(t0.Add(30*time.Second), 20*time.Second); a != 1 || tot != 3 {
		t.Fatalf("after 30s with traffic on one stream: active=%d total=%d, want 1/3", a, tot)
	}
	ss[1].Close()
	if _, tot, _ := l.flowActivity(t0.Add(31*time.Second), 20*time.Second); tot != 2 {
		t.Fatalf("closed stream still counted: total=%d, want 2", tot)
	}
}
