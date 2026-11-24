//go:build linux

package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/engine"
	"github.com/hosseintaghipoursori-alt/hs2-tunnel/handoff"
)

// fakeChild is a child whose "process" is a goroutine on the channel end.
type fakeChild struct {
	pid  int
	done chan struct{}
	once sync.Once
}

func (c *fakeChild) Pid() int              { return c.pid }
func (c *fakeChild) Done() <-chan struct{} { return c.done }
func (c *fakeChild) Kill()                 { c.once.Do(func() { close(c.done) }) }

type zduRig struct {
	o        *zduOld
	dir      string
	notes    []string
	logs     []string
	mu       sync.Mutex
	paused   int
	resumed  int
	exited   chan struct{}
	child    *fakeChild
	behave   func(c *handoff.Conn, ch *fakeChild) // the new process
	statsU   int
	spawnErr error
	readsOff int // S7: TUN read-off calls
}

func (r *zduRig) note(s string) (bool, error) {
	r.mu.Lock()
	r.notes = append(r.notes, s)
	r.mu.Unlock()
	return true, nil
}

func (r *zduRig) logText() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.logs, "\n")
}

func newRig(t *testing.T, now time.Time) *zduRig {
	t.Helper()
	dir := t.TempDir()
	os.Chmod(dir, 0o700)
	zduRootUID = uint32(os.Getuid())
	t.Cleanup(func() { zduRootUID = 0 })
	r := &zduRig{dir: dir, exited: make(chan struct{})}
	bin := filepath.Join(dir, "hs2-new")
	os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755)
	os.WriteFile(filepath.Join(dir, "u.next"), []byte(bin+"\nHS2_X=1\n"), 0o644)
	r.o = &zduOld{
		unit: "u", runDir: dir, now: func() time.Time { return now },
		probation: 300 * time.Millisecond, floor: zduDrainFloor, period: 50 * time.Millisecond, ready: 2 * time.Second,
		spawn: func(b string, env []string, ch *os.File) (zduChild, error) {
			if r.spawnErr != nil {
				return nil, r.spawnErr
			}
			if env[len(env)-1] != "HS2_HANDOVER_FD=3" {
				t.Errorf("env tail %q", env[len(env)-1])
			}
			fd, _ := syscall.Dup(int(ch.Fd()))
			c, err := handoff.FromFD(fd)
			if err != nil {
				t.Fatal(err)
			}
			r.child = &fakeChild{pid: 4242, done: make(chan struct{})}
			go r.behave(c, r.child)
			return r.child, nil
		},
		notify: r.note,
		fds: func() ([]handoff.FD, error) {
			pr, pw, err := os.Pipe()
			if err != nil {
				return nil, err
			}
			pw.Close()
			return []handoff.FD{{Name: "tun:fake", File: pr}}, nil
		},
		pause:  func() int { r.mu.Lock(); r.paused++; r.mu.Unlock(); return 1 },
		resume: func() int { r.mu.Lock(); r.resumed++; r.mu.Unlock(); return 1 },
		stats: func() (int, int) {
			r.mu.Lock()
			defer r.mu.Unlock()
			return 3, r.statsU
		},
		readsOff: func() string { r.mu.Lock(); r.readsOff++; r.mu.Unlock(); return "hs0" },
		exit:     func() { close(r.exited) },
		hello:    func() handoff.Hello { return handoff.Hello{Proto: handoff.Proto} },
		logf: func(f string, a ...any) {
			r.mu.Lock()
			r.logs = append(r.logs, fmt.Sprintf(f, a...))
			r.mu.Unlock()
		},
		override:  time.Hour,
		drainDone: make(chan struct{}),
		peers:     func() (bool, string) { return true, "ZDU-OK links=3 zdu=3" },
	}
	// Review 4: the drain now writes DRAIN-END/DRAIN-ABORT; end a drain the
	// test left running before TempDir's RemoveAll (cleanups run LIFO).
	t.Cleanup(func() {
		r.mu.Lock()
		r.statsU = 0
		r.mu.Unlock()
		for i := 0; i < 60; i++ {
			s := r.status(t)
			if !strings.HasPrefix(s, "OK") && !strings.HasPrefix(s, "PROBATION") {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		time.Sleep(20 * time.Millisecond)
	})
	return r
}

func (r *zduRig) status(t *testing.T) string {
	b, _ := os.ReadFile(filepath.Join(r.dir, "u.handover"))
	return strings.TrimSpace(string(b))
}

var okTime = time.Date(2026, 11, 19, 10, 20, 0, 0, time.UTC)

func goodChild(c *handoff.Conn, ch *fakeChild) {
	t, err := c.Take(handoff.Hello{Proto: handoff.Proto}, time.Second)
	if err != nil {
		ch.Kill()
		return
	}
	if t.Ready(time.Second) != nil {
		ch.Kill()
		return
	}
	go t.Conn.FollowDrain(func(int) {}, 5*time.Second)
}

func TestZDUTakeoverOK(t *testing.T) {
	r := newRig(t, okTime)
	r.behave = goodChild
	r.statsU = 2
	if got := r.o.handover(); got != "ok" {
		t.Fatalf("result %q\n%s", got, r.logText())
	}
	if r.paused != 1 || r.resumed != 0 {
		t.Fatalf("paused %d resumed %d", r.paused, r.resumed)
	}
	if r.readsOff != 1 || !strings.Contains(r.logText(), "stopped reading TUN hs0") {
		t.Fatalf("S7: TUN reads not stopped at commit (%d)\n%s", r.readsOff, r.logText())
	}
	if !strings.HasPrefix(r.notes[0], "MAINPID=4242\nREADY=1") {
		t.Fatalf("notify %q", r.notes)
	}
	if s := r.status(t); s != "OK 4242" {
		t.Fatalf("status %q", s)
	}
	select {
	case <-r.exited:
		t.Fatal("exited with users left")
	case <-time.After(150 * time.Millisecond):
	}
	r.mu.Lock()
	r.statsU = 0
	r.mu.Unlock()
	select {
	case <-r.exited:
	case <-time.After(2 * time.Second):
		t.Fatal("no exit at users 0")
	}
	if !strings.Contains(r.logText(), "drain end: no user connections left") {
		t.Fatal(r.logText())
	}
	// A second SIGUSR2 after commit is refused.
	if got := r.o.handover(); got != "refused-busy" {
		t.Fatalf("second %q", got)
	}
}

func TestZDUTakeoverDeadlineExit(t *testing.T) {
	r := newRig(t, okTime)
	r.behave = goodChild
	r.statsU = 5
	r.o.override = time.Millisecond // deadline already passed at the first drain tick
	r.o.floor = 2 * time.Second     // the fake clock moves 1 s per read
	cur := okTime
	var mu sync.Mutex
	r.o.now = func() time.Time { mu.Lock(); defer mu.Unlock(); cur = cur.Add(time.Second); return cur }
	// b447: users left at the deadline: the links close paced, before the exit
	var paced atomic.Int32
	r.o.pace = func() (int, int, int) {
		select {
		case <-r.exited:
			t.Error("paced close after the exit")
		default:
		}
		paced.Add(1)
		return 3, 5, 2
	}
	if got := r.o.handover(); got != "ok" {
		t.Fatalf("result %q\n%s", got, r.logText())
	}
	select {
	case <-r.exited:
	case <-time.After(2 * time.Second):
		t.Fatal("no exit at the deadline")
	}
	if !strings.Contains(r.logText(), "deadline") {
		t.Fatal(r.logText())
	}
	if paced.Load() != 1 || !strings.Contains(r.logText(), "drain end: closed 3 links (5 users) in 2 batches") {
		t.Fatalf("paced %d\n%s", paced.Load(), r.logText())
	}
}

// b447: a drain that ends with users 0 has nothing to pace.
func TestZDUTakeoverNoPaceWithoutUsers(t *testing.T) {
	r := newRig(t, okTime)
	r.behave = goodChild
	r.statsU = 0
	r.o.pace = func() (int, int, int) { t.Error("paced with users 0"); return 0, 0, 0 }
	if got := r.o.handover(); got != "ok" {
		t.Fatalf("result %q\n%s", got, r.logText())
	}
	select {
	case <-r.exited:
	case <-time.After(2 * time.Second):
		t.Fatal("no exit at users 0")
	}
}

// TestZDUDrainEnd: drill b1 (HS2_ZDU_DRAIN=40 s, probation 45 s) must not
// end the drain at OK; the end is raised to OK + 60 s, a long one is kept.
func TestZDUDrainEnd(t *testing.T) {
	st := okTime
	ok := st.Add(45 * time.Second)
	end, c := zduDrainEnd(st.Add(40*time.Second), st, ok, 45*time.Second, zduDrainFloor)
	if !c || !end.Equal(st.Add(105*time.Second)) {
		t.Fatalf("b1: end %v clamped %v", end, c)
	}
	// a late OK (slow READY) moves the floor with it
	end, c = zduDrainEnd(st.Add(2*time.Minute), st, st.Add(100*time.Second), 45*time.Second, zduDrainFloor)
	if !c || !end.Equal(st.Add(160*time.Second)) {
		t.Fatalf("late OK: end %v clamped %v", end, c)
	}
	// exactly at the floor, and the HH:03 rule (>= 5 min): untouched
	for _, d := range []time.Duration{105 * time.Second, 200 * time.Second, 43 * time.Minute} {
		if end, c := zduDrainEnd(st.Add(d), st, ok, 45*time.Second, zduDrainFloor); c || !end.Equal(st.Add(d)) {
			t.Fatalf("%v: end %v clamped %v", d, end, c)
		}
	}
	// HH:03 deadlines from ZDUDeadline never need the clamp (ZDUDrainMin > probation + floor)
	if engine.ZDUDrainMin < zduProbation+zduDrainFloor {
		t.Fatalf("ZDUDrainMin %v < probation + floor", engine.ZDUDrainMin)
	}
}

// TestZDUTakeoverDrainClamped: a 40 s override with the clock standing still
// at OK: the old process logs the clamp and keeps draining (no exit at OK).
func TestZDUTakeoverDrainClamped(t *testing.T) {
	r := newRig(t, okTime)
	r.behave = goodChild
	r.statsU = 5
	r.o.override = 40 * time.Second
	r.o.probation = 300 * time.Millisecond
	cur := okTime
	var mu sync.Mutex
	r.o.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return cur }
	if got := r.o.handover(); got != "ok" {
		t.Fatalf("result %q\n%s", got, r.logText())
	}
	lt := r.logText()
	if !strings.Contains(lt, "it will be raised at OK") || !strings.Contains(lt, "drain deadline clamped from 2026-11-19T10:20:40Z to 2026-11-19T10:21:00Z") {
		t.Fatal(lt)
	}
	// 40 s after SIGUSR2: the unclamped deadline has passed, the clamped one not
	mu.Lock()
	cur = okTime.Add(50 * time.Second)
	mu.Unlock()
	select {
	case <-r.exited:
		t.Fatalf("exited before OK + floor\n%s", r.logText())
	case <-time.After(300 * time.Millisecond):
	}
	mu.Lock()
	cur = okTime.Add(61 * time.Second)
	mu.Unlock()
	select {
	case <-r.exited:
	case <-time.After(2 * time.Second):
		t.Fatal("no exit at the clamped deadline")
	}
	if !strings.Contains(r.logText(), "deadline 2026-11-19T10:21:00Z reached") {
		t.Fatal(r.logText())
	}
}

