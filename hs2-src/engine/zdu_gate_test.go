package engine

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func zduHoldCleanup(t *testing.T) {
	t.Cleanup(func() {
		zduHold.Store(false)
		zduHoldCh.Store(nil)
	})
}

func acceptCh(ln net.Listener) chan net.Conn {
	ch := make(chan net.Conn, 4)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			ch <- c
		}
	}()
	return ch
}

func gotConn(ch chan net.Conn, d time.Duration) bool {
	select {
	case c := <-ch:
		c.Close()
		return true
	case <-time.After(d):
		return false
	}
}

// Review issue 4: a new process (ZDUHoldUsers) accepts links before GO but
// no user connection, reads no UDP datagram and serves no exit user stream
// until ZDUOpenUsers; then all of them flow. The old code accepted and read
// at once (S9: ~700 crowd fails + ~1070 UDP lost per reverted handover).
func TestZDUHoldUsersUntilGO(t *testing.T) {
	zduKeyOn(t)
	zduHoldCleanup(t)
	ZDUHoldUsers()
	user, err := zduListen("127.0.0.1:0", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer user.Close()
	link, err := ListenLink("127.0.0.2:0") // another address: the registry is by address
	if err != nil {
		t.Fatal(err)
	}
	defer link.Close()
	pc, err := listenPacketUDP("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	ua, la := acceptCh(user), acceptCh(link)
	for _, a := range []string{user.Addr().String(), link.Addr().String()} {
		c, err := net.Dial("tcp", a)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
	}
	if !gotConn(la, 2*time.Second) {
		t.Fatal("link listener did not accept before GO")
	}
	if gotConn(ua, 300*time.Millisecond) {
		t.Fatal("user listener accepted before GO")
	}
	got := make(chan int, 1)
	go func() {
		b := make([]byte, 64)
		n, _, _ := pc.ReadFrom(b)
		got <- n
	}()
	uc, err := net.Dial("udp", pc.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer uc.Close()
	uc.Write([]byte("hello"))
	select {
	case <-got:
		t.Fatal("UDP read before GO")
	case <-time.After(300 * time.Millisecond):
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	if zduWaitGO(ctx) {
		t.Fatal("exit user stream served before GO")
	}
	cancel()
	waited := make(chan bool, 1)
	go func() { waited <- zduWaitGO(context.Background()) }()
	if n := ZDUOpenUsers(); n != 2 {
		t.Fatalf("opened %d, want 2 (user listener + UDP)", n)
	}
	if !gotConn(ua, 2*time.Second) {
		t.Fatal("user listener did not accept after GO")
	}
	select {
	case n := <-got:
		if n != 5 {
			t.Fatalf("read %d", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("UDP datagram not read after GO (lost?)")
	}
	if !<-waited {
		t.Fatal("exit stream not released at GO")
	}
	if ZDUOpenUsers() != 0 || ZDUUsersHeld() {
		t.Fatal("second open")
	}
}

// Key off: ListenLink is ListenReuse (a plain *net.TCPListener), no hold.
func TestZDUGateKeyOff(t *testing.T) {
	setZDU(t, false)
	zduHoldCleanup(t)
	ZDUHoldUsers()
	if ZDUUsersHeld() {
		t.Fatal("held with the key off")
	}
	ln, err := ListenLink("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if _, ok := ln.(*net.TCPListener); !ok {
		t.Fatalf("%T", ln)
	}
	if !zduWaitGO(context.Background()) {
		t.Fatal("wait")
	}
	m := mcManager(newMCLink(capMConn))
	m.target.Store(7)
	if m.ctlTarget() != 7 {
		t.Fatal(m.ctlTarget())
	}
}

// Review issue 4: the old process stops accepting on its link listeners only
// (users go on); resume gives them back.
func TestZDUPauseLinkAcceptOnly(t *testing.T) {
	zduKeyOn(t)
	user, err := zduListen("127.0.0.1:0", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer user.Close()
	link, err := ListenLink("127.0.0.2:0") // another address: the registry is by address
	if err != nil {
		t.Fatal(err)
	}
	defer link.Close()
	ua, la := acceptCh(user), acceptCh(link)
	time.Sleep(50 * time.Millisecond)
	if n := ZDUPauseLinkAccept(); n < 1 {
		t.Fatalf("paused %d", n)
	}
	t.Cleanup(func() { ZDUResumeLinkAccept() })
	for _, a := range []string{user.Addr().String(), link.Addr().String()} {
		c, err := net.Dial("tcp", a)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
	}
	if !gotConn(ua, 2*time.Second) {
		t.Fatal("user accept stopped")
	}
	if gotConn(la, 300*time.Millisecond) {
		t.Fatal("link accepted after the link pause")
	}
	ZDUResumeLinkAccept()
	if !gotConn(la, 2*time.Second) {
		t.Fatal("link not accepted after resume")
	}
}

// Review issue 4: the boot boost adds the pool's own target to ctlTarget
// (the exit dials that many more links into the new process) and clears.
func TestZDUBoostCtlTarget(t *testing.T) {
	setZDU(t, true)
	m := mcManager(newMCLink(capMConn))
	m.target.Store(12)
	zduLMs.mu.Lock()
	saved := zduLMs.l
	zduLMs.l = []*LinkManager{m}
	zduLMs.mu.Unlock()
	t.Cleanup(func() { zduLMs.mu.Lock(); zduLMs.l = saved; zduLMs.mu.Unlock() })
	ep, eo := zduExitPeers.Swap(nil), zduExitPoolRef.Swap(nil) // an earlier test's exit
	t.Cleanup(func() { zduExitPeers.Store(ep); zduExitPoolRef.Store(eo) })
	if m.ctlTarget() != 12 {
		t.Fatal(m.ctlTarget())
	}
	if b, _ := ZDUBoost(true); b != 12 || m.ctlTarget() != 24 {
		t.Fatalf("boost %d ctl %d", b, m.ctlTarget())
	}
	if need := ZDUWarmNeed(); need != 6 {
		t.Fatalf("warm need %d, want 6 (ceil(12/2))", need)
	}
	ZDUBoost(false)
	if m.ctlTarget() != 12 {
		t.Fatal(m.ctlTarget())
	}
}

// Review H1 (fails before the fix: a forward exit needed 1 link, a 2-3
// target pool 1): never one link for a multi-link pool, every pool kind.
func TestZDUWarmNeedNeverOne(t *testing.T) {
	for _, c := range [][2]int{{0, 1}, {1, 1}, {2, 2}, {3, 2}, {4, 2}, {5, 3}, {12, 6}, {200, 100}} {
		if got := zduNeedOf(c[0]); got != c[1] {
			t.Fatalf("need(%d) = %d, want %d", c[0], got, c[1])
		}
	}
	zduLMs.mu.Lock()
	saved := zduLMs.l
	zduLMs.l = nil
	zduLMs.mu.Unlock()
	t.Cleanup(func() { zduLMs.mu.Lock(); zduLMs.l = saved; zduLMs.mu.Unlock() })
	ep, eo := zduExitPeers.Swap(&linkPeers{}), zduExitPoolRef.Swap(nil) // a direct exit: peers, no pool
	t.Cleanup(func() { zduExitPeers.Store(ep); zduExitPoolRef.Store(eo) })
	if n := ZDUWarmNeed(); n != zduNeedFloor {
		t.Fatalf("direct exit warm need %d, want %d", n, zduNeedFloor)
	}
}

// Review issue 4: with the exit pool at its ceiling the boost cannot grow it,
// so idle links (no user) are closed to make their slots redial.
func TestZDUBootCloseAtCeiling(t *testing.T) {
	setZDU(t, true)
	var ls []*zduCloseLink
	m := mcManager(newMCLink(capMConn))
	m.links = nil
	for i := 0; i < 4; i++ {
		l := &zduCloseLink{mcLink: newMCLink(capMConn)}
		ls = append(ls, l)
		m.links = append(m.links, &managedLink{link: l, id: i, mtr: l.mtr})
	}
	pi := *m.exitInfo.Load()
	pi.MaxLinks = 8
	m.exitInfo.Store(&pi)
	if n := m.zduBootClose(); n != 0 {
		t.Fatalf("closed %d below the ceiling", n)
	}
	pi.MaxLinks = 4
	m.exitInfo.Store(&pi)
	m.links[0].users.Store(3) // busy: kept
	if n := m.zduBootClose(); n != zduBootClose {
		t.Fatalf("closed %d", n)
	}
	if !ls[0].Alive() || ls[1].Alive() || ls[2].Alive() || !ls[3].Alive() {
		t.Fatalf("closed: %v %v %v %v (want the 2 first idle ones)", !ls[0].Alive(), !ls[1].Alive(), !ls[2].Alive(), !ls[3].Alive())
	}
}

// Review M3 (fails before the fix): users 0 is not idle when the link carries
// TUN flows (l3mtcp: L3 lanes are not users). A link whose L3 stamp is recent
// is never closed; an unmetered link (activity unknown) neither.
func TestZDUBootCloseKeepsL3(t *testing.T) {
	setZDU(t, true)
	var ls []*zduCloseLink
	m := mcManager(newMCLink(capMConn))
	m.links = nil
	for i := 0; i < 4; i++ {
		l := &zduCloseLink{mcLink: newMCLink(capMConn)}
		ls = append(ls, l)
		m.links = append(m.links, &managedLink{link: l, id: i, mtr: l.mtr})
	}
	pi := *m.exitInfo.Load()
	pi.MaxLinks = 4
	m.exitInfo.Store(&pi)
	pl := &l3Link{zduL3: &ls[0].mtr.zduL3At} // a TUN packet over link 0
	zduMarkL3(pl.zduL3)
	zduMarkL3(&ls[1].mtr.zduL3At)
	if n := m.zduBootClose(); n != zduBootClose {
		t.Fatalf("closed %d", n)
	}
	if !ls[0].Alive() || !ls[1].Alive() || ls[2].Alive() || ls[3].Alive() {
		t.Fatalf("closed: %v %v %v %v (want only the 2 links without L3)", !ls[0].Alive(), !ls[1].Alive(), !ls[2].Alive(), !ls[3].Alive())
	}
	ls[2].closed.Store(false)
	m.links[2].mtr = nil // unknown activity: busy
	ls[3].closed.Store(false)
	ls[3].mtr.zduL3At.Store(ctrlNow() - int64(zduBootL3Recent) - 1) // stale stamp: idle
	if n := m.zduBootClose(); n != 1 || !ls[2].Alive() || ls[3].Alive() {
		t.Fatalf("closed %d (unmetered kept %v, stale closed %v)", n, ls[2].Alive(), !ls[3].Alive())
	}
	setZDU(t, false)
	var at atomic.Int64
	zduMarkL3(&at)
	if at.Load() != 0 {
		t.Fatal("key off: L3 stamp written")
	}
}

type zduCloseLink struct {
	*mcLink
	closed atomic.Bool
}

func (c *zduCloseLink) Close() error { c.closed.Store(true); return nil }
func (c *zduCloseLink) Alive() bool  { return !c.closed.Load() }

// Review issue 4 (S9 rerun: the new Iran got 0 links in 20 s): with the exit
// pool at its ceiling the old Iran's boost cannot grow it, so a ZDU exit
// starts zduExitBoot spare dials per boost episode; they land on the new Iran
// and turn the old slots legacy, and the new Iran's own target then grows its
// pool in full. The old code clamped and dialed nothing.
func TestZDUExitBootAtCeiling(t *testing.T) {
	setZDU(t, true)
	zduResetExitStat(t)
	b0 := zduExitBootN.Load()
	zp := newZDUExitPool(t, 1, 5)
	zp.setIran(zduInstOld)
	zp.setTarget(5)
	eventually(t, "5 old links", func() bool { return zp.liveOf(zduInstOld, false) == 5 })
	zp.setIran(zduInstNew) // only the new Iran accepts now
	zp.setTarget(10)       // the old Iran's boost: above the ceiling 5
	eventually(t, "boot links on the new Iran", func() bool { return zp.liveOf(zduInstNew, false) == zduExitBoot })
	if n := zduExitBootN.Load() - b0; n != zduExitBoot {
		t.Fatalf("boot spares %d", n)
	}
	zp.setTarget(10) // the same episode: no more spares
	if n := zduExitBootN.Load() - b0; n != zduExitBoot {
		t.Fatalf("boot spares %d after a repeated boost", n)
	}
	// the new Iran's own target fills its pool next to the legacy old links
	sendCtl(t, zp.exitPool, zp.meterOf(zduInstNew), 5)
	eventually(t, "5 new links", func() bool { return zp.liveOf(zduInstNew, false) == 5 })
	if zp.liveOf(zduInstOld, false) != 5 {
		t.Fatal("old links touched")
	}
}

// Key off: a target above the ceiling is clamped, nothing more.
func TestZDUExitBootKeyOff(t *testing.T) {
	setZDU(t, false)
	b0 := zduExitBootN.Load()
	zp := newZDUExitPool(t, 1, 5)
	zp.setTarget(10)
	eventually(t, "5 links", func() bool { return zp.liveCount() == 5 })
	time.Sleep(100 * time.Millisecond)
	if zp.slots() != 5 || zduExitBootN.Load() != b0 {
		t.Fatalf("slots %d boot %d", zp.slots(), zduExitBootN.Load()-b0)
	}
}

// Review M2 (fails before the fix): a link listener made while link accept is
// paused (an LB reload in the warm wait) starts paused; at the commit the old
// process drops its copies, so a listener only it held refuses dials.
func TestZDULinkReloadPausedAndDropped(t *testing.T) {
	setZDU(t, true)
	t.Cleanup(func() { ZDUResumeLinkAccept() })
	ZDUPauseLinkAccept()
	ln, err := ListenLink("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ch := acceptCh(ln)
	c, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if gotConn(ch, 200*time.Millisecond) {
		t.Fatal("a link listener made during the pause accepted")
	}
	if n := ZDUDropLinkListeners(); n < 1 {
		t.Fatalf("dropped %d", n)
	}
	if c, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second); err == nil {
		c.Close()
		t.Fatal("dial to a dropped link listener connected")
	}
	if gotConn(ch, 100*time.Millisecond) {
		t.Fatal("dropped listener's Accept returned a conn")
	}
}

// Review 4 M3: the exit pool at MaxLinks with EVERY old link carrying TUN
// traffic (the Iran's zduBootClose closes none; TestZDUBootCloseKeepsL3). The
// warm need is still reached without cutting anything: the boot spares land
// on the new Iran, the old slots turn legacy (not counted toward want), and
// the new Iran's own target grows its pool next to them. That is a bounded
// temporary over-cap: at most hi legacy + hi current + zduExitBoot spares,
// and it reverts by itself as the legacy links end (they retire, no redial).
func TestZDUExitAtCeilingAllBusyWarmNeed(t *testing.T) {
	setZDU(t, true)
	zduResetExitStat(t)
	const hi = 6
	zp := newZDUExitPool(t, 1, hi)
	zp.setIran(zduInstOld)
	zp.setTarget(hi)
	eventually(t, "old pool at its ceiling", func() bool { return zp.liveOf(zduInstOld, false) == hi })
	zp.setIran(zduInstNew)
	zp.setTarget(2 * hi) // the old Iran's boot boost; no old link is closed
	eventually(t, "boot spares on the new Iran", func() bool { return zp.liveOf(zduInstNew, false) == zduExitBoot })
	sendCtl(t, zp.exitPool, zp.meterOf(zduInstNew), hi)
	need := zduNeedOf(hi)
	eventually(t, "warm need on the new Iran", func() bool { return zp.liveOf(zduInstNew, false) >= need })
	eventually(t, "new pool full", func() bool { return zp.liveOf(zduInstNew, false) == hi })
	if n := zp.liveOf(zduInstOld, false); n != hi {
		t.Fatalf("old links cut: %d of %d left", n, hi)
	}
	if s := zp.slots(); s > 2*hi+zduExitBoot {
		t.Fatalf("over-cap unbounded: %d slots > 2*%d+%d", s, hi, zduExitBoot)
	}
	// auto-revert: the old Iran's drain ends its links; their slots retire
	zp.liveOf(zduInstOld, true)
	eventually(t, "back to the ceiling", func() bool { return zp.slots() <= hi+zduExitBoot && zp.liveOf(zduInstOld, false) == 0 })
	if zp.liveOf(zduInstNew, false) < hi {
		t.Fatalf("new pool shrank to %d", zp.liveOf(zduInstNew, false))
	}
}

// b441 (l437 finding 6, fails before the fix): an lb cut-drain overshoot
// above the ceiling spent the boot spares on the old Iran, and the handover
// boost that came while the overshoot lasted was the same episode: no spare,
// the new Iran got 0 links in 20 s. A rise of at least zduBootJump re-arms;
// a small rise does not.
func TestZDUExitBootAfterOvershoot(t *testing.T) {
	setZDU(t, true)
	zduResetExitStat(t)
	const hi = 6
	b0 := zduExitBootN.Load()
	zp := newZDUExitPool(t, 1, hi)
	zp.setIran(zduInstOld)
	zp.setTarget(hi)
	eventually(t, "old pool at its ceiling", func() bool { return zp.liveOf(zduInstOld, false) == hi })
	zp.setTarget(hi + 1) // cut-drain replacements: above the ceiling, no handover
	eventually(t, "overshoot spares on the old Iran", func() bool { return zp.liveOf(zduInstOld, false) == hi+zduExitBoot })
	zp.setTarget(hi + 2) // a small rise: the same episode
	time.Sleep(100 * time.Millisecond)
	if n := zduExitBootN.Load() - b0; n != zduExitBoot {
		t.Fatalf("small rise re-armed: boot spares %d", n)
	}
	zp.setIran(zduInstNew)    // the handover: only the new Iran accepts now
	zp.setTarget(hi + 2 + hi) // the old Iran's boost on top of the overshoot
	eventually(t, "boot spares on the new Iran", func() bool { return zp.liveOf(zduInstNew, false) == zduExitBoot })
	if n := zduExitBootN.Load() - b0; n != 2*zduExitBoot {
		t.Fatalf("boot spares %d, want %d", n, 2*zduExitBoot)
	}
	sendCtl(t, zp.exitPool, zp.meterOf(zduInstNew), hi)
	eventually(t, "new pool full", func() bool { return zp.liveOf(zduInstNew, false) == hi })
}
