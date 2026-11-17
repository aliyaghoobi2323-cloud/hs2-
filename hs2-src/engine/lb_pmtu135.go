package engine

import (
	"os"
	"sort"
	"time"
)

// mt135: per-path MTU visibility and an optional LB hint.
//
// Every link is its own TCP connection, so the kernel keeps a path MTU per
// socket (PMTU discovery from ICMP "frag needed", or MTU probing with
// net.ipv4.tcp_mtu_probing=1 on a blackhole) and the send MSS derived from it.
// The figure used is the EFFECTIVE path MTU (effMtu): blackhole probing
// lowers the MSS but leaves the PMTU at the route's value.
// Each side reads both from TCP_INFO for what IT sends, and the control
// channel (control.go) carries each side's figure to the other, so a path's
// status row shows both directions: own = this side's sends, peer = the other
// side's.
//
// Per path and direction the view keeps the lowest and median path MTU, the
// median send MSS, the path's usual MTU (the highest median of the last
// pmtuUsualWin) and how many links sit below it. A "drop" is a reading where
// many of the path's links lowered their MTU since the last one
// (pmtuDropMin links and pmtuDropFrac of those read): the kernel shrinking
// packets after a large-packet blackhole or a smaller tunnel on the way. A
// lower MTU alone is healthy (the kernel did its job), so the LB only uses a
// drop as a hint — and only beside loss or a stall — behind HS2_LB_PMTU_HINT
// (default off).

const (
	ctrlMtuBit   = uint64(1) << 62
	ctrlMtuShift = 40
	ctrlSeqMask  = uint64(1)<<ctrlMtuShift - 1

	pmtuUsualWin   = 10 * time.Minute
	pmtuRecent     = 10 * time.Minute // doctor: "dropped recently"
	pmtuHintRecent = 2 * time.Minute  // the LB hint: a drop this fresh
	pmtuDropMin    = 2
	pmtuDropFrac   = 0.25
	pmtuDropGap    = time.Minute // one drop event per path-direction per minute (the peer's figures trickle in over a few pongs)
	// pmtuFloor: a lower figure is no path MTU (a socket not connected yet
	// or closing reads 68; the kernel's PMTU discovery floor is 552).
	pmtuFloor = 552
	// PmtuWarn: a path MTU under this (the IPv6 minimum) is worth a warning.
	PmtuWarn = 1280
)

// lbPmtuHint is HS2_LB_PMTU_HINT (default off).
var lbPmtuHint = os.Getenv("HS2_LB_PMTU_HINT") == "1"

// effMtu is a socket's effective path MTU: the kernel's PMTU, or the send
// MSS plus the IP and TCP headers (timestamps on: 52 bytes IPv4, 72 IPv6)
// when that is lower. TCP's blackhole probing (tcp_mtu_probing) shrinks the
// MSS and leaves the PMTU at the route's value, so the PMTU alone misses it.
func effMtu(pmtu, mss uint32, v6 bool) uint32 {
	if mss == 0 {
		return pmtu
	}
	h := uint32(52)
	if v6 {
		h = 72
	}
	return min(pmtu, mss+h)
}

// ctrlSplitRetrans splits the pong's exitRetrans field (see control.go).
func ctrlSplitRetrans(v uint64) (rt uint64, pmtu, mss uint32) {
	return v & 0xFFFFFFFF, uint32(v >> 48), uint32(v>>32) & 0xFFFF
}

// ctrlJoinRetrans builds the field for an edge that asked for the MTU.
func ctrlJoinRetrans(rt uint64, pmtu, mss uint32) uint64 {
	return rt&0xFFFFFFFF | uint64(min(pmtu, 0xFFFF))<<48 | uint64(min(mss, 0xFFFF))<<32
}

// LBPmtu is one direction of a path's MTU view.
type LBPmtu struct {
	Min      int `json:"pmtu_min"`
	Med      int `json:"pmtu_med"`
	MssMed   int `json:"mss_med,omitempty"`
	Usual    int `json:"pmtu_usual"`            // the highest median over the last 10 min
	Low      int `json:"pmtu_low,omitempty"`    // links below the usual MTU now
	Links    int `json:"links"`                 // links with a reading
	Drops    int `json:"pmtu_drops,omitempty"`  // drop events since start
	LastDrop int `json:"last_drop_s,omitempty"` // seconds since the last drop (absent: none)
	dropAt   time.Time
}

// RecentDrop: the last drop is under pmtuRecent old.
func (p *LBPmtu) RecentDrop() bool {
	return p != nil && p.Drops > 0 && p.LastDrop < int(pmtuRecent/time.Second)
}

// pmtuReading is one link's figures.
type pmtuReading struct {
	key       any
	pmtu, mss uint32
}

type pmtuMedAt struct {
	at  time.Time
	med uint32
}

// pmtuPath is one path-direction's history.
type pmtuPath struct {
	prev   map[any]uint32
	meds   []pmtuMedAt
	drops  int
	dropAt time.Time
}

// pmtuTrack keeps the history of every path-direction ("path|dir").
type pmtuTrack struct {
	p      map[string]*pmtuPath
	hinted map[string]time.Time // HS2_LB_PMTU_HINT: the drop last logged as a hint, per path
	now    func() time.Time
}

func newPmtuTrack() *pmtuTrack {
	return &pmtuTrack{p: map[string]*pmtuPath{}, hinted: map[string]time.Time{}, now: time.Now}
}

