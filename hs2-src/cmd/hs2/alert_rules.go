package main

// The alert rule engine (`hs2 alert`): pure functions over one snapshot of the
// tunnel (alertSnap) and the persisted state (alertState). No I/O here — the
// collector (alert.go) builds the snapshot, this decides what to say, and the
// sender delivers it. Every condition alert is de-duplicated: it fires once
// after its streak, repeats at most every ResendMin while still true, and
// sends one "resolved" message when it clears.

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// alertStatus is the part of the daemon's status JSON the rules read. Only the
// fields used here; an older/newer daemon simply leaves some empty.
type alertStatus struct {
	Role     string            `json:"role"`
	Links    int               `json:"links"`
	Target   int               `json:"target"`
	Serving  *int              `json:"serving,omitempty"`
	Users    int               `json:"users"`
	Phase    string            `json:"phase"`
	Retiring int               `json:"retiring,omitempty"`
	Mbit     float64           `json:"mbit"`
	PeakMbit float64           `json:"peak_mbit,omitempty"`
	CertDays *int              `json:"cert_days,omitempty"`
	PID      int               `json:"pid"`
	Updated  int64             `json:"updated"`
	RTTp50   float64           `json:"obs_ctrl_rtt_p50_ms,omitempty"`
	RTTp99   float64           `json:"obs_ctrl_rtt_p99_ms,omitempty"`
	Paths    []alertPath       `json:"lb_paths,omitempty"`
	Total    *alertPath        `json:"lb_total,omitempty"`
	Restarts *int              `json:"restarts,omitempty"` // if a daemon ever reports them
	Panics   *int              `json:"panics,omitempty"`
	Extra    map[string]string `json:"-"`
}

type alertPath struct {
	Local      string   `json:"local"`
	Remote     string   `json:"remote"`
	Target     int      `json:"target"`
	Links      int      `json:"links"`
	Serving    int      `json:"serving"`
	State      string   `json:"state"`
	AutoReason string   `json:"auto_reason,omitempty"`
	LossDown   *float64 `json:"loss_down_pct,omitempty"`
	LossUp     *float64 `json:"loss_up_pct,omitempty"`
	RTTp50     float64  `json:"rtt_p50_ms,omitempty"`
	RTTp99     float64  `json:"rtt_p99_ms,omitempty"`
}

func (p alertPath) key() string { return p.Local + ">" + p.Remote }

// cut: the path carries nothing and is not expected to soon — cooling after
// failed dials, probing, the peer gone, or a share that wants links but has
// none at all.
func (p alertPath) cut() bool {
	switch p.State {
	case "cooling", "probing", "no-peer":
		return true
	case "draining":
		return false
	}
	return p.Target > 0 && p.Links == 0
}

// ips lists the addresses a path uses (local "*" = default route, skipped).
func (p alertPath) ips() []string {
	var out []string
	if p.Local != "" && p.Local != "*" {
		out = append(out, p.Local)
	}
	for _, r := range strings.Split(p.Remote, ",") {
		if r = strings.TrimSpace(r); r != "" && r != "*" {
			out = append(out, r)
		}
	}
	return out
}

// alertSnap is everything one check observed.
type alertSnap struct {
	Now       time.Time
	Unit      string
	Active    string       // systemctl is-active: active / failed / inactive / activating ...
	UptimeS   int64        // seconds since the unit (re)entered active, -1 = unknown
	MainPID   int          // 0 = unknown / not running
	NRestarts int          // -1 = unknown
	Panics    []panicLine  // journal panic/fatal lines of the last minutes
	Status    *alertStatus // nil: no fresh status file
	PSI60     float64      // /proc/pressure/cpu some avg60, -1 = unknown
	CertDays  int          // -1 = no cert / unknown
	ProbeMs   float64      // light probe, ms (0 = not run)
	ProbeErr  string       // light probe failure
}

type panicLine struct {
	TS   int64 // unix microseconds (journal)
	Text string
}