func TestZDUTakeoverProbationCrashReverts(t *testing.T) {
	r := newRig(t, okTime)
	r.behave = func(c *handoff.Conn, ch *fakeChild) {
		goodChild(c, ch)
		time.Sleep(100 * time.Millisecond)
		ch.Kill() // dies in probation
		c.Close()
	}
	if got := r.o.handover(); got != "reverted" {
		t.Fatalf("result %q\n%s", got, r.logText())
	}
	if r.paused != 1 || r.resumed != 1 {
		t.Fatalf("paused %d resumed %d", r.paused, r.resumed)
	}
	if r.readsOff != 0 {
		t.Fatal("S7: TUN reads stopped by a reverted handover")
	}
	self := fmt.Sprintf("MAINPID=%d\nREADY=1", os.Getpid())
	if len(r.notes) != 2 || !strings.HasPrefix(r.notes[1], self) {
		t.Fatalf("notes %q", r.notes)
	}
	if s := r.status(t); !strings.HasPrefix(s, "REVERTED") {
		t.Fatalf("status %q", s)
	}
	select {
	case <-r.exited:
		t.Fatal("old exited after a revert")
	default:
	}
	// A new handover may start again.
	r.behave = goodChild
	r.o.drainDone = nil
	if got := r.o.handover(); got != "ok" {
		t.Fatalf("retry %q", got)
	}
}

