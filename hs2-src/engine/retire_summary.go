package engine

import (
	"strings"
	"sync"
	"time"
)

// retireSum (b271 Q6): the "link(s) N retiring" / "back in service" lines
// (prod: ~40 per 10 min, intentional pool trimming) folded into one summary
// line a minute. The first retireSumLines events of each minute are logged
// verbatim as before; the rest are counted and summed up at the minute's
// end with the pool's latest state. Logging only: the retire logic is
// untouched.
const (
	retireSumLines = 3
	retireSumWin   = time.Minute
)

type retireSum struct {
	logf func(string, ...any)

	mu        sync.Mutex
	winStart  time.Time
	n         int // events in this window, logged or not
	backEv    int // folded events
	goneEv    int
	backLinks int // links in the folded events
	goneLinks int
	up, s, r  int // the latest folded event's pool state
	flushing  bool
	afterFunc func(time.Duration, func()) // tests replace it
}

func newRetireSum(logf func(string, ...any)) *retireSum {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &retireSum{logf: logf, afterFunc: func(d time.Duration, f func()) { time.AfterFunc(d, f) }}
}

// note logs one tick's back/retiring links (either may be empty), or folds
// them into the minute's summary.
func (b *retireSum) note(now time.Time, back, gone []string, up, S, R int) {
	if b == nil || len(back) == 0 && len(gone) == 0 { // nil: a test's bare LinkManager{}
		return
	}
	b.mu.Lock()
	if now.Sub(b.winStart) >= retireSumWin {
		b.winStart, b.n = now, 0
	}
	b.n++
	if b.n <= retireSumLines {
		b.mu.Unlock()
		if len(back) > 0 {
			b.logf("mtcp: link(s) %s back in service (%d up: %d serving, %d retiring)", strings.Join(back, ","), up, S, R)
		}
		if len(gone) > 0 {
			b.logf("mtcp: link(s) %s retiring — no new connections; each closes once its connections end (%d up: %d serving, %d retiring)",
				strings.Join(gone, ","), up, S, R)
		}
		return
	}
	if len(back) > 0 {
		b.backEv++
		b.backLinks += len(back)
	}
	if len(gone) > 0 {
		b.goneEv++
		b.goneLinks += len(gone)
	}
	b.up, b.s, b.r = up, S, R
	if !b.flushing {
		b.flushing = true
		b.afterFunc(b.winStart.Add(retireSumWin).Sub(now), b.flush)
	}
	b.mu.Unlock()
}

// flush writes the summary of the folded events.
func (b *retireSum) flush() {
	b.mu.Lock()
	be, ge, bl, gl, up, S, R := b.backEv, b.goneEv, b.backLinks, b.goneLinks, b.up, b.s, b.r
	b.backEv, b.goneEv, b.backLinks, b.goneLinks, b.flushing = 0, 0, 0, 0, false
	b.mu.Unlock()
	if be+ge > 0 {
		b.logf("mtcp: retire summary (last %s, beyond the first %d events): %d link(s) back in service in %d event(s), %d link(s) retiring in %d event(s) — now %d up: %d serving, %d retiring",
			fmtDur(retireSumWin), retireSumLines, bl, be, gl, ge, up, S, R)
	}
}
