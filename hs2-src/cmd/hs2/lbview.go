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
	if m := lbPmtuText(p); m != "" {
		s += " · mtu " + m
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

// lbPmtuText is a row's path MTU (mt135): the median per direction
// (down/up), the lowest when it differs, and a drop in the last 10 minutes.
func lbPmtuText(p engine.LBPathView) string {
	if p.PmtuDown == nil && p.PmtuUp == nil {
		return ""
	}
	f := func(x *engine.LBPmtu) string {
		if x == nil {
			return "-"
		}
		return fmt.Sprint(x.Med)
	}
	s := "↓" + f(p.PmtuDown) + " ↑" + f(p.PmtuUp)
	lo := 0
	for _, x := range []*engine.LBPmtu{p.PmtuDown, p.PmtuUp} {
		if x != nil && x.Min < x.Med && (lo == 0 || x.Min < lo) {
			lo = x.Min
		}
	}
	if lo > 0 {
		s += fmt.Sprintf(" (min %d)", lo)
	}
	for _, x := range []*engine.LBPmtu{p.PmtuDown, p.PmtuUp} {
		if x.RecentDrop() {
			s += fmt.Sprintf(" dropped %ds ago", x.LastDrop)
			break
		}
	}
	return s
}

// lbPmtuFindings is `hs2 doctor`'s MTU check over the status files' paths
// (mt135): a warning when a direction's path MTU is under 1280 or dropped
// in the last 10 minutes; one OK line when every path reads fine.
func lbPmtuFindings(st []namedStatus) []finding {
	var out []finding
	n := 0
	for _, s := range st {
		if s.age > 30 {
			continue // a stopped tunnel or a leftover file (reported elsewhere)
		}
		for _, p := range s.ls.LBPaths {
			for _, d := range []struct {
				x   *engine.LBPmtu
				dir string
			}{{p.PmtuDown, "down"}, {p.PmtuUp, "up"}} {
				x := d.x
				if x == nil {
					continue
				}
				n++
				name := p.Local + " > " + p.Remote + " (" + d.dir + ")"
				switch {
				case x.Min < engine.PmtuWarn:
					out = append(out, finding{Sev: "WARN", Area: "path MTU",
						What: fmt.Sprintf(tr("%s: path MTU %d on %d of %d links (median %d) — under 1280", "%s: MTU مسیر %d روی %d از %d لینک (میانه %d) — کمتر از 1280"), name, x.Min, max(x.Low, 1), x.Links, x.Med),
						Why:  tr("a hop on this path carries small packets only (a tunnel or a broken MTU); TCP shrinks its segments, throughput per link drops", "یک هاپ در این مسیر فقط بسته‌های کوچک عبور می‌دهد؛ TCP سگمنت‌ها را کوچک می‌کند و سرعت هر لینک کم می‌شود"),
						Do:   tr("check the provider's MTU on this IP; keep net.ipv4.tcp_mtu_probing=1", "MTU دیتاسنتر را روی این IP بررسی کنید؛ net.ipv4.tcp_mtu_probing=1 بماند")})
				case x.RecentDrop():
					out = append(out, finding{Sev: "WARN", Area: "path MTU",
						What: fmt.Sprintf(tr("%s: links lowered their path MTU %ds ago (now median %d, usual %d, %d of %d below)", "%s: لینک‌ها %d ثانیه پیش MTU مسیر را کم کردند (میانه %d، معمول %d، %d از %d کمتر)"), name, x.LastDrop, x.Med, x.Usual, x.Low, x.Links),
						Why:  tr("large packets stopped passing on this path (a blackhole or a new smaller hop); the kernel adapted, users may have stalled meanwhile", "بسته‌های بزرگ در این مسیر رد نشدند؛ کرنل خودش را تطبیق داد ولی ممکن است کاربران لحظه‌ای گیر کرده باشند"),
						Do:   tr("watch this path's loss and stalls; repeated drops mean an unstable hop", "loss و گیر این مسیر را زیر نظر بگیرید؛ افت‌های تکراری یعنی هاپ ناپایدار")})
				}
			}
		}
	}
	if len(out) == 0 && n > 0 {
		out = append(out, finding{Sev: "OK", Area: "path MTU", What: fmt.Sprintf(tr("path MTU fine on all paths (%d path directions read)", "MTU همهٔ مسیرها سالم است (%d جهت مسیر)"), n)})
	}
	return out
}
