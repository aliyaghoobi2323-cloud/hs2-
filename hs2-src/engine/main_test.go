package engine

import (
	"os"
	"strconv"
	"testing"
)

// TestMain applies R1's kill switches from the environment as cmd/hs2's
// applyTuning does, so the whole suite runs on the old path too:
// HS2_TUNE_SCHED=0 go test ./engine/ (TestAngleSchedOff), HS2_TUNE_HQG=0,
// HS2_TUNE_SENDCAP=0, HS2_TUNE_CLASS=0. (The links' caps take the switches
// from the same variables: tlscarrier.LocalAuthHdr.)
func TestMain(m *testing.M) {
	for _, k := range []struct {
		env string
		v   *int
	}{{"HS2_TUNE_SCHED", &TuneSched}, {"HS2_TUNE_HQG", &TuneHQG}, {"HS2_TUNE_SENDCAP", &TuneSendCap}, {"HS2_TUNE_CLASS", &TuneClass},
		{"HS2_TUNE_HQG_SEGS", &HQGSegs}, {"HS2_TUNE_HQG_GATE", &HQGGate}, {"HS2_TUNE_SENDCAP_BLOCKED", &SendCapBlocked},
		{"HS2_TUNE_QUIET", &TuneQuiet}, {"HS2_TUNE_EARLYDONE", &TuneEarlyDone}, {"HS2_TUNE_ALPHA", &TuneAlpha}, {"HS2_TUNE_HQG_QUIET_SEGS", &HQGQuietSegs}} {
		if s := os.Getenv(k.env); s != "" {
			if n, err := strconv.Atoi(s); err == nil {
				*k.v = n
			}
		}
	}
	os.Exit(m.Run())
}
