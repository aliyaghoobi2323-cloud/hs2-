package engine

import (
	"fmt"
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
var stripeShare bool // loadEnvKeys

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
	ml, _ := m.pickStripeSharedWhyLocked(avoid, pool)
	return ml
}

// stripeShareWhy (diagnostics, diag60): why the shared pick found no link —
// each serving link counted once, under the first rule that left it out.
// Production (5000 users, 70 serving links, every link with a user) logged
// "stripe pick: none (free=1..6 chosen=false)" 150+ times in 5 single
// uploads, and 8 of the 10 uploads never got a stripe sub; the line did not
// say which rule held the shared pick.
type stripeShareWhy struct {
	serving, busy, info, pressed, avoided, copies, call, lane int
	cand, own, total                                          int
	copyOK                                                    int // candidates with copies (SHARE_COPIES)
	laneBusy                                                  int // candidates the lane used lately (SHARE_COPIES_LANEQ)
	dlPrim                                                    int // fx212: copies' links left out for a light primary (download)
	// dlp217 (DL_PRIM=2): copies' links with idle primaries (tier 1), with
	// hot ones (tier 2), hot ones refused (HOTK spent), and this
	// connection's shared subs already on hot links
	dlQuiet, dlHot, dlHotRef, ownHot int
	tier                             int  // dlp217: the picked link's tier (dlV2)
	picked                           bool // a link was returned
	half, kCap, totalCap             bool
}

func (w stripeShareWhy) String() string {
	s := fmt.Sprintf("serving=%d busy=%d cand=%d skip[info=%d pressed=%d avoid=%d copies=%d call=%d lane=%d] copy-ok=%d lane-busy=%d own=%d total=%d dlprim=%d",
		w.serving, w.busy, w.cand, w.info, w.pressed, w.avoided, w.copies, w.call, w.lane, w.copyOK, w.laneBusy, w.own, w.total, w.dlPrim)
	if w.dlQuiet+w.dlHot+w.dlHotRef+w.ownHot > 0 {
		s += fmt.Sprintf(" dlq=%d dlhot=%d dlhotref=%d ownhot=%d", w.dlQuiet, w.dlHot, w.dlHotRef, w.ownHot)
	}
	switch {
	case w.kCap:
		s += " refused=share-k"
	case w.totalCap:
		s += " refused=share-total"
	case w.cand == 0:
		s += " refused=no-candidate"
	case w.half:
		s += " refused=half-heavy"
	}
	return s
}

