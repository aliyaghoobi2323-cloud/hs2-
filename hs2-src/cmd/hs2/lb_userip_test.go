package main

import "testing"

// rev53a: --ip of hs2 lb add-ip/remove-ip/park/unpark: IPv6 in brackets (as
// written in addr) is accepted and canonicalised; a zone, the unspecified
// address and a non-IP are refused (park/unpark used the IP "<nil>").
func TestLBUserIP(t *testing.T) {
	for in, want := range map[string]string{
		"192.0.2.7":        "192.0.2.7",
		" 192.0.2.7 ":      "192.0.2.7",
		"2001:DB8:0:0::1":  "2001:db8::1",
		"[2001:db8::1]":    "2001:db8::1",
		"fd00:10::2":       "fd00:10::2",
		"::ffff:192.0.2.7": "192.0.2.7",
	} {
		if got, ok := userIP(in); !ok || got != want {
			t.Errorf("userIP(%q) = %q,%v; want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "x", "fe80::1%eth0", "[fe80::1%eth0]", "0.0.0.0", "::", "[2001:db8::1", "2001:db8::1]:443", "192.0.2.7:443"} {
		if got, ok := userIP(in); ok {
			t.Errorf("userIP(%q) = %q, accepted", in, got)
		}
	}
}
