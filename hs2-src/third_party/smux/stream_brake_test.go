package smux

import (
	"encoding/binary"
	"io"
	"math/rand"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// waitUpdates waits until the peer has seen n window updates of its first
// stream.
func (p *rawPeer) waitUpdates(n int) [][2]uint32 {
	p.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		upds := p.updates()
		if len(upds) >= n {
			return upds
		}
		if time.Now().After(deadline) {
			p.t.Fatalf("%d window updates, want %d: %v", len(upds), n, upds)
		}
		time.Sleep(time.Millisecond)
	}
}

// rcvWinOf is the window the stream announced last (or will at its first
// update).
func rcvWinOf(s *Stream) uint32 {
	s.bufferLock.Lock()
	defer s.bufferLock.Unlock()
	return s.rcvWin
}

// keepUp has the peer send n more bytes on its first stream a frame at a
// time, each read as soon as it is in: a reader that keeps up.
func keepUp(t *testing.T, p *rawPeer, st *Stream, n int) {
	t.Helper()
	for ; n > 0; n -= 4096 {
		p.sendInWindow(4096, 4096)
		p.settle(st)
		readN(t, st, 4096, 4096)
	}
}

// The first resize of a reader that kept up with the window the peer started
// with is a jump to the full window; one that did not keep up does not jump,
// and a stream that has resized once only doubles from then on (the regrow
// test).
func TestAdaptiveFirstResizeJumps(t *testing.T) {
	config := adaptiveConfig()
	_, stream, p := newRawPeer(t, config)
	keepUp(t, p, stream, initialPeerWindow/2+4096)
	p.sync(stream)
	upds := p.updates()
	if len(upds) < 2 || upds[0][1] != initialPeerWindow || upds[1][1] != uint32(config.MaxStreamBuffer) {
		t.Fatalf("reader that keeps up: updates %v, want the window the peer assumes, then the full %d", upds, config.MaxStreamBuffer)
	}

	_, slow, q := newRawPeer(t, config)
	q.send(4096, config.MaxFrameSize)
	readN(t, slow, 4096, 4096)
	q.sync(slow)
	q.fill(config.MaxFrameSize) // the whole window at once
	q.settle(slow)
	readN(t, slow, initialPeerWindow/2, 4096) // leaves the rest unread
	q.sync(slow)
	if upds := q.updates(); len(upds) < 2 || upds[1][1] > initialPeerWindow {
		t.Fatalf("reader that did not keep up: updates %v, want no jump", upds)
	}
}

// A reader that keeps up with a trickle (a slow link, or a new connection
// still opening its congestion window) jumps only to what it reads in
// jumpSpan at that pace, not to the full window: many of them must not hold
// the session's buffer between them once the data comes faster.
func TestAdaptiveFirstResizeJumpsToPace(t *testing.T) {
	config := adaptiveConfig()
	_, stream, p := newRawPeer(t, config)
	for n := 0; n < initialPeerWindow/2+4096; n += 4096 {
		p.sendInWindow(4096, 4096) // about 800 KB/s
		p.settle(stream)
		readN(t, stream, 4096, 4096)
		time.Sleep(5 * time.Millisecond)
	}
	p.sync(stream)
	upds := p.updates()
	if len(upds) < 2 || upds[1][1] < 2*initialPeerWindow || upds[1][1] >= uint32(config.MaxStreamBuffer) {
		t.Fatalf("reader keeping up with a trickle: updates %v, want a second window from %d up to (not) %d", upds, 2*initialPeerWindow, config.MaxStreamBuffer)
	}
	t.Logf("second window %d", upds[1][1])
}

// brakeRig: a stream whose reader kept up until its window jumped to the
// full one, and a second stream on the same session holding `held` bytes
// unread.
func brakeRig(t *testing.T, held int) (*Session, *Stream, *rawPeer) {
	t.Helper()
	config := adaptiveConfig()
	session, stream, p := newRawPeer(t, config)
	keepUp(t, p, stream, initialPeerWindow/2+4096)
	p.sync(stream)
	if w := p.lastWindow(); w != uint32(config.MaxStreamBuffer) {
		t.Fatalf("rig: window %d, want the full %d", w, config.MaxStreamBuffer)
	}
	if held > 0 {
		other, sid := p.open(session)
		p.sendOn(sid, held, config.MaxFrameSize)
		waitUnread(t, other, held)
	}
	return session, stream, p
}

