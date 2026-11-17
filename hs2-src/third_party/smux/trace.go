package smux

import (
	"sync"
	"sync/atomic"
	"time"
)

// hs2 dl76: an opt-in per-stream trace (diagnostics only; nothing on the wire
// changes). EnableTrace once at start; Stream.Trace marks a stream; TraceRows
// reports, per marked stream, what it did since the last call: bytes written
// and how long its writers waited for the peer's window (window-bound) or
// inside the session's frame writer (socket/shaper-bound), bytes received and
// the receive window and unread backlog. Off: one atomic load per Trace call,
// nothing else.

var traceOn atomic.Bool

var traced sync.Map // *Stream -> struct{}

// EnableTrace turns Stream.Trace on for the process.
func EnableTrace() { traceOn.Store(true) }

type streamTrace struct {
	winWaitNs, sockNs, wrBytes, rdBytes atomic.Int64
	// the previous TraceRows values
	pWin, pSock, pWr, pRd int64
}

// Trace marks s for TraceRows (no-op unless EnableTrace).
func (s *Stream) Trace() {
	if !traceOn.Load() || s.tr != nil {
		return
	}
	s.tr = new(streamTrace)
	traced.Store(s, struct{}{})
}

// TraceRow is one traced stream's interval.
type TraceRow struct {
	ID                uint32
	Wr, Rd            int64         // bytes written / received in the interval
	WinWait, Sock     time.Duration // writers waiting for window / in the frame writer
	PeerWin, Inflight int64         // the peer's window and our unacknowledged bytes now
	RecvWin, Buffered int64         // our advertised window and unread bytes now
	Auto, AutoOff     bool          // adaptive window present / switched off
}

// TraceRows returns each traced stream's interval since the last call and
// forgets streams that have closed.
func TraceRows() []TraceRow {
	var out []TraceRow
	traced.Range(func(k, _ any) bool {
		s := k.(*Stream)
		t := s.tr
		w, so, wr, rd := t.winWaitNs.Load(), t.sockNs.Load(), t.wrBytes.Load(), t.rdBytes.Load()
		r := TraceRow{ID: s.id, Wr: wr - t.pWr, Rd: rd - t.pRd, WinWait: time.Duration(w - t.pWin), Sock: time.Duration(so - t.pSock)}
		t.pWin, t.pSock, t.pWr, t.pRd = w, so, wr, rd
		r.PeerWin = int64(atomic.LoadUint32(&s.peerWindow))
		r.Inflight = int64(int32(atomic.LoadUint32(&s.numWritten) - atomic.LoadUint32(&s.peerConsumed)))
		s.bufferLock.Lock()
		for _, b := range s.buffers {
			r.Buffered += int64(len(b))
		}
		r.RecvWin = int64(s.sess.config.MaxStreamBuffer)
		if s.aw != nil {
			r.Auto, r.AutoOff = true, s.aw.off
			if s.aw.off {
				if s.aw.offWin > 0 {
					r.RecvWin = int64(s.aw.offWin)
				}
			} else {
				r.RecvWin = int64(s.aw.win)
			}
		}
		s.bufferLock.Unlock()
		out = append(out, r)
		select {
		case <-s.die:
			traced.Delete(s)
		default:
		}
		return true
	})
	return out
}
