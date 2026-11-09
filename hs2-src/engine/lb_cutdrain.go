package engine

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// Cut-path drain on the reverse edge (HS2_LB_CUT_DRAIN=1, with HS2_LB_AUTO2).
//
// When the dialing side's balancer cuts a path (an Iran IP lossy on every
// Kharej IP, say), the path's target drops to lb2Keep, but the edge only ever
// retired IDLE links: a busy link on the cut path never empties, so the path
// stayed at 12 links for a target of 2, its 300 users on 7% loss, and new
// connections still landed there.
//
// With the key on, every reconcile tick the serving links of a cut path beyond
// its target (at least lb2Keep, the probe links that let it be seen
// recovering) are retired at once, busy or not:
//   - they take no new connections (retiring);
//   - their connections idle for drainIdle are closed as for any retiring
//     link, and those only trickling (not flowing) after cutDrainForce rather
//     than retireForce — they reconnect onto a serving link on a good path; a
//     connection still moving data is never closed (a heavy user stays);
//   - the exit is asked for a replacement for each (ctlTarget), so the serving
//     count stays at its target on the good paths (make before break);
//   - while the path stays cut they never come back into service; once it is
//     no longer cut they are ordinary retiring links again.
//
// Only as many drain as the exit can replace (its ceiling less the links up),
// trickling connections are closed a few links a tick (cutDrainForcePerTick),
// and a cut path keeps lb2Keep serving links: cut-drained ones come back when
// the exit's rebalance retires its idle probes.
//
// Nothing happens when every reported path is cut (no good path to move to).
var lbCutDrain = os.Getenv("HS2_LB_CUT_DRAIN") == "1"

// cutDrainForce: trickling connections on a cut-drained link are closed after
// this long (retireForce for other retiring links).
var cutDrainForce = 60 * time.Second

// cutDrainForcePerTick: cut-drained links whose trickling connections are
// force-closed per drain tick (each closes up to idleReclaimMax, spaced by
// closeJitter), so a cut path's hundreds of users reconnect over tens of
// seconds instead of one burst of handshakes onto the good paths.
var cutDrainForcePerTick = 3

// cutDrainNoRoom is the drain room on a dialing pool (no exit ceiling).
var cutDrainNoRoom = 1 << 30

// cutPathsFromReport is the cut paths of the dialing side's report, keyed as
// the edge keys its links ("edge IP>dialer IP", "edge IP>*" for a path from the
// default route), each with the links it keeps. Nil when none is cut or every
// path is.
func cutPathsFromReport(rep LBReport) map[string]int {
	var out map[string]int
	good := false
	for _, p := range rep.Paths {
		if !p.Cut {
			good = true
			continue
		}
		if out == nil {
			out = map[string]int{}
		}
		l := p.Local
		if l == "" {
			l = "*"
		}
		out[p.Remote+">"+l] = max(p.Target, lb2Keep)
	}
	if !good {
		return nil
	}
	return out
}

// cutKeep returns the links a cut path keeps, and whether path is cut.
func cutKeep(cut map[string]int, path string) (int, bool) {
	if len(cut) == 0 || path == "" {
		return 0, false
	}
	if n, ok := cut[path]; ok {
		return n, true
	}
	if i := strings.IndexByte(path, '>'); i >= 0 {
		if n, ok := cut[path[:i]+">*"]; ok {
			return n, true
		}
	}
	return 0, false
}

