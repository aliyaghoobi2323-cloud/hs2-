package engine

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// slowCloseListener takes a while to close, like the icmp listener, whose
// Close removes its reply rule from the kernel (an nft/iptables command).
type slowCloseListener struct {
	closed atomic.Bool
}

func (l *slowCloseListener) Accept(ctx context.Context) (Carrier, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (l *slowCloseListener) Close() error {
	time.Sleep(150 * time.Millisecond)
	l.closed.Store(true)
	return nil
}

type idleTun struct{ ctx context.Context }

func (t idleTun) MTU() int                    { return 1280 }
func (t idleTun) Write(p []byte) (int, error) { return len(p), nil }
func (t idleTun) Read(p []byte) (int, error) {
	<-t.ctx.Done()
	return 0, t.ctx.Err()
}

// When a run returns, its listener must already be CLOSED — not merely asked
// to close from a goroutine. The daemon exits as soon as the run returns, and
// a close still in flight then never happens: the icmp tunnel's nft table was
// left behind on 19 of 20 stops.
func TestRunClosesListenerBeforeReturning(t *testing.T) {
	for _, c := range []struct {
		name string
		run  func(ctx context.Context, ln *slowCloseListener) error
	}{
		{"exit direct (kharej listens)", func(ctx context.Context, ln *slowCloseListener) error {
			return RunDgExit(ctx, DgConfig{Dev: idleTun{ctx}, Min: 1, Max: 2, PerLink: 8, Listener: ln})
		}},
		{"edge reverse (iran listens)", func(ctx context.Context, ln *slowCloseListener) error {
			return RunDgEdge(ctx, DgConfig{Dev: idleTun{ctx}, Min: 1, Max: 2, PerLink: 8, Reverse: true, Listener: ln})
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			ln := &slowCloseListener{}
			done := make(chan struct{})
			go func() { c.run(ctx, ln); close(done) }()
			time.Sleep(100 * time.Millisecond)
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("run did not return after cancel")
			}
			if !ln.closed.Load() {
				t.Fatal("run returned while its listener was still closing (the process would exit before Close finished)")
			}
		})
	}
}
