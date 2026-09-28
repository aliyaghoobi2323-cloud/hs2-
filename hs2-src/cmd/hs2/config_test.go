package main

import (
	"strings"
	"testing"
)

const goodKey = "abababababababababababababababababababababababababababababababab"

func iranCfg() fileConfig {
	return fileConfig{Mode: "dial", Carrier: "mtcp", Addr: "203.0.113.5:2096", SNI: "vpn.example.com",
		SharedKey: goodKey, ForwardPorts: "8443,443", MinLinks: 8, MaxLinks: 16, PerLink: 8}
}

func kharejCfg() fileConfig {
	return fileConfig{Mode: "listen", Carrier: "mtcp", Addr: "0.0.0.0:2096", SharedKey: goodKey,
		Expose: "127.0.0.1:8443", CertFile: "c.pem", KeyFile: "k.pem"}
}

func TestValidateAcceptsInstallerConfigs(t *testing.T) {
	for _, fc := range []fileConfig{iranCfg(), kharejCfg()} {
		if err := validate(fc); err != nil {
			t.Fatalf("valid config rejected: %v", err)
		}
	}
	v6 := iranCfg()
	v6.Addr = "[2001:db8::1]:2096"
	v6.BindLocalIP = "2001:db8::2"
	v6.UserListenIP = "192.0.2.10"
	if err := validate(v6); err != nil {
		t.Fatalf("IPv6 config rejected: %v", err)
	}
	k := kharejCfg()
	k.Addr = ":2096" // listen on all addresses
	if err := validate(k); err != nil {
		t.Fatalf("empty listen host rejected: %v", err)
	}
}

func TestValidateRejects(t *testing.T) {
	cases := []struct {
		name string
		mod  func(*fileConfig)
		want string
	}{
		{"key not hex", func(c *fileConfig) { c.SharedKey = "zz" + goodKey[2:] }, "shared_key is not valid hex"},
		{"key odd length (truncated paste)", func(c *fileConfig) { c.SharedKey = goodKey[:63] }, "shared_key is not valid hex"},
		{"key too short", func(c *fileConfig) { c.SharedKey = "abcd" }, "only 2 bytes"},
		{"key missing", func(c *fileConfig) { c.SharedKey = "" }, "shared_key is missing"},
		{"bad mode", func(c *fileConfig) { c.Mode = "client" }, "mode must be"},
		{"bad carrier", func(c *fileConfig) { c.Carrier = "udp" }, "unknown carrier"},
		{"addr no port", func(c *fileConfig) { c.Addr = "203.0.113.5" }, "addr"},
		{"addr port range", func(c *fileConfig) { c.Addr = "203.0.113.5:70000" }, "not 1-65535"},
		{"addr bad host", func(c *fileConfig) { c.Addr = "203.0.113.5.9:2096" }, "neither an IP"},
		{"addr v6 without brackets", func(c *fileConfig) { c.Addr = "2001:db8::1:2096" }, "want host:port"},
		{"dial empty host", func(c *fileConfig) { c.Addr = ":2096" }, "host is missing"},
		{"port letters", func(c *fileConfig) { c.ForwardPorts = "8443,abc" }, `"abc" is not a port`},
		{"port zero", func(c *fileConfig) { c.ForwardPorts = "0" }, `"0" is not a port`},
		{"port leading zero", func(c *fileConfig) { c.ForwardPorts = "0443" }, `"0443" is not a port`},
		{"port duplicate", func(c *fileConfig) { c.ForwardPorts = "443, 443" }, "listed twice"},
		{"ports empty", func(c *fileConfig) { c.ForwardPorts = " , " }, "forward_ports is empty"},
		{"egress ip garbage", func(c *fileConfig) { c.BindLocalIP = "1.2.3" }, "bind_local_ip"},
		{"user ip garbage", func(c *fileConfig) { c.UserListenIP = "all" }, "user_listen_ip"},
		{"sni missing", func(c *fileConfig) { c.SNI = "" }, "sni"},
		{"links inverted", func(c *fileConfig) { c.MinLinks, c.MaxLinks = 20, 16 }, "larger than max_links"},
		{"links huge", func(c *fileConfig) { c.MaxLinks = 1000 }, "unreasonable"},
		{"mtu", func(c *fileConfig) { c.MTU = 100 }, "mtu"},
	}
	for _, tc := range cases {
		fc := iranCfg()
		tc.mod(&fc)
		err := validate(fc)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want error containing %q", tc.name, err, tc.want)
		}
	}
	k := kharejCfg()
	k.Expose = "8443"
	if err := validate(k); err == nil || !strings.Contains(err.Error(), "expose") {
		t.Errorf("bad expose accepted: %v", err)
	}
	k = kharejCfg()
	k.CertFile = ""
	if err := validate(k); err == nil || !strings.Contains(err.Error(), "cert_file") {
		t.Errorf("missing cert accepted: %v", err)
	}
}

// All problems are reported together, not one per restart.
func TestValidateReportsAll(t *testing.T) {
	fc := iranCfg()
	fc.SharedKey, fc.ForwardPorts, fc.BindLocalIP = "x", "0", "nope"
	err := validate(fc)
	if err == nil || strings.Count(err.Error(), "\n  - ") != 3 {
		t.Fatalf("want 3 problems reported, got: %v", err)
	}
}
