package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func mode(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

// sec370 1g: atomic writes are 0600 even over a stale world-readable temp or
// target, and leave no temp behind.
func TestWriteFileAtomic0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix modes")
	}
	d := t.TempDir()
	p := filepath.Join(d, "x.json")
	os.WriteFile(p, []byte("old"), 0o644)
	os.WriteFile(p+".tmp", []byte("stale"), 0o666) // the old fixed temp name
	if err := writeFileAtomic(p, []byte("new")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "new" || mode(t, p) != 0o600 {
		t.Fatalf("content %q mode %o", b, mode(t, p))
	}
	ents, _ := os.ReadDir(d)
	if len(ents) != 2 { // x.json and the untouched stale x.json.tmp
		t.Fatalf("temp left behind: %v", ents)
	}
	// alert.conf: 0600 in a 0700 dir
	ac := filepath.Join(d, "etc", "alert.conf")
	if err := writeAlertConf(ac, "token=x\n"); err != nil {
		t.Fatal(err)
	}
	if mode(t, ac) != 0o600 || mode(t, filepath.Dir(ac)) != 0o700 {
		t.Fatalf("alert.conf %o dir %o", mode(t, ac), mode(t, filepath.Dir(ac)))
	}
}

// sec370 1g: status / stats files are 0600 in a 0700 dir; HS2_RUN_SHARED=1
// keeps the old 0644 / 0755.
func TestRunFilesModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix modes")
	}
	d := t.TempDir()
	defer func(s string) { statusRunDir = s }(statusRunDir)
	statusRunDir = filepath.Join(d, "run") // the shared run writes its marker here
	rootHook(t)
	sp := filepath.Join(d, "lib", "stats-hs2.json")
	if err := saveStats(sp, &statsFile{}); err != nil {
		t.Fatal(err)
	}
	if mode(t, sp) != 0o600 || mode(t, filepath.Dir(sp)) != 0o700 {
		t.Fatalf("stats %o dir %o", mode(t, sp), mode(t, filepath.Dir(sp)))
	}
	st := filepath.Join(d, "status.json")
	writeStatusFile(st, liveStatus{})
	if mode(t, st) != 0o600 {
		t.Fatalf("status %o", mode(t, st))
	}
	t.Setenv("HS2_RUN_SHARED", "1")
	writeStatusFile(st, liveStatus{})
	sp2 := filepath.Join(d, "lib2", "stats-hs2.json")
	saveStats(sp2, &statsFile{})
	if mode(t, st) != 0o644 || mode(t, sp2) != 0o644 || mode(t, filepath.Dir(sp2)) != 0o755 {
		t.Fatalf("HS2_RUN_SHARED=1: status %o stats %o dir %o", mode(t, st), mode(t, sp2), mode(t, filepath.Dir(sp2)))
	}
	if _, err := os.Stat(filepath.Join(statusRunDir, "run-shared")); err != nil {
		t.Fatalf("shared stats run left no marker: %v", err)
	}
}

// b376: a daemon running with HS2_RUN_SHARED=1 from its unit leaves a marker;
// doctor (without that env) then leaves /run/hs2 and the stats files alone
// instead of chmodding them back every tick, and says so.
func TestDoctorPermsHonoursSharedMarker(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix modes")
	}
	d := t.TempDir()
	defer func(a, b string) { statusRunDir, varLibHS2Dir = a, b }(statusRunDir, varLibHS2Dir)
	statusRunDir, varLibHS2Dir = filepath.Join(d, "run"), filepath.Join(d, "lib")
	oldRoots := permRootsFunc
	permRootsFunc = func() []string { return []string{varLibHS2Dir, statusRunDir} }
	defer func() { permRootsFunc = oldRoots }()
	rootHook(t)
	os.MkdirAll(statusRunDir, 0o755)
	os.Chmod(statusRunDir, 0o755)
	os.MkdirAll(varLibHS2Dir, 0o700)
	os.WriteFile(filepath.Join(statusRunDir, "hs2.json"), []byte("{}"), 0o644)
	stats := filepath.Join(varLibHS2Dir, "stats-hs2.json")
	os.WriteFile(stats, []byte("{}"), 0o644)
	other := filepath.Join(varLibHS2Dir, "history.json")
	os.WriteFile(other, []byte("{}"), 0o644)
	t.Setenv("HS2_RUN_SHARED", "")

	// no marker: everything is loose
	if n := len(scanPerms(permRootsFunc())); n != 4 {
		t.Fatalf("without marker: %d issues, want 4", n)
	}
	// the daemon (env from its unit) writes the marker
	t.Setenv("HS2_RUN_SHARED", "1")
	markRunShared()
	t.Setenv("HS2_RUN_SHARED", "")
	var out bytes.Buffer
	if rc := doctorPerms(true, &out); rc != 0 || !strings.Contains(out.String(), "HS2_RUN_SHARED") {
		t.Fatalf("rc %d: %s", rc, out.String())
	}
	if mode(t, statusRunDir) != 0o755 || mode(t, stats) != 0o644 {
		t.Fatal("--perms-fix fought the shared daemon")
	}
	if mode(t, other) != 0o600 {
		t.Fatal("a non-shared file was not fixed")
	}
}

