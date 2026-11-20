//go:build linux

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/encap"
	"github.com/hosseintaghipoursori-alt/hs2-tunnel/engine"
	"github.com/hosseintaghipoursori-alt/hs2-tunnel/handoff"
	"github.com/hosseintaghipoursori-alt/hs2-tunnel/sdnotify"
	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tun"
)

// ZDU slot S6: the takeover path in the running daemon (proposals/zdu350.md
// §7 S6). Everything here is behind HS2_ZDU=1: with the key off zduStart
// returns at once, no SIGUSR2 handler is installed (SIGUSR2 keeps Go's
// default: the process dies, exactly like rel69p), no socket, no file.
//
// Old process (SIGUSR2):
//  1. refuse inside HH:00-HH:03:59 UTC (engine.ZDURefused) and when a
//     handover is already running;
//  2. read /run/hs2/UNIT.next (root-owned regular file, not group/other
//     writable, no symlink): line 1 the new binary (absolute, root-owned,
//     not group/other writable, executable), then KEY=VALUE env lines;
//  3. socketpair; spawn the new binary with the same argv, the unit's
//     environment plus the UNIT.next env and HS2_HANDOVER_FD=3;
//  4. Give: HELLO, FDS (every TCP listener + the TUN), wait READY, GO;
//  5. pause accept, sd_notify MAINPID=<new> + READY=1, UNIT.handover
//     "PROBATION pid";
//  6. probation (45 s): the new process exiting, or the channel breaking,
//     = REVERTED: resume accept on the kept fds, MAINPID=<self>, kill the
//     child, UNIT.handover "REVERTED reason";
//  7. after probation: UNIT.handover "OK pid", drain: DRAINSTAT every 2 s,
//     exit at the deadline (engine.ZDUDeadline, next HH:03 or
//     HS2_ZDU_DRAIN) or when no link/user is left.
//
// New process (HS2_HANDOVER_FD set): handoff.TakeOrCold before the engine
// starts (fds adopted by name: "tcp:ADDR" listeners, "tun:NAME"); the first
// READY (S2 notifyReady) sends the hand-off READY and waits for GO, then
// follows DRAINSTAT into the Iran foreign-drain term.

const (
	zduProbation   = 45 * time.Second
	zduDrainPeriod = 2 * time.Second
	// zduDrainFloor: the drain never ends sooner than this after the commit
	// (OK). The deadline counts from SIGUSR2 and includes the probation, so a
	// short HS2_ZDU_DRAIN (lab drill b1: 40 s < 45 s probation) made the old
	// process exit right at OK and cut every call it still carried.
	zduDrainFloor = 60 * time.Second
)

// zduHungAfter (review 4 low): a probation accept queue that stays non-empty
// this long = a hung new process (a live one empties it within milliseconds).
const zduHungAfter = 10 * time.Second

// zduDrainEnd returns the effective drain end: deadline, but never before
// max(start+probation, ok) + floor. clamped reports a raise.
func zduDrainEnd(deadline, start, ok time.Time, probation, floor time.Duration) (end time.Time, clamped bool) {
	min := start.Add(probation)
	if ok.After(min) {
		min = ok
	}
	min = min.Add(floor)
	if deadline.Before(min) {
		return min.UTC(), true
	}
	return deadline, false
}

// Counters, also on the takeover log lines.
var (
	zduHOStarted   atomic.Int64
	zduHORefused   atomic.Int64
	zduHOFailed    atomic.Int64
	zduHOReverted  atomic.Int64
	zduHOCommitted atomic.Int64
	zduTakeAdopted atomic.Int64
	zduTakeFailed  atomic.Int64 // a hand-off child's takeover failed (exit 1, never a cold start)
)

func zduCounters() string {
	return fmt.Sprintf("zdu_ho_started=%d zdu_ho_refused=%d zdu_ho_failed=%d zdu_ho_reverted=%d zdu_ho_ok=%d zdu_take_adopted=%d zdu_take_failed=%d zdu_late_accepts=%d",
		zduHOStarted.Load(), zduHORefused.Load(), zduHOFailed.Load(), zduHOReverted.Load(),
		zduHOCommitted.Load(), zduTakeAdopted.Load(), zduTakeFailed.Load(), engine.ZDULateAccepts.Load())
}

// zduUnit is the unit name the /run/hs2 files are keyed on: HS2_UNIT, else
// the systemd unit from /proc/self/cgroup, else "hs2".
func zduUnit() string {
	if u := os.Getenv("HS2_UNIT"); u != "" {
		return strings.TrimSuffix(u, ".service")
	}
	if b, err := os.ReadFile("/proc/self/cgroup"); err == nil {
		for _, ln := range strings.Split(string(b), "\n") {
			if i := strings.LastIndex(ln, "/"); i >= 0 && strings.HasSuffix(ln, ".service") {
				return strings.TrimSuffix(ln[i+1:], ".service")
			}
		}
	}
	return "hs2"
}

// zduNext is a validated UNIT.next.
type zduNext struct {
	Bin string
	Env []string
}

// zduOwnerOK: uid 0 (or want, for tests), not group/other writable.
var zduRootUID = uint32(0)

func zduSafeFile(path string, wantExec bool) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink", path)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != zduRootUID {
		return fmt.Errorf("%s is not owned by root", path)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s is group/other writable (%v)", path, fi.Mode().Perm())
	}
	if wantExec && fi.Mode().Perm()&0o100 == 0 {
		return fmt.Errorf("%s is not executable", path)
	}
	return nil
}

