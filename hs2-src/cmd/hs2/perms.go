package main

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// sec370 (sec353 1g, L1/L3): hs2's own files hold the tunnel keys (configs,
// certs' keys, alert.conf's bot token, the version history) or show the
// tunnel topology (/run/hs2 status). New files are written 0600 in 0700
// directories; files from older releases are tightened by the installer and
// by `hs2 doctor --perms-fix` (the deep doctor lists them as fix "perms").
//
// HS2_RUN_SHARED=1 keeps the old world-readable /run/hs2 status and the 24 h
// stats file (0644 in a 0755 dir), for a local non-root reader that needs them.

var (
	etcHS2Dir     = "/etc/hs2"
	varLibHS2Dir  = "/var/lib/hs2"
	permRootsFunc = func() []string { return []string{etcHS2Dir, varLibHS2Dir, statusRunDir} }
)

// runShared: HS2_RUN_SHARED=1 (the old modes for /run/hs2 and stats).
func runShared() bool { return os.Getenv("HS2_RUN_SHARED") == "1" }

// runSharedMarker (b376): a daemon or stats run with HS2_RUN_SHARED=1 leaves
// this file in /run/hs2, so `hs2 doctor --perms(-fix)` and the installer,
// which usually run without the unit's environment, see the shared mode and
// leave its files alone instead of fighting the daemon every tick. It lives
// on tmpfs: a reboot clears it, and the next shared run writes it again.
var runSharedMarker = func() string { return filepath.Join(statusRunDir, "run-shared") }

// markRunShared writes the marker when this process runs shared (stats runs).
func markRunShared() {
	if runShared() && runSharedRoot() {
		_ = os.WriteFile(runSharedMarker(), []byte("HS2_RUN_SHARED=1\n"), 0o644)
	}
}

// runSharedRoot: only root writes or removes the marker or widens /run/hs2
// (a hook for the tests).
var runSharedRoot = func() bool { return os.Geteuid() == 0 }

// syncRunShared (b378) is the daemon's start step, after MkdirAll(statusRunDir):
// shared, it widens statusRunDir to runDirMode() (MkdirAll never chmods an
// existing dir, and the installer's first upgrade to a shared daemon runs
// before the marker exists, so it may have left /run/hs2 0700) and writes the
// marker; not shared, it removes a stale marker so doctor and the installer
// check /run/hs2 and the stats files again. Root only; a symlinked or
// non-root-owned /run/hs2 is never touched.
func syncRunShared() {
	if !runSharedRoot() {
		return
	}
	if !runShared() {
		if fi, err := os.Lstat(runSharedMarker()); err == nil && fi.Mode().IsRegular() {
			_ = os.Remove(runSharedMarker())
		}
		return
	}
	fi, err := os.Lstat(statusRunDir)
	if err != nil || !fi.IsDir() || !trustedOwner(fi) {
		return
	}
	if fi.Mode().Perm() != runDirMode() {
		_, _ = chmodNoFollow(statusRunDir, runDirMode())
	}
	markRunShared()
}

// runSharedEffective: HS2_RUN_SHARED=1 here, or a daemon on this box runs so.
func runSharedEffective() bool {
	if runShared() {
		return true
	}
	fi, err := os.Lstat(runSharedMarker())
	return err == nil && fi.Mode().IsRegular()
}

// sharedExempt: with the shared mode, /run/hs2 and the 24 h stats files are
// the daemon's to keep 0644/0755.
func sharedExempt(p string) bool {
	if filepath.Dir(p) == varLibHS2Dir {
		if m, _ := filepath.Match("stats-*.json", filepath.Base(p)); m {
			return true
		}
	}
	return false
}

// runFileMode / runDirMode: the modes of status/stats files and their dirs.
func runFileMode() os.FileMode {
	if runShared() {
		return 0o644
	}
	return 0o600
}

func runDirMode() os.FileMode {
	if runShared() {
		return 0o755
	}
	return 0o700
}

// writeFileAtomicMode writes b to a fresh temp file beside path (a random
// name, created O_EXCL, so a stale temp left by a crash never lends its mode),
// sets mode and renames it over path. The temp is removed on any failure.
func writeFileAtomicMode(path string, b []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			f.Close()
			os.Remove(tmp)
		}
	}()
	if _, err := f.Write(b); err != nil {
		return err
	}
	if err := f.Chmod(mode); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	return nil
}

// permIssue is one file or directory with group/other access.
type permIssue struct {
	Path      string
	Have, Fix os.FileMode
	Root      string // b378: the scanned root it is under ("" = its own dir)
}

