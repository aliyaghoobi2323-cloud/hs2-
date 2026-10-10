package smux

import (
	"bytes"
	"encoding/binary"
	"io"
	"math/rand"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func adaptiveConfig() *Config {
	config := DefaultConfig()
	config.Version = 2
	config.KeepAliveDisabled = true
	config.MaxFrameSize = 4096
	config.MaxReceiveBuffer = 4 << 20
	config.MaxStreamBuffer = 1 << 20
	config.MinStreamBuffer = 16 << 10
	config.StreamLagTarget = 32 << 10
	return config
}

func TestAdaptiveVerifyConfig(t *testing.T) {
	cases := []struct {
		name string
		edit func(c *Config)
		ok   bool
	}{
		{"off", func(c *Config) { c.MinStreamBuffer, c.StreamLagTarget = 0, 0 }, true},
		{"on", func(c *Config) {}, true},
		{"min equals max", func(c *Config) { c.MinStreamBuffer = c.MaxStreamBuffer }, true},
		{"min equals two frames", func(c *Config) { c.MinStreamBuffer = 2 * c.MaxFrameSize }, true},
		{"min under two frames", func(c *Config) { c.MinStreamBuffer = 2*c.MaxFrameSize - 1 }, false},
		{"min over max", func(c *Config) { c.MinStreamBuffer = c.MaxStreamBuffer + 1 }, false},
		{"no target", func(c *Config) { c.StreamLagTarget = 0 }, false},
		{"target without min", func(c *Config) { c.MinStreamBuffer = 0 }, false},
		{"negative min", func(c *Config) { c.MinStreamBuffer = -1 }, false},
		{"negative target", func(c *Config) { c.StreamLagTarget = -1 }, false},
		{"version 1", func(c *Config) { c.Version = 1 }, false},
	}
	for _, tc := range cases {
		c := adaptiveConfig()
		tc.edit(c)
		if err := VerifyConfig(c); (err == nil) != tc.ok {
			t.Errorf("%s: VerifyConfig = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}

// rawPeer plays the sending side of a session's streams by hand over a
// net.Pipe, so a test sees every window update the session under test sends
// and decides exactly how far ahead of the reader the data runs. Its window
// bookkeeping (room, sendInWindow) is for its first stream and honours the
// announced window like writeV2 does.
type rawPeer struct {
	t    *testing.T
	conn net.Conn
	ver  byte
	sid  uint32      // first stream
	syn  chan uint32 // stream ids as their SYNs arrive

	mu       sync.Mutex
	sent     uint32 // bytes of the first stream sent so far
	consumed uint32 // from its last UPD
	window   uint32 // from its last UPD
	upds     [][2]uint32
	updated  chan struct{}
}

// newRawPeer returns a client session with config, its first stream, and the
// raw peer feeding it.
func newRawPeer(t *testing.T, config *Config) (*Session, *Stream, *rawPeer) {
	t.Helper()
	c1, c2 := net.Pipe()
	session, err := Client(c1, config)
	if err != nil {
		t.Fatal(err)
	}
	p := &rawPeer{t: t, conn: c2, ver: byte(config.Version), window: initialPeerWindow,
		syn: make(chan uint32, 16), updated: make(chan struct{}, 1)}
	go p.readLoop()
	t.Cleanup(func() { session.Close(); c2.Close() })
	stream, sid := p.open(session)
	p.mu.Lock()
	p.sid = sid
	p.mu.Unlock()
	return session, stream, p
}

// open opens one more stream on session and returns it with its id.
func (p *rawPeer) open(session *Session) (*Stream, uint32) {
	p.t.Helper()
	stream, err := session.OpenStream()
	if err != nil {
		p.t.Fatal(err)
	}
	select {
	case sid := <-p.syn:
		return stream, sid
	case <-time.After(5 * time.Second):
		p.t.Fatal("no SYN")
	}
	return nil, 0
}

func (p *rawPeer) readLoop() {
	var hdr rawHeader
	for {
		if _, err := io.ReadFull(p.conn, hdr[:]); err != nil {
			return
		}
		body := make([]byte, hdr.Length())
		if _, err := io.ReadFull(p.conn, body); err != nil {
			return
		}
		switch hdr.Cmd() {
		case cmdSYN:
			p.syn <- hdr.StreamID()
		case cmdUPD:
			var upd updHeader
			copy(upd[:], body)
			p.mu.Lock()
			if hdr.StreamID() == p.sid {
				p.consumed, p.window = upd.Consumed(), upd.Window()
				p.upds = append(p.upds, [2]uint32{upd.Consumed(), upd.Window()})
			}
			p.mu.Unlock()
			select {
			case p.updated <- struct{}{}:
			default:
			}
		}
	}
}

// room is how many more bytes the window lets the peer send.
func (p *rawPeer) room() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	inflight := int32(p.sent - p.consumed)
	return int(int32(p.window) - inflight)
}

// send pushes n bytes on the first stream in frames of at most frameSize.
func (p *rawPeer) send(n, frameSize int) { p.sendOn(p.sid, n, frameSize) }

// sendOn pushes n bytes on stream sid in frames of at most frameSize.
func (p *rawPeer) sendOn(sid uint32, n, frameSize int) {
	for n > 0 {
		sz := n
		if sz > frameSize {
			sz = frameSize
		}
		frame := make([]byte, headerSize+sz)
		frame[0] = p.ver
		frame[1] = cmdPSH
		binary.LittleEndian.PutUint16(frame[2:], uint16(sz))
		binary.LittleEndian.PutUint32(frame[4:], sid)
		if _, err := p.conn.Write(frame); err != nil {
			p.t.Fatal(err)
		}
		if sid == p.sid {
			p.mu.Lock()
			p.sent += uint32(sz)
			p.mu.Unlock()
		}
		n -= sz
	}
}

// fill sends all the window allows.
func (p *rawPeer) fill(frameSize int) {
	if r := p.room(); r > 0 {
		p.send(r, frameSize)
	}
}

func (p *rawPeer) lastWindow() uint32 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.window
}

func (p *rawPeer) updates() [][2]uint32 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([][2]uint32(nil), p.upds...)
}

func (p *rawPeer) state() (sent, consumed, window uint32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sent, p.consumed, p.window
}

// sendInWindow sends n bytes, waiting for window updates as writeV2 would;
// it fails the test if the window stays shut.
func (p *rawPeer) sendInWindow(n, frameSize int) {
	for n > 0 {
		r := p.room()
		if r <= 0 {
			select {
			case <-p.updated:
				continue
			case <-time.After(2 * time.Second):
				sent, consumed, window := p.state()
				p.t.Fatalf("window shut for good: sent %d consumed %d window %d", sent, consumed, window)
			}
		}
		if r > n {
			r = n
		}
		p.send(r, frameSize)
		n -= r
	}
}

// sync waits until the peer has seen the latest window update s sent: the
// update travels through the session's writer and the pipe on goroutines of
// their own, and a fill that runs before it arrives sends less than the window
// allows, which reads as a reader running dry.
func (p *rawPeer) sync(s *Stream) {
	p.t.Helper()
	s.bufferLock.Lock()
	want, read := s.numRead-s.incr, s.numRead != 0
	s.bufferLock.Unlock()
	if !read {
		return
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		p.mu.Lock()
		n := len(p.upds)
		seen := n > 0 && p.upds[n-1][0] == want
		p.mu.Unlock()
		if seen {
			return
		}
		if time.Now().After(deadline) {
			p.t.Fatalf("update for consumed %d never reached the peer", want)
		}
		time.Sleep(100 * time.Microsecond)
	}
}

// settle waits until s holds every byte the peer sent it and has not read
// (the session pushes a frame just after the pipe hands it over).
func (p *rawPeer) settle(s *Stream) {
	p.t.Helper()
	sent, _, _ := p.state()
	s.bufferLock.Lock()
	read := s.numRead
	s.bufferLock.Unlock()
	waitUnread(p.t, s, int(sent-read))
}

// step is one round of a reader that takes n bytes a round from a peer that
// refills the whole window: the peer acts on the latest update, then the
// reader reads once everything sent has arrived.
func (p *rawPeer) step(s *Stream, frameSize, n int) {
	p.t.Helper()
	p.sync(s)
	p.fill(frameSize)
	p.settle(s)
	readN(p.t, s, n, n)
}

// readN reads exactly n bytes from the stream in reads of at most chunk.
func readN(t *testing.T, s *Stream, n, chunk int) {
	t.Helper()
	buf := make([]byte, chunk)
	for n > 0 {
		k := chunk
		if k > n {
			k = n
		}
		s.SetReadDeadline(time.Now().Add(5 * time.Second))
		m, err := s.Read(buf[:k])
		if err != nil {
			t.Fatalf("read: %v (%d bytes still due)", err, n)
		}
		n -= m
	}
}

func unreadOf(s *Stream) int {
	s.bufferLock.Lock()
	defer s.bufferLock.Unlock()
	return s.unread
}

// waitUnread waits until the stream holds want unread bytes.
func waitUnread(t *testing.T, s *Stream, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := unreadOf(s)
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("unread = %d, want %d", got, want)
		}
		time.Sleep(time.Millisecond)
	}
}

