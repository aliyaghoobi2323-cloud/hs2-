package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xtaci/smux"
)

// ---- an emulated path ---------------------------------------------------------

// wcPipe is one direction of an emulated path: a bottleneck of rate bytes/s
// with an unbounded queue, then a one-way delay.
type wcPipe struct {
	mu      sync.Mutex
	cond    *sync.Cond
	q       []wcChunk
	lastDep time.Time
	rate    float64
	delay   time.Duration
	closed  bool
}

type wcChunk struct {
	at time.Time
	b  []byte
}

func newWcPipe(rate float64, delay time.Duration) *wcPipe {
	p := &wcPipe{rate: rate, delay: delay}
	p.cond = sync.NewCond(&p.mu)
	return p
}

func (p *wcPipe) setRate(r float64) {
	p.mu.Lock()
	p.rate = r
	p.mu.Unlock()
}

func (p *wcPipe) write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return 0, io.ErrClosedPipe
	}
	dep := time.Now()
	if p.lastDep.After(dep) {
		dep = p.lastDep
	}
	dep = dep.Add(time.Duration(float64(len(b)) / p.rate * float64(time.Second)))
	p.lastDep = dep
	p.q = append(p.q, wcChunk{dep.Add(p.delay), append([]byte(nil), b...)})
	p.cond.Broadcast()
	return len(b), nil
}

func (p *wcPipe) read(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for {
		if p.closed {
			return 0, io.EOF
		}
		if len(p.q) == 0 {
			p.cond.Wait()
			continue
		}
		c := &p.q[0]
		if d := time.Until(c.at); d > 0 {
			p.mu.Unlock()
			time.Sleep(d)
			p.mu.Lock()
			continue
		}
		n := copy(b, c.b)
		if c.b = c.b[n:]; len(c.b) == 0 {
			p.q = p.q[1:]
		}
		return n, nil
	}
}

func (p *wcPipe) close() {
	p.mu.Lock()
	p.closed = true
	p.cond.Broadcast()
	p.mu.Unlock()
}

// wcPathEnd is one end of an emulated path.
type wcPathEnd struct{ in, out *wcPipe }

func (e wcPathEnd) Read(b []byte) (int, error)       { return e.in.read(b) }
func (e wcPathEnd) Write(b []byte) (int, error)      { return e.out.write(b) }
func (e wcPathEnd) Close() error                     { e.in.close(); e.out.close(); return nil }
func (e wcPathEnd) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (e wcPathEnd) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (e wcPathEnd) SetDeadline(time.Time) error      { return nil }
func (e wcPathEnd) SetReadDeadline(time.Time) error  { return nil }
func (e wcPathEnd) SetWriteDeadline(time.Time) error { return nil }

// wcPath is a path between an edge and an exit: downloads at down bytes/s,
// uploads unlimited (in effect), delay each way.
type wcPath struct{ down, up *wcPipe }

func newWcPath(down float64, delay time.Duration) *wcPath {
	return &wcPath{down: newWcPipe(down, delay), up: newWcPipe(1<<30, delay)}
}

func (p *wcPath) edge() wcPathEnd { return wcPathEnd{in: p.down, out: p.up} }
func (p *wcPath) exit() wcPathEnd { return wcPathEnd{in: p.up, out: p.down} }

// wcTestConfig is the engine's smux config scaled down (Need 256 KiB, at
// most 2 MiB), so a few MB/s over 100 ms are held by Need.
func wcTestConfig() *smux.Config {
	cfg := newSmuxConfig()
	cfg.NeedStreamBuffer = 256 << 10
	cfg.MaxStreamBuffer = 2 << 20
	cfg.WindowPool = smux.NewWindowPool(16 << 20)
	return cfg
}

// wcSessions is an edge (smux client, metered by mtr) and an exit session
// over conns a and b.
func wcSessions(t *testing.T, a, b net.Conn, cfg *smux.Config, mtr *linkMeter) (edge, exit *smux.Session) {
	t.Helper()
	var c net.Conn = a
	if mtr != nil {
		c = &meteredConn{Conn: a, m: mtr}
	}
	edge, err := smux.Client(c, cfg)
	if err != nil {
		t.Fatal(err)
	}
	exit, err = smux.Server(b, wcTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { edge.Close(); exit.Close(); a.Close(); b.Close() })
	return edge, exit
}

// wcDownload opens a stream on edge over which exit sends as fast as the
// window lets it, read at once; it returns the edge's end.
func wcDownload(t *testing.T, edge, exit *smux.Session) *smux.Stream {
	t.Helper()
	st, err := edge.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		x, err := exit.AcceptStream()
		if err != nil {
			return
		}
		buf := make([]byte, 32<<10)
		for {
			if _, err := x.Write(buf); err != nil {
				return
			}
		}
	}()
	go io.Copy(io.Discard, st)
	return st
}

// runWcSet wakes ws as the process's runner does, until the test ends.
func runWcSet(t *testing.T, ws *wcSet) {
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		tk := time.NewTicker(wcRampTick)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
				ws.wakeAll()
			}
		}
	}()
	t.Cleanup(func() { close(stop); <-done })
}

// wcLogs collects log lines.
type wcLogs struct {
	mu    sync.Mutex
	lines []string
}

func (w *wcLogs) set(ws *wcSet) {
	f := func(format string, a ...any) {
		w.mu.Lock()
		w.lines = append(w.lines, fmt.Sprintf(format, a...))
		w.mu.Unlock()
	}
	ws.logf.Store(&f)
}

