package main

import (
	"fmt"
	"testing"
	"time"
)

// R1's config fields pass hs2 check with neither an error nor a warning,
// all at once as the installer writes them: on the Iran server "port_map"
// with a user port's proxy flag and "proxy_from" (PROXY protocol, R1 task
// 12), on the kharej server "port_map" targets, and on both the "link"
// block the installer keeps for `hs2 link` (C15, task 13).
func TestCheckAcceptsR1Fields(t *testing.T) {
	link := `, "link": {"endpoint": "edge.example:2096", "domain": "t.example", "transport": "tcp", "udp": false, "panel": "8443"}`
	cases := map[string]string{
		"iran":   fmt.Sprintf(iranCfg, `, "port_map": "8443/proxy", "proxy_from": "127.0.0.1, 10.0.0.0/8"`+link),
		"kharej": kharejCfg(t, `, "expose": "127.0.0.1:8443", "port_map": "8443,2053=127.0.0.1:2096"`+link),
	}
	for name, cfg := range cases {
		errs, warns := checkConfig([]byte(cfg), localIs("1.2.3.4"), time.Now())
		if len(errs)+len(warns) != 0 {
			t.Errorf("%s: errors %q, warnings %q", name, errs, warns)
		}
	}
}