func zduReadNext(path string) (zduNext, error) {
	var n zduNext
	if err := zduSafeFile(path, false); err != nil {
		return n, err
	}
	f, err := os.Open(path)
	if err != nil {
		return n, err
	}
	defer f.Close()
	sc := bufio.NewScanner(io4k(f))
	for sc.Scan() {
		ln := strings.TrimSpace(sc.Text())
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		if n.Bin == "" {
			n.Bin = ln
			continue
		}
		k, _, ok := strings.Cut(ln, "=")
		if !ok || k == "" || strings.ContainsAny(k, " \t") {
			return zduNext{}, fmt.Errorf("%s: bad env line %q", path, ln)
		}
		if k == "HS2_HANDOVER_FD" {
			return zduNext{}, fmt.Errorf("%s: HS2_HANDOVER_FD may not be set", path)
		}
		n.Env = append(n.Env, ln)
	}
	if err := sc.Err(); err != nil {
		return zduNext{}, err
	}
	if n.Bin == "" || !filepath.IsAbs(n.Bin) || filepath.Clean(n.Bin) != n.Bin {
		return zduNext{}, fmt.Errorf("%s: binary path %q must be absolute and clean", path, n.Bin)
	}
	if err := zduSafeFile(n.Bin, true); err != nil {
		return zduNext{}, err
	}
	return n, nil
}

// io4k caps UNIT.next at 4 KiB.
func io4k(f *os.File) *bufio.Reader { return bufio.NewReaderSize(&limitR{f, 4096}, 4096) }

type limitR struct {
	f *os.File
	n int
}

func (l *limitR) Read(p []byte) (int, error) {
	if l.n <= 0 {
		return 0, errors.New("UNIT.next larger than 4 KiB")
	}
	if len(p) > l.n {
		p = p[:l.n]
	}
	k, err := l.f.Read(p)
	l.n -= k
	return k, err
}

// zduChild is the spawned new process.
type zduChild interface {
	Pid() int
	Done() <-chan struct{} // closed when it exited
	Kill()
}

type execChild struct {
	cmd  *exec.Cmd
	done chan struct{}
}

func (c *execChild) Pid() int              { return c.cmd.Process.Pid }
func (c *execChild) Done() <-chan struct{} { return c.done }
func (c *execChild) Kill()                 { c.cmd.Process.Kill() }

func zduSpawnExec(bin string, env []string, ch *os.File) (zduChild, error) {
	cmd := exec.Command(bin, os.Args[1:]...)
	cmd.Env = env
	cmd.Stdin = nil
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.ExtraFiles = []*os.File{ch} // fd 3
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c := &execChild{cmd: cmd, done: make(chan struct{})}
	go func() { cmd.Wait(); close(c.done) }()
	return c, nil
}

// zduMergeEnv: base with the UNIT.next overrides, HS2_HANDOVER_FD=3 last.
func zduMergeEnv(base, over []string) []string {
	idx := map[string]int{}
	out := make([]string, 0, len(base)+len(over)+1)
	add := func(kv string) {
		k, _, _ := strings.Cut(kv, "=")
		if k == "HS2_HANDOVER_FD" {
			return
		}
		if i, ok := idx[k]; ok {
			out[i] = kv
			return
		}
		idx[k] = len(out)
		out = append(out, kv)
	}
	for _, kv := range base {
		add(kv)
	}
	for _, kv := range over {
		add(kv)
	}
	return append(out, "HS2_HANDOVER_FD=3")
}

// zduOld is the old process's takeover machine; every side effect is a
// field so the tests drive each step and failure path.
type zduOld struct {
	unit      string
	runDir    string
	now       func() time.Time
	override  time.Duration
	probation time.Duration
	floor     time.Duration // zduDrainFloor: least drain after OK
	period    time.Duration
	ready     time.Duration
	spawn     func(bin string, env []string, ch *os.File) (zduChild, error)
	notify    func(state string) (bool, error)
	fds       func() ([]handoff.FD, error)
	pause     func() int
	resume    func() int
	stats     func() (links, users int)
	readsOff  func() string // S7: stop TUN reads at commit ("" = no TUN)
	drainMode func() string // S7 B6: drain mode at commit (advert, status writer off, GOMEMLIMIT)
	// peers (review F2): every live link's peer announced capExtZDU and an
	// instance (engine.ZDUPeerCheck), with the status line; nil = no check.
	peers func() (bool, string)
	// Review issue 4 (zdu_gate.go), nil = none: stop/resume accept on the
	// link listeners only (handover start / failure), and the boot boost of
	// the Iran pools' poolctl target (returns boost, idle links closed).
	linkPause, linkResume func() int
	boost                 func(on bool) (int, int)
	// linkDrop (review M2, nil = none): at the commit, close this process's
	// copies of the link listeners (one the new process did not bind then
	// refuses dials instead of holding them paused through the drain).
	linkDrop func() int
	// direct (review H1): this side runs in direct (non-reverse) mode, where
	// only Iran dials and a live handover is not designed yet: refused.
	direct bool
	// l3Quiet (issue 6): no TUN packet came from a link for the grace.
	l3Quiet func(now time.Time) (bool, time.Duration)
	exit    func()
	// exitFail (review 4 H1, nil = exit): the new process died during the
	// drain. The default sends MAINPID=<self> and exits 1, so systemd (249 has
	// no pidfd and cannot see the alien MAINPID die) restarts the unit cold
	// at once instead of after the drain hour.
	exitFail func(why string)
	// parentDraining (review 4 M1, nil = none): this process is a hand-off
	// child whose parent still drains: a new handover is refused (3
	// generations at once were never designed nor tested).
	parentDraining func() (pid int, draining bool)
	hello          func() handoff.Hello
	// hups / hupChild (review 4 low, nil = none): a reload (SIGHUP, sent to
	// MAINPID = this process) between the spawn and GO never reached the new
	// process; after MAINPID moved it is forwarded once.
	// acceptQ (review 4 low, nil = none): the kernel accept-queue length of
	// our (paused) listeners. In probation only the new process accepts, so a
	// queue that stays non-empty for hungAfter means it is alive but hung:
	// REVERTED (the alive + channel check alone passed a hung child).
	acceptQ   func() int
	hungAfter time.Duration
	hups      func() int64
	hupChild  func(pid int) error
	logf      func(string, ...any)
	busy      atomic.Bool
	// drainDone (tests) is closed when the drain loop returns.
	drainDone chan struct{}
}

