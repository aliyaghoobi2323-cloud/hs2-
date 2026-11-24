package engine

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// HS2_MCONN_STRIPE_SHARE_COPIES=1 (adopt61, Iran, with HS2_MCONN_STRIPE_SHARE):
// a link that carries lite's copy-only subs may take a shared stripe sub;
// the links a call rides stay off it.
//
// Why: the shared pick (pickStripeSharedWhyLocked) skipped every link with
// a copy-only stream (linkCopies > 0). Those copies are not only the calls'
// (HS2_MCONN_CALL): lite gives EVERY proven-light connection a second,
// copy-only sub on another link. With thousands of light users every link
// carries some, so the rule left no candidate. The production-like lab
// (lab2, crowd 3000 over 40 links, a call, a game, one upload):
//
//	stripe pick: none (free=27 empty=0 chosen=false up=false) share:
//	serving=40 busy=11 cand=0 skip[... copies=29 ...] refused=no-candidate
//
// and production showed the same (8 of 10 single uploads never striped and
// ran raw on one link). The rule was also inverted: a light user's PRIMARY
// sub on a link never blocked the share, its redundant copy did — and a copy
// held behind a stripe's queue costs its user nothing while the primary
// delivers first.
//
// With the key:
//
//   - a copy-only stream no longer keeps a link out of the shared pick;
//     among the candidates the fewest copies go first (after the fewest
//     flowing user streams), so the light users' second copies keep the
//     emptier links;
//
//   - the links a call rides are marked as HS2_MCONN_STRIPE_CALLFREE marks
//     them (callMarkLocked) and the shared pick skips them, so the call's
//     two copies never get a stripe placed on them. Only the pick reads the
//     mark: the stripe sender's CALLFREE rule stays with its own key.
//
//   - a connection may be held to stripeShareCopiesK shared subs on such
//     links (HS2_MCONN_STRIPE_SHARE_COPIES_K; 0, the default: only
//     HS2_MCONN_STRIPE_SHARE_K, 8, bounds them). Lab 2 (adopt61, crowd
//     3000 over 40 links at max, call, game, one upload; mean of p99s
//     against the key off): with 8, upload median 808 -> 1163 Mbit/s, p10
//     705 -> 933 (n=10, p=0.007), crowd +0.9 ms, call +2.2, game +5.1 (one
//     121 ms run; median +1.6), echo +3.1; with 4 (n=6), median 912, call
//     +10.8 ms (p=0.018). Neither met the +3 ms budget: the key stays off.
//     share62 (uploads only, LANEQ; the upload after a real 10 s warm-up):
//     n=20, upload median 920 -> 1193 Mbit/s, p10 590 -> 828 (p=0.016),
//     the light p99s within +2.0 ms (call +2.0, p=0.083); the download and
//     multi-upload runs (n>=6) are still to do: still off (CHANGELOG share62).
//
// Off: nothing changes (the copies rule as before, no call marks).
var stripeShareCopies atomic.Bool

func init() { stripeShareCopies.Store(os.Getenv("HS2_MCONN_STRIPE_SHARE_COPIES") == "1") }

// stripeShareCopiesK: shared subs per connection on links with copies.
var stripeShareCopiesK = func() int {
	if v, err := strconv.Atoi(os.Getenv("HS2_MCONN_STRIPE_SHARE_COPIES_K")); err == nil && v >= 0 {
		return v
	}
	return 0
}()

// stripeLinkCopies: linkCopies, a variable for tests.
var stripeLinkCopies = linkCopies

// callMarkOn: calls mark their links (CALLFREE, or the shared pick's
// SHARE_COPIES call rule).
func callMarkOn() bool { return stripeCallFree || stripeShareCopies.Load() }

// share62: the key serves UPLOADS only (the stripe Iran sends). A download's
// stripe (the exit sends it) keeps the copies rule as with the key off: with
// the key on everywhere, the adopt61 lab's single downloads striped too
// (338-458 -> 830 Mbit/s) and the light users' p99 rose with them; the key
// was aimed at the upload, where production's single connections ran raw on
// one link. Iran opens every sub, so the open's avoid list carries the
// direction (stripeCopyUpHint, never equal to a link); without it — a
// download, or a growth not for an upload — nothing changes.
//
// HS2_MCONN_STRIPE_SHARE_COPIES_LANEQ (on unless "0"; with the key): among
// the candidate links, those the TUN lane (HS2_L3_LANE) sent no interactive
// packet over (a game's, a call's, a ping's) in the last stripeLaneQuiet go
// first; a lane link is taken only when no other is left. Unlike
// HS2_MCONN_STRIPE_SHARE_LANE it never refuses: the stripe keeps its links,
// they are just not the game's.
var stripeShareCopiesLaneQ = os.Getenv("HS2_MCONN_STRIPE_SHARE_COPIES_LANEQ") != "0"