// An adaptive stream starts at the window the peer assumes anyway; a reader
// that leaves the peer's whole window unread (the peer refills it at once)
// has its window cut within a few updates to MinStreamBuffer, and never
// holds more than the window it announced.
func TestAdaptiveWindowShrinksForSlowReader(t *testing.T) {
	config := adaptiveConfig()
	_, stream, p := newRawPeer(t, config)
	maxUnread := 0
	for i := 0; i < 200; i++ {
		p.step(stream, config.MaxFrameSize, 4096)
		if i > 100 {
			if u := unreadOf(stream); u > maxUnread {
				maxUnread = u
			}
		}
	}
	upds := p.updates()
	if upds[0][1] != initialPeerWindow {
		t.Fatalf("first update window %d, want %d", upds[0][1], initialPeerWindow)
	}
	settled := uint32(2*config.StreamLagTarget + 2*config.MaxFrameSize)
	if len(upds) < 6 || upds[5][1] > settled {
		t.Fatalf("window not at %d or less by the sixth update: %v", settled, upds)
	}
	for i, u := range upds {
		if u[1] < uint32(config.MinStreamBuffer) || u[1] > uint32(config.MaxStreamBuffer) {
			t.Fatalf("update %d window %d out of [%d, %d]", i, u[1], config.MinStreamBuffer, config.MaxStreamBuffer)
		}
	}
	w := p.lastWindow()
	if w > settled {
		t.Fatalf("slow reader: window %d, want %d or less (updates %v)", w, settled, upds)
	}
	if maxUnread > int(w) {
		t.Fatalf("held %d unread with a %d window", maxUnread, w)
	}
}

