package engine

import "time"

// Clock-step safe timestamps for atomics.
//
// Many hot-path fields keep "when" as an int64 in an atomic (last record read,
// a mute or retire start, a log limiter). They used to hold UnixNano — the
// wall clock — so an NTP step (or an operator's `date -s`) moved every one of
// them at once: a step back of an hour kept a dgtun link muted, a retire
// pending and every log limiter silent for that hour; a step forward made
// every sub look idle at once. monoNano encodes a time from the process's
// monotonic clock instead (it reads like UnixNano at start-up and then only
// moves forward with real elapsed time), and monoTime turns such a value back
// into a time.Time that carries the monotonic reading, so now.Sub(monoTime(x))
// and monoNano(now)-x measure real elapsed time whatever the wall clock does.
//
// A time without a monotonic reading (a test's fake clock built with
// time.Unix) encodes as its UnixNano, so fake clocks keep their meaning.

var (
	monoBase     = time.Now()
	monoBaseNano = monoBase.UnixNano()
)

// monoNano: t as clock-step safe nanoseconds (see the file comment).
func monoNano(t time.Time) int64 {
	if t == t.Round(0) { // no monotonic reading: a fake clock
		return t.UnixNano()
	}
	return monoBaseNano + int64(t.Sub(monoBase))
}

// monoTime: the time.Time of a monoNano value (with a monotonic reading).
func monoTime(ns int64) time.Time {
	return monoBase.Add(time.Duration(ns - monoBaseNano))
}
