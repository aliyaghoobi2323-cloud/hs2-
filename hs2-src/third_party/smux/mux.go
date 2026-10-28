// Package smux is a multiplexing library for Golang.
//
// It relies on an underlying connection to provide reliability and ordering, such as TCP or KCP,
// and provides stream-oriented multiplexing over a single channel.
package smux

import (
	"errors"
	"fmt"
	"io"
	"math"
	"time"
)

// Config is used to tune the Smux session
type Config struct {
	// SMUX Protocol version, support 1,2
	Version int

	// Disabled keepalive
	KeepAliveDisabled bool

	// KeepAliveInterval is how often to send a NOP command to the remote
	KeepAliveInterval time.Duration

	// KeepAliveTimeout is how long the session
	// will be closed if no data has arrived
	KeepAliveTimeout time.Duration

	// MaxFrameSize is used to control the maximum
	// frame size to sent to the remote
	MaxFrameSize int

	// MaxReceiveBuffer is used to control the maximum
	// number of data in the buffer pool
	MaxReceiveBuffer int

	// MaxStreamBuffer is used to control the maximum
	// number of data per stream
	MaxStreamBuffer int

	// MinStreamBuffer turns on the adaptive per-stream receive window
	// (protocol version 2) when positive; the wire format does not change,
	// a peer of any version simply honours the window it is told.
	//
	// An adaptive stream starts at the 256 KiB window a peer assumes before
	// any update (clamped to [MinStreamBuffer, MaxStreamBuffer]) and
	// resizes it at each window update from the least backlog its reader
	// left unread since the previous one: over StreamLagTarget it shrinks
	// by the excess, down to MinStreamBuffer; under a quarter of the target
	// it doubles, up to MaxStreamBuffer (NeedStreamBuffer, when set),
	// unless the session ran out of MaxReceiveBuffer meanwhile. At the first
	// resize a reader that kept up
	// gets at once what it reads in a second at its pace so far (from the
	// transfer's own start, past a handshake's wait), if that is more
	// (MaxStreamBuffer for a transfer that starts fast), so a large transfer
	// starts as fast as with the fixed window. A reader slower than
	// its share of the link then holds about StreamLagTarget of the
	// session's MaxReceiveBuffer instead of up to MaxStreamBuffer, while one
	// that keeps up still gets the full window.
	//
	// What a window already granted can only be taken back early: while a
	// stream's backlog stays over StreamLagTarget (for 2 ms or more), the
	// other streams hold a third of MaxReceiveBuffer, its reader at its
	// pace since then would take a second or more to read it, and the peer
	// may still send over twice StreamLagTarget more, the stream is braked:
	// an update leaves at once, without waiting for the reader, that halves
	// the window (to no less than what the stream holds), and while it
	// still lags, each frame after that update went out halves it again. A
	// reader that kept up and then slowed down is cut back within a few
	// frames; one that only paused, and reads all it holds within a second
	// of the first brake, gets its window back at once. The cost that
	// remains: what the peer had already sent before those updates arrive
	// still lands in the session's buffer, so this keeps the buffer from
	// emptying only while about a round trip of the link's data fits in it.
	// Zero (the default) keeps the fixed window of MaxStreamBuffer.
	MinStreamBuffer int

	// StreamLagTarget is the unread backlog, in bytes, an adaptive stream
	// aims for (see MinStreamBuffer). Must be positive when MinStreamBuffer
	// is, zero otherwise.
	StreamLagTarget int

	// NeedStreamBuffer, when positive, is as far as an adaptive stream's
	// window grows on its reader keeping up alone. Past it, up to
	// MaxStreamBuffer, a window grows only as far as the ceiling the
	// session's owner set for the stream allows (Stream.SetCoreCeil, the
	// window core: 0, the default, keeps it within NeedStreamBuffer), and,
	// with a WindowPool, only by what the pool has left. Zero (the default):
	// adaptive windows grow to MaxStreamBuffer. Requires MinStreamBuffer.
	NeedStreamBuffer int

	// WindowPool, with NeedStreamBuffer, is what windows past
	// NeedStreamBuffer take their excess from: a window grows only as far
	// as the pool has left, and the session's MaxReceiveBuffer grows by
	// what its streams hold from the pool, until their windows are down
	// again and what came in above them has been read (a stream idle for
	// a few seconds takes its window down with an update). A session whose
	// buffer runs full borrows from it too, whatever its windows, up to
	// twice MaxReceiveBuffer more than its windows hold, until that is free
	// again. The sessions sharing one pool so hold at most its size past
	// their MaxReceiveBuffer in all, and the other streams of a session keep
	// at least the room they would have with every window within
	// NeedStreamBuffer. Nil: the excess is bounded by MaxStreamBuffer alone,
	// in a session buffer of MaxReceiveBuffer.
	WindowPool *WindowPool

	// Sched, when set, gives each session a frame scheduler of its own
	// (hs2's link scheduler, C2) in place of the shaper: every frame goes to
	// it, control frames in LaneCtrl, and the session's writer takes them
	// one at a time (sched.go). Nil (the default): the shaper's heap.
	Sched func() FrameSched

	// FlowInfo, with Sched: the user and the lane of the stream's next data
	// frame, asked as its writer cuts it. Nil: the user tag of the stream's
	// SYN, in LaneBulk.
	FlowInfo func(st *Stream) (tag uint32, lane uint8)

	// BulkFrame, with Sched: the most a bulk data frame carries on a link
	// that wrote rate bytes/s over the last second (within MaxFrameSize).
	// Nil: MaxFrameSize.
	BulkFrame func(rate float64) int

	// LaneFrame, with Sched: the most a latency-lane data frame carries
	// (within MaxFrameSize). 0: as a bulk one.
	LaneFrame int

	// UserFrames, with Sched: the most data frames of one user's streams
	// queued at once; the writer of one more waits in that user's FIFO,
	// the writers of other users do not. 0: no bound.
	UserFrames int

	// EarlyDone, with Sched: a stream's writer may queue its next data
	// frame as soon as the session's writer has copied the one before (its
	// place among the session's frames is then fixed), not once that one
	// is written: a stream has one frame queued while one is written, so a
	// user of one stream still has a frame queued each time the writer
	// takes the next (P10 (b)). False: the writer waits for the write.
	EarlyDone bool

	// UserMem, with Sched: the account of a user's queued bytes, asked once
	// per user as its first frame is queued. The session calls it with the
	// bytes of each data frame it queues (n > 0) and takes (n < 0): for a
	// frame it may refuse (false; force: the user has nothing else queued in
	// the session, it must take it), and the frame's writer waits as for
	// UserFrames. Nil: no account.
	UserMem func(tag uint32) func(n int, force bool) bool

	// BulkGate, with Sched: the session's writer asks it before it writes a
	// bulk-lane data frame of size bytes (hs2's in-flight budget of a BULK
	// link, R1 round 3 γ2): 0 or less, write it now; more, ask again within
	// that long. While a bulk frame waits so, the writer still writes the
	// control and latency lanes' frames (a FrameSched that is an
	// UrgentSched) as they come, and the frame keeps its place: no other
	// bulk frame goes before it. Nil: bulk frames go as they are taken.
	BulkGate func(size int) time.Duration

	// FlushEach, with Sched (lab, the γ2 batching's measure): the writer
	// flushes a conn that holds back a tail (hs2's shaper) after each frame
	// that is not bulk, even with more frames queued: no record carries two
	// of them. False (the default): it flushes once it runs out of frames,
	// so a record takes everything queued by then (no wait for more).
	FlushEach bool

	// Coalesce, with Sched and a conn that holds back a short tail
	// (hs2's shaper, which pads a short record to its sampled size): after
	// a frame that is not bulk the tail waits up to this long, from the
	// first such frame, for more to share its record before it goes out
	// padded, while the session wrote at least CoalesceMin such frames over
	// the last second (fewer: each padded record costs little, and the
	// wait would be all cost). A frame of a stream marked SetUrgent ends
	// the wait. 0 (the default): the tail goes out as soon as the writer
	// runs dry, as before. (R1 round 3: the window of candidate gamma1,
	// merged by the judge.)
	Coalesce    time.Duration
	CoalesceMin float64
	// CoalesceJitter: each wait is Coalesce times a uniform draw in [0.5,
	// 1.5), not Coalesce itself: a fixed hold left the QUIET link's packets
	// 4.26 ms apart at the median on the rig (gamma1's capture), a timing
	// mark of its own.
	CoalesceJitter bool
}