func (o *zduOld) status(s string) {
	p := filepath.Join(o.runDir, o.unit+".handover")
	// b396 (rel69q sec370): the status writer's atomic owner-only write (a
	// random O_EXCL temp, so old and new never share one: review L3; 0600,
	// 0644 with HS2_RUN_SHARED=1). hs2-zdu-upgrade.sh reads it as root.
	writeFileAtomicMode(p, []byte(s+"\n"), runFileMode())
}

// zduDirect (review H1): set by main from the config (!reverse) before
// zduStart; a direct-mode process refuses every handover.
var zduDirect bool

// zduDirectLine is UNIT.peers in direct mode (the script refuses on it).
const zduDirectLine = "ZDU-DIRECT mode=direct: live handover needs reverse mode"

// zduSpanMargin: slack on top of the warm wait and the probation for the
// end-of-hour refusal.
const zduSpanMargin = 15 * time.Second

// zduPeersPeriod: how often UNIT.peers is rewritten.
var zduPeersPeriod = 5 * time.Second

// writePeers writes the peer check (review F2) to RUNDIR/UNIT.peers for
// hs2-zdu-upgrade.sh: "ZDU-OK links=N zdu=N" or "ZDU-NO links=N zdu=M". Only
// the current process writes it (a draining old one does not).
func (o *zduOld) writePeers() {
	if o.peers == nil || engine.ZDUDraining() {
		return
	}
	_, line := o.peers()
	if o.direct {
		line = zduDirectLine
	}
	p := filepath.Join(o.runDir, o.unit+".peers")
	writeFileAtomicMode(p, []byte(line+"\n"), runFileMode()) // b396: as status (sec370); random temp: review L3
}

