package main

// Per-hour history for the dashboard's 24 h sparklines (menu M2).
//
//	hs2 stats sample [--etc /etc/hs2] [--run /run/hs2] [--state /var/lib/hs2]
//	hs2 stats show -u <unit>
//
// A sample folds each running tunnel's live status (link RTT p50/p99, Mbit,
// users) into an hourly aggregate kept for 7 days in
// /var/lib/hs2/stats-<unit>.json (a few KB). Nothing runs on its own: the
// alert check (`hs2 alert`, every 60 s when alerts are on) takes a sample
// for its tunnel, and the menu can turn on an optional hs2-stats.timer
// (every 5 min) for tunnels without alerts. The tunnel daemon is not touched.

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const statsKeep = 7 * 24 // hours

type statsHour struct {
	H        int64   `json:"h"` // unix time of the hour's start
	N        int     `json:"n"` // samples
	RTTN     int     `json:"rtt_n,omitempty"`
	RTTSum   float64 `json:"rtt_sum,omitempty"` // p50 samples, ms
	RTTMax   float64 `json:"rtt_max,omitempty"` // worst p99 seen, ms
	MbitSum  float64 `json:"mbit_sum,omitempty"`
	MbitMax  float64 `json:"mbit_max,omitempty"`
	UsersMax int     `json:"users_max,omitempty"`
}

type statsFile struct {
	Unit  string      `json:"unit"`
	Hours []statsHour `json:"hours"`
}

func (h statsHour) rttAvg() float64 {
	if h.RTTN == 0 {
		return 0
	}
	return h.RTTSum / float64(h.RTTN)
}

func (h statsHour) mbitAvg() float64 {
	if h.N == 0 {
		return 0
	}
	return h.MbitSum / float64(h.N)
}

func statsPath(dir, unit string) string { return filepath.Join(dir, "stats-"+unit+".json") }

func loadStats(path string) *statsFile {
	sf := &statsFile{}
	if b, err := os.ReadFile(path); err == nil {
		if json.Unmarshal(b, sf) != nil {
			sf = &statsFile{}
		}
	}
	return sf
}

func saveStats(path string, sf *statsFile) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, _ := json.Marshal(sf)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// statusRTT is the link round trip the status reports: the control RTT, else
// the LB total's.
func statusRTT(ls *liveStatus) (p50, p99 float64) {
	p50, p99 = ls.ObsCtrlRTTP50, ls.ObsCtrlRTTP99
	if p50 == 0 && ls.LBTotal != nil {
		p50, p99 = ls.LBTotal.RTTp50, ls.LBTotal.RTTp99
	}
	return
}

// addSample folds one status into its hour and drops hours older than 7 days.
func (sf *statsFile) addSample(ls *liveStatus, now time.Time) {
	h := now.Truncate(time.Hour).Unix()
	i := len(sf.Hours) - 1
	if i < 0 || sf.Hours[i].H != h {
		// keep the slice ordered even if the clock went back
		sf.Hours = append(sf.Hours, statsHour{H: h})
		sort.Slice(sf.Hours, func(a, b int) bool { return sf.Hours[a].H < sf.Hours[b].H })
		// merge duplicates (clock jumps)
		out := sf.Hours[:0]
		for _, x := range sf.Hours {
			if n := len(out); n > 0 && out[n-1].H == x.H {
				out[n-1] = mergeHour(out[n-1], x)
				continue
			}
			out = append(out, x)
		}
		sf.Hours = out
		for i = range sf.Hours {
			if sf.Hours[i].H == h {
				break
			}
		}
	}
	b := &sf.Hours[i]
	b.N++
	if p50, p99 := statusRTT(ls); p50 > 0 {
		b.RTTN++
		b.RTTSum += p50
		if p99 > b.RTTMax {
			b.RTTMax = p99
		}
	}
	b.MbitSum += ls.Mbit
	if ls.Mbit > b.MbitMax {
		b.MbitMax = ls.Mbit
	}
	if ls.Users > b.UsersMax {
		b.UsersMax = ls.Users
	}
	cut := h - int64(statsKeep-1)*3600
	j := 0
	for j < len(sf.Hours) && sf.Hours[j].H < cut {
		j++
	}
	sf.Hours = sf.Hours[j:]
}

