package engine

import (
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// st244: pace the lite raw upgrades (HS2_ST244_UPG_RATE=N upgrades a second,
// Iran = the edge decides upgrades; 0 / unset = off, as before).
//
// Every lite connection that is mLiteAfter (1 s) old and quiet opens a second
// sub (rawTick -> upgradeRaw: a pick over the pool, a smux open, the UPGRADE
// handshake on both sides). An open storm (lab 4x: 6000 new connections in
// ~1.5 s) is followed ~1 s later by a second storm of 6000 upgrades, which
// lands on the same links, LinkManager read lock and both processes' CPU
// while the storm's own opens are still being served — the storm-second
// crowd p99 spike (st244.md). The second sub is an insurance copy, never on
// a user's latency path: the connection works raw until it comes. So the
// upgrades take tokens from one global bucket (N a second, burst N/10); a
// connection that finds the bucket empty stays raw and asks again at its next
// poll (mLitePoll later), so a storm's upgrades spread over a few seconds
// instead of one, and steady churn (a few hundred a second) never waits.
// Call upgrades (callUp, a call wants its copy now) and stripe opens are not
// paced.
//
// rel69h: on by default on the edge (envdef row "auto" = max(500, 100 x
// GOMAXPROCS) a second: lab3 6 edge cores -> 600/s, a 20-core Iran -> 2000/s,
// so the bucket grows with the box that has to absorb the storm). An explicit
// HS2_ST244_UPG_RATE wins (a number; 0 = off); HS2_DEFAULTS=legacy and the
// exit (which never upgrades) keep it off. Read in loadEnvKeys.
var st244Rate atomic.Int64 // upgrades/s, 0 = off

// st244RateFrom parses HS2_ST244_UPG_RATE: "auto" = the per-core default, a
// number = that rate (<= 0 or junk: off).
func st244RateFrom(v string) int64 {
	v = strings.TrimSpace(v)
	if strings.EqualFold(v, "auto") {
		return int64(max(500, 100*runtime.GOMAXPROCS(0)))
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

var st244Bucket struct {
	mu     sync.Mutex
	tokens float64
	last   int64 // unix nanos of the last refill
}

var st244Stat struct {
	passed, deferred, interactive atomic.Uint64
}

// st246: interactive flows skip the pacing. callUp covers a call over
// mDupMaxRate (384 kbit/s) one way; a low-rate voice call (Opus 16-32 kbit/s)
// or a game (20-60 small packets a second) takes lite's small rule instead
// and would wait for a token like an idle storm connection. Such a flow moves
// bytes both ways all the time, so the quieter direction of the last lite
// sample (mLiteAfter, 1 s) at HS2_ST246_INTERACTIVE_BPS bytes/s or more
// (default 1000 = 8 kbit/s; 0 = off) upgrades at once. A storm's new
// connections (one request, then quiet) and the crowd's request/response
// connections stay under it and are still paced (lab: counter "interactive").
var st246InteractiveBps atomic.Int64

func init() { st246InteractiveBps.Store(int64(envIntDefault("HS2_ST246_INTERACTIVE_BPS", 1000))) }

// st246Interactive: true (and counted) when the pacing is on and lo, the
// quieter direction of the connection's last sample in bytes/s, is at the
// interactive floor.
func st246Interactive(lo float64) bool {
	if st244Rate.Load() <= 0 {
		return false
	}
	f := st246InteractiveBps.Load()
	if f <= 0 || lo < float64(f) {
		return false
	}
	st244Stat.interactive.Add(1)
	return true
}

// st244UpgradeToken takes one upgrade token (true) or reports the bucket
// empty (false). Always true when the key is off.
func st244UpgradeToken(now time.Time) bool {
	r := st244Rate.Load()
	if r <= 0 {
		return true
	}
	burst := float64(r) / 10
	if burst < 1 {
		burst = 1
	}
	b := &st244Bucket
	t := monoNano(now) // rel69h: monotonic (TestNoWallClockNanosInEngine)
	b.mu.Lock()
	if b.last == 0 {
		b.tokens = burst
	} else if d := t - b.last; d > 0 {
		b.tokens += float64(r) * float64(d) / 1e9
		if b.tokens > burst {
			b.tokens = burst
		}
	}
	b.last = t
	ok := b.tokens >= 1
	if ok {
		b.tokens--
	}
	b.mu.Unlock()
	if ok {
		st244Stat.passed.Add(1)
	} else {
		st244Stat.deferred.Add(1)
	}
	return ok
}

func st244LogPart() string {
	if st244Rate.Load() <= 0 {
		return ""
	}
	return fmt.Sprintf(" · st244 upg rate %d passed %d deferred %d interactive %d", st244Rate.Load(),
		st244Stat.passed.Load(), st244Stat.deferred.Load(), st244Stat.interactive.Load())
}
