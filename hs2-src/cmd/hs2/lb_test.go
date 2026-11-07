package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func lbFC(t *testing.T, js string) fileConfig {
	t.Helper()
	var fc fileConfig
	if err := json.Unmarshal([]byte(js), &fc); err != nil {
		t.Fatal(err)
	}
	return fc
}

const lbKharejRev = `{"mode":"listen","reverse":true,"carrier":"mtcp","addr":"10.1.0.1:443","bind_local_ip":"10.0.0.1",
 "lb":{"enabled":true,"iran":[{"ip":"10.1.0.1","weight":70},{"ip":"10.1.0.2","weight":30}],
       "kharej":[{"ip":"10.0.0.1","weight":100}]}}`

func TestLBPathsReverseExit(t *testing.T) {
	paths, err := lbPaths(lbFC(t, lbKharejRev))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 || !paths[0].Primary || paths[0].Addr != "10.1.0.1:443" || paths[0].Local != "10.0.0.1" ||
		paths[1].Addr != "10.1.0.2:443" || paths[0].Weight != 7000 || paths[1].Weight != 3000 {
		t.Fatalf("paths %+v", paths)
	}
}

func TestLBValidation(t *testing.T) {
	cases := []struct{ js, want string }{
		{strings.Replace(lbKharejRev, `"weight":30`, `"weight":20`, 1), "sum to 90"},
		{strings.Replace(lbKharejRev, `"10.1.0.2"`, `"10.1.0.1"`, 1), "listed twice"},
		{strings.Replace(lbKharejRev, `"10.1.0.2"`, `"nope"`, 1), "not a valid IP"},
		// primary not listed: it joins with an equal share (50), the rest must be 50
		{strings.Replace(lbKharejRev, `{"ip":"10.1.0.1","weight":70},`, ``, 1), "need 50"},
		{strings.Replace(lbKharejRev, `"enabled":true`, `"enabled":true,"mode":"bogus"`, 1), `must be "fixed" or "auto"`},
		{strings.Replace(lbKharejRev, `"carrier":"mtcp"`, `"carrier":"udp"`, 1), "mtcp/l3mtcp"},
	}
	for _, c := range cases {
		err := validateLB(lbFC(t, c.js))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("want %q, got %v", c.want, err)
		}
	}
	if err := validateLB(lbFC(t, strings.Replace(lbKharejRev, `"enabled":true`, `"enabled":true,"mode":"auto"`, 1))); err != nil {
		t.Fatalf("mode auto: %v", err)
	}
	// primary not listed, the rest leaving its equal share: ok
	ok := strings.Replace(lbKharejRev, `{"ip":"10.1.0.1","weight":70},{"ip":"10.1.0.2","weight":30}`, `{"ip":"10.1.0.2","weight":50}`, 1)
	paths, err := lbPaths(lbFC(t, ok))
	if err != nil || len(paths) != 2 || !paths[0].Primary || paths[0].Weight != paths[1].Weight {
		t.Fatalf("implicit primary: %+v %v", paths, err)
	}
}

// The listening iran side (reverse) listens on its extra IPs; with a wildcard
// addr it needs none; its list need not repeat the dialing side's exactness.
func TestLBListenIPs(t *testing.T) {
	edge := `{"mode":"dial","reverse":true,"carrier":"mtcp","addr":"10.1.0.1:443",
 "lb":{"enabled":true,"iran":[{"ip":"10.1.0.1","weight":70},{"ip":"10.1.0.2","weight":30}]}}`
	fc := lbFC(t, edge)
	if err := validateLB(fc); err != nil {
		t.Fatal(err)
	}
	if ips := lbListenIPs(fc); len(ips) != 1 || ips[0] != "10.1.0.2" {
		t.Fatalf("listen %v", ips)
	}
	fc = lbFC(t, strings.Replace(edge, "10.1.0.1:443", "0.0.0.0:443", 1))
	if err := validateLB(fc); err != nil || lbListenIPs(fc) != nil {
		t.Fatalf("wildcard: %v %v", err, lbListenIPs(fc))
	}
}

// Without lb (absent or disabled) nothing changes.
func TestLBOffIsInert(t *testing.T) {
	for _, js := range []string{
		`{"mode":"listen","reverse":true,"carrier":"mtcp","addr":"10.1.0.1:443"}`,
		strings.Replace(lbKharejRev, `"enabled":true`, `"enabled":false`, 1),
	} {
		fc := lbFC(t, js)
		if lbOn(fc) || validateLB(fc) != nil || lbListenIPs(fc) != nil {
			t.Fatalf("lb not inert for %s", js)
		}
	}
}
