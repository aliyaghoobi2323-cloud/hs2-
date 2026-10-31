package engine

import (
	"bytes"
	"io"
	"testing"
	"time"
)

// The move back (moveback.go; opt-in at both ends): a connection that moved
// to the BULK link for its download (the exit's hint) moves back to the
// QUIET link once light, and carries on there both ways; every byte of the
// download arrives in order. Without the switch it stays on BULK.
func TestMoveBackLight(t *testing.T) {
	angleCores(t)
	if testing.Short() {
		t.Skip("emulated links, ~11 s")
	}
	for _, on := range []bool{true, false} {
		t.Run(map[bool]string{true: "on", false: "off"}[on], func(t *testing.T) {
			if on {
				t.Setenv("HS2_TUNE_MOVE_BACK", "1")
			}
			// restored once the rig's mover ended (a cleanup registered
			// before the rig's runs after them): a defer ran while the mover
			// could still read them (a race -race saw in the off case)
			v, l, h := TuneMoveBack, moveBackLight, moveBackHold
			t.Cleanup(func() { TuneMoveBack, moveBackLight, moveBackHold = v, l, h })
			TuneMoveBack = map[bool]int{true: 1, false: 0}[on]
			moveBackLight, moveBackHold = 800*time.Millisecond, 1500*time.Millisecond
			// 10 Mbit/s: the 3 MiB download lasts ~2.5 s, past the exit's
			// hint and the mover's tick even under -race and a loaded host
			// (at 40 Mbit/s it once ended in 0.67 s, never moved)
			r := hintRig(t, angleRateBps, 10*time.Millisecond)
			st := r.open(8, qpPhase)
			b := make([]byte, 32<<10)
			var off int64
			// the move does not race the end of the download: the exit hints
			// a persistent send backlog (hint.go: its writer inside big
			// writes 3 ticks in a row), and the CI once saw this download
			// end unmoved («never moved (0 moves)», then 5 of 5 alone; 100
			// runs of the old form in 2-core slices, 25 of them next to
			// CPU hogs, did not bring it back: the cause is not known).
			// After its first part the reader holds until the move: the
			// exit's writer then waits inside a big write, the very backlog
			// the hint is for. TestHintMovesDownload keeps the reader going
			// and bounds the hint's time
			const holdAt = 256 << 10
			for off < qpPhaseBytes {
				if off >= holdAt && r.q.moved.Load() == 0 {
					waitQuiet(t, "the download moved on the exit's hint (its reader holding)", func() bool { return r.q.moved.Load() == 1 })
				}
				n, err := st.Read(b)
				for i := 0; i < n; i++ {
					if b[i] != phaseByte(off+int64(i)) {
						t.Fatalf("byte %d out of order", off+int64(i))
					}
				}
				off += int64(n)
				if err != nil {
					t.Fatalf("read %d of %d: %v", off, qpPhaseBytes, err)
				}
			}
			if r.q.moved.Load() != 1 {
				t.Fatalf("the download never moved (%d moves)", r.q.moved.Load())
			}
			if !on {
				time.Sleep(3 * time.Second)
				if r.q.movedBack.Load() != 0 {
					t.Fatalf("moved back with the switch off")
				}
				st.Close()
				return
			}
			waitQuiet(t, "moved back", func() bool { return r.q.movedBack.Load() == 1 })
			gs := st.(*groupStream)
			if gs.cur().link != r.quiet.edge {
				t.Fatal("moved back, but not on the QUIET link")
			}
			msg := bytes.Repeat([]byte("light again "), 100)
			if _, err := st.Write(msg); err != nil {
				t.Fatal(err)
			}
			got := make([]byte, len(msg))
			if _, err := io.ReadFull(st, got); err != nil || !bytes.Equal(got, msg) {
				t.Fatalf("the echo after the move back: %v", err)
			}
			if gs.g.Moves() != 2 {
				t.Fatalf("group moves %d, want 2", gs.g.Moves())
			}
			st.Close()
		})
	}
}
