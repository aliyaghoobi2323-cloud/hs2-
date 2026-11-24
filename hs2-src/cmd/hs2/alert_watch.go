package main

// b352 watchdog checks for `hs2 alert` (also run as `hs2 watch`). Same model
// as the rest of alert*.go: a oneshot run by hs2-alert-<unit>.timer every
// 60 s, never inside the tunnel process, reading only what already exists:
//
//   - the status file (/run/hs2/<config>.status.json): its age (stale), the
//     user count (sharp drop), per-path loss;
//   - the journal lines the panic check already reads: "every sub died"
//     bursts and error kinds never seen before;
//   - /proc/stat (host CPU idle), /proc/meminfo, statfs of / and /var/log;
//   - optional light probes: a TCP connect THROUGH the tunnel (probe_target,
//     3 connects per check) and a TCP connect to the other side's public
//     port (peer_probe; from Kharej this sees an Iran side that is down or
//     cut even when Iran cannot reach Telegram itself).
//
// Every rule goes through the same cond() as the old ones: N bad checks in a
// row before it fires, one message per incident (resent at most every
// resend_min), and one "resolved" message.
//
// Auto-rollback (rollback=dry|on, default off): when a release was installed
// less than rollback_hours ago and a severe alert fires (panic, users with no
// serving link, tunnel probe p90 >= 3x baseline for 5 checks), the previous
// ExecStart drop-in (<rollback_dropin>.prev) is put back and the unit
// restarted — at most once per release (the .prev file is consumed).

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// watchCfg: the b352 keys of alert.conf (embedded in alertCfg).
type watchCfg struct {
	LossPct        float64 // a path's loss (down or up) above this for 3 checks (0 = off)
	DarkLossPct    float64 // a served path drained to links 0 / target 0 with loss >= this (or cut) for 3 checks (0 = off)
	DiskPct        float64 // / or /var/log used above this (crit at +5, max 99)
	MemPct         float64 // RAM used (1 - MemAvailable/MemTotal) above this
	IdlePct        float64 // host CPU idle below this for 10 checks: upgrade hint (0 = off)
	UsersDropPct   float64 // users fell by more than this % within 3 min (0 = off)
	UsersMin       int     // ... from at least this many users
	DropSkip       string  // "00-06": UTC minutes of each hour not judged (hourly xray restart)
	StaleS         int     // status file older than this while the unit is up (0 = off)
	SubDiedMin     int     // "every sub died" lines per check: at least this many AND 5x the usual
	NewErrors      bool    // report error kinds never seen before (after a 1 h learning time)
	ProbeTarget    string  // host:port reached through the tunnel (light probe), "" = off
	ProbeFactor    float64 // probe p90 above baseline x this (and +20 ms) for 3 checks: warn
	ProbeLossPct   float64 // failed probe connects in the last 10 checks above this %: crit
	PeerProbe      string  // host:port of the other side's tunnel port, "" = off
	Rollback       string  // off | dry | on
	RollbackHours  float64 // only a release younger than this is rolled back
	RollbackDropin string  // the ExecStart drop-in; the previous one is <path>.prev
}

func defaultWatchCfg() watchCfg {
	return watchCfg{
		LossPct: 5, DarkLossPct: 50, DiskPct: 90, MemPct: 92, IdlePct: 10, UsersDropPct: 50, UsersMin: 50,
		DropSkip: "00-06", StaleS: 90, SubDiedMin: 30, NewErrors: true,
		ProbeFactor: 2, ProbeLossPct: 20, Rollback: "off", RollbackHours: 6,
	}
}

// watchConfKeys are the keys `hs2 alert conf set` accepts for the watchdog.
var watchConfKeys = []string{"loss_pct", "dark_loss_pct", "disk_pct", "mem_pct", "cpu_idle_pct", "users_drop_pct", "users_min",
	"users_drop_skip", "stale_s", "sub_died_min", "new_errors", "probe_target", "probe_factor", "probe_loss_pct",
	"peer_probe", "rollback", "rollback_hours", "rollback_dropin"}

// watchRuleNames are the b352 alert kinds (disable= / hs2 alert rule).
var watchRuleNames = []string{"stale", "loss", "dark", "disk", "mem", "cpuidle", "usersdrop", "subdied", "newerr",
	"tprobe", "tprobeslow", "tprobex3", "peer", "rollback"}

