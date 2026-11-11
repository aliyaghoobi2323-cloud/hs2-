package main

// Menu M4, section 3 "Load balancing & IPs": `hs2 lb …`. The menu only renders
// this output; every change goes through the semantic diff + the M1 safe apply
// (hs2 apply --confirm-within, automatic rollback) under the admin lock.
//
//	hs2 lb show    -u U [--lang fa] [--history]   live path table (+ cuts of the last 24 h)
//	hs2 lb history -u U [--hours 24]              cut / recovery / flap events per path
//	hs2 lb suggest -u U [--peer IP]               ping the peer(s) from every local IP, rank, suggest the primary
//	hs2 lb add-ip  -u U --ip X [--weight W] [--sni S] [--cert F --key F] [--front tls]
//	hs2 lb remove-ip -u U --ip X
//	hs2 lb mode    -u U fixed|auto
//	hs2 lb weights -u U ip=w,ip=w… | --equal
//	hs2 lb park    -u U --ip X --hours N          weight 0 until then; restored by hs2-lb-park.timer
//	hs2 lb unpark  -u U --ip X
//	hs2 lb park --sweep                           (the timer) restore every expired park
//	hs2 lb parks                                  list parked IPs
//
// Common flags of the changing commands: --confirm-within 5m, --yes, --dry-run.

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/engine"
)

var (
	parkDir       = "/var/lib/hs2"
	parkSystemctl = realSystemctl
	lbParkTimer   = "hs2-lb-park.timer"
	lbParkSvc     = "hs2-lb-park.service"
	lbJournalFor  = func(unit string, since time.Duration) string {
		args := []string{"-u", cleanUnit(unit) + ".service", "--since", fmt.Sprintf("-%ds", int(since.Seconds())), "-o", "short-iso", "--no-pager", "-q"}
		out, err := exec.Command("journalctl", append(args, "--grep", "lb auto2: (path|flap)")...).Output()
		if err != nil && len(out) == 0 { // journalctl without pcre2 (no --grep), or nothing matched: filter here
			out, _ = exec.Command("journalctl", args...).Output()
		}
		return string(out)
	}
)

func lbCmd(args []string) {
	if len(args) == 0 {
		fmt.Println("usage: hs2 lb show|history|suggest|add-ip|remove-ip|mode|weights|park|unpark|parks …  (hs2 lb --supported)")
		os.Exit(2)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "--supported":
		fmt.Println("lb-v1")
	case "show":
		lbShowCmd(rest)
	case "history":
		lbHistoryCmd(rest)
	case "suggest":
		lbSuggestCmd(rest)
	case "parks":
		lbParksCmd(rest)
	case "add-ip", "remove-ip", "mode", "weights", "park", "unpark":
		os.Exit(lbChangeCmd(sub, rest))
	default:
		fmt.Println("unknown lb command: " + sub)
		os.Exit(2)
	}
}

// ---- live path table --------------------------------------------------------

func lbTableRows(ls liveStatus) [][]string {
	var rows [][]string
	for _, p := range ls.LBPaths {
		rows = append(rows, lbRow(p.Local+" > "+p.Remote, p))
	}
	if ls.LBTotal != nil {
		rows = append(rows, lbRow(tr("total", "جمع"), *ls.LBTotal))
	}
	return rows
}

func lbRow(name string, p engine.LBPathView) []string {
	share := fmt.Sprintf("%.0f%%", p.SharePct)
	if p.EffPct > 0 {
		share += fmt.Sprintf("/%.0f%%", p.EffPct)
	}
	loss := "-"
	var l []string
	if p.LossDown != nil {
		l = append(l, fmt.Sprintf("↓%.1f", *p.LossDown))
	}
	if p.LossUp != nil {
		l = append(l, fmt.Sprintf("↑%.1f", *p.LossUp))
	}
	if len(l) > 0 {
		loss = strings.Join(l, " ") + "%"
	}
	rtt := "-"
	if p.RTTp50 > 0 {
		rtt = fmt.Sprintf("%.0f/%.0f", p.RTTp50, p.RTTp99)
	}
	auto := "-"
	if p.AutoFactor > 0 {
		auto = fmt.Sprintf("×%.2f", p.AutoFactor)
		if p.AutoReason != "" {
			auto += " " + p.AutoReason
		}
	}
	return []string{name, share, fmt.Sprintf("%d/%d", p.Links, p.Target), strconv.Itoa(p.Users),
		fmt.Sprintf("↓%.1f ↑%.1f", p.DownMbit, p.UpMbit), loss, rtt, auto, lbPathState(p)}
}

