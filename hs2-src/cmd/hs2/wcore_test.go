package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/engine"
)

// The window core's line: the connections whose window grew past the base,
// their windows and the pool; nothing while none has.
func TestStatusWindowsLine(t *testing.T) {
	useTempStatusDir(t)
	cfg := filepath.Join(t.TempDir(), "ir.json")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startStatusWriter(ctx, fileConfig{Mode: "dial", Carrier: "mtcp"}, cfg, func() engine.PoolStats {
		return engine.PoolStats{Links: 8, Target: 8, Min: 2, Max: 64, Phase: "steady", Users: 3,
			FastConns: 3, FastWinMiB: 14.24, PoolMiB: 128}
	})
	var ls liveStatus
	for end := time.Now().Add(3 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if b, err := os.ReadFile(statusPath(cfg)); err == nil && json.Unmarshal(b, &ls) == nil && ls.Users > 0 {
			break
		}
		if time.Now().After(end) {
			t.Fatal("no status file")
		}
	}
	if l := windowsLine(ls); l != "fast connections: 3 (windows 14 MiB of 128 MiB pool)" {
		t.Fatalf("windows line %q", l)
	}
	if l := windowsLine(liveStatus{PoolMiB: 128}); l != "" {
		t.Fatalf("windows line with none: %q", l)
	}
}

// The window core's lab knobs reach the engine.
func TestApplyTuningWcoreKnobs(t *testing.T) {
	on, q, p, stage, fixed, trace := engine.Wcore, engine.WcoreQTarget, engine.WcoreProbeQ, engine.WcoreStage, engine.WcoreFixedWin, engine.WcoreTrace
	pace, burst, upload := engine.WcorePaceCap, engine.WcoreBurstShare, engine.WcoreUpload
	t.Cleanup(func() {
		engine.Wcore, engine.WcoreQTarget, engine.WcoreProbeQ, engine.WcoreStage, engine.WcoreFixedWin, engine.WcoreTrace = on, q, p, stage, fixed, trace
		engine.WcorePaceCap, engine.WcoreBurstShare, engine.WcoreUpload = pace, burst, upload
	})
	if engine.WcoreUpload != 0 {
		t.Fatalf("the exit's core for uploads is on by default (WcoreUpload %d): off until step 3", engine.WcoreUpload)
	}
	t.Setenv("HS2_TUNE_WCORE", "0")
	t.Setenv("HS2_TUNE_WCORE_UPLOAD", "1")
	t.Setenv("HS2_TUNE_WCORE_QTARGET", "12")
	t.Setenv("HS2_TUNE_WCORE_PROBEQ", "18")
	t.Setenv("HS2_TUNE_WCORE_STAGE", "2")
	t.Setenv("HS2_TUNE_WCORE_FIXEDWIN", "3145728")
	t.Setenv("HS2_TUNE_WCORE_TRACE", "/tmp/wcore.jsonl")
	t.Setenv("HS2_TUNE_WCORE_PACECAP", "1.25")
	t.Setenv("HS2_TUNE_WCORE_BURST", "0.55")
	applyTuning()
	if engine.WcorePaceCap != 1.25 || engine.WcoreBurstShare != 0.55 || engine.WcoreUpload != 1 {
		t.Fatalf("lab experiments: pacing cap %g, burst share %g, the exit's core %d", engine.WcorePaceCap, engine.WcoreBurstShare, engine.WcoreUpload)
	}
	if engine.Wcore != 0 || engine.WcoreQTarget != 12 || engine.WcoreProbeQ != 18 || engine.WcoreStage != 2 ||
		engine.WcoreFixedWin != 3<<20 || engine.WcoreTrace != "/tmp/wcore.jsonl" {
		t.Fatalf("knobs: on %d, qTarget %d, probeQ %d, stage %d, fixed %d, trace %q", engine.Wcore, engine.WcoreQTarget,
			engine.WcoreProbeQ, engine.WcoreStage, engine.WcoreFixedWin, engine.WcoreTrace)
	}
}

// The exit's pacing net is on by default; its knobs reach the engine
// (HS2_TUNE_PACENET=0 turns it off).
func TestApplyTuningPaceNetKnobs(t *testing.T) {
	on, k, loss := engine.PaceNet, engine.PaceNetK, engine.PaceNetLoss
	t.Cleanup(func() { engine.PaceNet, engine.PaceNetK, engine.PaceNetLoss = on, k, loss })
	if engine.PaceNet != 1 {
		t.Fatalf("the pacing net is off by default (PaceNet %d)", engine.PaceNet)
	}
	t.Setenv("HS2_TUNE_PACENET", "0")
	t.Setenv("HS2_TUNE_PACENET_K", "1.25")
	t.Setenv("HS2_TUNE_PACENET_LOSS", "0.01")
	applyTuning()
	if engine.PaceNet != 0 || engine.PaceNetK != 1.25 || engine.PaceNetLoss != 0.01 {
		t.Fatalf("pacing net %d, k %g, loss %g", engine.PaceNet, engine.PaceNetK, engine.PaceNetLoss)
	}
}
