package engine

import (
	"os"
	"strconv"
	"strings"

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
// up) doubles its window, up to HS2_SMUX_AUTOWIN_MAX_MB (32), and a window
// update goes out every eighth of the window (HS2_SMUX_AUTOWIN_DIV; upstream:
// every half), so a sender waits at the limit less per round trip. Light streams
// never get there and keep the 2 MiB window they have today.
//
// Light users first: a stream grows only while its probes show no queue on
// its links (loop RTT within 2 ms + 1/16 of the minimum), halves when its
// last four probes all show a queue (over 5 ms + 1/8: its in-flight data is
// waiting in a socket or on the path, where light users' packets would wait
// behind it), and is held to HS2_SMUX_AUTOWIN_NEIGHBOR_MB (4) while a light
// user (another stream receiving under 8 Mbit/s) is active on the same link.
// The receiver measures this itself, so no signal has to cross the tunnel.
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
		// the cap while a light user receives on the same link (0: none)
		NeighborMax: num("HS2_SMUX_AUTOWIN_NEIGHBOR_MB", 4, 0, 256) << 20,
		Budget:      smux.NewWindowBudget(int64(budget) << 20),
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
