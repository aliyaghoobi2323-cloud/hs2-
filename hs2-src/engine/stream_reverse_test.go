package engine

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	mrand "math/rand/v2"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

// startReverseTunnel brings up the stream tunnel in REVERSE: the iran edge
// LISTENS (TLS server) and the kharej exit DIALS in, while the data path and
// smux roles stay the same as direct (iran originates user streams, kharej
// delivers to the panel).
func startReverseTunnel(t *testing.T, nLinks int) *tunnel {
	return startReverseTunnelLog(t, nLinks, nil)
}

func startReverseTunnelLog(t *testing.T, nLinks int, iranLog func(string, ...any)) *tunnel {
	key := bytes.Repeat([]byte{0x5a}, 32)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	panel := echoPanel(t)

	// iran edge: a TLS-carrier server the kharej dials into. Wrap the listener so
	// the test can close the accepted links (the reverse carriers) to force the
	// kharej to redial.
	rawIranLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var lmu = make(chan struct{}, 1)
	lmu <- struct{}{}
	var live []net.Conn
	iranLn := &trackListener{Listener: rawIranLn, onAccept: func(c net.Conn) { <-lmu; live = append(live, c); lmu <- struct{}{} }}
	iranSrv := &tlscarrier.Server{SharedKey: key, Cert: testCert(t), BackendAddr: "127.0.0.1:1"}
	port := freePort(t)
	go RunIran(ctx, IranConfig{
		RevServer: iranSrv, RevListener: iranLn,
		Min: nLinks, Max: nLinks, PerLink: 50, // pin the pattern so the test is deterministic
		ListenIP: "127.0.0.1", Ports: []string{port}, UDP: true,
		Log: iranLog,
	})

	// kharej exit: dials nLinks carriers to the iran edge (pinned pool).
	iranAddr := iranLn.Addr().String()
	go RunKharej(ctx, KharejConfig{
		Panel:    panel,
		RevLinks: nLinks, RevMin: nLinks, RevMax: nLinks,
		RevDial: func() (*tlscarrier.Carrier, error) {
			return tlscarrier.DialFrom(iranAddr, "lab.example.com", key, "")
		},
	})

	tn := &tunnel{userAddr: "127.0.0.1:" + port, cancel: cancel, kill: func() {
		<-lmu
		for _, c := range live {
			c.Close()
		}
		live = nil
		lmu <- struct{}{}
	}}
	// Wait until an end-to-end echo actually works (a reverse link is up).
	for i := 0; i < 120; i++ {
		if echoOnce(tn.userAddr, 256) {
			return tn
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("reverse tunnel never carried an echo")
	return nil
}

func echoOnce(addr string, size int) bool {
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return false
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	msg := make([]byte, size)
	rand.Read(msg)
	if _, err := c.Write(msg); err != nil {
		return false
	}
	got := make([]byte, size)
	if _, err := io.ReadFull(c, got); err != nil {
		return false
	}
	return bytes.Equal(got, msg)
}

func TestReverseStreamTCPAndUDP(t *testing.T) {
	tn := startReverseTunnel(t, 3)
	for i := 0; i < 5; i++ {
		tcpEcho(t, tn.userAddr, 1<<20)
	}
	u, err := net.Dial("udp", tn.userAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	for i := 0; i < 20; i++ {
		msg := []byte("rev-datagram-" + strconv.Itoa(i))
		u.Write(msg)
		u.SetReadDeadline(time.Now().Add(3 * time.Second))
		b := make([]byte, 100)
		n, err := u.Read(b)
		if err != nil || string(b[:n]) != string(msg) {
			t.Fatalf("reverse udp echo %d: %q err=%v", i, b[:n], err)
		}
	}
}

// A reverse link that dies is redialed by the kharej and users get through again.
func TestReverseStreamRedials(t *testing.T) {
	tn := startReverseTunnel(t, 2)
	tcpEcho(t, tn.userAddr, 4096)
	// Kill every accepted reverse link; the kharej's maintainExitLink must redial
	// and the tunnel must carry traffic again.
	tn.kill()
	ok := false
	for i := 0; i < 100; i++ {
		if echoOnce(tn.userAddr, 512) {
			ok = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !ok {
		t.Fatal("reverse tunnel did not recover after its links were killed")
	}
	tcpEcho(t, tn.userAddr, 64<<10)
}

// The iran edge logs every reverse link that goes down, with a reason, and its
// "now N" counts only live links: after all links are killed and redialed the
// count must never exceed the number the kharej keeps (the old code counted
// dead links still waiting for the next health tick and printed "now 16" for 8).
func TestReverseLinkLogsAreHonest(t *testing.T) {
	var mu sync.Mutex
	var lines []string
	logf := func(f string, a ...any) {
		mu.Lock()
		lines = append(lines, fmt.Sprintf(f, a...))
		mu.Unlock()
	}
	const n = 3
	tn := startReverseTunnelLog(t, n, logf)
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		for i := 0; i < 200; i++ {
			if ok() {
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("%s; log:\n%s", what, strings.Join(lines, "\n"))
	}
	count := func(sub string) int {
		mu.Lock()
		defer mu.Unlock()
		c := 0
		for _, l := range lines {
			if strings.Contains(l, sub) {
				c++
			}
		}
		return c
	}
	waitFor("links never came up", func() bool { return count("up from 127.0.0.1") >= n })
	tn.kill()
	waitFor("link closures were not logged", func() bool { return count("down: ") >= n })
	waitFor("links were not redialed", func() bool { return count("up from 127.0.0.1") >= 2*n })
	re := regexp.MustCompile(`\(now (\d+)\)`)
	mu.Lock()
	defer mu.Unlock()
	for _, l := range lines {
		if !strings.Contains(l, "reverse link") {
			continue
		}
		if m := re.FindStringSubmatch(l); m != nil {
			if v, _ := strconv.Atoi(m[1]); v > n {
				t.Fatalf("log claims %d links but only %d exist: %q", v, n, l)
			}
		}
		if strings.Contains(l, "down: ") && strings.HasSuffix(strings.TrimSpace(strings.SplitN(l, "down: ", 2)[1]), ":") {
			t.Fatalf("empty reason: %q", l)
		}
	}
}

// End to end over real TLS: the reverse exit's live link count follows the
// edge's target sent on the pool-control channel. The exit comes up warm (8),
// like production, and is brought down to the edge's 4 only by the edge
// closing idle links — the exit retires those slots and redials none — then
// grows at once. The shrink under load, with a connection held open on a
// retiring link, is TestReverseShrinkKeepsHeldConnection.
func TestReverseExitPoolFollowsEdgeTarget(t *testing.T) {
	r := startV2(t, v2Opts{min: 2, max: 8, perLink: 50, pin: 4, exitLinks: 8, exitMin: 2, exitMax: 8})
	r.waitFor("the exit's warm 8 links up", 5*time.Second, func() bool { return r.lm.Stats().Links == 8 })
	r.waitFor("the warm 8 shrink to the edge's target 4", 20*time.Second, func() bool {
		s, e := r.lm.Stats(), r.exitView()
		return s.Links == 4 && s.Serving == 4 && e.Links == 4 && e.Target == 4
	})
	if n := r.dials.Load(); n != 8 {
		t.Fatalf("the exit dialled %d links; want its warm 8 only (retired slots are not redialled)", n)
	}
	r.lm.pin.Store(7)
	r.waitFor("the exit grows to the target 7", 10*time.Second, func() bool {
		s, e := r.lm.Stats(), r.exitView()
		return s.Links == 7 && s.Serving == 7 && e.Links == 7 && e.Target == 7
	})
	if n := r.dials.Load(); n != 11 {
		t.Fatalf("the exit dialled %d links in all; want 8 + 3", n)
	}
	if !echoOnce(r.userAddr, 4096) {
		t.Fatal("tunnel did not carry new connections after resizing")
	}
	r.checkNoRedial()
}

// 28. Shrinking never cuts a connection that is in use and costs no dials; a
// retiring link comes back into service before anything new is dialled.
//
// Envelope 2..8, drainIdle 2 s, pinned at 7, with 20 idle connections, two
// bulk flows and one held connection (a round trip every 200 ms, not a bulk
// flow) on a link of its own (heldScenario). Pin 2: the pool serves 2 within
// 5 s (the bulk links); the idle connections on the retiring links get a clean
// EOF and the emptied links close, so within 15 s only the held link is left
// beyond the 2; the held connection never fails and the exit dials nothing.
// Pin 6: the held link is back in service before the first new link arrives,
// and the exit dials exactly the 3 missing links. Pin 2 again, then close the
// held connection: exactly 2 links remain, still with no dial.
func TestReverseShrinkKeepsHeldConnection(t *testing.T) {
	if testing.Short() {
		t.Skip("real-time pool resizing (~30s)")
	}
	r := startV2(t, v2Opts{min: 2, max: 8, perLink: 8, pin: 7, target: 7,
		exitLinks: 7, exitMin: 2, exitMax: 8, drainIdle: 2 * time.Second})
	r.waitFor("7 serving links", 10*time.Second, func() bool {
		s := r.lm.Stats()
		return s.Serving == 7 && s.Links == 7 && r.exitView().Links == 7
	})
	sc := r.startHeldScenario(20)
	exitMark := r.exit.mark()

	// 7 → 2 with the held connection's link retiring.
	heldID := r.shrinkWithHeld(sc)

	// 3 → 6: un-retire first; the exit dials only what is missing.
	dials := r.dials.Load()
	mark := r.edge.mark()
	r.lm.pin.Store(6)
	r.waitFor("6 serving links", 8*time.Second, func() bool {
		s := r.lm.Stats()
		return s.Serving == 6 && s.Links == 6 && s.Retiring == 0 && r.exitView().Links == 6
	})
	back := r.edge.first(mark, fmt.Sprintf("link(s) %d back in service", heldID))
	up := r.edge.first(mark, "reverse link", " up from ")
	if back < 0 || up < 0 || back > up {
		t.Fatalf("retiring link %d was not back in service before a new link came up (line %d vs %d):\n%s",
			heldID, back, up, strings.Join(r.edge.snapshot()[mark:], "\n"))
	}
	time.Sleep(2500 * time.Millisecond) // an extra dial would show by now
	if d := r.dials.Load() - dials; d != 3 {
		t.Fatalf("the exit dialled %d links to go from 2 serving + 1 retiring to 6; want 3", d)
	}
	if row, ok := r.row(heldID); !ok || !row.serving {
		t.Fatalf("held link %d is not serving after the grow: %+v", heldID, r.rows())
	}
	sc.checkFlows(t, "after growing")

	// 6 → 2 again: the held link (busier than the three new, empty ones, idler
	// than the bulk links) retires with them; the empty ones close, the held
	// one stays until its connection ends.
	dials = r.dials.Load()
	r.lm.pin.Store(2)
	r.waitFor("4 links retiring, the held one among them", 5*time.Second, func() bool {
		row, ok := r.row(heldID)
		s := r.lm.Stats()
		return s.Serving == 2 && s.Retiring == 4 && ok && row.retiring
	})
	r.waitFor("the 3 empty links close while the held one stays", 12*time.Second, func() bool {
		s := r.lm.Stats()
		return s.Links == 3 && s.Retiring == 1 && s.HeldBy == 1 && r.exitView().Links == 3
	})
	sc.checkFlows(t, "during the second shrink")
	r.closeHeld(sc)
	if d := r.dials.Load() - dials; d != 0 {
		t.Fatalf("%d link(s) dialled during the second shrink; want none", d)
	}
	for i, b := range sc.bulk {
		if err := b.close(); err != nil {
			t.Fatalf("bulk flow %d broke: %v", i+1, err)
		}
	}
	if n := sc.probeSurvivors(t); n == 0 {
		t.Fatal("no idle connection was left on the serving links to check")
	}
	if n := r.exit.count("retired — pattern shrinking", exitMark); n != 8 {
		t.Fatalf("the exit retired %d slots; want 8 (4 per shrink)", n)
	}
	r.checkNoRedial()
}

// ratePanel serves one fixed payload, paced to rate bytes/s, to each
// connection that sends 'D'. A connection that sends nothing just sits there,
// like an idle xray connection, until the tunnel closes it.
func ratePanel(t *testing.T, payload []byte, rate int) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				var cmd [1]byte
				if _, err := io.ReadFull(c, cmd[:]); err != nil || cmd[0] != 'D' {
					return
				}
				chunk := rate / 20
				start := time.Now()
				for off := 0; off < len(payload); off += chunk {
					end := min(off+chunk, len(payload))
					if _, err := c.Write(payload[off:end]); err != nil {
						return
					}
					time.Sleep(time.Until(start.Add(time.Duration(float64(end) / float64(rate) * float64(time.Second)))))
				}
			}()
		}
	}()
	return ln.Addr().String()
}

// download fetches the rate panel's payload through the tunnel.
type download struct {
	done chan struct{}
	n    int64
	sum  [sha256.Size]byte
	err  error
	took time.Duration
}

func startDownload(addr string) *download {
	d := &download{done: make(chan struct{})}
	go func() {
		defer close(d.done)
		start := time.Now()
		c, err := net.DialTimeout("tcp", addr, 3*time.Second)
		if err != nil {
			d.err = err
			return
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(45 * time.Second))
		if _, err := c.Write([]byte{'D'}); err != nil {
			d.err = err
			return
		}
		h := sha256.New()
		d.n, d.err = io.Copy(h, c)
		copy(d.sum[:], h.Sum(nil))
		d.took = time.Since(start)
	}()
	return d
}

// 29. The production shape, unpinned: 200 idle connections spread over the
// warm 8 links and 2 long rate-limited downloads. The controller (clocks
// shortened) steps the pool down to min; the retiring links are emptied only by
// reclaiming connections idle for drainIdle, never younger ones, and then
// closed, so the pool falls to ≤ min+1 links with the exit dialling nothing,
// while both downloads complete intact.
func TestReverseShrinkUnderIdleConnsKeepsDownloads(t *testing.T) {
	if testing.Short() {
		t.Skip("real-time pool resizing (~25s)")
	}
	const (
		drainIdle = 5 * time.Second
		rate      = 256 << 10 // per download, bytes/s
		size      = 4 << 20   // per download: ~16 s at rate
		nIdle     = 200
		nYoung    = 20
	)
	payload := make([]byte, size)
	rng := mrand.New(mrand.NewPCG(28, 29))
	for i := 0; i < len(payload); i += 8 {
		binary.LittleEndian.PutUint64(payload[i:], rng.Uint64())
	}
	want := sha256.Sum256(payload)
	start := time.Now()
	r := startV2(t, v2Opts{min: 2, max: 8, perLink: 8, exitLinks: 8, exitMin: 2, exitMax: 8,
		drainIdle: drainIdle, panel: ratePanel(t, payload, rate),
		tune: func(tu *apTunables) {
			tu.histTicks = 5                         // 10 s windows instead of 60 s
			tu.shrinkDwell = 3500 * time.Millisecond // 2 ticks below target, not 60 s
			tu.shrinkStep = 1500 * time.Millisecond  // a step every tick, not every 30 s
			tu.noShrinkAfterGrow = 3500 * time.Millisecond
		}})
	r.waitFor("the warm 8 links", 10*time.Second, func() bool {
		return r.lm.Stats().Serving == 8 && r.exitView().Links == 8
	})

	var dls [2]*download
	for i := range dls {
		dls[i] = startDownload(r.userAddr)
	}
	var conns []*idleConn
	t.Cleanup(func() {
		for _, ic := range conns {
			ic.c.Close()
		}
	})
	for i := 0; i < nIdle; i++ {
		ic, err := dialIdle(r.userAddr, false)
		if err != nil {
			t.Fatalf("idle connection %d: %v", i, err)
		}
		conns = append(conns, ic)
	}
	r.waitFor("every connection placed on a link", 10*time.Second, func() bool { return r.lm.Stats().Users == nIdle+2 })

	// Young connections, opened as the first shrink step lands: they go to the
	// links still serving, some of which the next steps retire while these are
	// only seconds old.
	r.waitFor("the controller's first shrink step", 12*time.Second, func() bool { return r.lm.Stats().Target < 8 })
	for i := 0; i < nYoung; i++ {
		ic, err := dialIdle(r.userAddr, false)
		if err != nil {
			t.Fatalf("young connection %d: %v", i, err)
		}
		conns = append(conns, ic)
	}

	r.waitFor("≤ min+1 links at both ends", 28*time.Second-time.Since(start), func() bool {
		s, e := r.lm.Stats(), r.exitView()
		return s.Links <= 3 && s.Serving <= 3 && e.Links >= 0 && e.Links <= 3
	})
	s := r.lm.Stats()
	t.Logf("%.1fs: %d links (%d serving, target %d), %d connections open", time.Since(start).Seconds(),
		s.Links, s.Serving, s.Target, s.Users)
	if s.Target != 2 {
		t.Fatalf("the controller settled at %d serving links; want min (2): %s", s.Target, s.Reason)
	}

	for i, d := range dls {
		select {
		case <-d.done:
		case <-time.After(20 * time.Second):
			t.Fatalf("download %d did not finish", i+1)
		}
		if d.err != nil || d.n != size || d.sum != want {
			t.Fatalf("download %d: %d of %d bytes, err %v, sha256 ok %v", i+1, d.n, size, d.err, d.sum == want)
		}
		t.Logf("download %d: %d bytes in %.1fs, sha256 ok", i+1, d.n, d.took.Seconds())
	}

	reclaimed, young := 0, 0
	for i, ic := range conns {
		if !ic.ended() {
			continue
		}
		if !errors.Is(ic.endErr, io.EOF) {
			t.Fatalf("connection %d ended with %v; want a clean EOF (FIN, not RST)", i, ic.endErr)
		}
		if age := ic.endAt.Sub(ic.opened); age < drainIdle {
			t.Fatalf("connection %d was closed %.1fs after it opened: younger than drainIdle (%s)", i, age.Seconds(), drainIdle)
		}
		reclaimed++
		if i >= nIdle {
			young++
		}
	}
	if young == 0 {
		t.Fatal("no young connection was on a retiring link: the age rule was not exercised")
	}
	t.Logf("reclaimed %d idle connections (%d of the %d young ones, each only after %s idle)", reclaimed, young, nYoung, drainIdle)
	if n := r.dials.Load(); n != 8 {
		t.Fatalf("the exit dialled %d links; want its warm 8 only", n)
	}
	r.checkNoRedial()
}
