package engine

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// pingTimes runs openControl over a real smux pair and records when each ping
// reaches the exit; busy makes the link move activeBytes between ticks.
func pingTimes(t *testing.T, busy bool, d time.Duration, answer func(seq uint64) bool) []time.Time {
	t.Helper()
	a, b := net.Pipe()
	mtr := &linkMeter{statsPoll: make(chan struct{}, 1)}
	cli, _, err := newSession(a, false, nil, mtr)
	if err != nil {
		t.Fatal(err)
	}
	srv, _, err := newSession(b, true, nil, &linkMeter{})
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	defer srv.Close()
	var mu sync.Mutex
	var at []time.Time
	go func() {
		st, err := srv.AcceptStream()
		if err != nil {
			return
		}
		var k [1]byte
		io.ReadFull(st, k[:])
		ping, pong := make([]byte, ctrlPingLen), make([]byte, ctrlPongLen)
		for {
			if _, err := io.ReadFull(st, ping); err != nil {
				return
			}
			mu.Lock()
			at = append(at, time.Now())
			mu.Unlock()
			if answer != nil && !answer(binary.BigEndian.Uint64(ping)) {
				continue
			}
			copy(pong, ping)
			st.Write(pong)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	if busy {
		go func() {
			for ctx.Err() == nil {
				mtr.rdBytes.Add(activeBytes)
				time.Sleep(100 * time.Millisecond)
			}
		}()
	}
	openControl(ctx, &mtcpLink{sess: cli, mtr: mtr}, nil)
	mu.Lock()
	defer mu.Unlock()
	return append([]time.Time(nil), at...)
}

// An active link pings on the fixed controlInterval beat (the cadence the
// download-loss rule is tuned against); an idle one only every few ticks.
func TestControlPingCadence(t *testing.T) {
	if testing.Short() {
		t.Skip("real-time cadence")
	}
	at := pingTimes(t, true, 13*time.Second, nil)
	if len(at) < 3 {
		t.Fatalf("active: %d pings in 13 s", len(at))
	}
	for i := 1; i < len(at); i++ {
		if gap := at[i].Sub(at[i-1]); gap < controlInterval-300*time.Millisecond || gap > controlInterval+300*time.Millisecond {
			t.Fatalf("active ping gap %s, want the fixed %s", gap, controlInterval)
		}
	}
	idle := pingTimes(t, false, 19*time.Second, nil)
	if len(idle) > 3 {
		t.Fatalf("idle: %d pings in 19 s, want at most 3", len(idle))
	}
}

// One unanswered ping does not end the control channel: later pings and
// their pongs keep flowing (the late pong is skipped by its seq).
func TestControlSurvivesLatePong(t *testing.T) {
	if testing.Short() {
		t.Skip("real-time cadence")
	}
	at := pingTimes(t, true, 17*time.Second, func(seq uint64) bool { return seq != 2 })
	if len(at) < 4 {
		t.Fatalf("%d pings in 17 s: the channel stopped after an unanswered ping", len(at))
	}
}
