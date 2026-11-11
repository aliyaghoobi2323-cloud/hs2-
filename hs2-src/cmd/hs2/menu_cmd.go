package main

// The command-line face of menu M1 (every menu action is also a command):
//
//	hs2 config history  -u <unit>
//	hs2 config show     -u <unit> [--version N]
//	hs2 config diff     -u <unit> (--new <file> | --version N)
//	hs2 config validate [-u <unit>] --new <file>
//	hs2 config lock|unlock|lock-status [--pid P] [--force]
//	hs2 apply   -u <unit> --new <file> [--confirm-within 5m] [--health-gate] [--reason r] [--no-wait] [--force]
//	hs2 apply   --confirm -u <unit>
//	hs2 apply   --status  -u <unit>
//	hs2 apply   --supported
//	hs2 rollback -u <unit> [--to N] [--force]
//
// --lang fa on any of them prints Persian.

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"
)

var menuSubs = map[string]bool{"history": true, "show": true, "diff": true, "validate": true, "lock": true, "unlock": true, "lock-status": true}

func die(format string, a ...any) {
	fmt.Printf("ERROR: "+format+"\n", a...)
	os.Exit(1)
}

func unitFlags(fs *flag.FlagSet) (unit, cfg, lang *string) {
	unit = fs.String("u", "hs2", "systemd unit of the tunnel")
	cfg = fs.String("c", "", "config file (default: from the unit name)")
	lang = fs.String("lang", "en", "output language: en|fa")
	return
}

func resolveCfg(unit, cfg string) string {
	if cfg != "" {
		return cfg
	}
	return configForUnit(unit)
}

func menuConfigCmd(sub string, args []string) {
	fs := flag.NewFlagSet("config "+sub, flag.ExitOnError)
	unit, cfg, lang := unitFlags(fs)
	newF := fs.String("new", "", "proposed config file")
	ver := fs.Int("version", 0, "stored version number")
	pid := fs.Int("pid", 0, "lock: the holder's pid (default: the caller, e.g. the menu shell)")
	force := fs.Bool("force", false, "lock: break another admin's lock")
	fs.Parse(args)
	deepLang = *lang
	path := resolveCfg(*unit, *cfg)
	switch sub {
	case "history":
		vs, err := listVersions(*unit)
		if err != nil {
			die("%v", err)
		}
		if len(vs) == 0 {
			fmt.Println(tr("no stored versions yet", "هنوز نسخه‌ای ذخیره نشده"))
			return
		}
		cur := currentVersion(*unit, path, vs)
		for _, v := range vs {
			mark := ""
			if v.N == cur {
				mark = "  <- " + tr("current", "فعلی")
			}
			fmt.Printf("%4d  %s  %s%s\n", v.N, v.Time.Format("2006-01-02 15:04:05"), v.Reason, mark)
		}
		if st, ok := loadState(*unit); ok {
			fmt.Printf(tr("last apply: v%d->v%d %s (%s)\n", "آخرین اعمال: v%d->v%d %s (%s)\n"), st.From, st.To, st.State, st.Detail)
		}
	case "show":
		p := path
		if *ver > 0 {
			v, err := findVersion(*unit, *ver)
			if err != nil {
				die("%v", err)
			}
			p = v.Path
		}
		b, err := os.ReadFile(p)
		if err != nil {
			die("%v", err)
		}
		os.Stdout.Write(b)
	case "diff":
		cur, err := os.ReadFile(path)
		if err != nil {
			die("%v", err)
		}
		var nb []byte
		ndi := readDropins(*unit)
		switch {
		case *newF != "":
			if nb, err = os.ReadFile(*newF); err != nil {
				die("%v", err)
			}
		case *ver > 0:
			v, err := findVersion(*unit, *ver)
			if err != nil {
				die("%v", err)
			}
			nb, _ = os.ReadFile(v.Path)
			ndi = v.dropins()
		default:
			die("diff needs --new <file> or --version N")
		}
		d, err := semanticDiff(cur, nb, readDropins(*unit), ndi)
		if err != nil {
			die("%v", err)
		}
		printDiff(d)
	case "validate":
		if *newF == "" {
			die("validate needs --new <file>")
		}
		nb, err := os.ReadFile(*newF)
		if err != nil {
			die("%v", err)
		}
		cur, _ := os.ReadFile(path)
		errs, warns := validateFull(nb, cur, path, realHost)
		printValidation(errs, warns)
		if len(errs) > 0 {
			os.Exit(1)
		}
	case "lock":
		p := *pid
		if p == 0 {
			p = os.Getppid()
		}
		if _, err := acquireLock(p, *force, time.Now()); err != nil {
			die("%v", err)
		}
		fmt.Printf("locked (pid %d)\n", p)
	case "unlock":
		l, ok := readLock()
		if ok && l.PID != os.Getppid() && l.PID != *pid && !*force && !lockStale(l, time.Now()) {
			die(tr("the lock belongs to %s (pid %d) — use --force", "قفل متعلق به %s (pid %d) است — --force بزنید"), l.Owner, l.PID)
		}
		os.Remove(adminLockPath)
		fmt.Println("unlocked")
	case "lock-status":
		l, ok := readLock()
		if !ok {
			fmt.Println(tr("free", "آزاد"))
			return
		}
		st := ""
		if lockStale(l, time.Now()) {
			st = " (" + tr("stale", "کهنه") + ")"
		}
		fmt.Printf("held by %s pid %d %s since %s%s\n", l.Owner, l.PID, l.TTY, time.Unix(l.Time, 0).Format("2006-01-02 15:04:05"), st)
	}
}