// parseWatchKey handles one b352 key; ok false = not a watchdog key.
func parseWatchKey(w *watchCfg, k, v string) (ok bool, err error) {
	num := func(min, max float64) float64 {
		f, e := strconv.ParseFloat(v, 64)
		if e != nil || f < min || f > max {
			err = fmt.Errorf("%s wants a number %g..%g", k, min, max)
			return 0
		}
		return f
	}
	hostPort := func() string {
		if v == "" || v == "off" {
			return ""
		}
		if _, p, e := net.SplitHostPort(v); e != nil || p == "" {
			err = fmt.Errorf("%s is host:port or off", k)
			return ""
		}
		return v
	}
	switch k {
	case "loss_pct":
		w.LossPct = num(0, 100)
	case "dark_loss_pct":
		w.DarkLossPct = num(0, 100)
	case "disk_pct":
		w.DiskPct = num(0, 100)
	case "mem_pct":
		w.MemPct = num(0, 100)
	case "cpu_idle_pct":
		w.IdlePct = num(0, 100)
	case "users_drop_pct":
		w.UsersDropPct = num(0, 100)
	case "users_min":
		w.UsersMin = int(num(1, 1e7))
	case "users_drop_skip":
		if _, _, e := parseSkip(v); e != nil {
			err = e
		} else {
			w.DropSkip = v
		}
	case "stale_s":
		w.StaleS = int(num(0, 86400))
	case "sub_died_min":
		w.SubDiedMin = int(num(0, 1e7))
	case "new_errors":
		switch v {
		case "on", "1", "true":
			w.NewErrors = true
		case "off", "0", "false":
			w.NewErrors = false
		default:
			err = fmt.Errorf("new_errors is on or off")
		}
	case "probe_target":
		w.ProbeTarget = hostPort()
	case "peer_probe":
		w.PeerProbe = hostPort()
	case "probe_factor":
		w.ProbeFactor = num(1, 100)
	case "probe_loss_pct":
		w.ProbeLossPct = num(0, 100)
	case "rollback":
		if v != "off" && v != "dry" && v != "on" {
			err = fmt.Errorf("rollback is off, dry or on")
		} else {
			w.Rollback = v
		}
	case "rollback_hours":
		w.RollbackHours = num(0, 720)
	case "rollback_dropin":
		if v != "" && !strings.HasPrefix(v, "/") {
			err = fmt.Errorf("rollback_dropin is an absolute path")
		} else {
			w.RollbackDropin = v
		}
	default:
		return false, nil
	}
	return true, err
}

// parseSkip: "MM-MM" (UTC minutes of the hour, end exclusive) or off.
func parseSkip(v string) (from, to int, err error) {
	if v == "" || v == "off" {
		return 0, 0, nil
	}
	a, b, ok := strings.Cut(v, "-")
	x, e1 := strconv.Atoi(strings.TrimSpace(a))
	y, e2 := strconv.Atoi(strings.TrimSpace(b))
	if !ok || e1 != nil || e2 != nil || x < 0 || x > 59 || y < 1 || y > 60 || x >= y {
		return 0, 0, fmt.Errorf("users_drop_skip is MM-MM (UTC minutes, e.g. 00-06) or off")
	}
	return x, y, nil
}

// inSkip: now falls in the hourly skip window (UTC minute).
func inSkip(w watchCfg, now time.Time) bool {
	a, b, err := parseSkip(w.DropSkip)
	if err != nil || a == b {
		return false
	}
	m := now.UTC().Minute()
	return m >= a && m < b
}

// ---- snapshot and state additions ----

// watchSnap: what the b352 collectors saw (part of alertSnap).
type watchSnap struct {
	StatusAgeS int64    // -1 unknown (no unit / not read), -2 no status file
	DiskPct    float64  // highest of / and /var/log, -1 unknown
	DiskPath   string   // which one
	MemPct     float64  // -1 unknown
	CPU        cpuTimes // /proc/stat now (zero = unknown)
	SubDied    int      // "every sub died" lines in the journal window, -1 unknown
	ErrLines   []panicLine
	TProbe     []float64 // probe_target connect times, ms; -1 = failed; nil = not run
	PeerErr    *string   // nil = not run, "" = reachable
}

type userSample struct {
	T int64 `json:"t"`
	N int   `json:"n"`
}

// watchState: the b352 part of alert-<unit>.json.
type watchState struct {
	CPUBusy    uint64             `json:"cpu_busy,omitempty"`
	CPUTotal   uint64             `json:"cpu_total,omitempty"`
	Users      []userSample       `json:"users_hist,omitempty"`
	DropRef    int                `json:"drop_ref,omitempty"`
	SubDied    []int              `json:"sub_died_hist,omitempty"`
	ErrSigs    map[string]int64   `json:"err_sigs,omitempty"`
	ErrSince   int64              `json:"err_since,omitempty"`
	JournalTS  int64              `json:"journal_ts,omitempty"`
	TProbe     [][]float64        `json:"tprobe_hist,omitempty"` // per check, last 10
	TProbeBase []float64          `json:"tprobe_base,omitempty"` // one p50 per 5 min, 24 h
	TProbeAt   int64              `json:"tprobe_at,omitempty"`
	Rolled     map[string]int64   `json:"rolled,omitempty"` // drop-in mtime -> when rolled back
	LastVals   map[string]float64 `json:"last_vals,omitempty"`
	PathUsed   map[string]int64   `json:"path_used,omitempty"` // path key -> last check it had links (dark rule)
}

