package censor

import (
	"fmt"
	"math"
	"math/bits"
	"net/netip"
	"sort"
	"strings"
	"time"
)

// Evaluator: one goroutine, 5 s tick, 30 s trigger window (6 ticks), episodes
// per Iran IP (censor351 §3.2), rule classification into 16 classes (§3.3).

const (
	tick        = 5 * time.Second
	winTicks    = 6  // 30 s trigger window
	ringTicks   = 12 // 60 s kept per scope
	closeAfter  = 60 * time.Second
	openLogAt   = 30 * time.Second
	ownGrace    = 5 * time.Minute
	peerGrace   = 2 * time.Minute
	maxScopes   = 50
	maxEpisodes = 64
	maxKh       = 64
	maxLinesMin = 30
	freshLife   = 120 * time.Second
	// slot 9 (s): a kill on a link younger than youngLife is the
	// at-once / after-N-bytes signature (rst-inject, fingerprint-kill): a
	// busy carrier link reaches any byte trigger within seconds. A
	// delayed kill (RST after N seconds of age) never kills a link that
	// young; its redials live a while, then die (young < age < freshLife).
	youngLife = 10 * time.Second
	// slot 11 (w): the most a link carries, both directions (Death.Bytes is
	// acked + received), when an injector resets it at connect, in the TLS
	// handshake or in the first records: a ClientHello (<= 2 KB with a
	// post-quantum key share), the server flight (a real certificate chain
	// on tls, 3-5 KB; Reality's own certificate is smaller) and the first
	// tunnel records. rst-inject is an immediate reset, so its young kills
	// fall within it. A young kill past it is a reset after N bytes (a busy
	// link reaches any byte trigger within seconds): fingerprint-kill.
	hsBytes = 8 << 10
	// slot 9 (q): the queue state of a tick with no reading of its own is
	// the last reading's, for at most qHoldTicks ticks.
	qHoldTicks = 2
	// slot 10 (u): clean-path RSTs after a standing queue that open a new
	// episode: the C1 rst-inject necessity (rst >= 3), evidence of its own.
	splitRST = 3

	// slot 4 (censor351 §6.5): a sustained cut keeps its episode open while
	// the hook data still shows the path dead (no success since the last
	// dial timeout / unreachable or silent reading), at most cutHoldMax.
	cutHoldMax = 2 * time.Hour
	// slot 4: peer restart under l3mtcp = a mass RST on every Iran IP
	// inside massSpan (each IP >= massPerIP RSTs), then a successful redial
	// or accept on every IP within massConfirm, with at most massPostMax
	// further RSTs after the burst (injection keeps resetting the redials).
	massSpan    = 10 * time.Second
	massConfirm = 30 * time.Second
	massPerIP   = 2
	massPostMax = 2
	// slot 7 (m): a restart kills every link on every IP, and the LB
	// spreads the links evenly over the Iran IPs, so the burst's deaths
	// are balanced across IPs. massBalance is the largest IP's death count
	// over the smallest's that still counts as one restart (room for an
	// uneven split of idle and busy links); a one-IP injection burst next
	// to a few deaths on the other IPs is far outside it.
	massBalance = 4
	// slot 5 (e): the old no-fast-RST drop (most links on >= 80 % of the
	// scopes dying in one tick) is only a candidate. It is a peer restart
	// once every scope has redialed or accepted, the path stays clean for
	// oldObserve after the drop (at most massPostMax timeouts, unreachables
	// or silent readings in all), and both happen within oldObserve.
	// Bufferbloat, loss and outages kill links on every IP too, but their
	// timeouts and silence go on after the drop.
	oldObserve = massSpan + massConfirm
	// slot 5 (f, g): a mean queue at or above this explains resends and
	// slow handshakes (bufferbloat signature, the O6 threshold).
	queueExplainMs = 100
)

// Class names: 9 censorship + 7 ordinary (censor351 §1, §3.3).
const (
	ClRSTInject = iota
	ClHandshakeBlock
	ClBlackhole
	ClPortBlock
	ClFingerprintKill
	ClThrottle
	ClIntlCut
	ClDelayedKill
	ClUDPDiff
	ClCongestion
	ClPathOutage
	ClISPOutage
	ClPeerRestart
	ClOwnRestart
	ClBufferbloat
	ClAsymLoss
	NClasses
	ClUnknown = -1
	// step back §7.4: a censorship verdict that fails stage 1 (the IP
	// recovers after our own redials, R1) or lacks its own signature (R2)
	// is logged as an anomaly at low, its exact class as hint=.
	ClAnomaly = NClasses
)

// Families (step back §7.4, stage 2): the verdict gated at med+ is the
// family; the exact class is a hint, never gated.
const (
	FamNone = iota
	FamReset
	FamHandshake
	FamBlock
	FamThrottle
	FamUDP
)

var famName = [...]string{"-", "reset", "handshake", "block", "throttle", "udp"}

// FamilyOf returns the family of censorship class c (FamNone otherwise).
func FamilyOf(c int) int {
	switch c {
	case ClRSTInject, ClFingerprintKill, ClDelayedKill:
		return FamReset
	case ClHandshakeBlock:
		return FamHandshake
	case ClBlackhole, ClPortBlock, ClIntlCut:
		return FamBlock
	case ClThrottle:
		return FamThrottle
	case ClUDPDiff:
		return FamUDP
	}
	return FamNone
}

const (
	// R1 (step back §7.4): a run of >= recoverOK accepts / dials that
	// succeed, with fewer than half as many fresh kills, is the IP
	// recovering after our own redials (a restart refills; an injector
	// does not let redials live). It vetoes stage 1 for the families whose
	// evidence is lost or failed links (reset, handshake, block).
	recoverOK = 8
	// R2: delayed-kill reaches med only on its own signature, a lifetime
	// cluster of >= dkLifeShare among >= dkLifeMin kills of links born
	// after the episode opened ((z): a pool refilled at one moment dies at
	// one age whatever resets it; the redial wave's links do not).
	dkLifeShare = 0.7
	dkLifeMin   = 8
	// (z): without a redial wave old enough to die inside the episode (an
	// age T longer than the episode so far), the whole-episode cluster
	// counts only when its kills are spread over >= dkTicksMin ticks: a
	// pool refilled at one moment and reset at one moment dies in one or
	// two ticks; a delayed kill kills each link as it reaches T.
	dkTicksMin = 4
)

var className = [NClasses]string{
	"rst-inject", "handshake-block", "blackhole", "port-block", "fingerprint-kill",
	"throttle", "intl-cut", "delayed-kill", "udp-diff",
	"congestion", "path-outage", "isp-outage", "peer-restart", "own-restart", "bufferbloat", "asym-loss",
}

// IsCensorship reports whether class c is one of the 9 censorship classes.
func IsCensorship(c int) bool { return c >= ClRSTInject && c <= ClUDPDiff }

// ClassName returns the class's log name.
func ClassName(c int) string {
	if c == ClAnomaly {
		return "anomaly"
	}
	if c < 0 || c >= NClasses {
		return "unknown"
	}
	return className[c]
}

const (
	confUnknown = iota
	confLow
	confMed
	confHigh
)

var confName = [...]string{"unknown", "low", "med", "high"}

// ctr is the counters of one scope over one tick (or summed over an episode).
type ctr struct {
	dials, dialOK              uint32
	stage                      [nDialStages]uint32
	dialR, failR, dialT, failT uint32
	death                      [nDeathClasses]uint32
	rstFast, fresh, freshKill  uint32
	// slot 9 (q): RSTs (and fast RSTs) that arrived in a tick with no
	// standing queue; a queue cannot explain them.
	rstNoQ, rstFastNoQ uint32
	// slot 9 (p): RSTs on links born before an armed mass burst (the
	// dead process's leftovers), filled only while a burst is armed.
	rstPreBurst uint32
	// step back R1: RST / timeout kills on the scope after one of our
	// redials or accepts succeeded in the open episode (the redials being
	// killed too: an injector, not a one-shot restart).
	killPostOK uint32
	// b414: resets on links the Iran end reported it aborted itself
	// (NotePeerAborts); kept out of every rule, shown as peerab=.
	peerAb                                        uint32
	acc                                           [nAcceptOutcomes]uint32
	samples, silent, lossy, highQ                 uint32
	moving                                        uint32 // slot 5: readings with bytes acked (path carries data)
	lossSum, qSum, sx, sy, sxx, syy, sxy, rateSum float64
	peerLossSum                                   float64
	peerLossN                                     uint32
	busySum, limSum                               float64 // M7: busy links, path-limited share
	limN                                          uint32
	khSeen, khBad                                 uint64 // Kharej IP index bitsets
}

func (c *ctr) add(o *ctr) {
	c.dials += o.dials
	c.dialOK += o.dialOK
	for i := range c.stage {
		c.stage[i] += o.stage[i]
	}
	c.dialR += o.dialR
	c.failR += o.failR
	c.dialT += o.dialT
	c.failT += o.failT
	for i := range c.death {
		c.death[i] += o.death[i]
	}
	c.rstFast += o.rstFast
	c.fresh += o.fresh
	c.freshKill += o.freshKill
	c.rstNoQ += o.rstNoQ
	c.rstFastNoQ += o.rstFastNoQ
	c.rstPreBurst += o.rstPreBurst
	c.killPostOK += o.killPostOK
	c.peerAb += o.peerAb
	for i := range c.acc {
		c.acc[i] += o.acc[i]
	}
	c.samples += o.samples
	c.silent += o.silent
	c.lossy += o.lossy
	c.highQ += o.highQ
	c.moving += o.moving
	c.lossSum += o.lossSum
	c.qSum += o.qSum
	c.sx += o.sx
	c.sy += o.sy
	c.sxx += o.sxx
	c.syy += o.syy
	c.sxy += o.sxy
	c.rateSum += o.rateSum
	c.peerLossSum += o.peerLossSum
	c.peerLossN += o.peerLossN
	c.busySum += o.busySum
	c.limSum += o.limSum
	c.limN += o.limN
	c.khSeen |= o.khSeen
	c.khBad |= o.khBad
}

func (c *ctr) connFails() uint32 {
	return c.stage[DialConnTimeout] + c.stage[DialConnRefused] + c.stage[DialConnUnreach] + c.stage[DialConnOther]
}
func (c *ctr) hsFails() uint32 {
	return c.stage[DialHelloTimeout] + c.stage[DialHelloReset] + c.stage[DialHelloEOF]
}
func (c *ctr) unexplained() uint32 {
	return c.death[DeathRST] + c.death[DeathTimeout] + c.death[DeathUnreach]
}
func (c *ctr) accFails() uint32 { return c.acc[AcceptNoHello] + c.acc[AcceptReset] + c.acc[AcceptEOF] }