// alertCfg holds the thresholds (and the channels, see alert.go).
type alertCfg struct {
	BotToken    string
	ChatID      string
	Proxy       string
	APIURL      string // default https://api.telegram.org (tests point it at a fake)
	Lang        string // fa | en
	SummaryAt   string // HH:MM local, "" / "off" = no summary
	ResendMin   int
	RTTFactor   float64
	RTTMinAddMs float64
	PSIPct      float64
	CertDays    int
	FlapCuts    int
	Probe       string // off | tcp | tls
	LogFile     string
	Host        string // label in messages (default: hostname)
}

func defaultAlertCfg() alertCfg {
	return alertCfg{
		APIURL: "https://api.telegram.org", Lang: "fa", SummaryAt: "09:00",
		ResendMin: 30, RTTFactor: 1.6, RTTMinAddMs: 30, PSIPct: 25, CertDays: 10,
		FlapCuts: 4, Probe: "off", LogFile: "/var/log/hs2-alert.log",
	}
}

// Persisted state (/var/lib/hs2/alert-<unit>.json).
type alertState struct {
	Active    map[string]*activeAlert `json:"active"`
	Streak    map[string]int          `json:"streak"`
	PathCut   map[string]bool         `json:"path_cut"`
	CutTimes  map[string][]int64      `json:"cut_times"`
	RTT       map[string][]float64    `json:"rtt"`
	RTTHour   map[string][]float64    `json:"rtt_hour"` // "key|HH" -> samples of that hour-of-day, last days
	RTTAt     map[string]int64        `json:"rtt_at"`
	LastPID   int                     `json:"last_pid"`
	LastNRest int                     `json:"last_nrestarts"`
	LastPanic int64                   `json:"last_panic_us"`
	Day       alertDay                `json:"day"`
	LastSum   string                  `json:"last_summary"` // YYYY-MM-DD
	LastCheck int64                   `json:"last_check"`
	Recent    []string                `json:"recent,omitempty"` // last messages (for the menu)
}

type activeAlert struct {
	Sev      string `json:"sev"`
	Since    int64  `json:"since"`
	LastSent int64  `json:"last_sent"`
	Text     string `json:"text"`
}

// alertDay accumulates the daily summary.
type alertDay struct {
	Since     int64              `json:"since"`
	PeakUsers int                `json:"peak_users"`
	PeakMbit  float64            `json:"peak_mbit"`
	HourSum   map[string]float64 `json:"hour_sum"` // "HH" -> sum of RTT samples
	HourN     map[string]int     `json:"hour_n"`
	Cuts      map[string]int     `json:"cuts"` // per IP
	Restarts  int                `json:"restarts"`
	Alerts    int                `json:"alerts"`
}

func newAlertState() *alertState {
	return &alertState{}
}

func (st *alertState) fill(now time.Time) {
	if st.Active == nil {
		st.Active = map[string]*activeAlert{}
	}
	if st.Streak == nil {
		st.Streak = map[string]int{}
	}
	if st.PathCut == nil {
		st.PathCut = map[string]bool{}
	}
	if st.CutTimes == nil {
		st.CutTimes = map[string][]int64{}
	}
	if st.RTT == nil {
		st.RTT = map[string][]float64{}
	}
	if st.RTTHour == nil {
		st.RTTHour = map[string][]float64{}
	}
	if st.RTTAt == nil {
		st.RTTAt = map[string]int64{}
	}
	if st.Day.Since == 0 {
		st.Day.Since = now.Unix()
	}
	if st.Day.HourSum == nil {
		st.Day.HourSum = map[string]float64{}
	}
	if st.Day.HourN == nil {
		st.Day.HourN = map[string]int{}
	}
	if st.Day.Cuts == nil {
		st.Day.Cuts = map[string]int{}
	}
}

// Severities.
const (
	sevCrit = "crit"
	sevWarn = "warn"
	sevInfo = "info"
	sevOK   = "ok" // resolved
)

// alertMsg is one outgoing message.
type alertMsg struct {
	Key  string
	Sev  string
	Text string // body without the host header
}

