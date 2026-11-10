package main

import (
	"log"
	"strings"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/engine"
	"github.com/hosseintaghipoursori-alt/hs2-tunnel/envdef"
	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

// defaultsApply: whether the production default table (envdef) applies to
// this config: the l3mtcp carrier in reverse, the layout it was proven on.
// Every other carrier, and direct mode, keeps the old defaults (off).
func defaultsApply(fc fileConfig) bool {
	return (fc.Carrier == "l3mtcp" || fc.Carrier == "l3") && fc.Reverse
}

// applyDefaults turns the default table on (runCmd, before anything starts).
func applyDefaults(fc fileConfig) {
	if !defaultsApply(fc) {
		return
	}
	side := "exit"
	if fc.Mode == "dial" {
		side = "edge"
	}
	if envdef.Legacy() {
		log.Printf("defaults: HS2_DEFAULTS=legacy — production defaults off, only explicit HS2_ keys apply")
		return
	}
	on := engine.ApplyDefaults(fc.Mode == "dial")
	coverNginx = tlscarrier.CoverNginx
	log.Printf("defaults: production set on (%s side, %d keys from defaults; HS2_DEFAULTS=legacy turns them off): %s",
		side, len(on), strings.Join(on, " "))
}
