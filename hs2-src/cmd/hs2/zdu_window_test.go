package main

import (
	"strings"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/engine"
	"github.com/hosseintaghipoursori-alt/hs2-tunnel/handoff"
)

// b430: `hs2 zdu-window` answers with the handover's own end-of-hour rule
// (zduHandoverSpan + engine.ZDURefusedSpan), so the script can no longer let
// through what the process then refuses (HH:56:30-HH:57:59 before).
func TestZDUWindowLineMatchesHandover(t *testing.T) {
	if engine.ZDUStormMin() != 1 {
		t.Skip("HS2_ZDU_STORM_MIN set")
	}
	h := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	span := zduHandoverSpan(handoff.ReadyTimeout, zduProbation)
	if span != 150*time.Second {
		t.Fatalf("span %v want 150s", span)
	}
	for s := 0; s < 3600; s++ {
		now := h.Add(time.Duration(s) * time.Second)
		proc := engine.ZDURefused(now) || engine.ZDURefusedSpan(now, span)
		ok := strings.HasPrefix(zduWindowLine(now, 0, handoff.ReadyTimeout, zduProbation), "OK")
		if ok == proc {
			t.Fatalf("HH:%02d:%02d: zdu-window ok=%v, handover refused=%v", s/60, s%60, ok, proc)
		}
		// with slack the answer is never less strict than the process
		if strings.HasPrefix(zduWindowLine(now, 10*time.Second, handoff.ReadyTimeout, zduProbation), "OK") && proc {
			t.Fatalf("HH:%02d:%02d: slack 10 s let through a refused start", s/60, s%60)
		}
	}
	at := func(m, s int) string {
		return zduWindowLine(h.Add(time.Duration(m)*time.Minute+time.Duration(s)*time.Second), 0, handoff.ReadyTimeout, zduProbation)
	}
	if !strings.HasPrefix(at(57, 29), "OK") || !strings.HasPrefix(at(57, 30), "window opens in") ||
		!strings.HasPrefix(at(0, 0), "window HH:00-HH:03 UTC") || !strings.HasPrefix(at(4, 0), "OK") {
		t.Fatalf("57:29 %q 57:30 %q 00:00 %q 04:00 %q", at(57, 29), at(57, 30), at(0, 0), at(4, 0))
	}
}