// rtt baseline: one sample per rttSampleEvery per key, 24 h kept.
const (
	rttSampleEvery = 5 * time.Minute
	rttKeep        = 288 // 24 h at 5 min
	rttMinSamples  = 12  // an hour of history before judging
	rttHourKeep    = 84  // per hour-of-day: 7 days × 12
	rttHourMin     = 24  // two days of that hour before it replaces the 24 h median
)

// evalAlerts runs every rule on one snapshot and returns what to send.
func evalAlerts(cfg alertCfg, st *alertState, s alertSnap) []alertMsg {
	now := s.Now
	st.fill(now)
	first := st.LastCheck == 0
	st.LastCheck = now.Unix()
	tx := alertText(cfg.Lang)
	var out []alertMsg
	seen := map[string]bool{}

	// cond handles a level condition: streak, de-dup, resend, resolve.
	cond := func(key string, on bool, need int, sev, text string) {
		seen[key] = true
		if !on {
			st.Streak[key] = 0
			if a := st.Active[key]; a != nil {
				delete(st.Active, key)
				out = append(out, alertMsg{Key: key, Sev: sevOK, Text: tx.resolved(a.Text, now.Sub(time.Unix(a.Since, 0)))})
			}
			return
		}
		st.Streak[key]++
		if st.Streak[key] < need {
			return
		}
		a := st.Active[key]
		if a == nil {
			st.Active[key] = &activeAlert{Sev: sev, Since: now.Unix(), LastSent: now.Unix(), Text: text}
			st.Day.Alerts++
			out = append(out, alertMsg{Key: key, Sev: sev, Text: text})
			return
		}
		a.Text = text
		if now.Unix()-a.LastSent >= int64(cfg.ResendMin)*60 {
			a.LastSent = now.Unix()
			out = append(out, alertMsg{Key: key, Sev: sev, Text: tx.still(text, now.Sub(time.Unix(a.Since, 0)))})
		}
	}
	// keep: a conditional key that was not evaluated this round (no data)
	// stays as it is (neither resolved nor counted).
	keep := func(prefix string) {
		for k := range st.Active {
			if strings.HasPrefix(k, prefix) {
				seen[k] = true
			}
		}
	}

	// 1. the unit
	active := s.Active == "active" || s.Active == "reloading"
	// "failed"/"inactive" at once; a transient "activating"/"deactivating"
	// (a restart in progress) only when it lasts two checks.
	need := 1
	if s.Active != "failed" && s.Active != "inactive" {
		need = 2
	}
	wasDown := st.Active["unit"] != nil
	cond("unit", !active, need, sevCrit, tx.unitDown(s.Unit, s.Active))
	// settling: the first minutes after a (re)start — the pool ramps, the
	// LB probes its paths, RTT smoothing restarts. Pattern rules wait.
	settling := s.UptimeS >= 0 && s.UptimeS < alertSettleS
	if active && !first {
		auto := s.NRestarts >= 0 && st.LastNRest >= 0 && s.NRestarts > st.LastNRest
		restarted := auto || (st.LastPID != 0 && s.MainPID != 0 && s.MainPID != st.LastPID)
		if restarted {
			st.Day.Restarts++
			// coming back from a reported outage: the "resolved" says it all
			if !wasDown {
				n := -1
				if auto {
					n = s.NRestarts
				}
				out = append(out, alertMsg{Key: "restart", Sev: sevWarn, Text: tx.restarted(s.Unit, n)})
			}
		}
	}
	if s.MainPID != 0 {
		st.LastPID = s.MainPID
	}
	if s.NRestarts >= 0 {
		st.LastNRest = s.NRestarts
	}
	var newest int64
	var fresh []panicLine
	for _, p := range s.Panics {
		if p.TS > st.LastPanic {
			fresh = append(fresh, p)
		}
		if p.TS > newest {
			newest = p.TS
		}
	}
	if newest > st.LastPanic {
		st.LastPanic = newest
	}
	if len(fresh) > 0 && !first {
		out = append(out, alertMsg{Key: "panic", Sev: sevCrit, Text: tx.panicked(s.Unit, len(fresh), clip(fresh[0].Text, 160))})
	}

	// 2. links, paths, RTT — only with a fresh status file
	if s.Status != nil && active {
		ls := s.Status
		if ls.Users > st.Day.PeakUsers {
			st.Day.PeakUsers = ls.Users
		}
		pm := ls.Mbit
		if ls.PeakMbit > pm {
			pm = ls.PeakMbit
		}
		if pm > st.Day.PeakMbit {
			st.Day.PeakMbit = pm
		}
		serving := ls.Links
		if ls.Serving != nil {
			serving = *ls.Serving
		}
		// Critical, immediate: users are connected but nothing serves them.
		cond("dead", ls.Users > 0 && serving == 0, 1, sevCrit, tx.dead(ls.Users))
		// Links low: only against the autopilot's CURRENT target, only on a
		// pool big enough to matter (a quiet server shrinks to min 2), only
		// in a settled phase (not ramping/probing/shrinking), never right
		// after a restart; retiring links are not counted as missing (they
		// are outside both serving and target); 3 checks in a row.
		if settling || ls.Target <= alertLinksMinTarget || !steadyPhase(ls.Phase) {
			keep("links")
			st.Streak["links"] = 0
		} else {
			low := float64(serving) < 0.5*float64(ls.Target)
			cond("links", low, 3, sevWarn, tx.linksLow(serving, ls.Target))
		}
		if settling {
			keep("ipcut:")
			keep("flap:")
			keep("rtt")
			trackPathsQuiet(st, ls)
		} else {
			evalPaths(cfg, st, ls, now, tx, cond, &out)
			if ls.Links > 0 {
				evalRTT(cfg, st, ls, now, tx, cond)
			} else {
				keep("rtt")
			}
		}
	} else {
		keep("links")
		keep("dead")
		keep("ipcut:")
		keep("flap:")
		keep("rtt")
	}

	// 3. host CPU pressure
	if s.PSI60 >= 0 {
		cond("psi", s.PSI60 > cfg.PSIPct, 3, sevWarn, tx.psi(s.PSI60))
	} else {
		keep("psi")
	}

	// 4. certificate
	if s.CertDays >= 0 {
		sev := sevWarn
		if s.CertDays < 3 {
			sev = sevCrit
		}
		cond("cert", s.CertDays < cfg.CertDays, 1, sev, tx.cert(s.CertDays))
	} else {
		keep("cert")
	}

	// 5. light probe
	if cfg.Probe == "tcp" || cfg.Probe == "tls" {
		if active {
			cond("probe", s.ProbeErr != "", 3, sevWarn, tx.probe(cfg.Probe, s.ProbeErr))
		} else {
			keep("probe")
		}
	}

	// Keys that are active but were not evaluated at all this round (a path
	// that disappeared from the config, say) resolve.
	var gone []string
	for k := range st.Active {
		if !seen[k] {
			gone = append(gone, k)
		}
	}
	sort.Strings(gone)
	for _, k := range gone {
		a := st.Active[k]
		delete(st.Active, k)
		st.Streak[k] = 0
		out = append(out, alertMsg{Key: k, Sev: sevOK, Text: tx.resolved(a.Text, now.Sub(time.Unix(a.Since, 0)))})
	}
	for k := range st.Streak {
		if !seen[k] && st.Active[k] == nil {
			delete(st.Streak, k)
		}
	}
	return out
}

