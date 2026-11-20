package engine

import (
	"fmt"
	"os"
	"time"
)

// g401 (proposals/zdu350.md, step-3 F1): reconcile sizes the pool for the
// current peer generation only.
//
// During a Kharej handover the old Kharej's links stay up on Iran for the
// connections that process drains; they are peerOld (zdu_counts.go): not
// serving, not in aliveLocked / countsLocked, so the autopilot's target T is
// the current generation's link count. reconcile, though, built its serving
// set from every live link that was not degraded, draining or suspect, marks
// ignored. Traced on lab1 (g401 A/B, HS2_ZDU_TRACE): while the new Kharej's
// links arrive in the hold phase (zdu_phase.go) THEY carry the mark, so
// reconcile saw old (200) + new links against T = 200 and marked the surplus
// retiring - the new links, the idlest. At the flip to full the marks swap:
// the 200 old links now filled "serving" in reconcile's view (serving == T,
// nothing is brought back), and all 200 new links stayed retiring for the
// whole drain ("serving=200 (old 200) retiring=200"). Users still reached
// them through the picker's retiring tier, but drainTick closes a retiring
// link once it is empty: at a reconnect storm (every user conn ends) or any
// quiet moment the new links were closed, the new Kharej - whose target from
// poolctl was still 200 - redialed each ("exit link down; redial"), and the
// redialed links came back retiring-eligible again. S9d (g400): ~63 new links
// churned in the storm minute while the old Kharej drained, whole-run call
// p99 12 s vs 233 ms in the control arm.
//
// With the key on a peerOld link is now outside reconcile: neither serving
// (it does not fill T) nor retiring (it is never brought back to serve).
// It stays up for its draining connections; the old Kharej closes it as
// they end (its drain mode retires every slot whose link ends) or exits at
// its drain end. Key off: zduOld is never set, reconcile is unchanged.
var zduReconFix = true

// zdu review 5 (M): the mix phase (zdu_phase.go) clears every peerOld mark
// so both generations serve. reconcile then counted old (O) + new (N) links
// against T again and retired the surplus - the idlest, usually the new
// links; drainTick closed each once empty and older than 4 s, and the new
// Kharej redialed it. A slow dial-up (N between ceil(O/2) and O for seconds,
// up to 60 s) sits in exactly that state. While mixing, zduRefreshLocked now
// sets zduMixOld on the older generation's links: they keep serving (no
// routing mark), but reconcile leaves them out, so it sizes T against the
// new generation as in the hold and full phases. The flag is recomputed on
// every refresh (cleared outside mix, so a revert counts the old links
// again). Key off: never set.
var zduReconMixFix = true

// zduReconOut: the link is outside reconcile as an older generation's.
func zduReconOut(ml *managedLink) bool {
	return ml.zduOld || ml.zduMixOld && zduReconMixFix
}

// zduReconTrace: HS2_ZDU_TRACE=1 (lab only) logs every reconcile tick that
// sees a peerOld link: T, serving, retiring, the old links, and the links
// brought back / marked retiring by generation.
var zduReconTrace = os.Getenv("HS2_ZDU_TRACE") == "1"

// zduReconTraceLocked is the HS2_ZDU_TRACE line for one reconcile tick.
// Caller holds m.mu.
func zduReconTraceLocked(m *LinkManager, T int, serving, retiring []*managedLink, back, gone []string) {
	if !zduReconTrace || !zduOn.Load() {
		return
	}
	old, oldS, oldR, oldGone := 0, 0, 0, 0
	gid := map[string]bool{}
	for _, g := range gone {
		gid[g] = true
	}
	for _, ml := range m.links {
		if !zduReconOut(ml) || !ml.link.Alive() {
			continue
		}
		old++
		if gid[fmt.Sprint(ml.id)] {
			oldGone++
		}
	}
	for _, ml := range serving {
		if zduReconOut(ml) {
			oldS++
		}
	}
	for _, ml := range retiring {
		if zduReconOut(ml) {
			oldR++
		}
	}
	if old == 0 && len(gone) == 0 && len(back) == 0 {
		return
	}
	ph := "full/none"
	if m.zduMixNow() != nil {
		ph = "mix"
	}
	m.log("zdu-trace: reconcile %s T=%d serving=%d (old %d) retiring=%d (old %d) peerold=%d back=%d retire_new=%d retire_old=%d phase=%s fix=%v",
		time.Now().UTC().Format("15:04:05.000"), T, len(serving), oldS, len(retiring), oldR, old, len(back), len(gone)-oldGone, oldGone, ph, zduReconFix)
}
