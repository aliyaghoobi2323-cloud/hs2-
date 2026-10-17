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
