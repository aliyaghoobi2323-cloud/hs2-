package main

import (
	"bytes"
	"log"
	"os"
	"strings"
	"testing"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/engine"
)

// An HS2_TUNE_* value that does not parse leaves the switch as it was and
// says so in the log (the R1 review, part C): the switches are the field's
// way back, and "HS2_TUNE_QUIET=off" was dropped without a word, QUIET on.
// A good value is applied and logged as before.
func TestApplyTuningSaysWhatItIgnores(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	quiet, k, early := engine.TuneQuiet, engine.PaceNetK, engine.TuneEarlyDone
	ratio := engine.TwoWayRatio.Get()
	defer func() {
		engine.TuneQuiet, engine.PaceNetK, engine.TuneEarlyDone = quiet, k, early
		engine.TwoWayRatio.Set(ratio)
	}()
	t.Setenv("HS2_TUNE_QUIET", "off")
	t.Setenv("HS2_TUNE_PACENET_K", "x1.5")
	t.Setenv("HS2_TUNE_TWOWAY_RATIO", "16x")
	t.Setenv("HS2_TUNE_EARLYDONE", "0")
	applyTuning()
	if engine.TuneQuiet != quiet || engine.PaceNetK != k || engine.TwoWayRatio.Get() != ratio {
		t.Fatalf("a value that does not parse changed a switch: QUIET %d (was %d), PACENET_K %g (was %g), TWOWAY_RATIO %d (was %d)",
			engine.TuneQuiet, quiet, engine.PaceNetK, k, engine.TwoWayRatio.Get(), ratio)
	}
	if engine.TuneEarlyDone != 0 {
		t.Fatalf("HS2_TUNE_EARLYDONE=0 not applied: %d", engine.TuneEarlyDone)
	}
	out := buf.String()
	for _, want := range []string{
		`tuning: HS2_TUNE_QUIET="off" ignored`,
		`tuning: HS2_TUNE_PACENET_K="x1.5" ignored`,
		`tuning: HS2_TUNE_TWOWAY_RATIO="16x" ignored`,
		"tuning: HS2_TUNE_EARLYDONE=0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
}