// currentVersion is the NEWEST stored version equal to the live config and
// drop-ins (several versions can hold the same bytes: a pre-apply snapshot
// and its rollback); 0 = none.
func currentVersion(unit, path string, vs []cfgVersion) int {
	cur, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	di := readDropins(unit)
	for i := len(vs) - 1; i >= 0; i-- {
		if b, err := os.ReadFile(vs[i].Path); err == nil && string(b) == string(cur) && sameDropins(vs[i].dropins(), di) {
			return vs[i].N
		}
	}
	for i := len(vs) - 1; i >= 0; i-- { // the drop-ins changed by hand since
		if b, err := os.ReadFile(vs[i].Path); err == nil && string(b) == string(cur) {
			return vs[i].N
		}
	}
	return 0
}

func applyCmd(args []string) {
	fs := flag.NewFlagSet("apply", flag.ExitOnError)
	unit, cfg, lang := unitFlags(fs)
	newF := fs.String("new", "", "proposed config file")
	within := fs.Duration("confirm-within", 5*time.Minute, "roll back unless confirmed within this time")
	gate := fs.Bool("health-gate", false, "also require >= 1 serving link within 60 s")
	reason := fs.String("reason", "manual", "short reason for the history and audit log")
	confirm := fs.Bool("confirm", false, "confirm the apply in trial")
	status := fs.Bool("status", false, "show the apply state")
	noWait := fs.Bool("no-wait", false, "do not follow the health gate")
	force := fs.Bool("force", false, "break another admin's lock")
	supported := fs.Bool("supported", false, "exit 0: this binary has safe apply")
	worker := fs.Bool("worker", false, "internal: the detached worker")
	expire := fs.Bool("expire", false, "internal: the expiry timer")
	fs.Parse(args)
	deepLang = *lang
	ops := realOps{}
	switch {
	case *supported:
		fmt.Println("apply-v1")
		return
	case *worker:
		runWorker(ops, *unit)
		return
	case *expire:
		expireApply(ops, *unit)
		return
	case *status:
		st, ok := loadState(*unit)
		if !ok {
			fmt.Println(tr("no apply recorded", "اعمالی ثبت نشده"))
			return
		}
		printState(st)
		return
	case *confirm:
		if err := confirmApply(ops, *unit); err != nil {
			die("%v", err)
		}
		fmt.Println(tr("confirmed — the new config stays.", "تأیید شد — پیکربندی جدید می‌ماند."))
		return
	}
	if *newF == "" {
		die("apply needs --new <file> (or --confirm / --status)")
	}
	if *within < 30*time.Second {
		die("--confirm-within must be at least 30s")
	}
	path := resolveCfg(*unit, *cfg)
	nb, err := os.ReadFile(*newF)
	if err != nil {
		die("%v", err)
	}
	cur, _ := os.ReadFile(path)
	errs, warns := validateFull(nb, cur, path, realHost)
	printValidation(errs, warns)
	if len(errs) > 0 {
		die(tr("the new config is invalid — nothing was changed", "پیکربندی جدید نامعتبر است — چیزی تغییر نکرد"))
	}
	release, err := acquireLock(os.Getpid(), *force, time.Now())
	if err != nil {
		die("%v", err)
	}
	defer release()
	st, d, err := startApply(ops, applyReq{Unit: *unit, Cfg: path, Reason: *reason, New: nb, Within: *within, HealthGate: *gate})
	if d != nil {
		fmt.Println(tr("changes:", "تغییرات:"))
		printDiff(d)
	}
	if err != nil {
		release()
		die("%v", err)
	}
	fmt.Printf(tr("applied in trial mode (v%d -> v%d); restart runs detached — safe if this SSH session drops.\n",
		"در حالت آزمایشی اعمال شد (v%d -> v%d)؛ راه‌اندازی مجدد جدا از این نشست اجرا می‌شود — قطع SSH مشکلی ندارد.\n"), st.From, st.To)
	if len(st.Bilateral) > 0 {
		fmt.Println(tr("ON THE OTHER SERVER: apply the same value(s) for: ", "روی سرور مقابل: همین مقدار(ها) را اعمال کنید برای: ") + fmt.Sprint(st.Bilateral))
		fmt.Println(tr("  e.g. hs2 config diff there, then hs2 apply -u <its unit> --new <its edited file>",
			"  مثلاً آن‌جا hs2 config diff و سپس hs2 apply -u <واحد آن> --new <فایل ویرایش‌شده>"))
	}
	if !*noWait {
		last := ""
		for i := 0; i < int(healthWindow/time.Second)+15; i++ {
			s, _ := loadState(*unit)
			if s.State != last {
				fmt.Printf("  state: %s %s\n", s.State, s.Detail)
				last = s.State
			}
			if s.State != stRestarting && s.State != stHealth {
				break
			}
			time.Sleep(time.Second)
		}
		if s, _ := loadState(*unit); s.State == stRolledBack || s.State == stFailed {
			release()
			die(tr("the new config did not pass the health check — %s: %s", "پیکربندی جدید از بررسی سلامت رد نشد — %s: %s"), s.State, s.Detail)
		}
	}
	fmt.Printf(tr("confirm within %s:  hs2 apply --confirm -u %s   (otherwise it rolls back to v%d by itself)\n",
		"ظرف %s تأیید کنید:  hs2 apply --confirm -u %s   (وگرنه خودکار به v%d برمی‌گردد)\n"), within.String(), cleanUnit(*unit), st.From)
}

