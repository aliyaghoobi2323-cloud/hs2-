package engine

import (
	"fmt"
	"math"
	"math/bits"
	"math/rand/v2"
	"sort"
	"strings"
	"testing"
	"time"
)

// A flow-level plant for the autopilot: user connections pinned to links, a
// per-link cap (per-connection throttling: each link is one TCP connection)
// and a shared path cap, both shared max-min fairly; idle connections that the
// panel closes 300 s after their last byte; the retiring/close-when-empty
// actuator of LinkManager (same pick ordering, same victim ordering); dial
// latency; and, in reverse, an exit that keeps slots = up links, dials only
// while below the edge's target and retires a slot whose link the edge closed
// while it holds more slots than the target (exit_pool.go). The autopilot under
// test is the real one; the plant only produces the samples it sees.

const (
	kbit = 1000.0 / 8 // bytes/s
	mbit = 1e6 / 8    // bytes/s
)

type simConn struct {
	id       int
	link     *simLink
	demand   float64 // bytes/s wanted while active; +Inf = backlogged
	remain   float64 // bytes left of a finite transfer; < 0: none (rate flow)
	until    time.Time
	bulk     bool // reconnects when its transfer ends, until `until`
	segment  float64
	cv       float64 // per-tick lognormal demand noise
	hb       time.Duration
	nextHB   time.Time
	pendHS   float64 // handshake bytes to move on the first tick
	lastByte time.Time
	ewma     float64
	steady   uint8
	got      float64 // bytes moved this tick
	want     float64 // bytes/s wanted this tick
	prevWant float64
	busy     bool // the actuator considered it active when it was closed
}

type simLink struct {
	id                              int
	serving, retiring               bool
	born, servingSince, retireSince time.Time
	conns                           []*simConn
	picks                           int
	pickHist                        [pickWindow - 1]int
	rates                           [5]float64
	doms                            [3]float64
	n                               int
	rate, rate10, sustained         float64
	hist                            uint8
	lastRaw                         bool
	pressed                         bool
	flowing, recent                 int
	lastByte                        time.Time
	closedAt                        time.Time
}

type simCfg struct {
	min, max, perLink int
	linkCap, pathCap  float64 // bytes/s; 0 = none
	reverse           bool
	exitDials         bool // reverse: the exit dials the shortfall (false: it never does)
	statsOK           bool // download pressure visible (false: older exit)
	noPoolCtl         bool // reverse: exit ignores targets
	seed              uint64
	idleClose         time.Duration // panel connIdle (0: 300 s)
	drainIdle         time.Duration // reclaim on retiring links (0: 310 s)
	startT            int           // 0: warm
	tun               func(*apTunables)
}

type sim struct {
	t      testing.TB
	cfg    simCfg
	a      *autopilot
	rnd    *rand.Rand
	now    time.Time
	tick   int
	links  []*simLink
	nextID int
	conns  map[*simConn]struct{}
	gens   []func(s *sim)

	pending      []simPending // direct dials / reverse exit dials in flight
	exitSlots    int          // reverse: exit slots (up + dialing)
	targetDropAt time.Time
	lastT        int

	// metrics
	probes, succ, fails, inconcl, aborts, restores, shrinks int
	relieved                                                int
	dials, closes, cuts                                     int
	tChanges                                                int
	maxT, maxPhys                                           int
	notes                                                   []string
	demand, carried                                         float64 // this tick, bytes/s
	lastDec                                                 apDecision
	emptyReason                                             int
	onTick                                                  func(s *sim)
}

type simPending struct {
	due  int
	exit bool
}

func newSim(t testing.TB, cfg simCfg) *sim {
	if cfg.perLink == 0 {
		cfg.perLink = 8
	}
	if cfg.idleClose == 0 {
		cfg.idleClose = 300 * time.Second
	}
	if cfg.drainIdle == 0 {
		cfg.drainIdle = drainIdleDefault
	}
	s := &sim{t: t, cfg: cfg, rnd: rand.New(rand.NewPCG(cfg.seed, cfg.seed^0x9e3779b97f4a7c15)),
		now: time.Unix(1_700_000_000, 0), conns: map[*simConn]struct{}{}}
	s.a = newAutopilot(cfg.min, cfg.max, cfg.perLink)
	s.a.rnd = s.rnd.Float64
	if cfg.tun != nil {
		cfg.tun(&s.a.tun)
	}
	n := warmSize(cfg.min, cfg.max)
	if cfg.startT > 0 {
		n = cfg.startT
		s.a.T = n
	}
	for i := 0; i < n; i++ {
		s.addLink(false)
	}
	s.exitSlots = n
	s.lastT = s.a.T
	return s
}

