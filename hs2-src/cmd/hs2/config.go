package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// `hs2 config` edits a config file safely and JSON-aware, so the installer (and
// the operator) never has to hand-edit or sed the JSON:
//
//	hs2 config -c cfg get  <key>
//	hs2 config -c cfg set  <key> <value>
//	hs2 config -c cfg unset <key>
//
// Only a whitelist of keys is editable — the adaptive link envelope, the idle
// reclaim of a shrinking pool and the tuning section — and each is type-checked, so a typo can never turn a number
// into a string or introduce an unknown key. Numbers keep their integer form
// (no scientific notation), and after a set the whole file is re-validated with
// the same checker as `hs2 check`; a change that would not pass is refused and
// the file is left untouched.

// configKey describes an editable key: whether it is nested under "tuning" and
// whether its value is an int or a string.
type configKey struct {
	parent string // "" for top-level, "tuning" for the tuning object
	name   string
	isInt  bool
}

var configKeys = map[string]configKey{
	"min_links": {"", "min_links", true},
	"max_links": {"", "max_links", true},
	"per_link":  {"", "per_link", true},

	"drain_idle_sec": {"", "drain_idle_sec", true},

	"tuning.mode":           {"tuning", "mode", false},
	"tuning.congestion":     {"tuning", "congestion", false},
	"tuning.qdisc":          {"tuning", "qdisc", false},
	"tuning.rmem_max":       {"tuning", "rmem_max", true},
	"tuning.wmem_max":       {"tuning", "wmem_max", true},
	"tuning.netdev_backlog": {"tuning", "netdev_backlog", true},
	"tuning.somaxconn":      {"tuning", "somaxconn", true},
}

func configCmd(args []string) {
	fs := flag.NewFlagSet("config", flag.ExitOnError)
	cfgPath := fs.String("c", "", "config file (JSON)")
	fs.Parse(args)
	rest := fs.Args()
	if *cfgPath == "" || len(rest) < 2 {
		fmt.Println("usage: hs2 config -c config.json get|set|unset <key> [value]")
		fmt.Println("editable keys: " + strings.Join(sortedKeys(), ", "))
		os.Exit(2)
	}
	op, key := rest[0], rest[1]
	ck, ok := configKeys[key]
	if !ok {
		fmt.Printf("unknown key %q. editable keys: %s\n", key, strings.Join(sortedKeys(), ", "))
		os.Exit(2)
	}
	m, err := readConfigMap(*cfgPath)
	if err != nil {
		fmt.Printf("ERROR: %v\n", err)
		os.Exit(1)
	}
	switch op {
	case "get":
		fmt.Println(getKey(m, ck))
		return
	case "set":
		if len(rest) < 3 {
			fmt.Println("set needs a value")
			os.Exit(2)
		}
		if err := setKey(m, ck, rest[2]); err != nil {
			fmt.Printf("ERROR: %v\n", err)
			os.Exit(1)
		}
	case "unset":
		unsetKey(m, ck)
	default:
		fmt.Printf("unknown op %q (get|set|unset)\n", op)
		os.Exit(2)
	}

	out, err := marshalConfig(m)
	if err != nil {
		fmt.Printf("ERROR: %v\n", err)
		os.Exit(1)
	}
	// Validate before writing: never persist a config that would not start.
	if errs, _ := checkConfig(out, isLocalIP, time.Now()); len(errs) > 0 {
		fmt.Println("ERROR: the change would make the config invalid, so it was NOT saved:")
		for _, e := range errs {
			fmt.Println("  - " + e)
		}
		os.Exit(1)
	}
	if err := writeFileAtomic(*cfgPath, out); err != nil {
		fmt.Printf("ERROR: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("set %s = %s\n", key, getKey(m, ck))
}

// readConfigMap decodes the file preserving integers (UseNumber) so re-writing
// never turns 33554432 into 3.35e+07.
func readConfigMap(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("config is not valid JSON: %w", err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

func marshalConfig(m map[string]any) ([]byte, error) {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func objFor(m map[string]any, ck configKey) map[string]any {
	if ck.parent == "" {
		return m
	}
	if sub, ok := m[ck.parent].(map[string]any); ok {
		return sub
	}
	sub := map[string]any{}
	m[ck.parent] = sub
	return sub
}

func getKey(m map[string]any, ck configKey) string {
	obj := m
	if ck.parent != "" {
		sub, ok := m[ck.parent].(map[string]any)
		if !ok {
			return ""
		}
		obj = sub
	}
	v, ok := obj[ck.name]
	if !ok {
		return ""
	}
	return fmt.Sprintf("%v", v)
}

func setKey(m map[string]any, ck configKey, val string) error {
	obj := objFor(m, ck)
	if ck.parent == "" && ck.name == "max_links" && strings.EqualFold(strings.TrimSpace(val), "auto") {
		val = "0" // auto: the ceiling follows this server's hardware (see linkCeiling)
	}
	if ck.isInt {
		n, err := strconv.Atoi(strings.TrimSpace(val))
		if err != nil {
			return fmt.Errorf("%s needs a whole number, got %q", ck.name, val)
		}
		obj[ck.name] = json.Number(strconv.Itoa(n))
	} else {
		obj[ck.name] = val
	}
	return nil
}

func unsetKey(m map[string]any, ck configKey) {
	if ck.parent == "" {
		delete(m, ck.name)
		return
	}
	if sub, ok := m[ck.parent].(map[string]any); ok {
		delete(sub, ck.name)
		if len(sub) == 0 {
			delete(m, ck.parent)
		}
	}
}

func writeFileAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func sortedKeys() []string {
	out := make([]string, 0, len(configKeys))
	for k := range configKeys {
		out = append(out, k)
	}
	// simple insertion sort keeps top-level keys before tuning.* readably
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
