package smux

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"sync/atomic"
	"time"
)

// hs2 mem67: frozen-session defence (Config.StallTimeout; 0 = off, upstream).
//
// The receive side shares one token bucket (MaxReceiveBuffer) among all
// streams. A stream nobody reads keeps its window's worth of bytes in the
// bucket; a few of them empty it, recvLoop stops reading the socket, the
// peer's TCP window closes and EVERY write on the peer's side of this link
// blocks for good, while the reverse direction (and so keepalive) stays
// alive, so nothing ever closes the session.
//
// Two guards, both on progress, never on rate:
//   - recv: recvLoop has waited on an empty bucket and no stream has read a
//     single byte for StallTimeout/2: the streams holding unread bytes that
//     have not read for that long are evicted. Their buffers are dropped
//     (tokens back to the bucket), they are closed locally (reads return
//     ErrStreamEvicted, writes ErrClosedPipe) and a FIN tells the peer. The
//     session and every stream that is read keep running.
//   - send: one write to the underlying conn has not returned for
//     StallTimeout (no room in the socket for that long: the peer stopped
//     reading the link): the session is closed, so every goroutine blocked on
//     it gets an error and the link is replaced.

// ErrStreamEvicted is returned by Read on a stream dropped by the recv guard.
var ErrStreamEvicted = errors.New("smux: stream evicted (unread while the session buffer was full)")

var stallEvicts, stallEvictBytes, stallSendCloses atomic.Int64

// StallCounts returns the streams evicted (and their dropped bytes) and the
// sessions closed for a frozen send path, process-wide.
func StallCounts() (evicts, evictBytes, sendCloses int64) {
	return stallEvicts.Load(), stallEvictBytes.Load(), stallSendCloses.Load()
}

// StallLog, if set before sessions start, receives one line per guard action.
var StallLog func(string)

// StallDump, if set, is called with the session's sendLoop signature
// ("sendLoop(0x...)") before a frozen session is closed (diagnosis).
var StallDump func(sig string)

func stallLogf(format string, args ...any) {
	if f := StallLog; f != nil {
		f(fmt.Sprintf(format, args...))
	}
}

func nowNano() int64 { return time.Now().UnixNano() }

// closedErr is what a read on a locally closed stream returns.
func (s *Stream) closedErr() error {
	if s.evicted.Load() {
		return ErrStreamEvicted
	}
	return io.EOF
}

// buffered returns the stream's unread bytes.
func (s *Stream) buffered() (n int) {
	s.bufferLock.Lock()
	for _, b := range s.buffers {
		n += len(b)
	}
	s.bufferLock.Unlock()
	return
}

func (s *Stream) finSeen() bool {
	select {
	case <-s.chFinEvent:
		return true
	default:
		return false
	}
}

// stallWatch runs while the session lives (StallTimeout > 0).
func (s *Session) stallWatch() {
	t := s.config.StallTimeout
	tick := t / 6
	if tick < 20*time.Millisecond {
		tick = 20 * time.Millisecond
	}
	tk := time.NewTicker(tick)
	defer tk.Stop()
	for {
		select {
		case <-s.die:
			return
		case <-tk.C:
		}
		now := nowNano()
		if ws := s.writeBusySince.Load(); ws != 0 && now-ws > int64(t) {
			stallSendCloses.Add(1)
			stallLogf("smux stall: send path frozen %.1fs (one conn write never returned), closing session %p with %d streams",
				float64(now-ws)/1e9, s, s.NumStreams())
			if f := StallDump; f != nil {
				f(fmt.Sprintf("sendLoop(%p)", s))
			}
			s.Close()
			return
		}
		rw := s.recvWaitSince.Load()
		if rw == 0 {
			continue
		}
		if lt := s.lastTokens.Load(); lt > rw {
			rw = lt
		}
		if now-rw > int64(t/2) {
			s.evictUnread(now, int64(t/2))
		}
	}
}

// evictUnread drops the streams that hold unread bytes and have not read for
// idle ns, largest first, until the bucket is positive again.
func (s *Session) evictUnread(now, idle int64) {
	type cand struct {
		st  *Stream
		buf int
	}
	var cs []cand
	s.streamLock.Lock()
	for _, st := range s.streams {
		if now-st.lastRead.Load() < idle {
			continue
		}
		if n := st.buffered(); n > 0 {
			cs = append(cs, cand{st, n})
		}
	}
	s.streamLock.Unlock()
	sort.Slice(cs, func(i, j int) bool { return cs[i].buf > cs[j].buf })
	for _, c := range cs {
		if atomic.LoadInt32(&s.bucket) > 0 {
			break
		}
		st := c.st
		st.evicted.Store(true)
		fresh := false
		st.dieOnce.Do(func() { close(st.die); fresh = true })
		stallEvicts.Add(1)
		stallEvictBytes.Add(int64(c.buf))
		stallLogf("smux stall: evict stream %d (%d unread B, read %d B in all, idle %.1fs, peer fin %v) on session %p",
			st.id, c.buf, atomic.LoadUint32(&st.numRead), float64(now-st.lastRead.Load())/1e9, st.finSeen(), s)
		s.streamClosed(st.id) // tokens back, out of the map
		if fresh {
			go s.writeFrame(newFrame(byte(s.config.Version), cmdFIN, st.id))
		}
	}
}