const (
	watchErrSigMax   = 500
	watchErrLearnS   = 3600
	watchNewErrPer   = 3
	watchUsersKeepS  = 600
	watchUsersWinS   = 190
	watchTProbeKeep  = 10
	watchTProbeJudge = 3 // checks pooled for the p90
	watchSubDiedKeep = 60
	watchDarkNeed    = 3     // checks drained + bad before the dark alert
	watchDarkUsedS   = 86400 // a path counts as "in use" for a day after its last links
	watchDarkOffPct  = 20.0  // an active dark alert holds while loss stays >= this
)

// ---- rules ----

// evalWatch runs the b352 rules; called from evalAlerts with its cond/keep.
func evalWatch(cfg alertCfg, st *alertState, s alertSnap, active, settling, first bool, tx alertTexts, cond condFn, keep func(string)) []alertMsg {
	w, ws, now := cfg.watchCfg, &st.Watch, s.Now
	var out []alertMsg
	if ws.LastVals == nil {
		ws.LastVals = map[string]float64{}
	}
	skip := inSkip(w, now)

	// status file stale while the unit runs
	if w.StaleS > 0 && active && !settling && s.W.StatusAgeS != -1 {
		age := s.W.StatusAgeS
		cond("stale", age == -2 || age > int64(w.StaleS), 2, sevWarn, tx.stale(age))
	} else {
		keep("stale")
	}

	// per-path loss
	if s.Status != nil && active && !settling && w.LossPct > 0 {
		for _, p := range s.Status.Paths {
			if p.cut() || p.Links == 0 {
				keep("loss:" + p.key())
				continue
			}
			l := 0.0
			if p.LossDown != nil && *p.LossDown > l {
				l = *p.LossDown
			}
			if p.LossUp != nil && *p.LossUp > l {
				l = *p.LossUp
			}
			cond("loss:"+p.key(), l > w.LossPct, 3, sevWarn, tx.loss(shortPathKey(p.key()), l))
		}
	} else {
		keep("loss:")
	}

	// a path drained dark (wd365 miss): the LB took it to target 0 / links 0
	// because it is bad, so the loss rule (links > 0) and ipcut (target > 0)
	// both look away while the path is effectively down.
	statusFresh := s.W.StatusAgeS != -2 && (w.StaleS <= 0 || s.W.StatusAgeS <= int64(w.StaleS))
	if s.Status != nil && active && !settling && w.DarkLossPct > 0 && statusFresh {
		evalDark(w, st, s.Status.Paths, now, tx, cond, keep)
	} else {
		keep("dark:")
	}

	// disk / RAM
	if s.W.DiskPct >= 0 && w.DiskPct > 0 {
		sev := sevWarn
		if s.W.DiskPct >= minF(w.DiskPct+5, 99) {
			sev = sevCrit
		}
		cond("disk", s.W.DiskPct > w.DiskPct, 2, sev, tx.disk(s.W.DiskPath, s.W.DiskPct))
	} else {
		keep("disk")
	}
	if s.W.MemPct >= 0 && w.MemPct > 0 {
		cond("mem", s.W.MemPct > w.MemPct, 3, sevWarn, tx.mem(s.W.MemPct))
	} else {
		keep("mem")
	}

	// host CPU idle (upgrade hint): from /proc/stat between two checks
	if c := s.W.CPU; c.total() > 0 {
		busy := c.total() - c.idle - c.iowait
		if ws.CPUTotal > 0 && c.total() > ws.CPUTotal && busy >= ws.CPUBusy && w.IdlePct > 0 {
			dt := float64(c.total() - ws.CPUTotal)
			idle := 100 * (dt - float64(busy-ws.CPUBusy)) / dt
			ws.LastVals["cpu_idle"] = idle
			cond("cpuidle", idle < w.IdlePct, 10, sevWarn, tx.cpuIdle(idle, c.cores))
		} else {
			keep("cpuidle")
		}
		ws.CPUBusy, ws.CPUTotal = busy, c.total()
	} else {
		keep("cpuidle")
	}

	// users: a sharp drop (not in the hourly xray-restart window)
	if s.Status != nil && active {
		n := s.Status.Users
		ref := 0
		for _, u := range ws.Users {
			if now.Unix()-u.T <= watchUsersWinS && u.N > ref {
				ref = u.N
			}
		}
		if w.UsersDropPct > 0 && !settling && !skip {
			if a := st.Active["usersdrop"]; a != nil && ws.DropRef > 0 {
				// incident: on until back to 70 % of where it fell from
				cond("usersdrop", float64(n) < 0.7*float64(ws.DropRef), 1, sevWarn, tx.usersDrop(ws.DropRef, n))
			} else {
				on := ref >= w.UsersMin && float64(n) < float64(ref)*(1-w.UsersDropPct/100)
				if on && ws.DropRef < ref {
					ws.DropRef = ref // the highest count it fell from
				}
				cond("usersdrop", on, 2, sevWarn, tx.usersDrop(ws.DropRef, n))
			}
			if st.Active["usersdrop"] == nil && st.Streak["usersdrop"] == 0 {
				ws.DropRef = 0
			}
		} else {
			keep("usersdrop")
			st.Streak["usersdrop"] = 0
		}
		ws.Users = append(ws.Users, userSample{T: now.Unix(), N: n})
		for len(ws.Users) > 0 && now.Unix()-ws.Users[0].T > watchUsersKeepS {
			ws.Users = ws.Users[1:]
		}
	} else {
		keep("usersdrop")
	}

	// journal: "every sub died" bursts against this server's own usual rate
	if s.W.SubDied >= 0 {
		usual, _ := median(intsF(ws.SubDied))
		burst := w.SubDiedMin > 0 && len(ws.SubDied) >= 10 && s.W.SubDied >= w.SubDiedMin && float64(s.W.SubDied) > 5*usual
		cond("subdied", burst, 2, sevWarn, tx.subDied(s.W.SubDied, usual))
		if st.Active["subdied"] == nil && st.Streak["subdied"] == 0 { // bursts do not raise the baseline
			ws.SubDied = append(ws.SubDied, s.W.SubDied)
			if len(ws.SubDied) > watchSubDiedKeep {
				ws.SubDied = ws.SubDied[len(ws.SubDied)-watchSubDiedKeep:]
			}
		}
	} else {
		keep("subdied")
	}

	// journal: error kinds never seen before (event, not a condition)
	if w.NewErrors && len(s.W.ErrLines) > 0 {
		if ws.ErrSigs == nil {
			ws.ErrSigs = map[string]int64{}
		}
		if ws.ErrSince == 0 {
			ws.ErrSince = now.Unix()
		}
		learning := now.Unix()-ws.ErrSince < watchErrLearnS || first
		var fresh []string
		newest := ws.JournalTS
		for _, l := range s.W.ErrLines {
			if l.TS <= ws.JournalTS {
				continue
			}
			if l.TS > newest {
				newest = l.TS
			}
			sig := errSig(l.Text)
			if sig == "" {
				continue
			}
			if _, ok := ws.ErrSigs[sig]; ok {
				ws.ErrSigs[sig] = now.Unix()
				continue
			}
			if len(ws.ErrSigs) >= watchErrSigMax {
				pruneSigs(ws.ErrSigs)
			}
			ws.ErrSigs[sig] = now.Unix()
			if !learning {
				fresh = append(fresh, clip(l.Text, 140))
			}
		}
		ws.JournalTS = newest
		if len(fresh) > 0 {
			more := len(fresh) - watchNewErrPer
			if more > 0 {
				fresh = fresh[:watchNewErrPer]
			}
			out = append(out, alertMsg{Key: "newerr", Sev: sevInfo, Text: tx.newErrors(fresh, more)})
		}
	} else if ws.ErrSince == 0 && w.NewErrors {
		ws.ErrSince = now.Unix()
	}

	// light probe through the tunnel
	if s.W.TProbe != nil && active && !settling {
		ws.TProbe = append(ws.TProbe, s.W.TProbe)
		if len(ws.TProbe) > watchTProbeKeep {
			ws.TProbe = ws.TProbe[len(ws.TProbe)-watchTProbeKeep:]
		}
		var recent []float64
		fails, n := 0, 0
		for i, c := range ws.TProbe {
			for _, v := range c {
				n++
				if v < 0 {
					fails++
					continue
				}
				if i >= len(ws.TProbe)-watchTProbeJudge {
					recent = append(recent, v)
				}
			}
		}
		lossPct := 0.0
		if n > 0 {
			lossPct = 100 * float64(fails) / float64(n)
		}
		ws.LastVals["tprobe_loss"] = lossPct
		cond("tprobe", n >= 15 && lossPct > w.ProbeLossPct, 2, sevCrit, tx.tprobeLoss(w.ProbeTarget, lossPct))
		base, okBase := median(ws.TProbeBase)
		p90 := pctl(recent, 0.9)
		ws.LastVals["tprobe_p90"] = p90
		if okBase && len(ws.TProbeBase) >= rttMinSamples && len(recent) >= 3 && !skip {
			slow := p90 > base*w.ProbeFactor && p90 > base+20
			cond("tprobeslow", slow, 3, sevWarn, tx.tprobeSlow(p90, base))
			cond("tprobex3", p90 >= 3*base && p90 > base+30, 5, sevCrit, tx.tprobeSlow(p90, base))
		} else {
			keep("tprobeslow")
			keep("tprobex3")
		}
		// baseline: one median per 5 min while nothing is wrong
		if med, ok := median(s.W.TProbe); ok && st.Active["tprobeslow"] == nil && st.Active["tprobe"] == nil &&
			now.Unix()-ws.TProbeAt >= int64(rttSampleEvery/time.Second) && med > 0 {
			ws.TProbeAt = now.Unix()
			ws.TProbeBase = append(ws.TProbeBase, med)
			if len(ws.TProbeBase) > rttKeep {
				ws.TProbeBase = ws.TProbeBase[len(ws.TProbeBase)-rttKeep:]
			}
		}
	} else {
		keep("tprobe")
		keep("tprobeslow")
		keep("tprobex3")
	}

	// the other side's tunnel port (from Kharej: is Iran there at all?)
	if s.W.PeerErr != nil {
		cond("peer", *s.W.PeerErr != "", 3, sevCrit, tx.peerDown(w.PeerProbe, *s.W.PeerErr))
	} else {
		keep("peer")
	}
	return out
}

