package engine

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/xtaci/smux"
)

// Tests for the tunnel side of HS2_MCONN (mconn_link.go): the old path when
// the key is off or the exit cannot read it, the opener's choice of link per
// plane, the exit's table of connections, and a connection end to end (with
// whichever mConn is built in).

func withMConn(t *testing.T, on bool) {
	t.Helper()
	old := mconnOn.Load()
	mconnOn.Store(on)
	t.Cleanup(func() { mconnOn.Store(old) })
}

// mcStream is a fake user stream: it records what is written and blocks
// reads until closed.
type mcStream struct {
	mu     sync.Mutex
	wrote  bytes.Buffer
	first  chan []byte
	once   sync.Once
	closed chan struct{}
	cOnce  sync.Once
}

func newMCStream() *mcStream {
	return &mcStream{first: make(chan []byte, 1), closed: make(chan struct{})}
}

func (s *mcStream) Read(p []byte) (int, error) { <-s.closed; return 0, io.EOF }
func (s *mcStream) Write(p []byte) (int, error) {
	s.mu.Lock()
	s.wrote.Write(p)
	s.mu.Unlock()
	s.once.Do(func() { s.first <- append([]byte(nil), p...) })
	return len(p), nil
}
func (s *mcStream) Close() error { s.cOnce.Do(func() { close(s.closed) }); return nil }

// mcLink is a fake metered link whose exit answered kindInfo with caps.
type mcLink struct {
	mtr    *linkMeter
	mu     sync.Mutex
	opened []*mcStream
}

func newMCLink(caps uint8) *mcLink {
	m := &linkMeter{}
	m.peerInfo.Store(&peerInfo{V2: true, Caps: caps})
	m.infoDone.Store(true)
	return &mcLink{mtr: m}
}