// ceilingLimited (M7): on average at least one busy link per reading, and
// the busy links network-limited for at least half their time.
func (c *ctr) ceilingLimited() bool {
	return c.samples > 0 && c.limN >= 3 && c.busySum/float64(c.samples) >= 1 && c.limSum/float64(c.limN) >= 0.5
}

func (c *ctr) meanLoss() float64 {
	if c.samples == 0 {
		return 0
	}
	return c.lossSum / float64(c.samples)
}
func (c *ctr) meanQ() float64 {
	if c.samples == 0 {
		return 0
	}
	return c.qSum / float64(c.samples)
}
func (c *ctr) corr() float64 {
	n := float64(c.samples)
	if n < 4 {
		return 0
	}
	vx := c.sxx/n - (c.sx/n)*(c.sx/n)
	vy := c.syy/n - (c.sy/n)*(c.sy/n)
	if vx <= 1e-12 || vy <= 1e-12 {
		return 0
	}
	return (c.sxy/n - (c.sx/n)*(c.sy/n)) / math.Sqrt(vx*vy)
}

// killStats is an episode's kill-death evidence (RST/timeout deaths).
type killStats struct {
	bytesHist  [24]uint32
	lifeHist   [20]uint32
	killDeaths uint32
	// slot 9 (s): kill deaths by link age: young (< youngLife) and mid
	// (youngLife .. freshLife).
	killYoung, killMid uint32
	// slot 11 (w): young kills (with TCP_INFO) by bytes at death: within
	// the handshake bytes (killHS) or past them (killBig).
	killHS, killBig uint32
	// slot 10 (t): spread of the fresh kills (< freshLife) in log space,
	// bytes at death and age at death: sums of ln and ln^2. A byte trigger
	// (fingerprint-kill) kills at one byte count at any age; an age trigger
	// (delayed-kill) kills at one age with any byte count.
	fkN                      float64
	fkLb, fkLb2, fkLl, fkLl2 float64
	// step back (z): kills of links born after the episode opened (the
	// redial wave), by age at death; R2 reads their cluster.
	bornLife  [20]uint32
	bornKills uint32
}

// minus returns k less the earlier snapshot o (the kills since o).
func (k killStats) minus(o killStats) killStats {
	for i := range k.bytesHist {
		k.bytesHist[i] -= min(o.bytesHist[i], k.bytesHist[i])
	}
	for i := range k.lifeHist {
		k.lifeHist[i] -= min(o.lifeHist[i], k.lifeHist[i])
	}
	k.killDeaths -= min(o.killDeaths, k.killDeaths)
	k.killYoung -= min(o.killYoung, k.killYoung)
	k.killMid -= min(o.killMid, k.killMid)
	k.killHS -= min(o.killHS, k.killHS)
	k.killBig -= min(o.killBig, k.killBig)
	for i := range k.bornLife {
		k.bornLife[i] -= min(o.bornLife[i], k.bornLife[i])
	}
	k.bornKills -= min(o.bornKills, k.bornKills)
	k.fkN, k.fkLb, k.fkLb2, k.fkLl, k.fkLl2 = k.fkN-o.fkN, k.fkLb-o.fkLb, k.fkLb2-o.fkLb2, k.fkLl-o.fkLl, k.fkLl2-o.fkLl2
	return k
}

// episode is one run of trouble on one scope.
type episode struct {
	id           string
	scope        netip.Addr
	opened, last time.Time
	// b414: the open time a held reset's own time would have given (one
	// tick earlier, when resets held for a late abort report opened it);
	// zero means opened. Used only for "born after the open" and for which
	// released resets join the kill statistics.
	bornFrom time.Time
	sum      ctr
	killStats
	// slot 10 (u): the episode saw a standing-queue tick. The clean ticks
	// after the queue's last tick are held apart (pend, pendKill) until
	// they carry rst evidence of their own (a new episode: the evidence
	// changed kind) or the queue comes back (they join this episode).
	sawQueue      bool
	pend          ctr
	pendKill      killStats // the episode's kill stats at its last queued tick
	pending       bool
	usersAtOpen   float64 // users ratio vs baseline when it opened (-1 unknown)
	xray          bool
	grace         string // "own" / "peer" / ""
	peerMass      int    // slot 4: RST deaths a confirmed peer-restart burst explained (0: none)
	cutHeld       bool   // slot 4: the quiet close was held at least once (path still cut)
	allIPs        bool   // every known Iran IP had an open episode together
	ispsTogether  int
	logged        bool
	lastUpd       time.Time
	class, conf   int
	alt, altScore int
	hint          int // step back: the exact class of an anomaly (ClUnknown: none)
	killTicks     int // (z): ticks in which the episode's kills arrived
	killPrev      uint32
	veto          string // step back: why stage 1 failed ("recovered", "nolife", "")
}

type scope struct {
	ip      netip.Addr
	buckets [ringTicks]ctr
	cur     int
	ep      *episode
	seq     int
	rateHr  [24]float64 // hourly baseline of mean path rate (in-memory, M7)
	rateDay [24]int     // day index the baseline was last written (needs ≥ 1 day)
	hourAcc float64
	hourN   int
	hourAt  int
	pin     pathIn // M7/O7 inputs as received (status only, for lab proof)
	// slot 4: the path's dead state as the hooks show it (set by a dial
	// timeout / unreachable or a silent reading, cleared by any success).
	dead      bool
	deadSince time.Time
	// slot 4 mass-RST bookkeeping (see massSpan)
	massRst int
	massOK  bool
	oldOK   bool // slot 5: redial / accept seen since the old-signature drop
	// slot 8 (o): the burst's own RST evidence (RSTs, fast RSTs, fresh
	// kills), the only part a confirmed restart explains
	massRstB, massFastB, massFreshB uint32
	// slot 9 (q): ticks left in which the last reading's standing queue
	// still holds (qHoldTicks after the last queued reading).
	qHold int
	// step back R1: a redial / accept succeeded while the episode was open
	// (kills from then on count as killPostOK); cleared with no episode.
	okAfterKill bool
	// b414: when okAfterKill was set (the first success in the open
	// episode), so a held reset released later is judged at its own time.
	okAt time.Time
	// b414: this tick's flushHeld released resets of the previous tick.
	released bool
	// b414 peer-abort matching (Kharej side). peerSig: the Iran end of
	// this scope has reported its own aborts at least once (a newer Iran);
	// until then nothing below is used and resets count as they always
	// did. peerCredit: reported aborts not yet matched to a reset, valid
	// until peerCreditAt+peerWin. held: resets waiting up to peerHold for
	// the report (it rides the next control ping, 2-4 s on a busy link).
	peerSig      bool
	peerCredit   uint64
	peerCreditAt time.Time
	held         []event
	// b414 v4: peerOrphan: the last link on this IP whose pings carried the
	// count ended (evPeerOrphan); held resets then wait for the next link's
	// first ping (any evPeerAbort clears it) or PeerOrphanHold.
	peerOrphan bool
	// b414 v5: when the Iran end's own-abort count on this IP last rose
	// (an evPeerAbort with n > 0); zero: never. Gates the orphan hold.
	peerAbAt time.Time
}

// b414: how long a Kharej reset waits for the Iran end's abort report, and
// how long a report stays valid for a later reset. Neither is a score knob:
// they bound the control ping's cadence (controlInterval 3 s, 2-4 s off the
// beat) plus one evaluator tick.
const (
	peerHold = 5 * time.Second
	peerWin  = 10 * time.Second
	heldCap  = 256
	// PeerOrphanHold (b414 v4): how long a reset on an IP with no live
	// reporting link waits for the next link's first ping, which carries
	// the aborts the dead links could not report. Two minutes: an Iran end
	// that has not got a link through by then is cut off (the episode is
	// the handshake failures, judged without the resets), and a reset is
	// still counted at its own time, so only the verdict line is later.
	PeerOrphanHold = 2 * time.Minute
)

// pathIn tallies the path-sample inputs one scope received since start:
// samples, how many had PeerLoss / PathLimited known, the maxima, and the
// last sample. Read-only bookkeeping for `hs2 status --censor`; no rule
// reads it.
type pathIn struct {
	n, peerN, limN, busyN uint32
	peerMax, limMax       float64
	busyMax               uint16
	last                  PathSample
}

func (q *pathIn) add(p PathSample) {
	q.n++
	q.last = p
	if p.PeerLoss >= 0 {
		q.peerN++
		q.peerMax = max(q.peerMax, p.PeerLoss)
	}
	if p.PathLimited >= 0 {
		q.limN++
		q.limMax = max(q.limMax, p.PathLimited)
	}
	if p.BusyLinks > 0 {
		q.busyN++
		q.busyMax = max(q.busyMax, p.BusyLinks)
	}
}

type evaluator struct {
	side   Side
	logf   func(string, ...any)
	start  time.Time
	debug  bool
	ispMap map[netip.Addr]string
	scopes map[netip.Addr]*scope
	khIdx  map[netip.Addr]int
	// path samples received / of them without an Iran-IP scope (status only)
	pathEv, pathNoScope uint64
	// domestic witness (M6): opens/s now vs a 1 h EWMA
	usersLast   uint64
	usersLastAt time.Time
	usersRate   float64
	usersBase   float64
	usersHave   bool
	peerRestart time.Time
	// b437 Q2: ZDU generation changes. ownUntil: the own-restart grace's end
	// for a ZDU child (its handover's end; zero: start + ownGrace).
	// peerZDU: the last peer grace came from a peer generation change, not
	// a restart. genAt: the last generation event of the peer (a burst of
	// old-link ends opens one grace, at its first). nGenOld / nGenNew:
	// peerOld link ends / new peer generations seen (status).
	ownUntil         time.Time
	ownZDU, peerZDU  bool
	genAt            time.Time
	nGenOld, nGenNew uint64
	// slot 4: the mass-RST peer-restart candidate (zero: none)
	massAt   time.Time
	massPost int
	// slot 6 (i): RSTs inside the burst (a FIN-only burst is a retire)
	massRstSeen int
	nMass       uint64 // confirmed mass-RST peer restarts (status only)
	// slot 5 (e): the old-signature candidate (zero: none)
	oldAt   time.Time
	oldPost int
	nOld    uint64 // confirmed old-signature peer restarts (status only)
	// counters for status
	nEpisodes   [NClasses + 2][4]uint32
	closedLines []string
	lines       int
	linesMin    int64
	folded      int
	evals       uint64
	now         func() time.Time
	testPanic   bool
	rec         *recorder // lab-only event recorder (HS2_CENSOR_RECORD); nil: off
}

