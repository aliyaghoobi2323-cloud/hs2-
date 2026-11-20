//go:build linux

package main

import (
	"path/filepath"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/engine"
	"github.com/hosseintaghipoursori-alt/hs2-tunnel/handoff"
)

// ZDU slot S5: the named hand-off socket of a unit, /run/hs2/UNIT.sock
// (owner root, 0600). S6 wires it into the takeover path; this is the one
// place that opens it, and only with HS2_ZDU=1: with the key off no
// directory or socket is created and nothing changes.

var zduRunDir = "/run/hs2"

func zduHandoffPath(unit string) string { return filepath.Join(zduRunDir, unit+".sock") }

// zduHandoffListen opens the unit's hand-off socket for the old process, or
// returns nil with HS2_ZDU off. allowed: the new release's binary path (from
// /run/hs2/UNIT.next, S6); this binary is always allowed.
func zduHandoffListen(unit string, allowed ...string) (*handoff.Listener, error) {
	if !engine.ZDUEnabled() {
		return nil, nil
	}
	return handoff.Listen(zduHandoffPath(unit), handoff.DefaultPolicy(allowed...))
}

// zduHello is this process's HELLO: protocol, instance, generation and the
// config hash.
// zduTakeHello is the new process's Hello in Take.
func zduTakeHello() handoff.Hello { return zduHello(0, [32]byte{}) }

func zduHello(gen uint32, cfgHash [32]byte) handoff.Hello {
	s, r := engine.ZDUInstance()
	return handoff.Hello{Proto: handoff.Proto, Features: handoff.FeatListening, InstStart: s, InstRand: r, Gen: gen, ConfigHash: cfgHash}
}
