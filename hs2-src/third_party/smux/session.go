package smux

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultAcceptBacklog = 1024
	maxShaperSize        = 1024
	openCloseTimeout     = 30 * time.Second // stream open/close timeout
	abandonCloseTimeout  = 3 * time.Second  // hs2 fx247: Close's frames after a CloseWrite timed out
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
	jump   uint32 // hs2 SmallFirst: the request ranks as if queued jump requests earlier
	result chan writeResult
	own    *reqOwn // hs2 rc134
}

// reqOwn (rc134): who may read a queued request's frame.data, and the
// writer's copy once it gave up.
type reqOwn struct {
	st   atomic.Uint32 // reqPending/reqTaken/reqCopying/reqDetached/reqReleased
	data []byte        // reqDetached: the copy sendLoop sends instead
}

// hs2 rc134: who owns a queued request's frame.data. Upstream writeFrame
// returned on its deadline (or the session's end) while the request still sat
// in the shaper or s.writes, and sendLoop copied frame.data later: the caller
// had already reused the buffer (hs2 returns its pooled chunk on the error),
// so the frame carried another record's bytes (race detector: getChunkN vs
// sendLoop's slicecopy). The request still goes out later as before (the
// control loop's stuck measure counts on the queued pings, control.go), but
// with a copy the writer makes before it returns; a request sendLoop already
// took is waited for until sendLoop is done reading its data (the copy into
// its buffer, a few us; with WriteBuffers, the socket write).
const (
	reqPending uint32 = iota
	reqTaken
	reqCopying
	reqDetached
	reqReleased
)

// take (sendLoop): the data to send for req, now sendLoop's to read.
func (o *reqOwn) take(data []byte) []byte {
	for {
		switch o.st.Load() {
		case reqPending:
			if o.st.CompareAndSwap(reqPending, reqTaken) {
				return data
			}
		case reqDetached:
			o.st.Store(reqTaken)
			return o.data
		default: // reqCopying: the writer is copying it
			runtime.Gosched()
		}
	}
}

// reqCtx (hs2 gc295): a request's result channel and owner state in one
// pooled object. writeFrameQueued made both per frame (the top allocation
// site by objects in the gc295 storm profiles, ~7 M/min on a busy exit). The
// channel is no longer closed after its one result, so a context whose result
// the writer received goes back to reqCtxPool; one the writer gave up on
// (giveUp: sendLoop may still send into it) is left to the GC.
type reqCtx struct {
	own    reqOwn
	result chan writeResult
}

var reqCtxPool = sync.Pool{New: func() any { return &reqCtx{result: make(chan writeResult, 1)} }}

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

	streams    map[uint32]*Stream // all streams in this session
	streamLock sync.Mutex         // locks streams

	die     chan struct{} // flag session has died
	dieOnce sync.Once
	// hs2 b307: set (inside dieOnce, just before die closes) so IsClosed is
	// one atomic load instead of a non-blocking channel receive; the LB pick
	// loops call it for every link several times per pick (sc307 scale test)
	dead atomic.Bool

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
	writesB   chan *[]writeRequest // Config.SendBatch (sendbatch.go)

	aw sessAW // hs2: loop-RTT minimum for Config.AutoWindow

	// hs2 mem67 (stall.go): unix ns when the current conn write started (0:
	// idle), when recvLoop began waiting on an empty bucket (0: not waiting),
	// and when a stream last returned tokens
	writeBusySince, recvWaitSince, lastTokens atomic.Int64
}

