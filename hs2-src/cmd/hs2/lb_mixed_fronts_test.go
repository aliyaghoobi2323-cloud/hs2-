package main

import (
	"encoding/json"
	"testing"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

// rel47: every lb list edit (park, unpark, weights, add-ip, remove-ip) on a
// listening side with MIXED per-IP fronts (A tls, B reality, C both) keeps
// each remaining IP's whole profile, the private key included (lbIP's front
// never marshals it; withOwnList puts the file's back by IP), and the
// announcement follows the list: the profiles of the IPs still listed, by IP,
// public parts only.
func TestLBEditsKeepMixedFronts(t *testing.T) {
	priv, pub, _ := tlscarrier.RealityKeygen()
	raw := []byte(lbFrontConf(t, priv, "", ""))
	fc := parseFC(t, string(raw))

	step := func(name string, b []byte, f func([]lbIP) ([]lbIP, error)) []byte {
		t.Helper()
		var cfc fileConfig
		if err := json.Unmarshal(b, &cfc); err != nil {
			t.Fatal(err)
		}
		nb, err := withOwnList(b, cfc, f)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return nb
	}
	check := func(name string, b []byte, want map[string]string) {
		t.Helper()
		nfc := parseFC(t, string(b))
		if err := validateLB(nfc); err != nil {
			t.Fatalf("%s: the edited config does not validate: %v\n%s", name, err, b)
		}
		got := map[string]string{}
		for _, e := range nfc.LB.Kharej {
			k := "tls"
			if e.Front != nil {
				k = e.Front.Kind
				if e.Front.PrivateKey != priv {
					t.Fatalf("%s: %s lost its private key", name, e.IP)
				}
				if e.Front.Target != "www.site.ir:443" || len(e.Front.ShortIDs) != 1 || len(e.Front.ServerNames) != 1 {
					t.Fatalf("%s: %s profile changed: %+v", name, e.IP, e.Front)
				}
				if e.Front.Kind == "both" && (e.Front.FallbackMaxBytes != 1000 || e.SNI != "c.ours.ir") {
					t.Fatalf("%s: C lost its limits / sni: %+v %q", name, e.Front, e.SNI)
				}
			}
			got[e.IP] = k
		}
		if len(got) != len(want) {
			t.Fatalf("%s: list %v, want %v", name, got, want)
		}
		for ip, k := range want {
			if got[ip] != k {
				t.Fatalf("%s: %s is %q, want %q", name, ip, got[ip], k)
			}
		}
		// the announcement: one entry per listed IP, reality/both with the
		// public key only, tls with none
		adv := lbOwnAdvert(nfc)
		for _, e := range adv {
			w, ok := want[e.IP]
			if !ok {
				continue // the tunnel's primary (addr) when it is not listed
			}
			if w == "tls" {
				if e.Front != nil {
					t.Fatalf("%s: tls IP %s announced %+v", name, e.IP, e.Front)
				}
				continue
			}
			if e.Front == nil || e.Front.Kind != w || e.Front.PublicKey != pub || e.Front.PrivateKey != "" || e.Front.Target != "" {
				t.Fatalf("%s: announced %s = %+v", name, e.IP, e.Front)
			}
		}
		for ip := range want {
			found := false
			for _, e := range adv {
				found = found || e.IP == ip
			}
			if !found {
				t.Fatalf("%s: %s not announced: %+v", name, ip, adv)
			}
		}
	}
	all := map[string]string{"10.0.0.1": "tls", "10.0.0.2": "reality", "10.0.0.3": "both"}

	b := step("park B", raw, func(l []lbIP) ([]lbIP, error) { return parkWeights(l, "10.0.0.2") })
	check("park B", b, all)
	if w := parseFC(t, string(b)).LB.Kharej[1].Weight; w != 0 {
		t.Fatalf("park: B weight %d", w)
	}
	b = step("unpark B", b, func(l []lbIP) ([]lbIP, error) { return unparkWeights(l, fc.LB.Kharej, "10.0.0.2"), nil })
	check("unpark B", b, all)
	if w := parseFC(t, string(b)).LB.Kharej[1].Weight; w != 30 {
		t.Fatalf("unpark: B weight %d", w)
	}
	b = step("weights", b, func(l []lbIP) ([]lbIP, error) {
		l[0].Weight, l[1].Weight, l[2].Weight = 20, 50, 30
		return l, nil
	})
	check("weights", b, all)
	b = step("add-ip D", b, func(l []lbIP) ([]lbIP, error) {
		return addIPList(fc, l, lbIP{IP: "10.0.0.4"}, -1, nil)
	})
	check("add-ip D", b, map[string]string{"10.0.0.1": "tls", "10.0.0.2": "reality", "10.0.0.3": "both", "10.0.0.4": "tls"})
	// removing B shifts C's list index: the keys are matched by IP, not by
	// position (A is the tunnel's primary and stays)
	b = step("remove-ip B", b, func(l []lbIP) ([]lbIP, error) { return removeIP(l, "10.0.0.2") })
	check("remove-ip B", b, map[string]string{"10.0.0.1": "tls", "10.0.0.3": "both", "10.0.0.4": "tls"})
	b = step("remove-ip D", b, func(l []lbIP) ([]lbIP, error) { return removeIP(l, "10.0.0.4") })
	check("remove-ip D", b, map[string]string{"10.0.0.1": "tls", "10.0.0.3": "both"})
}
