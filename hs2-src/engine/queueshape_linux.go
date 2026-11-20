//go:build linux

package engine

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// qShaper holds the tunnel's egress at the controller's cap with tc, on the
// interface its links leave by:
//
//	root 1fe1: htb, default class 1fe1:1 (everything else: unlimited, fq_codel)
//	class 1fe1:N at the cap → cake (flow queueing: a game's packets do not wait
//	behind a download's), filtered by the peers' address AND port: only this
//	tunnel's own links, never another tunnel's or anything else to that IP
//
// Several hs2 processes on one server (hs2 and hs2-out, 2026-11-09) share the
// root: each takes a class of its own, N, held by an flock on
// <qLockDir>/hs2-qshape.<dev>.<N> while it is in place (a class whose lock
// nobody holds was left by a process that is gone, and is cleaned up), and
// every change to the tree is made under <qLockDir>/hs2-qshape.lock. The root
// goes (tc qdisc del root: the kernel's default comes back) with the last
// class. Before, the second process tried to replace the first's root
// ("Change operation not supported"), and the first's filter, by the exit's
// IP alone, held the second's traffic to that IP under a cap measured on the
// first's links only.
//
// A root qdisc an operator set up (anything but the kernel's defaults or this
// tree) is never replaced: the controller is told it cannot shape, once.
type qShaper struct {
	logf      func(string, ...any)
	want      string // the interface to shape; "": the one the route to the first peer leaves by
	dev       string
	minor     int      // this process's class, 1fe1:<minor>
	slot      *os.File // the flock that holds it
	peers     []qKey   // m4f251: grouped by (peer IP, listen port) on the side that accepted the links
	rate      float64
	drop      uint64 // the leaf's drop counter at the last drops()
	installed bool
	parked    bool // hl233: in place with no cap (park)
	gen       int  // hl234: the filters' generation (qFilterPriosGen)
	refused   bool
	// pk237 (HS2_QUEUE_CTL_SHAPER=ifb): the htb tree lives on an ifb (qdev),
	// the links' packets are steered there by clsact egress filters on dev
	// only while a cap holds (steer); dev's root qdisc is never touched.
	ifb   bool
	qdev  string // where the htb tree is: dev (root mode) or the ifb
	steer bool   // ifb mode: the links' redirect filters are on dev
	// pk239 (HS2_QUEUE_CTL_SHAPER=mqchild): the htb tree is the qdisc of
	// dev's LAST mq child (mqParent); the links' packets are moved onto that
	// TX queue (skbedit queue_mapping mqQueue + priority) by clsact egress
	// filters only while a cap holds. No mirred: the original skb stays, and
	// with it TCP Small Queues (pk238).
	mqc      bool
	mqParent string // the mq child the tree is grafted on, e.g. "1:8"
	mqQueue  int    // its TX queue index (0-based)
	// fx247 (audit246 M1): the mode asked for (a fallback to root mode is
	// per install; a reinstall asks again), the tree self-check, and the
	// lost state (the tree vanished: no root graft until a light tick).
	mqWant, ifbWant bool
	checkAt         time.Time // next self-check of the tree
	suspect         bool      // a tc call on the tree failed: check now
	lost            bool      // the tree vanished; reinstall pending
	// audit246 L5: a first install's root graft under a cap (park's light
	// install did not happen) is held since graftHeldAt, for qGraftHoldMax;
	// graftHeld: mqReady refused only for that hold (mqchild mode kept).
	graftHeldAt     time.Time
	graftHeld       bool
	errLog, skipLog qRateLog
}

// qRateLog (fx247): at most one line a minute per kind; the ones held back
// are counted on the next.
type qRateLog struct {
	at   time.Time
	held int
}

func (r *qRateLog) logf(logf func(string, ...any), f string, a ...any) {
	now := time.Now()
	if !r.at.IsZero() && now.Sub(r.at) < time.Minute {
		r.held++
		return
	}
	if r.held > 0 {
		f += fmt.Sprintf(" (+%d like it held back)", r.held)
	}
	r.at, r.held = now, 0
	logf(f, a...)
}

// fx247 counters.
var qStat struct {
	execTimeouts atomic.Uint64 // tc/ip execs killed at qExecTimeout
	lockTimeouts atomic.Uint64 // ticks skipped: the tree lock not had in qLockWait
	reinstalls   atomic.Uint64 // the tree found gone and put back
	deferred     atomic.Uint64 // reinstalls waiting for a light tick (no root graft under load)
	keptLive     atomic.Uint64 // a root-mode install that left a live mqchild tree in place
}

// steers: the links reach the tree through clsact egress filters (ifb,
// mqchild), not through filters on the tree's own root (root mode).
func (s *qShaper) steers() bool { return s.ifb || s.mqc }

func newQShaper(logf func(string, ...any)) *qShaper {
	return &qShaper{logf: logf, ifb: qShaperMode == "ifb", mqc: qShaperMode == "mqchild", ifbWant: qShaperMode == "ifb", mqWant: qShaperMode == "mqchild"}
}

// newQShaperOn shapes dev ("": the route's interface, as newQShaper).
func newQShaperOn(dev string, logf func(string, ...any)) *qShaper {
	return &qShaper{logf: logf, want: dev, ifb: qShaperMode == "ifb", mqc: qShaperMode == "mqchild", ifbWant: qShaperMode == "ifb", mqWant: qShaperMode == "mqchild"}
}

// qShaperMode (pk237): HS2_QUEUE_CTL_SHAPER. "root" (default): rel69c's htb
// tree as the interface's root qdisc. "ifb": the interface's root qdisc (mq +
// fq_codel per TX queue, or whatever the operator set) is never touched; the
// tree is on an ifb, and only the tunnel links' packets go through it, and
// only while a cap holds (clsact egress u32 -> skbedit priority -> mirred
// redirect). Lab pk237: the parked root htb costs every other service on the
// NIC p50/p99 latency and softirq at 8 Gbit/s; with ifb, parked = the plain
// mq path.
//
// pk238: do NOT use "ifb" for hs2's own links. act_mirred clones the skb at
// egress and frees the original at once, so the link sockets lose TSQ
// backpressure: under a cap each BBR link pushes its whole cwnd into cake,
// which drops (real-hs2 ABAB: ~97k cake drops per run vs 0 in root mode,
// heavy -10..-35 %, a 522 ms call gap, crowd p99 730 ms; netns bench 16 BBR
// flows: ~5000 drops/retransmits vs ~30). Steering into a dedicated mq child
// (clsact skbedit queue_mapping + priority, htb+cake as that child) keeps the
// original skb and TSQ (bench: 34-35 drops) — the candidate for pk239.
//
// pk239: "mqchild": on an mq root (multi-queue NIC: the Iran edge) the root
// stays mq (kept; only a default handle-0 mq is re-added once with a handle); htb + cake (the same tree as root mode) is
// grafted once, at the first start while parked, as the qdisc of the LAST mq
// child, plus clsact once. Parked: no filters (native per-queue path). Under
// a cap: clsact egress u32 for this process's links -> skbedit queue_mapping
// <last queue> priority 1fe1:<class> (no mirred, no clone: TSQ intact). Other
// traffic XPS puts on that queue (~1/N) passes its 40 Gbit fq_codel default
// class. One TX queue, a non-mq root, kernel < 5.18 (queue_mapping at clsact
// egress not honoured) or no act_skbedit: root mode (logged).
//
// rel69e: "mqchild" is the envdef default on both sides (the exit, 1 TX queue,
// falls back to root mode by itself); HS2_QUEUE_CTL_SHAPER=root (or any other
// value, or HS2_DEFAULTS=legacy) is rel69d. Read by loadEnvKeys
// (defaults22.go) through parseQShaperMode.

