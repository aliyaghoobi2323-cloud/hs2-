package engine

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"testing"
)

// m4f251 (audit246 M4): 600 accepted links from a handful of edge addresses
// are one filter per address; dialed links stay keyed by the peer's port.
func TestQKeySetM4Grouped(t *testing.T) {
	listen := netip.MustParseAddrPort("10.0.0.1:8443")
	var s qKeySet
	for i := 0; i < 600; i++ {
		peer := netip.AddrPortFrom(netip.AddrFrom4([4]byte{10, 1, 0, byte(1 + i%16)}), uint16(30000+i))
		s.add(listen, peer, true)
	}
	if len(s.keys) != 16 {
		t.Fatalf("600 accepted links from 16 addresses: %d keys, want 16", len(s.keys))
	}
	for _, k := range s.keys {
		if !k.sport || k.port != 8443 {
			t.Fatalf("accepted key %v: want sport 8443", k)
		}
	}
	// 600 from 2 addresses (the reverse edge: the kharej's 1-4 IPs)
	var r qKeySet
	for i := 0; i < 600; i++ {
		r.add(netip.MustParseAddrPort("10.0.0.1:443"), netip.AddrPortFrom(netip.AddrFrom4([4]byte{10, 2, 0, byte(1 + i%2)}), uint16(20000+i)), true)
	}
	if len(r.keys) != 2 {
		t.Fatalf("reverse edge: %d keys, want 2", len(r.keys))
	}
	// dialed: per (address, port), as before: 600 links to 4 exit ports = 4
	var d qKeySet
	for i := 0; i < 600; i++ {
		local := netip.AddrPortFrom(netip.MustParseAddr("10.0.0.9"), uint16(40000+i))
		d.add(local, netip.AddrPortFrom(netip.MustParseAddr("10.3.0.1"), uint16(443+i%4)), false)
	}
	if len(d.keys) != 4 || d.keys[0].sport {
		t.Fatalf("dialed: %d keys %v, want 4 dport keys", len(d.keys), d.keys)
	}
	// churn: new ephemeral peer ports do not change an accepted key set
	before := slices.Clone(s.keys)
	var s2 qKeySet
	for i := 0; i < 600; i++ {
		s2.add(listen, netip.AddrPortFrom(netip.AddrFrom4([4]byte{10, 1, 0, byte(1 + i%16)}), uint16(50000+i)), true)
	}
	if !slices.Equal(before, s2.keys) {
		t.Fatal("link churn changed the accepted key set")
	}
}

func TestQKeyMatch(t *testing.T) {
	v4 := qLinkKey(netip.MustParseAddrPort("10.0.0.1:8443"), netip.MustParseAddrPort("[::ffff:10.1.0.2]:51000"), true)
	if got := v4.match(); got != "match ip dst 10.1.0.2/32 match ip protocol 6 0xff match ip sport 8443 0xffff" {
		t.Fatalf("v4 accepted: %q", got)
	}
	d := qLinkKey(netip.MustParseAddrPort("10.0.0.1:51000"), netip.MustParseAddrPort("10.1.0.2:443"), false)
	if got := d.match(); got != "match ip dst 10.1.0.2/32 match ip dport 443 0xffff" {
		t.Fatalf("v4 dialed (unchanged): %q", got)
	}
	v6 := qLinkKey(netip.MustParseAddrPort("[fd00::1]:8443"), netip.MustParseAddrPort("[fd00::2]:51000"), true)
	if got := v6.match(); !strings.Contains(got, "ip6 protocol 6 0xff match ip6 sport 8443") || !strings.HasPrefix(got, "match ip6 dst fd00::2/128") {
		t.Fatalf("v6 accepted: %q", got)
	}
	// an accepted link whose local address is unknown falls back to dport
	if k := qLinkKey(netip.AddrPort{}, netip.MustParseAddrPort("10.1.0.2:51000"), true); k.sport {
		t.Fatalf("no local port: %v", k)
	}
	// same IP/port as sport vs dport keys are different filters
	a := qKey{ip: netip.MustParseAddr("10.1.0.2"), port: 443}
	b := a
	b.sport = true
	if a.compare(b) == 0 || a.compare(b) != -b.compare(a) {
		t.Fatal("compare ignores sport")
	}
	_ = fmt.Sprint(a, b)
}
