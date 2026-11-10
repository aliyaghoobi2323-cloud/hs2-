package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/engine"
	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

// A per-IP "front" in the lb lists: tls (or absent) is today's behaviour and
// is never announced; a front this build lacks is refused at config load.
func TestLBFrontConfigPlumbing(t *testing.T) {
	raw := `{"mode":"listen","addr":"10.0.0.1:443","lb":{"enabled":true,
	  "kharej":[{"ip":"10.0.0.1","weight":50},{"ip":"10.0.0.2","weight":50,"front":{"kind":"tls"}}],
	  "iran":[{"ip":"10.9.0.1","weight":100}]}}`
	var fc fileConfig
	if err := json.Unmarshal([]byte(raw), &fc); err != nil {
		t.Fatal(err)
	}
	adv := lbOwnAdvert(fc)
	if len(adv) != 2 {
		t.Fatalf("advert %v", adv)
	}
	for _, e := range adv {
		if e.Front != nil {
			t.Fatalf("a tls front was announced: %v", adv)
		}
	}
	if f := lbFrontOf(fc.LB.Kharej, "10.0.0.2"); f == nil || f.NormKind() != "tls" {
		t.Fatalf("lbFrontOf %+v", f)
	}
	// dialing side: a path carries the announced front (else its own list's)
	fd := fc
	fd.Mode = "dial"
	lbc := *fc.LB
	lbc.Iran = nil // the dialing side: its default source only
	fd.LB = &lbc
	paths, err := lbPathsAdvert(fd, []engine.LBAdvertIP{{IP: "10.0.0.1", Weight: 50},
		{IP: "10.0.0.2", Weight: 50, Front: &tlscarrier.FrontProfile{Kind: "tls", Target: "x:443"}}})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, p := range paths {
		if p.Remote == "10.0.0.2" {
			if p.Front == nil || p.Front.Target != "x:443" {
				t.Fatalf("path %s front %+v", p.Name(), p.Front)
			}
			n++
		} else if p.Front != nil {
			t.Fatalf("path %s front %+v", p.Name(), p.Front)
		}
	}
	if n == 0 {
		t.Fatalf("no path to 10.0.0.2: %v", paths)
	}
	// phase 1: reality is refused in the config
	bad := strings.Replace(raw, `"front":{"kind":"tls"}`, `"front":{"kind":"reality"}`, 1)
	var fb fileConfig
	json.Unmarshal([]byte(bad), &fb)
	if _, _, err := lbSideWeights("local", fb.LB.Kharej, "10.0.0.1", true); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("reality accepted: %v", err)
	}
	// and a path with an unsupported front gets a dialer that always fails
	d := lbPathDialer(engine.LBPath{Addr: "10.0.0.2:443", Front: &tlscarrier.FrontProfile{Kind: "reality"}}, "", []byte("k"))
	if _, err := d.DialLink(t.Context()); err == nil {
		t.Fatal("an unsupported front dialed")
	}
}