// handover runs one takeover. It returns once the old process has either
// resumed (refused, failed, reverted) or committed (then drain runs until
// exit is called).
func (o *zduOld) handover() (result string) {
	start := o.now()
	if o.direct {
		zduHORefused.Add(1)
		o.logf("zdu: handover refused: direct (non-reverse) mode: only Iran dials there, so the new Kharej cannot get its own links and the reconcile retires them; live handover is supported in reverse mode only (use a cold restart, hs2-safe-deploy.sh) (%s)", zduCounters())
		return "refused-direct"
	}
	if o.parentDraining != nil {
		if pp, d := o.parentDraining(); d {
			zduHORefused.Add(1)
			o.logf("zdu: handover refused: the previous handover's old process (pid %d) still drains; wait for its \"drain end\" (%s)", pp, zduCounters())
			return "refused-draining"
		}
	}
	if engine.ZDURefused(start) {
		zduHORefused.Add(1)
		o.logf("zdu: handover refused: %v (%s)", engine.ErrZDUWindow, zduCounters())
		return "refused-window"
	}
	// Review minor: READY + probation must end before the window opens; a
	// SIGUSR2 at HH:58 would commit inside the storm. Review L1: READY may
	// take the whole READY timeout (child exec + start + warm wait), not
	// only the warm wait.
	if span := max(o.ready, zduWarmMax) + o.probation + zduSpanMargin; engine.ZDURefusedSpan(start, span) {
		zduHORefused.Add(1)
		o.logf("zdu: handover refused: %v: READY + probation (%v) would reach the window (%s)", engine.ErrZDUWindow, span, zduCounters())
		return "refused-window"
	}
	if !o.busy.CompareAndSwap(false, true) {
		zduHORefused.Add(1)
		o.logf("zdu: handover refused: one is already running")
		return "refused-busy"
	}
	committed := false
	defer func() {
		if !committed {
			o.busy.Store(false)
		}
	}()
	deadline, err := engine.ZDUDeadline(start, o.override)
	if err != nil {
		zduHORefused.Add(1)
		o.logf("zdu: handover refused: %v", err)
		return "refused-window"
	}
	if o.peers != nil {
		if ok, line := o.peers(); !ok {
			zduHORefused.Add(1)
			o.logf("zdu: handover refused: peers: %s: a peer without ZDU (capExtZDU + instance) has no generation pin (%s)", line, zduCounters())
			return "refused-peers"
		}
	}
	next, err := zduReadNext(filepath.Join(o.runDir, o.unit+".next"))
	if err != nil {
		zduHORefused.Add(1)
		o.logf("zdu: handover refused: UNIT.next: %v (%s)", err, zduCounters())
		return "refused-next"
	}
	zduHOStarted.Add(1)
	o.logf("zdu: handover start: new binary %s, drain deadline %s", next.Bin, deadline.UTC().Format(time.RFC3339))
	if _, c := zduDrainEnd(deadline, start, start, o.probation, o.floor); c {
		o.logf("zdu: warning: drain deadline %s is less than probation %v + %v after SIGUSR2 (HS2_ZDU_DRAIN too short); it will be raised at OK",
			deadline.UTC().Format(time.RFC3339), o.probation, o.floor)
	}
	booted := false
	unboot := func() {
		if !booted {
			return
		}
		booted = false
		if o.linkResume != nil {
			o.linkResume()
		}
		if o.boost != nil {
			o.boost(false)
		}
	}
	fail := func(why string, ch zduChild) string {
		zduHOFailed.Add(1)
		if ch != nil {
			ch.Kill()
		}
		unboot() // links: accept again here, poolctl target back
		o.status("REVERTED " + why)
		o.logf("zdu: handover failed, still serving (nothing paused): %s (%s)", why, zduCounters())
		return "failed"
	}
	oldEnd, newEnd, err := handoff.Pair()
	if err != nil {
		return fail("socketpair: "+err.Error(), nil)
	}
	defer func() {
		if !committed {
			oldEnd.Close()
		}
	}()
	nf, err := newEnd.File()
	newEnd.Close()
	if err != nil {
		return fail("socketpair file: "+err.Error(), nil)
	}
	var hup0 int64
	if o.hups != nil {
		hup0 = o.hups()
	}
	child, err := o.spawn(next.Bin, zduMergeEnv(os.Environ(), next.Env), nf)
	nf.Close()
	if err != nil {
		return fail("spawn: "+err.Error(), nil)
	}
	o.logf("zdu: handover: new process pid %d spawned", child.Pid())
	// Review issue 4: from boot on every new link lands on the new process
	// (only it accepts on the shared link listener); the Iran pools ask the
	// exit for that many more links (reverse bootstrap). The new process
	// serves no user before GO. Review M2: boot runs once the new process
	// has its listeners and adopted the fds (LISTENING), not at the spawn:
	// a child that hangs before its engine listens leaves link accept here.
	boot := func() {
		booted = true
		var lp, bst, bcl int
		if o.linkPause != nil {
			lp = o.linkPause()
		}
		if o.boost != nil {
			bst, bcl = o.boost(true)
		}
		if lp+bst+bcl > 0 {
			o.logf("zdu: handover: new process listening: link accept moved to it (%d link listeners paused here), poolctl boot +%d, %d idle links closed (exit pool at its ceiling)", lp, bst, bcl)
		}
	}
	fds, err := o.fds()
	if err == nil && len(fds) == 0 {
		err = errors.New("nothing to hand over (no listener, no tun)")
	}
	if err != nil {
		return fail("fds: "+err.Error(), child)
	}
	// A child that dies before READY must not leave Give waiting 90 s.
	go func() {
		select {
		case <-child.Done():
			oldEnd.Close()
		case <-time.After(o.ready + 10*time.Second):
		}
	}()
	t0 := time.Now()
	_, err = oldEnd.GiveListening(o.hello(), fds, handoff.StepTimeout, o.ready, boot)
	for _, f := range fds {
		f.File.Close() // dups; ours stay open (the revert)
	}
	if err != nil {
		return fail("give: "+err.Error(), child)
	}
	select {
	case <-child.Done():
		return fail("new process exited at GO", child)
	default:
	}
	n := o.pause()
	o.notify(fmt.Sprintf("MAINPID=%d\nREADY=1\nSTATUS=hs2 handover to pid %d (probation)", child.Pid(), child.Pid()))
	o.status(fmt.Sprintf("PROBATION %d", child.Pid()))
	o.logf("zdu: handover GO to pid %d after %v: %d listeners paused, MAINPID=%d sent, probation %v",
		child.Pid(), time.Since(t0).Round(time.Millisecond), n, child.Pid(), o.probation)
	// Review 4 low: a reload between the spawn and GO reached only us; its
	// handlers are installed in the child by now (READY comes after them).
	if o.hups != nil && o.hupChild != nil {
		if h := o.hups(); h != hup0 {
			err := o.hupChild(child.Pid())
			o.logf("zdu: handover: %d reload(s) (SIGHUP) came between the spawn and GO: forwarded to pid %d (err %v)", h-hup0, child.Pid(), err)
		}
	}

	// Probation: the new process alive and its channel open. DRAINSTAT every
	// period doubles as the channel health check (a dead peer = EPIPE).
	why := o.watch(oldEnd, child, o.probation)
	if why != "" {
		zduHOReverted.Add(1)
		o.resume()
		unboot()
		self := os.Getpid()
		o.notify(fmt.Sprintf("MAINPID=%d\nREADY=1\nSTATUS=hs2 handover reverted: %s", self, why))
		child.Kill()
		o.status("REVERTED " + why)
		o.logf("zdu: handover REVERTED in probation: %s; accept resumed on the kept fds, MAINPID=%d (%s)", why, self, zduCounters())
		return "reverted"
	}
	committed = true
	zduHOCommitted.Add(1)
	if o.boost != nil && booted {
		o.boost(false) // the new process drives the pool now (drain mode sends no target)
	}
	if end, c := zduDrainEnd(deadline, start, o.now(), o.probation, o.floor); c {
		o.logf("zdu: drain deadline clamped from %s to %s: at least %v after OK (probation %v counts from SIGUSR2; HS2_ZDU_DRAIN too short)",
			deadline.UTC().Format(time.RFC3339), end.Format(time.RFC3339), o.floor, o.probation)
		deadline = end
	}
	// S7 TUN read off: from here the new process alone takes packets from
	// the shared TUN queue (it has had its links since READY and passed
	// probation), so none is lost when this process exits. The drain keeps
	// the conn data path: user sockets, links, and TUN writes of what the
	// draining links still deliver.
	if o.readsOff != nil {
		if tn := o.readsOff(); tn != "" {
			o.logf("zdu: drain: stopped reading TUN %s (the new process takes every packet; writes continue)", tn)
		}
	}
	if o.linkDrop != nil && booted {
		if n := o.linkDrop(); n > 0 {
			o.logf("zdu: drain: closed this process's copies of %d link listeners (the new process holds the ones it binds; any other refuses dials now)", n)
		}
	}
	if o.drainMode != nil {
		if s := o.drainMode(); s != "" {
			o.logf("zdu: drain: %s", s)
		}
	}
	o.status(fmt.Sprintf("OK %d", child.Pid()))
	o.logf("zdu: handover OK: pid %d passed probation; draining until %s (%s)",
		child.Pid(), deadline.UTC().Format(time.RFC3339), zduCounters())
	go o.drain(oldEnd, child, deadline)
	return "ok"
}