// renderLBTable: the live LB page as aligned columns.
func renderLBTable(ls liveStatus, hist map[string]int) string {
	rows := lbTableRows(ls)
	if len(rows) == 0 {
		return tr("No load-balancing paths in the live status (lb off, or the tunnel is not running).",
			"مسیر لودبالانسی در وضعیت زنده نیست (lb خاموش است یا تونل اجرا نمی‌شود).") + "\n"
	}
	hdr := []string{tr("path", "مسیر"), tr("share/eff", "سهم/مؤثر"), tr("links", "لینک"), tr("users", "کاربر"),
		"Mbit", tr("loss", "افت"), "RTT ms", tr("auto", "خودکار"), tr("state", "وضعیت")}
	if hist != nil {
		hdr = append(hdr, tr("cuts 24h", "قطع ۲۴س"))
		for i := range rows {
			n := 0
			if i < len(ls.LBPaths) {
				n = hist[lbHistKey(ls.LBPaths[i].Local, ls.LBPaths[i].Remote)]
			}
			if i < len(ls.LBPaths) {
				rows[i] = append(rows[i], strconv.Itoa(n))
			} else {
				rows[i] = append(rows[i], "")
			}
		}
	}
	all := append([][]string{hdr}, rows...)
	w := make([]int, len(hdr))
	for _, r := range all {
		for i, c := range r {
			if i < len(w) && dispWidth(c) > w[i] {
				w[i] = dispWidth(c)
			}
		}
	}
	var b strings.Builder
	for _, r := range all {
		for i, c := range r {
			b.WriteString(c)
			if i < len(r)-1 {
				b.WriteString(strings.Repeat(" ", w[i]-dispWidth(c)+2))
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

func lbHistKey(local, remote string) string { return local + ">" + remote }

// ---- cut history (journal) ---------------------------------------------------

type lbEvent struct {
	When, Path, Kind, Text string
}

var lbEvRe = regexp.MustCompile(`^(\S+) .*lb auto2: path (\S+) (cut|recovering|flapping|back from flap penalty|fast recovery)(.*)$`)

// parseLBEvents reads `journalctl -o short-iso` lines of the auto2 judge.
func parseLBEvents(log string) []lbEvent {
	var out []lbEvent
	for _, ln := range strings.Split(log, "\n") {
		m := lbEvRe.FindStringSubmatch(strings.TrimSpace(ln))
		if m == nil {
			continue
		}
		kind := m[3]
		switch kind {
		case "back from flap penalty":
			kind = "back"
		case "fast recovery":
			kind = "fast-recovery"
		}
		out = append(out, lbEvent{When: m[1], Path: m[2], Kind: kind, Text: strings.TrimSpace(strings.TrimLeft(m[4], " :—-"))})
	}
	return out
}

// cutCounts: cuts per path key ("local>remote" as the judge names it).
func cutCounts(ev []lbEvent) map[string]int {
	m := map[string]int{}
	for _, e := range ev {
		if e.Kind == "cut" {
			m[e.Path]++
		}
	}
	return m
}

func lbStatusOf(unit string) (liveStatus, bool) {
	cfg := configForUnit(unit)
	b, err := os.ReadFile(filepath.Join(statusRunDir, filepath.Base(statusPath(cfg))))
	if err != nil {
		return liveStatus{}, false
	}
	var ls liveStatus
	if json.Unmarshal(b, &ls) != nil {
		return liveStatus{}, false
	}
	return ls, true
}

func lbShowCmd(args []string) {
	fs := flag.NewFlagSet("lb show", flag.ExitOnError)
	unit := fs.String("u", "hs2", "tunnel")
	lang := langFlag(fs)
	hist := fs.Bool("history", true, "add the cuts of the last 24 h")
	fs.Parse(args)
	setLang(*lang)
	_, fc, _ := loadUnit(*unit)
	fmt.Println(lbConfigSummary(fc))
	ls, ok := lbStatusOf(*unit)
	if !ok {
		fmt.Println(tr("(no live status — is the tunnel running?)", "(وضعیت زنده نیست — تونل اجرا می‌شود؟)"))
	}
	var h map[string]int
	if *hist {
		h = cutCounts(parseLBEvents(lbJournalFor(*unit, 24*time.Hour)))
	}
	fmt.Print(renderLBTable(ls, h))
	if ps := loadParks(*unit); len(ps) > 0 {
		for _, p := range ps {
			fmt.Printf(tr("parked: %s until %s\n", "پارک‌شده: %s تا %s\n"), p.IP, time.Unix(p.Until, 0).Format("2006-01-02 15:04"))
			if p.Tries >= 3 {
				fmt.Printf(tr("  automatic restore of %s rolled back 3 times — restore by hand: hs2 lb unpark -u %s --ip %s\n",
					"  بازگرداندن خودکار %s سه بار برگشت خورد — دستی برگردانید: hs2 lb unpark -u %s --ip %s\n"), p.IP, cleanUnit(*unit), p.IP)
			}
		}
	}
}

func lbConfigSummary(fc fileConfig) string {
	if !lbOn(fc) {
		return tr("lb: off (one path). Add a local IP to start load balancing.", "lb: خاموش (یک مسیر). برای شروع لودبالانس یک IP محلی اضافه کنید.")
	}
	f := func(l []lbIP) string {
		var s []string
		for _, e := range l {
			s = append(s, fmt.Sprintf("%s=%d", e.IP, e.Weight))
		}
		if len(s) == 0 {
			return "-"
		}
		return strings.Join(s, " ")
	}
	mode := fc.LB.Mode
	if mode == "" {
		mode = "fixed"
	}
	return fmt.Sprintf("lb: %s · iran: %s · kharej: %s", mode, f(fc.LB.Iran), f(fc.LB.Kharej))
}

func lbHistoryCmd(args []string) {
	fs := flag.NewFlagSet("lb history", flag.ExitOnError)
	unit := fs.String("u", "hs2", "tunnel")
	hours := fs.Int("hours", 24, "look back")
	lang := langFlag(fs)
	fs.Parse(args)
	setLang(*lang)
	ev := parseLBEvents(lbJournalFor(*unit, time.Duration(*hours)*time.Hour))
	if len(ev) == 0 {
		fmt.Printf(tr("no path cut in the last %d h.\n", "در %d ساعت گذشته هیچ مسیری قطع نشد.\n"), *hours)
		return
	}
	for p, n := range cutCounts(ev) {
		fmt.Printf("%-40s %s %d\n", p, tr("cuts:", "قطع:"), n)
	}
	for _, e := range ev {
		fmt.Printf("%s  %-30s %-14s %s\n", e.When, e.Path, e.Kind, e.Text)
	}
}

// ---- local IP suggestion ----------------------------------------------------

type ipProbe struct {
	IP      string
	Peer    string
	RTT     float64 // avg ms
	Loss    float64 // %
	OK      bool
	Current bool
}

var lbPing = func(src, dst string) pingResult {
	args := []string{"-n", "-q", "-c", "5", "-i", "0.2", "-W", "1", "-I", src}
	if ip := net.ParseIP(dst); ip != nil && ip.To4() == nil {
		args = append(args, "-6")
	}
	out, _ := exec.Command("ping", append(args, dst)...).Output()
	return parsePing(string(out))
}

// rankIPs orders probes: answering first, then lower loss (1 % steps), then
// lower RTT; the current primary stays first when within 1 ms and 1 % of the
// best (no churn for nothing).
func rankIPs(ps []ipProbe) []ipProbe {
	out := append([]ipProbe(nil), ps...)
	sort.SliceStable(out, func(a, b int) bool {
		x, y := out[a], out[b]
		if x.OK != y.OK {
			return x.OK
		}
		if lx, ly := int(x.Loss), int(y.Loss); lx != ly {
			return lx < ly
		}
		return x.RTT < y.RTT
	})
	if len(out) > 1 && out[0].OK && !out[0].Current {
		for i, p := range out {
			if p.Current && p.OK && p.RTT-out[0].RTT < 1 && p.Loss-out[0].Loss < 1 {
				c := out[i]
				copy(out[1:i+1], out[0:i])
				out[0] = c
				break
			}
		}
	}
	return out
}

// lbSide: which list of lb is this server's own, and the peer's IPs.
func lbSide(fc fileConfig) (own string, peers []string) {
	iran := fc.Mode == "dial"
	own = "kharej"
	if iran {
		own = "iran"
	}
	pi, pk := lbPrimaries(fc)
	peerPrim := pk
	if !iran {
		peerPrim = pi
	}
	if peerPrim == "" && dialing(fc) { // this side dials addr: that is the peer
		if h, _, err := net.SplitHostPort(fc.Addr); err == nil && net.ParseIP(h) != nil && !net.ParseIP(h).IsUnspecified() {
			peerPrim = h
		}
	}
	if peerPrim != "" {
		peers = append(peers, peerPrim)
	}
	if fc.LB != nil {
		l := fc.LB.Kharej
		if !iran {
			l = fc.LB.Iran
		}
		for _, e := range l {
			if e.IP != peerPrim {
				peers = append(peers, e.IP)
			}
		}
	}
	return own, peers
}

func ownList(fc fileConfig) []lbIP {
	if fc.LB == nil {
		return nil
	}
	if own, _ := lbSide(fc); own == "iran" {
		return fc.LB.Iran
	}
	return fc.LB.Kharej
}

func lbSuggestCmd(args []string) {
	fs := flag.NewFlagSet("lb suggest", flag.ExitOnError)
	unit := fs.String("u", "hs2", "tunnel")
	peer := fs.String("peer", "", "peer IP to ping (default: the peer's IPs from the config)")
	lang := langFlag(fs)
	fs.Parse(args)
	setLang(*lang)
	_, fc, _ := loadUnit(*unit)
	_, peers := lbSide(fc)
	if *peer != "" {
		peers = []string{*peer}
	}
	if len(peers) == 0 {
		die(tr("no peer IP known for %s — pass --peer IP", "IP سرور مقابل برای %s معلوم نیست — --peer IP بدهید"), *unit)
	}
	cur := fc.BindLocalIP
	if h, _, err := net.SplitHostPort(fc.Addr); err == nil && fc.Mode == "dial" == fc.Reverse {
		cur = h // this side listens on addr
	}
	fmt.Println(tr("Pinging the peer from every local public IP (5 pings each)…", "پینگ سرور مقابل از هر IP عمومی محلی (هرکدام ۵ پینگ)…"))
	var ps []ipProbe
	for _, ip := range localPublicIPs() {
		for _, p := range peers {
			if (net.ParseIP(ip).To4() == nil) != (net.ParseIP(p).To4() == nil) {
				continue
			}
			r := lbPing(ip, p)
			ps = append(ps, ipProbe{IP: ip, Peer: p, RTT: r.Avg, Loss: r.Loss, OK: r.OK, Current: ip == cur})
		}
	}
	fmt.Print(renderSuggest(rankIPs(ps), ownList(fc)))
}

func renderSuggest(ps []ipProbe, own []lbIP) string {
	var b strings.Builder
	inLB := map[string]bool{}
	for _, e := range own {
		inLB[e.IP] = true
	}
	for i, p := range ps {
		mark := "  "
		if i == 0 && p.OK {
			mark = "★ "
		}
		st := tr("no answer", "بی‌پاسخ")
		if p.OK {
			st = fmt.Sprintf("%.1f ms  loss %.0f%%", p.RTT, p.Loss)
		}
		var tags []string
		if p.Current {
			tags = append(tags, tr("current primary", "اصلی فعلی"))
		}
		if inLB[p.IP] {
			tags = append(tags, tr("in lb", "در lb"))
		}
		t := ""
		if len(tags) > 0 {
			t = " (" + strings.Join(tags, ", ") + ")"
		}
		fmt.Fprintf(&b, "%s%-18s → %-18s %s%s\n", mark, p.IP, p.Peer, st, t)
	}
	if len(ps) > 0 && ps[0].OK {
		fmt.Fprintf(&b, tr("Suggested primary: %s (lowest loss, then lowest RTT).\n", "IP اصلی پیشنهادی: %s (کمترین افت، سپس کمترین RTT).\n"), ps[0].IP)
	} else {
		b.WriteString(tr("No local IP got an answer — check the peer address and ICMP.\n", "هیچ IP محلی پاسخ نگرفت — آدرس سرور مقابل و ICMP را بررسی کنید.\n"))
	}
	return b.String()
}

// ---- weights ------------------------------------------------------------------

// splitEqual gives n IPs weights summing to 100 (the remainder to the first).
func splitEqual(l []lbIP) []lbIP {
	out := append([]lbIP(nil), l...)
	if len(out) == 0 {
		return out
	}
	each := 100 / len(out)
	for i := range out {
		out[i].Weight = each
	}
	out[0].Weight += 100 - each*len(out)
	return out
}

// parseWeights: "ip=w,ip=w" over the current list; every listed IP must be in
// it, every IP of it must be given, the sum must be 100.
func parseWeights(spec string, cur []lbIP) ([]lbIP, error) {
	want := map[string]int{}
	for _, f := range strings.Split(spec, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		ip, w, ok := strings.Cut(f, "=")
		n, err := strconv.Atoi(strings.TrimSpace(w))
		if !ok || err != nil || n < 0 || n > 100 {
			return nil, fmt.Errorf(tr("bad weight %q (want ip=0..100)", "وزن نادرست %q (باید ip=0..100 باشد)"), f)
		}
		p := net.ParseIP(strings.TrimSpace(ip))
		if p == nil {
			return nil, fmt.Errorf(tr("%q is not an IP", "%q یک IP نیست"), ip)
		}
		want[p.String()] = n
	}
	out := append([]lbIP(nil), cur...)
	sum := 0
	for i, e := range out {
		w, ok := want[net.ParseIP(e.IP).String()]
		if !ok {
			return nil, fmt.Errorf(tr("no weight given for %s (give every IP of this side)", "برای %s وزنی داده نشد (وزن همهٔ IPهای این سمت را بدهید)"), e.IP)
		}
		out[i].Weight = w
		sum += w
		delete(want, net.ParseIP(e.IP).String())
	}
	for ip := range want {
		return nil, fmt.Errorf(tr("%s is not in this side's lb list (add it first)", "%s در فهرست lb این سمت نیست (اول اضافه کنید)"), ip)
	}
	if err := weightsSum100(out); err != nil {
		return nil, err
	}
	return out, nil
}

func weightsSum100(l []lbIP) error {
	sum := 0
	for _, e := range l {
		sum += e.Weight
	}
	if len(l) > 0 && sum != 100 {
		return fmt.Errorf(tr("weights sum to %d, must be 100", "جمع وزن‌ها %d است، باید ۱۰۰ باشد"), sum)
	}
	return nil
}

// ---- park / unpark --------------------------------------------------------------

type lbPark struct {
	Unit  string `json:"unit"`
	IP    string `json:"ip"`
	Until int64  `json:"until"`
	Prior []lbIP `json:"prior"`           // this side's list before the park
	Tries int    `json:"tries,omitempty"` // timer restores started (3 that rolled back = give up)
}

func parkPath(unit string) string { return filepath.Join(parkDir, "lb-park-"+cleanUnit(unit)+".json") }

func loadParks(unit string) []lbPark {
	var ps []lbPark
	if b, err := os.ReadFile(parkPath(unit)); err == nil {
		json.Unmarshal(b, &ps)
	}
	return ps
}

func saveParks(unit string, ps []lbPark) error {
	if len(ps) == 0 {
		os.Remove(parkPath(unit))
		return nil
	}
	os.MkdirAll(parkDir, 0o755)
	b, _ := json.MarshalIndent(ps, "", "  ")
	return writeFileAtomic(parkPath(unit), b)
}

// parkWeights: ip gets weight 0, its weight goes to the others in proportion
// to theirs (equal split when they are all 0). It never parks the last
// active IP.
func parkWeights(l []lbIP, ip string) ([]lbIP, error) {
	out := append([]lbIP(nil), l...)
	idx, rest := -1, 0
	active := 0
	for i, e := range out {
		if e.IP == ip {
			idx = i
		} else {
			rest += e.Weight
		}
		if e.Weight > 0 {
			active++
		}
	}
	if idx < 0 {
		return nil, fmt.Errorf(tr("%s is not in this side's lb list", "%s در فهرست lb این سمت نیست"), ip)
	}
	if out[idx].Weight == 0 {
		return nil, fmt.Errorf(tr("%s is already at weight 0", "وزن %s همین حالا صفر است"), ip)
	}
	if active <= 1 || len(out) < 2 {
		return nil, errors.New(tr("cannot park the only active IP", "تنها IP فعال را نمی‌شود پارک کرد"))
	}
	free := out[idx].Weight
	out[idx].Weight = 0
	given := 0
	last := -1
	for i := range out {
		if i == idx {
			continue
		}
		var add int
		if rest > 0 {
			add = free * out[i].Weight / rest
		} else {
			add = free / (len(out) - 1)
		}
		out[i].Weight += add
		given += add
		last = i
	}
	out[last].Weight += free - given
	return out, nil
}

// unparkWeights: back to the list before the park when this side's IPs are
// still the same set; otherwise ip rejoins with an equal share and the rest
// are scaled to fit.
func unparkWeights(cur, prior []lbIP, ip string) []lbIP {
	same := len(cur) == len(prior)
	if same {
		m := map[string]bool{}
		for _, e := range cur {
			m[e.IP] = true
		}
		for _, e := range prior {
			if !m[e.IP] {
				same = false
			}
		}
	}
	if same {
		w := map[string]int{}
		for _, e := range prior {
			w[e.IP] = e.Weight
		}
		out := append([]lbIP(nil), cur...)
		for i := range out {
			out[i].Weight = w[out[i].IP]
		}
		return out
	}
	out := append([]lbIP(nil), cur...)
	idx := -1
	for i, e := range out {
		if e.IP == ip {
			idx = i
		}
	}
	if idx < 0 {
		return out
	}
	share := 100 / len(out)
	rest := 100 - share
	sum := 0
	for i, e := range out {
		if i != idx {
			sum += e.Weight
		}
	}
	given := 0
	last := -1
	for i := range out {
		if i == idx {
			continue
		}
		if sum > 0 {
			out[i].Weight = out[i].Weight * rest / sum
		} else {
			out[i].Weight = rest / (len(out) - 1)
		}
		given += out[i].Weight
		last = i
	}
	out[idx].Weight = share
	if last >= 0 {
		out[last].Weight += rest - given
	}
	return out
}

// expiredParks splits parks into those due now and those still running; a
// park whose timer restore rolled back 3 times is no longer due (given up).
func expiredParks(ps []lbPark, now time.Time) (due, keep []lbPark) {
	for _, p := range ps {
		if p.Until <= now.Unix() && p.Tries < 3 {
			due = append(due, p)
		} else {
			keep = append(keep, p)
		}
	}
	return
}

func parkTimerTexts(bin string) (svc, timer string) {
	svc = "[Unit]\nDescription=hs2 lb: restore parked IPs whose time is up (hs2 lb park --sweep)\n[Service]\nType=oneshot\nExecStart=" + bin + " lb park --sweep\n"
	timer = "[Unit]\nDescription=hs2 lb park restore (every minute, survives reboot)\n[Timer]\nOnBootSec=1min\nOnUnitActiveSec=1min\n[Install]\nWantedBy=timers.target\n"
	return
}

func ensureParkTimer(on bool) {
	if on {
		svc, tm := parkTimerTexts(selfBin())
		writeFileAtomic(filepath.Join(systemdDir, lbParkSvc), []byte(svc))
		writeFileAtomic(filepath.Join(systemdDir, lbParkTimer), []byte(tm))
		parkSystemctl("daemon-reload")
		parkSystemctl("enable", "--now", lbParkTimer)
		return
	}
	parkSystemctl("disable", "--now", lbParkTimer)
	os.Remove(filepath.Join(systemdDir, lbParkTimer))
	os.Remove(filepath.Join(systemdDir, lbParkSvc))
	parkSystemctl("daemon-reload")
}

func anyParks() bool {
	ms, _ := filepath.Glob(filepath.Join(parkDir, "lb-park-*.json"))
	return len(ms) > 0
}

func lbParksCmd(args []string) {
	ms, _ := filepath.Glob(filepath.Join(parkDir, "lb-park-*.json"))
	if len(ms) == 0 {
		fmt.Println(tr("no parked IP.", "IP پارک‌شده‌ای نیست."))
	}
	for _, m := range ms {
		u := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(m), "lb-park-"), ".json")
		for _, p := range loadParks(u) {
			fmt.Printf("%s  %s  %s %s\n", u, p.IP, tr("until", "تا"), time.Unix(p.Until, 0).Format("2006-01-02 15:04:05"))
		}
	}
}

// parkSweep (the timer): restore every expired park through the safe apply,
// confirm a restore whose trial is healthy (only then the park is dropped),
// and give up after 3 restores that rolled back (the IP stays at weight 0,
// shown by `hs2 lb show`; restore it by hand with `hs2 lb unpark`).
func parkSweep(now time.Time) int {
	ms, _ := filepath.Glob(filepath.Join(parkDir, "lb-park-*.json"))
	for _, m := range ms {
		u := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(m), "lb-park-"), ".json")
		ps := loadParks(u)
		st, hasSt := loadState(u)
		mine := hasSt && st.Reason == "lb-unpark-timer"
		if mine && st.State == stTrial {
			if err := confirmApply(realOps{}, u); err == nil {
				_, keep := expiredParks(ps, now)
				saveParks(u, keep)
				fmt.Printf("%s: restore confirmed\n", u)
			}
			continue
		}
		if hasSt && st.live() {
			continue // an apply (ours or an admin's) is still running
		}
		due, _ := expiredParks(ps, now)
		if len(due) == 0 {
			continue
		}
		if _, err := os.Stat(configForUnit(u)); err != nil {
			// the tunnel was deleted while parked: nothing to restore
			fmt.Printf("%s: no config any more — park dropped\n", u)
			saveParks(u, nil)
			continue
		}
		path, fc, cur := loadUnit(u)
		var nb []byte
		for _, p := range due {
			b, err := withOwnList(cur, fc, func(l []lbIP) ([]lbIP, error) { return unparkWeights(l, p.Prior, p.IP), nil })
			if err != nil {
				fmt.Printf("%s: %v\n", u, err)
				continue
			}
			cur, nb = b, b
			json.Unmarshal(cur, &fc)
		}
		for i := range ps {
			if ps[i].Until <= now.Unix() {
				ps[i].Tries++
			}
		}
		saveParks(u, ps)
		if nb == nil {
			continue
		}
		if _, _, err := startApply(realOps{}, applyReq{Unit: u, Cfg: path, Reason: "lb-unpark-timer", New: nb, Within: 3 * time.Minute, HealthGate: true}); err != nil {
			fmt.Printf("%s: restore failed: %v (retry next round)\n", u, err)
			continue
		}
		audit(now, "hs2-lb-park.timer", "", u, "lb-unpark", "timer", "restore "+parkIPs(due), "started")
		fmt.Printf("%s: restoring %s\n", u, parkIPs(due))
	}
	if !anyParks() {
		ensureParkTimer(false)
	}
	return 0
}