// qIfbName: the ifb that holds dev's tree in ifb mode (IFNAMSIZ 15).
func qIfbName(dev string) string {
	n := "hs2q" + dev
	if len(n) > 15 {
		n = n[:15]
	}
	return n
}

// qIfbQueues (pk237): the ifb's TX queues = dev's (capped at 16): each ifb
// queue reinjects in its own tasklet, so the shaped traffic is not held to
// one CPU (lab: 1 queue 1.8, 8 queues 3.1 Gbit/s through an open class).
func qIfbQueues(dev string) int {
	m, _ := filepath.Glob("/sys/class/net/" + dev + "/queues/tx-*")
	return min(max(len(m), 1), 16)
}

const qHandle = "1fe1"

// qMaxMinor bounds the classes (hs2 processes) on one interface.
const qMaxMinor = 0xff

// qLockDir holds the shapers' locks (a var: tests point it elsewhere).
var qLockDir = "/run"

// qDefaultRoots are the root qdiscs a kernel or a distribution sets up.
var qDefaultRoots = []string{"noqueue", "pfifo_fast", "fq_codel", "fq", "mq", "pfifo", "sfq"}

// fx247 (audit246 M2): every tc / ip exec ends within qExecTimeout (a tc
// stuck on RTNL no longer freezes the queue loop with the tree lock held).
var (
	qExecTimeout = 5 * time.Second
	qLockWait    = 5 * time.Second
	qTCBin       = "tc" // tests point these at a fake
	qIPBin       = "ip"
)

// errQTimeout: a tc/ip exec killed at qExecTimeout (the tree's state unknown).
var errQTimeout = errors.New("timed out")

// qExec runs name args (stdin: in) within qExecTimeout; combined: stderr too.
func qExec(in string, combined bool, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), qExecTimeout)
	defer cancel()
	c := exec.CommandContext(ctx, name, args...)
	c.WaitDelay = time.Second // a grandchild holding the pipes does not hold us
	if in != "" {
		c.Stdin = strings.NewReader(in)
	}
	var out []byte
	var err error
	if combined {
		out, err = c.CombinedOutput()
	} else {
		out, err = c.Output()
	}
	if ctx.Err() == context.DeadlineExceeded {
		qStat.execTimeouts.Add(1)
		return out, fmt.Errorf("%w after %v (%v)", errQTimeout, qExecTimeout, err)
	}
	return out, err
}