// watch sends DRAINSTAT every period for d; "" = healthy for all of d.
func (o *zduOld) watch(c *handoff.Conn, child zduChild, d time.Duration) string {
	end := time.NewTimer(d)
	defer end.Stop()
	tk := time.NewTicker(o.period)
	defer tk.Stop()
	var qSince time.Time // first sample of the current non-empty accept-queue run
	for {
		select {
		case <-child.Done():
			return "new process exited"
		case <-end.C:
			return ""
		case <-tk.C:
			l, u := o.stats()
			if err := c.SendDrainStat(handoff.DrainStat{Links: uint32(l), Users: uint32(u)}); err != nil {
				return "channel: " + err.Error()
			}
			if o.acceptQ != nil && o.hungAfter > 0 {
				if q := o.acceptQ(); q > 0 {
					if qSince.IsZero() {
						qSince = time.Now()
					} else if time.Since(qSince) >= o.hungAfter {
						return fmt.Sprintf("new process not accepting: accept queue %d non-empty for %v", q, time.Since(qSince).Round(time.Second))
					}
				} else {
					qSince = time.Time{}
				}
			}
		}
	}
}

func (o *zduOld) drain(c *handoff.Conn, child zduChild, deadline time.Time) {
	defer func() {
		if o.drainDone != nil {
			close(o.drainDone)
		}
	}()
	tk := time.NewTicker(o.period)
	defer tk.Stop()
	t0 := o.now()
	l3Held := false
	chanOK := true
	for {
		l, u := o.stats()
		if chanOK {
			if err := c.SendDrainStat(handoff.DrainStat{Links: uint32(l), Users: uint32(u)}); err != nil {
				// Review 4 H1: a broken channel with the child alive is not a
				// dark tunnel (the child serves; it only stopped following
				// DRAINSTAT): keep draining our users, stop sending. A broken
				// channel because the child died is caught just below.
				chanOK = false
				o.logf("zdu: drain: hand-off channel broken: %v (new pid %d still watched)", err, child.Pid())
			}
		}
		select {
		case <-child.Done():
			o.drainAbort(c, child, o.now(), t0, l, u)
			return
		default:
		}
		now := o.now()
		// Review issue 6: TUN packets still arriving on this process's links
		// are traffic too (the user count does not see them).
		l3q := true
		if u == 0 && o.l3Quiet != nil {
			var since time.Duration
			if l3q, since = o.l3Quiet(now); !l3q && !l3Held {
				l3Held = true
				o.logf("zdu: drain: no user connections left, but a TUN packet came from a link %v ago: waiting until none for the grace", since.Round(time.Millisecond))
			}
		}
		if u == 0 && l3q {
			o.logf("zdu: drain end: no user connections left after %v (links %d); exiting", now.Sub(t0).Round(time.Second), l)
			break
		}
		if !now.Before(deadline) {
			o.logf("zdu: drain end: deadline %s reached with links %d users %d; exiting", deadline.UTC().Format(time.RFC3339), l, u)
			break
		}
		select {
		case <-tk.C:
		case <-child.Done():
			// Review 4 H1: the new process (MAINPID) died after the commit
			// (e.g. OOM with two processes). This process has TUN reads off,
			// its link listeners dropped and user accept paused: it cannot
			// serve. Exit now (non-zero, MAINPID back to us) so Restart=
			// starts the unit cold within seconds, not after the drain hour.
			o.drainAbort(c, child, o.now(), t0, l, u)
			return
		}
	}
	o.status(fmt.Sprintf("DRAIN-END %d", child.Pid()))
	if chanOK {
		c.SendDrainStat(handoff.DrainStat{})
	}
	c.Close()
	o.exit()
}

// drainAbort ends the drain because the new process died (review 4 H1).
func (o *zduOld) drainAbort(c *handoff.Conn, child zduChild, now, t0 time.Time, l, u int) {
	why := fmt.Sprintf("new process pid %d died during the drain", child.Pid())
	o.status("DRAIN-ABORT " + why)
	o.logf("zdu: drain end: ABORT: %s after %v (links %d users %d here, TUN reads off, accept paused: cannot serve); exiting 1 for a cold restart (%s)",
		why, now.Sub(t0).Round(time.Second), l, u, zduCounters())
	c.Close()
	if o.exitFail != nil {
		o.exitFail(why)
		return
	}
	o.exit()
}

// ---- new process side ----

var zduTake struct {
	mu    sync.Mutex
	t     *handoff.Taken
	once  sync.Once
	drain func(links int) // Iran: LinkManager foreign-drain sink
}

// zduParentPid (review 4 M1): a hand-off child's parent (the old process)
// while it may still drain; 0 once it exited (we were reparented).
var zduParentPid atomic.Int64

// zduParentDraining: the old process that handed over to us is still our
// parent (it exits at its drain end; then getppid changes).
func zduParentDraining() (int, bool) {
	pp := int(zduParentPid.Load())
	if pp == 0 {
		return 0, false
	}
	if os.Getppid() == pp {
		return pp, true
	}
	zduParentPid.Store(0)
	return 0, false
}

// zduHups counts SIGHUPs (main's reload handler, key on); the old process
// forwards a reload that came between the spawn and GO (review 4 low).
var zduHups atomic.Int64

