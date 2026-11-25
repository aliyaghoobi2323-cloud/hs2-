package engine

import (
	"log"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// rc15b (lab3 b449j, link kills every 4 s + 15 % blackholes every 12 s;
// lab1 ZDU exit at 12k): two fixes for K4M2 (HEDGE8 default on).
//
// HS2_RC15_HEAVY_KBPS (default 32; 0 = the old cumulative rule): the heavy
// rule (hedge3HeavyLocked: one copy on the primary, Iran drops the hedge sub)
// fired on a hedged conn once it had moved hedge3Bytes (4096) in total. A
// long-lived light conn (the crowd: ~1 small request/s for 20 s) crossed 4 KB
// after a few requests, lost its second copy and rode one records sub, which
// has no raw rescue: under link kills its requests waited for the sub-death
// resend or a lost-copy reset (lab3 rc15b d1: crowd p99 3617 ms, Iran
// lost-copy resets 74 vs rc14 10-18; heavy=6132 of 6581 hedged conns). With
// the rule off (HEDGE3_BYTES 1 MB, arm e1) crowd p99 173 ms and 14 resets.
// Now a hedged conn is heavy only when it moved more than hedge3Bytes plus
// HEAVY_KBPS for every second of its age: a download or a burst crosses it
// at once (one copy, as before), a light conn keeps both copies (its
// duplicates stay under the lite budget, liteBud).
//
// HS2_RC15_HEDGE_NOWAIT (default on): Iran's hedge sub open never waits for a
// HS2_B457_PICKCAP slot. Every new lite conn opened its hedge sub through the
// cap (2 picks at once), so at a ZDU drain end the hedge opens queued with the
// rescue joins (lab1 12k: capwait 13971..14525 waits, max 676..907 ms, vs
// rc14 1549..3054, max 62..97). Without a free slot the hedge is skipped (the
// conn goes on with its first sub alone, as a non-hedged conn does).
var (
	rc15HeavyBps    = rc15EnvKbps("HS2_RC15_HEAVY_KBPS", 32) << 10
	rc15HedgeNoWait = envBoolDefault("HS2_RC15_HEDGE_NOWAIT", true)
)

func rc15EnvKbps(name string, def uint64) uint64 {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		log.Printf("rc15b: %s=%q not a non-negative integer, default %d", name, v, def)
		return def
	}
	return n
}

var rc15bStat struct {
	rateHeld  atomic.Uint64 // heavy checks over hedge3Bytes held back by the rate allowance
	noWait    atomic.Uint64 // hedge opens skipped: no free pick slot
	rateHeavy atomic.Uint64 // hedged conns made heavy past the allowance
}

func init() {
	if liteHedge3 {
		log.Printf("rc15b: heavy allowance %d KB/s, hedge open nowait=%v", rc15HeavyBps>>10, rc15HedgeNoWait)
	}
}

// rc15HeavyLim (under m.mu): the bytes a hedged conn may move either way and
// still be light at now.
func (m *mConn) rc15HeavyLim(now time.Time) uint64 {
	if rc15HeavyBps == 0 || m.zsBorn.IsZero() {
		return hedge3Bytes
	}
	age := now.Sub(m.zsBorn)
	if age <= 0 {
		return hedge3Bytes
	}
	return hedge3Bytes + uint64(float64(rc15HeavyBps)*age.Seconds())
}

// b457PickTry takes a pick slot without waiting; ok false: none free (the
// cap off: always ok).
func b457PickTry() (rel func(), ok bool) {
	p := b457PickCap.Load()
	if p == nil {
		return b457Noop, true
	}
	c := *p
	select {
	case c <- struct{}{}:
		return func() { <-c }, true
	default:
		return nil, false
	}
}

var rc15bLogAt atomic.Int64

// rc15bLog: the counters, at most every 10 s (from hedge3Log).
func rc15bLog(now time.Time) {
	n := monoNano(now)
	at := rc15bLogAt.Load()
	if n-at < int64(10*time.Second) || !rc15bLogAt.CompareAndSwap(at, n) {
		return
	}
	log.Printf("rc15b: heavy_rate=%d rate_held=%d hedge_nowait_skip=%d", rc15bStat.rateHeavy.Load(), rc15bStat.rateHeld.Load(), rc15bStat.noWait.Load())
	rc15cLog()
	rc15dLog()
}
