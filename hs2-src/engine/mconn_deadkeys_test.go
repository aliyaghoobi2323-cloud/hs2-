package engine

import (
	"io"
	"runtime"
	"testing"
	"time"
	"weak"
)

// mem68: the links a long-lived connection's subs died on must be collected
// once nothing else holds them; while alive they are still avoided.
func TestMConnDeadKeysDoNotPinLinks(t *testing.T) {
	m := newMConn(1, true, nil)
	const n = 50
	var ws []weak.Pointer[mtcpLink]
	alive := &mtcpLink{flows: map[*countedStream]struct{}{}}
	for i := 0; i < n; i++ {
		l := &mtcpLink{flows: map[*countedStream]struct{}{}}
		ws = append(ws, weak.Make(l))
		m.mu.Lock()
		m.noteDeadKeyLocked(any(Link(l)))
		m.mu.Unlock()
	}
	m.mu.Lock()
	m.noteDeadKeyLocked(any(Link(alive)))
	if got := len(m.deadKeys); got != mDeadKeys {
		t.Fatalf("deadKeys len %d, want %d", got, mDeadKeys)
	}
	m.mu.Unlock()
	for i := 0; i < 3; i++ {
		runtime.GC()
	}
	gone := 0
	for _, w := range ws {
		if w.Value() == nil {
			gone++
		}
	}
	if gone != n {
		t.Fatalf("%d of %d dead links still referenced after GC", n-gone, n)
	}
	m.mu.Lock()
	av := m.deadAvoidLocked()
	m.mu.Unlock()
	if len(av) != 1 || !avoided(alive, av) {
		t.Fatalf("avoid list %v: want only the live link", av)
	}
	runtime.KeepAlive(alive)
	runtime.KeepAlive(m)
}

// A dead sub must not stay in the call-score slots, and a shorter rescoring
// clears the slots past the live subs.
func TestMConnScoreSubForgetsDeadSubs(t *testing.T) {
	m := newMConn(2, true, nil)
	subs := make([]*msub, 3)
	for i := range subs {
		subs[i] = &msub{m: m, done: make(chan struct{})}
	}
	m.mu.Lock()
	m.callScoresLocked(subs)
	m.forgetScoreSubLocked(subs[1])
	if m.scoreSub[1] != nil {
		t.Fatal("dead sub still in its score slot")
	}
	m.scoreAt = time.Time{}
	m.callScoresLocked(subs[:1])
	for i := 1; i < len(m.scoreSub); i++ {
		if m.scoreSub[i] != nil {
			t.Fatalf("slot %d still holds a sub after rescoring one", i)
		}
	}
	m.mu.Unlock()
}

type bigRW struct{ buf []byte }

func (*bigRW) Read([]byte) (int, error)  { return 0, io.EOF }
func (*bigRW) Write([]byte) (int, error) { return 0, io.EOF }
func (*bigRW) Close() error              { return nil }

// mem68 root cause: N upgraded connections whose raw first sub died must not
// keep its stream (and with it the link) after the sub is gone.
func TestMConnDeadRaw1ReleasesStream(t *testing.T) {
	old := mconnDeadlineFix.Load()
	mconnDeadlineFix.Store(true)
	defer mconnDeadlineFix.Store(old)
	const n = 100
	var ms []*mConn
	var ws []weak.Pointer[bigRW]
	for i := 0; i < n; i++ {
		m := newMConn(uint64(100+i), true, nil)
		rw := &bigRW{buf: make([]byte, 32<<10)}
		ws = append(ws, weak.Make(rw))
		l := &mtcpLink{}
		m.mu.Lock()
		s := m.addSubLocked(rw, any(Link(l)), false)
		m.raw1 = s
		m.mu.Unlock()
		// the sub's loops run and die (the link was killed)
		m.subLoopEnter(s)
		s.close()
		m.mu.Lock()
		s.dead = true
		m.removeSubLocked(s)
		m.mu.Unlock()
		m.subLoopExit(s)
		ms = append(ms, m)
	}
	for i := 0; i < 3; i++ {
		runtime.GC()
	}
	held := 0
	for _, w := range ws {
		if w.Value() != nil {
			held++
		}
	}
	if held != 0 {
		t.Fatalf("%d of %d dead raw first subs still hold their stream", held, n)
	}
	for _, m := range ms {
		if m.raw1 == nil || m.raw1.key != nil {
			t.Fatal("raw1 must stay set (raw marker) with its key dropped")
		}
		if _, err := m.raw1.rw.Write([]byte{1}); err == nil {
			t.Fatal("the stand-in must fail writes")
		}
	}
	runtime.KeepAlive(ms)
}

// While the reader is still raw nothing is released.
func TestMConnRaw1KeptWhileRaw(t *testing.T) {
	old := mconnDeadlineFix.Load()
	mconnDeadlineFix.Store(true)
	defer mconnDeadlineFix.Store(old)
	m := newMConn(7, true, nil)
	rw := &bigRW{}
	m.mu.Lock()
	s := m.addSubLocked(rw, nil, false)
	m.raw1 = s
	m.mu.Unlock()
	m.rawR.Store(true)
	m.subLoopEnter(s)
	s.close()
	m.mu.Lock()
	s.dead = true
	m.mu.Unlock()
	m.subLoopExit(s)
	if m.raw1.rw != mSub(rw) {
		t.Fatal("released while the reader is still raw")
	}
}

// mem69: the exit's raw first sub (attachRaw) carries an onClose closure over
// its exitConn, whose link meter holds the session; once released, nothing of
// the dead link may stay reachable from the live connection.
func TestMConnDeadRaw1ReleasesOnClose(t *testing.T) {
	old := mconnDeadlineFix.Load()
	mconnDeadlineFix.Store(true)
	defer mconnDeadlineFix.Store(old)
	const n = 50
	var ms []*mConn
	var ws []weak.Pointer[linkMeter]
	calls := 0
	for i := 0; i < n; i++ {
		m := newMConnExitRaw(uint64(300 + i))
		mtr := &linkMeter{path: "dead"}
		ws = append(ws, weak.Make(mtr))
		c := &exitConn{mtr: mtr, sub: true}
		m.attachRaw(&bigRW{}, func() {
			calls++
			_ = c.mtr.path
		})
		s := m.raw1
		// upgraded: neither direction raw any more
		m.rawR.Store(false)
		m.rawW.Store(false)
		m.subLoopEnter(s)
		s.close()
		m.mu.Lock()
		s.dead = true
		m.removeSubLocked(s)
		m.mu.Unlock()
		m.subLoopExit(s)
		ms = append(ms, m)
	}
	if calls != n {
		t.Fatalf("onClose ran %d times, want %d", calls, n)
	}
	for i := 0; i < 3; i++ {
		runtime.GC()
	}
	held := 0
	for _, w := range ws {
		if w.Value() != nil {
			held++
		}
	}
	if held != 0 {
		t.Fatalf("%d of %d dead raw first subs still hold their link meter (onClose)", held, n)
	}
	for _, m := range ms {
		if m.raw1 == nil || m.raw1.onClose != nil {
			t.Fatal("raw1 must stay set with onClose dropped")
		}
		m.raw1.close() // a second close must not run onClose again (or panic)
	}
	if calls != n {
		t.Fatalf("onClose ran again after release: %d", calls)
	}
	runtime.KeepAlive(ms)
}
