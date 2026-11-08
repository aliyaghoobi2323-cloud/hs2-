package engine

import (
	"os"
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

func init() { mconnBatch.Store(os.Getenv("HS2_MCONN_BATCH") == "1") }

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
		s.batch = make([]byte, 0, mRecMax)
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
