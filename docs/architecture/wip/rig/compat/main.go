// compat: the need window (vendored smux, this tree) as the receiver of an
// upstream v1.5.24 sender and of the base hs2 (c9825f2) smux sender, over an
// in-memory path with a rate and a one-way delay (no loss). Checks data
// integrity, that the larger windows are honoured (throughput past what
// 2 MiB per round trip allows), that a window taken down by the idle sweep
// is honoured too (no more lands than the receiver allowed), and that the
// pool is whole again at the end.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"reflect"
	"sync"
	"time"
	"unsafe"

	oldsmux "example.com/oldsmux"
	upsmux "example.com/upsmux"
	"github.com/xtaci/smux"
)

// ---- in-memory path: rate (bytes/s) and one-way delay per direction ----

type pkt struct {
	at time.Time
	b  []byte
}

type half struct {
	mu       sync.Mutex
	nextFree time.Time
	rate     float64
	delay    time.Duration
	q        chan pkt
	pend     []byte
	closed   chan struct{}
	once     sync.Once
}

func newHalf(rate float64, delay time.Duration) *half {
	return &half{rate: rate, delay: delay, q: make(chan pkt, 1<<16), closed: make(chan struct{})}
}

func (h *half) write(p []byte) (int, error) {
	select {
	case <-h.closed:
		return 0, io.ErrClosedPipe
	default:
	}
	for off := 0; off < len(p); off += 1448 {
		end := min(off+1448, len(p))
		b := append([]byte(nil), p[off:end]...)
		h.mu.Lock()
		now := time.Now()
		if h.nextFree.Before(now) {
			h.nextFree = now
		}
		h.nextFree = h.nextFree.Add(time.Duration(float64(len(b)) / h.rate * 1e9))
		at := h.nextFree.Add(h.delay)
		// a sender may not run far ahead of the path: block while the queue
		// holds more than 4 s of it (a socket buffer)
		wait := time.Until(h.nextFree) - 4*time.Second
		h.mu.Unlock()
		if wait > 0 {
			time.Sleep(wait)
		}
		select {
		case h.q <- pkt{at, b}:
		case <-h.closed:
			return off, io.ErrClosedPipe
		}
	}
	return len(p), nil
}

func (h *half) read(p []byte) (int, error) {
	if len(h.pend) == 0 {
		select {
		case pk := <-h.q:
			if d := time.Until(pk.at); d > 0 {
				time.Sleep(d)
			}
			h.pend = pk.b
		case <-h.closed:
			return 0, io.EOF
		}
	}
	n := copy(p, h.pend)
	h.pend = h.pend[n:]
	return n, nil
}

func (h *half) close() { h.once.Do(func() { close(h.closed) }) }

type end struct{ in, out *half }

func (e end) Read(p []byte) (int, error)  { return e.in.read(p) }
func (e end) Write(p []byte) (int, error) { return e.out.write(p) }
func (e end) Close() error                { e.in.close(); e.out.close(); return nil }

func path(rate float64, delay time.Duration) (a, b io.ReadWriteCloser) {
	ab, ba := newHalf(rate, delay), newHalf(rate, delay)
	return end{in: ba, out: ab}, end{in: ab, out: ba}
}

// ---- the receivers and senders ----

var rttFlag = flag.Duration("rtt", 150*time.Millisecond, "round trip")
var mbit = flag.Float64("mbit", 300, "path rate, Mbit/s")
var size = flag.Int("mb", 64, "MiB to send")
var trial = flag.Bool("trial", true, "probe: grow past NeedStreamBuffer by trial (GrowByTrial)")

func newCfg(pool *smux.WindowPool) *smux.Config {
	c := smux.DefaultConfig()
	c.Version = 2
	c.KeepAliveInterval = 2 * time.Second
	c.KeepAliveTimeout = 30 * time.Second
	c.MaxFrameSize = 16 << 10
	c.MaxReceiveBuffer = 8 << 20
	c.MaxStreamBuffer = 16 << 20
	c.MinStreamBuffer = 64 << 10
	c.StreamLagTarget = 128 << 10
	c.NeedStreamBuffer = 2 << 20
	c.WindowPool = pool
	c.GrowByTrial = *trial
	rtt := *rttFlag
	c.RoundTrip = func() time.Duration { return rtt }
	return c
}

func upCfg() *upsmux.Config {
	c := upsmux.DefaultConfig()
	c.Version = 2
	c.KeepAliveInterval = 2 * time.Second
	c.KeepAliveTimeout = 30 * time.Second
	c.MaxFrameSize = 16 << 10
	c.MaxReceiveBuffer = 8 << 20
	c.MaxStreamBuffer = 2 << 20
	return c
}

