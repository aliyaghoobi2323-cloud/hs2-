package engine

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// rateConn is a link socket's send side: up to burst bytes go at once, then
// rate bytes/s (the path, or a DPI throttle on that one flow).
type rateConn struct {
	net.Conn
	rate, burst float64
	mu          sync.Mutex
	tokens      float64
	last        time.Time
}

func (c *rateConn) Write(p []byte) (int, error) {
	if c.rate > 0 {
		c.mu.Lock()
		now := time.Now()
		if c.last.IsZero() {
			c.last, c.tokens = now, c.burst
		}
		c.tokens = min(c.burst, c.tokens+c.rate*now.Sub(c.last).Seconds()) - float64(len(p))
		c.last = now
		var d time.Duration
		if c.tokens < 0 {
			d = time.Duration(-c.tokens / c.rate * float64(time.Second))
		}
		c.mu.Unlock()
		time.Sleep(d)
	}
	return c.Conn.Write(p)
}

// udpSeen records, per client id, when each datagram reached the exit.
type udpSeen struct {
	mu   sync.Mutex
	lats map[uint32][]time.Duration
}

func (r *udpSeen) add(id uint32, d time.Duration) {
	r.mu.Lock()
	r.lats[id] = append(r.lats[id], d)
	r.mu.Unlock()
}

func (r *udpSeen) get(id uint32) []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Duration(nil), r.lats[id]...)
}

// rateLink is an edge mtcp link over a loopback TCP pair whose edge side
// sends at rate (0: loopback speed), and an exit that reads every stream:
// UDP ones are decoded into seen, the rest discarded.
func rateLink(t *testing.T, ctx context.Context, rate float64, seen *udpSeen) *mtcpLink {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	acc := make(chan net.Conn, 1)
	go func() { c, _ := ln.Accept(); acc <- c }()
	cc, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	sc := <-acc
	cli, _, err := newSession(&rateConn{Conn: cc, rate: rate, burst: 32 << 10}, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv, _, err := newSession(sc, true, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	go func() { <-ctx.Done(); cli.Close(); srv.Close(); cc.Close(); sc.Close() }()
	go func() {
		for {
			st, err := srv.AcceptStream()
			if err != nil {
				return
			}
			go func() {
				var k [1]byte
				if _, err := io.ReadFull(st, k[:]); err != nil {
					return
				}
				if k[0] != kindUDP {
					io.Copy(io.Discard, st)
					return
				}
				buf := make([]byte, maxDatagram)
				for {
					p, err := readDatagram(st, buf)
					if err != nil {
						return
					}
					if len(p) >= 12 {
						seen.add(binary.BigEndian.Uint32(p), time.Duration(time.Now().UnixNano()-int64(binary.BigEndian.Uint64(p[4:]))))
					}
				}
			}()
		}
	}()
	return &mtcpLink{sess: cli}
}

// sendUDP sends n datagrams of size bytes every gap, tagged with id and the
// time sent.
func sendUDP(addr string, id uint32, size int, gap time.Duration, n int, sent *atomic.Int64) {
	c, err := net.Dial("udp", addr)
	if err != nil {
		return
	}
	defer c.Close()
	b := make([]byte, size)
	binary.BigEndian.PutUint32(b, id)
	next := time.Now()
	for i := 0; i < n; i++ {
		binary.BigEndian.PutUint64(b[4:], uint64(time.Now().UnixNano()))
		c.Write(b)
		sent.Add(1)
		next = next.Add(gap)
		time.Sleep(time.Until(next))
	}
}

// One user of a UDP port whose link is throttled (8 KB/s, with another
// user's upload on it) must not hold up the port's other users: the port's
// one read loop used to write each datagram to its flow's link itself, and
// the other user, on a healthy link, got 34% of its datagrams, 430 ms late.
func TestUDPFlowDoesNotHoldUpThePort(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	seen := &udpSeen{lats: map[uint32][]time.Duration{}}
	lm := NewLinkManager(nil, 1, 4, 8, nil)
	slow := rateLink(t, ctx, 8<<10, seen)
	fast := rateLink(t, ctx, 0, seen)
	addManaged(lm, slow)
	up, err := slow.OpenStream() // another user's upload on the throttled link
	if err != nil {
		t.Fatal(err)
	}
	up.Write([]byte{kindTCP})
	go func() {
		b := make([]byte, 32<<10)
		for ctx.Err() == nil {
			if _, err := up.Write(b); err != nil {
				return
			}
		}
	}()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	go serveUserUDP(ctx, pc, lm, 0, func(string, ...any) {})
	addr := pc.LocalAddr().String()

	var sentA, sentB atomic.Int64
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); sendUDP(addr, 1, 300, 30*time.Millisecond, 100, &sentA) }() // A: on the throttled link
	time.Sleep(300 * time.Millisecond)
	addManaged(lm, fast) // B lands on the healthy link
	time.Sleep(500 * time.Millisecond)
	wg.Add(1)
	go func() { defer wg.Done(); sendUDP(addr, 2, 200, 10*time.Millisecond, 200, &sentB) }() // B: 100/s for 2 s
	wg.Wait()
	time.Sleep(300 * time.Millisecond)
	b := seen.get(2)
	sort.Slice(b, func(i, j int) bool { return b[i] < b[j] })
	got := float64(len(b)) / float64(sentB.Load())
	if got < 0.95 {
		t.Fatalf("B, on a healthy link, got %d of %d datagrams (%.0f%%): held up by A's throttled link", len(b), sentB.Load(), 100*got)
	}
	if p99 := b[len(b)*99/100]; p99 > 150*time.Millisecond {
		t.Fatalf("B's datagrams p99 %v late: held up by A's throttled link", p99)
	}
	if e := entryOf(lm, fast); e == nil || e.users.Load() == 0 {
		t.Fatal("B was not placed on the healthy link (test setup)")
	}
}