// stripeCopyUpHint (in a stripe open's avoid list): the sub is an upload's,
// the SHARE_COPIES pick applies.
type stripeCopyUpHint struct{}

// stripeCopyUpHintLocked (Iran, under m.mu): mark an upload's stripe open.
func (m *mConn) stripeCopyUpHintLocked(avoid []any) []any {
	if stripeShareCopies.Load() && m.iran && (m.stGrowUp || m.stripeDlCopiesOKLocked()) {
		avoid = append(avoid, stripeCopyUpHint{})
		if !m.stGrowUp {
			avoid = append(avoid, stripeCopyDlHint{}) // fx212
		}
	}
	return avoid
}

// stripeDlCopiesOKLocked (Iran, under m.mu): HS2_MCONN_STRIPE_DL_COPIES lets
// this download's open take a copies' link. dl188 (fair share): only while
// the connection has fewer than stripeDlCopiesMax subs (the raw / first sub
// included; HS2_MCONN_STRIPE_DL_COPIES_MAX, 0: no bound) — a new download's
// first subs, never a big background stripe's tenth: the dl186 arms (dc1/dc2)
// gave the copies' links to the background stripes (173 shared picks a run)
// while every test download's open was still refused.
func (m *mConn) stripeDlCopiesOKLocked() bool {
	if !stripeDlCopies.Load() || m.stGrowUp {
		return false
	}
	mx := int(stripeDlCopiesMax.Load())
	return mx <= 0 || 1+m.liveStripeLocked() < mx
}

// stripeRawCopyHintLocked (Iran, under m.mu, the raw upgrade's open): the
// DL_COPIES hint. dl188: the raw upgrade (mconn_raw.go) built its avoid list
// without stripeCopyUpHintLocked, so DL_COPIES never reached the first stripe
// open of a raw download — exactly the opens that fail in a full pool (dc1:
// 1347 of 1349 pick-none lines copy-ok=0).
//
// ul197: the same hole for UPLOADS. share62 meant SHARE_COPIES for every
// upload open, but stripeCopyUpHintLocked (the grow path) is the only one
// that adds it: the raw upgrade's first stripe open of a lite raw UPLOAD went
// out without the hint, so in a full pool (every link carries lite's copies)
// it was refused ("pickwhy raw=true copyhint=false ... copies+light=181"),
// retried and refused again, and the upload ran its whole life raw on one
// link (lab2 uptrace: "subs=0 ... grow=6 openfail=6"). With
// HS2_MCONN_STRIPE_UP_RAW_COPIES=1 an upload's raw upgrade takes the hint as
// its later opens do (stripeUpRawCopiesOK); off: as before.
func (m *mConn) stripeRawCopyHintLocked(avoid []any) []any {
	if stripeShareCopies.Load() && m.iran && (m.stripeDlCopiesOKLocked() || m.stripeUpRawCopiesOKLocked()) {
		avoid = append(avoid, stripeCopyUpHint{})
		if !m.stGrowUp {
			avoid = append(avoid, stripeCopyDlHint{}) // fx212
		}
	}
	return avoid
}

// stripeUpRawCopies: HS2_MCONN_STRIPE_UP_RAW_COPIES (ul197, Iran, default on since rel68b).
var stripeUpRawCopies atomic.Bool

// stripeUpRawCopiesOKLocked (Iran, under m.mu): the raw upgrade grows an upload.
func (m *mConn) stripeUpRawCopiesOKLocked() bool {
	return stripeUpRawCopies.Load() && m.stGrowUp
}

// stripeDlCopiesMax: HS2_MCONN_STRIPE_DL_COPIES_MAX (dl188, default 4).
var stripeDlCopiesMax atomic.Int32

// HS2_MCONN_STRIPE_DL_COPIES=1 (dl186, Iran; default on since rel68b; NOT yet measured
// in the lab when added): downloads too take the SHARE_COPIES rule. dl186
// lab2 trace: of ~220 serving links in a full pool, 202-210 were passed over
// by a download's shared pick only because lite's copy-only subs ride them
// ("skip[... copies=209 ...]"), so a download's growth found no link while
// its first sub sat on a lossy one. A copy held behind the stripe on the
// exit's sender costs its user nothing while its primary delivers first (the
// share62 argument for uploads); call links and lane links keep their rules.
var stripeDlCopies atomic.Bool

