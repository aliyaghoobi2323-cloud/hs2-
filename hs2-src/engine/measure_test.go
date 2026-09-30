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

// flowStats counts a stream as flowing only while its rate EWMA is at least
// flowingRate: a handshake-sized write is not, a sustained transfer is, and
// closed streams are forgotten.
func TestFlowStatsFlowingNeedsSustainedRate(t *testing.T) {
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
	echo := func(s stream, n int) {
		b := make([]byte, n)
		if _, err := s.Write(b); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(s, b); err != nil {
			t.Fatal(err)
		}
	}
	t0 := time.Now()
	const dt = 2 * time.Second
	if fs := l.flowStats(t0, dt, 20*time.Second); fs.open != 3 || fs.flowing != 0 {
		t.Fatalf("fresh streams: %+v, want 3 open, 0 flowing", fs)
	}
	echo(ss[1], 2<<10) // a handshake: 4 KiB once (both ways)
	for i := 1; i <= 3; i++ {
		echo(ss[0], 64<<10) // 128 KiB per tick both ways: a real transfer
		fs := l.flowStats(t0.Add(time.Duration(i)*dt), dt, 20*time.Second)
		if i >= 2 && fs.flowing != 1 {
			t.Fatalf("tick %d: flowing=%d, want 1 (only the sustained stream): %+v", i, fs.flowing, fs)
		}
	}
	ss[2].Close()
	if fs := l.flowStats(t0.Add(8*time.Second), dt, 20*time.Second); fs.open != 2 {
		t.Fatalf("closed stream still counted: %+v", fs)
	}
	// Idle for a minute: the EWMA decays and nothing flows.
	var fs flowSnap
	for i := 0; i < 30; i++ {
		fs = l.flowStats(t0.Add(8*time.Second+time.Duration(i+1)*dt), dt, 20*time.Second)
	}
	if fs.flowing != 0 || fs.recent != 0 {
		t.Fatalf("after a quiet minute: %+v, want nothing flowing or recent", fs)
	}
}
