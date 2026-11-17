package engine

import (
	"testing"
	"time"
)

// hs128 (HS2_MCONN_STRIPE_KEEPSTALL_LAZY): with the key on, the sender's
// ack-stall poll starts only once a sub with data waiting stops being
// acked; a sub with only an unacked tail and nothing to send does not
// start it; the poll stops again when nothing is suspect; and a dark sub is
// still closed by the rule.
func TestKeepStallLazyPoll(t *testing.T) {
	fm := setKeepStall(t, true)
	keepStallLazy.Store(true)
	m := newMConn(1281, false, nil)
	a, b := ksSubN(m, 1), ksSubN(m, 2)
	m.subs = []*msub{a, b}
	m.stN = 2
	t0 := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	// a: an unacked tail, nothing queued (quiet: smux window-update lag)
	fm.write(m, a, t0, keepChunk(1000, 16<<10), 1<<30)
	a.qBytes = 0
	off := uint64(1000 + 16<<10)
	for i := 0; i < 100; i++ { // b flows, acked, for 1 s
		now := t0.Add(time.Duration(i) * 10 * time.Millisecond)
		fm.write(m, b, now, keepChunk(off, 16<<10), 0)
		off += 16 << 10
	}
	if m.ksTicking {
		t.Fatal("quiet: the poll started (only an unacked tail, nothing to send)")
	}
	// a now has data waiting and no ack: suspect
	a.qBytes = 1 << 20
	t1 := t0.Add(time.Second)
	fm.write(m, b, t1.Add(150*time.Millisecond), keepChunk(off, 16<<10), 0)
	off += 16 << 10
	if !m.ksTicking {
		t.Fatal("a stalled sub with data waiting did not start the poll")
	}
	var st []*msub
	for i := 0; i < 40 && len(st) == 0; i++ {
		now := t1.Add(time.Duration(200+50*i) * time.Millisecond)
		fm.write(m, b, now, keepChunk(off, 16<<10), 0)
		off += 16 << 10
		st, _ = m.ackStallTickLocked(now)
	}
	if len(st) != 1 || st[0] != a {
		t.Fatalf("lazy: the dark sub was not closed (stalled %d)", len(st))
	}
	m.releaseSentLocked()
}
