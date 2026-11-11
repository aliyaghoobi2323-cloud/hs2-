package engine

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/envdef"
	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

// Default-on production keys (envdef.Table). Every key in the table is read
// here, through envDefault, and nowhere else in the package: at process start
// (init) the table is dormant, so the values are exactly what the environment
// says (the old defaults); ApplyDefaults — called by cmd/hs2 for the l3mtcp
// carrier in reverse, before anything starts — activates the table for this
// server's side and reads every key again.

// envDefault is os.Getenv with the default table (envdef.Get).
func envDefault(k string) string { return envdef.Get(k) }

func envOn(k string) bool { return envDefault(k) == "1" }

func init() { loadEnvKeys() }

// loadEnvKeys (re)reads every key of the default table with each key's own
// parser, as it was read before the table existed.
func loadEnvKeys() {
	shapeSmall = envOn("HS2_SHAPE_SMALL")
	linkProbation = envOn("HS2_LINK_PROBATION")
	partialRefill = envOn("HS2_PARTIAL_REFILL")
	pickLane = parseLane(envDefault("HS2_PICK_LANE"))
	pickClass = envDefault("HS2_PICK_CLASS") == "1" || pickLane > 0
	l3LaneOn = envOn("HS2_L3_LANE")
	heavyBacklog = envOn("HS2_HEAVY_BACKLOG")
	l3DupOn = envOn("HS2_L3_DUP")
	setMConnMode(envDefault("HS2_MCONN"))
	fastLink = envOn("HS2_FAST_LINK")
	mconnBatch.Store(envOn("HS2_MCONN_BATCH"))
	shapeBulk = envOn("HS2_SHAPE_BULK")
	quietIdle = envOn("HS2_QUIET_IDLE")
	mconnDeadlineFix.Store(envOn("HS2_MCONN_DEADLINE_FIX"))
	mconnCloseHS.Store(envOn("HS2_MCONN_CLOSE_HS"))
	lbAuto2 = envOn("HS2_LB_AUTO2")
	lbSteer = envBoolDefault("HS2_LB_STEER", lbAuto2)
	setMConnCall(envDefault("HS2_MCONN_CALL"))
	mconnCallAvoid = envOn("HS2_MCONN_CALL_AVOID")
	stripeK.Store(int32(parseStripe(envDefault("HS2_MCONN_STRIPE"))))
	stripeUp.Store(envOn("HS2_MCONN_STRIPE_UP"))
	stripeShare = envOn("HS2_MCONN_STRIPE_SHARE")
	stripeRel = envOn("HS2_MCONN_STRIPE_REL")
	stripeResend.Store(envOn("HS2_MCONN_STRIPE_RESEND"))
	lbCutDrain = envOn("HS2_LB_CUT_DRAIN")
	stripeQWin = msFromEnv(envDefault("HS2_MCONN_STRIPE_QWIN_MS"), 0)
	smuxAutoWin = autoWinFromEnv(envDefault)
	stripeAWSolo = envOn("HS2_STRIPE_AW_SOLO")
	stripeAWHeavy = stripeAWHeavyFromEnv(envDefault("HS2_STRIPE_AW_HEAVY"))
	lbFastRecover = envOn("HS2_LB_FAST_RECOVER")
	lbIPCutGrace = graceFromEnv(envDefault("HS2_LB_START_GRACE_S"))
	lbFillPct = fillPctFromEnv(envDefault("HS2_LB_FILL_PCT"))
	lbSparePerRound = sparePerRoundFromEnv(envDefault("HS2_LB_SPARE_PER_ROUND"), lbFillPct)
	lbRecConfirm = envOn("HS2_LB_REC_CONFIRM")
	lbFlapPen = envOn("HS2_LB_FLAP_PEN")
	lbJudgeData = envOn("HS2_LB_JUDGE_DATA")
	lbEvac = envOn("HS2_LB_EVAC")
	lbRescue = envOn("HS2_LB_RESCUE")
	lbRescueRaw = envOn("HS2_LB_RESCUE_RAW")
	lbRescueCallFree = envOn("HS2_LB_RESCUE_CALLFREE")
	lbCallRescue = envOn("HS2_LB_CALL_RESCUE")
	lbCallLate = envOn("HS2_LB_CALL_LATE")
	lbFastVerdict = envOn("HS2_LB_FAST_VERDICT")
	refillWait = msFromEnv(envDefault("HS2_REFILL_WAIT_MS"), 0)
	refillWaitBytes = envOn("HS2_REFILL_WAIT_BYTES")
	startDial = envOn("HS2_START_DIAL")
	linkSpreadLog = os.Getenv("HS2_LINK_SPREAD_LOG") == "1"
}

// msFromEnv is envMsDefault on a value already read.
func msFromEnv(v string, d time.Duration) time.Duration {
	if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && f >= 0 {
		return time.Duration(f * float64(time.Millisecond))
	}
	return d
}

// ApplyDefaults turns the default table on for this server (edge: the iran
// side, mode "dial") and rereads every key, here and in tlscarrier. It must
// run before the tunnel starts. It returns the defaults now in force (keys
// the environment leaves unset), or nil under HS2_DEFAULTS=legacy.
func ApplyDefaults(edge bool) []string {
	if !envdef.Activate(edge) {
		return nil
	}
	loadEnvKeys()
	tlscarrier.LoadEnvKeys()
	return envdef.Applied()
}
