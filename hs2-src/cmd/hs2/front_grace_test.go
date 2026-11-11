package main

import (
	"testing"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

func TestSwitchGrace(t *testing.T) {
	g := realitySwitchGraceDur
	both := &frontEntry{kind: tlscarrier.FrontBoth}
	real := &frontEntry{kind: tlscarrier.FrontReality}
	for i, c := range []struct {
		built bool
		old   *frontEntry
		def   string
		want  bool
	}{
		{false, nil, "tls", false},    // start: no grace
		{true, nil, "tls", true},      // tls -> reality
		{true, nil, "", true},         // tls (default) -> reality
		{true, both, "tls", true},     // both -> reality
		{true, real, "tls", false},    // reality -> reality (key change)
		{true, nil, "reality", false}, // the tunnel's front was reality
		{true, both, "reality", true}, // both on this IP
	} {
		if got := switchGrace(c.built, c.old, c.def) == g && g > 0; got != c.want {
			t.Errorf("case %d: grace %v, want %v", i, got, c.want)
		}
	}
}