// observe folds one reading of a path-direction's links; nil when none of
// them has a figure. drop: this reading is a drop event.
func (t *pmtuTrack) observe(k string, rs []pmtuReading) (v *LBPmtu, drop bool) {
	now := t.now()
	p := t.p[k]
	if p == nil {
		p = &pmtuPath{prev: map[any]uint32{}}
		t.p[k] = p
	}
	var ms, ss []uint32
	lowered := 0
	seen := make(map[any]bool, len(rs))
	for _, r := range rs {
		if r.pmtu < pmtuFloor {
			continue
		}
		seen[r.key] = true
		if old, ok := p.prev[r.key]; ok && r.pmtu < old {
			lowered++
		}
		p.prev[r.key] = r.pmtu
		ms = append(ms, r.pmtu)
		if r.mss > 0 {
			ss = append(ss, r.mss)
		}
	}
	for key := range p.prev {
		if !seen[key] {
			delete(p.prev, key)
		}
	}
	if len(ms) == 0 {
		return nil, false
	}
	if lowered >= pmtuDropMin && float64(lowered) >= pmtuDropFrac*float64(len(ms)) &&
		(p.drops == 0 || now.Sub(p.dropAt) >= pmtuDropGap) {
		drop = true
		p.drops++
		p.dropAt = now
	}
	med := medU32(ms)
	p.meds = append(p.meds, pmtuMedAt{now, med})
	for len(p.meds) > 1 && now.Sub(p.meds[0].at) > pmtuUsualWin {
		p.meds = p.meds[1:]
	}
	if len(p.meds) > 256 {
		p.meds = p.meds[len(p.meds)-256:]
	}
	usual := uint32(0)
	for _, m := range p.meds {
		usual = max(usual, m.med)
	}
	v = &LBPmtu{Min: int(ms[0]), Med: int(med), Usual: int(usual), Links: len(ms), Drops: p.drops, dropAt: p.dropAt}
	if len(ss) > 0 {
		v.MssMed = int(medU32(ss))
	}
	for _, m := range ms {
		if m < usual {
			v.Low++
		}
	}
	if p.drops > 0 {
		v.LastDrop = int(now.Sub(p.dropAt) / time.Second)
	}
	return v, drop
}

// forget drops the history of path-directions not in keep.
func (t *pmtuTrack) forget(keep map[string]bool) {
	for k := range t.p {
		if !keep[k] {
			delete(t.p, k)
		}
	}
	for k := range t.hinted {
		if !keep[k] {
			delete(t.hinted, k)
		}
	}
}

// medU32 sorts xs and returns its (lower) median.
func medU32(xs []uint32) uint32 {
	sort.Slice(xs, func(i, j int) bool { return xs[i] < xs[j] })
	return xs[(len(xs)-1)/2]
}

// pmtuMinOf is the lower of two directions' minimum (0: neither).
func pmtuMinOf(a, b *LBPmtu) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return b.Min
	case b == nil:
		return a.Min
	}
	return min(a.Min, b.Min)
}

// pmtuHint (HS2_LB_PMTU_HINT, dialing side, fast auto): a path whose own
// sending links dropped their MTU lately AND that reads loss or a stall
// this tick is read as bad — the large-packet blackhole that cost the users
// time before the kernel shrank its segments. A drop alone changes nothing.
func (b *LB) pmtuHint(rs map[int]q2Reading) {
	if !lbPmtuHint || len(rs) == 0 {
		return
	}
	type held struct {
		key  any
		path int
		h    lbHooks
	}
	b.mu.Lock()
	hs := make([]held, 0, len(b.hooks))
	for k, h := range b.hooks {
		if i, ok := b.held[k]; ok && h.stats != nil {
			hs = append(hs, held{k, i, h})
		}
	}
	names := make([]string, len(b.paths))
	for j, p := range b.paths {
		names[j] = p.Name()
	}
	if b.pmtuT == nil {
		b.pmtuT = newPmtuTrack()
	}
	t := b.pmtuT
	b.mu.Unlock()
	per := map[int][]pmtuReading{}
	for _, x := range hs {
		if ts, ok := x.h.stats(); ok {
			per[x.path] = append(per[x.path], pmtuReading{x.key, ts.pmtu, ts.sndMss})
		}
	}
	keep := map[string]bool{}
	for j, name := range names {
		keep[name] = true
		v, drop := t.observe(name, per[j])
		if drop {
			b.log("lb pmtu: path %s: links lowered their path MTU (min %d, median %d, usual %d, %d of %d below)", name, v.Min, v.Med, v.Usual, v.Low, v.Links)
		}
		r, ok := rs[j]
		if !ok || v == nil || v.Drops == 0 || t.now().Sub(v.dropAt) > pmtuHintRecent {
			continue
		}
		if r.silent || r.noAck || r.loss >= lb2CutLoss/2 {
			if r.loss < lb2CutLoss*1.5 {
				r.loss = lb2CutLoss * 1.5
			}
			rs[j] = r
			if t.hinted[name].Equal(v.dropAt) {
				continue // told once per drop
			}
			t.hinted[name] = v.dropAt
			b.log("lb pmtu: path %s: MTU drop %ds ago with loss/stall — read as degraded (HS2_LB_PMTU_HINT)", name, int(t.now().Sub(v.dropAt)/time.Second))
		}
	}
	t.forget(keep)
}
