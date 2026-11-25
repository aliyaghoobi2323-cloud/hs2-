package engine

import (
	"log"
	"os"
	"sync/atomic"
	"time"
)

// b460 HS2_LITE_HEDGE4=1 (both sides, lite; default off): F1d. It turns on
// HEDGE3 (mconn_b459_hedge3.go) and changes one thing at the exit.
//
// F1c's burst cost (2-4 MB p90 +50 ms on lab2 <-> lab 4): when the hedge sub
// reached Kharej first (exit_first, 28-43 % of hedged conns) it created the
// conn and stayed its primary, so once the heavy rule made the conn single
// copy the rest of the download rode the hedge sub's link: the link Iran
// picked second (the hedge avoids the first sub's key, the lane pick's best
// link). With HEDGE4 the exit makes Iran's first sub the primary as soon as
// it joins a hedge-created conn, so the one-copy part of a heavy transfer
// rides the first sub's link, as on a non-hedged conn. Bytes already queued
// on the hedge sub are still written there (the receiver orders by offset).
//
// Not done here: the first sub of a hedged conn is still records, not raw
// (a records -> raw switch needs a new boundary handshake; see
// lab/b460/PLAN.txt).
var liteHedge4 = os.Getenv("HS2_LITE_HEDGE4") == "1" || os.Getenv("HS2_LITE_HEDGE7") == "1" || liteHedge8Key || liteHedge10 // b465: HEDGE7 runs on HEDGE4

var hedge4Stat struct {
	firstPref atomic.Uint64 // Kharej: Iran's first sub joined a hedge-created conn and became primary
	switched  atomic.Uint64 // ... and the primary changed (it was the hedge sub)
	firstB    atomic.Uint64 // one-copy bytes of heavy hedged conns on Iran's first sub
	hedgeB    atomic.Uint64 // one-copy bytes of heavy hedged conns on another sub (the hedge's)
	ends      atomic.Uint64 // hedged conns ended
	errEnds   atomic.Uint64 // ... with an error (not a clean close)
	rstEnds   atomic.Uint64 // ... with an RST
	heavyErr  atomic.Uint64 // heavy hedged conns ended with an error
}

// hedge4WantFirstLocked (Kharej, under m.mu): the next sub added is Iran's
// first sub joining a conn its hedge sub created.
func (m *mConn) hedge4WantFirstLocked() {
	if liteHedge4 && !m.iran {
		m.h4Want = true
	}
}

// hedge4AddLocked (addSubLocked, before setPrimaryLocked): make it primary.
func (m *mConn) hedge4AddLocked(s *msub, bulk bool) {
	if !m.h4Want || bulk {
		return
	}
	m.h4Want = false
	m.h4First = s
	old := m.primary.Load()
	m.prefPrimary = s
	hedge4Stat.firstPref.Add(1)
	if old != nil && old != s {
		hedge4Stat.switched.Add(1)
		m.tr.ev("hedge4: primary %d -> first sub %d (sent %d)", old.idx, s.idx, m.sendOff)
	}
}

// hedge4OneCopyLocked (sendLocked one-copy path): count where a heavy hedged
// conn's single copy went.
func (m *mConn) hedge4OneCopyLocked(p *msub, n int) {
	if !liteHedge4 || !m.h3Heavy {
		return
	}
	// Iran's primary is its first sub; the exit's too unless the hedge
	// sub created the conn (then: once the first sub joined, HEDGE4)
	if m.iran || !m.h4Made || p == m.h4First {
		hedge4Stat.firstB.Add(uint64(n))
		return
	}
	hedge4Stat.hedgeB.Add(uint64(n))
}

// hedge4EndLocked (end): resets and errors of hedged conns.
func (m *mConn) hedge4EndLocked(err error, rst bool) {
	if !liteHedge4 || !m.h3 {
		return
	}
	hedge4Stat.ends.Add(1)
	if err != nil && err != errMConnClosed {
		hedge4Stat.errEnds.Add(1)
		if m.h3Heavy {
			hedge4Stat.heavyErr.Add(1)
		}
	}
	if rst {
		hedge4Stat.rstEnds.Add(1)
	}
	hedge4Log(time.Now())
}

var hedge4LogAt atomic.Int64

// hedge4Log: the counters, at most every 10 s.
func hedge4Log(now time.Time) {
	if !liteHedge4 {
		return
	}
	n := monoNano(now)
	at := hedge4LogAt.Load()
	if n-at < int64(10*time.Second) || !hedge4LogAt.CompareAndSwap(at, n) {
		return
	}
	log.Printf("b460 hedge4: heavy=%d first_pref=%d switched=%d first=%dKB hedge=%dKB ends=%d err=%d rst=%d heavy_err=%d",
		hedge3Stat.heavy.Load(), hedge4Stat.firstPref.Load(), hedge4Stat.switched.Load(),
		hedge4Stat.firstB.Load()>>10, hedge4Stat.hedgeB.Load()>>10,
		hedge4Stat.ends.Load(), hedge4Stat.errEnds.Load(), hedge4Stat.rstEnds.Load(), hedge4Stat.heavyErr.Load())
}
