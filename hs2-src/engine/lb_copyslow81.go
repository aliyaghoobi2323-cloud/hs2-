package engine

// lbv81: HS2_LB_SLOW_SKIP_COPY=1 (lb89: default ON on the edge via envdef;
// HS2_LB_SLOW_SKIP_COPY=0 turns it off).
//
// slowStreams names the "trickling (not flowing)" streams that the force
// close on retiring links (retireForce, and cutDrainForce on a cut-drained
// link) ends. It reads the per-stream rate EWMA, but flowStats never folds a
// copyOnly stream (an mconn's second light sub, HS2_MCONN / HS2_MCONN_CALL):
// its EWMA stays 0 and steady 0, so EVERY copy sub on the link looks
// trickling however much it carries. The byte snapshot that guards the close
// does not save it either: the first candidate is checked microseconds after
// its snapshot. Closing a call's copy sub ends the call when its other sub
// missed copies recently (subDied: errMConnLost, RST). lbv81 soak lab2: path
// c cut-drained at t=164, cutDrainForce (60 s) later the edge logged "1
// trickling (not flowing): 1 on cut-path links retiring 1m+" and the probe's
// 2 Mbit call ended at t=226 with "write: broken pipe" (it carried 192 ms
// frames up to its last sample; the evac path, lightStreams, already skips
// copyOnly streams for this reason).
//
// With the key on, slowStreams skips copyOnly streams: a copy sub is closed
// only when idle (idleStreams, drainIdle) or with its link.
var slowSkipCopy bool // loadEnvKeys: HS2_LB_SLOW_SKIP_COPY
