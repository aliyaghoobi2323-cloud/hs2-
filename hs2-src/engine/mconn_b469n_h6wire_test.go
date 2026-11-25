package engine

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"
)

func setH6(t *testing.T, on bool) {
	t.Helper()
	old := liteH6
	liteH6 = on
	t.Cleanup(func() { liteH6 = old })
}

// h6Pipe starts Iran's copy sender and the exit's role-8 reader on a pipe;
// it returns the bytes the exit wrote back on the copy sub (must stay 0)
// once both ended.
func h6Pipe(t *testing.T, iran, exit *mConn, until time.Time, delay time.Duration) func() int {
	t.Helper()
	iran.mu.Lock()
	hs := &h6Send{wake: make(chan struct{}, 1)}
	iran.h6s = hs
	iran.mu.Unlock()
	a, b := net.Pipe()
	back := make(chan int, 1)
	sent := make(chan struct{})
	go func() {
		iran.h6Copy(hs, a, until)
		close(sent)
	}()
	go func() { // what the exit writes on the copy sub reaches Iran here
		n, _ := io.Copy(io.Discard, a)
		back <- int(n)
	}()
	read := make(chan struct{})
	go func() {
		time.Sleep(delay)
		h6ReadLoop(b, exit)
		close(read)
	}()
	return func() int {
		for _, c := range []chan struct{}{sent, read} {
			select {
			case <-c:
			case <-time.After(5 * time.Second):
				t.Fatal("copy sender/reader did not end")
			}
		}
		b.Close()
		a.Close()
		return <-back
	}
}

// End to end on a raw pair: Iran's copy and the raw stream, the exit's
// merge; the request byte exact, the response raw only (nothing on the copy
// sub, no msub for it), the copy capped at h6N, with the copy early or late.
func TestB469nH6Pipeline(t *testing.T) {
	setH6(t, true)
	for _, delay := range []time.Duration{0, 100 * time.Millisecond} {
		for _, size := range []int{1, 1500, 4096, 9000} {
			_, iran, exit := newRawPair(t)
			if exit.h6 == nil {
				t.Fatal("exit raw conn without h6")
			}
			done := h6Pipe(t, iran, exit, time.Now().Add(300*time.Millisecond), delay)
			up := randBytes(size, uint64(size))
			down := randBytes(64<<10, 9)
			m0 := h6Stat.mismatch.Load()
			both(t,
				func() error { return xfer(iran, up, 700, 0) },
				func() error { return readN(exit, up) },
			)
			both(t,
				func() error { return xfer(exit, down, 8<<10, 0) },
				func() error { return readN(iran, down) },
			)
			if back := done(); back != 0 {
				t.Fatalf("exit wrote %d bytes on the copy sub", back)
			}
			exit.h6mu.Lock()
			hEnd := exit.h6.hEnd
			freed := exit.h6.freed
			exit.h6mu.Unlock()
			if want := uint64(min(size, h6N)); hEnd != want && !freed {
				t.Fatalf("size %d delay %v: copy hEnd %d want %d", size, delay, hEnd, want)
			}
			if liveSubs(exit) != 1 {
				t.Fatalf("exit subs %d: the copy sub must not be an msub", liveSubs(exit))
			}
			if h6Stat.mismatch.Load() != m0 {
				t.Fatal("raw vs copy mismatch")
			}
			closeAndWait(t, iran, exit)
		}
	}
}

// The copy lost after its first frame, or garbled: the raw stream still
// carries every byte; an upgrade later keeps both ways exact.
func TestB469nH6CopyLost(t *testing.T) {
	setH6(t, true)
	for _, mode := range []string{"cut", "gap", "junk"} {
		_, iran, exit := newRawPair(t)
		up := randBytes(512<<10, 11)
		a, b := net.Pipe()
		go func() {
			switch mode {
			case "cut":
				a.Write(h6Frame(h6FrData, 0, up[:1000]))
			case "gap":
				a.Write(h6Frame(h6FrData, 0, up[:1000]))
				a.Write(h6Frame(h6FrData, 2000, up[2000:3000]))
			case "junk":
				a.Write([]byte{9, 0, 0, 0, 0, 0, 1, 1})
			}
			a.Close()
		}()
		h6ReadLoop(b, exit)
		b.Close()
		both(t,
			func() error {
				if err := xfer(iran, up[:100<<10], 3000, 0); err != nil {
					return err
				}
				forceUpgrade(iran)
				return xfer(iran, up[100<<10:], 16<<10, 0)
			},
			func() error { return readN(exit, up) },
		)
		exit.h6mu.Lock()
		ended := exit.h6.ended
		exit.h6mu.Unlock()
		if !ended {
			t.Fatalf("%s: copy not ended", mode)
		}
		closeAndWait(t, iran, exit)
	}
}

