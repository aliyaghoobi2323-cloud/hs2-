package main

import (
	"fmt"
	"testing"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/engine"
)

// rev53c: the production LB (3 kharej IPv4 + 1 IPv6 against 6 Iran IPv4 + 1
// IPv6: 19 paths) on a host with two tunnels. The deep doctor pinged only the
// first 8 paths in status order — kharej IP 3, the IPv6 path and the second
// tunnel were never checked.
func TestDeepPathsCoverEveryAddress(t *testing.T) {
	lb := func(iranBase int) []engine.LBPathView {
		var v []engine.LBPathView
		for _, k := range []string{"10.30.0.1", "10.30.0.2", "10.30.0.3"} {
			for i := 0; i < 6; i++ {
				v = append(v, engine.LBPathView{Local: k, Remote: fmt.Sprintf("10.30.%d.%d:2096", iranBase, 11+i)})
			}
		}
		return append(v, engine.LBPathView{Local: "fd30::1", Remote: fmt.Sprintf("[fd3%d::11]:2096", iranBase)})
	}
	st := []namedStatus{
		{file: "etc-hs2-config.json", ls: liveStatus{LBPaths: lb(0)}},
		{file: "etc-hs2-t2.json", ls: liveStatus{LBPaths: lb(1)}},
	}
	paths := collectPaths(&deepEnv{}, st)
	if len(paths) > pathProbeMax {
		t.Fatalf("%d probes, cap %d", len(paths), pathProbeMax)
	}
	got := map[string]bool{}
	for _, p := range paths {
		got[p.local], got[p.remote] = true, true
	}
	for _, want := range []string{"10.30.0.3", "fd30::1", "fd30::11", "fd31::11", "10.30.1.16", "10.30.0.16"} {
		if !got[want] {
			t.Errorf("%s never probed (%d probes)", want, len(paths))
		}
	}
	// a small setup keeps every path, in order
	one := collectPaths(&deepEnv{}, st[:1])
	if len(one) != 19 || one[0].remote != "10.30.0.11" || one[18].remote != "fd30::11" {
		t.Fatalf("19-path LB: %d probes", len(one))
	}
}
