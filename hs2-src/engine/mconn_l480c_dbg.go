package engine

import (
	"log"
	"os"
	"sync/atomic"
	"time"
)

// l480c HS2_L480C_DBG=1 (lab debug only, default off; log lines only, no
// behaviour change): one line per lost-copy reset (subLostResets) with the
// conn's class hints (bytes each way, age, call, hedge, heavy) so a lab run
// can say which conns the resets hit and when.
var l480cDbg = os.Getenv("HS2_L480C_DBG") == "1"

var l480cDbgN atomic.Int64

func (m *mConn) l480cLostLocked(s *msub, site string, cause error, lost bool) {
	if !l480cDbg || l480cDbgN.Add(1) > 3000 {
		return
	}
	r := &m.r
	r.mu.Lock()
	rnext, rfin := r.next, r.fin
	r.mu.Unlock()
	idx, sh, sf := -1, false, false
	if s != nil {
		idx, sh, sf = s.idx, s == m.hedgeSub.Load(), s == m.h4First
	}
	log.Printf("l480c lost: site=%s iran=%v id=%x age=%d sub=%d shedge=%v sfirst=%v cause=%v lost=%v hole=%v lite=%v mode=%d heavy=%v callOn=%v callUp=%v h4made=%v hedged=%v subs=%d sendOff=%d rnext=%d rfin=%v peerFin=%v wclosed=%v closing=%v",
		site, m.iran, m.id&0xffffff, time.Since(m.zsBorn).Milliseconds(), idx, sh, sf, cause, lost, m.holeRecentLocked(),
		m.lite, m.mode, m.heavy, m.callOn, m.callUp, m.h4Made, m.hedgeSub.Load() != nil, len(m.subs), m.sendOff, rnext, rfin,
		m.peerFin, m.wclosed, m.closing)
}
