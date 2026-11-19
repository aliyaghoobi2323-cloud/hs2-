package main

import (
	"os"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// b376: /dev/null is a character device but not a terminal.
func TestIsTTYRejectsDevNull(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("termios ioctl")
	}
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Skip(err)
	}
	defer f.Close()
	if isTTY(f) {
		t.Fatal("/dev/null taken for a terminal")
	}
}

// b376: a SIGINT at the no-echo prompt restores echo and exits 130.
func TestRestoreOnSignal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signals")
	}
	restored := make(chan struct{}, 1)
	code := make(chan int, 1)
	stop := restoreOnSignal(func() { restored <- struct{}{} }, func(c int) { code <- c })
	defer stop()
	syscall.Kill(os.Getpid(), syscall.SIGINT)
	select {
	case c := <-code:
		if c != 130 {
			t.Fatalf("exit %d, want 130", c)
		}
		select {
		case <-restored:
		default:
			t.Fatal("exit before echo was restored")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SIGINT not handled")
	}
}

// b376: stop ends the watch without running restore.
func TestRestoreOnSignalStop(t *testing.T) {
	ran := false
	stop := restoreOnSignal(func() { ran = true }, func(int) {})
	stop()
	time.Sleep(10 * time.Millisecond)
	if ran {
		t.Fatal("restore ran without a signal")
	}
}
