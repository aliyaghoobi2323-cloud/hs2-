package smux

import (
	"encoding/binary"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Stream implements net.Conn
type Stream struct {
	id   uint32
	sess *Session

	buffers [][]byte
	heads   [][]byte // slice heads kept for recycle

	bufferLock sync.Mutex
	frameSize  int

	// notify a read event
	chReadEvent chan struct{}

	// flag the stream has closed
	die     chan struct{}
	dieOnce sync.Once
	// fx170: die was closed by Close (this side ended the stream on
	// purpose), not by the session's end
	closedHere atomic.Bool

	// FIN command
	chFinEvent   chan struct{}
	finEventOnce sync.Once

	// hs2 hc183 (half-close): wcloseOnce sends this side's one cmdFIN
	// (CloseWrite or Close); wclosed: no more Write. halfOpen (set by the
	// engine only when the peer announced half-close): the peer's FIN ends
	// only its direction, so a Write waiting for window does not give up
	// when it arrives (the peer still reads and sends window updates).
	wcloseOnce sync.Once
	wclosed    atomic.Bool
	halfOpen   atomic.Bool
	cwFailed   atomic.Bool // hc213: CloseWriteTimeout's FIN failed
	// peerGone (hc183b): closed on a second cmdFIN from the peer — a
	// half-open peer that already sent its FIN (CloseWrite) and now dropped
	// the stream (Close), so nothing will read what this side still writes.
	// hc213: also on a zero-length cmdPSH, sent ahead of the FINs by a
	// half-open peer that closed without CloseWrite (abandoned, not done).
	peerGone     chan struct{}
	peerGoneOnce sync.Once
	abandonCh    chan struct{} // hc213: the empty push only
	abandonOnce  sync.Once

	// deadlines
	readDeadline  atomic.Value
	writeDeadline atomic.Value

	// hs2 er94: closed when SetWriteDeadline moves the deadline into the
	// past, so a Write already waiting (window, shaper, a dead socket) wakes:
	// upstream reads the deadline once per Write, and a raw write stuck on a
	// blackholed link never returned (RESCUE could not take the writer off).
	wcutMu sync.Mutex
	wcut   chan struct{}

	// per stream sliding window control
	numRead    uint32 // number of consumed bytes
	numWritten uint32 // count num of bytes written
	incr       uint32 // counting for sending

	// UPD command
	peerConsumed uint32        // num of bytes the peer has consumed
	peerWindow   uint32        // peer window, initialized to 256KB, updated by peer
	chUpdate     chan struct{} // notify of remote data consuming and window update

	// hs2: adaptive receive window (Config.AutoWindow, v2 only; nil = upstream)
	aw *streamAW

	// hs2 aw163: AutoWindow's UPDs leave in the order they were computed.
	// A shrink sends its UPD from another goroutine while a read may be
	// sending an older one; the older arriving last moved the sender's limit
	// below what the receiver counts on, and both sides waited forever.
	// updSeq numbers each UPD under bufferLock; updSem/updSent drop one
	// computed before the last sent (the later one carries the newer state).
	// fx168: updSem (a one-slot semaphore, not a mutex) is taken with the
	// read deadline and the stream's end: its holder may wait in a full
	// shaper (one-way blackhole) and a Read must not wait past its deadline
	// for it.
	updSeq  uint32
	updSem  chan struct{}
	updSent uint32

	// hs2 mem67 (stall.go): unix ns of the last read (or creation); set when
	// the stall guard dropped this stream
	lastRead atomic.Int64
	evicted  atomic.Bool

	// hs2 dl76 (trace.go): nil unless Trace
	tr *streamTrace
}

// newStream initiates a Stream struct
func newStream(id uint32, frameSize int, sess *Session) *Stream {
	s := new(Stream)
	TrackObject(s, &objStreams)
	s.id = id
	s.chReadEvent = make(chan struct{}, 1)
	s.chUpdate = make(chan struct{}, 1)
	s.frameSize = frameSize
	s.sess = sess
	s.die = make(chan struct{})
	s.updSem = make(chan struct{}, 1)
	s.chFinEvent = make(chan struct{})
	s.peerGone = make(chan struct{})
	s.abandonCh = make(chan struct{})
	s.peerWindow = initialPeerWindow // set to initial window size
	s.lastRead.Store(nowNano())
	if sess.config.Version == 2 && sess.config.AutoWindow != nil {
		s.aw = newStreamAW(sess.config.AutoWindow, sess.config.MaxStreamBuffer)
	}
	return s
}

// ID returns the unique stream ID.
func (s *Stream) ID() uint32 {
	return s.id
}

// Read implements net.Conn
func (s *Stream) Read(b []byte) (n int, err error) {
	for {
		n, err = s.tryRead(b)
		if err == ErrWouldBlock {
			if ew := s.waitRead(); ew != nil {
				return 0, ew
			}
		} else {
			return n, err
		}
	}
}

// tryRead is the nonblocking version of Read
func (s *Stream) tryRead(b []byte) (n int, err error) {
	if s.sess.config.Version == 2 {
		return s.tryReadv2(b)
	}

	if len(b) == 0 {
		return 0, nil
	}

	s.bufferLock.Lock()
	if len(s.buffers) > 0 {
		n = copy(b, s.buffers[0])
		s.buffers[0] = s.buffers[0][n:]
		if len(s.buffers[0]) == 0 {
			s.buffers[0] = nil
			s.buffers = s.buffers[1:]
			// full recycle
			defaultAllocator.Put(s.heads[0])
			s.heads = s.heads[1:]
		}
	}
	s.bufferLock.Unlock()

	if n > 0 {
		s.lastRead.Store(nowNano())
		s.sess.returnTokens(n)
		return n, nil
	}

	select {
	case <-s.die:
		return 0, s.closedErr()
	default:
		return 0, ErrWouldBlock
	}
}

func (s *Stream) tryReadv2(b []byte) (n int, err error) {
	if len(b) == 0 {
		return 0, nil
	}

	var notifyConsumed, notifyWin, notifySeq uint32
	s.bufferLock.Lock()
	if len(s.buffers) > 0 {
		n = copy(b, s.buffers[0])
		s.buffers[0] = s.buffers[0][n:]
		if len(s.buffers[0]) == 0 {
			s.buffers[0] = nil
			s.buffers = s.buffers[1:]
			// full recycle
			defaultAllocator.Put(s.heads[0])
			s.heads = s.heads[1:]
		}
	}

	// in an ideal environment:
	// if more than half of buffer has consumed, send read ack to peer
	// based on round-trip time of ACK, continous flowing data
	// won't slow down because of waiting for ACK, as long as the
	// consumer keeps on reading data
	// s.numRead == n also notify window at the first read
	s.numRead += uint32(n)
	s.incr += uint32(n)
	if s.aw != nil {
		notifyConsumed, notifyWin, notifySeq = s.awConsumed(uint32(n))
	} else if s.incr >= uint32(s.sess.config.MaxStreamBuffer/2) || s.numRead == uint32(n) {
		notifyConsumed = s.numRead
		s.incr = 0
	}
	s.bufferLock.Unlock()

	if n > 0 {
		s.lastRead.Store(nowNano())
		s.sess.returnTokens(n)
		if notifyConsumed > 0 {
			err := s.sendWindowUpdateSeq(notifyConsumed, notifyWin, notifySeq)
			return n, err
		} else {
			return n, nil
		}
	}

	select {
	case <-s.die:
		return 0, s.closedErr()
	default:
		return 0, ErrWouldBlock
	}
}

// WriteTo implements io.WriteTo
func (s *Stream) WriteTo(w io.Writer) (n int64, err error) {
	if s.sess.config.Version == 2 {
		return s.writeTov2(w)
	}

	for {
		var buf []byte
		s.bufferLock.Lock()
		if len(s.buffers) > 0 {
			buf = s.buffers[0]
			s.buffers = s.buffers[1:]
			s.heads = s.heads[1:]
		}
		s.bufferLock.Unlock()

		if buf != nil {
			nw, ew := w.Write(buf)
			s.lastRead.Store(nowNano())
			s.sess.returnTokens(len(buf))
			defaultAllocator.Put(buf)
			if nw > 0 {
				n += int64(nw)
			}

			if ew != nil {
				return n, ew
			}
		} else if ew := s.waitRead(); ew != nil {
			return n, ew
		}
	}
}

func (s *Stream) writeTov2(w io.Writer) (n int64, err error) {
	for {
		var notifyConsumed, notifyWin, notifySeq uint32
		var buf []byte
		s.bufferLock.Lock()
		if len(s.buffers) > 0 {
			buf = s.buffers[0]
			s.buffers = s.buffers[1:]
			s.heads = s.heads[1:]
		}
		s.numRead += uint32(len(buf))
		s.incr += uint32(len(buf))
		if s.aw != nil {
			notifyConsumed, notifyWin, notifySeq = s.awConsumed(uint32(len(buf)))
		} else if s.incr >= uint32(s.sess.config.MaxStreamBuffer/2) || s.numRead == uint32(len(buf)) {
			notifyConsumed = s.numRead
			s.incr = 0
		}
		s.bufferLock.Unlock()

		if buf != nil {
			nw, ew := w.Write(buf)
			s.lastRead.Store(nowNano())
			s.sess.returnTokens(len(buf))
			defaultAllocator.Put(buf)
			if nw > 0 {
				n += int64(nw)
			}

			if ew != nil {
				return n, ew
			}

			if notifyConsumed > 0 {
				if err := s.sendWindowUpdateSeq(notifyConsumed, notifyWin, notifySeq); err != nil {
					return n, err
				}
			}
		} else if ew := s.waitRead(); ew != nil {
			return n, ew
		}
	}
}

// sendWindowUpdateSeq sends a UPD numbered seq (nextUPDSeqLocked; 0: no
// order kept, upstream) unless one numbered later was queued already. UPDs
// of one stream are queued under updSem in their number's order and the
// shaper keeps that order (same class, same jump, request seq), so updSem is
// held only until the frame is queued, never while it waits for the socket.
// fx168 (audit rel66 M6): a holder may still wait in a full shaper (the
// socket write stuck, a one-way blackhole); taking updSem gives up at the
// read deadline, the stream's end or the session's, as the queueing itself
// does.
func (s *Stream) sendWindowUpdateSeq(consumed, window, seq uint32) error {
	if seq == 0 {
		return s.sendWindowUpdate(consumed, window)
	}
	deadline, stop := s.readDeadlineCh()
	defer stop()
	select {
	case s.updSem <- struct{}{}:
	case <-deadline:
		return ErrTimeout
	case <-s.die:
		return io.ErrClosedPipe
	case <-s.sess.die:
		return io.ErrClosedPipe
	case <-s.sess.chSocketWriteError:
		return s.sess.socketWriteError.Load().(error)
	}
	if s.updSent != 0 && int32(seq-s.updSent) <= 0 {
		<-s.updSem
		return nil // stale: a later UPD told the peer the newer state
	}
	s.updSent = seq
	return s.sendUPD(consumed, window, deadline, func() { <-s.updSem })
}

// readDeadlineCh: a channel that fires at the read deadline (nil: none) and
// its stop.
func (s *Stream) readDeadlineCh() (<-chan time.Time, func()) {
	if d, ok := s.readDeadline.Load().(time.Time); ok && !d.IsZero() {
		timer := time.NewTimer(time.Until(d))
		return timer.C, func() { timer.Stop() }
	}
	return nil, func() {}
}

// nextUPDSeqLocked numbers a UPD (bufferLock held); never 0.
func (s *Stream) nextUPDSeqLocked() uint32 {
	s.updSeq++
	if s.updSeq == 0 {
		s.updSeq = 1
	}
	return s.updSeq
}

// window 0: the configured MaxStreamBuffer (upstream); AutoWindow passes its own.
func (s *Stream) sendWindowUpdate(consumed, window uint32) error {
	deadline, stop := s.readDeadlineCh()
	defer stop()
	return s.sendUPD(consumed, window, deadline, nil)
}

// sendUPD: queued (if not nil) runs once the frame is queued (or given up on).
func (s *Stream) sendUPD(consumed, window uint32, deadline <-chan time.Time, queued func()) error {
	if window == 0 {
		window = uint32(s.sess.config.MaxStreamBuffer)
	}
	frame := newFrame(byte(s.sess.config.Version), cmdUPD, s.id)
	var hdr updHeader
	binary.LittleEndian.PutUint32(hdr[:], consumed)
	binary.LittleEndian.PutUint32(hdr[4:], window)
	frame.data = hdr[:]
	_, err := s.sess.writeFrameQueued(frame, deadline, nil, CLSDATA, queued)
	return err
}

func (s *Stream) waitRead() error {
	var timer *time.Timer
	var deadline <-chan time.Time
	if d, ok := s.readDeadline.Load().(time.Time); ok && !d.IsZero() {
		timer = time.NewTimer(time.Until(d))
		defer timer.Stop()
		deadline = timer.C
	}

	select {
	case <-s.chReadEvent:
		return nil
	case <-s.chFinEvent:
		// BUG(xtaci): Fix for https://github.com/xtaci/smux/issues/82
		s.bufferLock.Lock()
		defer s.bufferLock.Unlock()
		if len(s.buffers) > 0 {
			return nil
		}
		return io.EOF
	case <-s.sess.chSocketReadError:
		return s.sess.socketReadError.Load().(error)
	case <-s.sess.chProtoError:
		return s.sess.protoError.Load().(error)
	case <-deadline:
		return ErrTimeout
	case <-s.die:
		return io.ErrClosedPipe
	}

}

// Write implements net.Conn
//
// Note that the behavior when multiple goroutines write concurrently is not deterministic,
// frames may interleave in random way.
func (s *Stream) Write(b []byte) (n int, err error) {
	if s.wclosed.Load() {
		return 0, io.ErrClosedPipe // hs2 hc183: CloseWrite ended this direction
	}
	if s.sess.config.Version == 2 {
		return s.writeV2(b)
	}

	cut := s.writeCut()
	var deadline <-chan time.Time
	if d, ok := s.writeDeadline.Load().(time.Time); ok && !d.IsZero() {
		timer := time.NewTimer(time.Until(d))
		defer timer.Stop()
		deadline = timer.C
	}

	// check if stream has closed
	select {
	case <-s.die:
		return 0, io.ErrClosedPipe
	default:
	}

	// frame split and transmit
	sent := 0
	frame := newFrame(byte(s.sess.config.Version), cmdPSH, s.id)
	bts := b
	for len(bts) > 0 {
		sz := len(bts)
		if sz > s.frameSize {
			sz = s.frameSize
		}
		frame.data = bts[:sz]
		bts = bts[sz:]
		n, err := s.sess.writeFrameCut(frame, deadline, cut, CLSDATA)
		s.numWritten++
		sent += n
		if err != nil {
			return sent, err
		}
	}

	return sent, nil
}

func (s *Stream) writeV2(b []byte) (n int, err error) {
	// check empty input
	if len(b) == 0 {
		return 0, nil
	}

	// check if stream has closed
	select {
	case <-s.die:
		return 0, io.ErrClosedPipe
	default:
	}

	// create write deadline timer
	cut := s.writeCut()
	var deadline <-chan time.Time
	if d, ok := s.writeDeadline.Load().(time.Time); ok && !d.IsZero() {
		timer := time.NewTimer(time.Until(d))
		defer timer.Stop()
		deadline = timer.C
	}

	// frame split and transmit process
	sent := 0
	frame := newFrame(byte(s.sess.config.Version), cmdPSH, s.id)

	for {
		// per stream sliding window control
		// [.... [consumed... numWritten] ... win... ]
		// [.... [consumed...................+rmtwnd]]
		var bts []byte
		// note:
		// even if uint32 overflow, this math still works:
		// eg1: uint32(0) - uint32(math.MaxUint32) = 1
		// eg2: int32(uint32(0) - uint32(1)) = -1
		// security check for misbehavior
		inflight := int32(atomic.LoadUint32(&s.numWritten) - atomic.LoadUint32(&s.peerConsumed))
		if inflight < 0 {
			return 0, ErrConsumed
		}

		win := int32(atomic.LoadUint32(&s.peerWindow)) - inflight
		if win > 0 {
			if win > int32(len(b)) {
				bts = b
				b = nil
			} else {
				bts = b[:win]
				b = b[win:]
			}

			for len(bts) > 0 {
				sz := len(bts)
				if sz > s.frameSize {
					sz = s.frameSize
				}
				frame.data = bts[:sz]
				bts = bts[sz:]
				var t0 time.Time
				if s.tr != nil {
					t0 = time.Now()
				}
				n, err := s.sess.writeFrameCut(frame, deadline, cut, CLSDATA)
				if s.tr != nil {
					s.tr.sockNs.Add(int64(time.Since(t0)))
					s.tr.wrBytes.Add(int64(sz))
				}
				atomic.AddUint32(&s.numWritten, uint32(sz))
				sent += n
				if err != nil {
					return sent, err
				}
			}
		}

		// if there is any data remaining to be sent
		// wait until stream closes, window changes or deadline reached
		// this blocking behavior will inform upper layer to do flow control
		if len(b) > 0 {
			var tw time.Time
			if s.tr != nil {
				tw = time.Now()
			}
			finCh := s.chFinEvent
			if s.halfOpen.Load() {
				finCh = nil // hs2 hc183: a half-closed peer still reads
			}
			select {
			case <-finCh: // if fin arrived, future window update is impossible
				return 0, io.EOF
			case <-s.die:
				return sent, io.ErrClosedPipe
			case <-deadline:
				return sent, ErrTimeout
			case <-cut:
				return sent, ErrTimeout
			case <-s.sess.chSocketWriteError:
				return sent, s.sess.socketWriteError.Load().(error)
			case <-s.chUpdate:
				if s.tr != nil {
					s.tr.winWaitNs.Add(int64(time.Since(tw)))
				}
				continue
			}
		} else {
			return sent, nil
		}
	}
}

// Close implements net.Conn
func (s *Stream) Close() error {
	var once bool
	var err error
	s.dieOnce.Do(func() {
		s.closedHere.Store(true)
		close(s.die)
		once = true
	})

	if once {
		fin := false
		s.wcloseOnce.Do(func() { fin = true; s.wclosed.Store(true) })
		half := s.halfOpen.Load()
		if s.cwFailed.Load() {
			fin = true // hc213: its CloseWrite's FIN timed out: abandoned
		}
		// hs2 fx247 (audit246 L2): the close frames share one deadline
		// (openCloseTimeout in all, not each; abandonCloseTimeout once the
		// CloseWrite's FIN timed out: the writer is known stuck), and stop
		// at the first failure
		dl := openCloseTimeout
		if s.cwFailed.Load() {
			dl = abandonCloseTimeout
		}
		deadline := time.After(dl)
		if fin && half {
			// hs2 hc213: closed without a CloseWrite toward a half-open peer:
			// the stream is abandoned (a reset user, a cut relay), not
			// half-closed. Say so before the FIN: a zero-length cmdPSH
			// (ignored by any peer as an empty push) closes PeerGone on a
			// peer that knows it ahead of the EOF, and the second FIN below
			// does the same on a hc183b peer right behind the EOF.
			_, err = s.sess.writeFrameInternal(newFrame(byte(s.sess.config.Version), cmdPSH, s.id), deadline, CLSCTRL)
		}
		if err == nil && (fin || half) {
			// hs2 hc183: one FIN; after CloseWrite toward a half-open peer
			// a second one says "dropped" (hc183b: PeerGone on its side)
			_, err = s.sess.writeFrameInternal(newFrame(byte(s.sess.config.Version), cmdFIN, s.id), deadline, CLSCTRL)
		}
		if err == nil && fin && half {
			_, err = s.sess.writeFrameInternal(newFrame(byte(s.sess.config.Version), cmdFIN, s.id), deadline, CLSCTRL)
		}
		s.sess.streamClosed(s.id)
		return err
	} else {
		return io.ErrClosedPipe
	}
}

// CloseWrite (hs2 hc183) ends this side's direction: a FIN after the bytes
// already written; Read goes on until the peer's FIN. Writes after it fail.
// Close later releases the stream (without a second FIN).
func (s *Stream) CloseWrite() error {
	select {
	case <-s.die:
		return io.ErrClosedPipe
	default:
	}
	err := error(io.ErrClosedPipe)
	s.wcloseOnce.Do(func() {
		s.wclosed.Store(true)
		_, err = s.sess.writeFrame(newFrame(byte(s.sess.config.Version), cmdFIN, s.id))
	})
	return err
}

// CloseWriteTimeout (hs2 hc213) is CloseWrite whose FIN waits at most d for
// the session's writer (CloseWrite: openCloseTimeout). On a failure the
// stream counts as abandoned: a later Close toward a half-open peer marks it
// so (the empty push and two FINs), since this FIN may never have left.
func (s *Stream) CloseWriteTimeout(d time.Duration) error {
	select {
	case <-s.die:
		return io.ErrClosedPipe
	default:
	}
	err := error(io.ErrClosedPipe)
	s.wcloseOnce.Do(func() {
		s.wclosed.Store(true)
		t := time.NewTimer(d)
		defer t.Stop()
		_, err = s.sess.writeFrameInternal(newFrame(byte(s.sess.config.Version), cmdFIN, s.id), t.C, CLSCTRL)
		if err != nil {
			s.cwFailed.Store(true)
		}
	})
	return err
}

// SetHalfOpen (hs2 hc183): the peer keeps reading after its FIN (it
// announced half-close), so this side's writes outlive that FIN.
func (s *Stream) SetHalfOpen(on bool) { s.halfOpen.Store(on) }

// ClosedHere (fx170): the stream was ended by its own Close on this side —
// a read that failed with io.ErrClosedPipe was not cut by a dead link.
func (s *Stream) ClosedHere() bool { return s.closedHere.Load() }

// GetDieCh returns a readonly chan which can be readable
// when the stream is to be closed.
func (s *Stream) GetDieCh() <-chan struct{} {
	return s.die
}

// SetReadDeadline sets the read deadline as defined by
// net.Conn.SetReadDeadline.
// A zero time value disables the deadline.
func (s *Stream) SetReadDeadline(t time.Time) error {
	s.readDeadline.Store(t)
	s.notifyReadEvent()
	return nil
}

// SetWriteDeadline sets the write deadline as defined by
// net.Conn.SetWriteDeadline.
// A zero time value disables the deadline.
func (s *Stream) SetWriteDeadline(t time.Time) error {
	s.writeDeadline.Store(t)
	if !t.IsZero() && !time.Now().Before(t) {
		s.wcutMu.Lock()
		if s.wcut != nil {
			close(s.wcut)
			s.wcut = nil
		}
		s.wcutMu.Unlock()
	}
	return nil
}

// writeCut: the channel SetWriteDeadline closes (hs2 er94).
func (s *Stream) writeCut() <-chan struct{} {
	s.wcutMu.Lock()
	defer s.wcutMu.Unlock()
	if s.wcut == nil {
		s.wcut = make(chan struct{})
	}
	return s.wcut
}

// SetDeadline sets both read and write deadlines as defined by
// net.Conn.SetDeadline.
// A zero time value disables the deadlines.
func (s *Stream) SetDeadline(t time.Time) error {
	if err := s.SetReadDeadline(t); err != nil {
		return err
	}
	if err := s.SetWriteDeadline(t); err != nil {
		return err
	}
	return nil
}

// session closes
func (s *Stream) sessionClose() { s.dieOnce.Do(func() { close(s.die) }) }

// LocalAddr satisfies net.Conn interface
func (s *Stream) LocalAddr() net.Addr {
	if ts, ok := s.sess.conn.(interface {
		LocalAddr() net.Addr
	}); ok {
		return ts.LocalAddr()
	}
	return nil
}

// RemoteAddr satisfies net.Conn interface
func (s *Stream) RemoteAddr() net.Addr {
	if ts, ok := s.sess.conn.(interface {
		RemoteAddr() net.Addr
	}); ok {
		return ts.RemoteAddr()
	}
	return nil
}

// pushBytes append buf to buffers
func (s *Stream) pushBytes(buf []byte) (written int, err error) {
	s.bufferLock.Lock()
	s.buffers = append(s.buffers, buf)
	s.heads = append(s.heads, buf)
	if s.tr != nil {
		s.tr.rdBytes.Add(int64(len(buf)))
	}
	if s.aw != nil {
		s.awPushed(uint32(len(buf)))
	}
	s.bufferLock.Unlock()
	return
}

// recycleTokens transform remaining bytes to tokens(will truncate buffer)
func (s *Stream) recycleTokens() (n int) {
	s.bufferLock.Lock()
	for k := range s.buffers {
		n += len(s.buffers[k])
		defaultAllocator.Put(s.heads[k])
	}
	s.buffers = nil
	s.heads = nil
	s.bufferLock.Unlock()
	return
}

// notify read event
func (s *Stream) notifyReadEvent() {
	select {
	case s.chReadEvent <- struct{}{}:
	default:
	}
}

// update command
func (s *Stream) update(consumed uint32, window uint32) {
	// hs2 aw163: a UPD older than the last one (a smaller consumed: the peer
	// sent it before, it overtook) would move the limit back below what the
	// peer counts on, and neither side would speak again. The peer's
	// consumed count only grows; one with the same count is taken (its
	// window may be the newer one). recvLoop is the only caller.
	if int32(consumed-atomic.LoadUint32(&s.peerConsumed)) < 0 {
		staleUPDs.Add(1)
		return
	}
	atomic.StoreUint32(&s.peerConsumed, consumed)
	atomic.StoreUint32(&s.peerWindow, window)
	select {
	case s.chUpdate <- struct{}{}:
	default:
	}
}

// mark this stream has been closed in protocol
func (s *Stream) fin() {
	first := false
	s.finEventOnce.Do(func() {
		close(s.chFinEvent)
		first = true
	})
	if !first { // hs2 hc183b: a second FIN: the half-open peer dropped the stream
		s.peerGoneOnce.Do(func() { close(s.peerGone) })
	}
}

// PeerGone (hs2 hc183b) is closed when the peer, having half-closed its
// direction, dropped the stream (its second FIN): writes here go nowhere.
func (s *Stream) PeerGone() <-chan struct{} { return s.peerGone }

// abandoned (hs2 hc213): the peer's zero-length cmdPSH: it dropped the stream
// without a CloseWrite (its FINs follow).
func (s *Stream) abandoned() {
	s.abandonOnce.Do(func() { close(s.abandonCh) })
	s.peerGoneOnce.Do(func() { close(s.peerGone) })
}

// PeerAbandoned (hs2 hc213) is closed by the peer's "abandoned" mark only
// (not by the second FIN that also follows a normal end after CloseWrite):
// the EOF that follows it is a cut, not the end of the peer's data.
func (s *Stream) PeerAbandoned() <-chan struct{} { return s.abandonCh }

// staleUPDs counts the window updates update dropped as older than the last
// one (StaleUPDs), process-wide.
var staleUPDs atomic.Uint64

// StaleUPDs: window updates dropped as older than the stream's last one.
func StaleUPDs() uint64 { return staleUPDs.Load() }

// AckMarks (hs2 hs112): bytes written on the stream and the peer's last
// reported consumed count (both wrap at 2^32). A byte at written-mark w has
// been read by the peer's application once int32(consumed-w) >= 0.
func (s *Stream) AckMarks() (written, consumed uint32) {
	return atomic.LoadUint32(&s.numWritten), atomic.LoadUint32(&s.peerConsumed)
}