func printState(st applyState) {
	left := time.Until(time.Unix(st.Deadline, 0)).Round(time.Second)
	fmt.Printf("unit %s  state %s  v%d -> v%d  by %s  reason %s\n", st.Unit, st.State, st.From, st.To, st.Who, st.Reason)
	if st.State == stTrial && left > 0 {
		fmt.Printf("  %s left to confirm (hs2 apply --confirm -u %s)\n", left, st.Unit)
	}
	if st.Detail != "" {
		fmt.Println("  " + st.Detail)
	}
}

func rollbackCmd(args []string) {
	fs := flag.NewFlagSet("rollback", flag.ExitOnError)
	unit, cfg, lang := unitFlags(fs)
	to := fs.Int("to", 0, "version to go back to (default: the version before the current config)")
	force := fs.Bool("force", false, "break another admin's lock")
	fs.Parse(args)
	deepLang = *lang
	ops := realOps{}
	release, err := acquireLock(os.Getpid(), *force, time.Now())
	if err != nil {
		die("%v", err)
	}
	defer release()
	if st, ok := loadState(*unit); ok && st.live() && (*to == 0 || *to == st.From) {
		doRollback(ops, *unit, "manual rollback")
		s, _ := loadState(*unit)
		fmt.Printf("%s: %s\n", s.State, s.Detail)
		return
	}
	path := resolveCfg(*unit, *cfg)
	n := *to
	if n == 0 {
		cur, _ := os.ReadFile(path)
		vs, _ := listVersions(*unit)
		for i := len(vs) - 1; i >= 0; i-- {
			if b, err := os.ReadFile(vs[i].Path); err == nil && string(b) != string(cur) {
				n = vs[i].N
				break
			}
		}
		if n == 0 {
			release()
			die(tr("no earlier version to roll back to", "نسخهٔ قبلی برای برگشت نیست"))
		}
	}
	v, err := findVersion(*unit, n)
	if err != nil {
		release()
		die("%v", err)
	}
	cur, _ := os.ReadFile(path)
	vb, _ := os.ReadFile(v.Path)
	d, _ := semanticDiff(cur, vb, readDropins(*unit), v.dropins())
	printDiff(d)
	snapshotCurrent(*unit, path, "pre-rollback", time.Now())
	who, tty := whoAmI()
	res := "ok"
	if err := restoreVersion(ops, *unit, path, n, "rollback-"+strconv.Itoa(n)); err != nil {
		res = err.Error()
	}
	audit(time.Now(), who, tty, *unit, "rollback", "to v"+strconv.Itoa(n), diffSummary(d), res)
	fmt.Printf(tr("rolled back to v%d: %s\n", "به v%d برگشت: %s\n"), n, res)
}