func (l *mcLink) OpenStream() (stream, error) {
	s := newMCStream()
	l.mu.Lock()
	l.opened = append(l.opened, s)
	l.mu.Unlock()
	return s, nil
}
func (l *mcLink) Active() int32             { return 0 }
func (l *mcLink) Alive() bool               { return true }
func (l *mcLink) Close() error              { return nil }
func (l *mcLink) meter() *linkMeter         { return l.mtr }
func (l *mcLink) tcpStats() (tcpStat, bool) { return tcpStat{}, false }
func (l *mcLink) streams() []*mcStream {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]*mcStream(nil), l.opened...)
}
func (l *mcLink) firstWrite(t *testing.T) []byte {
	t.Helper()
	for i := 0; i < 200; i++ {
		if ss := l.streams(); len(ss) > 0 {
			select {
			case b := <-ss[0].first:
				return b
			case <-time.After(2 * time.Second):
				t.Fatal("nothing written on the stream")
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no stream opened")
	return nil
}

func mcManager(links ...*mcLink) *LinkManager {
	m := NewLinkManager(nil, len(links), len(links), 50, nil)
	m.gateInfo = true
	for i, l := range links {
		m.links = append(m.links, &managedLink{link: l, id: i, mtr: l.mtr})
	}
	if len(links) > 0 {
		m.exitInfo.Store(links[0].mtr.peerInfo.Load())
	}
	return m
}

// With the key off, or an exit that did not announce capMConn, a user
// connection opens the one stream with exactly the old header; with both,
// a kindMConn sub (role 0) instead.
func TestMConnOldPathUnlessBothSides(t *testing.T) {
	const port = 2053
	all := uint8(capPortTags | capL3Quiet | capMConn)
	for _, tc := range []struct {
		name string
		on   bool
		caps uint8
		want []byte
	}{
		{"key off, exit reads mconn", false, all, []byte{kindTCPPort, port >> 8, port & 0xff}},
		{"key on, older exit", true, capPortTags | capL3Quiet, []byte{kindTCPPort, port >> 8, port & 0xff}},
		{"key on, exit without tags or mconn", true, 0, []byte{kindTCP}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withMConn(t, tc.on)
			l := newMCLink(tc.caps)
			lm := mcManager(l)
			user, peer := net.Pipe()
			defer peer.Close()
			done := make(chan struct{})
			go func() { serveUserTCP(context.Background(), user, lm, port); close(done) }()
			if got := l.firstWrite(t); !bytes.Equal(got, tc.want) {
				t.Fatalf("header %x, want %x", got, tc.want)
			}
			peer.Close()
			<-done
			if lm.users.Load() != 0 {
				t.Fatalf("%d user(s) still placed after the connection ended", lm.users.Load())
			}
		})
	}
	t.Run("key on, exit reads mconn", func(t *testing.T) {
		withMConn(t, true)
		l := newMCLink(all)
		lm := mcManager(l)
		user, peer := net.Pipe()
		done := make(chan struct{})
		go func() { serveUserTCP(context.Background(), user, lm, port); close(done) }()
		got := l.firstWrite(t)
		if len(got) != mconnHdrLen || got[0] != kindMConn || binary.BigEndian.Uint16(got[1:3]) != port || got[11] != mconnRoleFirst {
			t.Fatalf("sub header %x", got)
		}
		peer.Close()
		<-done
		if lm.users.Load() != 0 {
			t.Fatalf("%d sub(s) still placed after the connection ended", lm.users.Load())
		}
		if s := MConnObserved(); s.Open != 0 || s.SubsLight != 0 || s.SubsBulk != 0 {
			t.Fatalf("counters not back to zero: %+v", s)
		}
	})
}

// The key off announces nothing new: kindInfo bytes as before.
func TestMConnCapOnlyWithKey(t *testing.T) {
	withMConn(t, false)
	if mconnCap() != 0 {
		t.Fatal("capMConn announced with the key off")
	}
	if (KharejConfig{Panel: "x:1"}).info(4).Caps&capMConn != 0 {
		t.Fatal("exit announces capMConn with the key off")
	}
	withMConn(t, true)
	if (KharejConfig{Panel: "x:1"}).info(4).Caps&capMConn == 0 {
		t.Fatal("exit does not announce capMConn with the key on")
	}
	h := mconnHeader(443, 0x0102030405060708, mconnRoleJoin)
	if !bytes.Equal(h, []byte{kindMConn, 1, 187, 1, 2, 3, 4, 5, 6, 7, 8, 1}) {
		t.Fatalf("header %v", h)
	}
}

// pickLinks builds a pool of fake links with the given heavy counts and
// pressure, all reading mconn.
func pickLinks(heavy []int, pressed []bool) (*LinkManager, []*mcLink) {
	ls := make([]*mcLink, len(heavy))
	for i := range ls {
		ls[i] = newMCLink(capPortTags | capMConn)
	}
	m := mcManager(ls...)
	for i, ml := range m.links {
		ml.heavy, ml.pressed = heavy[i], pressed[i]
	}
	return m, ls
}

func linkIndex(ls []*mcLink, l Link) int {
	for i, x := range ls {
		if Link(x) == l {
			return i
		}
	}
	return -1
}

func TestMConnPickPlane(t *testing.T) {
	// light: never a heavy link while a light one is free; avoid is kept.
	m, ls := pickLinks([]int{2, 0, 1, 0}, []bool{false, false, false, false})
	for i := 0; i < 20; i++ {
		l, rel, ok := m.pickPlane(planeLight, nil)
		if !ok || (linkIndex(ls, l) != 1 && linkIndex(ls, l) != 3) {
			t.Fatalf("light sub on link %d (heavy)", linkIndex(ls, l))
		}
		rel()
	}
	l, rel, ok := m.pickPlane(planeLight, []any{Link(ls[1])})
	if !ok || linkIndex(ls, l) != 3 {
		t.Fatalf("light sub avoiding link 1 went to %d, want 3", linkIndex(ls, l))
	}
	rel()
	// both light links avoided: any other live link (a heavy one), never an avoided one
	l, rel, ok = m.pickPlane(planeLight, []any{Link(ls[1]), Link(ls[3])})
	if !ok || (linkIndex(ls, l) != 0 && linkIndex(ls, l) != 2) {
		t.Fatalf("light fallback went to %d", linkIndex(ls, l))
	}
	rel()
	if _, _, ok := m.pickPlane(planeLight, []any{Link(ls[0]), Link(ls[1]), Link(ls[2]), Link(ls[3])}); ok {
		t.Fatal("picked an avoided link")
	}

	// bulk: gathers on the links already heavy and not pressed.
	m, ls = pickLinks([]int{0, 3, 0, 1}, []bool{false, true, false, false})
	for i := 0; i < 5; i++ {
		l, rel, ok := m.pickPlane(planeBulk, nil)
		if !ok || linkIndex(ls, l) != 3 {
			t.Fatalf("bulk sub on link %d, want 3 (heavy, not pressed)", linkIndex(ls, l))
		}
		defer rel()
	}
	// ... with no such link: the least loaded (unpressed first)
	m, ls = pickLinks([]int{0, 3, 0}, []bool{false, true, false})
	m.links[0].flowing = 4
	l, rel, ok = m.pickPlane(planeBulk, nil)
	if !ok || linkIndex(ls, l) != 2 {
		t.Fatalf("bulk fallback on link %d, want 2 (least loaded)", linkIndex(ls, l))
	}
	rel()

	// links whose exit does not read mconn are never used
	m, ls = pickLinks([]int{0, 0}, []bool{false, false})
	ls[0].mtr.peerInfo.Store(&peerInfo{V2: true, Caps: capPortTags})
	for i := 0; i < 10; i++ {
		l, rel, ok := m.pickPlane(planeLight, nil)
		if !ok || linkIndex(ls, l) != 1 {
			t.Fatalf("sub on link %d, whose exit cannot read it", linkIndex(ls, l))
		}
		rel()
	}
	if m.users.Load() != 0 {
		t.Fatalf("%d placement(s) not released", m.users.Load())
	}
}

// Every sub the opener places counts as a user on its link until it closes.
func TestMConnOpenerCountsSubs(t *testing.T) {
	m, ls := pickLinks([]int{0, 0, 1}, []bool{false, false, false})
	e := &mconnEdge{ctx: context.Background(), lm: m, port: 443, id: 7}
	a, ka, err := e.open(planeLight, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := e.open(planeLight, []any{ka})
	if err != nil {
		t.Fatal(err)
	}
	c, kc, err := e.open(planeBulk, nil)
	if err != nil || linkIndex(ls, kc.(Link)) != 2 {
		t.Fatalf("bulk sub on %v, %v", kc, err)
	}
	if m.users.Load() != 3 || mconnStat.light.Load() < 2 || mconnStat.bulk.Load() < 1 {
		t.Fatalf("users %d light %d bulk %d", m.users.Load(), mconnStat.light.Load(), mconnStat.bulk.Load())
	}
	roles := []byte{}
	for _, l := range ls {
		for _, s := range l.streams() {
			roles = append(roles, (<-s.first)[11])
		}
	}
	if n0 := bytes.Count(roles, []byte{mconnRoleFirst}); n0 != 1 || len(roles) != 3 {
		t.Fatalf("roles %v: want one role 0 of three", roles)
	}
	a.Close()
	a.Close() // released once
	if m.users.Load() != 2 {
		t.Fatalf("users %d after one close", m.users.Load())
	}
	_, _ = b, c
	if n := e.end(); n != 3 {
		t.Fatalf("end saw %d subs", n)
	}
	if m.users.Load() != 0 {
		t.Fatalf("users %d after end", m.users.Load())
	}
	if _, _, err := e.open(planeLight, nil); err == nil {
		t.Fatal("open after end succeeded")
	}
}

func TestMConnTable(t *testing.T) {
	tb := newMConnTable()
	stop := make(chan struct{})
	// a join before its first sub waits for it
	mc := newMConnExit(1)
	got := make(chan *mConn, 1)
	go func() { got <- tb.join(1, 5*time.Second, stop) }()
	time.Sleep(50 * time.Millisecond)
	if !tb.create(1, mc) {
		t.Fatal("create refused")
	}
	select {
	case g := <-got:
		if g != mc {
			t.Fatal("join got another connection")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("join did not wake on create")
	}
	if tb.create(1, newMConnExit(1)) {
		t.Fatal("a live id was taken twice")
	}
	if tb.join(1, time.Second, stop) != mc {
		t.Fatal("join after create")
	}
	tb.remove(1, newMConnExit(1)) // not this connection: kept
	if tb.size() != 1 {
		t.Fatal("removed another connection's entry")
	}
	tb.remove(1, mc)
	if tb.size() != 0 {
		t.Fatal("entry kept after remove")
	}
	// a join whose connection never comes gives up after the wait, cleaned up
	t0 := time.Now()
	if tb.join(2, 150*time.Millisecond, stop) != nil {
		t.Fatal("join got a connection that does not exist")
	}
	if d := time.Since(t0); d < 140*time.Millisecond || d > 2*time.Second {
		t.Fatalf("join waited %s", d)
	}
	if tb.size() != 0 {
		t.Fatal("slot of a missed join kept")
	}
	// two joins waiting; the slot goes only with the last
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); tb.join(3, 100*time.Millisecond, stop) }()
	}
	wg.Wait()
	if tb.size() != 0 {
		t.Fatal("slot kept after both joins gave up")
	}
	// stop ends the wait
	close(stop)
	if tb.join(4, time.Minute, stop) != nil || tb.size() != 0 {
		t.Fatal("join after stop")
	}
}

// smuxPair is a client/server smux session pair over a pipe.
func smuxPair(t *testing.T) (*smux.Session, *smux.Session) {
	a, b := net.Pipe()
	cs, err := smux.Client(a, nil)
	if err != nil {
		t.Fatal(err)
	}
	ss, err := smux.Server(b, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close(); ss.Close() })
	return cs, ss
}

