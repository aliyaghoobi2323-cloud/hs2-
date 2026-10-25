package smux

import (
	"container/heap"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultAcceptBacklog = 1024
	maxShaperSize        = 1024
	openCloseTimeout     = 30 * time.Second // stream open/close timeout
)

// define frame class
type CLASSID int

const (
	CLSCTRL CLASSID = iota
	CLSDATA
)

var (
	ErrInvalidProtocol = errors.New("invalid protocol")
	ErrConsumed        = errors.New("peer consumed more than sent")
	ErrGoAway          = errors.New("stream id overflows, should start a new connection")
	ErrTimeout         = errors.New("timeout")
	ErrWouldBlock      = errors.New("operation would block on IO")
)

type writeRequest struct {
	class  CLASSID
	frame  Frame
	seq    uint32
	result chan writeResult
	// fair-queue start tag of a data frame (fairQueue), set by the shaper
	start uint64
	// the frame's data as sendLoop reads it (nil: the frame has none)
	data *frameData
}

// frameData hands a frame's data from its writer to sendLoop. A writer that
// stops waiting for its frame (frameWritten: the session died, a write error,
// its deadline) returns only once sendLoop is done with the caller's buffer,
// or has left sendLoop a copy of it: the caller may reuse the buffer at once,
// and a frame still queued goes out with the data it was written with.
type frameData struct {
	mu   sync.Mutex
	b    []byte
	read bool // sendLoop has read b (it holds mu while it does)
}

// take gives sendLoop the frame's data, to read until it calls done: until
// it has copied it, or, over a connection that writes by WriteBuffers, until
// the connection has written it.
func (r writeRequest) take() []byte {
	if r.data == nil {
		return r.frame.data
	}
	r.data.mu.Lock()
	r.data.read = true
	return r.data.b
}

func (r writeRequest) done() {
	if r.data != nil {
		r.data.mu.Unlock()
	}
}

// giveUp: the writer of r stops waiting for its frame; see frameData.
func (r writeRequest) giveUp() {
	if r.data == nil {
		return
	}
	r.data.mu.Lock()
	if !r.data.read {
		r.data.b = append([]byte(nil), r.data.b...)
	}
	r.data.mu.Unlock()
}

type writeResult struct {
	n   int
	err error
}

// Session defines a multiplexed connection for streams
type Session struct {
	conn io.ReadWriteCloser

	config           *Config
	nextStreamID     uint32 // next stream identifier
	nextStreamIDLock sync.Mutex

	bucket       int32         // token bucket
	bucketNotify chan struct{} // used for waiting for tokens
	parks        uint32        // times recvLoop ran out of tokens (and waited, unless it borrowed)
	borrows      uint32        // pool: of those, times the bucket grew from the WindowPool instead (borrow)
	brakes       uint32        // adaptive: braking window updates sent (Stream.checkLag)
	extra        int32         // pool: the bucket's size past MaxReceiveBuffer: the streams' claims on the WindowPool and spare
	spare        int32         // pool: of extra, what no window covers any more (returnSpare)
	spareLock    sync.Mutex    // pool: one returnSpare or borrow at a time
	poolShorts   uint32        // pool: windows the WindowPool could not cover in full (setWindow)
	turnRef      atomic.Int64  // core: the window turnaround reference, ns (0: none; SetCoreRef)
	qBrake       atomic.Int64  // core: how far past turnRef a turnaround holds growth back, ns
	growOK       atomic.Bool   // core: windows may grow past NeedStreamBuffer
	noCover      atomic.Bool   // core: no stream holds its cover (SetCover)
	rcvd         atomic.Uint64 // core: data bytes received
	snapLock     sync.Mutex    // core: one CoreSnapshot at a time
	snapList     []*Stream     // core: CoreSnapshot's copy of the streams (under snapLock)
	pwLock       sync.Mutex    // core: one PeerWindows at a time

	streams    map[uint32]*Stream // all streams in this session
	streamLock sync.Mutex         // locks streams

	die     chan struct{} // flag session has died
	dieOnce sync.Once

	// socket error handling
	socketReadError      atomic.Value
	socketWriteError     atomic.Value
	chSocketReadError    chan struct{}
	chSocketWriteError   chan struct{}
	socketReadErrorOnce  sync.Once
	socketWriteErrorOnce sync.Once

	// smux protocol errors
	protoError     atomic.Value
	chProtoError   chan struct{}
	protoErrorOnce sync.Once

	chAccepts chan *Stream

	dataReady int32 // flag data has arrived

	goAway int32 // flag id exhausted

	deadline atomic.Value

	requestID uint32            // write request monotonic increasing
	shaper    chan writeRequest // a shaper for writing
	writes    chan writeRequest
}

