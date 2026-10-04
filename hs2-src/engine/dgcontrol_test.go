package engine

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/core"
)

// dgRig is an edge pool and an exit pool joined carrier by carrier through
// in-memory pairs, so what one side tells the other over TypeClose can be
// watched on the other side's carrier.
type dgRig struct {
	t          *testing.T
	ctx        context.Context
	edge, exit *dgPool
	peer       map[*dgLink]*dgLink // edge carrier -> the exit's end of it
	mu         sync.Mutex
	logs       []string
}

func newDgRig(t *testing.T, reverse bool) *dgRig {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	r := &dgRig{t: t, ctx: ctx, peer: map[*dgLink]*dgLink{}}
	logf := func(f string, a ...any) {
		r.mu.Lock()
		r.logs = append(r.logs, fmt.Sprintf(f, a...))
		r.mu.Unlock()
	}
	edgeTUN, exitTUN := newFakeTUN(1400), newFakeTUN(1400)
	for _, d := range []*fakeTUN{edgeTUN, exitTUN} {
		go func(d *fakeTUN) { // a TUN nobody reads would stall the pools' readers
			for {
				select {
				case <-d.out:
				case <-ctx.Done():
					return
				}
			}
		}(d)
	}
	r.edge = newDgPool(edgeTUN, 1, 32, 8, logf)
	r.exit = newDgPool(exitTUN, 1, 32, 8, logf)
	r.exit.downSender = true
	if reverse {
		r.edge.accept = true
		r.exit.revExit = true
	}
	return r
}

func (r *dgRig) log() string { r.mu.Lock(); defer r.mu.Unlock(); return strings.Join(r.logs, "\n") }

// carrier joins one carrier to both pools; rx (if not zero) is when the
// edge's end last heard its peer.
func (r *dgRig) carrier(rx ...time.Time) (*dgLink, *dgLink, *dgFakeCarrier, *dgFakeCarrier) {
	e, x := newDgFakePair()
	if len(rx) > 0 {
		e.rx.Store(rx[0].UnixNano())
	}
	el := r.edge.add(r.ctx, e, "test")
	xl := r.exit.add(r.ctx, x, "test")
	r.peer[el] = xl
	return el, xl, e, x
}

