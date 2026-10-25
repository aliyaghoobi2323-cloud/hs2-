package engine

import (
	"io"
	"net"
	"testing"
	"time"

	"github.com/xtaci/smux"
)

// The smux config turns on windows past SmuxNeedStreamBuffer (under the
// window core's ceilings) from the engine's sizes, takes the process's pool,
// and keeps a valid config under the lab overrides: a fixed window stays the
// 2 MiB it was, and a stream buffer too small for a window past the need
// leaves it off (the wedge tests shrink the buffers).
func TestSmuxConfigWindowPool(t *testing.T) {
	c := newSmuxConfig()
	if c.NeedStreamBuffer != SmuxNeedStreamBuffer || c.MaxStreamBuffer != SmuxStreamBuffer || c.WindowPool == nil || c.WindowPool != windowPool() {
		t.Fatalf("need %d max %d pool %p, want %d, %d and the process's pool", c.NeedStreamBuffer, c.MaxStreamBuffer, c.WindowPool, SmuxNeedStreamBuffer, SmuxStreamBuffer)
	}
	if err := smux.VerifyConfig(c); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name              string
		min, stream, sess int
		wantNeed, wantMax int
	}{
		{"fixed window (lab)", 0, SmuxStreamBuffer, SmuxSessionBuffer, 0, SmuxNeedStreamBuffer},
		{"small buffers (wedge tests)", SmuxMinStreamBuffer, 512 << 10, 1 << 20, 0, 512 << 10},
		{"stream buffer at the need", SmuxMinStreamBuffer, SmuxNeedStreamBuffer, SmuxSessionBuffer, 0, SmuxNeedStreamBuffer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			min, stream, sess := SmuxMinStreamBuffer, SmuxStreamBuffer, SmuxSessionBuffer
			SmuxMinStreamBuffer, SmuxStreamBuffer, SmuxSessionBuffer = tc.min, tc.stream, tc.sess
			defer func() { SmuxMinStreamBuffer, SmuxStreamBuffer, SmuxSessionBuffer = min, stream, sess }()
			c := newSmuxConfig()
			if c.NeedStreamBuffer != tc.wantNeed || c.MaxStreamBuffer != tc.wantMax {
				t.Fatalf("need %d max %d, want %d %d", c.NeedStreamBuffer, c.MaxStreamBuffer, tc.wantNeed, tc.wantMax)
			}
			if err := smux.VerifyConfig(c); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A lab need or adaptive window that leaves no room for windows past the
// need keeps a fixed window the link's buffer holds (no pool brings the
// rest): a valid config, not a 16 MiB window that smux refuses on every
// link. A smallest window above it turns the adaptive window off, as with
// the 2 MiB window before the pool.
func TestSmuxConfigFixedWindowFitsTheBuffer(t *testing.T) {
	for _, tc := range []struct {
		name            string
		need, min       int
		wantMax, wantLo int
	}{
		{"need 0", 0, SmuxMinStreamBuffer, smuxFixedWindow, SmuxMinStreamBuffer},
		{"need below the adaptive window", 32 << 10, SmuxMinStreamBuffer, 32 << 10, 0},
		{"need at the stream buffer", SmuxStreamBuffer, SmuxMinStreamBuffer, SmuxSessionBuffer, SmuxMinStreamBuffer},
		{"need past the stream buffer", 2 * SmuxStreamBuffer, SmuxMinStreamBuffer, SmuxSessionBuffer, SmuxMinStreamBuffer},
		{"adaptive window above the need", SmuxNeedStreamBuffer, 3 << 20, SmuxNeedStreamBuffer, 0},
		{"adaptive window at twice the need", SmuxNeedStreamBuffer, 4 << 20, SmuxNeedStreamBuffer, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			need, min := SmuxNeedStreamBuffer, SmuxMinStreamBuffer
			SmuxNeedStreamBuffer, SmuxMinStreamBuffer = tc.need, tc.min
			defer func() { SmuxNeedStreamBuffer, SmuxMinStreamBuffer = need, min }()
			c := newSmuxConfig()
			if err := smux.VerifyConfig(c); err != nil {
				t.Fatalf("need %d max %d min %d pool %v: %v", c.NeedStreamBuffer, c.MaxStreamBuffer, c.MinStreamBuffer, c.WindowPool != nil, err)
			}
			if c.WindowPool != nil || c.NeedStreamBuffer != 0 || c.MaxStreamBuffer != tc.wantMax || c.MinStreamBuffer != tc.wantLo {
				t.Fatalf("need %d max %d min %d pool %v, want a fixed %d window, smallest %d", c.NeedStreamBuffer, c.MaxStreamBuffer, c.MinStreamBuffer, c.WindowPool != nil, tc.wantMax, tc.wantLo)
			}
			if SmuxTuningNotes() == nil {
				t.Fatal("no tuning note says windows past the need are off")
			}
		})
	}
	if n := SmuxTuningNotes(); n != nil {
		t.Fatalf("the default config: %q", n)
	}
}

// Without a ceiling from the window core a fast download's window stays
// within SmuxNeedStreamBuffer: the real link stack takes nothing from the
// pool for it and its buffer stays SmuxSessionBuffer.
func TestFastLinkWithoutCeilingTakesNoPool(t *testing.T) {
	edge, exit, _ := guardedPair(t)
	free := windowPool().Free()
	go func() {
		st, err := exit.AcceptStream()
		if err != nil {
			return
		}
		buf := make([]byte, 32<<10)
		for n := 0; n < 96<<20; n += len(buf) {
			if _, err := st.Write(buf); err != nil {
				return
			}
		}
		st.Close()
	}()
	st, err := edge.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	st.Write([]byte{0})
	st.SetReadDeadline(time.Now().Add(60 * time.Second))
	if n, err := io.Copy(io.Discard, st); (err != nil && err != io.EOF) || n != 96<<20 { // (smux's WriteTo ends with io.EOF)
		t.Fatalf("read %d, %v", n, err)
	}
	if edge.Extra() != 0 || windowPool().Free() != free {
		t.Fatalf("a download without a ceiling held %d past SmuxSessionBuffer, pool %d of %d", edge.Extra(), windowPool().Free(), free)
	}
}

// openFrozen opens n users whose apps never read. The exit side of each is
// accepted and fed on goroutines of its own: once the link's buffer is full,
// the exit's first read of a new stream waits for its window update to get
// through the stuck link, and the test must go on driving the guard
// meanwhile (waiting on it here hung the test whenever the pushers filled
// the buffer before the last user was open: a loaded machine).
func (r *wedgeRig) openFrozen(n int) []*wUser {
	r.t.Helper()
	go func() {
		for i := 0; i < n; i++ {
			xs, err := r.exit.AcceptStream()
			if err != nil {
				return
			}
			go func() {
				var one [1]byte
				if _, err := io.ReadFull(xs, one[:]); err == nil {
					pushForever(xs)
				}
			}()
		}
	}()
	var users []*wUser
	for i := 0; i < n; i++ {
		st, err := r.edge.OpenStream()
		if err != nil {
			r.t.Fatal(err)
		}
		if _, err := st.Write([]byte{1}); err != nil {
			r.t.Fatal(err)
		}
		local, app := net.Pipe()
		u := &wUser{app: app, ended: make(chan struct{})}
		go func() { relayStream(local, st, r.g); close(u.ended) }()
		r.t.Cleanup(func() {
			app.Close()
			select {
			case <-u.ended:
			case <-time.After(10 * time.Second):
			}
		})
		users = append(users, u)
	}
	return users
}

// With the production buffers, apps that never read and fill the link's 8
// MiB make its buffer borrow up to twice as much again from the window pool
// (no window past SmuxNeedStreamBuffer needed), and the link's other users
// keep moving; once they fill that too, the session waits as before and the
// wedge guard closes exactly the stuck relays. All the pool comes back once
// they are gone.
func TestFrozenAppsFillTheGrownBufferThenTheGuard(t *testing.T) {
	if testing.Short() {
		t.Skip("real-time readers")
	}
	free := windowPool().Free()
	r := newWedgeRig(t, true)
	healthy := r.open(readAt(0))
	// each holds the 256 KiB a stream starts with: 40 of them, 10 MiB
	stuck := r.openFrozen(40)
	time.Sleep(time.Second)
	before := healthy.got.Load()
	time.Sleep(time.Second)
	if healthy.got.Load()-before < 4<<20 {
		t.Fatalf("healthy user moved %d KiB in 1 s next to 10 MiB of frozen apps", (healthy.got.Load()-before)>>10)
	}
	if e := r.edge.Extra(); e <= 0 || e > 2*SmuxSessionBuffer {
		t.Fatalf("buffer past SmuxSessionBuffer: %d, want some, at most %d", e, 2*SmuxSessionBuffer)
	}
	for i := 0; i < 4; i++ { // a link that flows gets nothing closed
		if k := r.lookFast(); k != 0 {
			t.Fatalf("guard closed %d relays while the grown buffer had room", k)
		}
	}
	// 100 more: 35 MiB, past three times SmuxSessionBuffer
	stuck = append(stuck, r.openFrozen(100)...)
	killed := 0
	for i := 0; i < 40 && killed < len(stuck); i++ {
		killed += r.lookFast()
	}
	if killed != len(stuck) {
		t.Fatalf("guard closed %d stuck relays, want %d", killed, len(stuck))
	}
	if ended(healthy) {
		t.Fatal("the healthy relay was closed")
	}
	before = healthy.got.Load()
	time.Sleep(time.Second)
	if healthy.got.Load()-before < 4<<20 {
		t.Fatalf("healthy user moved only %d KiB in 1 s after the release", (healthy.got.Load()-before)>>10)
	}
	deadline := time.Now().Add(5 * time.Second)
	for r.edge.Extra() != 0 || windowPool().Free() != free {
		if time.Now().After(deadline) {
			t.Fatalf("after the release: buffer %d past SmuxSessionBuffer, pool %d of %d", r.edge.Extra(), windowPool().Free(), free)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