func newEvaluator(side Side, logf func(string, ...any), start time.Time) *evaluator {
	return &evaluator{side: side, logf: logf, start: start, scopes: map[netip.Addr]*scope{},
		khIdx: map[netip.Addr]int{}, ispMap: map[netip.Addr]string{}, now: time.Now}
}

func (e *evaluator) run() {
	defer func() {
		if e.rec != nil {
			e.rec.flush()
		}
		if r := recover(); r != nil {
			active.Store(false)
			disabled.Store(true)
			e.logf("censor351: evaluator panic: %v — detector disabled (no effect on the tunnel)", r)
		}
	}()
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case ev := <-ring:
			if e.rec != nil {
				e.rec.event(&ev)
			}
			e.ingest(ev)
		case <-t.C:
			now := e.now()
			if e.rec != nil {
				e.rec.tick(now)
				if e.rec.err != nil {
					e.logf("censor351: recorder off: %v", e.rec.err)
					e.rec.f.Close()
					e.rec = nil
				}
			}
			e.step(now)
		}
	}
}

// ids maps an event to (Iran IP, Kharej IP).
func (e *evaluator) ids(local, remote netip.Addr) (ir, kh netip.Addr) {
	if e.side == SideIran {
		return local, remote
	}
	return remote, local
}

func (e *evaluator) scopeOf(ir netip.Addr) *scope {
	if !ir.IsValid() {
		return nil
	}
	s := e.scopes[ir]
	if s == nil {
		if len(e.scopes) >= maxScopes {
			return nil
		}
		s = &scope{ip: ir}
		e.scopes[ir] = s
	}
	return s
}

func (e *evaluator) khBit(kh netip.Addr) uint64 {
	if !kh.IsValid() {
		return 0
	}
	i, ok := e.khIdx[kh]
	if !ok {
		if len(e.khIdx) >= maxKh {
			return 0
		}
		i = len(e.khIdx)
		e.khIdx[kh] = i
	}
	return 1 << uint(i)
}

func log2b(v uint64, n int) int {
	b := bits.Len64(v)
	if b >= n {
		b = n - 1
	}
	return b
}

func (e *evaluator) ingest(ev event) {
	if e.testPanic {
		panic("censor test panic")
	}
	if ev.kind == evUsers {
		e.noteUsers(ev.n, time.Unix(0, ev.at))
		return
	}
	if ev.kind == evGen {
		e.noteGen(ev)
		return
	}
	ir, kh := e.ids(ev.local, ev.remote)
	if ev.kind == evPath {
		e.pathEv++
	}
	if ev.kind == evAccept {
		// Accept outcomes only count for Kharej IPs we know as link peers
		// (seen on a link, or authenticated here): scanners never create
		// scopes (bounded memory, no scanner noise).
		if _, known := e.khIdx[kh]; !known && AcceptOutcome(ev.stage) != AcceptOK {
			return
		}
	}
	s := e.scopeOf(ir)
	if s == nil {
		if ev.kind == evPath {
			e.pathNoScope++
		}
		return
	}
	c := &s.buckets[s.cur]
	bit := e.khBit(kh)
	c.khSeen |= bit
	switch ev.kind {
	case evDial:
		st := DialStage(ev.stage)
		switch st {
		case DialOK:
			s.dead = false
		case DialConnTimeout, DialConnUnreach:
			s.markDead(time.Unix(0, ev.at))
		}
		c.dials++
		if st < nDialStages {
			c.stage[st]++
		}
		ok := st == DialOK
		if ok {
			c.dialOK++
			s.noteOK(time.Unix(0, ev.at))
		}
		switch ev.front {
		case FrontReality:
			c.dialR++
			if !ok {
				c.failR++
			}
		case FrontTLS:
			c.dialT++
			if !ok {
				c.failT++
			}
		}
		if !ok && st <= DialConnOther && st != DialConnRefused {
			c.khBad |= bit
		}
	case evAccept:
		if AcceptOutcome(ev.stage) == AcceptOK {
			s.dead = false
			s.noteOK(time.Unix(0, ev.at))
		}
		if ev.stage < uint8(nAcceptOutcomes) {
			c.acc[ev.stage]++
		}
	case evPeerOrphan:
		// b414 v5: hold only with evidence that the Iran end itself is
		// killing links on this IP: its own-abort count rose within
		// PeerOrphanHold before the last link ended. The Iran end counts a
		// link only when its own socket ends in ECONNABORTED (killed on
		// the Iran host, e.g. ss -K), never for a reset that reached it,
		// so an injector cannot raise the count; and an own-abort round
		// that leaves links on the IP is reported by their next ping
		// (2-4 s), so own aborts that end in an orphan show the rise
		// first. With no recent rise the last links died to something the
		// Iran end did not do (an injected reset kills them all before any
		// ping), and their resets keep the v1 peerHold. The window equals
		// the engine's ctrlAbortLastAge: an older count cannot credit the
		// next link's first ping anyway.
		peerStat.orphans.Add(1)
		at := time.Unix(0, ev.at)
		if s.peerSig && !s.peerAbAt.IsZero() && at.Sub(s.peerAbAt) <= PeerOrphanHold {
			s.peerOrphan = true
			peerStat.orphanHolds.Add(1)
		}
	case evPeerAbort:
		s.peerOrphan = false
		s.peerSig = true
		n := ev.n
		if n > 0 {
			peerStat.reports.Add(1)
			peerStat.repAborts.Add(n)
		} else {
			peerStat.edgeSigs.Add(1)
		}
		for n > 0 && len(s.held) > 0 { // resets that came before the report
			s.held = s.held[1:]
			c.peerAb++
			peerStat.credited.Add(1)
			n--
		}
		if len(s.held) == 0 {
			s.held = nil
		}
		at := time.Unix(0, ev.at)
		if ev.n > 0 {
			s.peerAbAt = at
		}
		if at.Sub(s.peerCreditAt) > peerWin {
			s.peerCredit = 0
		}
		s.peerCredit += n
		s.peerCreditAt = at
	case evDeath:
		if e.side == SideKharej && s.peerSig && ev.d.Class == DeathRST {
			at := time.Unix(0, ev.at)
			if s.peerCredit > 0 && at.Sub(s.peerCreditAt) <= peerWin {
				s.peerCredit--
				c.peerAb++
				peerStat.credited.Add(1)
				return
			}
			if len(s.held) < heldCap {
				s.held = append(s.held, ev)
				peerStat.held.Add(1)
				return
			}
		}
		e.death(s, c, ev, bit, false)
	case evPath:
		p := ev.p
		s.pin.add(p)
		c.samples++
		c.lossSum += p.Loss
		c.qSum += p.QueueMs
		c.sx += p.Loss
		c.sy += p.QueueMs
		c.sxx += p.Loss * p.Loss
		c.syy += p.QueueMs * p.QueueMs
		c.sxy += p.Loss * p.QueueMs
		c.rateSum += p.RateBps
		if p.PeerLoss >= 0 {
			c.peerLossSum += p.PeerLoss
			c.peerLossN++
		}
		c.busySum += float64(p.BusyLinks)
		if p.PathLimited >= 0 && p.BusyLinks > 0 {
			c.limSum += p.PathLimited
			c.limN++
		}
		if p.Silent {
			s.markDead(time.Unix(0, ev.at))
		} else if p.RateBps > 0 {
			s.dead = false // bytes acked: two-way delivery
			c.moving++
		}
		if p.Silent {
			c.silent++
			c.khBad |= bit
		} else if p.Loss >= 0.03 {
			c.lossy++
			c.khBad |= bit
		}
		if p.QueueMs >= 100 {
			c.highQ++
		}
	}
}

// death is the evDeath part of ingest (b414: also run for a held reset
// that no abort report claimed within peerHold, released true). A released
// reset is judged at its own time, not the release tick's: it is a kill
// after recovery (R1) only if it came after the episode's first success,
// and it joins the episode's kill statistics only if it came after the
// episode opened (a reset at or before the opening tick is part of the
// evidence that opened it, as when it is not held).
func (e *evaluator) death(s *scope, c *ctr, ev event, bit uint64, released bool) {
	d := ev.d
	evAt := time.Unix(0, ev.at)
	if d.Class < nDeathClasses {
		c.death[d.Class]++
	}
	if d.Class == DeathLocal || d.Class == DeathEOF || d.Class == DeathAbort {
		return // our own close, a clean peer close, or killed on this host
	}
	life := time.Duration(d.LifeMs) * time.Millisecond
	fresh := life < freshLife
	if fresh {
		c.fresh++
	}
	if d.Class == DeathRST && d.HaveTCP && d.MinRTTUs > 0 {
		rho := float64(d.LastSentMs) * 1000 / float64(d.MinRTTUs)
		lim := 0.8
		if e.side == SideIran {
			lim = 0.5
		}
		if rho < lim {
			c.rstFast++
		}
	}
	if d.Class == DeathRST && !e.massAt.IsZero() && d.HaveTCP {
		// slot 9 (p): an old link of the dead process: born before the
		// burst and nothing received on it since (the peer process is
		// gone). An injected RST on a live old link follows data from
		// the live peer, so its last receive is recent.
		if evAt.Add(-life).Before(e.massAt) && evAt.Add(-time.Duration(d.LastRecvMs)*time.Millisecond).Before(e.massAt) {
			c.rstPreBurst++
		}
	}
	if d.Class == DeathRST || d.Class == DeathTimeout {
		if fresh {
			c.freshKill++
		}
		postOK := s.okAfterKill
		if released {
			postOK = postOK && evAt.After(s.okAt)
		}
		if postOK {
			c.killPostOK++
		}
		if ep := s.ep; ep != nil && (!released || evAt.After(ep.born())) {
			ep.killDeaths++
			if life < youngLife {
				ep.killYoung++
				if d.HaveTCP && d.Bytes <= hsBytes {
					ep.killHS++
				} else if d.HaveTCP {
					ep.killBig++
				}
			} else if fresh {
				ep.killMid++
			}
			if fresh {
				lb := math.Log(float64(max(d.Bytes, 1)))
				ll := math.Log(float64(max(d.LifeMs, 1)))
				ep.fkN++
				ep.fkLb += lb
				ep.fkLb2 += lb * lb
				ep.fkLl += ll
				ep.fkLl2 += ll * ll
			}
			ep.bytesHist[log2b(d.Bytes, 24)]++
			ep.lifeHist[log2b(uint64(d.LifeMs/1000), 20)]++
			if evAt.Add(-life).After(ep.born()) {
				ep.bornKills++
				ep.bornLife[log2b(uint64(d.LifeMs/1000), 20)]++
			}
		}
	}
	c.khBad |= bit
}