func newSession(config *Config, conn io.ReadWriteCloser, client bool) *Session {
	s := new(Session)
	TrackObject(s, &objSessions)
	regSession(s)
	s.die = make(chan struct{})
	s.conn = conn
	s.config = config
	s.streams = make(map[uint32]*Stream)
	liveSessions.Add(1)
	s.chAccepts = make(chan *Stream, defaultAcceptBacklog)
	s.bucket = int32(config.MaxReceiveBuffer)
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

	if config.SendBatch > 0 { // hs2 lk179 (sendbatch.go)
		s.writesB = make(chan *[]writeRequest)
		go s.shaperLoopBatch()
		go s.sendLoopBatch()
	} else {
		go s.shaperLoop()
		go s.sendLoop()
	}
	go s.recvLoop()
	if !config.KeepAliveDisabled {
		go s.keepalive()
	}
	if config.StallTimeout > 0 {
		go s.stallWatch()
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

	if _, err := s.writeFrame(newFrame(byte(s.config.Version), cmdSYN, sid)); err != nil {
		return nil, err
	}

	s.streamLock.Lock()
	defer s.streamLock.Unlock()
	select {
	case <-s.chSocketReadError:
		return nil, s.socketReadError.Load().(error)
	case <-s.chSocketWriteError:
		return nil, s.socketWriteError.Load().(error)
	case <-s.die:
		return nil, io.ErrClosedPipe
	default:
		s.streams[sid] = stream
		liveStreams.Add(1)
		return stream, nil
	}
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
		s.dead.Store(true)
		close(s.die)
		once = true
	})

	if once {
		s.streamLock.Lock()
		for k := range s.streams {
			s.streams[k].sessionClose()
			s.streams[k].awRelease()
		}
		liveStreams.Add(-int64(len(s.streams)))
		liveSessions.Add(-1)
		// hs2 mem66: a stream still held (an mConn keeps its dead first
		// sub) held this session and, through the map, every stream open
		// when it died with their unread buffers; the map goes now
		// (streamClosed after this finds nothing to remove)
		clear(s.streams)
		s.streamLock.Unlock()
		return s.conn.Close()
	} else {
		return io.ErrClosedPipe
	}
}

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
	// fx170: the carrier's EOF (a FIN or close_notify on the link) ends
	// every stream mid-way; a stream's Read reported it as io.EOF, the same
	// as the peer's FIN on that stream, so a relay took a cut stream as a
	// complete one (a truncated download handed to the user with a clean
	// FIN). Only cmdFIN is a stream's EOF.
	if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
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
	return s.dead.Load()
}

// DeadFlag (hs2 b325) is the session's closed flag itself (true once it is
// closed), so a caller scanning many sessions can read it without the method
// call. Read-only for the caller.
func (s *Session) DeadFlag() *atomic.Bool { return &s.dead }

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
	if s.streams[sid] == nil { // hs2 mem66: the session's Close cleared the map
		s.streamLock.Unlock()
		return
	}
	if n := s.streams[sid].recycleTokens(); n > 0 { // return remaining tokens to the bucket
		if atomic.AddInt32(&s.bucket, int32(n)) > 0 {
			s.notifyBucket()
		}
	}
	s.streams[sid].awRelease()
	if !s.IsClosed() { // hs2 mem66: a closed session's Close counts its map
		if _, ok := s.streams[sid]; ok {
			liveStreams.Add(-1)
		}
		delete(s.streams, sid)
	}
	s.streamLock.Unlock()
}

// returnTokens is called by stream to return token after read
func (s *Session) returnTokens(n int) {
	s.lastTokens.Store(nowNano())
	if atomic.AddInt32(&s.bucket, int32(n)) > 0 {
		s.notifyBucket()
	}
}