// zduExit is os.Exit (tests replace it).
var zduExit = os.Exit

// zduTakeFail ends a child started with HS2_HANDOVER_FD whose takeover
// failed (review F3): never a cold start. A cold start runs `ip link del`
// on the LIVE TUN and binds its own sockets while the old process still
// serves (and is about to Kill this child). Every received fd is closed
// first; exit(1) is the old process's "failed" path: nothing paused.
func zduTakeFail(logf func(string, ...any), files []*os.File, format string, args ...any) {
	for _, f := range files {
		if f != nil {
			f.Close()
		}
	}
	zduTakeFailed.Add(1)
	logf("zdu: takeover failed: "+format+"; exiting (no cold start: the old process keeps serving) (%s)", append(args, zduCounters())...)
	zduExit(1)
}

// zduTakeover runs in a new process before the engine opens anything:
// HS2_HANDOVER_FD set -> take the fds or exit(1); unset -> nothing.
func zduTakeover(logf func(string, ...any)) {
	v := os.Getenv("HS2_HANDOVER_FD")
	if v == "" {
		return
	}
	os.Unsetenv("HS2_HANDOVER_FD") // never leak into a later child
	fd, err := strconv.Atoi(v)
	if err != nil || fd < 3 {
		zduTakeFail(logf, nil, "bad HS2_HANDOVER_FD %q", v)
		return
	}
	if !engine.ZDUEnabled() {
		zduTakeFail(logf, []*os.File{os.NewFile(uintptr(fd), "handoff")}, "HS2_HANDOVER_FD set but HS2_ZDU is off")
		return
	}
	c, err := handoff.FromFD(fd) // consumes fd (closed on success and on error)
	if err != nil {
		zduTakeFail(logf, nil, "channel: %v", err)
		return
	}
	// zdu391: the Hello carries FeatListening (zduHello), so the old process
	// waits for our LISTENING before it moves link accept here.
	t, err := c.Take(zduTakeHello(), handoff.StepTimeout)
	if err != nil {
		c.Close()
		zduTakeFail(logf, nil, "hand-off refused: %v", err)
		return
	}
	// S7: order this instance after the parent's even if the clock stepped
	// back between the two starts (before any link announces it).
	if own, _ := engine.ZDUInstance(); own <= t.Peer.InstStart {
		logf("zdu: takeover: own start %d not after the parent's %d (clock step back): using %d",
			own, t.Peer.InstStart, engine.ZDUFollowParent(t.Peer.InstStart))
	}
	// Check every fd before adopting any: a failure then closes them all.
	for _, f := range t.FDs {
		switch {
		case strings.HasPrefix(f.Name, "tun:"):
			if err := tun.CheckFD(strings.TrimPrefix(f.Name, "tun:"), f.File); err != nil {
				zduTakeFail(logf, zduTakenFiles(t), "tun fd refused: %v", err)
				return
			}
		case strings.HasPrefix(f.Name, "tcp:"), strings.HasPrefix(f.Name, "udp:"):
		default:
			zduTakeFail(logf, zduTakenFiles(t), "unknown fd %q", f.Name)
			return
		}
	}
	var nl, nu, nt int
	for _, f := range append([]handoff.FD(nil), t.FDs...) {
		switch {
		case strings.HasPrefix(f.Name, "tcp:"):
			engine.ZDUAdoptListener(f.Name, t.File(f.Name))
			nl++
		case strings.HasPrefix(f.Name, "udp:"):
			engine.ZDUAdoptPacketConn(f.Name, t.File(f.Name))
			nu++
		case strings.HasPrefix(f.Name, "tun:"):
			if err := tun.AdoptFD(strings.TrimPrefix(f.Name, "tun:"), t.File(f.Name)); err != nil {
				zduTakeFail(logf, zduTakenFilesDrop(t), "tun fd refused: %v", err)
				return
			}
			nt++
		}
	}
	engine.ZDUHoldUsers() // review issue 4: no user accept, UDP read or exit user stream before GO (engine/zdu_gate.go)
	zduTakeAdopted.Add(1)
	zduParentPid.Store(int64(os.Getppid())) // review 4 M1: no handover while it drains
	zduTake.mu.Lock()
	zduTake.t = t
	zduTake.mu.Unlock()
	logf("zdu: takeover: adopted %d listeners, %d udp sockets and %d tun fds from the instance %d", nl, nu, nt, t.Peer.InstStart)
	// Lab drills only (zdu391 M2 check): a child that hangs before its
	// engine listens. Read only in a hand-off child (key on).
	if d, err := time.ParseDuration(os.Getenv("HS2_ZDU_LAB_HANG")); err == nil && d > 0 {
		logf("zdu: takeover: lab hang %v before the engine starts (HS2_ZDU_LAB_HANG)", d)
		time.Sleep(d)
	}
}

// zduTakenFilesDrop is zduTakenFiles after closing what was adopted already.
func zduTakenFilesDrop(t *handoff.Taken) []*os.File {
	engine.ZDUDropAdopted()
	tun.DropHanded()
	return zduTakenFiles(t)
}

// zduTakenFiles closes the channel and returns the fds still in t.
func zduTakenFiles(t *handoff.Taken) []*os.File {
	out := make([]*os.File, 0, len(t.FDs))
	for _, f := range t.FDs {
		out = append(out, f.File)
	}
	t.FDs = nil
	t.Conn.Close()
	return out
}

// zduTakeReady runs at the first READY of a new process: READY on the
// channel, wait GO, then follow DRAINSTAT in the background.
func zduTakeReady(logf func(string, ...any)) {
	zduTake.mu.Lock()
	t := zduTake.t
	zduTake.mu.Unlock()
	if t == nil {
		return
	}
	// Async: some carriers call notifyReady right before their blocking run.
	go zduTakeReadyRun(t, logf)
}

