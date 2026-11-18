package engine

import (
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// er94 lab B94_1: almost every rescue (126 of 125 started) ended "join
// missed" on the exit: the conns were opened into the path as it blackholed,
// their role-3 first sub (open + first bytes) never reached the exit, so the
// RESCUE join found no conn there and the user connection failed. Iran has
// read nothing (Ri 0) and its ring holds every byte from 0, so the join goes
// as role 4, the exit opens the connection on it in records mode and answers
// ACK(0,0), Iran resends [0, Bi).
func TestRescueFreshFirstSubLost(t *testing.T) {
	setRescue(t, true)
	setLite(t, true)
	resetLiteBudget()
	exit := newMConnExit(9) // what serveMConnSub opens for a role-4 join of an unknown id
	exit.mu.Lock()
	exit.rscFresh = true
	exit.mu.Unlock()
	var mu sync.Mutex
	opens, fresh := 0, 0
	open := func(p mPlane, avoid []any) (mSub, any, error) {
		a, b := net.Pipe()
		ia, eb := newTSub(a), newTSub(b)
		mu.Lock()
		key := opens
		opens++
		mu.Unlock()
		if key == 0 { // the first sub: lost in the blackhole, the exit never sees it
			ia.raw = true
			go io.Copy(io.Discard, eb)
			return ia, key, nil
		}
		if _, ok := takeRescueFreshMark(avoid); ok {
			mu.Lock()
			fresh++
			mu.Unlock()
		}
		exit.attach(eb, mconnRoleJoin)
		return ia, key, nil
	}
	iran, err := newMConnIran(9, open)
	if err != nil {
		t.Fatal(err)
	}
	if iran.raw1 == nil || !iran.rawW.Load() {
		t.Fatal("the first sub is not raw")
	}
	iran.rescueEnable()
	req := randBytes(6000, 11)
	if _, err := iran.Write(req); err != nil {
		t.Fatal(err)
	}
	served0 := rescueStat.served.Load()
	rescueNow(t, iran)
	mu.Lock()
	f := fresh
	mu.Unlock()
	if f != 1 {
		t.Fatalf("rescue join sent with role 4: %d, want 1", f)
	}
	resp, more, more2 := randBytes(9000, 12), randBytes(40000, 13), randBytes(30000, 14)
	both(t,
		func() error { return readN(exit, req) },
		func() error { return exitWrite(exit, resp) },
		func() error { return readN(iran, resp) },
	)
	both(t,
		func() error { return xfer(iran, more, 4000, 0) },
		func() error { return readN(exit, more) },
		func() error { return xfer(exit, more2, 4000, 0) },
		func() error { return readN(iran, more2) },
	)
	if rescueStat.served.Load() == served0 {
		t.Fatal("the exit did not count the fresh rescue as served")
	}
	mWaitFor(t, "Iran ring freed", time.Second, func() bool {
		iran.wmu.Lock()
		defer iran.wmu.Unlock()
		return iran.ring.buf == nil
	})
	closeAndWait(t, iran, exit)
}

func exitWrite(m *mConn, p []byte) error {
	_, err := m.Write(p)
	return err
}

// tcpPair: a loopback TCP pair (kernel buffers: bytes written are "sent"
// before the peer reads them, unlike net.Pipe).
func tcpPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ch := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		ch <- c
	}()
	a, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	b := <-ch
	if b == nil {
		t.Fatal("accept failed")
	}
	return a, b
}

// er94: the exit's ring no longer holds [Ri, Bk) (a false trigger on a live
// path with more in flight than the ring): before, the conn ended (nocover);
// now the exit answers NAK, Iran reads [Ri, Bk) on the first sub itself and
// the conn goes on, every byte once, in order.
func TestRescueNakKeepsConn(t *testing.T) {
	setRescue(t, true)
	setLite(t, true)
	resetLiteBudget()
	oldCap := rescueRingCapV.Load()
	rescueRingCapV.Store(4 << 10)
	t.Cleanup(func() { rescueRingCapV.Store(oldCap) })
	exit := newMConnExitRaw(10)
	var mu sync.Mutex
	opens := 0
	open := func(p mPlane, avoid []any) (mSub, any, error) {
		mu.Lock()
		key := opens
		opens++
		mu.Unlock()
		if key == 0 {
			a, b := tcpPair(t)
			ia, eb := newTSub(a), newTSub(b)
			ia.raw = true
			exit.attach(eb, mconnRoleFirstRaw)
			return ia, key, nil
		}
		a, b := net.Pipe()
		ia, eb := newTSub(a), newTSub(b)
		exit.attach(eb, mconnRoleJoin)
		return ia, key, nil
	}
	iran, err := newMConnIran(10, open)
	if err != nil {
		t.Fatal(err)
	}
	iran.rescueEnable()
	exit.rescueEnable()
	req := randBytes(3000, 21)
	both(t,
		func() error { return xfer(iran, req, 1000, 0) },
		func() error { return readN(exit, req) },
	)
	resp := randBytes(60000, 22) // in the kernel buffers, unread by Iran: more than the exit's ring
	if err := exitWrite(exit, resp); err != nil {
		t.Fatal(err)
	}
	nak0, nakd0 := rescueStat.nak.Load(), rescueStat.nakd.Load()
	rescueNow(t, iran)
	more, more2 := randBytes(20000, 23), randBytes(20000, 24)
	both(t,
		func() error {
			if err := readN(iran, resp); err != nil {
				return err
			}
			return readN(iran, more2)
		},
		func() error { return readN(exit, more) },
		func() error { return xfer(iran, more, 4000, 0) },
		func() error { return xfer(exit, more2, 4000, 0) },
	)
	if rescueStat.nak.Load() == nak0 || rescueStat.nakd.Load() == nakd0 {
		t.Fatal("no NAK counted")
	}
	closeAndWait(t, iran, exit)
}

// er94 lab B95: the sweeper freed the ring of a conn waiting for its ACK
// (rawW is off after the switch): 24-29 rescues per run ended "nocover".
func TestRescueSweepKeepsRingUntilAck(t *testing.T) {
	_, iran, exit := newRescuePair(t)
	a := randBytes(2000, 31)
	both(t,
		func() error { return xfer(iran, a, 1000, 0) },
		func() error { return readN(exit, a) },
	)
	iran.rscCut.Store(true) // as rescueStart leaves it until the ACK
	iran.rawW.Store(false)
	rescueSweep(time.Now())
	iran.wmu.Lock()
	kept := iran.ring.buf != nil
	iran.wmu.Unlock()
	iran.rscCut.Store(false)
	iran.rawW.Store(true)
	if !kept {
		t.Fatal("the sweeper freed a ring a rescue still needs")
	}
	closeAndWait(t, iran, exit)
}
