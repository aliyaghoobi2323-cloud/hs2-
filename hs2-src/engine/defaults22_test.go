package engine

import (
	"os"
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
	if mconnOn.Load() || stripeK.Load() != 0 || smuxAutoWin != nil || lbAuto2 {
		t.Fatal("dormant table changed the defaults")
	}

	if on := ApplyDefaults(true); len(on) == 0 {
		t.Fatal("edge: nothing applied")
	}
	if !mconnLite.Load() || stripeK.Load() != 8 || !stripeUp.Load() || smuxAutoWin == nil || !smuxAutoWin.Rel ||
		smuxAutoWin.Share != 32<<20 || stripeAWHeavy != 2 || !stripeAWSolo || pickLane == 0 || !pickClass ||
		!linkProbation || !partialRefill || !lbAuto2 || !lbSteer || !mconnCall.Load() || stripeQWin != 8*time.Second ||
		lbIPCutGrace != 20*time.Second || !tlscarrier.QuietIdle || !tlscarrier.Batch || !tlscarrier.Resume {
		t.Fatal("edge: production set not in force")
	}
	if lbFillPct != 0 || lbSparePerRound != 1 || lbRecConfirm {
		t.Fatal("edge: exit-only keys on")
	}

	ApplyDefaults(false)
	if lbFillPct != 25 || lbSparePerRound != 3 || !lbRecConfirm {
		t.Fatal("exit: exit-only keys off")
	}
	if stripeAWHeavy != 0 || stripeAWSolo || pickLane != 0 || linkProbation || partialRefill {
		t.Fatal("exit: edge-only keys on")
	}

	os.Setenv("HS2_MCONN_STRIPE", "0")
	os.Setenv("HS2_SMUX_AUTOWIN", "")
	ApplyDefaults(false)
	if stripeK.Load() != 0 || smuxAutoWin != nil {
		t.Fatal("explicit off lost")
	}
	if !mconnLite.Load() {
		t.Fatal("other defaults lost with one key off")
	}

	os.Setenv("HS2_DEFAULTS", "legacy")
	envdef.Deactivate()
	loadEnvKeys()
	tlscarrier.LoadEnvKeys()
	if ApplyDefaults(true) != nil {
		t.Fatal("legacy: defaults applied")
	}
	if mconnOn.Load() || lbAuto2 || tlscarrier.QuietIdle {
		t.Fatal("legacy: defaults in force")
	}
}