// An exit with the key but no copy (old Iran): exact both ways through an
// upgrade, hbuf never allocated.
func TestB469nH6NoCopy(t *testing.T) {
	setH6(t, true)
	_, iran, exit := newRawPair(t)
	up, down := randBytes(1<<20, 21), randBytes(1<<20, 22)
	both(t,
		func() error {
			if err := xfer(iran, up[:64<<10], 5000, 0); err != nil {
				return err
			}
			forceUpgrade(iran)
			return xfer(iran, up[64<<10:], 32<<10, 0)
		},
		func() error { return xfer(exit, down, 32<<10, 0) },
		func() error { return readN(exit, up) },
		func() error { return readN(iran, down) },
	)
	exit.h6mu.Lock()
	hb := exit.h6.hbuf
	exit.h6mu.Unlock()
	if hb != nil {
		t.Fatal("hbuf allocated without a copy")
	}
	closeAndWait(t, iran, exit)
}

// Mixed version: the cap is announced only with the key; Iran wants it in
// the exit's info (an rc12 exit never has bit 17); an exit with the key off
// has no h6 on its conns (role 8 refused in serveH6Sub).
func TestB469nH6Caps(t *testing.T) {
	setLite(t, true)
	oldOn := mconnOn.Load()
	mconnOn.Store(true)
	t.Cleanup(func() { mconnOn.Store(oldOn) })
	setH6(t, false)
	if infoExt()&capExtLiteH6 != 0 {
		t.Fatal("cap announced with the key off")
	}
	if newMConnExitRaw(1).h6 != nil {
		t.Fatal("h6 with the key off")
	}
	liteH6 = true
	if infoExt()&capExtLiteH6 == 0 {
		t.Fatal("cap not announced with the key on")
	}
	if newMConnExitRaw(2).h6 == nil {
		t.Fatal("no h6 with the key on")
	}
	if capExtLiteH6&(capExtMConnXS|capExtZDU|capExtHalfClose|1<<16) != 0 {
		t.Fatal("bit clash")
	}
	if h6PeerOK(nil) {
		t.Fatal("no link: no peer")
	}
	// the avoid mark is taken out and seen
	av, ok := takeH6Mark([]any{1, h6Mark{}, 2})
	if !ok || len(av) != 2 {
		t.Fatalf("takeH6Mark %v %v", av, ok)
	}
	if _, ok := takeH6Mark([]any{hedgeMark{}}); ok {
		t.Fatal("hedge mark read as h6")
	}
}

// exit_first: a role-8 sub before the raw sub waits for its conn (pending
// attach) up to h6Wait, else it is dropped.
func TestB469nH6ExitFirst(t *testing.T) {
	setH6(t, true)
	old := h6Wait
	h6Wait = 150 * time.Millisecond
	t.Cleanup(func() { h6Wait = old })
	tbl := newMConnTable()
	stop := make(chan struct{})
	defer close(stop)
	mc := newMConnExitRaw(7)
	defer mc.Close()
	go func() {
		time.Sleep(40 * time.Millisecond)
		if tbl.reserve(7, mc) {
			tbl.publish(7, mc)
		}
	}()
	f0, j0, d0 := h6Wire.first.Load(), h6Wire.joined.Load(), h6Wire.dropped.Load()
	if got := h6Join(tbl, 7, stop); got != mc {
		t.Fatal("pending role-8 sub not attached to the late raw conn")
	}
	if h6Wire.first.Load() != f0+1 || h6Wire.joined.Load() != j0+1 {
		t.Fatal("exit_first/joined counters")
	}
	t0 := time.Now()
	if h6Join(tbl, 8, stop) != nil {
		t.Fatal("joined a conn that never came")
	}
	if d := time.Since(t0); d < 140*time.Millisecond || d > time.Second {
		t.Fatalf("drop after %v", d)
	}
	if h6Wire.dropped.Load() != d0+1 {
		t.Fatal("dropped counter")
	}
	// a conn without h6 (records first sub): dropped at once
	mr := newMConnExit(9)
	defer mr.Close()
	if tbl.reserve(9, mr) {
		tbl.publish(9, mr)
	}
	if h6Join(tbl, 9, stop) != nil {
		t.Fatal("joined a conn without h6")
	}
}

