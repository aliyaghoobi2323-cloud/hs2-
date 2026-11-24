package censor

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
)

// Status is the read-only `censor` block of the status file. It is written
// by the evaluator (a fresh JSON copy each tick) and only ever rendered.
type Status struct {
	V        int               `json:"v"`
	Level    int               `json:"level"`
	Side     string            `json:"side"`
	Disabled bool              `json:"disabled,omitempty"`
	Dropped  uint64            `json:"dropped"`
	Evals    uint64            `json:"evals"`
	Scopes   int               `json:"scopes"`
	PathEv   [2]uint64         `json:"path_ev,omitempty"`   // path samples received, of them without a scope
	PeerMass uint64            `json:"peer_mass,omitempty"` // confirmed mass-RST peer restarts (slot 4)
	PeerOld  uint64            `json:"peer_old,omitempty"`  // confirmed old-signature peer restarts (slot 5)
	GenOld   uint64            `json:"gen_old,omitempty"`   // b437: link ends of the peer's older (ZDU) process, not counted as deaths
	GenNew   uint64            `json:"gen_new,omitempty"`   // b437: newer peer generations seen (ZDU takeovers)
	Open     []string          `json:"open,omitempty"`      // open episodes: "ip class/conf dur"
	Episodes map[string]uint32 `json:"episodes,omitempty"`  // closed: "class/conf" → count
	Recent   []string          `json:"recent,omitempty"`    // last closed episode lines
	Paths    []string          `json:"paths,omitempty"`     // per Iran IP: path-sample inputs received (M7/O7)
	Peer     *PeerStatus       `json:"peer,omitempty"`      // b429: abort-report path counters
}

var statusJSON atomic.Pointer[[]byte]

func setStatus(e *evaluator) {
	st := Status{V: 1, Level: 1, Side: sideName(e.side), Dropped: dropped.Load(), Evals: e.evals,
		Scopes: len(e.scopes), Episodes: map[string]uint32{}, PathEv: [2]uint64{e.pathEv, e.pathNoScope}, PeerMass: e.nMass, PeerOld: e.nOld, GenOld: e.nGenOld, GenNew: e.nGenNew}
	for _, s := range e.scopes {
		if ep := s.ep; ep != nil {
			st.Open = append(st.Open, fmt.Sprintf("%s %s/%s %s", ep.id, ClassName(ep.class), confName[ep.conf], ep.last.Sub(ep.opened).Round(1e9)))
		}
	}
	sort.Strings(st.Open)
	for c := range e.nEpisodes {
		for f, n := range e.nEpisodes[c] {
			if n > 0 {
				name := "unknown"
				if c < NClasses {
					name = className[c]
				} else if c == NClasses+1 {
					name = "anomaly"
				}
				st.Episodes[name+"/"+confName[f]] = n
			}
		}
	}
	st.Recent = append([]string(nil), e.closedLines...)
	for _, s := range e.scopes {
		if q := &s.pin; q.n > 0 {
			l := q.last
			st.Paths = append(st.Paths, fmt.Sprintf("%s n=%d peer_known=%d peer_max=%.3f busy_n=%d busy_max=%d lim_known=%d lim_max=%.2f last: loss=%.3f peer=%.3f busy=%d lim=%.2f q=%.0fms rate=%.0f",
				s.ip, q.n, q.peerN, q.peerMax, q.busyN, q.busyMax, q.limN, q.limMax, l.Loss, l.PeerLoss, l.BusyLinks, l.PathLimited, l.QueueMs, l.RateBps))
		}
	}
	sort.Strings(st.Paths)
	st.Peer = peerStatus()
	b, err := json.Marshal(st)
	if err != nil {
		return
	}
	statusJSON.Store(&b)
}

// StatusJSON returns the status block (nil when the detector never started).
// After a self-disable it reports disabled.
func StatusJSON() json.RawMessage {
	p := statusJSON.Load()
	if p == nil {
		return nil
	}
	if disabled.Load() {
		var st Status
		if json.Unmarshal(*p, &st) == nil {
			st.Disabled = true
			st.Dropped = dropped.Load()
			if b, err := json.Marshal(st); err == nil {
				return b
			}
		}
	}
	return append(json.RawMessage(nil), *p...)
}

// Render turns a status block (from the status file) into text for
// `hs2 status --censor`. Pure: it reads only its argument.
func Render(raw []byte) string {
	if len(raw) == 0 {
		return "censor351: detector off (HS2_CENSOR_DETECT=0 on this server)\n"
	}
	var st Status
	if json.Unmarshal(raw, &st) != nil {
		return "censor351: status block unreadable\n"
	}
	var b strings.Builder
	state := "on"
	if st.Disabled {
		state = "DISABLED after an evaluator panic (no effect on the tunnel)"
	}
	fmt.Fprintf(&b, "censor351 v%d (detect and log only): %s, level %d, side %s\n", st.V, state, st.Level, st.Side)
	fmt.Fprintf(&b, "  scopes (Iran IPs): %d   evaluations: %d   dropped events: %d\n", st.Scopes, st.Evals, st.Dropped)
	fmt.Fprintf(&b, "  path samples: %d received, %d without an Iran-IP scope\n", st.PathEv[0], st.PathEv[1])
	if p := st.Peer; p != nil {
		fmt.Fprintf(&b, "  peer abort reports (kharej): %d received (%d aborts, %d edge signals), %d resets credited, %d held, %d released; orphans %d (%d long holds)\n",
			p.Reports, p.RepAborts, p.EdgeSigs, p.Credited, p.Held, p.Released, p.Orphans, p.OrphanHolds)
		fmt.Fprintf(&b, "  own aborts (iran): %d counted, %d reported\n", p.OwnAborts, p.OwnReported)
	}
	if len(st.Open) == 0 {
		b.WriteString("  open episodes: none\n")
	} else {
		b.WriteString("  open episodes:\n")
		for _, o := range st.Open {
			fmt.Fprintf(&b, "    %s\n", o)
		}
	}
	if len(st.Episodes) > 0 {
		keys := make([]string, 0, len(st.Episodes))
		for k := range st.Episodes {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteString("  closed episodes (class/confidence: count):\n")
		for _, k := range keys {
			fmt.Fprintf(&b, "    %-28s %d\n", k, st.Episodes[k])
		}
	}
	if len(st.Paths) > 0 {
		b.WriteString("  path inputs (per Iran IP):\n")
		for _, p := range st.Paths {
			fmt.Fprintf(&b, "    %s\n", p)
		}
	}
	if len(st.Recent) > 0 {
		b.WriteString("  recent:\n")
		for _, r := range st.Recent {
			fmt.Fprintf(&b, "    %s\n", r)
		}
	}
	return b.String()
}
