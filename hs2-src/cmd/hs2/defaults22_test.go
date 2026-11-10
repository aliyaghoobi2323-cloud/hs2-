package main

import "testing"

func TestDefaultsApply(t *testing.T) {
	for _, c := range []struct {
		carrier string
		reverse bool
		want    bool
	}{
		{"l3mtcp", true, true}, {"l3", true, true}, {"l3mtcp", false, false},
		{"mtcp", true, false}, {"tls", true, false}, {"dgtun", true, false}, {"udp", true, false},
	} {
		if got := defaultsApply(fileConfig{Carrier: c.carrier, Reverse: c.reverse}); got != c.want {
			t.Errorf("%s reverse=%v: %v, want %v", c.carrier, c.reverse, got, c.want)
		}
	}
}
