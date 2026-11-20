package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

// iranRevConf is the production shape of a reverse tunnel's iran listener
// (b411): the iran side listens (mode dial + reverse), its own lb list holds
// the reality canary IP; the kharej list holds only what the menu could
// write on the iran side: the kharej primary (its default-route IP, which the
// iran config never knows) is NOT listed and the kharej IPv6 is "auto6",
// which only kharej resolves.
func iranRevConf(priv, kharej string) string {
	return `{"mode":"dial","reverse":true,"carrier":"l3mtcp","addr":"0.0.0.0:443","sni":"tun.ours.ir","max_links":300,
	  "lb":{"enabled":true,
	  "iran":[{"ip":"10.9.0.1","weight":50},{"ip":"10.9.0.5","weight":50,"front":{"kind":"both","target":"www.site.ir:443","server_names":["www.site.ir"],"private_key":"` + priv + `","short_ids":["a1b2"]}}],
	  "kharej":` + kharej + `}}`
}

func freshPeers(t *testing.T) {
	t.Helper()
	old := tlscarrier.DefaultPeers
	tlscarrier.DefaultPeers = tlscarrier.NewPeerAllow()
	peerStoreOnce.Store(false)
	oldPath := configPath
	t.Cleanup(func() {
		tlscarrier.DefaultPeers.Flush()
		tlscarrier.DefaultPeers = old
		peerStoreOnce.Store(false)
		configPath = oldPath
	})
}

func resetPeerStoreForTest() { peerStoreOnce.Store(false) }

func tcpA(s string) net.Addr { return &net.TCPAddr{IP: net.ParseIP(s), Port: 40000} }

// b411 reproduction: with the production iran config shape the static
// allowlist was EMPTY (status static 0): the kharej list has only "auto6"
// and the kharej primary is unlisted. The peer IPs that authenticated a link
// in an earlier run must be static from the first second of the next start.
func TestB411IranProdShapeStaticAtStart(t *testing.T) {
	freshPeers(t)
	priv, _, _ := tlscarrier.RealityKeygen()
	dir := t.TempDir()
	configPath = filepath.Join(dir, "hs2.json")
	raw := iranRevConf(priv, `[{"ip":"auto6","weight":50}]`)
	os.WriteFile(configPath, []byte(raw), 0o600)
	fc := parseFC(t, raw)
	if err := validateLB(fc); err != nil {
		t.Fatalf("prod-shaped config must validate: %v", err)
	}
	// run 1: kharej's IPv4 and IPv6 authenticate links
	syncPeerExempt(fc, quietLog)
	if s := tlscarrier.DefaultPeers.Snap(); s.Static != 0 {
		t.Fatalf("run 1 start: static %d, want 0 (nothing literal in lb.kharej, nothing saved)", s.Static)
	}
	tlscarrier.DefaultPeers.Learn(tcpA("203.0.113.7"))
	tlscarrier.DefaultPeers.Learn(tcpA("2001:db8:1:2::5"))
	tlscarrier.DefaultPeers.Flush()

	// run 2 (iran restart / cold start): a new process, nothing learned yet
	tlscarrier.DefaultPeers = tlscarrier.NewPeerAllow()
	peerStoreOnce.Store(false)
	syncPeerExempt(fc, quietLog)
	s := tlscarrier.DefaultPeers.Snap()
	if s.Static < 2 || s.Learned != 0 {
		t.Fatalf("run 2 start: static %d learned %d, want static >= 2 before any link authenticates", s.Static, s.Learned)
	}
	p := tlscarrier.DefaultPeers
	if !p.Exempt(tcpA("203.0.113.7")) || !p.Exempt(tcpA("2001:db8:1:2::5")) {
		t.Fatal("run 2: the kharej IPs of run 1 must be exempt before learning")
	}
	if !p.Exempt(tcpA("2001:db8:1:2::abcd")) {
		t.Fatal("run 2: kharej's auto6 moved inside its /64: must stay exempt")
	}
	if p.Exempt(tcpA("203.0.113.8")) || p.Exempt(tcpA("2001:db8:1:3::5")) {
		t.Fatal("same-/24 neighbour or another /64 must not be exempt")
	}
	// reload keeps the saved IPs and replaces only the configured part
	fc2 := parseFC(t, iranRevConf(priv, `[{"ip":"auto6","weight":50},{"ip":"198.51.100.9","weight":25}]`))
	syncPeerExempt(fc2, nil)
	if !p.Exempt(tcpA("198.51.100.9")) || !p.Exempt(tcpA("203.0.113.7")) {
		t.Fatal("reload: configured + saved must both be exempt")
	}
	syncPeerExempt(fc, nil)
	if p.Exempt(tcpA("198.51.100.9")) || !p.Exempt(tcpA("203.0.113.7")) {
		t.Fatal("reload back: the removed configured IP must go, the saved one stay")
	}
}

