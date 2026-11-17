package main

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
)

// rt80: tunnel 2's classic layout — unit hs2-out.service runs
// /etc/hs2/hs2-out.json; `tunnel list` shows hs2-out.
func TestClassicHs2PrefixedTunnelResolves(t *testing.T) {
	menuEnv(t)
	os.MkdirAll(systemdDir, 0o755)
	cfg := filepath.Join(hs2CfgDir, "hs2-out.json")
	os.WriteFile(cfg, []byte(`{"mode":"dial","addr":"203.0.113.43:2096","reverse":true,"carrier":"l3mtcp"}`), 0o600)
	os.WriteFile(filepath.Join(systemdDir, "hs2-out.service"), []byte("[Service]\nExecStart=/usr/local/bin/hs2 run -c "+cfg+"\n"), 0o644)
	for _, in := range []string{"hs2-out", "hs2-out.service", "hs2-hs2-out", "out", " hs2-out "} {
		u := canonicalUnit(in)
		if u != "hs2-out" {
			t.Fatalf("canonicalUnit(%q) = %q, want hs2-out", in, u)
		}
		if p := configForUnit(u); p != cfg {
			t.Fatalf("configForUnit(%q) = %q, want %q", u, p, cfg)
		}
	}
	rows := tunnelRows(hs2CfgDir, nil)
	if len(rows) != 1 || rows[0].Unit != "hs2-out" || configForUnit(rows[0].Unit) != cfg {
		t.Fatalf("tunnel list / config disagree: %+v", rows)
	}
	// the flag resolves itself
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	u := unitFlag(fs, "hs2", "tunnel")
	if err := fs.Parse([]string{"-u", "hs2-hs2-out"}); err != nil || *u != "hs2-out" {
		t.Fatalf("-u hs2-hs2-out -> %q (%v)", *u, err)
	}
}

// Without a unit file (unit not installed yet) the config is still found.
func TestClassicConfigWithoutUnitFile(t *testing.T) {
	menuEnv(t)
	cfg := filepath.Join(hs2CfgDir, "hs2-out.json")
	os.WriteFile(cfg, []byte(`{"mode":"listen","addr":"203.0.113.43:2096"}`), 0o600)
	if canonicalUnit("hs2-hs2-out") != "hs2-out" || configForUnit("hs2-out") != cfg {
		t.Fatal("classic config not resolved without a unit file")
	}
	// the modern layout is unchanged
	m := filepath.Join(hs2CfgDir, "de1.json")
	os.WriteFile(m, []byte(`{"mode":"listen","addr":"0.0.0.0:443"}`), 0o600)
	if canonicalUnit("hs2-de1") != "hs2-de1" || configForUnit("hs2-de1") != m || canonicalUnit("de1") != "hs2-de1" {
		t.Fatal("modern layout broke")
	}
	if configForUnit("hs2") != filepath.Join(hs2CfgDir, "config.json") || configForUnit("hs2-nope") != filepath.Join(hs2CfgDir, "nope.json") {
		t.Fatal("defaults changed")
	}
}

// A drop-in's ExecStart (last one wins) names the config.
func TestUnitExecConfigDropin(t *testing.T) {
	menuEnv(t)
	d := filepath.Join(systemdDir, "hs2-z.service.d")
	os.MkdirAll(d, 0o755)
	a := filepath.Join(hs2CfgDir, "a.json")
	b := filepath.Join(hs2CfgDir, "b.json")
	os.WriteFile(a, []byte(`{"mode":"listen"}`), 0o600)
	os.WriteFile(b, []byte(`{"mode":"listen"}`), 0o600)
	os.WriteFile(filepath.Join(systemdDir, "hs2-z.service"), []byte("[Service]\nExecStart=/bin/hs2 run -c "+a+"\n"), 0o644)
	os.WriteFile(filepath.Join(d, "10-x.conf"), []byte("[Service]\nExecStart=\nExecStart=/opt/hs2 run -c "+b+"\n"), 0o644)
	if p := configForUnit("hs2-z"); p != b {
		t.Fatalf("got %q want %q", p, b)
	}
	if p, ok := execConfigOf("ExecStart=/bin/hs2 alert -c /x.json"); !ok || p != "" {
		t.Fatal("alert line taken as a tunnel config")
	}
}

// rt80 bug 2: the code's endpoint is the tunnel IP, never the default IP.
func TestCodeEndpointIsTunnelIP(t *testing.T) {
	iran := fileConfig{Mode: "dial", Reverse: true, Addr: "203.0.113.43:2096", Carrier: "l3mtcp"}
	c := codeFor(specFromConfig("hs2-out", iran, "203.0.113.41"))
	if c.Endpoint != "203.0.113.43:2096" {
		t.Fatalf("iran listener endpoint %q", c.Endpoint)
	}
	// wildcard listener with an lb list: the lb IP
	w := iran
	w.Addr = "0.0.0.0:2096"
	w.LB = &lbConfig{Enabled: true, Iran: []lbIP{{IP: "203.0.113.43", Weight: 100}}}
	if c := codeFor(specFromConfig("hs2-out", w, "203.0.113.41")); c.Endpoint != "203.0.113.43:2096" {
		t.Fatalf("wildcard+lb endpoint %q", c.Endpoint)
	}
	// wildcard, no lb: the public IP (as before)
	w.LB = nil
	if c := codeFor(specFromConfig("hs2-out", w, "203.0.113.41")); c.Endpoint != "203.0.113.41:2096" {
		t.Fatalf("wildcard endpoint %q", c.Endpoint)
	}
	// private bind (1:1 NAT): the public IP
	p := iran
	p.Addr = "10.0.0.5:2096"
	if c := codeFor(specFromConfig("hs2-out", p, "203.0.113.41")); c.Endpoint != "203.0.113.41:2096" {
		t.Fatalf("NAT endpoint %q", c.Endpoint)
	}
	// kharej dialer: endpoint = its peer, dialer = its bind_local_ip
	k := fileConfig{Mode: "listen", Reverse: true, Addr: "203.0.113.43:2096", BindLocalIP: "91.1.2.3", Carrier: "l3mtcp"}
	c = codeFor(specFromConfig("hs2-out", k, "91.9.9.9"))
	if c.Endpoint != "203.0.113.43:2096" || c.Dialer != "91.1.2.3" {
		t.Fatalf("kharej code %+v", c)
	}
	// the joiner (kharej) keeps the tunnel IP as its peer
	s := specFromCode(c, tunHostEnv{})
	_ = s
	ci := codeFor(specFromConfig("hs2-out", iran, "203.0.113.41"))
	if sk := specFromCode(ci, tunHostEnv{}); sk.Peer != "203.0.113.43" {
		t.Fatalf("joiner peer %q", sk.Peer)
	}
}

func TestPeerMoveWarn(t *testing.T) {
	if peerMoveWarn("1.1.1.1:2096", "1.1.1.1:2096", nil) != "" {
		t.Fatal("same peer warned")
	}
	if peerMoveWarn("203.0.113.43:2096", "203.0.113.41:2096", nil) == "" {
		t.Fatal("move not warned")
	}
}
