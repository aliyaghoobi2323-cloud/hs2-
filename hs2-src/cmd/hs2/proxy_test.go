package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// The proxy flag lives in the Iran server's port_map ("P/proxy", one of its
// user ports), with proxy_from: hs2 check validates both and says where they
// have no effect.
func TestCheckProxyPorts(t *testing.T) {
	cases := []struct {
		name, cfg      string
		wantErr, wantW string
	}{
		{"iran proxy port", fmt.Sprintf(iranCfg, `, "port_map": "8443/proxy"`), "", ""},
		{"iran proxy port and proxy_from", fmt.Sprintf(iranCfg, `, "port_map": "8443/proxy", "proxy_from": "127.0.0.1, 10.0.0.0/8"`), "", ""},
		{"not a user port", fmt.Sprintf(iranCfg, `, "port_map": "2053/proxy"`), "", `2053/proxy is not one of this server's user ports`},
		{"unknown flag", fmt.Sprintf(iranCfg, `, "port_map": "8443/tls"`), `unknown flag "tls"`, ""},
		{"a target on iran", fmt.Sprintf(iranCfg, `, "port_map": "8443/proxy,2053"`), "", "no effect on the Iran server"},
		{"bad proxy_from", fmt.Sprintf(iranCfg, `, "port_map": "8443/proxy", "proxy_from": "10.0.0.0/33"`), `"proxy_from":`, ""},
		{"proxy_from without a proxy port", fmt.Sprintf(iranCfg, `, "proxy_from": "10.0.0.1"`), "", `"proxy_from" has no effect`},
		{"proxy flag on kharej", kharejCfg(t, `, "expose": "127.0.0.1:8443", "port_map": "8443/proxy"`), "", "has no effect on the kharej server"},
	}
	for _, c := range cases {
		errs, warns := checkConfig([]byte(c.cfg), localIs("1.2.3.4"), time.Now())
		all, allW := strings.Join(errs, "\n"), strings.Join(warns, "\n")
		if c.wantErr == "" && len(errs) > 0 {
			t.Errorf("%s: unexpected errors: %s", c.name, all)
		}
		if c.wantErr != "" && !strings.Contains(all, c.wantErr) {
			t.Errorf("%s: errors %q, want %q", c.name, all, c.wantErr)
		}
		if c.wantW == "" && c.wantErr == "" && (strings.Contains(allW, "proxy") || strings.Contains(allW, "port_map")) {
			t.Errorf("%s: unexpected warnings: %s", c.name, allW)
		}
		if c.wantW != "" && !strings.Contains(allW, c.wantW) {
			t.Errorf("%s: warnings %q, want %q", c.name, allW, c.wantW)
		}
	}
}
