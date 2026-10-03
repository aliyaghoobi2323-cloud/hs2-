package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/engine"
)

// The Kharej side writes its own counts into the status file, and hs2 status
// / hs2 doctor show them; a file from an older hs2 says where they live.
func TestKharejStatusCarriesItsOwnCounts(t *testing.T) {
	useTempStatusDir(t)
	cfg := filepath.Join(t.TempDir(), "kh.json")
	fc := fileConfig{Mode: "listen", Reverse: true, Carrier: "mtcp"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startStatusWriter(ctx, fc, cfg, func() engine.PoolStats {
		return engine.PoolStats{Links: 64, Target: 64, Min: 2, Max: 300, Phase: "following",
			Users: 5892, Flowing: 512, MbitPerS: 183.24, PeakMbit: 201.51, Counted: true}
	})
	var ls liveStatus
	deadline := time.Now().Add(3 * time.Second)
	for {
		if b, err := os.ReadFile(statusPath(cfg)); err == nil && json.Unmarshal(b, &ls) == nil && ls.Users > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no status file")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !ls.Counted || ls.Users != 5892 || ls.Flowing != 512 || ls.Mbit != 183.2 || ls.PeakMbit != 201.5 {
		t.Fatalf("status: %+v", ls)
	}
	if ls.Serving != nil {
		t.Fatal("the exit wrote the edge's pool detail")
	}
	line := trafficLine(ls)
	if line != "5892 connections (512 active) · 183.2 Mbit/s (peak 201.5 in the last minute)" {
		t.Fatalf("traffic line: %q", line)
	}
	d := &doctorReport{}
	checkRunning(d, cfg)
	if s := strings.Join(d.lines, ""); !strings.Contains(s, "64 link(s) up · 5892 connections (512 active) · 183.2 Mbit/s") {
		t.Fatalf("doctor: %v", d.lines)
	}
	// An older hs2 on the Kharej server: no counts — say where they are.
	old := liveStatus{Role: "Kharej side", Links: 64}
	if l := trafficLine(old); !strings.Contains(l, "counted on the Iran server") {
		t.Fatalf("older Kharej: %q", l)
	}
	// An older Iran file is shown as it always was.
	if l := trafficLine(liveStatus{Role: "Iran side", Users: 10, Mbit: 1}); !strings.HasPrefix(l, "10 connections · 1.0 Mbit/s") {
		t.Fatalf("older Iran: %q", l)
	}
}
