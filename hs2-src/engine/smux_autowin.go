package engine

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tune"
	"github.com/xtaci/smux"
)

// HS2_SMUX_AUTOWIN=1: an adaptive per-stream smux receive window
// (third_party/smux/autowin.go). Off (the default) the sessions are exactly
// what they were, the same bytes on the wire.
//
// Why: smux advertises a fixed SmuxStreamBuffer (2 MiB) window and sends the
// window update only after half of it was read, so one stream — one user
// connection, or one mconn sub — moves about 1 MiB per round trip: 100-150
// Mbit/s at 80 ms however fast the links are. With the switch a stream that
// proves window-bound (its sender waits at the limit while the reader keeps
// up) grows its window (x2 up to 8 MiB, then +25%), up to
// HS2_SMUX_AUTOWIN_MAX_MB (32), and a window
// update goes out every eighth of the window (HS2_SMUX_AUTOWIN_DIV; upstream:
// every half), so a sender waits at the limit less per round trip. Light streams
// never get there and keep the 2 MiB window they have today.
//
// Light users first: a stream grows only while its probes show no queue on
// its links (loop RTT within 1 ms + 1/32 of the path's minimum), loses a
// quarter when its last two probes both show a queue (over 3 ms + 1/16; both
// thresholds times HS2_SMUX_AUTOWIN_QUEUE_PCT, default 30%: its
// in-flight data is
// waiting in a socket or on the path, where light users' packets would wait
// behind it), and is held to HS2_SMUX_AUTOWIN_NEIGHBOR_MB (2, the base) while a light
// user (another stream receiving under 8 Mbit/s) is active on the same link.
// The receiver measures this itself, so no signal has to cross the tunnel.
//
// Striped connections (HS2_MCONN_STRIPE) keep upstream's window on Iran's
// side (autoWinOffForStripe below): the stripe spreads a download over links,
// the adaptive window serves what is not striped.
//
// Compatibility: no capability bit. The window travels in every update frame
// and an older peer's smux honours whatever it carries, so either side may
// run the switch alone: on Iran it speeds up downloads (Iran receives), on
// the kharej side uploads.
//
// Memory: growth is drawn from one process-wide budget,
// HS2_SMUX_AUTOWIN_BUDGET_MB (default 512, at most an eighth of RAM, at least
// 64): beyond it no stream grows, near it the largest windows shrink to a fair
// share, and an idle grown window halves every 2 s. A window is credit, not
// memory; memory is used only by data a reader has not taken, so the
// worst case beyond today's is the budget, reached only if every grown
// stream's reader stalls. Each session's token bucket grows with its streams'
// windows, so a grown stream never starves the link's other streams.

// smuxAutoWin is the shared AutoWindow (nil: switch off).
var smuxAutoWin = autoWinFromEnv(os.Getenv)

func autoWinFromEnv(get func(string) string) *smux.AutoWindow {
	if strings.TrimSpace(get("HS2_SMUX_AUTOWIN")) != "1" {
		return nil
	}
	num := func(k string, def, lo, hi int) int {
		if v, err := strconv.Atoi(strings.TrimSpace(get(k))); err == nil && v >= lo && v <= hi {
			return v
		}
		return def
	}
	budget := 512
	if ram, _ := tune.Detect(); ram > 0 {
		budget = min(budget, max(ram/8, 64))
	}
	budget = num("HS2_SMUX_AUTOWIN_BUDGET_MB", budget, 16, 1<<14)
	return &smux.AutoWindow{
		Max:       num("HS2_SMUX_AUTOWIN_MAX_MB", 32, 1, 256) << 20,
		UpdateDiv: num("HS2_SMUX_AUTOWIN_DIV", 8, 2, 64),
		// the cap while a light user (a call, a game, an app ping) receives on
		// the same link (0: none). 2 = the base window: no growth beside a light
		// user — a 4 MiB window queued ~100 ms ahead of a call on its link
		NeighborMax: num("HS2_SMUX_AUTOWIN_NEIGHBOR_MB", 2, 0, 256) << 20,
		// HS2_SMUX_AUTOWIN_QUEUE_PCT: the queue thresholds in percent (30: the
		// 3 Gbit/s lab, 8 uploads at 0% loss: at 100 the light users' p99 rose
		// 5-25 ms, at 30 the app pings, games and ICMP stayed at the baseline)
		QueueScale: float64(num("HS2_SMUX_AUTOWIN_QUEUE_PCT", 30, 10, 1000)) / 100,
		Budget:     smux.NewWindowBudget(int64(budget) << 20),
		// HS2_SMUX_AUTOWIN_REL=1: queue rules on the excess over the path's
		// standing jitter, and the light-neighbour cap only while the
		// stream's own probes show that excess (AutoWindow.Rel)
		Rel: strings.TrimSpace(get("HS2_SMUX_AUTOWIN_REL")) == "1",
		// HS2_SMUX_AUTOWIN_NB1=1 (with REL): one probe over qLow is enough
		// for the light-neighbour brake (default: two in a row).
		NeighborOne: strings.TrimSpace(get("HS2_SMUX_AUTOWIN_NB1")) == "1",
		// HS2_SMUX_AUTOWIN_SHARE_MB (0: off): the total window the concurrently
		// heavy streams share — one alone keeps MAX_MB, n together each
		// max(base, SHARE/n), so 8 uploads hold about one BDP, not 8x MAX
		Share: num("HS2_SMUX_AUTOWIN_SHARE_MB", 0, 0, 4096) << 20,
	}
}