func (s *sim) addLink(retiring bool) *simLink {
	l := &simLink{id: s.nextID, serving: !retiring, retiring: retiring, born: s.now, servingSince: s.now}
	if retiring {
		l.retireSince = s.now
	}
	s.nextID++
	s.links = append(s.links, l)
	return l
}

func (s *sim) counts() (S, R int) {
	for _, l := range s.links {
		if l.retiring {
			R++
		} else {
			S++
		}
	}
	return
}

// ---- traffic ----------------------------------------------------------------

// pick places a new connection exactly as LinkManager.pickLocked does.
func (s *sim) pick() *simLink {
	for tier := 0; tier < 2; tier++ {
		var chosen *simLink
		var best pickKey
		ties := 0
		for _, l := range s.links {
			if tier == 0 && l.retiring {
				continue
			}
			rp := l.picks
			for _, p := range l.pickHist {
				rp += p
			}
			k := newPickKey(l.pressed, l.flowing, l.picks, rp, len(l.conns), s.cfg.perLink)
			switch {
			case chosen == nil || k.less(best):
				chosen, best, ties = l, k, 1
			case !best.less(k):
				ties++
				if s.rnd.IntN(ties) == 0 {
					chosen = l
				}
			}
		}
		if chosen != nil {
			return chosen
		}
	}
	return nil
}

func (s *sim) open(c *simConn) *simConn {
	l := s.pick()
	if l == nil {
		return nil
	}
	l.picks++
	c.link = l
	c.lastByte = s.now
	l.conns = append(l.conns, c)
	s.conns[c] = struct{}{}
	return c
}

func (s *sim) closeConn(c *simConn) {
	l := c.link
	for i, x := range l.conns {
		if x == c {
			l.conns = append(l.conns[:i], l.conns[i+1:]...)
			break
		}
	}
	delete(s.conns, c)
}

// idle opens a connection that does a handshake and then sits idle.
func (s *sim) idle() *simConn { return s.open(&simConn{remain: -1, pendHS: 4 << 10}) }

// heartbeat opens a connection that sends 200 B every minute forever.
func (s *sim) heartbeat() *simConn {
	return s.open(&simConn{remain: -1, pendHS: 4 << 10, hb: time.Minute, nextHB: s.now.Add(time.Duration(s.rnd.IntN(60)) * time.Second)})
}

// web opens a connection that transfers size bytes as fast as it can, then
// idles (keep-alive) until the panel closes it.
func (s *sim) web(size float64) *simConn {
	return s.open(&simConn{demand: math.Inf(1), remain: size})
}

// rate opens a flow that wants r bytes/s (noise cv) for d, then idles.
func (s *sim) rate(r, cv float64, d time.Duration) *simConn {
	return s.open(&simConn{demand: r, remain: -1, cv: cv, until: s.now.Add(d)})
}

// bulk opens a backlogged download of seg bytes that reconnects when done,
// until d has passed.
func (s *sim) bulk(seg float64, d time.Duration) *simConn {
	return s.open(&simConn{demand: math.Inf(1), remain: seg, segment: seg, bulk: true, until: s.now.Add(d)})
}

func (s *sim) expDur(mean time.Duration) time.Duration {
	return time.Duration(s.rnd.ExpFloat64() * float64(mean))
}

// poisson returns the number of arrivals in one tick at rate perSec.
func (s *sim) poisson(perSec float64) int {
	l := perSec * healthTick.Seconds()
	n, p := 0, math.Exp(-l)
	for u := s.rnd.Float64(); u > p; u *= s.rnd.Float64() {
		n++
	}
	return n
}

func (s *sim) lognormal(cv float64) float64 {
	if cv <= 0 {
		return 1
	}
	sig := math.Sqrt(math.Log(1 + cv*cv))
	return math.Exp(s.rnd.NormFloat64()*sig - sig*sig/2)
}

// ---- allocation --------------------------------------------------------------

