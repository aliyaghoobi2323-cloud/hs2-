package engine

import (
	"bytes"
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func setOpenNoLock(t *testing.T, on bool) {
	t.Helper()
	prev := b312OpenNoLock.Load()
	b312OpenNoLock.Store(on)
	t.Cleanup(func() { b312OpenNoLock.Store(prev) })
}

// b312Roles: the role byte of every stream opened on the pool, and whether
// every one of them is closed.
func b312Roles(ls []*mcLink) (roles []byte, open int) {
	for _, l := range ls {
		for _, s := range l.streams() {
			select {
			case b := <-s.first:
				roles = append(roles, b[11])
				s.first <- b
			default:
				roles = append(roles, 0xff) // nothing written
			}
			select {
			case <-s.closed:
			default:
				open++
			}
		}
	}
	return roles, open
}

// Concurrent opens on one edge (light and bulk, as the manager's
// openLight/openBulk goroutines run them), some closed at once: one role 0,
// every other a join with the copy mark, counters exact, nothing leaked.
func TestB312ConcurrentOpens(t *testing.T) {
	setLite(t, false)
	for _, on := range []bool{true, false} {
		setOpenNoLock(t, on)
		m, ls := pickLinks([]int{0, 0, 0, 1, 1, 0}, []bool{false, false, false, false, false, false})
		e := &mconnEdge{ctx: context.Background(), lm: m, port: 443, id: 31}
		gl0, gb0 := mconnPlaneGauge(planeLight).Load(), mconnPlaneGauge(planeBulk).Load()
		first, _, err := e.open(planeLight, nil)
		if err != nil {
			t.Fatal(err)
		}
		const G, N = 16, 25
		var ok, kept atomic.Int64
		var wg sync.WaitGroup
		for g := 0; g < G; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				for i := 0; i < N; i++ {
					p := planeLight
					if (g+i)%3 == 0 {
						p = planeBulk
					}
					s, _, err := e.open(p, nil)
					if err != nil {
						continue
					}
					ok.Add(1)
					if i%2 == 0 {
						s.Close()
						s.Close() // released once
					} else {
						kept.Add(1)
					}
				}
			}(g)
		}
		wg.Wait()
		e.mu.Lock()
		subs, opened := len(e.subs), e.opened
		e.mu.Unlock()
		if opened != int(ok.Load())+1 || subs != int(kept.Load())+1 {
			t.Fatalf("on=%v opened %d subs %d, want %d/%d", on, opened, subs, ok.Load()+1, kept.Load()+1)
		}
		if u := m.users.Load(); u != int32(subs) {
			t.Fatalf("on=%v users %d, want %d (one per open sub)", on, u, subs)
		}
		roles, _ := b312Roles(ls)
		if len(roles) != opened {
			t.Fatalf("on=%v %d streams for %d opens", on, len(roles), opened)
		}
		if n0 := bytes.Count(roles, []byte{mconnRoleFirst}); n0 != 1 || bytes.Count(roles, []byte{mconnRoleJoin}) != opened-1 {
			t.Fatalf("on=%v roles %v: want one role 0, the rest joins", on, roles)
		}
		if n := e.end(); n != opened {
			t.Fatalf("on=%v end saw %d, want %d", on, n, opened)
		}
		if u := m.users.Load(); u != 0 {
			t.Fatalf("on=%v users %d after end", on, u)
		}
		if _, open := b312Roles(ls); open != 0 {
			t.Fatalf("on=%v %d streams left open", on, open)
		}
		if gl, gb := mconnPlaneGauge(planeLight).Load(), mconnPlaneGauge(planeBulk).Load(); gl != gl0 || gb != gb0 {
			t.Fatalf("on=%v gauges light %d->%d bulk %d->%d", on, gl0, gl, gb0, gb)
		}
		if _, _, err := e.open(planeLight, nil); err == nil {
			t.Fatal("open after end succeeded")
		}
		_ = first
	}
}

// end racing opens: whatever the interleaving, every sub opened is closed
// and released (either end closed it, or the install found the edge ended),
// opened == installed, and no open succeeds after end returned.
func TestB312CloseRacesOpen(t *testing.T) {
	setLite(t, false)
	setOpenNoLock(t, true)
	e0 := b312Open.ended.Load()
	var refused int64
	for it := 0; it < 300; it++ {
		m, ls := pickLinks([]int{0, 0, 0, 1}, []bool{false, false, false, false})
		e := &mconnEdge{ctx: context.Background(), lm: m, port: 443, id: uint64(it)}
		if _, _, err := e.open(planeLight, nil); err != nil {
			t.Fatal(err)
		}
		var ok atomic.Int64
		var wg sync.WaitGroup
		start := make(chan struct{})
		for g := 0; g < 6; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				<-start
				p := planeLight
				if g%2 == 1 {
					p = planeBulk
				}
				if _, _, err := e.open(p, nil); err == nil {
					ok.Add(1)
				}
			}(g)
		}
		var n int
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if it%2 == 1 {
				time.Sleep(time.Duration(it%7) * 10 * time.Microsecond)
			}
			n = e.end()
		}()
		close(start)
		wg.Wait()
		if n != int(ok.Load())+1 {
			t.Fatalf("it %d: end saw %d, %d opens succeeded (+1 first)", it, n, ok.Load())
		}
		if u := m.users.Load(); u != 0 {
			t.Fatalf("it %d: %d placements leaked", it, u)
		}
		roles, open := b312Roles(ls)
		if open != 0 {
			t.Fatalf("it %d: %d streams left open", it, open)
		}
		refused += int64(len(roles) - n)
		e.mu.Lock()
		if len(e.subs) != 0 {
			t.Fatalf("it %d: %d subs after end", it, len(e.subs))
		}
		e.mu.Unlock()
	}
	if got := b312Open.ended.Load() - e0; got != refused {
		t.Fatalf("ended counter %d, streams refused at install %d", got, refused)
	}
	t.Logf("installs refused after end: %d", refused)
}

