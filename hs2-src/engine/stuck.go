package engine

import (
	"fmt"
	"time"
)

// A stuck link: a path throttled to a few packets a second, the way DPI
// slows a flow it does not like, without cutting it. It still delivers a
// keepalive now and then, so it is never "suspect" (nothing received for
// suspectAfter), and it moves too little for the loss rule to judge it
// (activeBytes), so it was never "degraded" either: it kept serving, took
// new users — it looked the least loaded — and every one of them waited
// tens of seconds per reply, until the users gave up or TCP reset it. In the
// load test 2–6 of 10 such links served until the end.
//
// The control ping travels behind the link's own traffic both ways, so how
// long the oldest unanswered one has waited (ctrlWaitOf) is how long the
// link's users wait. A link is stuck when that wait is stuckWait or more
// while it moves less than activeBytes, for stuckStreak samples in a row —
// and the path is shown to work: another link carrying traffic answers
// promptly now, and got an answer to a ping sent after this link's oldest
// one went out (a whole round trip that started later; a pong that merely
// arrives later may have left before an outage, and an idle link answers
// quickly through any squeeze, having nothing queued). When every busy link
// waits, or more than a third of them at once, it is the path or the other
// server, down or slow for all: moving users between links would not help,
// so none is drained (said once a minute). At most drainHeadroom are drained
// per tick, the longest waits first. And a link whose own reader was parked on
// a full receive buffer lately (sessGuard.parkedAt) waits on its own users, not
// its path: the wedge guard ends those users, and the link keeps the rest (in
// the load test a link wedged by apps that stopped reading was taken for
// stuck 1.7 s after the guard had freed it). A stuck link is degraded at once (no new
// users, its replacement comes), and its connections that moved no data for
// drainStall close without the maxDrain wait.
const (
	stuckWait   = 6 * time.Second
	stuckStreak = 2
)

// stuckObs is a link found stuck in this sample, for the log line.
type stuckObs struct {
	ml    *managedLink
	wait  time.Duration
	moved uint64 // bytes both ways in the sample
	sent  int64  // ctrlNow when its oldest unanswered ping went out
}

// fmtBytes says a small byte count the way the stuck line wants it.
func fmtBytes(n uint64) string {
	switch {
	case n < 1<<10:
		return fmt.Sprintf("%d B", n)
	case n < 1<<20:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	}
}
