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

	// FIN command
	chFinEvent   chan struct{}
	finEventOnce sync.Once

	// deadlines
	readDeadline  atomic.Value
	writeDeadline atomic.Value

	// per stream sliding window control
	numRead    uint32 // number of consumed bytes
	numWritten uint32 // count num of bytes written
	incr       uint32 // counting for sending
	rcvWin     uint32 // window announced in our last UPD (adaptive: under bufferLock)
	unread     int    // bytes in buffers not yet read (under bufferLock)
	minUnread  int    // adaptive: least unread left by a read since our last UPD
	lastParks  uint32 // adaptive: sess.parks at our last UPD

	// UPD command
	peerConsumed uint32        // num of bytes the peer has consumed
	peerWindow   uint32        // peer window, initialized to 256KB, updated by peer
	chUpdate     chan struct{} // notify of remote data consuming and window update
}

// newStream initiates a Stream struct
func newStream(id uint32, frameSize int, sess *Session) *Stream {
	s := new(Stream)
	s.id = id
	s.chReadEvent = make(chan struct{}, 1)
	s.chUpdate = make(chan struct{}, 1)
	s.frameSize = frameSize
	s.sess = sess
	s.die = make(chan struct{})
	s.chFinEvent = make(chan struct{})
	s.peerWindow = initialPeerWindow // set to initial window size
	s.rcvWin = uint32(sess.config.MaxStreamBuffer)
	if min := sess.config.MinStreamBuffer; min > 0 {
		// An adaptive stream starts at the window the peer assumes before
		// any update and earns the rest by keeping up, so a reader that is
		// slow from the start never holds a full MaxStreamBuffer.
		if s.rcvWin > initialPeerWindow {
			s.rcvWin = initialPeerWindow
		}
		if s.rcvWin < uint32(min) {
			s.rcvWin = uint32(min)
		}
		s.lastParks = atomic.LoadUint32(&sess.parks)
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
	s.unread -= n
	s.bufferLock.Unlock()

	if n > 0 {
		s.sess.returnTokens(n)
		return n, nil
	}

	select {
	case <-s.die:
		return 0, io.EOF
	default:
		return 0, ErrWouldBlock
	}
}

func (s *Stream) tryReadv2(b []byte) (n int, err error) {
	if len(b) == 0 {
		return 0, nil
	}

	var notify bool
	var notifyConsumed, notifyWindow uint32
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
	s.unread -= n

	// in an ideal environment:
	// if more than half of buffer has consumed, send read ack to peer
	// based on round-trip time of ACK, continous flowing data
	// won't slow down because of waiting for ACK, as long as the
	// consumer keeps on reading data
	// s.numRead == n also notify window at the first read
	if n > 0 {
		notify, notifyConsumed, notifyWindow = s.consumed(uint32(n))
	}
	s.bufferLock.Unlock()

	if n > 0 {
		s.sess.returnTokens(n)
		if notify {
			err := s.sendWindowUpdate(notifyConsumed, notifyWindow)
			return n, err
		} else {
			return n, nil
		}
	}

	select {
	case <-s.die:
		return 0, io.EOF
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
		s.unread -= len(buf)
		s.bufferLock.Unlock()

		if buf != nil {
			nw, ew := w.Write(buf)
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
		var notify bool
		var notifyConsumed, notifyWindow uint32
		var buf []byte
		s.bufferLock.Lock()
		if len(s.buffers) > 0 {
			buf = s.buffers[0]
			s.buffers = s.buffers[1:]
			s.heads = s.heads[1:]
		}
		s.unread -= len(buf)
		if len(buf) > 0 {
			notify, notifyConsumed, notifyWindow = s.consumed(uint32(len(buf)))
		}
		s.bufferLock.Unlock()

		if buf != nil {
			nw, ew := w.Write(buf)
			s.sess.returnTokens(len(buf))
			defaultAllocator.Put(buf)
			if nw > 0 {
				n += int64(nw)
			}

			if ew != nil {
				return n, ew
			}

			if notify {
				if err := s.sendWindowUpdate(notifyConsumed, notifyWindow); err != nil {
					return n, err
				}
			}
		} else if ew := s.waitRead(); ew != nil {
			return n, ew
		}
	}
}

// consumed accounts n bytes just read (n > 0) and reports whether a window
// update is due, with the consumed count and window it must carry. The caller
// holds bufferLock and has already taken the n bytes off unread.
//
// An update is due at the first read and each time half of the window
// announced in the last update has been read since. The peer may run at most
// rcvWin bytes past the consumed count of our last update; we always read
// rcvWin/2 of that and announce again, so the peer never waits for an update
// that is not coming, whatever the window does in between.
func (s *Stream) consumed(n uint32) (notify bool, consumed, window uint32) {
	s.numRead += n
	s.incr += n
	if s.unread < s.minUnread {
		s.minUnread = s.unread
	}
	first := s.numRead == n
	if s.incr < s.rcvWin/2 && !first {
		return false, 0, 0
	}
	s.incr = 0
	if cfg := s.sess.config; cfg.MinStreamBuffer > 0 {
		parks := atomic.LoadUint32(&s.sess.parks)
		if !first {
			s.adaptWindow(cfg, parks != s.lastParks)
		}
		s.lastParks = parks
		s.minUnread = s.unread
	}
	return true, s.numRead, s.rcvWin
}

// adaptWindow resizes the adaptive receive window (Config.MinStreamBuffer >
// 0) at a window update, from the least backlog the reader left unread
// after any of its reads since the previous update: what it held on to the
// whole time, however the peer's bursts arrived.
//
// A backlog over StreamLagTarget is surplus: the window shrinks by twice the
// excess (an update releases half the window, so that takes the excess off the
// next backlog), down to MinStreamBuffer. A backlog under a quarter of the
// target means the reader ran dry: the window doubles, up to MaxStreamBuffer
// -- unless the session's reader waited for buffer space since the previous
// update (parked): then the stream starved behind the other streams, not
// behind its own window, and growing it would only deepen the jam.
func (s *Stream) adaptWindow(cfg *Config, parked bool) {
	standing := s.minUnread
	switch {
	case standing > cfg.StreamLagTarget:
		excess := uint64(standing - cfg.StreamLagTarget)
		if 2*excess >= uint64(s.rcvWin)-uint64(cfg.MinStreamBuffer) {
			s.rcvWin = uint32(cfg.MinStreamBuffer)
		} else {
			s.rcvWin -= uint32(2 * excess)
		}
	case standing < cfg.StreamLagTarget/4 && !parked:
		s.rcvWin *= 2
		if s.rcvWin > uint32(cfg.MaxStreamBuffer) {
			s.rcvWin = uint32(cfg.MaxStreamBuffer)
		}
	}
}

func (s *Stream) sendWindowUpdate(consumed, window uint32) error {
	var timer *time.Timer
	var deadline <-chan time.Time
	if d, ok := s.readDeadline.Load().(time.Time); ok && !d.IsZero() {
		timer = time.NewTimer(time.Until(d))
		defer timer.Stop()
		deadline = timer.C
	}

	frame := newFrame(byte(s.sess.config.Version), cmdUPD, s.id)
	var hdr updHeader
	binary.LittleEndian.PutUint32(hdr[:], consumed)
	binary.LittleEndian.PutUint32(hdr[4:], window)
	frame.data = hdr[:]
	_, err := s.sess.writeFrameInternal(frame, deadline, CLSDATA)
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
	if s.sess.config.Version == 2 {
		return s.writeV2(b)
	}

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
		n, err := s.sess.writeFrameInternal(frame, deadline, CLSDATA)
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
				n, err := s.sess.writeFrameInternal(frame, deadline, CLSDATA)
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
			select {
			case <-s.chFinEvent: // if fin arrived, future window update is impossible
				return 0, io.EOF
			case <-s.die:
				return sent, io.ErrClosedPipe
			case <-deadline:
				return sent, ErrTimeout
			case <-s.sess.chSocketWriteError:
				return sent, s.sess.socketWriteError.Load().(error)
			case <-s.chUpdate:
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
		close(s.die)
		once = true
	})

	if once {
		_, err = s.sess.writeFrame(newFrame(byte(s.sess.config.Version), cmdFIN, s.id))
		s.sess.streamClosed(s.id)
		return err
	} else {
		return io.ErrClosedPipe
	}
}

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
	return nil
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
	s.unread += len(buf)
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
	s.unread = 0
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
	atomic.StoreUint32(&s.peerConsumed, consumed)
	atomic.StoreUint32(&s.peerWindow, window)
	select {
	case s.chUpdate <- struct{}{}:
	default:
	}
}

// mark this stream has been closed in protocol
func (s *Stream) fin() {
	s.finEventOnce.Do(func() {
		close(s.chFinEvent)
	})
}