func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func intsF(v []int) []float64 {
	out := make([]float64, len(v))
	for i, x := range v {
		out[i] = float64(x)
	}
	return out
}

// pctl: the q-quantile (nearest rank) of v; 0 for none.
func pctl(v []float64, q float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	i := int(q*float64(len(s))+0.999999) - 1
	if i < 0 {
		i = 0
	}
	if i >= len(s) {
		i = len(s) - 1
	}
	return s[i]
}

var (
	reErrLine = regexp.MustCompile(`(?i)\b(error|err=|failed|fatal)\b`)
	reSigNum  = regexp.MustCompile(`\[[0-9a-fA-F:.]+\]|[0-9a-fA-F]*[0-9][0-9a-fA-F:.x/%-]*`)
	reSigWS   = regexp.MustCompile(`\s+`)
)

// isErrLine: plain substring tests (a regexp here cost ~2 us a line, most of
// a check's CPU on a chatty journal).
func isErrLine(l string) bool {
	for _, w := range [...]string{"error", "Error", "ERROR", "failed", "FAILED", "fatal", "FATAL", "err="} {
		if strings.Contains(l, w) {
			return true
		}
	}
	return false
}

// errSig: an error line without its numbers, addresses and ids — the "kind".
func errSig(text string) string {
	// b437 F5: a draining old process prefixes its lines; same kind.
	text = strings.Replace(text, "[draining] ", "", 1)
	s := reSigNum.ReplaceAllString(text, "#")
	s = reSigWS.ReplaceAllString(strings.TrimSpace(s), " ")
	if len(s) > 80 {
		s = s[:80]
	}
	return s
}