// A stream whose reader has stopped keeping up for brakeLag while the other
// streams hold 1/brakeShare of the session's buffer gets window updates at
// once, without reading: consumed is what it has read, and the window halves
// at each frame the peer still sends, until the peer may send little more
// than the stream holds. Reading then works as usual.
func TestAdaptiveBrakesLaggingStream(t *testing.T) {
	config := adaptiveConfig()
	session, stream, p := brakeRig(t, config.MaxReceiveBuffer/brakeShare+4096)
	n0 := len(p.updates())
	read0 := stream.numRead
	lag := config.StreamLagTarget + 8192
	p.send(lag, config.MaxFrameSize) // over the target: the lag starts
	waitUnread(t, stream, lag)
	time.Sleep(brakeLag + 10*time.Millisecond)
	p.send(4096, config.MaxFrameSize) // the frame after brakeLag brakes
	upds := p.waitUpdates(n0 + 1)
	if upds[n0] != [2]uint32{read0, uint32(config.MaxStreamBuffer / 2)} {
		t.Fatalf("braking update %v, want consumed %d, half the window (all updates %v)", upds[n0], read0, upds)
	}
	// the peer goes on as far as the window lets it, a frame at a time
	extra := 0
	for extra < config.MaxStreamBuffer {
		sent, _, _ := p.state()
		n := len(p.updates())
		if p.room() <= 0 {
			break
		}
		p.send(4096, config.MaxFrameSize)
		extra += 4096
		waitUnread(t, stream, int(sent+4096-read0)) // all it was sent but what the rig read
		deadline := time.Now().Add(20 * time.Millisecond)
		for len(p.updates()) == n && time.Now().Before(deadline) {
			time.Sleep(100 * time.Microsecond) // a brake, if one is due
		}
	}
	if r := p.room(); r > 0 {
		t.Fatalf("peer may still send %d bytes after %d more", r, extra)
	}
	_, consumed, window := p.state()
	if consumed != read0 || int(window) > unreadOf(stream)+2*config.StreamLagTarget {
		t.Fatalf("last update consumed %d window %d, holding %d (sent %d more)", consumed, window, unreadOf(stream), extra)
	}
	if n := atomic.LoadUint32(&session.brakes); n < 2 {
		t.Fatalf("%d brakes, want the window halved more than once", n)
	}
	t.Logf("%d brakes, peer stopped after %d more bytes, window %d", atomic.LoadUint32(&session.brakes), extra, window)
	// the reader catches up through the usual updates
	readN(t, stream, unreadOf(stream), 4096)
	for i := 0; i < 64; i++ {
		p.sendInWindow(16384, config.MaxFrameSize)
		readN(t, stream, 16384, 4096)
	}
}

