package engine

import (
	"net"
	"sort"
	"sync"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
	"github.com/xtaci/smux"
)

// R1's cores on each link, as far as the kill switches and the caps both
// ends have allow (newSessionCaps): the link scheduler (C2, linksched.go),
// C3's guard (hostq.go) and C4-lite (sendcap.go), on every link at both
// ends. One goroutine steps them each schedTick (the pattern of the window
// core's runner, wcore_run.go): each link's C4-lite reads its socket when
// due and moves its pacing cap and its C3 (sendCapLink.step), and its
// scheduler ticks at the link's rate, the link meter's written bytes over
// the last second (§3.2: C4's delivery rate from R3). On the exit the pacing
// net hands its cap to the link's C4-lite (wcLink.sc), the lesser of the two
// in force. CoreLines gives each link's status lines (§3.1 to §3.4), and a
// line a minute sums the schedulers up in the log (§7).

// r1RateSpan: the scheduler's rate is the link's written bytes over this.
const r1RateSpan = time.Second

type r1Wrote struct {
	at    int64
	wrote uint64
}

// r1Link is one link's R1 cores.
type r1Link struct {
	id   int
	sess *smux.Session
	ls   *linkSched   // nil: C2 off on this link
	hq   *hostQ       // nil: C3 off
	sc   *sendCapLink // nil: neither C4-lite nor C3 reads the socket
	mtr  *linkMeter
	// the runner's: the meter's written bytes of the last r1RateSpan
	wr []r1Wrote
	// the status lines': when they were last made, and the flips then
	lineAt  int64
	flipsAt [2]uint64
}

type r1Set struct {
	mu   sync.Mutex
	all  map[*r1Link]struct{}
	next int
	once sync.Once
	// the minute's line
	logAt               int64
	logLane, logBlocked uint64
	logDrops            [2]uint64
	list                []*r1Link
}

var r1Links r1Set

// add registers a link's cores (ls, hq: nil when off) and gives it C4-lite
// where it runs: on its socket tcp, when the caps both ends have and this
// side's switch allow it; then the exit's pacing net (wc) hands its cap to
// it. The socket is read for C3 alone where C4-lite does not run.
func (r *r1Set) add(sess *smux.Session, ls *linkSched, hq *hostQ, tcp *net.TCPConn, mtr *linkMeter, wc *wcLink, caps uint32) {
	capOn := !SendCapOff() && caps&tlscarrier.CapSendCap != 0
	var sc *sendCapLink
	if tcp != nil && (capOn || hq != nil) {
		if sys := newCapSys(tcp); sys != nil {
			sc = newSendCapLink(sys, hq)
			sc.core.off = !capOn
			if capOn && wc != nil {
				wc.sc.Store(sc)
			}
		}
	}
	if ls == nil && sc == nil && hq == nil {
		return
	}
	l := &r1Link{sess: sess, ls: ls, hq: hq, sc: sc, mtr: mtr, lineAt: ctrlNow()}
	r.mu.Lock()
	if r.all == nil {
		r.all = map[*r1Link]struct{}{}
	}
	r.next++
	l.id = r.next
	r.all[l] = struct{}{}
	r.mu.Unlock()
	r.once.Do(func() { go r.run() })
}

func (r *r1Set) run() {
	t := time.NewTicker(schedTick)
	defer t.Stop()
	for range t.C {
		r.step(ctrlNow())
	}
}

// step steps every live link and drops the ended ones.
func (r *r1Set) step(now int64) {
	r.mu.Lock()
	r.list = r.list[:0]
	for l := range r.all {
		if l.sess.IsClosed() {
			delete(r.all, l)
			continue
		}
		r.list = append(r.list, l)
	}
	list := r.list
	r.mu.Unlock()
	for _, l := range list {
		l.step(now)
	}
	if time.Duration(now-r.logAt) >= time.Minute {
		r.logMinute(now, list)
	}
	clear(list)
}

