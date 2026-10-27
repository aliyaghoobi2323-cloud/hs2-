package smux

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math/rand"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Groups (group.go): a logical connection over a main stream that moves
// once to a stream of another session, its bytes in order both ways.

// dwire is one direction of an emulated TCP connection: in order, each
// write arriving delay (+ up to jitter) later, and nothing at all before
// stallTill (an RTO on the path: what was sent waits).
type dwire struct {
	mu        sync.Mutex
	cond      *sync.Cond
	q         []dchunk
	last      time.Time
	delay     time.Duration
	jitter    time.Duration
	stallTill time.Time
	closed    bool
	rnd       *rand.Rand
}

type dchunk struct {
	at time.Time
	b  []byte
}

func newDwire(delay, jitter time.Duration, seed int64) *dwire {
	w := &dwire{delay: delay, jitter: jitter, rnd: rand.New(rand.NewSource(seed))}
	w.cond = sync.NewCond(&w.mu)
	return w
}

func (w *dwire) write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, io.ErrClosedPipe
	}
	at := time.Now().Add(w.delay)
	if w.jitter > 0 {
		at = at.Add(time.Duration(w.rnd.Int63n(int64(w.jitter))))
	}
	if at.Before(w.last) {
		at = w.last // TCP: in order
	}
	w.last = at
	w.q = append(w.q, dchunk{at, append([]byte(nil), b...)})
	w.cond.Broadcast()
	return len(b), nil
}

func (w *dwire) read(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for {
		if w.closed {
			return 0, io.EOF
		}
		if len(w.q) == 0 {
			w.cond.Wait()
			continue
		}
		due := w.q[0].at
		if w.stallTill.After(due) {
			due = w.stallTill
		}
		if d := time.Until(due); d > 0 {
			w.mu.Unlock()
			time.Sleep(d)
			w.mu.Lock()
			continue
		}
		c := &w.q[0]
		n := copy(b, c.b)
		if c.b = c.b[n:]; len(c.b) == 0 {
			w.q = w.q[1:]
		}
		return n, nil
	}
}

// stall holds back everything on the wire for d from now.
func (w *dwire) stall(d time.Duration) {
	w.mu.Lock()
	w.stallTill = time.Now().Add(d)
	w.mu.Unlock()
}

func (w *dwire) close() {
	w.mu.Lock()
	w.closed = true
	w.cond.Broadcast()
	w.mu.Unlock()
}

type dend struct{ in, out *dwire }

func (e *dend) Read(b []byte) (int, error)  { return e.in.read(b) }
func (e *dend) Write(b []byte) (int, error) { return e.out.write(b) }
func (e *dend) Close() error                { e.in.close(); e.out.close(); return nil }

// glink is an edge's (client) and an exit's (server) session over one
// emulated connection; up and down are its two directions.
type glink struct {
	edge, exit *Session
	up, down   *dwire
}

func newGlink(t *testing.T, delay, jitter time.Duration, seed int64, cfg func() *Config) *glink {
	t.Helper()
	l := &glink{up: newDwire(delay, jitter, seed), down: newDwire(delay, jitter, seed+1)}
	var err error
	if l.edge, err = Client(&dend{in: l.down, out: l.up}, cfg()); err != nil {
		t.Fatal(err)
	}
	if l.exit, err = Server(&dend{in: l.up, out: l.down}, cfg()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.edge.Close(); l.exit.Close(); l.up.close(); l.down.close() })
	return l
}

func groupConfig() *Config {
	c := schedConfig()
	c.MaxFrameSize = 1024
	c.EarlyDone = true
	return c
}

// pattern is byte i of a direction's stream: the reader checks each byte.
func pattern(dir byte, i int64) byte { return byte(i*7+i>>8) ^ dir }

