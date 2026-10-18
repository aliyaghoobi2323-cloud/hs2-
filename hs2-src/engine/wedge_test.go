package engine

import (
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xtaci/smux"
)

// guardedPair is a real smux session pair over loopback TCP built by
// newSession, as on a link: the edge (client) side carries a meter, so its
// guard is reachable through edgeMtr.guard.
func guardedPair(t *testing.T) (edge, exit *smux.Session, edgeMtr *linkMeter) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	acc := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		acc <- c
	}()
	a, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	b := <-acc
	edgeMtr = &linkMeter{statsPoll: make(chan struct{}, 1)}
	edge, _, err = newSession(a, false, nil, edgeMtr)
	if err != nil {
		t.Fatal(err)
	}
	exit, _, err = newSession(b, true, nil, &linkMeter{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { edge.Close(); exit.Close(); a.Close(); b.Close() })
	if edgeMtr.guard == nil {
		t.Fatal("newSession left no guard in the meter")
	}
	return edge, exit, edgeMtr
}

// pushForever writes to w until it fails, counting nothing.
func pushForever(w io.Writer) {
	buf := make([]byte, 16<<10)
	for {
		if _, err := w.Write(buf); err != nil {
			return
		}
	}
}

// wedgeRig is a guarded smux link with user relays whose local apps read at
// a chosen pace; every relay is ended when the test ends.
type wedgeRig struct {
	t          *testing.T
	edge, exit *smux.Session
	g          *sessGuard
	clock      time.Time // synthetic look clock (lookFast)
}

type wUser struct {
	app   net.Conn
	ended chan struct{}
	got   atomic.Int64
}

// newWedgeRig: manual = the test drives the guard's looks (lookFast); else
// the production guard loop looks at it on its own tick.
func newWedgeRig(t *testing.T, manual bool) *wedgeRig {
	edge, exit, mtr := guardedPair(t)
	mtr.guard.manual.Store(manual)
	return &wedgeRig{t: t, edge: edge, exit: exit, g: mtr.guard, clock: time.Now()}
}

// open starts a user whose exit side pushes as fast as allowed; read is the
// app's reading loop (nil: an app that never reads).
func (r *wedgeRig) open(read func(u *wUser)) *wUser {
	r.t.Helper()
	return r.openPush(read, pushForever)
}

// openPush is open with push as what the exit side does with the stream.
func (r *wedgeRig) openPush(read func(u *wUser), push func(w io.Writer)) *wUser {
	r.t.Helper()
	st, err := r.edge.OpenStream()
	if err != nil {
		r.t.Fatal(err)
	}
	if _, err := st.Write([]byte{1}); err != nil {
		r.t.Fatal(err)
	}
	xs, err := r.exit.AcceptStream()
	if err != nil {
		r.t.Fatal(err)
	}
	var one [1]byte
	io.ReadFull(xs, one[:])
	go push(xs)
	local, app := net.Pipe()
	u := &wUser{app: app, ended: make(chan struct{})}
	go func() { relayStream(local, st, r.g); close(u.ended) }()
	if read != nil {
		go read(u)
	}
	r.t.Cleanup(func() { // no relay outlives its test
		app.Close()
		select {
		case <-u.ended:
		case <-time.After(10 * time.Second):
		}
	})
	return u
}

// readAt returns an app reading about rate bytes/s (0: as fast as it can).
func readAt(rate int) func(u *wUser) {
	return func(u *wUser) {
		buf := make([]byte, 4<<10)
		for {
			n, err := u.app.Read(buf)
			u.got.Add(int64(n))
			if err != nil {
				return
			}
			if rate > 0 {
				time.Sleep(time.Duration(n) * time.Second / time.Duration(rate))
			}
		}
	}
}

// openHeld opens n users whose apps never read, each seen in a write (its
// relay holding data it cannot hand over) before any of them fills the
// link's buffer: more of them than the buffer holds, one opened after it
// filled would get its first data only once the guard released the others,
// on a link that then never fills again.
func (r *wedgeRig) openHeld(n int) []*wUser {
	r.t.Helper()
	release := make(chan struct{})
	r.t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	held := func(w io.Writer) {
		w.Write(make([]byte, 4<<10))
		<-release
		pushForever(w)
	}
	var users []*wUser
	for i := 0; i < n; i++ {
		users = append(users, r.openPush(nil, held))
		r.waitWriting(i + 1)
	}
	close(release)
	return users
}

