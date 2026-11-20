//go:build linux

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/engine"
)

// HS2_ZDU off: no hand-off socket and not even its directory.
func TestZDUHandoffKeyOffNoSocket(t *testing.T) {
	if engine.ZDUEnabled() {
		t.Skip("HS2_ZDU=1 in the environment")
	}
	orig := zduRunDir
	zduRunDir = filepath.Join(t.TempDir(), "run-hs2")
	defer func() { zduRunDir = orig }()
	l, err := zduHandoffListen("hs2-kh")
	if l != nil || err != nil {
		t.Fatalf("key off: %v %v", l, err)
	}
	if _, err := os.Stat(zduRunDir); !os.IsNotExist(err) {
		t.Fatal("key off created the run directory")
	}
	h := zduHello(3, [32]byte{1})
	if h.Gen != 3 || h.ConfigHash[0] != 1 || h.InstStart == 0 {
		t.Fatalf("hello %+v", h)
	}
}

// HS2_ZDU on: the socket is created 0600 under the run directory.
func TestZDUHandoffKeyOnSocket(t *testing.T) {
	if !engine.ZDUEnabled() {
		t.Skip("needs HS2_ZDU=1")
	}
	if os.Geteuid() != 0 {
		t.Skip("needs root (policy uid 0)")
	}
	orig := zduRunDir
	zduRunDir = filepath.Join(t.TempDir(), "run-hs2")
	defer func() { zduRunDir = orig }()
	l, err := zduHandoffListen("hs2-kh")
	if err != nil || l == nil {
		t.Fatal(err)
	}
	defer l.Close()
	fi, err := os.Stat(zduHandoffPath("hs2-kh"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", fi, err)
	}
}