// scanPerms walks roots (symlinks are not followed, a missing root is
// skipped) and returns every entry with group or other bits: directories
// should be 0700 and files owner-only (the owner's own bits are kept). With
// HS2_RUN_SHARED=1 (here or via the marker) the /run/hs2 root (statusRunDir)
// and /var/lib/hs2/stats-*.json are not scanned. The ACME
// webroot /var/lib/hs2/acme (public challenge tokens) is left alone.
func scanPerms(roots []string) []permIssue {
	var out []permIssue
	seen := map[string]bool{}
	shared := runSharedEffective()
	for _, root := range roots {
		if root == "" || seen[root] || (shared && root == statusRunDir) {
			continue
		}
		seen[root] = true
		filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.Type()&fs.ModeSymlink != 0 {
				return nil
			}
			if d.IsDir() && p == filepath.Join(varLibHS2Dir, "acme") {
				return filepath.SkipDir // public ACME challenge tokens
			}
			if shared && !d.IsDir() && sharedExempt(p) {
				return nil
			}
			fi, err := d.Info()
			if err != nil {
				return nil
			}
			m := fi.Mode().Perm()
			if m&0o077 == 0 {
				return nil
			}
			want := m &^ 0o077
			if d.IsDir() {
				want = 0o700
			}
			out = append(out, permIssue{Path: p, Have: m, Fix: want, Root: root})
			return nil
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// fixPerms applies the fixes; it returns how many were changed and the errors.
func fixPerms(is []permIssue) (int, []error) {
	n := 0
	var errs []error
	for _, i := range is {
		// b376: never chmod through a symlink swapped in since the scan
		// (chmodNoFollow: one fd, opened without following any symlink)
		// (chmodBeneath: the root's parent may be a root-made symlinked
		// system dir; nothing under the hs2 root is followed)
		prefix := filepath.Dir(i.Path)
		if i.Root != "" && (i.Path == i.Root || strings.HasPrefix(i.Path, i.Root+"/")) {
			prefix = filepath.Dir(i.Root)
		}
		rel, err := filepath.Rel(prefix, i.Path)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		changed, err := chmodBeneath(prefix, rel, i.Fix)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if changed {
			n++
		}
	}
	return n, errs
}

// doctorPerms: `hs2 doctor --perms` (report) / `--perms-fix` (fix). 0 = clean
// or fixed.
func doctorPerms(fix bool, out io.Writer) int {
	is := scanPerms(permRootsFunc())
	if runSharedEffective() {
		fmt.Fprintln(out, tr("NOTE: HS2_RUN_SHARED=1 is in use (env or "+runSharedMarker()+"): "+statusRunDir+" and the stats files keep 0644/0755 and are not checked",
			"توجه: HS2_RUN_SHARED=1 فعال است (env یا "+runSharedMarker()+"): "+statusRunDir+" و فایل‌های آمار 0644/0755 می‌مانند و بررسی نمی‌شوند"))
	}
	if len(is) == 0 {
		fmt.Fprintln(out, tr("OK: hs2's files are owner-only (dirs 0700, files 0600): ", "درست: فایل‌های hs2 فقط برای مالک است (پوشه 0700، فایل 0600): ")+strings.Join(permRootsFunc(), " "))
		return 0
	}
	for _, i := range is {
		fmt.Fprintf(out, "  %04o -> %04o  %s\n", i.Have, i.Fix, i.Path)
	}
	if !fix {
		fmt.Fprintf(out, tr("%d file(s)/dir(s) readable by other local users — fix: hs2 doctor --perms-fix\n", "%d فایل/پوشه برای کاربران محلی دیگر خواندنی است — اصلاح: hs2 doctor --perms-fix\n"), len(is))
		return 1
	}
	n, errs := fixPerms(is)
	for _, e := range errs {
		fmt.Fprintln(out, "  ERROR: "+e.Error())
	}
	fmt.Fprintf(out, tr("fixed %d of %d\n", "%d از %d اصلاح شد\n"), n, len(is))
	// b378: whatever is still loose (could not be fixed for any reason) is
	// listed and the exit code says so.
	left := scanPerms(permRootsFunc())
	for _, i := range left {
		fmt.Fprintf(out, tr("  NOT FIXED: %04o  %s\n", "  اصلاح نشد: %04o  %s\n"), i.Have, i.Path)
	}
	if len(errs) > 0 || len(left) > 0 {
		return 1
	}
	return 0
}

// checkPerms: the deep doctor's finding (category tunnel) with fix "perms".
func checkPerms(env *deepEnv) []finding {
	if env.etc == "" {
		return nil
	}
	roots := []string{env.etc, env.runDir}
	if env.etc == etcHS2Dir {
		roots = permRootsFunc()
	}
	is := scanPerms(roots)
	if len(is) == 0 {
		return nil
	}
	ex := is[0].Path
	if len(is) > 1 {
		ex += fmt.Sprintf(tr(" and %d more", " و %d مورد دیگر"), len(is)-1)
	}
	return []finding{{Sev: "WARN", Area: tr("file permissions", "مجوز فایل‌ها"), Cat: "tunnel",
		What: fmt.Sprintf(tr("%d hs2 file(s)/dir(s) readable by other local users: %s", "%d فایل/پوشهٔ hs2 برای کاربران محلی دیگر خواندنی است: %s"), len(is), ex),
		Why:  tr("they hold the tunnel keys or show the tunnel topology", "کلیدهای تونل را دارند یا ساختار تونل را نشان می‌دهند"),
		Do:   "hs2 doctor --perms-fix",
		Fix:  &fixAction{Kind: "perms", Title: tr("make hs2's files owner-only (dirs 0700, files 0600)", "فقط-مالک کردن فایل‌های hs2 (پوشه 0700، فایل 0600)")}}}
}