// waitWriting waits until n of the link's relays are in a local write
// (holding data their app does not take).
func (r *wedgeRig) waitWriting(n int) {
	r.t.Helper()
	for end := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		r.g.mu.Lock()
		w := 0
		for rw := range r.g.relays {
			if rw.wseq.Load()&1 == 1 {
				w++
			}
		}
		r.g.mu.Unlock()
		if w >= n {
			return
		}
		if time.Now().After(end) {
			r.t.Fatalf("%d relays in a write after 5 s, want %d", w, n)
		}
	}
}

// lookFast advances the synthetic clock one production tick per look (real
// time 150 ms) — for cases without slow readers, whose pace is real time.
func (r *wedgeRig) lookFast() int {
	time.Sleep(150 * time.Millisecond)
	r.clock = r.clock.Add(guardTick)
	k, _ := r.g.lookAt(r.clock)
	return k
}

func ended(u *wUser) bool {
	select {
	case <-u.ended:
		return true
	default:
		return false
	}
}

// smallSessionBuffer shrinks the link's smux buffers for one test so that a
// handful of apps that never read fill the session buffer: with the adaptive
// stream window (mtcp_link.go) such an app holds only the 256 KiB it is first
// offered, so it takes some 32 of them to fill the production 8 MiB — the
// guard's job when it is full is the same.
func smallSessionBuffer(t *testing.T) {
	stream, sess := SmuxStreamBuffer, SmuxSessionBuffer
	SmuxStreamBuffer, SmuxSessionBuffer = 512<<10, 1<<20
	t.Cleanup(func() { SmuxStreamBuffer, SmuxSessionBuffer = stream, sess })
}

// Five users whose apps stop reading fill the link's whole receive buffer:
// without the guard every other stream on the link would stop for good (and
// the other side's TCP would kill the link). The guard must close exactly the
// stuck ones, and the healthy user must keep receiving on the same link.
func TestWedgeGuardReleasesStuckReaders(t *testing.T) {
	smallSessionBuffer(t)
	r := newWedgeRig(t, true)
	stuck := r.openHeld(5)
	healthy := r.open(readAt(0))
	killed := 0
	for i := 0; i < 15 && killed < len(stuck); i++ {
		killed += r.lookFast()
	}
	if killed != len(stuck) {
		t.Fatalf("guard closed %d stuck relays, want %d", killed, len(stuck))
	}
	// the stuck check reads this: the link was held up by its own users
	if p := r.g.parkedAt.Load(); p == 0 || ctrlNow()-p > int64(5*time.Second) {
		t.Fatalf("a parked reader left no recent parkedAt (%d)", p)
	}
	for i, u := range stuck {
		select {
		case <-u.ended:
		case <-time.After(5 * time.Second):
			t.Fatalf("stuck relay %d did not end after the guard closed it", i)
		}
	}
	if r.edge.IsClosed() || ended(healthy) {
		t.Fatal("the link or the healthy relay was closed")
	}
	before := healthy.got.Load()
	time.Sleep(time.Second)
	if healthy.got.Load()-before < 64<<10 {
		t.Fatalf("healthy user moved only %d bytes in 1s after the release", healthy.got.Load()-before)
	}
	for i := 0; i < 6; i++ { // a flowing link never gets anything closed
		if k := r.lookFast(); k != 0 {
			t.Fatalf("guard closed %d relays on a flowing link", k)
		}
	}
}

// A single app that stops reading does not fill the link's buffer (its stream
// window is at most a quarter of it): the link keeps flowing and the guard
// leaves that connection alone — a paused reader is not a reason to cut it.
func TestWedgeGuardSparesLonePausedReader(t *testing.T) {
	r := newWedgeRig(t, true)
	paused := r.open(nil)
	for i := 0; i < 12; i++ {
		if k := r.lookFast(); k != 0 {
			t.Fatalf("guard closed a lone paused reader (look %d)", i)
		}
	}
	if ended(paused) {
		t.Fatal("relay ended")
	}
}

// A slow reader next to the stalled ones returns tokens a trickle at a time,
// so the link is never quite at zero reads; that must not hide the stall
// (it used to, for 23 s and more — past the other side's 20 s), and the slow
// reader, which is still reading, must not be closed with the stalled ones.
// This runs the production guard loop on its own 2 s tick: the slow reader's
// pace is real time.
func TestWedgeGuardTricklingReaderDoesNotHideStall(t *testing.T) {
	if testing.Short() {
		t.Skip("real-time guard ticks")
	}
	smallSessionBuffer(t)
	r := newWedgeRig(t, false)
	stuck := r.openHeld(6)
	slow := r.open(readAt(16 << 10))
	fast := r.open(readAt(0))
	start := time.Now()
	for _, u := range stuck {
		select {
		case <-u.ended:
		case <-time.After(20*time.Second - time.Since(start)):
			t.Fatalf("a stalled relay still holds the link after %s (the other side gives up at 20 s)", time.Since(start).Round(time.Second))
		}
	}
	took := time.Since(start)
	if took > 12*time.Second {
		t.Fatalf("released only after %s", took)
	}
	if ended(slow) || ended(fast) {
		t.Fatal("a reading relay was closed with the stalled ones")
	}
	slowAt, fastAt := slow.got.Load(), fast.got.Load()
	time.Sleep(3 * time.Second)
	if slow.got.Load() == slowAt || fast.got.Load()-fastAt < 1<<20 {
		t.Fatalf("after the release: slow +%d B, fast +%d B in 3 s", slow.got.Load()-slowAt, fast.got.Load()-fastAt)
	}
	t.Logf("stalled relays released after %s; slow reader kept (%d B), fast reader kept (%d B)", took.Round(100*time.Millisecond), slow.got.Load(), fast.got.Load())
}

