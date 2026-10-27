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
	// sf: the frame in the session's scheduler (Config.Sched; nil: none)
	sf *schedFrame
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
	rcvd         atomic.Uint64 // core: data bytes received
	snapLock     sync.Mutex    // core: one CoreSnapshot at a time
	snapList     []*Stream     // core: CoreSnapshot's copy of the streams (under snapLock)
	pwLock       sync.Mutex    // core: one PeerWindows at a time

	streams    map[uint32]*Stream // all streams in this session
	streamLock sync.Mutex         // locks streams
	badSyn     atomic.Uint64      // SYNs refused for their payload (BadSyn)

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

	// sch: the frame scheduler (Config.Sched) in place of the shaper and
	// its channels (nil: none); a pair's QUIET writer's
	sch *schedState

	// a pair of connections (pair.go): BULK's connection, its writer's
	// scheduler and its reader's data flag (nil, nil: one connection)
	bconn      io.ReadWriteCloser
	schB       *schedState
	dataReadyB int32
	client     bool
	// BULK data of the peer's streams whose SYN has not come yet, the bytes
	// held so, and the latest stream id the peer opened (under streamLock)
	early   map[uint32]*earlyBuf
	earlyN  int
	peerSid uint32
}

func newSession(config *Config, conn io.ReadWriteCloser, client bool) *Session {
	return newSessionPair(config, conn, nil, client)
}

// newSessionPair is newSession over conn and, when bulk is not nil, over the
// pair's BULK connection too (pair.go; config has a scheduler).
func newSessionPair(config *Config, conn, bulk io.ReadWriteCloser, client bool) *Session {
	s := new(Session)
	s.die = make(chan struct{})
	s.conn = conn
	s.bconn = bulk
	s.client = client
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

	if config.Sched != nil {
		s.sch = newSchedState(config)
		go s.sendLoopSched(s.sch, s.conn)
		if bulk != nil {
			s.schB = newSchedState(config)
			s.schB.bulkPath = true
			go s.sendLoopSched(s.schB, bulk)
			go s.recvLoop(bulk, true)
		}
	} else {
		go s.shaperLoop()
		go s.sendLoop()
	}
	go s.recvLoop(s.conn, false)
	if !config.KeepAliveDisabled {
		go s.keepalive()
	}
	return s
}

// OpenStream is used to create a new stream. Its SYN carries user tag 0 and
// no class hint, as the control streams have (OpenStreamSyn).
func (s *Session) OpenStream() (*Stream, error) {
	return s.OpenStreamSyn(EncodeSyn(0, 0))
}