// stripeCopyUp: the open may share a link with lite's copies — an upload's,
// or with HS2_MCONN_STRIPE_DL_COPIES any (stripeCopyUpHint in avoid).
func stripeCopyUp(avoid []any) bool {
	for _, a := range avoid {
		if _, ok := a.(stripeCopyUpHint); ok {
			return true
		}
	}
	return false
}

// stripeShareCopiesStat (diagnostics): shared picks under the key — on a
// link with copies, on a lane link, and those where a lane link was passed
// over for a quiet one.
var stripeShareCopiesStat struct {
	picks, copyLinks, laneLinks, laneSkipped atomic.Uint64
}

// stripeShareCopiesNote: count (and with HS2_MCONN_STRIPE_LOG, log) a shared
// pick made under the key.
func stripeShareCopiesNote(best *managedLink, nc int, laneBusy bool, w stripeShareWhy) {
	if best == nil {
		return
	}
	st := &stripeShareCopiesStat
	st.picks.Add(1)
	if nc > 0 {
		st.copyLinks.Add(1)
	}
	if laneBusy {
		st.laneLinks.Add(1)
	} else if w.laneBusy > 0 {
		st.laneSkipped.Add(1)
	}
	if stripeLog {
		log.Printf("stripe share-copies: up link %d copies=%d lane-busy=%v cand=%d cand-lane-busy=%d", best.id, nc, laneBusy, w.cand, w.laneBusy)
	}
}

// stripeDlCopiesMaxFromEnv: HS2_MCONN_STRIPE_DL_COPIES_MAX (0: no bound;
// unset or bad: 4).
func stripeDlCopiesMaxFromEnv() int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("HS2_MCONN_STRIPE_DL_COPIES_MAX"))); err == nil && v >= 0 {
		return v
	}
	return 4
}

// fx212 (an210 Q1, HS2_FX212_DL_PRIM, default on): a DOWNLOAD's open under
// HS2_MCONN_STRIPE_DL_COPIES may take a link with lite's copy-only subs only
// if no light PRIMARY rides it (every user stream there is a copy). The
// exit's download stripe fills that link's smux and TCP send queues at line
// rate; a crowd primary behind it waited (bs206 PL: crowd p99 +5 ms with
// DL_COPIES alone). A copy behind the stripe still costs its user nothing
// while its primary (elsewhere) delivers. Links without copies keep the
// rule they had with DL_COPIES off; uploads (Iran sends) are unchanged.
var fx212DlPrim = envBoolDefault("HS2_FX212_DL_PRIM", true) // "2" too: on, mode 2 (below)

// dlp217 (HS2_FX212_DL_PRIM=2, Iran): DL_PRIM v2. Lab3 (crowd 3000 over
// 40 links) showed mode 1 leaves almost no link: every copies' link also
// carries some of the crowd's long-lived light primaries (open, mostly
// idle), so the download fell back to its DL_COPIES-off median (118-168 vs
// 418 Mbit/s with DL_COPIES and no DL_PRIM). Mode 2 grades a copies' link
// instead of refusing it:
//
//   - tier 0: no light primary open (mode 1's only link);
//   - tier 1: primaries open, none moved a byte within fx212DlPrimQuiet
//     (HS2_FX212_DL_PRIM_QUIET_MS, default 1000): idle keepalive
//     connections, nobody waits behind the stripe right now;
//   - tier 2: primaries moved bytes lately ("hot"). A connection may hold at
//     most fx212DlPrimHotK (HS2_FX212_DL_PRIM_HOTK, default 1; 0: none) of
//     its shared subs on such links, the fewest hot primaries first.
//
// The tier ranks before the old order (flowing, copies, rate, users), so a
// download fills tier 0/1 links first and puts at most HOTK subs where
// light users are active. The download's own links are in its avoid list,
// so no two of its subs share a link. Counters: fx212DlPrimStat (the
// stripe stat line) and the pickwhy fields dlq / dlhot / dlhotref.
var fx212DlPrimMode = fx212DlPrimModeFrom(os.Getenv("HS2_FX212_DL_PRIM"))

// fx212DlPrimModeFrom: "0" off, "2" v2, anything else 1 (the default).
func fx212DlPrimModeFrom(v string) int {
	switch strings.TrimSpace(v) {
	case "0":
		return 0
	case "2":
		return 2
	}
	return 1
}