// When the link dies under a relay whose local app neither reads nor writes,
// the relay must still end (it used to hold the stream's buffers and its
// goroutines forever).
func TestRelayEndsWhenStreamDies(t *testing.T) {
	old := relayDieGrace.Load()
	relayDieGrace.Store(int64(300 * time.Millisecond))
	defer relayDieGrace.Store(old)

	edge, exit, _ := guardedPair(t)
	st, _ := edge.OpenStream()
	st.Write([]byte{1})
	xs, _ := exit.AcceptStream()
	var one [1]byte
	io.ReadFull(xs, one[:])
	xs.Write(make([]byte, 64<<10)) // buffered at the edge, never taken
	local, app := net.Pipe()       // the app never reads or writes
	defer app.Close()
	done := make(chan struct{})
	go func() { relayStream(local, st, nil); close(done) }()
	time.Sleep(100 * time.Millisecond)
	exit.Close() // the link dies
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("relay still running 5s after its stream's session died")
	}
}

// Readers that keep reading but more slowly than the link delivers (a phone on
// a weak connection pulling a large download) must not stall the other users
// of their link. With a fixed 2 MiB stream window each held 1–2 MiB of the
// 8 MiB session buffer, so a handful emptied it and the fast user crawled at
// their pace (the guard cannot help: their writes complete). The adaptive
// window keeps each to a few hundred KiB.
func TestSlowReadersDoNotStallLink(t *testing.T) {
	if testing.Short() {
		t.Skip("real-time readers")
	}
	r := newWedgeRig(t, true)
	for i := 0; i < 12; i++ {
		r.open(readAt(256 << 10))
	}
	fast := r.open(readAt(0))
	time.Sleep(3 * time.Second) // the slow readers' windows settle
	before := fast.got.Load()
	time.Sleep(3 * time.Second)
	if got := fast.got.Load() - before; got < 32<<20 {
		t.Fatalf("fast user moved %d KiB in 3 s next to 12 slow readers, want 32 MiB or more", got>>10)
	}
}

// readFastThenAt reads as fast as it can until ramp has passed since it was
// made, then about rate bytes/s.
func readFastThenAt(ramp time.Duration, rate int) func(u *wUser) {
	until := time.Now().Add(ramp)
	return func(u *wUser) {
		buf := make([]byte, 4<<10)
		for {
			n, err := u.app.Read(buf)
			u.got.Add(int64(n))
			if err != nil {
				return
			}
			if time.Now().After(until) {
				time.Sleep(time.Duration(n) * time.Second / time.Duration(rate))
			}
		}
	}
}

// Readers that keep up and then slow down (a player whose buffer filled, a
// phone moving to a weak connection) were granted the full window while they
// kept up; the adaptive window used to shrink it only at their next update,
// after they had read half of it at their new pace, so a dozen of them held
// the whole session buffer and the link's other users stalled until they had
// read it (measured: 10-17 s and more at 0 B/s). smux now takes the windows
// back as soon as the backlogs pile up. What the exit had already sent still
// arrives, and on this loopback rig (no TCP_NOTSENT_LOWAT, socket buffers of
// several MiB) that can still stall the link for a few seconds now and then;
// the test asks only that it recovers.
func TestReadersSlowingDownDoNotStallLink(t *testing.T) {
	if testing.Short() {
		t.Skip("real-time readers")
	}
	r := newWedgeRig(t, true)
	for i := 0; i < 12; i++ {
		r.open(readFastThenAt(3*time.Second, 32<<10))
	}
	fast := r.open(readAt(0))
	time.Sleep(4 * time.Second) // the readers have slowed down
	before := fast.got.Load()
	time.Sleep(10 * time.Second)
	if got := fast.got.Load() - before; got < 64<<20 {
		t.Fatalf("fast user moved %d KiB in the 10 s after 12 readers slowed down, want 64 MiB or more", got>>10)
	}
}