// flushHeld (b414) counts the held resets that no abort report claimed
// within peerHold as ordinary resets, in the current bucket.
func (e *evaluator) flushHeld(now time.Time) {
	for _, s := range e.scopes {
		s.released = false
		n := 0
		hold := peerHold
		if s.peerOrphan {
			hold = PeerOrphanHold
		}
		for n < len(s.held) && now.Sub(time.Unix(0, s.held[n].at)) >= hold {
			ev := s.held[n]
			_, kh := e.ids(ev.local, ev.remote)
			c := &s.buckets[s.cur]
			bit := e.khBit(kh)
			c.khSeen |= bit
			e.death(s, c, ev, bit, true)
			n++
		}
		if n > 0 {
			peerStat.released.Add(uint64(n))
			s.released = true
			s.held = s.held[n:]
			if len(s.held) == 0 {
				s.held = nil
			}
		}
	}
}

// born (b414) is the time links must be born after to count as the
// episode's redial wave: its open time, or one tick earlier when held
// resets opened it (bornFrom).
func (ep *episode) born() time.Time {
	if !ep.bornFrom.IsZero() {
		return ep.bornFrom
	}
	return ep.opened
}

// noteOK (R1): a redial or accept succeeded while an episode is open; the
// kills from now on are kills of recovered links. The kills that opened the
// episode (a restart's burst, with its refill often in the same tick) are
// before it and never count. at is the success's time (b414: a held reset
// released later is compared with it).
func (s *scope) noteOK(at time.Time) {
	if s.ep != nil && !s.okAfterKill {
		s.okAfterKill, s.okAt = true, at
	}
}

func (s *scope) markDead(at time.Time) {
	if !s.dead {
		s.dead, s.deadSince = true, at
	}
}

// massStep tracks the mass-RST peer-restart candidate (slot 4). A peer
// restart under l3mtcp resets every link at once, so the RSTs look injected
// (fast) and the old rstFast == 0 signature misses it. Real RST injection
// is told apart by timing and scope: it hits one IP (or a few), not every
// known Iran IP inside massSpan, and it keeps resetting the redials.
//
// slot 6 (i): killing the peer process closes every link at once, but the
// kernel sends FIN on links with nothing unread and RST only on the rest,
// so one IP can show 4 RSTs and 12 EOFs. The burst therefore counts the
// peer's deaths, RST and EOF together (≥ massPerIP per IP), and needs at
// least one RST in the burst: a graceful retire (FIN only) never resets
// a link, so pool retires on every IP cannot pass as a restart. The rest
// is unchanged: no resets flowing before (clean start), every known IP in
// the burst, a redial or accept on every IP after, and at most
// massPostMax RSTs after the burst (injection keeps resetting redials).
func peerDeaths(c *ctr) int { return int(c.death[DeathRST] + c.death[DeathEOF]) }

// massDbg logs why the mass rule dropped a candidate (slot 8, n). Lab
// diagnostics only: off unless HS2_CENSOR_DEBUG=1, at most one line per
// candidate (one per massSpan at the most).
func (e *evaluator) massDbg(reason string, now time.Time) {
	if !e.debug || e.logf == nil {
		return
	}
	b := make([]string, 0, len(e.scopes))
	for _, s := range e.scopes {
		b = append(b, fmt.Sprintf("%s:%d/rst%d/ok%v", s.ip, s.massRst, s.massRstB, s.massOK))
	}
	sort.Strings(b)
	e.logf("censor351: dbg mass reject reason=%s el=%s post=%d rstseen=%d ips=%s", reason,
		now.Sub(e.massAt).Round(time.Second), e.massPost, e.massRstSeen, strings.Join(b, ","))
}

// massTick adds one tick of a scope to the burst and notes a redial or
// accept. slot 8 (n): the arming tick counts too. A killed peer comes back
// in about 2 s and refills its pool at once, so all the redials often land
// in the same 5 s tick as the deaths; ignoring that tick lost every
// redial, and the confirm then failed for "no redial".
func (s *scope) massTick(c *ctr) {
	s.massRst += peerDeaths(c)
	s.massRstB += c.death[DeathRST]
	s.massFastB += c.rstFast
	s.massFreshB += c.freshKill
	if c.dialOK > 0 || c.acc[AcceptOK] > 0 {
		s.massOK = true
	}
}

func (e *evaluator) massStep(now time.Time) {
	nS := len(e.scopes)
	if nS < 2 {
		e.massAt = time.Time{}
		return
	}
	rstNow, hitNow := 0, 0
	for _, s := range e.scopes {
		c := &s.buckets[s.cur]
		rstNow += int(c.death[DeathRST])
		if peerDeaths(c) > 0 {
			hitNow++
		}
	}
	start := func() {
		e.massAt, e.massPost, e.massRstSeen = time.Time{}, 0, 0
		if hitNow == 0 {
			return
		}
		// A restart is sudden: RSTs already flowing in the trigger window
		// before this tick mean ongoing resets (injection), not a burst.
		pre := 0
		for _, s := range e.scopes {
			for i := 1; i < winTicks; i++ {
				pre += int(s.buckets[(s.cur-i+ringTicks)%ringTicks].death[DeathRST])
			}
		}
		if pre > massPostMax {
			if e.debug && e.logf != nil {
				e.logf("censor351: dbg mass skip reason=pre-rst pre=%d", pre)
			}
			return
		}
		e.massAt, e.massRstSeen = now, rstNow
		for _, s := range e.scopes {
			s.massRst, s.massOK = 0, false
			s.massRstB, s.massFastB, s.massFreshB = 0, 0, 0
			s.massTick(&s.buckets[s.cur])
		}
	}
	if e.massAt.IsZero() {
		start()
		return
	}
	el := now.Sub(e.massAt)
	if el < massSpan {
		for _, s := range e.scopes {
			c := &s.buckets[s.cur]
			s.massTick(c)
			e.massRstSeen += int(c.death[DeathRST])
		}
		return
	}
	lo, hiRst, top := -1, 0, 0
	for _, s := range e.scopes {
		top = max(top, s.massRst)
	}
	for _, s := range e.scopes {
		if s.massRst < massPerIP || e.massRstSeen == 0 {
			if top >= 4*massPerIP {
				e.massDbg("small-burst", now) // a near miss: some IP lost several links
			}
			start() // not every IP, or FIN only (retires): no mass burst (may be the start of the next one)
			return
		}
		if lo < 0 || s.massRst < lo {
			lo = s.massRst
		}
		hiRst = max(hiRst, int(s.massRstB))
	}
	// slot 11 (x): the balance is judged on the resets. A dying peer
	// closes its links with FIN (nothing unread) or RST, an injector only
	// resets, so an injection's excess on one IP is RSTs. How many links
	// each IP had is the LB's choice and moves after trouble: res9 run a,
	// a restart 40 s after an rst event on .1 (the LB had moved the links
	// to .3) killed .1 6 (3 RST) and .3 34 (8 RST + 26 FIN), rejected as
	// unbalanced (34 > 4 x 6), and the burst on .3 was read as rst-inject
	// high. So the largest IP's burst RSTs, not all its deaths, must stay
	// within massBalance x the smallest IP's deaths.
	if hiRst > lo*massBalance {
		e.massDbg("unbalanced", now)
		start() // slot 7 (m): one IP carries the burst (injection), the rest is background churn
		return
	}
	// slot 9 (p): an RST on a link born before the burst is a leftover of
	// the dead process (the peer kernel answers our next send on an old
	// link with an RST, at any time after the restart). Only RSTs on links
	// born after the burst, the redials, show resets going on; an injector
	// that keeps resetting must reset the redials too.
	postNow := rstNow // the redials' RSTs (leftovers taken out below)
	for _, s := range e.scopes {
		postNow -= int(s.buckets[s.cur].rstPreBurst)
	}
	e.massPost += max(postNow, 0)
	if e.massPost > massPostMax {
		e.massDbg("post-rst", now)
		start() // the resets go on (injection)
		return
	}
	all := true
	for _, s := range e.scopes {
		c := &s.buckets[s.cur]
		if c.dialOK > 0 || c.acc[AcceptOK] > 0 {
			s.massOK = true
		}
		all = all && s.massOK
	}
	// slot 6 (i): the post window is the whole massConfirm, as the rule
	// says (≤ massPostMax RSTs within it). Confirming on the first tick
	// with a redial looked at one tick only, so an injection that resets
	// one or two links a tick passed it. The peer grace still starts at
	// the burst (massAt).
	if el < massSpan+massConfirm {
		return
	}
	if !all {
		e.massDbg("no-redial", now)
		start() // no redial came back in time
		return
	}
	// Confirmed: the peer restarted. Its RSTs are explained, so they leave
	// the open episodes' rst/kill evidence (later RSTs count again).
	e.peerRestart = e.massAt
	e.peerZDU = false
	e.nMass++
	for _, s := range e.scopes {
		ep := s.ep
		if ep == nil {
			continue
		}
		// slot 8 (o): the restart explains the burst's RSTs, not the
		// episode's whole history. An episode open since before the burst
		// (an injection that ended a minute earlier, then a burst on every
		// IP) keeps its earlier RST evidence; res6 run d wiped 301 RSTs of
		// a .1 injection for a 16-link burst and read it as peer-restart.
		if ep.opened.Before(e.massAt.Add(-tick)) {
			ep.peerMass += int(min(s.massRstB, ep.sum.death[DeathRST]))
			ep.sum.death[DeathRST] -= min(s.massRstB, ep.sum.death[DeathRST])
			ep.sum.rstFast -= min(s.massFastB, ep.sum.rstFast)
			ep.sum.freshKill -= min(s.massFreshB, ep.sum.freshKill)
			ep.killDeaths -= min(s.massRstB, ep.killDeaths)
			continue
		}
		ep.peerMass += int(ep.sum.death[DeathRST])
		ep.sum.death[DeathRST], ep.sum.rstFast, ep.sum.freshKill = 0, 0, 0
		ep.killDeaths, ep.killYoung, ep.killMid, ep.killHS, ep.killBig = 0, 0, 0, 0, 0
		ep.bytesHist, ep.lifeHist = [24]uint32{}, [20]uint32{}
		ep.fkN, ep.fkLb, ep.fkLb2, ep.fkLl, ep.fkLl2 = 0, 0, 0, 0, 0
	}
	e.massAt, e.massPost, e.massRstSeen = time.Time{}, 0, 0
}