func within(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("not within %s: %s", d, what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A carrier closed on purpose is dead on the other side at once — it used
// to stay "alive" there for 15-17 s, taking new flows into the void.
func TestDgByeClosesTheOtherEnd(t *testing.T) {
	r := newDgRig(t, false)
	el, xl, _, _ := r.carrier()
	r.carrier()
	r.edge.closeLink(el)
	within(t, time.Second, "the exit's end of a closed carrier dies", func() bool { return !xl.alive() })
	if !strings.Contains(r.log(), "closed by the other server") {
		t.Fatalf("no line for it:\n%s", r.log())
	}
	// A whole pool stopping tells the other side too.
	r.edge.closeAll()
	within(t, time.Second, "every exit carrier dies when the edge stops", func() bool { return r.exit.count() == 0 || allDead(r.exit) })
}

func allDead(p *dgPool) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, l := range p.set {
		if l.alive() {
			return false
		}
	}
	return true
}

// What the edge retires, the exit stops placing new flows on (download used
// to keep every retiring carrier busy, so a shrink never finished); when it
// serves again the exit uses it again.
func TestDgRetireMirroredToTheOtherSide(t *testing.T) {
	r := newDgRig(t, false)
	for i := 0; i < 4; i++ {
		r.carrier()
	}
	r.edge.reconcile(r.ctx, 2)
	var retired []*dgLink
	r.edge.mu.RLock()
	for _, l := range r.edge.set {
		if l.retiring {
			retired = append(retired, r.peer[l])
		}
	}
	r.edge.mu.RUnlock()
	if len(retired) != 2 {
		t.Fatalf("%d retired, want 2", len(retired))
	}
	within(t, time.Second, "the exit learns which carriers retire", func() bool {
		return retired[0].peerRetiring() && retired[1].peerRetiring()
	})
	for f := uint32(0); f < 2000; f++ {
		if l := r.exit.pickHash(f); l == retired[0] || l == retired[1] {
			t.Fatalf("flow %d hashed onto a carrier the edge retires", f)
		}
	}
	r.edge.reconcile(r.ctx, 4) // grows back: un-retires them
	within(t, time.Second, "the exit learns they serve again", func() bool {
		return !retired[0].peerRetiring() && !retired[1].peerRetiring()
	})
}

// The reverse edge serves again what it retired (or what arrived spare) when
// its target rises: the exit never dials while those carriers live, so the
// edge used to stay below its target after any shrink.
func TestDgReverseEdgeUnretires(t *testing.T) {
	r := newDgRig(t, true)
	r.edge.target.Store(6)
	for i := 0; i < 6; i++ {
		r.carrier()
	}
	r.edge.target.Store(2)
	r.edge.reconcileReverseEdge()
	if s, rt := r.edge.countsLive(); s != 2 || rt != 4 {
		t.Fatalf("after the shrink: serving %d retiring %d, want 2/4", s, rt)
	}
	r.edge.target.Store(6)
	r.edge.reconcileReverseEdge()
	if s, rt := r.edge.countsLive(); s != 6 || rt != 0 {
		t.Fatalf("after the target rose: serving %d retiring %d, want 6/0", s, rt)
	}
	// A carrier born spare (it arrived above the target) serves when needed.
	el, xl, _, _ := r.carrier()
	if !el.retiring || !el.bornSpare {
		t.Fatal("a carrier above the target did not arrive spare")
	}
	within(t, time.Second, "the exit learns the spare retires", func() bool { return xl.peerRetiring() })
	r.edge.target.Store(7)
	r.edge.reconcileReverseEdge()
	if el.retiring || el.bornSpare {
		t.Fatal("the spare did not serve when the target rose")
	}
	within(t, time.Second, "the exit learns it serves", func() bool { return !xl.peerRetiring() })
}

// A fresh carrier proves the path works, so carriers that have heard nothing
// for 3 s are dead (a restarted peer drops their packets) and go at once;
// the live ones stay.
func TestDgSilentCarriersDroppedWhenAFreshOneArrives(t *testing.T) {
	r := newDgRig(t, false)
	// Three carriers whose peer went away 5 s ago (they arrived before that:
	// nothing was silent when each came up), one that hears it.
	var silent []*dgLink
	for i := 0; i < 3; i++ {
		el, _, _, _ := r.carrier()
		silent = append(silent, el)
	}
	live, _, le, _ := r.carrier()
	for _, l := range silent {
		l.car.(*dgFakeCarrier).rx.Store(time.Now().Add(-5 * time.Second).UnixNano())
	}
	le.rx.Store(time.Now().UnixNano())
	r.carrier() // the fresh arrival
	for i, l := range silent {
		if l.alive() {
			t.Fatalf("silent carrier %d still alive", i)
		}
	}
	if !live.alive() {
		t.Fatal("a live carrier was dropped")
	}
	if !strings.Contains(r.log(), "dropped 3 carrier(s) silent for 3s+") {
		t.Fatalf("no line:\n%s", r.log())
	}
}

// When every carrier is silent the dialing side dials one scout; when it
// comes up the dead carriers go and the pool can refill at once.
func TestDgScoutWhenEveryCarrierIsSilent(t *testing.T) {
	r := newDgRig(t, false)
	dialer, acc := newDgFakeLink()
	go func() {
		for {
			c, err := acc.Accept(r.ctx)
			if err != nil {
				return
			}
			r.exit.add(r.ctx, c, "accepted")
		}
	}()
	r.edge.dialer = dialer
	r.edge.gate = instantGate()
	var dead []*dgLink
	for i := 0; i < 3; i++ {
		el, _, _, _ := r.carrier()
		dead = append(dead, el)
	}
	for _, l := range dead { // the exit restarted 4 s ago
		l.car.(*dgFakeCarrier).rx.Store(time.Now().Add(-4 * time.Second).UnixNano())
	}
	r.edge.scoutIfSilent(r.ctx)
	within(t, 2*time.Second, "the scout replaces the silent carriers", func() bool {
		return !dead[0].alive() && !dead[1].alive() && !dead[2].alive() && r.edge.count() >= 1 && !allDead(r.edge)
	})
	lg := r.log()
	if !strings.Contains(lg, "dialing a scout carrier") || !strings.Contains(lg, "dropped 3 carrier(s)") {
		t.Fatalf("lines missing:\n%s", lg)
	}
	// Not while some carrier still hears the peer.
	r2 := newDgRig(t, false)
	r2.edge.dialer = dialer
	_, _, e1, _ := r2.carrier()
	_, _, e2, _ := r2.carrier()
	e1.rx.Store(time.Now().Add(-4 * time.Second).UnixNano())
	e2.rx.Store(time.Now().UnixNano())
	r2.edge.scoutIfSilent(r2.ctx)
	if r2.edge.dialing.Load() != 0 {
		t.Fatal("scouted while a carrier was alive")
	}
}

// A shrink finishes even under a download that never pauses: new flows avoid
// the retiring carriers on both sides at once, and after dgRetireForce the
// flows stuck to them move too, so they empty and close.
func TestDgShrinkFinishesUnderNonstopDownload(t *testing.T) {
	old := dgRetireForce
	dgRetireForce = 600 * time.Millisecond
	defer func() { dgRetireForce = old }()
	r := newDgRig(t, false)
	for i := 0; i < 6; i++ {
		r.carrier()
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // the exit sends 60 download flows nonstop
		defer wg.Done()
		tk := time.NewTicker(5 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
			}
			now := r.exit.now()
			for f := uint32(0); f < 60; f++ {
				if l := r.exit.pick(f*2654435761, now); l != nil {
					b := ipPacket(80, uint16(2000+f), []byte("dl"))
					l.car.SendFrame(core.TypeData, b)
					l.noteFlowSend(f*2654435761, len(b), now)
				}
			}
		}
	}()
	defer func() { close(stop); wg.Wait() }()
	time.Sleep(200 * time.Millisecond)
	r.edge.reconcile(r.ctx, 2)
	within(t, 5*time.Second, "the retiring carriers drain and close under download", func() bool {
		r.edge.drainTick()
		return r.edge.count() == 2
	})
}