// sec370 1g: the doctor finds loose modes under the roots, fixes them, never
// follows a symlink, and keeps the owner's exec bit.
func TestScanFixPerms(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix modes")
	}
	d := t.TempDir()
	etc, run := filepath.Join(d, "etc"), filepath.Join(d, "run")
	os.MkdirAll(filepath.Join(etc, "certs"), 0o755)
	os.Chmod(etc, 0o755)
	os.MkdirAll(run, 0o700)
	os.WriteFile(filepath.Join(etc, "hs2.json"), []byte("{}"), 0o644)
	os.WriteFile(filepath.Join(etc, "certs", "k.pem"), []byte("k"), 0o640)
	os.WriteFile(filepath.Join(etc, "ok.json"), []byte("{}"), 0o600)
	os.WriteFile(filepath.Join(run, "tool"), []byte("#!"), 0o755)
	outside := filepath.Join(d, "outside")
	os.WriteFile(outside, []byte("x"), 0o644)
	os.Symlink(outside, filepath.Join(etc, "link"))

	old := permRootsFunc
	permRootsFunc = func() []string { return []string{etc, run, filepath.Join(d, "missing")} }
	defer func() { permRootsFunc = old }()

	is := scanPerms(permRootsFunc())
	got := map[string]os.FileMode{}
	for _, i := range is {
		got[strings.TrimPrefix(i.Path, d)] = i.Fix
	}
	want := map[string]os.FileMode{"/etc": 0o700, "/etc/certs": 0o700, "/etc/hs2.json": 0o600, "/etc/certs/k.pem": 0o600, "/run/tool": 0o700}
	if len(got) != len(want) {
		t.Fatalf("scan: %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("scan %s: %o want %o (%v)", k, got[k], v, got)
		}
	}
	var out bytes.Buffer
	if rc := doctorPerms(false, &out); rc != 1 || mode(t, filepath.Join(etc, "hs2.json")) != 0o644 {
		t.Fatalf("report-only changed something or rc %d: %s", rc, out.String())
	}
	out.Reset()
	if rc := doctorPerms(true, &out); rc != 0 {
		t.Fatalf("fix rc %d: %s", rc, out.String())
	}
	for k, v := range want {
		if m := mode(t, d+k); m != v {
			t.Fatalf("after fix %s: %o want %o", k, m, v)
		}
	}
	if mode(t, outside) != 0o644 {
		t.Fatal("followed the symlink")
	}
	if len(scanPerms(permRootsFunc())) != 0 {
		t.Fatal("still loose after fix")
	}
	// the deep doctor finding carries fix "perms" -> hs2 doctor --perms-fix
	os.Chmod(filepath.Join(etc, "hs2.json"), 0o644)
	fs := checkPerms(&deepEnv{etc: etc, runDir: run})
	if len(fs) != 1 || fs[0].Fix == nil || fs[0].Fix.Kind != "perms" {
		t.Fatalf("finding: %+v", fs)
	}
	if a := fixCommand(*fs[0].Fix, "", 0); strings.Join(a, " ") != "doctor --perms-fix" {
		t.Fatalf("fix argv %v", a)
	}
}

