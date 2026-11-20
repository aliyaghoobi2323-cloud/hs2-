package engine

import (
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"
	"weak"

	"github.com/xtaci/smux"
)

// mem66: every minute, once the first sub exists, the levels that show
// whether finished connections still hold their subs and smux streams: live
// mConns (not yet ended), msubs not yet closed, edge subs open (light +
// bulk/stripe), streams in the smux sessions' maps, smux sessions open.
var memLogOnce sync.Once

// HS2_MEM_OBJS=1 (diagnosis, off by default): the line also counts the
// objects not yet collected (mConn, msub, smux Stream and Session), open or
// not: objects well above the open ones are finished ones still referenced.
var memObjMConns, memObjMSubs atomic.Int64

// mem68: the link layers under a session (HS2_MEM_OBJS): mtcpLink, the
// shapedConn smux writes through, the link meter and the session guard.
var memObjLinks, memObjShaped, memObjMeters, memObjGuards atomic.Int64

func init() {
	smux.CountObjects.Store(envBoolDefault("HS2_MEM_OBJS", false))
	memLogStart() // at start, not with the first sub: tests count goroutines
}

func memLogStart() { memLogOnce.Do(func() { go memLogLoop() }) }

func memLogLoop() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for range t.C {
		log.Print(memLogLine())
	}
}

func memLogLine() string {
	return fmt.Sprintf("mem: mconns=%d msubs=%d edgesubs=%d smuxstreams=%d smuxsessions=%d",
		obs.mconnOpen.Load(), obs.msubsLive.Load(), mconnStat.light.Load()+mconnStat.bulk.Load(),
		smux.LiveStreams(), smux.LiveSessions()) + smuxStallLine() + memObjLine() + zsLine() + b307CloseLine() + b312OpenLine() + b322Line() + b311CloseLine() + b313Line() + b325Line() + b327Line() + b337Line() + b339Line() + zduPinLine() + zduCountsLine()
}

func memObjLine() string {
	if !smux.CountObjects.Load() {
		return ""
	}
	st, se := smux.Objects()
	return fmt.Sprintf(" objs: mconns=%d msubs=%d streams=%d sessions=%d links=%d shaped=%d meters=%d guards=%d",
		memObjMConns.Load(), memObjMSubs.Load(), st, se, memObjLinks.Load(), memObjShaped.Load(), memObjMeters.Load(), memObjGuards.Load()) + memPinLine()
}

func memTrack[T any](p *T, c *atomic.Int64) { smux.TrackObject(p, c) }

// mem68 (HS2_MEM_OBJS): the live connections, held weakly, scanned each
// minute for references to dead subs that would pin their links.
var memMConns struct {
	mu sync.Mutex
	m  map[weak.Pointer[mConn]]struct{}
}

func memRegMConn(m *mConn) {
	if !smux.CountObjects.Load() && !zsOn.Load() {
		return
	}
	memMConns.mu.Lock()
	if memMConns.m == nil {
		memMConns.m = map[weak.Pointer[mConn]]struct{}{}
	}
	memMConns.m[weak.Make(m)] = struct{}{}
	memMConns.mu.Unlock()
}

// memPinLine: among live (not ended) connections, those whose raw first sub
// is dead, dead subs in call-score slots, a dead bulk sub, and dead links in
// deadKeys still strongly held.
func memPinLine() string {
	memMConns.mu.Lock()
	var ms []*mConn
	for w := range memMConns.m {
		if m := w.Value(); m != nil {
			ms = append(ms, m)
		} else {
			delete(memMConns.m, w)
		}
	}
	memMConns.mu.Unlock()
	live, raw, rawDead, rawHeld, score, bulk, dk := 0, 0, 0, 0, 0, 0, 0
	for _, m := range ms {
		m.mu.Lock()
		if !m.ended {
			live++
			if m.raw1 != nil {
				raw++
				if m.raw1.dead {
					rawDead++
					if _, ok := m.raw1.rw.(deadSubRW); !ok {
						rawHeld++ // still holding its stream
					}
				}
			}
			for _, s := range m.scoreSub {
				if s != nil && s.dead {
					score++
				}
			}
			if m.bulk != nil && m.bulk.dead {
				bulk++
			}
			for _, k := range m.deadKeys {
				if _, ok := k.(*mtcpLink); ok {
					dk++
				}
			}
		}
		m.mu.Unlock()
	}
	return fmt.Sprintf(" pins: mconns=%d raw=%d raw1dead=%d raw1held=%d scoredead=%d bulkdead=%d strongdeadkeys=%d released=%d", live, raw, rawDead, rawHeld, score, bulk, dk, obs.mconnRaw1Released.Load())
}