func TestZDUTakeoverChannelBreakReverts(t *testing.T) {
	r := newRig(t, okTime)
	r.behave = func(c *handoff.Conn, ch *fakeChild) {
		goodChild(c, ch)
		time.Sleep(80 * time.Millisecond)
		c.Close() // health: channel gone, process "alive"
	}
	if got := r.o.handover(); got != "reverted" {
		t.Fatalf("result %q\n%s", got, r.logText())
	}
	if r.resumed != 1 {
		t.Fatal("not resumed")
	}
	<-r.child.Done() // the child was killed
}

func TestZDUTakeoverRefusedHandoffCold(t *testing.T) {
	r := newRig(t, okTime)
	r.behave = func(c *handoff.Conn, ch *fakeChild) {
		// a new process with another protocol: Take aborts -> its cold path
		_, err := c.Take(handoff.Hello{Proto: handoff.Proto + 1}, time.Second)
		if err == nil {
			panic("take ok")
		}
		ch.Kill()
	}
	if got := r.o.handover(); got != "failed" {
		t.Fatalf("result %q", got)
	}
	if r.paused != 0 || len(r.notes) != 0 {
		t.Fatalf("paused %d notes %q", r.paused, r.notes)
	}
	if s := r.status(t); !strings.HasPrefix(s, "REVERTED give") {
		t.Fatalf("status %q", s)
	}
	r.spawnErr = os.ErrNotExist
	if got := r.o.handover(); got != "failed" {
		t.Fatalf("spawn fail %q", got)
	}
	// The new side's TakeOrCold runs cold on a refused hand-off.
	a, b, _ := handoff.Pair()
	go func() { a.Recv(time.Second); a.Send(&handoff.Msg{Type: handoff.TypeAbort, Reason: "no"}, time.Second) }()
	cold := false
	tk, terr, _ := handoff.TakeOrCold(func() (*handoff.Taken, error) {
		return b.Take(handoff.Hello{Proto: handoff.Proto}, time.Second)
	}, func() error { cold = true; return nil })
	if tk != nil || terr == nil || !cold {
		t.Fatalf("cold %v err %v", cold, terr)
	}
}

func TestZDUTakeoverChildDiesBeforeReady(t *testing.T) {
	r := newRig(t, okTime)
	r.o.ready = 5 * time.Second
	r.behave = func(c *handoff.Conn, ch *fakeChild) {
		c.Take(handoff.Hello{Proto: handoff.Proto}, time.Second)
		ch.Kill() // crash before READY
	}
	t0 := time.Now()
	if got := r.o.handover(); got != "failed" {
		t.Fatalf("result %q", got)
	}
	if d := time.Since(t0); d > 2*time.Second {
		t.Fatalf("waited %v for a dead child", d)
	}
}

