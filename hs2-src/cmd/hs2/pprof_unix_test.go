package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// sec370 1b: HS2_PPROF=unix:PATH serves on a 0600 socket; cmdline is gone
// unless HS2_PPROF_CMDLINE=1; a non-socket file at PATH is never replaced.
func TestPprofUnixSocket0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets")
	}
	d := t.TempDir()
	p := filepath.Join(d, "run", "pprof.sock")
	ln, err := pprofListen("unix:" + p)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	fi, err := os.Lstat(p)
	if err != nil || fi.Mode()&os.ModeSocket == 0 || fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket %v mode %v", err, fi.Mode())
	}
	if di, _ := os.Stat(filepath.Dir(p)); di.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v", di.Mode().Perm())
	}
	if ents, _ := os.ReadDir(filepath.Dir(p)); len(ents) != 1 {
		t.Fatalf("temp dir left behind: %v", ents)
	}
	go http.Serve(ln, pprofMux())
	c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", p)
	}}}
	get := func(path string) int {
		r, err := c.Get("http://x" + path)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode
	}
	if s := get("/debug/pprof/"); s != 200 {
		t.Fatalf("index %d", s)
	}
	if s := get("/debug/pprof/cmdline"); s != 404 {
		t.Fatalf("cmdline %d, want 404", s)
	}
	t.Setenv("HS2_PPROF_CMDLINE", "1")
	if s := httpStatus(t, pprofMux(), "/debug/pprof/cmdline"); s != 200 {
		t.Fatalf("cmdline with HS2_PPROF_CMDLINE=1: %d", s)
	}
	// a regular file at the path is not replaced
	f := filepath.Join(d, "plain")
	os.WriteFile(f, []byte("x"), 0o600)
	if _, err := pprofListen("unix:" + f); err == nil {
		t.Fatal("replaced a regular file")
	}
	if b, _ := os.ReadFile(f); string(b) != "x" {
		t.Fatal("regular file changed")
	}
	// a stale socket is replaced
	ln.Close()
	stale, err := net.Listen("unix", p)
	if err != nil {
		t.Fatal(err)
	}
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	stale.Close()
	ln2, err := pprofListen("unix:" + p)
	if err != nil {
		t.Fatalf("stale socket: %v", err)
	}
	ln2.Close()
	for _, bad := range []string{"unix:rel.sock", "0.0.0.0:0", "10.0.0.1:6060", "garbage"} {
		if l, err := pprofListen(bad); err == nil {
			l.Close()
			t.Fatalf("%q accepted", bad)
		}
	}
}

func httpStatus(t *testing.T, h http.Handler, path string) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go http.Serve(ln, h)
	r, err := http.Get("http://" + ln.Addr().String() + path)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	return r.StatusCode
}

// sec370 1b: a non-root user cannot connect to the root-owned 0600 socket.
func TestPprofUnixSocketNonRoot(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() != 0 {
		t.Skip("needs root (to drop to nobody)")
	}
	if _, err := exec.LookPath("setpriv"); err != nil {
		t.Skip("no setpriv")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3")
	}
	d := t.TempDir()
	os.Chmod(filepath.Dir(d), 0o755) // t.TempDir's top dir is 0700
	os.Chmod(d, 0o755)
	p := filepath.Join(d, "pprof.sock")
	ln, err := pprofListen("unix:" + p)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go http.Serve(ln, pprofMux())
	os.Chmod(filepath.Dir(p), 0o755) // only the socket's own mode guards it here
	py := "import socket,sys\ns=socket.socket(socket.AF_UNIX)\ntry:\n s.connect(sys.argv[1])\nexcept PermissionError:\n sys.exit(13)\nsys.exit(0)\n"
	cmd := exec.Command("setpriv", "--reuid=65534", "--regid=65534", "--clear-groups", "python3", "-c", py, p)
	out, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 13 {
		t.Fatalf("nobody: want EACCES (exit 13), got %v: %s", err, out)
	}
	// control: the same probe connects once the socket is opened up
	os.Chmod(p, 0o666)
	if out, err := exec.Command("setpriv", "--reuid=65534", "--regid=65534", "--clear-groups", "python3", "-c", py, p).CombinedOutput(); err != nil {
		t.Fatalf("control: nobody could not connect to a 0666 socket: %v %s", err, out)
	}
}

// b376: the default heap dump dir is on disk and 0700 (re-tightened if wider);
// HS2_HEAPDUMP_DIR still overrides it.
func TestHeapDumpDirDefault(t *testing.T) {
	d := t.TempDir()
	defer func(s string) { heapDumpDefaultDir = s }(heapDumpDefaultDir)
	heapDumpDefaultDir = filepath.Join(d, "lib", "heap")
	t.Setenv("HS2_HEAPDUMP_DIR", "")
	if heapDumpDefaultDir == pprofRunDir {
		t.Fatal("default must not be the /run tmpfs")
	}
	got, err := heapDumpDir()
	if err != nil || got != heapDumpDefaultDir {
		t.Fatalf("%q %v", got, err)
	}
	os.Chmod(got, 0o755)
	heapDumpDir()
	if fi, _ := os.Stat(got); fi.Mode().Perm() != 0o700 {
		t.Fatalf("default dir %04o, want 0700", fi.Mode().Perm())
	}
	alt := filepath.Join(d, "alt")
	t.Setenv("HS2_HEAPDUMP_DIR", alt)
	if got, err := heapDumpDir(); err != nil || got != alt {
		t.Fatalf("override: %q %v", got, err)
	}
}

// b376: at start a dead daemon's pprof-<pid>.sock is removed; a live pid's
// socket and a regular file with that name are kept.
func TestSweepStalePprofSocks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets")
	}
	d := t.TempDir()
	c := exec.Command("true")
	if err := c.Run(); err != nil {
		t.Skip(err)
	}
	dead := c.Process.Pid
	mkSock := func(p string) {
		ln, err := net.Listen("unix", p)
		if err != nil {
			t.Fatal(err)
		}
		ln.(*net.UnixListener).SetUnlinkOnClose(false)
		ln.Close()
	}
	deadSock := filepath.Join(d, fmt.Sprintf("pprof-%d.sock", dead))
	liveSock := filepath.Join(d, fmt.Sprintf("pprof-%d.sock", os.Getppid()))
	mkSock(deadSock)
	mkSock(liveSock)
	c2 := exec.Command("true")
	c2.Run()
	file := filepath.Join(d, fmt.Sprintf("pprof-%d.sock", c2.Process.Pid))
	os.WriteFile(file, []byte("x"), 0o600)
	sweepStalePprofSocks(d)
	if _, err := os.Lstat(deadSock); err == nil {
		t.Fatal("dead pid's socket kept")
	}
	if _, err := os.Lstat(liveSock); err != nil {
		t.Fatal("live pid's socket removed")
	}
	if _, err := os.Lstat(file); err != nil {
		t.Fatal("a regular file was removed")
	}
}
