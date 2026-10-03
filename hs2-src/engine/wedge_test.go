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

// Four users whose apps stop reading fill the link's whole receive buffer:
// without the guard every other stream on the link would stop for good (and
// the other side's TCP would kill the link). The guard must close exactly the
// stuck ones, and the healthy user must keep receiving on the same link.
func TestWedgeGuardReleasesStuckReaders(t *testing.T) {
	edge, exit, mtr := guardedPair(t)
	g := mtr.guard

	type user struct {
		local  net.Conn // relay's side
		app    net.Conn // the user's app (reads or not)
		closed atomic.Bool
		ended  chan struct{}
	}
	open := func() *user {
		st, err := edge.OpenStream()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.Write([]byte{1}); err != nil {
			t.Fatal(err)
		}
		xs, err := exit.AcceptStream()
		if err != nil {
			t.Fatal(err)
		}
		var one [1]byte
		io.ReadFull(xs, one[:])
		go pushForever(xs) // the exit sends the download as fast as allowed
		local, app := net.Pipe()
		u := &user{local: local, app: app, ended: make(chan struct{})}
		go func() {
			relayStream(local, st, g)
			close(u.ended)
		}()
		return u
	}
	const stuckN = 5
	var stuck []*user
	for i := 0; i < stuckN; i++ {
		stuck = append(stuck, open()) // their apps never read
	}
	healthy := open()
	var got atomic.Int64
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := healthy.app.Read(buf)
			got.Add(int64(n))
			if err != nil {
				return
			}
			time.Sleep(2 * time.Millisecond) // a slow but live reader
		}
	}()

	// Drive the guard faster than its production tick.
	deadline := time.Now().Add(20 * time.Second)
	killed := 0
	for time.Now().Before(deadline) && killed < stuckN {
		time.Sleep(150 * time.Millisecond)
		k, _ := g.look()
		killed += k
	}
	if killed != stuckN {
		t.Fatalf("guard closed %d stuck relays, want %d", killed, stuckN)
	}
	for i, u := range stuck {
		select {
		case <-u.ended:
		case <-time.After(5 * time.Second):
			t.Fatalf("stuck relay %d did not end after the guard closed it", i)
		}
	}
	// The healthy user is still on the same, still-open link, and moving.
	if edge.IsClosed() {
		t.Fatal("the link died")
	}
	select {
	case <-healthy.ended:
		t.Fatal("the guard closed the healthy relay")
	default:
	}
	before := got.Load()
	time.Sleep(time.Second)
	if got.Load()-before < 64<<10 {
		t.Fatalf("healthy user moved only %d bytes in 1s after the release", got.Load()-before)
	}
	// And a healthy, flowing link never gets anything closed.
	for i := 0; i < 6; i++ {
		if k, _ := g.look(); k != 0 {
			t.Fatalf("guard closed %d relays on a flowing link", k)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// A single app that stops reading does not fill the link's buffer (its stream
// window is a quarter of it): the link keeps flowing and the guard must leave
// that connection alone — a paused reader is not a reason to cut it.
func TestWedgeGuardSparesLonePausedReader(t *testing.T) {
	edge, exit, mtr := guardedPair(t)
	g := mtr.guard
	st, _ := edge.OpenStream()
	st.Write([]byte{1})
	xs, _ := exit.AcceptStream()
	var one [1]byte
	io.ReadFull(xs, one[:])
	go pushForever(xs)
	local, app := net.Pipe()
	defer app.Close()
	ended := make(chan struct{})
	go func() { relayStream(local, st, g); close(ended) }()
	for i := 0; i < 12; i++ {
		time.Sleep(150 * time.Millisecond)
		if k, _ := g.look(); k != 0 {
			t.Fatalf("guard closed a lone paused reader (look %d)", i)
		}
	}
	select {
	case <-ended:
		t.Fatal("relay ended")
	default:
	}
}

// When the link dies under a relay whose local app neither reads nor writes,
// the relay must still end (it used to hold the stream's buffers and its
// goroutines forever).
func TestRelayEndsWhenStreamDies(t *testing.T) {
	old := relayDieGrace
	relayDieGrace = 300 * time.Millisecond
	defer func() { relayDieGrace = old }()

	edge, exit, _ := guardedPair(t)
	st, _ := edge.OpenStream()
	st.Write([]byte{1})
	xs, _ := exit.AcceptStream()
	var one [1]byte
	io.ReadFull(xs, one[:])
	xs.Write(make([]byte, 64<<10)) // buffered at the edge, never taken
	local, app := net.Pipe()       // the app never reads or writes
	defer app.Close()
	ended := make(chan struct{})
	go func() { relayStream(local, st, nil); close(ended) }()
	time.Sleep(100 * time.Millisecond)
	exit.Close() // the link dies
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("relay still running 5s after its stream's session died")
	}
}