// oldStep (slot 5, e): the old peer-restart signature, ≥ 80 % of the scopes
// (≥ 2) losing ≥ massPerIP links in one tick with no fast RST, sets the peer
// grace only once the redials prove the peer came back: every scope has a
// successful dial or accept and the path stays clean (≤ massPostMax
// timeouts, unreachables or silent readings) for oldObserve after the drop.
// Bufferbloat, loss and outages also drop links on every IP at once, but
// their trouble goes on after the drop (more timeouts, silent readings).
func (e *evaluator) oldStep(now time.Time) {
	hit, total := 0, 0
	for _, s := range e.scopes {
		total++
		c := &s.buckets[s.cur]
		if c.death[DeathRST]+c.death[DeathEOF]+c.death[DeathTimeout] >= massPerIP && c.rstFast == 0 {
			hit++
		}
	}
	sig := total >= 2 && hit*5 >= total*4
	start := func() {
		e.oldAt, e.oldPost = time.Time{}, 0
		if !sig {
			return
		}
		// A restart is sudden: path trouble already in the trigger window
		// before this tick means the path is dropping the links (the end of
		// a bufferbloat or loss stretch), as massStep requires a clean start.
		pre := 0
		for _, s := range e.scopes {
			for i := 1; i < winTicks; i++ {
				pre += pathTrouble(&s.buckets[(s.cur-i+ringTicks)%ringTicks])
			}
		}
		if pre > massPostMax {
			return
		}
		e.oldAt = now
		for _, s := range e.scopes {
			s.oldOK = false
		}
	}
	if e.oldAt.IsZero() {
		start()
		return
	}
	all := true
	for _, s := range e.scopes {
		c := &s.buckets[s.cur]
		if c.dialOK > 0 || c.acc[AcceptOK] > 0 {
			s.oldOK = true
		}
		all = all && s.oldOK
		e.oldPost += pathTrouble(c)
	}
	if e.oldPost > massPostMax {
		start() // the path trouble goes on: not a restart (may be a new drop)
		return
	}
	if now.Sub(e.oldAt) < oldObserve {
		return
	}
	if all {
		e.peerRestart = e.oldAt
		e.peerZDU = false
		e.nOld++
	}
	e.oldAt, e.oldPost = time.Time{}, 0
}

// noteGen (b437 Q2) takes a ZDU generation event. The peer's generation
// change is handled like a peer restart (e.peerRestart: episodes it opens
// get the peer grace); a link of the peer's older process that ended is
// not counted as a death (its process closed it at its drain end, or
// reclaimed it idle). Our own handover's end ends the own-restart grace.
func (e *evaluator) noteGen(ev event) {
	at := time.Unix(0, ev.at)
	switch ev.stage {
	case genOldEnd, genPeerNew:
		if ev.stage == genOldEnd {
			e.nGenOld++
		} else {
			e.nGenNew++
		}
		// one grace per change: the first event after a quiet peer grace
		// opens it; the rest of the burst (120 closes over a few seconds)
		// must not move it past the episode the burst opened.
		if e.genAt.IsZero() || at.Sub(e.genAt) >= peerGrace {
			if at.After(e.peerRestart) {
				e.peerRestart, e.peerZDU = at, true
			}
		}
		e.genAt = at
	case genOwnTakeover:
		e.ownZDU = true
	case genOwnDone:
		e.ownZDU = true
		if end := at.Add(tick); e.ownUntil.IsZero() && end.Before(e.start.Add(ownGrace)) || end.Before(e.ownUntil) {
			e.ownUntil = end
		}
	}
}

func (e *evaluator) noteUsers(total uint64, at time.Time) {
	if !e.usersLastAt.IsZero() && total >= e.usersLast {
		dt := at.Sub(e.usersLastAt).Seconds()
		if dt > 0 {
			r := float64(total-e.usersLast) / dt
			e.usersRate = r
			if !e.usersHave {
				e.usersBase, e.usersHave = r, true
			} else {
				a := dt / 3600 // 1 h EWMA
				if a > 1 {
					a = 1
				}
				e.usersBase += a * (r - e.usersBase)
			}
		}
	}
	e.usersLast, e.usersLastAt = total, at
}

// usersRatio: users arriving now vs the baseline; -1 unknown.
func (e *evaluator) usersRatio() float64 {
	if !e.usersHave || e.usersBase < 0.05 {
		return -1
	}
	return e.usersRate / e.usersBase
}

func (s *scope) window(n int) ctr {
	var w ctr
	for i := 0; i < n; i++ {
		w.add(&s.buckets[(s.cur-i+ringTicks)%ringTicks])
	}
	return w
}

func triggered(w *ctr) bool {
	switch {
	case w.silent > 0, w.lossy >= 2:
		return true
	case w.dials >= 3 && (w.dials-w.dialOK)*2 >= w.dials:
		return true
	case w.unexplained() >= 3:
		return true
	case w.accFails() >= 3:
		return true
	case w.highQ >= 4: // bufferbloat / shaper queue
		return true
	case w.peerLossN >= 2 && w.peerLossSum/float64(w.peerLossN) >= 0.03: // the other direction's loss
		return true
	}
	return false
}

// triggeredScope adds the scope's throttle trigger: the window's mean path
// rate under half the hour's baseline (a baseline from an earlier day only).
func triggeredScope(s *scope, w *ctr, now time.Time) bool {
	if triggered(w) {
		return true
	}
	if base := s.baseline(now); base > 0 && w.samples >= 3 && w.rateSum > 0 {
		return w.rateSum/float64(w.samples) < 0.5*base
	}
	return false
}

func inXrayWindow(t time.Time) bool {
	t = t.UTC()
	s := t.Minute()*60 + t.Second()
	return s >= 30 && s <= 150
}

// step runs every tick: triggers, episode open/update/close, rotation.
func (e *evaluator) step(now time.Time) {
	e.evals++
	e.flushHeld(now)
	e.oldStep(now)
	e.massStep(now)
	open := 0
	for _, s := range e.scopes {
		if s.ep != nil {
			open++
		}
	}
	ips := make([]netip.Addr, 0, len(e.scopes))
	for ip := range e.scopes {
		ips = append(ips, ip)
	}
	sort.Slice(ips, func(i, j int) bool { return ips[i].Less(ips[j]) })
	for _, ip := range ips {
		s := e.scopes[ip]
		clean := s.tickQueue()
		w := s.window(winTicks)
		cur := s.buckets[s.cur]
		e.hourBaseline(s, &cur, now)
		trig := triggeredScope(s, &w, now)
		// slot 4: the path is still cut as the hooks show it: hold the quiet
		// close (the LB cut and backed off, so evidence stops arriving).
		held := !trig && s.ep != nil && s.dead && now.Sub(s.ep.opened) < cutHoldMax
		if held {
			s.ep.last = now
			s.ep.cutHeld = true
			s.ep.sum.add(&cur)
		}
		if trig {
			if s.ep == nil && open < maxEpisodes {
				s.seq++
				s.ep = &episode{id: fmt.Sprintf("%s#%d", ip, s.seq), scope: ip, opened: now, usersAtOpen: e.usersRatio(), class: ClUnknown}
				s.ep.sum = w // the evidence that opened it
				if s.released {
					// b414: resets of the last tick, released this
					// tick, helped open it: they would have opened it
					// one tick earlier without the hold.
					s.ep.bornFrom = now.Add(-tick)
				}
				open++
			}
			if s.ep != nil {
				s.ep.last = now
			}
		}
		if s.ep == nil {
			s.okAfterKill, s.okAt = false, time.Time{}
		}
		if ep := s.ep; ep != nil {
			if ep.killDeaths > ep.killPrev {
				ep.killTicks++ // (z): ticks with kills (spread in time)
				ep.killPrev = ep.killDeaths
			}
			if trig && !ep.opened.Equal(now) {
				// evidence counts while the trigger holds, not the clean tail
				switch {
				case !clean:
					ep.sawQueue = true
					ep.unpend()
					ep.sum.add(&cur)
					// the kills so far are the queue part's (the tick's
					// deaths are ingested before its step)
					ep.pendKill = ep.killStats
				case ep.sawQueue:
					// slot 10 (u): clean ticks after a standing queue
					ep.pending = true
					ep.pend.add(&cur)
					if ep.pend.rstNoQ >= splitRST {
						s.ep = e.split(s, ep, now) // closes one, opens one
					}
				default:
					ep.sum.add(&cur)
				}
			}
			if inXrayWindow(now) && e.side == SideKharej {
				ep.xray = true
			}
			switch {
			case e.ownUntil.IsZero() && now.Sub(e.start) < ownGrace || now.Before(e.ownUntil):
				ep.grace = "own"
			case !e.peerRestart.IsZero() && now.Sub(e.peerRestart) < peerGrace && !ep.opened.Before(e.peerRestart.Add(-tick)):
				// slot 8 (o): only an episode the restart opened. One open
				// since before it had trouble the restart cannot explain;
				// it is judged on what is left once the burst is taken out.
				ep.grace = "peer"
			}
		}
	}
	// cross-IP synchrony (M11): all known Iran IPs in trouble together.
	nOpen, isps := 0, map[string]bool{}
	for _, s := range e.scopes {
		if s.ep != nil && now.Sub(s.ep.last) < closeAfter {
			nOpen++
			if isp := e.ispMap[s.ip]; isp != "" {
				isps[isp] = true
			}
		}
	}
	for _, ip := range ips {
		s := e.scopes[ip]
		ep := s.ep
		if ep == nil {
			continue
		}
		if len(e.scopes) >= 2 && nOpen == len(e.scopes) {
			ep.allIPs = true
		}
		if len(isps) > ep.ispsTogether {
			ep.ispsTogether = len(isps)
		}
		e.classify(ep, s, now)
		switch {
		case now.Sub(ep.last) >= closeAfter:
			if ep.pending {
				ep.unpend()
				e.classify(ep, s, now)
			}
			e.emit(ep, "close", now)
			e.nEpisodes[clIdx(ep.class)][ep.conf]++
			s.ep = nil
		case !ep.logged && now.Sub(ep.opened) >= openLogAt:
			ep.logged = true
			ep.lastUpd = now
			e.emit(ep, "open", now)
		}
	}
	for _, s := range e.scopes {
		s.cur = (s.cur + 1) % ringTicks
		s.buckets[s.cur] = ctr{}
	}
	setStatus(e)
}

func clIdx(c int) int {
	if c < 0 {
		return NClasses
	}
	if c == ClAnomaly {
		return NClasses + 1
	}
	return c
}