// DefaultConfig is used to return a default configuration
func DefaultConfig() *Config {
	return &Config{
		Version:           1,
		KeepAliveInterval: 10 * time.Second,
		KeepAliveTimeout:  30 * time.Second,
		MaxFrameSize:      32768,
		MaxReceiveBuffer:  4194304,
		MaxStreamBuffer:   65536,
	}
}

// VerifyConfig is used to verify the sanity of configuration
func VerifyConfig(config *Config) error {
	if !(config.Version == 1 || config.Version == 2) {
		return errors.New("unsupported protocol version")
	}
	if !config.KeepAliveDisabled {
		if config.KeepAliveInterval == 0 {
			return errors.New("keep-alive interval must be positive")
		}
		if config.KeepAliveTimeout < config.KeepAliveInterval {
			return fmt.Errorf("keep-alive timeout must be larger than keep-alive interval")
		}
	}
	if config.MaxFrameSize <= 0 {
		return errors.New("max frame size must be positive")
	}
	if config.MaxFrameSize > 65535 {
		return errors.New("max frame size must not be larger than 65535")
	}
	if config.MaxReceiveBuffer <= 0 {
		return errors.New("max receive buffer must be positive")
	}
	if config.MaxStreamBuffer <= 0 {
		return errors.New("max stream buffer must be positive")
	}
	if config.MaxStreamBuffer > config.MaxReceiveBuffer && (config.WindowPool == nil ||
		config.NeedStreamBuffer > config.MaxReceiveBuffer) { // past NeedStreamBuffer a window brings its buffer from the pool
		return errors.New("max stream buffer must not be larger than max receive buffer")
	}
	if config.MaxStreamBuffer > math.MaxInt32 {
		return errors.New("max stream buffer cannot be larger than 2147483647")
	}
	if config.MinStreamBuffer < 0 || config.StreamLagTarget < 0 {
		return errors.New("min stream buffer and stream lag target must not be negative")
	}
	if config.MinStreamBuffer > 0 {
		if config.Version != 2 {
			return errors.New("adaptive stream window requires protocol version 2")
		}
		if config.MinStreamBuffer < 2*config.MaxFrameSize {
			return errors.New("min stream buffer must be at least twice the max frame size")
		}
		if config.MinStreamBuffer > config.MaxStreamBuffer {
			return errors.New("min stream buffer must not be larger than max stream buffer")
		}
		if config.StreamLagTarget == 0 {
			return errors.New("stream lag target must be positive when min stream buffer is set")
		}
	} else if config.StreamLagTarget != 0 {
		return errors.New("stream lag target requires min stream buffer")
	}
	if config.NeedStreamBuffer < 0 {
		return errors.New("need stream buffer must not be negative")
	}
	if config.NeedStreamBuffer > 0 {
		if config.MinStreamBuffer == 0 {
			return errors.New("need stream buffer requires the adaptive stream window")
		}
		if config.NeedStreamBuffer < config.MinStreamBuffer || config.NeedStreamBuffer > config.MaxStreamBuffer {
			return errors.New("need stream buffer must be within [min stream buffer, max stream buffer]")
		}
	} else if config.WindowPool != nil {
		return errors.New("window pool requires need stream buffer")
	}
	if config.LaneFrame < 0 || config.UserFrames < 0 {
		return errors.New("lane frame and user frames must not be negative")
	}
	if config.Sched == nil && (config.FlowInfo != nil || config.BulkFrame != nil || config.LaneFrame != 0 ||
		config.UserFrames != 0 || config.UserMem != nil || config.BulkGate != nil || config.FlushEach || config.Coalesce != 0) {
		return errors.New("flow info, bulk frame, lane frame, user frames, user mem, bulk gate, flush each and coalesce require a frame scheduler")
	}
	if config.Coalesce < 0 || config.CoalesceMin < 0 {
		return errors.New("coalesce and coalesce min must not be negative")
	}
	return nil
}

// Server is used to initialize a new server-side connection.
func Server(conn io.ReadWriteCloser, config *Config) (*Session, error) {
	if config == nil {
		config = DefaultConfig()
	}
	if err := VerifyConfig(config); err != nil {
		return nil, err
	}
	return newSession(config, conn, false), nil
}

// Client is used to initialize a new client-side connection.
func Client(conn io.ReadWriteCloser, config *Config) (*Session, error) {
	if config == nil {
		config = DefaultConfig()
	}

	if err := VerifyConfig(config); err != nil {
		return nil, err
	}
	return newSession(config, conn, true), nil
}