// pickStripeSharedWhyLocked: pickStripeSharedLocked and, when it gives no
// link, why (stripeShareWhy).
func (m *LinkManager) pickStripeSharedWhyLocked(avoid []any, pool *peerInfo) (*managedLink, stripeShareWhy) {
	var w stripeShareWhy
	now := ctrlNow()
	var best *managedLink
	bestC := 0 // best's copy-only streams (0 unless SHARE_COPIES)
	// share62: SHARE_COPIES serves an upload's open only (stripeCopyUpHint)
	shareCopies := stripeShareCopies.Load() && stripeCopyUp(avoid)
	// fx212: a download's copies' links must carry no light primary
	dlPrim := shareCopies && fx212DlPrim && stripeCopyDl(avoid)
	dlV2 := dlPrim && fx212DlPrimMode == 2 // dlp217: grade, not refuse
	bestCut := false                       // best on a cut path (HS2_LB_STRIPE_STEER)
	bestLB := false                        // best's lane busy (HS2_MCONN_STRIPE_SHARE_COPIES_LANEQ)
	bestT, bestH := 0, 0                   // dlp217: best's tier and hot primaries (dlV2)
	for _, ml := range m.links {
		if n := ml.stShared.Load(); n > 0 {
			w.total += int(n) // every connection's shared subs
			if avoided(ml.link, avoid) || stripeEvacSkip(ml.link) {
				w.own++ // this connection's shared sub (avoid: its links)
				if ml.stDlHot.Load() > 0 {
					w.ownHot++ // dlp217: placed on a hot link (tier 2)
				}
			}
		}
	}
	for _, ml := range m.links {
		if !ml.serving() {
			continue
		}
		w.serving++
		if ml.heavy > 0 || stripeOn(ml) {
			w.busy++
			continue
		}
		if m.gateInfo && ml.mtr != nil && !ml.mtr.infoDone.Load() || !linkMConn(ml, pool) {
			w.info++
			continue
		}
		switch {
		case ml.pressed:
			w.pressed++
			continue
		case avoided(ml.link, avoid):
			w.avoided++
			continue
		}
		nc, tier, hot := 0, 0, 0
		if c := stripeLinkCopies(ml.link); c > 0 {
			if !shareCopies {
				w.copies++
				continue
			}
			if dlV2 {
				// dlp217: idle primaries rank after none, hot ones last and
				// at most fx212DlPrimHotK per connection
				prims, h := stripeLinkPrimHot(ml.link, fx212DlPrimQuiet)
				fx212DlPrimStat.scanned.Add(1)
				switch {
				case prims == 0:
				case h == 0:
					tier = 1
					w.dlQuiet++
				case w.ownHot >= fx212DlPrimHotK:
					w.copies++
					w.dlHotRef++
					fx212DlPrimStat.hotRef.Add(1)
					continue
				default:
					tier, hot = 2, h
					w.dlHot++
				}
			} else if dlPrim && stripeLinkPrims(ml.link) > 0 {
				w.copies++ // fx212: a download's stripe never joins a light primary on a copies' link
				w.dlPrim++
				continue
			}
			nc = c // HS2_MCONN_STRIPE_SHARE_COPIES (mconn_stripe_sharecopy.go)
			w.copyOK++
		}
		if (stripeCallFree || shareCopies) && callLinkOn(ml.mtr, time.Now()) {
			w.call++
			continue // HS2_MCONN_STRIPE_CALLFREE / SHARE_COPIES: a call rides it
		}
		if stripeShareLane && laneAtOf(ml) != 0 && now-laneAtOf(ml) < int64(stripeLaneQuiet) {
			w.lane++
			continue // the TUN lane's interactive flows ride it
		}
		cut := m.stripeCutLocked(ml) // HS2_LB_STRIPE_STEER (mconn_stripe_steer.go)
		if cut && m.stSteerNoCut {
			continue
		}
		w.cand++
		lb := shareCopies && stripeShareCopiesLaneQ && laneAtOf(ml) != 0 && now-laneAtOf(ml) < int64(stripeLaneQuiet)
		if lb {
			w.laneBusy++ // share62: a game's or a call's lane rides it, the others first
		}
		// dlp217: the tier (0 without dlV2) and the hot primaries rank
		// before the old order
		tb := tier < bestT || tier == bestT && hot < bestH
		te := tier == bestT && hot == bestH
		if best == nil || !cut && bestCut || cut == bestCut && (!lb && bestLB || lb == bestLB && (tb || te && (ml.flowing < best.flowing || ml.flowing == best.flowing && (nc < bestC || nc == bestC && (ml.rate10 < best.rate10 || ml.rate10 == best.rate10 && ml.users.Load() < best.users.Load()))))) {
			best, bestC, bestLB, bestCut, bestT, bestH = ml, nc, lb, cut, tier, hot
		}
	}
	w.tier = bestT
	if dlV2 {
		defer func() { fx212DlPrimNote(w) }()
	}
	if stripeShareK > 0 && w.own >= stripeShareK {
		w.kCap = true
		return nil, w
	}
	if best != nil && bestC > 0 && stripeShareCopiesK > 0 && w.own >= stripeShareCopiesK {
		w.kCap = true // HS2_MCONN_STRIPE_SHARE_COPIES_K: fewer subs on the copies' links
		return nil, w
	}
	if stripeShareTotal > 0 && w.total >= stripeShareTotal {
		w.totalCap = true
		return nil, w // the pool's budget is spent (several stripes share it)
	}
	if best == nil {
		return nil, w
	}
	if 2*(w.busy+1) > w.serving {
		w.half = true
		return nil, w
	}
	if shareCopies {
		stripeShareCopiesNote(best, bestC, bestLB, w) // diagnostics (share62)
	}
	w.picked = true
	return best, w
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