// OpenStreamSyn creates a new stream whose SYN carries p (EncodeSyn).
func (s *Session) OpenStreamSyn(p [SynPayloadLen]byte) (*Stream, error) {
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
	// a pair's SYNs are queued in stream id order (pair.go): the lock is
	// held until this one is
	pair := s.bconn != nil
	if !pair {
		s.nextStreamIDLock.Unlock()
	}

	stream := newStream(sid, s.config.MaxFrameSize, s)
	stream.syn = p

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
		if pair {
			s.nextStreamIDLock.Unlock()
		}
		return nil, err
	}

	f := newFrame(byte(s.config.Version), cmdSYN, sid)
	f.data = stream.syn[:]
	if pair {
		deadline := time.After(openCloseTimeout)
		req, err := s.queueFrame(f, deadline, CLSCTRL)
		s.nextStreamIDLock.Unlock()
		if err == nil {
			_, err = s.frameWritten(req, deadline)
		}
		if err != nil {
			s.streamClosed(sid)
			return nil, err
		}
		return stream, nil
	}
	if _, err := s.writeFrame(f); err != nil {
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
		s.dropEarly()
		s.streamLock.Unlock()
		if s.config.WindowPool != nil {
			s.returnSpare() // as far as it is free
			if atomic.LoadInt32(&s.extra) > 0 {
				// what the streams still hold goes back as they let it go,
				// and the rest closeGrace from now (pool.go)
				time.AfterFunc(closeGrace, s.dropClaims)
			}
		}
		if s.bconn != nil {
			s.bconn.Close() // a pair closes together
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

// badSyns counts the SYNs every session of the process refused (BadSyns).
var badSyns atomic.Uint64

// BadSyn returns how many SYNs the session refused: without the payload,
// with one of another length, or with a role and no group (SynPayloadLen).
func (s *Session) BadSyn() uint64 { return s.badSyn.Load() }

// BadSyns is BadSyn summed over every session the process has had.
func BadSyns() uint64 { return badSyns.Load() }

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

// recvLoop keeps on reading from underlying connection if tokens are
// available: conn, the session's (QUIET of a pair), or the pair's BULK
// (bulk).
func (s *Session) recvLoop(conn io.Reader, bulk bool) {
	var hdr rawHeader
	var updHdr updHeader
	var ext [szCmdMOV]byte
	pair := s.bconn != nil
	ready := &s.dataReady
	if bulk {
		ready = &s.dataReadyB
	}
	// a pair's QUIET reader waits for buffer room only past MaxReceiveBuffer
	// more than BULK's (pair.go)
	low := int32(0)
	if pair && !bulk {
		low = -int32(s.config.MaxReceiveBuffer)
	}

	for {
		for atomic.LoadInt32(&s.bucket) <= low && !s.IsClosed() {
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
		if _, err := io.ReadFull(conn, hdr[:]); err == nil {
			atomic.StoreInt32(ready, 1)
			if hdr.Version() != byte(s.config.Version) {
				s.notifyProtoError(ErrInvalidProtocol)
				return
			}
			sid := hdr.StreamID()
			switch hdr.Cmd() {
			case cmdNOP:
			case cmdSYN:
				if bulk { // a pair's control frames are QUIET's
					s.notifyProtoError(ErrInvalidProtocol)
					return
				}
				var syn [SynPayloadLen]byte
				var rerr error
				good := hdr.Length() == SynPayloadLen
				if good {
					_, rerr = io.ReadFull(conn, syn[:])
				} else if hdr.Length() > 0 {
					_, rerr = io.CopyN(io.Discard, conn, int64(hdr.Length()))
				}
				if rerr != nil {
					s.notifyReadError(rerr)
					return
				}
				s.streamLock.Lock()
				_, known := s.streams[sid]
				if pair && s.peerOpened(sid) && int32(sid-s.peerSid) > 0 {
					s.peerSid = sid
				}
				if good && synValid(syn) {
					if !known {
						stream := newStream(sid, s.config.MaxFrameSize, s)
						stream.syn = syn
						s.streams[sid] = stream
						if pair {
							s.adoptEarly(stream)
						}
						select {
						case s.chAccepts <- stream:
						case <-s.die:
						}
					}
				} else {
					// No v3 payload: no stream. The opener gets a FIN for it
					// (sent aside, so the reader never waits on the writer:
					// the FINs of several refusals leave in no set order).
					s.badSyn.Add(1)
					badSyns.Add(1)
					if pair {
						s.dropEarlyOf(sid)
					}
					if !known {
						go s.writeFrame(s.finFrame(sid, 0))
					}
				}
				s.streamLock.Unlock()
			case cmdFIN:
				var off uint32
				if pair {
					// a pair's FIN carries the count of data bytes sent
					if bulk || hdr.Length() != szFinOff {
						s.notifyProtoError(ErrInvalidProtocol)
						return
					}
					if _, err := io.ReadFull(conn, ext[:szFinOff]); err != nil {
						s.notifyReadError(err)
						return
					}
					off = binary.LittleEndian.Uint32(ext[:])
				}
				s.streamLock.Lock()
				if stream, ok := s.streams[sid]; ok {
					if pair {
						stream.finAt(off)
					} else {
						stream.fin()
					}
					stream.notifyReadEvent()
				}
				s.streamLock.Unlock()
			case cmdPSH:
				if hdr.Length() > 0 {
					newbuf := defaultAllocator.Get(int(hdr.Length()))
					if written, err := io.ReadFull(conn, newbuf); err == nil {
						s.rcvd.Add(uint64(written))
						s.streamLock.Lock()
						if stream, ok := s.streams[sid]; ok {
							n := written
							if pair {
								// into the stream in its order, or held
								// until its MOV (pair.go)
								var perr error
								if n, perr = stream.rxPair(newbuf, bulk); perr != nil {
									s.streamLock.Unlock()
									s.notifyProtoError(perr)
									return
								}
							} else {
								stream.pushBytes(newbuf)
							}
							if n > 0 {
								free := atomic.AddInt32(&s.bucket, -int32(n))
								stream.notifyReadEvent()
								if s.config.MinStreamBuffer > 0 {
									stream.checkLag(int(free))
								}
							}
						} else if pair && bulk && s.peerOpened(sid) && int32(sid-s.peerSid) > 0 {
							// its SYN is still on its way on QUIET
							if perr := s.holdEarly(sid, newbuf); perr != nil {
								s.streamLock.Unlock()
								s.notifyProtoError(perr)
								return
							}
						}
						s.streamLock.Unlock()
					} else {
						s.notifyReadError(err)
						return
					}
				}
			case cmdUPD:
				if _, err := io.ReadFull(conn, updHdr[:]); err == nil {
					s.streamLock.Lock()
					if stream, ok := s.streams[sid]; ok {
						stream.update(updHdr.Consumed(), updHdr.Window())
					}
					s.streamLock.Unlock()
				} else {
					s.notifyReadError(err)
					return
				}
			case cmdMOV:
				if !pair || bulk || hdr.Length() != szCmdMOV {
					s.notifyProtoError(ErrInvalidProtocol)
					return
				}
				if _, err := io.ReadFull(conn, ext[:]); err != nil {
					s.notifyReadError(err)
					return
				}
				s.streamLock.Lock()
				if stream, ok := s.streams[sid]; ok {
					n, perr := stream.mov(ext[0], binary.LittleEndian.Uint32(ext[1:]))
					if perr != nil {
						s.streamLock.Unlock()
						s.notifyProtoError(perr)
						return
					}
					if n > 0 {
						free := atomic.AddInt32(&s.bucket, -int32(n))
						stream.notifyReadEvent()
						if s.config.MinStreamBuffer > 0 {
							stream.checkLag(int(free))
						}
					}
				}
				s.streamLock.Unlock()
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
			if s.bconn != nil {
				// each connection of a pair is kept alive, and watched, on
				// its own (pair.go)
				nop := newFrame(byte(s.config.Version), cmdNOP, 0)
				rq, eq := s.queueSched(nop, CLSCTRL, 0, LaneCtrl, false)
				rb, eb := s.queueSched(nop, CLSCTRL, 0, LaneCtrl, true)
				if eq == nil {
					s.frameWritten(rq, tickerPing.C)
				}
				if eb == nil {
					s.frameWritten(rb, tickerPing.C)
				}
			} else {
				s.writeFrameInternal(newFrame(byte(s.config.Version), cmdNOP, 0), tickerPing.C, CLSCTRL)
			}
			s.notifyBucket() // force a signal to the recvLoop
			if s.config.WindowPool != nil {
				s.sweepClaims()
			}
		case <-tickerTimeout.C:
			quiet := !atomic.CompareAndSwapInt32(&s.dataReady, 1, 0)
			bulkQuiet := s.bconn != nil && !atomic.CompareAndSwapInt32(&s.dataReadyB, 1, 0)
			// recvLoop may block while bucket is 0 (a pair's QUIET reader:
			// MaxReceiveBuffer past it), in this case, session should not be
			// closed.
			low := int32(0)
			if s.bconn != nil {
				low = -int32(s.config.MaxReceiveBuffer)
			}
			if b := atomic.LoadInt32(&s.bucket); quiet && b > low || bulkQuiet && b > 0 {
				s.Close()
				return
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
	if s.sch != nil {
		// (a data frame comes here only from a writer with no flow: user 0,
		// bulk; a pair's control frames go on QUIET)
		return s.queueSched(f, class, 0, LaneBulk, false)
	}
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
	if req.sf != nil {
		req.sf.sc.cancel(req.sf) // out of its user's FIFO if it still waits there
	}
	req.giveUp()
	return 0, err
}