// pruneSigs drops the oldest-seen half.
func pruneSigs(m map[string]int64) {
	type kv struct {
		k string
		t int64
	}
	var all []kv
	for k, t := range m {
		all = append(all, kv{k, t})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].t < all[j].t })
	for _, e := range all[:len(all)/2] {
		delete(m, e.k)
	}
}

// ---- auto-rollback (default off) ----

// rollbackPlan says whether this check should put the previous drop-in back.
// dropinMtime 0 = no drop-in; prev = the .prev file exists.
func rollbackPlan(w watchCfg, st *alertState, msgs []alertMsg, dropinMtime int64, prev bool, now time.Time) (do bool, why string) {
	if w.Rollback != "dry" && w.Rollback != "on" || w.RollbackDropin == "" || dropinMtime <= 0 || !prev {
		return false, ""
	}
	if float64(now.Unix()-dropinMtime) > w.RollbackHours*3600 {
		return false, ""
	}
	if st.Watch.Rolled[strconv.FormatInt(dropinMtime, 10)] != 0 {
		return false, "" // this release was rolled back (or tried) already
	}
	for _, m := range msgs {
		if m.Key == "panic" && m.Sev == sevCrit {
			return true, "panic"
		}
	}
	if a := st.Active["dead"]; a != nil && now.Unix()-a.Since >= 60 {
		return true, "users connected but no link serves them"
	}
	if st.Active["tprobex3"] != nil {
		return true, "tunnel probe p90 >= 3x baseline for 5 min"
	}
	return false, ""
}

// doRollbackDropin: <dropin> -> <dropin>.bad, <dropin>.prev -> <dropin>,
// daemon-reload, restart the unit.
func doRollbackDropin(unit, dropin string) error {
	if err := os.Rename(dropin, dropin+".bad"); err != nil {
		return err
	}
	if err := os.Rename(dropin+".prev", dropin); err != nil {
		_ = os.Rename(dropin+".bad", dropin)
		return err
	}
	if out, err := exec.Command("systemctl", "daemon-reload").CombinedOutput(); err != nil {
		return fmt.Errorf("daemon-reload: %v %s", err, clip(string(out), 120))
	}
	if out, err := exec.Command("systemctl", "restart", unit).CombinedOutput(); err != nil {
		return fmt.Errorf("restart: %v %s", err, clip(string(out), 120))
	}
	return nil
}

