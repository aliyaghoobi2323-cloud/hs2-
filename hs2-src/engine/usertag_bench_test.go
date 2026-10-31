package engine

import (
	"net/netip"
	"testing"
)

func BenchmarkUserTag(b *testing.B) {
	ip := netip.MustParseAddr("192.0.2.7")
	for _, sw := range []struct {
		name string
		v    int
	}{{"keyed", 1}, {"unkeyed", 0}} {
		b.Run(sw.name, func(b *testing.B) {
			defer func(v int) { TuneTagKey.Set(v) }(TuneTagKey.Get())
			TuneTagKey.Set(sw.v)
			b.ReportAllocs()
			for b.Loop() {
				UserTag(ip, classV6Prefix)
			}
		})
	}
}
