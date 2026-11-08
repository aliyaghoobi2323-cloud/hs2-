package main

import (
	"fmt"
	"strings"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/engine"
)

// lbPathLines is `hs2 status`'s per-path view: one line per path and one for
// the totals (nil when the status has no lb_paths). The installer's lb menu
// renders the same fields as a table.
func lbPathLines(ls liveStatus) []string {
	if len(ls.LBPaths) == 0 {
		return nil
	}
	out := make([]string, 0, len(ls.LBPaths)+1)
	for _, p := range ls.LBPaths {
		out = append(out, lbPathLine(p.Local+" > "+p.Remote, p))
	}
	if ls.LBTotal != nil {
		out = append(out, lbPathLine("total", *ls.LBTotal))
	}
	return out
}

func lbPathLine(name string, p engine.LBPathView) string {
	s := fmt.Sprintf("%s  links %d/%d (share %.0f%%) · %d users · ↓%.1f ↑%.1f Mbit/s", name, p.Links, p.Target, p.SharePct, p.Users, p.DownMbit, p.UpMbit)
	var loss []string
	if p.LossDown != nil {
		loss = append(loss, fmt.Sprintf("↓%.1f%%", *p.LossDown))
	}
	if p.LossUp != nil {
		loss = append(loss, fmt.Sprintf("↑%.1f%%", *p.LossUp))
	}
	if len(loss) > 0 {
		s += " · loss " + strings.Join(loss, " ")
	}
	if p.RTTp50 > 0 {
		s += fmt.Sprintf(" · rtt %.0f/%.0f ms", p.RTTp50, p.RTTp99)
	}
	return s + " · " + lbPathState(p)
}

// lbPathState is a row's state in words.
func lbPathState(p engine.LBPathView) string {
	var st []string
	switch p.State {
	case "", "ok":
	case "cooling":
		st = append(st, fmt.Sprintf("cooled %ds", p.CooledS))
	default:
		st = append(st, p.State)
	}
	if p.Degraded > 0 {
		st = append(st, fmt.Sprintf("degraded %d", p.Degraded))
	}
	if p.Draining > 0 {
		st = append(st, fmt.Sprintf("draining %d", p.Draining))
	}
	if p.Suspect > 0 {
		st = append(st, fmt.Sprintf("suspect %d", p.Suspect))
	}
	if p.Fails > 0 {
		st = append(st, fmt.Sprintf("%d failed dial(s)", p.Fails))
	}
	if p.Primary {
		st = append(st, "primary")
	}
	if len(st) == 0 {
		return "ok"
	}
	return strings.Join(st, ", ")
}
