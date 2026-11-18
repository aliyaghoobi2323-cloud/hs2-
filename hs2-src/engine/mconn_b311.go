package engine

import (
	"log"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// b311 (p311 heavy A/B): HS2_B311_HEAVYSYNC (default on; only with
// HS2_B307_ASYNCCLOSE on).
//
// b307's async close let every closer go on before its sub's smux
// Stream.Close had queued the FIN. For a heavy connection (one that ever
// striped, or moved to bulk) that runs the sub's death bookkeeping —
// subDied/subGone, the stripe count, the growth rule's next open, rescue
// resends — ahead of the old stream, which is still in the link's session
// and still being fed by the peer until the FIN reaches it. With the key
// on, a heavy connection's subs close as before b307 (the closer waits for
// the stream's close); light connections, the many whose closes piled up in
// the Once at 3-4x, keep the async close.
//
// HS2_B311_TRACE=1: one log line per heavy-connection sub close (caller,
// how long the stream's close took, async or not), to see the mechanism.
var (
	b311HeavySync atomic.Bool
	b311Trace     atomic.Bool
	b311TraceAll  atomic.Bool // HS2_B311_TRACE=all: every sub close, light ones too
)

func init() {
	b311HeavySync.Store(envBoolDefault("HS2_B311_HEAVYSYNC", true))
	b311Trace.Store(envBoolDefault("HS2_B311_TRACE", false) || os.Getenv("HS2_B311_TRACE") == "all")
	b311TraceAll.Store(os.Getenv("HS2_B311_TRACE") == "all")
}

var b311Stat struct {
	heavySync atomic.Int64 // heavy-connection sub closes kept synchronous
}

// b311Heavy marks m heavy (it striped, or moved to bulk): from now its
// subs' closes wait for their streams (b311HeavySync).
func (m *mConn) b311MarkHeavy() { m.b311Heavy.Store(true) }

// b311SyncClose: s's close must wait for its stream's close.
func (s *msub) b311SyncClose() bool {
	if !b307AsyncClose.Load() {
		return true
	}
	if b311HeavySync.Load() && s.m != nil && s.m.b311Heavy.Load() {
		b311Stat.heavySync.Add(1)
		return true
	}
	return false
}

// b311TraceStart: the start of a traced close (zero: not traced).
func (s *msub) b311TraceStart() (time.Time, string) {
	if !b311Trace.Load() || s.m == nil || (!s.m.b311Heavy.Load() && !b311TraceAll.Load()) {
		return time.Time{}, ""
	}
	caller := "?"
	if pc, _, _, ok := runtime.Caller(2); ok {
		if f := runtime.FuncForPC(pc); f != nil {
			caller = f.Name()
			if i := strings.LastIndex(caller, "."); i >= 0 {
				caller = caller[i+1:]
			}
		}
	}
	return time.Now(), caller
}

func (s *msub) b311TraceEnd(t0 time.Time, caller string, async bool) {
	if t0.IsZero() {
		return
	}
	log.Printf("b311 close %x iran=%v sub %d heavy=%v bulk=%v by=%s async=%v took=%v",
		s.m.id&0xffffff, s.m.iran, s.idx, s.m.b311Heavy.Load(), s.bulk.Load(), caller, async, time.Since(t0).Round(100*time.Microsecond))
}

// b311CloseLine: " b311: heavysync=N" on the mem line.
func b311CloseLine() string {
	if n := b311Stat.heavySync.Load(); n > 0 {
		return " b311: heavysync=" + strconv.FormatInt(n, 10)
	}
	return ""
}