func qTC(args ...string) (string, error) {
	out, err := qExec("", true, qTCBin, args...)
	if err != nil {
		return string(out), fmt.Errorf("tc %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// qTCBatch runs tc commands (one per line) in one exec; -force: a failed line
// does not stop the rest.
func qTCBatch(cmds string) error {
	if cmds == "" {
		return nil
	}
	if out, err := qExec(cmds, true, qTCBin, "-force", "-batch", "-"); err != nil {
		return fmt.Errorf("tc -batch: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func qRouteDev(a netip.Addr) string {
	out, err := qExec("", false, qIPBin, "route", "get", a.String())
	if err != nil {
		return ""
	}
	f := strings.Fields(string(out))
	for i := 0; i+1 < len(f); i++ {
		if f[i] == "dev" {
			return f[i+1]
		}
	}
	return ""
}

// qWire: the cap is in the payload the links deliver (TCP_INFO bytes_acked);
// tc counts whole packets, TCP/IP and Ethernet headers included — ~4.5% more
// on full-size segments. Without it every cap held the links ~4.5% under its
// own number.
const qWire = 1.045

func qBits(rate float64) string { return fmt.Sprintf("%dbit", int64(math.Round(rate*qWire*8))) }

// qHtbArgs: the capped class's htb rate and ceil, and (HS2_QUEUE_CTL_KEEP,
// hl214) burst/cburst of qHtbBurst of the rate. tc's default burst is the
// rate over the timer's HZ (~60 KB at 500 Mbit/s), under one GSO super
// packet (64 KB): lab d8 under a 490 Mbit/s cap that did not bind on average
// showed 1.47 M htb overlimits in 1.93 M packets and the links delivered
// only ~0.89 of the cap. A few ms of burst lets the paced trains through at
// the cap's average rate; at most that much queues on the path.
func qHtbArgs(rate float64) []string {
	a := []string{"rate", qBits(rate), "ceil", qBits(rate)}
	if queueCtlKeep && rate < qParkRate {
		b := strconv.FormatInt(max(int64(rate*qWire*qHtbBurst.Seconds()), 128<<10), 10)
		a = append(a, "burst", b, "cburst", b)
	}
	return a
}

const qHtbBurst = 4 * time.Millisecond

// qTreeLock serialises changes to the tc tree across hs2 processes; the
// returned func lets go. fx247 (audit246 M2): not had within qLockWait (a
// process stuck holding it): false, the caller skips this tick.
func qTreeLock() (func(), bool) {
	f, err := os.OpenFile(filepath.Join(qLockDir, "hs2-qshape.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return func() {}, true
	}
	dl := time.Now().Add(qLockWait)
	for syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		if time.Now().After(dl) {
			f.Close()
			qStat.lockTimeouts.Add(1)
			return func() {}, false
		}
		time.Sleep(10 * time.Millisecond)
	}
	return func() { f.Close() }, true // closing drops the flock
}

// qTrySlot takes class minor's lock on dev if no live process holds it.
func qTrySlot(dev string, minor int) *os.File {
	f, err := os.OpenFile(filepath.Join(qLockDir, fmt.Sprintf("hs2-qshape.%s.%x", dev, minor)), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil
	}
	if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		f.Close()
		return nil
	}
	return f
}

var qClassRe = regexp.MustCompile(`class htb ` + qHandle + `:([0-9a-f]+) `)

// qClasses lists the minors of the classes under 1fe1: on dev (1 included).
func qClasses(dev string) []int {
	out, _ := qTC("class", "show", "dev", dev)
	var ms []int
	for _, m := range qClassRe.FindAllStringSubmatch(out, -1) {
		if v, err := strconv.ParseUint(m[1], 16, 16); err == nil {
			ms = append(ms, int(v))
		}
	}
	return ms
}

// qFilterPrios: a class's filters, IPv4 and IPv6 (one protocol per prio).
func qFilterPrios(minor int) (v4, v6 string) {
	return qFilterPriosGen(minor, 0)
}

// qFilterPriosGen (hl234): the filters of generation gen (0, 1). A new peer
// set goes in under the other generation's prios before the old ones are
// deleted, so the links' packets always match a filter of the class.
func qFilterPriosGen(minor, gen int) (v4, v6 string) {
	return strconv.Itoa(0x2000*gen + minor), strconv.Itoa(0x2000*gen + 0x1000 + minor)
}

func qClassID(minor int) string { return fmt.Sprintf("%s:%x", qHandle, minor) }

// qDropClass takes class minor's filters and class (its leaf with it) off dev.
func qDropClass(dev string, minor int) {
	qDropFilters(dev, false, minor)
	qTC("class", "del", "dev", dev, "classid", qClassID(minor))
}

// qDropFilters takes class minor's filters (both generations) off dev: off
// the htb root, or (egress) off dev's clsact egress hook (ifb mode).
func qDropFilters(dev string, egress bool, minor int) {
	var b strings.Builder
	for gen := 0; gen < 2; gen++ {
		v4, v6 := qFilterPriosGen(minor, gen)
		for _, p := range []string{v4, v6} {
			if egress {
				fmt.Fprintf(&b, "filter del dev %s egress prio %s\n", dev, p)
			} else {
				fmt.Fprintf(&b, "filter del dev %s parent %s: prio %s\n", dev, qHandle, p)
			}
		}
	}
	qTCBatch(b.String()) // a delete of an empty prio fails, harmlessly
}

// apply installs the shaper (once) and sets the cap and the peers; false
// when this server cannot be shaped (no tc, an operator's root qdisc).
func (s *qShaper) apply(rate float64, peers []qKey) bool {
	return s.set(rate, peers, true)
}

// set is apply; in ifb mode steer says whether the links' packets go
// through the tree at all (always under a cap; root mode: always).
func (s *qShaper) set(rate float64, peers []qKey, steer bool) bool {
	if s.refused {
		return false
	}
	s.parked = false
	if s.installed && (s.suspect || !time.Now().Before(s.checkAt)) {
		// fx247 (audit246 M1): the tree can vanish under us (a root-mode
		// hs2 replacing the mq root, an operator's qdisc del, a NIC
		// re-created): found gone, it is put back
		s.checkAt, s.suspect = time.Now().Add(qCheckEvery), false
		if !s.treeHere() {
			s.lostTree()
		}
	}
	if !s.installed {
		dev := s.want
		if s.lost && s.dev != "" {
			dev = s.dev
		}
		if dev == "" {
			dev = qRouteDev(peers[0].ip)
		}
		if dev == "" {
			return s.refuse("no route to %s found", peers[0].ip)
		}
		lost := s.lost
		if !s.install(dev, rate) {
			// refused: the lane stands aside; otherwise (a lock not had, a
			// root graft held back to a light tick) retried next tick
			return !s.refused
		}
		s.checkAt, s.graftHeldAt = time.Now().Add(qCheckEvery), time.Time{}
		if lost {
			qStat.reinstalls.Add(1)
			s.logf("queue: shaper tree on %s found gone and put back (class %s, fx247)", dev, qClassID(s.minor))
		}
		if s.mqc {
			s.logf("queue: shaper on %s as mq child %s (tc htb + cake on TX queue %d; root qdisc untouched, links moved there only under a cap, pk239)", dev, s.mqParent, s.mqQueue)
		} else if s.ifb {
			s.logf("queue: shaper on %s via %s (tc htb + cake on an ifb; %s's root qdisc untouched, links steered there only under a cap, pk237)", dev, s.qdev, dev)
		} else if rate >= qParkRate {
			s.logf("queue: shaper on %s (tc htb + cake), parked with no cap: a cap later changes the class, the root qdisc is not swapped under load (hl233)", dev)
		} else {
			s.logf("queue: shaper on %s (tc htb + cake), tunnel egress to the edge at %.1f Mbit/s", dev, mbitps(rate))
		}
	}
	if queueCtlKeep {
		// hl218 (HS2_QUEUE_CTL_KEEP): the peers come in map order (a new
		// order every tick), so the filters were deleted and re-added every
		// tick; meanwhile the links' packets took the default (unshaped)
		// class: lab u8 under an edge cap, 43 MB of 943 MB (4.6 %) went
		// unshaped — the links delivered up to 5 % over the cap with no
		// queue. Compare them sorted.
		peers = slices.Clone(peers)
		slices.SortFunc(peers, qKey.compare)
	}
	steer = steer || !s.steers() || rate < qParkRate
	if s.steers() && !steer {
		if s.steer {
			unlock, ok := qTreeLock()
			if !ok {
				s.skipLog.logf(s.logf, "queue: the tc tree lock not had in %v, tick skipped (fx247)", qLockWait)
				return true
			}
			qDropFilters(s.dev, true, s.minor)
			unlock()
			s.steer = false
		}
		s.peers = slices.Clone(peers)
	} else if !slices.Equal(peers, s.peers) || (s.steers() && !s.steer) {
		// hl234 (lab hl234 hS1 u8): del-all then one tc exec per filter left
		// the links unshaped (the default class) for ~185 ms (40 links,
		// ~4.6 ms per exec) on every tick the link set changed - at a burst's
		// onset every tick: 46 MB of the u8 phase went unshaped, the links
		// delivered 484 over a 462.7 cap and the path queued 77 ms while our
		// own cake held 50 us. Now the new set goes in under the other
		// generation's prios in one tc batch, then the old prios go.
		unlock, ok := qTreeLock()
		if !ok {
			s.skipLog.logf(s.logf, "queue: the tc tree lock not had in %v, tick skipped (fx247)", qLockWait)
			return true
		}
		ov4, ov6 := qFilterPriosGen(s.minor, s.gen)
		gen := 1 - s.gen
		v4, v6 := qFilterPriosGen(s.minor, gen)
		flow := qClassID(s.minor)
		var b strings.Builder
		for _, p := range peers {
			m := p.match() // m4f251: dst IP + dport (dialed) or + TCP sport (accepted)
			if s.steers() {
				// htb takes a packet whose priority is one of its classes
				// straight to it (htb_classify): no filters on the ifb.
				act := fmt.Sprintf("action skbedit priority %s pipe action mirred egress redirect dev %s", flow, s.qdev)
				if s.mqc {
					// pk239: the same skb onto the tree's TX queue
					// (skb->tc_skip_txqueue: XPS does not re-pick it)
					act = fmt.Sprintf("action skbedit queue_mapping %d priority %s", s.mqQueue, flow)
				}
				if p.ip.Is6() {
					fmt.Fprintf(&b, "filter add dev %s egress protocol ipv6 prio %s u32 %s %s\n", s.dev, v6, m, act)
				} else {
					fmt.Fprintf(&b, "filter add dev %s egress protocol ip prio %s u32 %s %s\n", s.dev, v4, m, act)
				}
			} else if p.ip.Is6() {
				fmt.Fprintf(&b, "filter add dev %s parent %s: protocol ipv6 prio %s u32 %s flowid %s\n", s.dev, qHandle, v6, m, flow)
			} else {
				fmt.Fprintf(&b, "filter add dev %s parent %s: protocol ip prio %s u32 %s flowid %s\n", s.dev, qHandle, v4, m, flow)
			}
		}
		ferr := qTCBatch(b.String())
		if ferr != nil {
			s.errLog.logf(s.logf, "queue: %v", ferr)
		}
		// the old generation (both prios: a delete of an empty prio fails, harmlessly)
		if s.steers() {
			qTCBatch(fmt.Sprintf("filter del dev %s egress prio %s\nfilter del dev %s egress prio %s\n", s.dev, ov4, s.dev, ov6))
		} else {
			qTCBatch(fmt.Sprintf("filter del dev %s parent %s: prio %s\nfilter del dev %s parent %s: prio %s\n", s.dev, qHandle, ov4, s.dev, qHandle, ov6))
		}
		s.gen = gen
		s.steer = s.steers()
		unlock()
		s.peers = slices.Clone(peers)
		if ferr != nil {
			// fx247 (audit246 M1): a filter not in place: the set is
			// re-sent next tick (and the tree checked), not taken as done
			s.peers, s.suspect = nil, true
		}
	}
	if rate != s.rate {
		if _, err := qTC(append([]string{"class", "change", "dev", s.qdev, "parent", qHandle + ":", "classid", qClassID(s.minor), "htb"}, qHtbArgs(rate)...)...); err != nil {
			s.errLog.logf(s.logf, "queue: %v", err)
			s.suspect = true // fx247: not taken as done; retried (the tree checked) next tick
		} else {
			s.rate = rate
		}
	}
	return true
}

// qCheckEvery (fx247): how often an installed shaper checks its tree is
// still there (one or two tc execs).
var qCheckEvery = 10 * time.Second

// treeHere (fx247): this process's class is still on qdev (and, mqchild, the
// htb still on the mq child). A timed-out check says yes (unknown: no churn).
func (s *qShaper) treeHere() bool {
	cls := qClassID(s.minor)
	out, err := qTC("class", "show", "dev", s.qdev, "classid", cls)
	if errors.Is(err, errQTimeout) {
		return true
	}
	if err != nil || !strings.Contains(out, "class htb "+cls+" ") {
		return false
	}
	if s.mqc {
		out, err := qTC("qdisc", "show", "dev", s.dev, "parent", s.mqParent)
		if errors.Is(err, errQTimeout) {
			return true
		}
		return err == nil && strings.Contains(out, "htb "+qHandle+":")
	}
	return true
}

// lostTree (fx247): the tree is gone: its state forgotten (the class slot,
// and with it the minor, kept), the mode asked for again; the next install
// puts it back without a root graft unless the links are light (hl233).
func (s *qShaper) lostTree() {
	s.errLog.logf(s.logf, "queue: shaper tree on %s (class %s) is gone (another root qdisc, a qdisc del, a re-created device?): put back (fx247)", s.dev, qClassID(s.minor))
	if s.steers() {
		if unlock, ok := qTreeLock(); ok {
			qDropFilters(s.dev, true, s.minor) // they point at a class that is not there
			unlock()
		}
	}
	s.installed, s.steer, s.peers, s.rate, s.drop, s.lost = false, false, nil, 0, 0, true
	s.mqc, s.ifb = s.mqWant, s.ifbWant
}

// noRootGraft (fx247 + hl233): a reinstall under a cap waits for a light
// tick rather than swap the interface's root qdisc under load. audit246 L5:
// so does a first install under a cap while parking is on (HS2_QUEUE_CTL_KEEP:
// the graft belongs to park's light tick — after a reboot the kernel's mq
// has handle 0 again), for at most qGraftHoldMax: a lane that is never light
// gets its shaper with one graft rather than none.
func (s *qShaper) noRootGraft(rate float64) bool {
	if rate >= qParkRate {
		return false
	}
	if s.lost {
		return true
	}
	if !queueCtlKeep {
		return false
	}
	now := time.Now()
	if s.graftHeldAt.IsZero() {
		s.graftHeldAt = now
	}
	return now.Sub(s.graftHeldAt) < qGraftHoldMax
}

// qGraftHoldMax (audit246 L5): the longest a first install's root graft
// waits for a light tick under a cap.
var qGraftHoldMax = time.Minute

// rootGraftHeld: logs (rate-limited) and counts a held-back root graft.
func (s *qShaper) rootGraftHeld(dev string) bool {
	qStat.deferred.Add(1)
	s.skipLog.logf(s.logf, "queue: %s: putting the shaper back needs a root qdisc graft: held until the links are light (hl233, fx247)", dev)
	return false
}

// qMqTreeLive (fx247, audit246 M1, under the tree lock): dev's mq root holds
// an hs2 mqchild tree (root handle 1fe0:, or htb 1fe1: on an mq child) with
// a class a live process holds.
func qMqTreeLive(dev, root string) bool {
	all, err := qTC("qdisc", "show", "dev", dev)
	if err != nil {
		return true // unknown: never replace
	}
	if !strings.HasPrefix(strings.TrimSpace(root), "qdisc mq "+qMqHandle+":") && !strings.Contains(all, "qdisc htb "+qHandle+": parent ") {
		return false
	}
	return !qRootTreeGone(dev)
}

// install puts this process's class (and the shared root, when it is not
// there yet) on dev, under the tree lock.
func (s *qShaper) install(dev string, rate float64) bool {
	unlock, ok := qTreeLock()
	if !ok {
		s.skipLog.logf(s.logf, "queue: the tc tree lock not had in %v, install retried next tick (fx247)", qLockWait)
		return false
	}
	defer unlock()
	s.rate = rate // the rate install puts in (noRootGraft's light test)
	if s.ifb && !s.ifbReady(dev) {
		s.ifb = false // root mode, as rel69c
	}
	s.graftHeld = false
	if s.mqc && !s.mqReady(dev) {
		if s.graftHeld {
			// audit246 L5: refused only for a held graft — mqchild still
			// wanted, not a root-mode htb on the next (light) tick
			return s.rootGraftHeld(dev)
		}
		s.mqc = false // root mode, as rel69c
	}
	if s.mqc {
		return s.placeClass(dev, dev, rate)
	}
	if s.ifb {
		return s.placeClass(dev, qIfbName(dev), rate)
	}
	out, err := qTC("qdisc", "show", "dev", dev, "root")
	if err != nil {
		return s.refuse("%v", err)
	}
	f := strings.Fields(out)
	kind := ""
	if len(f) > 1 {
		kind = f[1]
	}
	ours := kind == "htb" && len(f) > 2 && f[2] == qHandle+":"
	if !ours && !slices.Contains(qDefaultRoots, kind) {
		return s.refuse("%s has a root qdisc set up by hand (%s) — left as it is", dev, kind)
	}
	if kind == "mq" && qMqTreeLive(dev, out) {
		// fx247 (audit246 M1): another live hs2 shapes on this mq's child
		// (mqchild): replacing the root would wipe its tree and drop on
		// every TX queue. Share it as an mq child, or wait.
		qStat.keptLive.Add(1)
		if s.mqReady(dev) {
			s.mqc = true
			return s.placeClass(dev, dev, rate)
		}
		s.skipLog.logf(s.logf, "queue: %s's mq root holds a live hs2 mqchild shaper tree: not replaced by a root-mode one; retried next tick (fx247)", dev)
		return false
	}
	if ours {
		// Classes whose process is gone (nobody holds their lock): cleaned up.
		for _, m := range qClasses(dev) {
			if m < 2 {
				continue
			}
			if l := qTrySlot(dev, m); l != nil && !s.adoptStale(l, m, "", dev) {
				qDropClass(dev, m)
				l.Close()
			}
		}
	} else {
		if s.noRootGraft(rate) {
			return s.rootGraftHeld(dev)
		}
		if _, err := qTC("qdisc", "replace", "dev", dev, "root", "handle", qHandle+":", "htb", "default", "1"); err != nil {
			return s.refuse("%v", err)
		}
		if _, err := qTC("class", "replace", "dev", dev, "parent", qHandle+":", "classid", qHandle+":1", "htb", "rate", "40gbit"); err == nil {
			qTC("qdisc", "replace", "dev", dev, "parent", qHandle+":1", "fq_codel")
		}
	}
	return s.placeClass(dev, dev, rate)
}

// ifbReady (pk237, under the tree lock): dev's clsact (added once if
// missing: the only graft on dev, at the first start while parked; left in
// place on removal, so a restart grafts nothing on dev) and the ifb with the
// htb tree (stale classes of gone processes cleaned up), and a probe that the
// kernel has the actions. false: fall back to root mode.
func (s *qShaper) ifbReady(dev string) bool {
	fail := func(f string, a ...any) bool {
		s.logf("queue: HS2_QUEUE_CTL_SHAPER=ifb not possible on %s ("+f+"), the shaper goes on the root qdisc as before", append([]any{dev}, a...)...)
		return false
	}
	out, err := qTC("qdisc", "show", "dev", dev)
	if err != nil {
		return fail("%v", err)
	}
	if !strings.Contains(out, "qdisc clsact ffff:") {
		if strings.Contains(out, "qdisc ingress ffff:") {
			return fail("an ingress qdisc is set up there")
		}
		if _, err := qTC("qdisc", "add", "dev", dev, "clsact"); err != nil {
			return fail("%v", err)
		}
	}
	ifb := qIfbName(dev)
	if _, err := qExec("", true, qIPBin, "link", "show", "dev", ifb); err != nil {
		n := strconv.Itoa(qIfbQueues(dev))
		if out, err := qExec("", true, qIPBin, "link", "add", ifb, "numtxqueues", n, "numrxqueues", n, "type", "ifb"); err != nil {
			return fail("ip link add %s type ifb: %v: %s", ifb, err, strings.TrimSpace(string(out)))
		}
	}
	qExec("", true, qIPBin, "link", "set", "dev", ifb, "up")
	out, _ = qTC("qdisc", "show", "dev", ifb, "root")
	if strings.Contains(out, "htb "+qHandle+":") {
		for _, m := range qClasses(ifb) {
			if m < 2 {
				continue
			}
			if l := qTrySlot(dev, m); l != nil && !s.adoptStale(l, m, dev, ifb) { // its process is gone
				qDropFilters(dev, true, m)
				qDropClass(ifb, m)
				l.Close()
			}
		}
	} else {
		// nothing is redirected to the ifb yet: no swap under load
		if _, err := qTC("qdisc", "replace", "dev", ifb, "root", "handle", qHandle+":", "htb", "default", "1"); err != nil {
			return fail("%v", err)
		}
		if _, err := qTC("class", "replace", "dev", ifb, "parent", qHandle+":", "classid", qHandle+":1", "htb", "rate", "40gbit"); err == nil {
			qTC("qdisc", "replace", "dev", ifb, "parent", qHandle+":1", "fq_codel")
		}
	}
	// the actions (act_skbedit, act_mirred), on a prio and an address no link uses
	err = qTCBatch(fmt.Sprintf("filter add dev %s egress protocol ip prio 65000 u32 match ip dst 127.255.255.254/32 action skbedit priority %s:1 pipe action mirred egress redirect dev %s\n", dev, qHandle, ifb))
	qTC("filter", "del", "dev", dev, "egress", "prio", "65000")
	if err != nil {
		return fail("%v", err)
	}
	return true
}

// qKernelAtLeast: the running kernel is at least maj.min.
func qKernelAtLeast(maj, min int) bool {
	var u syscall.Utsname
	if syscall.Uname(&u) != nil {
		return false
	}
	var b []byte
	for _, c := range u.Release {
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	return qReleaseAtLeast(string(b), maj, min)
}

var qReleaseRe = regexp.MustCompile(`^(\d+)\.(\d+)`)

func qReleaseAtLeast(rel string, maj, min int) bool {
	m := qReleaseRe.FindStringSubmatch(rel)
	if m == nil {
		return false
	}
	a, _ := strconv.Atoi(m[1])
	b, _ := strconv.Atoi(m[2])
	return a > maj || a == maj && b >= min
}

// qMqHandle: the handle mqchild gives a default (handle 0) mq root.
const qMqHandle = "1fe0"

var qMqRootRe = regexp.MustCompile(`^qdisc mq ([0-9a-f]*):\s+root`)

// mqReady (pk239, under the tree lock): dev has >= 2 TX queues, an mq root
// (kept as it is; the kernel's default handle-0 mq is re-added once with a
// handle, the one root graft, since a handle-0 qdisc cannot parent), a kernel that honours skbedit queue_mapping at clsact
// egress (5.18+), act_skbedit; the htb tree as the last mq child (grafted
// once, here, while parked; a tree already there is reused and its gone
// processes' classes cleaned up) and clsact (added once). false: root mode.
func (s *qShaper) mqReady(dev string) bool {
	fail := func(f string, a ...any) bool {
		f = "queue: HS2_QUEUE_CTL_SHAPER=mqchild not possible on %s (" + f + "), the shaper goes on the root qdisc as before"
		if s.lost {
			s.skipLog.logf(s.logf, f, append([]any{dev}, a...)...) // fx247: retried every tick
		} else {
			s.logf(f, append([]any{dev}, a...)...)
		}
		return false
	}
	if !qKernelAtLeast(5, 18) {
		return fail("kernel older than 5.18: skbedit queue_mapping is not honoured at egress")
	}
	out, err := qTC("qdisc", "show", "dev", dev, "root")
	if err != nil {
		return fail("%v", err)
	}
	if txq, _ := filepath.Glob("/sys/class/net/" + dev + "/queues/tx-*"); len(txq) != 1 && strings.HasPrefix(strings.TrimSpace(out), "qdisc htb "+qHandle+":") && qRootTreeGone(dev) {
		// rel69e: a root-mode tree whose processes are all gone (rel69c/d,
		// or root mode after a fallback: a stop by signal exits before the
		// queue loop's remove, so the root htb outlives the process). Put
		// the multi-queue root back (mq, children net.core.default_qdisc, as
		// the kernel's), with a handle: one root graft, at this first start,
		// while parked. On a 1-queue device mq is refused and the htb stays.
		if s.noRootGraft(s.rate) {
			s.graftHeld = true // install logs it (rootGraftHeld), mqchild kept (L5)
			return false
		}
		if _, err := qTC("qdisc", "replace", "dev", dev, "root", "handle", qMqHandle+":", "mq"); err != nil {
			return fail("the root qdisc is a root-mode htb left by a stopped process, and mq cannot replace it: %v", err)
		}
		s.logf("queue: %s: the root-mode htb left by a stopped process replaced by mq with handle %s: (one graft, parked) (rel69e)", dev, qMqHandle)
		if out, err = qTC("qdisc", "show", "dev", dev, "root"); err != nil {
			return fail("%v", err)
		}
	}
	r := qMqRootRe.FindStringSubmatch(strings.TrimSpace(out))
	if r == nil {
		return fail("the root qdisc is not mq: %s", strings.TrimSpace(out))
	}
	if r[1] == "0" {
		// the kernel's default mq has handle 0, and a qdisc with handle 0
		// cannot be a parent (tc: "Failed to find specified qdisc"): the same
		// mq (children: net.core.default_qdisc, as the kernel's) with a
		// handle, once, here at the first start while parked; later starts
		// find it and graft nothing
		if s.noRootGraft(s.rate) {
			s.graftHeld = true // install logs it (rootGraftHeld), mqchild kept (L5)
			return false
		}
		if _, err := qTC("qdisc", "replace", "dev", dev, "root", "handle", qMqHandle+":", "mq"); err != nil {
			return fail("%v", err)
		}
		s.logf("queue: %s: the default mq root re-added with handle %s: (one graft, parked) so its last child can hold the shaper (pk239)", dev, qMqHandle)
		r[1] = qMqHandle
	}
	all, err := qTC("qdisc", "show", "dev", dev)
	if err != nil {
		return fail("%v", err)
	}
	// the TX queues in use (real_num_tx_queues); without this netns's sysfs
	// (a test's unshare -n): the mq's highest child
	m, _ := filepath.Glob("/sys/class/net/" + dev + "/queues/tx-*")
	n := len(m)
	if n == 0 {
		for _, c := range regexp.MustCompile(` parent `+r[1]+`:([0-9a-f]+)`).FindAllStringSubmatch(all, -1) {
			if v, err := strconv.ParseUint(c[1], 16, 16); err == nil {
				n = max(n, int(v))
			}
		}
	}
	if n < 2 {
		return fail("%d TX queue", n)
	}
	parent := fmt.Sprintf("%s:%x", r[1], n)
	if !strings.Contains(all, "qdisc clsact ffff:") {
		if strings.Contains(all, "qdisc ingress ffff:") {
			return fail("an ingress qdisc is set up there")
		}
		if _, err := qTC("qdisc", "add", "dev", dev, "clsact"); err != nil {
			return fail("%v", err)
		}
	}
	// the action (act_skbedit with queue_mapping), on a prio and an address
	// no link uses, before anything is grafted on the mq child
	err = qTCBatch(fmt.Sprintf("filter add dev %s egress protocol ip prio 65000 u32 match ip dst 127.255.255.254/32 action skbedit queue_mapping %d priority %s:1\n", dev, n-1, qHandle))
	qTC("filter", "del", "dev", dev, "egress", "prio", "65000")
	if err != nil {
		return fail("%v", err)
	}
	child, _ := qTC("qdisc", "show", "dev", dev, "parent", parent)
	if strings.Contains(child, "htb "+qHandle+":") {
		for _, c := range qClasses(dev) {
			if c < 2 {
				continue
			}
			if l := qTrySlot(dev, c); l != nil && !s.adoptStale(l, c, dev, dev) { // its process is gone
				qDropFilters(dev, true, c)
				qDropClass(dev, c)
				l.Close()
			}
		}
	} else {
		// the one graft on the mq child, now, while nothing is steered to it
		if _, err := qTC("qdisc", "replace", "dev", dev, "parent", parent, "handle", qHandle+":", "htb", "default", "1"); err != nil {
			return fail("%v", err)
		}
		if _, err := qTC("class", "replace", "dev", dev, "parent", qHandle+":", "classid", qHandle+":1", "htb", "rate", "40gbit"); err == nil {
			qTC("qdisc", "replace", "dev", dev, "parent", qHandle+":1", "fq_codel")
		}
	}
	s.mqParent, s.mqQueue = parent, n-1
	return true
}

// qRootTreeGone (rel69e): every hs2 class on dev's root htb belongs to a
// process that is gone (its slot lock is free); under the tree lock.
func qRootTreeGone(dev string) bool {
	for _, c := range qClasses(dev) {
		if c < 2 {
			continue
		}
		l := qTrySlot(dev, c)
		if l == nil {
			return false // a live process (root mode) holds it: share its tree
		}
		l.Close()
	}
	return true
}

// adoptStale (rel69h): the first stale class found (its process gone, l its
// lock now held here) becomes this shaper's slot, class and leaf kept, only
// its filters dropped (egressDev: the clsact egress filters' device, "" for
// none; classDev: the htb's device) — placeClass then changes the class in
// place and keeps an identical cake leaf, so a restart re-creates nothing.
// false: this shaper has its slot already, the caller drops the class.
func (s *qShaper) adoptStale(l *os.File, minor int, egressDev, classDev string) bool {
	if s.slot != nil || minor > qMaxMinor {
		return false
	}
	if egressDev != "" {
		qDropFilters(egressDev, true, minor)
	}
	qDropFilters(classDev, false, minor)
	s.slot, s.minor = l, minor
	return true
}

// placeClass takes a class slot (lock) and puts this process's class and its
// leaf on qdev (dev itself, or dev's ifb); under the tree lock.
func (s *qShaper) placeClass(dev, qdev string, rate float64) bool {
	for m := 2; m <= qMaxMinor && s.slot == nil; m++ {
		if l := qTrySlot(dev, m); l != nil {
			s.slot, s.minor = l, m
		}
	}
	s.dev, s.qdev = dev, qdev
	if s.slot == nil {
		s.dropRootIfLast()
		return s.refuse("no free class on %s", dev)
	}
	s.installed = true
	cls := qClassID(s.minor)
	if _, err := qTC(append([]string{"class", "replace", "dev", qdev, "parent", qHandle + ":", "classid", cls, "htb"}, qHtbArgs(rate)...)...); err != nil {
		s.removeLocked()
		return s.refuse("%v", err)
	}
	keep, drop := qLeafCakeSame(qdev, cls)
	if !keep {
		if _, err := qTC("qdisc", "replace", "dev", qdev, "parent", cls, "cake", "besteffort", qCakeFlows); err != nil {
			if _, err := qTC("qdisc", "replace", "dev", qdev, "parent", cls, "fq_codel"); err != nil { // no cake module
				s.removeLocked()
				return s.refuse("%v", err)
			}
		}
	}
	s.rate, s.peers, s.drop, s.steer, s.lost = rate, nil, drop, false, false
	return true
}

// qLeafCakeSame (rel69h): the leaf under our own class cls on qdev is already
// a cake with our parameters (besteffort, qCakeFlows) — left by this slot's
// previous holder (a restart that skipped remove). Then it is kept (no leaf
// re-create on every start) and drop is its dropped counter now, the base for
// stats(). Any doubt (tc error or timeout, other qdisc, other mode): false,
// and placeClass replaces the leaf as before.
func qLeafCakeSame(qdev, cls string) (bool, uint64) {
	out, err := qTC("-s", "qdisc", "show", "dev", qdev, "parent", cls)
	if err != nil {
		return false, 0
	}
	line, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	f := strings.Fields(line)
	if len(f) < 5 || f[0] != "qdisc" || f[1] != "cake" || f[3] != "parent" || f[4] != cls ||
		!slices.Contains(f, "besteffort") || !slices.Contains(f, qCakeFlows) {
		return false, 0
	}
	var d uint64
	if m := qDroppedRe.FindStringSubmatch(out); m != nil {
		d, _ = strconv.ParseUint(m[1], 10, 64)
	}
	return true, d
}

func (s *qShaper) refuse(f string, a ...any) bool {
	s.refused = true
	s.logf("queue: cannot shape the tunnel's egress here: "+f+" — the path's queue is left as it is", a...)
	return false
}

var qDroppedRe = regexp.MustCompile(`\(dropped (\d+),`)

// park (hl233, HS2_QUEUE_CTL_KEEP): the shaper stays in place with no cap
// (the class at qParkRate) instead of coming off, and goes in so while the
// links are light. Installing swaps the interface's root qdisc: the kernel
// drops what is sent during the swap (dev_deactivate: noop qdisc until an RCU
// grace period ends). Lab hl232 fS2 d1a: the first cap's install at 476 Mbit/s
// under a 149 ms queue lost 0.04 % of that tick's packets (the only S run with
// loss on that tick) and the call's link waited one round trip (~300 ms) for
// its retransmit: call max gap 320 ms, every other S phase 28-56 ms. A cap
// that comes and goes swapped the root each time (remove: qdisc del root).
func (s *qShaper) park(peers []qKey) bool {
	if s.refused {
		return false
	}
	if !s.installed && len(peers) == 0 {
		return true
	}
	if len(peers) == 0 {
		peers = s.peers
	}
	// ifb mode: the redirect stays for the first park tick, and until the
	// leaf has drained (the class is at the park rate by then), so no link
	// packet overtakes its own queued ones; then the links take dev's own path.
	steer := s.steer && !(s.parked && s.backlog() == 0)
	if !s.set(qParkRate, peers, steer) {
		return false
	}
	s.parked = true
	return true
}

// backlog: the leaf's backlog in bytes now.
func (s *qShaper) backlog() uint64 {
	if !s.installed {
		return 0
	}
	out, err := qTC("-s", "qdisc", "show", "dev", s.qdev, "parent", qClassID(s.minor))
	if err != nil {
		s.suspect = !errors.Is(err, errQTimeout) // fx247: checked next tick
		return 0
	}
	return qBacklogOf(out)
}

func (s *qShaper) placed() bool { return s.installed }

// drops is how many packets the shaper's own leaf dropped since the last call
// (0 when it is not in place): each comes back as a retransmit that the cap,
// not the path, made.
func (s *qShaper) drops() uint64 {
	d, _ := s.stats()
	return d
}

// stats: drops (as drops) and the leaf's backlog in bytes now (hl234: the
// queue in the tunnel's own shaper, not the path's; 0 parked or not in place).
func (s *qShaper) stats() (uint64, uint64) {
	if !s.installed {
		return 0, 0
	}
	out, err := qTC("-s", "qdisc", "show", "dev", s.qdev, "parent", qClassID(s.minor))
	if err != nil {
		s.suspect = !errors.Is(err, errQTimeout) // fx247: checked next tick
		return 0, 0
	}
	bl := qBacklogOf(out)
	m := qDroppedRe.FindStringSubmatch(out)
	if m == nil {
		return 0, bl
	}
	n, _ := strconv.ParseUint(m[1], 10, 64)
	d := uint64(0)
	if s.parked {
		s.drop = n // hl233: with no cap a leaf drop is the interface's, not the cap's
		return 0, 0
	}
	if n >= s.drop {
		d = n - s.drop
	}
	s.drop = n
	return d, bl
}

var qBacklogRe = regexp.MustCompile(`backlog (\d+(?:\.\d+)?)([KM]?)b `)

// qBacklogOf: the first "backlog N[K|M]b" of tc -s output (tc's sprint_size:
// K/M are 1024 units), in bytes; 0 if none.
func qBacklogOf(out string) uint64 {
	m := qBacklogRe.FindStringSubmatch(out)
	if m == nil {
		return 0
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0
	}
	switch m[2] {
	case "K":
		v *= 1024
	case "M":
		v *= 1024 * 1024
	}
	return uint64(v)
}

// remove takes this process's class away, and the root with the last class
// (the kernel's default root qdisc comes back).
func (s *qShaper) remove() {
	if !s.installed {
		return
	}
	unlock, ok := qTreeLock()
	if !ok {
		s.skipLog.logf(s.logf, "queue: the tc tree lock not had in %v, remove retried next tick (fx247)", qLockWait)
		return
	}
	defer unlock()
	s.removeLocked()
	s.logf("queue: shaper on %s removed", s.dev)
}

func (s *qShaper) removeLocked() {
	if s.steers() {
		qDropFilters(s.dev, true, s.minor)
	}
	qDropClass(s.qdev, s.minor)
	s.dropRootIfLast()
	if s.slot != nil {
		s.slot.Close()
	}
	s.slot, s.installed, s.peers, s.rate, s.drop, s.parked, s.steer = nil, false, nil, 0, 0, false, false
}

// zduRelease (ZDU B6) gives this process's class slot up for the drain: its
// filters, its class and the slot's flock go, under the tree lock, but never
// the shared root (or the ifb / mq child tree): the new process shapes on
// it, and a root swap under its links is a graft. A root left with no hs2
// class is reused by the new process's install, or taken off by its remove.
// The shaper then refuses: this process never installs again.
func (s *qShaper) zduRelease() {
	s.refused = true
	if !s.installed && s.slot == nil {
		return
	}
	if unlock, ok := qTreeLock(); ok {
		defer unlock()
	} else {
		s.logf("queue: ZDU drain: the tc tree lock not had in %v, class released without it", qLockWait)
	}
	if s.installed {
		if s.steers() {
			qDropFilters(s.dev, true, s.minor)
		}
		qDropClass(s.qdev, s.minor)
	}
	if s.slot != nil {
		s.slot.Close()
	}
	s.slot, s.installed, s.peers, s.rate, s.drop, s.parked, s.steer = nil, false, nil, 0, 0, false, false
}

// dropRootIfLast takes the shared root off when no hs2 class is left on it.
func (s *qShaper) dropRootIfLast() {
	for _, m := range qClasses(s.qdev) {
		if m >= 2 && (m != s.minor || !s.installed) {
			return
		}
	}
	if s.mqc {
		// pk239: the tree stays on the mq child (default class only) and
		// clsact on dev: taking either off is a graft (drops), and a restart
		// reuses them with no graft at all
		return
	}
	if s.ifb {
		// the ifb goes with the last class; dev's clsact stays (an empty
		// clsact costs ~nothing; taking it off is a graft on dev, pk237)
		if s.qdev != s.dev {
			qExec("", true, qIPBin, "link", "del", "dev", s.qdev)
		}
		return
	}
	if out, _ := qTC("qdisc", "show", "dev", s.dev, "root"); !strings.Contains(out, "htb "+qHandle+":") {
		return
	}
	if _, err := qTC("qdisc", "del", "dev", s.dev, "root"); err != nil {
		s.logf("queue: %v", err)
	}
}

// qLinkDev is the interface a link's packets leave by: the route to its peer
// from its own local address (an edge's lb addresses can each have a route of
// their own); "" when not known.
func qLinkDev(tc *net.TCPConn) string {
	if tc == nil {
		return ""
	}
	r, err1 := netip.ParseAddrPort(tc.RemoteAddr().String())
	l, err2 := netip.ParseAddrPort(tc.LocalAddr().String())
	if err1 != nil || err2 != nil {
		return ""
	}
	out, err := qExec("", false, qIPBin, "route", "get", r.Addr().Unmap().String(), "from", l.Addr().Unmap().String())
	if err != nil {
		return qRouteDev(r.Addr().Unmap())
	}
	f := strings.Fields(string(out))
	for i := 0; i+1 < len(f); i++ {
		if f[i] == "dev" {
			return f[i+1]
		}
	}
	return ""
}

// qCakeFlows: cake's flow mode under the cap. "flows" (the default) shares
// the cap evenly over the tunnel's links — every link is one flow of the
// same host pair, so with ~150 links one download's link gets 1/150 of the
// cap (production-like lab: a single-stream download 1-3 Mbit/s while
// capped, 50-90 otherwise). HS2_QUEUE_CTL_FLOWBLIND=1 (default off): one
// queue under cake's AQM — each link takes what its own TCP asks for, a
// light link's packets wait at most cake's short standing queue (~5 ms
// target) instead of none.
var qCakeFlows = func() string {
	if os.Getenv("HS2_QUEUE_CTL_FLOWBLIND") == "1" {
		return "flowblind"
	}
	return "flows"
}()

// qLinkSport (m4f251) is the source port an accepted link's packets carry on
// the wire: our listen port, or, when the connection was redirected to it by
// netfilter (REDIRECT/DNAT from another port), the port the peer dialed —
// the reply's source is translated back before the egress qdisc sees it.
// SO_ORIGINAL_DST says which (it fails when the connection was not NATed or
// conntrack is not loaded: the local port then). 0: not known.
func qLinkSport(tc *net.TCPConn) uint16 {
	if tc == nil {
		return 0
	}
	l, err := netip.ParseAddrPort(tc.LocalAddr().String())
	if err != nil {
		return 0
	}
	port := l.Port()
	rc, err := tc.SyscallConn()
	if err != nil {
		return port
	}
	rc.Control(func(fd uintptr) {
		// sockaddr_in / sockaddr_in6 (port big-endian at bytes 2-3), read
		// through a getsockopt wrapper with a large enough buffer
		const soOriginalDst = 80 // SO_ORIGINAL_DST, IP6T_SO_ORIGINAL_DST
		if l.Addr().Unmap().Is4() {
			if m, err := syscall.GetsockoptIPv6Mreq(int(fd), syscall.SOL_IP, soOriginalDst); err == nil {
				if p := uint16(m.Multiaddr[2])<<8 | uint16(m.Multiaddr[3]); p != 0 {
					port = p
				}
			}
		} else if m, err := syscall.GetsockoptIPv6MTUInfo(int(fd), syscall.SOL_IPV6, soOriginalDst); err == nil {
			var b [2]byte // RawSockaddrInet6.Port holds the network-order bytes
			binary.NativeEndian.PutUint16(b[:], m.Addr.Port)
			if p := binary.BigEndian.Uint16(b[:]); p != 0 {
				port = p
			}
		}
	})
	return port
}
