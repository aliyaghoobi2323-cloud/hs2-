package smux

import (
	"io"
	"sync/atomic"
	"testing"
	"time"
)

// aw163: a window update that overtook a newer one (a shrink's UPD sent from
// its own goroutine while a read sent a later one) moved the sender's limit
// back below what the receiver counted on. The receiver had read everything
// and its next update was due only after more data: the sender waited for
// window, the reader for data, forever (TestAutoWindowBudgetBounds hung).
//
// Here the stale UPD is put on the wire by hand once the receiver has read
// everything; the sender must ignore it and keep writing.
func TestStaleUPDIgnored(t *testing.T) {
	cfg := func() *Config {
		c := DefaultConfig()
		c.Version = 2
		c.MaxStreamBuffer = 64 << 10
		c.MaxReceiveBuffer = 1 << 20
		return c
	}
	cli, srv := pair(t, time.Millisecond, cfg(), cfg())
	cs, err := cli.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	const first = 100 << 10
	if _, err := cs.Write(make([]byte, first)); err != nil {
		t.Fatal(err)
	}
	rs, err := srv.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(rs, make([]byte, first)); err != nil {
		t.Fatal(err)
	}
	// the receiver's last UPD (consumed 96 KiB) reaches the sender
	waitFor(t, func() bool { _, c := cs.AckMarks(); return c >= 96<<10 }, "the receiver's updates")
	before := StaleUPDs()
	// an old UPD arrives after it: consumed 1, the base window
	if err := rs.sendWindowUpdate(1, 0); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // it arrives (1 ms one way)
	if _, c := cs.AckMarks(); c < 96<<10 {
		t.Errorf("the sender took the stale update: consumed %d", c)
	} else if StaleUPDs() == before {
		t.Errorf("the stale update was not counted")
	}
	// more data flows: without the guard the sender's limit is 1+64 KiB,
	// below the 100 KiB it wrote, and the receiver (all read, 4 KiB since its
	// last update) never sends another
	done := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(rs, make([]byte, first))
		done <- err
	}()
	cs.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := cs.Write(make([]byte, first)); err != nil {
		t.Fatalf("write after a stale update: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reader stuck")
	}
}

// The receiver side: a UPD numbered before one already sent is not sent
// (here both carry the same consumed count, which the sender's guard takes,
// so only the order kept by sendWindowUpdateSeq decides the window).
func TestUPDSeqOrder(t *testing.T) {
	aw := newAW(1<<20, 1<<20)
	cli, srv := pair(t, time.Millisecond, awConfig(nil), awConfig(aw))
	cs, err := cli.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cs.Write(make([]byte, 1000)); err != nil {
		t.Fatal(err)
	}
	rs, err := srv.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(rs, make([]byte, 1000)); err != nil {
		t.Fatal(err)
	}
	rs.bufferLock.Lock()
	older := rs.nextUPDSeqLocked()
	newer := rs.nextUPDSeqLocked()
	rs.bufferLock.Unlock()
	if err := rs.sendWindowUpdateSeq(1000, 70<<10, newer); err != nil {
		t.Fatal(err)
	}
	if err := rs.sendWindowUpdateSeq(1000, 900<<10, older); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if w := atomic.LoadUint32(&cs.peerWindow); w != 70<<10 {
		t.Fatalf("the older update went out after the newer: peer window %d", w)
	}
}

// Concurrent shrinks and reads keep the wire order of their computation:
// many goroutines numbering and sending UPDs at once leave the sender with
// the last numbered state.
func TestUPDSeqConcurrent(t *testing.T) {
	aw := newAW(1<<20, 1<<20)
	cli, srv := pair(t, 0, awConfig(nil), awConfig(aw))
	cs, err := cli.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cs.Write(make([]byte, 10)); err != nil {
		t.Fatal(err)
	}
	rs, err := srv.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(rs, make([]byte, 10)); err != nil {
		t.Fatal(err)
	}
	stale0 := StaleUPDs()
	var consumed atomic.Uint32
	consumed.Store(10)
	doneC := make(chan struct{})
	const workers, each = 8, 200
	for w := 0; w < workers; w++ {
		go func() {
			defer func() { doneC <- struct{}{} }()
			for i := 0; i < each; i++ {
				rs.bufferLock.Lock()
				c := consumed.Add(1) // computed in number order, like numRead
				seq := rs.nextUPDSeqLocked()
				rs.bufferLock.Unlock()
				if err := rs.sendWindowUpdateSeq(c, 64<<10, seq); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	for w := 0; w < workers; w++ {
		<-doneC
	}
	want := consumed.Load()
	waitFor(t, func() bool { _, c := cs.AckMarks(); return c == want }, "the last update")
	if n := StaleUPDs() - stale0; n != 0 {
		// the sender saw an older consumed after a newer one: the
		// receiver's order did not hold
		t.Fatalf("stale updates at the sender: %d", n)
	}
}
