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
//   - it moves under stuckMoveFloor, or under half of what those links move
//     (median): a link whose users wait while it moves its share is waiting
//     on its own load — the path is full — not on a throttle;
//   - the path is not slow: it is when two or more links wait like that
//     (stuckWait+, moving under activeBytes) and they outnumber the links
//     that answer promptly — the path or the other server, slow or down for
//     most, and moving users between links would not help. Then none is
//     drained (said once a minute), no link is judged for loss either, nor
//     for as long again after, as long as the spell was slow all told
//     (stuckRecoverFor): links that waited through it recover on their own.
//     Links answering in 2-6 s, heavy ones waiting behind their own load and
//     ones that never answer (an older exit) do not count either way:
//     counted as slow, a few lossy or heavy links held off both rules for
//     good; and counting who answers keeps a minority of stuck links (4 of
//     10 at night, say) from passing for a slow path.
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
// a verdict; one that trickles is judged like a throttled path. And in a
// squeeze that drops rather than queues, light links that keep answering
// can outnumber heavy ones backing off: those are then judged one by one
// (at most drainHeadroom at a time).
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
	// judged for as long as that lasted, this long at least and
	// stuckRecoverMax at most — TCP backed off through it, each retry twice
	// as late as the one before, so it resumes up to that long after (and
	// never more than TCP_RTO_MAX, 2 min, apart).
	stuckRecover    = 30 * time.Second
	stuckRecoverMax = 2 * time.Minute
	// stuckMoveFloor: a link moving less than this per healthTick (6 KB/s,
	// about four full packets a second) while its users wait stuckWait is
	// throttled whatever the others move — the load test's "stuck" is 3
	// packets/s each way; above it, it must move less than half of what the
	// links that answer promptly move.
	stuckMoveFloor = 12 << 10
)

// stuckRecoverFor is how long after the last slow tick no link is judged:
// as long as the spell was slow, all told (one-tick blips add a tick each,
// not the time between them). Caller holds m.mu.
func (m *LinkManager) stuckRecoverFor() time.Duration {
	return min(max(stuckRecover, m.stuckSlowFor), stuckRecoverMax)
}

// lossObs is a link the loss rule found bad degradeStreak samples running,
// judged after the loop: not if this very tick turns out slow.
type lossObs struct {
	ml                   *managedLink
	dUp, dWr, dDown, dRd uint64
}

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
