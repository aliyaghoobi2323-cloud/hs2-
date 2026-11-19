package main

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/engine"
)

// pprofRunDir: where the unix socket goes by default (root-only).
var pprofRunDir = "/run/hs2"

// heapDumpDefaultDir (b376): heap dumps go to disk, not the /run tmpfs (20%
// of RAM; a big heap could fill it). HS2_HEAPDUMP_DIR overrides it.
var heapDumpDefaultDir = "/var/lib/hs2/heap"

// heapDumpDir: HS2_HEAPDUMP_DIR, else heapDumpDefaultDir; created 0700 (the
// default dir is also re-tightened to 0700 if it was wider).
func heapDumpDir() (string, error) {
	dir := strings.TrimSpace(os.Getenv("HS2_HEAPDUMP_DIR"))
	if dir != "" {
		return dir, os.MkdirAll(dir, 0o700)
	}
	dir = heapDumpDefaultDir
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("%s is not a directory", dir)
	} else if fi.Mode().Perm() != 0o700 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// startPprof serves Go's CPU and memory profiles when HS2_PPROF is set, for
// measuring where a busy server spends its CPU. Off unless set.
//
//	HS2_PPROF=unix                 unix socket /run/hs2/pprof-<pid>.sock, mode 0600 (root only)
//	HS2_PPROF=unix:/path/x.sock    the same at that path
//	HS2_PPROF=127.0.0.1:6060       TCP on a loopback address (the lab form)
//
//	curl --unix-socket /run/hs2/pprof-PID.sock -o cpu.prof 'http://x/debug/pprof/profile?seconds=30'
//	curl -o cpu.prof 'http://127.0.0.1:6060/debug/pprof/profile?seconds=30'
//
// Anything else is refused (the profiles show the process's internals; they
// are never exposed to the network). sec370 (sec353 1b, H5): a loopback port
// is reachable by every local user, the socket only by root, so prefer the
// socket on production boxes. /debug/pprof/cmdline is left out
// (HS2_PPROF_CMDLINE=1 puts it back) and heap dumps go to /var/lib/hs2/heap
// (HS2_HEAPDUMP_DIR), 0600 and O_EXCL, instead of a predictable
// world-readable /tmp path.
func startPprof() {
	addr := strings.TrimSpace(os.Getenv("HS2_PPROF"))
	if addr == "" {
		return
	}
	ln, err := pprofListen(addr)
	if err != nil {
		log.Printf("pprof: %v", err)
		return
	}
	// HS2_PPROF_LOCKS=N also samples lock contention (1 in N mutex events)
	// and blocking (events of N µs or more): /debug/pprof/mutex and /block.
	if n, err := strconv.Atoi(os.Getenv("HS2_PPROF_LOCKS")); err == nil && n > 0 {
		runtime.SetMutexProfileFraction(n)
		runtime.SetBlockProfileRate(n * 1000)
	}
	log.Printf("pprof: serving profiles on %s/debug/pprof/ (HS2_PPROF)", pprofWhere(ln))
	go http.Serve(ln, pprofMux())
}

func pprofWhere(ln net.Listener) string {
	if ln.Addr().Network() == "unix" {
		return "unix:" + ln.Addr().String() + " "
	}
	return "http://" + ln.Addr().String()
}

// pprofListen: a 0600 unix socket for "unix" / "unix:PATH", a TCP listener
// for a loopback host:port, an error for anything else.
func pprofListen(addr string) (net.Listener, error) {
	if addr == "unix" || strings.HasPrefix(addr, "unix:") {
		p := strings.TrimPrefix(strings.TrimPrefix(addr, "unix"), ":")
		if p == "" {
			sweepStalePprofSocks(pprofRunDir)
			p = filepath.Join(pprofRunDir, fmt.Sprintf("pprof-%d.sock", os.Getpid()))
		}
		if !filepath.IsAbs(p) {
			return nil, fmt.Errorf("HS2_PPROF=%q refused — the socket path must be absolute", addr)
		}
		return listenUnix0600(p)
	}
	host, _, err := net.SplitHostPort(addr)
	if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("HS2_PPROF=%q refused — use unix (a root-only socket) or a loopback address such as 127.0.0.1:6060", addr)
	}
	return net.Listen("tcp", addr)
}

// listenUnix0600 binds a unix socket at p that only its owner can connect to.
// It binds inside a fresh 0700 directory next to p, chmods the socket 0600 and
// only then renames it into place, so there is no moment where another user
// could connect. A stale socket at p (an old pid's) is replaced; any other
// kind of file there is not touched.
func listenUnix0600(p string) (net.Listener, error) {
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if fi, err := os.Lstat(p); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket — not replaced", p)
		}
		os.Remove(p)
	}
	tmpd, err := os.MkdirTemp(dir, ".pprof-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpd)
	tp := filepath.Join(tmpd, "s")
	ln, err := net.Listen("unix", tp)
	if err != nil {
		return nil, err
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := os.Chmod(tp, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	if err := os.Rename(tp, p); err != nil {
		ln.Close()
		return nil, err
	}
	return &renamedUnix{Listener: ln, path: p}, nil
}

// sweepStalePprofSocks (b376): removes dir's pprof-<pid>.sock sockets whose
// pid is not alive (left by a daemon that was killed); a live pid's socket
// and anything that is not a socket are kept.
func sweepStalePprofSocks(dir string) {
	ms, _ := filepath.Glob(filepath.Join(dir, "pprof-*.sock"))
	for _, m := range ms {
		n := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(m), "pprof-"), ".sock")
		pid, err := strconv.Atoi(n)
		if err != nil || pid <= 0 || pid == os.Getpid() || pidAlive(pid) {
			continue
		}
		if fi, err := os.Lstat(m); err == nil && fi.Mode()&os.ModeSocket != 0 {
			os.Remove(m)
		}
	}
}

// renamedUnix reports (and on Close removes) the socket's final path.
type renamedUnix struct {
	net.Listener
	path string
}

func (r *renamedUnix) Addr() net.Addr { return &net.UnixAddr{Name: r.path, Net: "unix"} }
func (r *renamedUnix) Close() error {
	err := r.Listener.Close()
	os.Remove(r.path)
	return err
}

func pprofMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	if os.Getenv("HS2_PPROF_CMDLINE") == "1" {
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	} else {
		mux.HandleFunc("/debug/pprof/cmdline", http.NotFound)
	}
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	// mem69 (HS2_MEM_OBJS diagnosis): heap dump with object labels. sec370:
	// written to HS2_HEAPDUMP_DIR (default /var/lib/hs2/heap, 0700) as
	// hs2heap-<pid>-<unixnano>.dump/.labels, 0600 and never over an existing
	// file. The dump holds key material: delete it after use.
	mux.HandleFunc("/debug/hs2heapdump", func(w http.ResponseWriter, _ *http.Request) {
		dir, err := heapDumpDir()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		p := filepath.Join(dir, fmt.Sprintf("hs2heap-%d-%d", os.Getpid(), time.Now().UnixNano()))
		if err := engine.MemHeapDump(p); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		fmt.Fprintln(w, p)
	})
	// hc183: half-close counters (halfCloses, halfCloseIdles, halfOpen now)
	mux.HandleFunc("/debug/hs2halfclose", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, engine.HalfCloseStats())
	})
	mux.HandleFunc("/debug/hs2sched", serveSched) // b322: runtime/metrics for storm diag
	return mux
}