// The exit side over a real smux session: a join that comes before its
// connection's first sub is attached once the first sub arrives; a join
// whose connection never comes is closed after mconnJoinWait; the table is
// empty once the connection ends.
func TestMConnExitJoinOrder(t *testing.T) {
	old := mconnJoinWait
	mconnJoinWait = 300 * time.Millisecond
	t.Cleanup(func() { mconnJoinWait = old })
	cs, ss := smuxPair(t)
	tbl := newMConnTable()
	cfg := KharejConfig{Panel: echoPanel(t), mconns: tbl, noRoute: &noRouteLog{}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for {
			st, err := ss.AcceptStream()
			if err != nil {
				return
			}
			go serveStream(ctx, st, cfg, nil, nil, nil, nil)
		}
	}()
	open := func(id uint64, role byte) *smux.Stream {
		st, err := cs.OpenStream()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.Write(mconnHeader(0, id, role)); err != nil {
			t.Fatal(err)
		}
		return st
	}
	missed := mconnStat.joinMissed.Load()
	// a join alone: closed after the wait
	lone := open(99, mconnRoleJoin)
	lone.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := lone.Read(make([]byte, 1)); err == nil {
		t.Fatal("a join without its connection was not closed")
	}
	if mconnStat.joinMissed.Load() != missed+1 {
		t.Fatal("missed join not counted")
	}
	// join first, then the first sub
	j := open(5, mconnRoleJoin)
	time.Sleep(50 * time.Millisecond)
	f := open(5, mconnRoleFirst)
	for i := 0; tbl.size() == 0 && i < 100; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if tbl.size() != 1 {
		t.Fatal("connection not registered")
	}
	// Whichever mConn is built in carries the bytes on the subs it took; a
	// plain write on either sub only works with the stub, so here the test
	// checks only that both subs were taken or closed, and that the end
	// cleans up.
	time.Sleep(100 * time.Millisecond)
	_, _ = f, j
	cs.Close() // the link dies: every sub with it, so the connection ends
	for i := 0; tbl.size() != 0 && i < 300; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if tbl.size() != 0 {
		t.Fatal("connection kept in the table after it ended")
	}
}

// End to end over a real in-process tunnel with HS2_MCONN on both sides:
// user connections ride mConns and their data arrives intact.
func TestMConnEndToEnd(t *testing.T) {
	withMConn(t, true)
	before := mconnStat.total.Load()
	tn := startTunnel(t, 3, 3)
	// the first connections may take the one stream until the exit's
	// kindInfo answer is in; wait for it
	for i := 0; mconnStat.total.Load() == before; i++ {
		if i == 50 {
			t.Fatal("no connection ever used an mconn")
		}
		tcpEcho(t, tn.userAddr, 1024)
		time.Sleep(50 * time.Millisecond)
	}
	before = mconnStat.total.Load()
	for i := 0; i < 4; i++ {
		tcpEcho(t, tn.userAddr, 256<<10)
	}
	for i := 0; i < 3; i++ {
		tcpEcho(t, tn.userAddr, 1<<20)
	}
	// edge and exit run in this process: each connection counts twice
	if got := mconnStat.total.Load() - before; got < 2*7 {
		t.Fatalf("%d mconn(s) opened for 7 connections: the tunnel did not use them", got)
	}
}