// hourBaseline keeps the per-hour mean path rate (M7, in memory). A baseline
// counts only once it was written on an earlier day than today.
func (e *evaluator) hourBaseline(s *scope, c *ctr, now time.Time) {
	h := now.UTC().Hour()
	if h != s.hourAt {
		if s.hourN > 0 && s.ep == nil {
			v := s.hourAcc / float64(s.hourN)
			if s.rateHr[s.hourAt] == 0 {
				s.rateHr[s.hourAt] = v
			} else {
				s.rateHr[s.hourAt] = 0.7*s.rateHr[s.hourAt] + 0.3*v
			}
			s.rateDay[s.hourAt] = dayOf(now) + 1 // +1: 0 means never
		}
		s.hourAcc, s.hourN, s.hourAt = 0, 0, h
	}
	if c.samples > 0 && c.rateSum > 0 {
		s.hourAcc += c.rateSum / float64(c.samples)
		s.hourN++
	}
}

func dayOf(t time.Time) int { return int(t.Unix() / 86400) }

// baseline returns the hour's baseline rate, or 0 when there is none from an
// earlier day.
func (s *scope) baseline(now time.Time) float64 {
	h := now.UTC().Hour()
	if s.rateDay[h] == 0 || s.rateDay[h]-1 >= dayOf(now) {
		return 0
	}
	return s.rateHr[h]
}

// histMode: share of the top bucket ± 1 neighbour among n entries.
func histMode(h []uint32) (int, float64) {
	var n uint32
	for _, v := range h {
		n += v
	}
	if n == 0 {
		return -1, 0
	}
	best, bi := uint32(0), -1
	for i := range h {
		s := h[i]
		if i > 0 {
			s += h[i-1]
		}
		if i+1 < len(h) {
			s += h[i+1]
		}
		if s > best {
			best, bi = s, i
		}
	}
	return bi, float64(best) / float64(n)
}

type score struct {
	nec  bool
	disc int
	high bool
}

// unpend (slot 10, u) folds the held clean ticks back into the episode
// (the queue came back, or the episode closes): one kind of evidence.
func (ep *episode) unpend() {
	if ep.pending {
		ep.sum.add(&ep.pend)
		ep.pend, ep.pending = ctr{}, false
	}
}

// split (slot 10, u): an episode whose evidence changed kind, a standing
// queue and then clean-path RSTs (res8 runs c and d: a bufferbloat or
// congestion episode stayed open into an rst injection, and the injection
// was credited to the queue's event), closes on the queue's evidence; the
// clean ticks and their kills open a new episode.
func (e *evaluator) split(s *scope, ep *episode, now time.Time) *episode {
	nk := ep.killStats.minus(ep.pendKill)
	ep.killStats = ep.pendKill
	pend := ep.pend
	ep.pend, ep.pending = ctr{}, false
	e.classify(ep, s, now)
	e.emit(ep, "close", now)
	e.nEpisodes[clIdx(ep.class)][ep.conf]++
	s.seq++
	n := &episode{id: fmt.Sprintf("%s#%d", s.ip, s.seq), scope: s.ip, opened: now, last: now, usersAtOpen: e.usersRatio(), class: ClUnknown}
	n.sum = pend
	n.killStats = nk
	n.grace = ep.grace
	return n
}