// zduWarmWait holds the hand-off READY until this process's link pools are
// warm (lab1 S6 drill: READY at "user ports bound" lost 3 ICMP packets that
// the new process read from the TUN before it had a link). A process with no
// link pool (Kharej exit) waits zduWarmNoPool.
var (
	zduWarmMax    = 20 * time.Second
	zduWarmStable = time.Second
	zduWarmNoPool = 2 * time.Second
)

func zduWarmWait(stats func() (int, int), pools, need func() int) (links int, waited time.Duration) {
	t0 := time.Now()
	if pools() == 0 {
		time.Sleep(zduWarmNoPool)
		return 0, time.Since(t0)
	}
	// Review issue 3/4: READY needs min(old_serving/2, warm) links per pool
	// (engine.ZDUWarmNeed), held for zduWarmStable, not one link.
	last, since := -1, time.Time{}
	for time.Since(t0) < zduWarmMax {
		l, _ := stats()
		last = l
		if l > 0 && l >= need() {
			if since.IsZero() {
				since = time.Now()
			}
			if time.Since(since) >= zduWarmStable {
				return l, time.Since(t0)
			}
		} else {
			since = time.Time{}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return last, time.Since(t0)
}

// zduWarmOK: a process with a link pool needs its warm need of links before
// READY (review issue 3: never one link for every new user).
func zduWarmOK(links, pools, need int) bool { return pools == 0 || (links > 0 && links >= need) }

func zduTakeReadyRun(t *handoff.Taken, logf func(string, ...any)) {
	zduTake.once.Do(func() {
		// Review M2: our listeners are up and the fds adopted (notifyReady
		// runs after the engine bound them): the old process moves link
		// accept here now, not at the spawn.
		if err := t.Listening(handoff.StepTimeout); err != nil {
			logf("zdu: takeover: LISTENING failed: %v (the old process keeps serving; exiting)", err)
			os.Exit(1)
		}
		l, w := zduWarmWait(engine.ZDUDrainStats, engine.ZDUPools, engine.ZDUWarmNeed)
		if !zduWarmOK(l, engine.ZDUPools(), engine.ZDUWarmNeed()) {
			// S7: never take over with no link: after GO this process reads
			// the TUN and owns the accept queue, and at commit the old one
			// stops reading. Exiting before READY = the old process's
			// "failed" path: nothing paused, it keeps serving.
			logf("zdu: takeover: %d links after %v, need %d: refusing the hand-off (the old process keeps serving; no user was served here); exiting", l, w.Round(time.Millisecond), engine.ZDUWarmNeed())
			os.Exit(1)
		}
		logf("zdu: takeover: link pools warm (%d links after %v, need %d), sending READY", l, w.Round(time.Millisecond), engine.ZDUWarmNeed())
		if n := engine.ZDUDropAdopted(); n > 0 {
			logf("zdu: takeover: %d handed listeners not bound by this config, closed", n)
		}
		if err := t.Ready(handoff.StepTimeout); err != nil {
			logf("zdu: takeover: READY/GO failed: %v (the old process keeps serving; exiting)", err)
			os.Exit(1)
		}
		tun.OpenReadGate()         // the adopted TUN reads from now on (never before GO)
		n := engine.ZDUOpenUsers() // review issue 4: users from GO on
		logf("zdu: takeover: READY sent, GO received; %d user listeners and sockets open; following the old process's drain", n)
		go func() {
			err := t.Conn.FollowDrain(func(links int) {
				zduTake.mu.Lock()
				d := zduTake.drain
				zduTake.mu.Unlock()
				if d != nil {
					d(links)
				}
			}, handoff.DrainSilence)
			logf("zdu: takeover: old process drain channel ended (%v)", err)
			t.Close()
		}()
	})
}

// zduSetDrainSink wires DRAINSTAT to an Iran LinkManager.
func zduSetDrainSink(f func(int)) {
	zduTake.mu.Lock()
	zduTake.drain = f
	zduTake.mu.Unlock()
}

// ---- wiring ----

var zduTun struct {
	mu  sync.Mutex
	dev *tun.Device
}

// zduNoteTun records the TUN device the old side hands over.
func zduNoteTun(d *tun.Device) {
	if !engine.ZDUEnabled() || d == nil {
		return
	}
	zduTun.mu.Lock()
	zduTun.dev = d
	zduTun.mu.Unlock()
}

// zduTunReadsOff stops this process's TUN reads (old process, at commit).
func zduTunReadsOff() string {
	zduTun.mu.Lock()
	d := zduTun.dev
	zduTun.mu.Unlock()
	if d == nil {
		return ""
	}
	d.StopReads()
	return d.Name()
}

func zduHandFDs() ([]handoff.FD, error) {
	names, files, err := engine.ZDUListenerFiles()
	if err != nil {
		return nil, err
	}
	out := make([]handoff.FD, 0, len(files)+1)
	for i := range files {
		out = append(out, handoff.FD{Name: names[i], File: files[i]})
	}
	zduTun.mu.Lock()
	d := zduTun.dev
	zduTun.mu.Unlock()
	if d != nil {
		f, err := handoff.Dup(d.File(), "tun:"+d.Name()) // never Fd(): it can clear O_NONBLOCK
		if err != nil {
			for _, f := range out {
				f.File.Close()
			}
			return nil, err
		}
		out = append(out, handoff.FD{Name: "tun:" + d.Name(), File: f})
	}
	return out, nil
}

// zduStart installs the SIGUSR2 trigger (key on only) and runs the
// new-process takeover. exit is the old process's final exit.
func zduStart(ctx context.Context, logf func(string, ...any), exit func()) {
	if !engine.ZDUEnabled() {
		if os.Getenv("HS2_HANDOVER_FD") != "" {
			zduTakeover(logf) // a hand-off child with the key off: exit(1), never cold
		}
		return
	}
	zduTakeover(logf)
	var over time.Duration
	if v := os.Getenv("HS2_ZDU_DRAIN"); v != "" {
		d, err := engine.ParseZDUDrain(v)
		if err != nil {
			logf("zdu: HS2_ZDU_DRAIN: %v (using the HH:03 rule)", err)
		}
		over = d
	}
	now := time.Now
	if v := os.Getenv("HS2_ZDU_FAKE_NOW"); v != "" { // lab drills only
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			off := t.Sub(time.Now())
			now = func() time.Time { return time.Now().Add(off) }
			logf("zdu: lab clock offset %v (HS2_ZDU_FAKE_NOW)", off.Round(time.Second))
		}
	}
	prob := zduProbation
	if v := os.Getenv("HS2_ZDU_PROBATION"); v != "" { // lab drills only
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			prob = d
		}
	}
	o := &zduOld{
		unit: zduUnit(), runDir: zduRunDir, now: now, override: over,
		probation: prob, floor: zduDrainFloor, period: zduDrainPeriod, ready: handoff.ReadyTimeout,
		spawn: zduSpawnExec, notify: sdnotify.Notify, fds: zduHandFDs,
		pause: engine.ZDUPauseAccept, resume: engine.ZDUResumeAccept,
		stats: engine.ZDUDrainStats, readsOff: zduTunReadsOff, drainMode: zduDrainMode, exit: exit, logf: logf,
		linkPause: engine.ZDUPauseLinkAccept, linkResume: engine.ZDUResumeLinkAccept, boost: engine.ZDUBoost,
		linkDrop: engine.ZDUDropLinkListeners, direct: zduDirect,
		l3Quiet: func(time.Time) (bool, time.Duration) { return engine.ZDUL3Quiet(time.Now(), engine.ZDUL3DrainGrace) },
		hello:   func() handoff.Hello { return zduHello(0, [32]byte{}) },
		exitFail: func(why string) {
			sdnotify.Notify(fmt.Sprintf("MAINPID=%d\nSTATUS=hs2 drain aborted: %s; exiting 1 for a cold restart", os.Getpid(), why))
			encap.ReleaseAllEchoGuards()
			os.Exit(1)
		},
		parentDraining: zduParentDraining,
		hups:           zduHups.Load,
		acceptQ:        engine.ZDUAcceptQueue,
		hungAfter:      zduHungAfter,
		hupChild:       func(pid int) error { return syscall.Kill(pid, syscall.SIGHUP) },
		peers: func() (bool, string) {
			ok, _, _ := engine.ZDUPeerCheck()
			return ok, engine.ZDUPeerLine()
		},
	}
	if err := os.MkdirAll(zduRunDir, runDirMode()); err != nil { // b396: as the status writer (sec370)
		logf("zdu: %s: %v", zduRunDir, err)
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGUSR2)
	logf("zdu: takeover armed: SIGUSR2 hands unit %s over to the binary in %s", o.unit, filepath.Join(zduRunDir, o.unit+".next"))
	go func() {
		pt := time.NewTicker(zduPeersPeriod)
		defer pt.Stop()
		o.writePeers()
		for {
			select {
			case <-ctx.Done():
				signal.Stop(sig)
				return
			case <-sig:
				go o.handover()
			case <-pt.C:
				o.writePeers()
			}
		}
	}()
}