func TestZDUTakeoverWindowRefused(t *testing.T) {
	for _, ts := range []string{"10:00:00", "10:01:30", "10:03:59"} {
		at, _ := time.Parse("2006-01-02 15:04:05", "2026-11-19 "+ts)
		r := newRig(t, at)
		r.behave = func(*handoff.Conn, *fakeChild) { t.Error("spawned in the window") }
		if got := r.o.handover(); got != "refused-window" {
			t.Fatalf("%s: %q", ts, got)
		}
	}
	r := newRig(t, time.Date(2026, 11, 19, 10, 4, 0, 0, time.UTC))
	r.behave = goodChild
	if got := r.o.handover(); got != "ok" {
		t.Fatalf("10:04: %q", got)
	}
}

func TestZDUTakeoverBadNext(t *testing.T) {
	r := newRig(t, okTime)
	r.behave = func(*handoff.Conn, *fakeChild) { t.Error("spawned with a bad UNIT.next") }
	next := filepath.Join(r.dir, "u.next")
	bin := filepath.Join(r.dir, "hs2-new")
	cases := []struct {
		name string
		prep func()
	}{
		{"missing", func() { os.Remove(next) }},
		{"group-writable", func() { os.WriteFile(next, []byte(bin+"\n"), 0o664); os.Chmod(next, 0o664) }},
		{"symlink", func() {
			os.WriteFile(next+".real", []byte(bin+"\n"), 0o644)
			os.Symlink(next+".real", next)
		}},
		{"relative", func() { os.WriteFile(next, []byte("hs2-new\n"), 0o644) }},
		{"unclean", func() { os.WriteFile(next, []byte(r.dir+"/../x/hs2\n"), 0o644) }},
		{"bin-writable", func() { os.WriteFile(next, []byte(bin+"\n"), 0o644); os.Chmod(bin, 0o777) }},
		{"bin-noexec", func() { os.WriteFile(next, []byte(bin+"\n"), 0o644); os.Chmod(bin, 0o644) }},
		{"bin-missing", func() { os.WriteFile(next, []byte(bin+"-nope\n"), 0o644) }},
		{"bad-env", func() { os.WriteFile(next, []byte(bin+"\nnot env\n"), 0o644) }},
		{"handover-fd-env", func() { os.WriteFile(next, []byte(bin+"\nHS2_HANDOVER_FD=9\n"), 0o644) }},
		{"too-big", func() { os.WriteFile(next, []byte(bin+"\nA="+strings.Repeat("x", 5000)+"\n"), 0o644) }},
		{"other-owner", func() { os.WriteFile(next, []byte(bin+"\n"), 0o644); zduRootUID = 12345 }},
	}
	for _, c := range cases {
		os.Remove(next)
		os.Remove(next + ".real")
		os.Chmod(bin, 0o755)
		zduRootUID = uint32(os.Getuid())
		c.prep()
		if got := r.o.handover(); got != "refused-next" {
			t.Fatalf("%s: %q", c.name, got)
		}
	}
	zduRootUID = uint32(os.Getuid())
	os.Remove(next)
	os.WriteFile(next, []byte("# comment\n"+bin+"\nHS2_ZDU_DRAIN=2m\n"), 0o600)
	n, err := zduReadNext(next)
	if err != nil || n.Bin != bin || len(n.Env) != 1 {
		t.Fatalf("good next: %+v %v", n, err)
	}
}

func TestZDUMergeEnv(t *testing.T) {
	got := zduMergeEnv([]string{"A=1", "HS2_HANDOVER_FD=7", "B=2"}, []string{"B=3", "C=4"})
	want := "A=1 B=3 C=4 HS2_HANDOVER_FD=3"
	if strings.Join(got, " ") != want {
		t.Fatalf("%q", got)
	}
}

