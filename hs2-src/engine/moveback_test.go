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
		t.Skip("emulated links, ~5 s")
	}
	for _, on := range []bool{true, false} {
		t.Run(map[bool]string{true: "on", false: "off"}[on], func(t *testing.T) {
			if on {
				t.Setenv("HS2_TUNE_MOVE_BACK", "1")
			}
			defer func(v int, l, h time.Duration) { TuneMoveBack, moveBackLight, moveBackHold = v, l, h }(TuneMoveBack, moveBackLight, moveBackHold)
			TuneMoveBack = map[bool]int{true: 1, false: 0}[on]
			moveBackLight, moveBackHold = 800*time.Millisecond, 1500*time.Millisecond
			r := hintRig(t, 4*angleRateBps, 10*time.Millisecond)
			st := r.open(8, qpPhase)
			b := make([]byte, 32<<10)
			var off int64
			for off < qpPhaseBytes {
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