// No brake while the other streams hold under 1/brakeShare of the session's
// buffer (one stream's own backlog is its window's business), none where
// the peer may send little more than the stream holds, none for a reader
// that keeps a pace taking its backlog down within brakeDrain, and none for
// a burst the reader takes at once.
func TestAdaptiveNoBrake(t *testing.T) {
	config := adaptiveConfig()
	t.Run("no pressure", func(t *testing.T) {
		session, stream, p := brakeRig(t, config.MaxReceiveBuffer/brakeShare-3*config.MaxFrameSize)
		n0 := len(p.updates())
		p.send(4*config.StreamLagTarget, config.MaxFrameSize)
		waitUnread(t, stream, 4*config.StreamLagTarget)
		time.Sleep(brakeLag + 10*time.Millisecond)
		p.send(4096, config.MaxFrameSize)
		waitUnread(t, stream, 4*config.StreamLagTarget+4096)
		time.Sleep(10 * time.Millisecond)
		if n := atomic.LoadUint32(&session.brakes); n != 0 || len(p.updates()) != n0 {
			t.Fatalf("%d brakes, updates %v", n, p.updates())
		}
	})
	t.Run("window at home", func(t *testing.T) {
		// a slow reader whose window the usual updates have sized: the
		// peer may send little more than the stream holds
		session, stream, p := brakeRig(t, config.MaxReceiveBuffer/2)
		stream.bufferLock.Lock()
		stream.rcvWin = uint32(3 * config.StreamLagTarget)
		stream.bufferLock.Unlock()
		p.send(config.StreamLagTarget+8192, config.MaxFrameSize)
		waitUnread(t, stream, config.StreamLagTarget+8192)
		time.Sleep(brakeLag + 10*time.Millisecond)
		p.send(4096, config.MaxFrameSize)
		waitUnread(t, stream, config.StreamLagTarget+12288)
		time.Sleep(10 * time.Millisecond)
		if n := atomic.LoadUint32(&session.brakes); n != 0 {
			t.Fatalf("%d brakes of a window with little to take back", n)
		}
	})
	t.Run("keeps its pace", func(t *testing.T) {
		// over the target for well past brakeLag, but read at a pace
		// that takes the backlog down in far less than brakeDrain
		session, stream, p := brakeRig(t, config.MaxReceiveBuffer/2)
		T := config.StreamLagTarget
		p.send(T, config.MaxFrameSize) // at the target: no lag yet
		waitUnread(t, stream, T)
		p.send(16384, config.MaxFrameSize) // over it: the lag starts
		waitUnread(t, stream, T+16384)
		readN(t, stream, 8192, 8192) // the reader is at it at once
		for end := time.Now().Add(100 * time.Millisecond); time.Now().Before(end); {
			p.send(8192, config.MaxFrameSize)
			waitUnread(t, stream, T+16384)
			readN(t, stream, 8192, 8192)
			time.Sleep(time.Millisecond)
		}
		if n := atomic.LoadUint32(&session.brakes); n != 0 {
			t.Fatalf("%d brakes of a reader keeping its pace", n)
		}
	})
	t.Run("burst taken at once", func(t *testing.T) {
		// a frame takes the backlog over the target and the reader takes
		// it all before the next one: the lag never outlives a frame
		session, stream, p := brakeRig(t, config.MaxReceiveBuffer/2)
		T := config.StreamLagTarget
		for i := 0; i < 50; i++ {
			p.send(T, config.MaxFrameSize)
			p.send(8192, 8192)
			waitUnread(t, stream, T+8192)
			readN(t, stream, T+8192, 8192)
		}
		if n := atomic.LoadUint32(&session.brakes); n != 0 {
			t.Fatalf("%d brakes for backlogs read before the next frame", n)
		}
	})
}

// Window updates from the reader and from brakes reach the peer in order:
// the consumed count never goes back, and the peer, keeping to the latest
// window it saw, is never left waiting for good -- under a reader that keeps
// stopping, next to a stream holding half the session's buffer.
func TestAdaptiveBrakeUpdatesInOrder(t *testing.T) {
	config := adaptiveConfig()
	session, stream, p := brakeRig(t, config.MaxReceiveBuffer/2)
	const total = 32 << 20
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // reader: bursts of random reads, then a stop of 40-80 ms
		defer wg.Done()
		rng := rand.New(rand.NewSource(1))
		buf := make([]byte, 16384)
		next := 0
		for got := 0; got < total; {
			stream.SetReadDeadline(time.Now().Add(10 * time.Second))
			n, err := stream.Read(buf[:1+rng.Intn(len(buf))])
			if err != nil {
				t.Errorf("read: %v", err)
				return
			}
			got += n
			// stop once caught up, so the backlog builds while it
			// stops: the stream lags at no pace at all
			if got >= next && unreadOf(stream) <= config.StreamLagTarget {
				time.Sleep(time.Duration(40+rng.Intn(40)) * time.Millisecond)
				next = got + 256<<10 + rng.Intn(512<<10)
			}
		}
	}()
	p.sendInWindow(total, config.MaxFrameSize) // fails the test if the window stays shut
	wg.Wait()
	upds := p.updates()
	for i := 1; i < len(upds); i++ {
		if int32(upds[i][0]-upds[i-1][0]) < 0 {
			t.Fatalf("update %d consumed %d after %d", i, upds[i][0], upds[i-1][0])
		}
	}
	if n := atomic.LoadUint32(&session.brakes); n == 0 {
		t.Fatal("never braked")
	}
	t.Logf("%d updates, %d brakes", len(upds), atomic.LoadUint32(&session.brakes))
}