func newSession(config *Config, conn io.ReadWriteCloser, client bool) *Session {
	s := new(Session)
	s.die = make(chan struct{})
	s.conn = conn
	s.config = config
	s.streams = make(map[uint32]*Stream)
	s.chAccepts = make(chan *Stream, defaultAcceptBacklog)
	s.bucket = int32(config.MaxReceiveBuffer)
	s.growOK.Store(true)
	s.bucketNotify = make(chan struct{}, 1)
	s.shaper = make(chan writeRequest)
	s.writes = make(chan writeRequest)
	s.chSocketReadError = make(chan struct{})
	s.chSocketWriteError = make(chan struct{})
	s.chProtoError = make(chan struct{})

	if client {
		s.nextStreamID = 1
	} else {
		s.nextStreamID = 0
	}

	go s.shaperLoop()
	go s.recvLoop()
	go s.sendLoop()
	if !config.KeepAliveDisabled {
		go s.keepalive()
	}
	return s
}

// OpenStream is used to create a new stream
func (s *Session) OpenStream() (*Stream, error) {
	if s.IsClosed() {
		return nil, io.ErrClosedPipe
	}

	// generate stream id
	s.nextStreamIDLock.Lock()
	if s.goAway > 0 {
		s.nextStreamIDLock.Unlock()
		return nil, ErrGoAway
	}

	s.nextStreamID += 2
	sid := s.nextStreamID
	if sid == sid%2 { // stream-id overflows
		s.goAway = 1
		s.nextStreamIDLock.Unlock()
		return nil, ErrGoAway
	}
	s.nextStreamIDLock.Unlock()

	stream := newStream(sid, s.config.MaxFrameSize, s)

	// The stream is known before its SYN leaves: the peer may send on it as
	// soon as it has the SYN, before writeFrame returns, and recvLoop drops
	// the frames of a stream it does not know.
	var err error
	s.streamLock.Lock()
	select {
	case <-s.chSocketReadError:
		err = s.socketReadError.Load().(error)
	case <-s.chSocketWriteError:
		err = s.socketWriteError.Load().(error)
	case <-s.die:
		err = io.ErrClosedPipe
	default:
		s.streams[sid] = stream
	}
	s.streamLock.Unlock()
	if err != nil {
		return nil, err
	}

	if _, err := s.writeFrame(newFrame(byte(s.config.Version), cmdSYN, sid)); err != nil {
		s.streamClosed(sid)
		return nil, err
	}
	return stream, nil
}

// Open returns a generic ReadWriteCloser
func (s *Session) Open() (io.ReadWriteCloser, error) {
	return s.OpenStream()
}

// AcceptStream is used to block until the next available stream
// is ready to be accepted.
func (s *Session) AcceptStream() (*Stream, error) {
	var deadline <-chan time.Time
	if d, ok := s.deadline.Load().(time.Time); ok && !d.IsZero() {
		timer := time.NewTimer(time.Until(d))
		defer timer.Stop()
		deadline = timer.C
	}

	select {
	case stream := <-s.chAccepts:
		return stream, nil
	case <-deadline:
		return nil, ErrTimeout
	case <-s.chSocketReadError:
		return nil, s.socketReadError.Load().(error)
	case <-s.chProtoError:
		return nil, s.protoError.Load().(error)
	case <-s.die:
		return nil, io.ErrClosedPipe
	}
}

