package engine

import (
	"os"
	"strconv"
	"time"
)

// HS2_MCONN_STRIPE_SHARE=1 (Iran; with HS2_MCONN_STRIPE): the stripe in a
// busy pool. Production (8-16k users over 100-150 links) has almost no link
// without a user, and the pool is at max_links, so the stripe's rules found
// no link (pickStripe: "none (free=29 chosen=false)" in the crowded lab, 0
// picks in 40 s) and asked the pool for one it could not add; the subs it did
// get went unused under the gate for a moment and lite's idle rule closed
// them 10 s later (cause=EOF retired=true), freeing the link to start over.
// The download stayed on 1-2 subs. With the key:
//
//   - a link for a stripe sub: when no empty link (or one set aside) is
//     there, a lightly loaded one — serving, no download on it (heavy == 0),
//     not pressed, no stripe already, no call copy (HS2_MCONN_CALL's copies
//     stay off it) — the lowest rate first, then the fewest users. Its light
//     users keep their sub; from then on the link counts as heavy, so new
//     light placements, the TUN lane and the call copies avoid it, and
//     lite's dual copy of a proven-light connection already has a second
//     link. At most half of the serving links may be heavy or stripe after
//     the pick: the light users keep at least half the pool.
//   - while the striped connection is still fast (its last sample at least
//     stripeWarmRate), its stripe subs are kept: Iran's idle rule skips them,
//     and Iran sends a STRIPE record (the peer ignores a repeat) on a sub
//     that has been quiet for stripeWarmEvery, so the sender's "cold for
//     mLiteIdle/2" rule does not retire it either. The scavenger's gate
//     decides, as before, how many of them carry data; a sub left out is
//     there again the moment the gate widens instead of being re-opened on
//     a link it may not find. The pool's shrink keeps a stripe link until
//     its sub is gone (reconcile).
//
// The light users on a shared link pay for the stripe (the link's queue and
// loss recovery), so a shared sub is bounded (the lab with every link
// crowded: 14-22 subs per connection raised the light users' p99):
//
//   - at most stripeShareK (HS2_MCONN_STRIPE_SHARE_K, default 8; 0: no
//     bound) shared subs per connection; STRIPE_UP's growth to 32 goes on
//     only on links of the stripe's own;
//   - with HS2_MCONN_STRIPE_SHARE_LANE=1 (off by default): a link the TUN
//     lane sent an interactive packet over (ping, game, call) in the last
//     stripeLaneQuiet is not shared. Off because in a crowded pool the
//     lane's small packets touch nearly every link: the lab's 1 % download
//     then found one shared link instead of 8 (140 Mbit/s vs 550-700);
//   - among the rest, the fewest flowing user streams first, then the
//     lowest rate, then the fewest users.
//
// Off (the default): nothing changes. No capability bit: only Iran decides,
// and the repeated STRIPE record is what every exit with the stripe reads.
var stripeShare = os.Getenv("HS2_MCONN_STRIPE_SHARE") == "1"

// stripeShareLane: HS2_MCONN_STRIPE_SHARE_LANE=1 (see above).
var stripeShareLane = os.Getenv("HS2_MCONN_STRIPE_SHARE_LANE") == "1"

// stripeShareK: shared stripe subs per connection (HS2_MCONN_STRIPE_SHARE_K).
var stripeShareK = func() int {
	if v, err := strconv.Atoi(os.Getenv("HS2_MCONN_STRIPE_SHARE_K")); err == nil && v >= 0 {
		return v
	}
	return 8
}()

// stripeShareTotal: shared stripe subs of the whole pool, every connection
// together (HS2_MCONN_STRIPE_SHARE_TOTAL; 0, the default: no bound). The
// per-connection bound alone let 4 parallel downloads take 4×8 light links
// (the lab, down4 at 1 % loss: crowd p95 +9 ms, game p99 +21 ms); a pool
// budget keeps the light users' share the same however many stripes run.
var stripeShareTotal = func() int {
	if v, err := strconv.Atoi(os.Getenv("HS2_MCONN_STRIPE_SHARE_TOTAL")); err == nil && v >= 0 {
		return v
	}
	return 0
}()

