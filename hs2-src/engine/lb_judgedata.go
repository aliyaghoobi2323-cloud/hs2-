package engine

import (
	"errors"
	"net"
)

// The fast judge rates paths by data-plane quality (HS2_LB_JUDGE_DATA=1).
//
// Phase-4 lab (3 equal paths A tls, B reality, C both): at ~10 s one dial on B
// failed with "tlscarrier: reading server proof: i/o timeout" — TCP had
// connected; the front's handshake did not finish in time. That 5 s quality
// tick on B had 2 dials, 1 failed: fail 0.5, and q2Goodness's 4×fail term
// took B's goodness from ~0.65 to ~0.28 next to A and C at the SAME 1.1%
// loss: factor 1.00→0.19 with the reason "lower: loss 1.1%" (the reason
// named only the loss). B then climbed back 0.1 a reading, but the busy pool
// only retires idle links, so B ended the 45 s runs at ~37 links instead of
// ~51 (every rep). One handshake out of two is no sample, and a front's
// handshake failure is the front's business: the fallback / cool-down /
// start back-off (lb.go resultLocked: fails, until, backoff) handle it.
//
// With the key on, a reading's fail share counts only dials that failed
// BEFORE TCP connected (connect refused / SYN timeout: the path itself) and
// only once the tick had judgeFailMinDials dials, so one unlucky dial cannot
// move a path. Failures after the connect (TLS, Reality, the auth proof,
// the cover) never reach the judge. Off: the old rule (every failed dial).
var lbJudgeData bool // loadEnvKeys: HS2_LB_JUDGE_DATA

// judgeFailMinDials: dials a tick needs before its connect failures count.
var judgeFailMinDials = 4

// judgeFail is a reading's dial-failure share from a tick's counts: dials
// started, failed, and failed before TCP connected.
func judgeFail(dials, fails, connFails int) float64 {
	if dials <= 0 {
		return 0
	}
	if !lbJudgeData {
		return float64(fails) / float64(dials)
	}
	if dials < judgeFailMinDials {
		return 0
	}
	return float64(connFails) / float64(dials)
}

// dialConnectStage reports whether a dial error happened before the TCP
// connection was up (the path's failure, not the front's): a *net.OpError
// whose Op is "dial". Errors from the handshake on a connected socket (read
// / write timeouts, EOF, a front's own errors) are not.
func dialConnectStage(err error) bool {
	var oe *net.OpError
	return errors.As(err, &oe) && oe.Op == "dial"
}
