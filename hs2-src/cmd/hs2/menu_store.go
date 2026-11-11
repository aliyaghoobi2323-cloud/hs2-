package main

// Menu redesign M1: the versioned config store every new menu screen (and the
// `hs2 config history|show|diff|validate`, `hs2 apply`, `hs2 rollback`
// commands) builds on.
//
// Layout, one directory per systemd unit:
//
//	/var/lib/hs2/history/<unit>/NNNN-<YYYYMMDD-HHMMSS>-<reason>.json          the config, byte for byte
//	/var/lib/hs2/history/<unit>/NNNN-<YYYYMMDD-HHMMSS>-<reason>.dropins.json  {"name.conf": "content"} of
//	                                                                          /etc/systemd/system/<unit>.service.d
//
// Versions are numbered from 1 and only ever grow; the store keeps the newest
// historyKeep of them (rotation deletes the oldest pair).

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var (
	historyRoot  = "/var/lib/hs2/history"
	historyKeep  = 50
	hs2CfgDir    = "/etc/hs2"
	systemdDir   = "/etc/systemd/system"
	reasonUnsafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)
	versionName  = regexp.MustCompile(`^(\d{4,})-(\d{8}-\d{6})-(.*)\.json$`)
)

// cfgVersion is one stored version.
type cfgVersion struct {
	N      int
	Time   time.Time
	Reason string
	Path   string // the config copy
}

func (v cfgVersion) dropinPath() string {
	return strings.TrimSuffix(v.Path, ".json") + ".dropins.json"
}

// configForUnit is the inverse of unitForConfig: hs2 -> /etc/hs2/config.json,
// hs2-<n> -> /etc/hs2/<n>.json (and a trailing ".service" is ignored).
func configForUnit(unit string) string {
	unit = strings.TrimSuffix(unit, ".service")
	if unit == "hs2" || unit == "" {
		return filepath.Join(hs2CfgDir, "config.json")
	}
	if n, ok := strings.CutPrefix(unit, "hs2-"); ok {
		return filepath.Join(hs2CfgDir, n+".json")
	}
	return filepath.Join(hs2CfgDir, unit+".json")
}

func cleanUnit(u string) string { return strings.TrimSuffix(u, ".service") }

func historyDir(unit string) string { return filepath.Join(historyRoot, cleanUnit(unit)) }

// listVersions returns the stored versions, oldest first.
func listVersions(unit string) ([]cfgVersion, error) {
	ents, err := os.ReadDir(historyDir(unit))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []cfgVersion
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".dropins.json") {
			continue
		}
		m := versionName.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		t, _ := time.ParseInLocation("20060102-150405", m[2], time.Local)
		out = append(out, cfgVersion{N: n, Time: t, Reason: m[3], Path: filepath.Join(historyDir(unit), e.Name())})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].N < out[j].N })
	return out, nil
}

func findVersion(unit string, n int) (cfgVersion, error) {
	vs, err := listVersions(unit)
	if err != nil {
		return cfgVersion{}, err
	}
	for _, v := range vs {
		if v.N == n {
			return v, nil
		}
	}
	return cfgVersion{}, fmt.Errorf("%s has no stored version %d (see: hs2 config history -u %s)", cleanUnit(unit), n, cleanUnit(unit))
}

// readDropins reads the unit's drop-in directory: file name -> content.
func readDropins(unit string) map[string]string {
	d := filepath.Join(systemdDir, cleanUnit(unit)+".service.d")
	out := map[string]string{}
	ents, _ := os.ReadDir(d)
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".conf") {
			continue
		}
		if b, err := os.ReadFile(filepath.Join(d, e.Name())); err == nil {
			out[e.Name()] = string(b)
		}
	}
	return out
}

func (v cfgVersion) dropins() map[string]string {
	out := map[string]string{}
	if b, err := os.ReadFile(v.dropinPath()); err == nil {
		_ = json.Unmarshal(b, &out)
	}
	return out
}

// saveVersion stores cfg (and the drop-ins) as the next version and rotates.
// When the newest stored version is already byte-identical (config and
// drop-ins) it is reused instead of adding a duplicate.
func saveVersion(unit string, cfg []byte, dropins map[string]string, reason string, now time.Time) (cfgVersion, error) {
	dir := historyDir(unit)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return cfgVersion{}, err
	}
	vs, err := listVersions(unit)
	if err != nil {
		return cfgVersion{}, err
	}
	next := 1
	if len(vs) > 0 {
		last := vs[len(vs)-1]
		next = last.N + 1
		if b, err := os.ReadFile(last.Path); err == nil && string(b) == string(cfg) && sameDropins(last.dropins(), dropins) {
			return last, nil
		}
	}
	reason = strings.Trim(reasonUnsafe.ReplaceAllString(reason, "_"), "_")
	if reason == "" {
		reason = "manual"
	}
	if len(reason) > 40 {
		reason = reason[:40]
	}
	v := cfgVersion{N: next, Time: now, Reason: reason,
		Path: filepath.Join(dir, fmt.Sprintf("%04d-%s-%s.json", next, now.Format("20060102-150405"), reason))}
	if err := writeFileAtomic(v.Path, cfg); err != nil {
		return cfgVersion{}, err
	}
	os.Chmod(v.Path, 0o600) // holds the tunnel key
	db, _ := json.MarshalIndent(dropins, "", "  ")
	if err := writeFileAtomic(v.dropinPath(), db); err != nil {
		return cfgVersion{}, err
	}
	rotateVersions(unit)
	return v, nil
}

func sameDropins(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// rotateVersions deletes the oldest versions beyond historyKeep.
func rotateVersions(unit string) {
	vs, _ := listVersions(unit)
	for len(vs) > historyKeep {
		os.Remove(vs[0].Path)
		os.Remove(vs[0].dropinPath())
		vs = vs[1:]
	}
}

// snapshotCurrent stores the unit's live config + drop-ins.
func snapshotCurrent(unit, cfgPath, reason string, now time.Time) (cfgVersion, error) {
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		return cfgVersion{}, err
	}
	return saveVersion(unit, b, readDropins(unit), reason, now)
}

// restoreDropins makes the unit's drop-in directory equal want; reports
// whether anything changed (the caller then runs daemon-reload).
func restoreDropins(unit string, want map[string]string) (bool, error) {
	d := filepath.Join(systemdDir, cleanUnit(unit)+".service.d")
	have := readDropins(unit)
	if sameDropins(have, want) {
		return false, nil
	}
	if err := os.MkdirAll(d, 0o755); err != nil {
		return false, err
	}
	for k := range have {
		if _, ok := want[k]; !ok {
			os.Remove(filepath.Join(d, k))
		}
	}
	for k, v := range want {
		if have[k] != v {
			if err := writeFileAtomic(filepath.Join(d, k), []byte(v)); err != nil {
				return true, err
			}
		}
	}
	return true, nil
}