func (w *wcLogs) all() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.lines...)
}

func (w *wcLogs) count(sub string) int {
	n := 0
	for _, l := range w.all() {
		if strings.Contains(l, sub) {
			n++
		}
	}
	return n
}

// readTrace parses every whole line of a trace file; each must parse.
func readTrace(t *testing.T, path string) []wcTraceLine {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	if i := bytes.LastIndexByte(b, '\n'); i >= 0 {
		b = b[:i+1]
	} else {
		return nil
	}
	var out []wcTraceLine
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var x wcTraceLine
		dec := json.NewDecoder(bytes.NewReader(sc.Bytes()))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&x); err != nil {
			t.Fatalf("trace line %q: %v", sc.Text(), err)
		}
		if _, err := time.Parse(time.RFC3339Nano, x.T); err != nil || x.Link == "" || x.Side == "" || x.State == "" {
			t.Fatalf("trace line missing its time, link, side or state: %q", sc.Text())
		}
		out = append(out, x)
	}
	return out
}

func wcWaitFor(t *testing.T, d time.Duration, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(d); !ok(); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("after %v: %s", d, what)
		}
	}
}

// ---- the runner on a real smux pair -------------------------------------------

// The runner over a real smux pair on an emulated path of 5 MB/s and a 100
// ms round trip, where Need (256 KiB) holds a download to half the path:
// while the autopilot probes, the core starts nothing and no window passes
// Need although the stream counts as fast; then the fast reader's window
// grows past Need and HOLD keeps it there; and when the path slows to a
// fifth, the core drains it back to Need. Every trace line parses, and the
// log lines say what happened.
func TestWcoreRunnerGrowsAndDrains(t *testing.T) {
	p := newWcPath(5<<20, 50*time.Millisecond)
	cfg := wcTestConfig()
	need := uint32(cfg.NeedStreamBuffer)
	mtr := &linkMeter{}
	mtr.rttMin.Store(100_000) // the control ping's floor (µs): T_base without TCP_INFO
	edge, exit := wcSessions(t, p.edge(), p.exit(), cfg, mtr)
	ws := &wcSet{tracePath: filepath.Join(t.TempDir(), "trace")}
	var logs wcLogs
	logs.set(ws)
	l := newWcLink(edge, cfg, nil, mtr, false, nil)
	l.setName("link 1")
	var probing atomic.Bool
	probing.Store(true)
	l.ap.Store(&probing)
	ws.put(l)
	st := wcDownload(t, edge, exit)
	runWcSet(t, ws)

	// the autopilot probes: the core starts nothing of its own
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if s := wcState(l.inStats.state.Load()); s != wcOff || st.CoreCeil() != 0 {
			t.Fatalf("while the autopilot probes: core %v, ceiling %d", s, st.CoreCeil())
		}
	}
	tr := readTrace(t, ws.tracePath)
	fast := false
	for _, x := range tr {
		for _, s := range x.Streams {
			if s.Win > need {
				t.Fatalf("a window past Need while the autopilot probes: %+v", x)
			}
		}
		fast = fast || x.NF > 0 && x.APProbing
	}
	if !fast {
		t.Fatalf("the stream never counted as fast while the autopilot probed (nothing was held back): %d lines", len(tr))
	}

	// growth: past Need, to HOLD past it
	probing.Store(false)
	var peak uint32
	wcWaitFor(t, 30*time.Second, "no window past Need held", func() bool {
		for _, x := range readTrace(t, ws.tracePath) {
			for _, s := range x.Streams {
				peak = max(peak, s.Win)
			}
		}
		return peak > need && st.CoreCeil() > need && wcState(l.inStats.state.Load()) == wcHold
	})

	// the path slows to a fifth: drained back to Need
	from := len(readTrace(t, ws.tracePath))
	p.down.setRate(1 << 20)
	wcWaitFor(t, 30*time.Second, "not drained back to Need", func() bool {
		tr := readTrace(t, ws.tracePath)
		drained := false
		for _, x := range tr[from:] {
			for _, e := range x.Events {
				drained = drained || strings.HasPrefix(e, "drain:")
			}
			if drained && len(x.Streams) == 1 && x.Streams[0].Win <= need && st.CoreCeil() == 0 {
				return true
			}
		}
		return false
	})
	if logs.count("link 1: path ") == 0 || logs.count("link 1: downloads fill the path at ~") == 0 {
		t.Errorf("no drain line, or no \"fill the path\" line: %q", logs.all())
	}
	if n := wcoreNote(&linkMeter{wc: l}); !strings.HasPrefix(n, "window core: up to ") {
		t.Errorf("link-end note %q", n)
	}
	t.Logf("peak window %.2f MiB; lines %q", float64(peak)/(1<<20), logs.all())
	if testing.Verbose() {
		wcTraceDump(t, readTrace(t, ws.tracePath))
	}
}

// wcTraceDump logs a trace one short line per evaluation.
func wcTraceDump(t *testing.T, tr []wcTraceLine) {
	t.Helper()
	for _, x := range tr {
		var ss strings.Builder
		for _, s := range x.Streams {
			fmt.Fprintf(&ss, " [w%.0fK c%.0fK u%d h%d l%d t%.0f/%.0f f%v]", float64(s.Win)/1024, float64(s.Ceil)/1024, s.Upd, s.Hit, s.Lag, s.TurnMin, s.TurnLast, s.F)
		}
		t.Logf("%s %-7s B%4.0fK/%4.0fK nF%d RF%5.1f RL%5.1f hit%.2f tb%3.0f turn%3.0f qt%3.0f qref%3.0f tau%3.0f grow%v ap%v%s %v",
			x.T[17:23], x.State, float64(x.B)/1024, float64(x.BPub)/1024, x.NF, mbitps(x.RF), mbitps(x.RLink), x.Hit,
			x.TBase, x.TurnBase, x.QTurn, x.QRef, x.Tau, x.GrowOK, x.APProbing, ss.String(), x.Events)
	}
}

