package engine

import (
	"log"
	"os"
	"sync/atomic"
	"time"
)

// b466 HS2_LITE_HEDGE8=1 (Kharej acts; default off; turns on HEDGE7, HEDGE4
// and HEDGE3 too, so set it on both sides): F1j. When Iran's hedge sub
// reaches the exit before Iran's first sub (exit_first, 30-40 % of hedged
// conns in F1h/F1i), the hedge sub created the conn, became its primary, and
// HEDGE4 later switched the primary to the first sub (switched=190-300 a
// run). With the key the hedge sub first waits up to hedge8Wait
// (HS2_LITE_HEDGE8_MS, default 20) for the first sub's conn and joins it;
// only when the first sub is later still (its open segment lost, the case
// the hedge is for) does the hedge sub create the conn as before. No wire
// change (Iran unchanged; an exit without the key behaves as HEDGE7).
var liteHedge8 = liteHedge8Key || liteHedge10

// rc15: HEDGE8 (K4M2 with HEDGE3_BYTES 4096 / HEDGE3_MS 200) is the default;
// HS2_LITE_HEDGE8=0 turns it (and the HEDGE3/4/7 it implies) off.
var liteHedge8Key = os.Getenv("HS2_LITE_HEDGE8") != "0" // b468l: HEDGE10 runs on HEDGE8

var hedge8Wait = time.Duration(hedge3EnvU64("HS2_LITE_HEDGE8_MS", 20)) * time.Millisecond

var hedge8Stat struct {
	waited atomic.Uint64 // hedge subs that waited for the first sub
	joined atomic.Uint64 // ... and joined its conn (no exit_first)
	made   atomic.Uint64 // ... and created the conn after the wait
}

func init() {
	if liteHedge8 {
		log.Printf("b466 hedge8: exit hedge sub waits %v for the first sub", hedge8Wait)
	}
}

// exitHedge8Wait (Kharej, serveMConnSub, before reserve): a hedge sub with
// the key waits up to hedge8Wait for the first sub's conn; it returns that
// conn (the caller attaches the hedge sub as a join) or nil (go on: create).
func exitHedge8Wait(tbl *mconnTable, id uint64, role byte, stop <-chan struct{}) *mConn {
	if !liteHedge8 || role != mconnRoleHedge || hedge8Wait <= 0 {
		return nil
	}
	hedge8Stat.waited.Add(1)
	mc := tbl.join(id, hedge8Wait, stop)
	if mc == nil {
		hedge8Stat.made.Add(1)
		return nil
	}
	hedge8Stat.joined.Add(1)
	hedge3Stat.exitJoin.Add(1)
	return mc
}

// HS2_B466_DBG=1 (debug only, no behaviour change; Kharej counters):
// how late Iran's first sub joins a hedge-created conn, how often a hedged
// conn's one-copy send waits on a full primary queue, and the bytes queued
// on the hedge sub after HEDGE4's primary switch. Logged every 10 s.
var b466Dbg = os.Getenv("HS2_B466_DBG") == "1"

// first-join delay buckets: <1 <5 <20 <50 <200 >=200 ms
var b466Lim = [...]time.Duration{time.Millisecond, 5 * time.Millisecond, 20 * time.Millisecond, 50 * time.Millisecond, 200 * time.Millisecond}

var b466Stat struct {
	late     [len(b466Lim) + 1]atomic.Uint64
	ocWait   atomic.Uint64 // hedged one-copy sends that waited (primary queue full)
	ocWaitH  atomic.Uint64 // ... on a hedge-created conn
	madeHvy  atomic.Uint64 // hedge-created conns that went heavy
	plainHvy atomic.Uint64 // first-created hedged conns that went heavy
}

// b466MadeLocked (Kharej): the hedge sub created this conn.
func (m *mConn) b466MadeLocked() {
	if b466Dbg {
		m.h8Made = time.Now()
	}
}

// b466FirstJoin (Kharej): Iran's first sub joined a hedge-created conn.
func (m *mConn) b466FirstJoin() {
	if !b466Dbg {
		return
	}
	m.mu.Lock()
	t := m.h8Made
	m.mu.Unlock()
	if t.IsZero() {
		return
	}
	d := time.Since(t)
	i := 0
	for i < len(b466Lim) && d >= b466Lim[i] {
		i++
	}
	b466Stat.late[i].Add(1)
	b466Log(time.Now())
}

// b466OcWaitLocked (sendLocked one-copy path, primary full).
func (m *mConn) b466OcWaitLocked() {
	if !b466Dbg || !m.h3 {
		return
	}
	b466Stat.ocWait.Add(1)
	if m.h4Made {
		b466Stat.ocWaitH.Add(1)
	}
}

// b466HeavyLocked (hedge3HeavyLocked, once).
func (m *mConn) b466HeavyLocked() {
	if !b466Dbg || m.iran {
		return
	}
	if m.h4Made {
		b466Stat.madeHvy.Add(1)
	} else {
		b466Stat.plainHvy.Add(1)
	}
}

var b466LogAt atomic.Int64

func b466Log(now time.Time) {
	if !b466Dbg {
		return
	}
	n := monoNano(now)
	at := b466LogAt.Load()
	if n-at < int64(10*time.Second) || !b466LogAt.CompareAndSwap(at, n) {
		return
	}
	l := &b466Stat.late
	log.Printf("b466 dbg: first_late <1=%d <5=%d <20=%d <50=%d <200=%d >=200=%d oc_wait=%d oc_wait_made=%d heavy_made=%d heavy_first=%d h8 waited=%d joined=%d made=%d",
		l[0].Load(), l[1].Load(), l[2].Load(), l[3].Load(), l[4].Load(), l[5].Load(),
		b466Stat.ocWait.Load(), b466Stat.ocWaitH.Load(), b466Stat.madeHvy.Load(), b466Stat.plainHvy.Load(),
		hedge8Stat.waited.Load(), hedge8Stat.joined.Load(), hedge8Stat.made.Load())
}

// hedge8Log (from hedge3Log, at most every 10 s): engagement with the key.
func hedge8Log() {
	if liteHedge8 {
		log.Printf("b466 hedge8: waited=%d joined=%d made=%d", hedge8Stat.waited.Load(), hedge8Stat.joined.Load(), hedge8Stat.made.Load())
	}
}