// The copy ends on its own at until with fewer than h6N bytes (a short
// request, the half-close case): the end frame carries the sent count, the
// exit's copy ends and frees once delivered; the conn's EOF comes after the
// bytes.
func TestB469nH6ShortEnd(t *testing.T) {
	setH6(t, true)
	_, iran, exit := newRawPair(t)
	done := h6Pipe(t, iran, exit, time.Now().Add(50*time.Millisecond), 0)
	up := randBytes(1000, 31)
	both(t,
		func() error { return xfer(iran, up, 1000, 0) },
		func() error { return readN(exit, up) },
	)
	if done() != 0 {
		t.Fatal("bytes on the copy sub")
	}
	exit.h6mu.Lock()
	h := *exit.h6
	exit.h6mu.Unlock()
	if !h.ended || !h.freed || h.d != 1000 {
		t.Fatalf("copy state ended=%v freed=%v d=%d", h.ended, h.freed, h.d)
	}
	iran.Close()
	res := make(chan error, 1)
	go func() {
		p := make([]byte, 10)
		n, err := exit.Read(p)
		if n != 0 {
			err = bytes.ErrTooLarge
		}
		res <- err
	}()
	select {
	case err := <-res:
		if err == nil || err == bytes.ErrTooLarge {
			t.Fatalf("after close: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no EOF after the request")
	}
	closeAndWait(t, iran, exit)
}

type h6WrapSub struct{ mSub }

func (w h6WrapSub) mconnRaw() bool { return true }
func (w h6WrapSub) mconnH6() bool  { return true }
func (w h6WrapSub) SetReadDeadline(t time.Time) error {
	return w.mSub.(readDeadliner).SetReadDeadline(t)
}

// Iran's side end to end: a first sub toward an h6 exit starts the copy
// (h6StartLocked), the copy sub is opened with the h6 mark (role 8), the
// exit merges it; byte exact, the copy sub never part of Iran's subs.
func TestB469nH6IranStart(t *testing.T) {
	setH6(t, true)
	setLite(t, true)
	resetLiteBudget()
	n := &tNet{exit: newMConnExitRaw(5), raw: true}
	marked := make(chan struct{}, 1)
	first := true
	open := func(p mPlane, avoid []any) (mSub, any, error) {
		if av, ok := takeH6Mark(avoid); ok {
			if len(av) == 0 {
				return nil, nil, errMConnNoLink
			}
			a, b := net.Pipe()
			go func() { h6ReadLoop(b, n.exit); b.Close() }()
			marked <- struct{}{}
			return a, "h6", nil
		}
		s, k, err := n.open(p, avoid)
		if err == nil && first {
			first = false
			s = h6WrapSub{s}
		}
		return s, k, err
	}
	o0 := h6Wire.opened.Load()
	iran, err := newMConnIran(5, open)
	if err != nil {
		t.Fatal(err)
	}
	closeAtEnd(t, iran, n.exit)
	if iran.h6s == nil || iran.raw1 == nil {
		t.Fatal("copy not started on a raw first sub")
	}
	select {
	case <-marked:
	case <-time.After(2 * time.Second):
		t.Fatal("no role-8 open")
	}
	up := randBytes(6000, 41)
	both(t,
		func() error { return xfer(iran, up, 1200, 0) },
		func() error { return readN(n.exit, up) },
	)
	mWaitFor(t, "copy received", 2*time.Second, func() bool {
		n.exit.h6mu.Lock()
		defer n.exit.h6mu.Unlock()
		return n.exit.h6.hEnd == uint64(h6N) || n.exit.h6.freed
	})
	if h6Wire.opened.Load() != o0+1 {
		t.Fatal("opened counter")
	}
	if liveSubs(iran) != 1 {
		t.Fatalf("iran subs %d: the copy sub must not be an msub", liveSubs(iran))
	}
	closeAndWait(t, iran, n.exit)
}