// applyRollback runs after the rules (runAlertOnce); returns a message or nil.
func applyRollback(cfg alertCfg, st *alertState, unit string, msgs []alertMsg, now time.Time, dry bool) *alertMsg {
	w := cfg.watchCfg
	if w.Rollback != "dry" && w.Rollback != "on" || w.RollbackDropin == "" {
		return nil
	}
	var mt int64
	if fi, err := os.Stat(w.RollbackDropin); err == nil {
		mt = fi.ModTime().Unix()
	}
	_, perr := os.Stat(w.RollbackDropin + ".prev")
	do, why := rollbackPlan(w, st, msgs, mt, perr == nil, now)
	if !do {
		return nil
	}
	tx := alertText(cfg.Lang)
	if st.Watch.Rolled == nil {
		st.Watch.Rolled = map[string]int64{}
	}
	if w.Rollback == "dry" || dry {
		if !dry {
			st.Watch.Rolled[strconv.FormatInt(mt, 10)] = now.Unix()
		}
		return &alertMsg{Key: "rollback", Sev: sevWarn, Text: tx.rollback(why, true, "")}
	}
	st.Watch.Rolled[strconv.FormatInt(mt, 10)] = now.Unix()
	e := ""
	if err := doRollbackDropin(unit, w.RollbackDropin); err != nil {
		e = err.Error()
	}
	return &alertMsg{Key: "rollback", Sev: sevCrit, Text: tx.rollback(why, false, e)}
}

// ---- collection (read-only; linux bits in alert_watch_linux.go) ----

func collectWatch(cfg alertCfg, s *alertSnap, statusBytes []byte, statusErr error, journal string) {
	w := cfg.watchCfg
	s.W.StatusAgeS = -1
	if s.Active == "active" {
		switch {
		case statusErr != nil:
			s.W.StatusAgeS = -2
		default:
			var u struct {
				Updated int64 `json:"updated"`
			}
			if jsonUnmarshalUpdated(statusBytes, &u.Updated) && u.Updated > 0 {
				s.W.StatusAgeS = s.Now.Unix() - u.Updated
				if s.W.StatusAgeS < 0 {
					s.W.StatusAgeS = 0
				}
			}
		}
	}
	s.W.DiskPct, s.W.DiskPath = diskUsedPct("/", "/var/log")
	s.W.MemPct = memUsedPct("/proc/meminfo")
	if c, ok := readProcStat("/proc"); ok {
		s.W.CPU = c
	}
	s.W.SubDied, s.W.ErrLines = parseJournalWatch(journal)
	if w.ProbeTarget != "" && s.Active == "active" {
		s.W.TProbe = tcpProbe(w.ProbeTarget, 3, 3*time.Second)
	}
	if w.PeerProbe != "" {
		v := tcpProbe(w.PeerProbe, 1, 5*time.Second)
		e := ""
		if len(v) == 1 && v[0] < 0 {
			// one retry: a single lost SYN is not "the other side is gone"
			if v = tcpProbe(w.PeerProbe, 1, 5*time.Second); v[0] < 0 {
				e = "connect failed"
			}
		}
		s.W.PeerErr = &e
	}
}

// jsonUnmarshalUpdated pulls "updated":N out of the status file without a
// second full decode.
func jsonUnmarshalUpdated(b []byte, out *int64) bool {
	i := strings.Index(string(b), `"updated":`)
	if i < 0 {
		return false
	}
	rest := strings.TrimLeft(string(b[i+len(`"updated":`):]), " ")
	j := 0
	for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
		j++
	}
	v, err := strconv.ParseInt(rest[:j], 10, 64)
	if err != nil {
		return false
	}
	*out = v
	return true
}

// parseJournalWatch: the journal window (short-unix) -> "every sub died"
// count and the error lines.
func parseJournalWatch(out string) (subDied int, errs []panicLine) {
	if out == "" {
		return -1, nil
	}
	for _, l := range strings.Split(out, "\n") {
		if l == "" {
			continue
		}
		if strings.Contains(l, "every sub died") {
			subDied++
			continue // its own rule, not a "new error kind"
		}
		if !isErrLine(l) || strings.Contains(l, "panic:") {
			continue
		}
		ts, rest, _ := strings.Cut(l, " ")
		f, err := strconv.ParseFloat(ts, 64)
		if err != nil {
			continue
		}
		if i := strings.Index(rest, ": "); i >= 0 {
			rest = rest[i+2:]
		}
		errs = append(errs, panicLine{TS: int64(f * 1e6), Text: rest})
	}
	return subDied, errs
}

