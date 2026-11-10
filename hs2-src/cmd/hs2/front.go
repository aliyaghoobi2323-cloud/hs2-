package main

import (
	"net"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/engine"
	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

// Link fronts (reality phase 1): the per-IP "front" of the lb lists and the
// dial side of a path. Only the "tls" front exists yet; a nil / default
// profile is exactly today's behaviour (see tlscarrier/front.go).

// lbFrontOf is the front profile of ip in list (nil: none set = tls).
func lbFrontOf(list []lbIP, ip string) *tlscarrier.FrontProfile {
	for _, e := range list {
		if x := net.ParseIP(e.IP); x != nil && x.String() == ip {
			return e.Front
		}
	}
	return nil
}

// lbAnnouncedFront is what the listening side announces for its own ip:
// nothing (nil) for the default tls front, so the advert keeps its stage-6
// bytes.
func lbAnnouncedFront(own []lbIP, ip string) *tlscarrier.FrontProfile {
	f := lbFrontOf(own, ip)
	if f.IsDefault() {
		return nil
	}
	return f
}

// lbPathDialer is the edge's link dialer for one lb path over its front. A
// path whose front this build lacks gets a dialer that always fails (the path
// cools like a dead one) instead of silently dialing the wrong front.
func lbPathDialer(p engine.LBPath, sni string, key []byte) engine.LinkDialer {
	d, err := engine.NewMTCPDialerFront(p.Addr, sni, key, p.Local, p.Front)
	if err != nil {
		return engine.FailingDialer(err)
	}
	return d
}