const (
	alertSettleS        = 180 // after a (re)start: pattern rules wait
	alertLinksMinTarget = 4   // links-low ignored for pools of this size or less
	rttSustain          = 5   // checks above the threshold before an RTT alert
)

// steadyPhase: the autopilot is not deliberately changing the pool size.
// An empty phase (older daemon) counts as steady.
func steadyPhase(p string) bool {
	switch p {
	case "", "steady", "holding", "following", "listening":
		return true
	}
	return false
}

// trackPathsQuiet records path cut state without counting transitions (the
// settle window after a restart: every path starts cut and comes up).
func trackPathsQuiet(st *alertState, ls *alertStatus) {
	for _, p := range ls.Paths {
		st.PathCut[p.key()] = p.cut()
	}
}

type condFn func(key string, on bool, need int, sev, text string)

func evalPaths(cfg alertCfg, st *alertState, ls *alertStatus, now time.Time, tx alertTexts, cond condFn, out *[]alertMsg) {
	if len(ls.Paths) == 0 {
		return
	}
	// cuts (ok -> cut transitions) per path, 1 h window
	hourAgo := now.Add(-time.Hour).Unix()
	seenPath := map[string]bool{}
	for _, p := range ls.Paths {
		k := p.key()
		seenPath[k] = true
		c := p.cut()
		if c && !st.PathCut[k] {
			st.CutTimes[k] = append(st.CutTimes[k], now.Unix())
			for _, ip := range p.ips() {
				st.Day.Cuts[ip]++
			}
		}
		st.PathCut[k] = c
		var kept []int64
		for _, t := range st.CutTimes[k] {
			if t >= hourAgo {
				kept = append(kept, t)
			}
		}
		st.CutTimes[k] = kept
		cond("flap:"+k, len(kept) >= cfg.FlapCuts, 1, sevWarn, tx.flap(k, len(kept)))
	}
	for k := range st.PathCut {
		if !seenPath[k] {
			delete(st.PathCut, k)
			delete(st.CutTimes, k)
		}
	}
	// an IP is fully cut when every path that uses it is cut
	all := map[string]int{}
	cutN := map[string]int{}
	reason := map[string]string{}
	for _, p := range ls.Paths {
		for _, ip := range p.ips() {
			all[ip]++
			if p.cut() {
				cutN[ip]++
				if reason[ip] == "" {
					reason[ip] = firstNonEmpty(p.AutoReason, p.State)
				}
			}
		}
	}
	ips := make([]string, 0, len(all))
	for ip := range all {
		ips = append(ips, ip)
	}
	sort.Strings(ips)
	for _, ip := range ips {
		cond("ipcut:"+ip, cutN[ip] == all[ip], 3, sevCrit, tx.ipCut(ip, all[ip], reason[ip]))
	}
}