// fx212DlPrimQuiet: a primary idle this long does not make its link hot.
var fx212DlPrimQuiet = func() time.Duration {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("HS2_FX212_DL_PRIM_QUIET_MS"))); err == nil && v >= 0 {
		return time.Duration(v) * time.Millisecond
	}
	return time.Second
}()

// fx212DlPrimHotK: a download's shared subs on links with hot primaries.
var fx212DlPrimHotK = func() int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("HS2_FX212_DL_PRIM_HOTK"))); err == nil && v >= 0 {
		return v
	}
	return 1
}()

// fx212DlPrimStat (dlp217 diagnostics): download shared picks under mode 2
// by tier, the hot links refused for HOTK, the copies' links the picks
// scanned (eligible = scanned - hotref), and picks that found no link.
var fx212DlPrimStat struct {
	picks, tier0, tier1, tier2, hotRef, scanned, noLink atomic.Uint64
}

// fx212DlPrimStatString: the counters for the stripe stat line.
func fx212DlPrimStatString() string {
	st := &fx212DlPrimStat
	return fmt.Sprintf("dlprim2[picks=%d t0=%d t1=%d t2=%d hotref=%d scanned=%d none=%d]",
		st.picks.Load(), st.tier0.Load(), st.tier1.Load(), st.tier2.Load(), st.hotRef.Load(), st.scanned.Load(), st.noLink.Load())
}

// stripeLinkPrimHot: linkPrimHot, a variable for tests.
var stripeLinkPrimHot = linkPrimHot

// linkPrimHot: l's open primaries (as linkPrimaries) and how many of them
// moved a byte within quiet (since the sampler's last look, or its last
// active time is that recent). The sampler's bookkeeping (prevBytes,
// lastActive) is written under flowMu, which this holds.
func linkPrimHot(l Link, quiet time.Duration) (prims, hot int) {
	ml, ok := l.(*mtcpLink)
	if !ok {
		return 0, 0
	}
	now := time.Now()
	ml.flowMu.Lock()
	for cs := range ml.flows {
		if cs.copyOnly.Load() || cs.done.Load() {
			continue
		}
		prims++
		if cs.bytes.Load() != cs.prevBytes || now.Sub(cs.lastActive) < quiet {
			hot++
		}
	}
	ml.flowMu.Unlock()
	return prims, hot
}

// stripeCopyDlHint (in a stripe open's avoid list, with stripeCopyUpHint):
// the open is a download's (the DL_COPIES rule, fx212).
type stripeCopyDlHint struct{}

func stripeCopyDl(avoid []any) bool {
	for _, a := range avoid {
		if _, ok := a.(stripeCopyDlHint); ok {
			return true
		}
	}
	return false
}

// stripeLinkPrims: linkPrimaries, a variable for tests.
var stripeLinkPrims = linkPrimaries

// linkPrimaries: open user streams on l that are not copy-only (light
// primaries, or any other user stream).
func linkPrimaries(l Link) int {
	ml, ok := l.(*mtcpLink)
	if !ok {
		return 0
	}
	n := 0
	ml.flowMu.Lock()
	for cs := range ml.flows {
		if !cs.copyOnly.Load() && !cs.done.Load() {
			n++
		}
	}
	ml.flowMu.Unlock()
	return n
}

// fx212DlPrimNote (dlp217): count one download shared pick under mode 2.
func fx212DlPrimNote(w stripeShareWhy) {
	st := &fx212DlPrimStat
	defer fx212DlPrimLog()
	if !w.picked {
		st.noLink.Add(1)
		return
	}
	st.picks.Add(1)
	switch w.tier {
	case 0:
		st.tier0.Add(1)
	case 1:
		st.tier1.Add(1)
	default:
		st.tier2.Add(1)
	}
}

// fx212DlPrimLogAt: unix ns of the last dlprim2 line (one per
// fx212DlPrimLogEvery at most; b437 F11: was 10 s, prod Iran ~430 lines/h of
// cumulative counters).
var fx212DlPrimLogAt atomic.Int64

// fx212DlPrimLogEvery (b437 F11): the dlprim2 counters' line period.
const fx212DlPrimLogEvery = 5 * time.Minute

// fx212DlPrimLog: the counters' line, at most every fx212DlPrimLogEvery (from the picks).
func fx212DlPrimLog() {
	now := monoNano(time.Now())
	at := fx212DlPrimLogAt.Load()
	if now-at < int64(fx212DlPrimLogEvery) || !fx212DlPrimLogAt.CompareAndSwap(at, now) {
		return
	}
	log.Printf("stripe %s quiet=%v hotk=%d", fx212DlPrimStatString(), fx212DlPrimQuiet, fx212DlPrimHotK)
}