// zduDrainMode enters B6 drain mode (old process, at the commit): the edge
// advert and the status writer stop (engine.ZDUEnterDrain), and the memory
// limit becomes the runtime memory in use x 1.25 (zduDrainMemLimit) unless a lower one is set, so the
// shrinking old process gives memory back to the new one (two processes on
// one host during the drain).
func zduDrainMode() string {
	if !engine.ZDUEnterDrain() {
		return ""
	}
	lim, heap := zduDrainMemLimit()
	return fmt.Sprintf("drain mode: advert and status writer off, GOMEMLIMIT %d MiB (runtime memory in use %d MiB)", lim>>20, heap>>20)
}

// zduDrainMemLimit sets the memory limit to the runtime's total memory in
// use x 1.25 (at least use + 64 MiB) when that is below the current one, and
// returns the limit in force and the use it was computed from. Review F4: the
// Go limit counts all runtime memory (stacks, GC metadata, free spans not
// yet released), not the heap alone; the old HeapInuse x 1.25 could sit
// below the current use at commit, and the GC then ran at its 50 % CPU cap
// through the whole drain. The limit is never below the use at commit.
func zduDrainMemLimit() (limit, use int64) {
	use = zduMemInUse()
	want := use + max(use/4, 64<<20)
	cur := debug.SetMemoryLimit(-1)
	if want < cur {
		debug.SetMemoryLimit(want)
		return want, use
	}
	return cur, use
}

// zduMemInUse is what the Go memory limit compares against: all memory
// mapped by the runtime minus what it returned to the OS.
func zduMemInUse() int64 {
	s := []metrics.Sample{{Name: "/memory/classes/total:bytes"}, {Name: "/memory/classes/heap/released:bytes"}}
	metrics.Read(s)
	if s[0].Value.Kind() != metrics.KindUint64 || s[1].Value.Kind() != metrics.KindUint64 {
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		return int64(ms.Sys - ms.HeapReleased)
	}
	return int64(s[0].Value.Uint64() - s[1].Value.Uint64())
}
