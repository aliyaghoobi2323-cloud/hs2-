package engine

import (
	"io"
	"math/rand"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

// A link's path emulated for its sockets together (round 2, β1): one
// bottleneck each way that the link's sockets share, a pair's two or a
// single one, so a pair's QUIET and BULK compete for the same rate as they
// do on the rig.

// ppOpts shape a pipe: a FIFO bottleneck (fifo: first written, first
// served) or a fair-queued one (fq_codel's per-flow queues: the sockets with
// bytes waiting take turns, a packet each); and a dropping one (loss: a
// packet of a socket with a standing queue, lossQ bytes or more waiting, is
// lost with this probability, and arrives a round trip late, its resend,
// with every later byte of its socket behind it, as TCP holds them).
type ppOpts struct {
	fifo  bool
	loss  float64
	lossQ int
	seed  int64
}

// ppPkt: the bottleneck serves a socket at most this much at a turn.
const ppPkt = 1514

// ppDir is one direction of the path: the sending sockets and the
// bottleneck that serves them.
type ppDir struct {
	mu     sync.Mutex
	cond   *sync.Cond
	rate   float64
	delay  time.Duration
	o      ppOpts
	rng    *rand.Rand
	socks  []*ppSock
	order  []*ppSock // fifo: the socket of each chunk written, in order
	rr     int
	closed bool
}

// ppSock is a socket's sending side in a direction: what is written and not
// served yet (its unsent bytes, under its low-water mark), and what was
// served, with when it arrives.
type ppSock struct {
	d       *ppDir
	q       [][]byte
	unsent  int
	lowat   int
	maxNS   int
	in      []wcChunk
	lastArr time.Time
	served  int64
	lost    int
}

func newPPDir(rate float64, delay time.Duration, n int, o ppOpts) *ppDir {
	d := &ppDir{rate: rate, delay: delay, o: o, rng: rand.New(rand.NewSource(o.seed + 1))}
	d.cond = sync.NewCond(&d.mu)
	for range n {
		d.socks = append(d.socks, &ppSock{d: d, lowat: 32 << 10})
	}
	go d.run()
	return d
}

// pick is the socket the bottleneck serves next (nil: none has bytes
// waiting).
func (d *ppDir) pick() *ppSock {
	if d.o.fifo {
		if len(d.order) == 0 {
			return nil
		}
		return d.order[0]
	}
	for k := range d.socks {
		s := d.socks[(d.rr+k)%len(d.socks)]
		if s.unsent > 0 {
			d.rr = (d.rr + k + 1) % len(d.socks)
			return s
		}
	}
	return nil
}

func (d *ppDir) run() {
	last := time.Now()
	credit := 0.0
	for {
		time.Sleep(200 * time.Microsecond)
		now := time.Now()
		d.mu.Lock()
		if d.closed {
			d.mu.Unlock()
			return
		}
		credit += d.rate * now.Sub(last).Seconds()
		last = now
		for credit >= 1 {
			s := d.pick()
			if s == nil {
				credit = min(credit, ppPkt) // an idle path saves up no more than a packet
				break
			}
			n := min(len(s.q[0]), ppPkt, int(credit))
			piece := append([]byte(nil), s.q[0][:n]...)
			s.q[0] = s.q[0][n:]
			if len(s.q[0]) == 0 {
				s.q = s.q[1:]
				if d.o.fifo {
					d.order = d.order[1:]
				}
			}
			arr := now.Add(d.delay)
			if d.o.loss > 0 && s.unsent >= d.o.lossQ && d.rng.Float64() < d.o.loss {
				arr = arr.Add(2 * d.delay) // its resend, a round trip on
				s.lost++
			}
			s.unsent -= n
			s.served += int64(n)
			if arr.Before(s.lastArr) {
				arr = s.lastArr // in order: behind the hole
			}
			s.lastArr = arr
			s.in = append(s.in, wcChunk{arr, piece})
			credit -= float64(n)
		}
		d.cond.Broadcast()
		d.mu.Unlock()
	}
}

func (d *ppDir) close() {
	d.mu.Lock()
	d.closed = true
	d.cond.Broadcast()
	d.mu.Unlock()
}

// waitBelowLocked waits (mu held) until less than n is unsent; false: the
// path closed.
func (s *ppSock) waitBelowLocked(n int) bool {
	for !s.d.closed && s.unsent >= n {
		s.d.cond.Wait()
	}
	return !s.d.closed
}

func (s *ppSock) write(b []byte) (int, error) {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if s.lowat > 0 && !s.waitBelowLocked(s.lowat) || d.closed {
		return 0, io.ErrClosedPipe
	}
	s.q = append(s.q, append([]byte(nil), b...))
	s.unsent += len(b)
	s.maxNS = max(s.maxNS, s.unsent)
	if d.o.fifo {
		d.order = append(d.order, s)
	}
	return len(b), nil
}

func (s *ppSock) read(b []byte) (int, error) {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	for {
		if d.closed {
			return 0, io.EOF
		}
		if len(s.in) == 0 {
			d.cond.Wait()
			continue
		}
		c := &s.in[0]
		if w := time.Until(c.at); w > 0 {
			d.mu.Unlock()
			time.Sleep(w)
			d.mu.Lock()
			continue
		}
		n := copy(b, c.b)
		if c.b = c.b[n:]; len(c.b) == 0 {
			s.in = s.in[1:]
		}
		return n, nil
	}
}

// ppEnd is one end of one of the link's connections: it reads what the
// other end's socket sent, writes through its own (its hostSys too).
type ppEnd struct{ in, out *ppSock }

func (e *ppEnd) Read(b []byte) (int, error)       { return e.in.read(b) }
func (e *ppEnd) Write(b []byte) (int, error)      { return e.out.write(b) }
func (e *ppEnd) Close() error                     { e.in.d.close(); e.out.d.close(); return nil }
func (e *ppEnd) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (e *ppEnd) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (e *ppEnd) SetDeadline(time.Time) error      { return nil }
func (e *ppEnd) SetReadDeadline(time.Time) error  { return nil }
func (e *ppEnd) SetWriteDeadline(time.Time) error { return nil }

func (e *ppEnd) setLowat(n int) error {
	e.out.d.mu.Lock()
	e.out.lowat = n
	e.out.d.cond.Broadcast()
	e.out.d.mu.Unlock()
	return nil
}

func (e *ppEnd) notsent() (int, error) {
	e.out.d.mu.Lock()
	defer e.out.d.mu.Unlock()
	return e.out.unsent, nil
}

// waitBelow: until less than n is unsent, or the socket polls writable
// (under half its mark unsent, Linux 5.10 on).
func (e *ppEnd) waitBelow(n int) error {
	d := e.out.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if !e.out.waitBelowLocked(max(n, e.out.lowat/2)) {
		return io.ErrClosedPipe
	}
	return nil
}

var _ hostSys = (*ppEnd)(nil)

// ppStats is what a direction's sockets carried: served bytes and lost
// packets, and the most unsent seen at a write.
func (d *ppDir) stats() (served []int64, lost []int, most []int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, s := range d.socks {
		served, lost, most = append(served, s.served), append(lost, s.lost), append(most, s.maxNS)
	}
	return
}

// newAngleLinkPipe is newAngleLinkCaps over a shared-bottleneck pipe: a pair
// (pair: its QUIET and BULK, each way, share one bottleneck) or a link of
// one connection, both ends built as the links are (newSessionPairCaps).
func newAngleLinkPipe(t *testing.T, rate float64, delay time.Duration, caps uint32, pair bool, o ppOpts) *angleLink {
	t.Helper()
	n := 1
	if pair {
		n = 2
	}
	down, up := newPPDir(rate, delay, n, o), newPPDir(rate, delay, n, ppOpts{fifo: o.fifo, loss: o.loss, lossQ: o.lossQ, seed: o.seed + 7})
	l := &angleLink{t: t, pdown: down, pup: up}
	edgeQ, exitQ := &ppEnd{in: down.socks[0], out: up.socks[0]}, &ppEnd{in: up.socks[0], out: down.socks[0]}
	var edgeB, exitB net.Conn
	if pair {
		edgeB, exitB = &ppEnd{in: down.socks[1], out: up.socks[1]}, &ppEnd{in: up.socks[1], out: down.socks[1]}
	}
	var err error
	l.edge, _, err = newSessionPairCaps(edgeQ, edgeB, false, nil, &linkMeter{statsPoll: make(chan struct{}, 1)}, caps)
	if err != nil {
		t.Fatal(err)
	}
	l.exit, _, err = newSessionPairCaps(exitQ, exitB, true, nil, &linkMeter{}, caps)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.edge.Close(); l.exit.Close(); down.close(); up.close() })
	go l.acceptLoop()
	return l
}

// pairCaps: the caps of a link of two R1 round-2 binaries (a pair).
func pairCaps() uint32 { return tlscarrier.LocalAuthHdr().Caps | tlscarrier.CapPair }