// pump writes total bytes of dir's pattern to w in chunks of random size
// (at most 8 KiB); onWrote (may be nil) sees the running count.
func pump(w io.Writer, dir byte, total int64, rnd *rand.Rand, onWrote func(int64)) error {
	var off int64
	buf := make([]byte, 8<<10)
	for off < total {
		n := int64(1 + rnd.Intn(len(buf)))
		if n > total-off {
			n = total - off
		}
		for i := int64(0); i < n; i++ {
			buf[i] = pattern(dir, off+i)
		}
		k, err := w.Write(buf[:n])
		off += int64(k)
		if err != nil {
			return err
		}
		if onWrote != nil {
			onWrote(off)
		}
	}
	return nil
}

// drain reads r to its end and checks every byte against dir's pattern; got
// counts what it read so far. The first error (a byte out of order
// included), nil at io.EOF.
func drain(r io.Reader, dir byte, got *atomic.Int64) error {
	var off int64
	buf := make([]byte, 3000)
	for {
		n, err := r.Read(buf)
		for i := 0; i < n; i++ {
			if buf[i] != pattern(dir, off+int64(i)) {
				return errors.New("a byte out of order")
			}
		}
		off += int64(n)
		got.Store(off)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// exitGroups serves the exit's side of two links: each main stream it
// accepts becomes a group, which a move stream on the other link joins.
type exitGroups struct {
	mu     sync.Mutex
	groups map[uint32]*Group
	got    chan *Group
	moves  atomic.Int32
}

func serveExit(t *testing.T, links ...*Session) *exitGroups {
	x := &exitGroups{groups: map[uint32]*Group{}, got: make(chan *Group, 16)}
	for _, s := range links {
		s := s
		go func() {
			for {
				st, err := s.AcceptStream()
				if err != nil {
					return
				}
				id, role, _ := st.GroupInfo()
				switch role {
				case RoleMain:
					g := AcceptGroup(st)
					if g == nil {
						t.Errorf("stream %d (group %d): no group", st.ID(), id)
						st.Close()
						continue
					}
					x.mu.Lock()
					x.groups[id] = g
					x.mu.Unlock()
					x.got <- g
				case RoleMove:
					x.mu.Lock()
					g := x.groups[id]
					x.mu.Unlock()
					if g == nil || g.Attach(st) != nil {
						st.Close() // no live group: refused
						continue
					}
					x.moves.Add(1)
				}
			}
		}()
	}
	return x
}

// A connection moves once from link A to link B in the middle of a
// transfer both ways; each side reads every byte in order and the end.
// Link A is slow (its tail arrives after B's first frames: the reorder at
// the switch) and, in the second run, stalls for 300 ms just as the move
// happens (an RTO on the old link).
func TestGroupMoveKeepsOrder(t *testing.T) {
	for _, tc := range []struct {
		name         string
		delayA, jitA time.Duration
		delayB       time.Duration
		stallA       time.Duration
	}{
		{"old link slower", 30 * time.Millisecond, 10 * time.Millisecond, 2 * time.Millisecond, 0},
		{"old link stalls at the switch", 10 * time.Millisecond, 5 * time.Millisecond, 10 * time.Millisecond, 300 * time.Millisecond},
		{"new link slower", 2 * time.Millisecond, 0, 40 * time.Millisecond, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newGlink(t, tc.delayA, tc.jitA, 1, groupConfig)
			b := newGlink(t, tc.delayB, 0, 3, groupConfig)
			x := serveExit(t, a.exit, b.exit)
			g, err := a.edge.OpenGroup(0xABCD, 0, 77)
			if err != nil {
				t.Fatal(err)
			}
			const total = 3 << 20
			var xg *Group
			select {
			case xg = <-x.got:
			case <-time.After(5 * time.Second):
				t.Fatal("the exit never accepted the group")
			}
			moved := make(chan struct{})
			var once sync.Once
			pumps := make(chan error, 2)
			upEnd, dnEnd := make(chan error, 1), make(chan error, 1)
			var upN, dnN atomic.Int64
			go func() { // the edge's upload; it moves at 1 MiB
				pumps <- pump(g, 'u', total, rand.New(rand.NewSource(6)), func(off int64) {
					if off >= 1<<20 {
						once.Do(func() {
							if tc.stallA > 0 {
								a.up.stall(tc.stallA)
								a.down.stall(tc.stallA)
							}
							if _, err := g.Move(b.edge, 0xABCD); err != nil {
								t.Errorf("move: %v", err)
							}
							if _, err := g.Move(b.edge, 0xABCD); err != ErrNoMove {
								t.Errorf("a second move: %v, want ErrNoMove", err)
							}
							close(moved)
						})
					}
				})
			}()
			go func() { // the exit's download
				pumps <- pump(xg, 'd', total, rand.New(rand.NewSource(5)), nil)
			}()
			go func() { upEnd <- drain(xg, 'u', &upN) }()
			go func() { dnEnd <- drain(g, 'd', &dnN) }()
			for i := 0; i < 2; i++ {
				select {
				case err := <-pumps:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(60 * time.Second):
					t.Fatalf("stuck: read up %d, down %d of %d", upN.Load(), dnN.Load(), total)
				}
			}
			<-moved
			if g.Moved() == nil || x.moves.Load() != 1 {
				t.Fatalf("moved %v, exit attached %d", g.Moved() != nil, x.moves.Load())
			}
			// both sides read all; the edge closes, the exit reads the end
			// and closes, the edge's own reader stops at its close
			waitFor(t, "every byte read", func() bool { return upN.Load() == total && dnN.Load() == total })
			g.Close()
			select {
			case err := <-upEnd:
				if err != nil {
					t.Fatalf("exit's reader: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the exit never read the end")
			}
			xg.Close()
			if err := <-dnEnd; err != nil && err != io.ErrClosedPipe {
				t.Fatalf("edge's reader: %v", err)
			}
			// both main streams are let go
			waitFor(t, "main streams closed", func() bool { return a.edge.NumStreams() == 0 && a.exit.NumStreams() == 0 })
		})
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: never", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// While the move stream's frames wait for the old link's tail, they wait in
// its own buffer, within its window: what the exit holds for the move
// stream never passes the window it announced.
func TestGroupReorderWithinWindow(t *testing.T) {
	a := newGlink(t, 5*time.Millisecond, 0, 11, groupConfig)
	b := newGlink(t, time.Millisecond, 0, 13, groupConfig)
	x := serveExit(t, a.exit, b.exit)
	g, err := a.edge.OpenGroup(1, 0, 5)
	if err != nil {
		t.Fatal(err)
	}
	xg := <-x.got
	// the old link holds back its tail for a second while the move stream
	// carries on
	if _, err := g.Write(bytes.Repeat([]byte{1}, 50<<10)); err != nil {
		t.Fatal(err)
	}
	a.up.stall(time.Second)
	if _, err := g.Write(bytes.Repeat([]byte{2}, 30<<10)); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Move(b.edge, 1); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := g.Write(bytes.Repeat([]byte{3}, 2<<20))
		done <- err
	}()
	var most int
	var s1 *Stream
	deadline := time.Now().Add(900 * time.Millisecond)
	for time.Now().Before(deadline) {
		if s1 = xg.Moved(); s1 != nil {
			s1.bufferLock.Lock()
			if s1.unread > most {
				most = s1.unread
			}
			s1.bufferLock.Unlock()
		}
		time.Sleep(time.Millisecond)
	}
	if s1 == nil {
		t.Fatal("the move stream never arrived")
	}
	t.Logf("move stream held %d B unread while the old link stalled (its window %d B)", most, initialPeerWindow)
	if most == 0 || most > initialPeerWindow {
		t.Fatalf("move stream held %d B, want > 0 and <= its window %d", most, initialPeerWindow)
	}
	// nothing of the move stream was handed on before the old link's tail
	got := make([]byte, 80<<10)
	if _, err := io.ReadFull(xg, got); err != nil {
		t.Fatal(err)
	}
	for i, c := range got {
		want := byte(1)
		if i >= 50<<10 {
			want = 2
		}
		if c != want {
			t.Fatalf("byte %d is %d, want %d", i, c, want)
		}
	}
	go io.Copy(io.Discard, xg)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// The attacher refuses nothing of a live group, and a move stream of a
// group that has ended is refused: the edge reads the end.
func TestGroupMoveAfterEnd(t *testing.T) {
	a := newGlink(t, 20*time.Millisecond, 0, 21, groupConfig)
	b := newGlink(t, 60*time.Millisecond, 0, 23, groupConfig)
	x := serveExit(t, a.exit, b.exit)
	g, err := a.edge.OpenGroup(1, 0, 9)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	xg := <-x.got
	buf := make([]byte, 5)
	if _, err := io.ReadFull(xg, buf); err != nil {
		t.Fatal(err)
	}
	// the edge moves while the exit ends the connection: the exit's FIN on
	// the main stream arrives first, its move stream is refused after
	if _, err := g.Move(b.edge, 1); err != nil {
		t.Fatal(err)
	}
	x.mu.Lock()
	delete(x.groups, 9)
	x.mu.Unlock()
	xg.Close()
	rd := make(chan error, 1)
	go func() {
		_, err := g.Read(buf)
		rd <- err
	}()
	select {
	case err := <-rd:
		if err != io.EOF && err != io.ErrClosedPipe {
			t.Fatalf("read %v, want the end", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the edge never read the end")
	}
	if x.moves.Load() != 0 {
		t.Fatal("a move stream joined an ended group")
	}
}

// FuzzGroupReassembly: a group's frames arrive on its two streams in any
// interleaving (each stream's own in order, the main stream's numbered
// before the move stream's), with FINs anywhere after each stream's last
// frame, and reads of any size: the reader hands on every byte in order and
// then the end. Both ends' rules: the attacher knows the move stream's base
// from its SYN, the opener only from its frames.
func FuzzGroupReassembly(f *testing.F) {
	f.Add([]byte{3, 9, 200, 17, 1, 0, 255, 4, 4, 4, 90, 7})
	f.Add([]byte{0, 0, 0, 0})
	f.Add([]byte{40, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20})
	f.Add([]byte{255, 128, 64, 32, 16, 8, 4, 2, 1, 0, 99, 98, 97})
	f.Fuzz(func(t *testing.T, in []byte) {
		at := 0
		next := func() int {
			if at >= len(in) {
				at++
				return at * 37 % 251
			}
			v := int(in[at])
			at++
			return v
		}
		nFrames := 1 + next()%40
		split := next() % (nFrames + 1) // frames before it on the main stream
		opener := next()%2 == 0
		// a connection with frames past the split moved; one without may
		// have moved too (and sent nothing on the move stream)
		moves := split < nFrames || next()%2 == 0
		attachAt := next() % (nFrames + 2)
		sizes := make([]int, nFrames)
		total := 0
		for i := range sizes {
			sizes[i] = 1 + next()%300
			total += sizes[i]
		}
		r := newReasm(t, opener)
		// the order the frames reach their streams: two in-order queues
		// interleaved by the fuzz, FINs after each queue's last frame; the
		// main stream's FIN comes after the move stream is known (the
		// opener sends it only after the attacher's; the attacher only once
		// it moved too)
		var qa, qb []int
		for i := 0; i < nFrames; i++ {
			if i < split {
				qa = append(qa, i)
			} else {
				qb = append(qb, i)
			}
		}
		finA, finB := false, !moves
		pushed := 0
		offs := make([]int, nFrames+1)
		for i, s := range sizes {
			offs[i+1] = offs[i] + s
		}
		got := make([]byte, 0, total)
		readSome := func() {
			for {
				buf := make([]byte, 1+next()%400)
				n, err, wait := r.g.tryRead(buf)
				got = append(got, buf[:n]...)
				if wait || n == 0 || err != nil {
					return
				}
			}
		}
		for !(len(qa) == 0 && len(qb) == 0 && finA && finB) {
			if moves && pushed == attachAt {
				r.attach(uint32(split))
			}
			pushed++
			c := next() % 5
			switch {
			case c < 2 && len(qa) > 0:
				i := qa[0]
				qa = qa[1:]
				r.push(r.s0, uint32(i), offs[i], sizes[i])
			case c < 4 && len(qb) > 0:
				r.attach(uint32(split))
				i := qb[0]
				qb = qb[1:]
				r.push(r.s1, uint32(i), offs[i], sizes[i])
			case len(qa) == 0 && !finA:
				if moves {
					r.attach(uint32(split))
				}
				r.s0.fin()
				finA = true
			case len(qb) == 0 && !finB:
				r.attach(uint32(split))
				r.s1.fin()
				finB = true
			}
			readSome()
		}
		// the rest, to the end
		for i := 0; ; i++ {
			buf := make([]byte, 1+next()%400)
			n, err, wait := r.g.tryRead(buf)
			got = append(got, buf[:n]...)
			if wait {
				t.Fatalf("the reader waits after all frames and FINs arrived (got %d of %d)", len(got), total)
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("read: %v (got %d of %d)", err, len(got), total)
			}
			if i > total+10 {
				t.Fatal("no end")
			}
		}
		if len(got) != total {
			t.Fatalf("got %d of %d bytes", len(got), total)
		}
		for i, c := range got {
			if c != pattern('f', int64(i)) {
				t.Fatalf("byte %d out of order", i)
			}
		}
	})
}

// reasm is a group fed by hand, as a session's reader would feed its two
// streams (pushSeq, fin), over sessions whose peers discard what they get.
type reasm struct {
	t      *testing.T
	g      *Group
	s0, s1 *Stream
	sa, sb *Session
	opener bool
}

func discardSession(t *testing.T, client bool) *Session {
	c1, c2 := net.Pipe()
	go io.Copy(io.Discard, c2)
	var s *Session
	var err error
	if client {
		s, err = Client(c1, groupConfig())
	} else {
		s, err = Server(c1, groupConfig())
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(); c2.Close() })
	return s
}

func newReasm(t *testing.T, opener bool) *reasm {
	r := &reasm{t: t, opener: opener, sa: discardSession(t, opener), sb: discardSession(t, opener)}
	if opener {
		g, err := r.sa.OpenGroup(1, 0, 42)
		if err != nil {
			t.Fatal(err)
		}
		r.g, r.s0 = g, g.Main()
		return r
	}
	st := newStream(2, r.sa.config.MaxFrameSize, r.sa)
	st.syn = EncodeSynGroup(1, 0, RoleMain, 42, 0)
	st.seqOn = true
	r.sa.streamLock.Lock()
	r.sa.streams[2] = st
	r.sa.streamLock.Unlock()
	r.g = AcceptGroup(st)
	r.s0 = st
	return r
}

// attach brings the move stream (base: the first number on it).
func (r *reasm) attach(base uint32) {
	if r.g.Moved() != nil {
		return
	}
	if r.opener {
		// the opener's own move: its base is its own next number (nothing
		// sent here), the peer's frames tell the rest
		s1, err := r.g.Move(r.sb, 1)
		if err != nil {
			r.t.Fatal(err)
		}
		r.s1 = s1
		return
	}
	st := newStream(4, r.sb.config.MaxFrameSize, r.sb)
	st.syn = EncodeSynGroup(1, 2, RoleMove, 42, base)
	st.seqOn = true
	r.sb.streamLock.Lock()
	r.sb.streams[4] = st
	r.sb.streamLock.Unlock()
	if err := r.g.Attach(st); err != nil {
		r.t.Fatal(err)
	}
	r.s1 = st
}

// push puts frame seq (bytes [from, from+n) of the 'f' pattern) on s.
func (r *reasm) push(s *Stream, seq uint32, from, n int) {
	head := make([]byte, seqLen+n)
	binary.BigEndian.PutUint32(head, seq)
	for i := 0; i < n; i++ {
		head[seqLen+i] = pattern('f', int64(from+i))
	}
	atomic.AddInt32(&s.sess.bucket, -int32(n))
	s.pushSeq(head, seq)
}
