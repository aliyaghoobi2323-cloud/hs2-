package main

import (
	"strings"
	"testing"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/engine"
)

func TestLBPmtuText135(t *testing.T) {
	p := engine.LBPathView{Local: "1.1.1.1", Remote: "2.2.2.2",
		PmtuDown: &engine.LBPmtu{Min: 1500, Med: 1500, Links: 4, Usual: 1500},
		PmtuUp:   &engine.LBPmtu{Min: 1200, Med: 1300, Links: 4, Usual: 1500, Low: 4, Drops: 1, LastDrop: 30}}
	s := lbPmtuText(p)
	if !strings.Contains(s, "↓1500 ↑1300") || !strings.Contains(s, "min 1200") || !strings.Contains(s, "dropped 30s ago") {
		t.Fatalf("text %q", s)
	}
	if lbPmtuText(engine.LBPathView{}) != "" {
		t.Fatal("no MTU should give no text")
	}
	fs := lbPmtuFindings([]namedStatus{{ls: liveStatus{LBPaths: []engine.LBPathView{p}}}})
	if len(fs) != 1 || fs[0].Sev != "WARN" {
		t.Fatalf("findings %+v", fs)
	}
	p.PmtuUp = &engine.LBPmtu{Min: 1500, Med: 1500, Links: 4, Usual: 1500}
	fs = lbPmtuFindings([]namedStatus{{ls: liveStatus{LBPaths: []engine.LBPathView{p}}}})
	if len(fs) != 1 || fs[0].Sev != "OK" {
		t.Fatalf("findings ok %+v", fs)
	}
}
