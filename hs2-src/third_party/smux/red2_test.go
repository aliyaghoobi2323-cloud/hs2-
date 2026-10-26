package smux

import (
	"testing"
	"time"
)

// Red team round 2 (Z5 window core, part A): the fix round's rules -- claims
// that stay with their data after a session closes (closeGrace), the
// session's share of the pool (FreeFor) -- and what they promise for memory.

// A session whose buffer ran full borrowed from the pool; its link dies
// with that data unread (a link wedged by readers that stopped is the one
// most likely to die). What it borrowed is in the stream buffers until the
// relays let the streams go (hs2's relayDieGrace), just as a claim's data
// is: it must stay off the pool until then, or the pool is handed out a
// second time while the first data is still in memory.
func TestRed2BorrowedDataKeepsItsPoolAfterClose(t *testing.T) {
	defer func(g time.Duration) { closeGrace = g }(closeGrace)
	closeGrace = 300 * time.Millisecond
	const size = 4 << 20
	for _, how := range []string{"closed", "read", "never"} {
		pool := NewWindowPool(size)
		cfg := poolConfig(pool)
		session, stream, p := newRawPeer(t, cfg)
		sendWithin(t, p, p.sid, cfg.MaxReceiveBuffer+600<<10, cfg.MaxFrameSize, "more than the buffer, not read")
		p.settle(stream)
		_, sp := extraOf(session)
		if sp < 600<<10 {
			t.Fatalf("%s: borrowed %d, want at least what came in past MaxReceiveBuffer", how, sp)
		}
		session.Close()
		past := unreadOf(stream) - cfg.MaxReceiveBuffer // in memory past the session's own buffer
		if held := size - pool.Free(); held < past {
			t.Errorf("%s: link closed holding %d unread (%d past its MaxReceiveBuffer, all of it borrowed); the pool holds %d for it: %d of that data is handed out again",
				how, unreadOf(stream), past, held, past-held)
		}
		switch how {
		case "closed": // its relay lets it go: the pool comes back at once
			stream.Close()
			if pool.Free() != size {
				t.Errorf("%s: pool free %d after the stream let its data go, want %d", how, pool.Free(), size)
			}
		case "read": // its relay drains it: the pool comes back as it is read
			buf := make([]byte, 64<<10)
			for unreadOf(stream) > 0 { // (its update fails: the session is closed)
				if n, _ := stream.Read(buf); n == 0 {
					t.Fatalf("%s: nothing read with %d unread", how, unreadOf(stream))
				}
			}
			if pool.Free() != size {
				t.Errorf("%s: pool free %d after all of it was read, want %d", how, pool.Free(), size)
			}
		case "never": // nothing lets go of it: closeGrace
			deadline := time.Now().Add(closeGrace + time.Second)
			for pool.Free() != size && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			if pool.Free() != size {
				t.Errorf("%s: pool free %d closeGrace after the close, want %d", how, pool.Free(), size)
			}
		}
		if e, sp := extraOf(session); e != 0 || sp != 0 {
			t.Errorf("%s: closed session left with extra %d spare %d", how, e, sp)
		}
	}
}

// The session's share of the pool (FreeFor) binds its windows, not its
// borrowing (borrow, exempt by design: a fast link's claims must not keep
// it from borrowing for what was in flight to its other streams). A link
// with a frozen fast app holds half the pool in claims; when its other
// readers then stop too (frozen apps rarely come alone: a phone that
// sleeps) it borrows the rest. Every other link of the process can then
// neither grow a window past NeedStreamBuffer nor borrow: the situation
// the share was meant to prevent, reached by one link instead of two.
// (Logged, not failed: a design limit, WINDOW_CORE §4.5; the data is in
// memory, nothing leaks. Its bound, and the pool back once the frozen
// streams close: TestPoolBorrowPastTheShareIsBoundedAndComesBack.)
func TestRed2OneLinkTakesThePoolByBorrowing(t *testing.T) {
	probe := poolConfig(NewWindowPool(1))
	excess := probe.MaxStreamBuffer - probe.NeedStreamBuffer
	size := 2 * excess
	pool := NewWindowPool(size)
	cfg := poolConfig(pool)
	a, sa, pa := newRawPeer(t, cfg)
	sa.SetCoreCeil(uint32(cfg.MaxStreamBuffer))
	keepUp(t, pa, sa, 4<<20)
	pa.sync(sa)
	pa.fill(cfg.MaxFrameSize) // the fast app freezes
	pa.settle(sa)
	claim, _ := extraOf(a)
	// its other users' apps stop too
	other, sid := pa.open(a)
	n := cfg.MaxReceiveBuffer + borrowMax*cfg.MaxReceiveBuffer
	go pa.trySendOn(sid, n, cfg.MaxFrameSize)
	deadline := time.Now().Add(2 * time.Second)
	for pool.Free() > 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	held, spare := extraOf(a)
	// another link: a fast reader under a ceiling, and a reader that stops
	b, sb, pb := newRawPeer(t, poolConfig(pool))
	sb.SetCoreCeil(uint32(cfg.MaxStreamBuffer))
	keepUp(t, pb, sb, 2<<20)
	pb.sync(sb)
	w := maxWindow(pb.updates())
	ob, sidb := pb.open(b)
	go pb.trySendOn(sidb, cfg.MaxReceiveBuffer+64<<10, cfg.MaxFrameSize)
	time.Sleep(100 * time.Millisecond)
	t.Logf("pool %d KiB: the frozen link holds %d KiB (claims %d, borrowed %d; %d KiB unread on its other stream); free %d KiB. "+
		"The other link: window up to %d KiB (need %d), %d KiB taken in of %d sent, borrows %d, parks %d",
		size>>10, held>>10, claim>>10, spare>>10, unreadOf(other)>>10, pool.Free()>>10,
		w>>10, cfg.NeedStreamBuffer>>10, unreadOf(ob)>>10, (cfg.MaxReceiveBuffer+64<<10)>>10, b.Borrows(), b.Parks())
}
