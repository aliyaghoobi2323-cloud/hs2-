package engine

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

// The exit's move hint (hint.go): its codec, its rule, and the edge's
// mover taking it, on the emulated QUIET and BULK links of quiet_test.go.

func TestHintCodec(t *testing.T) {
	var b [hintMsgLen]byte
	hintEncode(b[:], 0xA1B2C3D4)
	if got := b; got != [hintMsgLen]byte{1, 1, 0, 0, 0xA1, 0xB2, 0xC3, 0xD4} {
		t.Fatalf("wire % x", got)
	}
	if id, ok := hintDecode(b[:]); !ok || id != 0xA1B2C3D4 {
		t.Fatalf("decode: %x %v", id, ok)
	}
	for _, bad := range [][]byte{
		{2, 1, 0, 0, 0, 0, 0, 1}, // another version
		{1, 2, 0, 0, 0, 0, 0, 1}, // another type
		{1, 1, 0, 0, 0, 0, 0, 0}, // no group
		{1, 1, 0, 0, 0, 0, 1},    // short
	} {
		if _, ok := hintDecode(bad); ok {
			t.Fatalf("decoded % x", bad)
		}
	}
}

// FuzzHintDecode: the edge's parser of what the exit sends (§5.10 step 5:
// every parser of the peer's data is fuzzed) never panics, and what it
// takes is what the encoder makes.
func FuzzHintDecode(f *testing.F) {
	var b [hintMsgLen]byte
	hintEncode(b[:], 7)
	f.Add(b[:])
	f.Add([]byte{})
	f.Add([]byte{1, 1, 0, 0, 0, 0, 0, 0, 9, 9})
	f.Fuzz(func(t *testing.T, p []byte) {
		id, ok := hintDecode(p)
		if !ok {
			return
		}
		var re [hintMsgLen]byte
		hintEncode(re[:], id)
		if id == 0 || !bytes.Equal(re[:2], p[:2]) || !bytes.Equal(re[4:], p[4:8]) {
			t.Fatalf("% x decoded as %x", p, id)
		}
	})
}

// bigWrites counts only the writes of hintWriteMin bytes and more, and the
// one in progress.
func TestBigWrites(t *testing.T) {
	var b bigWrites
	slow := func(d time.Duration) func([]byte) (int, error) {
		return func(p []byte) (int, error) { time.Sleep(d); return len(p), nil }
	}
	b.write(make([]byte, hintWriteMin-1), slow(30*time.Millisecond))
	if v := b.total(ctrlNow()); v != 0 {
		t.Fatalf("a small write counted: %v", time.Duration(v))
	}
	b.write(make([]byte, hintWriteMin), slow(30*time.Millisecond))
	if v := time.Duration(b.total(ctrlNow())); v < 25*time.Millisecond || v > 200*time.Millisecond {
		t.Fatalf("a big write of 30 ms counted %v", v)
	}
	done := make(chan struct{})
	go b.write(make([]byte, 32<<10), func(p []byte) (int, error) { <-done; return len(p), nil })
	time.Sleep(50 * time.Millisecond)
	if v := time.Duration(b.total(ctrlNow())); v < 70*time.Millisecond {
		t.Fatalf("the write in progress not counted: %v", v)
	}
	close(done)
}

// The rule: busy in big writes past hintBusy for hintRun ticks in a row;
// a gap, or a busy share under it, starts it again.
func TestHintRule(t *testing.T) {
	tick := int64(moveTick)
	run := func(share []float64) int {
		var r hintRule
		now := int64(time.Hour)
		var busy int64
		r.tick(busy, now)
		for i, s := range share {
			now += tick
			busy += int64(s * float64(tick))
			if r.tick(busy, now) {
				return i
			}
		}
		return -1
	}
	rep := func(n int, v float64) []float64 {
		out := make([]float64, n)
		for i := range out {
			out[i] = v
		}
		return out
	}
	if at := run(rep(10, 1)); at != hintRun-1 {
		t.Fatalf("a backlog: due at tick %d, want %d (300 ms)", at, hintRun-1)
	}
	if at := run(rep(50, 0.4)); at >= 0 {
		t.Fatalf("40%% busy: due at tick %d", at)
	}
	alt := make([]float64, 50)
	for i := range alt {
		if i%3 != 2 {
			alt[i] = 1
		}
	}
	if at := run(alt); at >= 0 {
		t.Fatalf("two busy ticks, one not, again: due at %d", at)
	}
	// a skipped tick (a late runner) is no run
	var r hintRule
	now := int64(time.Hour)
	r.tick(0, now)
	now += 3 * tick
	if r.tick(3*tick, now) {
		t.Fatal("three ticks at once counted")
	}
}