// Accept Returns a generic ReadWriteCloser instead of smux.Stream
func (s *Session) Accept() (io.ReadWriteCloser, error) {
	return s.AcceptStream()
}

// Close is used to close the session and all streams.
func (s *Session) Close() error {
	var once bool
	s.dieOnce.Do(func() {
		close(s.die)
		once = true
	})

	if once {
		s.streamLock.Lock()
		for k := range s.streams {
			s.streams[k].sessionClose()
		}
		s.streamLock.Unlock()
		if s.config.WindowPool != nil {
			s.returnSpare() // as far as it is free
			if atomic.LoadInt32(&s.extra) > 0 {
				// what the streams still hold goes back as they let it go,
				// and the rest closeGrace from now (pool.go)
				time.AfterFunc(closeGrace, s.dropClaims)
			}
		}
		return s.conn.Close()
	} else {
		return io.ErrClosedPipe
	}
}

// Parks returns how many times the session's reader has run out of
// MaxReceiveBuffer space: each time, every stream of the session stalled
// until a reader made room, unless the buffer grew from the WindowPool
// instead (Borrows).
func (s *Session) Parks() uint32 { return atomic.LoadUint32(&s.parks) }

// Borrows returns how many of Parks the session's buffer grew from the
// WindowPool instead of waiting (Config.WindowPool).
func (s *Session) Borrows() uint32 { return atomic.LoadUint32(&s.borrows) }

// Brakes returns how many braking window updates the session's adaptive
// streams have sent (Config.MinStreamBuffer).
func (s *Session) Brakes() uint32 { return atomic.LoadUint32(&s.brakes) }

// CloseChan can be used by someone who wants to be notified immediately when this
// session is closed
func (s *Session) CloseChan() <-chan struct{} {
	return s.die
}

// notifyBucket notifies recvLoop that bucket is available
func (s *Session) notifyBucket() {
	select {
	case s.bucketNotify <- struct{}{}:
	default:
	}
}

func (s *Session) notifyReadError(err error) {
	s.socketReadErrorOnce.Do(func() {
		s.socketReadError.Store(err)
		close(s.chSocketReadError)
	})
}

func (s *Session) notifyWriteError(err error) {
	s.socketWriteErrorOnce.Do(func() {
		s.socketWriteError.Store(err)
		close(s.chSocketWriteError)
	})
}

func (s *Session) notifyProtoError(err error) {
	s.protoErrorOnce.Do(func() {
		s.protoError.Store(err)
		close(s.chProtoError)
	})
}

// IsClosed does a safe check to see if we have shutdown
func (s *Session) IsClosed() bool {
	select {
	case <-s.die:
		return true
	default:
		return false
	}
}

// NumStreams returns the number of currently open streams
func (s *Session) NumStreams() int {
	if s.IsClosed() {
		return 0
	}
	s.streamLock.Lock()
	defer s.streamLock.Unlock()
	return len(s.streams)
}

// SetDeadline sets a deadline used by Accept* calls.
// A zero time value disables the deadline.
func (s *Session) SetDeadline(t time.Time) error {
	s.deadline.Store(t)
	return nil
}

// LocalAddr satisfies net.Conn interface
func (s *Session) LocalAddr() net.Addr {
	if ts, ok := s.conn.(interface {
		LocalAddr() net.Addr
	}); ok {
		return ts.LocalAddr()
	}
	return nil
}

// RemoteAddr satisfies net.Conn interface
func (s *Session) RemoteAddr() net.Addr {
	if ts, ok := s.conn.(interface {
		RemoteAddr() net.Addr
	}); ok {
		return ts.RemoteAddr()
	}
	return nil
}

// notify the session that a stream has closed
func (s *Session) streamClosed(sid uint32) {
	s.streamLock.Lock()
	if n := s.streams[sid].recycleTokens(); n > 0 { // return remaining tokens to the bucket
		if atomic.AddInt32(&s.bucket, int32(n)) > 0 {
			s.notifyBucket()
		}
	}
	s.returnSpare()
	delete(s.streams, sid)
	s.streamLock.Unlock()
}