// The serve notice goes out once, as one control datagram: when it was lost
// the exit used to treat the carrier as retiring for good (no new flows, its
// sticky ones forced off after dgRetireForce) while both sides counted it
// serving. A retire notice the other side stops renewing now expires after
// dgPeerRetireStale; a renewed one (the retiring side repeats it) holds.
func TestDgPeerRetireNoticeExpiresUnlessRenewed(t *testing.T) {
	r := newDgRig(t, false)
	for i := 0; i < 4; i++ {
		r.carrier()
	}
	r.edge.reconcile(r.ctx, 3)
	var xl *dgLink
	r.edge.mu.RLock()
	for _, l := range r.edge.set {
		if l.retiring {
			xl = r.peer[l]
		}
	}
	r.edge.mu.RUnlock()
	within(t, time.Second, "the exit learns the carrier retires", func() bool { return xl.peerRetiring() })
	base := time.Now()
	r.exit.clock = func() time.Time { return base.Add(dgPeerRetireStale / 2) }
	r.exit.sampleHealth()
	if !xl.peerRetiring() {
		t.Fatal("a fresh retire notice expired")
	}
	// The edge serves it again but its serve notice is lost: nothing renews
	// the retire notice any more.
	r.exit.clock = func() time.Time { return base.Add(dgPeerRetireStale + time.Second) }
	r.exit.sampleHealth()
	if xl.peerRetiring() || xl.retiringAt.Load() != 0 {
		t.Fatal("a retire notice nobody renewed did not expire")
	}
	on := 0
	for f := uint32(0); f < 4000; f++ {
		if r.exit.pickHash(f) == xl {
			on++
		}
	}
	if on < 600 {
		t.Fatalf("%d of 4000 new flows on the carrier after the notice expired, want about a quarter", on)
	}
	if !strings.Contains(r.log(), "serves again here") {
		t.Fatalf("the expiry is not logged:\n%s", r.log())
	}
	// A retiring side keeps renewing: the reminder sets it again.
	r.exit.onCloseFrame(xl, []byte{closeRetire})
	if !xl.peerRetiring() {
		t.Fatal("a renewed notice did not take")
	}
}

