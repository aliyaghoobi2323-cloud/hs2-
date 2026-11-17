package engine

import (
	"context"
	"runtime"
	"testing"
	"time"
	"weak"
)

// mem70: an open waiting for a link (none fits) must not keep the links in
// its avoid list alive — the raw first sub's link and the remembered dead
// ones, built as the raw upgrade builds them.
func TestMConnWaitingOpenDoesNotPinLinks(t *testing.T) {
	setLite(t, false)
	lm, _ := pickLinks([]int{}, []bool{})
	m := newMConn(1, true, nil)
	const n = 20
	var ws []weak.Pointer[mtcpLink]
	raw := &mtcpLink{flows: map[*countedStream]struct{}{}}
	ws = append(ws, weak.Make(raw))
	m.mu.Lock()
	for i := 0; i < n; i++ {
		l := &mtcpLink{flows: map[*countedStream]struct{}{}}
		ws = append(ws, weak.Make(l))
		m.noteDeadKeyLocked(any(Link(l)))
	}
	avoid := append([]any{avoidKey(any(Link(raw)))}, m.deadAvoidLocked()...)
	m.mu.Unlock()
	raw = nil
	if len(avoid) < 2 {
		t.Fatalf("avoid list %v: want the raw link and the dead ones", avoid)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e := &mconnEdge{ctx: ctx, lm: lm, port: 443, id: 9, first: true}
	done := make(chan error, 1)
	go func(av []any) {
		_, _, err := e.open(planeLight, av)
		done <- err
	}(avoid)
	avoid = nil
	time.Sleep(300 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("open returned before its wait: %v", err)
	default:
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
	select {
	case <-done:
		t.Fatal("open returned during the check: the wait was not covered")
	default:
	}
	if held != 0 {
		t.Fatalf("%d of %d links still referenced by a waiting open", held, len(ws))
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("open found a link in an empty pool")
	}
	runtime.KeepAlive(m)
}

// An avoid entry by ID still keeps the picker off that link.
func TestAvoidKeyMatchesLink(t *testing.T) {
	a := &mtcpLink{}
	b := &mtcpLink{}
	av := []any{avoidKey(any(Link(a)))}
	if !avoided(a, av) || avoided(b, av) {
		t.Fatal("avoid by ID does not match its own link only")
	}
	if a.avoidID() != a.avoidID() || a.avoidID() == b.avoidID() {
		t.Fatal("avoid IDs not stable or not distinct")
	}
}
