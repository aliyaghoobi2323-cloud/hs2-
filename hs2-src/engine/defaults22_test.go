package engine

import (
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/envdef"
	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

// TestApplyDefaults: no HS2_ env → after ApplyDefaults the engine runs the
// production set for its side; legacy and explicit keys are honoured.
func TestApplyDefaults(t *testing.T) {
	keys := []string{"HS2_DEFAULTS"}
	for _, k := range envdef.Table {
		keys = append(keys, k.Name)
	}
	saved := map[string]*string{}
	for _, k := range keys {
		if v, ok := os.LookupEnv(k); ok {
			saved[k] = &v
		} else {
			saved[k] = nil
		}
		os.Unsetenv(k)
	}
	t.Cleanup(func() {
		for k, v := range saved {
			if v != nil {
				os.Setenv(k, *v)
			} else {
				os.Unsetenv(k)
			}
		}
		envdef.Deactivate()
		loadEnvKeys()
		tlscarrier.LoadEnvKeys()
	})

	envdef.Deactivate()
	loadEnvKeys()
	if mconnOn.Load() || stripeK.Load() != 0 || smuxAutoWin != nil || lbAuto2 || queueCtlOn || queueCtlUpOn || exitQueue.on || edgeQueue.on {
		t.Fatal("dormant table changed the defaults")
	}

	if on := ApplyDefaults(true); len(on) == 0 {
		t.Fatal("edge: nothing applied")
	}
	if !mconnLite.Load() || stripeK.Load() != 8 || !stripeUp.Load() || smuxAutoWin == nil || !smuxAutoWin.Rel ||
		smuxAutoWin.Share != 32<<20 || stripeAWHeavy != 2 || !stripeAWSolo || pickLane == 0 || !pickClass ||
		!linkProbation || !partialRefill || !lbAuto2 || !lbSteer || !mconnCall.Load() || stripeQWin != 8*time.Second ||
		lbIPCutGrace != 20*time.Second || !tlscarrier.QuietIdle || !tlscarrier.Batch || !tlscarrier.Resume || !stripeResend.Load() {
		t.Fatal("edge: production set not in force")
	}
	if lbFillPct != 0 || lbSparePerRound != 1 || lbRecConfirm || lbFlapPen || lbSilent {
		t.Fatal("edge: exit-only keys on")
	}
	if !lbRescue.Load() || lbFastVerdict || lbRescueRaw {
		t.Fatal("edge: HS2_LB_RESCUE should default on, HS2_LB_FAST_VERDICT off")
	}
	if !stripeShareCopies.Load() || !stripeShareCopiesLaneQ {
		t.Fatal("edge: HS2_MCONN_STRIPE_SHARE_COPIES (share62) should default on, with the lane order")
	}
	if !lbRescueCut || l3CutAvoid {
		t.Fatal("edge: HS2_LB_RESCUE_CUT should default on, HS2_LB_RESCUE_CUT_L3 off (stuck61)")
	}
	if !slowSkipCopy {
		t.Fatal("edge: HS2_LB_SLOW_SKIP_COPY should default on (lb89)")
	}
	if !lbCutNoNew {
		t.Fatal("edge: HS2_LB_CUT_NONEW should default on (fl108)")
	}
	if !rescueOn.Load() || !rescueRecOn.Load() {
		t.Fatal("edge: HS2_MCONN_RESCUE (and _REC) should default on (rel61)")
	}
	if !rescueJoinPick.Load() || !rescueScaleOn.Load() || !rescueRetryOn.Load() {
		t.Fatal("edge: rel64 wants HS2_MCONN_RESCUE_JOINPICK, _SCALE and _RETRY on")
	}
	if !queueCtlUpOn || !edgeQueue.on || queueCtlOn || exitQueue.on || !queueCtlFast || !queueCtlKeep || !linkCCRefresh.Load() {
		t.Fatal("edge: rel69a wants HS2_QUEUE_CTL_UP, _FAST, _KEEP and HS2_LINK_CC_REFRESH on, HS2_QUEUE_CTL (exit) off")
	}
	if qShaperMode != "mqchild" {
		t.Fatalf("edge: rel69e wants HS2_QUEUE_CTL_SHAPER=mqchild, got %q", qShaperMode)
	}
	if dl242Rescue.Load() {
		t.Fatal("edge: HS2_DL243_RESCUE is exit-only (rel69f)")
	}
	if dl259On.Load() {
		t.Fatal("edge: HS2_DL259_PATHQ is exit-only (dl259)")
	}
	if want := int64(max(500, 100*runtime.GOMAXPROCS(0))); st244Rate.Load() != want {
		t.Fatalf("edge: rel69h wants HS2_ST244_UPG_RATE auto = %d, got %d", want, st244Rate.Load())
	}
	if stripeHeavyFloor != 4<<20 || stripeHeavyFloorGate != 2 || stripeHeavyFloorSoft {
		t.Fatal("edge: dl79 wants HS2_STRIPE_HEAVY_FLOOR_MB=4 with HS2_STRIPE_HEAVY_FLOOR_GATE=2, no soft")
	}
	if !lmFastPick.Load() {
		t.Fatal("edge: HS2_LM_FASTPICK should default on (fp185)")
	}
	if fx212DlPrimMode != 2 {
		t.Fatal("edge: rel68e wants HS2_FX212_DL_PRIM=2 (rank mode)")
	}
	if !darkcallOn.Load() || darkcallTrace.Load() || !lbStripeSteer || !stripeUpRawCopies.Load() || stripeUpHyst.Load() ||
		!stripeDlCopies.Load() || stripeDlCopiesMax.Load() != 4 || rescueRstOn.Load() || !mconnCallResend.Load() || !mconnSubResend.Load() {
		t.Fatal("edge: rel68c wants SUB_RESEND on; rel68b wants HS2_MCONN_DARKCALL, HS2_LB_STRIPE_STEER, _UP_RAW_COPIES, _DL_COPIES (MAX 4), CALL_RESEND on; DARKCALL_TRACE, UPGATE_HYST, RESCUE_RST off")
	}

	ApplyDefaults(false)
	if lbFillPct != 25 || lbSparePerRound != 3 || !lbRecConfirm || !lbFlapPen || !lbSilent || !stripeResend.Load() {
		t.Fatal("exit: exit-only keys off (or the both-sides resend)")
	}
	if stripeAWHeavy != 0 || stripeAWSolo || pickLane != 0 || linkProbation || partialRefill || lbRescue.Load() || stripeShareCopies.Load() {
		t.Fatal("exit: edge-only keys on")
	}
	if stripeHeavyFloor != 0 || stripeHeavyFloorGate != 0 {
		t.Fatal("exit: the heavy floor is edge-only (dl78)")
	}
	if lbRescueCut || l3CutAvoid {
		t.Fatal("exit: HS2_LB_RESCUE_CUT is edge-only, HS2_LB_RESCUE_CUT_L3 not a default")
	}
	if slowSkipCopy {
		t.Fatal("exit: HS2_LB_SLOW_SKIP_COPY is edge-only (lb89)")
	}
	if lbCutNoNew {
		t.Fatal("exit: HS2_LB_CUT_NONEW is edge-only (fl108)")
	}
	if lmFastPick.Load() {
		t.Fatal("exit: HS2_LM_FASTPICK is edge-only (fp185)")
	}
	if fx212DlPrimMode != 1 {
		t.Fatal("exit: HS2_FX212_DL_PRIM=2 is edge-only (rel68e)")
	}
	if !darkcallOn.Load() || !lbStripeSteer || stripeUpRawCopies.Load() || stripeDlCopies.Load() || stripeUpHyst.Load() || !mconnCallResend.Load() || !mconnSubResend.Load() {
		t.Fatal("exit: rel68c wants SUB_RESEND on; rel68b wants HS2_MCONN_DARKCALL and HS2_LB_STRIPE_STEER on, _UP_RAW_COPIES/_DL_COPIES edge-only")
	}
	if !rescueOn.Load() || !rescueRecOn.Load() {
		t.Fatal("exit: HS2_MCONN_RESCUE (and _REC) should default on (rel61)")
	}
	if rescueJoinPick.Load() || !rescueScaleOn.Load() || rescueRetryOn.Load() {
		t.Fatal("exit: rel64 wants HS2_MCONN_RESCUE_SCALE on, _JOINPICK and _RETRY edge-only")
	}
	if !queueCtlOn || !exitQueue.on || queueCtlUpOn || edgeQueue.on || !queueCtlFast || !queueCtlKeep || !linkCCRefresh.Load() {
		t.Fatal("exit: qctl63 wants HS2_QUEUE_CTL on, HS2_QUEUE_CTL_UP edge-only; rel69a wants _FAST, _KEEP, HS2_LINK_CC_REFRESH on")
	}
	if qShaperMode != "mqchild" {
		t.Fatalf("exit: rel69e wants HS2_QUEUE_CTL_SHAPER=mqchild (falls back to root on 1 queue), got %q", qShaperMode)
	}
	if !dl242Rescue.Load() {
		t.Fatal("exit: rel69f wants HS2_DL243_RESCUE on")
	}
	if dl259On.Load() {
		t.Fatal("exit: HS2_DL259_PATHQ is off by default since prod A/B 2026-11-17")
	}
	if st244Rate.Load() != 0 {
		t.Fatal("exit: HS2_ST244_UPG_RATE is edge-only (rel69h)")
	}

	os.Setenv("HS2_MCONN_STRIPE", "0")
	os.Setenv("HS2_SMUX_AUTOWIN", "")
	os.Setenv("HS2_LB_FLAP_PEN", "0")
	os.Setenv("HS2_QUEUE_CTL", "0")
	ApplyDefaults(false)
	if stripeK.Load() != 0 || smuxAutoWin != nil || lbFlapPen || queueCtlOn || exitQueue.on {
		t.Fatal("explicit off lost")
	}
	if !mconnLite.Load() {
		t.Fatal("other defaults lost with one key off")
	}
	if !lbJudgeData || lbEvac {
		t.Fatal("HS2_LB_JUDGE_DATA should default on, HS2_LB_EVAC off")
	}

	os.Setenv("HS2_DEFAULTS", "legacy")
	envdef.Deactivate()
	loadEnvKeys()
	tlscarrier.LoadEnvKeys()
	if ApplyDefaults(true) != nil {
		t.Fatal("legacy: defaults applied")
	}
	if darkcallOn.Load() || lbStripeSteer || stripeDlCopies.Load() || stripeUpRawCopies.Load() {
		t.Fatal("legacy: rel68b defaults in force")
	}
	if mconnOn.Load() || lbAuto2 || tlscarrier.QuietIdle || lbFlapPen || lbJudgeData || stripeShareCopies.Load() || queueCtlUpOn || edgeQueue.on || lmFastPick.Load() ||
		queueCtlFast || queueCtlKeep || linkCCRefresh.Load() {
		t.Fatal("legacy: defaults in force")
	}
}
