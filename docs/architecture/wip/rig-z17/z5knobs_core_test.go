package engine

// window core rig knobs (z11, scratch), built only into trees with the
// window core: they map the rig's environment to what cmd/hs2 applyTuning
// sets from HS2_TUNE_WCORE* (engine/wcore_run.go), so one test binary covers
// every option.
//
//	Z5_WCORE       Wcore (0: off)
//	Z5_WC_TRACE    WcoreTrace (a JSON line per evaluation per link)
//	Z5_WC_FIXED    WcoreFixedWin, bytes (every fast stream pinned there)
//	Z5_WC_QTARGET  WcoreQTarget, ms
//	Z5_WC_PROBEQ   WcoreProbeQ, ms
//	Z5_WC_STAGE    WcoreStage
//	Z5_WC_PACECAP  WcorePaceCap (z12: the sender's pacing cap, x delivery rate)
//	Z5_WC_BURST    WcoreBurstShare (z12: B held where the burst after PROBE_RTT fits Q_cap)
//	Z5_WC_UPLOAD   WcoreUpload (z15: the exit-side upload core, off by default until step 3)
//	Z5_MAXBUF      SmuxStreamBuffer (MaxStreamBuffer), MiB: past 16 MiB for
//	               the calibration's fixed windows of 3 BDP on 1000/100
//	Z5_PACENET     PaceNet (z17: the exit's pacing net; 0 off)
//	Z5_PACENET_K   PaceNetK (the cap's factor over the delivery rate)
//	Z5_PACENET_LOSS PaceNetLoss (the resend share that engages it)
//
// and point the core's log lines at the rig (z5SetWcLog).

import (
	"os"
	"strconv"
)

func init() {
	num := func(k string) (float64, bool) {
		v := os.Getenv(k)
		if v == "" {
			return 0, false
		}
		f, err := strconv.ParseFloat(v, 64)
		return f, err == nil
	}
	if v, ok := num("Z5_WCORE"); ok {
		Wcore = int(v)
	}
	if v, ok := num("Z5_WC_QTARGET"); ok {
		WcoreQTarget = int(v)
	}
	if v, ok := num("Z5_WC_PROBEQ"); ok {
		WcoreProbeQ = int(v)
	}
	if v, ok := num("Z5_WC_STAGE"); ok {
		WcoreStage = int(v)
	}
	if v, ok := num("Z5_WC_FIXED"); ok {
		WcoreFixedWin = int(v)
	}
	if v := os.Getenv("Z5_WC_TRACE"); v != "" {
		WcoreTrace = v
	}
	if v, ok := num("Z5_WC_PACECAP"); ok {
		WcorePaceCap = v
	}
	if v, ok := num("Z5_WC_BURST"); ok {
		WcoreBurstShare = v
	}
	if v, ok := num("Z5_WC_UPLOAD"); ok {
		WcoreUpload = int(v)
	}
	if v, ok := num("Z5_PACENET"); ok {
		PaceNet = int(v)
	}
	if v, ok := num("Z5_PACENET_K"); ok {
		PaceNetK = v
	}
	if v, ok := num("Z5_PACENET_LOSS"); ok {
		PaceNetLoss = v
	}
	if v, ok := num("Z5_MAXBUF"); ok {
		SmuxStreamBuffer = int(v * (1 << 20))
	}
	z5SetWcLog = setWcoreLog
}
