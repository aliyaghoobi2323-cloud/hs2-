package engine

import (
	"log"
	"sync/atomic"
	"time"
)

// l487 (lab only, HS2_L487_DBG=1): one line per call-rate window (either
// direction >= 3 KB/s and both <= 64 KB/s) with the call sampler's state and
// the darkcall gate inputs, to find why some runs never see a call conn as a
// call (darkcall visits 0).
var l487Dbg = envBoolDefault("HS2_L487_DBG", false)

var l487DbgN atomic.Int64

func (m *mConn) l487DbgRollLocked(now time.Time, w time.Duration, sent int, got uint64, like bool) {
	if !l487Dbg || w <= 0 {
		return
	}
	sec := w.Seconds()
	hs, hg := float64(sent)/sec, float64(got)/sec
	if max(hs, hg) < 3000 || max(hs, hg) > 64000 {
		return
	}
	if l487DbgN.Add(1) > 40000 {
		return
	}
	m.rkMu.Lock()
	rk := m.rkInit
	m.rkMu.Unlock()
	log.Printf("l487 roll %x iran=%v w=%dms sent=%d got=%d like=%v streak=%d prev=%.0f callOn=%v likeAge=%v lite=%v rsc=%v rk=%v mode=%d stN=%d heavy=%v subs=%d",
		m.id&0xffffff, m.iran, w.Milliseconds(), sent, got, like, m.txStreak, m.txPrev, m.callOn,
		func() time.Duration {
			if m.callLikeAt.IsZero() {
				return -1
			}
			return now.Sub(m.callLikeAt).Round(time.Millisecond)
		}(), m.lite, m.rscCap, rk, m.mode, m.stN, m.heavy, m.liveSubsLocked())
}