// The point of the key: a join open waiting for a link (none fits) does
// not hold e.mu, so forget/end/other opens are not queued behind it; with
// the key off it does (the old behaviour).
func TestB312WaitDoesNotHoldEdgeLock(t *testing.T) {
	setLite(t, false)
	for _, on := range []bool{true, false} {
		setOpenNoLock(t, on)
		lm, _ := pickLinks([]int{}, []bool{})
		ctx, cancel := context.WithCancel(context.Background())
		e := &mconnEdge{ctx: ctx, lm: lm, port: 443, id: 5, first: true}
		done := make(chan error, 1)
		go func() { _, _, err := e.open(planeLight, nil); done <- err }()
		time.Sleep(100 * time.Millisecond)
		got := e.mu.TryLock()
		if got {
			e.mu.Unlock()
		}
		if got != on {
			t.Fatalf("on=%v: e.mu free during the wait = %v", on, got)
		}
		if on {
			e.end() // end during the wait: the open stops at its next pick gap
		}
		cancel()
		select {
		case err := <-done:
			if err == nil {
				t.Fatalf("on=%v: open found a link in an empty pool", on)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("on=%v: open did not return", on)
		}
		if lm.users.Load() != 0 {
			t.Fatal("placement leaked")
		}
	}
}

// The ended check runs before every pick: an edge ended while an open
// waited for a gap stops picking (no placement after end).
func TestB312EndStopsWaitingOpen(t *testing.T) {
	setLite(t, false)
	setOpenNoLock(t, true)
	lm, _ := pickLinks([]int{}, []bool{})
	e := &mconnEdge{ctx: context.Background(), lm: lm, port: 443, id: 6, first: true}
	done := make(chan error, 1)
	t0 := time.Now()
	go func() { _, _, err := e.open(planeLight, nil); done <- err }()
	time.Sleep(50 * time.Millisecond)
	e.end()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("open succeeded on an ended edge")
		}
		if d := time.Since(t0); d >= mconnNextWait {
			t.Fatalf("open waited %v after end (its whole wait)", d)
		}
	case <-time.After(mconnNextWait + 2*time.Second):
		t.Fatal("open did not return")
	}
}

// The first open keeps the locked path even with the key on; key off
// counts every open as locked.
func TestB312FirstOpenLocked(t *testing.T) {
	setLite(t, false)
	setOpenNoLock(t, true)
	m, ls := pickLinks([]int{0, 0}, []bool{false, false})
	e := &mconnEdge{ctx: context.Background(), lm: m, port: 443, id: 8}
	l0, n0 := b312Open.locked.Load(), b312Open.nolock.Load()
	if _, _, err := e.open(planeLight, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.open(planeLight, nil); err != nil {
		t.Fatal(err)
	}
	if b312Open.locked.Load()-l0 != 1 || b312Open.nolock.Load()-n0 != 1 {
		t.Fatalf("locked +%d nolock +%d, want +1/+1", b312Open.locked.Load()-l0, b312Open.nolock.Load()-n0)
	}
	roles, _ := b312Roles(ls)
	if !bytes.Equal(roles, []byte{mconnRoleFirst, mconnRoleJoin}) && !bytes.Equal(roles, []byte{mconnRoleJoin, mconnRoleFirst}) {
		t.Fatalf("roles %v", roles)
	}
	e.end()
	if m.users.Load() != 0 {
		t.Fatal("placement leaked")
	}
	if b312OpenLine() == "" {
		t.Fatal("mem line empty after an unlocked open")
	}
}

// A stripe open (one try) on an edge without a first sub, or ended, fails
// without a placement, with the key on or off.
func TestB312StripeRefused(t *testing.T) {
	setLite(t, false)
	for _, on := range []bool{true, false} {
		setOpenNoLock(t, on)
		m, _ := pickLinks([]int{0, 0}, []bool{false, false})
		e := &mconnEdge{ctx: context.Background(), lm: m, port: 443, id: 9}
		if _, _, err := e.open(planeStripe, nil); err == nil {
			t.Fatalf("on=%v stripe before the first sub", on)
		}
		e.first = true
		e.end()
		if _, _, err := e.open(planeStripe, nil); err == nil {
			t.Fatalf("on=%v stripe after end", on)
		}
		if m.users.Load() != 0 {
			t.Fatalf("on=%v placement leaked", on)
		}
	}
}