func waterFill(d []float64, c float64) []float64 {
	out := make([]float64, len(d))
	idx := make([]int, len(d))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool { return d[idx[a]] < d[idx[b]] })
	rem, left := c, len(d)
	for _, i := range idx {
		share := rem / float64(left)
		if d[i] <= share {
			out[i] = d[i]
		} else {
			out[i] = share
		}
		rem -= out[i]
		left--
	}
	return out
}

func (s *sim) allocate() {
	dt := healthTick.Seconds()
	type la struct {
		l      *simLink
		act    []*simConn
		d      []float64
		dSum   float64
		cap    float64
		alloc  float64
		inner  []float64
		offerd float64
	}
	var las []*la
	var linkDem []float64
	s.demand = 0
	for _, l := range s.links {
		x := &la{l: l}
		for _, c := range l.conns {
			c.got = 0
			w := 0.0
			switch {
			case c.pendHS > 0:
				w = c.pendHS / dt
			case c.hb > 0 && !s.now.Before(c.nextHB):
				w = 200 / dt
				c.nextHB = s.now.Add(c.hb)
			case c.remain > 0:
				w = math.Min(c.demand, c.remain/dt)
			case c.remain < 0 && c.demand > 0 && s.now.Before(c.until):
				w = c.demand * s.lognormal(c.cv)
			}
			c.want = w
			if w > 0 {
				x.act = append(x.act, c)
				x.d = append(x.d, w)
				x.dSum += w
			}
		}
		lc := math.Inf(1)
		if s.cfg.linkCap > 0 {
			lc = s.cfg.linkCap
		}
		x.cap = math.Min(lc, x.dSum)
		s.demand += x.dSum
		las = append(las, x)
		linkDem = append(linkDem, x.cap)
	}
	pc := math.Inf(1)
	if s.cfg.pathCap > 0 {
		pc = s.cfg.pathCap
	}
	la2 := waterFill(linkDem, pc)
	s.carried = 0
	for i, x := range las {
		x.alloc = la2[i]
		inner := waterFill(x.d, x.alloc)
		got := 0.0
		for j, c := range x.act {
			b := inner[j] * dt
			if c.pendHS > 0 {
				c.pendHS = 0
			} else if c.remain > 0 {
				b = math.Min(b, c.remain)
				c.remain -= b
				if c.remain < 1 {
					c.remain = 0
				}
			}
			c.got = b
			got += b
		}
		// The link's sender is blocked when it has more to send than the
		// path lets through (and it moved a real amount).
		x.l.lastRaw = x.dSum > x.alloc*1.02 && x.alloc*dt >= pressMinBytes
		rate := got / dt
		s.carried += rate
		l := x.l
		l.rate = rate
		l.rates[l.n%len(l.rates)] = rate
		l.doms[l.n%len(l.doms)] = rate
		l.n++
		l.rate10 = mean(l.rates[:min(l.n, len(l.rates))])
		l.sustained = 0
		if l.n >= len(l.doms) {
			l.sustained = math.Min(l.doms[0], math.Min(l.doms[1], l.doms[2]))
		}
	}
}

// ---- one tick ----------------------------------------------------------------

