package engine

import (
	"fmt"
	"log"
	"strings"
	"time"
)

// dl184 diagnostics (HS2_MCONN_STRIPE_LOG=1, Iran): why a heavy connection
// never striped. Every return of the growth rule (stripeTickLocked), the
// download sampler and the raw open counts one reason; a connection that
// ends without ever striping after it lived at least dlTraceMinLife and
// received at least dlTraceMinBytes logs one line with the counts:
//
//	stripe nostripe 1a2b3c: life 12.4s recvd 46 MB max 31 Mbit/s [low=0 nothot=3 openfail=4 ...]
//
// Off (the key unset): nothing is counted or logged beyond an int add.
const (
	trOff = iota
	trWin
	trEarly
	trHeldOpen
	trHeldHs
	trHeldRetry
	trHeldCall
	trKCap
	trLow
	trNotHot
	trUpRoom
	trUnused
	trSlot
	trGrow
	trOpenFail
	trSteerHeld
	trDlCold
	trDlSolo
	trLightOp
	trLiteUp
	trN
)

var trNames = [trN]string{"off", "win", "early", "held-open", "held-hs", "held-retry", "held-call", "kcap", "low", "nothot",
	"uproom", "unused", "slot", "grow", "openfail", "steerheld", "dlcold", "dlsolo", "lightop", "liteup"}

const (
	dlTraceMinLife  = 6 * time.Second
	dlTraceMinBytes = 20 << 20
)

// stTr: the reason counts (under m.mu).
type stTr struct {
	n     [trN]uint16
	max   float64 // the largest 1 s growth-sample rate (bytes/s)
	upMax float64 // ul197: the same for the upload direction
	gw    [gwN]uint16
	wMax  int
	cutQ  [4]uint16 // ul197: cut samples by cutQ excess over cutAt: <1 ms, <3 ms, <10 ms, more
	held  uint16    // ul197: cut samples the hysteresis held (stripeUpHyst)
}

// trGateLocked (ul197, Iran's upload gate, under m.mu): one gate decision.
func (m *mConn) trGateLocked(why, w int, cutQ, cutAt time.Duration) {
	if !stripeLog {
		return
	}
	t := &m.stTrace
	if t.gw[why] < 65535 {
		t.gw[why]++
	}
	t.wMax = max(t.wMax, w)
	if why == gwCut {
		i := 3
		switch d := cutQ - cutAt; {
		case d < time.Millisecond:
			i = 0
		case d < 3*time.Millisecond:
			i = 1
		case d < 10*time.Millisecond:
			i = 2
		}
		if t.cutQ[i] < 65535 {
			t.cutQ[i]++
		}
	}
}

var gwNames = [gwN]string{"grow", "cut", "band", "hold", "token", "full"}

func (m *mConn) trLocked(r int) {
	if stripeLog && m.stTrace.n[r] < 65535 {
		m.stTrace.n[r]++
	}
}

func (m *mConn) trRateLocked(rate float64) {
	if stripeLog && rate > m.stTrace.max {
		m.stTrace.max = rate
	}
}

// trString: the non-zero counts.
func (t *stTr) String() string {
	var b strings.Builder
	for i, v := range t.n {
		if v > 0 {
			fmt.Fprintf(&b, " %s=%d", trNames[i], v)
		}
	}
	return strings.TrimSpace(b.String())
}

// stripeTraceEndLocked (end, under m.mu): the line for a heavy connection
// that never striped.
func (m *mConn) stripeTraceEndLocked() {
	if stripeLog && m.iran && m.sendOff >= dlTraceMinBytes && !m.stBorn.IsZero() {
		// ul197: every heavy upload, striped or not: why it widened or did not
		t := &m.stTrace
		var g strings.Builder
		for i, v := range t.gw {
			if v > 0 {
				fmt.Fprintf(&g, " %s=%d", gwNames[i], v)
			}
		}
		log.Printf("stripe uptrace %x: life %v sent %d MB subs=%d wmax=%d upmax %.0f Mbit/s active=%d [%s] gate[%s] cutx[<1ms=%d <3=%d <10=%d more=%d held=%d]", m.id&0xffffff,
			time.Since(m.stBorn).Round(100*time.Millisecond), m.sendOff>>20, m.stN, t.wMax, t.upMax*8/1e6, stripeStat.active.Load(), t.String(), strings.TrimSpace(g.String()),
			t.cutQ[0], t.cutQ[1], t.cutQ[2], t.cutQ[3], t.held)
	}
	if !stripeLog || !m.iran || m.stK < 2 || m.stN > 0 || m.rawBorn.IsZero() {
		return
	}
	life := time.Since(m.rawBorn)
	r := &m.r
	r.mu.Lock()
	got := max(uint64(m.rawRecvd.Load()), r.next)
	r.mu.Unlock()
	if life < dlTraceMinLife || got < dlTraceMinBytes {
		return
	}
	log.Printf("stripe nostripe %x: life %v recvd %d MB sent %d MB max %.0f Mbit/s raw=%v active=%d dlactive=%d [%s]", m.id&0xffffff,
		life.Round(100*time.Millisecond), got>>20, m.sendOff>>20, m.stTrace.max*8/1e6, m.rawW.Load(), stripeStat.active.Load(), stripeDlActive.Load(), m.stTrace.String())
}