// classify applies the censor351 §3.3 rules to the episode's evidence.
func (e *evaluator) classify(ep *episode, s *scope, now time.Time) {
	c := &ep.sum
	dur := now.Sub(ep.opened)
	users := ep.usersAtOpen
	if r := e.usersRatio(); r >= 0 && (users < 0 || r < users) {
		users = r
	}
	nKhSeen := bits.OnesCount64(c.khSeen)
	nKhBad := bits.OnesCount64(c.khBad & c.khSeen)
	otherClean := e.othersClean(s.ip)
	var sc [NClasses]score

	// C1 rst-inject
	rst, rstFast := rstUnexplained(c)
	// slot 9 (s): the delayed-kill age signature. Its redials outlive the
	// young window and then die (mid-age kills), and no link dies young.
	// An at-once or after-N-bytes reset kills busy redials within seconds.
	dkSig := ep.killMid >= 3 && ep.killYoung*4 <= ep.killMid
	// slot 11 (w): the after-N-bytes signature. The young kills die past
	// the handshake bytes: a byte trigger on busy links (res9: connbytes
	// 20000 kills in 1-2 s, read as rst-inject 6/7 on both sides). An
	// immediate reset kills within the handshake bytes.
	// The byte count is the trigger only when the kills cluster on it (C5's
	// 0.6 share); young kills at scattered byte counts are not a byte
	// trigger and leave rst-inject as it was.
	_, fpShare := histMode(ep.bytesHist[:])
	fpSig := ep.killBig >= 3 && ep.killHS*4 <= ep.killBig && fpShare >= 0.6
	if rst >= 3 {
		x := &sc[ClRSTInject]
		x.nec = true
		if rstFast*3 >= rst*2 {
			x.disc++
		}
		if otherClean {
			x.disc++
		}
		if ep.killHS >= 2 && rstFast > 0 {
			x.disc++ // repeats on young links within the handshake, with injection timing
		}
		x.high = e.side == SideIran && rstFast >= 5
		if dkSig || fpSig {
			// the resets are the delayed kill's (age) or the fingerprint
			// kill's (bytes) mechanism, not an immediate reset (C9, C5).
			x.disc, x.high = min(x.disc, 1), false
		}
	}
	// C2 handshake-block
	{
		x := &sc[ClHandshakeBlock]
		connOK := c.dials - c.connFails()
		// slot 5: a standing queue (O6 signature) explains hellos that time
		// out; only resets and EOFs then count as a blocked handshake.
		hs, af := c.hsFails(), c.accFails()
		if queued(c) {
			hs, af = c.stage[DialHelloReset]+c.stage[DialHelloEOF], c.acc[AcceptReset]+c.acc[AcceptEOF]
		}
		if c.dials >= 4 && connOK*5 >= c.dials*4 && hs*2 >= c.dials {
			x.nec = true
		}
		if af >= 4 && af*2 >= af+c.acc[AcceptOK] {
			x.nec = true
			x.disc++ // M2 sees the hello dropped / reset
		}
		if x.nec {
			if c.silent == 0 {
				x.disc++ // established links keep acking
			}
			if (c.dialR > 0 && c.dialT > 0) && ((c.failR == 0) != (c.failT == 0)) {
				x.disc++ // one front ok
			}
			if otherClean {
				x.disc++
			}
			x.high = c.hsFails() >= 4 && c.accFails() >= 4 && dur >= 2*time.Minute
		}
		// b414 v6 (held-out #6): one reset injector, not two censors. A
		// handshake-block censor drops or blackholes the ClientHello /
		// ServerHello; it does not also kill links that are already
		// established. When most failed handshakes died by a reset (not a
		// timeout) and established links on the same IP died by resets with
		// injection timing in the same episode (C1 necessity and a fast
		// one), the handshake resets are the same injector hitting the
		// redials in their TLS stage: on an IP with few live links they can
		// outnumber the link resets, and rst-inject is the verdict. A real
		// handshake-block keeps its verdict: its handshakes time out (or
		// its established links do not die by fast reset). Not inside our
		// own or the peer's restart grace: a restart resets the links and
		// the handshakes in flight, the ordinary explanation (replays #3,
		// #4, #5: own-restart high would turn rst-inject low).
		if x.nec && ep.grace == "" && hsResetInject(c, rst, rstFast) {
			x.disc, x.high = min(x.disc, 1), false
			sc[ClRSTInject].disc++
		}
	}
	// C3 blackhole
	to := c.stage[DialConnTimeout]
	if ep.allIPs {
		// every Iran IP at once: intl-cut / whitelist below, not one IP's blackhole
	} else if c.dials >= 4 && to*10 >= c.dials*9 && (nKhBad >= 2 || c.silent > 0) {
		x := &sc[ClBlackhole]
		x.nec = true
		if c.stage[DialConnUnreach] == 0 && c.death[DeathUnreach] == 0 {
			x.disc++
		}
		if dur > 10*time.Minute {
			x.disc++
		}
		if users >= 0.5 {
			x.disc++
		}
		x.high = dur > 30*time.Minute && users >= 0.5
	} else if c.silent >= 2 && nKhSeen >= 2 && nKhBad >= 2 && c.dials == 0 {
		// Iran side: no dials of its own; every Kharej IP silent on this IP.
		x := &sc[ClBlackhole]
		x.nec = true
		if dur > 10*time.Minute {
			x.disc++
		}
		if users >= 0.5 {
			x.disc++
		}
		x.high = dur > 30*time.Minute && users >= 0.5
	}
	// C4 port-block: needs the canary port (M12, D5) — never in level 1.
	// C5 fingerprint-kill / C9 delayed-kill
	// slot 6 (j): under a standing queue (queued) long-lived links die by
	// RST after the queue stalls them, and their lifetimes cluster at the
	// queue's onset. An injected kill is fast relative to the RTT (the RST
	// arrives right after our send); with no fast RST the queue explains
	// the deaths, so neither kill class is necessary.
	// slot 9 (q): under a standing queue only the clean ticks' RSTs count.
	kills := ep.killDeaths >= 8 && !queueExplainsRST(c)
	if queueExplainsRST(c) {
		kills = rst >= 8
	}
	if kills || (dkSig && rst >= 3) {
		_, bshare := histMode(ep.bytesHist[:])
		lmode, lshare := histMode(ep.lifeHist[:])
		// slot 9 (s): a lifetime cluster below youngLife is the at-once
		// signature (busy redials reset within seconds), not an age T.
		if lmode < log2b(uint64(youngLife/time.Second), 20)+1 {
			lshare = 0
		}
		// slot 10 (t): which trigger the fresh kills cluster on, bytes or
		// age, from the spread of ln(bytes) and ln(age) at death (the log2
		// bins tie when both fall in one bin: res8 run b, 98 % / 98 %).
		tight := killTighter(ep)
		byBytes := tight > 0 || (tight == 0 && bshare >= lshare)
		byAge := tight < 0 || (tight == 0 && lshare > bshare)
		if fpSig && !dkSig {
			// slot 11 (w): young kills past the handshake are no age
			// trigger (a delayed kill never kills young), so their ages say
			// nothing; the byte count is the trigger, at any age.
			byBytes, byAge = true, false
		}
		if c.freshKill >= 8 {
			x := &sc[ClFingerprintKill]
			x.nec = true
			if bshare >= 0.6 && byBytes {
				x.disc++ // bytes cluster at N, tighter than the lifetimes
			}
			if (c.dialR > 0 && c.dialT > 0) && ((c.failR == 0) != (c.failT == 0)) {
				x.disc++
			}
			if bshare >= 0.6 && c.freshKill >= 12 && byBytes {
				x.disc++ // repeats on new links at the same N
			}
			x.high = bshare >= 0.7 && dur >= 2*time.Hour && byBytes
			if dkSig && !byBytes {
				// slot 10 (t): the redials outlive the young window and die
				// at one age, not at one byte count: the delayed kill's
				// signature, as for rst-inject in slot 9 (s).
				x.disc, x.high = min(x.disc, 1), false
			}
		}
		x := &sc[ClDelayedKill]
		x.nec = true
		if lshare >= 0.6 && byAge {
			x.disc++ // lifetimes cluster at T, tighter than the bytes
		}
		if dkSig {
			x.disc++ // slot 9 (s): redials live past youngLife, then die
		}
		if c.silent == 0 {
			x.disc++ // not a dead path
		}
		x.high = lshare >= 0.7 && dur >= 2*time.Hour
	}
	// C6 throttle (needs a baseline from an earlier day)
	if base := s.baseline(now); base > 0 && c.samples > 0 {
		rate := c.rateSum / float64(c.samples)
		if rate < 0.5*base {
			x := &sc[ClThrottle]
			x.nec = true
			if c.meanLoss() < 0.01 {
				x.disc++
			}
			if c.corr() < 0.3 {
				x.disc++
			}
			if otherClean {
				x.disc++
			}
			// The path rate is acked bytes/s, which also falls when users ask
			// for less. Only busy links that spend most of their time
			// network-limited (M7) tell a ceiling from low demand; without
			// that, throttle stays capped at low (never a med+ verdict).
			if c.ceilingLimited() {
				x.disc++
			} else if x.disc > 1 {
				x.disc = 1
			}
		}
	}
	// C7 intl-cut / whitelist
	// slot 5 (g): necessity is a real cut on this IP (cutSig: the path
	// carries nothing), not just some silence or failed handshakes while the
	// path still moves data, and no queue that explains the failures. The
	// old "no refused dial" discriminator restated the necessity (always
	// true), so it is now "every Iran IP's path is dead now" (hook state).
	if ep.allIPs && c.death[DeathRST] == 0 && c.stage[DialConnRefused] == 0 && cutSig(c) && !queued(c) {
		x := &sc[ClIntlCut]
		x.nec = true
		if users >= 0.5 {
			x.disc++
		}
		if ep.ispsTogether >= 2 {
			x.disc++
		}
		if e.allDead() {
			x.disc++
		}
		x.high = users >= 0.5 && len(e.scopes) >= 2
	}
	// C8 udp-diff: TCP-only production, no datagram path is hooked.
	// O1 congestion
	// slot 13: loss with silent readings and no queue is an outage (nothing
	// acked, every resend counts as loss), not congestion: a congested path
	// drops from a full queue. Recording #1 (snapshot): all 26 blackhole and
	// one-way blackhole episodes read congestion (Iran med) with silent 2-8,
	// q 0-15 ms, corr -0.6..-1; every congestion label had silent 0 and
	// q >= 173 ms. Same exclusion as O7 asym-loss.
	if c.meanLoss() >= 0.015 && (c.silent == 0 || queued(c)) {
		x := &sc[ClCongestion]
		x.nec = true
		if c.corr() > 0.5 {
			x.disc++
		}
		if nKhSeen >= 2 && nKhBad == nKhSeen {
			x.disc++
		}
		if h := now.UTC().Hour(); h >= 15 && h <= 20 {
			x.disc++
		}
		if rst == 0 && c.connFails() == 0 {
			x.disc++
		}
		x.high = x.disc >= 4
	}
	// O2 path-outage
	if c.silent > 0 || c.stage[DialConnUnreach] > 0 || c.death[DeathTimeout] > 0 {
		x := &sc[ClPathOutage]
		x.nec = true
		if c.stage[DialConnUnreach]+c.death[DeathUnreach] > 0 {
			x.disc++
		}
		if dur < 10*time.Minute {
			x.disc++
		}
		if nKhSeen >= 2 && nKhBad < nKhSeen {
			x.disc++
		}
		x.high = x.disc >= 2
	}
	// O3 isp-outage
	if isp := e.ispMap[s.ip]; isp != "" && users >= 0 && users < 0.5 {
		x := &sc[ClISPOutage]
		x.nec = true
		x.disc++ // users of this ISP vanished
		if e.othersClean(s.ip) {
			x.disc++ // other IPs fine
		}
		x.high = x.disc >= 2
	}
	// O4 peer-restart
	// slot 10 (r): an episode open since before a confirmed restart (a
	// standing queue on that IP) gets no peer grace (slot 8 (o)), its burst
	// RSTs are taken out (peerMass), and only the queue was left: res8 read
	// three restarts as bufferbloat low. A confirmed restart explains the
	// burst window, so it takes precedence over the ordinary classes there;
	// it does not when censorship evidence is left (slot 8 (o) unchanged).
	censorNec := false
	for i := range sc {
		if IsCensorship(i) && sc[i].nec {
			censorNec = true
		}
	}
	if ep.grace == "peer" || (ep.peerMass > 0 && !censorNec) {
		x := &sc[ClPeerRestart]
		x.nec = true
		if c.rstFast == 0 || ep.peerMass > 0 {
			x.disc++ // no injection timing, or a confirmed mass burst on every IP
		}
		if c.dialOK > 0 || c.acc[AcceptOK] > 0 || ep.peerMass > 0 {
			x.disc++
		}
		x.high = x.disc >= 2
	}
	// O5 own-restart: our own start grace, or the peer refusing on all IPs.
	if ep.grace == "own" || (c.dials > 0 && c.stage[DialConnRefused]*2 >= c.dials && e.allRefused()) {
		x := &sc[ClOwnRestart]
		x.nec = true
		x.disc += 2
		x.high = true
	}
	// O6 bufferbloat
	if c.samples > 0 && c.meanQ() >= 100 && c.meanLoss() < 0.01 {
		x := &sc[ClBufferbloat]
		x.nec = true
		x.disc++
		if base := s.baseline(now); base > 0 && c.rateSum/float64(c.samples) >= 0.8*base {
			x.disc++
		}
		x.high = x.disc >= 2
	}
	// O7 asym-loss
	if c.peerLossN > 0 && c.samples > 0 {
		pl := c.peerLossSum / float64(c.peerLossN)
		ml := c.meanLoss()
		// slot 5 (f): one direction's loss is the asymmetric-path signature
		// only when nothing else explains it: a queue (bufferbloat /
		// congestion overflow drops the sender's segments) or silence (an
		// outage: nothing acked, every resend counts as loss).
		//
		// slot 7: only our own send direction (ml lossy, pl clean). queued()
		// reads our readings, i.e. the queue in our send direction; the
		// queue in the peer's send direction is not reported to us (the
		// peer sends its loss, not its queue). So when only the peer's
		// direction is lossy (pl >= 3 %, ml < 1 %), a congestion or
		// bufferbloat queue on that direction (tail drops) cannot be ruled
		// out here: the lab congestion runs on Iran showed ploss 14-17 %
		// with q = 0 ms and were called asym-loss high. The peer's own
		// detector sees that direction as its ml with its queue and judges
		// it there, so every direction is still covered once.
		if ml >= 0.03 && pl < 0.01 && !queued(c) && c.silent == 0 && !ep.cutHeld {
			x := &sc[ClAsymLoss]
			x.nec = true
			if c.hsFails() == 0 && c.accFails() == 0 {
				x.disc++
			}
			if rst == 0 {
				x.disc++
			}
			x.high = x.disc >= 2
		}
	}

	ep.class, ep.conf, ep.alt, ep.altScore = pick(&sc, ep)
	gate(ep)
}

// recovered (R1): the IP recovers after our own redials: >= recoverOK
// successes, and fewer than half as many fresh kills or kills after the
// recovery began (an injector keeps killing the redials).
func recovered(c *ctr) bool {
	ok := c.dialOK + c.acc[AcceptOK]
	return ok >= recoverOK && c.freshKill*2 < ok && c.killPostOK*2 < ok
}

// ownLifeCluster (R2 + (z)): the delayed kill's own signature, a lifetime
// cluster among the kills of the redial wave (links born after the episode
// opened), or, for an age longer than the episode so far, among all kills
// when they are spread in time (not one reset of a pool refilled at once).
func ownLifeCluster(ep *episode) bool {
	if ep.bornKills >= dkLifeMin {
		if _, sh := histMode(ep.bornLife[:]); sh >= dkLifeShare {
			return true
		}
	}
	if ep.killDeaths >= dkLifeMin && ep.killTicks >= dkTicksMin {
		_, sh := histMode(ep.lifeHist[:])
		return sh >= dkLifeShare
	}
	return false
}

// gate (step back §7.4) is the two-stage verdict. Stage 1, binary
// "censorship-like": a censorship class whose IP does not recover after our
// own redials (R1). Stage 2, the family, best effort; the exact class stays
// as the verdict's class (a hint, never gated beyond its family). A
// censorship class failing stage 1, or a delayed-kill without its own
// lifetime cluster (R2), is an anomaly at low with the class as hint.
// Ordinary classes are not accusations and pass unchanged.
func gate(ep *episode) {
	ep.hint, ep.veto = ClUnknown, ""
	if !IsCensorship(ep.class) || ep.conf == confUnknown {
		return
	}
	switch f := FamilyOf(ep.class); {
	case (f == FamReset || f == FamHandshake || f == FamBlock) && recovered(&ep.sum):
		ep.veto = "recovered"
	case ep.class == ClDelayedKill && ep.conf >= confMed && !ownLifeCluster(ep):
		ep.veto = "nolife"
	}
	if ep.veto != "" {
		ep.hint, ep.class, ep.conf = ep.class, ClAnomaly, confLow
	}
}