// Key off: zduStart installs nothing, so SIGUSR2 keeps Go's default for an
// un-notified SIGUSR2 (ignored: the process keeps running, exactly as
// rel69p). Checked in a child process: it survives and logs nothing.
func TestZDUKeyOffSIGUSR2Default(t *testing.T) {
	if os.Getenv("ZDU_SIGCHILD") == "1" {
		zduStart(t.Context(), func(f string, a ...any) { fmt.Println(fmt.Sprintf(f, a...)) }, func() {})
		syscall.Kill(os.Getpid(), syscall.SIGUSR2)
		time.Sleep(2 * time.Second)
		os.Exit(0) // still alive: SIGUSR2 was ignored (Go default)
	}
	if engine.ZDUEnabled() {
		t.Skip("HS2_ZDU=1 in the environment")
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestZDUKeyOffSIGUSR2Default$", "-test.v")
	cmd.Env = append(os.Environ(), "ZDU_SIGCHILD=1", "HS2_ZDU=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("key off: SIGUSR2 changed the process: %v: %s", err, out)
	}
	if strings.Contains(string(out), "zdu:") {
		t.Fatalf("key off logged zdu lines: %s", out)
	}
	if _, err := os.Stat(filepath.Join(zduRunDir, "hs2.sock")); err == nil && os.Getuid() != 0 {
		t.Fatal("socket created")
	}
}

// The engine half: pause stops accept on the shared socket, the adopted
// copy takes the queue, resume brings the old accept back (the revert).
func TestZDUListenerPauseAdoptResume(t *testing.T) {
	if !engine.ZDUEnabled() {
		t.Skip("needs HS2_ZDU=1 (engine reads the key at init)")
	}
	ln, err := engine.ListenReuse("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr := ln.Addr().String()
	names, files, err := engine.ZDUListenerFiles()
	if err != nil {
		t.Fatal(err)
	}
	var f *os.File
	for i, n := range names {
		if n == engine.ZDULnName("127.0.0.1:0") {
			f = files[i]
		} else {
			files[i].Close()
		}
	}
	if f == nil {
		t.Fatalf("not registered: %v", names)
	}
	acc := func(l net.Listener, tag string, out chan<- string) {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			out <- tag
			c.Close()
		}
	}
	got := make(chan string, 100)
	go acc(ln, "old", got)
	dial := func() {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		c.Close()
	}
	dial()
	if s := <-got; s != "old" {
		t.Fatal(s)
	}
	late0 := engine.ZDULateAccepts.Load()
	engine.ZDUPauseAccept()
	// "new process": a listener on the received fd.
	nl, err := net.FileListener(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	go acc(nl, "new", got)
	// Gate fix: the old acc goroutine may already be inside accept(2) past
	// its deadline check when the pause lands (package net cannot make the
	// two atomic); it then takes one dial, serves it (the old process drains
	// what it took) and counts a late accept. Exactly that case is tolerated:
	// at most one old accept, and only when ZDULateAccepts says it was late.
	olds := 0
	for i := 0; i < 20; i++ {
		dial()
		if s := <-got; s != "new" {
			olds++
			if olds > 1 || engine.ZDULateAccepts.Load()-late0 != 1 {
				t.Fatalf("paused old accepted (%d): olds %d late %d", i, olds, engine.ZDULateAccepts.Load()-late0)
			}
		}
	}
	nl.Close() // the new process dies
	engine.ZDUResumeAccept()
	for i := 0; i < 5; i++ {
		dial()
		select {
		case s := <-got:
			if s != "old" {
				t.Fatal(s)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("old accept did not resume")
		}
	}
}

func TestZDUWarmWait(t *testing.T) {
	defer func(a, b, c time.Duration) { zduWarmMax, zduWarmStable, zduWarmNoPool = a, b, c }(zduWarmMax, zduWarmStable, zduWarmNoPool)
	zduWarmMax, zduWarmStable, zduWarmNoPool = time.Second, 200*time.Millisecond, 10*time.Millisecond
	// no pool: short fixed wait
	if l, w := zduWarmWait(func() (int, int) { return 0, 0 }, func() int { return 0 }, func() int { return 1 }); l != 0 || w > 200*time.Millisecond {
		t.Fatalf("no pool %d %v", l, w)
	}
	// links grow 0 -> 6 then hold: READY after they are stable
	t0 := time.Now()
	var mu sync.Mutex
	n := 0
	go func() {
		for i := 0; i < 6; i++ {
			time.Sleep(40 * time.Millisecond)
			mu.Lock()
			n++
			mu.Unlock()
		}
	}()
	l, w := zduWarmWait(func() (int, int) { mu.Lock(); defer mu.Unlock(); return n, 0 }, func() int { return 1 }, func() int { return 6 })
	if l != 6 || w < 400*time.Millisecond || time.Since(t0) > 900*time.Millisecond {
		t.Fatalf("grow: links %d after %v", l, w)
	}
	// never a link: gives up at zduWarmMax
	if l, w := zduWarmWait(func() (int, int) { return 0, 0 }, func() int { return 1 }, func() int { return 1 }); l != 0 || w < time.Second {
		t.Fatalf("no links %d %v", l, w)
	}
	// Review issue 3: one stable link is not warm when the need is 6 (the
	// old wait sent READY after 1 link held 200 ms)
	if l, w := zduWarmWait(func() (int, int) { return 1, 0 }, func() int { return 1 }, func() int { return 6 }); w < time.Second || zduWarmOK(l, 1, 6) {
		t.Fatalf("1 of 6 links: READY after %v", w)
	}
}

// S7: a process with a link pool never sends READY with no link (the old
// process then keeps serving); a pool-less process is not held.
func TestZDUWarmOK(t *testing.T) {
	for _, c := range []struct {
		links, pools, need int
		want               bool
	}{{0, 0, 1, true}, {0, 1, 1, false}, {3, 1, 1, true}, {1, 2, 1, true}, {1, 1, 6, false}, {6, 1, 6, true}} {
		if got := zduWarmOK(c.links, c.pools, c.need); got != c.want {
			t.Errorf("links %d pools %d: %v", c.links, c.pools, got)
		}
	}
}

// Review F2: a handover is refused (nothing spawned, nothing paused, no
// status written) unless every live link's peer runs ZDU; UNIT.peers carries
// the check for hs2-zdu-upgrade.sh.
func TestZDUTakeoverRefusedPeers(t *testing.T) {
	r := newRig(t, okTime)
	r.o.peers = func() (bool, string) { return false, "ZDU-NO links=3 zdu=2" }
	spawned := false
	r.o.spawn = func(string, []string, *os.File) (zduChild, error) { spawned = true; return nil, fmt.Errorf("no") }
	if got := r.o.handover(); got != "refused-peers" {
		t.Fatalf("handover %q, want refused-peers", got)
	}
	if spawned || r.paused != 0 || r.status(t) != "" {
		t.Fatalf("refusal had side effects: spawned %v paused %d status %q", spawned, r.paused, r.status(t))
	}
	if !strings.Contains(r.logText(), "zdu: handover refused: peers: ZDU-NO links=3 zdu=2") {
		t.Fatalf("log: %s", r.logText())
	}
	r.o.writePeers()
	b, _ := os.ReadFile(filepath.Join(r.dir, "u.peers"))
	if string(b) != "ZDU-NO links=3 zdu=2\n" {
		t.Fatalf("u.peers %q", b)
	}
	r.o.peers = func() (bool, string) { return true, "ZDU-OK links=3 zdu=3" }
	r.o.writePeers()
	b, _ = os.ReadFile(filepath.Join(r.dir, "u.peers"))
	if string(b) != "ZDU-OK links=3 zdu=3\n" {
		t.Fatalf("u.peers %q", b)
	}
}

// Review minor: a SIGUSR2 so close to the window that READY + probation
// would reach it is refused like one inside it.
func TestZDUTakeoverRefusedEndOfHour(t *testing.T) {
	r := newRig(t, time.Date(2026, 11, 19, 10, 59, 40, 0, time.UTC))
	if got := r.o.handover(); got != "refused-window" {
		t.Fatalf("handover at HH:59:40 = %q, want refused-window", got)
	}
	if r.status(t) != "" || r.paused != 0 {
		t.Fatal("refusal had side effects")
	}
}

// Review H1 (fails before the fix): direct (non-reverse) mode refuses every
// handover with no side effect, and UNIT.peers says so for the script.
func TestZDUTakeoverRefusedDirect(t *testing.T) {
	r := newRig(t, okTime)
	r.o.direct = true
	spawned := false
	r.o.spawn = func(string, []string, *os.File) (zduChild, error) { spawned = true; return nil, fmt.Errorf("no") }
	if got := r.o.handover(); got != "refused-direct" {
		t.Fatalf("handover %q, want refused-direct", got)
	}
	if spawned || r.paused != 0 || r.status(t) != "" {
		t.Fatal("refusal had side effects")
	}
	if !strings.Contains(r.logText(), "direct (non-reverse) mode") {
		t.Fatalf("log: %s", r.logText())
	}
	r.o.writePeers()
	b, _ := os.ReadFile(filepath.Join(r.dir, "u.peers"))
	if !strings.HasPrefix(string(b), "ZDU-DIRECT ") {
		t.Fatalf("u.peers %q", b)
	}
}

// Review L1 (fails before the fix): the end-of-hour guard counts the whole
// READY timeout (child exec + start + warm wait), not only the warm wait.
func TestZDUTakeoverRefusedEndOfHourStartup(t *testing.T) {
	r := newRig(t, time.Date(2026, 11, 19, 10, 59, 0, 0, time.UTC))
	r.o.ready = handoff.ReadyTimeout
	if got := r.o.handover(); got != "refused-window" {
		t.Fatalf("handover at HH:59:00 with a 90 s READY timeout = %q, want refused-window", got)
	}
}

// Review L3 (fails before the fix): UNIT.peers goes through a per-pid tmp;
// a shared "UNIT.peers.tmp" (here: blocked by a directory) is never used.
func TestZDUPeersPerPidTmp(t *testing.T) {
	r := newRig(t, okTime)
	os.Mkdir(filepath.Join(r.dir, "u.peers.tmp"), 0o700)
	r.o.writePeers()
	b, _ := os.ReadFile(filepath.Join(r.dir, "u.peers"))
	if string(b) != "ZDU-OK links=3 zdu=3\n" {
		t.Fatalf("u.peers %q", b)
	}
}

// Review M2 (fails before the fix): link accept moves to the new process
// (linkPause + boost) only after its LISTENING, not at the spawn; at the
// commit the old process drops its link listener copies.
func TestZDUTakeoverBootAfterListening(t *testing.T) {
	r := newRig(t, okTime)
	feat := handoff.Hello{Proto: handoff.Proto, Features: handoff.FeatListening}
	r.o.hello = func() handoff.Hello { return feat }
	var lp, boosts, drops atomic.Int32
	r.o.linkPause = func() int { lp.Add(1); return 1 }
	r.o.linkResume = func() int { return 1 }
	r.o.boost = func(on bool) (int, int) {
		if on {
			boosts.Add(1)
		}
		return 0, 0
	}
	r.o.linkDrop = func() int { drops.Add(1); return 1 }
	early := make(chan int32, 1)
	r.behave = func(c *handoff.Conn, ch *fakeChild) {
		tk, err := c.Take(feat, time.Second)
		if err != nil {
			ch.Kill()
			return
		}
		time.Sleep(100 * time.Millisecond)
		early <- lp.Load() + boosts.Load()
		if tk.Listening(time.Second) != nil {
			ch.Kill()
			return
		}
		if tk.Ready(time.Second) != nil {
			ch.Kill()
			return
		}
		go tk.Conn.FollowDrain(func(int) {}, 5*time.Second)
	}
	if got := r.o.handover(); got != "ok" {
		t.Fatalf("handover %q: %s", got, r.logText())
	}
	if n := <-early; n != 0 {
		t.Fatalf("link accept moved before LISTENING (%d calls)", n)
	}
	if lp.Load() != 1 || boosts.Load() != 1 || drops.Load() != 1 {
		t.Fatalf("pause %d boost %d drop %d", lp.Load(), boosts.Load(), drops.Load())
	}
}

// Review M2: a new process that never reports LISTENING (hung before its
// engine listens) never takes link accept; the handover fails, nothing moved.
func TestZDUTakeoverNoListeningNoBoot(t *testing.T) {
	r := newRig(t, okTime)
	feat := handoff.Hello{Proto: handoff.Proto, Features: handoff.FeatListening}
	r.o.hello = func() handoff.Hello { return feat }
	r.o.ready = 300 * time.Millisecond
	var lp atomic.Int32
	r.o.linkPause = func() int { lp.Add(1); return 1 }
	r.behave = func(c *handoff.Conn, ch *fakeChild) {
		if _, err := c.Take(feat, time.Second); err != nil {
			ch.Kill()
		}
		// hangs: no LISTENING, no READY
	}
	if got := r.o.handover(); got != "failed" {
		t.Fatalf("handover %q", got)
	}
	if lp.Load() != 0 {
		t.Fatal("link accept paused for a child that never listened")
	}
}

// zdu391 (lab1 S9c, every real handover reverted with "got type 7, want 3"):
// the new process's Take Hello carries FeatListening, the same Hello its
// LISTENING follows, so a ZDU old process waits for LISTENING before READY.
func TestZDUTakeHelloListening(t *testing.T) {
	if zduTakeHello().Features&handoff.FeatListening == 0 {
		t.Fatal("Take Hello without FeatListening")
	}
	if zduTakeHello().Proto != handoff.Proto {
		t.Fatal("Take Hello proto")
	}
}

// Review 4 H1 (fails before the fix): the new process dies after the commit
// (e.g. OOM). The old one has TUN reads off and accept paused: it must end
// the drain at once with the failure exit (cold restart), not keep
// "draining" until users 0 or HH:03.
func TestZDUDrainChildDiesExitsFail(t *testing.T) {
	r := newRig(t, okTime)
	r.behave = goodChild
	r.statsU = 5 // users left: the old drain would wait for the deadline
	failed := make(chan string, 1)
	r.o.exitFail = func(why string) { failed <- why }
	if got := r.o.handover(); got != "ok" {
		t.Fatalf("result %q\n%s", got, r.logText())
	}
	time.Sleep(120 * time.Millisecond)
	t0 := time.Now()
	r.child.Kill() // the new process dies (MAINPID)
	select {
	case why := <-failed:
		if !strings.Contains(why, "4242") || time.Since(t0) > time.Second {
			t.Fatalf("why %q after %v", why, time.Since(t0))
		}
	case <-r.exited:
		t.Fatal("clean exit (0) instead of the failure exit")
	case <-time.After(2 * time.Second):
		t.Fatalf("still draining 2 s after the new process died\n%s", r.logText())
	}
	if s := r.status(t); !strings.HasPrefix(s, "DRAIN-ABORT") {
		t.Fatalf("status %q", s)
	}
	if !strings.Contains(r.logText(), "drain end: ABORT") {
		t.Fatal(r.logText())
	}
}

// Review 4 H1: a broken channel while the new process is alive is not a dark
// tunnel: the old keeps draining its users (no failure exit) and ends at
// users 0 as usual (DRAIN-END).
func TestZDUDrainChannelBrokenChildAlive(t *testing.T) {
	r := newRig(t, okTime)
	r.statsU = 2
	closed := make(chan *handoff.Conn, 1)
	r.behave = func(c *handoff.Conn, ch *fakeChild) {
		tk, err := c.Take(handoff.Hello{Proto: handoff.Proto}, time.Second)
		if err != nil || tk.Ready(time.Second) != nil {
			ch.Kill()
			return
		}
		go tk.Conn.FollowDrain(func(int) {}, 5*time.Second)
		closed <- tk.Conn
	}
	failed := make(chan string, 1)
	r.o.exitFail = func(why string) { failed <- why }
	if got := r.o.handover(); got != "ok" {
		t.Fatalf("result %q\n%s", got, r.logText())
	}
	(<-closed).Close() // the child stops following DRAINSTAT, stays alive
	select {
	case why := <-failed:
		t.Fatalf("failure exit with the new process alive: %s", why)
	case <-r.exited:
		t.Fatal("exited with users left")
	case <-time.After(400 * time.Millisecond):
	}
	r.mu.Lock()
	r.statsU = 0
	r.mu.Unlock()
	select {
	case <-r.exited:
	case <-time.After(2 * time.Second):
		t.Fatalf("no exit at users 0\n%s", r.logText())
	}
	if s := r.status(t); s != "DRAIN-END 4242" {
		t.Fatalf("status %q", s)
	}
}

// Review 4 M1 (fails before the fix): a hand-off child whose parent still
// drains refuses a new handover (no spawn, nothing touched).
func TestZDUTakeoverRefusedParentDraining(t *testing.T) {
	r := newRig(t, okTime)
	r.behave = func(c *handoff.Conn, ch *fakeChild) { t.Error("spawned while the parent drains") }
	r.o.parentDraining = func() (int, bool) { return 777, true }
	if got := r.o.handover(); got != "refused-draining" {
		t.Fatalf("result %q", got)
	}
	if !strings.Contains(r.logText(), "pid 777) still drains") || r.paused != 0 {
		t.Fatal(r.logText())
	}
	r.o.parentDraining = func() (int, bool) { return 0, false }
	r.behave = goodChild
	r.statsU = 0
	if got := r.o.handover(); got != "ok" {
		t.Fatalf("after the parent's drain end: %q\n%s", got, r.logText())
	}
}

// zduParentDraining: draining while the recorded pid is our parent; a
// reparent (the old exited) clears it.
func TestZDUParentDraining(t *testing.T) {
	t.Cleanup(func() { zduParentPid.Store(0) })
	zduParentPid.Store(0)
	if _, d := zduParentDraining(); d {
		t.Fatal("no parent recorded: draining")
	}
	zduParentPid.Store(int64(os.Getppid()))
	if p, d := zduParentDraining(); !d || p != os.Getppid() {
		t.Fatalf("parent alive: %d %v", p, d)
	}
	zduParentPid.Store(int64(os.Getppid()) + 99999)
	if _, d := zduParentDraining(); d || zduParentPid.Load() != 0 {
		t.Fatal("reparented: still draining")
	}
}

// Review 4 low (fails before the fix): a reload (SIGHUP to MAINPID = old)
// between the spawn and GO is forwarded to the new process once.
func TestZDUTakeoverHupForwarded(t *testing.T) {
	r := newRig(t, okTime)
	var hups atomic.Int64
	var got atomic.Int64
	r.o.hups = hups.Load
	r.o.hupChild = func(pid int) error { got.Store(int64(pid)); return nil }
	r.behave = func(c *handoff.Conn, ch *fakeChild) {
		hups.Add(1) // systemctl reload while the child starts
		goodChild(c, ch)
	}
	r.statsU = 0
	if res := r.o.handover(); res != "ok" {
		t.Fatalf("result %q\n%s", res, r.logText())
	}
	if got.Load() != 4242 || !strings.Contains(r.logText(), "forwarded to pid 4242") {
		t.Fatalf("SIGHUP not forwarded (%d)\n%s", got.Load(), r.logText())
	}
	// none came: nothing forwarded
	r2 := newRig(t, okTime)
	r2.o.hups = func() int64 { return 3 }
	r2.o.hupChild = func(int) error { t.Error("forwarded without a reload"); return nil }
	r2.behave = goodChild
	if res := r2.o.handover(); res != "ok" {
		t.Fatalf("result %q", res)
	}
}

// Review 4 low (fails before the fix): a new process that is alive with its
// channel open but never accepts (hung) fails probation: the accept queue of
// the paused listeners stays non-empty for hungAfter -> REVERTED.
func TestZDUTakeoverHungChildReverts(t *testing.T) {
	r := newRig(t, okTime)
	r.behave = goodChild
	r.o.probation = 600 * time.Millisecond
	r.o.hungAfter = 200 * time.Millisecond
	r.o.acceptQ = func() int { return 7 }
	if got := r.o.handover(); got != "reverted" {
		t.Fatalf("result %q\n%s", got, r.logText())
	}
	if s := r.status(t); !strings.Contains(s, "not accepting: accept queue 7") {
		t.Fatalf("status %q", s)
	}
	// a queue that empties between samples is a healthy child
	r2 := newRig(t, okTime)
	r2.behave = goodChild
	r2.statsU = 0
	r2.o.probation = 600 * time.Millisecond
	r2.o.hungAfter = 200 * time.Millisecond
	var n atomic.Int64
	r2.o.acceptQ = func() int { return int(n.Add(1) % 2 * 5) }
	if got := r2.o.handover(); got != "ok" {
		t.Fatalf("healthy: %q\n%s", got, r2.logText())
	}
}

// b454: a pace that needs longer than the time left starts before the
// deadline (lead = paceLead(users) + one drain period), then the exit.
func TestB454PaceStartsBeforeDeadline(t *testing.T) {
	r := newRig(t, okTime)
	r.behave = goodChild
	r.statsU = 5
	r.o.override = time.Hour // the deadline is far off
	var asked atomic.Int32
	r.o.paceLead = func(u int) time.Duration {
		if u != 5 {
			t.Errorf("lead asked for %d users, want 5", u)
		}
		asked.Add(1)
		return 2 * time.Hour
	}
	var paced atomic.Int32
	r.o.pace = func() (int, int, int) { paced.Add(1); return 3, 5, 2 }
	if got := r.o.handover(); got != "ok" {
		t.Fatalf("result %q\n%s", got, r.logText())
	}
	select {
	case <-r.exited:
	case <-time.After(5 * time.Second):
		t.Fatalf("no exit: the pace did not start before the deadline\n%s", r.logText())
	}
	if paced.Load() != 1 || asked.Load() == 0 || !strings.Contains(r.logText(), "before the deadline") {
		t.Fatalf("paced %d asked %d\n%s", paced.Load(), asked.Load(), r.logText())
	}
}
