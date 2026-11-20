package engine

import (
	"net"
	"testing"
	"time"
)

// zduKeyOn turns the key on for one test.
func zduKeyOn(t *testing.T) {
	t.Helper()
	was := zduOn.Load()
	zduOn.Store(true)
	t.Cleanup(func() { zduOn.Store(was) })
}

// S7: a UDP user socket is handed over (one kernel socket, same port), the
// old process stops reading at the pause and the new one gets every
// datagram, the old can still reply from the same port, and a resume (the
// revert) gives the old process its reads back.
func TestZDUUDPHandover(t *testing.T) {
	zduKeyOn(t)
	const addr = "127.0.0.1:0"
	old, err := listenPacketUDP(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	if _, ok := old.(*zduPC); !ok {
		t.Fatalf("key on: %T, want *zduPC", old)
	}
	port := old.LocalAddr().String()
	names, files, err := ZDUListenerFiles()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for i, n := range names {
		if n == ZDUPCName(addr) {
			ZDUAdoptPacketConn(n, files[i]) // what the new process does with FDS
			found = true
		} else {
			files[i].Close()
		}
	}
	if !found {
		t.Fatalf("udp socket not in the hand-off: %v", names)
	}
	a0 := ZDUAdoptedUDP.Load()
	nw, err := listenPacketUDP(addr) // the new process binds "the same" address
	if err != nil {
		t.Fatal(err)
	}
	defer nw.Close()
	if ZDUAdoptedUDP.Load() != a0+1 || nw.LocalAddr().String() != port {
		t.Fatalf("not adopted: %s vs %s", nw.LocalAddr(), port)
	}
	// One test process plays both: the registry is keyed by name, so put the
	// old process's socket back as the registered one (what pause acts on).
	zduPCs.mu.Lock()
	zduPCs.m[ZDUPCName(addr)] = old.(*zduPC)
	zduPCs.mu.Unlock()
	got := make(chan string, 64)
	read := func(pc net.PacketConn, tag string) {
		b := make([]byte, 64)
		for {
			n, _, err := pc.ReadFrom(b)
			if err != nil {
				return
			}
			got <- tag + ":" + string(b[:n])
		}
	}
	ZDUPauseAccept() // GO: the old process stops reading
	defer ZDUResumeAccept()
	go read(old, "old")
	go read(nw, "new")
	cl, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	dst, _ := net.ResolveUDPAddr("udp", port)
	for i := 0; i < 20; i++ {
		cl.WriteTo([]byte("x"), dst)
	}
	for i := 0; i < 20; i++ {
		select {
		case s := <-got:
			if s != "new:x" {
				t.Fatalf("datagram %d went to %q while paused", i, s)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d of 20 datagrams", i)
		}
	}
	// The old process still replies to its own flows from the same port.
	if _, err := old.WriteTo([]byte("r"), cl.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 8)
	cl.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, from, err := cl.ReadFrom(b)
	if err != nil || string(b[:n]) != "r" || from.String() != port {
		t.Fatalf("reply %q from %v: %v", b[:n], from, err)
	}
	// Revert: the new process dies, the old one reads again.
	nw.Close()
	ZDUResumeAccept()
	time.Sleep(50 * time.Millisecond)
	cl.WriteTo([]byte("y"), dst)
	select {
	case s := <-got:
		if s != "old:y" {
			t.Fatalf("after resume %q", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("old process does not read after the resume")
	}
}

// Key off: a plain *net.UDPConn, nothing registered.
func TestZDUUDPKeyOff(t *testing.T) {
	was := zduOn.Load()
	zduOn.Store(false)
	defer zduOn.Store(was)
	pc, err := listenPacketUDP("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	if _, ok := pc.(*net.UDPConn); !ok {
		t.Fatalf("key off: %T", pc)
	}
	if len(zduAllPC()) != 0 {
		t.Fatal("registered with the key off")
	}
}

// S7: the new instance is always after the parent's, also when the wall
// clock stepped back between the two starts.
func TestZDUFollowParent(t *testing.T) {
	cases := []struct{ self, parent, want uint64 }{
		{100, 50, 100}, // normal: own start is later
		{100, 100, 101},
		{100, 500, 501}, // NTP step back
		{100, 0, 100},   // no parent instance (pre-ZDU HELLO)
	}
	for _, c := range cases {
		got := zduAfter(zduInst{Start: c.self, Rand: 7}, c.parent)
		if got.Start != c.want || got.Rand != 7 {
			t.Errorf("self %d parent %d: %+v want %d", c.self, c.parent, got, c.want)
		}
		if c.parent != 0 && !got.newer(zduInst{Start: c.parent, Rand: ^uint32(0)}) {
			t.Errorf("self %d parent %d: not newer", c.self, c.parent)
		}
	}
	saved := zduSelf
	defer func() { zduSelf = saved }()
	zduSelf = zduInst{Start: 10, Rand: 3}
	if s := ZDUFollowParent(99); s != 100 {
		t.Fatal(s)
	}
	if s, _ := ZDUInstance(); s != 100 {
		t.Fatal(s)
	}
}

// S7: the Kharej old process's DRAINSTAT counts its exit links and the user
// connections on them (it has no LinkManager), so it drains instead of
// leaving at probation's end with users "0".
func TestZDUDrainStatsExit(t *testing.T) {
	old := zduExitPeers.Load()
	t.Cleanup(func() { zduExitPeers.Store(old) })
	zduExitPeers.Store(nil)
	l0, u0 := ZDUDrainStats()
	p := &linkPeers{}
	a, b := &linkMeter{}, &linkMeter{}
	a.users.Store(3)
	b.users.Store(1)
	p.add(a)
	p.add(b)
	zduExitPeers.Store(p)
	l, u := ZDUDrainStats()
	if l-l0 != 2 || u-u0 != 4 {
		t.Fatalf("exit drain stats +%d links +%d users, want +2 +4", l-l0, u-u0)
	}
	a.users.Store(0)
	b.users.Store(0)
	if _, u := ZDUDrainStats(); u != u0 {
		t.Fatalf("users %d after the exit conns closed", u)
	}
}

// S7: the exit's link set counts as a pool for the new process's warm wait.
func TestZDUPoolsExit(t *testing.T) {
	old := zduExitPeers.Load()
	t.Cleanup(func() { zduExitPeers.Store(old) })
	zduExitPeers.Store(nil)
	n0 := ZDUPools()
	zduExitPeers.Store(&linkPeers{})
	if ZDUPools() != n0+1 {
		t.Fatal("exit link set not counted as a pool")
	}
}
