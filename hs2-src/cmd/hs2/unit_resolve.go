package main

// rt80: one consistent mapping between what an operator types for -u (the
// name `hs2 tunnel list` shows, the systemd unit, with or without .service,
// a doubled "hs2-hs2-…" prefix, or the bare config name) and the real
// systemd unit + config file. Before, `-u hs2-out` on a classic install
// (unit hs2-out.service, /etc/hs2/hs2-out.json) looked for /etc/hs2/out.json,
// and `-u hs2-hs2-out` found the config but restarted hs2-hs2-out.service.

import (
	"bufio"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// unitSearchDirs: where systemd unit files may live (the first one is the
// admin's, overridable in tests through systemdDir).
func unitSearchDirs() []string {
	return []string{systemdDir, "/run/systemd/system", "/usr/lib/systemd/system", "/lib/systemd/system"}
}

// unitFileExists: a <unit>.service file is installed.
func unitFileExists(unit string) bool {
	if unit == "" {
		return false
	}
	for _, d := range unitSearchDirs() {
		if _, err := os.Stat(filepath.Join(d, unit+".service")); err == nil {
			return true
		}
	}
	return false
}

// configCandidates: the config paths a unit name can mean, preferred first.
func configCandidates(unit string) []string {
	if unit == "hs2" || unit == "" {
		return []string{filepath.Join(hs2CfgDir, "config.json")}
	}
	if n, ok := strings.CutPrefix(unit, "hs2-"); ok {
		return []string{filepath.Join(hs2CfgDir, n+".json"), filepath.Join(hs2CfgDir, unit+".json")}
	}
	return []string{filepath.Join(hs2CfgDir, unit+".json"), filepath.Join(hs2CfgDir, "hs2-"+unit+".json")}
}

// execConfigOf: the -c PATH of an "hs2 run" ExecStart line ("" if none).
// handled: the line set (or reset, "ExecStart=") ExecStart.
func execConfigOf(line string) (path string, handled bool) {
	line = strings.TrimSpace(line)
	v, ok := strings.CutPrefix(line, "ExecStart=")
	if !ok {
		return "", false
	}
	f := strings.Fields(v)
	run := false
	for i := 0; i < len(f); i++ {
		if f[i] == "run" {
			run = true
		}
		if run && f[i] == "-c" && i+1 < len(f) {
			return f[i+1], true
		}
		if run && strings.HasPrefix(f[i], "-c=") {
			return strings.TrimPrefix(f[i], "-c="), true
		}
	}
	return "", true
}

// unitExecConfig: the config the unit runs, from its unit file and then its
// drop-ins (in name order; a later ExecStart wins). "" when unknown.
func unitExecConfig(unit string) string {
	if unit == "" {
		return ""
	}
	var files []string
	for _, d := range unitSearchDirs() {
		f := filepath.Join(d, unit+".service")
		if _, err := os.Stat(f); err == nil {
			files = append(files, f)
			break
		}
	}
	var drops []string
	for _, d := range unitSearchDirs() {
		m, _ := filepath.Glob(filepath.Join(d, unit+".service.d", "*.conf"))
		drops = append(drops, m...)
	}
	sort.Slice(drops, func(a, b int) bool { return filepath.Base(drops[a]) < filepath.Base(drops[b]) })
	files = append(files, drops...)
	cfg := ""
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(fh)
		for sc.Scan() {
			if p, ok := execConfigOf(sc.Text()); ok {
				cfg = p
			}
		}
		fh.Close()
	}
	return cfg
}

// canonicalUnit maps what was typed for -u to the tunnel's systemd unit.
// An installed unit of that exact name always wins; otherwise the tunnel
// configs are matched by unit name (as `hs2 tunnel list` shows it), by the
// config's base name, by "hs2-"+base, and by the unit without its "hs2-".
func canonicalUnit(in string) string {
	u := cleanUnit(strings.TrimSpace(in))
	if u == "" || u == "hs2" || unitFileExists(u) {
		return u
	}
	var hits []string
	for _, t := range listTunnelConfigs(hs2CfgDir) {
		base := strings.TrimSuffix(filepath.Base(t.cfg), ".json")
		if u == t.unit || u == base || u == "hs2-"+base || u == strings.TrimPrefix(t.unit, "hs2-") {
			if !slices.Contains(hits, t.unit) {
				hits = append(hits, t.unit)
			}
		}
	}
	if len(hits) == 1 {
		return hits[0]
	}
	for _, h := range hits { // ambiguous: the one with an installed unit
		if unitFileExists(h) {
			return h
		}
	}
	return u
}

// knownTunnels: the unit names `hs2 tunnel list` shows (for error hints).
func knownTunnels() []string {
	var out []string
	for _, t := range listTunnelConfigs(hs2CfgDir) {
		out = append(out, t.unit)
	}
	return out
}

// unitValue is a -u flag that resolves itself to the real unit when set.
type unitValue struct{ p *string }

func (v unitValue) String() string {
	if v.p == nil {
		return ""
	}
	return *v.p
}

func (v unitValue) Set(s string) error {
	*v.p = canonicalUnit(s)
	return nil
}

// unitFlag registers -u (the tunnel) with canonical resolution.
func unitFlag(fs *flag.FlagSet, def, usage string) *string {
	p := new(string)
	*p = def
	fs.Var(unitValue{p}, "u", usage)
	return p
}
