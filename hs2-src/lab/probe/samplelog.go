package main

// lbv81: -samplelog FILE writes every measured latency as "CLASS SEC MS"
// (crowd c, call k, game g) and every crowd failure as "F SEC 0", so a long
// soak can be cut into phases offline. -crowdlife D: each crowd connection
// closes after a uniform 0.5x-1.5x D and redials (user reconnect churn);
// a churn close is not a failure.

// dc254: the samples were written through a 1 MiB bufio.Writer under
// sampleMu straight into the harness FIFO. Each time the buffer filled
// (~every 10 s at crowd 3000) the flush blocked under the lock until the
// python reader drained 1 MiB, and every call/game/echo/crowd goroutine
// that took a sample waited too: all classes showed a 0.5-1.2 s "freeze"
// at the same second, a probe artifact, not the tunnel. Now sample() only
// appends to a memory buffer; a writer goroutine swaps it out and writes it
// outside the lock (a slow reader delays the log, never the measurement).
// Confirmed on lab1 (dc254 X1-X3): lo captures on both ends showed the
// replies reaching the client socket on time (edge RTT max 183 ms, server
// 4 ms) while the probe logged 450-700 ms; with this writer the 10.3 s
// spikes are gone (call max 634/707 -> 323 ms). The earlier "E1 still had
// them" ran the old probe: the fixed binary was put in the work dir while
// the previous segment ran, and that segment's cleanup deleted it.

import (
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"
)

var (
	sampleMu   sync.Mutex
	sampleBuf  []byte
	sampleF    *os.File
	sampleDone chan struct{}
	sampleStop chan struct{}
	crowdLife  time.Duration
	sampleFile string
)

func sampleOpen() {
	if sampleFile == "" {
		return
	}
	f, err := os.Create(sampleFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	sampleMu.Lock()
	sampleF, sampleBuf = f, make([]byte, 0, 1<<20)
	sampleDone, sampleStop = make(chan struct{}), make(chan struct{})
	sampleMu.Unlock()
	go sampleWriter(f, sampleStop, sampleDone)
}

// sampleWriter writes the buffered samples every 50 ms, outside sampleMu.
func sampleWriter(f *os.File, stop, done chan struct{}) {
	defer close(done)
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	spare := make([]byte, 0, 1<<20)
	for {
		select {
		case <-stop:
			sampleMu.Lock()
			b := sampleBuf
			sampleBuf = nil
			sampleMu.Unlock()
			f.Write(b)
			return
		case <-t.C:
		}
		sampleMu.Lock()
		b := sampleBuf
		sampleBuf = spare[:0]
		sampleMu.Unlock()
		if len(b) > 0 {
			f.Write(b)
		}
		spare = b
	}
}

func sample(class string, sec, v float64) {
	sampleMu.Lock()
	if sampleF != nil {
		b := append(sampleBuf, class...)
		b = append(b, ' ')
		b = strconv.AppendFloat(b, sec, 'f', 2, 64)
		b = append(b, ' ')
		b = strconv.AppendFloat(b, v, 'f', 1, 64)
		sampleBuf = append(b, '\n')
	}
	sampleMu.Unlock()
}

func sampleClose() {
	sampleMu.Lock()
	f, stop, done := sampleF, sampleStop, sampleDone
	sampleF = nil
	sampleMu.Unlock()
	if f == nil {
		return
	}
	close(stop)
	<-done
	f.Close()
}