// pick chooses the top class and runner-up and applies the confidence caps.
func pick(sc *[NClasses]score, ep *episode) (cls, conf, alt, altScore int) {
	cls, alt = ClUnknown, ClUnknown
	best, second := -1, -1
	val := func(i int) int {
		if !sc[i].nec {
			return -1
		}
		v := sc[i].disc * 2
		if sc[i].high {
			v++
		}
		if !IsCensorship(i) {
			v++ // ties go to the ordinary explanation (a false accusation costs more)
		}
		return v
	}
	for i := 0; i < NClasses; i++ {
		v := val(i)
		if v < 0 {
			continue
		}
		switch {
		case v > best:
			second, alt = best, cls
			best, cls = v, i
		case v > second:
			second, alt = v, i
		}
	}
	if cls == ClUnknown {
		return ClUnknown, confUnknown, ClUnknown, 0
	}
	if alt != ClUnknown {
		altScore = sc[alt].disc
	}
	x := sc[cls]
	switch {
	case x.disc >= 2:
		conf = confMed
	case x.disc == 1:
		conf = confLow
	default:
		conf = confUnknown
	}
	if x.high && conf >= confMed {
		conf = confHigh
	}
	if IsCensorship(cls) {
		// high only when no other censorship class also scores ≥ 2.
		for i := 0; i < NClasses; i++ {
			if i != cls && IsCensorship(i) && sc[i].nec && sc[i].disc >= 2 && conf == confHigh {
				conf = confMed
			}
		}
		// the xray window, our own start and the peer's restart cap
		// censorship at low (censor351 §3.2).
		if (ep.xray || ep.grace != "") && conf > confLow {
			conf = confLow
		}
	}
	return
}

// pathTrouble (slot 5): one tick's signs of a troubled path (not of a peer
// that went away): timeouts, unreachables, silent or high-queue readings.
func pathTrouble(c *ctr) int {
	return int(c.death[DeathTimeout] + c.death[DeathUnreach] + c.silent + c.highQ + c.stage[DialConnTimeout] + c.stage[DialConnUnreach])
}

// killTighter (slot 10, t) compares the spread of the episode's fresh
// kills in log space: +1 when the bytes at death are tighter than the ages
// at death (a byte trigger), -1 when the ages are tighter (an age trigger),
// 0 with fewer than 3 fresh kills or equal spread.
func killTighter(ep *episode) int {
	if ep.fkN < 3 {
		return 0
	}
	n := ep.fkN
	vb := ep.fkLb2/n - (ep.fkLb/n)*(ep.fkLb/n)
	vl := ep.fkLl2/n - (ep.fkLl/n)*(ep.fkLl/n)
	switch {
	case vb < vl:
		return 1
	case vl < vb:
		return -1
	}
	return 0
}

// queued (slot 5): the episode's readings show a standing queue (the O6
// bufferbloat signature), which explains resends and slow handshakes.
func queued(c *ctr) bool {
	return c.samples > 0 && (c.meanQ() >= queueExplainMs || c.highQ*2 >= c.samples)
}

// queueExplainsRST (slot 7, l): under a standing queue (queued) the RSTs
// are explained by the queue unless most of them carry injection timing.
// The fast test (time since our last send < lim x min RTT) only tells an
// injected RST from a peer's when the link's send timing is tied to the
// RST. Under a standing queue the links that die are the busy ones (they
// fill the queue), and a busy link's last send is always recent, so a
// peer-side RST (the peer giving up on a stalled link) lands "fast" by
// chance on every link that was sending at that moment, about a third of
// the deaths in the lab bufferbloat runs. Measuring against the current
// RTT (srtt, queue included) does not help: a larger RTT only widens the
// fast window. What does separate them is the share: an injector times
// every reset to our segments, so its RSTs are fast nearly all the time
// (the C1 2/3 discriminator), while chance timing gives the busy share.
// So under a queue, rst-inject and the kill classes need the fast share
// to be the C1 majority (rstFast*3 >= rst*2); slot 6 (j)'s rstFast == 0
// is the special case of this.
//
// slot 9 (q): the queue only explains the RSTs that arrived while it
// stood. An episode lives as long as its trigger holds, so a bufferbloat
// episode that an RST injection follows at once keeps the queue's mean
// for the whole run (res7 run a: q 2442 ms at open, then 263 RSTs on a
// clean path, read as bufferbloat low). The RSTs of ticks with no standing
// queue (rstNoQ) are never explained by it.
func queueExplainsRST(c *ctr) bool {
	return queued(c) && c.rstFast*3 < c.death[DeathRST]*2
}

// rstUnexplained (slot 9, q) is the episode's RST evidence a standing
// queue does not explain, with its fast share: everything when the queue
// explains nothing, else only the RSTs of clean ticks.
func rstUnexplained(c *ctr) (rst, fast uint32) {
	if !queueExplainsRST(c) {
		return c.death[DeathRST], c.rstFast
	}
	return min(c.rstNoQ, c.death[DeathRST]), min(c.rstFastNoQ, c.rstFast)
}

// hsResetInject (b414 v6) reports the reset injector's signature in an
// episode with failed handshakes: resets are the majority of the failed
// handshakes (dial and accept side together), and established links died
// by unexplained resets (C1 necessity, rst >= 3) at least one of them with
// injection timing.
func hsResetInject(c *ctr, rst, rstFast uint32) bool {
	fails := c.hsFails() + c.accFails()
	resets := c.stage[DialHelloReset] + c.acc[AcceptReset]
	return rst >= 3 && rstFast > 0 && fails > 0 && resets*2 > fails
}

// tickQueue (slot 9, q) marks the current tick's RSTs as clean-path RSTs
// when no standing queue holds: none in this tick's own readings, and
// none in the last qHoldTicks ticks' (a tick may have no reading).
func (s *scope) tickQueue() (clean bool) {
	c := &s.buckets[s.cur]
	switch {
	case c.samples > 0 && queued(c):
		s.qHold = qHoldTicks
	case s.qHold > 0 && c.samples == 0:
		s.qHold--
	default:
		s.qHold = 0
		c.rstNoQ, c.rstFastNoQ = c.death[DeathRST], c.rstFast
		return true
	}
	c.rstNoQ, c.rstFastNoQ = 0, 0
	return false
}

// cutSig (slot 5): the path is cut, not just degraded: dials time out (the
// C3 blackhole necessity), or most readings are silent (nothing acked), or
// hellos keep failing with none getting through.
func cutSig(c *ctr) bool {
	switch {
	case c.dials >= 4 && c.stage[DialConnTimeout]*10 >= c.dials*9:
		return true
	case c.silent >= 2 && c.silent >= c.moving:
		return true
	case c.accFails() >= 4 && c.acc[AcceptOK] == 0 && c.moving == 0:
		return true
	}
	return false
}

// allDead (slot 5): every known Iran IP's path is dead as the hooks show it.
func (e *evaluator) allDead() bool {
	if len(e.scopes) < 2 {
		return false
	}
	for _, s := range e.scopes {
		if !s.dead {
			return false
		}
	}
	return true
}

// othersClean: some other Iran IP is known and has no open episode.
func (e *evaluator) othersClean(ip netip.Addr) bool {
	for o, s := range e.scopes {
		if o != ip && s.ep == nil {
			return true
		}
	}
	return false
}

// allRefused: every scope's current window has refused dials.
func (e *evaluator) allRefused() bool {
	if len(e.scopes) == 0 {
		return false
	}
	for _, s := range e.scopes {
		w := s.window(winTicks)
		if w.dials > 0 && w.stage[DialConnRefused] == 0 {
			return false
		}
	}
	return true
}

func (e *evaluator) emit(ep *episode, st string, now time.Time) {
	c := &ep.sum
	min := now.Unix() / 60
	if min != e.linesMin {
		if e.folded > 0 {
			e.logf("censor351: +%d lines folded in the last minute (rate cap %d/min)", e.folded, maxLinesMin)
		}
		e.linesMin, e.lines, e.folded = min, 0, 0
	}
	if e.lines >= maxLinesMin {
		e.folded++
		return
	}
	e.lines++
	bm, bsh := histMode(ep.bytesHist[:])
	lm, lsh := histMode(ep.lifeHist[:])
	mode := func(i int, sh float64, unit string) string {
		if i < 0 || ep.killDeaths < 8 {
			return "-"
		}
		return fmt.Sprintf("2^%d%s:%.0f%%", i, unit, sh*100)
	}
	users := "-"
	if r := e.usersRatio(); r >= 0 {
		users = fmt.Sprintf("%.1fx", r)
	}
	grace := ep.grace
	if grace == "" {
		grace = "no"
	}
	// b437 Q2: a grace from a ZDU generation change, not a restart
	if grace == "own" && e.ownZDU || grace == "peer" && e.peerZDU {
		grace += "-zdu"
	}
	ploss := "-"
	if c.peerLossN > 0 {
		ploss = fmt.Sprintf("%.1f%%", 100*c.peerLossSum/float64(c.peerLossN))
	}
	fam, hint := FamilyOf(ep.class), "-"
	if ep.class == ClAnomaly {
		hint = ClassName(ep.hint) + "/" + ep.veto
	}
	line := fmt.Sprintf("censor351: v1 ep=%s st=%s scope=iranip class=%s conf=%s fam=%s hint=%s alt=%s:%d dur=%s kh=%d/%d dials=%d ok=%d to=%d ref=%d unr=%d hs=%d/%d acc=%d/%d deaths=%d rst=%d rstfast=%d eof=%d tmo=%d local=%d fresh=%d bmode=%s lmode=%s loss=%.1f%% q=%.0fms corr=%.2f silent=%d users=%s allip=%v front=r:%d/%d,t:%d/%d hour=%d grace=%s xray=%v ploss=%s peermass=%d cutheld=%v",
		ep.id, st, ClassName(ep.class), confName[ep.conf], famName[fam], hint, ClassName(ep.alt), ep.altScore, now.Sub(ep.opened).Round(time.Second),
		bits.OnesCount64(c.khBad&c.khSeen), bits.OnesCount64(c.khSeen),
		c.dials, c.dialOK, c.stage[DialConnTimeout], c.stage[DialConnRefused], c.stage[DialConnUnreach], c.hsFails(), c.stage[DialCert]+c.stage[DialAuth],
		c.accFails(), c.acc[AcceptOK]+c.accFails(),
		ep.killDeaths, c.death[DeathRST], c.rstFast, c.death[DeathEOF], c.death[DeathTimeout], c.death[DeathLocal], c.freshKill,
		mode(bm, bsh, "B"), mode(lm, lsh, "s"), c.meanLoss()*100, c.meanQ(), c.corr(), c.silent, users, ep.allIPs,
		c.failR, c.dialR, c.failT, c.dialT, now.UTC().Hour(), grace, ep.xray, ploss, ep.peerMass, ep.cutHeld)
	if c.peerAb > 0 { // b414: only against a newer Iran, so older lines stay as they were
		line += fmt.Sprintf(" peerab=%d", c.peerAb)
	}
	e.logf("%s", line)
	if st == "close" {
		e.closedLines = append(e.closedLines, strings.TrimPrefix(line, "censor351: "))
		if len(e.closedLines) > 16 {
			e.closedLines = e.closedLines[len(e.closedLines)-16:]
		}
	}
}
