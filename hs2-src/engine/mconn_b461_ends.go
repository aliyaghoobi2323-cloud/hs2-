package engine

import (
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// b461 (always on, counter only, no behaviour change): ends of lite conns
// with an error or an RST, on both sides, so an A/B has a baseline for
// HS2_LITE_HEDGE4's "b460 hedge4: ends/err/rst" (which counts hedged conns
// only, so it is zero in an arm without the hedge). Logged at most every 10 s
// as "b461 ends:"; heavy = moved more than hedge3Bytes either way.
var b461Stat struct {
	ends     atomic.Uint64 // lite conns ended
	errEnds  atomic.Uint64 // ... with an error (not a clean close)
	rstEnds  atomic.Uint64 // ... with an RST
	heavyErr atomic.Uint64 // ... with an error after moving more than hedge3Bytes
}

var b461LogAt atomic.Int64

// b461EndLocked (end, under m.mu).
func (m *mConn) b461EndLocked(err error, rst bool) {
	if !m.lite {
		return
	}
	b461Stat.ends.Add(1)
	if err != nil && err != errMConnClosed {
		b461Stat.errEnds.Add(1)
		b461ErrNote(err, rst)
		if m.b461HeavyLocked() {
			b461Stat.heavyErr.Add(1)
		}
	}
	if rst {
		b461Stat.rstEnds.Add(1)
	}
	b461Log(time.Now())
}

// b461HeavyLocked: moved more than hedge3Bytes either way (no side effect,
// unlike hedge3HeavyLocked).
func (m *mConn) b461HeavyLocked() bool {
	if m.h3Heavy || m.sendOff > hedge3Bytes {
		return true
	}
	r := &m.r
	r.mu.Lock()
	h := r.next > hedge3Bytes
	r.mu.Unlock()
	return h
}

func b461Log(now time.Time) {
	n := monoNano(now)
	at := b461LogAt.Load()
	if n-at < int64(10*time.Second) || !b461LogAt.CompareAndSwap(at, n) {
		return
	}
	log.Printf("b461 ends: lite=%d err=%d rst=%d heavy_err=%d",
		b461Stat.ends.Load(), b461Stat.errEnds.Load(), b461Stat.rstEnds.Load(), b461Stat.heavyErr.Load())
	if s := b461ErrLine(); s != "" {
		log.Printf("b461 enderr: %s", s)
	}
}

// b461ErrKinds: error ends by error text (+"/rst" when an RST was sent);
// at most b461ErrMax kinds, the rest under "other".
var (
	b461ErrMu    sync.Mutex
	b461ErrKinds = map[string]uint64{}
)

const b461ErrMax = 24

func b461ErrNote(err error, rst bool) {
	k := err.Error()
	if len(k) > 48 {
		k = k[:48]
	}
	if rst {
		k += "/rst"
	}
	b461ErrMu.Lock()
	if _, ok := b461ErrKinds[k]; !ok && len(b461ErrKinds) >= b461ErrMax {
		k = "other"
	}
	b461ErrKinds[k]++
	b461ErrMu.Unlock()
}

// b461ErrLine: "kind=n; ..." most first.
func b461ErrLine() string {
	b461ErrMu.Lock()
	type kv struct {
		k string
		n uint64
	}
	l := make([]kv, 0, len(b461ErrKinds))
	for k, n := range b461ErrKinds {
		l = append(l, kv{k, n})
	}
	b461ErrMu.Unlock()
	sort.Slice(l, func(i, j int) bool { return l[i].n > l[j].n || l[i].n == l[j].n && l[i].k < l[j].k })
	var b strings.Builder
	for i, x := range l {
		if i > 0 {
			b.WriteString("; ")
		}
		fmt.Fprintf(&b, "%s=%d", x.k, x.n)
	}
	return b.String()
}