func (s *sim) step() {
	s.now = s.now.Add(healthTick)
	s.tick++
	dt := healthTick

	// Links dialed earlier come up.
	kept := s.pending[:0]
	for _, p := range s.pending {
		if p.due > s.tick {
			kept = append(kept, p)
			continue
		}
		S, _ := s.counts()
		s.addLink(s.cfg.reverse && S >= s.a.T)
		s.dials++
	}
	s.pending = kept

	for _, g := range s.gens {
		g(s)
	}
	s.allocate()

	// Per-connection bookkeeping, as mtcpLink.flowStats does it.
	alpha := flowAlpha(dt)
	var ending []*simConn
	for _, l := range s.links {
		copy(l.pickHist[1:], l.pickHist[:len(l.pickHist)-1])
		l.pickHist[0], l.picks = l.picks, 0
		l.flowing, l.recent = 0, 0
		for _, c := range l.conns {
			steady := false
			if c.got > 0 {
				c.ewma += alpha * (c.got/dt.Seconds() - c.ewma)
				c.lastByte = s.now
				steady = c.got/dt.Seconds() >= flowSteadyRate
			} else {
				c.ewma -= alpha * c.ewma
			}
			c.steady = (c.steady<<1 | b2u(steady)) & 7
			if c.ewma >= flowingRate && s.now.Sub(c.lastByte) <= flowRecent || c.steady == 7 {
				l.flowing++
			}
			if s.now.Sub(c.lastByte) <= s.cfg.drainIdle {
				l.recent++
			}
			if c.lastByte.After(l.lastByte) {
				l.lastByte = c.lastByte
			}
			switch {
			case c.bulk && c.remain == 0:
				ending = append(ending, c) // segment done: reconnect
			case s.now.Sub(c.lastByte) >= s.cfg.idleClose:
				ending = append(ending, c) // the panel's idle timeout
			}
		}
		// Pressure: 2 of the last 3 raw samples; download stats arrive one
		// tick late (the exit's record is polled after the tick).
		l.hist = (l.hist<<1 | b2u(l.lastRaw)) & 15
		l.pressed = s.cfg.statsOK && !l.retiring && bits.OnesCount8((l.hist>>1)&7) >= 2
	}
	for _, c := range ending {
		s.closeConn(c)
		if c.bulk && s.now.Before(c.until) {
			s.bulk(c.segment, c.until.Sub(s.now))
		}
	}

	// Decide.
	smp := apSample{now: s.now, open: len(s.conns), growable: !s.cfg.noPoolCtl}
	for _, l := range s.links {
		smp.links = append(smp.links, apLink{id: l.id, serving: !l.retiring, retiring: l.retiring,
			servingSince: l.servingSince, pressed: l.pressed, rate: l.rate, rate10: l.rate10,
			sustained: l.sustained, flowing: l.flowing, open: len(l.conns)})
		smp.G += l.rate
		smp.flowing += l.flowing
	}
	hadProbe := s.a.pr != nil
	d := s.a.decide(smp)
	s.lastDec = d
	if d.reason == "" {
		s.emptyReason++
	}
	if !hadProbe && s.a.pr != nil {
		s.probes++
	}
	switch {
	case strings.Contains(d.note, "links kept:"):
		s.succ++
	case strings.Contains(d.note, "kept as headroom"):
		s.relieved++
	case strings.HasPrefix(d.note, "sized to"):
		s.fails++
	case strings.Contains(d.note, "kept as spares"):
		s.inconcl++
	case strings.Contains(d.note, "came up in"):
		s.aborts++
	case strings.Contains(d.note, "undone"):
		s.restores++
	case d.phase == apShrinking && d.note != "":
		s.shrinks++
	}
	if d.note != "" {
		s.notes = append(s.notes, fmt.Sprintf("%6s %s", fmtDur(time.Duration(s.tick)*healthTick), d.note))
	}
	T := d.target
	if T != s.lastT {
		s.tChanges++
		if T < s.lastT {
			s.targetDropAt = s.now
		}
		s.lastT = T
	}
	s.maxT = max(s.maxT, T)
	s.reconcile(T)
	s.drain()
	s.maxPhys = max(s.maxPhys, len(s.links))
	if s.onTick != nil {
		s.onTick(s)
	}
}

// reconcile mirrors LinkManager.reconcile (+ the exit in reverse).
func (s *sim) reconcile(T int) {
	var serving, retiring []*simLink
	for _, l := range s.links {
		if l.retiring {
			retiring = append(retiring, l)
		} else {
			serving = append(serving, l)
		}
	}
	if len(serving) < T && len(retiring) > 0 {
		sort.SliceStable(retiring, func(i, j int) bool {
			a, b := retiring[i], retiring[j]
			if len(a.conns) != len(b.conns) {
				return len(a.conns) > len(b.conns)
			}
			return a.lastByte.After(b.lastByte)
		})
		for len(serving) < T && len(retiring) > 0 {
			l := retiring[0]
			retiring = retiring[1:]
			l.retiring, l.serving, l.servingSince = false, true, s.now
			serving = append(serving, l)
		}
	}
	if len(serving) > T {
		sort.SliceStable(serving, func(i, j int) bool {
			a, b := serving[i], serving[j]
			switch {
			case a.flowing != b.flowing:
				return a.flowing < b.flowing
			case a.recent != b.recent:
				return a.recent < b.recent
			case len(a.conns) != len(b.conns):
				return len(a.conns) < len(b.conns)
			}
			return a.rate10 < b.rate10
		})
		for _, l := range serving[:len(serving)-T] {
			l.retiring, l.serving, l.retireSince, l.pressed = true, false, s.now, false
		}
	}
	S, R := s.counts()
	if !s.cfg.reverse {
		step := max(1, (T+3)/4)
		n := min(T-S, step)
		for i := 0; i < n && S+R+len(s.pending) < s.cfg.max; i++ {
			s.pending = append(s.pending, simPending{due: s.tick + 1})
			S++
		}
		return
	}
	// The exit: keep slots up to its (clamped) want, dialing the shortfall.
	want := min(max(T, s.cfg.min), s.cfg.max)
	if s.cfg.noPoolCtl {
		want = s.exitSlots
	}
	for s.cfg.exitDials && s.exitSlots < want {
		s.exitSlots++
		s.pending = append(s.pending, simPending{due: s.tick + 1 + s.rnd.IntN(2), exit: true})
	}
}

