package engine

import (
	"context"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

type panicAcceptor struct{ n atomic.Int64 }

func (a *panicAcceptor) AcceptFront(ctx context.Context, raw net.Conn, onTunnel func(*tlscarrier.Carrier)) {
	a.n.Add(1)
	var m map[string]int
	m["garbage hello"]++ // nil map write: what a parser bug looks like
}

type okAcceptor struct{}

func (okAcceptor) AcceptFront(ctx context.Context, raw net.Conn, onTunnel func(*tlscarrier.Carrier)) {
	onTunnel(nil) // the carrier itself does not matter here
}

// A flood of conns that each panic in the front's accept: the process lives,
// each conn is closed, every panic is counted and at most one line is logged.
func TestGuardAcceptPanicFlood(t *testing.T) {
	before := panicsRecovered.Load()
	panicLogAt.Store(0)
	var mu sync.Mutex
	var lines []string
	logf := func(f string, a ...any) { mu.Lock(); lines = append(lines, f); mu.Unlock() }
	acc := &panicAcceptor{}
	const N = 500
	var wg sync.WaitGroup
	closed := atomic.Int64{}
	for i := 0; i < N; i++ {
		c1, c2 := net.Pipe()
		wg.Add(1)
		go func() {
			defer wg.Done()
			guardAccept(context.Background(), acc, c1, logf, func(*tlscarrier.Carrier) { t.Error("onTunnel after a panic") })
			c2.SetReadDeadline(time.Now().Add(time.Second))
			if _, err := c2.Read(make([]byte, 1)); err != nil && !strings.Contains(err.Error(), "deadline") {
				closed.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := panicsRecovered.Load() - before; got != N {
		t.Fatalf("recovered %d panics, want %d", got, N)
	}
	if closed.Load() != N {
		t.Fatalf("%d of %d conns closed", closed.Load(), N)
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "recovered a panic") {
		t.Fatalf("log lines %d (%v), want exactly 1", len(lines), lines)
	}
}

func TestGuardAcceptHandsCarrier(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c2.Close()
	called := false
	guardAccept(context.Background(), okAcceptor{}, c1, nil, func(*tlscarrier.Carrier) { called = true })
	// okAcceptor hands nil: guardAccept only calls onTunnel with a carrier
	if called {
		t.Fatal("onTunnel called without a carrier")
	}
}