// recvLoop keeps on reading from underlying connection if tokens are available
func (s *Session) recvLoop() {
	var hdr rawHeader
	var updHdr updHeader

	for {
		if atomic.LoadInt32(&s.bucket) <= 0 && !s.IsClosed() {
			s.recvWaitSince.Store(nowNano())
			for atomic.LoadInt32(&s.bucket) <= 0 && !s.IsClosed() {
				select {
				case <-s.bucketNotify:
				case <-s.die:
					return
				}
			}
			s.recvWaitSince.Store(0)
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
					liveStreams.Add(1)
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
						s.streamLock.Lock()
						if stream, ok := s.streams[sid]; ok {
							stream.pushBytes(newbuf)
							atomic.AddInt32(&s.bucket, -int32(written))
							stream.notifyReadEvent()
						}
						s.streamLock.Unlock()
					} else {
						s.notifyReadError(err)
						return
					}
				} else {
					// hs2 hc213: an empty push is the peer's "abandoned"
					// mark (Stream.Close without CloseWrite, half-open)
					s.streamLock.Lock()
					if stream, ok := s.streams[sid]; ok {
						stream.abandoned()
					}
					s.streamLock.Unlock()
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

// shaper shapes the sending sequence among streams
func (s *Session) shaperLoop() {
	var reqs shaperHeap
	var next writeRequest
	var chWrite chan writeRequest
	var chShaper chan writeRequest

	for {
		// chWrite is not available until it has packet to send
		if len(reqs) > 0 {
			chWrite = s.writes
			next = reqs.popReq()
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
				reqs.pushReq(next)
			}
			reqs.pushReq(r)
		case chWrite <- next:
			if next.jump > 0 && overtook(reqs, next) {
				smallFirstOvertakes.Add(1)
			}
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
			data := request.own.take(request.frame.data)
			buf[0] = request.frame.ver
			buf[1] = request.frame.cmd
			binary.LittleEndian.PutUint16(buf[2:], uint16(len(data)))
			binary.LittleEndian.PutUint32(buf[4:], request.frame.sid)

			s.writeBusySince.Store(nowNano())
			if len(vec) > 0 {
				vec[0] = buf[:headerSize]
				vec[1] = data
				n, err = bw.WriteBuffers(vec)
				vec[1] = nil
				request.own.st.Store(reqReleased)
			} else {
				copy(buf[headerSize:], data)
				request.own.st.Store(reqReleased) // the data is ours now
				n, err = s.conn.Write(buf[:headerSize+len(data)])
			}

			s.writeBusySince.Store(0)
			n -= headerSize
			if n < 0 {
				n = 0
			}

			result := writeResult{
				n:   n,
				err: err,
			}

			request.result <- result
			// gc295: not closed — the channel goes back to reqCtxPool (writeFrameQueued)

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
	return s.writeFrameCut(f, deadline, nil, class)
}

// writeFrameCut: writeFrameInternal that also gives up when cut closes (hs2
// er94, Stream.SetWriteDeadline into the past).
func (s *Session) writeFrameCut(f Frame, deadline <-chan time.Time, cut <-chan struct{}, class CLASSID) (int, error) {
	return s.writeFrameQueued(f, deadline, cut, class, nil)
}

// writeFrameQueued: writeFrameCut that unlocks queued (if not nil) once the
// request is in the shaper (or was given up on before): requests queued in
// that lock's order leave in that order (same class and jump; aw163).
func (s *Session) writeFrameQueued(f Frame, deadline <-chan time.Time, cut <-chan struct{}, class CLASSID, queued func()) (int, error) {
	if queued != nil {
		defer func() {
			if queued != nil {
				queued()
			}
		}()
	}
	rc := reqCtxPool.Get().(*reqCtx)
	req := writeRequest{
		class:  class,
		frame:  f,
		seq:    atomic.AddUint32(&s.requestID, 1),
		result: rc.result,
		own:    &rc.own,
	}
	if sf := s.config.SmallFirst; sf > 0 && class == CLSDATA && len(f.data) <= sf {
		req.jump = SmallFirstJump
	}
	select {
	case s.shaper <- req:
	case <-s.die:
		return 0, io.ErrClosedPipe
	case <-s.chSocketWriteError:
		return 0, s.socketWriteError.Load().(error)
	case <-deadline:
		return 0, ErrTimeout
	case <-cut:
		return 0, ErrTimeout
	}
	if queued != nil {
		queued()
		queued = nil
	}

	var err error
	select {
	case result := <-req.result:
		// gc295: sendLoop is done with the request (it released the data
		// before it sent the one result): its channel and owner state are
		// free to reuse. A request given up on is never put back.
		rc.own.st.Store(reqPending)
		rc.own.data = nil
		reqCtxPool.Put(rc)
		return result.n, result.err
	case <-s.die:
		err = io.ErrClosedPipe
	case <-s.chSocketWriteError:
		err = s.socketWriteError.Load().(error)
	case <-deadline:
		err = ErrTimeout
	case <-cut:
		err = ErrTimeout
	}
	return s.giveUp(&req, err)
}

// giveUp (rc134): the writer stops waiting for req. A request sendLoop has
// not taken gets its own copy of the data (it still goes out, as upstream);
// a taken one is waited for until sendLoop is done reading the data. Either
// way the caller may reuse its buffer once this returns, and err is reported
// as before.
func (s *Session) giveUp(req *writeRequest, err error) (int, error) {
	o := req.own
	if o.st.CompareAndSwap(reqPending, reqCopying) {
		o.data = append([]byte(nil), req.frame.data...)
		o.st.Store(reqDetached)
		return 0, err
	}
	for i := 0; o.st.Load() == reqTaken; i++ {
		if i < 64 {
			runtime.Gosched()
			continue
		}
		select { // WriteBuffers: the data is the socket write's until it returns
		case r := <-req.result:
			return r.n, r.err
		case <-time.After(time.Millisecond):
		}
	}
	return 0, err
}
