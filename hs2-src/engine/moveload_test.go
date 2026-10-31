package engine

import (
	"encoding/binary"
	"io"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// R1 fix 1 (moveload.go): why the short VLESS connections of the main
// servers stayed on QUIET, and the load rule that moves them.

// TestMoveLoadRootCause: the per-connection rules against a connection of
// a few hundred KiB, tick by tick (100 ms each).
func TestMoveLoadRootCause(t *testing.T) {
	// the exit's hint: a download fed at its source's rate (3 Mbit/s, 37.5
	// KB a tick in one 32 KiB write that waits 20 % of the tick) is never
	// backlogged; one that is backlogged the whole time is hinted only at
	// its 4th tick (a tick starts the rule, then hintRun)
	var src, full hintRule
	srcAt, fullAt := -1, -1
	for i := 0; i < 20; i++ {
		now := int64(i+1) * int64(moveTick)
		if src.tick(int64(i)*int64(moveTick)/5, now) && srcAt < 0 {
			srcAt = i
		}
		if full.tick(int64(i)*int64(moveTick), now) && fullAt < 0 {
			fullAt = i
		}
	}
	t.Logf("exit hint: source-limited 3 Mbit/s download hinted at tick %d (-1: never in 2 s); backlogged at tick %d (%d ms): a 400 KiB download at 10 Mbit/s ended at tick 3",
		srcAt, fullAt, (fullAt+1)*100)
	if srcAt != -1 || fullAt < 3 {
		t.Fatalf("hint ticks: source-limited %d, backlogged %d", srcAt, fullAt)
	}
	// the edge's rules: an upload at its source's rate (writer in Write 20 %
	// of a tick) never moves; a download with the hint stream up is the
	// exit's; the load rule moves both once 64 KiB passed in a second
	var up, down moveRule
	upAt, downAt, loadUpAt, loadDownAt := -1, -1, -1, -1
	var tx, rx uint64
	var busy time.Duration
	for i := 0; i < 20; i++ {
		now := int64(i+1) * int64(moveTick)
		tx += 37500
		rx += 37500
		busy += moveTick / 5
		if d, _ := up.tick(tx, 0, busy, now, true, false); d && upAt < 0 {
			upAt = i
		}
		if d, _ := down.tick(0, rx, 0, now, true, false); d && downAt < 0 {
			downAt = i
		}
		if d, dir := up.loadDue(true, true); d && dir == "up" && loadUpAt < 0 {
			loadUpAt = i
		}
		if d, dir := down.loadDue(true, true); d && dir == "down" && loadDownAt < 0 {
			loadDownAt = i
		}
	}
	t.Logf("edge: 3 Mbit/s upload moved at tick %d, download (hint up) at %d; load rule: upload at %d, download at %d", upAt, downAt, loadUpAt, loadDownAt)
	if upAt != -1 || downAt != -1 || loadUpAt != 2 || loadDownAt != 2 {
		t.Fatalf("edge ticks: upload %d, download %d, load up %d, load down %d", upAt, downAt, loadUpAt, loadDownAt)
	}
	// TwoWay does not hold a VLESS download: its user's requests (1.5 and
	// 1 KiB in its first ticks) are under a sixteenth of what comes back
	// once the download passed 48 KiB
	user := []uint32{1500, 1000, 0, 0}
	other := []uint32{5000, 60000, 100000, 100000}
	ru, ro := make([]uint32, twoWayWin), make([]uint32, twoWayWin)
	for i := range user {
		ru[i], ro[i] = user[i], other[i]
		if st := twoWayState(ru, ro, i, i+1); i >= 1 && st != twoWayNo {
			t.Fatalf("a VLESS download, tick %d: two-way state %d", i, st)
		}
	}
}

// qpSized: the exit sends size bytes at rate bytes/s (the 8 bytes after the
// header) in 32 KiB writes, as a relay fed by a remote server, then ends.
const qpSized = 9

// sizedServe is qpSized's exit side (quietRig.serve).
func sizedServe(conn io.ReadWriter) {
	var h [8]byte
	if _, err := io.ReadFull(conn, h[:]); err != nil {
		return
	}
	size, rate := int(binary.BigEndian.Uint32(h[:4])), float64(binary.BigEndian.Uint32(h[4:]))
	b := make([]byte, 32<<10)
	t0 := time.Now()
	for off := 0; off < size; off += len(b) {
		n := min(len(b), size-off)
		if _, err := conn.Write(b[:n]); err != nil {
			return
		}
		if d := time.Duration(float64(off+n)/rate*1e9) - time.Since(t0); d > 0 {
			time.Sleep(d)
		}
	}
}

// TestMoveLoadShortDownloads: on a 40 Mbit/s QUIET link (and a BULK one),
// with the exit's hint as on the main servers, 8 connections a second of
// 200 to 500 KiB each at 3 Mbit/s from their source (about 24 Mbit/s
// together) next to a two-way 1.5 Mbit/s call: without the load rule
// nearly all of their bytes stay on QUIET, with it most go to BULK; the
// call stays on QUIET both times.
func TestMoveLoadShortDownloads(t *testing.T) {
	angleCores(t)
	if testing.Short() {
		t.Skip("emulated links, 14 s")
	}
	type res struct {
		quietShare        float64
		opened, moved     int
		callMoved         bool
		edgeLoad, exitHnt int64
	}
	run := func(on int) res {
		withTune(t, MoveLoad, on)
		r := hintRig(t, 4*angleRateBps, 10*time.Millisecond)
		defer quiesce(r)
		call := r.open(3, qpEcho)
		go io.Copy(io.Discard, call)
		callDone := make(chan struct{})
		go func() { defer close(callDone); callOn(t, call, 5500*time.Millisecond) }()
		q0, b0 := r.quiet.xmtr.wrBytes.Load(), r.bulk.xmtr.wrBytes.Load()
		l0, h0 := loadMoved.Load(), exitHints.load.Load()
		var wg sync.WaitGroup
		var opened, moved atomic.Int64
		rng := rand.New(rand.NewPCG(1, 2))
		tk := time.NewTicker(125 * time.Millisecond)
		for i := 0; i < 40; i++ {
			<-tk.C
			size := 200<<10 + rng.IntN(300<<10)
			st := r.open(uint32(10+i%7), qpSized)
			opened.Add(1)
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer st.Close()
				var h [8]byte
				binary.BigEndian.PutUint32(h[:4], uint32(size))
				binary.BigEndian.PutUint32(h[4:], 375000)
				if _, err := st.Write(h[:]); err != nil {
					return
				}
				got, _ := io.Copy(io.Discard, st)
				if got != int64(size) {
					t.Errorf("a connection read %d of %d", got, size)
				}
				if st.(*groupStream).g.Moved() != nil {
					moved.Add(1)
				}
			}()
		}
		tk.Stop()
		wg.Wait()
		<-callDone
		cm := call.(*groupStream).g.Moved() != nil
		call.Close()
		dq, db := r.quiet.xmtr.wrBytes.Load()-q0, r.bulk.xmtr.wrBytes.Load()-b0
		out := res{float64(dq) / float64(dq+db), int(opened.Load()), int(moved.Load()), cm, loadMoved.Load() - l0, exitHints.load.Load() - h0}
		t.Logf("MoveLoad %d: download bytes on QUIET %.0f %% (%.1f of %.1f MB); %d of %d connections moved; by load: edge %d, exit hints %d; call moved %v; %s",
			on, 100*out.quietShare, float64(dq)/1e6, float64(dq+db)/1e6, out.moved, out.opened, out.edgeLoad, out.exitHnt, cm, r.q.line())
		return out
	}
	before := run(0)
	after := run(1)
	if before.quietShare < 0.6 {
		t.Fatalf("without the load rule QUIET carried only %.0f %%: the rig does not show the fault", 100*before.quietShare)
	}
	if after.quietShare > 0.5 || after.moved < after.opened*3/4 {
		t.Fatalf("with the load rule QUIET carried %.0f %%, %d of %d moved", 100*after.quietShare, after.moved, after.opened)
	}
	if before.callMoved || after.callMoved {
		t.Fatalf("the call moved: without the rule %v, with it %v", before.callMoved, after.callMoved)
	}
}