// b376: fixPerms never chmods through a symlink swapped in after the scan,
// neither at the entry itself nor at a directory in its path.
func TestFixPermsSymlinkSwap(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix modes")
	}
	d := t.TempDir()
	victim := filepath.Join(d, "victim")
	os.WriteFile(victim, []byte("x"), 0o644)
	vdir := filepath.Join(d, "vdir")
	os.MkdirAll(vdir, 0o755)
	os.WriteFile(filepath.Join(vdir, "f"), []byte("x"), 0o644)
	etc := filepath.Join(d, "etc")
	os.MkdirAll(etc, 0o700)
	// the scan saw a file and a directory; both are now symlinks
	os.Symlink(victim, filepath.Join(etc, "a.json"))
	os.Symlink(vdir, filepath.Join(etc, "sub"))
	real := filepath.Join(etc, "real.json")
	os.WriteFile(real, []byte("{}"), 0o644)
	n, errs := fixPerms([]permIssue{
		{Path: filepath.Join(etc, "a.json"), Have: 0o644, Fix: 0o600, Root: etc},
		{Path: filepath.Join(etc, "sub"), Have: 0o755, Fix: 0o700, Root: etc},
		{Path: filepath.Join(etc, "sub", "f"), Have: 0o644, Fix: 0o600, Root: etc},
		{Path: real, Have: 0o644, Fix: 0o600, Root: etc},
		{Path: filepath.Join(etc, "gone"), Have: 0o644, Fix: 0o600, Root: etc},
	})
	// b378: a swapped-in symlink is not fixed and is reported; gone is not an error
	want := 2
	if runtime.GOOS == "linux" && openat2Works() {
		want = 3
	}
	if len(errs) != want {
		t.Fatalf("errors: %v (want %d)", errs, want)
	}
	if mode(t, victim) != 0o644 || mode(t, vdir) != 0o755 {
		t.Fatal("followed a symlinked entry")
	}
	if runtime.GOOS == "linux" && openat2Works() && mode(t, filepath.Join(vdir, "f")) != 0o644 {
		t.Fatal("followed a symlinked directory in the path")
	}
	if mode(t, real) != 0o600 || n < 1 {
		t.Fatalf("real file not fixed (n=%d)", n)
	}
}

// rootHook: the marker/chmod steps are root-only; the tests act as root.
func rootHook(t *testing.T) {
	old := runSharedRoot
	runSharedRoot = func() bool { return true }
	t.Cleanup(func() { runSharedRoot = old })
}

// b378: the daemon's start step widens a 0700 /run/hs2 (left by the installer
// before the first shared daemon wrote its marker) and writes the marker when
// shared, removes a stale marker when not, and does nothing as non-root or to
// a symlinked /run/hs2.
func TestSyncRunShared(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix modes")
	}
	d := t.TempDir()
	defer func(s string) { statusRunDir = s }(statusRunDir)
	statusRunDir = filepath.Join(d, "run")
	rootHook(t)
	os.Mkdir(statusRunDir, 0o700)
	marker := filepath.Join(statusRunDir, "run-shared")

	t.Setenv("HS2_RUN_SHARED", "1")
	syncRunShared()
	if mode(t, statusRunDir) != 0o755 {
		t.Fatalf("shared: /run/hs2 %o, want 755", mode(t, statusRunDir))
	}
	if _, err := os.Lstat(marker); err != nil {
		t.Fatalf("shared: no marker: %v", err)
	}

	t.Setenv("HS2_RUN_SHARED", "")
	syncRunShared()
	if _, err := os.Lstat(marker); err == nil {
		t.Fatal("not shared: stale marker kept")
	}
	if runSharedEffective() {
		t.Fatal("not shared: still effective")
	}

	// non-root: nothing at all
	os.Chmod(statusRunDir, 0o700)
	runSharedRoot = func() bool { return false }
	t.Setenv("HS2_RUN_SHARED", "1")
	syncRunShared()
	if mode(t, statusRunDir) != 0o700 {
		t.Fatal("non-root widened /run/hs2")
	}
	if _, err := os.Lstat(marker); err == nil {
		t.Fatal("non-root wrote the marker")
	}
	os.WriteFile(marker, nil, 0o644)
	t.Setenv("HS2_RUN_SHARED", "")
	syncRunShared()
	if _, err := os.Lstat(marker); err != nil {
		t.Fatal("non-root removed the marker")
	}
	os.Remove(marker)

	// a symlinked /run/hs2 is never chmodded through
	runSharedRoot = func() bool { return true }
	real := filepath.Join(d, "elsewhere")
	os.Mkdir(real, 0o700)
	statusRunDir = filepath.Join(d, "runlink")
	os.Symlink(real, statusRunDir)
	t.Setenv("HS2_RUN_SHARED", "1")
	syncRunShared()
	if mode(t, real) != 0o700 {
		t.Fatal("chmod followed a symlinked /run/hs2")
	}
}
