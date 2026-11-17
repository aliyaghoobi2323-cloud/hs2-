package smux

import (
	"bytes"
	"io"
	"sync/atomic"
	"testing"
	"time"
)

// floorTransfer: n bytes from cli to srv on a stream whose receiving end is
// marked SetHeavyFloor(floor); returns the receiving stream after the read.
func floorTransfer(t *testing.T, cli, srv *Session, n, floor int) (*Stream, *Stream) {
	t.Helper()
	return floorTransferMark(t, cli, srv, n, func(st *Stream) { st.SetHeavyFloor(floor) })
}

func floorTransferMark(t *testing.T, cli, srv *Session, n int, mark func(*Stream)) (*Stream, *Stream) {
	t.Helper()
	data := make([]byte, n)
	for i := range data {
		data[i] = byte(i * 7)
	}
	acc := make(chan *Stream, 1)
	got := make(chan []byte, 1)
	go func() {
		st, err := srv.AcceptStream()
		if err != nil {
			got <- nil
			return
		}
		mark(st)
		acc <- st
		b := make([]byte, n)
		io.ReadFull(st, b)
		got <- b
	}()
	st, err := cli.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	go st.Write(data)
	rs := <-acc
	if b := <-got; !bytes.Equal(b, data) {
		t.Fatal("data corrupted")
	}
	return st, rs
}

// A marked stream that receives at a heavy rate gets the floor as its base
// even when the budget allows no growth, the sender learns it, and closing
// returns everything.
func TestHeavyFloorRaisesBase(t *testing.T) {
	old := heavyFloorEpochBytes
	heavyFloorEpochBytes = 32 << 10
	t.Cleanup(func() { heavyFloorEpochBytes = old })
	aw := newAW(16<<10, 4<<20) // no room to grow: only the floor can raise it
	cli, srv := pair(t, 10*time.Millisecond, awConfig(nil), awConfig(aw))
	n0 := HeavyFloorN()
	ws, rs := floorTransfer(t, cli, srv, 4<<20, 512<<10)
	if HeavyFloorN() == n0 {
		t.Fatal("floor never applied")
	}
	rs.bufferLock.Lock()
	base, win := rs.aw.base, rs.aw.win
	rs.bufferLock.Unlock()
	if base != 512<<10 || win < base {
		t.Fatalf("base %d win %d", base, win)
	}
	waitFor(t, func() bool { return atomic.LoadUint32(&ws.peerWindow) >= 512<<10 }, "sender learns the floor")
	if u := aw.Budget.Stats().Used; u != 0 {
		t.Fatalf("floor drew %d from the budget", u)
	}
	ws.Close()
	rs.Close()
	waitFor(t, func() bool { return atomic.LoadInt32(&srv.bucket) == int32(srv.config.MaxReceiveBuffer) }, "bucket back to base")
}

// A stream that already grew keeps its accounting straight when the floor
// comes: the budget and the bucket go back to zero / the base afterwards.
func TestHeavyFloorAfterGrowth(t *testing.T) {
	old := heavyFloorEpochBytes
	heavyFloorEpochBytes = 32 << 10
	t.Cleanup(func() { heavyFloorEpochBytes = old })
	aw := newAW(64<<20, 2<<20)
	cli, srv := pair(t, 10*time.Millisecond, awConfig(nil), awConfig(aw))
	ws, rs := floorTransfer(t, cli, srv, 6<<20, 256<<10)
	rs.bufferLock.Lock()
	base := rs.aw.base
	rs.bufferLock.Unlock()
	if base != 256<<10 {
		t.Fatalf("base %d", base)
	}
	ws.Close()
	rs.Close()
	waitFor(t, func() bool { return aw.Budget.Stats().Used == 0 }, "budget returned")
	waitFor(t, func() bool { return atomic.LoadInt32(&srv.bucket) == int32(srv.config.MaxReceiveBuffer) }, "bucket back to base")
}

// Unmarked streams, and a floor not above the base, change nothing.
func TestHeavyFloorUnmarked(t *testing.T) {
	aw := newAW(64<<20, 2<<20)
	_, srv := pair(t, time.Millisecond, awConfig(nil), awConfig(aw))
	s := newStream(1, 16<<10, srv)
	s.SetHeavyFloor(32 << 10) // below the 64 KiB base
	if s.aw.floorWant != 0 {
		t.Fatal("floor below base kept")
	}
	var off Stream
	off.SetHeavyFloor(1 << 20) // no AutoWindow: no-op, no panic
}

