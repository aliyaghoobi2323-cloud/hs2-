package engine

import (
	"testing"
	"time"
)

func setHedge10(t *testing.T, on bool) {
	o, od := liteHedge10, b468lDbg
	setHedge8(t, true, 20*time.Millisecond) // key off = HEDGE8 alone
	liteHedge10, b468lDbg = on, true
	t.Cleanup(func() { liteHedge10, b468lDbg = o, od })
}

func b468lN() (n int64) {
	for i := range b468lStat {
		n += b468lStat[i].n.Load()
	}
	return
}

// b468l: a heavy hedged conn's one-copy response with HEDGE10 rides the big
// queue / big batches and arrives byte exact both ways; key off: the big rule
// never runs. The debug clock counts the exit conn that sent 2 MiB.
func TestB468lHedge10Bytes(t *testing.T) {
	for _, on := range []bool{false, true} {
		setHedge10(t, on)
		b0, n0 := hedge10Stat.big.Load(), b468lN()
		_, iran, exit := newHedge2Pair(t, nil)
		mWaitFor(t, "two subs", 3*time.Second, func() bool { return liveSubs(iran) == 2 })
		exit.exitHedge3Mark(false)
		req := randBytes(3000, 51)
		both(t, func() error { return xfer(iran, req, 1000, 0) }, func() error { return readN(exit, req) })
		resp := randBytes(6<<20, 52)
		both(t, func() error { return xfer(exit, resp, 64<<10, 0) }, func() error { return readN(iran, resp) })
		up := randBytes(1<<20, 53)
		both(t, func() error { return xfer(iran, up, 16<<10, 0) }, func() error { return readN(exit, up) })
		resp2 := randBytes(2<<20+777, 54)
		both(t, func() error { return xfer(exit, resp2, 100<<10, 0) }, func() error { return readN(iran, resp2) })
		big := hedge10Stat.big.Load() - b0
		if on && big == 0 {
			t.Fatal("key on: big rule never ran")
		}
		if !on && big != 0 {
			t.Fatalf("key off: big rule ran %d", big)
		}
		if b468lN()-n0 < 1 {
			t.Fatal("dbg: the exit conn's 2 MiB not counted")
		}
		t.Logf("on=%v big=%d writes=%d recs=%d", on, big, hedge10Stat.writes.Load(), hedge10Stat.recs.Load())
		iran.end(errMConnClosed, false)
		exit.end(errMConnClosed, false)
	}
}

// b468l: a call (callKeep) or a light hedged conn never gets the big rule.
func TestB468lHedge10Light(t *testing.T) {
	setHedge10(t, true)
	b0 := hedge10Stat.big.Load()
	_, iran, exit := newHedge2Pair(t, nil)
	mWaitFor(t, "two subs", 3*time.Second, func() bool { return liveSubs(iran) == 2 })
	exit.exitHedge3Mark(false)
	req := randBytes(1500, 61)
	both(t, func() error { return xfer(iran, req, 500, 0) }, func() error { return readN(exit, req) })
	resp := randBytes(3000, 62)
	both(t, func() error { return xfer(exit, resp, 1000, 0) }, func() error { return readN(iran, resp) })
	if d := hedge10Stat.big.Load() - b0; d != 0 {
		t.Fatalf("light conn: big rule ran %d", d)
	}
	exit.mu.Lock()
	b := exit.h10Big
	exit.mu.Unlock()
	if b {
		t.Fatal("light conn: h10Big set")
	}
	iran.end(errMConnClosed, false)
	exit.end(errMConnClosed, false)
}

// b468l: the writer's batch joins up to hedge10Batch bytes of queued records
// for a big-rule primary (the bytes are the records back to back, in order)
// and stays at one frame (mRecMax) otherwise.
func TestB468lBatch(t *testing.T) {
	setHedge10(t, true)
	m := newMConn(99, false, nil)
	defer m.end(errMConnClosed, false)
	s := &msub{m: m}
	m.primary.Store(s)
	for _, big := range []bool{false, true} {
		m.mu.Lock()
		m.h10Big = big
		var want []byte
		var first []byte
		for i := 0; i < 8; i++ {
			c := getChunkN(mRecData, mDataHdr+8000)
			for j := range c.buf[mDataHdr:] {
				c.buf[mDataHdr+j] = byte(i + j)
			}
			c.n = 8000
			if i == 0 {
				first = c.wire()
			} else {
				s.q = append(s.q, c)
				s.qBytes += c.qsize()
			}
			want = append(want, c.wire()...)
		}
		got := s.batchLocked(first)
		lim := mRecMax
		if big {
			lim = hedge10Batch
		}
		if len(got) > lim || len(got) < lim-len(first) {
			t.Fatalf("big=%v batch %d bytes, lim %d", big, len(got), lim)
		}
		if string(got) != string(want[:len(got)]) {
			t.Fatalf("big=%v batch bytes differ", big)
		}
		s.q, s.qBytes, s.curMore = nil, 0, nil
		m.mu.Unlock()
	}
}
