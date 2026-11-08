package engine

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

// queueCtlOn: HS2_QUEUE_CTL=1 runs the path-queue controller (queuectl.go)
// on the exit — the side that sends the downloads. It touches the server only
// while it caps: then a shaper (qShaper) holds the tunnel's egress to the
// edge at the cap; with no cap there is no shaper at all.
var queueCtlOn = os.Getenv("HS2_QUEUE_CTL") == "1"

// queueCtlUpOn: HS2_QUEUE_CTL_UP=1 runs the same controller on the edge — the
// side that sends the uploads (a user sending a video, the upstream half of a
// call). Each interface the links leave by gets its own controller and shaper:
// the edge's lb addresses can leave by different interfaces, each a path with
// a bottleneck of its own. A shaper is there only while its cap is.
var queueCtlUpOn = os.Getenv("HS2_QUEUE_CTL_UP") == "1"

// queueDebug: HS2_QUEUE_DEBUG=1 logs every tick's measurement and cap (lab).
var queueDebug = os.Getenv("HS2_QUEUE_DEBUG") == "1"

// exitQueue is the exit's live links, measured each tick for the controller.
var exitQueue = &qRunner{on: queueCtlOn, links: map[*tlscarrier.Carrier]qPrev{}}

// edgeQueue is the edge's live links (HS2_QUEUE_CTL_UP), one controller per
// interface they leave by.
var edgeQueue = &qRunner{on: queueCtlUpOn, byDev: true, links: map[*tlscarrier.Carrier]qPrev{}}

// qPrev is a link's TCP_INFO counters at the last tick.
type qPrev struct {
	acked            uint64
	segsOut, retrans uint64
	dev              string // byDev: the interface the link leaves by ("": not known)
	devSet           bool
}

type qRunner struct {
	mu    sync.Mutex
	links map[*tlscarrier.Carrier]qPrev // carrier → its counters at the last tick
	on    bool
	byDev bool // a controller per interface (the edge); else one for all (the exit)
}

// track counts car while its link is up; the returned func forgets it.
func (r *qRunner) track(car *tlscarrier.Carrier) func() {
	if !r.on || car == nil {
		return func() {}
	}
	r.mu.Lock()
	r.links[car] = qPrev{}
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		delete(r.links, car)
		r.mu.Unlock()
	}
}

// qBusyBytes: a link that moved at least this in a tick counts toward the
// delay (an idle link's smoothed RTT is stale).
const qBusyBytes = 64 << 10

// qGroup is one tick's measurement of the links behind one controller: the
// median queueing delay of the busy ones (smoothed RTT over the lowest; -1
// when none is busy), what they all delivered (bytes/s), the share of the
// segments they sent that were retransmits, and the peers they go to.
type qGroup struct {
	in                   qIn
	peers                []netip.Addr
	seen                 map[netip.Addr]bool
	delays               []time.Duration
	acked, segs, retrans uint64
}

// measure reads the links' TCP_INFO, one group per controller: the exit's
// one (key ""), or the edge's one per interface its links leave by.
func (r *qRunner) measure(dt time.Duration) map[string]*qGroup {
	r.mu.Lock()
	defer r.mu.Unlock()
	groups := map[string]*qGroup{}
	for car, prev := range r.links {
		tc := tcpConnOf(car)
		ts, ok := tcpStats(tc)
		if !ok {
			continue
		}
		if r.byDev && !prev.devSet {
			prev.dev, prev.devSet = qLinkDev(tc), true // once per link: it never moves
		}
		g := groups[prev.dev]
		if g == nil {
			g = &qGroup{seen: map[netip.Addr]bool{}}
			groups[prev.dev] = g
		}
		if ap, err := netip.ParseAddrPort(tc.RemoteAddr().String()); err == nil {
			if a := ap.Addr().Unmap(); !g.seen[a] {
				g.seen[a] = true
				g.peers = append(g.peers, a)
			}
		}
		cur := qPrev{acked: ts.bytesAcked, segsOut: uint64(ts.segsOut), retrans: ts.retrans, dev: prev.dev, devSet: prev.devSet}
		r.links[car] = cur
		if prev.acked == 0 || cur.acked < prev.acked || cur.segsOut < prev.segsOut || cur.retrans < prev.retrans {
			continue // first look at this link (or a counter wrapped): next tick
		}
		d := cur.acked - prev.acked
		g.acked += d
		g.segs += cur.segsOut - prev.segsOut
		g.retrans += cur.retrans - prev.retrans
		if d >= qBusyBytes && ts.minRttUs > 0 && ts.rttUs >= ts.minRttUs {
			g.delays = append(g.delays, time.Duration(ts.rttUs-ts.minRttUs)*time.Microsecond)
		}
	}
	for _, g := range groups {
		g.in = qIn{delay: -1, g: float64(g.acked) / dt.Seconds()}
		if len(g.delays) > 0 {
			sort.Slice(g.delays, func(i, j int) bool { return g.delays[i] < g.delays[j] })
			g.in.delay = g.delays[len(g.delays)/2]
		}
		if g.segs > 0 {
			g.in.loss = float64(g.retrans) / float64(g.segs)
		}
	}
	return groups
}

// qLane is one controller and the shaper it drives.
type qLane struct {
	ctl  qCtl
	sh   *qShaper
	logf func(string, ...any)
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
		lanes[dev] = &qLane{sh: newQShaperOn(dev, lf), logf: lf}
	}
	if !r.byDev {
		lane("") // the exit: one controller, on the route's interface
	}
	defer func() {
		for _, l := range lanes {
			l.sh.remove()
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
				in, peers := qIn{delay: -1}, []netip.Addr(nil) // no live link there (now)
				if g := groups[dev]; g != nil {
					in, peers = g.in, g.peers
				}
				in.now = now
				l.step(in, peers)
			}
		}
	}
}

// step runs one tick of a lane: the controller, then its shaper.
func (l *qLane) step(in qIn, peers []netip.Addr) {
	rate, note := l.ctl.step(in)
	if queueDebug {
		l.logf("queue-dbg delay=%s got=%.1f loss=%.2f%% cap=%.1f Mbit/s", in.delay.Round(time.Millisecond), mbitps(in.g), 100*in.loss, mbitps(rate))
	}
	if note != "" {
		l.logf("%s", note)
	}
	if rate > 0 && len(peers) > 0 {
		if !l.sh.apply(rate, peers) {
			l.ctl = qCtl{release: in.now.Add(qReleaseMax), releaseFor: qReleaseMax} // cannot shape here: stop trying for a while
		}
	} else {
		l.sh.remove()
	}
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
func trackEdgeLink(ctx context.Context, l Link) {
	ml, ok := l.(*mtcpLink)
	if !edgeQueue.on || !ok || ml.tls == nil || ml.sess == nil {
		return
	}
	untrack := edgeQueue.track(ml.tls)
	go func() {
		select {
		case <-ctx.Done():
		case <-ml.closed():
		}
		untrack()
	}()
}