// dl77: the queue gate drops a gated floor back to the old base (the sender
// learns the smaller window), holds it off with a doubling hold, and the
// accounting still comes back whole at close.
func TestHeavyFloorGatedDrop(t *testing.T) {
	old := heavyFloorEpochBytes
	heavyFloorEpochBytes = 32 << 10
	t.Cleanup(func() { heavyFloorEpochBytes = old })
	aw := newAW(16<<10, 4<<20)
	cli, srv := pair(t, 10*time.Millisecond, awConfig(nil), awConfig(aw))
	ws, rs := floorTransferMark(t, cli, srv, 4<<20, func(st *Stream) { st.SetHeavyFloorGated(512<<10, 1) })
	d0 := HeavyFloorDrops()
	rs.bufferLock.Lock()
	fb := rs.aw.floorBase
	if !rs.aw.floorGate || fb == 0 || fb >= 512<<10 {
		rs.bufferLock.Unlock()
		t.Fatalf("gate %v floorBase %d", rs.aw.floorGate, fb)
	}
	rs.awRaiseBaseLocked(512 << 10) // the test's loop may have queued: raised again
	rs.aw.floorAt = time.Now().UnixNano()
	rs.aw.floorHold = 0
	now := time.Now()
	rs.awDropFloorLocked(now)
	base, win, extra, hold := rs.aw.base, rs.aw.win, rs.aw.extra, rs.aw.floorHold
	may := rs.aw.awFloorMayRaiseLocked(now.UnixNano())
	rs.awRaiseBaseLocked(512 << 10) // a raise within the hold (as the epoch would)
	rs.awDropFloorLocked(now)       // dropped again soon: the hold doubles
	hold2 := rs.aw.floorHold
	rs.bufferLock.Unlock()
	if HeavyFloorDrops()-d0 < 2 || base != fb || win != base+extra || hold != floorHoldMin || hold2 != 2*floorHoldMin || may {
		t.Fatalf("base %d win %d extra %d hold %v/%v may %v", base, win, extra, hold, hold2, may)
	}
	var off Stream
	off.SetHeavyFloorGated(1<<20, 1) // no AutoWindow: no-op
	ws.Close()
	rs.Close()
	waitFor(t, func() bool { return aw.Budget.Stats().Used == 0 }, "budget returned")
	waitFor(t, func() bool { return atomic.LoadInt32(&srv.bucket) == int32(srv.config.MaxReceiveBuffer) }, "bucket back to base")
}

// dl77 soft: a cut takes a quarter off the raised base (never under the old
// one), and the next raise steps up a quarter, not back to the floor.
func TestHeavyFloorSoft(t *testing.T) {
	old := heavyFloorEpochBytes
	heavyFloorEpochBytes = 32 << 10
	t.Cleanup(func() { heavyFloorEpochBytes = old })
	aw := newAW(16<<10, 4<<20)
	cli, srv := pair(t, 10*time.Millisecond, awConfig(nil), awConfig(aw))
	ws, rs := floorTransferMark(t, cli, srv, 4<<20, func(st *Stream) {
		st.SetHeavyFloorGated(512<<10, 1)
		st.SetHeavyFloorSoft()
	})
	rs.bufferLock.Lock()
	fb := rs.aw.floorBase
	rs.awRaiseBaseLocked(512 << 10) // the test's loop may have cut it already
	now := time.Now()
	rs.aw.floorQE = false
	rs.awDropFloorLocked(now)
	b1 := rs.aw.base
	f1 := rs.aw.awFloorTargetLocked()
	rs.aw.floorQE = true
	f2 := rs.aw.awFloorTargetLocked()
	for i := 0; i < 20; i++ {
		rs.awDropFloorLocked(now)
	}
	b2, win, extra := rs.aw.base, rs.aw.win, rs.aw.extra
	rs.bufferLock.Unlock()
	if b1 != 384<<10 || f1 != 480<<10 || f2 != 0 || b2 != fb || win != b2+extra {
		t.Fatalf("b1 %d f1 %d f2 %d b2 %d win %d extra %d", b1, f1, f2, b2, win, extra)
	}
	ws.Close()
	rs.Close()
	waitFor(t, func() bool { return aw.Budget.Stats().Used == 0 }, "budget returned")
	waitFor(t, func() bool { return atomic.LoadInt32(&srv.bucket) == int32(srv.config.MaxReceiveBuffer) }, "bucket back to base")
}
