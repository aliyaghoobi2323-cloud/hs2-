// Package envdef holds the default values of the HS2_ environment keys that
// production (tunnel 1, reverse l3mtcp) runs with, so a plain install with no
// Environment= lines behaves like it.
//
// The defaults are dormant until Activate: package-level reads at process
// start see only the real environment (the old behaviour), and cmd/hs2 turns
// the table on — for this server's side — only for the l3mtcp carrier in
// reverse, the layout the values were proven on. Every other carrier and
// direct mode keep the old defaults (off).
//
// Rules:
//   - a key present in the environment always wins, whatever its value —
//     HS2_X=0, HS2_X=off and an empty HS2_X= all mean what they meant before
//     (off for every switch in the table);
//   - HS2_DEFAULTS=legacy (or 0 / off) keeps the table dormant: every key is
//     back to its old default unless set explicitly.
package envdef

import (
	"os"
	"sort"
	"strings"
	"sync/atomic"
)

// Side is where a default applies.
type Side uint8

const (
	Both Side = iota // edge (iran, mode "dial") and exit (kharej, mode "listen")
	Edge             // only the edge
	Exit             // only the exit
)

func (s Side) String() string {
	switch s {
	case Edge:
		return "edge"
	case Exit:
		return "exit"
	}
	return "both"
}

// Key is one default.
type Key struct {
	Name  string
	Value string
	Side  Side
}

// Table is the production set (tunnel 1 since 2026-11-10). Left out on
// purpose: HS2_MCONN_STRIPE_JUMP_MS (no proven gain).
var Table = []Key{
	{"HS2_SHAPE_FAST", "1", Both},
	{"HS2_SHAPE_SMALL", "1", Both},
	{"HS2_LINK_PROBATION", "1", Edge},
	{"HS2_PARTIAL_REFILL", "1", Edge},
	{"HS2_PICK_LANE", "on", Edge},
	{"HS2_L3_LANE", "1", Both},
	{"HS2_HEAVY_BACKLOG", "1", Both},
	{"HS2_L3_DUP", "1", Both},
	{"HS2_MCONN", "lite", Both},
	{"HS2_FAST_LINK", "1", Both},
	{"HS2_MCONN_BATCH", "1", Both},
	{"HS2_SHAPE_BULK", "1", Both},
	{"HS2_QUIET_IDLE", "1", Both},
	{"HS2_TLS_RESUME", "1", Both},
	{"HS2_AUTH_PAD", "1", Both},
	{"HS2_COVER_NGINX", "1", Both},
	{"HS2_MCONN_DEADLINE_FIX", "1", Both},
	{"HS2_MCONN_CLOSE_HS", "1", Both},
	{"HS2_LB_AUTO2", "1", Both},
	{"HS2_MCONN_CALL", "1", Both},
	{"HS2_MCONN_CALL_AVOID", "1", Both},
	{"HS2_MCONN_STRIPE", "8", Both},
	{"HS2_MCONN_STRIPE_UP", "1", Both},
	{"HS2_MCONN_STRIPE_SHARE", "1", Both},
	{"HS2_MCONN_STRIPE_REL", "1", Both},
	{"HS2_LB_CUT_DRAIN", "1", Both},
	{"HS2_MCONN_STRIPE_QWIN_MS", "8000", Both},
	{"HS2_SMUX_AUTOWIN", "1", Both},
	{"HS2_SMUX_AUTOWIN_REL", "1", Both},
	{"HS2_SMUX_AUTOWIN_SHARE_MB", "32", Both},
	{"HS2_STRIPE_AW_SOLO", "1", Edge},
	{"HS2_LB_FAST_RECOVER", "1", Both},
	{"HS2_LB_START_GRACE_S", "20", Both},
	{"HS2_STRIPE_AW_HEAVY", "2", Edge},
	{"HS2_LB_FILL_PCT", "25", Exit},
	{"HS2_LB_SPARE_PER_ROUND", "3", Exit},
	{"HS2_LB_REC_CONFIRM", "1", Exit},
	{"HS2_LB_FLAP_PEN", "1", Exit},
	{"HS2_LB_JUDGE_DATA", "1", Both},
	{"HS2_LB_RESCUE", "1", Edge},
}

var byName = func() map[string]Key {
	m := make(map[string]Key, len(Table))
	for _, k := range Table {
		m[k.Name] = k
	}
	return m
}()

// active: 0 dormant, 1 edge, 2 exit.
var active atomic.Int32

// Legacy reports HS2_DEFAULTS=legacy (or 0 / off): the table stays dormant.
func Legacy() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("HS2_DEFAULTS"))) {
	case "legacy", "0", "off", "false", "no":
		return true
	}
	return false
}

// Activate turns the table on for this side (edge: the iran server). It
// returns false (and changes nothing) under HS2_DEFAULTS=legacy.
func Activate(edge bool) bool {
	if Legacy() {
		active.Store(0)
		return false
	}
	if edge {
		active.Store(1)
	} else {
		active.Store(2)
	}
	return true
}

// Deactivate makes the table dormant again (tests).
func Deactivate() { active.Store(0) }

// Default is k's default for the active side ("" and false: none).
func Default(k string) (string, bool) {
	a := active.Load()
	if a == 0 {
		return "", false
	}
	d, ok := byName[k]
	if !ok || d.Side == Edge && a != 1 || d.Side == Exit && a != 2 {
		return "", false
	}
	return d.Value, true
}

// Get is os.Getenv with the table: an explicitly set key wins (even when
// empty), an unset one returns its default (or "").
func Get(k string) string {
	if v, ok := os.LookupEnv(k); ok {
		return v
	}
	d, _ := Default(k)
	return d
}

// Applied lists "KEY=value" for the defaults in force (keys left unset),
// sorted, for the start-up log.
func Applied() []string {
	var out []string
	for _, k := range Table {
		if _, set := os.LookupEnv(k.Name); set {
			continue
		}
		if v, ok := Default(k.Name); ok {
			out = append(out, k.Name+"="+v)
		}
	}
	sort.Strings(out)
	return out
}