// b411: literal IPv6 / IPv4 peers in lb.kharej (both shapes the menu can
// write) are static; an IPv6 peer is exempt by its /64; IPv4-mapped IPv6
// source addresses match the IPv4 entry.
func TestB411LiteralPeersV4V6(t *testing.T) {
	freshPeers(t)
	priv, _, _ := tlscarrier.RealityKeygen()
	configPath = ""
	fc := parseFC(t, iranRevConf(priv, `[{"ip":"203.0.113.7","weight":40},{"ip":"2001:db8:c0c:1234::2","weight":30},{"ip":"auto6","weight":30}]`))
	syncPeerExempt(fc, nil)
	p := tlscarrier.DefaultPeers
	if s := p.Snap(); s.Static != 2 {
		t.Fatalf("static %d, want 2 (auto6 skipped)", s.Static)
	}
	for _, ip := range []string{"203.0.113.7", "::ffff:203.0.113.7", "2001:db8:c0c:1234::2", "2001:db8:c0c:1234::99"} {
		if !p.Exempt(tcpA(ip)) {
			t.Fatalf("%s must be exempt", ip)
		}
	}
	if p.Exempt(tcpA("2001:db8:c0c:1235::2")) {
		t.Fatal("another /64 must not be exempt")
	}
}

// b411: the save file is bounded, ignores junk and old entries, and is 0600.
func TestB411SaveFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.peer-ips")
	now := time.Now()
	var b strings.Builder
	b.WriteString("# c\njunk\n0.0.0.0 1\n")
	fmt.Fprintf(&b, "192.0.2.1 %d\n", now.Add(-8*24*time.Hour).Unix()) // too old
	fmt.Fprintf(&b, "192.0.2.2 %d\n", now.Add(-6*24*time.Hour).Unix())
	for i := 0; i < 300; i++ {
		fmt.Fprintf(&b, "198.18.%d.%d %d\n", i/200, i%200+1, now.Add(-time.Duration(i)*time.Minute).Unix())
	}
	os.WriteFile(path, []byte(b.String()), 0o600)
	p := tlscarrier.NewPeerAllow()
	n, err := p.SetStore(path)
	if err != nil || n != 301 {
		t.Fatalf("loaded %d %v", n, err)
	}
	s := p.Snap()
	if s.Static != 256 || s.Saved != 256 {
		t.Fatalf("static %d saved %d, want the newest 256", s.Static, s.Saved)
	}
	if p.Exempt(tcpA("192.0.2.1")) || p.Exempt(tcpA("192.0.2.2")) {
		t.Fatal("old / oldest-over-the-limit entries must be dropped")
	}
	p.Learn(tcpA("192.0.2.77"))
	p.Flush()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/' {
		t.Fatalf("mode %v", st.Mode())
	}
	q := tlscarrier.NewPeerAllow()
	q.SetStore(path)
	if !q.Exempt(tcpA("192.0.2.77")) {
		t.Fatal("a newly authenticated IP must be in the save file")
	}
}
