package main

import (
	"strings"
	"testing"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

func TestValidateFront(t *testing.T) {
	priv, pub, _ := tlscarrier.RealityKeygen()
	lis := func(target string, names ...string) fileConfig {
		return fileConfig{Mode: "listen", Carrier: "l3mtcp", Front: &tlscarrier.FrontProfile{Kind: "reality",
			Target: target, ServerNames: names, PrivateKey: priv, ShortIDs: []string{"a1"}}}
	}
	if err := validateFront(lis("www.site.ir:443", "www.site.ir")); err != nil {
		t.Fatal(err)
	}
	if err := validateFront(fileConfig{Mode: "dial", Carrier: "mtcp"}); err != nil {
		t.Fatal("tls default", err)
	}
	for name, fc := range map[string]fileConfig{
		"cloudflare": lis("www.cloudflare.com:443", "www.cloudflare.com"),
		"cdn name":   lis("1.2.3.4:443", "x.fastly.net"),
		"names":      lis("www.site.ir:443", "other.ir"),
		"noise":      {Mode: "listen", Carrier: "noise", Front: lis("a.ir:443", "a.ir").Front},
		"dial no pub": {Mode: "dial", Carrier: "mtcp", Front: &tlscarrier.FrontProfile{Kind: "reality",
			ServerNames: []string{"a.ir"}, PrivateKey: priv, ShortIDs: []string{"a1"}, Target: "a.ir:443"}},
	} {
		if err := validateFront(fc); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	forced := lis("www.cloudflare.com:443", "www.cloudflare.com")
	forced.Front.ForceTarget = true
	if err := validateFront(forced); err != nil {
		t.Fatal("forced:", err)
	}
	// reverse: the Iran side (mode dial) listens and needs the private key
	rev := fileConfig{Mode: "dial", Reverse: true, Carrier: "l3mtcp", Front: &tlscarrier.FrontProfile{Kind: "reality",
		ServerNames: []string{"a.ir"}, PublicKey: pub, ShortIDs: []string{"a1"}}}
	if err := validateFront(rev); err == nil || !strings.Contains(err.Error(), "private_key") {
		t.Fatalf("reverse iran without private key: %v", err)
	}
	if tunnelFront(fileConfig{}) != nil || tunnelFront(rev) == nil {
		t.Fatal("tunnelFront")
	}
}

// The private key comes from this side's config file (and only from there).
func TestFrontPrivateKeyFromConfig(t *testing.T) {
	var fc fileConfig
	if err := fc.UnmarshalJSON([]byte(`{"mode":"listen","front":{"kind":"reality","private_key":"PRIV","public_key":"PUB"}}`)); err != nil {
		t.Fatal(err)
	}
	if fc.Front == nil || fc.Front.PrivateKey != "PRIV" || fc.Front.PublicKey != "PUB" {
		t.Fatalf("%+v", fc.Front)
	}
}
