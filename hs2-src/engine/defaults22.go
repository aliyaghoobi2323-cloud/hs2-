package engine

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/envdef"
	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
	"github.com/xtaci/smux"
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
	st244Rate.Store(st244RateFrom(envDefault("HS2_ST244_UPG_RATE"))) // rel69h (mconn_st244.go): table "auto" on the edge
	linkCCRefresh.Store(envOn("HS2_LINK_CC_REFRESH"))                // us200/cc203 (link_ccrefresh.go), both, default on since rel69a
	shapeSmall.Store(envOn("HS2_SHAPE_SMALL"))
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
	shapeBulk.Store(envOn("HS2_SHAPE_BULK"))
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
	stripeFast.Store(envOn("HS2_MCONN_STRIPE_FAST"))                                               // up55 (mconn_stripe_fast.go)
	stripeFastSolo.Store(envOn("HS2_MCONN_STRIPE_FAST_SOLO"))                                      // rel54b (mconn_stripe_fast.go)
	stripeDlFast.Store(envOn("HS2_MCONN_STRIPE_DL_FAST"))                                          // dr116 (mconn_stripe_dlfast.go)
	stripeDlFair.Store(envOn("HS2_MCONN_STRIPE_DL_FAIR"))                                          // cs141 (mconn_stripe_dlfair.go)
	lmFastPick.Store(envOn("HS2_LM_FASTPICK"))                                                     // lk162 (lm_fastpick.go)
	stripeDlShare.Store(envOn("HS2_MCONN_STRIPE_DL_SHARE"))                                        // dl184 (mconn_stripe_dlshare.go)
	stripeDlCopies.Store(envOn("HS2_MCONN_STRIPE_DL_COPIES"))                                      // dl186 (mconn_stripe_sharecopy.go)
	fx212DlPrimMode = fx212DlPrimModeFrom(envDefault("HS2_FX212_DL_PRIM"))                         // rel68e: table "2" on the edge (mconn_stripe_sharecopy.go)
	stripeDlCopiesMax.Store(int32(stripeDlCopiesMaxFromEnv()))                                     // dl188 (mconn_stripe_sharecopy.go)
	stripeUpRawCopies.Store(envOn("HS2_MCONN_STRIPE_UP_RAW_COPIES"))                               // ul197 (mconn_stripe_sharecopy.go)
	stripeUpHyst.Store(envOn("HS2_MCONN_STRIPE_UPGATE_HYST"))                                      // ul197 (mconn_stripe_uphyst.go)
	stripeHeadroomPct.Store(int32(envIntDef("HS2_MCONN_STRIPE_HEADROOM", 0)))                      // dl186 (mconn_stripe_headroom.go)
	stripeResGuard.Store(envOn("HS2_MCONN_STRIPE_RES_GUARD"))                                      // dr118 (mconn_stripe_resguard.go)
	stripeFastEarly.Store(int64(stripeEarlyFromEnv(envDefault("HS2_MCONN_STRIPE_FAST_EARLY_MS")))) // up57 (mconn_stripe_early.go)
	stripeUpSplit = envOn("HS2_MCONN_STRIPE_UP_SPLIT")                                             // gate74 (mconn_stripe_split.go)
	stripeUpPathQOn.Store(envOn("HS2_MCONN_STRIPE_UP_PATHQ"))                                      // up57 (mconn_stripe_pathq.go)
	stripeShareCopies.Store(envOn("HS2_MCONN_STRIPE_SHARE_COPIES"))                                // share62 (mconn_stripe_sharecopy.go)
	stripeResend.Store(envOn("HS2_MCONN_STRIPE_RESEND"))
	darkcallOn.Store(envOn("HS2_MCONN_DARKCALL"))          // cs193 (mconn_darkcall.go)
	darkcallTrace.Store(envOn("HS2_MCONN_DARKCALL_TRACE")) // cs194
	mconnSubResend.Store(envOn("HS2_MCONN_SUB_RESEND"))    // r68fix2..4, rel68c (mconn_sub_resend.go)
	keepStallOn.Store(keepStallFromEnv())                  // hs112 (mconn_stripe_keepstall.go)
	keepStallLazy.Store(keepStallLazyFromEnv())            // hs128
	stripeRebuildOn.Store(envOn("HS2_MCONN_STRIPE_REBUILD"))
	lbCutDrain = envOn("HS2_LB_CUT_DRAIN")
	slowSkipCopy = envOn("HS2_LB_SLOW_SKIP_COPY") // lb89 (lb_copyslow81.go)
	stripeQWin = msFromEnv(envDefault("HS2_MCONN_STRIPE_QWIN_MS"), 0)
	smuxAutoWin = autoWinFromEnv(envDefault)
	smuxSmallFirst = smallFirstFromEnv(envDefault("HS2_SMUX_SMALL_FIRST")) // lat58 (smux_smallfirst.go)
	smuxSendBatch = sendBatchFromEnv(envDefault("HS2_SMUX_SENDBATCH"))     // lk179 (smux_sendbatch.go)
	stripeAWSolo = envOn("HS2_STRIPE_AW_SOLO")
	stripeAWHeavy = stripeAWHeavyFromEnv(envDefault("HS2_STRIPE_AW_HEAVY"))
	stripeHeavyFloor = stripeHeavyFloorFromEnv(envDefault("HS2_STRIPE_HEAVY_FLOOR_MB"))                  // dl76 (smux_autowin.go)
	stripeHeavyFloorGate, _ = strconv.Atoi(strings.TrimSpace(envDefault("HS2_STRIPE_HEAVY_FLOOR_GATE"))) // dl77
	stripeHeavyFloorSoft = envOn("HS2_STRIPE_HEAVY_FLOOR_SOFT")                                          // dl77
	if us, err := strconv.Atoi(strings.TrimSpace(envDefault("HS2_STRIPE_HEAVY_FLOOR_QUS"))); err == nil && us > 0 {
		smux.HeavyFloorQueue = time.Duration(min(us, 50000)) * time.Microsecond // dl77: the gate's queue threshold
	}
	lbFastRecover = envOn("HS2_LB_FAST_RECOVER")
	lbIPCutGrace = graceFromEnv(envDefault("HS2_LB_START_GRACE_S"))
	lbFillPct = fillPctFromEnv(envDefault("HS2_LB_FILL_PCT"))
	lbSparePerRound = sparePerRoundFromEnv(envDefault("HS2_LB_SPARE_PER_ROUND"), lbFillPct)
	lbSpareEdge = envOn("HS2_LB_SPARE_EDGE")   // sp176 (lb_spare176.go)
	apShrinkHyst = envOn("HS2_AP_SHRINK_HYST") // sp176 (ap_hyst176.go)
	lbRecConfirm = envOn("HS2_LB_REC_CONFIRM")
	lbFlapPen = envOn("HS2_LB_FLAP_PEN")
	lbEdgeSync = envOn("HS2_LB_EDGE_SYNC") // judge58 (lb_edgesync.go)
	lbJudgeData = envOn("HS2_LB_JUDGE_DATA")
	lbEvac = envOn("HS2_LB_EVAC")
	lbRescue.Store(envOn("HS2_LB_RESCUE"))
	stripeEvacOn.Store(envOn("HS2_MCONN_STRIPE_EVAC")) // evac56 (mconn_stripe_evac.go)
	stripeEvacRaw.Store(envOn("HS2_MCONN_STRIPE_EVAC_RAW"))
	stripeEvacDown.Store(envOn("HS2_MCONN_STRIPE_EVAC_DOWN")) // evac58 (mconn_stripe_evac.go)
	rescueRecOn.Store(rescueRecFromEnv())                     // er97 (mconn_rescue_rec.go)
	rescueRstOn.Store(envOn("HS2_MCONN_RESCUE_RST"))          // rr155 (mconn_rescue_rst.go), default off
	rescueOn.Store(envOn("HS2_MCONN_RESCUE"))                 // er91 (mconn_rescue.go), default on since rel61 (envdef)
	loadRescue145()                                           // rs145 (mconn_rescue145.go)
	stripeOverlapOn.Store(envOn("HS2_MCONN_STRIPE_OVERLAP"))  // upov57 (mconn_stripe_overlap.go)
	stripeUpgIdle.Store(envOn("HS2_MCONN_STRIPE_UPG_IDLE"))   // evac58 (mconn_stripe_upgidle.go)
	lbRescueRaw = envOn("HS2_LB_RESCUE_RAW")
	lbRescueCut = envOn("HS2_LB_RESCUE_CUT")   // stuck59 (lb_rescue_cut.go)
	l3CutAvoid = envOn("HS2_LB_RESCUE_CUT_L3") // stuck59 (l3_cutavoid.go)
	lbRescueCallFree = envOn("HS2_LB_RESCUE_CALLFREE")
	lbCallRescue = envOn("HS2_LB_CALL_RESCUE")
	lbCallLate = envOn("HS2_LB_CALL_LATE")
	lbFastVerdict = envOn("HS2_LB_FAST_VERDICT")
	lbSilent = envOn("HS2_LB_SILENT")
	lbSilentRegrow = envOn("HS2_LB_SILENT_REGROW") // bh101 lab key, off (lb_silent.go)
	retireKeepIdle = envOn("HS2_RETIRE_KEEP_IDLE") // id125 (retire_keep.go)
	if n := envIntDef("HS2_RETIRE_KEEP_IDLE_MIN", 60); n > 0 {
		retireKeepIdleFor = time.Duration(n) * time.Minute
	}
	lbCutNoNew = envOn("HS2_LB_CUT_NONEW")       // fl107 (lb_flap24.go, steerDrawLocked)
	lbUpLoss = envOn("HS2_LB_UPLOSS")            // up148 (lb_uploss.go), edge, off
	lbStripeSteer = envOn("HS2_LB_STRIPE_STEER") // st149 (mconn_stripe_steer.go), both, default on since rel68b
	stripeSteerLog = envOn("HS2_LB_STRIPE_STEER_LOG")
	refillWait = msFromEnv(envDefault("HS2_REFILL_WAIT_MS"), 0)
	refillWaitBytes = envOn("HS2_REFILL_WAIT_BYTES")
	startDial = envOn("HS2_START_DIAL")
	queueCtlOn = envOn("HS2_QUEUE_CTL") // qctl63 (queuerun.go)
	exitQueue.on = queueCtlOn
	queueCtlUpOn = envOn("HS2_QUEUE_CTL_UP")
	edgeQueue.on = queueCtlUpOn
	queueCtlFast = envOn("HS2_QUEUE_CTL_FAST") // lat58, both, default on since rel69a
	queueCtlKeep = envOn("HS2_QUEUE_CTL_KEEP") // hl204..hl232 (queuectl.go, qseen.go), both, default on since rel69a
	linkSpreadLog = os.Getenv("HS2_LINK_SPREAD_LOG") == "1"
	qShaperMode = parseQShaperMode(envDefault("HS2_QUEUE_CTL_SHAPER")) // pk237/pk239 (queueshape_linux.go), both, mqchild by default since rel69e
	dl242Rescue.Store(dl243RescueKey())                                // dl243 (mconn_dl242.go), exit, default on since rel69f
	dl259On.Store(envDefault("HS2_DL259_PATHQ") == "1")                // dl259 (mconn_dl259.go), exit, default off since prod A/B 2026-11-17 (envdef)
}

// qShaperMode: HS2_QUEUE_CTL_SHAPER (see queueshape_linux.go): "root", "ifb"
// or "mqchild".
var qShaperMode = "root"

func parseQShaperMode(m string) string {
	switch m {
	case "ifb", "mqchild":
		return m
	}
	return "root"
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
