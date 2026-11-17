package engine

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

// queueCtlOn: HS2_QUEUE_CTL=1 runs the path-queue controller (queuectl.go)
// on the exit — the side that sends the downloads. It touches the server only
// while it caps: then a shaper (qShaper) holds the tunnel's egress to the
// edge at the cap; with no cap there is no shaper at all. qctl63: default on
// (envdef, exit side) for reverse l3mtcp; read in loadEnvKeys (defaults22.go).
var queueCtlOn bool

// queueCtlUpOn: HS2_QUEUE_CTL_UP=1 runs the same controller on the edge — the
// side that sends the uploads (a user sending a video, the upstream half of a
// call). Each interface the links leave by gets its own controller and shaper:
// the edge's lb addresses can leave by different interfaces, each a path with
// a bottleneck of its own. A shaper is there only while its cap is. qctl63:
// default on (envdef, edge side) for reverse l3mtcp; read in loadEnvKeys.
var queueCtlUpOn bool

// queueDebug: HS2_QUEUE_DEBUG=1 logs every tick's measurement and cap (lab).
var queueDebug = os.Getenv("HS2_QUEUE_DEBUG") == "1"

// queueCtlFast: HS2_QUEUE_CTL_FAST=1 (lat58; a default on both sides since
// rel69a, read in loadEnvKeys) caps a big queue sooner (qFastTicks instead of
// qStartTicks, queuectl.go).
var queueCtlFast bool

// queueCtlKeep: HS2_QUEUE_CTL_KEEP=1 (hl204; a default on both sides since
// rel69a, read in loadEnvKeys) keeps a cap whose
// demand went idle, at qBackFrac of the path's rate, while that rate is fresh
// (qPathMemory), instead of removing it (queuectl.go, the idle case). Without
// it every new heavy burst fills the path's queue for the controller's onset
// (qStartTicks ~10 s, FAST ~4 s) before it is capped again: in the lab a 10 s
// transfer ran its whole life with every light user 70-170 ms behind it.
var queueCtlKeep bool

// exitQueue is the exit's live links, measured each tick for the controller.
var exitQueue = &qRunner{on: queueCtlOn, links: map[*tlscarrier.Carrier]qPrev{}, st: &qCtlStatExit}

// edgeQueue is the edge's live links (HS2_QUEUE_CTL_UP), one controller per
// interface they leave by.
var edgeQueue = &qRunner{on: queueCtlUpOn, byDev: true, links: map[*tlscarrier.Carrier]qPrev{}, st: &qCtlStatEdge}

// qCtlStat (prod73): what a side's queue controller did, for hs2 status.
// Production ran HS2_QUEUE_CTL=1 on kharej with nothing to show whether it
// ever capped. starts / releases: caps set from none / taken off; changes:
// the cap moved while on; capped: lanes capped now; rateBps: their caps
// together now (bytes/s); cappedMs: lane-time spent capped.
type qCtlStat struct {
	starts, releases, changes atomic.Uint64
	capped                    atomic.Int32
	rateBps, cappedMs         atomic.Int64
	running                   atomic.Bool
	// probeUntil (hl228): monoNano until which a lane's probe step is in
	// force (qCtl.probing): the edge's stripe gate leaves a queue under
	// qHigh to the controller then.
	probeUntil atomic.Int64
	// onsetUntil (hl229): monoNano until which a lane's burst onset under a
	// cap at seen is in force (qCtl.onsetStart/onsetQueue): the edge's stripe
	// gate leaves a queue up to qOnsetGate to the controller then.
	onsetUntil atomic.Int64
	// onsetQ (hl231): the onset tick's queue (ns) the window from onsetUntil
	// was set for (0: a burst's start, qOnsetGate).
	onsetQ atomic.Int64
}

// probeNote (hl228): a lane's controller stepped a probe at at.
func (s *qCtlStat) probeNote(at time.Time) {
	if s == nil {
		return
	}
	u := monoNano(at.Add(qProbeQuiet))
	for {
		v := s.probeUntil.Load()
		if v >= u || s.probeUntil.CompareAndSwap(v, u) {
			return
		}
	}
}

// onsetNote (hl229): a lane's onset window runs until until (the latest
// set wins: the drop after the onset queue shortens the burst's window).
func (s *qCtlStat) onsetNote(until time.Time, q time.Duration) {
	if s != nil {
		s.onsetQ.Store(int64(q))
		s.onsetUntil.Store(monoNano(until))
	}
}

// onsetGate (hl231): the largest queue the edge's stripe gate leaves to the
// controller at now in an onset window (0: none): qOnsetGate, or the onset
// tick's own queue after the controller's no-hold drop.
func (s *qCtlStat) onsetGate(now time.Time) time.Duration {
	if !s.onset(now) {
		return 0
	}
	return max(qOnsetGate, time.Duration(s.onsetQ.Load()))
}

