package engine

import (
	"os"
	"testing"
)

// rc15p1Snap holds the rc15.1 keys as the package init left them (this file
// sorts after mconn_l47*/mconn_l48*, so its init runs after theirs), before
// any test toggles them.
var rc15p1Snap map[string]bool

func init() {
	rc15p1Snap = map[string]bool{
		"STICK": l480Stick.Load(), "LIVEPICK": l480LivePick.Load(), "NEWPICK": l480NewPick.Load(),
		"FASTOPEN": l478FastOpen.Load(), "TAILRES": l477TailRes.Load(), "PAIRSK": l480dPairSk.Load(),
		"CALLOPEN": l485CallOpen.Load(), "FRESHBACK": l488FreshBack.Load(),
		"CALLPEND": l480dCallPendOn.Load(), "CALLLIVE": l480dCallLiveOn.Load(),
		"JOINRES": l476JoinRes.Load(), "JOINPRIM": l476JoinPrim.Load(),
	}
}

// TestRC15p1Defaults: with no HS2_L47x/L48x key set, the rc15.1 release
// defaults hold (HS2_L480 family and HS2_L485_CALLOPEN on, CALLPEND/CALLLIVE
// and l476 off; rc15.3: HS2_L488_FRESHBACK on), including HS2_L480 turning on FASTOPEN/TAILRES whose own
// init (earlier files) stores off.
func TestRC15p1Defaults(t *testing.T) {
	for _, k := range []string{"HS2_L480", "HS2_L480_STICK", "HS2_L480_LIVEPICK", "HS2_L480_NEWPICK",
		"HS2_L478_FASTOPEN", "HS2_L477_TAILRES", "HS2_L480D_PAIRSK", "HS2_L480D_CALLPEND",
		"HS2_L480D_CALLLIVE", "HS2_L485_CALLOPEN", "HS2_L488_FRESHBACK", "HS2_L476_JOINRES", "HS2_L476_JOINPRIM"} {
		if _, ok := os.LookupEnv(k); ok {
			t.Skipf("%s set in the environment", k)
		}
	}
	want := map[string]bool{"STICK": true, "LIVEPICK": true, "NEWPICK": true, "FASTOPEN": true,
		"TAILRES": true, "PAIRSK": true, "CALLOPEN": true, "FRESHBACK": true,
		"CALLPEND": false, "CALLLIVE": false, "JOINRES": false, "JOINPRIM": false}
	for k, w := range want {
		if rc15p1Snap[k] != w {
			t.Errorf("%s default = %v, want %v", k, rc15p1Snap[k], w)
		}
	}
}