// A fast reader that only pauses for 10 ms (a GC pause, a busy core, a local
// socket that took a moment) while the other streams hold half the session's
// buffer may be braked, but once it has read all it held it gets its window
// back at once: it does not have to earn it back update by update.
func TestAdaptivePausedReaderGetsWindowBack(t *testing.T) {
	config := adaptiveConfig()
	session, stream, p := brakeRig(t, config.MaxReceiveBuffer/2)
	before := rcvWinOf(stream)
	// the reader pauses for 10 ms; the peer goes on at about 20 MB/s
	end := time.Now().Add(10 * time.Millisecond)
	for time.Now().Before(end) && p.room() > 20<<10 {
		p.send(20<<10, 4096)
		time.Sleep(time.Millisecond)
	}
	time.Sleep(5 * time.Millisecond)
	brakes, after := atomic.LoadUint32(&session.brakes), rcvWinOf(stream)
	// the reader is back and keeps up: how much must it read before its
	// window is full again?
	readN(t, stream, unreadOf(stream), 4096)
	read := 0
	for rcvWinOf(stream) < uint32(config.MaxStreamBuffer) && read < 16<<20 {
		p.sendInWindow(4096, 4096)
		p.settle(stream)
		readN(t, stream, 4096, 4096)
		read += 4096
	}
	t.Logf("pause: %d brakes, window %d KiB -> %d KiB, full again after %d KiB read", brakes, before>>10, after>>10, read>>10)
	if read > 64<<10 {
		t.Errorf("a 10 ms pause of a fast reader braked it %d times (window %d -> %d KiB) and it had to read %d KiB to get its window back", brakes, before>>10, after>>10, read>>10)
	}
}

// Read honours its deadline when its window update cannot be queued (the
// session's writer is stuck and its shaper full), braking update or not: a
// braking update waits no longer than brakeWait for its turn, so it never
// holds the reader's own update past the reader's deadline.
func TestAdaptiveReadDeadlineBehindBrakeUpdate(t *testing.T) {
	for _, brake := range []bool{false, true} {
		config := adaptiveConfig()
		c1, c2 := net.Pipe()
		session, err := Client(c1, config)
		if err != nil {
			t.Fatal(err)
		}
		var stop atomic.Bool
		syn := make(chan uint32, 1)
		go func() { // the peer reads until stop, then never again
			var hdr rawHeader
			for !stop.Load() {
				if _, err := io.ReadFull(c2, hdr[:]); err != nil {
					return
				}
				if _, err := io.ReadFull(c2, make([]byte, hdr.Length())); err != nil {
					return
				}
				if hdr.Cmd() == cmdSYN {
					syn <- hdr.StreamID()
				}
			}
		}()
		stream, err := session.OpenStream()
		if err != nil {
			t.Fatal(err)
		}
		sid := <-syn
		stop.Store(true)
		frame := make([]byte, headerSize+4096)
		frame[0], frame[1] = byte(config.Version), cmdPSH
		binary.LittleEndian.PutUint16(frame[2:], 4096)
		binary.LittleEndian.PutUint32(frame[4:], sid)
		if _, err := c2.Write(frame); err != nil {
			t.Fatal(err)
		}
		waitUnread(t, stream, 4096)
		// the writer is stuck (nobody reads c2); fill the shaper
		for i := 0; i < maxShaperSize+64; i++ {
			go session.writeFrameInternal(newFrame(byte(config.Version), cmdNOP, 0), nil, CLSDATA)
		}
		time.Sleep(200 * time.Millisecond)
		if brake {
			go func() { // what checkLag's goroutine does
				t := time.NewTimer(brakeWait)
				stream.writeWindowUpdate(t.C)
				t.Stop()
			}()
			time.Sleep(50 * time.Millisecond)
		}
		stream.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		start := time.Now()
		n, err := stream.Read(make([]byte, 4096))
		took := time.Since(start)
		session.Close()
		if took > time.Second {
			t.Errorf("brake %v: Read with a 100 ms deadline returned after %v (n %d, %v)", brake, took, n, err)
		}
	}
}

// A transfer that follows a small exchange after a wait (a TLS handshake:
// the first read is the server's hello, the response comes a round trip
// later) keeps up as well as one that starts at once, and jumps as far: its
// pace is measured from its own start, not from the hello.
func TestAdaptiveFirstResizeJumpsAfterHandshake(t *testing.T) {
	config := adaptiveConfig()
	_, stream, p := newRawPeer(t, config)
	p.send(5000, config.MaxFrameSize) // the server's hello
	p.settle(stream)
	readN(t, stream, 5000, 5000)
	p.sync(stream)
	time.Sleep(300 * time.Millisecond) // a round trip for the request
	keepUp(t, p, stream, initialPeerWindow/2+4096)
	p.sync(stream)
	upds := p.updates()
	if len(upds) < 2 || upds[1][1] != uint32(config.MaxStreamBuffer) {
		t.Errorf("reader that keeps up after a handshake: updates %v, want the full %d second", upds, config.MaxStreamBuffer)
	}
}