// onset (hl229): a lane's onset window is in force at now.
func (s *qCtlStat) onset(now time.Time) bool {
	return monoNano(now) < s.onsetUntil.Load()
}

// probing (hl228): a lane's probe step is in force at now.
func (s *qCtlStat) probing(now time.Time) bool {
	return monoNano(now) < s.probeUntil.Load()
}

var qCtlStatExit, qCtlStatEdge qCtlStat

// QueueCtlSnap: one side's queue controller (hs2 status).
type QueueCtlSnap struct {
	On                        bool
	Starts, Releases, Changes uint64
	Capped                    int32
	CapMbit                   float64
	CappedMs                  int64
}

// QueueCtlObserved: the exit's (HS2_QUEUE_CTL) and the edge's
// (HS2_QUEUE_CTL_UP) controllers.
func QueueCtlObserved() (exit, edge QueueCtlSnap) {
	f := func(s *qCtlStat) QueueCtlSnap {
		return QueueCtlSnap{On: s.running.Load(), Starts: s.starts.Load(), Releases: s.releases.Load(), Changes: s.changes.Load(), Capped: s.capped.Load(), CapMbit: mbitps(float64(s.rateBps.Load())), CappedMs: s.cappedMs.Load()}
	}
	return f(&qCtlStatExit), f(&qCtlStatEdge)
}

// note (one lane's tick): its cap went from was to now (bytes/s, 0 none)
// over dt.
func (s *qCtlStat) note(was, now float64, dt time.Duration) {
	if s == nil {
		return
	}
	if was > 0 {
		s.cappedMs.Add(dt.Milliseconds())
	}
	switch {
	case was <= 0 && now > 0:
		s.starts.Add(1)
		s.capped.Add(1)
	case was > 0 && now <= 0:
		s.releases.Add(1)
		s.capped.Add(-1)
	case was > 0 && now != was:
		s.changes.Add(1)
	}
	s.rateBps.Add(int64(now) - int64(was))
}

// qPrev is a link's TCP_INFO counters at the last tick.
type qPrev struct {
	acked, rcvd      uint64
	segsOut, retrans uint64
	dev              string // byDev: the interface the link leaves by ("": not known)
	devSet           bool
	accepted         bool   // m4f251: this side accepted the link (listener): filtered by our listen port
	sport            uint16 // m4f251, accepted: the source port its packets carry (qLinkSport; 0: not read yet)
}

type qRunner struct {
	mu    sync.Mutex
	links map[*tlscarrier.Carrier]qPrev // carrier → its counters at the last tick
	on    bool
	byDev bool // a controller per interface (the edge); else one for all (the exit)
	st    *qCtlStat
}

// track counts car while its link is up; the returned func forgets it.
// accepted: this side accepted the link from its listener (the exit in
// direct mode, the edge in reverse mode), not dialed it (m4f251).
func (r *qRunner) track(car *tlscarrier.Carrier, accepted bool) func() {
	if !r.on || car == nil {
		return func() {}
	}
	r.mu.Lock()
	r.links[car] = qPrev{accepted: accepted}
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		delete(r.links, car)
		r.mu.Unlock()
	}
}

// qLinkDevFn is qLinkDev (a test can stand in a slow one).
var qLinkDevFn = qLinkDev

// qBusyBytes: a link that moved at least this in a tick counts toward the
// delay (an idle link's smoothed RTT is stale).
const qBusyBytes = 64 << 10

// qGroup is one tick's measurement of the links behind one controller: the
// queueing delay of the busy ones (smoothed RTT over the lowest, their lower
// quartile — see qDelayOf; -1 when none is busy), what they all delivered
// (bytes/s), the segments they sent and retransmitted, and the peers (address
// and port: only the tunnel's own links are shaped) they go to.
type qGroup struct {
	in                   qIn
	peers                qKeySet // m4f251: (peer IP, listen port) for accepted links, (peer IP, port) for dialed
	delays               []time.Duration
	acked, segs, retrans uint64
	rcvd                 uint64
}

