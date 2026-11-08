package engine

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func withQuiet(t *testing.T) {
	old := quietIdle
	quietIdle = true
	t.Cleanup(func() { quietIdle = old })
}

// quietPeer makes the link's peer announce capQuietIdle (or not).
func quietPeer(m *linkMeter, on bool) {
	pi := &peerInfo{V2: true, Caps: capPortTags}
	if on {
		pi.Caps |= capQuietIdle
	}
	m.peerInfo.Store(pi)
}

// Without the key nothing changes: no cap, no quiet state, smux's own 24 s.
func TestQuietOffIsToday(t *testing.T) {
	if quietIdle {
		t.Skip("HS2_QUIET_IDLE set in the environment")
	}
	if quietCap() != 0 {
		t.Fatal("cap announced without the key")
	}
	if c := newSmuxConfig(); c.KeepAliveTimeout != 24*time.Second || c.KeepAliveInterval < 4*time.Second || c.KeepAliveInterval >= 8*time.Second {
		t.Fatalf("smux keepalive changed: %v / %v", c.KeepAliveInterval, c.KeepAliveTimeout)
	}
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	m := &linkMeter{}
	sess, _, err := newSession(a, false, nil, m)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if m.quiet != nil {
		t.Fatal("quiet state without the key")
	}
	var q *quietState
	if q.sendQuiet() || q.lenient() {
		t.Fatal("nil state is quiet")
	}
	l := newStreamL3Link(nil, func() bool { return true }, nil)
	if l.readDeadAfter() != l3StreamDeadAfter {
		t.Fatal("TUN reader deadline changed")
	}
}

// Who may go quiet and who may loosen its limits, edge and exit.
func TestQuietStates(t *testing.T) {
	withQuiet(t)
	users := false
	f := func() bool { return users }
	edgeM, exitM := &linkMeter{}, &linkMeter{}
	edge, exit := newQuietState(edgeM, true), newQuietState(exitM, false)
	edge.setUsers(f)
	exit.setUsers(f)
	quietPeer(edgeM, true)
	quietPeer(exitM, true)
	if edge.sendQuiet() || edge.lenient() {
		t.Fatal("a new link is quiet at once")
	}
	edge.l3At.Store(ctrlNow() - int64(quietL3Recent) - 1)
	exit.l3At.Store(ctrlNow() - int64(quietL3Recent) - 1)
	if !edge.sendQuiet() || !edge.lenient() {
		t.Fatal("idle edge not quiet")
	}
	if exit.sendQuiet() || exit.lenient() {
		t.Fatal("exit quiet before the edge said idle")
	}
	exit.peerIdle.Store(true)
	if !exit.sendQuiet() || !exit.lenient() {
		t.Fatal("exit not quiet on an idle link")
	}
	users = true // a user: neither goes quiet; the exit keeps strict limits only if the edge has it
	if edge.sendQuiet() || edge.lenient() || exit.sendQuiet() {
		t.Fatal("quiet with users")
	}
	if !exit.lenient() {
		t.Fatal("exit with a user the edge does not have must tolerate the edge's quiet")
	}
	users = false
	edge.markL3() // a TUN packet: busy again
	if edge.sendQuiet() || edge.lenient() {
		t.Fatal("quiet right after a TUN packet")
	}
	quietPeer(edgeM, false) // an older exit: never quiet towards it
	edge.l3At.Store(ctrlNow() - int64(quietL3Recent) - 1)
	if edge.sendQuiet() || edge.lenient() {
		t.Fatal("quiet towards a peer without the cap")
	}
}

// After a switch to strict, silence counts from the switch.
func TestQuietWatchGrace(t *testing.T) {
	var w quietWatch
	t0 := time.Now()
	last := t0.Add(-50 * time.Second)
	if ref := w.ref(true, last, t0); !ref.Equal(last) {
		t.Fatal("lenient ref moved")
	}
	if ref := w.ref(false, last, t0.Add(time.Second)); !ref.Equal(t0.Add(time.Second)) {
		t.Fatal("no grace on the switch to strict")
	}
	if ref := w.ref(false, last, t0.Add(5*time.Second)); !ref.Equal(t0.Add(time.Second)) {
		t.Fatal("grace not kept")
	}
	later := t0.Add(9 * time.Second)
	if ref := w.ref(false, later, t0.Add(10*time.Second)); !ref.Equal(later) {
		t.Fatal("a newer receive not used")
	}
}

