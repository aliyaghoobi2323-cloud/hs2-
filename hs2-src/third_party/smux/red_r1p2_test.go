package smux

import (
	"bytes"
	"fmt"
	"io"
	"testing"
	"time"
)

// R1 red team, second pass (method step 4; docs/architecture/wip/r1/
// redteam2/REPORT.fa.md): a move stream on a link without a frame scheduler
// (R2-4), and a group frame at the largest frame size (R2-5).

// noSchedConfig is a v2 session of hs2 without the frame scheduler (a link
// offered no CapSched).
func noSchedConfig() *Config {
	c := DefaultConfig()
	c.Version = 2
	c.KeepAliveDisabled = true
	c.MaxFrameSize = 1024
	return c
}

// R2-4: the peer opens a group's main stream on a link with the scheduler,
// then its move stream on a link without one. Attach must refuse it: taken,
// the exit's next write on the group went to frameFor of a session whose
// scheduler is nil, and the process died on a nil pointer (every user of
// the exit with it).
func TestRed2AttachNoSched(t *testing.T) {
	a := newGlink(t, time.Millisecond, 0, 41, groupConfig)
	b := newGlink(t, time.Millisecond, 0, 43, noSchedConfig)
	x := serveExit(t, a.exit)
	g, err := a.edge.OpenGroup(0xC0DE, 0, 77)
	if err != nil {
		t.Fatal(err)
	}
	var xg *Group
	select {
	case xg = <-x.got:
	case <-time.After(5 * time.Second):
		t.Fatal("the exit never accepted the group")
	}
	// the move stream, by hand on the link without a scheduler (Move
	// itself refuses such a session at the opener)
	if _, err := b.edge.OpenStreamSyn(EncodeSynGroup(0xC0DE, 2, RoleMove, 77, 0)); err != nil {
		t.Fatal(err)
	}
	st, err := b.exit.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	err = xg.Attach(st)
	if err == nil {
		// the old behaviour: show what the next write does
		werr := func() (e error) {
			defer func() {
				if r := recover(); r != nil {
					e = fmt.Errorf("panic: %v", r)
				}
			}()
			_, e = xg.Write([]byte("x"))
			return e
		}()
		t.Fatalf("a move stream on a session without a scheduler was attached; the group's next write: %v", werr)
	}
	if err != ErrNoMove {
		t.Fatalf("attach: %v, want ErrNoMove", err)
	}
	// the group still works on its main stream
	if _, err := xg.Write([]byte("ok")); err != nil {
		t.Fatalf("write after the refused move: %v", err)
	}
	buf := make([]byte, 2)
	g.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(g, buf); err != nil || string(buf) != "ok" {
		t.Fatalf("read after the refused move: %q %v", buf, err)
	}
}

// R2-5: with MaxFrameSize 65535 (VerifyConfig's limit; hs2 takes it from
// HS2_TUNE_SMUX_FRAME), a group frame's length on the wire is the data and
// seqLen: 65539 in a 16-bit field. The bytes must arrive as written.
func TestRed2GroupFrameMax(t *testing.T) {
	cfg := func() *Config {
		c := groupConfig()
		c.MaxFrameSize = 65535
		c.MaxStreamBuffer = 1 << 20
		c.MaxReceiveBuffer = 4 << 20
		return c
	}
	a := newGlink(t, time.Millisecond, 0, 45, cfg)
	x := serveExit(t, a.exit)
	g, err := a.edge.OpenGroup(0xF00D, 0, 78)
	if err != nil {
		t.Fatal(err)
	}
	var xg *Group
	select {
	case xg = <-x.got:
	case <-time.After(5 * time.Second):
		t.Fatal("the exit never accepted the group")
	}
	want := make([]byte, 3*65535+17)
	for i := range want {
		want[i] = byte(i*13 + i>>9)
	}
	go g.Write(want)
	got := make([]byte, len(want))
	xg.SetReadDeadline(time.Now().Add(10 * time.Second))
	n, err := io.ReadFull(xg, got)
	if err != nil || !bytes.Equal(got, want) {
		k := 0
		for k < n && got[k] == want[k] {
			k++
		}
		t.Fatalf("read %d of %d bytes (%v), first wrong byte at %d", n, len(want), err, k)
	}
}