const (
	// stripeWarmRate: the connection's rate (bytes/s, its faster direction)
	// that keeps its stripe subs (20 Mbit/s).
	stripeWarmRate = 20e6 / 8
	// stripeWarmEvery: a quiet stripe sub gets a STRIPE record this often.
	stripeWarmEvery = 2 * time.Second
	// stripeLaneQuiet: a link the TUN lane used this recently is not shared.
	stripeLaneQuiet = 10 * time.Second
)

// pickStripeSharedLocked (under m.mu): the least loaded light link for a
// stripe sub, or nil.
func (m *LinkManager) pickStripeSharedLocked(avoid []any, pool *peerInfo) *managedLink {
	serving, busy, own, total := 0, 0, 0, 0
	now := ctrlNow()
	var best *managedLink
	for _, ml := range m.links {
		if n := ml.stShared.Load(); n > 0 {
			total += int(n) // every connection's shared subs
			if avoided(ml.link, avoid) {
				own++ // this connection's shared sub (avoid: its links)
			}
		}
		if !ml.serving() {
			continue
		}
		serving++
		if ml.heavy > 0 || stripeOn(ml) {
			busy++
			continue
		}
		if m.gateInfo && ml.mtr != nil && !ml.mtr.infoDone.Load() || !linkMConn(ml, pool) {
			continue
		}
		if ml.pressed || avoided(ml.link, avoid) || linkCopies(ml.link) > 0 {
			continue
		}
		if stripeCallFree && callLinkOn(ml.mtr, time.Now()) {
			continue // HS2_MCONN_STRIPE_CALLFREE: a call rides it
		}
		if stripeShareLane && laneAtOf(ml) != 0 && now-laneAtOf(ml) < int64(stripeLaneQuiet) {
			continue // the TUN lane's interactive flows ride it
		}
		if best == nil || ml.flowing < best.flowing || ml.flowing == best.flowing && (ml.rate10 < best.rate10 || ml.rate10 == best.rate10 && ml.users.Load() < best.users.Load()) {
			best = ml
		}
	}
	if stripeShareK > 0 && own >= stripeShareK {
		return nil
	}
	if stripeShareTotal > 0 && total >= stripeShareTotal {
		return nil // the pool's budget is spent (several stripes share it)
	}
	if best == nil || 2*(busy+1) > serving {
		return nil
	}
	return best
}

// stripeNoteRateLocked (Iran, the growth tick): the connection's rate now.
func (m *mConn) stripeNoteRateLocked(now time.Time, rate float64) {
	if stripeShare && rate >= stripeWarmRate {
		m.stWarmUntil = now.Add(mLitePoll * 3)
	}
}

// stripeWarmKeepLocked: Iran's idle rule leaves s open.
func (m *mConn) stripeWarmKeepLocked(s *msub, now time.Time) bool {
	return stripeShare && s.stripe && now.Before(m.stWarmUntil) && !m.ended && !m.closing
}

// stripeWarmLocked (Iran, the growth tick): a STRIPE record on each quiet
// stripe sub of a fast connection, so the sender never finds it cold.
func (m *mConn) stripeWarmLocked(now time.Time) {
	if !stripeShare || !m.iran || m.stN == 0 || !now.Before(m.stWarmUntil) || m.ended || m.closing {
		return
	}
	p := m.primary.Load()
	t := now.Add(-stripeWarmEvery)
	for _, s := range m.subs {
		if !s.stripe || s == p || s.retired || s.dead || len(s.q) > 0 || s.cur != nil {
			continue
		}
		if s.lastWarm.After(t) || s.stPushed.After(t) {
			continue
		}
		s.lastWarm = now
		s.push(getChunk(mRecStripe))
	}
}

// laneAtOf: the TUN lane's last interactive packet on ml (0: none known).
func laneAtOf(ml *managedLink) int64 {
	if ml.mtr == nil {
		return 0
	}
	return ml.mtr.laneAt.Load()
}
