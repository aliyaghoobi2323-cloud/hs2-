package main

// Semantic config diff (menu M1): JSON keys flattened to dotted paths, the
// unit's drop-ins compared by Environment= key, and every field that must be
// the same on the OTHER server marked "bilateral".

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// bilateralKeys are the config paths that must match the peer: they travel
// in the hs2:// link or decide authentication / who dials. A path matches a
// key when it equals it or lies under it (lb.iran.0.ip under lb).
var bilateralKeys = []string{
	"shared_key", "carrier", "encap", "proto", "sni", "mtu", "reverse",
	"forward_ports", "psk", "remote_static", "local_pub", "front", "lb",
}

// addrPortChanged: "addr" is per-side (bind vs endpoint) but its PORT must
// match the peer's.
func addrPortChanged(o, n string) bool {
	_, op, _ := strings.Cut(o[strings.LastIndex(o, "]")+1:], ":")
	_, np, _ := strings.Cut(n[strings.LastIndex(n, "]")+1:], ":")
	return op != np
}

func isBilateral(path string) bool {
	// An lb IP's front lives on the listening side only: the dialer learns
	// it from the announcement (menu M5).
	if strings.HasPrefix(path, "lb.") && strings.Contains(path, ".front") {
		return false
	}
	for _, k := range bilateralKeys {
		if path == k || strings.HasPrefix(path, k+".") {
			return true
		}
	}
	return false
}

// sensitiveKey: values never printed in a diff.
func sensitiveKey(path string) bool {
	switch path {
	case "shared_key", "psk", "local_priv", "cover_seed":
		return true
	}
	// reality private keys (tunnel front and per lb IP)
	return strings.HasSuffix(path, ".private_key") || path == "private_key"
}

type diffEntry struct {
	Path      string
	Old, New  string // "" with Has* false = absent
	HasOld    bool
	HasNew    bool
	Bilateral bool
	Source    string // "config" or "dropin:<file>"
}

func (d diffEntry) String() string {
	show := func(s string, has bool) string {
		if !has {
			return "(absent)"
		}
		if sensitiveKey(d.Path) {
			return "(hidden)"
		}
		return s
	}
	tag := ""
	if d.Bilateral {
		tag = "  [bilateral]"
	}
	sign := "~"
	switch {
	case !d.HasOld:
		sign = "+"
	case !d.HasNew:
		sign = "-"
	}
	where := ""
	if d.Source != "config" {
		where = " (" + d.Source + ")"
	}
	return fmt.Sprintf("%s %s%s: %s -> %s%s", sign, d.Path, where, show(d.Old, d.HasOld), show(d.New, d.HasNew), tag)
}

// flattenJSON decodes raw and flattens it to path -> compact JSON value.
func flattenJSON(raw []byte) (map[string]string, error) {
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	out := map[string]string{}
	var walk func(p string, v any)
	walk = func(p string, v any) {
		switch t := v.(type) {
		case map[string]any:
			if len(t) == 0 && p != "" {
				out[p] = "{}"
			}
			for k, c := range t {
				walk(join(p, k), c)
			}
		case []any:
			if len(t) == 0 {
				out[p] = "[]"
			}
			for i, c := range t {
				walk(join(p, fmt.Sprint(i)), c)
			}
		case string:
			out[p] = t
		default:
			b, _ := json.Marshal(t)
			out[p] = string(b)
		}
	}
	walk("", v)
	return out, nil
}

func join(p, k string) string {
	if p == "" {
		return k
	}
	return p + "." + k
}

// diffMaps compares two flat maps.
func diffMaps(a, b map[string]string, source string) []diffEntry {
	keys := map[string]bool{}
	for k := range a {
		keys[k] = true
	}
	for k := range b {
		keys[k] = true
	}
	var out []diffEntry
	for k := range keys {
		ov, oh := a[k]
		nv, nh := b[k]
		if oh == nh && ov == nv {
			continue
		}
		out = append(out, diffEntry{Path: k, Old: ov, New: nv, HasOld: oh, HasNew: nh,
			Bilateral: source == "config" && (isBilateral(k) || (k == "addr" && addrPortChanged(ov, nv))), Source: source})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// envOfDropin extracts Environment= assignments (KEY=VALUE, quoted or not)
// and every other Key=Value line as "[Section]Key".
func envOfDropin(content string) map[string]string {
	out := map[string]string{}
	sec := ""
	for _, ln := range strings.Split(content, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || ln[0] == '#' || ln[0] == ';' {
			continue
		}
		if strings.HasPrefix(ln, "[") {
			sec = strings.Trim(ln, "[]")
			continue
		}
		k, v, ok := strings.Cut(ln, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		if k != "Environment" {
			out["["+sec+"]"+k] = strings.TrimSpace(v)
			continue
		}
		for _, f := range splitEnvWords(v) {
			if ek, ev, ok := strings.Cut(f, "="); ok {
				out[ek] = ev
			}
		}
	}
	return out
}

// splitEnvWords splits an Environment= value on spaces, honouring quotes.
func splitEnvWords(s string) []string {
	var out []string
	var cur strings.Builder
	q := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case q != 0 && c == q:
			q = 0
		case q == 0 && (c == '"' || c == '\''):
			q = c
		case q == 0 && (c == ' ' || c == '\t'):
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteByte(c)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// semanticDiff diffs two configs and two drop-in sets.
func semanticDiff(oldCfg, newCfg []byte, oldDI, newDI map[string]string) ([]diffEntry, error) {
	a, err := flattenJSON(oldCfg)
	if err != nil {
		return nil, fmt.Errorf("old config: %v", err)
	}
	b, err := flattenJSON(newCfg)
	if err != nil {
		return nil, fmt.Errorf("new config: %v", err)
	}
	out := diffMaps(a, b, "config")
	files := map[string]bool{}
	for k := range oldDI {
		files[k] = true
	}
	for k := range newDI {
		files[k] = true
	}
	names := make([]string, 0, len(files))
	for k := range files {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, f := range names {
		out = append(out, diffMaps(envOfDropin(oldDI[f]), envOfDropin(newDI[f]), "dropin:"+f)...)
	}
	return out, nil
}

func bilateralOf(d []diffEntry) []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range d {
		if e.Bilateral {
			top, _, _ := strings.Cut(e.Path, ".")
			if !seen[top] {
				seen[top] = true
				out = append(out, top)
			}
		}
	}
	return out
}

// printDiff writes a human-readable diff (plain text: narrow, colourless SSH
// terminals are fine).
func printDiff(d []diffEntry) {
	if len(d) == 0 {
		fmt.Println(tr("no differences", "تفاوتی نیست"))
		return
	}
	for _, e := range d {
		fmt.Println("  " + e.String())
	}
	if b := bilateralOf(d); len(b) > 0 {
		fmt.Println(tr("bilateral: the OTHER server must get the same value(s) for: ",
			"دوطرفه: سرور مقابل هم باید همین مقدار(ها) را بگیرد برای: ") + strings.Join(b, ", "))
	}
}

func diffSummary(d []diffEntry) string {
	if len(d) == 0 {
		return "no changes"
	}
	var parts []string
	for i, e := range d {
		if i == 6 {
			parts = append(parts, fmt.Sprintf("+%d more", len(d)-6))
			break
		}
		parts = append(parts, e.Path)
	}
	return strings.Join(parts, ",")
}