func (l *r1Link) step(now int64) {
	wrote := uint64(0)
	if l.mtr != nil {
		wrote = l.mtr.wrBytes.Load()
	}
	if l.sc != nil {
		l.sc.step(time.Duration(now), wrote)
	}
	if l.ls != nil {
		l.ls.tick(now, l.rate(now, wrote))
	}
}

// rate is the link's written bytes/s over the last r1RateSpan (0: no meter,
// or not yet a tick's worth: the scheduler's own count).
func (l *r1Link) rate(now int64, wrote uint64) float64 {
	if l.mtr == nil {
		return 0
	}
	l.wr = append(l.wr, r1Wrote{now, wrote})
	for len(l.wr) > 2 && time.Duration(now-l.wr[1].at) >= r1RateSpan {
		l.wr = l.wr[1:]
	}
	o := l.wr[0]
	if d := time.Duration(now - o.at); d >= schedTick {
		return float64(wrote-o.wrote) / d.Seconds()
	}
	return 0
}

// logMinute writes the schedulers' line of the minute (§7: "sched: light
// p99 … bulk frame … drops …"), when they moved anything.
func (r *r1Set) logMinute(now int64, list []*r1Link) {
	r.logAt = now
	var p99, frames []float64
	var lane, blocked uint64
	var drops [2]uint64
	n := 0
	for _, l := range list {
		ls := l.ls
		if ls == nil {
			continue
		}
		n++
		if _, p, ok := ls.lanePct(); ok {
			p99 = append(p99, p)
		}
		frames = append(frames, float64(ls.frameAt.Load())/1024)
		ls.mu.Lock()
		lane += uint64(ls.laneN)
		blocked += ls.blocked
		drops[0] += ls.drops[0]
		drops[1] += ls.drops[1]
		ls.mu.Unlock()
	}
	moved := lane != r.logLane || blocked != r.logBlocked || drops != r.logDrops
	dDrops := [2]uint64{drops[0] - min(drops[0], r.logDrops[0]), drops[1] - min(drops[1], r.logDrops[1])}
	dBlocked := blocked - min(blocked, r.logBlocked)
	r.logLane, r.logBlocked, r.logDrops = lane, blocked, drops
	if n == 0 || !moved {
		return
	}
	worst, med := 0.0, 0.0
	for _, p := range p99 {
		worst = max(worst, p)
	}
	if len(frames) > 0 {
		sort.Float64s(frames)
		med = frames[len(frames)/2]
	}
	coreLogf("sched: links %d, light p99 %.1f ms (worst link), bulk frame %.1f KiB (median link), drops light/bulk %d/%d, blocked-writers %d, users at memory cap %d (last minute)",
		n, worst, med, dDrops[0], dDrops[1], dBlocked, usersAtMemCap())
}

// CoreLines are the status lines of R1's cores on each link (REDESIGN
// §3.1 to §3.4), for hs2 status; each call starts the next one's span for
// the tallies that have one (C3's notsent and blocked ratio, the flips).
func CoreLines() []string {
	r1Links.mu.Lock()
	list := make([]*r1Link, 0, len(r1Links.all))
	for l := range r1Links.all {
		if !l.sess.IsClosed() {
			list = append(list, l)
		}
	}
	r1Links.mu.Unlock()
	sort.Slice(list, func(i, j int) bool { return list[i].id < list[j].id })
	now := ctrlNow()
	var out []string
	for _, l := range list {
		el := time.Duration(now - l.lineAt)
		l.lineAt = now
		if ls := l.ls; ls != nil {
			out = append(out, ls.line(l.id))
			light, bulk, unknown, flips := ls.classes()
			perMin := func(i int) float64 {
				if el <= 0 {
					return 0
				}
				return float64(flips[i]-l.flipsAt[i]) / el.Minutes()
			}
			// (the text of §3.1 as it is: the line follows its link's sched line)
			out = append(out, classLine(light, bulk, unknown, perMin(0), perMin(1)))
			l.flipsAt = flips
		}
		if l.hq != nil {
			out = append(out, l.hq.line(l.id, el))
		}
		if l.sc != nil && !l.sc.core.off {
			out = append(out, l.sc.line(l.id))
		}
	}
	return out
}