// A reader that keeps up (nothing left unread) gets the full window back,
// doubling per update.
func TestAdaptiveWindowRegrowsForFastReader(t *testing.T) {
	config := adaptiveConfig()
	_, stream, p := newRawPeer(t, config)
	for i := 0; i < 200; i++ { // slow phase, as above
		p.step(stream, config.MaxFrameSize, 4096)
	}
	p.sync(stream)
	if w := p.lastWindow(); w >= uint32(config.MaxStreamBuffer) {
		t.Fatalf("slow phase left window %d", w)
	}
	sent, _, _ := p.state()
	readN(t, stream, int(sent-stream.numRead), 4096) // drain
	n0 := len(p.updates())
	for i := 0; i < 1000 && p.lastWindow() < uint32(config.MaxStreamBuffer); i++ {
		p.sendInWindow(4096, config.MaxFrameSize) // data arrives no faster than read
		p.settle(stream)
		readN(t, stream, 4096, 4096)
		p.sync(stream)
	}
	var steps []uint32
	for _, u := range p.updates()[n0:] {
		steps = append(steps, u[1]>>10)
	}
	if w := p.lastWindow(); w != uint32(config.MaxStreamBuffer) {
		t.Fatalf("fast reader: window %d, want %d (KiB steps %v)", w, config.MaxStreamBuffer, steps)
	}
	for i := 1; i < len(steps); i++ {
		if steps[i] != 2*steps[i-1] && steps[i] != uint32(config.MaxStreamBuffer>>10) {
			t.Fatalf("window did not double per update: KiB steps %v", steps)
		}
	}
}

