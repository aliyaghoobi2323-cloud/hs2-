package engine

import (
	"context"
	"net/netip"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

// queueCtlOn: HS2_QUEUE_CTL=1 runs the path-queue controller (queuectl.go)
// on the exit — the side that sends the downloads. It touches the server only
// while it caps: then a shaper (qShaper) holds the tunnel's egress to the
// edge at the cap; with no cap there is no shaper at all.
var queueCtlOn = os.Getenv("HS2_QUEUE_CTL") == "1"

// queueDebug: HS2_QUEUE_DEBUG=1 logs every tick's measurement and cap (lab).
var queueDebug = os.Getenv("HS2_QUEUE_DEBUG") == "1"

// exitQueue is the exit's live links, measured each tick for the controller.
var exitQueue = &qRunner{links: map[*tlscarrier.Carrier]qPrev{}}

// qPrev is a link's TCP_INFO counters at the last tick.
type qPrev struct {
	acked            uint64
	segsOut, retrans uint64
}

type qRunner struct {
	mu    sync.Mutex
	links map[*tlscarrier.Carrier]qPrev // carrier → its counters at the last tick
}

// track counts car while its link is up; the returned func forgets it.
func (r *qRunner) track(car *tlscarrier.Carrier) func() {
	if !queueCtlOn {
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

// measure reads the links' TCP_INFO: the median queueing delay of the busy
// ones (smoothed RTT over the lowest; -1 when none is busy), what they all
// delivered (bytes/s), the share of the segments they sent that were
// retransmits, and the peers they go to.
func (r *qRunner) measure(dt time.Duration) (qIn, []netip.Addr) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var delays []time.Duration
	var acked, segs, retrans uint64
	seen := map[netip.Addr]bool{}
	var peers []netip.Addr
	for car, prev := range r.links {
		tc := tcpConnOf(car)
		ts, ok := tcpStats(tc)
		if !ok {
			continue
		}
		if ap, err := netip.ParseAddrPort(tc.RemoteAddr().String()); err == nil {
			if a := ap.Addr().Unmap(); !seen[a] {
				seen[a] = true
				peers = append(peers, a)
			}
		}
		cur := qPrev{acked: ts.bytesAcked, segsOut: uint64(ts.segsOut), retrans: ts.retrans}
		r.links[car] = cur
		if prev.acked == 0 || cur.acked < prev.acked || cur.segsOut < prev.segsOut || cur.retrans < prev.retrans {
			continue // first look at this link (or a counter wrapped): next tick
		}
		d := cur.acked - prev.acked
		acked += d
		segs += cur.segsOut - prev.segsOut
		retrans += cur.retrans - prev.retrans
		if d >= qBusyBytes && ts.minRttUs > 0 && ts.rttUs >= ts.minRttUs {
			delays = append(delays, time.Duration(ts.rttUs-ts.minRttUs)*time.Microsecond)
		}
	}
	in := qIn{delay: -1, g: float64(acked) / dt.Seconds()}
	if len(delays) > 0 {
		sort.Slice(delays, func(i, j int) bool { return delays[i] < delays[j] })
		in.delay = delays[len(delays)/2]
	}
	if segs > 0 {
		in.loss = float64(retrans) / float64(segs)
	}
	return in, peers
}

func (r *qRunner) run(ctx context.Context, logf func(string, ...any)) {
	var ctl qCtl
	sh := newQShaper(logf)
	defer sh.remove()
	t := time.NewTicker(healthTick)
	defer t.Stop()
	last := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			in, peers := r.measure(now.Sub(last))
			in.now = now
			last = now
			rate, note := ctl.step(in)
			if queueDebug {
				logf("queue-dbg delay=%s got=%.1f loss=%.2f%% cap=%.1f Mbit/s", in.delay.Round(time.Millisecond), mbitps(in.g), 100*in.loss, mbitps(rate))
			}
			if note != "" {
				logf("%s", note)
			}
			if rate > 0 && len(peers) > 0 {
				if !sh.apply(rate, peers) {
					ctl = qCtl{release: now.Add(qReleaseMax), releaseFor: qReleaseMax} // cannot shape here: stop trying for a while
				}
			} else {
				sh.remove()
			}
		}
	}
}