// returnTokens is called by stream to return token after read
func (s *Session) returnTokens(n int) {
	if atomic.AddInt32(&s.bucket, int32(n)) > 0 {
		s.notifyBucket()
	}
}

// recvLoop keeps on reading from underlying connection if tokens are available
func (s *Session) recvLoop() {
	var hdr rawHeader
	var updHdr updHeader

	for {
		for atomic.LoadInt32(&s.bucket) <= 0 && !s.IsClosed() {
			atomic.AddUint32(&s.parks, 1)
			if s.config.WindowPool != nil && s.borrow() {
				continue // the buffer grew instead (WindowPool)
			}
			select {
			case <-s.bucketNotify:
			case <-s.die:
				return
			}
		}

		// read header first
		if _, err := io.ReadFull(s.conn, hdr[:]); err == nil {
			atomic.StoreInt32(&s.dataReady, 1)
			if hdr.Version() != byte(s.config.Version) {
				s.notifyProtoError(ErrInvalidProtocol)
				return
			}
			sid := hdr.StreamID()
			switch hdr.Cmd() {
			case cmdNOP:
			case cmdSYN:
				s.streamLock.Lock()
				if _, ok := s.streams[sid]; !ok {
					stream := newStream(sid, s.config.MaxFrameSize, s)
					s.streams[sid] = stream
					select {
					case s.chAccepts <- stream:
					case <-s.die:
					}
				}
				s.streamLock.Unlock()
			case cmdFIN:
				s.streamLock.Lock()
				if stream, ok := s.streams[sid]; ok {
					stream.fin()
					stream.notifyReadEvent()
				}
				s.streamLock.Unlock()
			case cmdPSH:
				if hdr.Length() > 0 {
					newbuf := defaultAllocator.Get(int(hdr.Length()))
					if written, err := io.ReadFull(s.conn, newbuf); err == nil {
						s.rcvd.Add(uint64(written))
						s.streamLock.Lock()
						if stream, ok := s.streams[sid]; ok {
							stream.pushBytes(newbuf)
							free := atomic.AddInt32(&s.bucket, -int32(written))
							stream.notifyReadEvent()
							if s.config.MinStreamBuffer > 0 {
								stream.checkLag(int(free))
							}
						}
						s.streamLock.Unlock()
					} else {
						s.notifyReadError(err)
						return
					}
				}
			case cmdUPD:
				if _, err := io.ReadFull(s.conn, updHdr[:]); err == nil {
					s.streamLock.Lock()
					if stream, ok := s.streams[sid]; ok {
						stream.update(updHdr.Consumed(), updHdr.Window())
					}
					s.streamLock.Unlock()
				} else {
					s.notifyReadError(err)
					return
				}
			default:
				s.notifyProtoError(ErrInvalidProtocol)
				return
			}
		} else {
			s.notifyReadError(err)
			return
		}
	}
}

func (s *Session) keepalive() {
	tickerPing := time.NewTicker(s.config.KeepAliveInterval)
	tickerTimeout := time.NewTicker(s.config.KeepAliveTimeout)
	defer tickerPing.Stop()
	defer tickerTimeout.Stop()
	for {
		select {
		case <-tickerPing.C:
			s.writeFrameInternal(newFrame(byte(s.config.Version), cmdNOP, 0), tickerPing.C, CLSCTRL)
			s.notifyBucket() // force a signal to the recvLoop
			if s.config.WindowPool != nil {
				s.sweepClaims()
			}
		case <-tickerTimeout.C:
			if !atomic.CompareAndSwapInt32(&s.dataReady, 1, 0) {
				// recvLoop may block while bucket is 0, in this case,
				// session should not be closed.
				if atomic.LoadInt32(&s.bucket) > 0 {
					s.Close()
					return
				}
			}
		case <-s.die:
			return
		}
	}
}

