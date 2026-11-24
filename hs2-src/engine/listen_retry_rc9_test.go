//go:build linux

package engine

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// rc9: an old process after GO (users handed over) does not take a waiting
// user port when it frees; a revert (resume) lets it bind again.
func TestListenUserRetryHeldAfterGO(t *testing.T) {
	addr := b428FreePort(t)
	hold, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer zduOldPaused.Store(false)
	var served atomic.Pointer[net.Listener]
	w, err := listenUserRetry(ctx, "80/tcp", nil, func() (net.Listener, error) { return listenReuseRcvBufCold(addr, 0) }, func(l net.Listener) { served.Store(&l) })
	if err != nil || !w {
		t.Fatalf("waiting %v err %v", w, err)
	}
	zduOldPaused.Store(true) // ZDUPauseAccept at GO
	hold.Close()
	time.Sleep(4 * bindRetryPeriod)
	if served.Load() != nil {
		t.Fatal("old process after GO bound the freed port")
	}
	zduOldPaused.Store(false) // revert: ZDUResumeAccept
	dl := time.Now().Add(2 * time.Second)
	for served.Load() == nil && time.Now().Before(dl) {
		time.Sleep(10 * time.Millisecond)
	}
	l := served.Load()
	if l == nil {
		t.Fatal("never bound after the resume")
	}
	(*l).Close()
}