type countConn struct {
	net.Conn
	writes atomic.Int32
}

func (c *countConn) Write(p []byte) (int, error) { c.writes.Add(1); return len(p), nil }

// The NOP filter drops keepalives only while quiet, and lets one through
// once nothing went out for the gap.
func TestQuietNOPFilter(t *testing.T) {
	withQuiet(t)
	m := &linkMeter{}
	q := newQuietState(m, true)
	users := true
	q.setUsers(func() bool { return users })
	quietPeer(m, true)
	q.l3At.Store(ctrlNow() - int64(quietL3Recent) - 1)
	cc := &countConn{}
	c := &quietConn{Conn: cc, q: q}
	nop := []byte{2, smuxCmdNOP, 0, 0, 0, 0, 0, 0}
	syn := []byte{2, 0, 0, 0, 1, 0, 0, 0}
	if !isSmuxNOP(nop) || isSmuxNOP(syn) || isSmuxNOP([]byte{2, 3, 0, 0, 1, 0, 0, 0}) {
		t.Fatal("NOP detection")
	}
	c.Write(nop)
	if cc.writes.Load() != 1 {
		t.Fatal("NOP dropped on a busy link")
	}
	users = false
	if n, err := c.Write(nop); n != 8 || err != nil || cc.writes.Load() != 1 {
		t.Fatal("NOP not dropped on a quiet link")
	}
	c.Write(syn)
	if cc.writes.Load() != 2 {
		t.Fatal("a stream frame was dropped")
	}
	q.lastWrite = time.Now().Add(-quietNOPMax - time.Second)
	c.Write(nop)
	if cc.writes.Load() != 3 {
		t.Fatal("NOP not sent after the gap")
	}
	if q.nopGap < quietNOPMin || q.nopGap > quietNOPMax {
		t.Fatal("gap out of range", q.nopGap)
	}
}

// The session watchdog: 24 s-style strict limit on a busy link (here
// shortened), the long one while quiet-idle; the reason names the timeout.
func TestQuietSessionWatchdog(t *testing.T) {
	withQuiet(t)
	oldS, oldL, oldT := quietSessTimeout, quietSilent, quietWatchTick
	quietSessTimeout, quietSilent, quietWatchTick = 400*time.Millisecond, 3*time.Second, 50*time.Millisecond
	defer func() { quietSessTimeout, quietSilent, quietWatchTick = oldS, oldL, oldT }()

	run := func(idle bool) (time.Duration, string) {
		a, b := net.Pipe()
		defer b.Close()
		go io.Copy(io.Discard, b) // the peer reads but never answers
		m := &linkMeter{}
		quietPeer(m, true)
		start := time.Now()
		sess, why, err := newSession(a, false, nil, m)
		if err != nil {
			t.Fatal(err)
		}
		m.quiet.setUsers(func() bool { return !idle })
		if idle {
			m.quiet.l3At.Store(ctrlNow() - int64(quietL3Recent) - 1)
		}
		select {
		case <-sess.CloseChan():
		case <-time.After(9 * time.Second):
			sess.Close()
			return 0, "not closed"
		}
		return time.Since(start), why()
	}
	d, why := run(false)
	if d > 1500*time.Millisecond || !strings.Contains(why, "keepalive timeout") {
		t.Fatalf("busy link: closed after %v (%q)", d, why)
	}
	d, why = run(true)
	if d < 2500*time.Millisecond || d > 2*quietSilent+time.Second { // one to two windows, as smux
		t.Fatalf("idle link: closed after %v (%q), want ~%v", d, why, quietSilent)
	}
}