// measure reads the links' TCP_INFO, one group per controller: the exit's
// one (key ""), or the edge's one per interface its links leave by.
//
// audit246 L6: r.mu is held only to copy the link set and to store the new
// counters back — never across the reads (TCP_INFO per link, and on the
// edge one `ip route get` exec per new link): a 600-link redial storm used
// to block every new link's track (lm.OnLink, before its control channels)
// on the whole serial scan. Only the tick goroutine calls measure.
func (r *qRunner) measure(dt time.Duration) map[string]*qGroup {
	type qLinkPrev struct {
		car  *tlscarrier.Carrier
		prev qPrev
	}
	r.mu.Lock()
	snap := make([]qLinkPrev, 0, len(r.links))
	for car, prev := range r.links {
		snap = append(snap, qLinkPrev{car, prev})
	}
	r.mu.Unlock()
	next := make([]qLinkPrev, 0, len(snap)) // the links read this tick, with their new counters
	defer func() {
		r.mu.Lock()
		for _, n := range next {
			if _, ok := r.links[n.car]; ok { // not forgotten meanwhile
				r.links[n.car] = n.prev
			}
		}
		r.mu.Unlock()
	}()
	groups := map[string]*qGroup{}
	for _, lp := range snap {
		car, prev := lp.car, lp.prev
		tc := tcpConnOf(car)
		ts, ok := tcpStats(tc)
		if !ok {
			continue
		}
		if r.byDev && !prev.devSet {
			prev.dev, prev.devSet = qLinkDevFn(tc), true // once per link: it never moves
		}
		g := groups[prev.dev]
		if g == nil {
			g = &qGroup{}
			groups[prev.dev] = g
		}
		if ap, err := netip.ParseAddrPort(tc.RemoteAddr().String()); err == nil {
			if prev.accepted && prev.sport == 0 {
				prev.sport = qLinkSport(tc) // once per link: it never changes
			}
			g.peers.add(netip.AddrPortFrom(netip.IPv4Unspecified(), prev.sport), ap, prev.accepted)
		}
		cur := qPrev{acked: ts.bytesAcked, rcvd: ts.bytesRcvd, segsOut: uint64(ts.segsOut), retrans: ts.retrans, dev: prev.dev, devSet: prev.devSet, accepted: prev.accepted, sport: prev.sport}
		next = append(next, qLinkPrev{car, cur})
		if prev.acked == 0 || cur.acked < prev.acked || cur.rcvd < prev.rcvd || cur.segsOut < prev.segsOut || cur.retrans < prev.retrans {
			continue // first look at this link (or a counter wrapped): next tick
		}
		d := cur.acked - prev.acked
		g.acked += d
		g.rcvd += cur.rcvd - prev.rcvd
		g.segs += cur.segsOut - prev.segsOut
		g.retrans += cur.retrans - prev.retrans
		if d >= qBusyBytes && ts.minRttUs > 0 && ts.rttUs >= ts.minRttUs {
			g.delays = append(g.delays, time.Duration(ts.rttUs-ts.minRttUs)*time.Microsecond)
		}
	}
	for _, g := range groups {
		g.in = qIn{delay: qDelayOf(g.delays), g: float64(g.acked) / dt.Seconds(), rx: float64(g.rcvd) / dt.Seconds()}
		g.in.loss = qLossOf(g.retrans, g.segs, 0)
	}
	return groups
}

// qDelayOf is the queueing delay of a tick's busy links: their lower quartile,
// -1 when none is busy. A queue at the path's bottleneck holds every link
// that crosses it; a few links whose own round trip wanders (a busy peer, a
// path's jitter) must not look like one — the median of a handful of links
// is one of them.
func qDelayOf(d []time.Duration) time.Duration {
	if len(d) == 0 {
		return -1
	}
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	return d[len(d)/4]
}

// qLossOf is the share of the segments sent that the path lost: the
// retransmits less the drops of the tunnel's own shaper (each a retransmit
// the cap made, not the path).
func qLossOf(retrans, segs, shaperDrops uint64) float64 {
	if segs == 0 {
		return 0
	}
	if shaperDrops >= retrans {
		return 0
	}
	return float64(retrans-shaperDrops) / float64(segs)
}

// qLane is one controller and the shaper it drives.
type qLane struct {
	ctl  qCtl
	sh   *qShaper
	logf func(string, ...any)
	off  bool // the shaper cannot work here: the lane stands aside, quietly
	up   bool // hl228: the edge's lane (the upload): qCtl.up
	st   *qCtlStat
	cap  float64   // the cap in force after the last tick (0: none)
	at   time.Time // the last tick
}

func (r *qRunner) run(ctx context.Context, logf func(string, ...any)) {
	lanes := map[string]*qLane{}
	lane := func(dev string) {
		if lanes[dev] != nil {
			return
		}
		lf := logf
		if r.byDev {
			lf = qUpLog(dev, logf)
		}
		lanes[dev] = &qLane{sh: newQShaperOn(dev, lf), logf: lf, st: r.st, up: r.byDev}
	}
	if !r.byDev {
		lane("") // the exit: one controller, on the route's interface
	}
	if r.st != nil {
		r.st.running.Store(true)
	}
	defer func() {
		for _, l := range lanes {
			l.setCap(0, time.Now())
			l.sh.remove()
		}
		if r.st != nil {
			r.st.running.Store(false)
		}
	}()
	t := time.NewTicker(healthTick)
	defer t.Stop()
	last := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			groups := r.measure(now.Sub(last))
			last = now
			for dev := range groups {
				lane(dev)
			}
			for dev, l := range lanes {
				in, peers := qIn{delay: -1}, []qKey(nil) // no live link there (now)
				if g := groups[dev]; g != nil {
					in, peers = g.in, g.peers.keys
					d, bl := l.sh.stats()
					in.loss = qLossOf(g.retrans, g.segs, d)
					in.own = qOwnDelay(bl, l.cap) // hl234
				}
				in.now = now
				l.step(in, peers)
			}
		}
	}
}