func memUsedPct(path string) float64 {
	f, err := os.Open(path)
	if err != nil {
		return -1
	}
	defer f.Close()
	return parseMemInfo(f)
}

func parseMemInfo(r interface{ Read([]byte) (int, error) }) float64 {
	var total, avail float64 = -1, -1
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 {
			continue
		}
		v, err := strconv.ParseFloat(f[1], 64)
		if err != nil {
			continue
		}
		switch f[0] {
		case "MemTotal:":
			total = v
		case "MemAvailable:":
			avail = v
		}
	}
	if total <= 0 || avail < 0 {
		return -1
	}
	return 100 * (1 - avail/total)
}

// tcpProbe: n TCP connects (sequential), ms each, -1 = failed.
func tcpProbe(addr string, n int, timeout time.Duration) []float64 {
	out := make([]float64, 0, n)
	for i := 0; i < n; i++ {
		t0 := time.Now()
		c, err := net.DialTimeout("tcp", addr, timeout)
		if err != nil {
			out = append(out, -1)
			continue
		}
		out = append(out, float64(time.Since(t0).Microseconds())/1000)
		c.Close()
	}
	return out
}

// evalDark: a path that carried links (within watchDarkUsedS) and now has
// none or a target of 0, with evidence it is bad — loss >= DarkLossPct (the
// dialing side's smoothed figure, which the listening side keeps showing with
// no links), the auto judge's "cut" reason, or a cut state — for
// watchDarkNeed checks. Not judged: a path that never had links (a config
// spare), one with low loss and no cut verdict (a share set to 0, a healthy
// drain), and one whose every IP ipcut already reports. A path removed from
// the config leaves lb_paths, so its alert resolves as a gone key.
func evalDark(w watchCfg, st *alertState, paths []alertPath, now time.Time, tx alertTexts, cond condFn, keep func(string)) {
	ws := &st.Watch
	if ws.PathUsed == nil {
		ws.PathUsed = map[string]int64{}
	}
	all, cutN := map[string]int{}, map[string]int{} // per IP: paths, cut paths (ipcut's view)
	for _, p := range paths {
		for _, ip := range p.ips() {
			all[ip]++
			if p.cut() {
				cutN[ip]++
			}
		}
	}
	present := map[string]bool{}
	for _, p := range paths {
		k := p.key()
		if p.Remote == "" && p.Local != "" {
			// the listening side learns a path's remote from its links: with
			// none the row reads "local>" — judge it as the path it was
			var best int64
			for uk, t := range ws.PathUsed {
				if strings.HasPrefix(uk, p.Local+">") && uk != k && t > best {
					k, best = uk, t
				}
			}
		}
		key := "dark:" + k
		present[k] = true
		if p.Links > 0 {
			ws.PathUsed[k] = now.Unix()
		}
		used, ok := ws.PathUsed[k]
		if (p.Links > 0 && p.Target > 0) || !ok || now.Unix()-used > watchDarkUsedS {
			cond(key, false, watchDarkNeed, sevCrit, "")
			continue
		}
		// ipcut already ALERTING for every IP of the path: it reports it. Only
		// an active ipcut counts — a drained dark path's target flips between
		// 0 and a few links (lab wd365b: 0/5/10/41 with links 0), so ipcut's
		// own streak keeps restarting and the dark streak must not.
		ips := p.ips()
		owned := len(ips) > 0
		for _, ip := range ips {
			owned = owned && cutN[ip] == all[ip] && st.Active["ipcut:"+ip] != nil
		}
		if owned {
			keep(key) // an active dark alert neither resolves nor repeats
			continue
		}
		l := -1.0
		if p.LossDown != nil {
			l = *p.LossDown
		}
		if p.LossUp != nil && *p.LossUp > l {
			l = *p.LossUp
		}
		judged := strings.HasPrefix(p.AutoReason, "cut")
		switch p.State {
		case "cooling", "probing", "no-peer":
			judged = true
		}
		if l < 0 && !judged {
			keep(key) // no loss figure and no verdict: no evidence either way
			continue
		}
		bar := w.DarkLossPct
		if st.Active[key] != nil && bar > watchDarkOffPct {
			bar = watchDarkOffPct // hysteresis: resolves below 20 %, not just below 50 %
		}
		cond(key, judged || l >= bar, watchDarkNeed, sevCrit, tx.dark(k, l, p.AutoReason))
	}
	if len(paths) > 0 {
		for k := range ws.PathUsed {
			if !present[k] {
				delete(ws.PathUsed, k)
			}
		}
	}
}

// ---- texts ----