// With HS2_TUNE_WCORE_FIXEDWIN the controller's ceilings give way to the
// fixed window on every fast stream (no growth brake), the trace goes on,
// and nothing is logged as a decision of the core.
func TestWcoreRunnerFixedWindow(t *testing.T) {
	p := newWcPath(5<<20, 50*time.Millisecond)
	cfg := wcTestConfig()
	const fixed = 384 << 10
	mtr := &linkMeter{}
	mtr.rttMin.Store(100_000)
	edge, exit := wcSessions(t, p.edge(), p.exit(), cfg, mtr)
	ws := &wcSet{tracePath: filepath.Join(t.TempDir(), "trace")}
	var logs wcLogs
	logs.set(ws)
	l := newWcLink(edge, cfg, nil, mtr, false, nil)
	l.fixed = fixed
	ws.put(l)
	st := wcDownload(t, edge, exit)
	runWcSet(t, ws)
	wcWaitFor(t, 20*time.Second, "the window never reached the fixed one", func() bool {
		tr := readTrace(t, ws.tracePath)
		return st.CoreCeil() == fixed && len(tr) > 0 && len(tr[len(tr)-1].Streams) == 1 && tr[len(tr)-1].Streams[0].Win == fixed
	})
	time.Sleep(2 * time.Second)
	for _, x := range readTrace(t, ws.tracePath) {
		for _, s := range x.Streams {
			if s.Win > fixed || s.F && s.Ceil != fixed || !s.F && s.Ceil != 0 {
				t.Fatalf("a window past the fixed one, or a ceiling that is not it: %+v", x)
			}
		}
		if x.Fixed != fixed || x.TurnRef != 0 || !x.GrowOK {
			t.Fatalf("trace %+v: want fixed %d, no reference and growth allowed", x, fixed)
		}
	}
	if n := len(logs.all()); n != 0 {
		t.Fatalf("lines logged under fixed windows: %q", logs.all())
	}
}

// HS2_TUNE_WCORE=0: no session gets a core, nothing sets a ceiling, and on
// the path where the core grows windows none passes Need.
func TestWcoreOffIsBase(t *testing.T) {
	old := Wcore
	Wcore = 0
	t.Cleanup(func() { Wcore = old })
	a, b := net.Pipe()
	ma, mb := &linkMeter{}, &linkMeter{}
	cli, _, err := newSession(a, false, nil, ma)
	if err != nil {
		t.Fatal(err)
	}
	srv, _, err := newSession(b, true, nil, mb)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cli.Close(); srv.Close() })
	if ma.wc != nil || mb.wc != nil || wcRegistered(cli) || wcRegistered(srv) {
		t.Fatal("a session got a window core with the window core off")
	}
	cfg := wcTestConfig()
	if newWcLink(cli, cfg, nil, nil, false, nil) != nil {
		t.Fatal("newWcLink made a core with the window core off")
	}
	p := newWcPath(5<<20, 50*time.Millisecond)
	edge, exit := wcSessions(t, p.edge(), p.exit(), cfg, nil)
	wcDownload(t, edge, exit)
	need := uint32(cfg.NeedStreamBuffer)
	top := uint32(0)
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		s, _ := edge.CoreSnapshot(nil) // no runner: this is the only caller
		for _, x := range s {
			if x.Win > need || x.Ceil != 0 {
				t.Fatalf("with the window core off: %+v", x)
			}
			top = max(top, x.Win)
		}
	}
	if top < need {
		t.Fatalf("the window reached %d only, under Need %d: the path tested nothing", top, need)
	}
}

func wcRegistered(s *smux.Session) bool {
	wcores.mu.Lock()
	defer wcores.mu.Unlock()
	for l := range wcores.all {
		if l.sess == s {
			return true
		}
	}
	return false
}

// newSession registers both ends of a link with the process's runner (one
// goroutine for all), each with its direction; a closed session is dropped,
// and its sessions' goroutines end with it.
func TestWcoreRunnerRegistry(t *testing.T) {
	a, b := net.Pipe()
	ma, mb := &linkMeter{}, &linkMeter{}
	cli, _, err := newSession(a, false, nil, ma)
	if err != nil {
		t.Fatal(err)
	}
	srv, _, err := newSession(b, true, nil, mb)
	if err != nil {
		t.Fatal(err)
	}
	if ma.wc == nil || mb.wc == nil || ma.wc.side != wcEdge || mb.wc.side != wcExit {
		t.Fatalf("cores %+v %+v: want an edge's and an exit's", ma.wc, mb.wc)
	}
	if !wcRegistered(cli) || !wcRegistered(srv) {
		t.Fatal("not registered")
	}
	cli.Close()
	srv.Close()
	wcWaitFor(t, 3*time.Second, "closed sessions still registered", func() bool { return !wcRegistered(cli) && !wcRegistered(srv) })

	time.Sleep(200 * time.Millisecond)
	g0 := runtime.NumGoroutine()
	var all []*smux.Session
	for i := 0; i < 10; i++ {
		a, b := net.Pipe()
		c, _, err := newSession(a, false, nil, &linkMeter{})
		if err != nil {
			t.Fatal(err)
		}
		s, _, err := newSession(b, true, nil, &linkMeter{})
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, c, s)
	}
	for _, s := range all {
		s.Close()
	}
	wcWaitFor(t, 3*time.Second, "closed sessions still registered", func() bool {
		for _, s := range all {
			if wcRegistered(s) {
				return false
			}
		}
		return true
	})
	wcWaitFor(t, 5*time.Second, "goroutines left behind", func() bool { return runtime.NumGoroutine() <= g0+2 })
	buf := make([]byte, 1<<20)
	if n := strings.Count(string(buf[:runtime.Stack(buf, true)]), "engine.(*wcSet).run("); n != 1 {
		t.Fatalf("%d runner goroutines, want 1", n)
	}
}

