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
// while it moves less than activeBytes, for stuckStreak samples in a row,
// and the path is shown to work for the others:
//
//   - another link carrying traffic answers promptly now (its last round trip
//     and its current wait both under stuckPrompt) and got an answer to a
//     ping sent after this link's oldest one went out — a whole round trip
//     that started later (a pong that merely arrives later may have left
//     before an outage; an idle link answers quickly through any squeeze,
//     having nothing queued);
//   - it moves less than half of what those links move (median), or under
//     ctrlBusyBytes: a link whose users wait while it moves its share is
//     waiting on its own load — the path is full — not on a throttle;
//   - at least half the busy links answer promptly. Fewer, and it is the path
//     or the other server, slow or down for most: moving users between links
//     would not help, so none is drained (said once a minute), nor for
//     stuckRecover after — links that waited through it recover on their own.
//     Counting the links that answer, not those that wait, keeps a minority
//     of stuck links (4 of 10 at night, say) from passing for a slow path.
//
// At most drainHeadroom stuck links drain at a time, the longest waits first.
// A link whose own reader was parked on a full receive buffer lately
// (sessGuard.parkedAt) waits on its own users, not its path: the wedge guard
// ends those users, and the link keeps the rest (in the load test a link
// wedged by apps that stopped reading was taken for stuck 1.7 s after the
// guard had freed it). A suspect link (nothing received at all) is left to
// that rule. A stuck link is degraded at once (no new users, its replacement
// comes), and its connections that moved no data for drainStall close
// without the maxDrain wait.
//
// Not seen from here: a wedge on the exit's side (its reader parked by a slow
// target). The exit's guard frees readers that stopped within seconds, before
// a verdict; one that trickles is judged like a throttled path.
const (
	stuckWait   = 6 * time.Second
	stuckStreak = 2
	// stuckPrompt: a link counts as evidence that the path works only if its
	// last control round trip took less than this and none of its pings waits
	// longer — under load a healthy link answers in 0.1-0.6 s; in a path-wide
	// squeeze (the load test's 8 Mbit/s for everyone) every link answers in
	// seconds, and one that has just been answered is no proof.
	stuckPrompt = stuckWait / 3
	// stuckRecover: after the path was slow for most links, no link is
	// judged for this long — TCP backed off through it and resumes a little
	// after (in the load test, 27 links were drained just after a squeeze
	// without it).
	stuckRecover = 30 * time.Second
)

// stuckObs is a link found stuck in this sample, for the log line.
type stuckObs struct {
	ml      *managedLink
	wait    time.Duration
	moved   uint64  // bytes both ways in the sample
	perTick float64 // ... per healthTick
	sent    int64   // ctrlNow when its oldest unanswered ping went out
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
