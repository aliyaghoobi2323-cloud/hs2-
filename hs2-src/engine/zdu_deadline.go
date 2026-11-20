package engine

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// The drain deadline of a live handover (zdu350 §3 B6 and §7 slot S2).
//
// The Kharej xray restarts every hour at about HH:01 UTC on purpose (WG IP
// rotation), which drops every user connection anyway. So the old process
// drains until just after that drop: the next HH:01 UTC + 2 min = HH:03 UTC.
// A drain shorter than ZDUDrainMin (we are at HH:58-HH:59) takes the
// following hour instead. Nothing ever drains longer than ZDUDrainCap.
//
// A handover is refused in HH:00:00-HH:03:59 UTC: the restart storm is in
// progress and the new process would start under it.
//
// Pure functions: the caller passes the clock. All arithmetic is in UTC, so a
// local zone (Asia/Tehran +03:30, a DST jump) never moves the deadline.

const (
	ZDUDrainCap = 65 * time.Minute // longest drain, also the cap of the HS2_ZDU_DRAIN override
	ZDUDrainMin = 5 * time.Minute  // the HH:03 target must be at least this far away
	zduAnchor   = 3 * time.Minute  // HH:01 (xray restart) + 2 min
	zduRefuseTo = 4 * time.Minute  // refusal window is [HH:00:00, HH:04:00)
)

// ErrZDUWindow: a handover was asked for inside HH:00:00-HH:03:59 UTC.
var ErrZDUWindow = errors.New("zdu: no handover between HH:00 and HH:03 UTC (hourly xray restart)")

// ZDURefused reports whether a handover starting at now falls into the
// HH:00:00-HH:03:59 UTC refusal window.
func ZDURefused(now time.Time) bool {
	u := now.UTC()
	off := u.Sub(u.Truncate(time.Hour)) - zduStormShift()
	if off < 0 {
		off += time.Hour
	}
	return off < zduRefuseTo
}

// The xray restart is not a calendar timer: Kharej xray-loop.service sleeps
// 3600 s between restarts, so the minute it lands on is the phase of that
// loop (seen at HH:01 UTC), not a time-zone setting. HS2_ZDU_STORM_MIN
// (0-59, default 1, UTC minute of the restart; whole-hour zones give the
// same minute) moves the refusal window [M-1, M+3) and the drain anchor M+2
// with it when the loop's phase moves. Read once at start.
var zduStormMin = func() int {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("HS2_ZDU_STORM_MIN"))); err == nil && n >= 0 && n < 60 {
		return n
	}
	return 1
}()

// zduStormShift: how far the window start (M-1) is from HH:00 (0 by default).
func zduStormShift() time.Duration {
	return time.Duration((zduStormMin+59)%60) * time.Minute
}

// ZDUStormMin is the UTC minute of the hourly xray restart in use.
func ZDUStormMin() int { return zduStormMin }

// ZDURefusedSpan reports whether a handover starting at now would still be
// before its commit when the refusal window opens: any instant of [now,
// now+span] inside the window (review minor: a SIGUSR2 at HH:58 ran READY
// plus probation into the HH:01 storm). The window is 4 min wide, so a 30 s
// step cannot miss it.
func ZDURefusedSpan(now time.Time, span time.Duration) bool {
	for d := time.Duration(0); d < span; d += 30 * time.Second {
		if ZDURefused(now.Add(d)) {
			return true
		}
	}
	return ZDURefused(now.Add(span))
}

// ZDUDeadline returns when the old process must stop draining, for a
// handover that starts at now. override > 0 (HS2_ZDU_DRAIN) replaces the
// HH:03 rule with now+override, still capped at ZDUDrainCap. The result is in
// UTC. err is ErrZDUWindow inside the refusal window (deadline zero).
func ZDUDeadline(now time.Time, override time.Duration) (time.Time, error) {
	if ZDURefused(now) {
		return time.Time{}, ErrZDUWindow
	}
	u := now.UTC()
	limit := u.Add(ZDUDrainCap)
	if override > 0 {
		d := u.Add(override)
		if d.After(limit) {
			d = limit
		}
		return d, nil
	}
	// time.Truncate works on the absolute time (zone-free), and UTC hours
	// are whole multiples of an hour since the zero time.
	d := u.Truncate(time.Hour).Add(zduAnchor + zduStormShift())
	for !d.After(u) || d.Sub(u) < ZDUDrainMin {
		d = d.Add(time.Hour)
	}
	if d.After(limit) {
		d = limit
	}
	return d, nil
}

// ParseZDUDrain reads HS2_ZDU_DRAIN: a Go duration ("20m", "90s") or whole
// seconds ("1200"). "" or "0" = no override. Negative or bad values are an
// error (the caller then keeps the HH:03 rule and logs it).
func ParseZDUDrain(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return 0, nil
	}
	if n, err := strconv.Atoi(s); err == nil {
		if n < 0 {
			return 0, fmt.Errorf("HS2_ZDU_DRAIN %q: negative", s)
		}
		return time.Duration(n) * time.Second, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("HS2_ZDU_DRAIN %q: %v", s, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("HS2_ZDU_DRAIN %q: negative", s)
	}
	return d, nil
}