func (t alertTexts) stale(age int64) string {
	if age == -2 {
		return t.pick("فایل وضعیت تونل نیست (سرویس بالاست ولی وضعیت نمی‌نویسد)", "status file missing while the unit runs")
	}
	return t.pick(fmt.Sprintf("فایل وضعیت تونل %d ثانیه است به‌روز نشده (پردازه گیر کرده؟)", age),
		fmt.Sprintf("status file not updated for %d s (daemon stuck?)", age))
}

func (t alertTexts) loss(path string, l float64) string {
	return t.pick(fmt.Sprintf("اتلاف بالا روی مسیر %s: %.1f%%", path, l), fmt.Sprintf("high loss on path %s: %.1f%%", path, l))
}

func (t alertTexts) dark(path string, l float64, why string) string {
	ls := "?"
	if l >= 0 {
		ls = fmt.Sprintf("%.0f%%", l)
	}
	if why != "" && why != "ok" {
		ls += " (" + why + ")"
	}
	return t.pick(fmt.Sprintf("مسیر %s تاریک است و از سرویس خارج شد: لینک ۰، اتلاف %s", path, ls),
		fmt.Sprintf("path %s is dark and drained: 0 links, loss %s", path, ls))
}

func (t alertTexts) disk(path string, p float64) string {
	return t.pick(fmt.Sprintf("دیسک %s پر است: %.0f%%", path, p), fmt.Sprintf("disk %s nearly full: %.0f%%", path, p))
}

func (t alertTexts) mem(p float64) string {
	return t.pick(fmt.Sprintf("رم سرور تقریباً پر است: %.0f%% مصرف", p), fmt.Sprintf("server RAM nearly full: %.0f%% used", p))
}

func (t alertTexts) cpuIdle(idle float64, cores int) string {
	return t.pick(fmt.Sprintf("CPU سرور ۱۰ دقیقه است که تقریباً پر است: آزاد %.0f%% (%d هسته) — ارتقای سرور را در نظر بگیرید", idle, cores),
		fmt.Sprintf("server CPU nearly saturated for 10 min: %.0f%% idle (%d cores) — consider an upgrade", idle, cores))
}

func (t alertTexts) usersDrop(from, to int) string {
	return t.pick(fmt.Sprintf("کاربران ناگهان کم شد: %d ← %d (در کمتر از ۳ دقیقه)", from, to),
		fmt.Sprintf("users dropped sharply: %d -> %d (within 3 min)", from, to))
}

func (t alertTexts) subDied(n int, usual float64) string {
	return t.pick(fmt.Sprintf("موج قطع اتصال‌ها: %d «every sub died» در ۲ دقیقه (معمول %.0f)", n, usual),
		fmt.Sprintf("connection-death burst: %d \"every sub died\" in 2 min (usual %.0f)", n, usual))
}

func (t alertTexts) newErrors(lines []string, more int) string {
	s := t.pick("خطای نوع جدید در لاگ:", "new kind of error in the log:")
	for _, l := range lines {
		s += "\n• " + l
	}
	if more > 0 {
		s += t.pick(fmt.Sprintf("\n(+%d نوع دیگر)", more), fmt.Sprintf("\n(+%d more kinds)", more))
	}
	return s
}

func (t alertTexts) tprobeLoss(target string, l float64) string {
	return t.pick(fmt.Sprintf("پروب سبک از داخل تونل (%s) شکست می‌خورد: %.0f%% در ۱۰ دقیقهٔ اخیر", target, l),
		fmt.Sprintf("light probe through the tunnel (%s) failing: %.0f%% in the last 10 min", target, l))
}

func (t alertTexts) tprobeSlow(p90, base float64) string {
	return t.pick(fmt.Sprintf("تأخیر پروب سبک داخل تونل بالاست: p90 %.0f ms (معمول %.0f ms)", p90, base),
		fmt.Sprintf("light probe through the tunnel slow: p90 %.0f ms (usual %.0f ms)", p90, base))
}

func (t alertTexts) peerDown(addr, e string) string {
	return t.pick(fmt.Sprintf("سرور طرف مقابل (%s) در دسترس نیست: %s", addr, e),
		fmt.Sprintf("the other side (%s) is unreachable: %s", addr, e))
}

func (t alertTexts) rollback(why string, dry bool, e string) string {
	if dry {
		return t.pick("بازگشت خودکار (حالت آزمایشی، کاری انجام نشد) به نسخهٔ قبلی می‌شد: "+why,
			"auto-rollback (dry: nothing done) would restore the previous release: "+why)
	}
	if e != "" {
		return t.pick("بازگشت خودکار به نسخهٔ قبلی ناموفق: "+why+" — "+e, "auto-rollback to the previous release FAILED: "+why+" — "+e)
	}
	return t.pick("بازگشت خودکار به نسخهٔ قبلی انجام شد و سرویس ری‌استارت شد: "+why,
		"auto-rolled back to the previous release and restarted: "+why)
}
