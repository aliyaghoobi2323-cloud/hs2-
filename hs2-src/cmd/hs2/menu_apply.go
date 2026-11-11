package main

// Safe apply (menu M1): snapshot -> atomic write -> detached restart -> health
// gate -> trial with a confirm timer -> automatic rollback when the change is
// not confirmed in time or the tunnel turns unhealthy.
//
// State machine (/var/lib/hs2/apply/<unit>.json, every transition under an
// flock on that file):
//
//	restarting --restart--> health --gate ok--> trial --confirm--> confirmed
//	                          |                   |  \--deadline / unhealthy--> rolledback
//	                          \--gate fails-------+--------------------------> rolledback
//	                                                  (rollback restart fails -> failed)
//
// The worker that walks it runs as a transient systemd service (systemd-run),
// so the caller's SSH session dying changes nothing; a transient timer set to
// the deadline + grace runs `hs2 apply --expire` as a second, independent
// guard in case the worker itself is killed.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var (
	applyStateDir = "/var/lib/hs2/apply"
	auditLogPath  = "/var/log/hs2-changes.log"
	adminLockPath = "/run/hs2/admin.lock"
	lockStaleAge  = 2 * time.Hour
	healthWindow  = 60 * time.Second
	healthStable  = 8 * time.Second
	expireGrace   = 30 * time.Second
)

const (
	stRestarting = "restarting"
	stHealth     = "health"
	stTrial      = "trial"
	stConfirmed  = "confirmed"
	stRolledBack = "rolledback"
	stFailed     = "failed"
)

type applyState struct {
	Unit       string   `json:"unit"`
	Cfg        string   `json:"cfg"`
	State      string   `json:"state"`
	From       int      `json:"from_version"` // snapshot of the config before the apply
	To         int      `json:"to_version"`   // the applied config
	Started    int64    `json:"started"`
	Restarted  int64    `json:"restarted,omitempty"`
	Deadline   int64    `json:"deadline"`
	HealthGate bool     `json:"health_gate"`
	Detail     string   `json:"detail,omitempty"`
	Reason     string   `json:"reason"`
	Who        string   `json:"who"`
	TTY        string   `json:"tty,omitempty"`
	Bilateral  []string `json:"bilateral,omitempty"`
	Timer      string   `json:"timer,omitempty"`
	Summary    string   `json:"summary,omitempty"`
	// Reload (menu M5): the change is applied (and rolled back) with a
	// reload (SIGHUP: lb section and per-IP fronts) instead of a restart,
	// so established links keep running.
	Reload bool `json:"reload,omitempty"`
}

func (s *applyState) live() bool {
	return s.State == stRestarting || s.State == stHealth || s.State == stTrial
}

// sysOps is everything the state machine does to the host (tests fake it).
type sysOps interface {
	restart(unit string) error
	reload(unit string) error
	active(unit string) bool
	panicked(unit string, since time.Time) bool
	links(cfg string, since time.Time) int
	daemonReload()
	spawn(name string, args ...string) error
	timer(name string, after time.Duration, args ...string) error
	stopTimer(name string)
	now() time.Time
	sleep(d time.Duration)
}

type realOps struct{}

