package engine

import (
	"runtime"
	"strings"
)

// l485 trace (HS2_MCONN_TRACE only; no behaviour change): at every sub
// join, the caller chain that opened it and the chosen link's last-read age,
// tcp backoff and unacked, so a stuck call's open storm can be attributed.
func (m *mConn) l485TraceOpenLocked(s *msub, key any) {
	if m.tr == nil || s == nil {
		return
	}
	pcs := make([]uintptr, 12)
	n := runtime.Callers(3, pcs)
	fr := runtime.CallersFrames(pcs[:n])
	var chain []string
	for {
		f, more := fr.Next()
		fn := f.Function
		if i := strings.LastIndex(fn, "."); i >= 0 {
			fn = fn[i+1:]
		}
		chain = append(chain, fn)
		if !more || len(chain) >= 7 {
			break
		}
	}
	rdAge, bo, un := int64(-1), -1, -1
	if l, ok := key.(Link); ok {
		if mtr := linkMeterOf(l); mtr != nil {
			if rd := mtr.lastRd.Load(); rd != 0 {
				rdAge = (ctrlNow() - rd) / 1e6
			}
		}
		if ts, ok := linkTCPStatsOf(l); ok {
			bo, un = int(ts.backoff), int(ts.unacked)
		}
	}
	dark := 0
	for _, o := range m.subs {
		if o != s && (o.dc.isDark || o.dc.txDark) {
			dark++
		}
	}
	m.tr.ev("l485 open sub %d key=%p rdAge=%dms bo=%d unacked=%d darkSubs=%d via %s", s.idx, key, rdAge, bo, un, dark, strings.Join(chain, "<"))
}