// step runs one tick of a lane: the controller, then its shaper.
func (l *qLane) step(in qIn, peers []qKey) {
	if l.off {
		return
	}
	l.ctl.up = l.up
	rate, note := l.ctl.step(in)
	if rate > 0 && l.ctl.probeAt.Equal(in.now) {
		l.st.probeNote(in.now)
	}
	if rate > 0 && l.ctl.onsetAt.Equal(in.now) {
		l.st.onsetNote(l.ctl.onsetUntil, l.ctl.onsetQ) // hl229, hl231
	}
	if queueDebug {
		l.logf("queue-dbg delay=%s own=%s got=%.1f rx=%.1f loss=%.2f%% cap=%.1f Mbit/s", in.delay.Round(time.Millisecond), in.own.Round(time.Millisecond), mbitps(in.g), mbitps(in.rx), 100*in.loss, mbitps(rate))
	}
	if note != "" {
		l.logf("%s", note)
	}
	if rate > 0 && len(peers) > 0 {
		if !l.sh.apply(rate, peers) {
			// Cannot shape here (no tc, an operator's qdisc): the shaper said
			// so once; the lane stands aside for good — no cap notes that
			// change nothing.
			l.ctl, l.off = qCtl{}, true
			rate = 0
		}
	} else {
		rate = 0
		if queueCtlKeep && (l.sh.placed() || (len(peers) > 0 && in.delay < qHigh && in.g < qParkLight)) {
			// hl233: park, never swap the root qdisc under load (qShaper.park);
			// delay -1 (no busy link: an idle upload lane) is light too
			if !l.sh.park(peers) {
				l.ctl, l.off = qCtl{}, true
			}
		} else {
			l.sh.remove()
		}
	}
	l.setCap(rate, in.now)
}

// qOwnDelay (hl234): how long the shaper's backlog (bytes on the wire) waits
// at the cap in force (bytes/s of payload): the queue the links' round trip
// holds in the tunnel's own shaper, not on the path. 0 with no cap.
func qOwnDelay(backlog uint64, cap float64) time.Duration {
	if backlog == 0 || cap <= 0 {
		return 0
	}
	return time.Duration(float64(backlog) / (cap * qWire) * float64(time.Second))
}

// qParkRate (hl233): the parked shaper's class rate, bytes/s of payload
// (40 Gbit/s on the wire: no cap); qParkLight: the shaper goes in parked only
// while the links deliver under it (50 Mbit/s), so the root swap drops little.
const (
	qParkRate  = 40e9 / 8 / qWire
	qParkLight = 50e6 / 8
)

// setCap records the lane's cap in force (prod73 counters).
func (l *qLane) setCap(rate float64, now time.Time) {
	dt := time.Duration(0)
	if !l.at.IsZero() {
		dt = now.Sub(l.at)
	}
	l.at = now
	l.st.note(l.cap, rate, dt)
	l.cap = rate
}

// qUpLog marks the edge's (upload) lines with their interface and key.
func qUpLog(dev string, logf func(string, ...any)) func(string, ...any) {
	tag := "queue-up"
	if dev != "" {
		tag += " " + dev
	}
	return func(f string, a ...any) {
		f = fmt.Sprintf(f, a...) // the controller's notes come as ("%s", note)
		f = strings.Replace(f, "(HS2_QUEUE_CTL)", "(HS2_QUEUE_CTL_UP)", 1)
		f = strings.Replace(f, "egress to the edge", "egress to the exit", 1)
		switch {
		case strings.HasPrefix(f, "queue-dbg "):
			f = tag + "-dbg " + f[len("queue-dbg "):]
		case strings.HasPrefix(f, "queue: "):
			f = tag + ": " + f[len("queue: "):]
		}
		logf("%s", f)
	}
}

// trackEdgeLink counts an edge link for HS2_QUEUE_CTL_UP until it closes.
// accepted: the reverse edge, which accepts its links from the exit (m4f251).
func trackEdgeLink(ctx context.Context, l Link, accepted bool) {
	ml, ok := l.(*mtcpLink)
	if !edgeQueue.on || !ok || ml.tls == nil || ml.sess == nil {
		return
	}
	untrack := edgeQueue.track(ml.tls, accepted)
	go func() {
		select {
		case <-ctx.Done():
		case <-ml.closed():
		}
		untrack()
	}()
}
