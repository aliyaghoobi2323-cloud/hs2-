package engine

import (
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

// The R1 round-4 judge's tests for the red team's C4 (a QUIET link's death)
// and C5 (a user that stops reading), quietuser.go and rescue.go.

// killRig is quietRig with nq QUIET links, each one's wires its own (so one
// can be cut: a reset of its TCP connection, every byte in flight lost).
type killRig struct {
	*quietRig
	qs    []*classPair
	wires map[*classPair][2]*angleWire
}

func newKillRig(t *testing.T, nq int, rate float64, delay time.Duration) *killRig {
	t.Helper()
	if QuietOff() {
		t.Skip("HS2_TUNE_QUIET=0")
	}
	r := &quietRig{t: t}
	r.ctx, r.cancel = context.WithCancel(context.Background())
	t.Cleanup(r.cancel)
	k := &killRig{quietRig: r, wires: map[*classPair][2]*angleWire{}}
	pair := func(class uint32) *classPair {
		up, dn := newAngleWire(rate, delay), newAngleWire(rate, delay)
		r.wires = append(r.wires, up, dn)
		p := newClassPair(t, up, dn, classCaps(class))
		k.wires[p] = [2]*angleWire{up, dn}
		return p
	}
	for i := 0; i < nq; i++ {
		k.qs = append(k.qs, pair(tlscarrier.CapQuiet))
	}
	r.bulk = pair(tlscarrier.CapBulk)
	r.quiet = k.qs[0]
	t.Cleanup(func() {
		for _, w := range r.wires {
			w.close()
		}
	})
	r.lm = NewLinkManager(nil, 1, 1, 8, t.Logf)
	r.q = r.lm.enableQuiet()
	for _, p := range k.qs {
		r.q.place(p.edge, "")
	}
	addManaged(r.lm, r.bulk.edge)
	go r.q.runMover(r.ctx)
	for _, p := range append(append([]*classPair(nil), k.qs...), r.bulk) {
		go r.serve(p)
	}
	return k
}

// kill cuts p's wires both ways (a reset: what was in flight is gone) and
// waits for its edge session to end.
func (k *killRig) kill(p *classPair) {
	k.t.Helper()
	for _, w := range k.wires[p] {
		w.close()
	}
	waitQuiet(k.t, "the cut link's session ends", func() bool { return !p.edge.Alive() })
}

// busiest is the QUIET link with the most connections now (the worst one to
// lose).
func (k *killRig) busiest() *classPair {
	var best *classPair
	for _, p := range k.qs {
		if best == nil || p.edge.Active() > best.edge.Active() {
			best = p
		}
	}
	return best
}

// killEcho sends 8 bytes on st and reads them back within d.
func killEcho(st stream, d time.Duration) error {
	if dl, ok := st.(interface{ SetDeadline(time.Time) error }); ok {
		dl.SetDeadline(time.Now().Add(d))
		defer dl.SetDeadline(time.Time{})
	} else {
		if w, ok := st.(interface{ SetWriteDeadline(time.Time) error }); ok {
			w.SetWriteDeadline(time.Now().Add(d))
			defer w.SetWriteDeadline(time.Time{})
		}
		if r, ok := st.(interface{ SetReadDeadline(time.Time) error }); ok {
			r.SetReadDeadline(time.Now().Add(d))
			defer r.SetReadDeadline(time.Time{})
		}
	}
	b := []byte("01234567")
	if _, err := st.Write(b); err != nil {
		return err
	}
	_, err := io.ReadFull(st, b)
	return err
}

// killShare is one run of TestQuietKillShare: 12 users, each with 3 light
// connections (an echo each: a game, a call, a page's objects) and one
// download that moves to the BULK link; then the busiest QUIET link is cut.
// It returns the light connections that ended and the users that lost one,
// and whether every moved download still runs.
func killShare(t *testing.T, nq int) (cut, light, hit, users int, downsOK bool) {
	k := newKillRig(t, nq, 4*angleRateBps, 5*time.Millisecond)
	const nu, per = 12, 3
	type conn struct {
		user int
		st   stream
	}
	var lights []conn
	var downs []*atomic.Int64
	for u := 0; u < nu; u++ {
		tag := uint32(1000 + u)
		for i := 0; i < per; i++ {
			st := k.open(tag, qpEcho)
			if err := killEcho(st, 3*time.Second); err != nil {
				t.Fatalf("user %d light %d: %v", u, i, err)
			}
			lights = append(lights, conn{u, st})
		}
		st := k.open(tag, qpDown)
		n := new(atomic.Int64)
		downs = append(downs, n)
		go func() {
			b := make([]byte, 32<<10)
			for {
				m, err := st.Read(b)
				n.Add(int64(m))
				if err != nil {
					n.Store(-1 << 40)
					return
				}
			}
		}()
	}
	waitQuiet(t, "every download moved", func() bool { return k.q.moved.Load() >= nu })
	time.Sleep(300 * time.Millisecond)
	k.kill(k.busiest())
	time.Sleep(200 * time.Millisecond)
	lost := map[int]bool{}
	for _, c := range lights {
		if err := killEcho(c.st, 2*time.Second); err != nil {
			cut++
			lost[c.user] = true
		}
	}
	before := make([]int64, len(downs))
	for i, n := range downs {
		before[i] = n.Load()
	}
	time.Sleep(500 * time.Millisecond)
	downsOK = true
	for i, n := range downs {
		if n.Load() <= before[i] {
			downsOK = false
		}
	}
	for _, c := range lights {
		c.st.Close()
	}
	return cut, len(lights), len(lost), nu, downsOK
}

// TestQuietKillShare (the red team's C4, round4-judge.fa.md item 2): one of
// Q QUIET links dies under 12 users of 3 light connections each (and a
// moved download each). Spread by count (round 3) the death cuts ~1/Q of the
// light connections and hits nearly every user for Q >= 2; by user
// (QuietByUser) the same share of connections, all of ~1/Q of the users.
// The downloads that moved to the BULK link run on in both.
func TestQuietKillShare(t *testing.T) {
	angleCores(t)
	withTune(t, QuietLight, 0) // the links as the general ones: TestQuietKillShareLight has the light one
	for _, nq := range []int{1, 2, 4} {
		for _, by := range []int{0, 1} {
			t.Run(fmt.Sprintf("Q%d/byuser%d", nq, by), func(t *testing.T) {
				QuietByUser.Set(by)
				t.Cleanup(func() { QuietByUser.Set(0) })
				cut, light, hit, users, downsOK := killShare(t, nq)
				t.Logf("Q=%d by user %d: light connections cut %d of %d (%.0f%%), users hit %d of %d (%.0f%%), moved downloads run on %v",
					nq, by, cut, light, 100*float64(cut)/float64(light), hit, users, 100*float64(hit)/float64(users), downsOK)
				if !downsOK {
					t.Errorf("a moved download stopped with the QUIET link")
				}
				// what one death may cut: all of it with one link; with Q
				// links ~1/Q of the connections: by user ±1 user a link; by
				// count the busiest link (the one cut) may hold up to half
				// a share more (the moved downloads' slots come free)
				slack := per(nq)
				if by == 0 && nq > 1 {
					slack += light / (2 * nq)
				}
				if want := light / nq; cut < want-per(nq) || cut > want+slack {
					t.Errorf("cut %d of %d light connections, want about %d", cut, light, want)
				}
				switch {
				case nq == 1 && hit != users:
					t.Errorf("one QUIET link: %d of %d users hit, want all", hit, users)
				case nq > 1 && by == 1 && hit > (users+nq-1)/nq:
					t.Errorf("by user: %d of %d users hit, want at most %d", hit, users, (users+nq-1)/nq)
				case nq > 1 && by == 0 && hit <= (users+nq-1)/nq:
					t.Errorf("by count: only %d of %d users hit (the spread changed?)", hit, users)
				}
			})
		}
	}
}

// TestQuietKillShareLight: the default past five links, four general QUIET
// links and the light one (R1 round 5): one death cuts what it cut with
// four, a share of the connections (no one opens on the light link, so the
// other four hold them all); the light link adds none to the kill.
func TestQuietKillShareLight(t *testing.T) {
	angleCores(t)
	withTune(t, QuietLight, 2)
	for _, by := range []int{0, 1} {
		t.Run(fmt.Sprintf("byuser%d", by), func(t *testing.T) {
			QuietByUser.Set(by)
			t.Cleanup(func() { QuietByUser.Set(0) })
			cut, light, hit, users, downsOK := killShare(t, 5)
			t.Logf("four general and the light one, by user %d: light connections cut %d of %d, users hit %d of %d, moved downloads run on %v", by, cut, light, hit, users, downsOK)
			if !downsOK {
				t.Errorf("a moved download stopped with the QUIET link")
			}
			slack := per(4)
			if by == 0 {
				slack += light / 8
			}
			if want := light / 4; cut < want-per(4) || cut > want+slack {
				t.Errorf("cut %d of %d light connections, want about %d", cut, light, want)
			}
			if by == 1 && hit > (users+3)/4 {
				t.Errorf("by user: %d of %d users hit, want at most %d", hit, users, (users+3)/4)
			}
		})
	}
}

// per is the slack of an even spread over nq links: a link's share may be
// one user (3 connections) off.
func per(nq int) int {
	if nq == 1 {
		return 0
	}
	return 3
}

// TestQuietByUserSticky: with QuietByUser a user's connections stay on its
// link while it is up (and only lagging sends one elsewhere), new users go
// to the link with the fewest users, and a user whose link died gets another.
// TestQuietPack: with QuietPack a small pool's new connections go to its
// oldest QUIET link while that one does not lag, and to the next while it
// does; without it, by the fewest connections.
func TestQuietPack(t *testing.T) {
	angleCores(t)
	withTune(t, QuietLight, 0)
	defer QuietPack.Set(QuietPack.Get())
	for _, pack := range []int{0, 1} {
		QuietPack.Set(pack)
		k := newKillRig(t, 2, 4*angleRateBps, 5*time.Millisecond)
		var rels []func()
		on := map[*mtcpLink]int{}
		open := func(tag uint32) *mtcpLink {
			ctx := context.WithValue(k.ctx, userTagKey{}, tag)
			st, _, rel, ok := openStreamOn(ctx, k.lm, false, qpEcho)
			if !ok {
				t.Fatal("no stream opened")
			}
			rels = append(rels, rel)
			l := st.(*groupStream).ql
			on[l]++
			return l
		}
		first := open(1)
		for u := uint32(2); u <= 6; u++ {
			open(u)
		}
		t.Logf("pack %d: six connections over the QUIET links %v", pack, on)
		switch {
		case pack == 1 && on[first] != 6:
			t.Fatalf("pack: %d of 6 on the oldest QUIET link", on[first])
		case pack == 0 && (on[first] != 3 || len(on) != 2):
			t.Fatalf("spread: %v", on)
		}
		if pack == 1 {
			first.mtr.ctrlSlow.Store(true) // the oldest lags: the next one spills
			if l := open(7); l == first {
				t.Fatal("pack: a connection on the lagging link while the other did not lag")
			}
			first.mtr.ctrlSlow.Store(false)
		}
		for _, r := range rels {
			r()
		}
	}
}

func TestQuietByUserSticky(t *testing.T) {
	angleCores(t)
	withTune(t, QuietLight, 0)
	QuietByUser.Set(1)
	t.Cleanup(func() { QuietByUser.Set(0) })
	k := newKillRig(t, 2, 4*angleRateBps, 5*time.Millisecond)
	linkOf := func(st stream) *mtcpLink { return st.(*groupStream).ql }
	var rels []func()
	open := func(tag uint32) stream {
		// as a user connection: its slot is given back when it ends
		ctx := context.WithValue(k.ctx, userTagKey{}, tag)
		st, _, rel, ok := openStreamOn(ctx, k.lm, false, qpEcho)
		if !ok {
			t.Fatal("no stream opened")
		}
		rels = append(rels, rel)
		return st
	}
	a1, a2, a3 := open(1), open(1), open(1)
	b1 := open(2)
	if linkOf(a1) != linkOf(a2) || linkOf(a1) != linkOf(a3) {
		t.Fatalf("user 1's connections on more than one QUIET link")
	}
	if linkOf(b1) == linkOf(a1) {
		t.Fatalf("user 2 on user 1's link while the other had no user")
	}
	if line := k.q.userLine(); line == "" {
		t.Fatalf("no by-user status")
	} else {
		t.Log(line)
	}
	// user 1's link dies: its next connection goes to the other link, which
	// becomes its own
	var dead *classPair
	for _, p := range k.qs {
		if p.edge == linkOf(a1) {
			dead = p
		}
	}
	k.kill(dead)
	a4 := open(1)
	a5 := open(1)
	if linkOf(a4) == dead.edge || linkOf(a5) != linkOf(a4) {
		t.Fatalf("after its link died, user 1 is not on one live link")
	}
	if err := killEcho(a4, 3*time.Second); err != nil {
		t.Fatal(err)
	}
	for _, st := range []stream{a1, a2, a3, b1, a4, a5} {
		st.Close()
	}
	for _, rel := range rels {
		rel()
	}
	waitQuiet(t, "the users let go", func() bool {
		k.q.mu.Lock()
		defer k.q.mu.Unlock()
		return len(k.q.byUser) == 0
	})
}

// TestRescueDead (the red team's C4; RescueDead): a connection whose link
// dies before any of the exit's bytes reached its app — its app waiting for
// the panel's first bytes, or having sent one TLS hello — moves to another
// link and gets them there; without the switch it ends with the link. One
// whose app sent other bytes ends either way (they may have been acted on).
func TestRescueDead(t *testing.T) {
	angleCores(t)
	hello := []byte{22, 3, 1, 0, 5, 1, 0, 0, 1, 3} // a TLS record with a (short) ClientHello
	cases := []struct {
		name  string
		port  int
		first []byte
	}{
		{"server first", qpDown, nil},
		{"tls hello", qpEcho, hello},
		{"other bytes", qpEcho, []byte("GET / HTTP/1.1\r\n")},
	}
	for _, c := range cases {
		for _, on := range []int{0, 1} {
			t.Run(fmt.Sprintf("%s/rescue%d", c.name, on), func(t *testing.T) {
				RescueDead.Set(on)
				t.Cleanup(func() { RescueDead.Set(0) })
				// a long path: the link dies before the exit's answer is back
				k := newKillRig(t, 2, 4*angleRateBps, 150*time.Millisecond)
				ctx := context.WithValue(k.ctx, userTagKey{}, uint32(5))
				st, link, rel, ok := openStreamOn(ctx, k.lm, false, c.port)
				if !ok {
					t.Fatal("no stream opened")
				}
				rs := newRescuable(ctx, k.lm, c.port, st, link, rel)
				defer rs.release()
				if c.first != nil {
					if _, err := rs.Write(c.first); err != nil {
						t.Fatal(err)
					}
				}
				for _, p := range k.qs {
					if Link(p.edge) == link {
						for _, w := range k.wires[p] {
							w.close()
						}
					}
				}
				got := make(chan error, 1)
				go func() {
					b := make([]byte, max(len(c.first), 1))
					_, err := io.ReadFull(rs, b)
					if err == nil && c.first != nil && string(b) != string(c.first) {
						err = fmt.Errorf("echo %q", b)
					}
					got <- err
				}()
				var err error
				select {
				case err = <-got:
				case <-time.After(5 * time.Second):
					err = fmt.Errorf("nothing in 5 s")
				}
				moved := rs.on() != st
				t.Logf("%s, rescue %d: moved %v, read: %v (dead-link moves %d)", c.name, on, moved, err, k.lm.rescue.deadMoves.Load())
				want := on == 1 && c.name != "other bytes"
				if want && (err != nil || !moved) {
					t.Fatalf("not rescued: moved %v, %v", moved, err)
				}
				if !want && err == nil {
					t.Fatalf("read through without a rescue")
				}
				rs.Close()
			})
		}
	}
}

// TestQuietHoldCap (the red team's C5, round4-judge.fa.md item 7): a user
// whose app stops reading opens downloads in waves on the QUIET link (each
// pins its window in the link's session buffer); another user's echo on the
// same link is timed. Without the cap the link's buffer fills and its reader
// stops: the echo stalls; with QuietHoldCap the frozen user's connections
// leave the QUIET link (its new ones open on the BULK link) once it holds
// more than the cap unread, and the echo keeps its pace.
func TestQuietHoldCap(t *testing.T) {
	angleCores(t)
	for _, capB := range []int{0, 1 << 20} {
		t.Run(fmt.Sprintf("cap%d", capB), func(t *testing.T) {
			QuietHoldCap.Set(capB)
			t.Cleanup(func() { QuietHoldCap.Set(0) })
			r := newQuietRig(t, 12.5e6, 5*time.Millisecond, false)
			echo := r.open(9, qpEcho)
			if err := killEcho(echo, 3*time.Second); err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			var worst time.Duration
			var fails int
			stop := make(chan struct{})
			done := make(chan struct{})
			go func() {
				defer close(done)
				for {
					select {
					case <-stop:
						return
					case <-time.After(20 * time.Millisecond):
					}
					t0 := time.Now()
					err := killEcho(echo, 2*time.Second)
					d := time.Since(t0)
					mu.Lock()
					worst = max(worst, d)
					if err != nil {
						fails++
					}
					mu.Unlock()
					if err != nil {
						return
					}
				}
			}()
			// the frozen user: 8 downloads every 100 ms, never read
			var frozen []stream
			for w := 0; w < 30; w++ {
				for i := 0; i < 8; i++ {
					ctx := context.WithValue(r.ctx, userTagKey{}, uint32(7))
					st, _, _, ok := openStreamOn(ctx, r.lm, false, qpDown)
					if !ok {
						t.Fatal("no stream opened")
					}
					frozen = append(frozen, st)
				}
				time.Sleep(100 * time.Millisecond)
			}
			time.Sleep(time.Second)
			close(stop)
			<-done
			mu.Lock()
			w, f := worst, fails
			mu.Unlock()
			parks := r.quiet.edge.sess.Parks()
			t.Logf("cap %d KiB: %d frozen downloads; the other user's echo: worst %v, failed %d; QUIET session parks %d, unread there %d KiB; %s",
				capB>>10, len(frozen), w.Round(time.Millisecond), f, parks, quietUnread(r)>>10, r.q.userLine())
			if capB > 0 && (f > 0 || w > 500*time.Millisecond) {
				t.Errorf("with the cap the echo still stalled: worst %v, failed %d", w, f)
			}
			for _, st := range frozen {
				st.Close()
			}
			echo.Close()
		})
	}
}

// quietUnread is what the QUIET link's edge session holds unread now.
func quietUnread(r *quietRig) int {
	n := 0
	r.q.gmu.Lock()
	gs := append([]*groupStream(nil), r.q.groups...)
	r.q.gmu.Unlock()
	for _, s := range gs {
		n += unreadQuiet(s)
	}
	r.q.hold.mu.Lock()
	for _, s := range r.q.hold.moved {
		n += s.g.Main().Unread()
	}
	r.q.hold.mu.Unlock()
	return n
}