// shaper shapes the sending sequence among streams: control frames first,
// then the streams' data by bytes (fairQueue)
func (s *Session) shaperLoop() {
	var reqs shaperHeap
	var fq fairQueue
	var next writeRequest
	var chWrite chan writeRequest
	var chShaper chan writeRequest

	for {
		// chWrite is not available until it has packet to send
		if len(reqs) > 0 {
			chWrite = s.writes
			next = heap.Pop(&reqs).(writeRequest)
		} else {
			chWrite = nil
		}

		// control heap size, chShaper is not available until packets are less than maximum allowed
		if len(reqs) >= maxShaperSize {
			chShaper = nil
		} else {
			chShaper = s.shaper
		}

		// assertion on non nil
		if chShaper == nil && chWrite == nil {
			panic("both channel are nil")
		}

		select {
		case <-s.die:
			return
		case r := <-chShaper:
			if chWrite != nil { // next is valid, reshape
				heap.Push(&reqs, next)
			}
			if r.class == CLSDATA {
				fq.tag(&r)
			}
			heap.Push(&reqs, r)
		case chWrite <- next:
			fq.sent(&next)
		}
	}
}

func (s *Session) sendLoop() {
	var buf []byte
	var n int
	var err error
	var vec [][]byte // vector for writeBuffers

	bw, ok := s.conn.(interface {
		WriteBuffers(v [][]byte) (n int, err error)
	})

	if ok {
		buf = make([]byte, headerSize)
		vec = make([][]byte, 2)
	} else {
		buf = make([]byte, (1<<16)+headerSize)
	}

	for {
		select {
		case <-s.die:
			return
		case request := <-s.writes:
			buf[0] = request.frame.ver
			buf[1] = request.frame.cmd
			binary.LittleEndian.PutUint16(buf[2:], uint16(len(request.frame.data)))
			binary.LittleEndian.PutUint32(buf[4:], request.frame.sid)

			data := request.take()
			if len(vec) > 0 {
				vec[0] = buf[:headerSize]
				vec[1] = data
				n, err = bw.WriteBuffers(vec)
				request.done()
			} else {
				copy(buf[headerSize:], data)
				request.done()
				n, err = s.conn.Write(buf[:headerSize+len(data)])
			}

			n -= headerSize
			if n < 0 {
				n = 0
			}

			result := writeResult{
				n:   n,
				err: err,
			}

			request.result <- result
			close(request.result)

			// store conn error
			if err != nil {
				s.notifyWriteError(err)
				return
			}
		}
	}
}

// writeFrame writes the frame to the underlying connection
// and returns the number of bytes written if successful
func (s *Session) writeFrame(f Frame) (n int, err error) {
	return s.writeFrameInternal(f, time.After(openCloseTimeout), CLSCTRL)
}

// internal writeFrame version to support deadline used in keepalive
func (s *Session) writeFrameInternal(f Frame, deadline <-chan time.Time, class CLASSID) (int, error) {
	req, err := s.queueFrame(f, deadline, class)
	if err != nil {
		return 0, err
	}
	return s.frameWritten(req, deadline)
}

// queueFrame hands f to the shaper. Control frames leave in the order they
// were queued, and so do the data frames of one stream.
func (s *Session) queueFrame(f Frame, deadline <-chan time.Time, class CLASSID) (writeRequest, error) {
	req := writeRequest{
		class:  class,
		frame:  f,
		seq:    atomic.AddUint32(&s.requestID, 1),
		result: make(chan writeResult, 1),
	}
	if len(f.data) > 0 {
		req.data = &frameData{b: f.data}
	}
	select {
	case s.shaper <- req:
		return req, nil
	case <-s.die:
		return req, io.ErrClosedPipe
	case <-s.chSocketWriteError:
		return req, s.socketWriteError.Load().(error)
	case <-deadline:
		return req, ErrTimeout
	}
}

// frameWritten waits until the frame of req, queued by queueFrame, is
// written. If it stops waiting before, the frame's data is no longer the
// caller's to read (frameData).
func (s *Session) frameWritten(req writeRequest, deadline <-chan time.Time) (int, error) {
	var err error
	select {
	case result := <-req.result:
		return result.n, result.err
	case <-s.die:
		err = io.ErrClosedPipe
	case <-s.chSocketWriteError:
		err = s.socketWriteError.Load().(error)
	case <-deadline:
		err = ErrTimeout
	}
	req.giveUp()
	return 0, err
}
