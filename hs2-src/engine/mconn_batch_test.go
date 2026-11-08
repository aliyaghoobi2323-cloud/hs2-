package engine

import (
	"encoding/binary"
	"io"
	"testing"
	"time"
)

func setBatch(t *testing.T, on bool) {
	t.Helper()
	old := mconnBatch.Load()
	mconnBatch.Store(on)
	t.Cleanup(func() { mconnBatch.Store(old) })
}

// gameOverSlowTurns: a lite pair, dual; every write of the exit's subs costs
// a 50 ms turn (smux serving a download's frame on the same link first). The
// exit sends a game's 150-byte frames every 20 ms for 1 s; the worst delay of
// a frame at Iran is returned, after checking every byte arrived in order.
func gameOverSlowTurns(t *testing.T) time.Duration {
	_, iran, exit := newLitePair(t)
	mWaitFor(t, "dual", mLiteAfter+2*time.Second, func() bool { return liveSubs(iran) == 2 && liveSubs(exit) == 2 })
	exit.mu.Lock()
	for _, s := range exit.subs {
		s.rw.(*tSub).turn.Store(int64(50 * time.Millisecond))
	}
	exit.mu.Unlock()
	const frames, size = 50, 150
	t0 := time.Now()
	go func() {
		next := time.Now()
		for i := 0; i < frames; i++ {
			time.Sleep(time.Until(next))
			f := make([]byte, size)
			binary.BigEndian.PutUint64(f, uint64(time.Since(t0)))
			f[8] = byte(i)
			if _, err := exit.Write(f); err != nil {
				return
			}
			next = next.Add(20 * time.Millisecond)
		}
	}()
	var worst time.Duration
	buf := make([]byte, size)
	for i := 0; i < frames; i++ {
		if _, err := io.ReadFull(iran, buf); err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if int(buf[8]) != i {
			t.Fatalf("frame %d arrived as %d: out of order", i, buf[8])
		}
		at := time.Duration(binary.BigEndian.Uint64(buf))
		if d := time.Since(t0) - at; d > worst {
			worst = d
		}
	}
	closeAndWait(t, iran, exit)
	return worst
}

// Without batching each record pays its own turn: 20 records/s through a
// sub against 50 sent, so the game falls seconds behind. With
// HS2_MCONN_BATCH the backlog leaves in one turn and the delay stays near it.
func TestMConnLiteBatchGameBehindTurns(t *testing.T) {
	setBatch(t, false)
	off := gameOverSlowTurns(t)
	setBatch(t, true)
	on := gameOverSlowTurns(t)
	t.Logf("worst frame delay: batch off %v, on %v", off, on)
	if off < 900*time.Millisecond {
		t.Fatalf("batch off: worst %v, expected the per-record turns to pile up (>900ms)", off)
	}
	if on > 300*time.Millisecond {
		t.Fatalf("batch on: worst %v, want <= 300ms", on)
	}
}

// Batching keeps a transfer intact (records joined into one write are split
// again by the reader), both ways, with uneven pieces.
func TestMConnLiteBatchTransfer(t *testing.T) {
	setBatch(t, true)
	_, iran, exit := newLitePair(t)
	mWaitFor(t, "dual", mLiteAfter+2*time.Second, func() bool { return liveSubs(iran) == 2 && liveSubs(exit) == 2 })
	for _, m := range []*mConn{iran, exit} {
		m.mu.Lock()
		for _, s := range m.subs {
			s.rw.(*tSub).turn.Store(int64(2 * time.Millisecond))
		}
		m.mu.Unlock()
	}
	transfer(t, iran, exit, 300<<10, 300<<10)
}