// The exit learns the edge's idleness from the ping's top bit and still
// echoes the ping as is (an older edge or exit sees no change).
func TestQuietPingFlag(t *testing.T) {
	withQuiet(t)
	a, b := net.Pipe()
	defer a.Close()
	m := &linkMeter{}
	m.quiet = newQuietState(m, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go serveControl(ctx, b, nil, m)
	for _, idle := range []bool{true, false, true} {
		seq := uint64(7)
		if idle {
			seq |= ctrlIdleBit
		}
		ping := make([]byte, ctrlPingLen)
		binary.BigEndian.PutUint64(ping, seq)
		a.Write(ping)
		pong := make([]byte, ctrlPongLen)
		if _, err := io.ReadFull(a, pong); err != nil {
			t.Fatal(err)
		}
		if binary.BigEndian.Uint64(pong) != seq {
			t.Fatal("ping not echoed as sent")
		}
		if m.quiet.peerIdle.Load() != idle {
			t.Fatalf("peerIdle=%v after a ping saying %v", m.quiet.peerIdle.Load(), idle)
		}
	}
	var p ctrlPendingList
	p.add(7, 1)
	if _, ok := p.answer(uint64(7|ctrlIdleBit) &^ ctrlIdleBit); !ok {
		t.Fatal("flagged pong does not answer its ping")
	}
}

// TUN side channel: quiet keepalive and loosened limits only while idle and
// only towards a quiet peer; a TUN packet makes the link busy.
func TestQuietL3(t *testing.T) {
	withQuiet(t)
	m := &linkMeter{}
	q := newQuietState(m, true)
	q.setUsers(func() bool { return false })
	l := newStreamL3LinkQ(nil, func() bool { return true }, nil, q)
	if ka := l.keepalive(); ka >= quietL3Min {
		t.Fatal("quiet keepalive before the peer's cap", ka)
	}
	if l.readDeadAfter() != l3StreamDeadAfter {
		t.Fatal("reader deadline loosened before the peer's cap")
	}
	quietPeer(m, true)
	q.l3At.Store(ctrlNow() - int64(quietL3Recent) - 1)
	if ka := l.keepalive(); ka < quietL3Min || ka > quietL3Max {
		t.Fatal("idle keepalive", ka)
	}
	if l.readDeadAfter() != quietL3ReadDead {
		t.Fatal("reader deadline vs a quiet peer")
	}
	q.markL3()
	if ka := l.keepalive(); ka > 2*l3QuietKeepalive {
		t.Fatal("busy keepalive", ka)
	}
	if q.sendQuiet() {
		t.Fatal("quiet right after a TUN packet")
	}
}

// watchSession: the 12 s rule (shortened) on a busy link, the long limit on
// an idle one.
func TestQuietL3WatchSession(t *testing.T) {
	withQuiet(t)
	oldS, oldL := l3SessionSilent, quietSilent
	l3SessionSilent, quietSilent = 200*time.Millisecond, 2*time.Second
	defer func() { l3SessionSilent, quietSilent = oldS, oldL }()
	for _, idle := range []bool{false, true} {
		m := &linkMeter{}
		quietPeer(m, true)
		q := newQuietState(m, true)
		q.setUsers(func() bool { return !idle })
		if idle {
			q.l3At.Store(ctrlNow() - int64(quietL3Recent) - 1)
		}
		a, b := net.Pipe()
		start := time.Now()
		l := newL3Link(&pipePkt{a})
		l.qs = q
		go l.watchSession(func() uint64 { return 1 })
		<-l.done
		d := time.Since(start)
		b.Close()
		if !idle && d > time.Second || idle && d < 1500*time.Millisecond {
			t.Fatalf("idle=%v: dropped after %v", idle, d)
		}
	}
}

type pipePkt struct{ net.Conn }

func (p *pipePkt) WriteRaw(b []byte) error               { _, err := p.Write(b); return err }
func (p *pipePkt) ReadFrameReuse() (byte, []byte, error) { return 0, nil, io.EOF }
func (p *pipePkt) SetReadDeadline(t time.Time)           { p.Conn.SetReadDeadline(t) }
func (p *pipePkt) Close() error                          { return p.Conn.Close() }
