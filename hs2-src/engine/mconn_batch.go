package engine

import (
	"sync"
	"sync/atomic"
)

// HS2_MCONN_BATCH=1 (lite only, off by default): a sub's writer sends every
// record waiting in its queue in one write to its smux stream, up to one smux
// frame (mRecMax), instead of one write per record.
//
// Why: smux serves its streams one frame per stream per turn (a FIFO of
// writes, each waiting for its frame to reach the socket). A game's records
// are a few hundred bytes, but each one pays a whole turn, behind a 16 KiB
// frame of every download on the same link. On a link slowed by loss (2%:
// a few Mbit/s) a turn took longer than the game's 20-50 ms between frames,
// so the game's queue only grew: in the lab both of a game's subs sat on
// links with downloads, and its echoes came back 2-9 s late (frames lost at
// the end of the phase) while the reassembly showed no gap — both copies were
// simply slow. A raw stream does not have this: the relay's read takes what
// accumulated in one write. With batching, a backlog drains in one turn.
var mconnBatch atomic.Bool

// batchLocked (under m.mu, writer, s.cur just taken from the queue with wire
// first): moves the records that follow in the queue into s.curMore while
// they fit one frame with first, and returns their wires joined. Gated subs
// never get here (the writer waits).
func (s *msub) batchLocked(first []byte) []byte {
	total := len(first)
	k := 0
	for k < len(s.q) && total+len(s.q[k].wire()) <= mRecMax {
		total += len(s.q[k].wire())
		k++
	}
	if k == 0 {
		return first
	}
	if cap(s.batch) < mRecMax {
		s.batch = getMBatch()
	}
	b := append(s.batch[:0], first...)
	for i := 0; i < k; i++ {
		c := s.q[i]
		b = append(b, c.wire()...)
		s.qBytes -= c.qsize()
		s.curMore = append(s.curMore, c)
		s.q[i] = nil
	}
	s.q = s.q[k:]
	return b
}

// mBatchPool (gc295): the writers' batch buffers (mRecMax each). A sub used
// to make its own on its first batch and drop it when its writeLoop ended
// (mem66), so every short-lived lite sub of the crowd allocated one: the
// exit's second-largest allocation site in the gc295 storm profile
// (~210 MB/min). The ended writeLoop now hands it to the next sub.
var mBatchPool = sync.Pool{New: func() any { b := make([]byte, 0, mRecMax); return &b }}

func getMBatch() []byte { return (*mBatchPool.Get().(*[]byte))[:0] }

// putMBatch: b's writer is done with it (its writeLoop returned, so no
// Write still reads it).
func putMBatch(b []byte) {
	if cap(b) < mRecMax {
		return
	}
	b = b[:0]
	mBatchPool.Put(&b)
}