// A reader whose updates reach the peer late (an RTT) needs a window that
// covers what is in flight. The window settles where the reader never runs
// dry yet keeps no more than about StreamLagTarget unread -- the case a
// window sized on the backlog at update time gets wrong, as data arrives in
// bursts.
func TestAdaptiveWindowCoversPeerDelay(t *testing.T) {
	config := adaptiveConfig()
	_, stream, p := newRawPeer(t, config)
	const (
		delay   = 10      // ticks before the peer acts on an update
		perTick = 8 << 10 // the reader takes this much a tick
		path    = 64 << 10
		ticks   = 1200
	)
	var (
		seen               []int // tick each update was seen at
		consumed, window   = uint32(0), uint32(initialPeerWindow)
		sent, read         uint32
		starved, maxUnread int
		windows            []uint32
	)
	for k := 0; k < ticks; k++ {
		upds := p.updates()
		for len(seen) < len(upds) {
			seen = append(seen, k)
		}
		for i := range upds {
			if seen[i] <= k-delay {
				consumed, window = upds[i][0], upds[i][1]
			}
		}
		room := int(int32(window) - int32(sent-consumed))
		if room > path {
			room = path
		}
		if room > 0 {
			p.send(room, config.MaxFrameSize)
			sent += uint32(room)
		}
		waitUnread(t, stream, int(sent-read))
		n := int(sent - read)
		if n > perTick {
			n = perTick
		}
		if k >= ticks/2 {
			if n < perTick {
				starved++
			}
			if u := int(sent - read); u > maxUnread {
				maxUnread = u
			}
			windows = append(windows, window)
		}
		if n > 0 {
			readN(t, stream, n, 4096)
			read += uint32(n)
			p.sync(stream) // an update sent this tick is seen this tick
		}
	}
	if starved > 0 {
		t.Fatalf("reader ran dry in %d of the last %d ticks (windows %v)", starved, ticks/2, windows[len(windows)-5:])
	}
	// in flight is about delay*perTick = 80 KiB; the window should cover
	// that plus about twice the target, far from MaxStreamBuffer
	want := 2 * (delay*perTick + config.StreamLagTarget)
	if w := int(windows[len(windows)-1]); w > want+perTick || w < delay*perTick {
		t.Fatalf("window %d, want about %d", w, want)
	}
	if maxUnread > 4*config.StreamLagTarget {
		t.Fatalf("held up to %d unread, target %d", maxUnread, config.StreamLagTarget)
	}
	t.Logf("window %d, most unread %d", windows[len(windows)-1], maxUnread)
}

// While the session's reader waits for buffer space (other streams hold it)
// a stream that runs dry is starved by them, not by its window: its window
// must not grow then. Once the session flows again it grows as usual.
func TestAdaptiveWindowHoldsWhileParked(t *testing.T) {
	config := adaptiveConfig()
	config.MaxReceiveBuffer = config.MaxStreamBuffer
	session, stream, p := newRawPeer(t, config)
	other, otherSid := p.open(session)
	// The pipe blocks while the session is parked, so frames go out from
	// a goroutine, in order.
	type job struct {
		sid uint32
		n   int
	}
	jobs := make(chan job, 1024)
	sent := make(chan struct{})
	go func() {
		for j := range jobs {
			p.sendOn(j.sid, j.n, config.MaxFrameSize)
		}
		close(sent)
	}()
	// the other stream takes the whole session buffer and keeps it full
	jobs <- job{otherSid, config.MaxReceiveBuffer}
	waitUnread(t, other, config.MaxReceiveBuffer)
	parks0 := atomic.LoadUint32(&session.parks)
	for i := 0; i < 200; i++ {
		jobs <- job{p.sid, 4096}
		jobs <- job{otherSid, 4096}
		readN(t, other, 4096, 4096) // lets one frame in
		readN(t, stream, 4096, 4096)
	}
	close(jobs)
	<-sent
	if atomic.LoadUint32(&session.parks) == parks0 {
		t.Fatal("the session never parked")
	}
	upds := p.updates()
	if len(upds) < 4 {
		t.Fatalf("only %d updates", len(upds))
	}
	for i, u := range upds {
		if u[1] != initialPeerWindow {
			t.Fatalf("update %d grew the window to %d while the session was parked (%v)", i, u[1], upds)
		}
	}
	// the other stream drains and stops: no more parking, the window grows
	readN(t, other, unreadOf(other), 4096)
	for i := 0; i < 1000 && p.lastWindow() < uint32(config.MaxStreamBuffer); i++ {
		p.sendInWindow(4096, config.MaxFrameSize)
		readN(t, stream, 4096, 4096)
	}
	if w := p.lastWindow(); w != uint32(config.MaxStreamBuffer) {
		t.Fatalf("window %d after the session flows again, want %d", w, config.MaxStreamBuffer)
	}
}

