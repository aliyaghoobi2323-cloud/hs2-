package engine

import (
	"errors"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeTC writes a stand-in tc (shell script body) and points qTCBin at it.
func fakeTC(t *testing.T, body string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tc")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := qTCBin
	qTCBin = p
	t.Cleanup(func() { qTCBin = old })
}

// fx247 (audit246 M2): a hung tc ends at qExecTimeout; the tree lock is
// given up at qLockWait and the tick skipped, its state not committed.
func TestQFx247HungTCTimesOut(t *testing.T) {
	fakeTC(t, "sleep 30")
	defer func(d time.Duration) { qExecTimeout = d }(qExecTimeout)
	qExecTimeout = 300 * time.Millisecond
	t0 := time.Now()
	_, err := qTC("qdisc", "show")
	if !errors.Is(err, errQTimeout) || time.Since(t0) > 3*time.Second {
		t.Fatalf("qTC: %v after %v", err, time.Since(t0))
	}
	t0 = time.Now()
	if err := qTCBatch("qdisc show\n"); !errors.Is(err, errQTimeout) || time.Since(t0) > 3*time.Second {
		t.Fatalf("qTCBatch: %v after %v", err, time.Since(t0))
	}
	// a timed-out self-check says "still there" (unknown: no churn)
	s := &qShaper{logf: t.Logf, installed: true, qdev: "x", dev: "x", minor: 2}
	if !s.treeHere() {
		t.Fatal("a timed-out check took the tree as gone")
	}
	if qStat.execTimeouts.Load() < 3 {
		t.Fatalf("timeouts counted %d", qStat.execTimeouts.Load())
	}
}

func TestQFx247TreeLockDeadline(t *testing.T) {
	defer func(d string, w time.Duration) { qLockDir, qLockWait = d, w }(qLockDir, qLockWait)
	qLockDir, qLockWait = t.TempDir(), 200*time.Millisecond
	f, err := os.OpenFile(filepath.Join(qLockDir, "hs2-qshape.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_EX) // another (stuck) process holds it
	t0 := time.Now()
	if _, ok := qTreeLock(); ok || time.Since(t0) > 2*time.Second {
		t.Fatalf("lock had while held (%v) after %v", ok, time.Since(t0))
	}
	// set: a new peer set under a held lock: tick skipped, nothing committed
	fakeTC(t, "exit 0")
	var logs []string
	s := &qShaper{logf: func(f string, a ...any) { logs = append(logs, f) }, installed: true, mqc: true, dev: "x", qdev: "x", minor: 2, rate: 1e6, steer: true, checkAt: time.Now().Add(time.Hour)}
	p := qDsts(netip.MustParseAddrPort("10.0.0.1:443"))
	if !s.set(1e6, p, true) {
		t.Fatal("a skipped tick turned the lane off")
	}
	if s.peers != nil || len(logs) != 1 {
		t.Fatalf("peers committed %v, logs %q", s.peers, logs)
	}
	s.set(1e6, p, true) // rate-limited: no second line
	if len(logs) != 1 {
		t.Fatalf("not rate-limited: %q", logs)
	}
	f.Close()
	if !s.set(1e6, p, true) || len(s.peers) != 1 {
		t.Fatalf("after the lock is free: peers %v", s.peers)
	}
	// remove under a held lock: skipped, still installed (retried)
	f, _ = os.OpenFile(filepath.Join(qLockDir, "hs2-qshape.lock"), os.O_RDWR, 0o600)
	syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
	s.remove()
	if !s.installed {
		t.Fatal("remove went through a held lock")
	}
	f.Close()
}

// fx247 (audit246 M1 scenario C): a failed class change or filter batch is
// not taken as done: retried, and the tree checked next tick.
func TestQFx247FailedTCNotCommitted(t *testing.T) {
	defer func(d string) { qLockDir = d }(qLockDir)
	qLockDir = t.TempDir()
	fakeTC(t, `echo "RTNETLINK answers: No such file or directory" >&2; exit 2`)
	s := &qShaper{logf: t.Logf, installed: true, dev: "x", qdev: "x", minor: 2, rate: 1e6, checkAt: time.Now().Add(time.Hour)}
	p := qDsts(netip.MustParseAddrPort("10.0.0.1:443"))
	s.peers = p
	if !s.set(2e6, p, true) {
		t.Fatal("lane off")
	}
	if s.rate != 1e6 || !s.suspect {
		t.Fatalf("failed class change committed: rate %v suspect %v", s.rate, s.suspect)
	}
	s.suspect = false
	s.peers = nil
	s.set(1e6, p, true)
	if s.peers != nil || !s.suspect {
		t.Fatalf("failed filter batch committed: peers %v suspect %v", s.peers, s.suspect)
	}
	// suspect: the next tick checks the tree; gone -> forgotten, put back
	// (install: the fake tc fails the root show -> refused here)
	s.set(1e6, p, true)
	if s.installed || !s.lost && !s.refused {
		t.Fatalf("a gone tree not noticed: installed %v lost %v", s.installed, s.lost)
	}
}

// fx247 (audit246 M1) on a kernel: a root-mode install leaves a live mqchild
// tree in place (joins it as an mq child); a root-mode intruder (an older
// hs2: qdisc replace root htb) wiping the tree under a cap is noticed and
// the class put back without a root graft (into the intruder's htb); a tree
// whose root is gone entirely waits for a light tick to graft.
func TestQShaperFx247IntruderOnKernel(t *testing.T) {
	if os.Getenv(qNetnsEnv) != "1" {
		if os.Geteuid() != 0 {
			t.Skip("needs root (a private network namespace)")
		}
		unshare, err := exec.LookPath("unshare")
		if err != nil {
			t.Skip("no unshare")
		}
		cmd := exec.Command(unshare, "-n", "--", "sh", "-c",
			`ip link add v0 numtxqueues 4 numrxqueues 4 type veth peer name v1 numtxqueues 4 numrxqueues 4 && ip addr add 10.9.0.1/24 dev v0 && ip link set v1 up && ip link set v0 up && ip neigh replace 10.9.0.2 lladdr 02:00:00:00:00:02 dev v0 && tc qdisc replace dev v0 root handle 1: mq && exec "$0" -test.run '^TestQShaperFx247IntruderOnKernel$' -test.v`, os.Args[0])
		cmd.Env = append(os.Environ(), qNetnsEnv+"=1")
		out, err := cmd.CombinedOutput()
		t.Logf("in a private netns:\n%s", out)
		if err != nil {
			t.Fatalf("the test in its own netns failed: %v", err)
		}
		return
	}
	if _, err := exec.LookPath("tc"); err != nil {
		t.Skip("no tc")
	}
	if !qKernelAtLeast(5, 18) {
		t.Skip("kernel < 5.18")
	}
	defer func(k bool, m string, c time.Duration) { queueCtlKeep, qShaperMode, qCheckEvery = k, m, c }(queueCtlKeep, qShaperMode, qCheckEvery)
	queueCtlKeep, qShaperMode, qCheckEvery = true, "mqchild", 0
	qLockDir = t.TempDir()
	tcs := func(a ...string) string {
		out, _ := exec.Command("tc", a...).CombinedOutput()
		return string(out)
	}
	root0 := tcs("qdisc", "show", "dev", "v0", "root")
	var logs []string
	lf := func(f string, a ...any) { logs = append(logs, f) }
	s := newQShaper(lf)
	peer := netip.MustParseAddrPort("10.9.0.2:443")
	capRate := 12.5e6 / qWire
	if !s.park(qDsts(peer)) || !s.apply(capRate, qDsts(peer)) || !s.mqc {
		t.Fatalf("mqchild cap: %q", logs)
	}
	sent := func(port int) uint64 {
		u, err := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(netip.AddrPortFrom(peer.Addr(), uint16(port))))
		if err != nil {
			t.Fatal(err)
		}
		before := qClassSent(tcs("-s", "class", "show", "dev", "v0"), s.minor)
		for i := 0; i < 20; i++ {
			u.Write(make([]byte, 100))
		}
		u.Close()
		return qClassSent(tcs("-s", "class", "show", "dev", "v0"), s.minor) - before
	}
	if n := sent(443); n < 20 {
		t.Fatalf("not shaped: %d", n)
	}

	// 1. a root-mode shaper (HS2_QUEUE_CTL_SHAPER=root, or an mqReady
	// fallback) on the same dev: the live mq tree is not replaced
	r := &qShaper{logf: lf}
	if !r.apply(3e6/qWire, qDsts(netip.MustParseAddrPort("10.9.0.2:8443"))) {
		t.Fatalf("root-mode second: %q", logs)
	}
	if got := tcs("qdisc", "show", "dev", "v0", "root"); got != root0 || !r.mqc || qStat.keptLive.Load() == 0 {
		t.Fatalf("root replaced or not joined: %q -> %q (mqc %v)", root0, got, r.mqc)
	}
	if n := sent(443); n < 20 {
		t.Fatalf("first's cap gone after the second: %d", n)
	}
	r.remove()

	// 2a. an older root-mode hs2 (no fx247) replaces the root with handle
	// 1fe1: under our cap: the kernel grafts the existing htb 1fe1: (ours,
	// on the mq child) as the root: our class and cap stay (lab1 6.x)
	tcs("qdisc", "replace", "dev", "v0", "root", "handle", "1fe1:", "htb", "default", "1")
	t.Logf("after a root-mode replace: %s", tcs("qdisc", "show", "dev", "v0"))
	reb0 := qStat.reinstalls.Load()
	if !s.apply(capRate, qDsts(peer)) || !s.installed {
		t.Fatalf("after a root replace: %q", logs)
	}
	if n := sent(443); n < 20 {
		t.Fatalf("cap lost after a root replace (reinstalls %d): %d", qStat.reinstalls.Load()-reb0, n)
	}

	// 2b. the tree wiped (qdisc del root), then an older root-mode hs2 puts
	// its own root htb in and holds a class of its own (its slot lock)
	slot := qTrySlot("v0", 9)
	if slot == nil {
		t.Fatal("slot 9")
	}
	defer slot.Close()
	tcs("qdisc", "del", "dev", "v0", "root")
	tcs("qdisc", "replace", "dev", "v0", "root", "handle", "1fe1:", "htb", "default", "1")
	tcs("class", "replace", "dev", "v0", "parent", "1fe1:", "classid", "1fe1:1", "htb", "rate", "40gbit")
	tcs("class", "replace", "dev", "v0", "parent", "1fe1:", "classid", "1fe1:9", "htb", "rate", "5mbit")
	reb := qStat.reinstalls.Load()
	if !s.apply(capRate, qDsts(peer)) || !s.installed {
		t.Fatalf("after the intruder: installed %v %q", s.installed, logs)
	}
	if qStat.reinstalls.Load() != reb+1 {
		t.Fatalf("not reinstalled: %q", logs)
	}
	if c := tcs("class", "show", "dev", "v0"); !strings.Contains(c, "class htb "+qClassID(s.minor)+" ") || !strings.Contains(c, "class htb 1fe1:9 ") {
		t.Fatalf("our class not back or the intruder's gone: %s", c)
	}
	if !strings.HasPrefix(tcs("qdisc", "show", "dev", "v0", "root"), "qdisc htb 1fe1:") {
		t.Fatal("the intruder's root swapped under a cap")
	}
	if n := sent(443); n < 20 {
		t.Fatalf("cap not restored: %d", n)
	}
	if n := sent(444); n != 0 {
		t.Fatalf("other traffic in our class: %d", n)
	}
	slot.Close()

	// 3. the root goes entirely (qdisc del: veth's noqueue) under a cap: no
	// root graft under load; put back on the first light tick
	tcs("qdisc", "del", "dev", "v0", "root")
	root1 := tcs("qdisc", "show", "dev", "v0", "root")
	def := qStat.deferred.Load()
	if !s.apply(capRate, qDsts(peer)) {
		t.Fatalf("lane off: %q", logs)
	}
	if s.installed || qStat.deferred.Load() == def || tcs("qdisc", "show", "dev", "v0", "root") != root1 {
		t.Fatalf("root grafted under a cap: installed %v root %q", s.installed, tcs("qdisc", "show", "dev", "v0", "root"))
	}
	if !s.park(qDsts(peer)) || !s.installed {
		t.Fatalf("not put back on a light tick: %q", logs)
	}
	if c := tcs("class", "show", "dev", "v0"); !strings.Contains(c, "class htb "+qClassID(s.minor)+" ") {
		t.Fatalf("class not back: %s", c)
	}
	s.remove()
	t.Logf("logs: %q", logs)
}