// The edge's own rule with the hint on (rxRule false): what arrives never
// moves a connection, the upload rule is as it was. The app-limited call of
// TestMoveRuleAppLimitedCall (1.5 Mbit/s each way) stays.
func TestMoveRuleNoRxWithHint(t *testing.T) {
	var r moveRule
	var now int64
	var tx, rx uint64
	var busy time.Duration
	per := uint64(1.5e6 / 8 / float64(time.Second/moveTick))
	for i := 0; i < 100; i++ {
		now += int64(moveTick)
		tx += per
		rx += 60 << 10 // even a download's pace
		busy += time.Millisecond
		if due, dir := r.tick(tx, rx, busy, now, true, false); due {
			t.Fatalf("moved %s at tick %d with the hint on", dir, i)
		}
	}
	var u moveRule
	now, tx, busy = 0, 0, 0
	for i := 0; i < 10; i++ {
		now += int64(moveTick)
		tx += 40 << 10
		busy += moveTick
		if due, dir := u.tick(tx, 0, busy, now, true, false); due {
			if dir != "up" || i > 2 {
				t.Fatalf("an upload with a backlog: %s at tick %d", dir, i)
			}
			return
		}
	}
	t.Fatal("an upload with a backlog never moved with the hint on")
}

// hintRig is newQuietRig with the hint: the edge's kindHint stream on the
// QUIET link and the exit's hint loop, as RunIran and RunKharej run them.
func hintRig(t *testing.T, rate float64, delay time.Duration) *quietRig {
	t.Helper()
	if MoveHintOff() {
		t.Skip("HS2_TUNE_MOVE_HINT=0")
	}
	r := newQuietRig(t, rate, delay, false)
	if r.quiet.edge.caps&tlscarrier.CapHint == 0 {
		t.Fatalf("the QUIET link's caps %#x lack CapHint", r.quiet.edge.caps)
	}
	go openHint(r.ctx, r.quiet.edge, r.q)
	go runExitHints(r.ctx)
	waitQuiet(t, "the hint stream up", func() bool { return r.q.hinting(r.quiet.edge) })
	return r
}

// A download on the QUIET link moves on the exit's hint (the edge's own
// rule for what arrives is off while the hint stream is up), within a
// second, and the bytes arrive whole and in order.
func TestHintMovesDownload(t *testing.T) {
	angleCores(t)
	r := hintRig(t, 4*angleRateBps, 10*time.Millisecond)
	st := r.open(5, qpDown)
	var got atomic.Int64
	go func() {
		b := make([]byte, 64<<10)
		for {
			n, err := st.Read(b)
			got.Add(int64(n))
			if err != nil {
				return
			}
		}
	}()
	t0 := time.Now()
	waitQuiet(t, "the download moved on the hint", func() bool { return r.q.moved.Load() == 1 })
	if r.q.hinted.Load() != 1 {
		t.Fatalf("moved with %d hints", r.q.hinted.Load())
	}
	t.Logf("moved %v after it opened, %d B read", time.Since(t0), got.Load())
	if d := time.Since(t0); d > 2*time.Second {
		t.Fatalf("the hint took %v", d)
	}
	n0 := got.Load()
	time.Sleep(500 * time.Millisecond)
	if got.Load() <= n0 {
		t.Fatal("nothing read after the move")
	}
	st.Close()
}

// An app-limited sender at the exit (a 1.5 Mbit/s call: a 3.75 KB frame
// each 20 ms, a 19 KB key frame each 2 s) is never hinted, so it stays on
// the QUIET link, while the edge's old rule for what arrives (the hint off)
// moves it within a second (TestMoveRuleAppLimitedCall).
func TestHintCallStays(t *testing.T) {
	angleCores(t)
	if testing.Short() {
		t.Skip("emulated links, 4 s")
	}
	r := hintRig(t, 4*angleRateBps, 10*time.Millisecond)
	st := r.open(6, qpEcho)
	go io.Copy(io.Discard, st)
	stop := time.After(4 * time.Second)
	tk := time.NewTicker(20 * time.Millisecond)
	defer tk.Stop()
	frame := make([]byte, 3750)
	key := make([]byte, 5*3750)
	for i := 0; ; i++ {
		select {
		case <-stop:
			if r.q.moved.Load() != 0 || r.q.hinted.Load() != 0 {
				t.Fatalf("a 1.5 Mbit/s call: moved %d, hinted %d", r.q.moved.Load(), r.q.hinted.Load())
			}
			st.Close()
			return
		case <-tk.C:
		}
		p := frame
		if i%100 == 0 {
			p = key
		}
		binary.BigEndian.PutUint32(p, uint32(i))
		if _, err := st.Write(p); err != nil && !errors.Is(err, io.ErrClosedPipe) {
			t.Fatal(err)
		}
	}
}
