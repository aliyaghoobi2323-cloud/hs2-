package main

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

// sniLookup resolves a name (tests replace it).
var sniLookup = func(ctx context.Context, host string) ([]string, error) {
	return net.DefaultResolver.LookupHost(ctx, host)
}

// sniResolveWarnings (reality phase 4): every listening lb IP that serves our
// tls front (kind tls or "both") should be where its sni resolves. A name
// that resolves elsewhere is a correlation a prober can make (the name's real
// servers vs this IP serving its certificate), and a browser following the
// name never lands here. Wildcards and IP literals are skipped; a failed
// lookup is reported as such.
func sniResolveWarnings(ctx context.Context, fc fileConfig, lookup func(context.Context, string) ([]string, error)) []string {
	var out []string
	cache := map[string][]string{}
	for _, e := range lbLocalList(fc) {
		ip := net.ParseIP(e.IP)
		if ip == nil {
			continue
		}
		if k := e.Front.NormKind(); !e.Front.IsDefault() && k != tlscarrier.FrontTLS && k != tlscarrier.FrontBoth {
			continue
		}
		sni := e.SNI
		if sni == "" {
			sni = fc.SNI
		}
		sni = strings.TrimSuffix(strings.TrimSpace(sni), ".")
		if sni == "" || strings.HasPrefix(sni, "*.") || net.ParseIP(sni) != nil {
			continue
		}
		addrs, ok := cache[sni]
		if !ok {
			lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			a, err := lookup(lctx, sni)
			cancel()
			if err != nil {
				out = append(out, fmt.Sprintf("%s: our tls sni %q does not resolve (%v): it should resolve to this IP", e.IP, sni, err))
				cache[sni] = nil
				continue
			}
			addrs, cache[sni] = a, a
		}
		if addrs == nil {
			continue
		}
		found := false
		for _, a := range addrs {
			if x := net.ParseIP(a); x != nil && x.Equal(ip) {
				found = true
				break
			}
		}
		if !found {
			out = append(out, fmt.Sprintf("%s: our tls sni %q resolves to %s, not to this IP: point its DNS here (a prober can compare the name's addresses with the IP serving its certificate)", e.IP, sni, strings.Join(addrs, ",")))
		}
	}
	return out
}

// warnSNIResolve logs sniResolveWarnings in the background (config load and
// reload never wait on DNS).
func warnSNIResolve(fc fileConfig, logf func(string, ...any)) {
	if logf == nil || len(lbLocalList(fc)) == 0 {
		return
	}
	look := sniLookup
	go func() {
		for _, w := range sniResolveWarnings(context.Background(), fc, look) {
			logf("front: WARNING %s", w)
		}
	}()
}

// checkFrontSNI is the doctor's view of sniResolveWarnings.
func checkFrontSNI(d *doctorReport, fc fileConfig) {
	if len(lbLocalList(fc)) == 0 || !tunnelListens(fc) {
		return
	}
	ws := sniResolveWarnings(context.Background(), fc, sniLookup)
	if len(ws) == 0 {
		d.ok("front sni dns", "every tls/both IP's sni resolves to it")
		return
	}
	for _, w := range ws {
		d.warn("front sni dns", w)
	}
}