func evalRTT(cfg alertCfg, st *alertState, ls *alertStatus, now time.Time, tx alertTexts, cond condFn) {
	type cur struct {
		key string
		v   float64
	}
	overall := ls.RTTp50
	if ls.Total != nil && ls.Total.RTTp50 > 0 {
		overall = ls.Total.RTTp50
	}
	if overall > 0 {
		h := fmt.Sprintf("%02d", now.Hour())
		st.Day.HourSum[h] += overall
		st.Day.HourN[h]++
	}
	var items []cur
	if len(ls.Paths) >= 1 {
		for _, p := range ls.Paths {
			if p.RTTp50 > 0 && !p.cut() {
				items = append(items, cur{"rtt:" + p.key(), p.RTTp50})
			}
		}
	}
	if len(items) == 0 && overall > 0 {
		items = append(items, cur{"rtt", overall})
	}
	type judged struct {
		cur
		base, thr float64
		high      bool
	}
	var js []judged
	for _, it := range items {
		hk := it.key + "|" + fmt.Sprintf("%02d", now.Hour())
		base, ok := median(st.RTT[it.key])
		n := len(st.RTT[it.key])
		if hb, hok := median(st.RTTHour[hk]); hok && len(st.RTTHour[hk]) >= rttHourMin {
			base, ok, n = hb, true, len(st.RTTHour[hk])
		}
		j := judged{cur: it}
		if ok && n >= rttMinSamples && base > 0 {
			j.base = base
			j.thr = base * cfg.RTTFactor
			if j.thr < base+cfg.RTTMinAddMs {
				j.thr = base + cfg.RTTMinAddMs
			}
			j.high = it.v > j.thr
			js = append(js, j)
		}
		// baseline sample
		if now.Unix()-st.RTTAt[it.key] >= int64(rttSampleEvery/time.Second) {
			st.RTTAt[it.key] = now.Unix()
			r := append(st.RTT[it.key], it.v)
			if len(r) > rttKeep {
				r = r[len(r)-rttKeep:]
			}
			st.RTT[it.key] = r
			h := append(st.RTTHour[hk], it.v)
			if len(h) > rttHourKeep {
				h = h[len(h)-rttHourKeep:]
			}
			st.RTTHour[hk] = h
		}
	}
	// drop baselines for keys that no longer exist (a removed path); keep them
	// while a path is only cut — it comes back with the same baseline.
	valid := map[string]bool{"rtt": true}
	for _, p := range ls.Paths {
		valid["rtt:"+p.key()] = true
	}
	for k := range st.RTT {
		if !valid[k] {
			delete(st.RTT, k)
			delete(st.RTTAt, k)
		}
	}
	for k := range st.RTTHour {
		if i := strings.LastIndex(k, "|"); i > 0 && !valid[k[:i]] {
			delete(st.RTTHour, k)
		}
	}
	nHigh := 0
	for _, j := range js {
		if j.high {
			nHigh++
		}
	}
	wide := len(js) >= 2 && nHigh == len(js)
	if wide {
		var parts []string
		for _, j := range js {
			parts = append(parts, fmt.Sprintf("%s %.0f/%.0f", shortPathKey(j.key), j.v, j.base))
		}
		cond("rtt:all", true, rttSustain, sevWarn, tx.rttWide(parts))
		// the per-path alerts fold into the network-wide one
		for _, j := range js {
			cond(j.key, false, rttSustain, sevWarn, "")
		}
		return
	}
	if len(js) >= 2 {
		cond("rtt:all", false, rttSustain, sevWarn, "")
	}
	for _, j := range js {
		cond(j.key, j.high, rttSustain, sevWarn, tx.rttHigh(shortPathKey(j.key), j.v, j.base))
	}
}

