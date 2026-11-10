package engine

import (
	"os"
	"time"
)

// HS2_FAST_LINK=1: build new links faster (default off).
//
// What a link costs from "the pool wants one" to "it carries users", in the
// production reverse mode (the kharej dials the iran edge), at RTT r:
//
//	pause    the exit slot waits before it redials a link that ended:
//	         jitter(backoff), backoff doubling 0.5 → 8 s for every link that
//	         lived < 30 s — at peak, when links die every few tens of seconds,
//	         most slots sit at 2–8 s
//	gate     a turn from the process's dial gate: 8 handshakes at a time,
//	         starts 40–160 ms apart (~10 a second), so after a mass death the
//	         k-th lost link waits ~k/10 s
//	TCP      1 r
//	TLS 1.3  1 r (the edge has the client's Finished + auth record at 2.5 r)
//	kindInfo 1 r: the edge lets a link carry users only once it has the
//	         exit's answer on THAT link, even when the pool's other links
//	         already hold the same exit's answer
//
// With the key on:
//
//   - pause: a link that lived fastStableAfter (5 s) is redialed at once (the
//     gate still spaces the starts) and resets the slot's backoff; only a link
//     that died sooner (a path that kills links right after the handshake)
//     doubles it, as before.
//   - gate: 16 at a time, starts 20–80 ms apart (~20 a second): a mass death
//     of tens of links is rebuilt in about half the time. Still one gate per
//     process, still jittered spacing, never one burst.
//   - kindInfo: a new link carries users at once when the pool already holds
//     the exit's answer (LinkManager.exitInfo — the same exit process: it is
//     cleared when the last link goes); its own exchange runs on as before.
//     One round trip less per link, and the refill hold's waiters get it
//     sooner.
//
// Unchanged: the TLS fingerprint and SNI (uTLS Chrome preset, no session
// resumption: a resumed hello would carry a pre_shared_key the preset does
// not, and in TLS 1.3 resumption saves no round trip anyway), the auth and
// its channel binding, and the wire: no new cap, so either side may run with
// or without the key against any older peer.
var fastLink bool // loadEnvKeys

// Sub-keys, for telling the three parts apart in the lab: with HS2_FAST_LINK=1,
// HS2_FAST_LINK_PAUSE=0 / _GATE=0 / _INFO=0 turns that one part back off.
var (
	fastPauseOn = os.Getenv("HS2_FAST_LINK_PAUSE") != "0"
	fastGateOn  = os.Getenv("HS2_FAST_LINK_GATE") != "0"
	fastInfoOn  = os.Getenv("HS2_FAST_LINK_INFO") != "0"
)

func fastPause() bool { return fastLink && fastPauseOn }
func fastGate() bool  { return fastLink && fastGateOn }
func fastInfo() bool  { return fastLink && fastInfoOn }

// fastStableAfter: with HS2_FAST_LINK, a link that lived this long is
// redialed without a pause and resets its slot's backoff.
const fastStableAfter = 5 * time.Second

// slotRedialPause is how long an exit slot waits before it redials after its
// link ended, having lived `lived`, and updates the slot's backoff.
func slotRedialPause(lived time.Duration, backoff *time.Duration) time.Duration {
	if fastPause() && lived >= fastStableAfter {
		*backoff = slotBackoffMin
		return 0
	}
	if !fastPause() && lived >= slotStableAfter {
		*backoff = slotBackoffMin
	} else if *backoff < slotBackoffMax {
		*backoff *= 2
	}
	return jitterDur(*backoff)
}

// infoReadyLocked reports whether ml may carry user connections as far as the
// kindInfo exchange goes (LinkManager.gateInfo). Caller holds m.mu (read).
func (m *LinkManager) infoReadyLocked(ml *managedLink) bool {
	if !m.gateInfo || ml.mtr == nil || ml.mtr.infoDone.Load() {
		return true
	}
	return fastInfo() && m.exitInfo.Load() != nil
}