// cutDrainLocked retires the serving links of cut paths beyond what each
// keeps (the links with the fewest connections are kept: probes, few users)
// and clears the mark of links whose path is no longer cut. serving is the
// reconcile's list; it returns what is left serving and the links retired.
// Caller holds m.mu.
func (m *LinkManager) cutDrainLocked(serving []*managedLink, now time.Time) ([]*managedLink, []*managedLink) {
	if !lbCutDrain || m.cutPaths == nil {
		return serving, nil
	}
	cut := m.cutPaths()
	for _, ml := range m.links {
		if ml.cutDrain {
			if _, ok := cutKeep(cut, mlPath(ml)); !ok || !ml.retiring {
				ml.cutDrain = false
			}
		}
	}
	if len(cut) == 0 {
		return serving, nil
	}
	byPath := map[string][]*managedLink{}
	for _, ml := range serving {
		p := mlPath(ml)
		if _, ok := cutKeep(cut, p); ok {
			byPath[p] = append(byPath[p], ml)
		}
	}
	// A cut path keeps lb2Keep serving links (probes: its recovery is seen
	// through them). The exit's rebalance retires idle links on it, which
	// are its probes: put cut-drained links (fewest connections first) back.
	var back []*managedLink
	for _, ml := range m.links {
		if !ml.cutDrain || !ml.retiring || ml.draining || ml.degraded || !ml.link.Alive() {
			continue
		}
		p := mlPath(ml)
		if len(byPath[p]) >= lb2Keep {
			continue
		}
		back = append(back, ml)
	}
	sort.Slice(back, func(i, j int) bool { return back[i].open < back[j].open })
	for _, ml := range back {
		p := mlPath(ml)
		if len(byPath[p]) >= lb2Keep {
			continue
		}
		ml.retiring, ml.retireSince, ml.cutDrain = false, time.Time{}, false
		byPath[p] = append(byPath[p], ml)
		serving = append(serving, ml)
	}
	// Only as many links are drained as the exit can replace (its ceiling
	// less the links up): past that, a drained link is a serving slot lost
	// and the good paths' links carry more users each (the lab at its cap:
	// serving 150→117, ICMP p99 up). Replacements free no room, so links
	// closed (emptied) make the room for the next ones.
	room := cutDrainNoRoom
	if m.accept {
		room = max(0, m.exitCeilingLocked()-m.aliveLocked())
	}
	gone := map[*managedLink]bool{}
	var out []*managedLink
	for p, ls := range byPath {
		keep, _ := cutKeep(cut, p)
		if len(ls) <= keep || room <= 0 {
			continue
		}
		if n := len(ls) - keep; n > room {
			keep = len(ls) - room
		}
		sort.Slice(ls, func(i, j int) bool {
			a, b := ls[i], ls[j]
			if a.open != b.open {
				return a.open < b.open
			}
			return a.id < b.id
		})
		room -= len(ls) - keep
		for _, ml := range ls[keep:] {
			ml.retiring, ml.retireSince, ml.pressed, ml.cutDrain = true, now, false, true
			gone[ml] = true
			out = append(out, ml)
		}
	}
	if len(out) == 0 {
		return serving, nil
	}
	kept := serving[:0]
	for _, ml := range serving {
		if !gone[ml] {
			kept = append(kept, ml)
		}
	}
	return kept, out
}

// cutDrainingLocked counts the live cut-drained links (their slots are
// replaced). Caller holds m.mu.
func (m *LinkManager) cutDrainingLocked() int {
	n := 0
	for _, ml := range m.links {
		if ml.cutDrain && ml.retiring && ml.link.Alive() {
			n++
		}
	}
	return n
}

// cutDrainNote is the log line for links retired off cut paths.
func cutDrainNote(out []*managedLink) string {
	ids, users := make([]string, 0, 8), 0
	for i, ml := range out {
		users += ml.open
		if i < 8 {
			ids = append(ids, fmt.Sprint(ml.id))
		}
	}
	more := ""
	if len(out) > len(ids) {
		more = fmt.Sprintf(" +%d", len(out)-len(ids))
	}
	return fmt.Sprintf("mtcp: lb cut-drain: %d link(s) on cut paths retired (%s%s) with %d connection(s) — no new ones; idle and trickling ones reconnect onto good paths, flowing ones stay; the exit is asked for replacements",
		len(out), strings.Join(ids, ","), more, users)
}