func parkIPs(ps []lbPark) string {
	var s []string
	for _, p := range ps {
		s = append(s, p.IP)
	}
	return strings.Join(s, ",")
}

// withOwnList rewrites this side's lb list in raw config b.
func withOwnList(b []byte, fc fileConfig, f func([]lbIP) ([]lbIP, error)) ([]byte, error) {
	m, err := loadConfigMap(b)
	if err != nil {
		return nil, err
	}
	own, _ := lbSide(fc)
	lb := lbConfig{Enabled: true, Mode: "auto"}
	if fc.LB != nil {
		lb = *fc.LB
	}
	l := lb.Kharej
	if own == "iran" {
		l = lb.Iran
	}
	nl, err := f(append([]lbIP(nil), l...))
	if err != nil {
		return nil, err
	}
	if own == "iran" {
		lb.Iran = nl
	} else {
		lb.Kharej = nl
	}
	if lb.Kharej == nil {
		lb.Kharej = []lbIP{}
	}
	if lb.Iran == nil {
		lb.Iran = []lbIP{}
	}
	lb.Enabled = true
	m["lb"] = lb
	out, err := marshalConfig(m)
	if err != nil {
		return nil, err
	}
	// lbIP's front never marshals its private key: keep the file's.
	return keepPrivateKeys(b, out), nil
}