func mergeHour(a, b statsHour) statsHour {
	a.N += b.N
	a.RTTN += b.RTTN
	a.RTTSum += b.RTTSum
	a.MbitSum += b.MbitSum
	if b.RTTMax > a.RTTMax {
		a.RTTMax = b.RTTMax
	}
	if b.MbitMax > a.MbitMax {
		a.MbitMax = b.MbitMax
	}
	if b.UsersMax > a.UsersMax {
		a.UsersMax = b.UsersMax
	}
	return a
}

// last24 returns 24 hourly slots ending with the current hour (nil = no data).
func (sf *statsFile) last24(now time.Time) []*statsHour {
	out := make([]*statsHour, 24)
	if sf == nil {
		return out
	}
	cur := now.Truncate(time.Hour).Unix()
	for i := range sf.Hours {
		k := 23 - int((cur-sf.Hours[i].H)/3600)
		if k >= 0 && k < 24 && sf.Hours[i].H <= cur {
			out[k] = &sf.Hours[i]
		}
	}
	return out
}

// sampleStatsFor samples one tunnel if its status file is fresh (a stopped
// tunnel adds no zeros).
func sampleStatsFor(cfgPath, unit, runDir, stateDir string, now time.Time) error {
	b, err := os.ReadFile(filepath.Join(runDir, filepath.Base(statusPath(cfgPath))))
	if err != nil {
		return nil
	}
	var ls liveStatus
	if json.Unmarshal(b, &ls) != nil || now.Unix()-ls.Updated > 30 {
		return nil
	}
	p := statsPath(stateDir, unit)
	sf := loadStats(p)
	sf.Unit = unit
	sf.addSample(&ls, now)
	return saveStats(p, sf)
}

func statsCmd(args []string) {
	if len(args) == 0 || (args[0] != "sample" && args[0] != "show") {
		fmt.Println("usage: hs2 stats sample [--etc DIR] [--run DIR] [--state DIR]  |  hs2 stats show -u <unit>")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("stats", flag.ExitOnError)
	etc := fs.String("etc", "/etc/hs2", "tunnel configs")
	run := fs.String("run", statusRunDir, "live status directory")
	state := fs.String("state", alertStateDir, "state directory")
	unit := fs.String("u", "", "tunnel unit (show)")
	fs.Parse(args[1:])
	now := time.Now()
	if args[0] == "show" {
		sf := loadStats(statsPath(*state, *unit))
		for _, h := range sf.Hours {
			fmt.Printf("%s  n=%-3d rtt_p50=%6.1f ms  rtt_max=%6.1f ms  mbit_avg=%7.1f  mbit_max=%7.1f  users_max=%d\n",
				time.Unix(h.H, 0).Format("2006-01-02 15:04"), h.N, h.rttAvg(), h.RTTMax, h.mbitAvg(), h.MbitMax, h.UsersMax)
		}
		return
	}
	n := 0
	for _, t := range listTunnelConfigs(*etc) {
		if err := sampleStatsFor(t.cfg, t.unit, *run, *state, now); err != nil {
			fmt.Fprintln(os.Stderr, "hs2 stats:", t.unit, err)
			continue
		}
		n++
	}
	_ = n
}

type tunnelCfg struct {
	cfg, unit string
	fc        fileConfig
}

// listTunnelConfigs returns every tunnel config in dir (a JSON with a mode),
// sorted by unit name.
func listTunnelConfigs(dir string) []tunnelCfg {
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	var out []tunnelCfg
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var fc fileConfig
		if json.Unmarshal(b, &fc) != nil || (fc.Mode != "dial" && fc.Mode != "listen") {
			continue
		}
		out = append(out, tunnelCfg{f, unitForConfig(f), fc})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].unit < out[b].unit })
	return out
}