func shortPathKey(k string) string {
	k = strings.TrimPrefix(k, "rtt:")
	if k == "rtt" || k == "" {
		return "link"
	}
	return k
}

func median(v []float64) (float64, bool) {
	if len(v) == 0 {
		return 0, false
	}
	c := append([]float64(nil), v...)
	sort.Float64s(c)
	n := len(c)
	if n%2 == 1 {
		return c[n/2], true
	}
	return (c[n/2-1] + c[n/2]) / 2, true
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// dueSummary reports whether the daily summary is due now, and arms the
// state on the very first run (no summary of an empty day).
func dueSummary(cfg alertCfg, st *alertState, now time.Time) bool {
	if cfg.SummaryAt == "" || cfg.SummaryAt == "off" {
		return false
	}
	var hh, mm int
	if _, err := fmt.Sscanf(cfg.SummaryAt, "%d:%d", &hh, &mm); err != nil {
		return false
	}
	today := now.Format("2006-01-02")
	at := time.Date(now.Year(), now.Month(), now.Day(), hh, mm, 0, 0, now.Location())
	if st.LastSum == "" {
		st.LastSum = today
		if now.Before(at) {
			st.LastSum = now.AddDate(0, 0, -1).Format("2006-01-02")
		}
		return false
	}
	return !now.Before(at) && st.LastSum != today
}

// takeSummary formats and resets the daily stats.
func takeSummary(cfg alertCfg, st *alertState, now time.Time) string {
	st.fill(now)
	d := st.Day
	worstH, worstV := "", 0.0
	for h, sum := range d.HourSum {
		if n := d.HourN[h]; n > 0 && sum/float64(n) > worstV {
			worstH, worstV = h, sum/float64(n)
		}
	}
	var cuts []string
	ips := make([]string, 0, len(d.Cuts))
	for ip := range d.Cuts {
		ips = append(ips, ip)
	}
	sort.Strings(ips)
	for _, ip := range ips {
		cuts = append(cuts, fmt.Sprintf("%s×%d", ip, d.Cuts[ip]))
	}
	text := alertText(cfg.Lang).summary(d, worstH, worstV, cuts)
	st.LastSum = now.Format("2006-01-02")
	st.Day = alertDay{Since: now.Unix()}
	st.fill(now)
	return text
}