func oldCfg() *oldsmux.Config {
	c := oldsmux.DefaultConfig()
	c.Version = 2
	c.KeepAliveInterval = 2 * time.Second
	c.KeepAliveTimeout = 30 * time.Second
	c.MaxFrameSize = 16 << 10
	c.MaxReceiveBuffer = 8 << 20
	c.MaxStreamBuffer = 2 << 20
	c.MinStreamBuffer = 64 << 10
	c.StreamLagTarget = 128 << 10
	return c
}

// sender opens a stream on its side of the path and writes n bytes of a
// counting pattern; it returns the stream's writer side.
type opener func(io.ReadWriteCloser) (io.WriteCloser, func(), error)

func upOpener(c io.ReadWriteCloser) (io.WriteCloser, func(), error) {
	s, err := upsmux.Client(c, upCfg())
	if err != nil {
		return nil, nil, err
	}
	st, err := s.OpenStream()
	return st, func() { s.Close() }, err
}

func oldOpener(c io.ReadWriteCloser) (io.WriteCloser, func(), error) {
	s, err := oldsmux.Client(c, oldCfg())
	if err != nil {
		return nil, nil, err
	}
	st, err := s.OpenStream()
	return st, func() { s.Close() }, err
}

func newOpener(c io.ReadWriteCloser) (io.WriteCloser, func(), error) {
	s, err := smux.Client(c, newCfg(smux.NewWindowPool(128<<20)))
	if err != nil {
		return nil, nil, err
	}
	st, err := s.OpenStream()
	return st, func() { s.Close() }, err
}

func pattern(off int64, b []byte) {
	for i := range b {
		b[i] = byte((off + int64(i)) * 7 >> 3)
	}
}

// run: sender (by opener) -> need receiver; the reader takes everything,
// pausing pauseFor after pauseAt bytes (0: no pause). Reports Mbit/s of the
// part before the pause, whether the data was intact, the most the
// receiver held unread during the pause, and the pool at the end.
func run(name string, open opener, pauseAt int64, pauseFor time.Duration) error {
	pool := smux.NewWindowPool(128 << 20)
	a, b := path(*mbit*1e6/8, *rttFlag/2)
	rs, err := smux.Server(b, newCfg(pool))
	if err != nil {
		return err
	}
	w, closeSender, err := open(a)
	if err != nil {
		return err
	}
	total := int64(*size) << 20
	go func() {
		buf := make([]byte, 32<<10)
		for off := int64(0); off < total; off += int64(len(buf)) {
			pattern(off, buf)
			if _, err := w.Write(buf); err != nil {
				return
			}
		}
	}()
	st, err := rs.AcceptStream()
	if err != nil {
		return err
	}
	buf := make([]byte, 32<<10)
	want := make([]byte, 32<<10)
	var got int64
	start := time.Now()
	var fastDur time.Duration
	heldMax, extraMax := 0, 0
	for got < total {
		if pauseAt > 0 && got >= pauseAt && fastDur == 0 {
			fastDur = time.Since(start)
			end := time.Now().Add(pauseFor)
			for time.Now().Before(end) {
				time.Sleep(50 * time.Millisecond)
				// unread = what the receiver's buffer holds past the bucket
				if e := rs.Extra(); e > extraMax {
					extraMax = e
				}
				if h := unreadOf(st); h > heldMax {
					heldMax = h
				}
			}
		}
		st.SetReadDeadline(time.Now().Add(20 * time.Second))
		n, err := st.Read(buf)
		if n > 0 {
			pattern(got, want[:n])
			for i := 0; i < n; i++ {
				if buf[i] != want[i] {
					return fmt.Errorf("%s: corrupt at %d", name, got+int64(i))
				}
			}
			got += int64(n)
		}
		if err != nil {
			return fmt.Errorf("%s: read after %d: %v", name, got, err)
		}
		if e := rs.Extra(); e > extraMax {
			extraMax = e
		}
	}
	if fastDur == 0 {
		fastDur = time.Since(start)
	}
	fastBytes := total
	if pauseAt > 0 {
		fastBytes = pauseAt
	}
	st.Close()
	closeSender()
	time.Sleep(200 * time.Millisecond)
	rs.Close()
	time.Sleep(100 * time.Millisecond)
	fmt.Printf("%-28s %6.1f Mbit/s (2 MiB/RTT: %5.1f)  intact  extraMax %5.1f MiB  heldMax(pause) %5.1f MiB  borrows %d  pool back %v\n",
		name, float64(fastBytes)*8/fastDur.Seconds()/1e6, float64(2<<20)*8/rttFlag.Seconds()/1e6,
		float64(extraMax)/(1<<20), float64(heldMax)/(1<<20), rs.Borrows(), pool.Free() == 128<<20)
	if pool.Free() != 128<<20 {
		return errors.New(name + ": pool not whole at the end")
	}
	return nil
}

