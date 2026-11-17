//go:build linux

package engine

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// hc213 (audit211 H1): a user that resets mid-download on a half-close link.
// The edge closes its stream without a CloseWrite (raw first sub, one-stream
// path: one smux FIN before hc213). The exit must take that as the user gone
// (the "abandoned" mark / second FIN), not as a half-close: it releases the
// target at once (an RST) instead of writing into a dropped stream until its
// halfCloseIdle, and the half-open gauge goes back to 0.
func TestHalfCloseAbortDownload(t *testing.T) {
	oldIdle, oldPoll := halfCloseIdle.Load(), halfClosePoll.Load()
	halfCloseIdle.Store(int64(40 * time.Second)) // the exit's: 60 s
	halfClosePoll.Store(int64(200 * time.Millisecond))
	t.Cleanup(func() { halfCloseIdle.Store(oldIdle); halfClosePoll.Store(oldPoll) })
	hcPlanes(t, func(t *testing.T) {
		ended := make(chan time.Duration, 4)
		start := make(chan time.Time, 4)
		addr := startTunnelTarget(t, 3, 3, hcTarget(t, func(c *net.TCPConn) {
			b := make([]byte, 4)
			c.SetReadDeadline(time.Now().Add(10 * time.Second))
			if _, err := io.ReadFull(c, b); err != nil || string(b) != "DOWN" {
				return
			}
			c.SetReadDeadline(time.Time{})
			chunk := hcPattern(16<<10, 3)
			var t0 time.Time
			for i := 0; ; i++ {
				if i == 64 {
					t0 = <-start // the user is about to reset
				}
				if _, err := c.Write(chunk); err != nil {
					break
				}
				if !t0.IsZero() {
					time.Sleep(20 * time.Millisecond)
				}
				if !t0.IsZero() && time.Since(t0) > 30*time.Second {
					break
				}
			}
			ended <- time.Since(t0)
		}))
		waitHC(t, addr)
		aborts := halfCloseAborts.Load()
		c := hcDial(t, addr)
		c.Write([]byte("DOWN"))
		buf := make([]byte, 512<<10)
		if _, err := io.ReadFull(c, buf); err != nil {
			t.Fatal(err)
		}
		start <- time.Now()
		time.Sleep(100 * time.Millisecond)
		c.SetLinger(0)
		c.Close() // the user resets mid-download
		var d time.Duration
		select {
		case d = <-ended:
		case <-time.After(35 * time.Second):
			t.Fatal("the target was never released")
		}
		t.Logf("target released %v after the user's reset; aborts +%d", d.Round(time.Millisecond), halfCloseAborts.Load()-aborts)
		if d > 5*time.Second {
			t.Fatalf("target released after %v: want at once (not the exit's half-close idle)", d)
		}
		mWaitFor(t, "halfOpen back to 0", 10*time.Second, func() bool { return halfOpenNow.Load() == 0 })
	})
}

// hc213: a user that resets mid-upload (never sent its FIN): the target gets
// a reset, not a clean FIN that would pass the cut upload as complete.
func TestHalfCloseAbortUpload(t *testing.T) {
	hcPlanes(t, func(t *testing.T) {
		res := make(chan error, 4)
		addr := startTunnelTarget(t, 3, 3, hcTarget(t, func(c *net.TCPConn) {
			b := make([]byte, 2)
			c.SetReadDeadline(time.Now().Add(10 * time.Second))
			if _, err := io.ReadFull(c, b); err != nil || string(b) != "UP" {
				return
			}
			c.Write([]byte("ok")) // and the target's side ends: download done
			c.CloseWrite()
			n, err := io.Copy(io.Discard, readerDeadline{c, 20 * time.Second})
			if err == nil {
				// io.Copy: a clean EOF; an RST after the FIN shows in SO_ERROR
				time.Sleep(300 * time.Millisecond)
				err = sockErr(c)
			}
			if err == nil {
				err = errors.New("clean FIN after " + itoa(int(n)) + " bytes")
			} else {
				err = nil
			}
			res <- err
		}))
		waitHC(t, addr)
		c := hcDial(t, addr)
		c.Write([]byte("UP"))
		b := make([]byte, 2)
		c.SetReadDeadline(time.Now().Add(10 * time.Second))
		if _, err := io.ReadFull(c, b); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 16; i++ {
			c.Write(hcPattern(16<<10, byte(i)))
		}
		time.Sleep(300 * time.Millisecond)
		c.SetLinger(0)
		c.Close() // the user resets mid-upload
		select {
		case err := <-res:
			if err != nil {
				t.Fatalf("target: %v; want a reset", err)
			}
		case <-time.After(25 * time.Second):
			t.Fatal("the target never ended")
		}
	})
}

type readerDeadline struct {
	c *net.TCPConn
	d time.Duration
}

func (r readerDeadline) Read(p []byte) (int, error) {
	r.c.SetReadDeadline(time.Now().Add(r.d))
	return r.c.Read(p)
}

// hc213 (audit211 M1): an UPGRADE that reaches the exit after its raw
// half-close closes only the joining sub: the exit does not end (no RST
// record that could overtake the raw tail), and Iran reads the raw download
// to its end.
func TestHalfCloseUpgradeAfterRawFin(t *testing.T) {
	_, iran, exit := newRawPair(t)
	iran.mu.Lock()
	if iran.rawTimer != nil {
		iran.rawTimer.Stop() // no upgrade of its own (the 1 s rate rule)
	}
	iran.mu.Unlock()
	data := randBytes(200<<10, 77)
	got := make(chan error, 1)
	go func() { got <- readN(iran, data) }()
	if err := xfer(exit, data, 8<<10, 0); err != nil {
		t.Fatal(err)
	}
	// the exit's raw half-close (rawCloseWrite without the pipe's FIN)
	exit.mu.Lock()
	exit.wclosed = true
	exit.sendOff = uint64(exit.rawSent.Load())
	exit.mu.Unlock()
	late := rawHCUpgradeLate.Load()
	forceUpgrade(iran)
	mWaitFor(t, "late UPGRADE seen", 5*time.Second, func() bool { return rawHCUpgradeLate.Load() > late })
	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("iran read: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("iran read hung")
	}
	exit.mu.Lock()
	err := exit.err
	exit.mu.Unlock()
	if err == errMConnProto {
		t.Fatalf("exit ended with %v (an RST): want only the joining sub closed", err)
	}
	closeAndWait(t, iran, exit)
}