// wcPairStreams opens n streams of a session pair over a pipe, with a
// window update each (a snapshot sees a stream from its first update on),
// and returns the edge's ends.
func wcPairStreams(t *testing.T, cfg *smux.Config, n int) (*smux.Session, []*smux.Stream) {
	t.Helper()
	a, b := net.Pipe()
	edge, exit := wcSessions(t, a, b, cfg, nil)
	var sts []*smux.Stream
	for i := 0; i < n; i++ {
		st, err := edge.OpenStream()
		if err != nil {
			t.Fatal(err)
		}
		x, err := exit.AcceptStream()
		if err != nil {
			t.Fatal(err)
		}
		go x.Write([]byte{1})
		var k [1]byte
		if _, err := io.ReadFull(st, k[:]); err != nil {
			t.Fatal(err)
		}
		sts = append(sts, st)
	}
	return edge, sts
}

// The runner hands each stream its ceiling by id, none to a sampled stream
// the core left out, and takes the ceiling of a stream that left the
// snapshot back.
func TestWcoreRunnerCeilingsByID(t *testing.T) {
	cfg := wcTestConfig()
	cfg.NeedStreamBuffer = 512 << 10 // the start window (256 KiB) is in every snapshot
	sess, sts := wcPairStreams(t, cfg, 3)
	l := newWcLink(sess, cfg, nil, nil, false, nil)
	snap := func() {
		t.Helper()
		l.snap(time.Now())
	}
	wcWaitFor(t, 3*time.Second, "the streams' first updates", func() bool { snap(); return len(l.samples) == 3 })
	id := func(i int) uint32 { return sts[i].ID() }
	l.apply(wcOut{ceils: []wcCeil{{id(2), 600 << 10}, {id(0), 1 << 20}, {id(1), 768 << 10}}})
	if c := [3]uint32{sts[0].CoreCeil(), sts[1].CoreCeil(), sts[2].CoreCeil()}; c != [3]uint32{1 << 20, 768 << 10, 600 << 10} {
		t.Fatalf("ceilings %v", c)
	}
	sts[1].Close() // leaves the snapshot
	snap()
	if len(l.samples) != 2 {
		t.Fatalf("%d samples after a close, want 2", len(l.samples))
	}
	l.apply(wcOut{ceils: []wcCeil{{id(0), 1 << 20}}}) // and stream 2 is no fast one now
	if c := [3]uint32{sts[0].CoreCeil(), sts[1].CoreCeil(), sts[2].CoreCeil()}; c != [3]uint32{1 << 20, 0, 0} {
		t.Fatalf("ceilings %v after one left the snapshot and one the core's list, want only the first", c)
	}
	if len(l.ceiled) != 1 {
		t.Fatalf("the runner holds %d ceiled streams, want 1", len(l.ceiled))
	}
	// the lab's fixed window: on the fast streams only, no brake
	l.fixed = 900 << 10
	l.core.inF = map[uint32]bool{id(2): true}
	o := wcOut{ceils: []wcCeil{{id(0), 1 << 20}, {id(2), 0}}, turnRef: 5 * time.Millisecond, growOK: false}
	l.pin(&o)
	snap()
	l.apply(o)
	if c := [3]uint32{sts[0].CoreCeil(), sts[1].CoreCeil(), sts[2].CoreCeil()}; c != [3]uint32{0, 0, 900 << 10} || o.turnRef != 0 || !o.growOK {
		t.Fatalf("fixed window: ceilings %v, out %+v", c, o)
	}
}

// The runner and traffic at once: the runner steps the link on every
// millisecond while streams open, carry data and close (go test -race).
func TestWcoreRunnerRace(t *testing.T) {
	cfg := wcTestConfig()
	a, b := net.Pipe()
	mtr := &linkMeter{}
	edge, exit := wcSessions(t, a, b, cfg, mtr)
	ws := &wcSet{}
	l := newWcLink(edge, cfg, nil, mtr, false, nil)
	ws.put(l)
	var probing atomic.Bool
	l.ap.Store(&probing)
	go func() {
		for {
			x, err := exit.AcceptStream()
			if err != nil {
				return
			}
			go func() {
				buf := make([]byte, 16<<10)
				for i := 0; i < 64; i++ {
					if _, err := x.Write(buf); err != nil {
						return
					}
				}
				x.Close()
			}()
		}
	}()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				st, err := edge.OpenStream()
				if err != nil {
					return
				}
				io.Copy(io.Discard, st)
				st.Close()
			}
		}()
	}
	end := time.Now().Add(time.Second)
	for time.Now().Before(end) {
		l.next = time.Time{}
		l.wake(time.Now())
		probing.Store(!probing.Load())
		mtr.rdBytes.Add(1 << 20)
		time.Sleep(time.Millisecond)
	}
	close(stop)
	edge.Close()
	wg.Wait()
	ws.wakeAll()
}