// the reverse direction: the need (probe) sender -> an upstream v1.5.24 or
// c9825f2 receiver, which keeps its own fixed 2 MiB window: data intact, and
// the sender honours that window (no more in flight than it allows: the pace
// stays under 2 MiB per round trip).
type acceptor func(io.ReadWriteCloser) (io.ReadCloser, func(), error)

func upAcceptor(c io.ReadWriteCloser) (io.ReadCloser, func(), error) {
	s, err := upsmux.Server(c, upCfg())
	if err != nil {
		return nil, nil, err
	}
	st, err := s.AcceptStream()
	return st, func() { s.Close() }, err
}

func oldAcceptor(c io.ReadWriteCloser) (io.ReadCloser, func(), error) {
	s, err := oldsmux.Server(c, oldCfg())
	if err != nil {
		return nil, nil, err
	}
	st, err := s.AcceptStream()
	return st, func() { s.Close() }, err
}

func runRev(name string, acc acceptor) error {
	pool := smux.NewWindowPool(128 << 20)
	a, b := path(*mbit*1e6/8, *rttFlag/2)
	cs, err := smux.Client(a, newCfg(pool))
	if err != nil {
		return err
	}
	total := int64(*size) << 20
	w, err := cs.OpenStream()
	if err != nil {
		return err
	}
	go func() {
		buf := make([]byte, 32<<10)
		for off := int64(0); off < total; off += int64(len(buf)) {
			pattern(off, buf)
			if _, err := w.Write(buf); err != nil {
				return
			}
		}
	}()
	st, closeRecv, err := acc(b)
	if err != nil {
		return err
	}
	buf := make([]byte, 32<<10)
	want := make([]byte, 32<<10)
	var got int64
	start := time.Now()
	for got < total {
		n, err := st.Read(buf)
		if n > 0 {
			pattern(got, want[:n])
			for i := 0; i < n; i++ {
				if buf[i] != want[i] {
					return fmt.Errorf("%s: corrupt at %d", name, got+int64(i))
				}
			}
			got += int64(n)
		}
		if err != nil {
			return fmt.Errorf("%s: read after %d: %v", name, got, err)
		}
	}
	d := time.Since(start)
	w.Close()
	st.Close()
	closeRecv()
	cs.Close()
	time.Sleep(200 * time.Millisecond)
	mb := float64(total) * 8 / d.Seconds() / 1e6
	lim := float64(2<<20) * 8 / rttFlag.Seconds() / 1e6
	fmt.Printf("%-28s %6.1f Mbit/s (2 MiB/RTT: %5.1f)  intact  pool back %v\n", name, mb, lim, pool.Free() == 128<<20)
	if mb > lim*1.1 {
		return fmt.Errorf("%s: %.1f Mbit/s past the receiver's 2 MiB window (%.1f)", name, mb, lim)
	}
	if pool.Free() != 128<<20 {
		return errors.New(name + ": pool not whole at the end")
	}
	return nil
}

func main() {
	flag.Parse()
	_ = net.IPv4len
	fail := false
	for _, c := range []struct {
		name string
		a    acceptor
	}{{"need -> upstream v1.5.24", upAcceptor}, {"need -> hs2 c9825f2", oldAcceptor}} {
		if err := runRev(c.name, c.a); err != nil {
			fmt.Println("FAIL", err)
			fail = true
		}
	}
	for _, c := range []struct {
		name string
		o    opener
	}{{"upstream v1.5.24 -> need", upOpener}, {"hs2 c9825f2 -> need", oldOpener}, {"need -> need", newOpener}} {
		if err := run(c.name, c.o, 0, 0); err != nil {
			fmt.Println("FAIL", err)
			fail = true
		}
		if err := run(c.name+" +pause", c.o, 24<<20, 8*time.Second); err != nil {
			fmt.Println("FAIL", err)
			fail = true
		}
	}
	if fail {
		os.Exit(1)
	}
}

// unreadOf peeks at a stream's unread count (racy, for the report only)
func unreadOf(st *smux.Stream) int {
	f := reflect.ValueOf(st).Elem().FieldByName("unread")
	return int(*(*int)(unsafe.Pointer(f.UnsafeAddr())))
}
