package engine

import (
	"context"
	"encoding/binary"
	"io"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xtaci/smux"
)

// Red team round 2 (Z5 window core, part A).

// An edge whose exit is of the version before the window core, end to end
// over one real link: the exit answers the stats handshake with a recLen of
// 64 and sends 64-byte records (as b765677's serveStats does: no sender's
// fields), read by the edge's own openStats/runStats over a smux stream of
// the same link that carries a fast download, polled as the link manager
// polls. On the path where a new exit's records let the core grow the
// window past Need (TestWcoreRunnerGrowsAndDrains), the core never leaves
// OFF, no stream ever gets a ceiling, and no window passes Need.
// (TestWcoreStep1OldExitStaysOff stores the records in the meter itself.)
func TestRed2OldExitRealStatsNoCeiling(t *testing.T) {
	p := newWcPath(5<<20, 50*time.Millisecond)
	cfg := wcTestConfig()
	mtr := &linkMeter{statsPoll: make(chan struct{}, 1)}
	mtr.rttMin.Store(100_000)
	edge, exit := wcSessions(t, p.edge(), p.exit(), cfg, mtr)
	var sent atomic.Uint64
	go func() { // the exit: an old one's stats, and the download
		for {
			st, err := exit.AcceptStream()
			if err != nil {
				return
			}
			go func(st *smux.Stream) {
				defer st.Close()
				var hs [2]byte
				if _, err := io.ReadFull(st, hs[:]); err != nil {
					return
				}
				if hs[0] != kindStats {
					buf := make([]byte, 32<<10)
					for {
						n, err := st.Write(buf)
						sent.Add(uint64(n))
						if err != nil {
							return
						}
					}
				}
				st.Write([]byte{statsVer, statsRecMin, 3})
				start := time.Now()
				var q [4]byte
				for {
					if _, err := io.ReadFull(st, q[:]); err != nil {
						return
					}
					rec := make([]byte, statsRecMin)
					putStatsRec(rec, statsRec{seq: binary.BigEndian.Uint32(q[:]), flags: statsFlagChrono,
						mono: uint64(time.Since(start)), tx: sent.Load()})
					if _, err := st.Write(rec); err != nil {
						return
					}
				}
			}(st)
		}
	}()
	ws := &wcSet{tracePath: filepath.Join(t.TempDir(), "trace")}
	l := newWcLink(edge, cfg, nil, mtr, false, nil)
	ws.put(l)
	ctx, cancel := context.WithCancel(context.Background())
	statsDone := make(chan struct{})
	go func() { defer close(statsDone); openStats(ctx, &ctrlFakeLink{sess: edge, m: mtr}, nil) }()
	t.Cleanup(func() { cancel(); edge.Close(); <-statsDone })
	st, err := edge.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	st.Write([]byte{0xFF, 0xFF}) // not a stats stream: the download
	go io.Copy(io.Discard, st)
	runWcSet(t, ws)
	need := uint32(cfg.NeedStreamBuffer)
	var seq uint32
	for end := time.Now().Add(8 * time.Second); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		pollStats(mtr)
		if r := mtr.peer.Load(); r != nil {
			seq = r.seq
			if r.srtt != 0 || r.segsOut != 0 {
				t.Fatalf("an old exit's record read with the sender's fields: %+v", r)
			}
		}
		if s := wcState(l.inStats.state.Load()); s != wcOff || l.inStats.active.Load() || st.CoreCeil() != 0 {
			t.Fatalf("an old exit's records only (%d so far): core %v (active %v), ceiling %d", seq, s, l.inStats.active.Load(), st.CoreCeil())
		}
	}
	if mtr.statsState.Load() != statsOK || seq < 5 {
		t.Fatalf("stats state %d, %d records: the old exit's records never came", mtr.statsState.Load(), seq)
	}
	tr := readTrace(t, ws.tracePath)
	if fast := wcStayedOff(t, tr, need); fast < 5 {
		t.Fatalf("the stream counted as fast in %d of %d evaluations: the path tested nothing", fast, len(tr))
	}
	// (the download's cover, the windows' own without the core, is all the
	// buffer it may hold past its own: smux Cover)
	if e := edge.Extra(); e > int(smux.Cover(need, ^uint32(0))-need) {
		t.Fatalf("the edge's buffer is %d past its own with no ceiling", e)
	}
	t.Logf("%d records of 64 bytes, %d MiB downloaded, %d evaluations, core OFF throughout, no ceiling, no window past %d",
		seq, sent.Load()>>20, len(tr), smux.Cover(need, ^uint32(0)))
}

// A link whose frozen apps filled its buffer and made it borrow from the
// window pool dies (its session closes, as on a link lost while wedged).
// What it borrowed covers data its relays still hold, unread, until they let
// the streams go relayDieGrace later: until then that part of the pool is
// not handed to any other link (before red team round 2 it came back the
// moment the session closed); once the relays end it all comes back.
func TestRed2DeadLinkKeepsBorrowedPoolUntilRelaysLetGo(t *testing.T) {
	if testing.Short() {
		t.Skip("real-time relays")
	}
	grace := relayDieGrace.Load()
	relayDieGrace.Store(int64(time.Second))
	t.Cleanup(func() { relayDieGrace.Store(grace) })
	free := windowPool().Free()
	r := newWedgeRig(t, true)
	stuck := r.openFrozen(40) // 256 KiB each: 10 MiB, past SmuxSessionBuffer
	var borrowed int
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if b := free - windowPool().Free(); b > 0 && b == borrowed {
			break
		} else {
			borrowed = b
		}
	}
	if borrowed < 1<<20 || r.edge.Borrows() == 0 {
		t.Fatalf("the frozen apps made the link borrow %d (borrows %d): the rig tested nothing", borrowed, r.edge.Borrows())
	}
	r.edge.Close() // the link dies
	closedAt := time.Now()
	time.Sleep(100 * time.Millisecond)
	if held := free - windowPool().Free(); held < borrowed/2 {
		t.Fatalf("the link died holding %d KiB borrowed, its relays still holding the data: the pool holds %d KiB for it", borrowed>>10, held>>10)
	}
	for _, u := range stuck {
		select {
		case <-u.ended:
		case <-time.After(5 * time.Second):
			t.Fatal("a relay outlived relayDieGrace")
		}
	}
	for windowPool().Free() != free {
		if time.Since(closedAt) > 5*time.Second {
			t.Fatalf("the relays let go: pool %d of %d", windowPool().Free(), free)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Logf("borrowed %d KiB; held with the data after the death, whole %v after it (relayDieGrace %v)", borrowed>>10,
		time.Since(closedAt).Round(10*time.Millisecond), time.Second)
}