// A restarted exit (no bye: crash, kill, an upgrade from an older build)
// leaves the reverse edge with silent carriers. They are dropped when its
// first new carrier arrives — and are not counted when deciding whether that
// carrier is spare: counted as serving, the only live carrier was told to
// retire and the log said "(now 3) — spare".
func TestDgReverseFreshCarrierNotSpareBehindZombies(t *testing.T) {
	r := newDgRig(t, true)
	r.edge.target.Store(2)
	var old []*dgLink // the old exit's carriers; then it dies (no bye)
	for i := 0; i < 2; i++ {
		el, _, _, _ := r.carrier()
		old = append(old, el)
	}
	for _, l := range old {
		l.car.(*dgFakeCarrier).rx.Store(time.Now().Add(-5 * time.Second).UnixNano())
	}
	el, xl, _, _ := r.carrier()
	if el.retiring || el.bornSpare {
		t.Fatalf("the only live carrier arrived spare:\n%s", r.log())
	}
	if xl.peerRetiring() {
		t.Fatal("the exit was told to retire its only live carrier")
	}
	lg := r.log()
	if !strings.Contains(lg, "dropped 2 carrier(s) silent") || !strings.Contains(lg, "up (now 1)\n") && !strings.HasSuffix(lg, "up (now 1)") {
		t.Fatalf("log:\n%s", lg)
	}
}

// add() decides "spare" under the lock and keeps that answer: it used to read
// bornSpare again after unlocking while reconcileReverseEdge (pool goroutine)
// could serve the carrier and clear it — a data race, and a late closeRetire
// could follow the closeServe. (Run under -race.)
func TestDgAddSpareRacesReconcile(t *testing.T) {
	r := newDgRig(t, true)
	r.edge.target.Store(1)
	r.carrier()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for n := int32(2); ; n++ {
			select {
			case <-stop:
				return
			default:
			}
			r.edge.target.Store(n)
			r.edge.reconcileReverseEdge()
		}
	}()
	for i := 0; i < 30; i++ {
		r.carrier()
	}
	close(stop)
	<-done
}

// The direct exit forgets ended flows on its health tick: every download
// 5-tuple used to stay in the sticky map for the life of the process
// (review: 1,000,000 entries, ~70 MB, after the flows ended).
func TestDgDirectExitPrunesEndedFlows(t *testing.T) {
	r := newDgRig(t, false)
	for i := 0; i < 4; i++ {
		r.carrier()
	}
	now := time.Now()
	for f := uint32(0); f < 5000; f++ {
		r.exit.pick(f, now)
	}
	r.exit.clock = func() time.Time { return now.Add(flowletGap + time.Second) }
	r.exit.directExitTick()
	r.exit.stickyMu.Lock()
	n := len(r.exit.sticky)
	r.exit.stickyMu.Unlock()
	if n != 0 {
		t.Fatalf("%d ended flows still remembered after a tick", n)
	}
}
