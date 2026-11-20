package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

// b411 reproduction with the pre-fix API only (this test FAILS on rel69q /
// rel69r-rc4: static 0 with the production iran config shape). A kharej IPv4
// and IPv6 authenticated links in an earlier run (the save file next to the
// config); at the next start they must be static before any link
// authenticates.
func TestB411ReproStaticZero(t *testing.T) {
	old := tlscarrier.DefaultPeers
	tlscarrier.DefaultPeers = tlscarrier.NewPeerAllow()
	oldPath := configPath
	defer func() { tlscarrier.DefaultPeers = old; configPath = oldPath }()
	priv, _, _ := tlscarrier.RealityKeygen()
	dir := t.TempDir()
	configPath = filepath.Join(dir, "hs2.json")
	raw := iranRevConf(priv, `[{"ip":"auto6","weight":50}]`)
	os.WriteFile(configPath, []byte(raw), 0o600)
	now := time.Now().Unix()
	os.WriteFile(filepath.Join(dir, "hs2.peer-ips"), []byte(fmt.Sprintf("203.0.113.7 %d\n2001:db8:1:2::5 %d\n", now, now)), 0o600)
	resetPeerStoreForTest()
	fc := parseFC(t, raw)
	syncPeerExempt(fc, quietLog)
	s := tlscarrier.DefaultPeers.Snap()
	if s.Static == 0 {
		t.Fatalf("static 0 at start with the production iran config shape (learned %d): our links hit the pre-hello cap until learned", s.Learned)
	}
	if !tlscarrier.DefaultPeers.Exempt(tcpA("203.0.113.7")) || !tlscarrier.DefaultPeers.Exempt(tcpA("2001:db8:1:2::77")) {
		t.Fatal("the kharej IPs must be exempt at start (IPv6 by /64)")
	}
}