func (realOps) restart(u string) error {
	out, err := exec.Command("systemctl", "restart", u).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl restart %s: %v %s", u, err, strings.TrimSpace(string(out)))
	}
	return nil
}
func (realOps) reload(u string) error {
	out, err := exec.Command("systemctl", "reload", u).CombinedOutput()
	if err != nil {
		if o2, e2 := exec.Command("systemctl", "kill", "-s", "HUP", "--kill-who=main", u).CombinedOutput(); e2 != nil {
			return fmt.Errorf("systemctl reload %s: %v %s / kill -HUP: %v %s", u, err, strings.TrimSpace(string(out)), e2, strings.TrimSpace(string(o2)))
		}
	}
	return nil
}
func (realOps) active(u string) bool {
	out, _ := exec.Command("systemctl", "is-active", u).Output()
	return strings.TrimSpace(string(out)) == "active"
}
func (realOps) panicked(u string, since time.Time) bool {
	out, _ := exec.Command("journalctl", "-u", u, "--since", "@"+strconv.FormatInt(since.Unix(), 10), "-o", "cat", "--no-pager").Output()
	s := string(out)
	return strings.Contains(s, "panic:") || strings.Contains(s, "fatal error:")
}
func (realOps) links(cfg string, since time.Time) int {
	ls, ok := readLive(cfg)
	if !ok || ls.Updated < since.Unix() { // a status file of the process before the restart does not count
		return 0
	}
	return ls.Links
}
func (realOps) daemonReload() { exec.Command("systemctl", "daemon-reload").Run() }
func (realOps) spawn(name string, args ...string) error {
	exe, _ := os.Executable()
	a := append([]string{"--unit=" + name, "--collect", "--quiet", exe}, args...)
	out, err := exec.Command("systemd-run", a...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemd-run: %v %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
func (realOps) timer(name string, after time.Duration, args ...string) error {
	exe, _ := os.Executable()
	a := append([]string{"--unit=" + name, "--collect", "--quiet", fmt.Sprintf("--on-active=%ds", int(after.Seconds())), exe}, args...)
	out, err := exec.Command("systemd-run", a...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemd-run timer: %v %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
func (realOps) stopTimer(name string) {
	if name != "" {
		exec.Command("systemctl", "stop", name+".timer").Run()
	}
}
func (realOps) now() time.Time        { return time.Now() }
func (realOps) sleep(d time.Duration) { time.Sleep(d) }

// ---- state file -----------------------------------------------------------

func statePath(unit string) string { return filepath.Join(applyStateDir, cleanUnit(unit)+".json") }

// withState runs fn on the unit's state under an exclusive flock; fn's
// changes are saved when it returns nil.
func withState(unit string, fn func(st *applyState) error) error {
	if err := os.MkdirAll(applyStateDir, 0o700); err != nil {
		return err
	}
	lf, err := os.OpenFile(statePath(unit)+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lf.Close()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
	st := &applyState{}
	if b, err := os.ReadFile(statePath(unit)); err == nil {
		_ = json.Unmarshal(b, st)
	}
	if err := fn(st); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(st, "", "  ")
	return writeFileAtomic(statePath(unit), b)
}

func loadState(unit string) (applyState, bool) {
	var st applyState
	b, err := os.ReadFile(statePath(unit))
	if err != nil || json.Unmarshal(b, &st) != nil || st.Unit == "" {
		return applyState{}, false
	}
	return st, true
}

// ---- audit log ------------------------------------------------------------

func whoAmI() (who, tty string) {
	who = os.Getenv("SUDO_USER")
	if who == "" {
		who = os.Getenv("USER")
	}
	if who == "" {
		who = "root"
	}
	tty, _ = os.Readlink("/proc/self/fd/0")
	if !strings.HasPrefix(tty, "/dev/") {
		tty = "-"
	}
	return who, tty
}

func audit(now time.Time, who, tty, unit, action, reason, summary, result string) {
	os.MkdirAll(filepath.Dir(auditLogPath), 0o755)
	f, err := os.OpenFile(auditLogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s who=%s tty=%s unit=%s action=%s reason=%q changes=%q result=%q\n",
		now.Format(time.RFC3339), who, tty, cleanUnit(unit), action, reason, summary, result)
}

// ---- admin lock -----------------------------------------------------------

type adminLock struct {
	Owner string `json:"owner"`
	PID   int    `json:"pid"`
	TTY   string `json:"tty"`
	Time  int64  `json:"time"`
}

var pidAlive = func(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil || errors.Is(syscall.Kill(pid, 0), syscall.EPERM)
}

func readLock() (adminLock, bool) {
	var l adminLock
	b, err := os.ReadFile(adminLockPath)
	if err != nil || json.Unmarshal(b, &l) != nil {
		return adminLock{}, false
	}
	return l, true
}

// lockStale: the holder is gone, or the lock is older than lockStaleAge.
func lockStale(l adminLock, now time.Time) bool {
	return !pidAlive(l.PID) || now.Sub(time.Unix(l.Time, 0)) > lockStaleAge
}

// acquireLock takes the admin lock for pid. A lock already held by pid itself
// or by our parent (the menu shell that ran `hs2 config lock`) is fine. A
// stale lock is taken over with a note; a live one only with force.
func acquireLock(pid int, force bool, now time.Time) (release func(), err error) {
	who, tty := whoAmI()
	if l, ok := readLock(); ok {
		if l.PID == pid || l.PID == os.Getppid() || strconv.Itoa(l.PID) == os.Getenv("HS2_LOCK_PID") {
			return func() {}, nil
		}
		switch {
		case lockStale(l, now):
			fmt.Printf(tr("note: taking over a stale admin lock (%s pid %d since %s)\n", "توجه: قفل مدیریتی کهنه گرفته شد (%s pid %d از %s)\n"),
				l.Owner, l.PID, time.Unix(l.Time, 0).Format("15:04:05"))
		case force:
			fmt.Printf(tr("WARNING: --force: breaking the admin lock of %s (pid %d, %s, since %s) — their change may now conflict with yours\n",
				"هشدار: --force: قفل مدیریتی %s (pid %d، %s، از %s) شکسته شد — تغییر او ممکن است با تغییر شما تداخل کند\n"),
				l.Owner, l.PID, l.TTY, time.Unix(l.Time, 0).Format("15:04:05"))
		default:
			return nil, fmt.Errorf(tr("another admin is editing: %s (pid %d, %s) since %s — wait, or use --force",
				"مدیر دیگری در حال ویرایش است: %s (pid %d، %s) از %s — صبر کنید یا --force بزنید"),
				l.Owner, l.PID, l.TTY, time.Unix(l.Time, 0).Format("15:04:05"))
		}
	}
	os.MkdirAll(filepath.Dir(adminLockPath), 0o755)
	b, _ := json.Marshal(adminLock{Owner: who, PID: pid, TTY: tty, Time: now.Unix()})
	if err := writeFileAtomic(adminLockPath, b); err != nil {
		return nil, err
	}
	return func() {
		if l, ok := readLock(); ok && l.PID == pid {
			os.Remove(adminLockPath)
		}
	}, nil
}

// ---- the state machine ----------------------------------------------------

type applyReq struct {
	Unit, Cfg, Reason string
	New               []byte
	Within            time.Duration
	HealthGate        bool
	// NewDropins (menu M4): the unit's drop-in directory after the change
	// (file name -> content; nil = unchanged). Presets, feature toggles and
	// CPUAffinity are drop-ins; a rollback restores the snapshot's set.
	NewDropins map[string]string
	// Reload: apply with a reload instead of a restart (see applyState.Reload).
	Reload bool
}

// startApply does the caller's part: snapshot, write, hand over to the
// detached worker + expiry timer. Everything after it survives the caller.
func startApply(ops sysOps, r applyReq) (applyState, []diffEntry, error) {
	now := ops.now()
	if st, ok := loadState(r.Unit); ok && st.live() {
		return st, nil, fmt.Errorf(tr("an apply of %s is still in progress (state %s) — confirm it (hs2 apply --confirm -u %s) or roll it back first",
			"اعمال قبلی %s هنوز در جریان است (وضعیت %s) — اول تأیید کنید (hs2 apply --confirm -u %s) یا برگردانید"), st.Unit, st.State, cleanUnit(st.Unit))
	}
	cur, err := os.ReadFile(r.Cfg)
	if err != nil {
		return applyState{}, nil, err
	}
	if r.New == nil {
		r.New = cur
	}
	oldDI := readDropins(r.Unit)
	var d []diffEntry
	if r.NewDropins != nil {
		d, err = semanticDiff(cur, r.New, oldDI, r.NewDropins)
	} else {
		d, err = semanticDiff(cur, r.New, nil, nil)
	}
	if err != nil {
		return applyState{}, nil, err
	}
	if len(d) == 0 {
		return applyState{}, d, errors.New(tr("no changes — nothing to apply", "تغییری نیست — چیزی برای اعمال نیست"))
	}
	from, err := snapshotCurrent(r.Unit, r.Cfg, "pre-apply", now)
	if err != nil {
		return applyState{}, d, fmt.Errorf("snapshot: %v", err)
	}
	if err := writeFileAtomic(r.Cfg, r.New); err != nil {
		return applyState{}, d, err
	}
	if r.NewDropins != nil {
		if changed, err := restoreDropins(r.Unit, r.NewDropins); err != nil {
			writeFileAtomic(r.Cfg, cur)
			restoreDropins(r.Unit, oldDI)
			return applyState{}, d, err
		} else if changed {
			ops.daemonReload()
		}
	}
	to, err := saveVersion(r.Unit, r.New, readDropins(r.Unit), "apply-"+r.Reason, now)
	if err != nil {
		writeFileAtomic(r.Cfg, cur)
		return applyState{}, d, err
	}
	who, tty := whoAmI()
	ts := strconv.FormatInt(now.Unix(), 10)
	var st applyState
	err = withState(r.Unit, func(s *applyState) error {
		*s = applyState{Unit: cleanUnit(r.Unit), Cfg: r.Cfg, State: stRestarting, From: from.N, To: to.N,
			Started: now.Unix(), Deadline: now.Add(r.Within).Unix(), HealthGate: r.HealthGate,
			Reason: r.Reason, Who: who, TTY: tty, Bilateral: bilateralOf(d), Summary: diffSummary(d),
			Timer: "hs2-apply-expire-" + cleanUnit(r.Unit) + "-" + ts, Reload: r.Reload && r.NewDropins == nil}
		st = *s
		return nil
	})
	if err != nil {
		writeFileAtomic(r.Cfg, cur)
		return st, d, err
	}
	audit(now, who, tty, r.Unit, "apply", r.Reason, st.Summary, fmt.Sprintf("started v%d->v%d, confirm within %s", from.N, to.N, r.Within))
	// The independent guard first, then the worker.
	if err := ops.timer(st.Timer, r.Within+expireGrace, "apply", "--expire", "-u", st.Unit); err != nil {
		fmt.Println("WARN: " + err.Error())
	}
	if err := ops.spawn("hs2-apply-"+st.Unit+"-"+ts, "apply", "--worker", "-u", st.Unit); err != nil {
		// No detached worker: run it in-process (an SSH drop would then kill it,
		// but the timer still rolls back).
		fmt.Println("WARN: " + err.Error() + " — running the restart in this session")
		go runWorker(ops, st.Unit)
	}
	return st, d, nil
}

// runWorker walks restarting -> health -> trial -> (confirmed | rolledback).
func runWorker(ops sysOps, unit string) {
	var st applyState
	ok := false
	withState(unit, func(s *applyState) error {
		if s.State != stRestarting {
			return errors.New("not restarting")
		}
		s.State, s.Restarted = stHealth, ops.now().Unix()
		st, ok = *s, true
		return nil
	})
	if !ok {
		return
	}
	since := ops.now()
	act := ops.restart
	if st.Reload {
		act = ops.reload
	}
	if err := act(st.Unit); err != nil {
		doRollback(ops, st.Unit, "restart failed: "+err.Error())
		return
	}
	if why := healthGate(ops, st, since); why != "" {
		doRollback(ops, st.Unit, "health gate: "+why)
		return
	}
	withState(unit, func(s *applyState) error {
		if s.State != stHealth {
			return errors.New("moved on")
		}
		s.State, s.Detail = stTrial, "healthy"
		return nil
	})
	// Trial: wait for the confirm; roll back on the deadline or on a crash.
	for {
		cur, ok := loadState(unit)
		if !ok || cur.State != stTrial {
			return
		}
		if ops.now().Unix() >= cur.Deadline {
			doRollback(ops, unit, "not confirmed in time")
			return
		}
		if !ops.active(unit) || ops.panicked(unit, since) {
			doRollback(ops, unit, "tunnel turned unhealthy during the trial")
			return
		}
		ops.sleep(time.Second)
	}
}

// healthGate: "" when healthy. The unit must be active and panic-free for
// healthStable; with the gate on it must also have >= 1 link — all within
// healthWindow.
func healthGate(ops sysOps, st applyState, since time.Time) string {
	end := since.Add(healthWindow)
	var okSince time.Time
	last := "unit not active"
	for !ops.now().After(end) {
		switch {
		case ops.panicked(st.Unit, since):
			return "panic in the log"
		case !ops.active(st.Unit):
			okSince, last = time.Time{}, "unit not active"
		case st.HealthGate && ops.links(st.Cfg, since) < 1:
			okSince, last = time.Time{}, "no link serving"
		default:
			if okSince.IsZero() {
				okSince = ops.now()
			}
			if ops.now().Sub(okSince) >= healthStable {
				return ""
			}
		}
		ops.sleep(time.Second)
	}
	return last + " within " + healthWindow.String()
}

// doRollback restores the pre-apply version of a live apply.
func doRollback(ops sysOps, unit, why string) {
	var st applyState
	ok := false
	withState(unit, func(s *applyState) error {
		if !s.live() {
			return errors.New("not live")
		}
		st, ok = *s, true
		s.State, s.Detail = stRolledBack, why
		return nil
	})
	if !ok {
		return
	}
	res := tr("rolled back", "برگردانده شد")
	if err := restoreVersionMode(ops, st.Unit, st.Cfg, st.From, "rollback-"+strconv.Itoa(st.From), st.Reload); err != nil {
		res = "rollback FAILED: " + err.Error()
		withState(unit, func(s *applyState) error { s.State, s.Detail = stFailed, why+"; "+res; return nil })
	}
	ops.stopTimer(st.Timer)
	audit(ops.now(), st.Who, st.TTY, unit, "auto-rollback", why, st.Summary, fmt.Sprintf("%s to v%d", res, st.From))
}

// restoreVersion writes version n back (config + drop-ins) and restarts.
func restoreVersion(ops sysOps, unit, cfg string, n int, reason string) error {
	return restoreVersionMode(ops, unit, cfg, n, reason, false)
}

// restoreVersionMode: restoreVersion that reloads instead of restarting when
// reload is set and the drop-ins did not change.
func restoreVersionMode(ops sysOps, unit, cfg string, n int, reason string, reload bool) error {
	v, err := findVersion(unit, n)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(v.Path)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(cfg, b); err != nil {
		return err
	}
	if changed, err := restoreDropins(unit, v.dropins()); err != nil {
		return err
	} else if changed {
		ops.daemonReload()
		reload = false
	}
	saveVersion(unit, b, readDropins(unit), reason, ops.now())
	if reload {
		return ops.reload(unit)
	}
	return ops.restart(unit)
}

// confirmApply ends a trial successfully.
// errHealthPending: confirm before the health gate has finished (its text in
// the language chosen at run time).
var errHealthPending error = healthPending{}

type healthPending struct{}

func (healthPending) Error() string {
	return tr("the health check is still running — try again in a few seconds", "بررسی سلامت هنوز در جریان است — چند ثانیه دیگر دوباره بزنید")
}

func confirmApply(ops sysOps, unit string) error {
	var st applyState
	err := withState(unit, func(s *applyState) error {
		switch s.State {
		case stTrial:
			s.State, s.Detail = stConfirmed, "confirmed"
			st = *s
			return nil
		case stRestarting, stHealth:
			return errHealthPending
		case "":
			return errors.New(tr("no apply to confirm", "اعمالی برای تأیید نیست"))
		default:
			return fmt.Errorf(tr("nothing to confirm: the last apply ended as %s (%s)", "چیزی برای تأیید نیست: آخرین اعمال با وضعیت %s تمام شد (%s)"), s.State, s.Detail)
		}
	})
	if err != nil {
		return err
	}
	ops.stopTimer(st.Timer)
	cw, ct := whoAmI()
	audit(ops.now(), cw, ct, unit, "confirm", st.Reason, st.Summary, fmt.Sprintf("confirmed v%d", st.To))
	return nil
}

// expireApply is the timer's guard: roll back a live apply past its deadline.
func expireApply(ops sysOps, unit string) {
	if st, ok := loadState(unit); ok && st.live() && ops.now().Unix() >= st.Deadline {
		doRollback(ops, unit, "not confirmed in time (expiry timer)")
	}
}

// offerConfirm: an interactive change (the menu) is confirmed right there —
// the operator checks the tunnel, then answers y; anything else leaves the
// automatic rollback running. Before this the menu printed only the
// `hs2 apply --confirm` command, so a menu change rolled back by itself unless
// the operator went to a shell.
func offerConfirm(unit string, st applyState, within time.Duration) {
	if !st.live() || !isTTY(os.Stdin) {
		return
	}
	in := asker{bufioReader(), os.Stdout}
	v, err := in.ask(fmt.Sprintf(tr("Check it now (dashboard, users). Keep the change? y = confirm, Enter = let it roll back by itself in %s [y/N]",
		"اکنون بررسی کنید (داشبورد، کاربران). تغییر بماند؟ y = تأیید، Enter = خودش پس از %s برمی‌گردد [y/N]"), within), "n")
	if err != nil || (v != "y" && v != "Y") {
		fmt.Println(tr("Not confirmed — it rolls back by itself unless you run: hs2 apply --confirm -u "+cleanUnit(unit),
			"تأیید نشد — خودش برمی‌گردد مگر اجرا کنید: hs2 apply --confirm -u "+cleanUnit(unit)))
		return
	}
	// a quick y: wait for the health gate (it ends in trial, or rolls back)
	err = confirmApply(realOps{}, unit)
	for i := 0; errors.Is(err, errHealthPending) && i < 90; i++ {
		if i == 0 {
			fmt.Println(tr("waiting for the health check…", "در انتظار بررسی سلامت…"))
		}
		time.Sleep(time.Second)
		err = confirmApply(realOps{}, unit)
	}
	if err != nil {
		fmt.Println("ERROR: " + err.Error())
		return
	}
	fmt.Println(tr("confirmed — the new config stays.", "تأیید شد — پیکربندی جدید می‌ماند."))
}
