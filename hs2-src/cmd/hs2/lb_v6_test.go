package main

import (
	"reflect"
	"sort"
	"testing"
)

func TestLBSameFamily(t *testing.T) {
	for _, c := range []struct {
		l, r string
		ok   bool
	}{
		{"", "1.2.3.4", true},
		{"", "2a05::1", true},
		{"1.2.3.4", "5.6.7.8", true},
		{"2a01::1", "2a05::1", true},
		{"1.2.3.4", "2a05::1", false},
		{"2a01::1", "5.6.7.8", false},
	} {
		if got := lbSameFamily(c.l, c.r); got != c.ok {
			t.Errorf("lbSameFamily(%q, %q) = %v", c.l, c.r, got)
		}
	}
}

// The kharej side in reverse with an IPv6 address on both sides: v4 pairs with
// v4, v6 with v6, never across.
func TestLBPathsIPv6SameFamily(t *testing.T) {
	fc := fileConfig{Mode: "listen", Carrier: "l3mtcp", Reverse: true, Addr: "81.12.35.149:443", BindLocalIP: "91.107.167.67",
		LB: &lbConfig{Enabled: true, Mode: "auto",
			Iran:   []lbIP{{IP: "81.12.35.149", Weight: 50}, {IP: "2a05:9080:0:603:216:3eff:fefe:94d2", Weight: 50}},
			Kharej: []lbIP{{IP: "91.107.167.67", Weight: 50}, {IP: "2a01:4f8:1c17:f987::1", Weight: 50}}}}
	ps, err := lbPaths(fc)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range ps {
		got = append(got, p.Name()+" "+p.Addr)
	}
	sort.Strings(got)
	want := []string{
		"2a01:4f8:1c17:f987::1>2a05:9080:0:603:216:3eff:fefe:94d2 [2a05:9080:0:603:216:3eff:fefe:94d2]:443",
		"91.107.167.67>81.12.35.149 81.12.35.149:443",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("paths\n got %q\nwant %q", got, want)
	}
}

// The listening side on 0.0.0.0 still listens on its IPv6 addresses one by
// one; on [::] (both families) on none extra; on a v4 address on the others.
func TestLBListenIPsIPv6(t *testing.T) {
	mk := func(addr string) fileConfig {
		return fileConfig{Mode: "dial", Carrier: "l3mtcp", Reverse: true, Addr: addr,
			LB: &lbConfig{Enabled: true, Iran: []lbIP{{IP: "81.12.35.149", Weight: 50}, {IP: "5.57.38.162", Weight: 25},
				{IP: "2a05:9080:0:603:216:3eff:fefe:94d2", Weight: 25}}}}
	}
	if got := lbListenIPs(mk("0.0.0.0:443")); !reflect.DeepEqual(got, []string{"2a05:9080:0:603:216:3eff:fefe:94d2"}) {
		t.Fatalf("0.0.0.0: %q", got)
	}
	if got := lbListenIPs(mk("[::]:443")); got != nil {
		t.Fatalf("[::]: %q", got)
	}
	if got := lbListenIPs(mk("81.12.35.149:443")); !reflect.DeepEqual(got, []string{"5.57.38.162", "2a05:9080:0:603:216:3eff:fefe:94d2"}) {
		t.Fatalf("v4 addr: %q", got)
	}
}
