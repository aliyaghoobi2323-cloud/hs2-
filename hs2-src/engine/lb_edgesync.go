package engine

// judge58: HS2_LB_EDGE_SYNC=1 (edge, the Iran side; default on through
// envdef.Table) — the edge's own judge does not lift a cut while the dialing
// side's fresh report still cuts that path.
//
// Production, tunnel 1, 2026-11-11 19:00–01:00 (6 of 7 Iran IPs at 8–30%
// loss for hours): of 168 forced cuts that followed one of the edge's own
// recoveries, 165 came within 5 s of it ("recovering (loss back under
// 2.2%)" then "cut (loss 12.6%, as the dialing side judged)" with the same
// stale reason, again and again). The edge reads a cut path through the
// dialing side's tiny probe sample, so FAST_RECOVER / REC_CONFIRM lift its
// copy of the cut after their short hold (10 s) while the dialing side —
// whose judgement steers every new connection (repCut, repShare) and drains
// the cut paths' links — still holds it. The ping-pong changed no
// connection (the edge's own factor steers nothing) but it filled the log
// with false cut/recover pairs (203.0.113.10: 68 cuts in 4 h, cut periods
// p50 30 s, while the dialing side held one long cut), inflated the
// watchdog's cut count and hid the real verdict timeline.
//
// With the key on, a path the latest report (≤ 20 s old) marks cut stays
// cut in the edge's judge; its okSince keeps running, so once the report
// lifts the cut the edge follows at its next reading (its own hold already
// served). Without a fresh report the edge judges alone, as before.
var lbEdgeSync bool // loadEnvKeys: HS2_LB_EDGE_SYNC

// edgeSyncHolds reports whether the edge's judge must keep path q cut now.
func edgeSyncHolds(q *q2Path) bool { return lbEdgeSync && q.remCut }
