package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

// Reality phase 4 item 2: a tls/both IP whose sni resolves elsewhere (or not
// at all) is warned about at load and by the doctor; reality IPs, wildcards
// and correct names are not.
func TestSNIResolveWarnings(t *testing.T) {
	priv, _, _ := tlscarrier.RealityKeygen()
	fc := parseFC(t, lbFrontConf(t, priv, "", ""))
	dns := map[string][]string{"tun.ours.ir": {"10.0.0.7", "10.0.0.1"}, "c.ours.ir": {"10.0.0.9"}}
	look := func(_ context.Context, h string) ([]string, error) {
		if a, ok := dns[h]; ok {
			return a, nil
		}
		return nil, errors.New("no such host")
	}
	ws := sniResolveWarnings(context.Background(), fc, look)
	if len(ws) != 1 || !strings.HasPrefix(ws[0], "10.0.0.3: our tls sni \"c.ours.ir\" resolves to 10.0.0.9, not to this IP") {
		t.Fatalf("warnings %q", ws)
	}
	dns["c.ours.ir"] = []string{"10.0.0.3"}
	if ws := sniResolveWarnings(context.Background(), fc, look); len(ws) != 0 {
		t.Fatalf("correct DNS warned: %q", ws)
	}
	delete(dns, "tun.ours.ir")
	ws = sniResolveWarnings(context.Background(), fc, look)
	if len(ws) != 1 || !strings.Contains(ws[0], "10.0.0.1: our tls sni \"tun.ours.ir\" does not resolve") {
		t.Fatalf("lookup failure: %q", ws)
	}
	fc.SNI = "*.ours.ir" // wildcard: skipped
	if ws := sniResolveWarnings(context.Background(), fc, look); len(ws) != 0 {
		t.Fatalf("wildcard warned: %q", ws)
	}
	// the doctor line
	old := sniLookup
	sniLookup = look
	defer func() { sniLookup = old }()
	fc.SNI = "tun.ours.ir"
	d := &doctorReport{}
	checkFrontSNI(d, fc)
	if d.warns != 1 || !strings.Contains(strings.Join(d.lines, "\n"), "front sni dns") {
		t.Fatalf("doctor: %q", d.lines)
	}
}
