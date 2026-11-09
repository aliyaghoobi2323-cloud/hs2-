package engine

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"syscall"
	"testing"
)

// A bucket holds every value between its bounds and its midpoint is within
// ~12% of any value in it, so a quantile read back is that close.
func TestObsBucketAccuracy(t *testing.T) {
	prev := -1
	for v := uint64(0); v < 1<<22; v += 1 + v/97 {
		i := obsBucket(v)
		if i < prev {
			t.Fatalf("bucket of %d is %d, below the bucket %d of a smaller value", v, i, prev)
		}
		prev = i
		mid := obsBucketMid(i)
		if v >= 8 {
			if d := float64(mid) - float64(v); d > 0.125*float64(v) || -d > 0.125*float64(v) {
				t.Fatalf("value %d: bucket %d midpoint %d is off by more than 12.5%%", v, i, mid)
			}
		} else if mid != v { // below 8 every value has a bucket of its own
			t.Fatalf("small value %d: midpoint %d", v, mid)
		}
	}
	if i := obsBucket(^uint64(0)); i >= obsHistBuckets {
		t.Fatalf("largest value maps to bucket %d of %d", i, obsHistBuckets)
	}
}

func TestObsHistQuantile(t *testing.T) {
	var h obsHist
	if _, ok := h.snapshot().Quantile(0.5); ok {
		t.Fatal("an empty histogram has no quantile")
	}
	before := h.snapshot()
	for v := uint64(1); v <= 1000; v++ { // 1..1000 µs
		h.observe(v * 1000)
	}
	d := h.snapshot().Sub(before)
	if d.Count() != 1000 {
		t.Fatalf("count %d, want 1000", d.Count())
	}
	for _, c := range []struct{ q, want float64 }{{0.5, 500e3}, {0.99, 990e3}, {0, 1e3}} {
		v, _ := d.Quantile(c.q)
		if f := float64(v) / c.want; f < 0.88 || f > 1.12 {
			t.Errorf("q%.2f = %d, want ~%.0f", c.q, v, c.want)
		}
	}
}

// The shaper's counters add up to exactly what it put on the conn, and
// counting changes nothing it writes.
func TestObsShapeCounters(t *testing.T) {
	noShapeBulk(t) // these sizes are the default shaper's
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	sc := newShapedConn(a, nil)
	got := make(chan []byte)
	var wire bytes.Buffer
	go func() {
		rd := newShapedConn(&teeConn{Conn: b, w: &wire}, nil)
		var out bytes.Buffer
		io.CopyN(&out, rd, 3+40000)
		got <- out.Bytes()
	}()
	before := Observed()
	payload := append([]byte{1, 2, 3}, bytes.Repeat([]byte{7}, 40000)...)
	if _, err := sc.Write(payload[:3]); err != nil {
		t.Fatal(err)
	}
	if _, err := sc.Write(payload[3:]); err != nil {
		t.Fatal(err)
	}
	if out := <-got; !bytes.Equal(out, payload) {
		t.Fatal("the stream through the shaper changed")
	}
	d := Observed().Sub(before)
	if d.ShapeWrites != 2 || d.ShapeData != uint64(len(payload)) {
		t.Fatalf("writes %d data %d, want 2 and %d", d.ShapeWrites, d.ShapeData, len(payload))
	}
	if d.ShapeData+d.ShapePad != uint64(wire.Len()) {
		t.Fatalf("data %d + added %d != %d bytes on the conn", d.ShapeData, d.ShapePad, wire.Len())
	}
	if d.ShapeRecords < 2 {
		t.Fatalf("records %d", d.ShapeRecords)
	}
}

type teeConn struct {
	net.Conn
	w io.Writer
}

func (c *teeConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.w.Write(p[:n])
	return n, err
}

func TestObsExitDialCauses(t *testing.T) {
	before := Observed()
	noteExitDial(nil)
	noteExitDial(&net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.EADDRNOTAVAIL)})
	noteExitDial(&net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)})
	noteExitDial(obsTimeoutErr{})
	noteExitDial(errors.New("no route"))
	d := Observed().Sub(before)
	want := [5]uint64{5, 1, 1, 1, 1}
	if have := [5]uint64{d.ExitDials, d.ExitNoAddr, d.ExitRefused, d.ExitTimeout, d.ExitOther}; have != want {
		t.Fatalf("dials/noaddr/refused/timeout/other = %v, want %v", have, want)
	}
}

type obsTimeoutErr struct{}

func (obsTimeoutErr) Error() string   { return "i/o timeout" }
func (obsTimeoutErr) Timeout() bool   { return true }
func (obsTimeoutErr) Temporary() bool { return true }

// A control round trip lands in the whole histogram and in the light or the
// busy one by its link: a download-rate user stream (heavyN) or a stripe sub
// (stripeN) makes the link busy, and the light split stays free of it.
func TestObsCtrlRTTSplit(t *testing.T) {
	var light, heavy, stripe linkMeter
	heavy.heavyN.Store(2)
	stripe.stripeN.Store(1)
	if ctrlLinkBusy(nil) || ctrlLinkBusy(&light) || !ctrlLinkBusy(&heavy) || !ctrlLinkBusy(&stripe) {
		t.Fatal("busy: only a link with a heavy stream or a stripe sub")
	}
	before := Observed()
	for i := 0; i < 100; i++ {
		observeCtrlRTT(80_000, ctrlLinkBusy(&light)) // 80 ms on light links
	}
	for i := 0; i < 50; i++ {
		observeCtrlRTT(900_000, ctrlLinkBusy(&heavy)) // 900 ms behind a download
		observeCtrlRTT(1_500_000, ctrlLinkBusy(&stripe))
	}
	d := Observed().Sub(before)
	if d.CtrlRTT.Count() != 200 || d.CtrlRTTLight.Count() != 100 || d.CtrlRTTBusy.Count() != 100 {
		t.Fatalf("counts all %d light %d busy %d, want 200/100/100", d.CtrlRTT.Count(), d.CtrlRTTLight.Count(), d.CtrlRTTBusy.Count())
	}
	near := func(v uint64, want float64) bool { f := float64(v) / want; return f > 0.88 && f < 1.12 }
	if v, _ := d.CtrlRTTLight.Quantile(0.99); !near(v, 80e3) {
		t.Errorf("light p99 %d µs, want ~80 ms: a busy link leaked in", v)
	}
	if v, _ := d.CtrlRTTBusy.Quantile(0.5); v < 800e3 {
		t.Errorf("busy p50 %d µs, want ≥ 900 ms", v)
	}
	if v, _ := d.CtrlRTT.Quantile(0.99); v < 1_300e3 {
		t.Errorf("whole p99 %d µs, want the busy tail (kept for old readers)", v)
	}
}