// With the adaptive window off every update announces MaxStreamBuffer and is
// sent each MaxStreamBuffer/2 read, as upstream.
func TestAdaptiveOffAnnouncesMax(t *testing.T) {
	config := adaptiveConfig()
	config.MaxStreamBuffer = 256 << 10
	config.MinStreamBuffer, config.StreamLagTarget = 0, 0
	_, stream, p := newRawPeer(t, config)
	for i := 0; i < 400; i++ {
		p.step(stream, config.MaxFrameSize, 4096)
	}
	upds := p.updates()
	if len(upds) < 5 {
		t.Fatalf("only %d updates", len(upds))
	}
	half := uint32(config.MaxStreamBuffer / 2)
	for i, u := range upds {
		if u[1] != uint32(config.MaxStreamBuffer) {
			t.Fatalf("update %d window %d, want %d", i, u[1], config.MaxStreamBuffer)
		}
		if i > 0 && u[0]-upds[i-1][0] != half {
			t.Fatalf("update %d after %d bytes, want %d", i, u[0]-upds[i-1][0], half)
		}
	}
}

// unread follows the bytes held in buffers through Read, WriteTo and close.
func TestAdaptiveUnreadAccounting(t *testing.T) {
	for _, version := range []int{1, 2} {
		config := adaptiveConfig()
		if version == 1 {
			config.Version = 1
			config.MinStreamBuffer, config.StreamLagTarget = 0, 0
		}
		session, stream, p := newRawPeer(t, config)
		p.send(40000, config.MaxFrameSize)
		waitUnread(t, stream, 40000)
		if b := int(atomic.LoadInt32(&session.bucket)); b != config.MaxReceiveBuffer-40000 {
			t.Fatalf("v%d: bucket %d, want %d", version, b, config.MaxReceiveBuffer-40000)
		}
		readN(t, stream, 1000, 1000) // part of a frame
		waitUnread(t, stream, 39000)
		readN(t, stream, 9000, 3000)
		waitUnread(t, stream, 30000)
		var sink bytes.Buffer
		go stream.WriteTo(&sink)
		waitUnread(t, stream, 0)
		p.send(5000, config.MaxFrameSize)
		waitUnread(t, stream, 0) // WriteTo takes it at once
		stream.Close()
		waitUnread(t, stream, 0)
	}
	// close with data still buffered: recycleTokens clears unread
	config := adaptiveConfig()
	_, stream, p := newRawPeer(t, config)
	p.send(20000, config.MaxFrameSize)
	waitUnread(t, stream, 20000)
	stream.Close()
	waitUnread(t, stream, 0)
}

// The update that brings numRead across 2^32 to exactly 0 must still be sent:
// the window may have grown in it, and the peer would wait for the next one
// (upstream skips an update whose consumed count is 0).
func TestAdaptiveWindowWrap(t *testing.T) {
	config := adaptiveConfig()
	_, stream, p := newRawPeer(t, config)
	min := uint32(config.MinStreamBuffer)
	base := uint32(0) - min/2 // the first update falls on numRead == 0
	stream.bufferLock.Lock()
	stream.numRead = base
	stream.rcvWin = min
	stream.bufferLock.Unlock()
	p.mu.Lock()
	p.sent, p.consumed, p.window = base, base, min
	p.mu.Unlock()
	for total := 0; total < 4<<20; total += 4096 {
		p.sendInWindow(4096, config.MaxFrameSize)
		readN(t, stream, 4096, 4096)
	}
	if upds := p.updates(); upds[0] != [2]uint32{0, 2 * min} {
		t.Fatalf("first update %v, want consumed 0 (the wrap), window %d", upds[0], 2*min)
	}
	if w := p.lastWindow(); w != uint32(config.MaxStreamBuffer) {
		t.Fatalf("window %d after the wrap, want %d", w, config.MaxStreamBuffer)
	}
}