// ---- logs, notes and what the engine reads --------------------------------------

// wcLogLink is a core for the log tests (no session).
func wcLogLink(name string, side wcSide) *wcLink {
	cfg := wcTestConfig()
	cfg.MaxStreamBuffer = 16 << 20
	l := newWcLink(nil, cfg, nil, nil, side == wcExit, nil)
	l.setName(name)
	return l
}

// Each line has its cap per link (drain 30 s, "fill the path" 60 s, random
// loss and a window at its ceiling 10 min), past 8 links a minute the rest
// go into one line, and the pool's line comes once in 5 minutes for the
// whole process.
func TestWcoreRunnerLogCaps(t *testing.T) {
	ws := &wcSet{}
	var logs wcLogs
	logs.set(ws)
	now := time.Unix(1e9, 0)
	l := wcLogLink("link 3", wcEdge)
	drain := wcEvent{kind: wcEvDrain, why: wcWhyLoss, from: 6400 << 10, B: 3100 << 10, n: 2, q: 38 * time.Millisecond, loss: 0.021}
	filled := wcEvent{kind: wcEvFilled, B: 6554 << 10, n: 2, R: 287e6 / 8, T: 151 * time.Millisecond, q: 4 * time.Millisecond}
	lossy := wcEvent{kind: wcEvLossy, loss: 0.004, q: 18 * time.Millisecond, T: 100 * time.Millisecond}
	capped := wcEvent{kind: wcEvCapped, B: 16 << 20, n: 1, T: 480 * time.Millisecond}
	ev := func(at time.Duration, e ...wcEvent) { ws.logEvents(l, now.Add(at), wcOut{events: e}) }
	ev(0, drain, filled, lossy, capped)
	want := []string{
		"mtcp: link 3: path queue grew to 38 ms (resent 2.1%) — download windows 6.2 MiB → 3.0 MiB",
		"mtcp: link 3: downloads fill the path at ~287 Mbit/s (round trip 151 ms): window held at 6.4 MiB for 2 fast connection(s), queue ~4 ms",
		"mtcp: link 3: 0.4% loss with no queue — windows cover the resends (+18 ms)",
		"mtcp: link 3: one connection's window is at its 16 MiB ceiling (round trip 480 ms)",
	}
	if got := logs.all(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("lines\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	ev(10*time.Second, drain, filled, lossy, capped)
	ev(29*time.Second, drain)
	if n := len(logs.all()); n != 4 {
		t.Fatalf("%d lines within the caps, want still 4: %q", n, logs.all())
	}
	ev(31*time.Second, drain, filled)
	if logs.count("path queue grew") != 2 || logs.count("fill the path") != 1 {
		t.Fatalf("after 31 s: %q", logs.all())
	}
	ev(61*time.Second, filled, lossy)
	ev(9*time.Minute, lossy, capped)
	if logs.count("fill the path") != 2 || logs.count("loss with no queue") != 1 || logs.count("ceiling") != 1 {
		t.Fatalf("after a minute and 9: %q", logs.all())
	}
	ev(11*time.Minute, lossy, capped)
	if logs.count("loss with no queue") != 2 || logs.count("ceiling") != 2 {
		t.Fatalf("after 11 minutes: %q", logs.all())
	}

	// the exit's lines, and a cut between evaluations
	x := wcLogLink("link from 1.2.3.4:5678", wcExit)
	ws.logEvents(x, now, wcOut{events: []wcEvent{{kind: wcEvDrain, why: wcWhyLoss, from: 4 << 20, B: 2 << 20, q: -1}}})
	if !strings.HasSuffix(logs.all()[len(logs.all())-1], "link from 1.2.3.4:5678: packets arrived behind holes on a clean path — upload windows 4.0 MiB → 2.0 MiB") {
		t.Fatalf("exit's cut line: %q", logs.all()[len(logs.all())-1])
	}

	// 12 links draining within a minute: 8 lines, then one for the other 4
	ws2 := &wcSet{}
	var logs2 wcLogs
	logs2.set(ws2)
	for i := 0; i < 12; i++ {
		ws2.logEvents(wcLogLink(fmt.Sprintf("link %d", i), wcEdge), now.Add(time.Duration(i)*time.Second), wcOut{events: []wcEvent{drain}})
	}
	if n := logs2.count("path queue grew"); n != wcAggLinks {
		t.Fatalf("%d per-link drain lines in a minute, want %d", n, wcAggLinks)
	}
	ws2.flush(now.Add(30 * time.Second))
	ws2.flush(now.Add(61 * time.Second))
	if n := logs2.count("window core: 4 more link(s) drained their windows 4 time(s) in the last minute"); n != 1 {
		t.Fatalf("summary: %q", logs2.all())
	}
	ws2.flush(now.Add(3 * time.Minute))
	if n := len(logs2.all()); n != wcAggLinks+1 {
		t.Fatalf("%d lines, want no second summary", n)
	}

	// the pool: once in 5 minutes for the process, the links held lately
	ws3 := &wcSet{}
	var logs3 wcLogs
	logs3.set(ws3)
	poolEv := wcOut{events: []wcEvent{{kind: wcEvPool, n: 2}}}
	ws3.logEvents(wcLogLink("link 1", wcEdge), now, poolEv)
	ws3.logEvents(wcLogLink("link 2", wcEdge), now, wcOut{events: []wcEvent{{kind: wcEvPool, n: 3}}})
	ws3.flush(now.Add(time.Second))
	ws3.logEvents(wcLogLink("link 1", wcEdge), now.Add(time.Minute), poolEv)
	ws3.flush(now.Add(time.Minute))
	if n := logs3.count("window pool full"); n != 1 || logs3.count(" MiB): 5 fast connection(s) on 2 link(s) held below what their path carries") != 1 {
		t.Fatalf("pool lines %q", logs3.all())
	}
	ws3.logEvents(wcLogLink("link 1", wcEdge), now.Add(6*time.Minute), poolEv)
	ws3.flush(now.Add(6 * time.Minute))
	if n := logs3.count("window pool full"); n != 2 {
		t.Fatalf("pool lines after 6 minutes %q", logs3.all())
	}
}

// A HOLD budget more than 25% off the latest "fill the path" line is a
// knee that moved: it gets a line of its own.
func TestWcoreRunnerKneeMoveLine(t *testing.T) {
	ws := &wcSet{}
	var logs wcLogs
	logs.set(ws)
	now := time.Unix(1e9, 0)
	l := wcLogLink("link 4", wcEdge)
	ws.logEvents(l, now, wcOut{events: []wcEvent{{kind: wcEvFilled, B: 4 << 20, n: 1, R: 1e6, T: 100 * time.Millisecond}}})
	c := l.core
	c.state, c.lastAt, c.B, c.tBase = wcHold, now.Add(2*time.Minute), 4<<20+(512<<10), 100*time.Millisecond
	ws.logEvents(l, c.lastAt, wcOut{})
	if n := logs.count("fill the path"); n != 1 {
		t.Fatalf("a move of 12%%: %d lines", n)
	}
	c.B, c.lastAt = 6<<20, now.Add(3*time.Minute)
	ws.logEvents(l, c.lastAt, wcOut{})
	if n := logs.count("window held at 6.0 MiB"); n != 1 {
		t.Fatalf("a move of 50%%: %q", logs.all())
	}
}

// The link-end notes: nothing for a core that never left OFF; what it did
// otherwise, on the edge's linkStallNote and the exit's lines (cut back off
// the reason for the reverse exit's).
func TestWcoreRunnerNotes(t *testing.T) {
	a, b := net.Pipe()
	sess, _ := wcSessions(t, a, b, wcTestConfig(), nil)
	mtr := &linkMeter{}
	mtr.wc = wcLogLink("link 1", wcEdge)
	ml := &mtcpLink{sess: sess, mtr: mtr}
	if n := linkStallNote(ml); n != "" || wcoreEndNote(mtr) != "" {
		t.Fatalf("a core that never left OFF: %q %q", n, wcoreEndNote(mtr))
	}
	st := &mtr.wc.inStats
	st.active.Store(true)
	st.peakB.Store(9542041) // 9.1 MiB
	st.peakR.Store(math.Float64bits(412e6 / 8))
	st.drainQ.Store(3)
	st.drainLoss.Store(1)
	const want = "window core: up to 9.1 MiB at 412 Mbit/s, drained 3 time(s) for queue, 1 for loss, held by the window pool 0 time(s)"
	if n := linkStallNote(ml); n != " (while up: "+want+")" {
		t.Fatalf("edge note %q", n)
	}
	end := wcoreEndNote(mtr)
	if end != " (while up: "+want+")" {
		t.Fatalf("exit note %q", end)
	}
	why, note := cutEndNote("read: " + reasonPeerClosed + end)
	if why != "read: "+reasonPeerClosed || note != end || !edgeClosed(why) {
		t.Fatalf("cut %q / %q", why, note)
	}
	if why, note := cutEndNote("reset by the network or the other server"); note != "" || why == "" {
		t.Fatalf("no note: %q / %q", why, note)
	}
	st.drainRate.Store(2)
	if n := wcoreNote(mtr); !strings.Contains(n, "1 for loss, 2 for a slower path, held by") {
		t.Fatalf("with rate drains %q", n)
	}
}

// grownRate is what the fast streams carry past what Need windows would at
// their turnaround, 0 with the core OFF; the LinkManager takes it into its
// sample, and wcGrownOf reads 0 for a link without a core.
func TestWcoreGrownRate(t *testing.T) {
	l := wcLogLink("link 1", wcEdge)
	c := l.core
	now := time.Unix(1e9, 0)
	c.state, c.tBase = wcHold, 100*time.Millisecond
	c.last = wcMeas{f: []int{0, 1}, rF: 10e6}
	want := 10e6 - 2*float64(c.cfg.need)/0.1
	if g := l.grownRate(now); math.Abs(g-want) > 1 {
		t.Fatalf("grownRate %.0f, want %.0f", g, want)
	}
	c.state = wcOff
	if g := l.grownRate(now); g != 0 {
		t.Fatalf("grownRate %.0f in OFF", g)
	}
	if wcGrownOf(nil) != 0 || wcGrownOf(&linkMeter{}) != 0 {
		t.Fatal("grownRate of a link without a core")
	}

	m := NewLinkManager(nil, 2, 2, 50, nil)
	f := newMeteredFake()
	f.m.wc = wcLogLink("link 1", wcEdge)
	f.m.wc.inStats.grown.Store(math.Float64bits(3e6))
	ml := &managedLink{link: f, mtr: f.m}
	m.links = []*managedLink{ml}
	m.sampleHealth()
	f.m.rdBytes.Add(20 << 20)
	m.sampleHealth()
	if ml.grown != 3e6 || m.sample.grown != 3e6 || len(m.sample.links) != 1 || m.sample.links[0].grown != 3e6 {
		t.Fatalf("grown %.0f, sample %.0f %+v: want 3e6", ml.grown, m.sample.grown, m.sample.links)
	}
}

// The LinkManager publishes whether the autopilot is probing, and names the
// links' cores and hands them the flag as it adopts them.
func TestWcoreLinkManagerWiring(t *testing.T) {
	m := NewLinkManager(nil, 2, 4, 8, nil)
	m.ap.confirm.active = true
	m.decideTarget()
	if !m.apProbing.Load() {
		t.Fatal("not probing while a gain is being confirmed")
	}
	m.ap.confirm.active = false
	m.ap.pr = &apProbe{}
	m.decideTarget()
	if !m.apProbing.Load() {
		t.Fatal("not probing with a probe in flight")
	}
	m.ap.pr = nil
	m.decideTarget()
	if m.apProbing.Load() {
		t.Fatal("probing with neither")
	}
	m.ap.pr = &apProbe{}
	m.pin.Store(3)
	m.decideTarget()
	if m.apProbing.Load() {
		t.Fatal("probing while pinned")
	}

	mtr := &linkMeter{wc: wcLogLink("link to x", wcEdge)}
	m.newManaged(&mtcpLink{mtr: mtr}, 7, time.Now())
	if mtr.wc.label() != "link 7" || mtr.wc.ap.Load() != &m.apProbing {
		t.Fatalf("core named %q, flag %p (want %p)", mtr.wc.label(), mtr.wc.ap.Load(), &m.apProbing)
	}
}

// The autopilot sizes the pool by what the links carry for ordinary
// connections: the peak and the per-link capacity leave out what grown
// windows carry (one connection's pace on one link).
func TestAutopilotLeavesGrownOut(t *testing.T) {
	const mbit = 1e6 / 8
	run := func(grown float64) *autopilot {
		a := newAutopilot(2, 64, 8)
		now := time.Unix(1e9, 0)
		for tick := 0; tick < 3; tick++ {
			s := apSample{now: now.Add(time.Duration(tick) * healthTick), growable: true}
			for i := 0; i < 8; i++ {
				l := apLink{id: i, serving: true, rate: 100 * mbit, sustained: 100 * mbit, flowing: 1, open: 1}
				if i < 6 {
					l.pressed, l.rate, l.sustained, l.grown = true, 300*mbit, 300*mbit, grown
				}
				s.links = append(s.links, l)
				s.G += l.rate
				s.grown += l.grown
				s.flowing++
			}
			a.decide(s)
		}
		return a
	}
	base, core := run(0), run(250*mbit)
	if math.Abs(base.cCap-300*mbit) > 1 || math.Abs(base.gPeak-2000*mbit) > 1 {
		t.Fatalf("without grown windows: capacity %.0f, peak %.0f Mbit/s", base.cCap/mbit, base.gPeak/mbit)
	}
	if math.Abs(core.cCap-50*mbit) > 1 || math.Abs(core.gPeak-500*mbit) > 1 {
		t.Fatalf("with grown windows: capacity %.0f, peak %.0f Mbit/s, want 50 and 500", core.cCap/mbit, core.gPeak/mbit)
	}
	if math.Abs(core.gShown-2000*mbit) > 1 {
		t.Fatalf("the peak shown %.0f Mbit/s, want all of it (2000)", core.gShown/mbit)
	}
}

// The status line's numbers: the streams past Need on every registered
// link and what their windows are.
func TestWcoreFillStats(t *testing.T) {
	ws := &wcSet{}
	for i := 0; i < 3; i++ {
		l := wcLogLink("link", wcEdge)
		l.sess = &smux.Session{} // never closed: only counted here
		l.inStats.fastN.Store(int32(i))
		l.inStats.fastWin.Store(int64(i) << 20)
		ws.put(l)
	}
	var st PoolStats
	ws.fillStats(&st)
	if st.FastConns != 3 || st.FastWinMiB != 3 || st.PoolMiB != min(max(SmuxWindowPool, 0), 1<<30)>>20 {
		t.Fatalf("stats %+v", st)
	}
}

// What the runner takes from the edge's meter: the control ping's pongs
// since its previous snapshot (openControl keeps their round trips), its
// floor, the exit's latest record and the autopilot's flag; the exit's end
// takes none of them.
func TestWcoreRunnerEdgeInputs(t *testing.T) {
	cconn, sconn := net.Pipe()
	cfg := newSmuxConfig()
	cli, err := smux.Client(cconn, cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := smux.Server(sconn, newSmuxConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cli.Close(); srv.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		st, err := srv.AcceptStream()
		if err != nil {
			return
		}
		var kind [1]byte
		io.ReadFull(st, kind[:])
		serveControl(ctx, st, nil)
	}()
	mtr := &linkMeter{}
	go openControl(ctx, &ctrlFakeLink{sess: cli, m: mtr}, nil)
	wcWaitFor(t, controlInterval+3*time.Second, "no pong", func() bool { p := mtr.peerLoss.Load(); return p != nil && p.n >= 1 })
	if r := mtr.pongRTT[0].Load(); r == 0 || r != mtr.rttMicros.Load() {
		t.Fatalf("pong ring %d, rtt %d", r, mtr.rttMicros.Load())
	}

	l := newWcLink(cli, cfg, nil, mtr, false, nil)
	var probing atomic.Bool
	probing.Store(true)
	l.ap.Store(&probing)
	at := time.Now()
	mtr.peer.Store(&statsRec{seq: 1, srtt: 120_000, minRTT: 100_000, retrans: 7, segsOut: 1000, at: at})
	s := l.snap(time.Now())
	rtt := usDur(mtr.pongRTT[0].Load())
	if len(s.pongs) != 1 || s.pongs[0] != rtt || s.ctrlMin != usDur(mtr.rttMin.Load()) || !s.apProbing ||
		!s.sndAt.Equal(at) || s.sndSrtt != 120*time.Millisecond || s.sndMinRTT != 100*time.Millisecond || s.sndRetrans != 7 || s.sndSegsOut != 1000 {
		t.Fatalf("edge snapshot %+v", s)
	}
	if s := l.snap(time.Now()); len(s.pongs) != 0 {
		t.Fatalf("pongs %v taken twice", s.pongs)
	}
	// six more, of which the ring keeps the last four
	cur := mtr.peerLoss.Load().n
	for k := cur; k < cur+6; k++ {
		mtr.pongRTT[k%4].Store(uint64(1000 * (k + 1)))
	}
	mtr.peerLoss.Store(&peerLossRec{n: cur + 6})
	s = l.snap(time.Now())
	if len(s.pongs) != 4 || s.pongs[0] != usDur(1000*(cur+3)) || s.pongs[3] != usDur(1000*(cur+6)) {
		t.Fatalf("pongs %v, want the last four", s.pongs)
	}

	x := newWcLink(cli, cfg, nil, mtr, true, nil) // the exit's end of a link
	if s := x.snap(time.Now()); len(s.pongs) != 0 || s.ctrlMin != 0 || !s.sndAt.IsZero() || s.apProbing {
		t.Fatalf("the exit took the edge's inputs: %+v", s)
	}
}

// The session's reference goes to smux with the ceilings: past Need a
// window grows only while growOK, and while its turnaround is within the
// reference.
func TestWcoreRunnerSetsRef(t *testing.T) {
	cfg := wcTestConfig()
	need := uint32(cfg.NeedStreamBuffer)
	p := newWcPath(5<<20, time.Millisecond) // a reader that keeps up
	edge, exit := wcSessions(t, p.edge(), p.exit(), cfg, nil)
	st := wcDownload(t, edge, exit)
	l := newWcLink(edge, cfg, nil, nil, false, nil)
	win := func() uint32 {
		t.Helper()
		time.Sleep(600 * time.Millisecond) // 3 MB read: many updates
		l.snap(time.Now())
		for _, x := range l.samples {
			if x.ID == st.ID() {
				return x.Win
			}
		}
		t.Fatal("the stream is not in the snapshot")
		return 0
	}
	ceils := []wcCeil{{st.ID(), 1 << 20}}
	l.snap(time.Now())
	l.apply(wcOut{ceils: ceils, growOK: false})
	if w := win(); w > need {
		t.Fatalf("window %d past Need with growth held", w)
	}
	l.apply(wcOut{ceils: ceils, growOK: true, turnRef: time.Nanosecond})
	if w := win(); w > need {
		t.Fatalf("window %d past Need with every turnaround past the reference", w)
	}
	l.apply(wcOut{ceils: ceils, growOK: true})
	if w := win(); w != 1<<20 {
		t.Fatalf("window %d with growth allowed, want the ceiling", w)
	}
}

// The lab knobs as a core is made: the queue targets for both ends, stage
// 2, and the fixed window (at most MaxStreamBuffer).
func TestWcoreKnobs(t *testing.T) {
	q, p, qx, px := wcQTarget, wcProbeQ, wcQTargetExit, wcProbeQExit
	kq, kp, ks, kf := WcoreQTarget, WcoreProbeQ, WcoreStage, WcoreFixedWin
	t.Cleanup(func() {
		wcQTarget, wcProbeQ, wcQTargetExit, wcProbeQExit = q, p, qx, px
		WcoreQTarget, WcoreProbeQ, WcoreStage, WcoreFixedWin = kq, kp, ks, kf
	})
	WcoreQTarget, WcoreProbeQ, WcoreStage, WcoreFixedWin = 12, 18, 2, 64<<20
	wcApplyKnobs()
	if wcQTarget != 12*time.Millisecond || wcQTargetExit != 12*time.Millisecond || wcProbeQ != 18*time.Millisecond || wcProbeQExit != 18*time.Millisecond {
		t.Fatalf("queue targets %v %v %v %v", wcQTarget, wcQTargetExit, wcProbeQ, wcProbeQExit)
	}
	cfg := wcTestConfig()
	l := newWcLink(nil, cfg, nil, nil, true, nil)
	if l.core.cfg.stage != 2 || l.fixed != uint32(cfg.MaxStreamBuffer) || l.side != wcExit || l.label() != "link" {
		t.Fatalf("core %+v, fixed %d, side %v, name %q", l.core.cfg, l.fixed, l.side, l.label())
	}
	// no core where no window can grow past Need
	for _, edit := range []func(c *smux.Config){
		func(c *smux.Config) { c.WindowPool = nil },
		func(c *smux.Config) { c.NeedStreamBuffer = 0 },
		func(c *smux.Config) { c.MinStreamBuffer = 0 },
		func(c *smux.Config) { c.NeedStreamBuffer = c.MaxStreamBuffer },
	} {
		c := wcTestConfig()
		edit(c)
		if newWcLink(nil, c, nil, nil, false, nil) != nil {
			t.Fatalf("a core for %+v", c)
		}
	}
}
