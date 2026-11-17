package engine

import (
	"runtime"
	"testing"
)

// heavyStream stands for a smux stream that pins its session (frames, TLS
// buffers): 64 KB per stream.
type heavyStream struct {
	pin    []byte
	closed bool
}

func (h *heavyStream) Read(p []byte) (int, error)  { return 0, nil }
func (h *heavyStream) Write(p []byte) (int, error) { return len(p), nil }
func (h *heavyStream) Close() error                { h.closed = true; return nil }

// TestMConnEdgeForgetsClosedSubs (mem64): a long-lived connection whose subs
// are closed and re-opened many times (idle retire, link kill) must not keep
// the closed ones (and what their streams pin) until it ends.
func TestMConnEdgeForgetsClosedSubs(t *testing.T) {
	heap := func() uint64 {
		runtime.GC()
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return m.HeapAlloc
	}
	e := &mconnEdge{}
	add := func() *mconnSub {
		s := &mconnSub{stream: &heavyStream{pin: make([]byte, 64<<10)}, plane: planeStripe, release: func() {}, edge: e}
		mconnPlaneGauge(planeStripe).Add(1)
		e.mu.Lock()
		e.subs = append(e.subs, s)
		e.opened++
		e.mu.Unlock()
		return s
	}
	keep := add() // the connection's long-lived sub
	base := heap()
	for i := 0; i < 5000; i++ { // 5000 subs opened and closed over its life
		s := add()
		if i%3 == 0 {
			add().Close()
		}
		s.Close()
	}
	grown := int64(heap()) - int64(base)
	e.mu.Lock()
	n := len(e.subs)
	e.mu.Unlock()
	if n != 1 || e.subs[0] != keep {
		t.Fatalf("edge holds %d subs after the closed ones, want only the open one", n)
	}
	if grown > 4<<20 {
		t.Fatalf("heap grew %d KB over 5000 closed subs (pinned streams)", grown>>10)
	}
	if got := e.end(); got != e.opened || !keep.stream.(*heavyStream).closed {
		t.Fatalf("end reported %d subs (open one closed=%v), want all %d ever opened", got, keep.stream.(*heavyStream).closed, e.opened)
	}
	keep.Close() // a second close is a no-op
}