// SmuxAutoWinStats reports the window budget (ok false: switch off).
func SmuxAutoWinStats() (smux.WindowBudgetStats, bool) {
	if smuxAutoWin == nil {
		return smux.WindowBudgetStats{}, false
	}
	return smuxAutoWin.Budget.Stats(), true
}

// Striped connections (HS2_MCONN_STRIPE) keep upstream's window on Iran's
// side, where they receive the download: the stripe sender puts each chunk on
// the sub with the shortest queue and widens only while every sub is used,
// so it needs the fixed window's back-pressure — with faster window updates
// the first sub drained so fast that it took every chunk and the stripe
// stayed at 2 subs (3 Gbit/s lab, 1% loss: 72 Mbit/s instead of 420). The
// stripe already spreads a download over up to K links; the adaptive window
// is for what is not striped (uploads, plain streams).
func autoWinOffForStripe(lm *LinkManager, st any) {
	if smuxAutoWin == nil || !mconnStripeWanted(lm) {
		return
	}
	if o, ok := st.(interface{ SetAutoWindowOff() }); ok {
		o.SetAutoWindowOff()
	}
}

// mconnStripeWanted: Iran's connections to this exit may stripe (the
// condition serveUserMConn applies).
func mconnStripeWanted(lm *LinkManager) bool {
	return stripeK.Load() > 0 && mconnLite.Load() && mconnDeadlineFix.Load() && lm.exitInfo.Load().mconnStripe()
}

// A stripe that has settled gets the adaptive window back (Iran): once its
// width reached K, or stayed at 2 or more for stripeAWSettle, every sub of
// it may grow — the stripe no longer needs the subs to fill evenly to widen,
// and a striped download is then bound by K fixed windows (the 3 Gbit/s lab:
// 2 MiB windows held one striped download at 1% loss to ~250-420 Mbit/s,
// fixed 8 MiB windows reached ~900). When the width changes the subs go back
// to the fixed window until it settles again. The growth itself is under the
// same rules as every stream's (the queue rule, the process-wide allowance,
// the light-neighbour cap).
const stripeAWSettle = 3 * time.Second

// stripeAWSolo: HS2_STRIPE_AW_SOLO=1 (off by default). Iran opens every sub
// of a stripe-capable connection with the fixed window (autoWinOffForStripe)
// and gives the adaptive one back only once the stripe has widened. A
// download whose stripe never widens (production: stripe_bytes flat during a
// single download) then stays at 2 MiB a round trip: 154-196 Mbit/s down
// beside 1245 up on the same link. With the key, a connection that stayed at
// width 1 for stripeAWSettle gets the adaptive window on its sub too; if the
// stripe widens later the width change turns it off again, as before.
var stripeAWSolo = os.Getenv("HS2_STRIPE_AW_SOLO") == "1"

// stripeAW is an mConn's state for this (Iran, under m.mu).
type stripeAW struct {
	on    bool
	width int
	at    time.Time // when width last changed
}

// stripeAutoWinLocked runs with the stripe's growth tick (stripeTickLocked).
func (m *mConn) stripeAutoWinLocked(now time.Time) {
	if smuxAutoWin == nil || !m.iran || (m.stN == 0 && !stripeAWSolo) {
		return
	}
	a := &m.stAW
	w := 1 + m.liveStripeLocked()
	if w != a.width {
		a.width, a.at = w, now
		if a.on {
			a.on = false
			m.subsAutoWinLocked(false)
		}
		return
	}
	settled := w >= 2 && (w >= m.stK || now.Sub(a.at) >= stripeAWSettle)
	if stripeAWSolo && w == 1 && now.Sub(a.at) >= stripeAWSettle {
		// HS2_STRIPE_AW_SOLO: a stripe that never widened (its gate held
		// it: a queued path, a call, the conn cap) is one plain stream —
		// on a fixed 2 MiB window, ~200 Mbit/s at 80 ms whatever the link
		settled = true
	}
	if settled && !m.ended {
		a.on = true
		m.subsAutoWinLocked(true) // idempotent: also a sub that joined since
	}
}

func (m *mConn) subsAutoWinLocked(on bool) {
	for _, s := range m.subs {
		if s.retired || s.dead {
			continue
		}
		var st any = s.rw
		if ms, ok := s.rw.(*mconnSub); ok {
			st = ms.stream
		}
		if on {
			if o, ok := st.(interface{ SetAutoWindowOn() }); ok {
				o.SetAutoWindowOn()
			}
		} else if o, ok := st.(interface{ SetAutoWindowOff() }); ok {
			o.SetAutoWindowOff()
		}
	}
}
