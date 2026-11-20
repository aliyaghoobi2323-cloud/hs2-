//go:build linux

package handoff

import (
	"bufio"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func rightsOf(fds []FD) []byte {
	n := make([]int, len(fds))
	for i, f := range fds {
		n[i] = int(f.File.Fd())
	}
	return unix.UnixRights(n...)
}

// serve answers every connection with tag and closes it.
func serve(l net.Listener, tag string, wg *sync.WaitGroup) {
	defer wg.Done()
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go func() { c.Write([]byte(tag + "\n")); c.Close() }()
	}
}

// childTake: the new process of TestTwoProcessListenerHandoff.
func childTake() int {
	c, err := Dial(os.Getenv("HS2_HANDOFF_PATH"), Policy{UID: uint32(os.Getuid()), Self: selfExe()}, 2*time.Second)
	if err != nil {
		os.Stderr.WriteString("child dial: " + err.Error() + "\n")
		return 2
	}
	tk, err := c.Take(Hello{Proto: Proto, Gen: 2}, StepTimeout)
	if err != nil {
		os.Stderr.WriteString("child take: " + err.Error() + "\n")
		return 3
	}
	f := tk.File("user")
	l, err := net.FileListener(f)
	if err != nil {
		return 4
	}
	f.Close()
	var wg sync.WaitGroup
	wg.Add(1)
	go serve(l, "new", &wg)
	if err := tk.Ready(StepTimeout); err != nil {
		os.Stderr.WriteString("child ready: " + err.Error() + "\n")
		return 5
	}
	io.Copy(io.Discard, os.Stdin) // the parent closes our stdin: stop
	l.Close()
	return 0
}

// A real hand-off of a listening TCP socket between two processes while
// clients connect without pause: no connection is refused or reset, both
// processes answer, and the new one alone answers after GO.
func TestTwoProcessListenerHandoff(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	var swg sync.WaitGroup
	swg.Add(1)
	go serve(ln, "old", &swg)

	var ok, refused, other, oldN, newN, afterOld atomic.Int64
	var handed atomic.Bool
	var lastErr atomic.Value
	stop := make(chan struct{})
	var cwg sync.WaitGroup
	for i := 0; i < 8; i++ {
		cwg.Add(1)
		go func() {
			defer cwg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				goneOld := handed.Load()
				c, err := net.DialTimeout("tcp", addr, 2*time.Second)
				if err != nil {
					if errors.Is(err, syscall.ECONNREFUSED) {
						refused.Add(1)
					} else {
						other.Add(1)
					}
					lastErr.Store(err.Error())
					continue
				}
				c.SetReadDeadline(time.Now().Add(3 * time.Second))
				line, err := bufio.NewReader(c).ReadString('\n')
				c.Close()
				switch {
				case err != nil:
					other.Add(1)
					lastErr.Store("read: " + err.Error())
				case line == "old\n":
					oldN.Add(1)
					ok.Add(1)
					if goneOld {
						afterOld.Add(1)
					}
				case line == "new\n":
					newN.Add(1)
					ok.Add(1)
				}
			}
		}()
	}
	time.Sleep(300 * time.Millisecond)

	dir, _ := os.MkdirTemp("", "hs2ho")
	defer os.RemoveAll(dir)
	os.Chmod(dir, 0o700)
	path := filepath.Join(dir, "unit.sock")
	hl, err := Listen(path, testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	defer hl.Close()
	cmd := exec.Command(selfExe(), "-test.run=^$")
	cmd.Env = append(os.Environ(), childEnv+"=take", "HS2_HANDOFF_PATH="+path)
	stdin, _ := cmd.StdinPipe()
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Wait()
	c, err := hl.Accept(5 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	lf := listenerFile(t, ln)
	t0 := time.Now()
	if _, err := c.Give(Hello{Proto: Proto, Gen: 1}, []FD{{"user", lf}}, StepTimeout, ReadyTimeout); err != nil {
		t.Fatal(err)
	}
	// GO sent: the old process stops accepting (closes its copies); the
	// socket and its queue live on in the child
	lf.Close()
	ln.Close()
	swg.Wait()
	handed.Store(true)
	t.Logf("hand-off took %v", time.Since(t0))
	before := ok.Load()
	time.Sleep(time.Second)
	close(stop)
	cwg.Wait()
	stdin.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("child: %v", err)
	}
	t.Logf("ok=%d (old=%d new=%d, after GO=%d) refused=%d other=%d last=%v", ok.Load(), oldN.Load(), newN.Load(), ok.Load()-before, refused.Load(), other.Load(), lastErr.Load())
	if refused.Load() != 0 || other.Load() != 0 {
		t.Fatalf("connections failed during the hand-off: refused=%d other=%d (%v)", refused.Load(), other.Load(), lastErr.Load())
	}
	if oldN.Load() == 0 || newN.Load() == 0 || ok.Load()-before == 0 {
		t.Fatal("both processes must have answered")
	}
	if afterOld.Load() != 0 {
		t.Fatalf("%d answers from the old process after it stopped accepting", afterOld.Load())
	}
}