func loadConfigMap(b []byte) (map[string]any, error) {
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

// ---- the changing commands ------------------------------------------------------

func lbChangeCmd(sub string, args []string) int {
	fs := flag.NewFlagSet("lb "+sub, flag.ExitOnError)
	unit := fs.String("u", "hs2", "tunnel")
	ip := fs.String("ip", "", "local IP")
	weight := fs.Int("weight", -1, "add-ip: its weight (default: split equally)")
	sni := fs.String("sni", "", "add-ip: SNI on this IP (TLS)")
	cert := fs.String("cert", "", "add-ip: certificate on this IP (listening side)")
	key := fs.String("key", "", "add-ip: key on this IP")
	front := fs.String("front", "tls", "add-ip: front on this IP (tls; then hs2 reality front … for reality / both)")
	equal := fs.Bool("equal", false, "weights: split equally")
	hours := fs.Float64("hours", 0, "park: hours")
	sweep := fs.Bool("sweep", false, "park: restore expired parks (the timer)")
	within := fs.Duration("confirm-within", 5*time.Minute, "trial time before the automatic rollback")
	yes := fs.Bool("yes", false, "apply without asking")
	dry := fs.Bool("dry-run", false, "show the change only")
	lang := langFlag(fs)
	// positional value (mode fixed|auto, weights spec) may come first or last
	pos := parseInterleaved(fs, args)
	setLang(*lang)
	if sub == "park" && *sweep {
		return parkSweep(time.Now())
	}
	if !*dry {
		rel, err := acquireLock(os.Getpid(), false, time.Now())
		if err != nil {
			fmt.Println("ERROR: " + err.Error())
			return 1
		}
		defer rel()
	}
	path, fc, cur := loadUnit(*unit)
	if fc.Carrier != "mtcp" && fc.Carrier != "l3mtcp" && fc.Carrier != "l3" {
		fmt.Println(tr("ERROR: this carrier has no load balancing (mtcp / l3mtcp only)", "خطا: این حامل لودبالانس ندارد (فقط mtcp / l3mtcp)"))
		return 1
	}
	var nb []byte
	var err error
	var after func()
	switch sub {
	case "add-ip":
		p := net.ParseIP(*ip)
		if p == nil {
			fmt.Println(tr("ERROR: --ip must be an IP of this server", "خطا: --ip باید یک IP همین سرور باشد"))
			return 1
		}
		if !isLocalIP(p) {
			fmt.Printf(tr("ERROR: %s is not on any interface of this server (ip -br addr)\n", "خطا: %s روی هیچ کارت شبکهٔ این سرور نیست (ip -br addr)\n"), p)
			return 1
		}
		if *front != "tls" {
			fmt.Println(tr("ERROR: --front: add the IP with tls, then switch it with zero downtime: hs2 reality front -u U --ip IP reality --target HOST:443", "خطا: --front: IP را با tls اضافه کنید، سپس بدون قطعی عوض کنید: hs2 reality front -u U --ip IP reality --target HOST:443"))
			return 1
		}
		if (*cert == "") != (*key == "") {
			fmt.Println(tr("ERROR: give --cert and --key together", "خطا: --cert و --key را با هم بدهید"))
			return 1
		}
		nb, err = withOwnList(cur, fc, func(l []lbIP) ([]lbIP, error) {
			for _, e := range l {
				if net.ParseIP(e.IP).Equal(p) {
					return nil, fmt.Errorf(tr("%s is already in the list", "%s همین حالا در فهرست است"), p)
				}
			}
			if len(l) == 0 {
				// the primary joins the list with its own weight, so the
				// split is explicit
				pi, pk := lbPrimaries(fc)
				prim := pk
				if own, _ := lbSide(fc); own == "iran" {
					prim = pi
				}
				if prim == "" { // a wildcard listener: its public IP is the primary
					prim = firstOther(localPublicIPs(), p.String())
				}
				if prim != "" && prim != p.String() {
					l = append(l, lbIP{IP: prim})
				}
			}
			l = append(l, lbIP{IP: p.String(), SNI: *sni, CertFile: *cert, KeyFile: *key})
			if *weight >= 0 {
				l[len(l)-1].Weight = *weight
				rest := 100 - *weight
				others := l[:len(l)-1]
				if len(others) > 0 {
					eq := splitEqual(others)
					for i := range eq {
						eq[i].Weight = eq[i].Weight * rest / 100
					}
					s := 0
					for _, e := range eq {
						s += e.Weight
					}
					eq[0].Weight += rest - s
					copy(l, eq)
				}
				return l, nil
			}
			return splitEqual(l), nil
		})
	case "remove-ip":
		p := net.ParseIP(*ip)
		nb, err = withOwnList(cur, fc, func(l []lbIP) ([]lbIP, error) {
			var out []lbIP
			for _, e := range l {
				if p == nil || !net.ParseIP(e.IP).Equal(p) {
					out = append(out, e)
				}
			}
			if len(out) == len(l) {
				return nil, fmt.Errorf(tr("%s is not in the list", "%s در فهرست نیست"), *ip)
			}
			if len(out) == 0 {
				return []lbIP{}, nil
			}
			return splitEqual(out), nil
		})
	case "mode":
		if len(pos) != 1 || (pos[0] != "fixed" && pos[0] != "auto") {
			fmt.Println("usage: hs2 lb mode -u U fixed|auto")
			return 2
		}
		m, e := loadConfigMap(cur)
		if e != nil || fc.LB == nil {
			fmt.Println(tr("ERROR: lb is off — add a local IP first", "خطا: lb خاموش است — اول یک IP محلی اضافه کنید"))
			return 1
		}
		lb := *fc.LB
		lb.Mode = pos[0]
		m["lb"] = lb
		nb, err = marshalConfig(m)
	case "weights":
		nb, err = withOwnList(cur, fc, func(l []lbIP) ([]lbIP, error) {
			if *equal {
				return splitEqual(l), nil
			}
			if len(pos) != 1 {
				return nil, errors.New("usage: hs2 lb weights -u U ip=w,ip=w… | --equal")
			}
			return parseWeights(pos[0], l)
		})
	case "park":
		if *hours <= 0 || *hours > 24*14 {
			fmt.Println(tr("ERROR: --hours must be > 0 and at most 336 (14 days)", "خطا: --hours باید بزرگ‌تر از ۰ و حداکثر ۳۳۶ (۱۴ روز) باشد"))
			return 1
		}
		prior := ownList(fc)
		nb, err = withOwnList(cur, fc, func(l []lbIP) ([]lbIP, error) { return parkWeights(l, net.ParseIP(*ip).String()) })
		until := time.Now().Add(time.Duration(*hours * float64(time.Hour)))
		after = func() {
			ps := loadParks(*unit)
			ps = append(ps, lbPark{Unit: cleanUnit(*unit), IP: net.ParseIP(*ip).String(), Until: until.Unix(), Prior: prior})
			saveParks(*unit, ps)
			ensureParkTimer(true)
			fmt.Printf(tr("parked %s until %s (restored by %s, also after a reboot)\n", "%s تا %s پارک شد (%s برمی‌گرداند، حتی پس از ری‌بوت)\n"), *ip, until.Format("2006-01-02 15:04"), lbParkTimer)
		}
	case "unpark":
		var prior []lbIP
		var keep []lbPark
		found := false
		for _, p := range loadParks(*unit) {
			if p.IP == net.ParseIP(*ip).String() {
				prior, found = p.Prior, true
			} else {
				keep = append(keep, p)
			}
		}
		if !found {
			fmt.Printf(tr("%s is not parked.\n", "%s پارک نشده است.\n"), *ip)
			return 1
		}
		nb, err = withOwnList(cur, fc, func(l []lbIP) ([]lbIP, error) { return unparkWeights(l, prior, net.ParseIP(*ip).String()), nil })
		after = func() {
			saveParks(*unit, keep)
			if !anyParks() {
				ensureParkTimer(false)
			}
		}
	}
	if err != nil {
		fmt.Println("ERROR: " + err.Error())
		return 1
	}
	rc := safeApplyNew(*unit, path, cur, nb, "lb-"+sub, *within, *yes, *dry)
	if rc == 0 && !*dry && after != nil {
		if st, ok := loadState(*unit); ok && st.live() {
			after()
		}
	}
	if rc == 0 && !*dry {
		fmt.Println(tr("Bilateral: the lb lists live on BOTH servers. If the line above asks for it, run the printed `hs2 tunnel join --update` code on the other server.",
			"دوطرفه: فهرست‌های lb روی هر دو سرور هستند. اگر خط بالا خواست، کد چاپ‌شدهٔ `hs2 tunnel join --update` را روی سرور مقابل اجرا کنید."))
	}
	return rc
}

func setLang(l string) {
	if l == "fa" {
		deepLang = "fa"
	} else {
		deepLang = "en"
	}
}

// firstOther: the first IP of ips (same family as not) other than not.
func firstOther(ips []string, not string) string {
	n := net.ParseIP(not)
	for _, ip := range ips {
		p := net.ParseIP(ip)
		if p == nil || p.Equal(n) || (n != nil && (p.To4() == nil) != (n.To4() == nil)) {
			continue
		}
		return p.String()
	}
	return ""
}

// parseInterleaved parses flags and positional values in any order
// ("weights -u U SPEC --yes" as well as "weights SPEC -u U --yes").
func parseInterleaved(fs *flag.FlagSet, args []string) []string {
	var pos []string
	for {
		fs.Parse(args)
		args = fs.Args()
		if len(args) == 0 {
			return pos
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}
