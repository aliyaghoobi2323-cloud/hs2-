package engine

import (
	"context"
	"errors"
	"sync/atomic"
	"syscall"
	"time"
)

// b428: a user port whose bind fails with EADDRINUSE is retried in the
// background instead of failing the start. Moving a port between two units
// through ZDU handovers: the unit that gains the port starts while the other
// one still holds it; the old process of the unit that loses it closes its
// copy at GO (zdu_unused.go) and this bind succeeds within one retry period.
// Like b425's freebind (EADDRNOTAVAIL), the start never fails on a user
// address that becomes bindable later. The process does not exit, its ZDU
// probation does not fail, every other port serves normally meanwhile.
//
// One log line when it starts waiting and one when it binds.

// bindRetryPeriod is the retry period (200-500 ms).
var bindRetryPeriod = 300 * time.Millisecond

// BindWaits / BindWaitBound count user binds that waited and later bound.
var (
	BindWaits  atomic.Int64
	BindWaitOK atomic.Int64
)

// bindRetryHeld: no background bind while this process's users are handed
// over to a new process (ZDU, old process after GO).
var bindRetryHeld = func() bool { return zduOldPaused.Load() }

func isAddrInUse(err error) bool { return errors.Is(err, syscall.EADDRINUSE) }

// listenUserRetry binds with listen; on EADDRINUSE it returns at once and
// keeps retrying in the background until ctx ends, calling serve(x) once
// bound. Any other error is returned (the start fails as before).
func listenUserRetry[T any](ctx context.Context, what string, logf func(string, ...any), listen func() (T, error), serve func(T)) (waiting bool, err error) {
	x, err := listen()
	if err == nil {
		serve(x)
		return false, nil
	}
	if !isAddrInUse(err) {
		return false, err
	}
	BindWaits.Add(1)
	if logf != nil {
		logf("user port %s: address in use (another unit or process holds it); retrying every %v until it is free", what, bindRetryPeriod)
	}
	go func() {
		t0, other := time.Now(), false
		tk := time.NewTicker(bindRetryPeriod)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
			}
			if bindRetryHeld() {
				continue // rc9: handed over (old process after GO); a revert resumes
			}
			x, err := listen()
			if err == nil {
				BindWaitOK.Add(1)
				if logf != nil {
					logf("user port %s: bound after waiting %v", what, time.Since(t0).Round(time.Millisecond))
				}
				serve(x)
				return
			}
			if !isAddrInUse(err) && !other && logf != nil {
				other = true
				logf("user port %s: bind retry: %v (still retrying)", what, err)
			}
		}
	}()
	return true, nil
}