// drain mirrors LinkManager.drainTick (+ the panel idle close, which already
// happened in step).
func (s *sim) drain() {
	closes := 0
	kept := s.links[:0]
	guard := !s.cfg.reverse || (s.now.Sub(s.targetDropAt) >= retireAfterDrop && !s.cfg.noPoolCtl)
	for _, l := range s.links {
		if l.retiring && s.cfg.drainIdle > 0 {
			for _, c := range append([]*simConn(nil), l.conns...) {
				if s.now.Sub(c.lastByte) >= s.cfg.drainIdle {
					if c.want > 0 {
						s.cuts++
					}
					s.closeConn(c)
				}
			}
		}
		if l.retiring && guard && closes < maxClosesPerTick && len(l.conns) == 0 &&
			(!s.cfg.reverse || s.now.Sub(l.born) >= retireAfterDrop) {
			closes++
			s.closes++
			if s.cfg.reverse {
				want := min(max(s.a.T, s.cfg.min), s.cfg.max)
				if s.exitSlots > want {
					s.exitSlots-- // retireIfOver
				} else {
					s.pending = append(s.pending, simPending{due: s.tick + 1, exit: true})
				}
			}
			continue
		}
		kept = append(kept, l)
	}
	s.links = kept
}

func (s *sim) run(d time.Duration) {
	for end := s.now.Add(d); s.now.Before(end); {
		s.step()
	}
}

func (s *sim) serving() int { S, _ := s.counts(); return S }

// dump returns the controller's decision log for a failure message.
func (s *sim) dump() string {
	n := len(s.notes)
	from := max(0, n-40)
	return fmt.Sprintf("T=%d links=%d (%d serving) probes=%d succ=%d relieved=%d fail=%d inconcl=%d aborts=%d restores=%d dials=%d closes=%d\nlast reason: %s\n%s",
		s.a.T, len(s.links), s.serving(), s.probes, s.succ, s.relieved, s.fails, s.inconcl, s.aborts, s.restores, s.dials, s.closes,
		s.lastDec.reason, strings.Join(s.notes[from:], "\n"))
}

// ---- traffic generators -------------------------------------------------------

// genIdlePool keeps about n idle connections open: clients reconnect as the
// panel closes them after its idle timeout.
func genIdlePool(n int) func(*sim) {
	return func(s *sim) {
		for i := s.poisson(float64(n) / s.cfg.idleClose.Seconds()); i > 0; i-- {
			s.idle()
		}
	}
}

// genFlows keeps about n rate flows of r bytes/s (noise cv) active, each
// lasting mean `life`, arriving as a Poisson process. n may change over time.
func genFlows(n func(*sim) float64, r, cv float64, life time.Duration) func(*sim) {
	return func(s *sim) {
		for i := s.poisson(n(s) / life.Seconds()); i > 0; i-- {
			s.rate(r, cv, s.expDur(life))
		}
	}
}

func constN(n float64) func(*sim) float64 { return func(*sim) float64 { return n } }

// seedIdle opens n idle connections at once (already established).
func (s *sim) seedIdle(n int) {
	for i := 0; i < n; i++ {
		c := s.idle()
		c.pendHS = 0
		c.lastByte = s.now.Add(-time.Duration(s.rnd.IntN(int(s.cfg.idleClose.Seconds()))) * time.Second)
	}
}