// No deadlock and no corruption between two adaptive sessions with the
// smallest allowed window and a small lag target, under readers that change
// pace at random, through Read and through WriteTo; the windows must actually
// move down to MinStreamBuffer and back up.
func TestAdaptiveNoDeadlockSmallMin(t *testing.T) {
	config := adaptiveConfig()
	config.MinStreamBuffer = 2 * config.MaxFrameSize
	config.StreamLagTarget = config.MaxFrameSize
	c1, c2 := net.Pipe()
	client, err := Client(c1, config)
	if err != nil {
		t.Fatal(err)
	}
	server, err := Server(c2, config)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	defer server.Close()

	const streams = 4
	const size = 8 << 20
	pattern := func(off int) byte { return byte(off*7 + off>>13) }
	var wg sync.WaitGroup
	errs := make(chan error, 2*streams)
	var readers []*Stream
	for i := 0; i < streams; i++ {
		cs, err := client.OpenStream()
		if err != nil {
			t.Fatal(err)
		}
		readers = append(readers, cs)
		ss, err := server.AcceptStream()
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(2)
		go func() { // sender
			defer wg.Done()
			buf := make([]byte, 50000)
			for off := 0; off < size; {
				n := len(buf)
				if n > size-off {
					n = size - off
				}
				for k := 0; k < n; k++ {
					buf[k] = pattern(off + k)
				}
				if _, err := ss.Write(buf[:n]); err != nil {
					errs <- err
					return
				}
				off += n
			}
		}()
		viaWriteTo := i%2 == 1
		go func(seed int64) { // reader
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed))
			cs.SetReadDeadline(time.Now().Add(60 * time.Second))
			chk := &patternChecker{pattern: pattern, rng: rng}
			if viaWriteTo {
				w := &stopAfter{w: chk, n: size}
				_, err := cs.WriteTo(w)
				if err != errEnough {
					errs <- err
					return
				}
			} else {
				buf := make([]byte, 64<<10)
				for chk.off < size {
					k := 1 + rng.Intn(len(buf))
					n, err := cs.Read(buf[:k])
					if _, werr := chk.Write(buf[:n]); werr != nil {
						errs <- werr
						return
					}
					if err != nil {
						errs <- err
						return
					}
				}
			}
			if chk.err != nil {
				errs <- chk.err
			}
		}(int64(i))
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	lo := uint32(config.MaxStreamBuffer)
	prev := make([]uint32, streams)
	ups, downs := 0, 0
	timeout := time.After(90 * time.Second)
wait:
	for {
		select {
		case <-done:
			break wait
		case <-timeout:
			t.Fatal("deadlock: transfers did not finish")
		case <-time.After(100 * time.Microsecond):
		}
		for i, cs := range readers {
			cs.bufferLock.Lock()
			w := cs.rcvWin
			cs.bufferLock.Unlock()
			if w < lo {
				lo = w
			}
			if prev[i] != 0 && w > prev[i] {
				ups++
			} else if prev[i] != 0 && w < prev[i] {
				downs++
			}
			prev[i] = w
		}
	}
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if lo > uint32(config.MinStreamBuffer+config.MaxFrameSize) || ups == 0 || downs == 0 {
		t.Fatalf("windows went down to %d (want %d), moved up %d and down %d times", lo, config.MinStreamBuffer, ups, downs)
	}
	t.Logf("windows went down to %d, moved up %d and down %d times", lo, ups, downs)
}

// patternChecker verifies the bytes it is given and now and then pauses, so
// the reader's pace changes.
type patternChecker struct {
	pattern func(int) byte
	rng     *rand.Rand
	off     int
	err     error
}

func (c *patternChecker) Write(b []byte) (int, error) {
	for k := range b {
		if b[k] != c.pattern(c.off+k) && c.err == nil {
			c.err = io.ErrUnexpectedEOF
		}
	}
	c.off += len(b)
	switch r := c.rng.Intn(64); {
	case r == 0: // a long pause: the sender fills the window, which shrinks
		time.Sleep(time.Duration(10+c.rng.Intn(20)) * time.Millisecond)
	case r < 4:
		time.Sleep(time.Duration(c.rng.Intn(5)) * time.Millisecond)
	}
	return len(b), c.err
}

var errEnough = io.ErrShortWrite

// stopAfter ends a WriteTo once n bytes have been written.
type stopAfter struct {
	w io.Writer
	n int
}

func (s *stopAfter) Write(b []byte) (int, error) {
	n, err := s.w.Write(b)
	s.n -= n
	if err == nil && s.n <= 0 {
		err = errEnough
	}
	return n, err
}
